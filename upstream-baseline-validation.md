# Upstream pilot baseline — source integration handoff

Candidate is an **uncommitted merge** in the requested isolated worktree.
`HEAD` remains `90a0a050188c43b3884530c6cf5aee2cae5f8cff`; `MERGE_HEAD` is
`b5df315a82b38fd2379eb4255e311ab4fa7f588d`. No unresolved Git index conflicts.
See `upstream-baseline-disposition.json` for exact refs and residual ownership.

## Semantic conflict resolutions

- `internal/session/store.go`: upstream atomic terminal close, rollback and
  lifecycle fences coexist with fork CAS/purge-fenced reopening. The upstream
  unfenced `SetStatusOpen` did not replace the fork implementation.
- `internal/beads/event_payload{,_test}.go`: retain copied-map credential
  redaction and upstream envelope/payload identity consistency checks.
- `internal/api/idempotency_guard_test.go`: union of upstream session action
  keys/reset and fork request/ack classifications.
- `go.sum`: retain required parquet source fix and upstream checksum additions.
- Bazel files: union fork receipt/factory/RSI tests and pack assets with upstream
  tests and current dependency labels; dashboard asset list follows new build.
- `.github/workflows/bazel-test.yml`: fork hosted-runner fix plus upstream
  acceptance invocation/prewarm/cache behavior.
- OpenAPI, generated Go/TS clients and hashed SPA conflicts were resolved by
  generation from integrated source, not by choosing either stale artifact.

## Integration regressions and fixes

1. Exact tmux follow-up sites: auxiliary session/window/pane operations could
   still select a surviving prefix sibling. `TestAuxiliaryOpsNeverTargetPrefixSibling`
   failed in all eleven cases before the fix, then passed. Uses the executor
   seam; no live user session probe or source-string test.
2. Watcher exclusion: absolute `.gc` ancestors prevented valid reload events.
   `TestConfigWatchExcludesRuntimeRelativeToWatchRoot` failed before the change;
   it and the existing real watcher/reload tests pass afterwards.
3. Batch close: upstream's batched read cannot authorize an unfenced write over
   fork receipt/purge protections. Preserve batched verification, precheck all
   rows, fence each close, preserve close reason, refuse unsupported revisions,
   and report partial success on a later CAS conflict. Executable stale-revision
   and purge-preflight tests pass. This is per-row CAS, not atomic whole-batch
   mutation.
4. Existing upstream executable claim identity/live-drain, nudge retry and
   reload/capacity seams were adequate and retained. Fork HTTP receipt replay
   waits now consume the event stream instead of three polling sleeps.
5. Resource/golden/inventory unions were updated explicitly: retained #32/#33
   boundary proofs, two existing fork env keys, and one new tmux test. No test
   was removed to obtain a passing result; no sleep budget was increased.

## Validation environment

Working directory for Go commands is the requested worktree. Define these
abbreviations to reproduce the exact commands below:

```bash
B=/home/ricky/gascity-pilot/.gc/scripts/go-bounded.sh
G=/home/ricky/.local/opt/go1.26.6/bin
M=/home/ricky/.local/opt/make-4.4.1/usr/bin
```

Every top-level Go operation used `$B`. Sharded shell runners used its supported
`GC_BOUNDED_GO_BIN=/bin/bash` executor: the outer wrapper held the shared lock
and set `GOMAXPROCS=2`, `-p=1`, disk scratch, and `LOCAL_TEST_JOBS=1` for the
whole runner and its nested Go commands. No concurrent heavy validation jobs.
Host snapshots were `resource_scope=host`, YELLOW with no active memory PSI or
swap I/O; no pool growth was performed. Wrapper admission also checked disk/RAM.

Go 1.26 uses `GOTMPDIR` for `t.TempDir`, so test processes receive both
`TMPDIR=/var/tmp` and `GOTMPDIR=/var/tmp` through `-exec`. Compilation scratch
remains wrapper-owned. This avoids the Unix-socket length limit and synthetic
fixtures being discovered inside the live city's `.gc` tree. `GOFLAGS=-buildvcs=false`
must also be inherited by tests that build fixture binaries in Git worktrees.

## Exact successful commands/results

```bash
$B run ./cmd/genspec
$B run ./cmd/gen-client > internal/api/genclient/client_gen.next
mv -f internal/api/genclient/client_gen.next internal/api/genclient/client_gen.go
$B run ./cmd/genschema
$B run ./cmd/gen-command-census
```

All passed. The temporary bootstrap Go client came from fork HEAD solely to
allow compilation; the final client was generated from the merged live spec.
An initial genspec attempt failed on transient proxy.golang.org DNS; the next
attempt downloaded the exact required Beads module and passed. A `go generate`
attempt hit a tool timeout; direct bounded generation then completed.

```bash
GC_FAST_UNIT=1 GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null \
PATH="$G:$PATH" $B test -buildvcs=false -count=1 -timeout=3m \
  -exec 'env TMPDIR=/var/tmp GOTMPDIR=/var/tmp' ./internal/...
```

All internal packages passed except `internal/testenv` (golden omitted two
existing fork keys) and `internal/worker/workertest` (nested fixture build lacked
inherited buildvcs flag). Both then passed with:

```bash
GC_FAST_UNIT=1 GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null \
GOFLAGS=-buildvcs=false PATH="$G:$PATH" $B test -count=1 -timeout=5m \
  -exec 'env TMPDIR=/var/tmp GOTMPDIR=/var/tmp' \
  ./internal/testenv ./internal/worker/workertest ./internal/bootstrap/packs/core
```

The final core pack rerun passed after preserving upstream-retired
`dolt-target.sh` outside embedded packs, as a reference script.

```bash
PATH="$G:$PATH" GC_FAST_UNIT=1 GO_TEST_COUNT=1 \
GOFLAGS="-buildvcs=false '-exec=env TMPDIR=/var/tmp GOTMPDIR=/var/tmp'" \
GC_BOUNDED_GO_BIN=/bin/bash $B -c \
  'for shard in 1 2 3 4 5 6; do scripts/test-go-test-shard ./cmd/gc "$shard" 6 || exit; done'
```

Shards 1–5 passed (50.506s, 63.294s, 69.659s, 68.437s, 56.743s).
Shard 6 initially failed because inserting a test shifted an existing lint
test's pinned source line numbers. Moving the new test to the publication-test
file restored that existing guard; no lint exception was weakened.

```bash
PATH="$G:$PATH" GC_FAST_UNIT=1 GO_TEST_COUNT=1 \
GOFLAGS="-buildvcs=false '-exec=env TMPDIR=/var/tmp GOTMPDIR=/var/tmp'" \
GC_BOUNDED_GO_BIN=/bin/bash $B scripts/test-go-test-shard ./cmd/gc 6 6
GC_FAST_UNIT=1 $B test -count=1 -exec 'env TMPDIR=/var/tmp GOTMPDIR=/var/tmp' \
  -run '^(TestConfigWatchExcludesRuntimeRelativeToWatchRoot|TestFencedBatchClose)' ./cmd/gc
```

Passed (62.296s and 0.348s). After the final batch-close output-map fold, the
`TestFencedBatchClose` focused rerun also passed (0.349s).

```bash
$B test -count=1 ./internal/testpolicy/resourcecensus \
  -run '^TestRepositoryLedgerMatchesCensusAndDocumentation$' -args -update
$B test -count=1 -run '^TestRuntimeTmuxManifest' ./scripts
$B test -count=1 ./internal/bootstrap/packs/core
$B vet ./...
$B build -buildvcs=false -o gc-baseline-candidate ./cmd/gc
```

All passed. Build output was removed after validation. Hook path is `.githooks`;
no commit/hook-triggering publication was attempted.

Dashboard npm was not on PATH. `/tmp/opencode/npm` invoked the already installed
Node/npm CLI (npm 12.1.0); no global installation/config change was made. From
`internal/api/dashboardspa/web`, with `PATH="/tmp/opencode:$PATH"`:

```bash
npm ci --ignore-scripts --no-audit --no-fund
npm run generate:client
npm run build
npm run typecheck
npm run --workspace gas-city-dashboard-frontend typecheck:test
npm run --workspace gas-city-dashboard-frontend typecheck:e2e
npm run --workspace gas-city-dashboard-frontend test -- --maxWorkers=1 --no-file-parallelism
```

All passed; Vitest reported **103 files / 982 tests**, 132.90s. Error-boundary
tests deliberately log exceptions but pass. The generated frontend dist was
copied into the embedded dist after replacing obsolete generated chunks.
Isolated Vite preview on an ephemeral loopback port returned **HTTP 200,
1140 bytes** and the owned preview process was terminated.

## Broader attempts and unresolved blockers

```bash
PATH="$G:$PATH" GC_FAST_UNIT=1 GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null \
GOFLAGS=-buildvcs=false $B test -count=1 -timeout=5m \
  -exec 'env TMPDIR=/var/tmp GOTMPDIR=/var/tmp' \
  ./cmd/gen-command-census ./pkg/... ./examples/... ./test/... ./scripts/...
```

- Generator, public package, examples other than Dolt, docsync, qualification,
  packlint, and untagged test helper packages passed.
- **Historical failure: `examples/bd/dolt` timed out at 300.021s.** The follow-up
  below diagnoses the under-budget validation invocation and verifies the
  formerly named fixtures and complete package. The original failed result is
  retained; no product assertion or per-call timeout was relaxed.
- `scripts` initially failed because `make` was absent from PATH and the new
  tmux test needed manifest registration. With `$M` on PATH, all non-inventory
  tests passed. The inventory-only rerun passed after registration/count update:

```bash
PATH="$G:$M:$PATH" GC_FAST_UNIT=1 GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null \
GOFLAGS=-buildvcs=false $B test -count=1 -timeout=5m \
  -exec 'env TMPDIR=/var/tmp GOTMPDIR=/var/tmp' ./scripts
```

This second scripts run took 114.155s and failed only the two inventory count
assertions. The subsequent `TestRuntimeTmuxManifest` run passed (0.049s).

- `scripts/test-local-parallel fast` was also attempted under `$B`, with one
  job. Its first run exposed fixture placement/VCS problems fixed above; its
  second run exceeded the tool's 20-minute timeout during unit-core. It did not
  produce an overall passing fast-baseline result. Separate package/shard
  results above are the evidence; they do not erase the incomplete run.
- Bazel is unavailable: Bazel execution remains unqualified.
- Existing whitespace findings are unchanged upstream release-gate/docs/shell
  content and an unchanged fork mirror service. No whitespace fix is presented
  as behavioral qualification.
- Tagged real-provider, credentialed, live-inference and live-city trials were
  not run. Normal tests may skip unavailable external binaries; a package PASS
  is not a claim of real Herdr/tmux/Beads service qualification.

Main agent still owns independent review, staged pre-commit validation, necessity
review of retained compiled Workbench/factory seams, and any eventual commit.
The two reference integration histories were never replayed or overwritten.

## Follow-up: compact timeout diagnosis and resolution

Inspection began at staged tree
`2b45c285bb895aecff83ab1f0cb0b2b2f1a88651`, with no unstaged or untracked paths.
The staged merge inventory and upstream-relative script differences were
inspected. No source change occurred between the timeout and this diagnosis.

The captured 300-second stack named
`TestCompactScriptPrefersOriginWhenMultipleRemotesExist` and
`TestCompactScriptUsesExplicitRemote`; many other tests were still paused in
`testing.T.Parallel`. A temporary driver launched only the latter fixture via
the bounded launcher in its own process group and captured only descendants of
that launch. Its shell was waiting for a command-substitution pipe, while its
child was `timeout --kill-after=2 20 dolt ... SELECT DOLT_HASHOF_DB('HEAD')`.
The fixture-local fake `dolt` was first on PATH, `GC_CITY_PATH` pointed into its
owned `/var/tmp/TestCompactScriptUsesExplicitRemote...` directory, and its port
was the test's ephemeral listener. No lock-wait evidence or real/operator Dolt
process was found. That test completed normally in **4.165s**; the diagnostic
driver sent no termination signal. Temporary instrumentation remained outside
the source tree.

The two formerly named cases were then verified together:

```bash
GC_FAST_UNIT=1 GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null \
GOFLAGS=-buildvcs=false PATH="$G:$M:$PATH" $B test -count=1 -v -timeout=60s \
  -exec 'env TMPDIR=/var/tmp GOTMPDIR=/var/tmp' \
  -run '^(TestCompactScriptUsesExplicitRemote|TestCompactScriptPrefersOriginWhenMultipleRemotesExist)$' \
  ./examples/bd/dolt
```

**PASS, 4.378s**. The cases took 4.24s and 4.36s respectively.

```bash
GC_FAST_UNIT=1 GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null \
GOFLAGS=-buildvcs=false PATH="$G:$M:$PATH" $B test -count=1 -json -timeout=20m \
  -exec 'env TMPDIR=/var/tmp GOTMPDIR=/var/tmp' ./examples/bd/dolt \
  > /tmp/opencode/baseline-dolt-package.jsonl
```

**PASS, 347.605s**: 483 test/subtest pass events, zero fail or skip events.
The recorded interval was 09:29:46.966996583Z–09:35:34.571922177Z on 2026-10-01.
Aggregate completed-test time was 633.95s; the longest completed case was
10.99s. With the wrapper's `GOMAXPROCS=2`, Go permits two parallel tests at once,
so aggregate fixture work plus serial setup exceeds the earlier arbitrary
300-second package cap. The normal fast runner already supplies a **20-minute**
package budget. The fix is using that existing budget in this validation
invocation, not changing product code, subprocess timeout values, resource
admission, fixture concurrency or assertions. There is no product/test-harness
defect demonstrated here that would justify a new behavioral regression test.

Diagnostic logs are `/tmp/opencode/baseline-compact-focused.log` and
`/tmp/opencode/baseline-dolt-package.jsonl`. No broad test suite was rerun.

### Staged lint and hook prerequisites

```bash
PATH="$G:$M:$PATH" GOFLAGS=-buildvcs=false GC_BOUNDED_GO_BIN=/bin/bash \
  $B -c 'make check-hooks && make lint-changed LINT_CHANGED_SCOPE=staged LINT_FLAGS="--new-from-rev=HEAD --whole-files" GOLANGCI_LINT=/home/ricky/go/bin/golangci-lint'
PATH="$G:$M:/home/ricky/go/bin:$PATH" GOFLAGS=-buildvcs=false \
  GC_BOUNDED_GO_BIN=/bin/bash \
  $B -c 'make fmt-check-changed LINT_CHANGED_SCOPE=staged GOLANGCI_LINT=/home/ricky/go/bin/golangci-lint && make check-docs && make vet'
```

All **PASS**: `.githooks` active, staged lint **0 issues**, no formatter drift,
docsync 5.843s, vet clean. The outer wrapper retained the serial lock and limits
through Make and the nested Go/lint commands. No `--fix`, commit or hook bypass
was used.

OpenAPI and schemas were regenerated again through `$B run ./cmd/genspec` and
`$B run ./cmd/genschema`; `$B run ./cmd/gen-command-census -check` passed. The
Go client was generated into `internal/api/genclient/client_gen.check`, compared
byte-for-byte with `client_gen.go`, then the temporary generated check file was
removed. `git diff --exit-code` on all generated paths passed. Full pre-commit
execution, including the Beads hook chain, remains for the committing main
agent; no local `.beads` database exists in this worktree.

### Retained script rationale

Every currently embedded core script remains byte/mode identical to pinned
upstream. The Dolt pack now differs only by the empty-EOF-line cleanup noted
below. The active `gc-beads-bd.sh` behavioral exception is fork #33's
private store bridge: it preserves raw credentials for authoritative internal
reads while public projections redact them.

The five old `test/reaper_*_test.sh` sources and archived `dolt-target.sh` were
retained under the initial operator **no-script-removal** instruction. They are
**not required private contracts**, have no current runtime/order/CI callers,
and are not presented as executable tests of upstream's rewritten reaper.
Their historical extracted Step 4/Step 6/DB-map interfaces were retired
upstream. Keeping these source references does not restore those interfaces.
The helper is outside all embedded packs/imports/orders because upstream's
`TestCoreMaintenanceExecAssets` explicitly requires its runtime retirement.
The machine-readable manifest now records a separate reason for each of the
six files. Removing these reference sources would be a source-retention
decision, not a necessary code fix or a live-city retirement action.

### Remaining limits

The compact timeout is explained and its package now passes. No unexplained
compact failure remains. Bazel is unavailable, and full pre-commit invocation
plus independent review/commit remain outstanding. Earlier incomplete broad
runner attempts remain in the history; per-package/shard results are the
source-validation evidence. No commit, push, deploy, active-role change,
operator-process kill or live tmux probe occurred.

## Final review corrections

### Explicit city config errors and file-store IDs

Standards P1 identified that `effectiveFileStorePrefix` swallowed config-load
errors and let an explicitly selected city mint default `gc` IDs. The initial
executable regression reproduced that outcome for malformed, unreadable and
missing configs: each admitted the public `Store.Create` operation and minted
`gc-1`.

`fileStoreIDPrefixOpts` and `effectiveFileStorePrefix` now return errors, and
the file-store opener propagates them before constructing a store. Diagnostics
retain the selected config path. Standalone scopes with no selected city retain
default-prefix behavior. The factory now distinguishes its inferred fallback
scope path from an explicit city context; the standalone factory case caught
and prevented an over-strict first implementation.

The final regression exercises the real store factory and public `Store.Create`
boundary, checks all three invalid config shapes, and verifies that neither
scope nor city materializes a file store on refusal. Positive cases cover both
standalone openers and the existing configured HQ/rig prefix regression.

Focused graph tests exposed an old shared-file-store fixture containing retired
PackV1 agent and rig-path fields. It had relied on the swallowed config error.
The fixture now uses the existing site-binding writer, retaining its shared
city store and no-rig-state assertions.

```bash
GC_FAST_UNIT=1 GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null \
  $B test -count=1 -timeout=3m \
  -exec 'env TMPDIR=/var/tmp GOTMPDIR=/var/tmp' \
  -run '^(TestFileStoreCreateRejectsInvalidExplicitCity|TestStandaloneFileStoreCreateKeepsDefaultPrefix|TestOpenCompatibleFileStoreUsesCallerCityForIDPrefix|TestOpenRigAwareStore.*)$' \
  ./cmd/gc
```

Final result: **PASS, 0.651s**. The regression has no source-grep assertion.

### Executable Make memory-limit contract

`TestLintTargetsApplyMemoryLimit` no longer requires literal Makefile variable
expressions. It reuses the existing small Git/Go fixture, invokes real Make and
a recording lint executable, and observes child-process `GOMEMLIMIT` for all
seven lint/format targets. Both the default `6GiB` and explicit `1GiB` override
must replace an ambient `256MiB` value; each target must actually invoke the
recording executable. No new dependency or expensive lint analysis is needed
inside this test. Other pre-existing readonly-module source assertions are
outside this requested replacement.

```bash
PATH="$G:$M:$PATH" GOFLAGS=-buildvcs=false \
  $B test -count=1 -timeout=3m \
  -exec 'env TMPDIR=/var/tmp GOTMPDIR=/var/tmp' \
  -run '^TestLintTargetsApplyMemoryLimit$' ./scripts
$B vet ./...
```

Final results: **PASS, 1.743s**, and **vet PASS**.

### Whitespace and staged gates

- Removed one empty EOF line from `engdocs/bazel-quickstart.md` and
  `examples/bd/dolt/assets/scripts/runtime.sh`.
- Replaced trailing-space Markdown hard breaks with backslash hard breaks in
  `release-gates/ga-oyyeq5-systemd-unit-dir-gate.md`, preserving its rendering.
- Staged lint initially found one gofumpt multiline-literal issue in the new
  executable Make test. Its formatting was corrected; the subsequent gate
  passed with **0 issues**.

```bash
PATH="$G:$M:/home/ricky/go/bin:$PATH" GOFLAGS=-buildvcs=false \
  GC_BOUNDED_GO_BIN=/bin/bash $B -c \
  'make lint-changed LINT_CHANGED_SCOPE=staged LINT_FLAGS="--new-from-rev=HEAD --whole-files" GOLANGCI_LINT=/home/ricky/go/bin/golangci-lint && make fmt-check-changed LINT_CHANGED_SCOPE=staged GOLANGCI_LINT=/home/ricky/go/bin/golangci-lint'
git diff --cached --check HEAD
git diff --check HEAD
```

All **PASS**, against unchanged fork parent
`90a0a050188c43b3884530c6cf5aee2cae5f8cff`.
The staged source tree before this handoff update is
`2d728b4dd87864b04565ffb3d54b0025138848c2`.
Only focused affected tests and the requested vet/lint/format/diff checks were
run after these fixes. No broad suite, commit or push was performed.
