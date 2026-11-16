package tools_test

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestGetChargeStatusRejectsCallerControlledFieldsBeforeJournal(t *testing.T) {
	setDirectEnv(t, "DEMO", "14900")
	journal := os.Getenv("PAYVRO_DIRECT_JOURNAL")
	session := connect(t, io.Discard)
	listed, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var statusTool *mcp.Tool
	for _, tool := range listed.Tools {
		if tool.Name == "get_charge_status" {
			statusTool = tool
		}
	}
	if statusTool == nil {
		t.Fatal("get_charge_status missing from DIRECT MCP surface")
	}
	assertObjectSchema(t, statusTool.InputSchema, []string{"idempotencyKey"})

	created := call(t, session, "create_codi_charge", map[string]any{
		"amountCentavos": 100, "concept": "Curso corto", "method": "qr", "idempotencyKey": goodKey,
	})
	if created["status"] != "DEMO_ONLY" {
		t.Fatalf("qr create=%#v", created)
	}

	before, err := os.ReadFile(journal)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"apiKey", "account", "host", "mode", "status", "destination", "peyHost", "bankAccountId"} {
		args := map[string]any{"idempotencyKey": goodKey, field: "attacker-controlled"}
		if _, text, isErr := callRaw(t, session, "get_charge_status", args); !isErr {
			t.Fatalf("get_charge_status accepted extra field %q: %s", field, text)
		}
	}
	after, err := os.ReadFile(journal)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatalf("rejected extra status args mutated journal")
	}
}

func TestDirectSelectorFailsClosedWithoutExplicitModeOrStorePlusPeyKey(t *testing.T) {
	t.Run("direct credentials without PAYVRO_MCP_MODE expose no tools", func(t *testing.T) {
		setDirectEnv(t, "DEMO", "14900")
		t.Setenv("PAYVRO_MCP_MODE", "")
		session := connect(t, io.Discard)
		listed, err := session.ListTools(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(listed.Tools) != 0 {
			t.Fatalf("implicit DIRECT selected tools=%v", listed.Tools)
		}
	})
	t.Run("STORE mode plus pey key exposes no tools", func(t *testing.T) {
		t.Setenv("PAYVRO_MCP_MODE", "STORE")
		t.Setenv("PAYVRO_API_BASE_URL", "https://merchant.example")
		t.Setenv("PAYVRO_MERCHANT_TOKEN", testToken)
		t.Setenv("PAYVRO_MODE", "DEMO")
		t.Setenv("PEY_API_KEY", "synthetic-direct-key")
		t.Setenv("PEY_API_KEY_ID", "synthetic-account")
		t.Setenv("PAYVRO_MAX_AMOUNT_CENTAVOS", "14900")
		session := connect(t, io.Discard)
		listed, err := session.ListTools(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(listed.Tools) != 0 {
			t.Fatalf("STORE+PEY_API_KEY selected tools=%v", listed.Tools)
		}
	})
}

func TestMissingCapFailsDirectHandlersWithoutBlockingToolListing(t *testing.T) {
	setDirectEnv(t, "DEMO", "")
	session := connect(t, io.Discard)
	listed, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed.Tools) != 2 {
		t.Fatalf("missing cap should still list DIRECT tools, got %v", listed.Tools)
	}
	create, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "create_codi_charge", Arguments: map[string]any{
		"amountCentavos": 100, "concept": "Curso corto", "method": "link", "idempotencyKey": goodKey,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !create.IsError || !strings.Contains(resultText(create), "PAYVRO_MAX_AMOUNT_CENTAVOS") {
		t.Fatalf("missing cap create=%+v", create)
	}
	status, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "get_charge_status", Arguments: map[string]any{
		"idempotencyKey": goodKey,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !status.IsError || !strings.Contains(resultText(status), "PAYVRO_MAX_AMOUNT_CENTAVOS") {
		t.Fatalf("missing cap status=%+v", status)
	}
	if _, err := os.Stat(os.Getenv("PAYVRO_DIRECT_JOURNAL")); !os.IsNotExist(err) {
		t.Fatalf("missing cap touched journal: %v", err)
	}
}

func TestCreateAndStatusRejectZeroAmountHostModeAndUnknownStatusKey(t *testing.T) {
	setDirectEnv(t, "DEMO", "14900")
	journal := os.Getenv("PAYVRO_DIRECT_JOURNAL")
	session := connect(t, io.Discard)

	for _, amount := range []any{0, -1, "14900"} {
		res, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "create_codi_charge", Arguments: map[string]any{
			"amountCentavos": amount, "concept": "Curso corto", "method": "qr", "idempotencyKey": goodKey,
		}})
		if err != nil {
			t.Fatal(err)
		}
		if !res.IsError {
			t.Fatalf("invalid amount %v accepted: %+v", amount, res)
		}
	}
	for _, field := range []string{"host", "mode", "status", "account"} {
		args := map[string]any{"amountCentavos": 100, "concept": "Curso corto", "method": "link", "idempotencyKey": goodKey, field: "attacker-controlled"}
		if _, text, isErr := callRaw(t, session, "create_codi_charge", args); !isErr {
			t.Fatalf("link create accepted extra field %q: %s", field, text)
		}
	}
	if _, err := os.Stat(journal); !os.IsNotExist(err) {
		t.Fatalf("rejected creates persisted journal: %v", err)
	}

	unknownKey := "unknown-status-key-01"
	if _, text, isErr := callRaw(t, session, "get_charge_status", map[string]any{"idempotencyKey": unknownKey}); !isErr {
		t.Fatalf("unknown status key succeeded: %s", text)
	}
	if _, err := os.Stat(journal); !os.IsNotExist(err) {
		t.Fatalf("unknown status read created journal: %v", err)
	}
}

func TestSensitiveConceptRejectedOnLinkBeforeJournal(t *testing.T) {
	setDirectEnv(t, "DEMO", "14900")
	journal := os.Getenv("PAYVRO_DIRECT_JOURNAL")
	session := connect(t, io.Discard)
	concepts := []string{
		"Bearer payvro-token-0123456789",
		"CLABE 012 345 678 901 234 567",
		"tarjeta 4111-1111-1111-1111",
	}
	for i, concept := range concepts {
		if _, text, isErr := callRaw(t, session, "create_codi_charge", map[string]any{
			"amountCentavos": 100, "concept": concept, "method": "link", "idempotencyKey": fmt.Sprintf("link-sensitive-%04d", i),
		}); !isErr {
			t.Fatalf("link accepted sensitive concept %q: %s", concept, text)
		}
	}
	if _, err := os.Stat(journal); !os.IsNotExist(err) {
		t.Fatalf("link sensitive concepts created journal: %v", err)
	}
	safe := call(t, session, "create_codi_charge", map[string]any{
		"amountCentavos": 100, "concept": "Curso de 12 lecciones", "method": "link", "idempotencyKey": "link-safe-intent-0001",
	})
	if safe["status"] != "DEMO_ONLY" || safe["mode"] != "DEMO" {
		t.Fatalf("safe link create=%#v", safe)
	}
	status := call(t, session, "get_charge_status", map[string]any{"idempotencyKey": "link-safe-intent-0001"})
	if status["status"] != "DEMO_ONLY" {
		t.Fatalf("safe link status=%#v", status)
	}
}
