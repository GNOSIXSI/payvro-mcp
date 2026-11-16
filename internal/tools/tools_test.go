package tools_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/GNOSIXSI/payvro-mcp/internal/tools"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	testToken = "pat-test-DO-NOT-LEAK"
	goodKey   = "idem-fixture-aaaa-bbbb"
)

func TestToolHappyPathsSendContractRequests(t *testing.T) {
	var mu sync.Mutex
	var hits []recorded
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		hits = append(hits, recorded{r.Method, r.URL.Path, r.Header.Clone(), append([]byte(nil), body...)})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/merchant/offers":
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"offerId":"off_1","offerUrl":"https://payvro.example/o/off_1","mode":"DEMO"}`)
		case "/api/v1/merchant/checkouts":
			_, _ = io.WriteString(w, `{"checkoutUrl":"https://payvro.example/o/off_1","mode":"DEMO"}`)
		case "/api/v1/merchant/orders/ord_1":
			_, _ = io.WriteString(w, `{"orderId":"ord_1","status":"PENDING","mode":"DEMO","amountCentavos":14900,"reasonCategory":"NONE"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	t.Setenv("PAYVRO_API_BASE_URL", srv.URL)
	t.Setenv("PAYVRO_MERCHANT_TOKEN", testToken)
	t.Setenv("PAYVRO_MODE", "DEMO")
	var diag bytes.Buffer
	session := connect(t, &diag)

	offer := call(t, session, "create_offer", map[string]any{
		"approvedProductId": "prod_1",
		"approvalId":        "appr_1",
		"idempotencyKey":    goodKey,
	})
	if offer["offerId"] != "off_1" || offer["offerUrl"] != "https://payvro.example/o/off_1" || offer["mode"] != "DEMO" {
		t.Fatalf("offer = %#v", offer)
	}
	checkout := call(t, session, "create_checkout", map[string]any{
		"offerId":        "off_1",
		"idempotencyKey": goodKey,
	})
	if checkout["checkoutUrl"] != "https://payvro.example/o/off_1" || checkout["mode"] != "DEMO" {
		t.Fatalf("checkout = %#v", checkout)
	}
	status := call(t, session, "get_order_status", map[string]any{"orderId": "ord_1"})
	if status["status"] != "PENDING" || status["amountCentavos"] != float64(14900) {
		t.Fatalf("status = %#v", status)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(hits) != 3 {
		t.Fatalf("hits = %d", len(hits))
	}
	if hits[0].method != http.MethodPost || hits[0].path != "/api/v1/merchant/offers" || hits[0].header.Get("Idempotency-Key") != goodKey {
		t.Fatalf("offer request = %+v", hits[0])
	}
	if hits[0].header.Get("Authorization") != "Bearer "+testToken {
		t.Fatalf("authorization = %q", hits[0].header.Get("Authorization"))
	}
	var offerBody map[string]any
	if err := json.Unmarshal(hits[0].body, &offerBody); err != nil {
		t.Fatal(err)
	}
	if len(offerBody) != 2 || offerBody["approvedProductId"] != "prod_1" || offerBody["approvalId"] != "appr_1" {
		t.Fatalf("offer body = %#v", offerBody)
	}
	if hits[1].method != http.MethodPost || hits[1].path != "/api/v1/merchant/checkouts" {
		t.Fatalf("checkout request = %+v", hits[1])
	}
	if hits[2].method != http.MethodGet || hits[2].path != "/api/v1/merchant/orders/ord_1" || hits[2].header.Get("Idempotency-Key") != "" {
		t.Fatalf("status request = %+v", hits[2])
	}
	assertNoToken(t, diag.String())
}

func TestDirectSDKRejectsSensitiveConceptsUnknownFieldsAndUnknownTools(t *testing.T) {
	const activeSecret = "synthetic-active-bff-secret-never-store"
	journal := filepath.Join(t.TempDir(), "absent", "charges.jsonl")
	t.Setenv("PAYVRO_MCP_MODE", "DIRECT")
	t.Setenv("PAYVRO_API_BASE_URL", "")
	t.Setenv("PAYVRO_MERCHANT_TOKEN", "")
	t.Setenv("PAYVRO_MODE", "DEMO")
	t.Setenv("PAYVRO_MAX_AMOUNT_CENTAVOS", "14900")
	t.Setenv("PEY_API_KEY", "synthetic-payvro-key-for-test-only")
	t.Setenv("PEY_API_KEY_ID", "synthetic-account")
	t.Setenv("PEY_BANK_ACCOUNT_ID", "")
	t.Setenv("PAYVRO_PUBLIC_ORIGIN", "")
	t.Setenv("PAYVRO_DIRECT_JOURNAL", journal)
	t.Setenv("PAYVRO_BFF_SHARED_SECRET", activeSecret)
	var diag bytes.Buffer
	session := connect(t, &diag)
	listed, err := session.ListTools(context.Background(), nil)
	if err != nil || len(listed.Tools) != 2 {
		t.Fatalf("DIRECT tools=%v err=%v", listed, err)
	}
	registered := map[string]bool{}
	for _, tool := range listed.Tools {
		registered[tool.Name] = true
	}
	if !registered["create_codi_charge"] || !registered["get_charge_status"] {
		t.Fatalf("unexpected DIRECT tools: %v", registered)
	}

	concepts := []string{
		"Bearer payvro-token-0123456789",
		"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJzeW50aGV0aWMifQ.signature1234567890",
		"sk" + "_live_" + "1234567890abcdefghijklmnop",
		"CLABE 012 345 678 901 234 567",
		"tarjeta 4111-1111-1111-1111",
		"reference " + activeSecret,
	}
	for i, concept := range concepts {
		_, text, isErr := callRaw(t, session, "create_codi_charge", map[string]any{
			"amountCentavos": 14900, "concept": concept, "method": "qr", "idempotencyKey": fmt.Sprintf("direct-sensitive-%04d", i),
		})
		if !isErr {
			t.Fatalf("sensitive concept %q accepted: %s", concept, text)
		}
		if strings.Contains(text, concept) || strings.Contains(diag.String(), concept) || strings.Contains(text+diag.String(), activeSecret) {
			t.Fatalf("sensitive concept leaked through MCP output/stderr: result=%q stderr=%q", text, diag.String())
		}
	}

	for _, amount := range []any{float64(14900.5), float64(9007199254740992), float64(14901)} {
		res, callErr := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "create_codi_charge", Arguments: map[string]any{
			"amountCentavos": amount, "concept": "Curso español", "method": "qr", "idempotencyKey": "direct-invalid-amount-aaaa",
		}})
		if callErr == nil && !res.IsError {
			t.Fatalf("invalid amount %v was accepted: %+v", amount, res)
		}
	}

	for _, field := range []string{"amount", "currency", "bankAccountId", "apiKey", "paid", "enableLIVE", "config"} {
		args := map[string]any{"amountCentavos": 14900, "concept": "Curso español, edición 3", "method": "qr", "idempotencyKey": "direct-extra-field-aaaa"}
		args[field] = "attacker-controlled"
		if _, text, isErr := callRaw(t, session, "create_codi_charge", args); !isErr {
			t.Fatalf("DIRECT call accepted added field %q: %s", field, text)
		}
	}

	t.Setenv("PAYVRO_MAX_AMOUNT_CENTAVOS", "")
	res, callErr := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "create_codi_charge", Arguments: map[string]any{
		"amountCentavos": 14900, "concept": "Curso español", "method": "qr", "idempotencyKey": "direct-missing-cap-aaaa",
	}})
	if callErr == nil && !res.IsError {
		t.Fatalf("DIRECT create succeeded with no configured amount cap: %+v", res)
	}

	for _, name := range []string{"set_paid", "change_amount", "configure_receiver", "enable_live"} {
		_, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: map[string]any{}})
		if err == nil {
			t.Fatalf("nonexistent tool %q was registered", name)
		}
	}
	if _, err := os.Stat(filepath.Dir(journal)); !os.IsNotExist(err) {
		t.Fatalf("rejected SDK inputs created journal directory: %v", err)
	}
	if strings.Contains(diag.String(), activeSecret) {
		t.Fatalf("active secret leaked to stderr: %q", diag.String())
	}
}

func TestInheritedBFFSecretIsRedactedOnStoreToolError(t *testing.T) {
	const secret = `synthetic-bff-shared-secret-\"escaped\\fragment`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"message": "upstream echoed " + secret}})
	}))
	defer srv.Close()
	t.Setenv("PAYVRO_MCP_MODE", "STORE")
	t.Setenv("PAYVRO_API_BASE_URL", srv.URL)
	t.Setenv("PAYVRO_MERCHANT_TOKEN", testToken)
	t.Setenv("PAYVRO_MODE", "DEMO")
	t.Setenv("PAYVRO_BFF_SHARED_SECRET", secret)
	t.Setenv("PEY_API_KEY", "")
	var diag bytes.Buffer
	session := connect(t, &diag)
	_, text, isErr := callRaw(t, session, "get_order_status", map[string]any{"orderId": "order-1"})
	if !isErr {
		t.Fatalf("expected STORE backend error, got %q", text)
	}
	if strings.Contains(text, "synthetic-bff") || strings.Contains(diag.String(), "synthetic-bff") {
		t.Fatalf("inherited BFF secret leaked: tool=%q stderr=%q", text, diag.String())
	}
	if !strings.Contains(text+diag.String(), "[redacted]") {
		t.Fatalf("expected redaction marker in tool/stderr output: tool=%q stderr=%q", text, diag.String())
	}
}

func TestUnknownArgumentsRejected(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		http.Error(w, "unexpected", http.StatusInternalServerError)
	}))
	defer srv.Close()
	t.Setenv("PAYVRO_API_BASE_URL", srv.URL)
	t.Setenv("PAYVRO_MERCHANT_TOKEN", testToken)
	t.Setenv("PAYVRO_MODE", "DEMO")
	session := connect(t, io.Discard)

	forbidden := []string{"price", "amount", "amountCentavos", "receiver", "currency", "mode", "paymentState", "apiKey", "extra"}
	for _, field := range forbidden {
		args := map[string]any{
			"approvedProductId": "prod_1",
			"approvalId":        "appr_1",
			"idempotencyKey":    goodKey,
			field:               "149",
		}
		if _, errText, isErr := callRaw(t, session, "create_offer", args); !isErr {
			t.Fatalf("field %s was accepted: %s", field, errText)
		}
	}
	if _, _, isErr := callRaw(t, session, "create_checkout", map[string]any{
		"offerId": "off_1", "idempotencyKey": goodKey, "amount": "149",
	}); !isErr {
		t.Fatal("create_checkout accepted amount")
	}
	if _, _, isErr := callRaw(t, session, "get_order_status", map[string]any{
		"orderId": "ord_1", "buyerToken": testToken,
	}); !isErr {
		t.Fatal("get_order_status accepted buyerToken")
	}
	if hits != 0 {
		t.Fatalf("rejected arguments still called HTTP %d times", hits)
	}
}

func TestBadIdempotencyKeyRejectedBySchema(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
	}))
	defer srv.Close()
	t.Setenv("PAYVRO_API_BASE_URL", srv.URL)
	t.Setenv("PAYVRO_MERCHANT_TOKEN", testToken)
	t.Setenv("PAYVRO_MODE", "DEMO")
	session := connect(t, io.Discard)
	for _, key := range []string{"short", strings.Repeat("b", 129), "bad key-12345678", "slash/in-key-123"} {
		_, _, isErr := callRaw(t, session, "create_offer", map[string]any{
			"approvedProductId": "prod_1",
			"approvalId":        "appr_1",
			"idempotencyKey":    key,
		})
		if !isErr {
			t.Fatalf("key %q accepted", key)
		}
	}
	if hits != 0 {
		t.Fatalf("bad keys reached HTTP %d times", hits)
	}
}

func TestToolConfigFailures(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
	}))
	defer srv.Close()

	t.Run("missing token", func(t *testing.T) {
		t.Setenv("PAYVRO_API_BASE_URL", srv.URL)
		t.Setenv("PAYVRO_MERCHANT_TOKEN", "")
		t.Setenv("PAYVRO_MODE", "DEMO")
		var diag bytes.Buffer
		session := connect(t, &diag)
		text, isErr := mustErr(t, session, "get_order_status", map[string]any{"orderId": "ord_1"})
		if !strings.Contains(text, "PAYVRO_MERCHANT_TOKEN") {
			t.Fatalf("error = %s", text)
		}
		if !isErr {
			t.Fatal("expected tool error")
		}
		assertNoToken(t, text)
		assertNoToken(t, diag.String())
	})

	t.Run("non https", func(t *testing.T) {
		t.Setenv("PAYVRO_API_BASE_URL", "http://evil.example")
		t.Setenv("PAYVRO_MERCHANT_TOKEN", testToken)
		t.Setenv("PAYVRO_MODE", "DEMO")
		session := connect(t, io.Discard)
		text, _ := mustErr(t, session, "create_checkout", map[string]any{
			"offerId":        "off_1",
			"idempotencyKey": goodKey,
		})
		if !strings.Contains(text, "https") {
			t.Fatalf("error = %s", text)
		}
		assertNoToken(t, text)
	})

	t.Run("loopback http rejected outside DEMO", func(t *testing.T) {
		t.Setenv("PAYVRO_API_BASE_URL", srv.URL)
		t.Setenv("PAYVRO_MERCHANT_TOKEN", testToken)
		t.Setenv("PAYVRO_MODE", "LIVE")
		session := connect(t, io.Discard)
		text, _ := mustErr(t, session, "get_order_status", map[string]any{"orderId": "ord_1"})
		if !strings.Contains(text, "DEMO") {
			t.Fatalf("error = %s", text)
		}
		assertNoToken(t, text)
	})
	if hits != 0 {
		t.Fatalf("config failures reached HTTP %d times", hits)
	}
}

func TestToolAllowsDEMOLoopbackAndHidesToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"code":"UNAUTHENTICATED","message":"upstream rejected the order `+testToken+`"}}`)
	}))
	defer srv.Close()
	t.Setenv("PAYVRO_API_BASE_URL", srv.URL)
	t.Setenv("PAYVRO_MERCHANT_TOKEN", testToken)
	t.Setenv("PAYVRO_MODE", "DEMO")
	var diag bytes.Buffer
	session := connect(t, &diag)
	text, _ := mustErr(t, session, "get_order_status", map[string]any{"orderId": "ord_1"})
	assertNoToken(t, text)
	assertNoToken(t, diag.String())
	if !strings.Contains(text, "auth_error") {
		t.Fatalf("error = %s", text)
	}
	if !strings.Contains(text, "upstream rejected the order") {
		t.Fatalf("safe detail missing: %s", text)
	}
	if !strings.Contains(diag.String(), "upstream rejected the order") {
		t.Fatalf("stderr dropped the safe detail: %s", diag.String())
	}
}

func TestReusedConnectionDoesNotReplayOrExceedGETBudget(t *testing.T) {
	t.Run("one POST when a reused connection is dropped", func(t *testing.T) {
		posts, gets := startReuseServer(t, func(w http.ResponseWriter, r *http.Request, fresh bool) {
			if !fresh {
				dropConn(t, w)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			if r.Method == http.MethodPost {
				w.WriteHeader(http.StatusCreated)
				_, _ = io.WriteString(w, `{"offerId":"off_1","offerUrl":"https://payvro.example/o/off_1","mode":"DEMO"}`)
				return
			}
			_, _ = io.WriteString(w, `{"orderId":"ord_warm","status":"PENDING","mode":"DEMO","amountCentavos":14900,"reasonCategory":"NONE"}`)
		})
		session := connect(t, io.Discard)
		if _, _, isErr := callRaw(t, session, "get_order_status", map[string]any{"orderId": "ord_warm"}); isErr {
			t.Fatal("warmup GET failed")
		}
		_, _, _ = callRaw(t, session, "create_offer", map[string]any{
			"approvedProductId": "prod_1",
			"approvalId":        "appr_1",
			"idempotencyKey":    goodKey,
		})
		if posts() != 1 {
			t.Fatalf("POSTs = %d, want exactly 1 (transport replayed a reused connection)", posts())
		}
		if gets() != 1 {
			t.Fatalf("warmup GETs = %d, want 1", gets())
		}
	})

	t.Run("dropped connections and 503 stay within three GETs", func(t *testing.T) {
		_, getN := startReuseServer(t, func(w http.ResponseWriter, r *http.Request, fresh bool) {
			if r.URL.Path != "/api/v1/merchant/orders/ord_1" {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"orderId":"ord_warm","status":"PENDING","mode":"DEMO","amountCentavos":14900,"reasonCategory":"NONE"}`)
				return
			}
			if !fresh {
				dropConn(t, w)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"error":{"message":"unavailable"}}`)
		})
		session := connect(t, io.Discard)
		if _, _, isErr := callRaw(t, session, "get_order_status", map[string]any{"orderId": "ord_warm"}); isErr {
			t.Fatal("warmup GET failed")
		}
		before := getN()
		_, _, _ = callRaw(t, session, "get_order_status", map[string]any{"orderId": "ord_1"})
		if got := getN() - before; got > 3 {
			t.Fatalf("GETs = %d, want at most 3 including transport replays", got)
		}
	})
}

func startReuseServer(t *testing.T, h func(w http.ResponseWriter, r *http.Request, fresh bool)) (posts func() int, gets func() int) {
	t.Helper()
	var mu sync.Mutex
	seen := map[string]int{}
	var postN, getN int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		mu.Lock()
		seen[r.RemoteAddr]++
		fresh := seen[r.RemoteAddr] == 1
		if r.Method == http.MethodPost {
			postN++
		}
		if r.Method == http.MethodGet {
			getN++
		}
		mu.Unlock()
		h(w, r, fresh)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("PAYVRO_API_BASE_URL", srv.URL)
	t.Setenv("PAYVRO_MERCHANT_TOKEN", testToken)
	t.Setenv("PAYVRO_MODE", "DEMO")
	return func() int {
			mu.Lock()
			defer mu.Unlock()
			return postN
		}, func() int {
			mu.Lock()
			defer mu.Unlock()
			return getN
		}
}

func dropConn(t *testing.T, w http.ResponseWriter) {
	t.Helper()
	hj, ok := w.(http.Hijacker)
	if !ok {
		t.Fatal("response writer cannot hijack")
	}
	conn, _, err := hj.Hijack()
	if err != nil {
		t.Fatal(err)
	}
	conn.Close()
}

func TestListedToolsAreExactlyTheContract(t *testing.T) {
	t.Setenv("PAYVRO_API_BASE_URL", "https://merchant.example")
	t.Setenv("PAYVRO_MERCHANT_TOKEN", testToken)
	session := connect(t, io.Discard)
	result, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Tools) != 3 {
		t.Fatalf("tools = %d", len(result.Tools))
	}
	byName := map[string]*mcp.Tool{}
	for _, tool := range result.Tools {
		byName[tool.Name] = tool
	}
	offer := byName["create_offer"]
	checkout := byName["create_checkout"]
	status := byName["get_order_status"]
	if offer == nil || checkout == nil || status == nil {
		t.Fatalf("names = %#v", byName)
	}
	assertObjectSchema(t, offer.InputSchema, []string{"approvedProductId", "approvalId", "idempotencyKey"})
	assertObjectSchema(t, checkout.InputSchema, []string{"offerId", "idempotencyKey"})
	assertObjectSchema(t, status.InputSchema, []string{"orderId"})
	assertIdempotencySchema(t, offer.InputSchema)
	assertIdempotencySchema(t, checkout.InputSchema)

	if offer.Annotations == nil || offer.Annotations.ReadOnlyHint || !offer.Annotations.IdempotentHint {
		t.Fatalf("create_offer annotations = %+v", offer.Annotations)
	}
	if offer.Annotations.DestructiveHint == nil || *offer.Annotations.DestructiveHint {
		t.Fatal("create_offer must be non-destructive")
	}
	if offer.Annotations.OpenWorldHint == nil || !*offer.Annotations.OpenWorldHint {
		t.Fatal("create_offer must be open-world")
	}
	if checkout.Annotations == nil || !checkout.Annotations.IdempotentHint || checkout.Annotations.ReadOnlyHint {
		t.Fatalf("create_checkout annotations = %+v", checkout.Annotations)
	}
	if checkout.Annotations.DestructiveHint == nil || *checkout.Annotations.DestructiveHint {
		t.Fatal("create_checkout must be non-destructive")
	}
	if status.Annotations == nil || !status.Annotations.ReadOnlyHint {
		t.Fatalf("get_order_status annotations = %+v", status.Annotations)
	}
}

func TestOrderIDStaysOneOpaqueSegment(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.Method+" "+r.URL.Path)
		mu.Unlock()
		if r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v1/merchant/orders/") {
			id := strings.TrimPrefix(r.URL.Path, "/api/v1/merchant/orders/")
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"orderId":"`+id+`","status":"PENDING","mode":"DEMO","amountCentavos":14900,"reasonCategory":"NONE"}`)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()
	t.Setenv("PAYVRO_API_BASE_URL", srv.URL)
	t.Setenv("PAYVRO_MERCHANT_TOKEN", testToken)
	t.Setenv("PAYVRO_MODE", "DEMO")
	session := connect(t, io.Discard)

	rejected := []string{".", "..", "", "a/b", `a\b`, "%2e%2e", "a%2Fb", "a\nb", "a b", strings.Repeat("a", 129)}
	for _, id := range rejected {
		_, _, isErr := callRaw(t, session, "get_order_status", map[string]any{"orderId": id})
		if !isErr {
			t.Errorf("order id %q was accepted", id)
		}
	}
	mu.Lock()
	got := append([]string(nil), paths...)
	paths = nil
	mu.Unlock()
	if len(got) != 0 {
		t.Fatalf("rejected ids made %d requests: %v", len(got), got)
	}

	accepted := []string{"Ab9_-order", strings.Repeat("b", 128)}
	for _, id := range accepted {
		out := call(t, session, "get_order_status", map[string]any{"orderId": id})
		if out["orderId"] != id || out["status"] != "PENDING" {
			t.Fatalf("status for %s = %#v", id, out)
		}
	}
	mu.Lock()
	got = append([]string(nil), paths...)
	mu.Unlock()
	if len(got) != len(accepted) {
		t.Fatalf("requests = %v", got)
	}
	for i, id := range accepted {
		want := "GET /api/v1/merchant/orders/" + id
		if got[i] != want {
			t.Fatalf("request = %s, want %s", got[i], want)
		}
	}
}

type recorded struct {
	method string
	path   string
	header http.Header
	body   []byte
}

func connect(t *testing.T, diag io.Writer) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	server := mcp.NewServer(&mcp.Implementation{Name: "payvro-test", Version: "0"}, nil)
	tools.Register(server, diag)
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	if _, err := server.Connect(ctx, serverTransport, nil); err != nil {
		t.Fatal(err)
	}
	session, err := mcp.NewClient(&mcp.Implementation{Name: "tester", Version: "0"}, nil).Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func call(t *testing.T, session *mcp.ClientSession, name string, args map[string]any) map[string]any {
	t.Helper()
	raw, text, isErr := callRaw(t, session, name, args)
	if isErr {
		t.Fatalf("%s error: %s", name, text)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("structured %s: %v (%s)", name, err, text)
	}
	blob := string(raw) + text
	assertNoToken(t, blob)
	return out
}

func callRaw(t *testing.T, session *mcp.ClientSession, name string, args map[string]any) (json.RawMessage, string, bool) {
	t.Helper()
	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("call %s: %v", name, err)
	}
	text := resultText(res)
	assertNoToken(t, text)
	var raw json.RawMessage
	if res.StructuredContent != nil {
		raw, err = json.Marshal(res.StructuredContent)
		if err != nil {
			t.Fatal(err)
		}
	}
	return raw, text, res.IsError
}

func mustErr(t *testing.T, session *mcp.ClientSession, name string, args map[string]any) (string, bool) {
	t.Helper()
	_, text, isErr := callRaw(t, session, name, args)
	if !isErr {
		t.Fatalf("%s succeeded: %s", name, text)
	}
	return text, isErr
}

func resultText(res *mcp.CallToolResult) string {
	var b strings.Builder
	for _, item := range res.Content {
		if text, ok := item.(*mcp.TextContent); ok {
			b.WriteString(text.Text)
		}
	}
	return b.String()
}

func assertNoToken(t *testing.T, text string) {
	t.Helper()
	if strings.Contains(text, testToken) {
		t.Fatalf("token leaked: %s", text)
	}
}

func schemaObject(t *testing.T, schema any) map[string]any {
	t.Helper()
	blob, err := json.Marshal(schema)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(blob, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func assertObjectSchema(t *testing.T, schema any, props []string) {
	t.Helper()
	obj := schemaObject(t, schema)
	if obj["type"] != "object" {
		t.Fatalf("type = %#v", obj["type"])
	}
	if obj["additionalProperties"] != false {
		t.Fatalf("additionalProperties = %#v", obj["additionalProperties"])
	}
	gotProps, _ := obj["properties"].(map[string]any)
	if len(gotProps) != len(props) {
		t.Fatalf("properties = %#v", gotProps)
	}
	for _, name := range props {
		prop, ok := gotProps[name].(map[string]any)
		if !ok || prop["type"] != "string" {
			t.Fatalf("property %s = %#v", name, gotProps[name])
		}
		min, ok := prop["minLength"].(float64)
		if !ok || min < 1 {
			t.Fatalf("property %s minLength = %#v", name, prop["minLength"])
		}
	}
	required, _ := obj["required"].([]any)
	if len(required) != len(props) {
		t.Fatalf("required = %#v", required)
	}
	have := map[string]bool{}
	for _, item := range required {
		name, _ := item.(string)
		have[name] = true
	}
	for _, name := range props {
		if !have[name] {
			t.Fatalf("required missing %s in %#v", name, required)
		}
	}
}

func assertIdempotencySchema(t *testing.T, schema any) {
	t.Helper()
	obj := schemaObject(t, schema)
	props, _ := obj["properties"].(map[string]any)
	prop, _ := props["idempotencyKey"].(map[string]any)
	if prop["pattern"] != "^[A-Za-z0-9_-]+$" || prop["minLength"] != float64(16) || prop["maxLength"] != float64(128) {
		t.Fatalf("idempotencyKey schema = %#v", prop)
	}
}
