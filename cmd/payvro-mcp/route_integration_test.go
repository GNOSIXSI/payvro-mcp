package main_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The TypeScript integration test provides PAYVRO_API_BASE_URL from an in-process
// Next route handler and passes the built production MCP binary here.
func TestCreateOfferAgainstRealRouteHandler(t *testing.T) {
	binary := os.Getenv("PAYVRO_MCP_BINARY")
	productID := os.Getenv("PAYVRO_TEST_APPROVED_PRODUCT_ID")
	approvalID := os.Getenv("PAYVRO_TEST_APPROVAL_ID")
	if binary == "" || productID == "" || approvalID == "" {
		t.Skip("requires the merchant-route integration harness")
	}
	cmd := exec.Command(binary)
	cmd.Env = os.Environ()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	session, err := mcp.NewClient(&mcp.Implementation{Name: "route-integration", Version: "0"}, nil).
		Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		t.Fatalf("MCP initialize: %v", err)
	}
	defer func() {
		if err := session.Close(); err != nil {
			t.Errorf("MCP process close: %v", err)
		}
	}()

	result, err := session.CallTool(ctx, &mcp.CallToolParams{
		Name: "create_offer",
		Arguments: map[string]any{
			"approvedProductId": productID,
			"approvalId":        approvalID,
			"idempotencyKey":    "real-route-offer-key-001",
		},
	})
	if err != nil {
		t.Fatalf("create_offer call: %v", err)
	}
	if result.IsError {
		var details string
		for _, content := range result.Content {
			if text, ok := content.(*mcp.TextContent); ok {
				details += text.Text
			}
		}
		t.Fatalf("create_offer failed: %s", details)
	}
	encoded, err := json.Marshal(result.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	var offer struct {
		OfferID  string `json:"offerId"`
		OfferURL string `json:"offerUrl"`
		Mode     string `json:"mode"`
	}
	if err := json.Unmarshal(encoded, &offer); err != nil {
		t.Fatal(err)
	}
	if offer.OfferID == "" || offer.OfferURL == "" || offer.Mode != "DEMO" {
		t.Fatalf("unexpected create_offer response: %s", encoded)
	}
}
