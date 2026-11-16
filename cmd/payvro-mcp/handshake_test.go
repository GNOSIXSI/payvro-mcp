package main_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const handshakeToken = "pat-handshake-DO-NOT-LEAK"

func TestStdioHandshake(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/merchant/orders/ord_123" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer "+handshakeToken {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"orderId":"ord_123","status":"PENDING","mode":"DEMO","amountCentavos":14900,"reasonCategory":"NONE"}`)
	}))
	defer srv.Close()

	bin := filepath.Join(t.TempDir(), "payvro-mcp")
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Dir = "."
	build.Env = os.Environ()
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}

	cmd := exec.Command(bin)
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
		"PAYVRO_API_BASE_URL=" + srv.URL,
		"PAYVRO_MERCHANT_TOKEN=" + handshakeToken,
		"PAYVRO_MODE=DEMO",
	}
	var stderr synchronizedBuffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waited := false
	t.Cleanup(func() {
		if waited || cmd.Process == nil {
			return
		}
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})

	captured := &capturingReader{r: stdout}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	client := mcp.NewClient(&mcp.Implementation{Name: "handshake", Version: "0"}, nil)
	session, err := client.Connect(ctx, &mcp.IOTransport{Reader: captured, Writer: stdin}, nil)
	if err != nil {
		t.Fatalf("initialize: %v\nstderr: %s", err, stderr.String())
	}
	defer session.Close()

	listed, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed.Tools) != 3 {
		t.Fatalf("tools = %d", len(listed.Tools))
	}
	byName := map[string]*mcp.Tool{}
	for _, tool := range listed.Tools {
		byName[tool.Name] = tool
	}
	for _, name := range []string{"create_offer", "create_checkout", "get_order_status"} {
		if byName[name] == nil {
			t.Fatalf("missing %s in %#v", name, byName)
		}
	}
	assertProps(t, byName["create_offer"].InputSchema, "approvedProductId", "approvalId", "idempotencyKey")
	assertProps(t, byName["create_checkout"].InputSchema, "offerId", "idempotencyKey")
	assertProps(t, byName["get_order_status"].InputSchema, "orderId")
	if byName["create_offer"].Annotations == nil || byName["create_offer"].Annotations.ReadOnlyHint || byName["create_offer"].Annotations.DestructiveHint == nil || *byName["create_offer"].Annotations.DestructiveHint {
		t.Fatalf("create_offer annotations = %+v", byName["create_offer"].Annotations)
	}
	if byName["create_checkout"].Annotations == nil || !byName["create_checkout"].Annotations.IdempotentHint {
		t.Fatalf("create_checkout annotations = %+v", byName["create_checkout"].Annotations)
	}
	if byName["get_order_status"].Annotations == nil || !byName["get_order_status"].Annotations.ReadOnlyHint {
		t.Fatalf("get_order_status annotations = %+v", byName["get_order_status"].Annotations)
	}

	res, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name:      "get_order_status",
		Arguments: map[string]any{"orderId": "ord_123"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("tool error: %s", texts(res))
	}
	blob, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var status map[string]any
	if err := json.Unmarshal(blob, &status); err != nil {
		t.Fatal(err)
	}
	if status["status"] != "PENDING" || status["orderId"] != "ord_123" || status["mode"] != "DEMO" {
		t.Fatalf("status = %#v", status)
	}
	if strings.Contains(string(blob), handshakeToken) || strings.Contains(texts(res), handshakeToken) {
		t.Fatal("token leaked in tool result")
	}

	_ = session.Close()
	_ = stdin.Close()
	waitErr := cmd.Wait()
	waited = true
	if waitErr != nil {
		t.Fatalf("process exit: %v\nstderr: %s", waitErr, stderr.String())
	}

	lines := captured.lines()
	if len(lines) == 0 {
		t.Fatal("no stdout frames")
	}
	for _, line := range lines {
		if strings.Contains(line, handshakeToken) {
			t.Fatalf("token on stdout: %s", line)
		}
		if !jsonRPCFrame(line) {
			t.Fatalf("stdout line is not a JSON-RPC frame: %s", line)
		}
	}
	if strings.Contains(stderr.String(), handshakeToken) {
		t.Fatalf("token on stderr: %s", stderr.String())
	}
}

func TestDirectDemoThroughSDKStdioNeverContactsPey(t *testing.T) {
	var proxyRequests atomic.Int32
	providerProxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyRequests.Add(1)
		http.Error(w, "provider traffic forbidden in this test", http.StatusBadGateway)
	}))
	defer providerProxy.Close()
	bin := os.Getenv("PAYVRO_MCP_BINARY")
	if bin == "" {
		bin = filepath.Join(t.TempDir(), "payvro-mcp")
		build := exec.Command("go", "build", "-o", bin, ".")
		build.Env = os.Environ()
		if out, err := build.CombinedOutput(); err != nil {
			t.Fatalf("go build: %v\n%s", err, out)
		}
	}
	journal := sdkDirectTestJournalPath(t)
	const key = "synthetic-stdio-pey-key"
	cmd := exec.Command(bin)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME"), "PAYVRO_MCP_MODE=DIRECT", "PAYVRO_MODE=DEMO", "PAYVRO_MAX_AMOUNT_CENTAVOS=14900", "PEY_API_KEY=" + key, "PEY_API_KEY_ID=synthetic-account", "PAYVRO_DIRECT_JOURNAL=" + journal, "HTTPS_PROXY=" + providerProxy.URL, "HTTP_PROXY=" + providerProxy.URL, "NO_PROXY="}
	for _, name := range []string{"LOCALAPPDATA", "APPDATA", "SystemRoot", "TEMP", "TMP"} {
		if value := os.Getenv(name); value != "" {
			cmd.Env = append(cmd.Env, name+"="+value)
		}
	}
	var stderr synchronizedBuffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waited := false
	t.Cleanup(func() {
		if !waited && cmd.Process != nil {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
		}
	})
	captured := &capturingReader{r: stdout}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	session, err := mcp.NewClient(&mcp.Implementation{Name: "direct-stdio", Version: "0"}, nil).Connect(ctx, &mcp.IOTransport{Reader: captured, Writer: stdin}, nil)
	if err != nil {
		t.Fatalf("initialize: %v; stderr=%s", err, stderr.String())
	}
	listed, err := session.ListTools(ctx, nil)
	if err != nil || len(listed.Tools) != 2 {
		t.Fatalf("direct tools=%v err=%v", listed, err)
	}
	res, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "create_codi_charge", Arguments: map[string]any{"amountCentavos": 14900, "concept": "stdio demo", "method": "qr", "idempotencyKey": "stdio-direct-idem-0001"}})
	if err != nil || res.IsError {
		t.Fatalf("create result=%v err=%v stderr=%s", res, err, stderr.String())
	}
	blob, _ := json.Marshal(res.StructuredContent)
	if !strings.Contains(string(blob), `"status":"DEMO_ONLY"`) || !strings.Contains(string(blob), "NOT-PAYABLE") {
		t.Fatalf("not explicitly local demo: %s", blob)
	}
	status, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "get_charge_status", Arguments: map[string]any{"idempotencyKey": "stdio-direct-idem-0001"}})
	if err != nil || status.IsError {
		t.Fatalf("status=%v err=%v", status, err)
	}
	statusBlob, _ := json.Marshal(status.StructuredContent)
	if !strings.Contains(string(statusBlob), `"status":"DEMO_ONLY"`) {
		t.Fatalf("status=%s", statusBlob)
	}
	for i, concept := range []string{"Bearer payvro-token-0123456789", "CLABE 012 345 678 901 234 567", "tarjeta 4111-1111-1111-1111", "sk" + "_live_" + "1234567890abcdefghijklmnop", "reference " + key} {
		result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "create_codi_charge", Arguments: map[string]any{
			"amountCentavos": 14900, "concept": concept, "method": "qr", "idempotencyKey": fmt.Sprintf("stdio-secret-%04d", i),
		}})
		if err != nil || !result.IsError {
			t.Fatalf("sensitive concept was not rejected by SDK caller: result=%v err=%v", result, err)
		}
		if strings.Contains(texts(result), concept) || strings.Contains(captured.bufString(), concept) || strings.Contains(stderr.String(), concept) {
			t.Fatalf("sensitive concept leaked through MCP stdio: result=%q stdout=%q stderr=%q", texts(result), captured.bufString(), stderr.String())
		}
	}
	journalBytes, err := os.ReadFile(journal)
	if err != nil {
		t.Fatal(err)
	}
	for _, canary := range []string{"Bearer payvro-token-0123456789", "CLABE 012 345 678 901 234 567", "tarjeta 4111-1111-1111-1111", "sk" + "_live_" + "1234567890abcdefghijklmnop", "reference " + key} {
		if strings.Contains(string(journalBytes), canary) {
			t.Fatalf("rejected concept persisted in journal: %q", canary)
		}
	}
	_ = session.Close()
	_ = stdin.Close()
	waitErr := cmd.Wait()
	waited = true
	if waitErr != nil {
		t.Fatalf("process exit: %v stderr=%s", waitErr, stderr.String())
	}
	if strings.Contains(captured.bufString(), key) || strings.Contains(stderr.String(), key) {
		t.Fatal("direct API key leaked on stdio or stderr")
	}
	if _, err := os.Stat(journal); err != nil {
		t.Fatalf("journal not durable: %v", err)
	}
	if proxyRequests.Load() != 0 {
		t.Fatalf("DEMO SDK process attempted %d provider-proxy requests", proxyRequests.Load())
	}
}

func texts(res *mcp.CallToolResult) string {
	var b strings.Builder
	for _, item := range res.Content {
		if text, ok := item.(*mcp.TextContent); ok {
			b.WriteString(text.Text)
		}
	}
	return b.String()
}

func assertProps(t *testing.T, schema any, names ...string) {
	t.Helper()
	blob, err := json.Marshal(schema)
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]any
	if err := json.Unmarshal(blob, &obj); err != nil {
		t.Fatal(err)
	}
	if obj["type"] != "object" || obj["additionalProperties"] != false {
		t.Fatalf("schema = %s", blob)
	}
	props, _ := obj["properties"].(map[string]any)
	if len(props) != len(names) {
		t.Fatalf("properties = %#v", props)
	}
	for _, name := range names {
		prop, _ := props[name].(map[string]any)
		if prop["type"] != "string" {
			t.Fatalf("%s = %#v", name, prop)
		}
	}
	if _, ok := props["idempotencyKey"]; ok {
		prop := props["idempotencyKey"].(map[string]any)
		if prop["pattern"] != "^[A-Za-z0-9_-]+$" || prop["minLength"] != float64(16) || prop["maxLength"] != float64(128) {
			t.Fatalf("idempotencyKey = %#v", prop)
		}
	}
	if _, ok := props["orderId"]; ok && len(names) == 1 {
		prop := props["orderId"].(map[string]any)
		min, _ := prop["minLength"].(float64)
		if min < 1 {
			t.Fatalf("orderId = %#v", prop)
		}
	}
}

type synchronizedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *synchronizedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *synchronizedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type capturingReader struct {
	r   io.Reader
	mu  sync.Mutex
	buf bytes.Buffer
}

func (c *capturingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if n > 0 {
		c.mu.Lock()
		c.buf.Write(p[:n])
		c.mu.Unlock()
	}
	return n, err
}

func (c *capturingReader) Close() error { return nil }

func (c *capturingReader) lines() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	raw := strings.Split(c.buf.String(), "\n")
	var lines []string
	for _, line := range raw {
		if strings.TrimSpace(line) == "" {
			continue
		}
		lines = append(lines, line)
	}
	return lines
}

func jsonRPCFrame(line string) bool {
	if strings.TrimSpace(line) == "" {
		return false
	}
	var msg map[string]json.RawMessage
	if err := json.Unmarshal([]byte(line), &msg); err != nil {
		return false
	}
	var version string
	if err := json.Unmarshal(msg["jsonrpc"], &version); err != nil || version != "2.0" {
		return false
	}
	_, hasMethod := msg["method"]
	_, hasResult := msg["result"]
	_, hasError := msg["error"]
	_, hasID := msg["id"]
	switch {
	case hasResult || hasError:
		return hasID
	case hasMethod:
		return true
	default:
		return false
	}
}
