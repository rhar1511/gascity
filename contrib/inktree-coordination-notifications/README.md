# Inktree coordination notification policy

This contribution supplies Inktree's consumer policy for the role-neutral
`internal/coordinationnotify` primitives. It maps known router reason strings
to the released coordination reason codes, assigns fixed neutral copy, and
keeps both `live_dispatch` and `notification_dispatch` off. The pack is replay
only; it defines no order, transport binding, worker, route, or delivery
action.

The generic package owns deterministic envelope construction, assertion and
record projections, fixed-template rendering, switch validation, and bounded
event-addressed retry receipts. This directory owns the Inktree reason
vocabulary and copy. Unknown reason strings and switch values fail closed.

## Offline evidence

The evidence generator replays the seven retained synthetic cases in
`replay/corpus.v1.json`, hashes the exact corpus, each complete canonical replay
input, the policy document, and each resulting projection, and verifies those
projections against retained hashes before writing an offline receipt fixture.
It does not create a Forgejo delivery receipt or run a live canary. No live
dispatch or consumer overlay is included in this pack.
`evidence/verification.json` records which checks ran in the implementation
environment.

Regenerate the files from the repository root with:

```sh
go run ./contrib/inktree-coordination-notifications/cmd/evidence \
  --corpus contrib/inktree-coordination-notifications/replay/corpus.v1.json \
  --policy contrib/inktree-coordination-notifications/policy.json \
  --out contrib/inktree-coordination-notifications/evidence
```

Run focused tests with:

```sh
go test ./internal/coordinationnotify \
  ./contrib/inktree-coordination-notifications/...
```

The live consumer still needs a separately reviewed ordinary-pack adapter and
operator-managed identity/channel bindings. This contribution does not install
itself or alter a city.

## HITL contract for the future consumer adapter

The adapter must create envelopes from the accepted event-triggered order and
ordinary-pack seam. It must not add a controller fork or another routing path.
The semantic notification key remains `coord-<status>-<bead_id>` plus
`hold_epoch`; each transport attempt is identified by its opaque event ID and
destination. Accepted retries must satisfy the retained attempt bound,
cumulative backoff schedule, and delivery deadline. Suppressed receipts are
audit records and never restore accepted dedup state. The canonical identity is
the Gas City actor ID, resolved through source-author attestation and
administrator-managed channel bindings.

Retained `binding_id` values use `binding-<32 lowercase hex>` record keys. They
are neither handles nor caller-provided labels, and adapters resolve them only
through the operator-managed binding store.

Delivery requires explicit opt-in for both the request and the selected
channel. Practice routing delivers nothing. A preview shows the resolved
recipient and fixed wording before delivery, and every replay rechecks current
consent so a revoked grant cannot be reused. Administrative or safety notices
need separate, documented authority; ordinary coordination consent does not
authorize them.

For pull requests, update one durable summary. Append a new comment only for a
new hold epoch, an escalation, or final resolution. The adapter must preserve
these update rules across retries and deduplication.

The pure `computeEnvelope` path must meet p95 <= 100 ms and p99 <= 250 ms at
twice projected peak over seven representative days, excluding model and
network latency. Consider Rust only after in-process profiling, 100% replay
parity, at least 30% improvement in the failing percentile, and operational
and degraded-mode acceptance.

The existing seven-case replay corpus must remain intact. Add cases for
duplicate reminders, revoked consent, practice routing, PR summary updates,
unknown identity bindings, adapter timeout and retry, newer-schema readers,
and malformed or malicious payloads. These are consumer-adapter gates; this
offline contribution does not claim to implement or pass them. Both dispatch
switches remain off until the separately reviewed adapter and its gates are
accepted.
