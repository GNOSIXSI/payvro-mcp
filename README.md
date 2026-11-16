# PAYVRO MCP

A small [Model Context Protocol](https://modelcontextprotocol.io) server that lets an AI agent **create and check CoDi charges** in Mexico through the [pey API](https://docs.pey.mx), using **your own pey account and keys**.

It runs locally over stdio: your MCP client (Claude Code, Codex, Cursor, …) starts the binary, and in `DIRECT` mode the binary talks only to `https://api.pey.mx`.

- Two tools: `create_codi_charge` and `get_charge_status`.
- Amounts are integer centavos, always under a cap you set.
- A local journal records each attempt: the same `idempotencyKey` reuses the local result on retry, and an uncertain outcome needs manual review. It is not exactly-once at pey.
- `DEMO` mode runs fully offline and never contacts pey, so you can try it without an account.

> PAYVRO MCP is an independent open-source project. It is **not affiliated with, endorsed by, or supported by pey**. You need your own pey account, and you are responsible for the charges you create with it.

MIT licensed. Source only: there are no published binaries or packages yet.

## Quickstart (DEMO, about 60 seconds)

Requires Go 1.25 or newer (the `toolchain` line in `go.mod` downloads the pinned Go 1.27.3 automatically).

```sh
git clone https://github.com/GNOSIXSI/payvro-mcp.git
cd payvro-mcp
go build -o bin/payvro-mcp ./cmd/payvro-mcp
```

Register it with Claude Code in DEMO mode (no secrets involved):

```sh
claude mcp add payvro \
  --env PAYVRO_MCP_MODE=DIRECT \
  --env PAYVRO_MODE=DEMO \
  --env PAYVRO_MAX_AMOUNT_CENTAVOS=50000 \
  --env PEY_API_KEY_ID=demo-account \
  -- "$PWD/bin/payvro-mcp"
```

Then ask your agent: *"Create a CoDi QR charge of $149.00 MXN for 'Curso de prueba', then check its status."*
Both calls return `DEMO_ONLY`: a local, non-payable simulation. Nothing reaches pey.

## Going LIVE

LIVE creates real charges in your pey account. Before you switch:

1. Get an API key and key ID from your pey account.
2. Keep the key out of shell history and out of client config files. Load it from your secret manager in a small wrapper script that the MCP client runs:

   ```sh
   #!/bin/sh
   # payvro-mcp-live: not committed anywhere; the key comes from your secret store.
   set -eu
   export PAYVRO_MCP_MODE=DIRECT
   export PAYVRO_MODE=LIVE
   export PAYVRO_MAX_AMOUNT_CENTAVOS=100000   # hard cap per charge: $1,000.00 MXN
   export PEY_API_KEY_ID="your-key-id"
   export PEY_API_KEY="$(your-secret-manager read pey/api-key)"
   # export PAYVRO_PUBLIC_ORIGIN=https://your-site.example   # required for payment links
   # export PEY_BANK_ACCOUNT_ID=...                          # if your pey account needs it for QR
   exec /absolute/path/to/payvro-mcp
   ```

3. Register the wrapper instead of the binary: `claude mcp add payvro -- /absolute/path/to/payvro-mcp-live`.
4. Start with a very small cap and a test charge you pay yourself.

The automated tests cover DEMO and stubbed pey responses. They do not run against the live pey API.

## Other MCP clients

Any MCP client that can start a stdio process works the same way: run the binary (or your wrapper) with the environment below.

### Tested clients

Checked on 16 Nov 2026 on Linux, in `DEMO`, with a real `create_codi_charge` then `get_charge_status` call through the client (both returned `DEMO_ONLY`):

| Client | Version | Result | Note |
|---|---|---|---|
| Claude Code | 2.1.326 | works | `claude mcp add` as above, or `--mcp-config` |
| Codex CLI | 0.163.0 | works | Codex asks before running an MCP tool. With approvals off (`codex exec`), the call fails unless you set `default_tools_approval_mode = "approve"` under `[mcp_servers.payvro]`. |
| Cursor CLI (`cursor-agent`) | 2026.11.13 | works | Approve the server once: `cursor-agent mcp enable payvro` |
| Claude Desktop, Cursor (editor), GitHub Copilot, others | | not tested | Same stdio standard; should work. Reports welcome. |


**Codex CLI** (`~/.codex/config.toml`):

```toml
[mcp_servers.payvro]
command = "/absolute/path/to/payvro-mcp"
env = { PAYVRO_MCP_MODE = "DIRECT", PAYVRO_MODE = "DEMO", PAYVRO_MAX_AMOUNT_CENTAVOS = "50000", PEY_API_KEY_ID = "demo-account" }
```

**Cursor and other JSON-configured clients** (`.cursor/mcp.json` or the client's `mcp.json`):

```json
{
  "mcpServers": {
    "payvro": {
      "command": "/absolute/path/to/payvro-mcp",
      "env": {
        "PAYVRO_MCP_MODE": "DIRECT",
        "PAYVRO_MODE": "DEMO",
        "PAYVRO_MAX_AMOUNT_CENTAVOS": "50000",
        "PEY_API_KEY_ID": "demo-account"
      }
    }
  }
}
```

For LIVE, point `command` at your wrapper script rather than putting the key in these files.

## Tools

| Tool | Input | What it does |
|---|---|---|
| `create_codi_charge` | `amountCentavos` (integer, 1 to your cap), `concept` (1 to 100 chars), `method` (`qr` or `link`), `idempotencyKey` (16 to 128 chars, `[A-Za-z0-9_-]`) | Creates a CoDi QR or a pey payment link. The tool asks the agent to confirm amount and concept with a human first. Calling it again with the same key and the same intent reuses the local result (or reports `UNCERTAIN` if the first outcome was unknown); the same key with a different intent is rejected. |
| `get_charge_status` | `idempotencyKey` (the one used to create) | Read-only. Looks up the charge in pey with authenticated GETs and returns the provider state (for example `PENDING`, `PAID`, `EXPIRED`), or `NEEDS_REVIEW`, `UNCERTAIN` or `DEMO_ONLY`. `PAID` requires the amounts, currency and transaction identities to match exactly; anything inconsistent becomes `NEEDS_REVIEW`. |

The agent cannot pass keys, account IDs, hosts, currency, mode, destinations or payment status: extra arguments are rejected by the schema.

## Configuration

| Variable | Required | Meaning |
|---|---|---|
| `PAYVRO_MCP_MODE` | yes | `DIRECT` |
| `PAYVRO_MODE` | yes | `DEMO` (offline) or `LIVE` |
| `PAYVRO_MAX_AMOUNT_CENTAVOS` | yes | Maximum amount per charge, in centavos. Missing or invalid values make the tools fail. |
| `PEY_API_KEY_ID` | yes | Your pey key ID. Also scopes the local journal to that account. |
| `PEY_API_KEY` | LIVE only | Your pey API key. Read from the environment only; not written to tool results or the journal, and redacted from stdout/stderr (see limits in `SECURITY.md`). |
| `PAYVRO_PUBLIC_ORIGIN` | LIVE links | HTTPS origin where pey sends the buyer back (`/payment/return`). |
| `PEY_BANK_ACCOUNT_ID` | optional | Destination account for QR charges, if your pey account requires one. |
| `PAYVRO_DIRECT_JOURNAL` | optional | Path to the journal file (`.jsonl`). Defaults to your user config directory. Point it at a directory that does not exist yet (the server creates it private, `0700`) or one only you can write: a group- or world-writable directory makes charges fail closed. |

## Safety model, in short

- **Fixed host.** The pey host is compiled in. No base URL, redirect target or callback can be supplied at runtime.
- **Cap.** Every charge must be at or below `PAYVRO_MAX_AMOUNT_CENTAVOS`.
- **No blind retries.** A journal entry is written before every POST. If a POST's outcome is unknown, the charge becomes `UNCERTAIN` and the server does not retry it: check it in your pey dashboard. Do not delete the journal to "fix" it. The journal prevents duplicate attempts from this machine; it is not exactly-once at pey.
- **Leak checks on the concept.** Concepts that look like a token, an API key, a JWT or a long card/CLABE-style number are rejected before anything is sent. This is a heuristic, not full PII detection.
- **Redaction.** Configured secrets are redacted from stdout and stderr, including panics on the main goroutine. A panic in another goroutine is not covered.
- **Human confirmation is conversational.** The tool asks the agent to confirm with a person. That helps, but it is not cryptographic consent and does not stop prompt injection on its own. The cap is your real limit.

Details and the test that covers each guard: [`docs/SECURITY_MODEL.md`](docs/SECURITY_MODEL.md).

## Platform support

Tested on Linux in CI. macOS should work but is not tested. **Windows is not supported yet**: the code builds, but two tests fail on native Windows, including one around concurrent journal writes. Use Linux or WSL.

## STORE mode (advanced)

The binary also has a `STORE` mode (`PAYVRO_MCP_MODE=STORE`, tools `create_offer`, `create_checkout`, `get_order_status`) that talks to a PAYVRO merchant backend instead of pey. That backend is **not** part of this repository, so most users only need `DIRECT`. The two modes are exclusive: the server refuses to start tools when both are configured.

## Development

```sh
go vet ./...
go test -race ./...
```

Tests use synthetic keys and local stubs; they never contact pey. See [`CONTRIBUTING.md`](CONTRIBUTING.md) and [`SECURITY.md`](SECURITY.md).

## License

[MIT](LICENSE). Third-party licenses: [`THIRD_PARTY_NOTICES.md`](THIRD_PARTY_NOTICES.md).

Powered by GNOSIX.
