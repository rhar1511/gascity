#!/bin/sh
# Bounded, fail-open adapter for the local Graphify source index.
set -eu

SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
PROJECT_ROOT=$(CDPATH='' cd -- "$SCRIPT_DIR/.." && pwd)
GRAPH_PATH="$PROJECT_ROOT/graphify-out/graph.json"
RECEIPT_PATH="$PROJECT_ROOT/graphify-out/receipt.json"
DEFAULT_QUERY_BUDGET=1200
MAX_QUERY_BUDGET=1500
OUTPUT_MAX_BYTES=12000

# shellcheck source=scripts/graphify-receipt.sh
. "$SCRIPT_DIR/graphify-receipt.sh"

usage() {
  cat <<'EOF'
Usage: scripts/graphify-harness.sh <command> [args]

Commands:
  status                   report CLI, graph, and source-receipt freshness
  query "question"         scoped source query, capped at 1,200 tokens
  path "node A" "node B"  find the shortest dependency path
  explain "node"          explain a node and its neighbours
  affected "node"          show reverse dependency impact
  god-nodes [options]      list architectural hubs
  update [--force]         refresh code AST data without an LLM
EOF
}

has_graphify() {
  command -v graphify >/dev/null 2>&1
}

receipt_state() {
  if [ ! -f "$RECEIPT_PATH" ]; then
    printf '%s\n' missing
    return 0
  fi
  if [ "$GRAPH_PATH" -nt "$RECEIPT_PATH" ]; then
    printf '%s\n' stale
    return 0
  fi

  recorded_revision=$(graphify_receipt_field revision "$RECEIPT_PATH")
  recorded_fingerprint=$(graphify_receipt_field source_fingerprint "$RECEIPT_PATH")
  current_revision=$(graphify_receipt_revision "$PROJECT_ROOT")
  current_fingerprint=$(graphify_receipt_fingerprint "$PROJECT_ROOT")
  if [ -z "$recorded_revision" ] || [ -z "$recorded_fingerprint" ]; then
    printf '%s\n' missing
  elif [ "$recorded_revision" = unknown ] || [ "$current_revision" = unknown ] ||
       [ "$recorded_fingerprint" = unknown ] || [ "$current_fingerprint" = unknown ]; then
    printf '%s\n' unknown
  elif [ "$recorded_revision" != "$current_revision" ] ||
       [ "$recorded_fingerprint" != "$current_fingerprint" ]; then
    printf '%s\n' stale
  else
    printf '%s\n' fresh
  fi
}

print_status() {
  if ! has_graphify; then
    printf '%s\n' '[graphify] CLI not installed. Run scripts/setup-graphify.sh.'
  else
    printf '%s\n' "[graphify] CLI installed: $(graphify --version 2>/dev/null || printf 'version unknown')"
  fi
  if [ ! -f "$GRAPH_PATH" ]; then
    printf '%s\n' '[graphify] graph not built. Run scripts/setup-graphify.sh.'
  else
    printf '%s\n' "[graphify] graph: $GRAPH_PATH"
  fi
  if [ -f "$RECEIPT_PATH" ]; then
    printf '%s\n' "[graphify] receipt: $RECEIPT_PATH"
    printf '%s\n' "[graphify] freshness: $(receipt_state) revision=$(graphify_receipt_field revision "$RECEIPT_PATH") generated_at=$(graphify_receipt_field generated_at "$RECEIPT_PATH")"
  else
    printf '%s\n' '[graphify] source receipt missing; freshness is unknown.'
  fi
}

fallback_query() {
  question=$1
  printf '%s\n' '[graphify] bounded source fallback: rg results are advisory; verify the cited source.'
  if ! command -v rg >/dev/null 2>&1; then
    printf '%s\n' '[graphify] source fallback unavailable: rg is not installed.'
    return 0
  fi

  terms=$(printf '%s\n' "$question" | tr -cs '[:alnum:]_' '\n' | awk 'length($0) >= 3' | head -n 8 | paste -sd '|' -)
  if [ -z "$terms" ]; then
    printf '%s\n' '[graphify] source fallback found no searchable terms.'
    return 0
  fi

  fallback_output=$(mktemp -p /var/tmp gascity-graphify-fallback.XXXXXX)
  rg -n -i -m 20 \
    --glob '*.go' --glob '*.md' --glob '*.toml' --glob '*.sh' \
    --glob '!graphify-out/**' -e "$terms" "$PROJECT_ROOT" >"$fallback_output" 2>&1 || true
  cap_output "$fallback_output"
  rm -f "$fallback_output"
}

cap_output() {
  output_path=$1
  output_bytes=$(wc -c <"$output_path" | tr -d ' ')
  if [ "$output_bytes" -gt "$OUTPUT_MAX_BYTES" ]; then
    head -c "$OUTPUT_MAX_BYTES" "$output_path"
    printf '\n[graphify] output truncated at %s bytes (full output was %s bytes).\n' "$OUTPUT_MAX_BYTES" "$output_bytes"
  else
    cat "$output_path"
  fi
}

query_budget() {
  budget=${GRAPHIFY_QUERY_BUDGET:-$DEFAULT_QUERY_BUDGET}
  case "$budget" in
    ''|*[!0-9]*)
      printf '%s\n' "error: GRAPHIFY_QUERY_BUDGET must be an integer from 1 to $MAX_QUERY_BUDGET" >&2
      return 64
      ;;
  esac
  if [ "$budget" -lt 1 ] || [ "$budget" -gt "$MAX_QUERY_BUDGET" ]; then
    printf '%s\n' "error: GRAPHIFY_QUERY_BUDGET must be an integer from 1 to $MAX_QUERY_BUDGET" >&2
    return 64
  fi
  printf '%s\n' "$budget"
}

graph_ready() {
  has_graphify && [ -f "$GRAPH_PATH" ] && [ "$(receipt_state)" = fresh ]
}

run_query() {
  question=$1
  budget=$(query_budget)
  if ! has_graphify; then
    printf '%s\n' '[graphify] query unavailable: CLI not installed.'
    fallback_query "$question"
    return 0
  fi
  if [ ! -f "$GRAPH_PATH" ]; then
    printf '%s\n' '[graphify] query unavailable: graph is missing; run setup first.'
    fallback_query "$question"
    return 0
  fi
  freshness=$(receipt_state)
  if [ "$freshness" != fresh ]; then
    printf '%s\n' "[graphify] query unavailable: source graph is $freshness (receipt=$RECEIPT_PATH)."
    fallback_query "$question"
    return 0
  fi

  cd "$PROJECT_ROOT"
  query_output=$(mktemp -p /var/tmp gascity-graphify-query.XXXXXX)
  set +e
  graphify query "$question" --budget "$budget" --graph "$GRAPH_PATH" >"$query_output" 2>&1
  status=$?
  set -e
  if [ "$status" -ne 0 ]; then
    printf '%s\n' "[graphify] query failed with exit $status; using bounded source fallback."
    cap_output "$query_output"
    rm -f "$query_output"
    fallback_query "$question"
    return 0
  fi
  printf '%s\n' "[graphify] query source receipt: revision=$(graphify_receipt_field revision "$RECEIPT_PATH") freshness=$freshness budget=$budget"
  cap_output "$query_output"
  rm -f "$query_output"
}

run_graphify_command() {
  graphify_output=$(mktemp -p /var/tmp gascity-graphify-command.XXXXXX)
  set +e
  graphify "$@" --graph "$GRAPH_PATH" >"$graphify_output" 2>&1
  status=$?
  set -e
  cap_output "$graphify_output"
  rm -f "$graphify_output"
  return "$status"
}

command_name=${1:-status}
case "$command_name" in
  status)
    print_status
    ;;
  query)
    if [ "$#" -lt 2 ]; then
      usage >&2
      exit 64
    fi
    shift
    run_query "$*"
    ;;
  path|explain|affected|god-nodes)
    if ! graph_ready; then
      print_status
      exit 2
    fi
    shift
    cd "$PROJECT_ROOT"
    run_graphify_command "$command_name" "$@"
    ;;
  update)
    if ! has_graphify || [ ! -f "$GRAPH_PATH" ]; then
      print_status
      exit 2
    fi
    shift
    cd "$PROJECT_ROOT"
    graphify update . --no-cluster "$@"
    graphify cluster-only . --no-label --no-viz
    graphify_write_receipt "$PROJECT_ROOT" "$RECEIPT_PATH"
    ;;
  help|-h|--help)
    usage
    ;;
  *)
    usage >&2
    exit 64
    ;;
esac
