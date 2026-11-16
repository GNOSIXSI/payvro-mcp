# Security policy

PAYVRO MCP creates payment requests, so we take reports seriously.

## Reporting a vulnerability

Please report privately through **GitHub Security Advisories** on this repository ("Report a vulnerability" in the Security tab). Do not open a public issue.

Never include real API keys, journals, CLABEs, account holder names or live payment evidence in a report. Synthetic reproductions are enough.

## Supported versions

Only the latest commit on `main` is supported.

## Scope and known limits

- Secrets live in the process environment only, never in tool arguments.
- The pey host is fixed at build time; the cap in `PAYVRO_MAX_AMOUNT_CENTAVOS` bounds every charge.
- The concept leak check is a heuristic, not complete PII detection.
- Human confirmation requested by the tool is conversational, not cryptographic consent, and is not a complete defense against prompt injection.
- Redaction covers stdout, stderr and panics on the main goroutine. A panic in another goroutine is printed by the Go runtime and is not redacted.
- The local journal prevents duplicate attempts from this machine. It is not exactly-once delivery at pey.
- Journal file safety was measured on Linux. macOS is untested and Windows is not supported yet.

How each guard is enforced and tested: [`docs/SECURITY_MODEL.md`](docs/SECURITY_MODEL.md).

If you ever expose a pey key, rotate it in your pey account. Deleting the file or the commit does not revoke it.
