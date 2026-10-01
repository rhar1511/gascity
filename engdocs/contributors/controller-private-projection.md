---
title: "Private record projection contract"
description: "Presentation coverage, authoritative read boundaries, and local regression evidence for private records."
---

## Privacy contract

Generic bead views must not publish signed human-answer proofs or attempt
payloads. The shared presentation policy lives in
`internal/beads/private_projection.go`; the API, CLI, bridge, event wire, and
workflow views consume it.

`IsPrivatePresentationRecord` reuses the existing protected attempt-record
policy, including incomplete archive and content-payload markers. It also
recognizes `gc.decision_frontier.record = decision-frontier/answer/v1`.
`PublicBead` replaces those records with an identity-only private stub. On
ordinary owner beads, it copies the metadata map and removes the existing
private attempt fields, stdout/stderr/output JSON, attempt indexes, and session
request receipts. Public descriptions and ordinary metadata remain available.
`PublicBeads` creates a new output slice. Projection never modifies durable
records, cached metadata, or the source slice.

The generic API bead sanitizer continues to omit complete private rows rather
than returning stubs. Other generic views can use stubs without exposing any
record body. Presentation does not grant permission to read private payloads.

## Surface coverage

| Surface | Boundary and behavior |
| --- | --- |
| API bead list, ready, show, graph, children, create replies | Existing `publicAttemptEvidenceBead` delegates to the shared policy; private rows remain omitted or unavailable. |
| CLI `gc beads show/list`, cache envelopes, tables | Shared `bead_format.go` writers project values before JSON or text rendering. |
| CLI `gc ready` JSON | `toReadyBead` projects the domain record before copying presentation fields. Text uses shared bead writers. |
| CLI class-bound `gc bd show` and mutation replies | `printBdByIDBead` projects before either JSON-array or full text output. |
| CLI class-bound dependency list/tree | Embedded bead rows are projected at row construction; traversal uses the raw graph. |
| CLI provider passthrough | `bd_private_projection.go` admits a closed set of structured record commands, requests JSON, projects nested provider records, then renders JSON or text. It preserves public JSON bytes when no redaction is required. Empty successful output is safe. |
| Unstructured passthrough | SQL, export, history, unknown commands, field masks, templates, and output-file selection fail closed. A class-residency override does not override privacy. Known administrative commands retain their transport. |
| Provider diagnostics | Structured passthrough buffers stdout and withholds raw stderr. It retains exit status and emits safe failure/fallback diagnostics. Bridge provider errors preserve error identity while withholding their raw diagnostic text. |
| `gc bd-store-bridge` create/get/list/ready/children | All record replies pass through `bridgeBead`, which uses the shared projection. |
| API event lists, city/supervisor SSE | `PublicBeadEvent` projects raw or wrapped historical bead snapshots and clears private-record messages before typed wire construction. Malformed nonempty bead snapshots fail closed. |
| CLI local event projection | `localWireEvent` uses the same event policy and reports malformed snapshot projection failures without forwarding the snapshot/message. API-backed event output inherits the API projection. |
| API convoy list/status | Convoy and child bead responses are projected after progress is computed from authoritative state. |
| CLI convoy list/status/stranded/land summaries | Presentation titles, child rows, metadata-derived fields, and JSON status replies use projected records. Progress and lifecycle decisions use raw records. |
| API workflow snapshots and event workflow nodes | `workflowBeadResponseFromBead` projects before the workflow codec copies metadata. |
| Run views and dashboard run detail | `runproj.fromBead` and `toRunSnapshotBead` project at the presentation codec. API step titles and last-error extraction also project their record inputs. |

## Authoritative raw-read boundaries

These remain raw because they verify, retain, or operate on authoritative data:

- `beads.Store.Get`, `List`, protected readers, private payload reads, immutable
  archive readers, and store/cache persistence. Projection is not installed as
  a store wrapper or a `Bead.MarshalJSON` method.
- Human-answer ingestion, signature verification, decision-frontier matching,
  admission and retirement verification, and atomic controller transitions.
- Exact-scope authorized attempt-evidence and artifact API routes. Their
  authorizers and private HTTP credentials are unchanged.
- Durable event recording and `DecodeBeadEventPayload`, used by folds and
  internal state reconstruction. Outbound event views project a copy; they do
  not rewrite the event log.
- Operator storage-recovery dumps and store migrations. These are privileged
  recovery artifacts rather than generic bead views.

No city-wide payload-read grant, generic answer-proof reader, or broad private
HTTP permission is introduced. New generic renderers must call the shared
projection before copying descriptions or metadata into their output shape.

## Regression owners

- `TestPrivatePresentationPreservesAuthoritativeRecord` owns private-record
  classification, owner-metadata redaction, and non-mutation of authority.
- `TestPrivateEventPresentationKeepsRawDecoder` owns outbound snapshot/message
  privacy while preserving the raw event decoder.
- `TestPrivateProjectionExecutable` executes the real CLI entry point against
  a test-owned provider that returns a proof in its description and stderr. Its
  txtar covers passthrough show/list, bridge get/list, file-backed show, convoy
  JSON/text, and raw export refusal without a live service.
- `TestPrivateProjectionClassBindingRenderer` owns the class-binding full-record
  text and JSON printer.
- `TestPrivateProjectionEventAndWorkflowWire` owns list, city SSE, supervisor
  SSE, workflow-node JSON, and the API's full-row omission semantics.

All Go verification for this work uses the serial
`/home/ricky/gascity-pilot/.gc/scripts/go-bounded.sh` launcher. Retirement build
metadata, repository-tree inventories, HTTP body limits, and the cross-language
harness remain with their designated owners.

## Local validation evidence

The focused executable and shared-invariant regressions passed. The broader
API/run-view sweep passed (API 21.767 s; run projection 0.021 s), and the
decision-frontier human/answer/frontier tests passed (2.103 s), retaining raw
verification coverage. The beads projection and routing sweep passed (0.020 s).
The three lifecycle-preflight CLI fixtures passed together (0.558 s), and the
broader CLI projection, routing, bridge, mutation, and heartbeat sweep passed
(10.266 s) with no test-name exclusions:

```bash
GC_FAST_UNIT=1 GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null \
  /home/ricky/gascity-pilot/.gc/scripts/go-bounded.sh test ./cmd/gc \
  -run '^(TestGcBd|TestBdByID|TestBdMutation|TestRewriteBdHeartbeat|TestPrivateProjection|TestBdStoreBridge|TestBdRigQualified|TestResolveBdScopeTarget)' \
  -count=1 -parallel=1 -timeout=180s
```

The fixtures return current normal-work issue arrays with nonzero revisions
for enrollment reads and structured mutation replies. Mutation assertions
capture only the forwarded write, preserving registered and historic actor
compatibility, native heartbeat forwarding, and the single stale-`BD_BIN`
warning. An enrolled-work heartbeat case verifies the existing proof-required
refusal before any native lease refresh. Lifecycle preflight remains active.
This is a bounded CLI sweep, not a full-baseline claim.

Repository-wide bounded `vet ./...` passed. Bounded
`build -buildvcs=false -o /dev/null ./cmd/gc` passed; the default build had failed
while obtaining VCS status. CLI reference prose was generated with bounded
`run ./cmd/gc gen-doc`. The hook-owner script passed. `make` is unavailable in
this environment; dashboard regeneration and staged pre-commit were not run.
The existing dirty dashboard assets and generated API work were preserved.
