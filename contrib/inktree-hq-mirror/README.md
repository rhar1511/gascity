# Inktree HQ mirror integration

This contribution packages the Deacon-owned backup and mirror operation used
by the Inktree Gas City deployment. It is intentionally under `contrib/`:
Gas City provides the orchestration primitives, while this pack supplies
deployment-specific role scripts, orders, and optional systemd units.

The included `pack.toml` declares an on-demand `hq-mirror` operator. It does
not consume a permanent session slot. When the owner routes a PR-repair bead
to it, the prompt enforces one-bead ownership, exact-head diagnostics,
bounded retries, human-only approvals, and fail-closed resource handling.

## Included

- `roles/deacon/hq-backup-mirror.sh` mirrors the canonical HQ Dolt ref with
  normal fast-forward pushes, refreshes a local backup, runs a bounded restore
  drill, and records recovery receipts.
- `orders/` provides the hourly reconciliation and event-triggered mirror
  definitions. The event path suppresses automatic alerts; the hourly path is
  the durable failure reporter and deduplicates the escalation bead.
- `systemd/` contains optional host units for deployments that want commit
  watching outside the Gas City supervisor.

The script is fail-closed, uses an advisory lock, rejects broad directory
overrides, never force-pushes, and will not start or restart Dolt or Gas City
services. Install it only after reviewing the paths and service account for
the target host.

## Deliberately excluded

The workspace also contains an Unraid CI-image reconciler and a macOS SSH
tunnel plist. Those are host-local artifacts, not Gas City SDK assets, and are
not shipped in this contribution.

## Verification

Run the shell contract tests from this directory:

```sh
./test-pack-contract.sh
roles/deacon/test-hq-backup-mirror.sh
```

The test suite uses command shims and temporary directories; it does not touch
the live HQ database or push to a remote.

To use the pack in a city, import this directory from the city's `pack.toml`
and route work explicitly to the `hq-mirror` operator. Keep the city-level
session cap bounded; do not run this pack alongside a second mirror owner.
