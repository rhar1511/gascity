#!/usr/bin/env bash
set -euo pipefail

# max_modules re-baselined 2026-09-01 for beads v1.3.0-rc.1: measured 727 before
# and 737 after, with ten additions and no removals. Re-measured 2026-09-10 on
# the move to v1.3.0-rc.2: still 737, so the cap is carried forward unchanged
# rather than re-derived. Nine are the OpenAPI
# toolchain behind bd's new `bd serve` HTTP API (kin-openapi, oapi-codegen/v2,
# speakeasy-api/{openapi,jsonpath}, oasdiff/{yaml,yaml3}, vmware-labs/yaml-jsonpath,
# dprotaso/go-yit) plus zeebo/errs; the tenth is cloud.google.com/go/pubsub/v2,
# pulled through by the google.golang.org/api bump MVS forced alongside it. None
# of the OpenAPI stack links into gc -- only bd's internal/httpapi/apigen imports
# it, which the root beads package never reaches.
# Human-approved 2026-10-10 MCP SDK budget: exact baseline 737 -> 741.
# Four additions: modelcontextprotocol/go-sdk v1.8.0, google/jsonschema-go
# v0.4.3, segmentio/encoding v0.5.4 and segmentio/asm v1.1.3. They serve the
# opt-in standalone contrib/mcp-skills adapter; none links into cmd/gc.
# All vendor-family caps and the native binary-size cap stay unchanged.
max_modules="${GC_NATIVE_DEP_MAX_MODULES:-741}"
# max_binary_bytes re-baselined 2026-10-14 (gp-olwhg.9.8). Matching Go 1.26.9
# CGO_ENABLED=0 builds with -trimpath -buildvcs=false measured:
#   18943f295 (2026-09-27): 180,274,346 bytes, previous budget baseline
#   1eec6d3c8 (accepted PR43): 186,984,971 bytes
#   9e6a7e41b (PR47):         187,691,637 bytes (CI: 187,691,549)
# PR43 and PR47 have identical go.mod/go.sum and 737-module graphs. PR47's
# 706,666-byte increase is primarily lifecycle .text (349,824), .gopclntab
# (156,387), .rodata (55,680), and debug/symbol tables. No added dependency
# or product-metrics testhook accounts for the growth. Rebuilding 18943f295
# with this toolchain adds only 21,620 bytes to its old Go 1.26.6 measurement.
# Growth since that baseline is 7,417,291 bytes over 16.561 commit-days:
# about 447,870 bytes/day, including upstream and Beads updates in PR43.
# Applying the existing 30-day planning allowance and rounding up to the next
# decimal MB gives 202,000,000: 14,308,363 bytes (~31.95 days) above PR47.
# This short-window historical rate is an estimate, not a forecast guarantee.
# Keep -trimpath and CGO_ENABLED=0 below: they remove path/native-C variance.
# Re-baseline with fresh measurement + growth-rate evidence, not an
# arbitrary bump, when this next fails.
max_binary_bytes="${GC_NATIVE_DEP_MAX_BINARY_BYTES:-202000000}"
max_aws_modules="${GC_NATIVE_DEP_MAX_AWS_MODULES:-25}"
max_azure_modules="${GC_NATIVE_DEP_MAX_AZURE_MODULES:-9}"
max_dolthub_modules="${GC_NATIVE_DEP_MAX_DOLTHUB_MODULES:-15}"
max_google_api_modules="${GC_NATIVE_DEP_MAX_GOOGLE_API_MODULES:-1}"

modules="$(go list -m all)"
total_modules="$(printf '%s\n' "$modules" | sed '/^$/d' | wc -l | tr -d ' ')"
if [ "$total_modules" -gt "$max_modules" ]; then
	echo "native dependency guard: module graph has $total_modules modules; max is $max_modules" >&2
	exit 1
fi

counts="$(printf '%s\n' "$modules" | awk '
	/^github.com\/aws\/aws-sdk-go-v2( |\/)/ {aws++}
	/^github.com\/Azure\/azure-sdk-for-go( |\/)/ {azure++}
	/^github.com\/dolthub\// {dolthub++}
	/^github.com\/rhar1511\/beads / {beads++}
	/^github.com\/steveyegge\/beads / {upstreambeads++}
	/^google\.golang\.org\/api / {googleapi++}
	END {
		printf "aws=%d azure=%d dolthub=%d beads=%d upstreambeads=%d googleapi=%d\n",
			aws, azure, dolthub, beads, upstreambeads, googleapi
	}
')"
eval "$counts"

if [ "${beads:-0}" -ne 1 ]; then
	echo "native dependency guard: expected exactly one github.com/rhar1511/beads module, got ${beads:-0}" >&2
	exit 1
fi
if [ "${upstreambeads:-0}" -ne 0 ]; then
	echo "native dependency guard: stale upstream github.com/steveyegge/beads module remains alongside the owner library" >&2
	exit 1
fi
if [ "${aws:-0}" -gt "$max_aws_modules" ]; then
	echo "native dependency guard: AWS SDK module count ${aws:-0} exceeds $max_aws_modules" >&2
	exit 1
fi
if [ "${azure:-0}" -gt "$max_azure_modules" ]; then
	echo "native dependency guard: Azure SDK module count ${azure:-0} exceeds $max_azure_modules" >&2
	exit 1
fi
if [ "${dolthub:-0}" -gt "$max_dolthub_modules" ]; then
	echo "native dependency guard: DoltHub module count ${dolthub:-0} exceeds $max_dolthub_modules" >&2
	exit 1
fi
if [ "${googleapi:-0}" -gt "$max_google_api_modules" ]; then
	echo "native dependency guard: google.golang.org/api count ${googleapi:-0} exceeds $max_google_api_modules" >&2
	exit 1
fi

tmpdir="$(mktemp -d)"
trap 'rm -rf "$tmpdir"' EXIT INT TERM HUP
CGO_ENABLED=0 go build -trimpath -o "$tmpdir/gc" ./cmd/gc

go tool nm "$tmpdir/gc" > "$tmpdir/gc.nm"
for forbidden_symbol in \
	"main.runProductMetricsTesthookChild" \
	"main.newProductMetricsTesthookRecordHelpCommand" \
	"internal/productmetrics.OpenTesthook" \
	"internal/productmetrics.testhookLoopbackHost"
do
	if grep -Fq -- "$forbidden_symbol" "$tmpdir/gc.nm"; then
		echo "native dependency guard: normal gc contains product-metrics testhook symbol $forbidden_symbol" >&2
		exit 1
	fi
done

for forbidden_literal in \
	"GC_PRODUCT_METRICS_TESTHOOK_ENDPOINT" \
	"GC_PRODUCT_METRICS_TESTHOOK_CA_FILE" \
	"__testhook-record-help"
do
	if LC_ALL=C grep -aFq -- "$forbidden_literal" "$tmpdir/gc"; then
		echo "native dependency guard: normal gc contains product-metrics testhook literal $forbidden_literal" >&2
		exit 1
	fi
done

mkdir -p "$tmpdir/home" "$tmpdir/gc-home"
if ! HOME="$tmpdir/home" GC_HOME="$tmpdir/gc-home" \
	"$tmpdir/gc" metrics --help > "$tmpdir/metrics-help.txt" 2>&1; then
	echo "native dependency guard: normal gc metrics --help failed" >&2
	cat "$tmpdir/metrics-help.txt" >&2
	exit 1
fi
if grep -Fq -- "__testhook-record-help" "$tmpdir/metrics-help.txt"; then
	echo "native dependency guard: normal gc metrics --help exposes the product-metrics testhook command" >&2
	exit 1
fi

binary_bytes="$(wc -c < "$tmpdir/gc" | tr -d ' ')"
if [ "$binary_bytes" -gt "$max_binary_bytes" ]; then
	echo "native dependency guard: gc binary is $binary_bytes bytes; max is $max_binary_bytes" >&2
	exit 1
fi

echo "native dependency guard: modules=$total_modules aws=${aws:-0} azure=${azure:-0} dolthub=${dolthub:-0} googleapi=${googleapi:-0} binary_bytes=$binary_bytes"
