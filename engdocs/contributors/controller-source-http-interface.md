---
title: Protected source adapter HTTP protocol
description: Default-off human source flow and retained retirement verifier wiring.
---

This worktree is based on controller source
`56dd2908846d94514a28f62ba3279b1d985f760b`. These are source interfaces, not an
assigned qualified controller/backend release. They neither authorize a live
trial nor retire scripts or activate the private workflow.

## Trust and transport

Use the generated OpenAPI/Go/TypeScript client methods for these Huma routes.
Pin the controller endpoint, city, configured named prompt target and trust in
trusted application composition. Never select them from advisory intake JSON.
Every POST below requires a fresh request-bound `X-GC-City-Write` grant; the
authorized GET requires `X-GC-City-Read`. These requirements also apply when the
listener's global auth hardening is optional. City-write authority authenticates
the caller; only the separate `decision.answer` verifier authenticates answers.
POSTs retain the normal `X-GC-Request` requirement.

The **complete serialized UTF-8 HTTP body is limited to 1 MiB (1,048,576 bytes)**,
including JSON quoting/escaping, all fields and base64 expansion. All source
routes and the existing city-write body gate share
`citywriteauth.MaxHTTPBodyBytes`; oversized bodies return HTTP 413. Size the exact
body before minting its request-bound grant. Go retirement clients can use
`Request.HTTPEnvelope()` to serialize and enforce this same budget. The offline
retained parser's 16 MiB file bound is not an HTTP capability. Larger retained
inputs require trusted in-process composition; do not split a bound gate request
or raise global request limits to fit it. No route-specific increase is enabled.

No source route falls back to a filesystem mutation, alternate mailbox, direct
session send, or caller-provided success flag. Missing trusted composition or a
required backend contract returns unavailable. A transport failure is an unknown
outcome: keep the source held and replay the exact intent with fresh request auth.

## Human source-flow ports

All paths are relative to `/v0/city/{cityName}/bead/{id}/decision-frontier`.

| Pack port | Route and request | Response / adapter obligation |
| --- | --- | --- |
| `prepare_proposal` | POST `/prepare`, `{work_revision, proposal}` | `HumanSourcePreparation`: opaque token, expiry, physical scope/work/revision, configured `target_name`, exact session/generation/request `binding`, and independent-question delivery contract. Compare every configured expectation before creating the prepared client. |
| prepared client `ensure` | POST `/prepared`, `{preparation_token}`, stable `Idempotency-Key` | Current durable `Frontier`. Controller revalidates the target/backend fence even for HTTP replay. A token cannot be constructed from an ordinary frontier or binding object. |
| `read_authorized_frontier` | GET `/authorized?work_revision={original_frontier_revision}` | `AuthorizedFrontier`: frontier, freshly verified signed-answer authorities, exact persisted session/round evidence, current physical revision, and reservation/release transitions. Compare full work/map/ticket/question/provenance scope and the expected current physical revision. |
| `submit_answer` | POST `/authorized/answers`, exact `AnswerSubmission`, stable `Idempotency-Key` | Authoritative signed submission plus fresh protected readback. Opaque proof binds the complete current challenge. Changed replay bodies, worker keys and stale/cross-scope challenges fail closed. |
| `check_resume` | POST `/check-resume`, `{frontier_revision, physical_revision}` | Typed protected `ResumeEligibility`, bound to both revisions and the exact map. Readiness is not assignment, admission, merge or external-action permission; eventual actions still consume their own protected revision guard. |

Preparation tokens are bounded, per-city, short-lived transport references bound
to the authenticated issuing key. They contain no serialized authority and are
not persisted. A restart/expiry requires fresh preparation; the existing protected
frontier and session receipt remain the only durable decision/delivery ledger.
Do not redirect an existing frontier after a target or generation change.

The pack's `PromptBinding.execution_generation` text is the canonical decimal
representation of the wire's positive integer. Its target comes from
`HumanSourcePreparation.target_name`. The delivery contract must equal
`gascity.decision-frontier.independent-questions.v1` exactly. Numbered rounds
contain only currently independent open questions; held/dependency-blocked
questions remain outside delivery. Each ordered ticket/version round has its own
deterministic protected intent and session request receipt.

The protected frontier's `prompt.id`/`prompt.status` describe that exact current
round and match `session.binding.request_id`; the ordinary advisory read still
names the map's initial intent. The private consumer must recognize question
status `held` (including an interrupted answer reservation or a held answered
prerequisite) and exclude it from dependency satisfaction, readiness and
presentation. Older consumers accepting only `open`/`answered` must stay disabled
until their codec implements this contract; do not silently coerce held records
to open or answered. Missing `delivery_contract` on an older frontier is not
evidence that the independent-round contract is available.

Build `AnswerAuthority` only from a successful authenticated protected readback:
each authority row is joined to its exact frontier question and answer. Copy
city/store/work/revision/digest/map from that frontier, ticket/version from the
question, answer digest/resolution from the answer, and the reverified principal
from the authority row. Ordinary `AnswerView` identity fields and locally computed
hashes do not implement this port. Preserve acceptance, provider result,
acknowledgement and unverified effect as distinct session evidence. Human answers
do not fabricate a provider acknowledgement or operational effect.

Derive the pack's `controller_authorized` disposition only after the protected
readback and exact-revision eligibility operation both succeed with matching
scope and all required signed `answered` resolutions. Input JSON cannot supply
that disposition. Other answer policies remain pending. Generic bead
GET/list/graph/dependency projections hide retained signed proof records;
protected frontier views expose verified evidence without the proof envelope.

## Offline retirement verifier port

POST `/v0/city/{cityName}/retirement-release/verify` accepts the typed
`retirementrelease.Request`:

```json
{
  "gate": "human-review",
  "request_sha256": "<canonical checker gate-request SHA-256>",
  "manifest_json": "<complete canonical checker gate-request JSON text>",
  "retained_base64": "<standard base64 of retained proof bytes>"
}
```

The configured `RetirementSourceProvider` supplies the adapter. HTTP callers
cannot install `Compose`, a verifier, a permission authority or a policy. A
trusted offline application may invoke the same Go adapter directly. The adapter
requires independently reconstructed context/retention bindings, scoped canonical
permission, and current canonical verifier results; it revalidates composition
and authority before returning the checker-compatible `Verdict`.

The existing Python checker still owns the complete retained structural and
filesystem checks. Its trusted in-process verifier translates the full gate
request to canonical JSON, hashes those exact canonical bytes, submits retained
bytes, and verifies the returned `request_sha256` and closed status/assurance
sets. It must never deserialize a retained success report as a verdict. No
Python CLI plugin, trust override or network invocation is introduced here.

Unsupported gate contracts return `unavailable`. Signed observations alone do
not prove useful trial outcomes, boot-wide manual audit, retirement effect or
activation approval. Fixture assurance never establishes operational readiness;
all mandatory gate checks in the existing guard remain required.

## Qualification and race limits

See [human domain interface](controller-human-domain-interface.md) and
[retirement composition](controller-retirement-source-interface.md) for the
source/backend contracts and remaining P2 cross-row windows. A successful source
test is not evidence for a live trial, audit, retirement or activation. Qualification
must bind the eventual integrated artifact, backend, configuration, city and pack
revision and exercise concurrent stale writes, restart, lost responses and holds.
