#!/usr/bin/env bash
set -euo pipefail

COMMAND=${1:-reconcile}
DOLT_BIN=${HQ_DOLT_BIN:-dolt}
BD_BIN=${HQ_BD_BIN:-bd}
DB_DIR=${HQ_DB_DIR:-/home/ricky/gascity-production/dolt-data/hq}
CITY_DIR=${HQ_CITY_DIR:-/home/ricky/gascity-pilot}
BACKUP_DIR=${HQ_BACKUP_DIR:-/home/ricky/gascity-production/backups/hq-deacon}
REMOTE=${HQ_REMOTE:-origin}
LOCK_FILE=${HQ_LOCK_FILE:-$CITY_DIR/.gc/runtime/deacon/hq-backup-mirror.lock}
STALE_SECONDS=${HQ_STALE_SECONDS:-3600}
NOW_UTC=${HQ_NOW_UTC:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}
DATE_UTC=${HQ_DATE_UTC:-$(date -u +%Y-%m-%d)}
MONTH_UTC=${HQ_MONTH_UTC:-$(date -u +%Y-%m)}
ALERT_LABEL=${HQ_ALERT_LABEL:-deacon:hq-backup-mirror}
ALERTS=${HQ_ALERTS:-1}

require_absolute_safe_dir() {
  local value=$1 label=$2
  [[ "$value" = /* ]] || { printf '%s must be absolute: %s\n' "$label" "$value" >&2; exit 64; }
  [[ "$value" != / && "$value" != /home && "$value" != /home/ricky ]] || {
    printf '%s is too broad: %s\n' "$label" "$value" >&2
    exit 64
  }
}

require_absolute_safe_dir "$DB_DIR" HQ_DB_DIR
require_absolute_safe_dir "$CITY_DIR" HQ_CITY_DIR
require_absolute_safe_dir "$BACKUP_DIR" HQ_BACKUP_DIR

strip_ansi() {
  sed $'s/\033\[[0-9;]*m//g'
}

read_head() {
  local ref=${1:-}
  if [[ -n "$ref" ]]; then
    (cd "$DB_DIR" && "$DOLT_BIN" log "$ref" -n 1 --oneline) | strip_ansi | awk 'NR == 1 {print $1}'
  else
    (cd "$DB_DIR" && "$DOLT_BIN" log -n 1 --oneline) | strip_ansi | awk 'NR == 1 {print $1}'
  fi
}

read_schema_version() {
  (cd "$DB_DIR" && "$DOLT_BIN" sql -r csv -q \
    'select version from schema_migrations order by version desc limit 1') | tail -n 1 | tr -d '\r'
}

read_issue_count() {
  (cd "$DB_DIR" && "$DOLT_BIN" sql -r csv -q \
    'select count(*) from issues') | tail -n 1 | tr -d '\r'
}

digest_file() {
  local file=$1
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$file" | awk '{print $1}'
  else
    shasum -a 256 "$file" | awk '{print $1}'
  fi
}

record_alert() {
  local detail=$1 existing
  [[ "$ALERTS" == 1 ]] || return 0
  existing=$(
    "$BD_BIN" -C "$CITY_DIR" list --status open --label "$ALERT_LABEL" --json --limit 1 2>/dev/null \
      | jq -r '.[0].id // empty' 2>/dev/null || true
  )
  if [[ -n "$existing" ]]; then
    "$BD_BIN" -C "$CITY_DIR" comments add "$existing" \
      "Deacon HQ mirror failed at $NOW_UTC: $detail" >/dev/null 2>&1 || true
  else
    "$BD_BIN" -C "$CITY_DIR" create --type task --priority 1 \
      --title 'Deacon HQ backup/mirror failure' \
      --description "Automatic HQ reconciliation failed at $NOW_UTC: $detail" \
      --label "$ALERT_LABEL" --json >/dev/null 2>&1 || true
  fi
}

mirror_to_remote() {
  local status local_head remote_head push_output
  status=$(cd "$DB_DIR" && "$DOLT_BIN" status) || die "cannot read HQ status"
  [[ "$status" == *'nothing to commit, working tree clean'* ]] || die "HQ working set is not clean"
  local_head=$(read_head) || die "cannot read local HQ head"

  (cd "$DB_DIR" && "$DOLT_BIN" fetch "$REMOTE" >/dev/null 2>&1) || die "full-depth fetch from $REMOTE failed"
  remote_head=$(read_head "remotes/$REMOTE/main") || die "cannot read $REMOTE/main head"
  if [[ "$remote_head" != "$local_head" ]]; then
    if ! push_output=$(cd "$DB_DIR" && "$DOLT_BIN" push "$REMOTE" main:main 2>&1); then
      die "mirror push failed (normal fast-forward only): ${push_output//$'\n'/ }"
    fi
    (cd "$DB_DIR" && "$DOLT_BIN" fetch "$REMOTE" >/dev/null 2>&1) || die "post-push fetch failed"
    remote_head=$(read_head "remotes/$REMOTE/main") || die "cannot verify post-push remote head"
    [[ "$remote_head" == "$local_head" ]] || die "post-push head mismatch: local=$local_head remote=$remote_head"
  fi
  printf '%s\n' "$local_head"
}

mirror_command() {
  mkdir -p "$(dirname "$LOCK_FILE")"
  exec 9>"$LOCK_FILE"
  flock -n 9 || { printf '{"ok":true,"skipped":"already_running"}\n'; return 0; }
  local head
  head=$(mirror_to_remote)
  jq -cn --arg completed_at "$NOW_UTC" --arg head "$head" \
    '{schema:"gascity.deacon.hq-mirror.v1",ok:true,operation:"mirror",completed_at:$completed_at,head:$head}'
}

die() {
  local detail=$1
  record_alert "$detail"
  printf 'hq-backup-mirror: %s\n' "$detail" >&2
  exit 1
}

write_receipt() {
  local target=$1 head=$2 schema=$3 count=$4 backup_digest=$5 drill=$6 tmp
  mkdir -p "$(dirname "$target")"
  tmp="$target.tmp.$$"
  jq -n \
    --arg completed_at "$NOW_UTC" \
    --arg head "$head" \
    --argjson schema_version "$schema" \
    --argjson issue_count "$count" \
    --arg backup_manifest_sha256 "$backup_digest" \
    --arg restore_drill "$drill" \
    '{schema:"gascity.deacon.hq-backup.v1",completed_at:$completed_at,head:$head,schema_version:$schema_version,issue_count:$issue_count,backup_manifest_sha256:$backup_manifest_sha256,restore_drill:$restore_drill}' \
    >"$tmp"
  mv "$tmp" "$target"
}

prune_receipts() {
  local dir=$1 keep=$2
  [[ "$dir" == "$BACKUP_DIR"/manifests/* ]] || die "refusing retention outside backup manifest root: $dir"
  [[ "$keep" =~ ^[1-9][0-9]*$ ]] || die "invalid retention count: $keep"
  [[ -d "$dir" ]] || return 0
  mapfile -t files < <(find "$dir" -maxdepth 1 -type f -name '*.json' -print | sort)
  local excess=$((${#files[@]} - keep)) i
  (( excess > 0 )) || return 0
  for ((i = 0; i < excess; i++)); do
    rm -f -- "${files[$i]}"
  done
}

restore_drill() {
  local expected_head=$1 expected_schema=$2 expected_count=$3 drill_root drill_dir got_head got_schema got_count
  drill_root=$(mktemp -d "$BACKUP_DIR/.restore-drill.XXXXXX")
  drill_dir="$drill_root/hq"
  if ! "$DOLT_BIN" clone "file://$BACKUP_DIR/current" "$drill_dir" >/dev/null 2>&1; then
    rm -rf -- "$drill_root"
    return 1
  fi
  got_head=$(cd "$drill_dir" && "$DOLT_BIN" log -n 1 --oneline | strip_ansi | awk 'NR == 1 {print $1}')
  got_schema=$(cd "$drill_dir" && "$DOLT_BIN" sql -r csv -q \
    'select version from schema_migrations order by version desc limit 1' | tail -n 1 | tr -d '\r')
  got_count=$(cd "$drill_dir" && "$DOLT_BIN" sql -r csv -q \
    'select count(*) from issues' | tail -n 1 | tr -d '\r')
  rm -rf -- "$drill_root"
  [[ "$got_head" == "$expected_head" && "$got_schema" == "$expected_schema" && "$got_count" == "$expected_count" ]]
}

reconcile() {
  mkdir -p "$(dirname "$LOCK_FILE")" "$BACKUP_DIR/manifests/daily" "$BACKUP_DIR/manifests/monthly"
  exec 9>"$LOCK_FILE"
  flock -n 9 || { printf '{"ok":true,"skipped":"already_running"}\n'; return 0; }

  local local_head backup_digest schema count daily monthly drill=not_due
  local_head=$(mirror_to_remote)

  if ! (cd "$DB_DIR" && "$DOLT_BIN" backup sync-url "file://$BACKUP_DIR/current" --prune-with-grace-period 1h >/dev/null 2>&1); then
    die "local Dolt backup sync failed"
  fi
  [[ -f "$BACKUP_DIR/current/manifest" ]] || die "backup completed without a manifest"
  backup_digest=$(digest_file "$BACKUP_DIR/current/manifest") || die "cannot hash backup manifest"
  schema=$(read_schema_version) || die "cannot read schema version"
  count=$(read_issue_count) || die "cannot count HQ issues"
  [[ "$schema" =~ ^[0-9]+$ && "$count" =~ ^[0-9]+$ ]] || die "non-numeric verification result"

  daily="$BACKUP_DIR/manifests/daily/$DATE_UTC.json"
  monthly="$BACKUP_DIR/manifests/monthly/$MONTH_UTC.json"
  if [[ ! -f "$daily" ]]; then
    restore_drill "$local_head" "$schema" "$count" || die "restored backup did not match head/schema/count"
    drill=passed
    write_receipt "$daily" "$local_head" "$schema" "$count" "$backup_digest" "$drill"
  fi
  if [[ ! -f "$monthly" ]]; then
    cp "$daily" "$monthly"
  fi
  write_receipt "$BACKUP_DIR/last-success.json" "$local_head" "$schema" "$count" "$backup_digest" "$drill"
  prune_receipts "$BACKUP_DIR/manifests/daily" 30
  prune_receipts "$BACKUP_DIR/manifests/monthly" 12
  jq -c . "$BACKUP_DIR/last-success.json" | jq -c '. + {ok:true}'
}

status_command() {
  local receipt="$BACKUP_DIR/last-success.json" now_epoch completed_epoch age stale
  [[ -f "$receipt" ]] || { printf '{"ok":false,"stale":true,"reason":"missing_receipt"}\n'; return 1; }
  now_epoch=${HQ_NOW_EPOCH:-$(date -u +%s)}
  completed_epoch=$(python3 - "$receipt" <<'PY'
import datetime
import json
import sys

with open(sys.argv[1], encoding="utf-8") as handle:
    value = json.load(handle)["completed_at"]
print(int(datetime.datetime.fromisoformat(value.replace("Z", "+00:00")).timestamp()))
PY
  ) || { printf '{"ok":false,"stale":true,"reason":"invalid_receipt"}\n'; return 1; }
  age=$((now_epoch - completed_epoch))
  stale=false
  (( age <= STALE_SECONDS )) || stale=true
  jq -c --argjson age_seconds "$age" --argjson stale "$stale" \
    '. + {ok:($stale|not),stale:$stale,age_seconds:$age_seconds}' "$receipt"
  [[ "$stale" == false ]]
}

case "$COMMAND" in
  reconcile) reconcile ;;
  mirror) mirror_command ;;
  status) status_command ;;
  *) printf 'usage: %s {mirror|reconcile|status}\n' "$0" >&2; exit 64 ;;
esac
