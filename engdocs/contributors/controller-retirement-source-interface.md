---
title: Retirement source verification interface
description: Trusted, read-only composition of retained retirement evidence.
---

Retained evidence needs current authority checks before it can support a release
decision. The source adapter provides those checks without collecting evidence
or changing running work.

## Registration contract

The implementation package is `internal/retirementrelease`. Register a trusted
`*Adapter` constructed with `NewAdapter(Options)`, then call
`Adapter.Verify(context.Context, Request) (Verdict, error)` from an authenticated
endpoint or an offline application composition.

| Type | Contract |
| --- | --- |
| `Request` | `Gate`, `RequestSHA256`, `ManifestJSON`, `RetainedBase64`. The manifest is the complete canonical gate request emitted by the retained Python checker; retained bytes use standard base64. |
| `Verdict` | Checker-compatible `status`, `request_sha256`, `assurance`, `reason`. |
| `Options` | Explicit `Enabled`, trusted `Now`, and `Compose` callback. Zero configuration is off. |
| `Binding` | Gate, canonical request digest, candidate, release-bundle and inventory digests. |
| `TrustedInputs` | Required `Policy`, `CurrentContext`, `CurrentController`, `CurrentBuildIdentitySHA256`, host/boot identities, independently reconstructed manifest, loader configuration and closure, canonical authorities/verifiers, expectations and fenced join inputs. |
| `ProtocolPolicy` | Owning trusted configuration's reference/version, exact `ActivationScope`, `MinimumTrialDuration`, `MaximumTrialToReview`, and `MaximumContextAge`. Every field is explicit. |
| `ActivationScope` | Exact configured `Pack`, `Workflow`, `Session` and complete `RetiredScripts` set. |
| `Context` / `SourceIdentity` | Typed complete independently derived context, including all backend, runtime, configuration and source identities. |

`Compose` receives only `Binding`, never success claims or authority material from
retained bytes. It must independently resolve the entire expected manifest from
trusted retained references and current host, boot, generations, loaded inputs,
backend and release scope. The adapter compares it exactly and invokes composition
again after verification. HTTP authentication remains the endpoint owner's job;
the adapter additionally requires canonical scoped compatibility permission.

The returned `TrustedInputs` must bind `ExpectedManifestJSON`, `RetainedSHA256`,
and `Epoch` to independently retained inputs and current fences. `Assurance` is
trusted composition policy: use `fixture` for simulated sources, including every
deterministic test key. It is never selected by request JSON. `Now` is a trusted
clock; the expected context and request time must both be current within the
configured `MaximumContextAge`. Complete source identities and every release rig are compared as
part of the exact manifest, rather than reduced to the running binary revision.

## Owning protocol policy and independent context

`Compose` must resolve `TrustedInputs.Policy` from the owning trusted protocol
configuration. Missing or incomplete policy returns `protocol_policy_unavailable`.
The platform supplies no activation target, script-count default or trial/review
duration default. For the private retained protocol, the owner configures its
exact private pack/workflow/session, all three WIP script paths, 48-hour trial
minimum, seven-day trial-to-review maximum and five-minute context limit. These
values appear in deterministic test fixtures, not as production policy constants.

Every gate request is checked against that configured activation identity and
complete script set. Its candidate trial, review chronology and rig must satisfy
the configured bounds and release scope. The full policy, including its version,
all scopes and durations, is copied and compared during the second composition;
even a change that would also accept the request returns unavailable.

`CurrentContext` is independently derived; copying expected request context into
it is outside the contract. Its entire value must match the retained context,
including audience/workspace, both generations, all rigs and every controller,
pack, backend, runtime and configuration identity. Independently read `HostID` and
`BootID` must reproduce the context fingerprints using the canonical
`selectorwriter` domains. `CurrentController` is the current canonical selector
snapshot binding: generation must match context, and build must match the running
`Build.BuildID`. `CurrentBuildIdentitySHA256` must equal
`qualification.DigestJSON(Build)`.

Observation expectations must bind the independently loaded configuration
identity, build digest, selector snapshot, audience/workspace and runtime identity.
External-ledger expectations must additionally match the complete current
controller binding and context execution generation, including the fenced
registry's execution generation. A valid signature over different expectations
does not bypass these comparisons. The observation token schema does not directly
sign every host/backend context field; current composition supplies those
independent bindings, while reviewed runtime/backend proof remains an unavailable
gate until its complete authoritative adapter exists.

## Exact read permission

`PermissionScope` uses the existing `qualification.CompatibilityScope` and its
authority/prover contracts. The host authority must resolve the authenticated
principal from the supplied context and own the read policy. Its formula identity
is bound as follows:

| Field | Required value |
| --- | --- |
| `Formula.Name` | `retirement-source-read/` plus the exact gate |
| `Formula.ContentSHA256` | Canonical complete gate-request digest |
| `Formula.SourceSHA256` | Release-bundle digest |
| `Formula.CompiledSHA256` | Candidate-manifest digest |
| `EffectiveConfigSHA256` | Independently recomputed loaded configuration identity |
| `ReleaseRequestSHA256` | Canonical running-build/config release identity |

City, server, store and pinned pack scope come from host composition. The authority
must require the exact read capability rather than infer it from a release grant.
Permission is authorized and revalidated before canonical verification, then
revalidated against the second current composition. Changes to configuration,
authority scope, protocol policy, independent context, registry entries,
generations or fences return unavailable.

## Evidence and unavailable contracts

The adapter calls `qualification.NewSnapshot`, `qualification.Authorize`,
`qualification.AuthorizeCompatibility` and `qualification.RevalidateCompatibility`,
`selectorattestation.Verifier.VerifyObservation` / `VerifyReview`, and
`selectorwriter.Verifier.VerifyAndJoin` directly. It checks sealed evidence before
using it. Signature verification alone does not prove useful trial acceptance,
host-wide audit coverage, retirement effect or activation approval.

| Gate | Implemented source check |
| --- | --- |
| `human-review` | Reload current keys and pinned signed revocations, verify the sealed observation over the complete candidate trial window, then the exact approved review, reviewer, observation record, chronology and independently derived full delta digest. |
| `external-writers` | Verify the signed host record, all mandatory scopes, freshness, retention claim and usable sealed canonical ledger join. Independently retained bytes are digest-bound by composition. |
| `in-flight-resolution` | Perform the same host/ledger join and compare the canonical fenced registry-resolution digest with the joined digest; independently rederive the registry afterward. Its retained proof is the existing signed host record, with no second resolution format. |
| `compatibility` | Authorize and revalidate the separately derived canonical compatibility scope through its pinned policy and complete capability prover. |
| `trial-ledger` | Authenticate a sealed complete observation; remain unavailable because useful acceptance outcomes and independent execution proof have no complete adapter contract here. |
| `runtime-identities` | Recompute the captured loaded-input snapshot and compare source/config/build; remain unavailable pending reviewed backend/source-to-runtime evidence. |
| `release-authorization` | Invoke canonical release authorization; remain unavailable pending the complete private-pack release capability contract. |

The following gates are explicitly unavailable: `manual-audit`,
`retirement-authorization`, `startup-recovery`, `admission-v2`, `owner-fencing`,
`expansion`, `rollback`, `retirement-effect`, `activation-authorization` and
`human-gate-workflow`. The latter is supplied by the separate human-gate work.

The adapter validates canonical manifest structure, duplicate/missing/extra
fields, digest cross-bindings, reference syntax, configured chronology/context
bounds, exact configured activation scope and activation rig equality. The retained checker's complete candidate, inventory-floor,
caller-graph, completion and filesystem-integrity checks remain mandatory before
calling this gate interface. Composition must independently reconstruct their
inputs; copying the caller's manifest into `ExpectedManifestJSON` does not meet
the contract. Neither a short host observation nor an empty inventory proves a
qualified trial or exhaustive host enumeration.

Unsupported gates return `unavailable`; no generic callback can supply a passing
gate verdict. Deterministic fixture authority is marked `fixture` in trusted
composition. Saved verdicts are evidence, never execution permits. The adapter
has no readiness or mutation method. The parent must preserve every mandatory
gate in the release checker and revalidate at any eventual execution boundary.

## Bounded verification

The focused deterministic tests cover signed reviews, current authority reloads,
scoped read permission, signed external joins and registry fences, compatibility,
invalid manifests, exact configured activation policy, policy/context changes,
valid signed tokens with out-of-context expectations and unavailable contracts.
All fixtures use test-only Ed25519
seeds and in-memory retained bytes. Run tests, vet and build through
`/home/ricky/gascity-pilot/.gc/scripts/go-bounded.sh`, with one invocation at a time.
The source does not install keys, invoke live collectors or trials, or claim
operational readiness from these fixtures.
