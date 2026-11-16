package client_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GNOSIXSI/payvro-mcp/internal/client"
)

const (
	testToken = "pat-test-DO-NOT-LEAK"
	goodKey   = "idem-fixture-aaaa-bbbb"
)

type hit struct {
	Method string
	Path   string
	Header http.Header
	Body   []byte
}

type fakeAPI struct {
	srv  *httptest.Server
	mu   sync.Mutex
	hits []hit
}

func newFake(t *testing.T, h func(w http.ResponseWriter, r *http.Request, n int)) *fakeAPI {
	t.Helper()
	f := &fakeAPI{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
		}
		f.mu.Lock()
		f.hits = append(f.hits, hit{
			Method: r.Method,
			Path:   r.URL.Path,
			Header: r.Header.Clone(),
			Body:   body,
		})
		n := len(f.hits)
		f.mu.Unlock()
		h(w, r, n)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAPI) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.hits)
}

func (f *fakeAPI) all() []hit {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]hit, len(f.hits))
	copy(out, f.hits)
	return out
}

func demoClient(t *testing.T, base string) *client.Client {
	t.Helper()
	c, err := client.New(client.Config{BaseURL: base, Token: testToken, Mode: "DEMO"})
	if err != nil {
		t.Fatalf("New DEMO client: %v", err)
	}
	if c.HTTPTimeout() != 15*time.Second {
		t.Fatalf("timeout = %s, want 15s", c.HTTPTimeout())
	}
	return c
}

func writeJSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

func assertNoToken(t *testing.T, text string) {
	t.Helper()
	if strings.Contains(text, testToken) || strings.Contains(text, "Bearer "+testToken) {
		t.Fatalf("token leaked in output: %s", text)
	}
}

func TestCreateOfferHappyPath(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, n int) {
		writeJSON(w, http.StatusCreated, `{"offerId":"off_1","offerUrl":"https://payvro.example/o/off_1","mode":"DEMO"}`)
	})
	c := demoClient(t, f.srv.URL)
	got, err := c.CreateOffer(context.Background(), "prod_1", "appr_1", goodKey)
	if err != nil {
		t.Fatal(err)
	}
	if got.OfferID != "off_1" || got.OfferURL != "https://payvro.example/o/off_1" || got.Mode != "DEMO" {
		t.Fatalf("offer = %+v", got)
	}
	hits := f.all()
	if len(hits) != 1 {
		t.Fatalf("requests = %d, want 1", len(hits))
	}
	h := hits[0]
	if h.Method != http.MethodPost || h.Path != "/api/v1/merchant/offers" {
		t.Fatalf("request = %s %s", h.Method, h.Path)
	}
	if h.Header.Get("Authorization") != "Bearer "+testToken {
		t.Fatalf("authorization = %q", h.Header.Get("Authorization"))
	}
	if h.Header.Get("Idempotency-Key") != goodKey {
		t.Fatalf("idempotency key = %q", h.Header.Get("Idempotency-Key"))
	}
	if h.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("content-type = %q", h.Header.Get("Content-Type"))
	}
	var body map[string]any
	if err := json.Unmarshal(h.Body, &body); err != nil {
		t.Fatal(err)
	}
	if len(body) != 2 || body["approvedProductId"] != "prod_1" || body["approvalId"] != "appr_1" {
		t.Fatalf("body = %#v", body)
	}
	for _, forbidden := range []string{"price", "amount", "amountCentavos", "receiver", "currency", "mode", "apiKey"} {
		if _, ok := body[forbidden]; ok {
			t.Fatalf("body contains %s", forbidden)
		}
	}
}

func TestCreateCheckoutHappyPath(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, n int) {
		writeJSON(w, http.StatusOK, `{"checkoutUrl":"https://payvro.example/o/off_1","mode":"DEMO"}`)
	})
	c := demoClient(t, f.srv.URL)
	got, err := c.CreateCheckout(context.Background(), "off_1", goodKey)
	if err != nil {
		t.Fatal(err)
	}
	if got.CheckoutURL != "https://payvro.example/o/off_1" || got.Mode != "DEMO" {
		t.Fatalf("checkout = %+v", got)
	}
	h := f.all()[0]
	if h.Method != http.MethodPost || h.Path != "/api/v1/merchant/checkouts" {
		t.Fatalf("request = %s %s", h.Method, h.Path)
	}
	if h.Header.Get("Idempotency-Key") != goodKey || h.Header.Get("Authorization") != "Bearer "+testToken {
		t.Fatalf("headers = %#v", h.Header)
	}
	var body map[string]any
	if err := json.Unmarshal(h.Body, &body); err != nil {
		t.Fatal(err)
	}
	if len(body) != 1 || body["offerId"] != "off_1" {
		t.Fatalf("body = %#v", body)
	}
}

func TestGetOrderStatusHappyPath(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, n int) {
		writeJSON(w, http.StatusOK, `{"orderId":"ord_1","status":"PENDING","mode":"DEMO","amountCentavos":14900,"reasonCategory":"NONE"}`)
	})
	c := demoClient(t, f.srv.URL)
	got, err := c.GetOrderStatus(context.Background(), "ord_1")
	if err != nil {
		t.Fatal(err)
	}
	if got.OrderID != "ord_1" || got.Status != "PENDING" || got.Mode != "DEMO" || got.AmountCentavos != 14900 || got.ReasonCategory != "NONE" {
		t.Fatalf("status = %+v", got)
	}
	h := f.all()[0]
	if h.Method != http.MethodGet || h.Path != "/api/v1/merchant/orders/ord_1" {
		t.Fatalf("request = %s %s", h.Method, h.Path)
	}
	if h.Header.Get("Authorization") != "Bearer "+testToken {
		t.Fatalf("authorization = %q", h.Header.Get("Authorization"))
	}
	if h.Header.Get("Idempotency-Key") != "" {
		t.Fatalf("GET sent Idempotency-Key %q", h.Header.Get("Idempotency-Key"))
	}
	if len(h.Body) != 0 {
		t.Fatalf("GET body = %q", h.Body)
	}
}

func TestHTTPSOutsideDEMO(t *testing.T) {
	f := &fakeAPI{}
	f.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.hits = append(f.hits, hit{Method: r.Method, Path: r.URL.Path, Header: r.Header.Clone(), Body: body})
		f.mu.Unlock()
		writeJSON(w, http.StatusOK, `{"orderId":"ord_1","status":"PENDING","mode":"LIVE","amountCentavos":14900,"reasonCategory":"NONE"}`)
	}))
	t.Cleanup(f.srv.Close)

	c, err := client.New(client.Config{
		BaseURL:   f.srv.URL,
		Token:     testToken,
		Transport: f.srv.Client().Transport,
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.GetOrderStatus(context.Background(), "ord_1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Mode != "LIVE" || f.count() != 1 {
		t.Fatalf("mode=%s hits=%d", got.Mode, f.count())
	}
}

func TestBaseURLPolicy(t *testing.T) {
	_, err := client.New(client.Config{BaseURL: "http://evil.example", Token: testToken})
	if !errors.Is(err, client.ErrInvalidConfig) {
		t.Fatalf("http public host: %v", err)
	}
	_, err = client.New(client.Config{BaseURL: "http://127.0.0.1:9", Token: testToken, Mode: "LIVE"})
	if !errors.Is(err, client.ErrInvalidConfig) {
		t.Fatalf("loopback outside DEMO: %v", err)
	}
	_, err = client.New(client.Config{BaseURL: "http://localhost:9", Token: testToken})
	if !errors.Is(err, client.ErrInvalidConfig) {
		t.Fatalf("localhost outside DEMO: %v", err)
	}
	_, err = client.New(client.Config{BaseURL: "http://localhost.evil", Token: testToken, Mode: "DEMO"})
	if !errors.Is(err, client.ErrInvalidConfig) {
		t.Fatalf("localhost suffix: %v", err)
	}
	_, err = client.New(client.Config{BaseURL: "http://127.0.0.2:9", Token: testToken, Mode: "DEMO"})
	if !errors.Is(err, client.ErrInvalidConfig) {
		t.Fatalf("other loopback: %v", err)
	}
	if _, err = client.New(client.Config{BaseURL: "http://localhost:9", Token: testToken, Mode: "DEMO"}); err != nil {
		t.Fatalf("DEMO localhost rejected: %v", err)
	}
	if _, err = client.New(client.Config{BaseURL: "http://127.0.0.1:9", Token: testToken, Mode: "DEMO"}); err != nil {
		t.Fatalf("DEMO 127.0.0.1 rejected: %v", err)
	}
	if _, err = client.New(client.Config{BaseURL: "https://merchant.example", Token: testToken}); err != nil {
		t.Fatalf("https rejected: %v", err)
	}
	_, err = client.New(client.Config{BaseURL: "https://user:pass@merchant.example", Token: testToken})
	if !errors.Is(err, client.ErrInvalidConfig) {
		t.Fatalf("userinfo: %v", err)
	}
}

func TestMissingToken(t *testing.T) {
	_, err := client.New(client.Config{BaseURL: "https://merchant.example", Token: "  "})
	if !errors.Is(err, client.ErrInvalidConfig) {
		t.Fatalf("blank token: %v", err)
	}
	t.Setenv("PAYVRO_MERCHANT_TOKEN", "")
	t.Setenv("PAYVRO_API_BASE_URL", "")
	t.Setenv("PAYVRO_MODE", "")
	_, err = client.FromEnv()
	if !errors.Is(err, client.ErrInvalidConfig) {
		t.Fatalf("missing env: %v", err)
	}
	assertNoToken(t, err.Error())
}

func TestRejectsBadIdempotencyKeyAndEmptyID(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, n int) {
		t.Errorf("HTTP called for invalid input, hit %d", n)
	})
	c := demoClient(t, f.srv.URL)
	ctx := context.Background()
	badKeys := []string{"short", strings.Repeat("a", 129), "has space-1234567", "slash/key-123456", ""}
	for _, key := range badKeys {
		if _, err := c.CreateOffer(ctx, "prod_1", "appr_1", key); !isCode(err, client.CodeInvalidInput) {
			t.Fatalf("key %q: %v", key, err)
		}
		if _, err := c.CreateCheckout(ctx, "off_1", key); !isCode(err, client.CodeInvalidInput) {
			t.Fatalf("checkout key %q: %v", key, err)
		}
	}
	if _, err := c.CreateOffer(ctx, "", "appr_1", goodKey); !isCode(err, client.CodeInvalidInput) {
		t.Fatal(err)
	}
	if _, err := c.GetOrderStatus(ctx, "../secrets"); !isCode(err, client.CodeInvalidInput) {
		t.Fatal(err)
	}
	if _, err := c.GetOrderStatus(ctx, ""); !isCode(err, client.CodeInvalidInput) {
		t.Fatal(err)
	}
	if f.count() != 0 {
		t.Fatalf("invalid input reached HTTP %d times", f.count())
	}
}

func TestRedirectIsNotFollowed(t *testing.T) {
	var evil int
	var f *fakeAPI
	f = newFake(t, func(w http.ResponseWriter, r *http.Request, n int) {
		if r.URL.Path == "/evil" {
			evil++
			writeJSON(w, http.StatusOK, `{"offerId":"stolen","offerUrl":"https://evil.example","mode":"DEMO"}`)
			return
		}
		http.Redirect(w, r, f.srv.URL+"/evil", http.StatusFound)
	})
	c := demoClient(t, f.srv.URL)
	_, err := c.CreateOffer(context.Background(), "prod_1", "appr_1", goodKey)
	if !isCode(err, client.CodeRedirect) {
		t.Fatalf("redirect error: %v", err)
	}
	assertNoToken(t, err.Error())
	if evil != 0 {
		t.Fatalf("followed redirect, evil hits = %d", evil)
	}
	if f.count() != 1 {
		t.Fatalf("hits = %d, want 1", f.count())
	}
}

func TestStatusMapping(t *testing.T) {
	cases := []struct {
		status int
		code   string
	}{
		{http.StatusUnauthorized, client.CodeAuth},
		{http.StatusForbidden, client.CodeAuth},
		{http.StatusNotFound, client.CodeNotFound},
		{http.StatusConflict, client.CodeIdempotencyConflict},
		{http.StatusUnprocessableEntity, client.CodeInvalidInput},
		{http.StatusBadGateway, client.CodeUpstream},
	}
	for _, tc := range cases {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			f := newFake(t, func(w http.ResponseWriter, r *http.Request, n int) {
				writeJSON(w, tc.status, `{"error":{"code":"X","message":"upstream rejected the order `+testToken+`","requestId":"req_1"}}`)
			})
			c := demoClient(t, f.srv.URL)
			_, err := c.CreateOffer(context.Background(), "prod_1", "appr_1", goodKey)
			if !isCode(err, tc.code) {
				t.Fatalf("got %v, want %s", err, tc.code)
			}
			assertNoToken(t, err.Error())
			if !strings.Contains(err.Error(), "upstream rejected the order") {
				t.Fatalf("redaction dropped the safe message: %v", err)
			}
			if strings.Contains(err.Error(), "Authorization") || strings.Contains(err.Error(), "Bearer") {
				t.Fatalf("header leaked: %v", err)
			}
			if tc.status >= 500 && f.count() != 1 {
				t.Fatalf("POST 5xx retried, hits = %d", f.count())
			}
			if tc.status < 500 && f.count() != 1 {
				t.Fatalf("hits = %d", f.count())
			}
		})
	}
}

func TestPOSTNotRetriedOnNetworkError(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, n int) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Fatal("no hijacker")
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			t.Fatal(err)
		}
		conn.Close()
	})
	c := demoClient(t, f.srv.URL)
	_, err := c.CreateOffer(context.Background(), "prod_1", "appr_1", goodKey)
	if err == nil {
		t.Fatal("expected network error")
	}
	assertNoToken(t, err.Error())
	if f.count() != 1 {
		t.Fatalf("POST network error retried, hits = %d", f.count())
	}
}

func TestGETRetriesAtMostTwice(t *testing.T) {
	t.Run("succeeds on third attempt", func(t *testing.T) {
		f := newFake(t, func(w http.ResponseWriter, r *http.Request, n int) {
			if n < 3 {
				writeJSON(w, http.StatusServiceUnavailable, `{"error":{"code":"UNAVAILABLE","message":"upstream rejected the order `+testToken+`"}}`)
				return
			}
			writeJSON(w, http.StatusOK, `{"orderId":"ord_1","status":"PENDING","mode":"DEMO","amountCentavos":14900,"reasonCategory":"NONE"}`)
		})
		c := demoClient(t, f.srv.URL)
		got, err := c.GetOrderStatus(context.Background(), "ord_1")
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != "PENDING" || f.count() != 3 {
			t.Fatalf("status=%s hits=%d", got.Status, f.count())
		}
	})
	t.Run("stops after two retries", func(t *testing.T) {
		f := newFake(t, func(w http.ResponseWriter, r *http.Request, n int) {
			writeJSON(w, http.StatusInternalServerError, `{"error":{"message":"upstream rejected the order `+testToken+`"}}`)
		})
		c := demoClient(t, f.srv.URL)
		_, err := c.GetOrderStatus(context.Background(), "ord_1")
		if !isCode(err, client.CodeUpstream) {
			t.Fatal(err)
		}
		assertNoToken(t, err.Error())
		if !strings.Contains(err.Error(), "upstream rejected the order") {
			t.Fatalf("safe message missing: %v", err)
		}
		if f.count() != 3 {
			t.Fatalf("hits = %d, want 3 (1 + 2 retries)", f.count())
		}
	})
	t.Run("retries a dropped connection then succeeds", func(t *testing.T) {
		f := newFake(t, func(w http.ResponseWriter, r *http.Request, n int) {
			if n == 1 {
				hj, _ := w.(http.Hijacker)
				conn, _, err := hj.Hijack()
				if err != nil {
					t.Fatal(err)
				}
				conn.Close()
				return
			}
			writeJSON(w, http.StatusOK, `{"orderId":"ord_1","status":"PENDING","mode":"DEMO","amountCentavos":14900,"reasonCategory":"NONE"}`)
		})
		c := demoClient(t, f.srv.URL)
		if _, err := c.GetOrderStatus(context.Background(), "ord_1"); err != nil {
			t.Fatal(err)
		}
		if f.count() != 2 {
			t.Fatalf("hits = %d, want 2", f.count())
		}
	})
	t.Run("does not retry not found", func(t *testing.T) {
		f := newFake(t, func(w http.ResponseWriter, r *http.Request, n int) {
			writeJSON(w, http.StatusNotFound, `{"error":{"message":"missing"}}`)
		})
		c := demoClient(t, f.srv.URL)
		_, err := c.GetOrderStatus(context.Background(), "ord_1")
		if !isCode(err, client.CodeNotFound) || f.count() != 1 {
			t.Fatalf("err=%v hits=%d", err, f.count())
		}
	})
}

func TestResponseDoesNotEchoSecrets(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, n int) {
		writeJSON(w, http.StatusOK, `{"offerId":"off_1","offerUrl":"https://payvro.example/o/off_1","mode":"DEMO","grantToken":"`+testToken+`","buyerToken":"`+testToken+`"}`)
	})
	c := demoClient(t, f.srv.URL)
	_, err := c.CreateOffer(context.Background(), "prod_1", "appr_1", goodKey)
	if err == nil {
		t.Fatal("extra secret fields were accepted")
	}
	assertNoToken(t, err.Error())
	if strings.Contains(err.Error(), "grantToken") && strings.Contains(err.Error(), testToken) {
		t.Fatal(err)
	}
}

func TestFromEnvDEMOLoopback(t *testing.T) {
	f := newFake(t, func(w http.ResponseWriter, r *http.Request, n int) {
		writeJSON(w, http.StatusOK, `{"orderId":"ord_1","status":"PENDING","mode":"DEMO","amountCentavos":14900,"reasonCategory":"NONE"}`)
	})
	t.Setenv("PAYVRO_API_BASE_URL", f.srv.URL)
	t.Setenv("PAYVRO_MERCHANT_TOKEN", testToken)
	t.Setenv("PAYVRO_MODE", "DEMO")
	c, err := client.FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.GetOrderStatus(context.Background(), "ord_1"); err != nil {
		t.Fatal(err)
	}
}

func isCode(err error, code string) bool {
	var api *client.APIError
	if !errors.As(err, &api) {
		return false
	}
	return api.Code == code
}
