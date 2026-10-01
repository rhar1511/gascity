---
title: Source adapter coordination
description: Final ownership and handoff boundary for the private pack adapters.
---

# Source adapter coordination

Parent owns `internal/api` routes. Human-domain agent owns domain/beads/controller;
retirement agent owns `internal/retirementrelease`.

Human HTTP preparation retains `PreparedProposal` only in a bounded ephemeral
per-city cache; opaque token references it, never deserializes an authority.
Restart requires fresh preparation. Durable frontier remains the sole ledger.
`PreparedProposal.TargetBinding() PromptBinding` and
`PreparedProposal.TargetName() string` expose the exact preparation response.
The response includes the fixed independent-question delivery contract.

Implemented routes: POST `decision-frontier/prepare`, POST
`decision-frontier/prepared`, GET `decision-frontier/authorized`, POST
`decision-frontier/authorized/answers`, POST `decision-frontier/check-resume`.
All require verified request-bound city read/write principals even if global
middleware is otherwise optional. Original advisory endpoints remain compatible.
Retirement POST `/retirement-release/verify` uses the new typed adapter directly.

The final protocol and pack adapter mapping are in
[`controller-source-http-interface.md`](controller-source-http-interface.md).
The pack agent must implement the protected readback codec, including `held`
question status and current-round prompt binding, before enabling the consumer.
It must not treat an ordinary frontier client, saved JSON disposition or fixture
conformance report as an authority port.

Affected specs and Go/TypeScript clients were regenerated once from the frozen
source interfaces. Atomic source-flow backend conformance is implemented and
tested for the native memory ledger; production backends lacking the explicit
atomic contract remain unavailable. Actual trial/audit/release evidence and the
unsupported retirement gate contracts remain qualification dependencies.
