# Workbench

Workbench (`/workbench`) is the operational companion to Beads Canvas. It
projects the same Beads into a searchable work queue, status and priority
boards, rig groups, and a needs-attention view. Selecting a Bead keeps its
details, attempt output, diff, preview, and follow-up controls side by side.
Workbench does not create a second task record. Status and priority moves go
through Gas City's typed supervisor API; starting an attempt delegates to Gas
City's sling operation. PR actions remain unavailable until Gas City supplies
queue, policy, and conflict verdicts.

Selecting an earlier attempt shows that Session's output and an attempt-scoped
artifact read. Gas City currently does not retain a frozen per-attempt diff or
PR-state snapshot, so both are shown as unavailable. Workbench never substitutes
the selected Bead's current, mutable worktree diff for historical evidence.

## Wayfinder review

For a Wayfinder epic whose Notes say `Review: Lavish AXI`, or a Bead labeled
`wayfinder:prototype`, Workbench shows a compact review sequence inside the
selected Bead. A producer may publish these optional Bead metadata fields:

| Field | Meaning |
| --- | --- |
| `gc.prototype_url` | Allowlisted app route with A/B/C variants selected by `?variant=A`, `B`, or `C`. Existing query parameters are preserved. |
| `gc.wayfinder_review_url` | URL of an already-running local Lavish AXI review session. Only loopback HTTP(S) links without credentials are opened. |
| `gc.wayfinder_review_mode` | `lavish` marks an epic as using Lavish when its Notes have not yet been projected into the Bead description. |

The prototype route is for comparing real app context; Lavish reviews a
standalone HTML document in a separate tab. If the local review URL has not
been published on the Bead, the operator can paste it into the panel for this
view only; it is not saved as a decision or sent to Gas City. Workbench does not
embed Lavish, start its CLI, poll its transcript, or share artifacts. The owning agent runs
the local review loop. The operator can explicitly record annotations, prompt
answers, and approval on the selected Bead through Gas City's typed API. Each
entry is retained as a separate `gc.wayfinder_review.event.*` metadata record
with actor and timestamp. Approval requires an artifact target, its revision,
scope, and confirmation; an annotation or answer is never approval. Recording
approval does not publish an artifact, approve a PR, or start ticket rollout.
Missing or blocked links remain visible as such, never as a successful review.

The repository's `engdocs/research/lavish-axi-integration.md` records the
security and ownership boundary.
