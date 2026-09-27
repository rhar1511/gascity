# Software factory

Gas City provides the configuration validator and controller primitives for
software-factory workflows. Supply the workflow formulas and skills through an
explicit pack import. The core pack does not include `mol-software-factory`,
`mol-rsi-candidate`, or `gc-factory`.

The consuming pack must declare and test its controller compatibility, compile
its actual formulas, and preserve separate human approval and action policies.
A pack import alone does not authorize execution or deployment.

Create `.agent-factory/config.yaml` in the project being operated. Validate it before starting a run with `gc factory validate` (or pass an explicit config path). The config
must declare at least one repository with a worktree policy. `fresh-per-run`
worktrees require an explicit base ref. Sources are named and scoped; workflows
refer to those source IDs. Action policies use `never`, `manual`, `criteria`,
`after-fix`, or `after-merge`.

A minimal configuration is:

```yaml
version: 1
timezone: Australia/Sydney
repositories:
  - id: app
    provider: github
    remote: rhar1511/app
    worktree:
      mode: fresh-per-run
      base: origin/main
sources:
  - id: issues
    provider: github
    type: issue
    scope: rhar1511/app
workflows:
  collect:
    enabled: true
    sources: [issues]
    implement:
      mode: criteria
      allow: [verified defects in owned code]
      stop: [security-sensitive changes, unclear product intent]
    reply:
      mode: never
    close:
      mode: never
```

After importing a compatible workflow pack, run its `mol-software-factory`
formula with explicit targets for collection,
lookback, digest, implementation, review, gate, and delivery. Every code change
must carry a bead-specific `work_dir` and a clean `rig_root`; the worker must
refuse to edit when those paths are the same. Collection, review, and delivery
produce durable JSON evidence. Approval, merge, deploy, close, reply, and notify
remain separate decisions.

This formula does not install connectors or create scheduler jobs. Configure
those capabilities in the host and retain secrets there.
