# PAYVRO MCP security model

This document maps advertised DIRECT guards to the calling path that enforces them and to the tests that exercise that caller. It is not a cryptographic proof, not a LIVE payment audit, and not a Windows/macOS runtime certification.

Scope: this repository. STORE needs a separate PAYVRO merchant backend that is not part of this repository. DEMO is local and non-payable. LIVE has not been exercised by the automated tests. Human confirmation requested by tool prose is not cryptographic consent and is not a dependable prompt-injection defense. Concept filters are heuristic, not universal PII. The local journal is at-most-one local attempt, not provider exactly-once. POSIX `0700`/`0600` and directory `fsync` were measured on Linux; Windows and macOS journal safety are **NOT MEASURABLE** here.

## Caller surfaces

| Surface | Path | Role |
|---|---|---|
| MCP wire (DIRECT) | `internal/tools/tools.go` `Register` / `createDirectCharge` / `getDirectChargeStatus` | JSON Schema + SDK `CallTool` |
| stdio process | `cmd/payvro-mcp/main.go` | Real binary, stdout/stderr redaction |
| money/data | `internal/direct/direct.go` `CreateCharge` / `GetChargeStatus` | Cap, concept, journal, provider stubs |
| journal (Linux) | `internal/direct/journal.go` | Reserve/lock/status-read purity |
| redaction | `internal/redact/writer.go` plus `cmd/payvro-mcp/streams.go` | stdout/stderr/main-goroutine panic |

Missing `PAYVRO_MAX_AMOUNT_CENTAVOS` fails the handler (`FromEnv` on each tool call), not process initialization: `ListTools` still returns DIRECT tools.

## Guard → caller → tests

| Guard | Calling path | Positive / negative tests | Sibling route |
|---|---|---|---|
| Mandatory positive safe-integer cap | `direct.FromEnv` / `CreateCharge` amount vs `MaxAmount`; MCP schema integer | `TestDirectModeExposesOnlyDirectToolsAndRequiresAmountCap`, `TestMissingCapFailsDirectHandlersWithoutBlockingToolListing`, `TestAmountCapAndExactMoneyParsing` | create **and** `get_charge_status` |
| Integer centavos only | MCP `amountCentavos` integer schema | `TestDirectModeExposesOnlyDirectToolsAndRequiresAmountCap` (1.5), `TestCreateAndStatusRejectZeroAmountHostModeAndUnknownStatusKey` (0, -1, string) | qr **and** link |
| No caller-controlled key/account/host/mode/status/destination | `additionalProperties: false` on both DIRECT tools; env-only `PEY_*` / origin | `TestDirectSDKRejectsSensitiveConceptsUnknownFieldsAndUnknownTools`, `TestDirectDemoSDKJourneyAndAmbiguousConfigFailsClosed` (`destination` on link), `TestGetChargeStatusRejectsCallerControlledFieldsBeforeJournal`, `TestCreateAndStatusRejectZeroAmountHostModeAndUnknownStatusKey` | create qr/link **and** status |
| Strict extra-arg schema | MCP InputSchema `additionalProperties: false` | STORE: `TestUnknownArgumentsRejected`; DIRECT create + status tests above | `create_codi_charge` **and** `get_charge_status` |
| Concept secrets / 13–19 digit heuristic before reserve/provider | `containsSensitiveConceptMaterial` before `journal.Reserve` | `TestSensitiveConceptsAreRejectedBeforeJournalAndProviderPost` (12 allowed, 13 blocked, 19 blocked, 20 allowed; dotted/non-ASCII remain unblocked heuristic limits), `TestDirectSDKRejectsInheritedBFFSecretBeforeJournal`, `TestSensitiveConceptRejectedOnLinkBeforeJournal`, stdio `TestDirectDemoThroughSDKStdioNeverContactsPey` | qr **and** link |
| Key/intent idempotency; no retry UNCERTAIN | journal bind + reserved/uncertain finish | `TestDirectDemoSDKJourneyAndAmbiguousConfigFailsClosed`, `TestUncertainPOSTNeverRetriesAcrossDuplicateOrRestart`, `TestConcurrentSameKeyHasAtMostOnePOST` | qr **and** link (DEMO journey uses link; LIVE stubs cover qr) |
| No false PAID on weak/malformed/incomplete provider replies | `readQRStatus` / `readLinkStatus` | `TestQRListAndDetailMustAgreeBeforePaid` (including two distinct PAID summaries), `TestQRTruncatedPaginationCannotReportPaid` (page1 PAID + empty pages 2..5 with nextToken; complete bounded empty final page remains PAID), `TestLinkStatusMoneyIdentityAndStateAtGetChargeStatus` (base total, paid-with-zero-tx, clientReferenceId, root id, pending+paid tx, nonzero tip, 100+49 overblock half), `TestWrongCurrencyOrAmountNeverReturnsPaid`, `TestLinkCallerRequiresSupportedConsistentPaymentMethod`, `TestPaymentLinkHasTipMustBeExplicitBooleanAtStatusCaller`, `TestProviderMalformedAndRedirectResponsesAreUncertainAndNotFollowed` (302 to `api.pey.mx`, one authenticated hit) | qr **and** link status |
| stderr/stdout redaction (truncation/main-goroutine panic/wiring) | `installStreams`, main-goroutine panic recover, MCP middleware | `TestWriterRedactsTokenSplitAcrossWrites`, `TestPATNeverOnStdoutOrStderr`, `TestSDKParamsRejectedBeforeMiddlewareDoNotLeakPAT`, `TestPanicPathRedactsCanaryOnStderr` | STORE stdio **and** DIRECT panic env. Other-goroutine Go-runtime panics are **not** claimed protected. |
| Linux journal permissions/lock/status-read purity/corruption | `journal.go` | `TestGetStatusWithoutChargeDoesNotCreateJournal`, `TestGetStatusUnsafeJournalFailsClosedWithoutChangingPermissions`, `TestCorruptAndTruncatedJournalsBlockStatusAndProviderPost`, `TestNewDedicatedJournalDirectoryIsPrivateWithoutChangingSharedParent`, `TestExistingSharedJournalDirectoryPermissionsArePreservedAndBlockCreate`, MCP `TestCreateAndStatusRejectZeroAmountHostModeAndUnknownStatusKey` | create **and** status |
| DIRECT / STORE / mixed env | `tools.selectedMode` | `TestDirectModeExposesOnlyDirectToolsAndRequiresAmountCap`, `TestDirectSelectorFailsClosedWithoutExplicitModeOrStorePlusPeyKey`, mixed credentials in `TestDirectDemoSDKJourneyAndAmbiguousConfigFailsClosed` | explicit DIRECT, STORE+pey key, missing mode |

Windows journal ACL tests exist (`journal_windows_test.go`) but are **NOT MEASURABLE** on this Linux runner. macOS journal behavior is also unverified. Do not read this table as a native Windows or macOS safety claim.

## Honest limitations

- Conversational “please confirm amount/concept” is not cryptographic consent.
- Concept filters are heuristic, not universal PII: unlabeled names, emails, novel secret formats, 20-digit runs, non-ASCII digit sequences, and dotted groups are not comprehensively detected.
- Panic redaction applies to the **main goroutine** recover path only. A panic in any other goroutine is printed by the Go runtime to stderr and is not claimed protected.
- Local journal is not provider exactly-once.
- LIVE is not exercised by the automated tests; DEMO never contacts `api.pey.mx`.
- Direct `PAID` is an authenticated GET validation against the journaled resource, not store settlement, webhook proof, or fulfillment.
- Missing cap fails the handler, not initialization.
- Source only: there are no published binaries, packages or signed releases.
