#!/bin/sh
set -eu

SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
PROJECT_ROOT=$(CDPATH='' cd -- "$SCRIPT_DIR/.." && pwd)
TESTTMP=$(mktemp -d -p /var/tmp gascity-graphify-test.XXXXXX)
trap 'rm -rf "$TESTTMP"' EXIT HUP INT TERM

pass=0
fail=0
check_contains() {
  label=$1
  needle=$2
  haystack=$3
  if printf '%s\n' "$haystack" | grep -F -- "$needle" >/dev/null 2>&1; then
    pass=$((pass + 1))
  else
    printf 'not ok - %s (missing %s)\n' "$label" "$needle" >&2
    fail=$((fail + 1))
  fi
}

mkdir -p "$TESTTMP/project/scripts" "$TESTTMP/project/internal" "$TESTTMP/bin"
cp -f "$PROJECT_ROOT/scripts/graphify-receipt.sh" "$TESTTMP/project/scripts/graphify-receipt.sh"
cp -f "$PROJECT_ROOT/scripts/graphify-harness.sh" "$TESTTMP/project/scripts/graphify-harness.sh"
cp -f "$PROJECT_ROOT/scripts/setup-graphify.sh" "$TESTTMP/project/scripts/setup-graphify.sh"
chmod +x "$TESTTMP/project/scripts/"*.sh
printf 'package fixture\n' >"$TESTTMP/project/internal/fixture.go"
printf '/graphify-out/\n' >"$TESTTMP/project/.gitignore"
git -C "$TESTTMP/project" init -q
git -C "$TESTTMP/project" config user.email graphify-test@example.invalid
git -C "$TESTTMP/project" config user.name graphify-test
git -C "$TESTTMP/project" add .
git -C "$TESTTMP/project" commit -q -m fixture
printf 'package untracked\n' >"$TESTTMP/project/internal/untracked.go"

cat >"$TESTTMP/bin/rg" <<'EOF'
#!/bin/sh
printf '%s\n' "$*" >> "${GRAPHIFY_TEST_LOG:?}"
printf '%s\n' 'internal/fixture.go:1:package fixture'
EOF
chmod +x "$TESTTMP/bin/rg"

PATH=/usr/bin:/bin "$TESTTMP/project/scripts/graphify-harness.sh" status >"$TESTTMP/missing-status"
check_contains 'missing CLI is honest' 'CLI not installed' "$(cat "$TESTTMP/missing-status")"

PATH="$TESTTMP/bin:/usr/bin:/bin" GRAPHIFY_TEST_LOG="$TESTTMP/log" \
  "$TESTTMP/project/scripts/graphify-harness.sh" query 'session lifecycle' >"$TESTTMP/missing-query"
check_contains 'missing graph falls back to source' 'bounded source fallback' "$(cat "$TESTTMP/missing-query")"

cat >"$TESTTMP/bin/graphify" <<'EOF'
#!/bin/sh
printf '%s\n' "$*" >> "${GRAPHIFY_TEST_LOG:?}"
case "${1:-}" in
  --version) printf '%s\n' 'graphify 0.9.36' ;;
  extract) mkdir -p graphify-out; printf '{}\n' > graphify-out/graph.json ;;
  cluster-only|update) : ;;
  query) printf '%s\n' 'bounded graph result' ;;
esac
EOF
chmod +x "$TESTTMP/bin/graphify"
cat >"$TESTTMP/bin/uv" <<'EOF'
#!/bin/sh
printf '%s\n' "$*" >> "${GRAPHIFY_TEST_LOG:?}"
EOF
chmod +x "$TESTTMP/bin/uv"

GRAPHIFY_TEST_LOG="$TESTTMP/log" PATH="$TESTTMP/bin:/usr/bin:/bin" \
  "$TESTTMP/project/scripts/setup-graphify.sh" >/dev/null
log=$(cat "$TESTTMP/log")
check_contains 'setup uses AST-only extraction' 'extract . --code-only --no-cluster' "$log"
check_contains 'setup skips labels and visualization' 'cluster-only . --no-label --no-viz' "$log"
check_contains 'setup pins Graphify' 'tool install --force graphifyy[sql,terraform]==0.9.37' "$log"

: >"$TESTTMP/log"
GRAPHIFY_TEST_LOG="$TESTTMP/log" PATH="$TESTTMP/bin:/usr/bin:/bin" \
  "$TESTTMP/project/scripts/graphify-harness.sh" query 'session lifecycle' >"$TESTTMP/query"
log=$(cat "$TESTTMP/log")
check_contains 'query forwards explicit modest budget' 'query session lifecycle --budget 1200' "$log"
check_contains 'query forwards explicit graph' "--graph $TESTTMP/project/graphify-out/graph.json" "$log"
check_contains 'query reports source receipt' 'source receipt' "$(cat "$TESTTMP/query")"

printf 'package untracked_changed\n' >"$TESTTMP/project/internal/untracked.go"
: >"$TESTTMP/log"
GRAPHIFY_TEST_LOG="$TESTTMP/log" PATH="$TESTTMP/bin:/usr/bin:/bin" \
  "$TESTTMP/project/scripts/graphify-harness.sh" query 'session lifecycle' >"$TESTTMP/content-stale-query"
check_contains 'untracked content change is stale' 'source graph is stale' "$(cat "$TESTTMP/content-stale-query")"
if grep -F 'query session lifecycle' "$TESTTMP/log" >/dev/null 2>&1; then
  printf 'not ok - untracked content change must not invoke Graphify\n' >&2
  fail=$((fail + 1))
else
  pass=$((pass + 1))
fi

cat >"$TESTTMP/project/graphify-out/receipt.json" <<'EOF'
{
  "schema": 1,
  "mode": "code-only-ast",
  "revision": "stale",
  "source_fingerprint": "stale",
  "generated_at": "now",
  "graphify": "graphify 0.9.37",
  "graph": "graph.json"
}
EOF
: >"$TESTTMP/log"
GRAPHIFY_TEST_LOG="$TESTTMP/log" PATH="$TESTTMP/bin:/usr/bin:/bin" \
  "$TESTTMP/project/scripts/graphify-harness.sh" query 'session lifecycle' >"$TESTTMP/stale-query"
check_contains 'stale graph is surfaced' 'source graph is stale' "$(cat "$TESTTMP/stale-query")"
check_contains 'stale graph falls back to source' 'bounded source fallback' "$(cat "$TESTTMP/stale-query")"
if grep -F 'query session lifecycle' "$TESTTMP/log" >/dev/null 2>&1; then
  printf 'not ok - stale graph must not invoke Graphify\n' >&2
  fail=$((fail + 1))
else
  pass=$((pass + 1))
fi

: >"$TESTTMP/log"
GRAPHIFY_TEST_LOG="$TESTTMP/log" PATH="$TESTTMP/bin:/usr/bin:/bin" \
  "$TESTTMP/project/scripts/graphify-harness.sh" update --force >/dev/null
log=$(cat "$TESTTMP/log")
check_contains 'stale graph can be refreshed' 'update . --no-cluster --force' "$log"

GRAPHIFY_TEST_LOG="$TESTTMP/log" PATH="$TESTTMP/bin:/usr/bin:/bin" \
  "$TESTTMP/project/scripts/setup-graphify.sh" >/dev/null
cat >"$TESTTMP/bin/graphify" <<'EOF'
#!/bin/sh
printf '%s\n' "$*" >> "${GRAPHIFY_TEST_LOG:?}"
case "${1:-}" in
  --version) printf '%s\n' 'graphify 0.9.37' ;;
  query)
    i=0
    while [ "$i" -lt 2000 ]; do
      printf '%s\n' 'synthetic Graphify failure' >&2
      i=$((i + 1))
    done
    exit 7
    ;;
esac
EOF
chmod +x "$TESTTMP/bin/graphify"
: >"$TESTTMP/log"
GRAPHIFY_TEST_LOG="$TESTTMP/log" PATH="$TESTTMP/bin:/usr/bin:/bin" \
  "$TESTTMP/project/scripts/graphify-harness.sh" query 'session lifecycle' >"$TESTTMP/failure-query"
check_contains 'query failure is surfaced' 'query failed with exit 7' "$(cat "$TESTTMP/failure-query")"
check_contains 'query failure falls back to source' 'bounded source fallback' "$(cat "$TESTTMP/failure-query")"
failure_bytes=$(wc -c <"$TESTTMP/failure-query" | tr -d ' ')
if [ "$failure_bytes" -le 13000 ]; then
  pass=$((pass + 1))
else
  printf 'not ok - query failure output is bounded (%s bytes)\n' "$failure_bytes" >&2
  fail=$((fail + 1))
fi

if [ "$fail" -ne 0 ]; then
  printf '%s\n' "graphify harness: $pass passed, $fail failed" >&2
  exit 1
fi
printf '%s\n' "graphify harness: $pass passed"
