# Private attempt evidence with stock Beads

Gas City uses Beads 1.3.1's authenticated HTTP API for private retry evidence
on CLI-backed stores. Configure one `beads.private_evidence` entry for each
canonical city or rig store that owns the workflow:

```toml
[beads.private_evidence."city:example"]
endpoint = "http://127.0.0.1:8765"
project_id = "the-project-id-reported-by-this-store"
database = "the-database-reported-by-this-store"
token_file = "/absolute/path/to/owner-only-token"
```

Graph retry and ralph controls seal evidence for each closed attempt, including
passing attempts. Configure this transport before dispatching those workflows
on a CLI-backed scope; an absent or unavailable transport refuses advancement.

The endpoint must serve that exact store. Gas City verifies its authenticated
context, project, database, backend, and required routes. The token stays in
the token file and HTTP authorization header. Payload bytes travel in HTTP
bodies, never through `bd` arguments or subprocess output.

Content rows use `gc-ep-<sha256>` IDs and Beads' transactional create-only
operation. A duplicate or ambiguous create is resolved through exact readback
and digest verification. Reads include all pages and historical duplicates;
incomplete pagination and conflicting content are errors. The HTTP request
limit is 4 MiB, including JSON and base64 encoding; larger payloads fail without
falling back to the CLI.

With lifecycle admission disabled, ordinary unenrolled routing and status
updates retain the stock CLI path. Existing lifecycle enrollment, new
enrollment writes, explicit conditional writes, and stores configured with
the HTTP transport keep revision guards. Stock HTTP updates use
`expected_version`; the stock CLI does not implement the fork's
`--if-revision` flags. Disabling admission does not convert a failed guarded
write into an unconditional retry.

Private retry evidence does not require the fork's custom source-snapshot or
revision-transition APIs. Leave `revision_transitions` disabled when using
stock Beads. Durable lifecycle admission remains a separate opt-in feature
with its own backend requirements.

The exact-binary source-snapshot and revision-transition qualification is an
opt-in test for backends that implement those custom APIs. It is excluded
from the stock integration suite. To qualify such a backend explicitly:

```sh
BEADS_TEST_BD_BINARY=/absolute/path/to/custom/bd \
BEADS_TEST_DOLT_BINARY=/absolute/path/to/dolt \
go test -tags 'integration beads_revision_transitions' ./internal/beads \
  -run '^TestBdStoreDecisionFrontierSourceSnapshotExactBinaryIntegration$' \
  -count=1 -timeout=15m
```

That qualification still requires every custom capability and verifies exact
requests, replay, and recovery after a lost response. It does not qualify a
stock installation. Stock qualification uses the ordinary backend contracts
and real workflows with authenticated HTTP payloads and guarded updates.
