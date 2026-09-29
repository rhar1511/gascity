# Central PR queue and actions

`gc github pr backfill` reads the Gas City server's queue and policy verdicts.
The server combines durable work records, immutable attempt evidence, and fresh
forge observations. A local city configuration or GitHub token cannot supply a
replacement verdict when the server is unavailable.

```sh
gc github pr backfill --all --json
```

The JSON output uses schema version **2**. Its `queue` field preserves the
server's policy version, observation and freshness timestamps, source states,
exact head/base revisions, work records, attempt references, and permitted
actions. Version 1's locally computed readiness results and dispatch counts are
replaced by this server model. The `actions` array contains verified receipts
for actions submitted by this command.

By default, the text and JSON views include items with an available action.
`--all` also includes blocked items. A monitor argument filters the server's
items; the server's source availability remains visible. Partial and unavailable
sources must not be interpreted as an empty queue.

## Prepare repair work

```sh
gc github pr backfill --create-repair-beads --json
```

This submits only the `prepare` actions permitted by a complete server queue.
Each request copies the exact repository, PR, head SHA, base SHA and policy
version. The command prints its stable idempotency key before submitting it.
Repeating the same request uses that key again.

Earlier requests in a batch can succeed before a later request fails. Keep the
printed keys and inspect the server's queue and action receipts when recovering
from a partial batch.

The verified result is `work_prepared`. Admission and worker dispatch have their
own requirements; a prepared record does not establish that execution began.
Prepared work waits on its triage hold until an authorized actor supplies the
acceptance contract and readiness intent. Its proposed route grants no worker
assignment.

## Submit an exact attempt for review

Copy these values from one server queue item and its attempt reference:

```sh
gc github pr action queue_review \
  --monitor main \
  --repo owner/repository \
  --pr 123 \
  --head-sha "$candidate_sha" \
  --base-sha "$base_sha" \
  --policy-version "$policy_version" \
  --work-id "$work_id" \
  --attempt-id "$attempt_id" \
  --idempotency-key "$request_key"
```

The server revalidates the revisions, policy and evidence. The command rejects
unverified or mismatched receipts. It does not refresh the arguments and silently
submit a different revision after a stale-response error.

After a timeout or uncertain result, keep the same key and exact arguments when
retrying. Changing the key can represent a new action. The server retains the
original action's outcome; transport acceptance alone is not success.

`gc github pr action prepare` accepts the same revision and policy flags without
work or attempt IDs. This can also retry an exact prepare request whose key was
reported by `backfill`.

GitHub merge actions remain unavailable until the forge can enforce both the
human-approved head and base revisions. Queue submission does not authorize a
merge.

## Server availability

These commands route through the standalone or supervisor-managed city API.
`GC_NO_API=1`, an absent server, an unsupported route, and server errors fail
without local forge or ledger mutation. `--timeout` bounds the server request.
Use `--json-schema=result` to inspect each command's output schema.

Named remote contexts are also supported. For a direct server that requires
signed city-write grants, configure the context's existing `grant_command` to
obtain a fresh request-bound grant from its authorized issuer. The PR commands
use that transport; they do not mint their own permissions. An authority-fronted
deployment can supply the grant at its authenticated boundary. A bare local
client without that authority receives a rejection for PR mutations.
