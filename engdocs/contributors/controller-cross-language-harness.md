---
title: Cross-language HTTP fixture harness
description: Disposable loopback conformance fixture for signed human decisions and retirement evidence.
---

This harness lets pack clients exercise actual HTTP handlers with public fixture
credentials and an isolated memory ledger. It runs only in a tagged Go test;
fixture controls are never registered by a production server.

## Launch contract

From the owned worktree, run:

```bash
GC_CROSS_LANGUAGE_HARNESS=1 /home/ricky/gascity-pilot/.gc/scripts/go-bounded.sh test \
  -tags cross_language_harness ./internal/api -run '^TestCrossLanguageHarnessServe$' \
  -count=1 -v -timeout=3m
```

The test prints `GC_HARNESS_READY=<JSON>` with the following protocol inputs.
`GET /__fixture/info` returns the same shape, refreshed for current physical
revision and execution generation.

| Fields | Meaning |
|---|---|
| `base_url`, `city`, `cid` | Disposable loopback endpoint, city name, enforced tenant `cross-language-fixture-tenant` |
| `work_id`, `work_revision`, `physical_revision` | Source ID, original frontier revision, current physical source revision; revisions are decimal strings |
| `target_name`, `session_id`, `execution_generation` | Named target `review-coordinator`, session `session-human-source`, integer generation initially `7` |
| `read_header`, `write_header` | `X-GC-City-Read`, `X-GC-City-Write` |
| `read_audience`, `write_audience` | `gc-city-read`, `gc-city-write.v2` |
| `grant_version`, `grant_info_fields` | `gascity.dev/city-write-grant/v1` and the exact `GC_GRANT_INFO` field names below |
| `now`, `city_public_key`, `human_public_key`, `retirement` | Fixed signing clock, public fixture keys and canonical retirement request |

The source receives a real pre-frontier title update during initialization, so
revision `1` is already stale when readiness is published. Clients use the
published original revision; reservation and release can advance its physical
revision further.
The listener binds `127.0.0.1:0`; HOME is a test temporary directory. The run
ends on `POST /__fixture/stop` or its 120-second deadline. Clients must use the
printed address, never a live city. A new process starts an empty fixture.

## Fixture credentials and requests

All seeds below are public test data: Ed25519 seeds are 32 copies of the listed
byte. They represent no real account or authority.

| Purpose | Seed byte | Key ID |
|---|---:|---|
| City read/write grants | 72 | `k1` |
| Independent human answer | 71 | `answer-key` |
| Retirement observation | 1 | `observation-key` |
| Retirement review | 2 | `review-key` |
| Retirement revocations | 3 | `revocation-key` |

The city grant header is `X-GC-City-Read` for GET and `X-GC-City-Write` for
POST. Send `Content-Type: application/json`, `X-GC-Request: true`, and a stable
`Idempotency-Key` for ensure/answer retries. Each HTTP attempt needs a fresh
grant `jti`. Sign the exact UTF-8 grant JSON bytes and transmit
`base64url(payload).base64url(signature)` without padding. Grant fields are
`kid`, `aud`, `city`, `cid`, `epoch`, `iat`, `exp`, `jti`, `req`; `cid` is
the published nonempty fixture tenant and epoch is zero,
times use the printed fixture clock, expiration is clock +30 seconds. The
audience is `gc-city-read` for GET and `gc-city-write.v2` for POST.

Both real request verifiers enforce that tenant through
`citywriteauth.Options.CID` (the tenancy expectation field in this checkout).
The real read/write middleware rejects a missing or foreign CID with 403.
No fixture handler substitutes for request authentication.

The Python signing command receives `GC_GRANT_INFO` with exactly these fields:
`version`, `aud`, `city`, `method`, `path`, `canonical_query`, `body_sha256`,
`req_digest`. This object describes the exact outgoing request; it is distinct
from the signed grant claims above. The signer takes `cid` and signing time
from the published fixture information.

Compute the lowercase hex SHA-256 of the exact body bytes (empty bytes for GET).
Then hash this UTF-8 preimage to lowercase hex for `req`:

```text
METHOD + "\n" + PATH + ["\n" + CANONICAL_QUERY] + "\n" + BODY_SHA256
```

PATH includes `/v0/city/test-city`. Omit the bracketed query line when empty.
Canonical query uses Go URL encoding: sort decoded parameter names, preserve
the order of repeated values, percent-encode UTF-8 with spaces as `+`, and join
with `&`. The harness needs only `work_revision=<decimal>` for readback. The
grant binds the precise body serialization sent, not an independently
re-serialized object.

Human answers use a separate domain-separated Ed25519 proof with issuer
`city-governance`, subject `reviewer@example.test`, and the exact challenge from
authorized readback. The city grant key cannot authorize a human answer.

Human proof payload field order must match the canonical typed encoding:

```text
protocol,kid,iss,sub,scope,challenge,iat,exp,jti
challenge: city_ref,store_ref,work_id,work_revision,work_digest,map_id,
           ticket_id,question_id,question_version,answer_digest,resolution
```

Use compact JSON without whitespace, UTF-8 non-ASCII characters, Go JSON
escapes (`<`, `>`, `&` as `\u003c`, `\u003e`, `\u0026`, plus U+2028/U+2029),
and the field order above. `protocol` is `gascity.decision-answer.v1`; `scope`
is `decision.answer`. Set `iat` to fixture clock minus one second, `exp` to
clock plus 60 seconds, and `jti` to a distinct fixture answer ID. Sign
`b"gascity.decision-answer.v1\x00" + payload_bytes` and transmit the same
unpadded base64url payload/signature pair. The question supplies `ticket_id`,
`question_id` (its `id`) and `question_version` (its `version`); the frontier
supplies the remaining identity fields.

`answer_digest` is lowercase hex SHA-256 of compact Go JSON in this order:
`{"schema_version":1,"resolution":"answered","text":"<trimmed text>"}`.
For the simplest cross-language vector, use ASCII text without surrounding
whitespace or characters requiring special escapes. Submit
`{ticket_id,work_revision,question_version,resolution,text,proof}`. Use the same
text, proof and idempotency key on retry, with a fresh outer city grant.

## Human flow and held codec

All human paths below are relative to
`/v0/city/{city}/bead/{work_id}/decision-frontier`.

| Step | HTTP request | Result |
|---|---|---|
| Prepare | POST `/prepare`, `{work_revision,proposal:{questions:[{id,title,prompt,depends_on:[]}]}}` | Opaque `preparation_token`, exact target/session binding |
| Ensure | POST `/prepared`, `{preparation_token}`, stable idempotency key | `Frontier`, protected records and initial round |
| Readback | GET `/authorized?work_revision=<original revision>` | `AuthorizedFrontier` with verified answers, rounds, session receipt, physical revision |
| Answer | POST `/authorized/answers`, signed submission, stable idempotency key | Fresh `AuthorizedFrontier` |
| Resume check | POST `/check-resume`, `{frontier_revision,physical_revision}` | Exact-revision eligibility observation |

Response objects are direct JSON bodies, not nested under `body`. A proposal
with independent `first` and `parallel` questions and `dependent` depending on
both starts with two open questions; answering both yields a separate round
for `dependent`. Bind each answer to its exact question version. Preparation
expiry uses the real handler wall clock; signing/verifier times use the fixed
fixture clock printed at readiness.

Question status `held` is a first-class wire value. It is excluded from
`open_questions`, presentation, dependency satisfaction and resume eligibility.
Recognize it even when the question has an answer view (held answered
prerequisite). A dependency-blocked question also remains outside the open
round. An interrupted `answering:<answer-id>` reservation projects as `held`,
with no answer until the immutable write completes. Never coerce held to open
or answered. Session delivery `accepted` and effect `unverified` describe the
fixture receipt; they do not establish a runtime acknowledgement or effect.

## Fixture controls and process boundaries

`/__fixture/*` is a test-only control namespace, serialized with API requests.
The fixture publishes protocol inputs at `GET /__fixture/info`. Controls accept
one strict JSON object up to 4096 bytes; send `{}` when no arguments are needed.
They require no city grant. POST controls return `{action:"<control name>"}`;
GET controls return the information or snapshot object.

| Control | Body | Effect |
|---|---|---|
| GET `/__fixture/snapshot` | No body | Complete native ledger rows (including closed and ephemeral rows), dependencies, session info, receipt map and delivered messages; excludes auth nonce/replay counters and ephemeral preparation state |
| POST `/__fixture/generation` | `{generation:8}` | Require a positive JSON integer; update both persisted session metadata and the delivery fixture reader, plus published integer generation; keep prepared tokens so their stale execution binding is checked |
| POST `/__fixture/source-change` | `{}` | Reconstruct the native source with a changed digest-bearing title and incremented physical revision; old authorized reads and resume scope return 409 |
| POST `/__fixture/hold` | `{ticket_id,held:true}` or `{ticket_id,held:false}` | Reconstruct the memory image with canonical `hold:external` on that protected question; validates question record type |
| POST `/__fixture/interrupt-next-answer` | `{}` | Fail exactly one protected immutable answer create after reservation; all other backend ports delegate to native memory |
| POST `/__fixture/recompose` | `{}` | Reconstruct memory rows/dependencies and recompose the Huma handler/auth verifiers |
| POST `/__fixture/stop` | `{}` | Finish the serving test and shut down its listener |

Both hold and recompose invalidate ephemeral preparation tokens. Re-prepare if
another ensure is needed; existing protected frontier identities remain stable.
Source-change also recomposes the handler. Generation does not recompose it.
The source-change control uses the fixture's native memory reconstruction codec
because the generic source update correctly refuses protected frontier metadata.
Rows and dependencies in the snapshot are sorted by identity; JSON map keys are
stable. Compare snapshots before and after replay or eligibility checks to detect
ledger, session or delivery changes while fresh request grants consume nonces.
Controls cannot set arbitrary protected metadata, install trust, submit a
caller-selected verifier or assert success. API requests and controls are
serialized at the fixture boundary.

Production paths use the real
Huma registration, real grant middleware, protected decision service and native
memory backend. The retirement path uses canonical manifest parsing, signed
attestations and exact scoped fixture authority; it never accepts a caller's
success boolean. Its verdict assurance is `fixture`.

Handler recomposition and memory-ledger reconstruction model in-process
recovery. They do **not** prove durable recovery across process exit. Session
delivery is a deterministic fixture receipt port; no runtime agent is launched
and no real nudge, admission, trial, or retirement action occurs.

The one-shot interrupted create returns HTTP 404 (`bead-not-found`) under the
current stable-create error mapping. This response represents the injected
failure, not successful absence of a frontier. Readback shows the held answer
reservation; retry the exact submission with fresh city authentication. Held
or dependency-blocked answers and stale resume revisions return 409; missing
outer grants return 401; invalid answer proofs return 403.

## Canonical retirement fixture

POST the published `retirement` object unchanged to
`/v0/city/{city}/retirement-release/verify` with a fresh city-write grant. Its
`manifest_json` is the actual canonical checker manifest, `request_sha256`
binds those canonical bytes, and `retained_base64` is standard padded base64 of
the signed review token. The trusted composition independently retains the
expected manifest, source/build/config/host/boot/generation scope, proof digest,
observation, review, revocations and exact read-purpose permission.

The fixture has historical synthetic timestamps and deterministic source
identities; it performs no trial. Successful verification returns
`{status:"verified",request_sha256,assurance:"fixture",reason:"exact_signed_review_verified"}`.
Noncanonical manifest text returns 400; changing retained bytes returns 409
at the trusted retention fence. Other gate requests are outside this fixture's
authority. A caller's `{success:true}` or retained verdict cannot replace a
signed proof.

The complete HTTP envelope remains limited to **1 MiB (1,048,576 bytes)**,
including escaped manifest text and base64 expansion. Oversized bodies return
413 through the real middleware. No limit is increased by the harness.

## Ownership and conformance evidence

The pack owner owns Python clients/tests. This harness owns the socket and
fixture protocol only. Pack conformance must launch the command above, consume
the readiness JSON, run its actual clients through prepare/ensure, signed
answers, authorized readback, held questions, interrupted retry and
check-resume, then stop the fixture. Record that command and its result
separately from the Go socket driver proof. A Go proof alone is not Python
client conformance.

The Go socket driver is executable with:

```bash
/home/ricky/gascity-pilot/.gc/scripts/go-bounded.sh test \
  -tags cross_language_harness ./internal/api \
  -run '^TestCrossLanguageHarnessSocketDriver$' -count=1 -timeout=2m
```

It checks two independent questions, dependent gating, ensure replay, signed
answer authority, explicit held questions, one-shot protected interruption,
memory reconstruction and exact retry, answer identity replay, final readback,
exact and stale resume checks, canonical signed retirement, malformed manifest,
retained caller-success rejection and the common envelope limit. This is
real HTTP/socket evidence in one Go process. The serving entrypoint allows the
pack's actual client process to establish cross-language evidence independently.

The deterministic control and tenant regressions run with:

```bash
/home/ricky/gascity-pilot/.gc/scripts/go-bounded.sh test \
  -tags cross_language_harness ./internal/api \
  -run '^TestCrossLanguageHarness(HumanControls|ForeignCID|SocketDriver)$' \
  -count=1 -timeout=2m
```

## Actual Python conformance command

Build the tagged test executable with the bounded launcher, then run the pack's
unmodified tests read-only. The explicit binary path is the launch contract:
each test starts it with `GC_CROSS_LANGUAGE_HARNESS=1`,
`-test.run=^TestCrossLanguageHarnessServe$`, `-test.v`, `-test.timeout=3m`.

```bash
/home/ricky/gascity-pilot/.gc/scripts/go-bounded.sh test -c \
  -tags cross_language_harness \
  -o /tmp/opencode/controller-human-controls.test ./internal/api

GC_HUMAN_HTTP_FIXTURE_BIN=/tmp/opencode/controller-human-controls.test \
FIXTURE_OPENSSL=/usr/bin/openssl \
PYTHONDONTWRITEBYTECODE=1 PYTHONNOUSERSITE=1 \
timeout 180s python3 -m unittest discover \
  -s /home/ricky/gascity-worktrees/astra-rsi/gp-olwhg.8-human-gates/packs/self-healing-rsi/assets/coordination-router/tests \
  -p test_controller_http_conformance.py -v
```

The output directory must exist. The Python run uses disposable fixture HOME
directories and writes no Python bytecode to the pack. All HTTP calls use the
new loopback socket and public fixture seeds.

## Verification record

On 2026-10-01, the bounded tagged Go run passed the socket driver and the
existing human-source, retirement-source, signed-review, interrupted-reservation
and held-domain tests across the API, retirement and human-domain packages.
Tagged vet passed for those same packages. A separate subprocess launch of
`TestCrossLanguageHarnessServe` passed readiness JSON consumption, HTTP info
readback, HTTP stop and clean process exit.

On 2026-10-01, the actual Python command above passed **7/7 cases** against HEAD
`56dd2908846d94514a28f62ba3279b1d985f760b` plus the owned dirty fixture updates.
It exercised independent/downstream rounds, signed authority joins, read-only
resume, lost-response replay, held/interrupted retry, stale generation and source,
query/audience/tenant binding, worker/foreign/stale human proofs and unavailable
external evidence. Python emitted resource-cleanup warnings for rejected
`HTTPError` objects; all seven assertions and fixture shutdowns passed.

The bounded Go control/tenant/socket regressions and related human-source,
read/write authentication and session-delivery tests passed. Complete
`citywriteauth` and `decisionfrontier` package tests passed, and tagged vet was
clean for the API and both packages. All Go tests, test-executable builds and
vet commands used `/home/ricky/gascity-pilot/.gc/scripts/go-bounded.sh`.

The full retirement export uses the separate tagged serving test
`TestCrossLanguageRetirementHarnessServe`, with independently composed case
listeners serving the same production retirement route. Its eight-case plan,
consumer command and successful Python evidence are recorded in
[`controller-retirement-wire-harness.md`](controller-retirement-wire-harness.md).
Human-control evidence and retirement-export evidence remain distinct.

The parent coordinator reran the unchanged Python consumer successfully with:

```bash
/home/ricky/gascity-pilot/.gc/scripts/go-bounded.sh test -c \
  -tags cross_language_harness -buildvcs=false \
  -o /tmp/opencode/controller-human-final.test ./internal/api

GC_HUMAN_HTTP_FIXTURE_BIN=/tmp/opencode/controller-human-final.test \
FIXTURE_OPENSSL=/usr/bin/openssl \
PYTHONDONTWRITEBYTECODE=1 PYTHONNOUSERSITE=1 \
python3 -B -m unittest discover \
  -s /home/ricky/gascity-worktrees/astra-rsi/gp-olwhg.8-human-gates/packs/self-healing-rsi/assets/coordination-router/tests \
  -p test_controller_http_conformance.py -v
```

Result: **7 tests, 7 passed**, including clean fixture process teardown. The
consumer launches a fresh disposable tagged test executable per case with its
temporary HOME. No pack source or test assertions were modified.

The production read-auth resolver also inherits `GC_CITY_WRITE_CID` when a
separate `GC_CITY_READ_CID` is absent. Explicit conflicting identities fail at
boot. A deterministic regression exercises generic reads through the actual
resolver and middleware, rejecting foreign and missing CID grants signed by the
trusted key; this is not a fixture handler filter.
