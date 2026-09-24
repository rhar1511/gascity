# BuilderIO software-factory findings

Date: 2026-09-24

## Sources

- BuilderIO/skills repository: <https://github.com/BuilderIO/skills>
- Factory guide: <https://github.com/BuilderIO/skills/blob/main/docs/factory/README.md>
- Configuration reference: <https://github.com/BuilderIO/skills/blob/main/docs/factory/configuration.md>
- Factory coordination skill: <https://github.com/BuilderIO/skills/blob/main/skills/factory/SKILL.md>
- Factory collect skill: <https://github.com/BuilderIO/skills/blob/main/skills/factory-collect/SKILL.md>
- Factory review and ship skills: <https://github.com/BuilderIO/skills/tree/main/skills/factory-review-prs>

## Findings

Builder's Factory is a project-level policy convention at
`.agent-factory/config.yaml`. It names repositories, source scopes, worktree
behavior, workflows, independent action policies, schedules, and optional
prompt overlays. The file does not create integrations, grant credentials, or
prove that a host scheduler saved a job.

The workflow is intentionally staged: collect current signals, perform a
bounded history lookback, assemble a human decision digest, implement in an
isolated worktree, review a PR or candidate under separate rules, and ship only
when the shipping policy independently allows it. Fix permission does not imply
reply, approval, merge, deployment, closure, or recovery permission.

Factory reports should distinguish empty, unavailable, and truncated reads;
retain source links and pagination coverage; and hold when ownership,
authorization, required checks, or live host state is unclear. Recovery is
limited to interrupted work whose original authorization and worktree are still
valid.

## Gas City mapping

Gas City already provides the durable execution primitives needed by this
model: bead-scoped worktrees through `gc worktree`, formula steps, JSON output
artifacts, and the merged bounded RSI policy. The implementation adds a typed
loader/validator for the Builder configuration convention, a bundled
`mol-software-factory` formula for the staged lifecycle, and a `gc-factory`
skill that makes the isolation and gate rules explicit to workers.
