package tools_test

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// This was run and observed red against the store-only implementation before
// direct mode was built (commit 2ff93d2).
func TestDirectModeExposesOnlyDirectToolsAndRequiresAmountCap(t *testing.T) {
	setDirectEnv(t, "LIVE", "")
	session := connect(t, io.Discard)
	listed, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	byName := map[string]*mcp.Tool{}
	for _, tool := range listed.Tools {
		byName[tool.Name] = tool
	}
	if len(byName) != 2 || byName["create_codi_charge"] == nil || byName["get_charge_status"] == nil {
		t.Fatalf("DIRECT mode tool surface = %v; want only direct tools", toolNames(byName))
	}
	create := byName["create_codi_charge"]
	if create.Annotations == nil || create.Annotations.ReadOnlyHint {
		t.Fatalf("charge must be marked as write: %+v", create.Annotations)
	}
	assertDirectChargeSchema(t, create.InputSchema)

	// A valid integer amount cannot reach the provider or journal if the mandatory cap is absent.
	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "create_codi_charge", Arguments: map[string]any{
		"amountCentavos": 100, "concept": "test", "method": "qr", "idempotencyKey": goodKey,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(resultText(res), "PAYVRO_MAX_AMOUNT_CENTAVOS") {
		t.Fatalf("missing cap result: %+v", res)
	}
	if _, err := os.Stat(os.Getenv("PAYVRO_DIRECT_JOURNAL")); !os.IsNotExist(err) {
		t.Fatalf("missing cap touched journal: stat err=%v", err)
	}

	// A fractional centavo is rejected at the actual MCP tool caller boundary.
	res, err = session.CallTool(context.Background(), &mcp.CallToolParams{Name: "create_codi_charge", Arguments: map[string]any{
		"amountCentavos": 1.5, "concept": "test", "method": "qr", "idempotencyKey": goodKey,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatal("fractional centavos reached direct charge handler")
	}
}

func TestDirectDemoSDKJourneyAndAmbiguousConfigFailsClosed(t *testing.T) {
	setDirectEnv(t, "DEMO", "14900")
	session := connect(t, io.Discard)
	listed, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed.Tools) != 2 {
		t.Fatalf("direct tools=%v", listed.Tools)
	}
	args := map[string]any{"amountCentavos": 14900, "concept": "Producto de prueba", "method": "link", "idempotencyKey": goodKey}
	first := call(t, session, "create_codi_charge", args)
	if first["status"] != "DEMO_ONLY" || first["mode"] != "DEMO" || !strings.Contains(strings.ToLower(first["message"].(string)), "no se contactó") {
		t.Fatalf("demo create=%#v", first)
	}
	replay := call(t, session, "create_codi_charge", args)
	if replay["chargeId"] != first["chargeId"] {
		t.Fatalf("replay changed resource: %#v %#v", first, replay)
	}
	status := call(t, session, "get_charge_status", map[string]any{"idempotencyKey": goodKey})
	if status["status"] != "DEMO_ONLY" {
		t.Fatalf("demo status=%#v", status)
	}
	if _, text, isErr := callRaw(t, session, "create_codi_charge", map[string]any{"amountCentavos": 14800, "concept": "otro", "method": "link", "idempotencyKey": goodKey}); !isErr || !strings.Contains(text, "different intent") {
		t.Fatalf("different intent err=%v text=%s", isErr, text)
	}
	if _, _, isErr := callRaw(t, session, "create_codi_charge", map[string]any{"amountCentavos": 100, "concept": "x", "method": "qr", "idempotencyKey": goodKey, "destination": "https://evil.example"}); !isErr {
		t.Fatal("tool accepted caller-selected destination")
	}
	_ = session.Close()
	restarted := connect(t, io.Discard)
	restartedReplay := call(t, restarted, "create_codi_charge", args)
	if restartedReplay["chargeId"] != first["chargeId"] {
		t.Fatalf("server restart replay changed resource: %#v want %v", restartedReplay, first["chargeId"])
	}
	restartedStatus := call(t, restarted, "get_charge_status", map[string]any{"idempotencyKey": goodKey})
	if restartedStatus["status"] != "DEMO_ONLY" {
		t.Fatalf("restarted status=%#v", restartedStatus)
	}

	t.Setenv("PAYVRO_API_BASE_URL", "https://merchant.example")
	t.Setenv("PAYVRO_MERCHANT_TOKEN", testToken)
	ambiguous := connect(t, io.Discard)
	got, err := ambiguous.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Tools) != 0 {
		t.Fatalf("mixed STORE+DIRECT exposed tools: %v", toolNames(func() map[string]*mcp.Tool {
			m := map[string]*mcp.Tool{}
			for _, v := range got.Tools {
				m[v.Name] = v
			}
			return m
		}()))
	}
}

func TestDirectSDKRejectsInheritedBFFSecretBeforeJournal(t *testing.T) {
	const secret = "synthetic-inherited-bff-secret"
	setDirectEnv(t, "DEMO", "14900")
	t.Setenv("PAYVRO_BFF_SHARED_SECRET", secret)
	session := connect(t, io.Discard)
	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "create_codi_charge", Arguments: map[string]any{
		"amountCentavos": 100, "concept": secret, "method": "qr", "idempotencyKey": goodKey,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatalf("MCP caller accepted inherited secret concept: %+v", res)
	}
	if strings.Contains(resultText(res), secret) {
		t.Fatalf("inherited secret echoed in SDK result: %s", resultText(res))
	}
	if _, err := os.Stat(os.Getenv("PAYVRO_DIRECT_JOURNAL")); !os.IsNotExist(err) {
		t.Fatalf("MCP caller persisted inherited secret concept: stat err=%v", err)
	}
}

func setDirectEnv(t *testing.T, mode, cap string) {
	t.Helper()
	t.Setenv("PAYVRO_MCP_MODE", "DIRECT")
	t.Setenv("PAYVRO_MODE", mode)
	t.Setenv("PAYVRO_MAX_AMOUNT_CENTAVOS", cap)
	t.Setenv("PEY_API_KEY", "synthetic-direct-key")
	t.Setenv("PEY_API_KEY_ID", "synthetic-account")
	t.Setenv("PEY_BANK_ACCOUNT_ID", "")
	t.Setenv("PAYVRO_PUBLIC_ORIGIN", "")
	t.Setenv("PAYVRO_DIRECT_JOURNAL", directTestJournalPath(t))
	t.Setenv("PAYVRO_API_BASE_URL", "")
	t.Setenv("PAYVRO_MERCHANT_TOKEN", "")
}

func assertDirectChargeSchema(t *testing.T, schema any) {
	t.Helper()
	obj := schemaObject(t, schema)
	if obj["type"] != "object" || obj["additionalProperties"] != false {
		t.Fatalf("create schema = %#v", obj)
	}
	props, _ := obj["properties"].(map[string]any)
	if len(props) != 4 {
		t.Fatalf("create properties = %#v", props)
	}
	if props["amountCentavos"].(map[string]any)["type"] != "integer" {
		t.Fatalf("amount schema = %#v", props["amountCentavos"])
	}
	if props["method"].(map[string]any)["enum"] == nil {
		t.Fatalf("method schema = %#v", props["method"])
	}
}

func toolNames(byName map[string]*mcp.Tool) []string {
	out := make([]string, 0, len(byName))
	for name := range byName {
		out = append(out, name)
	}
	return out
}
