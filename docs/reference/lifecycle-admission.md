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
