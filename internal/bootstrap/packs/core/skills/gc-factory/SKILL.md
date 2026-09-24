---
name: gc-factory
description: >-
  Run the configured software factory with isolated worktrees, independent
  review, bounded evidence, and separate delivery gates.
---

# Gas City software factory

Read `.agent-factory/config.yaml` before starting a factory run. The file is
project policy; it names sources and action criteria but cannot create an
integration, grant credentials, or authorize an external action by itself.
Missing, unreadable, or unclear policy means hold.

Use `mol-software-factory` for the full lifecycle:

1. Collect only configured sources and record complete coverage. Keep empty,
   unavailable, and truncated reads distinct.
2. Run a bounded lookback when enabled and preserve source links and comparison
   windows.
3. Build a human decision digest for unclear intent, sensitive interpretation,
   policy changes, and delivery actions that lack separate authorization.
4. Implement eligible changes in a fresh bead-specific worktree. Run
   `gc worktree verify` before and after the change. Never target a rig root.
5. Review from a read-only checkout with a reviewer independent of the
   implementer. The reviewer cannot change criteria or approve its own work.
6. Apply the factory gate and retain the candidate commit, evidence, and
   rollback point.
7. Publish, approve, merge, deploy, close, reply, and notify only when each
   action's own policy is enabled and its live conditions still hold.

Keep the previous accepted revision available for rollback. Bound attempts and
stop conditions; a failed cycle is evidence for a later proposal, not a reason
to recurse indefinitely. Preserve user decision points for canon changes,
interpersonal action, sensitive interpretation, and real-world publication.
