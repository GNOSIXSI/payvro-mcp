package main_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const outputCanary = "pv5canaryPAT9f3e2a7b1"

func TestSDKParamsRejectedBeforeMiddlewareDoNotLeakPAT(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "payvro-mcp")
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Env = os.Environ()
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}

	const plain = "pv5canaryPAT9f3e2a7b1"
	const escaped = "pv5canary\"PAT\\9f3e2a7b1"
	offerArgs := map[string]any{
		"approvedProductId": "prod_1",
		"approvalId":        "appr_1",
		"idempotencyKey":    "idem-fixture-cccc-dddd",
	}

	t.Run("meta string", func(t *testing.T) {
		base, hits := rejectServer(t)
		stdout, stderr := rawExchange(t, bin, base, plain, toolCallLine(6, map[string]any{
			"name":      "create_offer",
			"_meta":     plain,
			"arguments": offerArgs,
		}))
		if hits() != 0 {
			t.Fatalf("rejected params still called HTTP %d times", hits())
		}
		assertInvalidParamsNoCanary(t, stdout, stderr, plain)
	})
	// segmentio/json drops unknown fields, so the canary has to sit in an
	// object that already fails to unmarshal. Otherwise the call reaches the
	// handler and the value is discarded without ever being written.
	t.Run("unknown top-level param", func(t *testing.T) {
		base, hits := rejectServer(t)
		stdout, stderr := rawExchange(t, bin, base, plain, toolCallLine(7, map[string]any{
			"name":      1,
			"injected":  plain,
			"arguments": offerArgs,
		}))
		if hits() != 0 {
			t.Fatalf("rejected params still called HTTP %d times", hits())
		}
		assertInvalidParamsNoCanary(t, stdout, stderr, plain)
	})
	t.Run("json string escapes", func(t *testing.T) {
		base, hits := rejectServer(t)
		params, err := json.Marshal(escaped)
		if err != nil {
			t.Fatal(err)
		}
		line := `{"jsonrpc":"2.0","id":8,"method":"tools/call","params":` + string(params) + `}`
		stdout, stderr := rawExchange(t, bin, base, escaped, line)
		if hits() != 0 {
			t.Fatalf("rejected params still called HTTP %d times", hits())
		}
		assertInvalidParamsNoCanary(t, stdout, stderr, "pv5canary")
		if strings.Contains(stdout, escaped) || strings.Contains(stderr, escaped) {
			t.Fatalf("raw canary survived JSON escaping\nstdout: %s\nstderr: %s", stdout, stderr)
		}
	})
}

func rejectServer(t *testing.T) (string, func() int) {
	t.Helper()
	var mu sync.Mutex
	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		n++
		mu.Unlock()
		http.Error(w, "unreachable", http.StatusBadGateway)
	}))
	t.Cleanup(srv.Close)
	return srv.URL, func() int {
		mu.Lock()
		defer mu.Unlock()
		return n
	}
}

func TestPATNeverOnStdoutOrStderr(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "payvro-mcp")
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Env = os.Environ()
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"orderId":"ord_1","status":"`+outputCanary+`","mode":"DEMO","amountCentavos":14900,"reasonCategory":"NONE"}`)
	}))
	t.Cleanup(srv.Close)

	t.Run("status body", func(t *testing.T) {
		stdout, stderr := callBinary(t, bin, srv.URL, "get_order_status", map[string]any{"orderId": "ord_1"})
		assertNoCanary(t, stdout, stderr)
		if !strings.Contains(stdout, "ord_1") {
			t.Fatalf("stdout dropped the order id: %s", stdout)
		}
	})
	t.Run("idempotency key", func(t *testing.T) {
		stdout, stderr := callBinary(t, bin, srv.URL, "create_offer", map[string]any{
			"approvedProductId": "prod_1",
			"approvalId":        "appr_1",
			"idempotencyKey":    outputCanary + "!",
		})
		assertNoCanary(t, stdout, stderr)
		if !strings.Contains(stdout, `"isError":true`) {
			t.Fatalf("invalid key was not a tool error: %s", stdout)
		}
	})
	t.Run("unknown field", func(t *testing.T) {
		stdout, stderr := callBinary(t, bin, srv.URL, "create_offer", map[string]any{
			"approvedProductId": "prod_1",
			"approvalId":        "appr_1",
			"idempotencyKey":    "idem-fixture-aaaa-bbbb",
			"injected":          outputCanary,
		})
		assertNoCanary(t, stdout, stderr)
		if !strings.Contains(stdout, `"isError":true`) {
			t.Fatalf("unknown field was not a tool error: %s", stdout)
		}
	})
}

func callBinary(t *testing.T, bin, baseURL, tool string, args map[string]any) (stdout, stderr string) {
	t.Helper()
	cmd := exec.Command(bin)
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
		"PAYVRO_API_BASE_URL=" + baseURL,
		"PAYVRO_MERCHANT_TOKEN=" + outputCanary,
		"PAYVRO_MODE=DEMO",
	}
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf
	outPipe, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	inPipe, err := cmd.StdinPipe()
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

	captured := &capturingReader{r: outPipe}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	client := mcp.NewClient(&mcp.Implementation{Name: "redaction", Version: "0"}, nil)
	session, err := client.Connect(ctx, &mcp.IOTransport{Reader: captured, Writer: inPipe}, nil)
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		waited = true
		t.Fatalf("initialize: %v\nstderr: %s", err, errBuf.String())
	}
	_, _ = session.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
	_ = session.Close()
	_ = inPipe.Close()
	_ = cmd.Wait()
	waited = true
	return captured.bufString(), errBuf.String()
}

func (c *capturingReader) bufString() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

func toolCallLine(id int, params any) string {
	blob, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  "tools/call",
		"params":  params,
	})
	if err != nil {
		panic(err)
	}
	return string(blob)
}

func rawExchange(t *testing.T, bin, baseURL, token string, calls ...string) (stdout, stderr string) {
	t.Helper()
	cmd := exec.Command(bin)
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
		"PAYVRO_API_BASE_URL=" + baseURL,
		"PAYVRO_MERCHANT_TOKEN=" + token,
		"PAYVRO_MODE=DEMO",
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	stderrPipe, err := cmd.StderrPipe()
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

	var stderrBuf bytes.Buffer
	var stderrMu sync.Mutex
	stderrDone := make(chan struct{})
	go func() {
		_, _ = io.Copy(&lockedWriter{mu: &stderrMu, w: &stderrBuf}, stderrPipe)
		close(stderrDone)
	}()

	lines := make(chan string, 32)
	go func() {
		sc := bufio.NewScanner(stdoutPipe)
		sc.Buffer(make([]byte, 64*1024), 2*1024*1024)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()

	initLine := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"redaction","version":"0"}}}`
	note := `{"jsonrpc":"2.0","method":"notifications/initialized"}`
	for _, line := range append([]string{initLine, note}, calls...) {
		if _, err := io.WriteString(stdin, line+"\n"); err != nil {
			t.Fatal(err)
		}
	}

	var collected strings.Builder
	deadline := time.After(15 * time.Second)
	need := len(calls)
	gotCalls := 0
	for gotCalls < need {
		select {
		case line, ok := <-lines:
			if !ok {
				t.Fatalf("stdout closed before the tool response\nstdout: %s\nstderr: %s", collected.String(), stderrBuf.String())
			}
			collected.WriteString(line)
			collected.WriteByte('\n')
			if strings.Contains(line, `"id":`) && !strings.Contains(line, `"id":1`) && !strings.Contains(line, `"id":1,`) {
				gotCalls++
			}
		case <-deadline:
			t.Fatalf("timed out waiting for the tool response\nstdout: %s\nstderr: %s", collected.String(), stderrBuf.String())
		}
	}
	_ = stdin.Close()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		waited = true
		<-stderrDone
		if err != nil {
			stderrMu.Lock()
			errText := stderrBuf.String()
			stderrMu.Unlock()
			t.Fatalf("process exit: %v\nstdout: %s\nstderr: %s", err, collected.String(), errText)
		}
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("process did not exit after stdin closed")
	}
	// Drain frames written before exit.
	drain := time.After(1 * time.Second)
	for {
		select {
		case line, ok := <-lines:
			if !ok {
				goto finished
			}
			collected.WriteString(line)
			collected.WriteByte('\n')
		case <-drain:
			goto finished
		}
	}
finished:
	<-stderrDone
	stderrMu.Lock()
	errText := stderrBuf.String()
	stderrMu.Unlock()
	return collected.String(), errText
}

type lockedWriter struct {
	mu *sync.Mutex
	w  io.Writer
}

func (l lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

func assertInvalidParamsNoCanary(t *testing.T, stdout, stderr, marker string) {
	t.Helper()
	if strings.TrimSpace(stdout) == "" {
		t.Fatal("stdout empty; the invalid-params frame was not observed")
	}
	if !strings.Contains(stdout, "-32602") {
		t.Fatalf("stdout was not an invalid-params error: %s", stdout)
	}
	if !strings.Contains(stdout, "payvro") {
		t.Fatalf("initialize result missing; redaction ate the session: %s", stdout)
	}
	if strings.Contains(stdout, marker) {
		t.Fatalf("canary on stdout: %s", stdout)
	}
	if strings.Contains(stderr, marker) {
		t.Fatalf("canary on stderr: %s", stderr)
	}
	if !strings.Contains(stdout, "[redacted]") {
		t.Fatalf("invalid params were dropped instead of redacted: %s", stdout)
	}
}

func assertNoCanary(t *testing.T, stdout, stderr string) {
	t.Helper()
	if strings.TrimSpace(stdout) == "" {
		t.Fatal("stdout empty; redaction was not observed")
	}
	if strings.Contains(stdout, outputCanary) {
		t.Fatalf("canary on stdout: %s", stdout)
	}
	if strings.Contains(stderr, outputCanary) {
		t.Fatalf("canary on stderr: %s", stderr)
	}
}
