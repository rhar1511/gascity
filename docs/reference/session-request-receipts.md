---
title: "Session request receipts"
description: "Track request acceptance, provider submission, and acknowledgement by an exact session execution."
---

# Session request receipts

A tracked request names a durable session ID, request ID, and execution
generation. The server stores acceptance before scheduling provider delivery.
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

After receiving the request, the intended execution acknowledges it:

```bash
gc session request ack request-456
```

The command takes its identity from `GC_SESSION_ID`, `GC_RUNTIME_EPOCH`, and
`GC_INSTANCE_TOKEN`. Credentials are sent in a header and are absent from the
public receipt. The server checks the exact request, generation, and current
execution credential; an old execution cannot acknowledge a replacement's
request. Repeating an acknowledgement preserves its original timestamp.

## HTTP surface

- `POST /v0/city/{city}/session/{id}/requests` accepts `request_id`,
  `generation`, and `message`, returning a durable receipt with HTTP 202.
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

## Deployment requirements

Writes require a backend with conditional row updates. The protocol refuses
an unconditional fallback. Runtime credentials must be isolated: an actor
that can read another execution's token can impersonate it. The in-process
session lock coordinates normal incarnation changes but does not fence an
arbitrary external writer or runtime replacement. These deployment boundaries
must be verified before using receipts as recovery evidence.

Receipts live on the durable session record. The session close/delete API refuses
to delete records containing receipts until an archive policy is implemented.
Direct backend deletion or automatic backend TTL remains outside that guard;
deployments must preserve the records before enabling this protocol. A tracked request does not itself enable recovery or grant
permission for an operation.
