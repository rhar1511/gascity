#!/usr/bin/env bash
set -euo pipefail

# Opt-in test dependency for the HTTP snapshot and lifecycle integration suites.
# The default released bd and its compatibility matrix remain separate.
source_ref="3c3423cf09a9557a85d4bebf31a425f49511dfed"
source_sha="51b9a43a8b72bf9ebf0ac617526709fe90390409f6689ed67db307eb83463614"
source_url="https://codeload.github.com/rhar1511/beads/tar.gz/$source_ref"

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

for tool in go curl tar pkg-config; do
  command -v "$tool" >/dev/null || { echo "Required tool missing: $tool" >&2; exit 1; }
done
export GOTOOLCHAIN=local
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
# Remove only this invocation's fresh source directory, never shared Go caches.
trap 'rm -rf -- "$build_dir"' EXIT
archive="$build_dir/source.tar.gz"
curl --fail --silent --show-error --location --retry 3 --connect-timeout 20 --max-time 180 "$source_url" -o "$archive"
if command -v sha256sum >/dev/null; then
  actual_sha="$(sha256sum "$archive" | awk '{print $1}')"
else
  actual_sha="$(shasum -a 256 "$archive" | awk '{print $1}')"
fi
if [[ "$actual_sha" != "$source_sha" ]]; then
  echo "Qualified Beads source checksum mismatch" >&2
  exit 1
fi
mkdir "$build_dir/source"
tar -xzf "$archive" --strip-components=1 -C "$build_dir/source"
(
  cd "$build_dir/source"
  go mod verify
  go build -mod=readonly -trimpath -ldflags "-X main.Commit=$source_ref" -o "$build_dir/bd" ./cmd/bd
)
"$build_dir/bd" version
install_dir="${RUNNER_TOOL_CACHE:-$HOME/.local/share}/gascity-qualified-bd/$source_ref/$os-$arch"
mkdir -p "$install_dir"
mv -f "$build_dir/bd" "$install_dir/bd"
printf '%s\n' "$source_ref $source_sha" > "$install_dir/source-provenance"
if [[ -n "${GITHUB_PATH:-}" ]]; then
  printf '%s\n' "$install_dir" >> "$GITHUB_PATH"
fi
if [[ -n "${GITHUB_ENV:-}" ]]; then
  printf 'BEADS_TEST_BD_BINARY=%s/bd\nGC_INTEGRATION_REAL_BD=%s/bd\n' "$install_dir" "$install_dir" >> "$GITHUB_ENV"
fi
printf 'Qualified integration bd: %s/bd\n' "$install_dir"
