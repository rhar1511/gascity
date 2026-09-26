# Trusted RSI evaluation

The RSI promotion gate fails closed unless the controller can verify a signed
evaluation record and a separate human approval for that exact evaluation.
Candidate and judge beads provide proposed code and review outputs; they do not
set policy, thresholds, attempt counts, measurements, or authority class.

## Root-city configuration

Only the root `city.toml` may configure evaluator trust. Fragments that define
`[rsi]` are rejected. File paths are relative to the city directory. Public
keys are canonical, unpadded base64url Ed25519 keys; private keys must stay out
of city configuration, candidate worktrees, judge worktrees, and bead metadata.

```toml
[rsi]
evaluation_file = ".gc/rsi/evaluation.json"
evaluation_key_id = "trusted-evaluator-2026"
evaluation_public_key = "<base64url-ed25519-public-key>"
human_approval_file = ".gc/rsi/approval.json"
human_approval_key_id = "release-approver"
human_approval_public_key = "<different-base64url-ed25519-public-key>"
```

The evaluator fields must be set together. Human-approval fields are optional
as a configuration group, but without them the gate remains pending and cannot
promote. An absent evaluator configuration is a rejection. A configured but
not-yet-created evaluation file, referenced evidence file, or human approval
file is pending; the gate remains open. Invalid signatures, malformed records,
unknown authority, and expired evidence are rejections. The first promotion
always requires a human signature, including ordinary optimization work.

The core `control-dispatcher` runs `gc convoy control --serve --follow`. When
the RSI gate returns pending, that live loop leaves it open and scans pending
control beads again on its next poll (currently the one-second pending-work
interval). An evaluator can write the signed report after the judges finish,
then a separate approver can write the matching approval. The next scan reads
both files and evaluates the same gate. No controller pause or manual gate
reopen is needed.

## Candidate and judge records

The candidate step emits only a proposal:

```json
{"candidate":{"id":"bundle-42","types":"<commit>","ai":"<commit>","frontend":"<commit>","inktree":"<commit>","evals":"<sha256>","parent":"bundle-41"}}
```

The evaluator signature binds the SHA-256 digest of the exact candidate bead
output bytes, including any extra untrusted fields. The controller decodes only
the candidate bundle from that output. Judge beads emit
`reviewquorum.LaneOutput`; the evaluator record binds each exact output digest
to the assigned judge bead, actor, session, and lane.

## Evaluation record and evidence

Each signed JSON envelope has `payload` and `signature` fields. The signature
is unpadded base64url Ed25519 over the exact UTF-8 bytes inside `payload`.
Unknown JSON fields are rejected. The evaluation payload uses schema
`gc.rsi.trusted-evaluation.v1` and policy version `gc.rsipolicy.v1`. It binds:

- an evaluation ID, issue/expiry times, evaluator key ID, candidate bead ID,
  and candidate output digest;
- the exact baseline and candidate revision bundles, parent link, and frozen
  evaluation-suite SHA-256;
- measured baseline/candidate metrics, all required safety/resource limits,
  authority class, and bounded attempt count that must match the controller's
  successful candidate retry attempt and formula-configured maximum;
- the candidate actor/session and permission profile, plus at least two
  independent judge actor/session records and their permission profiles;
- signed evidence references for baseline and candidate artifact identities,
  the acceptance ledger, the worker-time ledger, and each judge lane.

Evidence paths are relative to the evaluation file's directory and must remain
inside it. The referenced bytes are hashed and parsed strictly. Artifact
records must match their bundle and suite. The acceptance ledger must reproduce
the frozen acceptance-unit set and unique useful-unit results. Its set hash is
SHA-256 over sorted unit IDs joined by NUL bytes. Worker-time intervals must
use known bundles/phases, have valid non-overlapping actor sessions, bind the
candidate's implementation/recovery time and each judge's evaluation time,
and sum to the signed worker-hour totals. The policy recomputes score as useful
accepted units divided by total worker-hours; it does not trust a supplied
score that disagrees with those ledgers.

The human-approval payload uses schema `gc.rsi.human-approval.v1`. It must be
signed by a key different from the evaluator key and bind the SHA-256 digest of
the complete evaluation envelope, evaluation ID, candidate bead, baseline and
candidate bundle IDs, suite hash, and policy version. A reusable `promote=true`
flag is not accepted.

## Deployment trust boundary

The signature proves that the configured evaluator key signed the stated
claims. The controller does not inspect operating-system sandbox rules or
prove how metrics were produced. A deployment must therefore provide a
separate trusted runner that measures the declared suite and ledgers, controls
the evidence directory, and holds the evaluator private key outside candidate
and judge execution. Candidate and judge processes must be unable to write the
root `city.toml`, evaluator/approval records, held-out data, or signing keys;
judges may read public policy and criteria but may not alter them. The human
approval key must be held by an authorized person or service outside the
candidate, judge, and evaluator processes.

Permission fields in the signed record are evaluator assertions, not OS-level
enforcement. Do not configure an evaluator key until its runner can make those
assertions from an independently enforced execution boundary. This source
implementation does not include a metrics runner or install production keys.
Until a deployment supplies measured evidence, isolation, and separate human
approval, RSI promotion remains unavailable and the controller rejects the
gate.
