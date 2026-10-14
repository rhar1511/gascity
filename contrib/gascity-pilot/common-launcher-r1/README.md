# Common launcher source candidate — gp-olwhg.9.8

This candidate is prepared in the existing native-verified support worktree on
accepted cache merge cb255631bc54b0cd271989fd1c2ae384aa112141. The launcher files
are inactive: city configuration still uses the installed original launcher.
The cache baseline artifact and gp-8871a checkout remain unchanged.

## Ordinary source workspace flow

The source preparation supplies these public interfaces:

1. `gc beads create` uses the existing authenticated API to create an open,
   unassigned task in an explicit rig with an explicit idempotency key. It sends
   no custody metadata and has no raw-write fallback.
2. Native `gc worktree ensure` creates genuine source-owned provenance.
   `gc worktree prepare` verifies that original source and creation attempt,
   resolves an optional same-store execution step, and saves the exact source
   revision, transition head and workspace tuple in an exclusive owner-only
   request file. Preparation does not publish or claim.
3. `gc worktree publish` requires the saved request and its exact-byte digest.
   The typed controller independently verifies current admission, configured
   rig repository, original native owner and attempt before a protected
   revision-fenced workspace publication. An ambiguous retry uses the same
   request bytes; it does not refresh revision, head or attempt.
4. Pool binding resolves the admitted original source while retaining the
   execution step as trigger. It verifies the complete published custody tuple
   and original native provenance. Existing-session binding holds the session
   mutation and native workspace locks through the conditional session write.
   Binding reads the authoritative session row and nonzero revision, retaining
   the original conditional writer so its cache refresh remains part of the
   commit. Conflicting existing paths, source identities and attempts refuse.
5. The common launcher runs native verify, idempotent ensure and verify against
   that same source attempt, then uses `gc hook --claim --json`. The protected
   claim independently checks authoritative session/source custody while holding
   the native workspace lock. Session identity and the reciprocal claim stamp
   use the session front door and conditional writes. A post-commit refusal
   retains the durable receipt for exact recovery.
6. Provider execution requires the exact claimed trigger. A declared existing
   native conversation accepts only its exact resume command and key.

Generic API create/update and public metadata CAS reject workspace custody
fields, including legacy workdir aliases and trigger identity. Public audit
links remain available. The wrapper's public linkage reads are preflight
evidence; signed admission, current lineage and authoritative custody are
verified by the controller. No drain-member marker or trigger-owned provenance
is manufactured.

The native lock excludes cooperating Ensure/Cleanup operations during its
callback. It does not protect against arbitrary filesystem or Git mutation
outside the native interface. Normal worker readiness remains required:
publishing ownership does not make an infrastructure step eligible work.

## Validation evidence and remaining qualification

The retained R2 validation passed normal spec/client/census/docs generation,
session controls and native lock controls, then exposed a canonical no-op claim
patch rejection. That rejection was corrected without weakening patch validation.
R3 passed lifecycle, generic-custody, API and generated-client controls, then
exposed controller fixture and optional root-store handling failures.
R4 passed the corrected controller controls and the scripts policy package.
Its Mac timing guard was observed RED before the exact workflow change and
GREEN afterward. Original failures, inputs, waits and cleanup receipts remain
under the takeover evidence directory; none is relabeled as a pass.

The separate 49 mocked Python launcher controls passed, including a source
without the optional graph-store marker and explicit conflicting-marker
refusals. The isolated accepted-backend driver passed all 29 native controls:
genuine ownership, immutable preparation, typed publication and exact retry,
source-owned binding, protected claims, refusal and completion. Its provider
remains a fixture; these results do not qualify a real worker.

One earlier native run refused session equality. The next passed with the
production guard unchanged and an additional diagnostic read, with no field
differences reported. That earlier refusal remains unexplained.

A separate cached-session regression failed on a list-projected revision-zero
row. The explicit authoritative binding path then passed 34 pool/claim and
57 session-fence test entries, with no skips. It preserves ordinary cached
read behavior, the original conditional writer, and changed-incarnation
refusals. This demonstrates the cached-session correction; it does not explain
the earlier native refusal. Normal publication gates and final accepted-artifact
qualification remain required.

The Mac timing companion adds fresh per-test JSON output through the existing
Makefile flag. It preserves all test selection, the 20-minute package deadline,
the 40-minute job deadline and the external bd contract. The original full Mac
Tier A timeout remains an unresolved qualification failure; it does not prove
that its last cleanup command caused the preceding elapsed time.

## Preserved existing owners

Frontend `fr-ywbo` retains native conversation
`01a112c9-18e6-7a22-87b9-e194581fb928`, the original worktree
`/home/ricky/gascity-worktrees/frontend/fr-c8us/gp-wisp-57gx2`, and four edits.
Its closed legacy session beads are not reopened or replaced by ordinary
binding. No supported exact import/transfer for that closed tuple has been
established.

Herdr retains `gp-wisp-dgaa3`, source `inktree-3r67i.1`, native conversation
`01a11040-123c-7ad2-b7d6-967b17e2f60e`, and its existing declaration. Restoring
environment does not release a hold or authorize a new provider.
Legacy `gascity-isolation.json` alone is not native creation provenance.

## Installation sequence

Publish and qualify the combined common-launcher plus cache candidate first.
Use one final same-build controller and user-CLI cutover; retain the accepted
cache-only artifact as evidence. Then run current health/doctor checks and an
actual types canary through the ordinary common launcher, followed by legitimate
eligible work in all four rigs. Exact existing-owner recovery remains separate.
Mayor retains dispatch routing after fresh resource, admission and workspace
checks. Production services, keys, sessions and holds are unchanged by this
source preparation.
