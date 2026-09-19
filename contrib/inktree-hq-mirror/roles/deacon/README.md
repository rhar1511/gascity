# HQ backup and mirror role operation

`hq-backup-mirror.sh` is the deterministic Deacon-owned operation for the
canonical Gas City HQ. It never force-pushes. The post-commit order mirrors
normal fast-forward changes; the hourly order also refreshes a local Dolt
backup, performs the first restore drill of each UTC day, and retains 30 daily
plus 12 monthly recovery receipts.

The local backup is one incremental Dolt backup repository. Retention applies
to verified recovery-point receipts rather than duplicating the full 400+ MiB
repository for every day. Each receipt records the exact Dolt head, schema,
issue count, and backup-manifest digest used by the restore drill.

Failure is fail-closed and opens or updates one Beads escalation labeled
`deacon:hq-backup-mirror`. Event-triggered runs suppress that alert to prevent
a failure-created Bead from recursively triggering another alert; the hourly
reconciliation owns durable escalation.

`gascity-deacon-hq-mirror.path` watches the canonical Dolt manifest so commits
that do not emit `bead.updated` (notably comment-only commits) still trigger the
same lightweight role command. It suppresses alerts for the same recursion
reason. The hourly Gas City order is the sole durable failure reporter.

Public commands:

```sh
roles/deacon/hq-backup-mirror.sh mirror
roles/deacon/hq-backup-mirror.sh reconcile
roles/deacon/hq-backup-mirror.sh status
```
