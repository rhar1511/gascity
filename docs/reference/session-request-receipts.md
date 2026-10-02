---
title: "Session request receipts"
description: "Track request acceptance, provider submission, and acknowledgement by an exact session execution."
---

# Session request receipts

A tracked request names a durable session ID, request ID, and execution
generation. The server stores acceptance before scheduling provider delivery.
Acceptance also binds the original runtime session name and execution credential
digest; metadata changes cannot retarget delivery. New receipts store keys derived
from the full SHA-256 of the public request ID. Retained literal-ID and encoded
keys remain readable without migration; duplicate identities fail closed. An old
pending receipt without its original target cannot be delivered by inference.

It reserves the send before contacting the provider, so retrying the same
request does not send it twice. A crash after reservation leaves an unknown
outcome; submitting the same ID reads that outcome rather than retrying it.

```bash
gc session request submit gc-123 request-456 --generation 2 "report progress"
gc session request get gc-123 request-456
```

Tracked delivery targets an already live execution. It cannot wake, resume,
interrupt, or restart a session. Reusing a request ID with a different message
or generation is rejected. These commands require the central server and do
not fall back to local mutation when it is unavailable.

Permanent workflow deletion preflights all selected rows, acquires a
revision-guarded purge fence on every selected session, closes the rows, and
then checks the complete set again before removing dependencies or rows. The
fence rejects request acceptance and reopen, and a request based on an older
revision cannot commit after fence acquisition. While active it also protects
the row against type, session-identity metadata, and non-closing status changes;
these checks key only on the fence, not on the row's mutable type. Post-close
verification tracks each acquired fence by bead ID and exact owner token. On an
abort before dependency removal, newly acquired fences are conditionally
cleared; any rollback failure
leaves the affected closed session fenced rather than risking evidence loss.
Once irreversible deletion starts, a surviving row remains fenced if a later
store operation fails and the response reports a partial purge. Each final row
delete is conditional on the revision observed by the post-close verification,
so reconciler or other concurrent mutation cannot turn a checked row live and
then have the purge erase it.


After receiving the request, the intended execution acknowledges it. The envelope
includes an exact `acknowledge_with` command and explains that acknowledgement
confirms receipt only, not the requested effect:

```bash
gc session request ack request-456
```

The delivery envelope includes `--` before the request ID so IDs beginning
with a hyphen are treated as positional arguments by the CLI.

The command takes its identity from `GC_SESSION_ID`, `GC_RUNTIME_EPOCH`, and
`GC_INSTANCE_TOKEN`. Credentials are sent in a header and are absent from the
public receipt. The server checks the exact request, generation, and current
execution credential and prior provider-attempt reservation; an old execution cannot acknowledge a replacement's
request. Repeating an acknowledgement preserves its original timestamp.

## HTTP surface

- `POST /v0/city/{city}/session/{id}/requests` accepts `request_id`,
  `generation`, and `message`, returning a durable receipt with HTTP 202. An
  optional `Idempotency-Key` preserves the exact HTTP response on replay while
  the durable request ID still governs delivery. Worker/provider resolution
  happens after acceptance; failure before reservation leaves delivery pending.
  An explicit Workbench selector supplies both `work_id` and `claim_generation`;
  the controller verifies rather than trusts those selectors.
- `GET /v0/city/{city}/session/{id}/requests/{request_id}` reads that exact
  receipt, including after session closure or a generation change.
- `POST /v0/city/{city}/session/{id}/requests/{request_id}/ack` accepts
  `generation` and the `X-GC-Session-Token` header.

The normal city write authentication and CSRF checks still apply. The session
credential proves the execution identity in addition to those checks.

## Reading a receipt

`accepted_at` records server acceptance. `delivery_attempted_at` records the
reserved provider attempt; `delivery` and `provider_result_at` describe its
result. Neither stage sets `acknowledged_at`. Only an acknowledgement from the
intended execution sets that timestamp. `effect` remains `unverified`: receipt
of a request is not proof that its requested work succeeded.

Historical reads never substitute the latest request or generation. A corrupt
or unavailable record returns an error. Session plus generation alone is not
enough to attribute a request to a work attempt, because a persistent session
can serve several attempts.

When the session has a current work claim, the controller derives an optional
`attempt` binding from that work's authoritative store, owner, session and claim
generation. It records the original work revision as a decimal string. The
submit body cannot supply this binding. Acceptance and delivery check the
session's reciprocal work claim; a changed claim rejects the request.

The binding is an immutable observation of the selected attempt when the
request is accepted. It does not grant ownership, establish an atomic
transaction across the work and session stores, or authorize an effect. The
acknowledgement is session-execution scoped: the exact session generation and
credential may acknowledge receipt even after the session's current Bead claim
changes. That does not update the original attempt binding, assert that the
claim is still current, or prove the requested work took effect. Bound replays recheck the current reciprocal claim before scheduling delivery,
even when an HTTP response is cached; they preserve the original binding and
revision. Unattributed exact replays can read historical results after session
churn or closure. Requests accepted without a
binding remain unattributed, including after a later work claim.

An authorized exact attempt read includes matching receipts under
`related_records.acknowledgements.records`. The join checks the attempt ID,
original store and full execution identity. It works after the work row is
deleted, while the session receipt ledger is retained. Unattributed requests
are counted separately and never assigned to an attempt by inference. Missing
receipts and unavailable or corrupt evidence remain distinct.

Generic bead create/update endpoints reject session receipt metadata. Receipt
creation and acknowledgement must use the tracked session protocol.

## Append-only request ledger

New receipts include an append-only event ledger bound to the exact session,
execution generation, request ID, and message digest. Its folded view is also
attached to structured transcript responses beside the live provider
transcript. Acceptance, delivery reservation, provider result, session
acknowledgement, and the unverified effect remain separate events. Unknown
provider outcomes stay recorded and are never retried implicitly.

Attempt attribution in the ledger is derived from the controller-verified
`attempt` binding when one is available. Otherwise the ledger records an
explicit unavailable reason. Legacy version-1 receipts remain readable, but
their event history is explicitly unavailable; the server does not invent
events from flattened fields.

The ledger also persists exact, generation-bound references to provider
transcript entries and tool events. It stores references and digests rather
than provider payload copies. Missing, ambiguous, stale, or unavailable
references remain explicit.

## Deployment requirements

Writes require a backend with conditional row updates. The protocol refuses
an unconditional fallback. Runtime credentials must be isolated: an actor
that can read another execution's token can impersonate it. The in-process
session lock coordinates normal incarnation changes but does not fence an
arbitrary external writer or runtime replacement. Direct backend write access
can alter receipt metadata and must be restricted. These deployment boundaries
must be verified before using receipts as recovery evidence.

Receipts live on the durable session record. The session close/delete API refuses
to delete records containing receipts until an archive policy is implemented.
Direct backend deletion or automatic backend TTL remains outside that guard;
deployments must preserve the records before enabling this protocol. A tracked request does not itself enable recovery or grant
permission for an operation.
