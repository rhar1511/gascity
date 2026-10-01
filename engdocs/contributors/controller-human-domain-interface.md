---
title: Human gate domain interface
description: Private, default-off human source-flow integration contract.
---

## Composition boundary

The private human source flow uses `decisionfrontier.Service` with a trusted
`AnswerVerifier` and Q52 `SessionPromptDelivery`. HTTP authentication belongs to
the calling adapter; a city-write credential does not authorize an answer.
Unavailable capabilities return explicit errors and keep work held.

The protected writer must explicitly advertise the independent-round contract.
The native memory writer advertises the round-record/link format. The
source-flow ports additionally require `DecisionFrontierAtomicBackend` for
target-bound creation and conditional eligibility. The memory backend implements
both operations under its physical ledger lock, with the session bead in the
same ledger as source work. SQLite, remote, cache and proxy backends without an
explicit qualified atomic handle return unavailable before preparation writes.
Basic protected-record CAS support is insufficient.

Session validity is owned by the session domain: the adapter reads its typed
persisted response and shares `configuredPromptTarget` and
`persistedSessionGeneration` for named identity, lifecycle and canonical
generation checks. The native backend repeats only the row fence required while
holding its ledger lock; canonical name resolution and the same shared predicate
run against that transaction image. This is not a second session authority or a
role-selection policy. Keeping the backend free of an upward session import also
avoids a package cycle.

The following Go ports are the route-wiring contract (implementation names are
literal):

| Port | Domain method | Result |
| --- | --- | --- |
| `prepare_proposal` | `Service.PrepareProposal(ctx, store, scope, workID, workRevision, proposal)` | Opaque `PreparedProposal`, bound to the exact physical store, proposal, named session and generation |
| fenced ensure/replay | `Service.EnsurePrepared(ctx, store, prepared)` | Durable `Frontier`; refuses changed composition/binding before creating records or sending |
| `read_authorized_frontier` | `Service.ReadAuthorizedFrontier(ctx, store, scope, workID, workRevision)` | `AuthorizedFrontier`, with freshly verified answer principals, source transition evidence and exact session binding evidence |
| `submit_answer` | `Service.SubmitAnswer(ctx, store, scope, workID, submission)` | Signed-answer transition followed by authenticated readback |
| `check_resume` | `Service.CheckResume(ctx, store, scope, workID, frontierRevision, physicalRevision)` | `ResumeEligibility`, conditional on both the original map revision and current authoritative source row |

`PreparedProposal` has no caller-settable fields. Transport adapters must retain
the returned value server-side rather than deserialize it from a request. A
fresh preparation is required after restart; existing maps retain their original
binding and cannot be redirected by a config reload. `EnsurePrepared` handles
exact replay against the current durable map.

## Delivery and authority

The delivery contract is
`gascity.decision-frontier.independent-questions.v1`. Each round contains only
currently independent open questions, numbered in stable proposal order. Held
or dependent questions are excluded. Each exact ordered ticket/version set has
its own deterministic, persisted request ID and Q52 receipt; a downstream round
cannot reuse an earlier receipt. Acceptance, provider delivery, acknowledgement
and verified effect are separate evidence; acknowledgement is not human approval.

Protected answer records retain the opaque signed proof for trusted readback.
Public answer views never expose it. Readback re-runs the configured verifier
against the persisted exact challenge, including scope and answer digest; copied
subject/issuer/key fields or a caller boolean cannot establish authorization.
Expired/revoked authority and legacy records without proofs fail closed.

A new answer also requires an authenticated accepted or acknowledged Q52 round
covering that exact ticket/version. A definitively absent or unknown round cannot
commit an answer. Interrupted signed-answer recovery uses only a matching
delivered historical round; it cannot treat a newly hidden or locally presented
question as delivered. Readback verifies this coverage for recorded answers.

Resume eligibility is a revision-bound observation, not assignment, admission,
merge or external-action permission. Consumers must apply their action's own
conditional backend guard using the returned physical revision.

The memory eligibility operation checks the original map, exact current source
revision, reservation/release chain, current answer slots, question/source holds
and actual blocking-dependency states in one critical section. It writes nothing.
Target-bound creation validates the canonical named session and generation in
the same backend transaction that reserves source work and creates immutable
records. A changed target rolls the whole operation back.

Native protected release also checks every answered, unheld question against
accepted/acknowledged protected round intents with exact versions, presentation
and execution bindings, plus the current configured session/generation. Missing
round evidence or a changed execution retains the source hold even if a private
controller writer tries to bypass the domain answer method.

## P2 cross-row limits

Native transactions join source work, maps/tickets, protected round intents,
the current named session binding and dependency state in the existing ledger.
External signing trust, revocation and time-based expiry are not part of that
transaction; they can change after proof verification. Provider effect remains
unverified. Eligibility is an observation: a future source/session change still
requires the later action's own conditional guard. No response claims atomic
keyring or provider admission. Unsupported remote backends remain unavailable
until their own deployed atomic contract is qualified.
