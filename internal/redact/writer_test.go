package redact

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestWriterRedactsTokenSplitAcrossWrites(t *testing.T) {
	const token = "pv5canary\"PAT\\9f3e2a7b1"
	var buf bytes.Buffer
	w := NewWriter(&buf, token)
	mid := len(token) / 2
	if _, err := w.Write([]byte("pre-" + token[:mid])); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte(token[mid:] + "-post\n")); err != nil {
		t.Fatal(err)
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	got := buf.String()
	if strings.Contains(got, token) || strings.Contains(got, "pv5canary") {
		t.Fatalf("canary leaked across a split write: %q", got)
	}
	if !strings.Contains(got, "pre-[redacted]-post\n") {
		t.Fatalf("split write dropped the surrounding text: %q", got)
	}
}

func TestWriterRedactsJSONEscapedForm(t *testing.T) {
	const token = "pv5canary\"PAT\\9f3e2a7b1"
	quoted, err := json.Marshal(token)
	if err != nil {
		t.Fatal(err)
	}
	escaped := string(quoted[1 : len(quoted)-1])
	if escaped == token {
		t.Fatal("fixture did not need JSON escaping")
	}
	var buf bytes.Buffer
	w := NewWriter(&buf, token)
	mid := len(escaped) / 2
	if _, err := w.Write([]byte("left " + escaped[:mid])); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte(escaped[mid:] + " right")); err != nil {
		t.Fatal(err)
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	got := buf.String()
	if strings.Contains(got, escaped) || strings.Contains(got, "pv5canary") {
		t.Fatalf("JSON-escaped canary leaked: %q", got)
	}
	if !strings.Contains(got, "left [redacted] right") {
		t.Fatalf("escaped form dropped the surrounding text: %q", got)
	}
}

func TestWriterRedactsNestedJSONEscapesSplit(t *testing.T) {
	const token = "pv5canary\"PAT\\9f3e2a7b1"
	nested := token
	for level := 0; level < 3; level++ {
		quoted, err := json.Marshal(nested)
		if err != nil {
			t.Fatal(err)
		}
		nested = string(quoted[1 : len(quoted)-1])
	}
	if nested == token || !strings.Contains(nested, "pv5canary") {
		t.Fatalf("nested form = %q", nested)
	}
	var buf bytes.Buffer
	w := NewWriter(&buf, token)
	mid := len(nested) / 2
	if _, err := w.Write([]byte(nested[:mid])); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte(nested[mid:])); err != nil {
		t.Fatal(err)
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "pv5canary") || buf.String() != "[redacted]" {
		t.Fatalf("nested escape leaked: %q", buf.String())
	}
}

func TestWriterRedactsByteAtATime(t *testing.T) {
	const token = "redact-canary-alpha"
	var buf bytes.Buffer
	w := NewWriter(&buf, token)
	for _, b := range []byte("<<" + token + ">>") {
		if _, err := w.Write([]byte{b}); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	if buf.String() != "<<[redacted]>>" {
		t.Fatalf("byte-at-a-time leak: %q", buf.String())
	}
}

func TestWriterRedactsActivePeyKeyFromEnvironmentEscapedAndFragmented(t *testing.T) {
	const key = `synthetic-pey\"key\\fragment`
	t.Setenv("PEY_API_KEY", key)
	t.Setenv("PAYVRO_MERCHANT_TOKEN", "")
	var buf bytes.Buffer
	w := NewWriter(&buf, ActiveEnvSecrets()...)
	quoted, err := json.Marshal(key)
	if err != nil {
		t.Fatal(err)
	}
	escaped := string(quoted[1 : len(quoted)-1])
	mid := len(escaped) / 2
	_, _ = w.Write([]byte(`{"error":"` + escaped[:mid]))
	_, _ = w.Write([]byte(escaped[mid:] + `"}`))
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "synthetic-pey") || strings.Contains(buf.String(), key) {
		t.Fatalf("pey key leaked: %q", buf.String())
	}
	if !strings.Contains(buf.String(), "[redacted]") {
		t.Fatalf("redaction missing: %q", buf.String())
	}
}

func TestWriterRedactsInheritedBFFSecretEscapedAndFragmented(t *testing.T) {
	const secret = `synthetic-bff"shared\\secret`
	t.Setenv("PAYVRO_BFF_SHARED_SECRET", secret)
	t.Setenv("PAYVRO_MERCHANT_TOKEN", "")
	var buf bytes.Buffer
	w := NewWriter(&buf, ActiveEnvSecrets()...)
	quoted, err := json.Marshal(secret)
	if err != nil {
		t.Fatal(err)
	}
	fragment := string(quoted[1 : len(quoted)-1])
	for level := 0; level < 3; level++ {
		blob, err := json.Marshal(fragment)
		if err != nil {
			t.Fatal(err)
		}
		fragment = string(blob[1 : len(blob)-1])
	}
	mid := len(fragment) / 2
	_, _ = w.Write([]byte("before:" + fragment[:mid]))
	_, _ = w.Write([]byte(fragment[mid:] + ":after"))
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "synthetic-bff") || !strings.Contains(buf.String(), "[redacted]") {
		t.Fatalf("inherited BFF secret escaped redaction: %q", buf.String())
	}
}

func TestWriterKeepsNearMiss(t *testing.T) {
	const token = "redact-canary-alpha"
	var buf bytes.Buffer
	w := NewWriter(&buf, token)
	if _, err := w.Write([]byte("redact-canar")); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte("Y-and-ord_1\n")); err != nil {
		t.Fatal(err)
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	got := buf.String()
	if got != "redact-canarY-and-ord_1\n" {
		t.Fatalf("near-miss was rewritten: %q", got)
	}
}
