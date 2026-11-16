package redact

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"sort"
	"sync"
)

// marker is the only replacement text. It is not a credential.
var marker = []byte("[redacted]")

// escapeLevels is how many times a PAT can be JSON-escaped before it is
// written. The SDK prints the raw params with %q and then JSON-encodes the
// JSON-RPC error, so the bytes on stdout are several escapes deep.
const escapeLevels = 8

// Writer redacts configured secrets, and each JSON-escaped form of those
// secrets, from the bytes it writes. A secret split across Write calls stays
// in an internal tail until the next write completes it or Flush emits it.
type Writer struct {
	dst     io.Writer
	secrets [][]byte
	hold    []byte
	mu      sync.Mutex
}

func NewWriter(dst io.Writer, secrets ...string) *Writer {
	return &Writer{dst: dst, secrets: secretForms(secrets)}
}

// ActiveEnvSecrets returns only the values of known secret environment variables
// present in this process. Callers must never log or persist this slice.
func ActiveEnvSecrets() []string {
	keys := []string{
		"PAYVRO_MERCHANT_TOKEN", "PAYVRO_BFF_SHARED_SECRET", "PEY_API_KEY", "PEY_WEBHOOK_SECRET",
		"PEY_WEBHOOK_SECRET_PREVIOUS", "PAYVRO_DEMO_WEBHOOK_SECRET",
		"PAYVRO_GUEST_TOKEN_PEPPER", "PAYVRO_TOKEN_ENCRYPTION_KEY",
		"PAYVRO_AUDIT_HASH_KEY", "CONVEX_DEPLOY_KEY", "CLERK_SECRET_KEY",
	}
	var secrets []string
	for _, key := range keys {
		if value := os.Getenv(key); value != "" {
			secrets = append(secrets, value)
		}
	}
	return secrets
}

// RedactString scrubs direct and nested JSON-escaped forms of configured secrets.
func RedactString(value string, secrets ...string) string {
	forms := secretForms(secrets)
	return string(redactBytes([]byte(value), forms))
}

func (w *Writer) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.secrets) == 0 {
		return w.dst.Write(p)
	}
	data := make([]byte, 0, len(w.hold)+len(p))
	data = append(data, w.hold...)
	data = append(data, p...)
	w.hold = nil
	emit, hold := consume(data, w.secrets)
	if len(hold) > 0 {
		w.hold = hold
	}
	if len(emit) == 0 {
		return len(p), nil
	}
	if _, err := w.dst.Write(emit); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (w *Writer) Flush() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.hold) == 0 {
		return nil
	}
	_, err := w.dst.Write(w.hold)
	w.hold = nil
	return err
}

func secretForms(secrets []string) [][]byte {
	seen := map[string]struct{}{}
	var forms []string
	for _, secret := range secrets {
		cur := secret
		for level := 0; level < escapeLevels && cur != ""; level++ {
			if _, ok := seen[cur]; ok {
				break
			}
			seen[cur] = struct{}{}
			forms = append(forms, cur)
			next, ok := jsonEscaped(cur)
			if !ok || next == cur {
				break
			}
			cur = next
		}
	}
	// Longest first so a nested escape is removed whole.
	sort.Slice(forms, func(i, j int) bool { return len(forms[i]) > len(forms[j]) })
	out := make([][]byte, len(forms))
	for i, form := range forms {
		out[i] = []byte(form)
	}
	return out
}

func jsonEscaped(s string) (string, bool) {
	blob, err := json.Marshal(s)
	if err != nil || len(blob) < 2 {
		return "", false
	}
	return string(blob[1 : len(blob)-1]), true
}

func consume(data []byte, secrets [][]byte) (emit, hold []byte) {
	// Replace complete secrets before measuring the tail. Measuring first
	// would keep the last len(secret)-1 bytes of a secret that is already
	// whole, and the next write would emit them.
	redacted := redactBytes(append([]byte(nil), data...), secrets)
	n := holdLen(redacted, secrets)
	if n == 0 {
		return redacted, nil
	}
	return redacted[:len(redacted)-n], append([]byte(nil), redacted[len(redacted)-n:]...)
}

func redactBytes(data []byte, secrets [][]byte) []byte {
	for _, secret := range secrets {
		if len(secret) == 0 {
			continue
		}
		data = bytes.ReplaceAll(data, secret, marker)
	}
	return data
}

func holdLen(data []byte, secrets [][]byte) int {
	n := longestProperPrefix(data, secrets)
	if n == 0 || overlapsMarker(data, n) {
		return 0
	}
	return n
}

func longestProperPrefix(data []byte, secrets [][]byte) int {
	best := 0
	for _, secret := range secrets {
		max := len(secret) - 1
		if max > len(data) {
			max = len(data)
		}
		for n := max; n > best; n-- {
			if bytes.HasPrefix(secret, data[len(data)-n:]) {
				best = n
				break
			}
		}
	}
	return best
}

func overlapsMarker(data []byte, n int) bool {
	idx := bytes.LastIndex(data, marker)
	if idx < 0 {
		return false
	}
	return len(data)-n < idx+len(marker)
}
