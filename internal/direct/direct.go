// Package direct implements PAYVRO's standalone, operator-owned pey MCP mode.
// It deliberately has no connection to the store/Convex client.
package direct

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	redactor "github.com/GNOSIXSI/payvro-mcp/internal/redact"
)

const (
	providerBase       = "https://api.pey.mx"
	requestTimeout     = 10 * time.Second
	maxResponseBytes   = 64 * 1024
	maxJournalBytes    = 10 * 1024 * 1024
	maxJournalEntries  = 100_000
	maxConceptBytes    = 100
	maxQRBytes         = 4096
	maxPaymentURLBytes = 2048
	maxPages           = 5
	maxTransactions    = 20
)

var (
	ErrInvalidConfig       = errors.New("direct configuration is invalid")
	ErrInvalidInput        = errors.New("charge input is invalid")
	ErrIdempotencyConflict = errors.New("idempotency key is already bound to a different intent")
	ErrJournalUnavailable  = errors.New("local charge journal unavailable; manual review required")
	ErrUncertain           = errors.New("UNCERTAIN: provider outcome unknown; do not retry; manual review required")
	idRE                   = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
	keyRE                  = regexp.MustCompile(`^[A-Za-z0-9_-]{16,128}$`)
	amountEnvRE            = regexp.MustCompile(`^[1-9][0-9]{0,15}$`)
	decimalRE              = regexp.MustCompile(`^(0|[1-9][0-9]{0,15})(?:\.([0-9]{1,2}))?$`)
	bearerCredentialRE     = regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/-]{8,}`)
	jwtCredentialRE        = regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\b`)
	keyCredentialRE        = regexp.MustCompile(`(?i)\b(?:sk|pk)_(?:live|test)_[A-Za-z0-9]{12,}\b|\b(?:api[_-]?key|access[_-]?token)\s*[:=]\s*[A-Za-z0-9._~+/=-]{12,}`)
	financialDigitsRE      = regexp.MustCompile(`(^|[^0-9])(?:[0-9][ -]?){12,18}[0-9]([^0-9]|$)`)
)

type Config struct {
	Mode          string
	MaxAmount     int64
	APIKey        string
	APIKeyID      string
	BankAccountID string
	PublicOrigin  string
	JournalPath   string
	Transport     http.RoundTripper // test injection only; never configurable from environment
}

type Charge struct {
	ChargeID       string `json:"chargeId,omitempty"`
	Method         string `json:"method"`
	Status         string `json:"status"`
	Mode           string `json:"mode"`
	AmountCentavos int64  `json:"amountCentavos"`
	Currency       string `json:"currency"`
	Concept        string `json:"concept,omitempty"`
	QRPayload      string `json:"qrPayload,omitempty"`
	PaymentURL     string `json:"paymentUrl,omitempty"`
	ManualReview   bool   `json:"manualReview,omitempty"`
	Message        string `json:"message"`
}

type Service struct {
	cfg     Config
	account string
	journal *journal
	http    *http.Client
}

func FromEnv() (*Service, error) {
	if os.Getenv("PAYVRO_API_BASE_URL") != "" || os.Getenv("PAYVRO_MERCHANT_TOKEN") != "" {
		return nil, configError("store and direct credentials cannot be configured together")
	}
	maxRaw := os.Getenv("PAYVRO_MAX_AMOUNT_CENTAVOS")
	if !amountEnvRE.MatchString(maxRaw) {
		return nil, configError("PAYVRO_MAX_AMOUNT_CENTAVOS must be a positive safe integer")
	}
	max, err := strconv.ParseInt(maxRaw, 10, 64)
	if err != nil || max > 9007199254740991 {
		return nil, configError("PAYVRO_MAX_AMOUNT_CENTAVOS must be a positive safe integer")
	}
	cfg := Config{
		Mode:          os.Getenv("PAYVRO_MODE"),
		MaxAmount:     max,
		APIKey:        os.Getenv("PEY_API_KEY"),
		APIKeyID:      os.Getenv("PEY_API_KEY_ID"),
		BankAccountID: os.Getenv("PEY_BANK_ACCOUNT_ID"),
		PublicOrigin:  os.Getenv("PAYVRO_PUBLIC_ORIGIN"),
		JournalPath:   os.Getenv("PAYVRO_DIRECT_JOURNAL"),
	}
	return New(cfg)
}

func New(cfg Config) (*Service, error) {
	if cfg.Mode != "DEMO" && cfg.Mode != "LIVE" {
		return nil, configError("PAYVRO_MODE must be exactly DEMO or LIVE")
	}
	if cfg.MaxAmount <= 0 || cfg.MaxAmount > 9007199254740991 {
		return nil, configError("PAYVRO_MAX_AMOUNT_CENTAVOS must be a positive safe integer")
	}
	if !idRE.MatchString(cfg.APIKeyID) {
		return nil, configError("PEY_API_KEY_ID is required to bind the journal to an operator account")
	}
	if cfg.BankAccountID != "" && !idRE.MatchString(cfg.BankAccountID) {
		return nil, configError("PEY_BANK_ACCOUNT_ID is invalid")
	}
	if cfg.Mode == "LIVE" && (cfg.APIKey == "" || strings.TrimSpace(cfg.APIKey) != cfg.APIKey || strings.ContainsAny(cfg.APIKey, "\r\n")) {
		return nil, configError("PEY_API_KEY is required in LIVE")
	}
	if cfg.Mode == "LIVE" && cfg.PublicOrigin != "" {
		if _, err := validateOrigin(cfg.PublicOrigin); err != nil {
			return nil, configError("PAYVRO_PUBLIC_ORIGIN must be an operator-controlled HTTPS origin")
		}
	}
	journalPath, err := resolveJournalPath(cfg.JournalPath)
	if errors.Is(err, ErrJournalUnavailable) {
		return nil, ErrJournalUnavailable
	}
	if err != nil {
		return nil, configError("PAYVRO_DIRECT_JOURNAL must be a safe local dedicated .jsonl path")
	}
	cfg.JournalPath = journalPath
	accountInput := cfg.Mode + "\x00" + cfg.APIKeyID + "\x00" + cfg.BankAccountID
	accountHash := sha256.Sum256([]byte(accountInput))
	transport := noReplayTransport(cfg.Transport)
	return &Service{
		cfg:     cfg,
		account: hex.EncodeToString(accountHash[:]),
		journal: &journal{path: cfg.JournalPath},
		http: &http.Client{
			Timeout:       requestTimeout,
			Transport:     transport,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}, nil
}

func (s *Service) CreateCharge(ctx context.Context, amount int64, concept, method, idempotencyKey string) (Charge, error) {
	if amount <= 0 || amount > 9007199254740991 || amount > s.cfg.MaxAmount || !validConcept(concept) || containsSensitiveConceptMaterial(concept, s.cfg.APIKey) || (method != "qr" && method != "link") || !keyRE.MatchString(idempotencyKey) {
		return Charge{}, ErrInvalidInput
	}
	if method == "link" && s.cfg.Mode == "LIVE" {
		if _, err := validateOrigin(s.cfg.PublicOrigin); err != nil {
			return Charge{}, configError("PAYVRO_PUBLIC_ORIGIN is required for direct payment links")
		}
	}
	keyHash := digest(idempotencyKey)
	intentHash := digest(fmt.Sprintf("%s\x00%d\x00%s", method, amount, concept))
	event, fresh, err := s.journal.Reserve(s.account, keyHash, intentHash, method, amount, s.cfg.Mode)
	if err != nil {
		return Charge{}, err
	}
	if !fresh {
		switch event.State {
		case "created":
			return event.Charge, nil
		case "reserved", "uncertain":
			return uncertainCharge(event), ErrUncertain
		case "rejected":
			return Charge{}, errors.New("provider rejected this charge intent; it was not retried")
		default:
			return Charge{}, ErrJournalUnavailable
		}
	}

	if s.cfg.Mode == "DEMO" {
		fake := demoCharge(idempotencyKey, method, amount, concept)
		if err := s.journal.Finish(s.account, keyHash, intentHash, "created", fake, ""); err != nil {
			return uncertainCharge(event), ErrUncertain
		}
		return fake, nil
	}

	var ref string
	for tries := 0; tries < 16; tries++ {
		candidate, err := newReference(method)
		if err != nil {
			_ = s.journal.Finish(s.account, keyHash, intentHash, "uncertain", Charge{}, "")
			return uncertainCharge(event), ErrUncertain
		}
		err = s.journal.SetReference(s.account, keyHash, candidate)
		if errors.Is(err, errReferenceCollision) {
			continue
		}
		if err != nil {
			return uncertainCharge(event), ErrUncertain
		}
		ref = candidate
		break
	}
	if ref == "" {
		_ = s.journal.Finish(s.account, keyHash, intentHash, "uncertain", Charge{}, "")
		return uncertainCharge(event), ErrUncertain
	}
	created, uncertain, rejected := s.createProviderResource(ctx, amount, concept, method, ref)
	if rejected != "" {
		if err := s.journal.Finish(s.account, keyHash, intentHash, "rejected", Charge{}, rejected); err != nil {
			return uncertainCharge(event), ErrUncertain
		}
		return Charge{}, errors.New("provider rejected charge request; no automatic retry was made")
	}
	if uncertain {
		if created.ChargeID != "" && idRE.MatchString(created.ChargeID) {
			created.Method = method
			created.Status = "UNCERTAIN"
			created.Mode = "LIVE"
			created.AmountCentavos = amount
			created.Currency = "MXN"
			created.Concept = concept
			created.ManualReview = true
			created.Message = "La respuesta de creación fue inválida, pero se conservó un ID válido. Consulta get_charge_status; no se reintenta el POST."
			if err := s.journal.Finish(s.account, keyHash, intentHash, "uncertain", created, ""); err != nil {
				return uncertainCharge(event), ErrUncertain
			}
			event.Charge = created
			return uncertainCharge(event), ErrUncertain
		}
		_ = s.journal.Finish(s.account, keyHash, intentHash, "uncertain", Charge{}, "")
		return uncertainCharge(event), ErrUncertain
	}
	created.Method = method
	created.Status = "CREATED"
	created.Mode = "LIVE"
	created.AmountCentavos = amount
	created.Currency = "MXN"
	created.Concept = concept
	created.Message = "Recurso creado. El estado de pago aún no está verificado; consulta get_charge_status."
	if err := s.journal.Finish(s.account, keyHash, intentHash, "created", created, ""); err != nil {
		return uncertainCharge(event), ErrUncertain
	}
	return created, nil
}

func (s *Service) GetChargeStatus(ctx context.Context, idempotencyKey string) (Charge, error) {
	if !keyRE.MatchString(idempotencyKey) {
		return Charge{}, ErrInvalidInput
	}
	event, ok, err := s.journal.Find(s.account, digest(idempotencyKey))
	if err != nil {
		return Charge{}, err
	}
	if !ok {
		return Charge{}, errors.New("charge intent not found in this operator's local journal")
	}
	switch event.State {
	case "reserved":
		return uncertainCharge(event), ErrUncertain
	case "uncertain":
		if event.Charge.ChargeID == "" || !idRE.MatchString(event.Charge.ChargeID) {
			return uncertainCharge(event), ErrUncertain
		}
		return s.readProviderStatus(ctx, event)
	case "rejected":
		return Charge{}, errors.New("provider rejected this charge intent")
	case "created":
		if event.Charge.Mode == "DEMO" {
			return event.Charge, nil
		}
		return s.readProviderStatus(ctx, event)
	default:
		return Charge{}, ErrJournalUnavailable
	}
}

func (s *Service) createProviderResource(ctx context.Context, amount int64, concept, method, reference string) (Charge, bool, string) {
	var path string
	var body any
	if method == "qr" {
		path = "/codi/qr"
		fields := map[string]any{
			"amount": json.Number(formatMXN(amount)), "concept": concept, "numRef": reference,
			"expiration": 600, "generateImage": false,
		}
		if s.cfg.BankAccountID != "" {
			fields["bankAccountId"] = s.cfg.BankAccountID
		}
		body = fields
	} else {
		origin, err := validateOrigin(s.cfg.PublicOrigin)
		if err != nil {
			return Charge{}, false, ""
		}
		path = "/payment-link/create"
		body = map[string]any{
			"amount": json.Number(formatMXN(amount)), "successUrl": origin + "/payment/return",
			"backUrl": origin + "/payment/return", "hasTip": false, "clientReferenceId": reference,
		}
	}
	payload, status, err := s.request(ctx, http.MethodPost, path, body)
	if err != nil {
		return Charge{}, true, ""
	}
	if status < 200 || status >= 300 {
		if status >= 300 || status == http.StatusRequestTimeout || status == http.StatusTooManyRequests || status >= 500 {
			return Charge{}, true, ""
		}
		return Charge{}, false, "rejected"
	}
	if containsActiveCredential(string(payload), s.cfg.APIKey) {
		return Charge{}, true, ""
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(payload, &obj) != nil {
		return Charge{}, true, ""
	}
	id := rawString(obj["id"])
	if !idRE.MatchString(id) {
		return Charge{}, true, ""
	}
	out := Charge{ChargeID: id}
	if method == "qr" {
		qr := rawString(obj["qrString"])
		if !validQRPayload(qr, id) {
			return out, true, ""
		}
		out.QRPayload = qr
	} else {
		paymentURL := rawString(obj["url"])
		if !validHostedPaymentURL(paymentURL, id) {
			return out, true, ""
		}
		out.PaymentURL = paymentURL
	}
	return out, false, ""
}

func (s *Service) readProviderStatus(ctx context.Context, event journalEvent) (Charge, error) {
	out := event.Charge
	if out.ChargeID == "" || !idRE.MatchString(out.ChargeID) {
		return uncertainCharge(event), ErrUncertain
	}
	if out.Method == "qr" {
		status, err := s.readQRStatus(ctx, out.ChargeID, event.AmountCentavos)
		if err != nil {
			return Charge{}, err
		}
		out.Status = status
	} else {
		status, err := s.readLinkStatus(ctx, out.ChargeID, event.AmountCentavos, event.Reference)
		if err != nil {
			return Charge{}, err
		}
		out.Status = status
	}
	if out.Status == "PAID" {
		out.Message = "El GET autenticado reporta PAID y pasó las validaciones locales de identidad, MXN y monto. PAYVRO directo no verifica webhooks ni liquida en la tienda."
	} else {
		out.Message = "Estado leído por GET autenticado; no se afirma PAID. Si es desconocido o inconsistente, requiere revisión manual."
	}
	return out, nil
}

func (s *Service) readQRStatus(ctx context.Context, resourceID string, expected int64) (string, error) {
	cursor := ""
	seenCursor := map[string]bool{}
	var summaries []map[string]json.RawMessage
	for page := 0; page < maxPages; page++ {
		path := "/transaction/codi/qr/" + url.PathEscape(resourceID)
		if cursor != "" {
			path += "?nextToken=" + url.QueryEscape(cursor)
		}
		payload, status, err := s.request(ctx, http.MethodGet, path, nil)
		if err != nil {
			return "", errors.New("provider status GET failed")
		}
		if status < 200 || status >= 300 {
			return "", errors.New("provider status GET was rejected")
		}
		var root struct {
			Items     []map[string]json.RawMessage `json:"items"`
			NextToken string                       `json:"nextToken"`
		}
		if json.Unmarshal(payload, &root) != nil || root.Items == nil {
			return "NEEDS_REVIEW", nil
		}
		summaries = append(summaries, root.Items...)
		if len(summaries) > maxTransactions {
			return "NEEDS_REVIEW", nil
		}
		if root.NextToken == "" {
			break
		}
		if len(root.NextToken) > 2048 || seenCursor[root.NextToken] || page == maxPages-1 {
			return "NEEDS_REVIEW", nil
		}
		seenCursor[root.NextToken] = true
		cursor = root.NextToken
	}
	if len(summaries) == 0 {
		return "PENDING", nil
	}

	// The QR collection endpoint is only a summary. Resolve every bounded, unique
	// transaction ID through the fixed authenticated detail endpoint before using
	// any listed status as payment evidence.
	ids := make([]string, 0, len(summaries))
	seenIDs := make(map[string]bool, len(summaries))
	for _, summary := range summaries {
		id := rawString(summary["id"])
		if !idRE.MatchString(id) || seenIDs[id] {
			return "NEEDS_REVIEW", nil
		}
		seenIDs[id] = true
		ids = append(ids, id)
	}
	details := make([]map[string]json.RawMessage, 0, len(ids))
	for _, id := range ids {
		payload, status, err := s.request(ctx, http.MethodGet, "/transaction/"+url.PathEscape(id), nil)
		if err != nil {
			return "", errors.New("provider transaction GET failed")
		}
		if status < 200 || status >= 300 {
			return "", errors.New("provider transaction GET was rejected")
		}
		var detail map[string]json.RawMessage
		if json.Unmarshal(payload, &detail) != nil || detail == nil {
			return "NEEDS_REVIEW", nil
		}
		details = append(details, detail)
	}

	if len(summaries) != 1 {
		return "NEEDS_REVIEW", nil
	}
	summary, detail := summaries[0], details[0]
	listed, ok := parseQRListSummary(summary, resourceID, expected)
	if !ok || rawString(detail["id"]) != listed.id || !checkTransaction(detail, resourceID, "API_CODI_QR", expected, true) {
		return "NEEDS_REVIEW", nil
	}
	if !qrSummaryMatchesDetail(listed, detail) {
		return "NEEDS_REVIEW", nil
	}
	state := strings.ToUpper(rawString(detail["status"]))
	if state != "PAID" {
		return normalizedNonPaid(state), nil
	}
	return "PAID", nil
}

type qrListSummary struct {
	id, status, currency, paymentMethod string
	amount, base, fee, tip              int64
}

func parseQRListSummary(tx map[string]json.RawMessage, resourceID string, expected int64) (qrListSummary, bool) {
	id := rawString(tx["id"])
	status := strings.ToUpper(rawString(tx["status"]))
	switch status {
	case "PAID", "PENDING", "PROCESSING", "CREATED", "FAILED", "EXPIRED", "CANCELLED", "CANCELED":
	default:
		return qrListSummary{}, false
	}
	currency, method := rawString(tx["currency"]), rawString(tx["paymentMethod"])
	if !idRE.MatchString(id) || currency != "MXN" || method != "CODI" {
		return qrListSummary{}, false
	}
	for key, want := range map[string]string{"payableId": resourceID, "payableType": "API_CODI_QR"} {
		if raw, present := tx[key]; present && rawString(raw) != want {
			return qrListSummary{}, false
		}
	}
	breakdown := rawObject(tx["amountBreakdown"])
	if breakdown == nil {
		return qrListSummary{}, false
	}
	base := moneyCentavos(breakdown["base"])
	fee, tip := optionalMoneyCentavos(breakdown["fee"]), optionalMoneyCentavos(breakdown["tip"])
	amount := moneyCentavos(tx["amount"])
	if base <= 0 || base != expected || fee != 0 || tip != 0 || amount != base || amount < 0 {
		return qrListSummary{}, false
	}
	return qrListSummary{id: id, status: status, currency: currency, paymentMethod: method, amount: amount, base: base, fee: fee, tip: tip}, true
}

func qrSummaryMatchesDetail(summary qrListSummary, detail map[string]json.RawMessage) bool {
	breakdown := rawObject(detail["amountBreakdown"])
	if breakdown == nil {
		return false
	}
	return summary.status == strings.ToUpper(rawString(detail["status"])) &&
		summary.currency == rawString(detail["currency"]) &&
		summary.paymentMethod == rawString(detail["paymentMethod"]) &&
		summary.amount == moneyCentavos(detail["amount"]) &&
		summary.base == moneyCentavos(breakdown["base"]) &&
		summary.fee == optionalMoneyCentavos(breakdown["fee"]) &&
		summary.tip == optionalMoneyCentavos(breakdown["tip"])
}

func (s *Service) readLinkStatus(ctx context.Context, id string, expected int64, reference string) (string, error) {
	payload, status, err := s.request(ctx, http.MethodGet, "/payment-link/"+url.PathEscape(id), nil)
	if err != nil {
		return "", errors.New("provider status GET failed")
	}
	if status < 200 || status >= 300 {
		return "", errors.New("provider status GET was rejected")
	}
	var root struct {
		ID                string                   `json:"id"`
		Amount            json.RawMessage          `json:"amount"`
		Status            string                   `json:"status"`
		HasTip            *bool                    `json:"hasTip"`
		ClientReferenceID string                   `json:"clientReferenceId"`
		Transactions      []linkTransactionSummary `json:"transactions"`
	}
	if json.Unmarshal(payload, &root) != nil || root.ID != id || root.Transactions == nil || root.HasTip == nil || *root.HasTip || root.ClientReferenceID != reference || moneyCentavos(root.Amount) != expected {
		return "NEEDS_REVIEW", nil
	}
	state := strings.ToLower(root.Status)
	if len(root.Transactions) == 0 {
		if state == "paid" {
			return "NEEDS_REVIEW", nil
		}
		return normalizeLinkState(state), nil
	}
	if len(root.Transactions) > maxTransactions {
		return "NEEDS_REVIEW", nil
	}
	allPaid := true
	var baseTotal int64
	seen := map[string]bool{}
	for _, item := range root.Transactions {
		if !idRE.MatchString(item.TransactionID) || seen[item.TransactionID] || !validPaymentMethod(item.PaymentMethod) {
			return "NEEDS_REVIEW", nil
		}
		seen[item.TransactionID] = true
		txPayload, txStatus, txErr := s.request(ctx, http.MethodGet, "/transaction/"+url.PathEscape(item.TransactionID), nil)
		if txErr != nil || txStatus < 200 || txStatus >= 300 {
			return "", errors.New("provider transaction GET failed")
		}
		var tx map[string]json.RawMessage
		if json.Unmarshal(txPayload, &tx) != nil || rawString(tx["id"]) != item.TransactionID || !checkTransaction(tx, id, "PAYMENT_LINK", expected, false) || !linkSummaryMatchesDetail(item, tx) {
			return "NEEDS_REVIEW", nil
		}
		amount := breakdownBase(tx["amountBreakdown"])
		if amount <= 0 || baseTotal > math.MaxInt64-amount {
			return "NEEDS_REVIEW", nil
		}
		baseTotal += amount
		if strings.ToUpper(rawString(tx["status"])) != "PAID" {
			allPaid = false
		}
	}
	if baseTotal != expected {
		return "NEEDS_REVIEW", nil
	}
	if !allPaid || state != "paid" {
		return normalizeLinkState(state), nil
	}
	return "PAID", nil
}

func (s *Service) request(ctx context.Context, method, path string, body any) ([]byte, int, error) {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, 0, errors.New("provider request could not be encoded")
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, providerBase+path, reader)
	if err != nil {
		return nil, 0, errors.New("provider request could not be built")
	}
	req.GetBody = nil
	req.Close = true
	req.Header.Set("Authorization", "Bearer "+s.cfg.APIKey)
	req.Header.Set("Accept", "application/json")
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return nil, 0, errors.New("provider request failed")
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil || len(data) > maxResponseBytes {
		return nil, resp.StatusCode, errors.New("provider response unavailable or oversized")
	}
	return data, resp.StatusCode, nil
}

type linkTransactionSummary struct {
	TransactionID   string          `json:"transactionId"`
	PaymentMethod   string          `json:"paymentMethod"`
	Amount          json.RawMessage `json:"amount"`
	AmountBreakdown json.RawMessage `json:"amountBreakdown"`
}

func validPaymentMethod(method string) bool { return method == "CARD" || method == "CODI" }

func linkSummaryMatchesDetail(summary linkTransactionSummary, detail map[string]json.RawMessage) bool {
	if !validPaymentMethod(summary.PaymentMethod) || summary.PaymentMethod != rawString(detail["paymentMethod"]) {
		return false
	}
	summaryBreakdown := rawObject(summary.AmountBreakdown)
	detailBreakdown := rawObject(detail["amountBreakdown"])
	if summaryBreakdown == nil || detailBreakdown == nil {
		return false
	}
	summaryAmount := moneyCentavos(summary.Amount)
	summaryBase := moneyCentavos(summaryBreakdown["base"])
	summaryFee := optionalMoneyCentavos(summaryBreakdown["fee"])
	summaryTip := optionalMoneyCentavos(summaryBreakdown["tip"])
	detailAmount := moneyCentavos(detail["amount"])
	detailBase := moneyCentavos(detailBreakdown["base"])
	detailFee := optionalMoneyCentavos(detailBreakdown["fee"])
	detailTip := optionalMoneyCentavos(detailBreakdown["tip"])
	return summaryAmount >= 0 && summaryBase > 0 && summaryFee >= 0 && summaryTip >= 0 &&
		summaryAmount == detailAmount && summaryBase == detailBase && summaryFee == detailFee && summaryTip == detailTip
}

func checkTransaction(tx map[string]json.RawMessage, resourceID, payableType string, expected int64, qr bool) bool {
	status := strings.ToUpper(rawString(tx["status"]))
	switch status {
	case "PAID", "PENDING", "PROCESSING", "CREATED", "FAILED", "EXPIRED", "CANCELLED", "CANCELED":
	default:
		return false
	}
	currency := rawString(tx["currency"])
	if currency != "MXN" {
		return false
	}
	payable := rawString(tx["payableId"])
	typ := rawString(tx["payableType"])
	if payable != resourceID || typ != payableType {
		return false
	}
	id := rawString(tx["id"])
	if !idRE.MatchString(id) {
		return false
	}
	breakdown := rawObject(tx["amountBreakdown"])
	if breakdown == nil {
		return false
	}
	base := moneyCentavos(breakdown["base"])
	fee := optionalMoneyCentavos(breakdown["fee"])
	tip := optionalMoneyCentavos(breakdown["tip"])
	amount := moneyCentavos(tx["amount"])
	if base <= 0 || (qr && base != expected) || tip != 0 || fee < 0 || amount < 0 || base > math.MaxInt64-fee || base+fee > math.MaxInt64-tip || amount != base+fee+tip {
		return false
	}
	if qr && fee != 0 {
		return false
	}
	if qr && rawString(tx["paymentMethod"]) != "CODI" {
		return false
	}
	if !qr && !validPaymentMethod(rawString(tx["paymentMethod"])) {
		return false
	}
	return true
}

func moneyCentavos(raw json.RawMessage) int64 {
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return -1
	}
	value := strings.TrimSpace(string(raw))
	if value == "" {
		return -1
	}
	if value[0] == '"' {
		var s string
		if json.Unmarshal(raw, &s) != nil {
			return -1
		}
		value = s
	}
	m := decimalRE.FindStringSubmatch(value)
	if m == nil {
		return -1
	}
	whole, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil || whole > math.MaxInt64/100 {
		return -1
	}
	frac := m[2]
	if len(frac) == 1 {
		frac += "0"
	}
	cents := int64(0)
	if frac != "" {
		cents, _ = strconv.ParseInt(frac, 10, 64)
	}
	if whole*100 > math.MaxInt64-cents {
		return -1
	}
	return whole*100 + cents
}

func optionalMoneyCentavos(raw json.RawMessage) int64 {
	if len(raw) == 0 {
		return 0
	}
	return moneyCentavos(raw)
}

func breakdownBase(raw json.RawMessage) int64 {
	obj := rawObject(raw)
	if obj == nil {
		return -1
	}
	return moneyCentavos(obj["base"])
}

func rawObject(raw json.RawMessage) map[string]json.RawMessage {
	var out map[string]json.RawMessage
	if len(raw) == 0 || json.Unmarshal(raw, &out) != nil {
		return nil
	}
	return out
}

func rawString(raw json.RawMessage) string {
	var out string
	if len(raw) == 0 || json.Unmarshal(raw, &out) != nil {
		return ""
	}
	return out
}

func validConcept(s string) bool {
	if s == "" || strings.TrimSpace(s) != s || len(s) > maxConceptBytes || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

func validateOrigin(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" && u.Path != "/" || u.Port() != "" {
		return "", errors.New("invalid operator origin")
	}
	return "https://" + strings.ToLower(u.Host), nil
}

func validHostedPaymentURL(raw, id string) bool {
	if len(raw) == 0 || len(raw) > maxPaymentURLBytes {
		return false
	}
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.Host == "app.mipey.mx" && u.User == nil && u.Port() == "" && u.Path == "/payment-link/"+id && u.RawQuery == "" && u.Fragment == ""
}

func validQRPayload(qr, resourceID string) bool {
	if qr == "" || len(qr) > maxQRBytes || !utf8.ValidString(qr) {
		return false
	}
	var payload struct {
		Type int    `json:"TYP"`
		ID   string `json:"IDC"`
	}
	return json.Unmarshal([]byte(qr), &payload) == nil && payload.Type == 1 && payload.ID == resourceID
}

func formatMXN(cents int64) string { return fmt.Sprintf("%d.%02d", cents/100, cents%100) }

func normalizeLinkState(state string) string {
	switch state {
	case "created", "pending", "processing", "unpaid":
		return "PENDING"
	case "expired", "cancelled", "canceled":
		return "NOT_PAID"
	default:
		return "NEEDS_REVIEW"
	}
}

func normalizedNonPaid(state string) string {
	switch state {
	case "PENDING", "PROCESSING", "CREATED":
		return "PENDING"
	case "FAILED", "EXPIRED", "CANCELLED", "CANCELED":
		return "NOT_PAID"
	default:
		return "NEEDS_REVIEW"
	}
}

func demoCharge(key, method string, amount int64, concept string) Charge {
	h := digest(key)
	id := "DEMO-" + h[:20]
	out := Charge{ChargeID: id, Method: method, Status: "DEMO_ONLY", Mode: "DEMO", AmountCentavos: amount, Currency: "MXN", Concept: concept, Message: "Simulación local no pagable. No se contactó a api.pey.mx y no representa un cobro."}
	if method == "qr" {
		out.QRPayload = "PAYVRO-DEMO-NOT-PAYABLE-" + h[:24]
	} else {
		out.PaymentURL = "https://example.invalid/payvro-demo/" + h[:24]
	}
	return out
}

func uncertainCharge(event journalEvent) Charge {
	mode := event.Mode
	if mode == "" {
		mode = "LIVE"
	}
	message := "Resultado del POST incierto; no reintentar. Sin ID seguro no hay consulta automática; revisión manual con pey."
	if event.Charge.ChargeID != "" && idRE.MatchString(event.Charge.ChargeID) {
		message = "Resultado del POST incierto; se guardó un ID válido. Consulta get_charge_status por GET autenticado; nunca reintentes el POST."
	}
	return Charge{ChargeID: event.Charge.ChargeID, Method: event.Method, Status: "UNCERTAIN", Mode: mode, AmountCentavos: event.AmountCentavos, Currency: "MXN", ManualReview: true, Message: message}
}

func newReference(method string) (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	if method == "qr" {
		n := (uint32(raw[0])<<16 | uint32(raw[1])<<8 | uint32(raw[2])) % 10_000_000
		return fmt.Sprintf("%07d", n), nil
	}
	return "payvro-" + base64.RawURLEncoding.EncodeToString(raw[:]), nil
}

func digest(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }

func containsActiveCredential(value, extra string) bool {
	if value == "" {
		return false
	}
	secrets := redactor.ActiveEnvSecrets()
	if extra != "" {
		secrets = append(secrets, extra)
	}
	return redactor.RedactString(value, secrets...) != value
}

func containsSensitiveConceptMaterial(value, extra string) bool {
	return containsActiveCredential(value, extra) || bearerCredentialRE.MatchString(value) ||
		jwtCredentialRE.MatchString(value) || keyCredentialRE.MatchString(value) || financialDigitsRE.MatchString(value)
}

func configError(s string) error { return fmt.Errorf("%w: %s", ErrInvalidConfig, s) }

func noReplayTransport(rt http.RoundTripper) http.RoundTripper {
	if t, ok := rt.(*http.Transport); ok {
		clone := t.Clone()
		clone.DisableKeepAlives = true
		clone.ForceAttemptHTTP2 = false
		var p http.Protocols
		p.SetHTTP1(true)
		clone.Protocols = &p
		return clone
	}
	if rt != nil {
		return rt
	}
	t := &http.Transport{Proxy: http.ProxyFromEnvironment, TLSHandshakeTimeout: 10 * time.Second, ExpectContinueTimeout: time.Second, DisableKeepAlives: true}
	var p http.Protocols
	p.SetHTTP1(true)
	t.Protocols = &p
	return t
}

// ValidateAmountCap is exposed for tests and operator-side diagnostics; production
// callers use FromEnv, which never reads any .env file.
func ValidateAmountCap(raw string) (int64, error) {
	if !amountEnvRE.MatchString(raw) {
		return 0, ErrInvalidConfig
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || v <= 0 || v > 9007199254740991 {
		return 0, ErrInvalidConfig
	}
	return v, nil
}

// MaxResponseBytes is available for a boundedness assertion in tests.
func MaxResponseBytes() int { return maxResponseBytes }

// ScanJournal is intentionally not part of MCP; it is used by tests to assert
// that no provider credential was persisted.
func ScanJournal(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	s.Buffer(make([]byte, 1024), 20*1024)
	var lines []string
	for s.Scan() {
		lines = append(lines, s.Text())
	}
	return lines, s.Err()
}
