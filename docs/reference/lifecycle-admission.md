# Durable lifecycle admission

With `lifecycle.admission_enabled = true`, work must carry a reviewed, signed
admission receipt before the controller can materialize its workflow. The
configured primary admission signer and acceptance signer are separate
authorities. Store their private keys separately from controller permit and
revocation keys.

The backend must support authoritative source revisions, immutable admission
attachment receipts, and protected lifecycle transitions. A stock Beads version
number alone does not establish these capabilities. Configure and qualify the
exact backend and each store's authenticated private-evidence transport before
enabling admission. Controller transitions additionally require the host permit
broker and fresh signed revocation evidence; an admission signing key does not
grant that capability.

## Prepare, review, and attach

Prepare against one exact local source store and configured worker pool:

```sh
gc beads admission prepare task-id \
  --store-ref rig:api --route api/worker \
  --deliverable 'A reviewed pull request for the requested change' \
  --verification 'The specified acceptance checks pass' \
  --acceptance-authority reviewer --merge mr > admission-draft.json
```

Preparation reads the current source revision and resolves the route's effective
workflow, including pack defaults. It emits an unsigned receipt and the current
policy projection for review. It requires open, unenrolled work and an eligible
route; it does not change the source or start a worker. Review the work item,
deliverable, verification, route, formula sources, compiler inputs, and store
placement in the draft before signing.

```sh
gc beads admission attach admission-draft.json \
  --store-ref rig:api --key-file /private/admission/key.pkcs8.pem
```

Attachment recomputes the current policy and compares the complete review
projection and digest. It checks the signing key against the configured primary
authority, signs the receipt, and attaches it through a revision-fenced durable
transition. The policy JSON in the draft cannot supply trusted storage-plan
proofs. A changed source revision, policy, authority, or store requires a new
review; attachment does not silently update the draft.

The key must be a single Ed25519 PKCS#8 PEM block in a regular, owner-only file
inside an owner-only directory. Both must have trusted ownership. Symlinks and
unsupported ownership checks are refused. The command prints the attachment
proof, never the private key. An exact retry can recover the same immutable
attachment while its source state still matches the attachment proof. Later
lifecycle transitions are not an attachment retry.

Attachment does not launch workers or clear holds. Coordinate the final route
configuration, attachment, controller activation, and worker canary in the
deployment plan. A suspended route is ineligible for signing.

## Review and accept completion

Completion requires an explicit `lifecycle.completion_receipt_max_age` and
`lifecycle.completion_clock_skew` policy. Choose both durations for the deployment;
there are no inferred defaults. The acceptance signer named in the admission
reviews the delivered artifact and verification evidence before signing.

Prepare a draft for claimed source work:

```sh
gc beads completion prepare task-id \
  --store-ref rig:api \
  --deliverable-ref 'repository/pull/123@exact-head' \
  --verification-ref 'evidence://accepted-checks' > completion-draft.json
```

Preparation reads the current signed admission and route policy and records the
exact source revision, transition head, and acceptance timestamp. It does not
change work. Review the draft against the delivered artifact, verification
results, and admission requirements, then sign with the separate acceptance key:

```sh
gc beads completion accept completion-draft.json \
  --store-ref rig:api --key-file /private/acceptance/key.pkcs8.pem
```

The same owner-only key-file requirements apply. The command verifies that the
key belongs to the acceptance authority named in the current admission. Only
the signed receipt goes to the running controller; the private key stays local.
The controller verifies the admission, current workflow policy, source revision,
and receipt freshness, then attaches acceptance and its completion budget in one
protected transition before closing the source. Holds and deferrals prevent a
pending completion from progressing.

Use `completion prepare --work-outcome <shipped|no-op|blocked|abandoned>`
with optional `--work-commit`, `--work-branch`, and `--work-verification` to
include the reviewed work record. Any work-record flag requires a valid outcome.
Preparing a draft remains read-only; final close-policy checks belong to the
controller. Omitting all four flags preserves legacy preparation behavior.

The draft's receipt may include an optional `work_record` with `outcome`,
`commit`, `branch`, and `verification`. These fields are covered by the
acceptance signature. The controller writes their nonempty values as the
corresponding `gc.work_*` metadata in the same protected transition as acceptance
and its budget. Omitted fields preserve existing metadata, including the branch
stamped by the claim. Receipts without `work_record` retain their original
canonical signing bytes.

Protected completion uses the existing work-record close policy: violations
warn by default and block new completion writes when `GC_WORK_RECORD_ENFORCE`
is enabled. Shipped work is checked against its owning checkout; an unknown
checkout degrades only the reachability check to a warning. A pending close
rechecks that policy, including after a controller restart. An exact retry of
already-closed work preserves the original receipts and does not recheck
mutable Git references.

Retain the reviewed draft. An exact retry recovers the same committed acceptance
and close, including after a lost response. Changing the reviewed revision,
transition head, authority, or accepted artifact does not authorize a replacement
receipt. If work changes before acceptance commits, prepare and review a new
draft. Generic status writes cannot substitute for this completion path.

These commands accept source work with its own signed admission. They do not
complete generated workflow steps or delegate acceptance keys to workers.

## Graph workflows and policy upgrades

Policy projection version 4 adds a signed graph invocation binding. A supported
`graph.v2` admission uses one source in a rig store, with the generated input
convoy and workflow in that same store. Admission verifies exact convoy
membership, compiler variables, runtime variables, and the graph root key.
Materialization requires atomic graph apply and refuses sequential fallback.
Existing convoy inputs and cross-store graph placement are not admitted by
this binding.

Version 4 changes the policy digest for legacy formulas as well as graph
formulas. Receipts reviewed under projection version 3 do not authorize the new
policy automatically. Before upgrading an admission-enabled deployment,
inventory existing admitted work and plan its completion or explicit recovery
under the prior accepted runtime. Prepare and review new eligible work against
the final version 4 configuration. Do not rewrite old signatures, bypass
revision checks, or clear enrollment metadata to force re-admission.

The checkout's `scripts/protected-mutation/README.md` documents the host timer
and authority layout. Deployments need separate backup,
rollback, backend qualification, and real worker-canary evidence.

### Beginning source work from a claimed step

`gc hook --claim` discovers the generated execution step. A materialized source
is excluded from automatic claims so it cannot displace the step's execution
attribution. The hook reads immutable lifecycle receipts through its exact
configured city/store scope; those reads do not grant protected write authority.
Its managed instance token is passed in-process to the controller client and
remains excluded from work-query subprocess environments.

Inside a managed worker session, begin the source of the current claimed step:

```bash
gc hook begin-source SOURCE_ID --store-ref rig:NAME
```

The command reads the persisted execution claim and managed incarnation,
submits the typed request to the controller, and prints the source claim receipt
as JSON. It requires a controller and does not fall back to a generic claim.
Retrying reads the same committed claim without changing the execution step.

The typed lifecycle claim request accepts an optional `execution_claim` with
`work_id` and `claim_generation`. A worker that has already claimed a generated
step uses this context when beginning that step's admitted source work. The API
checks the current managed incarnation and reciprocal step claim. The controller
verifies the step's existing protected claim, signed parent policy and compiled
workflow membership before claiming the source. The session's current claim
remains the execution step, preserving `gc hook current` and request attribution.

Omitting this context retains ordinary claim behavior. A stale incarnation,
changed generation, unrelated step, missing proof, changed policy or held step
refuses the composed source claim. Exact retries retain the original source
revision and transition head. The source and step are separately fenced rows:
a concurrent external change can leave a committed source transition with an
error response, so an error does not authorize a generic claim or rollback.
The worker receives neither admission nor acceptance signing keys.

### Completing a protected execution step

After finishing the current generated step, a managed worker can run:

```bash
gc hook complete --store-ref rig:NAME --outcome pass
```

The command derives the step and generation from the persisted current claim,
then submits its exact revision and head through the authenticated controller.
The controller verifies the protected anchor and incarnation-bound claim,
current signed parent policy, holds, and compiled workflow membership. It
atomically closes the step and records `gc.outcome` (`pass`, `fail`, or
`skipped`). It does not close or accept the source. Reviewer acceptance still
uses `gc beads completion prepare` and `gc beads completion accept`.

The session's current claim stays on this step for retry and workflow lookup.
An exact retry returns the original receipt without another mutation; a changed
outcome conflicts. A later claim replaces the stamp normally. This path requires
the live session states allowed for claiming. A provider drain request leaves
the current step eligible to finish and retry. Once drain acknowledgement marks
the session pending stop, completion is refused, including retries. The core
formula has not yet been switched to this command.

Generic `gc bd heartbeat` cannot refresh enrolled work: it lacks the current
incarnation, claim and revision proof. A refusal leaves the source and execution
step unchanged. Do not use generic heartbeat or stale-lease reclaim as recovery
for an enrolled owner. Turning off admission does not erase durable enrollment.

Automatic lifecycle recovery is a separate opt-in, disabled by default. With
`lifecycle.recovery_enabled = false`, even a valid signed recovery request leaves
the work, intent, session and recovery budget unchanged and sends no nudge.
Admission and acceptance authorities do not enable recovery. Unsupported
descendant recovery remains held; source acceptance can still proceed through
its protected completion path while recovery is disabled.

A remaining static graph step in the same store can also claim and complete
after reviewer acceptance closes its source. The controller requires the current
verified source-close transition, its complete receipt chain, a still-trusted
acceptance bound to the admission contract, and the current signed route policy.
Source ownership and holds still apply, and the workflow root must remain open.
A raw closed status is insufficient. Receipt freshness is checked when acceptance
and closure commit; it is not reapplied to that historical close while finishing
the workflow. Tail operations preserve the accepted source without reopening it.
