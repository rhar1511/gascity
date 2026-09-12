#!/bin/sh
# Install the pinned local Graphify CLI and build a code-only architecture map.
set -eu

GRAPHIFY_VERSION=${GRAPHIFY_VERSION:-0.9.37}
SCRIPT_DIR=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
PROJECT_ROOT=$(CDPATH='' cd -- "$SCRIPT_DIR/.." && pwd)
GRAPH_PATH="$PROJECT_ROOT/graphify-out/graph.json"
RECEIPT_PATH="$PROJECT_ROOT/graphify-out/receipt.json"

# shellcheck source=scripts/graphify-receipt.sh
. "$SCRIPT_DIR/graphify-receipt.sh"

if ! command -v uv >/dev/null 2>&1; then
  printf '%s\n' "error: uv is required (https://docs.astral.sh/uv/)" >&2
  exit 1
fi

installed_version=
if command -v graphify >/dev/null 2>&1; then
  installed_version=$(graphify --version 2>/dev/null || true)
fi

case "$installed_version" in
  *"$GRAPHIFY_VERSION")
    :
    ;;
  *) uv tool install --force "graphifyy[sql,terraform]==$GRAPHIFY_VERSION" ;;
esac

cd "$PROJECT_ROOT"
printf '%s\n' "[graphify] Building local code graph (AST only; no documents, no LLM)..."
graphify extract . --code-only --no-cluster
graphify cluster-only . --no-label --no-viz
graphify_write_receipt "$PROJECT_ROOT" "$RECEIPT_PATH"
printf '%s\n' "[graphify] Ready: $GRAPH_PATH"
printf '%s\n' "[graphify] Receipt: $RECEIPT_PATH"
