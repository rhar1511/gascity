#!/usr/bin/env bash
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
PACK_TOML=$ROOT/pack.toml
AGENT_TOML=$ROOT/agents/hq-mirror/agent.toml
RECONCILE_ORDER=$ROOT/orders/deacon-hq-backup-reconcile.toml
MIRROR_ORDER=$ROOT/orders/deacon-hq-mirror-on-change.toml

grep -Fq 'name = "inktree-hq-mirror"' "$PACK_TOML"
grep -Fq 'schema = 2' "$PACK_TOML"
grep -Fq 'template = "hq-mirror"' "$PACK_TOML"
grep -Fq 'mode = "on_demand"' "$PACK_TOML"
grep -Fq 'max_active_sessions = 1' "$AGENT_TOML"
grep -Fq 'trigger = "cooldown"' "$RECONCILE_ORDER"
grep -Fq 'reserved_dispatch = true' "$RECONCILE_ORDER"
grep -Fq 'timeout = "1800s"' "$RECONCILE_ORDER"
grep -Fq 'trigger = "event"' "$MIRROR_ORDER"
grep -Fq 'on = "bead.updated"' "$MIRROR_ORDER"

printf '%s\n' 'PASS: inktree-hq-mirror pack contract'
