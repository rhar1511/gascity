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
