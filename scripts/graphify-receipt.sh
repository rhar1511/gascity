#!/bin/sh
# Shared receipt helpers for the local, code-only Graphify index.
set -eu

graphify_receipt_hash_stdin() {
  if command -v shasum >/dev/null 2>&1; then
    shasum -a 256 | awk '{print $1}'
  elif command -v sha256sum >/dev/null 2>&1; then
    sha256sum | awk '{print $1}'
  else
    printf '%s\n' unknown
  fi
}

graphify_receipt_revision() {
  if command -v git >/dev/null 2>&1 && git -C "$1" rev-parse HEAD 2>/dev/null; then
    return 0
  fi
  printf '%s\n' unknown
}

graphify_receipt_fingerprint() {
  project_root=$1
  if ! command -v git >/dev/null 2>&1 || ! git -C "$project_root" rev-parse --git-dir >/dev/null 2>&1; then
    printf '%s\n' unknown
    return 0
  fi

  {
    git -C "$project_root" rev-parse HEAD 2>/dev/null || true
    git -C "$project_root" diff --no-ext-diff HEAD -- . ':(exclude)graphify-out/**' 2>/dev/null || true
    git -C "$project_root" status --porcelain=v1 --untracked-files=all -- . ':(exclude)graphify-out/**' 2>/dev/null || true
    git -C "$project_root" ls-files --others --exclude-standard -- . ':(exclude)graphify-out/**' 2>/dev/null |
      while IFS= read -r path; do
        [ -n "$path" ] || continue
        printf '%s\n' "$path"
        if [ -f "$project_root/$path" ]; then
          if command -v shasum >/dev/null 2>&1; then
            shasum -a 256 "$project_root/$path"
          elif command -v sha256sum >/dev/null 2>&1; then
            sha256sum "$project_root/$path"
          fi
        fi
      done
  } | graphify_receipt_hash_stdin
}

graphify_receipt_version() {
  if command -v graphify >/dev/null 2>&1; then
    graphify --version 2>/dev/null | tr '\n' ' ' | awk '{$1=$1; print}'
  else
    printf '%s\n' unknown
  fi
}

graphify_write_receipt() {
  project_root=$1
  receipt_path=$2
  revision=$(graphify_receipt_revision "$project_root")
  fingerprint=$(graphify_receipt_fingerprint "$project_root")
  generated_at=$(date -u '+%Y-%m-%dT%H:%M:%SZ')
  graphify_version=$(graphify_receipt_version)
  receipt_tmp="$receipt_path.tmp.$$"

  umask 077
  mkdir -p "$(dirname "$receipt_path")"
  {
    printf '{\n'
    printf '  "schema": 1,\n'
    printf '  "mode": "code-only-ast",\n'
    printf '  "revision": "%s",\n' "$revision"
    printf '  "source_fingerprint": "%s",\n' "$fingerprint"
    printf '  "generated_at": "%s",\n' "$generated_at"
    printf '  "graphify": "%s",\n' "$graphify_version"
    printf '  "graph": "graph.json"\n'
    printf '}\n'
  } >"$receipt_tmp"
  mv -f "$receipt_tmp" "$receipt_path"
}

graphify_receipt_field() {
  field=$1
  receipt_path=$2
  sed -n "s/^[[:space:]]*\"$field\": \"\([^\"]*\)\".*/\1/p" "$receipt_path" | head -n 1
}
