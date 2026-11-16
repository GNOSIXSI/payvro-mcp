//go:build !windows

package direct

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	testKey  = "synthetic-pey-key-NEVER-STORE"
	testIdem = "direct-idempotency-0001"
)

type rewriteTransport struct {
	target   *url.URL
	next     http.RoundTripper
	mu       sync.Mutex
	requests []string
}

func (rt *rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme != "https" || req.URL.Host != "api.pey.mx" {
		return nil, errors.New("unexpected provider destination")
	}
	rt.mu.Lock()
	rt.requests = append(rt.requests, req.Method+" "+req.URL.RequestURI())
	rt.mu.Unlock()
	copyReq := req.Clone(req.Context())
	copyURL := *req.URL
	copyURL.Scheme = rt.target.Scheme
	copyURL.Host = rt.target.Host
	copyReq.URL = &copyURL
	copyReq.Host = rt.target.Host
	return rt.next.RoundTrip(copyReq)
}
func (rt *rewriteTransport) count(method string) int {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	n := 0
	for _, r := range rt.requests {
		if strings.HasPrefix(r, method+" ") {
			n++
		}
	}
	return n
}
func testConfig(t *testing.T, mode string, path string, transport http.RoundTripper) Config {
	t.Helper()
	// Normal test journals live in t.TempDir; make their existing parent private
	// explicitly instead of relying on production code to chmod it.
	if info, err := os.Stat(filepath.Dir(path)); err == nil && info.IsDir() && info.Mode().Perm() != 0700 {
		if err := os.Chmod(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
	}
	return Config{Mode: mode, MaxAmount: 14900, APIKey: testKey, APIKeyID: "acct-test-42", BankAccountID: "bank-test", PublicOrigin: "https://merchant.example", JournalPath: path, Transport: transport}
}
func fakeTransport(t *testing.T, h http.Handler) *rewriteTransport {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	target, _ := url.Parse(srv.URL)
	return &rewriteTransport{target: target, next: http.DefaultTransport}
}

func TestAmountCapAndExactMoneyParsing(t *testing.T) {
	for _, raw := range []string{"", "0", "-1", "+1", "01", "1.0", "1e3", "9007199254740992", "99999999999999999"} {
		if _, err := ValidateAmountCap(raw); err == nil {
			t.Errorf("accepted invalid cap %q", raw)
		}
	}
	if got, err := ValidateAmountCap("14900"); err != nil || got != 14900 {
		t.Fatalf("valid cap: %d %v", got, err)
	}
	for raw, want := range map[string]int64{`149`: 14900, `149.0`: 14900, `149.01`: 14901, `"0.05"`: 5, `1.23`: 123} {
		if got := moneyCentavos(json.RawMessage(raw)); got != want {
			t.Errorf("money %s = %d want %d", raw, got, want)
		}
	}
	for _, raw := range []string{`1.001`, `-1`, `1e2`, `"NaN"`, `null`, `""`, `999999999999999999999`} {
		if got := moneyCentavos(json.RawMessage(raw)); got >= 0 {
			t.Errorf("accepted invalid money %s = %d", raw, got)
		}
	}
}

func TestGetStatusWithoutChargeDoesNotCreateJournal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent", "journal.jsonl")
	svc, err := New(testConfig(t, "DEMO", path, nil))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.GetChargeStatus(context.Background(), testIdem); err == nil {
		t.Fatal("missing journal status succeeded")
	}
	if _, err := os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
		t.Fatalf("read-only status created journal directory: %v", err)
	}
}

func TestGetStatusUnsafeJournalFailsClosedWithoutChangingPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "charges.jsonl")
	svc, err := New(testConfig(t, "DEMO", path, nil))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateCharge(context.Background(), 14900, "existing demo", "qr", testIdem); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	_, statusErr := svc.GetChargeStatus(context.Background(), testIdem)
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !errors.Is(statusErr, ErrJournalUnavailable) {
		t.Fatalf("unsafe journal status error=%v; want fail closed", statusErr)
	}
	if before.Mode().Perm() != after.Mode().Perm() || after.Mode().Perm() != 0644 {
		t.Fatalf("read-only status changed journal permissions: before=%o after=%o", before.Mode().Perm(), after.Mode().Perm())
	}
	if _, err := os.Stat(path + ".lock"); !os.IsNotExist(err) {
		t.Fatalf("read-only status created lock: %v", err)
	}
	if _, err := svc.CreateCharge(context.Background(), 14900, "write hardens", "qr", "write-hardens-0001"); err != nil {
		t.Fatalf("write did not harden prior journal: %v", err)
	}
	written, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if written.Mode().Perm() != 0600 {
		t.Fatalf("write left journal permissions unsafe: mode=%o", written.Mode().Perm())
	}
}

func TestDirectoryMetadataSyncFailurePreventsProviderPost(t *testing.T) {
	root := t.TempDir()
	var posts atomic.Int32
	rt := fakeTransport(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posts.Add(1)
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	oldSync := journalDirectorySync
	journalDirectorySync = func(path string) error {
		if path == root {
			return errors.New("injected directory fsync failure")
		}
		return oldSync(path)
	}
	defer func() { journalDirectorySync = oldSync }()
	svc, err := New(testConfig(t, "LIVE", filepath.Join(root, "nested", "charges.jsonl"), rt))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = svc.CreateCharge(context.Background(), 14900, "durability", "qr", testIdem)
	if posts.Load() != 0 {
		t.Fatalf("provider POST occurred %d times after directory metadata sync failure", posts.Load())
	}
}

func TestCorruptAndTruncatedJournalsBlockStatusAndProviderPost(t *testing.T) {
	for name, mutate := range map[string]func([]byte) []byte{
		"truncated": func(data []byte) []byte { return data[:len(data)-1] },
		"corrupt": func(data []byte) []byte {
			return []byte(strings.Replace(string(data), `"version":1`, `"version":99`, 1))
		},
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "charges.jsonl")
			demo, err := New(testConfig(t, "DEMO", path, nil))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := demo.CreateCharge(context.Background(), 14900, "journal fixture", "qr", testIdem); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			data = mutate(data)
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(path, 0600); err != nil {
				t.Fatal(err)
			}
			var posts atomic.Int32
			rt := fakeTransport(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					posts.Add(1)
				}
				http.Error(w, "unexpected", http.StatusBadGateway)
			}))
			live, err := New(testConfig(t, "LIVE", path, rt))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := live.GetChargeStatus(context.Background(), testIdem); !errors.Is(err, ErrJournalUnavailable) {
				t.Fatalf("status error=%v; want fail closed", err)
			}
			if _, err := live.CreateCharge(context.Background(), 14900, "new intent", "qr", "new-intent-000001"); !errors.Is(err, ErrJournalUnavailable) {
				t.Fatalf("create error=%v; want fail closed", err)
			}
			if posts.Load() != 0 {
				t.Fatalf("corrupt journal reached provider %d times", posts.Load())
			}
		})
	}
}

func TestCrashAfterDurableReservationCannotPostAfterRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "charges.jsonl")
	var posts atomic.Int32
	rt := fakeTransport(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posts.Add(1)
		}
		http.Error(w, "unexpected", http.StatusBadGateway)
	}))
	cfg := testConfig(t, "LIVE", path, rt)
	first, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	intentHash := digest(fmt.Sprintf("qr\x00%d\x00%s", int64(14900), "crash before POST"))
	if _, fresh, err := first.journal.Reserve(first.account, digest(testIdem), intentHash, "qr", 14900, "LIVE"); err != nil || !fresh {
		t.Fatalf("durable reservation fresh=%v err=%v", fresh, err)
	}
	restarted, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	charge, err := restarted.CreateCharge(context.Background(), 14900, "crash before POST", "qr", testIdem)
	if !errors.Is(err, ErrUncertain) || charge.Status != "UNCERTAIN" || posts.Load() != 0 {
		t.Fatalf("after restart charge=%+v err=%v posts=%d", charge, err, posts.Load())
	}
}

func TestJournalEntryDirectorySyncFailurePreventsProviderPost(t *testing.T) {
	root := t.TempDir()
	var posts, directorySyncs atomic.Int32
	rt := fakeTransport(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posts.Add(1)
		}
		http.Error(w, "unexpected", http.StatusBadGateway)
	}))
	oldSync := journalDirectorySync
	journalDirectorySync = func(path string) error {
		if path == root && directorySyncs.Add(1) == 2 {
			return errors.New("injected journal entry fsync failure")
		}
		return oldSync(path)
	}
	defer func() { journalDirectorySync = oldSync }()
	svc, err := New(testConfig(t, "LIVE", filepath.Join(root, "charges.jsonl"), rt))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateCharge(context.Background(), 14900, "journal fsync", "qr", testIdem); !errors.Is(err, ErrJournalUnavailable) {
		t.Fatalf("create error=%v; want journal failure before provider POST", err)
	}
	if directorySyncs.Load() != 2 || posts.Load() != 0 {
		t.Fatalf("directory syncs=%d provider POSTs=%d; want failed second sync and no POST", directorySyncs.Load(), posts.Load())
	}
}

func TestBFFSecretInConceptIsRejectedBeforeJournalOrProviderPost(t *testing.T) {
	const secret = "synthetic-bff-shared-secret-never-store"
	t.Setenv("PAYVRO_BFF_SHARED_SECRET", secret)
	var posts atomic.Int32
	rt := fakeTransport(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posts.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"AB12CD34EF","qrString":"{\\"TYP\\":1,\\"IDC\\":\\"AB12CD34EF\\"}"}`)
	}))
	path := filepath.Join(t.TempDir(), "charges.jsonl")
	svc, err := New(testConfig(t, "LIVE", path, rt))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateCharge(context.Background(), 14900, secret, "qr", testIdem); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("secret concept error=%v; want invalid input", err)
	}
	if posts.Load() != 0 {
		t.Fatalf("secret concept reached provider POST %d times", posts.Load())
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("secret concept was persisted in journal: stat err=%v", err)
	}
}

func TestSensitiveConceptsAreRejectedBeforeJournalAndProviderPost(t *testing.T) {
	const active = "synthetic-active-bff-secret-never-store"
	t.Setenv("PAYVRO_BFF_SHARED_SECRET", active)
	cases := []struct {
		name    string
		concept string
		blocked bool
	}{
		{name: "active credential", concept: "referencia " + active, blocked: true},
		{name: "bearer token", concept: "Bearer payvro-token-0123456789", blocked: true},
		{name: "JWT", concept: "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJzeW50aGV0aWMifQ.signature1234567890", blocked: true},
		{name: "recognizable API key", concept: "sk" + "_live_" + "1234567890abcdefghijklmnop", blocked: true},
		{name: "CLABE grouped with spaces", concept: "CLABE 012 345 678 901 234 567", blocked: true},
		{name: "card grouped with hyphens", concept: "tarjeta 4111-1111-1111-1111", blocked: true},
		{name: "Spanish description and short numbers", concept: "Curso español, edición 3, $149", blocked: false},
		{name: "12 digits allowed", concept: "ref " + strings.Repeat("7", 12), blocked: false},
		{name: "13 digits blocked", concept: "ref " + strings.Repeat("7", 13), blocked: true},
		{name: "19 digits blocked", concept: "ref " + strings.Repeat("7", 19), blocked: true},
		{name: "20 digits allowed heuristic", concept: "ref " + strings.Repeat("7", 20), blocked: false},
		{name: "13 digits dotted undisclosed", concept: "ref 4111.1111.1111.1", blocked: false},
		{name: "nonASCII digits undisclosed", concept: "ref ７７７７７７７７７７７７７", blocked: false},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var posts atomic.Int32
			rt := fakeTransport(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					posts.Add(1)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"id":"QR000001","qrString":"{\"TYP\":1,\"IDC\":\"QR000001\"}"}`)
			}))
			path := filepath.Join(t.TempDir(), "journal.jsonl")
			svc, err := New(testConfig(t, "LIVE", path, rt))
			if err != nil {
				t.Fatal(err)
			}
			key := fmt.Sprintf("direct-concept-edge-%04d", i+1)
			_, err = svc.CreateCharge(context.Background(), 14900, tc.concept, "qr", key)
			if tc.blocked {
				if !errors.Is(err, ErrInvalidInput) {
					t.Fatalf("sensitive concept error=%v; want invalid input", err)
				}
				if posts.Load() != 0 {
					t.Fatalf("sensitive concept reached provider POST %d times", posts.Load())
				}
				if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
					t.Fatalf("sensitive concept reached journal: stat err=%v", statErr)
				}
				return
			}
			if err != nil || posts.Load() != 1 {
				t.Fatalf("legitimate concept rejected or not sent: err=%v POSTs=%d", err, posts.Load())
			}
		})
	}
}

func TestKnownResourceIDFromInvalidCreatePayloadCanBeReadWithoutReposting(t *testing.T) {
	for _, method := range []string{"qr", "link"} {
		t.Run(method, func(t *testing.T) {
			var posts, gets atomic.Int32
			var reference string
			path := filepath.Join(t.TempDir(), "charges.jsonl")
			rt := fakeTransport(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case method == "qr" && r.Method == http.MethodPost && r.URL.Path == "/codi/qr":
					posts.Add(1)
					var request struct {
						NumRef string `json:"numRef"`
					}
					if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
						t.Errorf("decode QR create body: %v", err)
					}
					reference = request.NumRef
					_, _ = io.WriteString(w, `{"id":"QR000001","qrString":"{\"TYP\":1,\"IDC\":\"WRONG\"}"}`)
				case method == "qr" && r.Method == http.MethodGet && r.URL.Path == "/transaction/codi/qr/QR000001":
					gets.Add(1)
					_, _ = io.WriteString(w, `{"items":[]}`)
				case method == "link" && r.Method == http.MethodPost && r.URL.Path == "/payment-link/create":
					posts.Add(1)
					var request struct {
						ClientReferenceID string `json:"clientReferenceId"`
					}
					if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
						t.Errorf("decode link create body: %v", err)
					}
					reference = request.ClientReferenceID
					_, _ = io.WriteString(w, `{"id":"LINK0001","url":"https://evil.example/payment-link/LINK0001"}`)
				case method == "link" && r.Method == http.MethodGet && r.URL.Path == "/payment-link/LINK0001":
					gets.Add(1)
					_ = json.NewEncoder(w).Encode(map[string]any{"id": "LINK0001", "amount": 149, "status": "created", "hasTip": false, "clientReferenceId": reference, "transactions": []any{}})
				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL)
					http.NotFound(w, r)
				}
			}))
			svc, err := New(testConfig(t, "LIVE", path, rt))
			if err != nil {
				t.Fatal(err)
			}
			created, createErr := svc.CreateCharge(context.Background(), 14900, "safe lookup", method, testIdem)
			wantID := "LINK0001"
			if method == "qr" {
				wantID = "QR000001"
			}
			if !errors.Is(createErr, ErrUncertain) || created.Status != "UNCERTAIN" || created.ChargeID != wantID {
				t.Fatalf("invalid create payload result=%+v err=%v; want durable known ID and UNCERTAIN", created, createErr)
			}
			if !strings.Contains(created.Message, "get_charge_status") {
				t.Fatalf("known ID was not offered for safe status lookup: %+v", created)
			}
			status, statusErr := svc.GetChargeStatus(context.Background(), testIdem)
			if statusErr != nil || status.Status != "PENDING" || status.ChargeID != wantID {
				t.Fatalf("read-only status=%+v err=%v", status, statusErr)
			}
			if posts.Load() != 1 || gets.Load() != 1 {
				t.Fatalf("requests POST=%d GET=%d; want one each", posts.Load(), gets.Load())
			}
		})
	}
}

func TestDemoIsLocalDurableAndNeverCallsProvider(t *testing.T) {
	var hits atomic.Int32
	rt := fakeTransport(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1); http.Error(w, "forbidden", 500) }))
	path := filepath.Join(t.TempDir(), "charges.jsonl")
	cfg := testConfig(t, "DEMO", path, rt)
	cfg.APIKey = testKey
	svc, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	got, err := svc.CreateCharge(context.Background(), 14900, "Demo concepto", "qr", testIdem)
	if err != nil {
		t.Fatal(err)
	}
	if got.Mode != "DEMO" || got.Status != "DEMO_ONLY" || !strings.Contains(got.QRPayload, "NOT-PAYABLE") {
		t.Fatalf("demo result=%+v", got)
	}
	replay, err := svc.CreateCharge(context.Background(), 14900, "Demo concepto", "qr", testIdem)
	if err != nil || replay.ChargeID != got.ChargeID {
		t.Fatalf("replay=%+v err=%v", replay, err)
	}
	if _, err = svc.CreateCharge(context.Background(), 14800, "Demo concepto", "qr", testIdem); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("changed intent error=%v", err)
	}
	status, err := svc.GetChargeStatus(context.Background(), testIdem)
	if err != nil || status.Status != "DEMO_ONLY" {
		t.Fatalf("demo status=%+v err=%v", status, err)
	}
	if hits.Load() != 0 {
		t.Fatalf("DEMO sent %d requests to provider", hits.Load())
	}
	lines, err := ScanJournal(path)
	if err != nil || len(lines) != 2 {
		t.Fatalf("journal lines=%d err=%v", len(lines), err)
	}
	for _, line := range lines {
		if strings.Contains(line, testKey) {
			t.Fatal("provider key persisted in journal")
		}
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("journal mode=%o", info.Mode().Perm())
	}
}

func TestDemoAndLiveShareJournalWithoutCrossModeReplay(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.jsonl")
	demoRT := fakeTransport(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("DEMO provider request: %s %s", r.Method, r.URL)
	}))
	demo, err := New(testConfig(t, "DEMO", path, demoRT))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := demo.CreateCharge(context.Background(), 14900, "mode boundary", "qr", testIdem); err != nil {
		t.Fatal(err)
	}
	var posts atomic.Int32
	liveRT := fakeTransport(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts.Add(1)
		_, _ = io.WriteString(w, `{"id":"AB12CD34EF","qrString":"{\"TYP\":1,\"IDC\":\"AB12CD34EF\"}"}`)
	}))
	live, err := New(testConfig(t, "LIVE", path, liveRT))
	if err != nil {
		t.Fatal(err)
	}
	got, err := live.CreateCharge(context.Background(), 14900, "mode boundary", "qr", testIdem)
	if err != nil || got.Status != "CREATED" || got.Mode != "LIVE" || posts.Load() != 1 {
		t.Fatalf("cross-mode create=%+v err=%v posts=%d", got, err, posts.Load())
	}
}

func TestLiveQRCreateAndAuthenticatedStatusValidateContract(t *testing.T) {
	var posts atomic.Int32
	var getPaths []string
	rt := fakeTransport(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+testKey {
			t.Errorf("wrong auth header")
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/codi/qr":
			posts.Add(1)
			body, _ := io.ReadAll(r.Body)
			var in map[string]any
			_ = json.Unmarshal(body, &in)
			if in["amount"] != float64(149) || in["expiration"] != float64(600) || in["generateImage"] != false || in["numRef"] == nil || in["bankAccountId"] != "bank-test" {
				t.Errorf("QR body=%s", body)
			}
			_, _ = io.WriteString(w, `{"id":"AB12CD34EF","qrString":"{\"TYP\":1,\"IDC\":\"AB12CD34EF\"}"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/transaction/codi/qr/AB12CD34EF":
			getPaths = append(getPaths, r.URL.Path)
			// Match the documented list summary: no payable identity is present.
			_, _ = io.WriteString(w, `{"items":[{"id":"TXN-0001","status":"PAID","currency":"MXN","paymentMethod":"CODI","amount":149,"amountBreakdown":{"base":149}}]}`)
		case r.Method == http.MethodGet && r.URL.Path == "/transaction/TXN-0001":
			getPaths = append(getPaths, r.URL.Path)
			_, _ = io.WriteString(w, `{"id":"TXN-0001","status":"PAID","currency":"MXN","paymentMethod":"CODI","payableId":"AB12CD34EF","payableType":"API_CODI_QR","amount":149,"amountBreakdown":{"base":149}}`)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
			http.NotFound(w, r)
		}
	}))
	path := filepath.Join(t.TempDir(), "journal.jsonl")
	cfg := testConfig(t, "LIVE", path, rt)
	svc, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	charge, err := svc.CreateCharge(context.Background(), 14900, "Consulta", "qr", testIdem)
	if err != nil || charge.Status != "CREATED" {
		t.Fatalf("create=%+v err=%v", charge, err)
	}
	postsBeforeStatus := posts.Load()
	status, err := svc.GetChargeStatus(context.Background(), testIdem)
	if err != nil || status.Status != "PAID" {
		t.Fatalf("status=%+v err=%v", status, err)
	}
	if posts.Load() != postsBeforeStatus || posts.Load() != 1 || rt.count("POST") != 1 {
		t.Fatalf("status read made a POST: before=%d after=%d", postsBeforeStatus, posts.Load())
	}
	if len(getPaths) != 2 || getPaths[0] != "/transaction/codi/qr/AB12CD34EF" || getPaths[1] != "/transaction/TXN-0001" {
		t.Fatalf("status GET paths=%v; want fixed list then detail endpoints", getPaths)
	}
	lines, err := ScanJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range lines {
		if strings.Contains(line, testKey) {
			t.Fatal("credential stored")
		}
	}
}

func TestQRListAndDetailMustAgreeBeforePaid(t *testing.T) {
	validList := `{"id":"TXN-0001","status":"PAID","currency":"MXN","paymentMethod":"CODI","amount":149,"amountBreakdown":{"base":149}}`
	validDetail := `{"id":"TXN-0001","status":"PAID","currency":"MXN","paymentMethod":"CODI","payableId":"AB12CD34EF","payableType":"API_CODI_QR","amount":149,"amountBreakdown":{"base":149}}`
	cases := map[string]struct {
		listed, detail string
		listItems      string
		wantGets       int
	}{
		"list method absent":              {listed: `{"id":"TXN-0001","status":"PAID","currency":"MXN","amount":149,"amountBreakdown":{"base":149}}`, detail: validDetail, wantGets: 1},
		"detail method absent":            {listed: validList, detail: `{"id":"TXN-0001","status":"PAID","currency":"MXN","payableId":"AB12CD34EF","payableType":"API_CODI_QR","amount":149,"amountBreakdown":{"base":149}}`, wantGets: 1},
		"detail identity absent":          {listed: validList, detail: `{"id":"TXN-0001","status":"PAID","currency":"MXN","paymentMethod":"CODI","amount":149,"amountBreakdown":{"base":149}}`, wantGets: 1},
		"list currency absent":            {listed: `{"id":"TXN-0001","status":"PAID","paymentMethod":"CODI","amount":149,"amountBreakdown":{"base":149}}`, detail: validDetail, wantGets: 1},
		"detail status unknown":           {listed: validList, detail: strings.Replace(validDetail, `"status":"PAID"`, `"status":"PAID MAYBE"`, 1), wantGets: 1},
		"detail method not CODI":          {listed: validList, detail: strings.Replace(validDetail, `"paymentMethod":"CODI"`, `"paymentMethod":"CARD"`, 1), wantGets: 1},
		"detail other resource":           {listed: validList, detail: strings.Replace(validDetail, `"payableId":"AB12CD34EF"`, `"payableId":"OTHER-QR"`, 1), wantGets: 1},
		"detail other type":               {listed: validList, detail: strings.Replace(validDetail, `"payableType":"API_CODI_QR"`, `"payableType":"API_CODI_PN"`, 1), wantGets: 1},
		"detail wrong currency":           {listed: validList, detail: strings.Replace(validDetail, `"currency":"MXN"`, `"currency":"USD"`, 1), wantGets: 1},
		"detail wrong base":               {listed: validList, detail: strings.Replace(validDetail, `"base":149`, `"base":148`, 1), wantGets: 1},
		"detail ID mismatch":              {listed: validList, detail: strings.Replace(validDetail, `"id":"TXN-0001"`, `"id":"TXN-0002"`, 1), wantGets: 1},
		"list detail status mismatch":     {listed: strings.Replace(validList, `"status":"PAID"`, `"status":"PENDING"`, 1), detail: validDetail, wantGets: 1},
		"list detail amount mismatch":     {listed: strings.Replace(validList, `"amount":149`, `"amount":148`, 1), detail: validDetail, wantGets: 1},
		"duplicate listed transaction ID": {listItems: `[` + validList + `,` + validList + `]`, wantGets: 0},
		"two distinct paid summaries":     {detail: validDetail, listItems: `[` + validList + `,` + strings.ReplaceAll(validList, "TXN-0001", "TXN-0002") + `]`, wantGets: 2},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if tc.listItems == "" {
				tc.listItems = "[" + tc.listed + "]"
			}
			var posts, detailGets atomic.Int32
			rt := fakeTransport(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer "+testKey {
					t.Errorf("missing fixed bearer authorization on %s", r.URL.Path)
				}
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.Method == http.MethodPost && r.URL.Path == "/codi/qr":
					posts.Add(1)
					_, _ = io.WriteString(w, `{"id":"AB12CD34EF","qrString":"{\"TYP\":1,\"IDC\":\"AB12CD34EF\"}"}`)
				case r.Method == http.MethodGet && r.URL.Path == "/transaction/codi/qr/AB12CD34EF":
					_, _ = io.WriteString(w, `{"items":`+tc.listItems+`}`)
				case r.Method == http.MethodGet && r.URL.Path == "/transaction/TXN-0001":
					detailGets.Add(1)
					_, _ = io.WriteString(w, tc.detail)
				case r.Method == http.MethodGet && r.URL.Path == "/transaction/TXN-0002":
					detailGets.Add(1)
					_, _ = io.WriteString(w, strings.ReplaceAll(validDetail, "TXN-0001", "TXN-0002"))
				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL)
					http.NotFound(w, r)
				}
			}))
			svc, err := New(testConfig(t, "LIVE", filepath.Join(t.TempDir(), "journal.jsonl"), rt))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := svc.CreateCharge(context.Background(), 14900, "QR parser regression", "qr", testIdem); err != nil {
				t.Fatalf("valid create failed: %v", err)
			}
			postsBeforeStatus := posts.Load()
			got, err := svc.GetChargeStatus(context.Background(), testIdem)
			if err != nil || got.Status != "NEEDS_REVIEW" {
				t.Fatalf("status=%+v err=%v; expected review", got, err)
			}
			if posts.Load() != postsBeforeStatus || posts.Load() != 1 {
				t.Fatalf("status lookup changed POST count: before=%d after=%d", postsBeforeStatus, posts.Load())
			}
			if int(detailGets.Load()) != tc.wantGets {
				t.Fatalf("detail GET count=%d want=%d", detailGets.Load(), tc.wantGets)
			}
		})
	}
}

func TestUncertainPOSTNeverRetriesAcrossDuplicateOrRestart(t *testing.T) {
	var posts atomic.Int32
	rt := fakeTransport(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"message":"temporarily unavailable"}`)
	}))
	path := filepath.Join(t.TempDir(), "journal.jsonl")
	cfg := testConfig(t, "LIVE", path, rt)
	svc, _ := New(cfg)
	got, err := svc.CreateCharge(context.Background(), 14900, "No retry", "qr", testIdem)
	if !errors.Is(err, ErrUncertain) || got.Status != "UNCERTAIN" || !got.ManualReview {
		t.Fatalf("outcome=%+v err=%v", got, err)
	}
	_, err = svc.CreateCharge(context.Background(), 14900, "No retry", "qr", testIdem)
	if !errors.Is(err, ErrUncertain) {
		t.Fatalf("replay err=%v", err)
	}
	restarted, _ := New(cfg)
	_, err = restarted.CreateCharge(context.Background(), 14900, "No retry", "qr", testIdem)
	if !errors.Is(err, ErrUncertain) {
		t.Fatalf("restart err=%v", err)
	}
	if _, err = svc.CreateCharge(context.Background(), 100, "Different intent", "qr", testIdem); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("changed intent err=%v", err)
	}
	if posts.Load() != 1 {
		t.Fatalf("POSTs=%d; want exactly one", posts.Load())
	}
	status, err := restarted.GetChargeStatus(context.Background(), testIdem)
	if !errors.Is(err, ErrUncertain) || status.Status != "UNCERTAIN" {
		t.Fatalf("status=%+v err=%v", status, err)
	}
	if rt.count("GET") != 0 {
		t.Fatalf("unknown resource ID triggered provider GET %d times", rt.count("GET"))
	}
}

func TestProviderMalformedAndRedirectResponsesAreUncertainAndNotFollowed(t *testing.T) {
	cases := map[string]struct {
		handler       http.Handler
		wantUncertain bool
	}{
		"malformed": {handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(201)
			_, _ = io.WriteString(w, `{"id":"","qrString":null}`)
		}), wantUncertain: true},
		"redirect same provider host": {handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "https://api.pey.mx/other", http.StatusFound)
		}), wantUncertain: true},
		"valid qr create": {handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"AB12CD34EF","qrString":"{\"TYP\":1,\"IDC\":\"AB12CD34EF\"}"}`)
		}), wantUncertain: false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var hits atomic.Int32
			base := fakeTransport(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer "+testKey {
					t.Errorf("missing fixed bearer authorization on %s", r.URL.Path)
				}
				hits.Add(1)
				tc.handler.ServeHTTP(w, r)
			}))
			svc, _ := New(testConfig(t, "LIVE", filepath.Join(t.TempDir(), "journal.jsonl"), base))
			_, err := svc.CreateCharge(context.Background(), 100, "case", "qr", testIdem)
			if tc.wantUncertain {
				if !errors.Is(err, ErrUncertain) {
					t.Fatalf("err=%v; want uncertain", err)
				}
			} else if err != nil {
				t.Fatalf("valid create err=%v", err)
			}
			if hits.Load() != 1 {
				t.Fatalf("authenticated request count=%d; want 1 (redirect must not be followed)", hits.Load())
			}
		})
	}
}

func TestWrongCurrencyOrAmountNeverReturnsPaid(t *testing.T) {
	for name, tx := range map[string]string{
		"wrong currency": `{"id":"TXN-0001","status":"PAID","currency":"USD","payableId":"AB12CD34EF","payableType":"API_CODI_QR","amount":149,"amountBreakdown":{"base":149}}`,
		"wrong base":     `{"id":"TXN-0001","status":"PAID","currency":"MXN","payableId":"AB12CD34EF","payableType":"API_CODI_QR","amount":149,"amountBreakdown":{"base":148}}`,
		"unknown status": `{"id":"TXN-0001","status":"paid maybe","currency":"MXN","payableId":"AB12CD34EF","payableType":"API_CODI_QR","amount":149,"amountBreakdown":{"base":149}}`,
	} {
		t.Run(name, func(t *testing.T) {
			rt := fakeTransport(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.Method == http.MethodPost {
					_, _ = io.WriteString(w, `{"id":"AB12CD34EF","qrString":"{\"TYP\":1,\"IDC\":\"AB12CD34EF\"}"}`)
				} else {
					_, _ = io.WriteString(w, `{"items":[`+tx+`]}`)
				}
			}))
			svc, _ := New(testConfig(t, "LIVE", filepath.Join(t.TempDir(), "journal.jsonl"), rt))
			_, _ = svc.CreateCharge(context.Background(), 14900, "case", "qr", testIdem)
			got, err := svc.GetChargeStatus(context.Background(), testIdem)
			if err != nil || got.Status != "NEEDS_REVIEW" {
				t.Fatalf("status=%+v err=%v", got, err)
			}
		})
	}
}

func TestLinkCreateAndStatusCheckIDsAndNoTip(t *testing.T) {
	var ref string
	rt := fakeTransport(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/payment-link/create":
			body, _ := io.ReadAll(r.Body)
			var in struct {
				Amount  json.Number `json:"amount"`
				Success string      `json:"successUrl"`
				Back    string      `json:"backUrl"`
				Tip     bool        `json:"hasTip"`
				Ref     string      `json:"clientReferenceId"`
			}
			_ = json.Unmarshal(body, &in)
			if in.Amount.String() != "149.00" || in.Success != "https://merchant.example/payment/return" || in.Back != in.Success || in.Tip {
				t.Errorf("link create body=%s", body)
			}
			ref = in.Ref
			_, _ = io.WriteString(w, `{"id":"LINK0001","url":"https://app.mipey.mx/payment-link/LINK0001"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/payment-link/LINK0001":
			b, _ := json.Marshal(map[string]any{"id": "LINK0001", "amount": 149, "status": "paid", "hasTip": false, "clientReferenceId": ref, "transactions": []any{map[string]any{"transactionId": "TXN-0001", "paymentMethod": "CARD", "amount": 149, "amountBreakdown": map[string]any{"base": 149, "fee": 0, "tip": 0}}}})
			_, _ = w.Write(b)
		case r.Method == http.MethodGet && r.URL.Path == "/transaction/TXN-0001":
			_, _ = io.WriteString(w, `{"id":"TXN-0001","status":"PAID","currency":"MXN","paymentMethod":"CARD","payableId":"LINK0001","payableType":"PAYMENT_LINK","amount":149,"amountBreakdown":{"base":149,"fee":0,"tip":0}}`)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL)
		}
	}))
	cfg := testConfig(t, "LIVE", filepath.Join(t.TempDir(), "journal.jsonl"), rt)
	svc, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	created, err := svc.CreateCharge(context.Background(), 14900, "Curso", "link", testIdem)
	if err != nil || created.PaymentURL != "https://app.mipey.mx/payment-link/LINK0001" {
		t.Fatalf("create=%+v err=%v", created, err)
	}
	status, err := svc.GetChargeStatus(context.Background(), testIdem)
	if err != nil || status.Status != "PAID" {
		t.Fatalf("status=%+v err=%v", status, err)
	}
}

func TestLinkCallerRequiresSupportedConsistentPaymentMethod(t *testing.T) {
	cases := []struct {
		name          string
		summaryMethod string
		detailMethod  string
		omitSummary   bool
		omitDetail    bool
		summaryAmount int64
		detailAmount  int64
		currency      string
		payableID     string
		want          string
	}{
		{name: "missing detail method", summaryMethod: "CARD", detailMethod: "CARD", omitDetail: true, want: "NEEDS_REVIEW"},
		{name: "missing summary method", summaryMethod: "CARD", detailMethod: "CARD", omitSummary: true, want: "NEEDS_REVIEW"},
		{name: "empty method", summaryMethod: "CARD", detailMethod: "", want: "NEEDS_REVIEW"},
		{name: "unknown method", summaryMethod: "WIRE", detailMethod: "WIRE", want: "NEEDS_REVIEW"},
		{name: "summary detail mismatch", summaryMethod: "CARD", detailMethod: "CODI", want: "NEEDS_REVIEW"},
		{name: "summary amount mismatch", summaryMethod: "CARD", detailMethod: "CARD", summaryAmount: 148, want: "NEEDS_REVIEW"},
		{name: "detail amount mismatch", summaryMethod: "CARD", detailMethod: "CARD", detailAmount: 148, want: "NEEDS_REVIEW"},
		{name: "detail currency mismatch", summaryMethod: "CARD", detailMethod: "CARD", currency: "USD", want: "NEEDS_REVIEW"},
		{name: "detail payable mismatch", summaryMethod: "CARD", detailMethod: "CARD", payableID: "OTHERLINK", want: "NEEDS_REVIEW"},
		{name: "valid card", summaryMethod: "CARD", detailMethod: "CARD", want: "PAID"},
		{name: "valid codi", summaryMethod: "CODI", detailMethod: "CODI", want: "PAID"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var reference string
			rt := fakeTransport(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.Method == http.MethodPost && r.URL.Path == "/payment-link/create":
					var input map[string]json.RawMessage
					if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
						t.Errorf("decode create request: %v", err)
					}
					_ = json.Unmarshal(input["clientReferenceId"], &reference)
					_, _ = io.WriteString(w, `{"id":"LINK0001","url":"https://app.mipey.mx/payment-link/LINK0001"}`)
				case r.Method == http.MethodGet && r.URL.Path == "/payment-link/LINK0001":
					summaryAmount := tc.summaryAmount
					if summaryAmount == 0 {
						summaryAmount = 149
					}
					transaction := map[string]any{"transactionId": "TXN-0001", "paymentMethod": tc.summaryMethod, "amount": summaryAmount, "amountBreakdown": map[string]any{"base": 149, "fee": 0, "tip": 0}}
					if tc.omitSummary {
						delete(transaction, "paymentMethod")
					}
					root := map[string]any{"id": "LINK0001", "amount": 149, "status": "paid", "hasTip": false, "clientReferenceId": reference, "transactions": []any{transaction}}
					_ = json.NewEncoder(w).Encode(root)
				case r.Method == http.MethodGet && r.URL.Path == "/transaction/TXN-0001":
					detailAmount := tc.detailAmount
					if detailAmount == 0 {
						detailAmount = 149
					}
					currency := tc.currency
					if currency == "" {
						currency = "MXN"
					}
					payableID := tc.payableID
					if payableID == "" {
						payableID = "LINK0001"
					}
					tx := map[string]any{"id": "TXN-0001", "status": "PAID", "currency": currency, "payableId": payableID, "payableType": "PAYMENT_LINK", "amount": detailAmount, "amountBreakdown": map[string]any{"base": 149, "fee": 0, "tip": 0}}
					if !tc.omitDetail {
						tx["paymentMethod"] = tc.detailMethod
					}
					_ = json.NewEncoder(w).Encode(tx)
				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL)
					http.NotFound(w, r)
				}
			}))
			svc, err := New(testConfig(t, "LIVE", filepath.Join(t.TempDir(), "journal.jsonl"), rt))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := svc.CreateCharge(context.Background(), 14900, "valid link", "link", testIdem); err != nil {
				t.Fatalf("create: %v", err)
			}
			got, err := svc.GetChargeStatus(context.Background(), testIdem)
			if err != nil || got.Status != tc.want {
				t.Fatalf("caller status=%+v err=%v, want %s", got, err, tc.want)
			}
		})
	}
}

func TestQRTruncatedPaginationCannotReportPaid(t *testing.T) {
	validList := `{"id":"TXN-0001","status":"PAID","currency":"MXN","paymentMethod":"CODI","amount":149,"amountBreakdown":{"base":149}}`
	validDetail := `{"id":"TXN-0001","status":"PAID","currency":"MXN","paymentMethod":"CODI","payableId":"AB12CD34EF","payableType":"API_CODI_QR","amount":149,"amountBreakdown":{"base":149}}`
	cases := []struct {
		name         string
		list         func(page int32) string
		want         string
		wantListGets int32
		wantDetails  int32
	}{
		{
			name: "page1 paid then empty pages 2..5 still nextToken",
			list: func(page int32) string {
				if page == 1 {
					return fmt.Sprintf(`{"items":[%s],"nextToken":"cursor-%d"}`, validList, page)
				}
				return fmt.Sprintf(`{"items":[],"nextToken":"cursor-%d"}`, page)
			},
			want:         "NEEDS_REVIEW",
			wantListGets: maxPages,
			wantDetails:  0,
		},
		{
			name: "complete bounded list page1 paid page2 empty no nextToken",
			list: func(page int32) string {
				if page == 1 {
					return fmt.Sprintf(`{"items":[%s],"nextToken":"cursor-%d"}`, validList, page)
				}
				return `{"items":[]}`
			},
			want:         "PAID",
			wantListGets: 2,
			wantDetails:  1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var posts, listGets, detailGets atomic.Int32
			rt := fakeTransport(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer "+testKey {
					t.Errorf("missing fixed bearer authorization on %s", r.URL.Path)
				}
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.Method == http.MethodPost && r.URL.Path == "/codi/qr":
					posts.Add(1)
					_, _ = io.WriteString(w, `{"id":"AB12CD34EF","qrString":"{\"TYP\":1,\"IDC\":\"AB12CD34EF\"}"}`)
				case r.Method == http.MethodGet && r.URL.Path == "/transaction/codi/qr/AB12CD34EF":
					page := listGets.Add(1)
					_, _ = io.WriteString(w, tc.list(page))
				case r.Method == http.MethodGet && r.URL.Path == "/transaction/TXN-0001":
					detailGets.Add(1)
					_, _ = io.WriteString(w, validDetail)
				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL)
					http.NotFound(w, r)
				}
			}))
			svc, err := New(testConfig(t, "LIVE", filepath.Join(t.TempDir(), "journal.jsonl"), rt))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := svc.CreateCharge(context.Background(), 14900, "pagination", "qr", testIdem); err != nil {
				t.Fatal(err)
			}
			postsBeforeStatus := posts.Load()
			got, err := svc.GetChargeStatus(context.Background(), testIdem)
			if err != nil || got.Status != tc.want {
				t.Fatalf("status=%+v err=%v; want %s", got, err, tc.want)
			}
			if listGets.Load() != tc.wantListGets || detailGets.Load() != tc.wantDetails || posts.Load() != postsBeforeStatus {
				t.Fatalf("list GETs=%d want=%d detail GETs=%d want=%d POSTs before=%d after=%d", listGets.Load(), tc.wantListGets, detailGets.Load(), tc.wantDetails, postsBeforeStatus, posts.Load())
			}
		})
	}
}

func TestLinkStatusMoneyIdentityAndStateAtGetChargeStatus(t *testing.T) {
	type txSpec struct {
		id     string
		base   int
		tip    int
		status string
	}
	cases := []struct {
		name    string
		rootMut func(map[string]any)
		txs     []txSpec
		omitTxs bool
		want    string
	}{
		{name: "valid single paid", txs: []txSpec{{id: "TXN-0001", base: 149, status: "PAID"}}, want: "PAID"},
		{name: "underpaid single tx base 100 vs 149", txs: []txSpec{{id: "TXN-0001", base: 100, status: "PAID"}}, want: "NEEDS_REVIEW"},
		{name: "paid without transactions", omitTxs: true, want: "NEEDS_REVIEW"},
		{name: "wrong clientReferenceId", rootMut: func(root map[string]any) { root["clientReferenceId"] = "payvro-other" }, txs: []txSpec{{id: "TXN-0001", base: 149, status: "PAID"}}, want: "NEEDS_REVIEW"},
		{name: "root id mismatch", rootMut: func(root map[string]any) { root["id"] = "LINK9999" }, txs: []txSpec{{id: "TXN-0001", base: 149, status: "PAID"}}, want: "NEEDS_REVIEW"},
		{name: "txs paid but link status pending", rootMut: func(root map[string]any) { root["status"] = "pending" }, txs: []txSpec{{id: "TXN-0001", base: 149, status: "PAID"}}, want: "PENDING"},
		{name: "tx tip nonzero", txs: []txSpec{{id: "TXN-0001", base: 149, tip: 5, status: "PAID"}}, want: "NEEDS_REVIEW"},
		{name: "two partial txs summing to expected", txs: []txSpec{{id: "TXN-0001", base: 100, status: "PAID"}, {id: "TXN-0002", base: 49, status: "PAID"}}, want: "PAID"},
		{name: "two paid txs summing over expected", txs: []txSpec{{id: "TXN-0001", base: 100, status: "PAID"}, {id: "TXN-0002", base: 100, status: "PAID"}}, want: "NEEDS_REVIEW"},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var reference string
			details := map[string]map[string]any{}
			summaries := make([]any, 0, len(tc.txs))
			for _, spec := range tc.txs {
				amount := spec.base + spec.tip
				bd := map[string]any{"base": spec.base, "fee": 0, "tip": spec.tip}
				summaries = append(summaries, map[string]any{"transactionId": spec.id, "paymentMethod": "CODI", "amount": amount, "amountBreakdown": bd})
				details[spec.id] = map[string]any{"id": spec.id, "status": spec.status, "currency": "MXN", "paymentMethod": "CODI", "payableId": "LINK0001", "payableType": "PAYMENT_LINK", "amount": amount, "amountBreakdown": bd}
			}
			rt := fakeTransport(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer "+testKey {
					t.Errorf("missing fixed bearer authorization on %s", r.URL.Path)
				}
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.Method == http.MethodPost && r.URL.Path == "/payment-link/create":
					var input map[string]json.RawMessage
					if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
						t.Errorf("decode create request: %v", err)
					}
					_ = json.Unmarshal(input["clientReferenceId"], &reference)
					_, _ = io.WriteString(w, `{"id":"LINK0001","url":"https://app.mipey.mx/payment-link/LINK0001"}`)
				case r.Method == http.MethodGet && r.URL.Path == "/payment-link/LINK0001":
					root := map[string]any{"id": "LINK0001", "amount": 149, "status": "paid", "hasTip": false, "clientReferenceId": reference}
					if tc.omitTxs {
						root["transactions"] = []any{}
					} else {
						root["transactions"] = summaries
					}
					if tc.rootMut != nil {
						tc.rootMut(root)
					}
					_ = json.NewEncoder(w).Encode(root)
				case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/transaction/"):
					id := strings.TrimPrefix(r.URL.Path, "/transaction/")
					if detail, ok := details[id]; ok {
						_ = json.NewEncoder(w).Encode(detail)
						return
					}
					http.NotFound(w, r)
				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL)
					http.NotFound(w, r)
				}
			}))
			key := fmt.Sprintf("direct-link-money-%04d", i+1)
			svc, err := New(testConfig(t, "LIVE", filepath.Join(t.TempDir(), "journal.jsonl"), rt))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := svc.CreateCharge(context.Background(), 14900, "link money identity", "link", key); err != nil {
				t.Fatalf("create: %v", err)
			}
			got, err := svc.GetChargeStatus(context.Background(), key)
			if err != nil || got.Status != tc.want {
				t.Fatalf("caller status=%+v err=%v, want %s", got, err, tc.want)
			}
		})
	}
}

func TestPaymentLinkHasTipMustBeExplicitBooleanAtStatusCaller(t *testing.T) {
	cases := map[string]struct {
		tip  json.RawMessage
		omit bool
		want string
	}{
		"absent": {omit: true, want: "NEEDS_REVIEW"},
		"null":   {tip: json.RawMessage("null"), want: "NEEDS_REVIEW"},
		"string": {tip: json.RawMessage(`"false"`), want: "NEEDS_REVIEW"},
		"true":   {tip: json.RawMessage("true"), want: "NEEDS_REVIEW"},
		"false":  {tip: json.RawMessage("false"), want: "PAID"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var ref string
			var posts atomic.Int32
			rt := fakeTransport(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.Method == http.MethodPost && r.URL.Path == "/payment-link/create":
					posts.Add(1)
					var in map[string]json.RawMessage
					if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
						t.Errorf("decode create request: %v", err)
					}
					_ = json.Unmarshal(in["clientReferenceId"], &ref)
					_, _ = io.WriteString(w, `{"id":"LINK0001","url":"https://app.mipey.mx/payment-link/LINK0001"}`)
				case r.Method == http.MethodGet && r.URL.Path == "/payment-link/LINK0001":
					root := map[string]json.RawMessage{
						"id": json.RawMessage(`"LINK0001"`), "amount": json.RawMessage("149"),
						"status": json.RawMessage(`"paid"`), "clientReferenceId": mustJSON(ref),
						"transactions": json.RawMessage(`[ {"transactionId":"TXN-0001","paymentMethod":"CODI","amount":149,"amountBreakdown":{"base":149,"fee":0,"tip":0}} ]`),
					}
					if !tc.omit {
						root["hasTip"] = tc.tip
					}
					if err := json.NewEncoder(w).Encode(root); err != nil {
						t.Errorf("encode link status: %v", err)
					}
				case r.Method == http.MethodGet && r.URL.Path == "/transaction/TXN-0001":
					_, _ = io.WriteString(w, `{"id":"TXN-0001","status":"PAID","currency":"MXN","paymentMethod":"CODI","payableId":"LINK0001","payableType":"PAYMENT_LINK","amount":149,"amountBreakdown":{"base":149,"fee":0,"tip":0}}`)
				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL)
					http.NotFound(w, r)
				}
			}))
			svc, err := New(testConfig(t, "LIVE", filepath.Join(t.TempDir(), "journal.jsonl"), rt))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := svc.CreateCharge(context.Background(), 14900, "Link parser regression", "link", testIdem); err != nil {
				t.Fatalf("valid create failed: %v", err)
			}
			postsBeforeStatus := posts.Load()
			got, err := svc.GetChargeStatus(context.Background(), testIdem)
			if err != nil || got.Status != tc.want {
				t.Fatalf("status=%+v err=%v; want %s", got, err, tc.want)
			}
			if posts.Load() != postsBeforeStatus || posts.Load() != 1 {
				t.Fatalf("status lookup changed POST count: before=%d after=%d", postsBeforeStatus, posts.Load())
			}
		})
	}
}

func mustJSON(value string) json.RawMessage {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return encoded
}

func TestNewDedicatedJournalDirectoryIsPrivateWithoutChangingSharedParent(t *testing.T) {
	root := t.TempDir()
	shared := filepath.Join(root, "shared")
	if err := os.Mkdir(shared, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(shared, 0755); err != nil {
		t.Fatal(err)
	}
	var posts atomic.Int32
	rt := fakeTransport(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posts.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"AB12CD34EF","qrString":"{\"TYP\":1,\"IDC\":\"AB12CD34EF\"}"}`)
	}))
	journalDir := filepath.Join(shared, "payvro-direct")
	svc, err := New(testConfig(t, "LIVE", filepath.Join(journalDir, "charges.jsonl"), rt))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateCharge(context.Background(), 14900, "dedicated directory", "qr", testIdem); err != nil {
		t.Fatalf("create in new dedicated directory failed: %v", err)
	}
	privateInfo, err := os.Stat(journalDir)
	if err != nil {
		t.Fatal(err)
	}
	sharedInfo, err := os.Stat(shared)
	if err != nil {
		t.Fatal(err)
	}
	if privateInfo.Mode().Perm() != 0700 || sharedInfo.Mode().Perm() != 0755 || posts.Load() != 1 {
		t.Fatalf("private mode=%o shared mode=%o POSTs=%d", privateInfo.Mode().Perm(), sharedInfo.Mode().Perm(), posts.Load())
	}
}

func TestExistingSharedJournalDirectoryPermissionsArePreservedAndBlockCreate(t *testing.T) {
	root := t.TempDir()
	shared := filepath.Join(root, "shared")
	if err := os.Mkdir(shared, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(shared, 0755); err != nil {
		t.Fatal(err)
	}
	var posts atomic.Int32
	rt := fakeTransport(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posts.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"AB12CD34EF","qrString":"{\"TYP\":1,\"IDC\":\"AB12CD34EF\"}"}`)
	}))
	journalPath := filepath.Join(shared, "charges.jsonl")
	svc, err := New(Config{Mode: "LIVE", MaxAmount: 14900, APIKey: testKey, APIKeyID: "acct-test-42", BankAccountID: "bank-test", PublicOrigin: "https://merchant.example", JournalPath: journalPath, Transport: rt})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateCharge(context.Background(), 14900, "shared directory", "qr", testIdem); !errors.Is(err, ErrJournalUnavailable) {
		t.Fatalf("create error=%v; want fail closed before provider POST", err)
	}
	info, err := os.Stat(shared)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0755 {
		t.Fatalf("pre-existing shared directory permissions changed: got %o want 755", info.Mode().Perm())
	}
	if posts.Load() != 0 {
		t.Fatalf("unsafe journal directory allowed %d provider POSTs", posts.Load())
	}
	for _, path := range []string{journalPath, journalPath + ".lock"} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("unsafe journal directory created %s: %v", filepath.Base(path), err)
		}
	}
}

func TestNoProviderPOSTWhenConfigurationOrInputInvalid(t *testing.T) {
	cfg := testConfig(t, "LIVE", filepath.Join(t.TempDir(), "journal.jsonl"), nil)
	cfg.MaxAmount = 0
	if _, err := New(cfg); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("zero cap err=%v", err)
	}
	var posts atomic.Int32
	cfg = testConfig(t, "LIVE", filepath.Join(t.TempDir(), "journal.jsonl"), fakeTransport(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posts.Add(1)
		}
		http.Error(w, "synthetic provider stub", http.StatusInternalServerError)
	})))
	svc, _ := New(cfg)
	for _, tc := range []struct {
		amount               int64
		concept, method, key string
	}{{0, "x", "qr", testIdem}, {14901, "x", "qr", testIdem}, {1, "x", "QR", testIdem}, {1, " x ", "qr", testIdem}, {1, "x", "qr", "bad"}} {
		if _, err := svc.CreateCharge(context.Background(), tc.amount, tc.concept, tc.method, tc.key); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("accepted %+v, err=%v", tc, err)
		}
	}
	if posts.Load() != 0 {
		t.Fatalf("invalid input made %d provider POSTs", posts.Load())
	}
}

func TestConcurrentSameKeyHasAtMostOnePOST(t *testing.T) {
	var posts atomic.Int32
	rt := fakeTransport(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts.Add(1)
		time.Sleep(40 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"AB12CD34EF","qrString":"{\"TYP\":1,\"IDC\":\"AB12CD34EF\"}"}`)
	}))
	svc, _ := New(testConfig(t, "LIVE", filepath.Join(t.TempDir(), "journal.jsonl"), rt))
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := svc.CreateCharge(context.Background(), 14900, "same", "qr", testIdem)
			if err != nil && !errors.Is(err, ErrUncertain) {
				t.Errorf("concurrent error=%v", err)
			}
		}()
	}
	wg.Wait()
	if posts.Load() != 1 {
		t.Fatalf("concurrent POSTs=%d", posts.Load())
	}
}
