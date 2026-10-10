# Optional individual-call prevention benchmark

This consumer tool evaluates one proposed CLI, MCP or skill call per frozen
pre-call opportunity. It executes no proposed calls, installs no host hooks,
and changes neither Gas City's RSI gate nor Beads permissions. Beads remains
the work/lesson authority; existing story benchmarks remain the end-to-end
user-agency guardrail.

## Run the synthetic demonstration

From the repository root:

```sh
go run ./contrib/toolcall-benchmark \
  --suite contrib/toolcall-benchmark/testdata/suite.json \
  --baseline contrib/toolcall-benchmark/testdata/baseline.example.json \
  --candidate contrib/toolcall-benchmark/testdata/candidate.example.json \
  --improver improver
```

The 12 checked-in synthetic opportunities demonstrate ten corrections and two
preserved valid controls. They are not real agent captures, held-out tests,
model-performance measurements or deployment evidence. Zero cost values in
these examples describe the fixture, not a free production preflight.

Exit 0 means a valid diagnostic report, including reports with regressions.
Exit 2 means invalid input or an I/O error. Neither code grants promotion
eligibility. Reports include paired per-case outcomes, improvements,
regressions and separate totals for correct decisions, invalid permitted calls,
proposals outside frozen authorization, necessary-call omissions, false
rejections and preflight calls/tokens/duration. A necessary-call deferral counts
as both an omission and a false rejection; do not add those counters together.

Optional `--out /absolute/path/report.json` writes an atomic private artifact.
Output paths aliasing suite or run inputs are refused.
Shell redirection uses the shell's permissions instead. Inputs are capped at
8 MiB each, 4096 cases, 64 alternatives per case and 64 JSON nesting levels; duplicate keys, unknown typed
fields, trailing JSON, missing costs, negative/overflowing costs, incomplete
runs, duplicate/unknown cases, stale suite hashes and self-capture are errors.

## Frozen contract

A suite has `version: 1`, `id` and `cases`. Each case contains:

- `id`, `story_id`, `context`: identity, intended user story and only information
  discoverable before the decision, including scoped capabilities/prerequisites.
- `category`: `wrong_tool`, `invalid_arguments`, `unsupported_capability`,
  `wrong_directory`, `missing_prerequisite`, `stale_skill`, `denied_operation`
  or `valid_control`. Categories are evaluator labels, not candidate hints.
- `permitted`: exact proposed-call alternatives authorized by the fixture.
- `accepted`: exact valid alternatives, all drawn from `permitted`. An empty
  array means a reasoned deferral is the correct decision. A nonempty array
  means a necessary valid next call exists; doing nothing cannot earn success.

Each call has `kind` (`cli`, `mcp`, `skill`), `target`, explicit `working_dir`,
and JSON `arguments`. CLI arguments represent argv arrays; MCP arguments
represent structured input objects. Skill cases bind identity and manifest
content digests in arguments. This is a closed-world replay contract, not a
universal CLI parser, JSON Schema interpreter, live authorization service or
security scanner. Fixtures can explicitly permit harmless invalid attempts
for separate syntax/capability scoring. Unlisted proposals are conservatively
outside the frozen authorization contract, not proof of general maliciousness.

Object-key order and whitespace do not change argument meaning. Scalar types,
array order, exact paths, target names and JSON number spellings do; integer
precision is preserved. Do not claim semantic equivalence beyond these rules.
Malformed JSON envelopes are invalid evidence, never a high score.

A run binds `suite_sha256` to SHA-256 of the exact suite bytes, `bundle_id` to
its source revision/artifact identity, and `captured_by` to an independent
collector. `proposals` must cover each case exactly once, in any order. Every
proposal supplies `case_id`, `action` (`call` or `defer`) and measured `cost`
(`calls`, `tokens`, `duration_ms`). `call` needs the call object; `defer` needs
a nonempty reason and must not contain a call. Costs cover added preflight
work, including tool/schema discovery; they do not include intended execution.

Capture identity strings, context correctness and cost values are declarations,
not authenticated evidence. A production harness must independently capture
those facts, bind them to immutable source/evaluator revisions and verify
signatures through the existing trusted-evaluation infrastructure. This tool
provides no model runner, automatic promotion or cryptographic verification.

## Prevent answer leakage and metric gaming

The evaluator keeps the full suite in its own workspace. Export candidate
inputs with:

```sh
go run ./contrib/toolcall-benchmark --suite /private/evaluator/suite.json \
  --contexts --out /private/candidate/contexts.json
```

The projection contains only suite identity/hash and case id/story/context;
it excludes permitted/accepted calls and category labels. Exporting does not
protect a suite the candidate can otherwise read. Maintain genuine holdouts
outside candidate-readable repositories and run them independently. A context
must contain sufficient discoverable facts, but no failure result or oracle
answer. Review that distinction when curating real failures.

Pair baseline and candidate on identical contexts and fixed permissions.
Keep evaluator criteria unchanged throughout the optimization cycle. Include
both invalid opportunities and valid controls, plus denied-operation cases
that forbid alternate credentials or routes. Unpredictable outages belong in
a separate operational record, not the preventable-failure numerator.

Prioritize recurring, costly failures from redacted real traces; retain source
provenance in evaluator-owned records and link the regression to its Bead.
Add a rule only where the existing schema, backend preflight or host guard
cannot already prevent it. Keep generation/improvement separate from judging;
a candidate cannot approve itself. Any later RSI wiring needs independent
review and explicit approval of the new evaluation contract.

## Cyclomatic diagnostics

Measure code branching separately with the published, pinned Go analyzer:

```sh
git rev-parse HEAD
git status --short -- internal/toolcallbench contrib/toolcall-benchmark
go run github.com/fzipp/gocyclo/cmd/gocyclo@v0.6.0 \
  -ignore '_test.go' -avg internal/toolcallbench contrib/toolcall-benchmark
```

Record the analyzer version, source revision/dirty state, identical path scope
and individual function results for baseline/candidate. Complexity counts
independent control-flow paths, not call-stack depth or tool-call correctness.
Do not combine it with prevention scores, require a lower number for promotion,
or split functions merely to reduce their maxima. Architectural review still
asks whether each branch and dependency earns its maintenance cost.

## Validation

```sh
go test -race ./internal/toolcallbench ./contrib/toolcall-benchmark
go vet ./internal/toolcallbench ./contrib/toolcall-benchmark
bazel test //internal/toolcallbench:toolcallbench_test \
  //contrib/toolcall-benchmark:toolcall-benchmark_test
```

Unit tests own oracle scoring and integrity. One testscript journey owns CLI
parsing, context projection and report output. Existing storybench tests own
story-suite semantics; existing host/backend tests own actual enforcement.

Design references: [Pstrucha's repository constraints](https://finance.biggo.com/news/6e17bccdea629159)
and [The Great Loops Debate](https://ai.engineer/talks/c35YoMdnI78-great-loops-debate).
