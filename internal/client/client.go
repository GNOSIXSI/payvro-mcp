package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

const (
	CodeAuth                = "auth_error"
	CodeNotFound            = "not_found"
	CodeIdempotencyConflict = "idempotency_conflict"
	CodeInvalidInput        = "invalid_input"
	CodeUpstream            = "upstream_error"
	CodeRedirect            = "redirect_error"
	CodeInvalidConfig       = "invalid_config"
	defaultTimeout          = 15 * time.Second
	maxGETRetries           = 2
	maxResponseBytes        = 16 * 1024
)

// ErrInvalidConfig marks a missing or rejected local configuration.
var ErrInvalidConfig = errors.New("invalid payvro configuration")

// APIError is a merchant API failure safe to show to a tool caller.
type APIError struct {
	Code    string
	Status  int
	Detail  string
	wrapped error
}

func (e *APIError) Error() string {
	msg := e.Code
	if e.Status != 0 {
		msg = fmt.Sprintf("%s: merchant API returned %d", e.Code, e.Status)
	}
	if e.Detail != "" {
		msg += ": " + e.Detail
	}
	return msg
}

func (e *APIError) Unwrap() error { return e.wrapped }

// Config is the fixed merchant API target. Token and base URL come from the environment.
type Config struct {
	BaseURL   string
	Token     string
	Mode      string
	Transport http.RoundTripper
}

// Offer is the public offer created for an approved product.
type Offer struct {
	OfferID  string `json:"offerId"`
	OfferURL string `json:"offerUrl"`
	Mode     string `json:"mode"`
}

// Checkout is the offer entry URL. It does not create a buyer order.
type Checkout struct {
	CheckoutURL string `json:"checkoutUrl"`
	Mode        string `json:"mode"`
}

// OrderStatus is the merchant's redacted view of one order.
type OrderStatus struct {
	OrderID        string `json:"orderId"`
	Status         string `json:"status"`
	Mode           string `json:"mode"`
	AmountCentavos int64  `json:"amountCentavos"`
	ReasonCategory string `json:"reasonCategory"`
}

// Client calls the three merchant routes used by the MCP tools.
type Client struct {
	base  *url.URL
	token string
	http  *http.Client
}

// New validates the base URL and token and builds a client that does not follow redirects.
func New(cfg Config) (*Client, error) {
	base, err := parseBaseURL(cfg.BaseURL, cfg.Mode)
	if err != nil {
		return nil, err
	}
	if err := validateToken(cfg.Token); err != nil {
		return nil, err
	}
	return &Client{
		base:  base,
		token: cfg.Token,
		http: &http.Client{
			Timeout:   defaultTimeout,
			Transport: transportWithoutReplay(cfg.Transport),
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}, nil
}

// FromEnv reads PAYVRO_API_BASE_URL, PAYVRO_MERCHANT_TOKEN and PAYVRO_MODE.
func FromEnv() (*Client, error) {
	return New(Config{
		BaseURL: os.Getenv("PAYVRO_API_BASE_URL"),
		Token:   os.Getenv("PAYVRO_MERCHANT_TOKEN"),
		Mode:    os.Getenv("PAYVRO_MODE"),
	})
}

// HTTPTimeout is the bound on each merchant request.
func (c *Client) HTTPTimeout() time.Duration { return c.http.Timeout }

// CreateOffer publishes an offer for an already approved product.
func (c *Client) CreateOffer(ctx context.Context, approvedProductID, approvalID, idempotencyKey string) (Offer, error) {
	if err := validateID(approvedProductID); err != nil {
		return Offer{}, err
	}
	if err := validateID(approvalID); err != nil {
		return Offer{}, err
	}
	if err := validateKey(idempotencyKey); err != nil {
		return Offer{}, err
	}
	payload, err := json.Marshal(struct {
		ApprovedProductID string `json:"approvedProductId"`
		ApprovalID        string `json:"approvalId"`
	}{approvedProductID, approvalID})
	if err != nil {
		return Offer{}, err
	}
	body, err := c.do(ctx, http.MethodPost, []string{"api", "v1", "merchant", "offers"}, idempotencyKey, payload)
	if err != nil {
		return Offer{}, err
	}
	var out Offer
	if err := decodeExact(body, []string{"offerId", "offerUrl", "mode"}, &out); err != nil {
		return Offer{}, err
	}
	if out.OfferID == "" || out.OfferURL == "" || out.Mode == "" {
		return Offer{}, &APIError{Code: CodeUpstream, Detail: "incomplete offer"}
	}
	return out, nil
}

// CreateCheckout returns the public entry URL for an offer.
func (c *Client) CreateCheckout(ctx context.Context, offerID, idempotencyKey string) (Checkout, error) {
	if err := validateID(offerID); err != nil {
		return Checkout{}, err
	}
	if err := validateKey(idempotencyKey); err != nil {
		return Checkout{}, err
	}
	payload, err := json.Marshal(struct {
		OfferID string `json:"offerId"`
	}{offerID})
	if err != nil {
		return Checkout{}, err
	}
	body, err := c.do(ctx, http.MethodPost, []string{"api", "v1", "merchant", "checkouts"}, idempotencyKey, payload)
	if err != nil {
		return Checkout{}, err
	}
	var out Checkout
	if err := decodeExact(body, []string{"checkoutUrl", "mode"}, &out); err != nil {
		return Checkout{}, err
	}
	if out.CheckoutURL == "" || out.Mode == "" {
		return Checkout{}, &APIError{Code: CodeUpstream, Detail: "incomplete checkout"}
	}
	return out, nil
}

// GetOrderStatus reads one redacted order. Network errors and 5xx are retried at most twice.
func (c *Client) GetOrderStatus(ctx context.Context, orderID string) (OrderStatus, error) {
	if err := validateID(orderID); err != nil {
		return OrderStatus{}, err
	}
	body, err := c.do(ctx, http.MethodGet, []string{"api", "v1", "merchant", "orders", orderID}, "", nil)
	if err != nil {
		return OrderStatus{}, err
	}
	var out OrderStatus
	keys := []string{"orderId", "status", "mode", "amountCentavos", "reasonCategory"}
	if err := decodeExact(body, keys, &out); err != nil {
		return OrderStatus{}, err
	}
	if out.OrderID == "" || out.Status == "" || out.Mode == "" || out.AmountCentavos < 0 {
		return OrderStatus{}, &APIError{Code: CodeUpstream, Detail: "incomplete order status"}
	}
	return out, nil
}

func (c *Client) do(ctx context.Context, method string, segments []string, idempotencyKey string, payload []byte) ([]byte, error) {
	attempts := 1
	if method == http.MethodGet {
		attempts = maxGETRetries + 1
	}
	var last error
	for attempt := 0; attempt < attempts; attempt++ {
		body, status, err := c.roundTrip(ctx, method, segments, idempotencyKey, payload)
		if err != nil {
			last = err
			if method != http.MethodGet || ctx.Err() != nil {
				return nil, err
			}
			continue
		}
		if err := classify(status, c.detail(body)); err != nil {
			last = err
			if method == http.MethodGet && status >= 500 && attempt+1 < attempts {
				continue
			}
			return nil, err
		}
		return body, nil
	}
	return nil, last
}

func (c *Client) roundTrip(ctx context.Context, method string, segments []string, idempotencyKey string, payload []byte) ([]byte, int, error) {
	endpoint := c.merchantURL(segments)
	var reader io.Reader
	if len(payload) > 0 {
		reader = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return nil, 0, &APIError{Code: CodeInvalidInput, Detail: "could not build request"}
	}
	// bytes.NewReader installs GetBody. Together with Idempotency-Key that makes
	// http.Transport replay a POST when a reused keep-alive connection dies.
	// Clearing GetBody keeps the POST non-replayable. Close stops the conn
	// from returning to the idle pool, so a later GET is not replayed either.
	req.GetBody = nil
	req.Close = true
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, errors.New("merchant API request failed: " + c.redact(err.Error()))
	}
	defer resp.Body.Close()
	buf, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, resp.StatusCode, errors.New("merchant API request failed: " + c.redact(err.Error()))
	}
	return buf, resp.StatusCode, nil
}

func classify(status int, detail string) error {
	switch {
	case status >= 200 && status < 300:
		return nil
	case status >= 300 && status < 400:
		return &APIError{Code: CodeRedirect, Status: status, Detail: "redirects are not followed"}
	}
	code := CodeUpstream
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		code = CodeAuth
	case http.StatusNotFound:
		code = CodeNotFound
	case http.StatusConflict:
		code = CodeIdempotencyConflict
	case http.StatusUnprocessableEntity:
		code = CodeInvalidInput
	}
	return &APIError{Code: code, Status: status, Detail: detail}
}

func (c *Client) detail(body []byte) string {
	var payload struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return ""
	}
	msg := c.redact(payload.Error.Message)
	msg = strings.ReplaceAll(msg, "Authorization", "")
	msg = strings.ReplaceAll(msg, "Bearer", "")
	msg = strings.TrimSpace(msg)
	if len(msg) > 200 {
		msg = msg[:200]
	}
	return msg
}

func (c *Client) redact(text string) string {
	if c.token == "" || text == "" {
		return text
	}
	return strings.ReplaceAll(text, c.token, "[redacted]")
}

func decodeExact(body []byte, keys []string, dst any) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil || len(raw) != len(keys) {
		return &APIError{Code: CodeUpstream, Detail: "unexpected response shape"}
	}
	for _, key := range keys {
		if _, ok := raw[key]; !ok {
			return &APIError{Code: CodeUpstream, Detail: "unexpected response shape"}
		}
	}
	if err := json.Unmarshal(body, dst); err != nil {
		return &APIError{Code: CodeUpstream, Detail: "unexpected response shape"}
	}
	return nil
}

func parseBaseURL(raw, mode string) (*url.URL, error) {
	const why = "PAYVRO_API_BASE_URL must use https; http://localhost and http://127.0.0.1 are allowed only when PAYVRO_MODE=DEMO"
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, configErr(why)
	}
	switch u.Scheme {
	case "https":
		return u, nil
	case "http":
		host := u.Hostname()
		if mode == "DEMO" && (host == "localhost" || host == "127.0.0.1") {
			return u, nil
		}
		return nil, configErr(why)
	default:
		return nil, configErr(why)
	}
}

func validateToken(token string) error {
	if token == "" || strings.TrimSpace(token) != token || strings.ContainsAny(token, " \r\n\t") {
		return configErr("PAYVRO_MERCHANT_TOKEN is not set")
	}
	return nil
}

var (
	idempotencyKeyRE = regexp.MustCompile(`^[A-Za-z0-9_-]{16,128}$`)
	opaqueIDRE       = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
)

func validateKey(key string) error {
	if !idempotencyKeyRE.MatchString(key) {
		return &APIError{Code: CodeInvalidInput, Detail: "idempotencyKey must be 16-128 characters matching [A-Za-z0-9_-]"}
	}
	return nil
}

func validateID(id string) error {
	if !opaqueIDRE.MatchString(id) {
		return &APIError{Code: CodeInvalidInput, Detail: "id must be one opaque segment matching [A-Za-z0-9_-]{1,128}"}
	}
	return nil
}

// merchantURL appends each segment with PathEscape so the id stays one
// segment. url.JoinPath would clean "." and ".." out of the orders route.
func (c *Client) merchantURL(segments []string) string {
	u := *c.base
	var path strings.Builder
	path.WriteString(strings.TrimRight(u.EscapedPath(), "/"))
	for _, seg := range segments {
		path.WriteByte('/')
		path.WriteString(url.PathEscape(seg))
	}
	if path.Len() == 0 {
		path.WriteByte('/')
	}
	u.RawQuery = ""
	u.Fragment = ""
	u.Path = path.String()
	u.RawPath = ""
	return u.String()
}

func configErr(detail string) error {
	return &APIError{Code: CodeInvalidConfig, Detail: detail, wrapped: ErrInvalidConfig}
}

// transportWithoutReplay returns a transport that does not replay requests.
// http.Transport retries a request it considers idempotent (every GET, and a
// POST that has GetBody plus Idempotency-Key) when a reused connection dies.
// That retry is invisible to the adapter's own attempt counter.
func transportWithoutReplay(rt http.RoundTripper) http.RoundTripper {
	if rt == nil {
		return newHTTP1Transport()
	}
	if t, ok := rt.(*http.Transport); ok {
		clone := t.Clone()
		disableReplay(clone)
		return clone
	}
	return rt
}

func newHTTP1Transport() *http.Transport {
	t := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
	disableReplay(t)
	return t
}

func disableReplay(t *http.Transport) {
	t.DisableKeepAlives = true
	t.ForceAttemptHTTP2 = false
	var proto http.Protocols
	proto.SetHTTP1(true)
	t.Protocols = &proto
}
