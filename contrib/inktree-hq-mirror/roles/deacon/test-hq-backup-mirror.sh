#!/usr/bin/env bash
set -euo pipefail

ROOT=$(CDPATH='' cd -- "$(dirname "${BASH_SOURCE[0]}")" && pwd)
SUBJECT="$ROOT/hq-backup-mirror.sh"
FIXTURES=()
cleanup_fixtures() {
  local path
  for path in "${FIXTURES[@]}"; do
    rm -rf "$path"
  done
}
trap cleanup_fixtures EXIT

fail() {
  printf 'FAIL: %s\n' "$*" >&2
  exit 1
}

assert_contains() {
  local haystack=$1 needle=$2
  [[ "$haystack" == *"$needle"* ]] || fail "expected output to contain: $needle"
}

make_fixture() {
  FIXTURE=$(mktemp -d "${TMPDIR:-/tmp}/hq-backup-mirror-test.XXXXXX")
  FIXTURES+=("$FIXTURE")
  mkdir -p "$FIXTURE/bin" "$FIXTURE/db" "$FIXTURE/city/.beads" "$FIXTURE/backups"
  : >"$FIXTURE/dolt.log"
  : >"$FIXTURE/bd.log"

  cat >"$FIXTURE/bin/dolt" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>"$FAKE_DOLT_LOG"
case "${1:-} ${2:-}" in
  "status ") printf 'On branch main\nnothing to commit, working tree clean\n' ;;
  "log -n") printf '\033[33mlocal-head \033[0mlatest\n' ;;
  "log remotes/origin/main")
    if [[ -f "$FAKE_DOLT_STATE" ]]; then
      printf '\033[33mlocal-head \033[0mremote\n'
    else
      printf '\033[33mremote-head \033[0mremote\n'
    fi
    ;;
  "fetch origin") : ;;
  "push origin")
    [[ "${FAKE_PUSH_FAIL:-0}" == 0 ]] || { printf 'non-fast-forward\n' >&2; exit 1; }
    : >"$FAKE_DOLT_STATE"
    ;;
  "backup sync-url")
    target=${3#file://}
    mkdir -p "$target"
    printf 'fake-manifest\n' >"$target/manifest"
    ;;
  "clone file://"*)
    target=${3}
    mkdir -p "$target/.dolt"
    ;;
  "sql -r")
    case "$*" in
      *schema_migrations*) printf 'version\n53\n' ;;
      *issues*) printf 'count(*)\n285\n' ;;
      *) exit 1 ;;
    esac
    ;;
  *) printf 'unexpected fake dolt invocation: %s\n' "$*" >&2; exit 97 ;;
esac
SH
  chmod +x "$FIXTURE/bin/dolt"

  cat >"$FIXTURE/bin/bd" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$*" >>"$FAKE_BD_LOG"
case "$*" in
  *"list --status open"*) printf '[]\n' ;;
  *"create "*) printf '[{"id":"hq-test-alert"}]\n' ;;
  *) : ;;
esac
SH
  chmod +x "$FIXTURE/bin/bd"

  export PATH="$FIXTURE/bin:$PATH"
  export FAKE_DOLT_LOG="$FIXTURE/dolt.log"
  export FAKE_BD_LOG="$FIXTURE/bd.log"
  export FAKE_DOLT_STATE="$FIXTURE/dolt.state"
  export HQ_DB_DIR="$FIXTURE/db"
  export HQ_CITY_DIR="$FIXTURE/city"
  export HQ_BACKUP_DIR="$FIXTURE/backups"
  export HQ_LOCK_FILE="$FIXTURE/lock"
  export HQ_DOLT_BIN="$FIXTURE/bin/dolt"
  export HQ_BD_BIN="$FIXTURE/bin/bd"
  export HQ_NOW_UTC="2026-09-13T05:00:00Z"
  export HQ_DATE_UTC="2026-09-13"
  export HQ_MONTH_UTC="2026-09"
  unset FAKE_PUSH_FAIL
}

test_reconcile_creates_verified_receipts() {
  make_fixture
  local output
  output=$($SUBJECT reconcile)
  assert_contains "$output" '"ok":true'
  assert_contains "$output" '"head":"local-head"'
  assert_contains "$output" '"schema_version":53'
  assert_contains "$output" '"issue_count":285'
  [[ -f "$FIXTURE/backups/current/manifest" ]] || fail "backup manifest missing"
  [[ -f "$FIXTURE/backups/manifests/daily/2026-09-13.json" ]] || fail "daily receipt missing"
  [[ -f "$FIXTURE/backups/manifests/monthly/2026-09.json" ]] || fail "monthly receipt missing"
  [[ -f "$FIXTURE/backups/last-success.json" ]] || fail "last-success receipt missing"
  grep -Fq 'backup sync-url file://' "$FIXTURE/dolt.log" || fail "backup was not synced"
  grep -Fq 'clone file://' "$FIXTURE/dolt.log" || fail "restore drill was not run"
}

test_reconcile_fails_closed_and_escalates_on_push_failure() {
  make_fixture
  export FAKE_PUSH_FAIL=1
  local output rc=0
  output=$($SUBJECT reconcile 2>&1) || rc=$?
  [[ $rc -ne 0 ]] || fail "divergent mirror unexpectedly succeeded"
  assert_contains "$output" 'mirror push failed'
  grep -Fq 'create ' "$FIXTURE/bd.log" || fail "failure did not create an escalation"
  [[ ! -e "$FIXTURE/backups/last-success.json" ]] || fail "failure wrote a success receipt"
}

test_status_rejects_stale_receipt() {
  make_fixture
  mkdir -p "$FIXTURE/backups"
  printf '{"completed_at":"2026-09-13T02:00:00Z"}\n' >"$FIXTURE/backups/last-success.json"
  export HQ_NOW_EPOCH=1789279200
  local output rc=0
  output=$($SUBJECT status 2>&1) || rc=$?
  [[ $rc -ne 0 ]] || fail "stale status unexpectedly succeeded"
  assert_contains "$output" '"stale":true'
}

test_mirror_only_does_not_run_backup() {
  make_fixture
  local output
  output=$($SUBJECT mirror)
  assert_contains "$output" '"ok":true'
  assert_contains "$output" '"operation":"mirror"'
  grep -Fq 'push origin main:main' "$FIXTURE/dolt.log" || fail "mirror did not push the local head"
  if grep -Fq 'backup sync-url' "$FIXTURE/dolt.log"; then
    fail "post-commit mirror ran the heavy backup path"
  fi
}

test_reconcile_creates_verified_receipts
test_reconcile_fails_closed_and_escalates_on_push_failure
test_status_rejects_stale_receipt
test_mirror_only_does_not_run_backup
printf 'PASS: hq-backup-mirror public contract\n'
