---
title: "Retirement wire fixture harness"
description: "Run the disposable retirement verification plan against a read-only external client."
---

## Purpose

The retirement wire harness exports signed, disposable fixture evidence for the
external Python checker. All eight cases use the actual Huma
`/v0/city/{city}/retirement-release/verify` route, request-bound Ed25519 city-write
grants, and canonical authority APIs. The plan is transient and fixture-only;
successful checks grant no activation, trial qualification, or execution permit.

The harness is compiled only with `cross_language_harness`. It creates eight
independently composed numeric-loopback listeners on port zero, a temporary
`HOME`, fixed fixture clocks, and test-only signing seeds. No production keys,
services, accounts, collectors, or trials are needed. Each listener has explicit
timeouts and a 1 MiB HTTP request-body limit.

## Run the Go wire proof

Run from `/home/ricky/gascity-worktrees/gascity/controller-human-gate-adapters`:

```bash
/home/ricky/gascity-pilot/.gc/scripts/go-bounded.sh test \
  -tags=cross_language_harness ./internal/api \
  -run '^TestCrossLanguageRetirementExportRealWire$' -count=1 -timeout=90s
```

## Export a serving plan

The coordinating process must capture stdout while this command remains running:

```bash
GC_RETIREMENT_WIRE_HARNESS=1 \
  /home/ricky/gascity-pilot/.gc/scripts/go-bounded.sh test \
  -tags=cross_language_harness ./internal/api \
  -run '^TestCrossLanguageRetirementHarnessServe$' -count=1 -v -timeout=150s
```

Wait for the line beginning `GC_RETIREMENT_PLAN=`. Save only the JSON following
the equals sign to a disposable file such as
`/tmp/opencode/retirement-wire-plan.json`. The coordinating process can read the
line directly from the subprocess pipe; no polling or readiness sleep is needed.
Keep the serving command alive while running:

```bash
PYTHONDONTWRITEBYTECODE=1 python3 \
  /home/ricky/gascity-worktrees/astra-rsi/gp-olwhg.3-retirement-release/tests/run-retirement-controller-wire.py \
  --plan /tmp/opencode/retirement-wire-plan.json
```

The exported object has exactly `schema_version: 1`, `fixture_only: true`, and
`cases`. Each case has exactly `name`, `base_url`, `city`, `cid`, `epoch`, `now`,
`write_kid`, `write_seed_base64`, `gate`, `manifest_json`, `retained_base64`, and
`expect`. Base URLs are numeric loopback origins. Manifests use complete canonical
JSON; seeds and retained proofs use canonical standard base64. The Python runner
mints only test HTTP grants, using each case's clock, identity, and seed.

## Cases and expected results

| Case | Composition | Expected result |
| --- | --- | --- |
| `positive-compatibility` | Exact scoped canonical authorization and revalidation | `verified`, `fixture`, `canonical_compatibility_verified` |
| `positive-human-review` | Signed observation and review bound to the candidate | `verified`, `fixture`, `exact_signed_review_verified` |
| `positive-external-writers` | Signed host record, canonical ledger, mandatory scope coverage, fenced registry | `verified`, `fixture`, `signed_fenced_external_ledger_join_verified` |
| `positive-in-flight-resolution` | Same canonical join plus independently digested resolution | `verified`, `fixture`, `signed_fenced_in_flight_resolution_join_verified` |
| `invalid-proof` | Retained review with a corrupted signature and matching retention digest | HTTP 503 |
| `revocation` | Valid signed revocation list naming the review record | HTTP 503 |
| `context-mismatch` | Independently composed generation differs from the manifest | HTTP 409 |
| `unsupported-trial-ledger` | Valid signed observation without the useful-acceptance trial contract | `unavailable`, `unavailable`, `signed_observation_verified_useful_acceptance_trial_contract_unavailable` |

Fixture authority composition is fixed before requests arrive. There is no
per-request outcome filter. Negative cases exercise the canonical signature,
revocation, and context checks. The native memory backend provides disposable
route state; this proof does not demonstrate process-durable restart recovery.

## Stop and clean up

After the runner completes, POST to `/__fixture/stop` on any exported `base_url`:

```bash
python3 - /tmp/opencode/retirement-wire-plan.json <<'PY'
import json
import sys
import urllib.request
with open(sys.argv[1], encoding="utf-8") as stream:
    plan = json.load(stream)
request = urllib.request.Request(
    plan["cases"][0]["base_url"] + "/__fixture/stop", data=b"", method="POST")
with urllib.request.build_opener(urllib.request.ProxyHandler({})).open(
        request, timeout=5) as response:
    assert response.status == 204
PY
rm -f /tmp/opencode/retirement-wire-plan.json
```

The fixture stop signal ends the serving test, whose cleanup shuts down and
joins every listener. A 120-second fixture deadline fails the test and performs
the same cleanup if the coordinating process omits stop. Plans and fixture seeds
should not be committed or retained as operational credentials.
## Actual external Python result

The unmodified external retirement runner passed all eight required cases against
the exported real Huma listeners: compatibility, human review, external writer
join, in-flight resolution, invalid proof, signed revocation, context mismatch,
and unavailable trial ledger. Positive results retained `fixture` assurance;
`activation_ready` and `trial_qualified` remained false.

From this controller worktree, the successful coordinated invocation was:

```bash
python3 -B scripts/run-retirement-wire-fixture.py \
  --bounded-go /home/ricky/gascity-pilot/.gc/scripts/go-bounded.sh \
  --runner /home/ricky/gascity-worktrees/astra-rsi/gp-olwhg.3-retirement-release/tests/run-retirement-controller-wire.py
```

The controller-owned coordinator launches the bounded tagged serving test,
captures `GC_RETIREMENT_PLAN`, writes the transient plan in disposable scope,
runs the explicit read-only external consumer, then stops all case listeners and
checks clean fixture exit. It does not edit the pack or create production trust.
