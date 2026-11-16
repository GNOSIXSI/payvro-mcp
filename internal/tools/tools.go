package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/GNOSIXSI/payvro-mcp/internal/client"
	"github.com/GNOSIXSI/payvro-mcp/internal/direct"
	redactor "github.com/GNOSIXSI/payvro-mcp/internal/redact"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const createOfferSchema = `{
  "type": "object",
  "additionalProperties": false,
  "required": ["approvedProductId", "approvalId", "idempotencyKey"],
  "properties": {
    "approvedProductId": {"type": "string", "minLength": 1, "maxLength": 256},
    "approvalId": {"type": "string", "minLength": 1, "maxLength": 256},
    "idempotencyKey": {"type": "string", "minLength": 16, "maxLength": 128, "pattern": "^[A-Za-z0-9_-]+$"}
  }
}`

const createCheckoutSchema = `{
  "type": "object",
  "additionalProperties": false,
  "required": ["offerId", "idempotencyKey"],
  "properties": {
    "offerId": {"type": "string", "minLength": 1, "maxLength": 256},
    "idempotencyKey": {"type": "string", "minLength": 16, "maxLength": 128, "pattern": "^[A-Za-z0-9_-]+$"}
  }
}`

const directChargeSchema = `{
  "type": "object",
  "additionalProperties": false,
  "required": ["amountCentavos", "concept", "method", "idempotencyKey"],
  "properties": {
    "amountCentavos": {"type": "integer", "minimum": 1, "maximum": 9007199254740991},
    "concept": {"type": "string", "minLength": 1, "maxLength": 100},
    "method": {"type": "string", "enum": ["qr", "link"]},
    "idempotencyKey": {"type": "string", "minLength": 16, "maxLength": 128, "pattern": "^[A-Za-z0-9_-]+$"}
  }
}`

const directStatusSchema = `{
  "type": "object",
  "additionalProperties": false,
  "required": ["idempotencyKey"],
  "properties": {
    "idempotencyKey": {"type": "string", "minLength": 16, "maxLength": 128, "pattern": "^[A-Za-z0-9_-]+$"}
  }
}`

const orderStatusSchema = `{
  "type": "object",
  "additionalProperties": false,
  "required": ["orderId"],
  "properties": {
    "orderId": {"type": "string", "minLength": 1, "maxLength": 256}
  }
}`

type createOfferIn struct {
	ApprovedProductID string `json:"approvedProductId"`
	ApprovalID        string `json:"approvalId"`
	IdempotencyKey    string `json:"idempotencyKey"`
}

type createCheckoutIn struct {
	OfferID        string `json:"offerId"`
	IdempotencyKey string `json:"idempotencyKey"`
}

type orderStatusIn struct {
	OrderID string `json:"orderId"`
}

type directChargeIn struct {
	AmountCentavos int64  `json:"amountCentavos"`
	Concept        string `json:"concept"`
	Method         string `json:"method"`
	IdempotencyKey string `json:"idempotencyKey"`
}

type directStatusIn struct {
	IdempotencyKey string `json:"idempotencyKey"`
}

type offerOut struct {
	OfferID  string `json:"offerId"`
	OfferURL string `json:"offerUrl"`
	Mode     string `json:"mode"`
}

type checkoutOut struct {
	CheckoutURL string `json:"checkoutUrl"`
	Mode        string `json:"mode"`
}

type orderStatusOut struct {
	OrderID        string `json:"orderId"`
	Status         string `json:"status"`
	Mode           string `json:"mode"`
	AmountCentavos int64  `json:"amountCentavos"`
	ReasonCategory string `json:"reasonCategory"`
}

type handlers struct {
	diag io.Writer
}

// Register exposes exactly one configuration mode. Diagnostics are written to diag, never stdout.
func Register(server *mcp.Server, diag io.Writer) {
	if diag == nil {
		diag = os.Stderr
	}
	h := &handlers{diag: diag}
	switch selectedMode() {
	case "DIRECT":
		h.registerDirect(server)
	case "STORE":
		h.registerStore(server)
	default:
		fmt.Fprintln(diag, "payvro-mcp: invalid or ambiguous mode configuration; set PAYVRO_MCP_MODE explicitly and configure only that mode")
	}
}

func selectedMode() string {
	mode := os.Getenv("PAYVRO_MCP_MODE")
	storeConfigured := os.Getenv("PAYVRO_API_BASE_URL") != "" || os.Getenv("PAYVRO_MERCHANT_TOKEN") != ""
	directKeyConfigured := os.Getenv("PEY_API_KEY") != ""
	switch mode {
	case "DIRECT":
		if storeConfigured {
			return ""
		}
		return "DIRECT"
	case "STORE":
		if directKeyConfigured {
			return ""
		}
		return "STORE"
	case "":
		if storeConfigured && !directKeyConfigured {
			return "STORE"
		} // safe legacy store configuration
	}
	return ""
}

func (h *handlers) registerStore(server *mcp.Server) {
	no, yes := false, true
	mcp.AddTool(server, &mcp.Tool{Name: "create_offer", Description: "Publica la oferta de un producto ya aprobado. No acepta precio, importe, receptor, moneda, modo ni credenciales.", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: &no, IdempotentHint: true, OpenWorldHint: &yes}, InputSchema: json.RawMessage(createOfferSchema)}, h.createOffer)
	mcp.AddTool(server, &mcp.Tool{Name: "create_checkout", Description: "Devuelve la URL pública de entrada al checkout. No crea un pedido ni un pago; es idempotente.", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: &no, IdempotentHint: true, OpenWorldHint: &yes}, InputSchema: json.RawMessage(createCheckoutSchema)}, h.createCheckout)
	mcp.AddTool(server, &mcp.Tool{Name: "get_order_status", Description: "Lee el estado redactado de un pedido del comercio. Solo lectura; no incluye tokens ni grants.", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, DestructiveHint: &no, IdempotentHint: true, OpenWorldHint: &yes}, InputSchema: json.RawMessage(orderStatusSchema)}, h.getOrderStatus)
	server.AddReceivingMiddleware(redactOutput)
}

func (h *handlers) registerDirect(server *mcp.Server) {
	no, yes := false, true
	mcp.AddTool(server, &mcp.Tool{Name: "create_codi_charge", Description: "Crea un cobro directo en la cuenta pey configurada por el operador. Antes de llamar, solicita confirmación humana explícita del monto y concepto; esa confirmación conversacional no es consentimiento criptográfico ni defensa infalible contra prompt injection. Respeta PAYVRO_MAX_AMOUNT_CENTAVOS. UNCERTAIN nunca se reintenta y requiere revisión manual.", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: &yes, IdempotentHint: true, OpenWorldHint: &yes}, InputSchema: json.RawMessage(directChargeSchema)}, h.createDirectCharge)
	mcp.AddTool(server, &mcp.Tool{Name: "get_charge_status", Description: "Consulta solo lectura el recurso del journal local del operador mediante GET autenticado de pey. No escribe estado ni sustituye webhooks/verificación de la tienda; estados desconocidos o inconsistentes requieren revisión.", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, DestructiveHint: &no, IdempotentHint: true, OpenWorldHint: &yes}, InputSchema: json.RawMessage(directStatusSchema)}, h.getDirectChargeStatus)
	server.AddReceivingMiddleware(redactOutput)
}

func (h *handlers) createDirectCharge(ctx context.Context, _ *mcp.CallToolRequest, in directChargeIn) (*mcp.CallToolResult, direct.Charge, error) {
	api, err := direct.FromEnv()
	if err != nil {
		return nil, direct.Charge{}, h.fail(err)
	}
	got, err := api.CreateCharge(ctx, in.AmountCentavos, in.Concept, in.Method, in.IdempotencyKey)
	if errors.Is(err, direct.ErrUncertain) {
		return nil, got, nil
	}
	if err != nil {
		return nil, direct.Charge{}, h.fail(err)
	}
	return nil, got, nil
}

func (h *handlers) getDirectChargeStatus(ctx context.Context, _ *mcp.CallToolRequest, in directStatusIn) (*mcp.CallToolResult, direct.Charge, error) {
	api, err := direct.FromEnv()
	if err != nil {
		return nil, direct.Charge{}, h.fail(err)
	}
	got, err := api.GetChargeStatus(ctx, in.IdempotencyKey)
	if errors.Is(err, direct.ErrUncertain) {
		return nil, got, nil
	}
	if err != nil {
		return nil, direct.Charge{}, h.fail(err)
	}
	return nil, got, nil
}

func (h *handlers) createOffer(ctx context.Context, _ *mcp.CallToolRequest, in createOfferIn) (*mcp.CallToolResult, offerOut, error) {
	api, err := client.FromEnv()
	if err != nil {
		return nil, offerOut{}, h.fail(err)
	}
	got, err := api.CreateOffer(ctx, in.ApprovedProductID, in.ApprovalID, in.IdempotencyKey)
	if err != nil {
		return nil, offerOut{}, h.fail(err)
	}
	return nil, offerOut{OfferID: got.OfferID, OfferURL: got.OfferURL, Mode: got.Mode}, nil
}

func (h *handlers) createCheckout(ctx context.Context, _ *mcp.CallToolRequest, in createCheckoutIn) (*mcp.CallToolResult, checkoutOut, error) {
	api, err := client.FromEnv()
	if err != nil {
		return nil, checkoutOut{}, h.fail(err)
	}
	got, err := api.CreateCheckout(ctx, in.OfferID, in.IdempotencyKey)
	if err != nil {
		return nil, checkoutOut{}, h.fail(err)
	}
	return nil, checkoutOut{CheckoutURL: got.CheckoutURL, Mode: got.Mode}, nil
}

func (h *handlers) getOrderStatus(ctx context.Context, _ *mcp.CallToolRequest, in orderStatusIn) (*mcp.CallToolResult, orderStatusOut, error) {
	api, err := client.FromEnv()
	if err != nil {
		return nil, orderStatusOut{}, h.fail(err)
	}
	got, err := api.GetOrderStatus(ctx, in.OrderID)
	if err != nil {
		return nil, orderStatusOut{}, h.fail(err)
	}
	return nil, orderStatusOut{
		OrderID:        got.OrderID,
		Status:         got.Status,
		Mode:           got.Mode,
		AmountCentavos: got.AmountCentavos,
		ReasonCategory: got.ReasonCategory,
	}, nil
}

func (h *handlers) fail(err error) error {
	msg := redactSecrets(err.Error())
	fmt.Fprintln(h.diag, "payvro-mcp:", msg)
	return errors.New(msg)
}

func redactOutput(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		res, err := next(ctx, method, req)
		redactMCPResult(res)
		return res, redactReturnedErr(err)
	}
}

func redactMCPResult(res mcp.Result) {
	ct, ok := res.(*mcp.CallToolResult)
	if !ok || ct == nil {
		return
	}
	for _, item := range ct.Content {
		if text, ok := item.(*mcp.TextContent); ok {
			text.Text = redactSecrets(text.Text)
		}
	}
	ct.StructuredContent = redactStructured(ct.StructuredContent)
	if len(ct.Meta) == 0 {
		return
	}
	blob, err := json.Marshal(ct.Meta)
	if err != nil {
		return
	}
	var meta mcp.Meta
	if json.Unmarshal([]byte(redactSecrets(string(blob))), &meta) == nil {
		ct.Meta = meta
	}
}

func redactStructured(v any) any {
	switch body := v.(type) {
	case nil:
		return nil
	case json.RawMessage:
		return json.RawMessage(redactSecrets(string(body)))
	case string:
		return redactSecrets(body)
	default:
		blob, err := json.Marshal(body)
		if err != nil {
			return v
		}
		return json.RawMessage(redactSecrets(string(blob)))
	}
}

func redactReturnedErr(err error) error {
	if err == nil {
		return nil
	}
	var wire *jsonrpc.Error
	if errors.As(err, &wire) && wire != nil {
		wire.Message = redactSecrets(wire.Message)
		if len(wire.Data) > 0 {
			wire.Data = json.RawMessage(redactSecrets(string(wire.Data)))
		}
		return err
	}
	msg := redactSecrets(err.Error())
	if msg == err.Error() {
		return err
	}
	return errors.New(msg)
}

func redactSecrets(msg string) string {
	if msg == "" {
		return msg
	}
	return redactor.RedactString(msg, redactor.ActiveEnvSecrets()...)
}
