---
title: Controller source adapter validation
description: Source-only evidence for human flow and retirement verifier composition.
---

## Scope

Worktree: `/home/ricky/gascity-worktrees/gascity/controller-human-gate-adapters`.
Branch: `implement/controller-human-gate-adapters-20260930`.
Base: `56dd2908846d94514a28f62ba3279b1d985f760b`.
The checks below initially covered uncommitted source. The final source commit
is recorded in this branch's history; this record does not assign a release pin
or qualify an installed controller/backend.

Implementation was divided between human-domain and retirement-composition source
subagents, then integrated at the Huma boundary. Independent spec and standards
reviews prompted fixes for transaction fences, interrupted-answer replay, legacy
route enforcement, prerequisite holds, retained-proof visibility, delivery-round
coverage and trusted protocol policy/context bindings. Final spec review found
no remaining concrete P0/P1 in the supported native-memory composition. The final
standards follow-up also fixed retirement HTTP failure-category projection and
made the malformed-manifest boundary test exercise an actual configured callback.

## Passing checks

Every Go invocation used `/home/ricky/gascity-pilot/.gc/scripts/go-bounded.sh` and its host-wide serial
lock. No raw Go test, vet or build invocation was used.

| Check | Evidence |
| --- | --- |
| Full domain/backend/verifier suites | `test ./internal/decisionfrontier ./internal/beads ./internal/retirementrelease -count=1` passed in the affected-package sweep. |
| Full API suite | `test ./internal/api -count=1` passed after classifying the new routes in the existing idempotency guard. |
| Generated Go client and schema drift | Full API and `./internal/api/genclient` suites passed with the regenerated source-derived artifacts. |
| Controller composition | `test ./cmd/gc -run '^TestControllerStateDecisionFrontier' -count=1` passed; disabled delivery retains the existing `unavailable` public projection. |
| Final signed flow regressions | `test ./internal/decisionfrontier ./internal/api -run '^(TestHumanDomain\|TestHumanSourceHTTP\|TestRetirementSourceHTTP\|TestDecisionFrontierAPI)' -count=1` passed after the final integration fixes. |
| Vet | `vet -buildvcs=false ./...` passed. The unstamped form was necessary because the environment's VCS-status lookup returned exit 128. |
| Build | `build -buildvcs=false ./cmd/gc ./internal/retirementrelease` passed; this is compilation evidence, not a qualified stamped release artifact. |
| Dashboard | Production, test and E2E TypeScript checks passed; generated client and embedded bundle were rebuilt. Dashboard SPA/BFF Go suites passed. |

OpenAPI, Go and TypeScript clients were each regenerated once after the public
interfaces were frozen. Later changes affected test triage and the existing
disabled-status projection, not the generated schema.

Signed deterministic fixtures exercise the real Huma handlers, request-bound
read/write authentication, canonical human verifier, protected frontier service
and native memory ledger. Coverage includes duplicate/changed/stale requests,
cross-scope signatures, worker-key rejection, backend generation changes behind
a stale reader, independent numbered rounds, held prerequisites, restarts,
non-answer policy, interrupted answer recovery, unavailable delivery, exact
resume revisions and proof nondisclosure. Retirement fixtures cover canonical
signed review/external-ledger verification, compatibility permission,
revocation/context/policy changes and invalid manifests.

## Qualification boundary

Native memory implements the joint-ledger transaction contracts. SQLite/remote
or wrapper backends lacking an explicitly qualified atomic handle remain
unavailable; protected row CAS alone is insufficient. Production source-flow
activation therefore remains held until its actual backend implements and passes
those contracts.

The retirement adapter performs supported canonical verification and reports
unsupported gates explicitly. Actual useful-outcome trial qualification,
boot-wide audit coverage, retirement/activation authority and effect, and the
remaining release-lane evidence have not been supplied. Fixture signatures and
successful tests do not establish operational readiness.

The remaining P2 boundary is external signing trust/time/provider execution and
future actions after an eligibility observation. Execution must revalidate its
own authority and revision fences. No source result is an execution permit.

## Final SPEC/STANDARDS follow-up

The shared private-record projection now covers generic API, CLI, store bridge,
event, convoy, workflow and run views, with copy-on-redact and raw authoritative
store/persistence/verification paths preserved. Actual CLI executable tests cover
text/JSON show/list, bridge get/list and unsafe export refusal. See
[`controller-private-projection.md`](controller-private-projection.md) for the
surface inventory. The three lifecycle-preflight fixtures have now been corrected
to return real issue-array read responses with nonzero revisions and preserve
mutation forwarding assertions; their focused and broader CLI sweeps pass without
failure exclusions. Production preflight rejection was not weakened.

`internal/retirementrelease/BUILD.bazel` declares the complete library/test inputs
and dependencies. The managed repository-tree metadata includes the package;
`tools/bazel/test_repo_tree.py` passes after synchronization.

The common HTTP budget is 1 MiB for the **whole serialized body**, shared by
source routes, write authentication and the retirement client's `HTTPEnvelope`.
Boundary/escaping tests and signed HTTP 413 rejection pass. Larger standalone
retained-file limits are not HTTP capabilities; no global limit was raised.

The tagged disposable socket harness and driver pass through real Huma handlers,
Ed25519 city grants, separate signed human answers, native memory transactions,
held/interrupted retries, resume checks and canonical retirement verification.
Its protocol is in
[`controller-cross-language-harness.md`](controller-cross-language-harness.md).
The unmodified actual Python human consumer now passes all seven cases, including
read-only snapshots, generation/source changes and tenant refusal. The unmodified
retirement wire consumer passes all eight exported signed-fixture cases, retaining
fixture assurance and false activation/trial qualification. Exact commands are in
the two harness documents. In-process memory reconstruction does not establish
durable process restart.

Final affected-package suites passed for beads, retirement, API and generated Go
client. Executable projection/bridge/controller tests, the raw-city routing
regression and tagged socket driver passed. Bounded repository-wide vet and the
CLI/retirement build passed with VCS stamping disabled. Updated specs and
Go/TypeScript clients were regenerated; dashboard production/test/E2E type checks
passed. Unsupported production Beads/Dolt atomic contracts remain fail-closed.

The initial validation performed no live keys, services, admissions, nudges,
trials, runtime no-mistakes execution, GitHub mutations or commits. Pack code and
other worktrees were not modified. Final pack protocol mapping is in
[`controller-source-http-interface.md`](controller-source-http-interface.md).

## Codex commit follow-up — 2026-10-01

Codex resumed the stopped commit task after its final `adapter.go` lint patch.
The pending fixes preserve behavior while removing unused results and builtin
shadowing, normalizing spelling, and distinguishing a failed qualification
snapshot from an unavailable snapshot without wrapping a nil error.

Bounded full retirement and decision-frontier suites passed, together with
focused backend, human/retirement HTTP and CLI regression checks. The normal
commit hooks then reported zero lint issues and passed code generation and vet.
The docs gate exposed these eight engineering records under the published
Mintlify tree; they were relocated to `engdocs/contributors/` with their relative
links preserved. No docs gate or hook was bypassed.

Both origin fetch and push URLs were verified as
`https://github.com/rhar1511/gascity.git`. Source commit completion does not
authorize a push, production activation, script retirement, or a live trial.

## PR23 reconciliation — 2026-10-02

The isolated controller branch reconciles exact PR23 head
`fd452caaadf7e0b1bac7f854e1c3a237aa743106` with reviewed controller source
`65002d16648b2bef2df18b1dac949375706fcd4b`. No foreign worktree or live runtime
was changed. The resolved source preserves signed work revisions, reciprocal
claim-generation fences, private-evidence context/capability checks and separate
proxy/Dolt process and listener ownership.

Existing tests reproduced missing acknowledgement guidance and refusal of the
durable proxied-server context. The shared envelope and context helpers now
retain both histories. Exact retained legacy transcript envelopes still match;
changed fields, extra fields and wrong execution identity remain rejected.
The API claim-rollover fixture now stamps its matching claim generation rather
than weakening production attribution checks. Focused session, Beads, worker,
lifecycle-recovery and API checks pass after the reproduced failures.

Dashboard production/test/E2E typechecks, build, serving and smoke checks pass.
Its Bazel asset inputs match the regenerated bundle. Bazel execution itself is
unavailable on this host and is not claimed as passing.

The first graph check stopped during a pinned-tool download because DNS was
unavailable. The supported installed Beads 1.3.0 override avoids that download.
With subprocess/owned-run/isolated-Dolt settings, the graph assertions passed but
exposed cleanup refusal when no supervisor had been started. Owned cleanup now
checks its process census before requesting a stop and independently rechecks
absence before root removal. The graph and bounded supervisor-timeout checks
then passed (6.481s). Uncertain census or remaining owned processes still refuse
cleanup; cross-run sweeps are not enabled.

These are source-integration checks, not proof of a production atomic backend,
an accepted merge, a qualified trial or permission to disable WIP orders.

Standards follow-up found an inherited Pi-reset write race. A new filesystem
regression reproduced both a lexical-root swap writing outside the retained
root and an escaping parent-directory swap being accepted, with mirrors
disabled. Native replacement now uses the retained `os.Root` for temporary
creation, permission enforcement and rename; temporary creation is exclusive.
Both swap cases and the existing mirror-failure behavior pass after the fix.
Discovery-side Gemini/OpenCode/ZCode reads share one root-confined opening
helper. The graph proxy wait now uses the existing context-bounded polling
helper instead of a separate timer/ticker loop.

The preliminary hook run completed lint (zero issues), code generation, vet
and docs checks, then was intentionally stopped before dashboard processing to
run the newly added red/green regression. It is not a completed hook pass; the
normal commit must rerun the full mandatory hooks on the final source.

### Exact-ref qualification

The operator explicitly authorized temporary-ref qualification in
`rhar1511/gascity` and gated landing of reviewed, CI-green head/base pairs.
The published GitHub GraphQL schema describes `updateRefs` as an atomic
transaction with an expected `beforeOid` for each ref. This differs from the
head-only REST merge operation, which the controller still refuses.

Two temporary refs, `qualification/exact-merge-20261002-base` and
`qualification/exact-merge-20261002-head`, were created atomically with expected
absence. Their initial OIDs were `284fd816f16de5a09db2f55ef26eb262bd71a31e`
and `fd452caaadf7e0b1bac7f854e1c3a237aa743106`. A matching pair advanced the
base ref to `90a0a050188c43b3884530c6cf5aee2cae5f8cff` while requiring the
head ref to remain at the exact approved OID. Both updates used `force:false`.

Stale head and stale base requests were rejected. Independent readback showed
neither paired operation partially changed the refs, including a stale-base
request paired with deletion of the head test ref. GitHub returned opaque
GraphQL errors for stale expectations rather than a stable conflict category;
such an error must remain failed/unknown pending exact readback, never a reason
for blind retries. The earlier non-fast-forward rejection alone is not relied
on as proof of the expected-head check.

Both temporary refs were then deleted atomically with their exact current OIDs,
and readback confirmed both absent. They can be recreated from the retained
immutable commits. No actual PR, main branch or live city was modified.

This qualifies the ref primitive only. Landing still requires an exact reviewed
merge artifact, passing current-head CI, repository/PR policy and independent
PR/commit readback. The existing controller merge capability remains disabled;
this experiment did not change trusted production composition or trial gates.

## Current-main reconciliation — 2026-10-02

The same isolated branch now reconciles main
`90a0a050188c43b3884530c6cf5aee2cae5f8cff` with controller merge
`5a53160f133727b4e1bff567594f3dfdf4aec9da`. Both histories are retained.
This is distinct from the subsequent reviewed upstream-baseline assembly.

Session receipts retain the incoming immutable target, execution-token and purge
fences together with the fork's append-only event ledger and exact-attempt
attribution. Literal and encoded retained keys remain readable without migration;
duplicate identities and missing historical delivery targets fail closed.
Generic acceptance replay remains immutable after churn, while attempt-bound and
cached HTTP replay recheck current claims. Acknowledgements require a prior
delivery reservation and the current execution credential.

The merged CLI/bridge keeps controller-owned metadata, signed session authority,
enrolled-work guards, private internal reads and exact-revision deletion.
Attempt capture may advance its private index, but public-field or claim changes
still refuse closure; timestamp comparison preserves instants across file-store
JSON round trips. Native Dolt keeps transaction-fenced parent and label changes.
RSI preserves trusted per-gate context and controller-reserved execution identity.

The production RSI formula stays outside the core catalog. Its independent-lane
prompt contract is tested in the private pack, whose exact-copy provenance now
pins the incoming source at `ae572374330af0d0742cc24584bf386ed4797234` with SHA-256
`4270cec24a63d4986fc39b8bb9c2c4448bbbbf715ded897af046deffe0b155f3`.
Controller conformance accepts only a verified scratch snapshot of those bytes.
Neither this source pin nor fixture conformance is activation authority.

Existing regressions reproduced the merge mismatches before fixes: delivery
fixtures omitted reservations; public reads expected private receipt storage;
bridge delete fixtures omitted revisions; ledger-bearing CLI output failed its
schemas; the metadata registry did not cover retained controller keys. Tests now
exercise the stronger merged contracts rather than bypassing those guards.
The metadata registry distinguishes actual keys from transcript hash domains.
Build manifests include ledger/transcript inputs and the session-authority
package, and the repository-source inventory check passes.

Bounded focused API and CLI sweeps pass, including cache/claim renewal, legacy
target refusal, receipt/ledger schemas, private projections and bridge guards.
The full API suite passes (93.909s). Dashboard production/test/E2E checks, embedded
bundle build and serving smoke pass. The remaining full suites pass: Beads
75.128s, session 3.867s, worker 21.283s, dispatch 2.821s, metadata registry
0.886s, control grants 0.033s and session authority 0.006s. The worker privacy
test now identifies the stored request by its persisted identity instead of
assuming that a hashed key contains the ID; all opacity and handle-recreation
assertions remain. Independent review and normal hooks must still pass before
this candidate can be published and landed.
No current-main reconciliation result is an accepted PR merge or permission to
disable WIP dispatch.

### Review corrections after the first reconciliation sweep

The normal commit was deliberately stopped during lint when independent review
found correctness defects. Its incomplete hook invocation is not a hook pass.
The following changes must be included in the final reviewed merge tree:

- Native parent/label CAS changes now also change a reserved auxiliary nonce in
  the issue row, in the same transaction. The pinned backend's auxiliary tables
  do not themselves mint an issue revision. A fixture that models this weaker
  behavior reproduced the reusable-token failure before the fix.
- `gc.claimed_at` is controller-reserved: generic clearing cannot re-enable the
  write-once RSI execution stamp. The generic-key regression failed before this
  change and passed afterward.
- A retained attempt-bound receipt cannot replay through generic acceptance
  after its reciprocal claim was cleared. Exact historical reads remain valid.
  Both implicit and explicit cached HTTP selectors exercise this refusal.
- CLI and API close capture share a checked refresh of the exact inspected row.
  A newly written evidence reference must match its sealed archive and original
  session/claim identity. Only the verified capture fields and private indexes
  may change; final writes retain the returned revision fence.
- Conditional deletion owns reference cleanup rather than performing unfenced
  `DepRemove` calls before the checked delete. The previous sequence reproduced
  a stale revision after its own edge removal. Protected surviving edge owners
  remain guarded. Ordinary memory-store deletion retains its prior dangling-edge
  behavior; conditional deletion is the explicit cascade contract.
- Backend session identity writes use an explicit internal transport and a
  session-owned conditional write. This is not a human grant or a worker API;
  public mutations still reject identity keys even with the transport marker.
  Other reserved authority, receipt, purge and RSI keys remain refused.

The first full follow-up run failed in session-wrapper capability resolution and
memory-store deletion compatibility, while evidence and metadata packages passed.
The wrapper now resolves CAS on its underlying store; ordinary deletion no
longer changes cache/dangling-edge semantics. Focused follow-up session and Beads
regressions passed (0.095s and 5.586s). The prior full-suite passes above predate
these corrections and do not qualify the new final source.

The initial real native auxiliary test failed explicitly because the default
backend selected embedded Dolt without CGO. It was not skipped or counted as a
backend pass. The new auxiliary qualification instead uses a disposable local
SQL-server listener/database; its final result is recorded separately. No test
uses the live city's Dolt listener or modifies production storage.

SQLite deletion now plans sources before targets so a cascade cannot invalidate
another selected row's already-verified revision. Cycles are refused before
closing/fencing/deleting any selected row, not repaired by accepting refreshed
revisions. Connected SQLite CLI/API purge and cycle-no-write regressions cover
this distinction. Final independent review, broader affected suites and complete
normal hooks are still required.

### Terminal purge and cache follow-up qualification

The surviving-reference contract now includes field-only ancestry. Deletion
planning unions `ParentID` with explicit edges, while checked deletion guards
current owners inside the memory/file lock or native/SQLite transaction. A
closed target cannot be purged if a reference owner is no longer closed. A
permitted cascade clears physical parent fields and mints fresh surviving-owner
revisions. Purge verification requires every selected row to remain closed;
the API stops at the first refused delete rather than deleting its ancestors.

The shared terminal-owner conformance case failed on all five compositions
before their guards were added. The parent-only case then exposed SQLite's
independent `parent_id` column: its incoming census now unions that column with
the dependency table and updates both column and retained JSON during cleanup.
The shared parent case covers active-owner refusal, allowed closed-owner
cleanup, fresh revisions and rejection of the old owner token. API
interleavings cover initial and late-added edge/parent references.

Cache conformance also reproduced a stale surviving-owner token after an
owner's conditional update followed by `Get`. Conditional eviction now marks
dependency coverage incomplete; a later cascade refreshes affected owners
rather than accepting a dependency-omitting row as complete coverage. The
strengthened shared case passed (5.045s) before the terminal-owner additions.

The first broader follow-up passed Beads (90.984s), session (6.617s), evidence
(2.273s), and metadata (1.118s), but API failed (108.372s): its client fixture
acknowledged before asynchronous delivery was reserved. It now waits for the
existing correlated delivery event. Both receipt clients reuse the generated
client and full guarded handler through the existing in-process transport,
removing two redundant sockets without bypassing request construction or
middleware. An initial fixture host was correctly rejected with 421; the
fixture now uses the already-allowed loopback host. These failures are retained
in `/var/tmp/controller-review-final-domain-suites.log` and
`/var/tmp/controller-terminal-and-client-final-green.log`; neither command is
an overall pass. Final affected-suite results are recorded after completion.

The source resource-census gate is still red on reconciled/inherited growth.
No ratchet, waiver expiry, or classification threshold was relaxed. The
combined upstream/controller artifact must pass that gate before publication.
The isolated native server proof and normal hooks are separate from production
atomic-frontier capability and the operational WIP cutover gates.

The final affected-domain run passed API (108.776s), Beads (67.727s), session
(3.769s), attempt evidence (1.924s), and metadata (1.091s); its complete log is
`/var/tmp/controller-closed-owner-qualified-domain.log`.

The isolated native terminal-cascade proof initially stopped at the normal
parent-close guard because its child was open. Its corrected fixture closes
both rows through normal operations, then reopens the child through checked
CAS before testing refusal. The unchanged active-owner refusal, retained-edge,
allowed closed-owner cascade, fresh-revision and stale-token assertions passed
against the disposable real Dolt server (13.169s), recorded in
`/var/tmp/controller-terminal-native-reopened-child.log`. This is not a
production atomic-frontier or live-trial qualification.

The focused CLI run also exposed unversioned SQLite fixture rows. Explicitly
closed creation preserves that status but leaves revision zero; subsequent
close calls are no-ops. The fixture now creates open rows and closes
leaf/child/root before purge so real transitions mint checked revisions. Both
failed setup runs remain recorded in
`/var/tmp/controller-closed-owner-qualified-cli.log` and
`/var/tmp/controller-terminal-cli-closed-fixture.log`. The final focused CLI
close-capture, bridge, purge, cycle and SQLite checked-delete sweep passed
(1.603s), recorded in `/var/tmp/controller-terminal-cli-versioned-fixture.log`.

Read-only standards and spec follow-ups found no unresolved hard violations or
P0-P2 findings in the final test-only corrections. Complete normal hooks and
the separate upstream-combined resource-census gate remain required. No result
here authorizes publication, script retirement or runtime activation by itself.

### Normal-hook lint and broader fixture follow-up

Two normal commit attempts completed with exit 1 in staged lint; neither is a
hook pass or an interrupted command. Logs are
`/var/tmp/controller-main-reconciliation-commit-final.log` and
`/var/tmp/controller-main-reconciliation-commit-6g.log`. The four findings were
an unreachable duplicate argument case, missing exported-handle comments, a
builtin-shadowing test local, and redundant boolean negation in prefix
validation. Their corrections preserve behavior. The lint formatter's
equivalent receipt-test switch is retained. Full control-grant tests passed
(0.047s), recorded in `/var/tmp/controller-ledger-prefix-hook-regressions.log`.

The broader regression command in
`/var/tmp/controller-hook-lint-fixes-regressions.log` passed Beads (3.987s) and
its selected control-grant tests (0.032s), but CLI failed (24.656s) with six
failures. This is not a full CLI pass. Receipt-specific refusal diagnostics,
fake writers without conditional capability, Unix socket path length, hidden
pool-route CAS capability, and unversioned GC seeds require distinct fixture
or composition corrections; production guards must not be weakened to make
these fixtures pass.

GC seeds now model persisted rows with explicit initial revisions, without
mutating the caller's seed slice. The first targeted run in
`/var/tmp/controller-versioned-gc-fixture-regressions.log` still failed: it
exposed old expectations that an active external reference could be orphaned
and that incidental parent ordering governed partial deletion. Revised tests
require refusal with the external edge preserved, retry only after external
completion, and leaf-first partial progress with remaining parents retained.
The complete GC group and receipt-diagnostic result are recorded separately
after completion. These are test-contract corrections, not relaxation of
terminal deletion, reference-owner or revision fences.

The complete GC rerun also exposed an inherited spy assertion requiring
separate dependency removal. The checked deletion contract now owns that
cleanup atomically; the corrected test prohibits unfenced batch deletion and
direct edge removal, and checks both edge directions for every deleted row.
The initial complete run remains in
`/var/tmp/controller-versioned-gc-complete-regressions.log` as a failure.
The final complete GC group plus receipt-diagnostic run passed (0.809s), in
`/var/tmp/controller-versioned-gc-complete-regressions-v2.log`. Read-only
standards and spec reviewers found no remaining hard violations or P0-P2
findings in these four fixture/comment deltas. Unsupported fake-writer,
socket-length and pool-capability cases remain outstanding before combined
publication, alongside the unchanged resource-census gate.
