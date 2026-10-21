#!/usr/bin/env bash
set -euo pipefail

# Native tests must select the released module linked into this consumer.
# The upstream archive/default/current/minimum CLI distributions are separate.
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)"
cd "$repo_root"
module="$(sed -n 's/^BEADS_LIBRARY_MODULE=//p' deps.env)"
version="$(sed -n 's/^BEADS_LIBRARY_VERSION=//p' deps.env)"
source_ref="$(sed -n 's/^BEADS_LIBRARY_REF=//p' deps.env)"
case "$module@$version:$source_ref" in
  github.com/rhar1511/beads@v1.3.2:11863eb43b8807adaa4aa0f2e9b8fd4aeb83f624)
    module_sum='h1:aQ6Di/V7qmK8F54HJhYMn0j3qCbUiHI7UdwEcp+EsNc='
    gomod_sum='h1:3GXUkW2Ju+qM/wiuj8XT6owIVUZzUkBZtdAIC4IVid4='
    ;;
  *) echo "Unsupported qualified Beads module/version/source pin" >&2; exit 1 ;;
esac
for pin in "$version $module_sum" "$version/go.mod $gomod_sum"; do
  if [[ "$(awk -v module="$module" -v pin="$pin" '$1 == module && $2 == substr(pin, 1, index(pin, " ") - 1) { print $2 " " $3 }' go.sum)" != "$pin" ]]; then
    echo "Qualified Beads requires the exact authenticated consumer go.sum entries" >&2
    exit 1
  fi
done

case "$(uname -s)" in
  Linux) os=linux ;;
  Darwin) os=darwin ;;
  *) echo "Unsupported qualified Beads host" >&2; exit 1 ;;
esac
case "$(uname -m)" in
  x86_64|amd64) arch=amd64 ;;
  arm64|aarch64) arch=arm64 ;;
  *) echo "Unsupported qualified Beads architecture" >&2; exit 1 ;;
esac
for tool in go jq pkg-config; do
  command -v "$tool" >/dev/null || { echo "Required tool missing: $tool" >&2; exit 1; }
done
export GOTOOLCHAIN=local GOENV=off GOWORK=off GOFLAGS=
if [[ "$(go env GOOS)" != "$os" || "$(go env GOARCH)" != "$arch" ]]; then
  echo "Qualified Beads requires a native host build" >&2
  exit 1
fi
if [[ "$os" == darwin ]] && command -v brew >/dev/null; then
  if icu_prefix="$(brew --prefix icu4c 2>/dev/null)"; then
    export PKG_CONFIG_PATH="$icu_prefix/lib/pkgconfig${PKG_CONFIG_PATH:+:$PKG_CONFIG_PATH}"
  fi
fi
if ! pkg-config --exists icu-i18n; then
  echo "Qualified Beads requires ICU headers and libraries (libicu-dev or Homebrew icu4c)" >&2
  exit 1
fi
export CGO_ENABLED=1
export CGO_CPPFLAGS="${CGO_CPPFLAGS:-} $(pkg-config --cflags icu-i18n)"
export CGO_LDFLAGS="${CGO_LDFLAGS:-} $(pkg-config --libs icu-i18n)"

build_dir="$(mktemp -d "${RUNNER_TEMP:-${TMPDIR:-/var/tmp}}/gascity-qualified-bd.XXXXXX")"
# Never remove shared module/build caches or an earlier installed executable.
trap 'rm -rf -- "$build_dir"' EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM
# The consumer imports storage, not the complete CLI package closure. Hydrate
# CLI-only checksums in a private copy; never edit the checkout's module files.
cp "$repo_root/go.mod" "$build_dir/original.go.mod"
cp "$repo_root/go.sum" "$build_dir/original.go.sum"
mkdir "$build_dir/consumer"
cp "$build_dir/original.go.mod" "$build_dir/consumer/go.mod"
cp "$build_dir/original.go.sum" "$build_dir/consumer/go.sum"
cd "$build_dir/consumer"
go mod edit -json > "$build_dir/consumer.json"
jq -e --arg module "$module" --arg version "$version" '
  [.Require[] | select(.Path == $module)] as $pins |
  ($pins | length) == 1 and $pins[0].Version == $version and ($pins[0].Indirect != true) and
  ([.Require[] | select(.Path == "github.com/steveyegge/beads")] | length) == 0 and
  ([.Replace[]? | select(.Old.Path == $module or .New.Path == $module)] | length) == 0
' "$build_dir/consumer.json" >/dev/null || {
  echo "Qualified Beads requires the direct, unreplaced consumer module pin" >&2
  exit 1
}
go list -mod=readonly -m -json "$module" > "$build_dir/resolved.json"
jq -e --arg module "$module" --arg version "$version" --arg sum "$module_sum" '
  .Path == $module and .Version == $version and .Sum == $sum and
  (.Replace == null) and (.Error == null) and (.Main != true)
' "$build_dir/resolved.json" >/dev/null || {
  echo "Qualified Beads consumer resolution differs from the released module" >&2
  exit 1
}
go mod download -json "$module@$version" > "$build_dir/download.json"
jq -e --arg module "$module" --arg version "$version" --arg sum "$module_sum" --arg gomod "$gomod_sum" '
  .Path == $module and .Version == $version and .Sum == $sum and
  .GoModSum == $gomod and (.Error == null)
' "$build_dir/download.json" >/dev/null || {
  echo "Qualified Beads module download authentication failed" >&2
  exit 1
}
# Load the selected graph before and after the normal CLI package resolution.
# The qualified consumer graph needs extra sums, not new module versions or
# requirements. Any graph or go.mod drift fails instead of becoming a fallback.
go list -mod=mod -m -json all > "$build_dir/modules-before.json"
if ! cmp -s "$build_dir/original.go.mod" go.mod; then
  echo "Qualified Beads initial module load changed the consumer go.mod" >&2
  exit 1
fi
go list -mod=mod -deps "$module/cmd/bd" > "$build_dir/cli-packages"
if ! cmp -s "$build_dir/original.go.mod" go.mod; then
  echo "Qualified Beads CLI resolution changed the consumer go.mod" >&2
  exit 1
fi
go list -mod=readonly -m -json all > "$build_dir/modules-after.json"
for phase in before after; do
  jq -se --arg module "$module" --arg version "$version" '
    length > 0 and all(.[]; .Replace == null) and
    ([.[] | select(.Path == $module and .Version == $version and .Main != true)] | length) == 1 and
    ([.[].Path] | length) == ([.[].Path] | unique | length)
  ' "$build_dir/modules-$phase.json" >/dev/null || {
    echo "Qualified Beads selected module graph is incomplete or replaced" >&2
    exit 1
  }
  jq -s '[.[] | {Path, Version}] | sort_by(.Path)' "$build_dir/modules-$phase.json" > "$build_dir/versions-$phase.json"
done
if ! cmp -s "$build_dir/versions-before.json" "$build_dir/versions-after.json"; then
  echo "Qualified Beads CLI resolution changed the selected module versions" >&2
  exit 1
fi
for pin in "$version $module_sum" "$version/go.mod $gomod_sum"; do
  if [[ "$(awk -v module="$module" -v pin="$pin" '$1 == module && $2 == substr(pin, 1, index(pin, " ") - 1) { print $2 " " $3 }' go.sum)" != "$pin" ]]; then
    echo "Qualified Beads staged release checksum changed during CLI resolution" >&2
    exit 1
  fi
done
go mod verify
# A dependency-package build retains real released Main.Path/Version/Sum;
# building ./cmd/bd from a source checkout would instead record (devel).
go build -mod=readonly -trimpath -o "$build_dir/bd" "$module/cmd/bd"
go version -m "$build_dir/bd" > "$build_dir/buildinfo"
awk -v module="$module" -v version="$version" -v sum="$module_sum" -v os="$os" -v arch="$arch" '
  $1 == "path" { paths++; if ($2 != module "/cmd/bd") bad=1 }
  $1 == "mod" { mods++; if ($2 != module || $3 != version || $4 != sum) bad=1 }
  $1 == "=>" { bad=1 }
  $1 == "build" && $2 == "CGO_ENABLED=1" { cgo++ }
  $1 == "build" && $2 == "GOOS=" os { hostos++ }
  $1 == "build" && $2 == "GOARCH=" arch { hostarch++ }
  /gms_pure_go/ { bad=1 }
  END { exit !(paths == 1 && mods == 1 && cgo == 1 && hostos == 1 && hostarch == 1 && !bad) }
' "$build_dir/buildinfo" || {
  echo "Qualified Beads binary lacks released module/native build provenance" >&2
  exit 1
}
"$build_dir/bd" version
if ! cmp -s "$build_dir/original.go.mod" go.mod ||
   ! cmp -s "$build_dir/original.go.mod" "$repo_root/go.mod" ||
   ! cmp -s "$build_dir/original.go.sum" "$repo_root/go.sum"; then
  echo "Qualified Beads consumer module input changed during the build" >&2
  exit 1
fi

install_dir="${RUNNER_TOOL_CACHE:-$HOME/.local/share}/gascity-qualified-bd/$source_ref/$os-$arch"
case "$install_dir" in
  /*) ;;
  *) echo "Qualified Beads install directory must be absolute" >&2; exit 1 ;;
esac
case "$install_dir" in
  *$'\n'*|*$'\r'*) echo "Qualified Beads install path contains a line break" >&2; exit 1 ;;
esac
mkdir -p "$install_dir"
mv -f "$build_dir/bd" "$install_dir/bd"
mv -f "$build_dir/buildinfo" "$install_dir/buildinfo"
cp go.mod "$install_dir/consumer.go.mod"
cp go.sum "$install_dir/consumer.go.sum"
mv -f "$build_dir/versions-after.json" "$install_dir/selected-module-versions.json"
printf '%s %s %s %s\n' "$module" "$version" "$module_sum" "$source_ref" > "$install_dir/source-provenance"
# Publish selection only after authentication, build, provenance and smoke pass.
if [[ -n "${GITHUB_PATH:-}" ]]; then
  printf '%s\n' "$install_dir" >> "$GITHUB_PATH"
fi
if [[ -n "${GITHUB_ENV:-}" ]]; then
  printf 'BEADS_TEST_BD_BINARY=%s/bd\nGC_INTEGRATION_REAL_BD=%s/bd\nGC_ACCEPTANCE_BD_BIN=%s/bd\n' "$install_dir" "$install_dir" "$install_dir" >> "$GITHUB_ENV"
fi
printf 'Qualified integration bd: %s/bd\n' "$install_dir"
