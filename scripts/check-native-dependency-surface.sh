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
max_modules="${GC_NATIVE_DEP_MAX_MODULES:-737}"
# max_binary_bytes re-baselined 2026-09-27 (gp-olwhg.9). Matching Go 1.26.6
# CGO_ENABLED=0 builds with -trimpath -buildvcs=false measured 177,910,874
# bytes at fork baseline 284fd816f and 180,252,726 at 18943f295. The latter
# is within 24 bytes of CI's normal VCS-stamped build (180,252,750).
# go.mod/go.sum are identical across that comparison; the 2,341,852-byte
# increase is primarily .text (1,068,672), .gopclntab (493,403), .rodata
# (244,864), and debug/symbol tables for the new controller APIs/lifecycle.
# The previous measured baseline was 172,098,757 on 2026-08-29 (ga-iuznq2).
# Its 29-day growth to the pre-task fork baseline is about 200,418 bytes/day.
# 187,000,000 leaves 6,747,250 bytes (~33.7 days at that observed rate),
# rounding a 30-day planning allowance up to the next decimal MB. This
# short-window historical rate is an estimate, not a forecast guarantee.
# Keep -trimpath and CGO_ENABLED=0 below: they remove path/native-C variance.
# Re-baseline with fresh measurement + growth-rate evidence, not an
# arbitrary bump, when this next fails.
max_binary_bytes="${GC_NATIVE_DEP_MAX_BINARY_BYTES:-187000000}"
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
	/^github.com\/steveyegge\/beads / {beads++}
	/^google\.golang\.org\/api / {googleapi++}
	END {
		printf "aws=%d azure=%d dolthub=%d beads=%d googleapi=%d\n",
			aws, azure, dolthub, beads, googleapi
	}
')"
eval "$counts"

if [ "${beads:-0}" -ne 1 ]; then
	echo "native dependency guard: expected exactly one github.com/steveyegge/beads module, got ${beads:-0}" >&2
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
