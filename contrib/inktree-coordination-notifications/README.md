# Inktree coordination notification policy

This contribution supplies Inktree's consumer policy for the role-neutral
`internal/coordinationnotify` primitives. It maps known router reason strings
to the released coordination reason codes, assigns fixed neutral copy, and
keeps both `live_dispatch` and `notification_dispatch` off. The pack is replay
only by default; it defines no order, worker, or route action. Its bounded
Forgejo canary command requires an explicit synthetic issue, token, and
notification-only override, while route dispatch remains off.

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

## Synthetic Forgejo canary

After offline evidence and exact-head review pass, the authorized synthetic
canary can target one open `req-syn-*` issue whose author is attested by the
same Forgejo token. The command turns on notification delivery for that run
only; `live_dispatch` remains off. It refuses an unattested author, records no
credentials or author identifier, posts fixed rendered copy, and uses both the
stable notification ID and a durable ledger snapshot to suppress a same-epoch
re-fire.

```sh
FORGEJO_TOKEN_FILE=/path/to/mode-600-token
GIT_DIR="$(git rev-parse --git-dir)" GIT_WORK_TREE="$PWD" \
  go run ./contrib/inktree-coordination-notifications/cmd/forgejo-canary \
  --token-file "$FORGEJO_TOKEN_FILE" \
  --repo inktri/inktree --issue <synthetic-issue-number> \
  --request-id req-syn-0003 --bead-id inktree-syn0003 \
  --authorization /path/to/canary-authorization.json \
  --verification contrib/inktree-coordination-notifications/evidence/verification.json \
  --out /path/outside/worktree/forgejo-canary.json
```

The mode-0400 authorization document and output must remain outside the clean
reviewed worktree, and the output directory must be mode 0700 and owned by the
operator. The authorization document must bind the synthetic request,
repository, issue, synthetic bead, opaque binding ID, and exact reviewed 40-hex
source head, and explicitly attest the offline gates, independent review, and
synthetic canary authorization. The command also verifies the retained
policy/corpus hashes and refuses redirects, unrelated comments, arbitrary HTTPS
origins, physical paths entering the worktree, and source-head drift before any
write. It also verifies that the running Go binary was built from that clean
reviewed head. Run the exact command twice. The first run must retain one
Forgejo comment receipt; the second must retain
`same_epoch_refire_suppressed: true` without a second comment. This command is
not a live route or worker dispatch path.

The authorization file is an operator-local control in the same trust domain as
the mode-0600 token; processes running as that operator can access both. A
durable intent is written and synced before the sole POST, then retained as a
completed target tombstone. If the POST outcome is uncertain and no comment is
visible, the command fails closed and will not retry that request. Do not delete
the intent to force a retry: inspect Forgejo
and the retained state, then authorize a new synthetic request if delivery was
not accepted.

```json
{
  "version": "inktree-forgejo-canary-authorization/v1",
  "request_id": "req-syn-0003",
  "bead_id": "inktree-syn0003",
  "repository": "inktri/inktree",
  "issue": 1234,
  "binding_id": "binding-0123456789abcdef0123456789abcdef",
  "reviewed_head": "0123456789abcdef0123456789abcdef01234567",
  "verification_sha256": "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
  "offline_gates_passed": true,
  "independent_review_passed": true,
  "synthetic_canary_authorized": true
}
```

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
