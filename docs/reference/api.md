---
title: Supervisor REST API
description: The typed HTTP + SSE control plane exposed by the `gc` supervisor.
---

The `gc` supervisor exposes a single, typed HTTP control plane
described by an OpenAPI 3.1 document. Everything the CLI does, any
third-party client can do too — there is no hidden surface.

See also: [The six primitives](/getting-started/how-gas-city-works) — the canonical model this
API projects over.

## Get the spec

- **<a href="https://raw.githubusercontent.com/gastownhall/gascity/main/docs/reference/schema/openapi.json" target="_blank" rel="noopener">openapi.json</a>** —
  the authoritative contract. Drop it into Stoplight, Postman,
  Swagger UI, or any OpenAPI-aware tool to browse operations
  interactively.
- **<a href="https://raw.githubusercontent.com/gastownhall/gascity/main/docs/reference/schema/events.json" target="_blank" rel="noopener">events.json</a>** —
  the `gc events` JSONL line schema. It references DTO components in
  `openapi.json`, so the API remains the source of truth.

## Endpoint families

The spec is the full reference. A brief summary of the surfaces:

- **Cities.** `GET /v0/cities`, `POST /v0/city`,
  `GET /v0/city/{cityName}`, `GET /v0/city/{cityName}/status`,
  `GET /v0/city/{cityName}/readiness`,
  `POST /v0/city/{cityName}/stop`.
- **Health & readiness.** `GET /health`, `GET /v0/readiness`,
  `GET /v0/provider-readiness`.
- **Agents.** `GET/POST/DELETE` under `/v0/city/{cityName}/agents`
  plus SSE `/v0/city/{cityName}/agents/{agent}/output/stream`.
- **Beads (work units).** CRUD under `/v0/city/{cityName}/beads`,
  query + hook operations, dependencies, labels.
- **Sessions.** CRUD under `/v0/city/{cityName}/sessions`, submit,
  prompt, resume, interaction response, transcript, SSE stream.
- **Connected-client external messaging.** `POST /v0/extmsg/clients`
  registers an external LLM client and returns a bearer token.
  `POST /v0/extmsg/inbound` (with `provider: "llm-client"`) delivers an
  inbound turn from the registered client to a city session.
  `GET /v0/extmsg/{provider}/{account_id}/{conversation_id}/subscribe`
  opens a long-lived SSE reply stream for that conversation.
  See [Connect an external LLM client](/guides/connected-clients) for
  the full integration guide including the SSE error catalog.
- **Mail, convoys, orders, formulas, participants,
  transcripts, adapters.** External messaging and orchestration
  surfaces; see the spec for per-operation shapes.
- **Events.** `GET /v0/events` + `GET /v0/events/stream` at
  supervisor scope, and `GET /v0/city/{cityName}/events` +
  `GET /v0/city/{cityName}/events/stream` at city scope.
- **Config & packs.** Per-city config and pack metadata under
  `/v0/city/{cityName}/config` and `/v0/city/{cityName}/packs`.
- **Pull-request actions.** `GET /v0/city/{cityName}/pr-actions/queue`
  returns fresh monitored PR state joined with durable repair work and exact
  immutable attempt-evidence references. `POST /v0/city/{cityName}/pr-actions`
  prepares repair work, records an exact revision for review, or merges after
  separate human approval. See the trust requirements below.

### Historical attempt reads

`GET /v0/city/{cityName}/bead/{id}/attempt-evidence/{attemptID}` reads one
immutable attempt; omitting the final attempt ID lists retained attempts.
These routes use the archived original store, work, repository, and workspace
scope even after the owner is deleted. They never resolve a latest-attempt alias.

The default authorizer requires a verified `X-GC-City-Read` grant with a nonempty
`sub` identifying the authenticated reader and a `read_scopes` array covering
every returned attempt. A city-only grant is insufficient. The existing
permission authority must authenticate the reader and check inherited access
against trusted original permission records before signing. Caller-supplied
paths, account names, or scope hashes are not permission evidence. Keep the
issuer's private key out of worker environments; the controller verifies only.

Each array entry is `gc-attempt-read.v1:` followed by the lowercase hexadecimal
SHA-256 of these exact UTF-8 fields joined by NUL: `gc-attempt-read.v1`, archived
store reference, work ID, repository root, workspace root, and attempt ID.
Fields must be nonempty and contain no NUL. The controller's
`attemptevidence.ReadGrantScope` helper implements this contract. Use the exact
archived spellings, including the original scope when current ownership changes.
A list grant must cover all listed archives; partial permission returns no list.

The envelope retains `aud=gc-city-read`, exact method/path/query binding,
single-use `jti`, the two-minute maximum lifetime, and the configured epoch floor.
Retries require fresh grants. Set `GC_CITY_READ_CID` to the deployment's
tenant-specific city identity when signers are shared across tenants; grants
with missing or different CID are then rejected. An unbound CID claim is not
proof of tenancy. `GC_CITY_READ_PUBKEY` or `read_auth_verify_key` installs the
trusted read authority, and `GC_CITY_READ_REQUIRED=1` makes absent trust a startup
error. Without verified read identity the historical routes remain unavailable.

An explicitly composed `AttemptEvidenceReadAuthorizerProvider` can enforce a
deployment's permission integration; returning no authorizer keeps reads disabled.
The bundled clients do not mint grants. Their deployment must supply them through
the permission authority. Configuring verification alone does not install or
validate that issuer, backups, or the release's inherited-permission integration.

### Pull-request action trust

The central controller computes the permitted actions; clients cannot submit a
role, approval boolean, policy, base/head revision, or evidence verdict as
authority. Action requests repeat the current queue's monitor, repository, PR,
head SHA, base SHA, policy version, work and attempt IDs, and provide an
`Idempotency-Key` header. The controller reloads work and forge state before
recording an intent and again before execution. A change or source outage makes
the action stale or unavailable. An idempotent replay returns its stored result
and does not repeat an effect.

`prepare` creates at most one durable repair-work bead for the exact monitor,
repository, PR, base SHA, and candidate SHA, even if concurrent callers use
different idempotency keys. The repair bead is a held triage candidate with
`hold:external`; its route is recorded only as a proposal. It is not runnable
until the separate signed lifecycle-admission path removes that hold and
installs a serving route. PR review or merge authority does not grant admission.
`queue_review` records that the exact revision is ready for review only when an
immutable attempt reference matches its current head and base. `merge` also
requires successful checks and a signed human grant. Queueing for review never
authorizes a merge. The GitHub adapter currently advertises merge as unavailable
and sends no merge request because GitHub's merge API cannot atomically require
the approved base SHA as well as the head SHA. A future adapter must enforce
both revisions before this action can become available. Before any enabled
adapter is called, the controller persists a non-retryable merge-submission
state. An unverified outcome stays `unknown`; even after the claim lease expires,
retrying the same key never repeats a merge submission.

Action receipts are stored as non-runnable controller ledger records and ordinary
city bead create/update/assign/close/reopen/delete routes reject changes to them.
This API protection does not replace host-level controls for direct access to
the underlying bead backend.

Every mutating action requires a verified city-write grant. Merge approval uses
a different Ed25519 keyring and issuer/subject map from `GC_PR_HUMAN_TRUST`, a
host-managed supervisor environment value. Each approval signature binds the
city, action scope, repository, PR, exact head/base SHAs, policy version, work
ID, idempotency key, and a short expiry. The configured human key must not
overlap a city-write key. A worker's city-write signature and a human-looking
`sub` claim do not establish human authority. Without the separate trusted
human keyring, merge is disabled.

The controller reads GitHub using its own `GH_TOKEN` or `GITHUB_TOKEN`; it does
not run `gh` as a request-time fallback. Missing credentials, inaccessible
repositories, unavailable work stores, and missing attempt evidence are shown
as unavailable or missing, not as an empty approved queue. Review and merge
remain unavailable until controller composition supplies the exact immutable
attempt-evidence reader. The evidence payload itself is not copied into an
action record.

The reader resolves the exact indexed attempt ID; it never substitutes the
latest attempt. Actionable evidence must match the current PR base and head,
identify the candidate work, use the `candidate_commit_delta` digest, and report
a clean candidate worktree. A dirty workspace snapshot or mismatched revision
is not proof of the reviewed commit. The host must keep the evaluator, evidence
store, human signer, and controller configuration outside candidate write
permissions. Signatures authenticate the evaluator's assertion; they do not by
themselves prove operating-system isolation or that measurements were collected
honestly.

The root-controlled `GC_PR_ACTION_POLICY` value contains one signed policy per
city. A policy names the monitored repositories, rig, allowed base branches,
repair route, and required checks. The server normalizes the policy and derives
its version from the SHA-256 digest of its canonical signed contents; a request
cannot select or change the version. The signature must come from the exact
key, issuer, and subject granted `pr.policy.write` by `GC_PR_HUMAN_TRUST`.
Neither city configuration nor a worker write grant can expand this policy.
An absent or invalid trusted policy leaves the queue unavailable.

`GC_PR_HUMAN_TRUST` is a supervisor-owned JSON trust document containing
Ed25519 public keys and explicit `(key_id, issuer, subject, scopes)` bindings.
Private human-grant keys stay in a separate human authorization service; they
must not be available to candidate workers or the city-write grant minter.
The same exact human authority map signs short-lived merge approvals that bind
city, repository, PR, head and base SHAs, policy version, work ID, and
idempotency key. A public key or key ID shared with the city-write grant set is
rejected. Missing human trust disables merge approval. Host isolation, signing
key custody, and evidence-provider permissions still require deployment
verification before any live promotion or merge release.

## Request and response headers

Every operation's header contract appears in the OpenAPI spec — if a
request header is required or a response header is promised, the
spec describes it. The two cross-cutting headers every API client
should know about:

- **`X-GC-Request`** (request header, required on all mutations).
  Anti-CSRF token required on every POST, PUT, PATCH, and DELETE.
  Any non-empty value is accepted; the header's presence is what
  the server checks. Requests without it are rejected with
  `403 csrf: X-GC-Request header required on mutation endpoints`.
  Leveraging the same-origin policy, a cross-origin attacker
  cannot set this header on a forged request. The generated Go
  and TypeScript clients set this header automatically; only raw
  HTTP clients need to remember it.
- **`X-GC-Request-Id`** (response header, every response).
  Opaque per-response identifier the server assigns for log
  correlation. Every response — success or error — carries this
  header; the spec declares it via a `$ref` to
  `components.headers.X-GC-Request-Id`. Include its value in bug
  reports so the server's logs can be traced.

SSE stream operations emit additional runtime-status headers before
the first event frame:

- **`stream-agent-output` / `stream-agent-output-qualified`**:
  `GC-Agent-Status` — set to `stopped` when the agent is not
  running and the stream is replaying transcript from the session
  log instead of live output.
- **`stream-session`**: `GC-Session-State` (e.g. `active`,
  `closed`) and `GC-Session-Status` (`stopped` when the session's
  underlying process is not running).

Each header's schema is documented in the operation's
`responses.200.headers` in the spec.

## Errors

Every error response is an RFC 9457 Problem Details body
(`application/problem+json`), described by `components.schemas.ErrorModel`.

**Branch on the machine-readable identity, not on prose.** An error carries a
stable `type` URN of the form `urn:gascity:error:<code>` and a convenience
`code` member (the URN's final segment) — for example
`type: "urn:gascity:error:bead-not-found"`, `code: "bead-not-found"`. This is
the canonical identifier to switch on; it never changes between occurrences and
is independent of the human-readable `title`/`detail`. The full catalog of
codes the API can return is published in the spec as the
`x-gascity-problem-types` extension on the `ErrorModel.type` schema.

The `detail` field remains a human-readable, occurrence-specific explanation.
Some legacy paths still encode a semantic hint as a `code: ` prefix on `detail`
(e.g. `not_found: ...`, `conflict: ...`, `read_only: ...`, `in_flight: ...`);
prefer the `type`/`code` members and treat detail-prefix parsing as
deprecated. An error whose body omits `code` is an as-yet-unconverted legacy
path — match it by `status` and `detail` until it gains a code.

The framework's built-in request validation (e.g. a required string posted
empty, or `limit=-1`) carries `type: "urn:gascity:error:validation-failed"`,
usually as `422 Unprocessable Entity` — but as `400 Bad Request` for a body it
cannot parse and `415 Unsupported Media Type` for an unsupported content type;
the `code`/`type` is the constant across those statuses, and the `errors` array
pinpoints the fields that failed. A few endpoints perform their own additional
validation and return a code-less `400`/`422` until converted, so treat a
missing `code` as a legacy path (match on `status`/`detail`). Operations that
enumerate their error responses (currently the bead and sling endpoints) list
each status explicitly in the spec; others declare a single catch-all `default`
error response. An enumerated list covers the operation's own errors plus the
always-applied middleware errors (e.g. `403` on mutations); framework-level
transport statuses may still occur, as with any HTTP API.

## Streaming

SSE endpoints set `Content-Type: text/event-stream` and emit typed
`event:` frames. The spec describes each event's payload schema under
the per-operation `responses.200.content.text/event-stream` entry.
Clients should follow the standard SSE reconnection protocol
(`Last-Event-ID` header) where the server supports it; the event bus
stream (`/v0/events/stream`) replays from the last received index.
When no cursor is supplied, event streams start at the current event
head and deliver future events only. Async `202 Accepted` responses
include an `event_cursor` captured before the operation starts; pass
that value as `after_cursor` or `after_seq` to wait for the operation's
request-result event without replaying unrelated historical backlog.

Fatal setup errors are returned as normal Problem Details responses
*before* the stream's 200 headers commit, never as a 200 stream that
closes immediately. For example, `GET /v0/events/stream` returns
`503 application/problem+json` with `detail: "no_providers: ..."`
when no running city has an event provider registered.

## Creating a city (asynchronous)

`POST /v0/city` is an **asynchronous** operation. The response is
`202 Accepted` returned as soon as the city has been scaffolded on
disk and registered with the supervisor. The slow finalize work
(pack materialization, bead store startup, formula resolution,
agent validation) runs on the supervisor reconciler's next tick.
Clients observe completion via the supervisor event stream — there
is nothing to poll.

### Response

```json
{
  "request_id": "req-...",
  "event_cursor": "__supervisor__:42,my-city:17"
}
```

Use `request_id` to correlate the completion event. Use `event_cursor`
as the `after_cursor` value on the supervisor event stream.

### Completion events

On the same `/v0/events/stream` the client will see:

- `city.created` (`CityLifecyclePayload`) — emitted by the scaffold
  step before `POST` returns. `subject` and payload `name` equal
  the resolved city name.
- `request.result.city.create` (`CityCreateSucceededPayload`) — the
  reconciler finished `prepareCityForSupervisor` successfully.
- `request.failed` (`RequestFailedPayload`) — the reconciler failed
  the async operation. Match `payload.request_id` to the 202 response.

Exactly one terminal event (`request.result.city.create` or
`request.failed`) lands per successful `POST`. Clients wait for the
returned `request_id`; no polling of `GET /v0/cities` or
`GET /v0/city/{cityName}/readiness` is required.

### Subscribe before or after POST

Either order works. The recommended flow is:

1. `POST /v0/city` and wait for `202 {request_id, event_cursor}`.
2. `GET /v0/events/stream?after_cursor=<event_cursor>`.
3. Read frames until `payload.request_id == response.request_id` and
   `type ∈ {"request.result.city.create", "request.failed"}`.

**Empty supervisor is fine.** The event stream works even when
no cities existed before the `POST`. `POST` writes the city to
the supervisor registry (`cities.toml`) and creates
`.gc/events.jsonl` synchronously before returning 202, so the
event multiplexer finds the new city on the very next
`buildMultiplexer` call. Subscribers do **not** need to retry on
`503 no_providers`; if that error surfaces after a successful
202, it's a bug.

### Errors

- `409 conflict: city already initialized at <path>` — the target
  directory already has a scaffolded city.
- `422` — invalid provider, invalid bootstrap profile, or other
  body-validation failure.
- `503` — a hard dependency is missing on the host, or a provider
  the city needs is not ready.
- `500` — unexpected scaffold failure; consult the server logs
  via the `X-GC-Request-Id` correlation header.

## Unregistering a city (asynchronous)

`POST /v0/city/{cityName}/unregister` removes a city from the
supervisor's registry and signals the supervisor to stop the city's
orchestrator. Like `POST /v0/city`, it is asynchronous: the response
is `202 Accepted` returned as soon as the registry entry is gone
and the supervisor is notified. The supervisor reconciler stops the
orchestrator on its next tick and emits the completion event.

The city directory on disk is **not** touched. This operation only
detaches the city from the supervisor; reattaching it later is a
simple `gc register`.

### Response

```json
{
  "request_id": "req-...",
  "event_cursor": "__supervisor__:43,my-city:21"
}
```

Pass `event_cursor` as `after_cursor` on `/v0/events/stream` and wait
for the terminal event whose payload contains the returned `request_id`.

### Completion events

On `/v0/events/stream` the client will see (in order):

- `city.unregister_requested`
  (`CityLifecyclePayload`) — emitted by the handler
  before the registry write so subscribers see the teardown start.
- `request.result.city.unregister`
  (`CityUnregisterSucceededPayload`) — emitted by the reconciler once
  the city's orchestrator has stopped.
- `request.failed` (`RequestFailedPayload`) — emitted by the
  reconciler if the orchestrator did not stop cleanly. Match
  `payload.request_id`.

Exactly one terminal event lands per successful unregister. Clients
wait for the returned `request_id`.

### Errors

- `404 not_found: city not registered with supervisor: <name>` — no
  entry in the registry for that name.
- `501` — supervisor has no Initializer wired (test-only configs).
- `500` — unexpected registry write failure.

## Event Contract

The event APIs, the SSE streams, and `gc events` are the same contract
at three different presentation layers. The API is the source of
truth.

For the explicit CLI output contract, including JSONL framing, empty-output
behavior, heartbeat suppression, and the `--seq` plain-text cursor format, see
[gc events Formats](/reference/events).

### City Scope

Per-city routes are available only after the supervisor marks the city
`running=true` in `GET /v0/cities`. During startup reconciliation, a city can
appear in the city list with `running=false` and `status=starting_agents`; in
that window typed `/v0/city/{cityName}/...` routes return `404` with
`not_found: city not found or not running: <cityName>`. The raw
`/v0/city/{cityName}/svc/*` workspace-service proxy is outside the Huma-typed
API surface and returns the static readiness detail
`not_found: city not found or not running`. Clients should use the supervisor
city list or lifecycle events as the readiness boundary before issuing per-city
requests.

- `GET /v0/city/{cityName}/events`
  returns `ListBodyWireEvent` and includes `X-GC-Index`.
- `GET /v0/city/{cityName}/events/stream`
  emits:
  - `event: event` with `EventStreamEnvelope`
  - `event: heartbeat` with `HeartbeatEvent`
- Async session mutations in that city (`session.create`,
  `session.message`, `session.submit`) complete on this stream. Match
  terminal `request.result.session.*` or `request.failed` events by
  `payload.request_id`.
- Resume:
  - `Last-Event-ID` or `after_seq`; omit both to start from the
    current city event head.
- `gc events` in city scope outputs one `TypedEventStreamEnvelope` JSON
  object per line.
- `gc events --watch` and `gc events --follow` in city scope output one
  `EventStreamEnvelope` JSON object per line.
- `gc events --seq` in city scope prints the API's `X-GC-Index` value.

### Supervisor Scope

- `GET /v0/events`
  returns `SupervisorEventListOutputBody` with `WireTaggedEvent` items.
- `GET /v0/events/stream`
  emits:
  - `event: tagged_event` with `TaggedEventStreamEnvelope`
  - `event: heartbeat` with `HeartbeatEvent`
- Async supervisor mutations (`city.create`, `city.unregister`) complete
  on this stream. Match terminal `request.result.city.*` or
  `request.failed` events by `payload.request_id`.
- Resume:
  - `Last-Event-ID` or `after_cursor`; omit both to start from the
    current supervisor event head.
- `gc events` in supervisor scope outputs one `TypedTaggedEventStreamEnvelope`
  JSON object per line.
- `gc events --watch` and `gc events --follow` in supervisor scope
  output one `TaggedEventStreamEnvelope` JSON object per line.
- `gc events --seq` in supervisor scope prints the current composite
  supervisor cursor, suitable for `--after-cursor`.

### Transport vs Semantic Type

- The SSE `event:` line is the transport envelope:
  `event`, `tagged_event`, or `heartbeat`.
- The semantic event kind is the JSON payload's `type` field:
  `bead.created`, `mail.sent`, `session.woke`, and so on.
- The CLI does not define a separate event schema. It streams the same
  DTOs and envelopes as JSONL.

## Versioning

The API is versioned by URL prefix (`/v0`). Breaking changes ship as
a new prefix; the current spec is the authoritative contract for
`v0`.
