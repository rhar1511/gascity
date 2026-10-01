package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/sessionauthority"
)

func withTestStdin(t *testing.T, input string, fn func()) {
	t.Helper()
	old := os.Stdin
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.WriteString(input); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	os.Stdin = r
	defer func() {
		os.Stdin = old
		_ = r.Close()
	}()
	fn()
}

func TestBdStoreBridgeBeadRedactsAttemptEvidenceMetadata(t *testing.T) {
	const privateValue = "private-attempt-evidence-payload"
	metadata := map[string]string{
		beadmeta.AttemptEvidenceArchiveAttemptIDMetadataKey:  "ae-private",
		beadmeta.AttemptEvidenceArchiveOwnerIDMetadataKey:    "gc-owner",
		beadmeta.AttemptEvidenceArchivePayloadMetadataKey:    privateValue,
		beadmeta.AttemptEvidenceArchiveDigestMetadataKey:     "private-digest",
		beadmeta.AttemptEvidenceIndexPrefix + "attempt-hash": privateValue,
		"gc.attempt": "3",
	}
	projected := bridgeBead(beads.Bead{ID: "gc-owner", Metadata: metadata})
	encoded, err := json.Marshal(projected)
	if err != nil {
		t.Fatalf("marshal bridge bead: %v", err)
	}
	if strings.Contains(string(encoded), privateValue) {
		t.Fatalf("bridge JSON exposed private attempt evidence: %s", encoded)
	}
	for key := range metadata {
		if beads.IsPrivatePresentationMetadataKey(key) {
			if _, present := projected.Metadata[key]; present {
				t.Errorf("bridge metadata retained private key %q", key)
			}
		}
	}
	if len(projected.Metadata) != 0 || projected.Title != "[private record]" {
		t.Fatalf("private archive must be an identity-only stub: %+v", projected)
	}
	if metadata[beadmeta.AttemptEvidenceArchivePayloadMetadataKey] != privateValue {
		t.Fatal("bridge projection mutated the store-owned metadata map")
	}
}

func TestBdStoreBridgeRefusesSessionAuthorityMetadata(t *testing.T) {
	t.Setenv(sessionauthority.HostTrustFileEnv, "/host/session-authority.json")
	dir := t.TempDir()
	tests := []struct {
		name  string
		op    string
		args  []string
		input string
	}{
		{
			name:  "create proof",
			op:    "create",
			input: `{"title":"forged","metadata":{"gc.authority_authorization.v1":"forged"}}`,
		},
		{
			name:  "update clears profile",
			op:    "update",
			args:  []string{"gc-session"},
			input: `{"metadata":{"gc.authority_profile":""}}`,
		},
		{
			name:  "update clears overrides",
			op:    "update",
			args:  []string{"gc-session"},
			input: `{"metadata":{"template_overrides":"{}"}}`,
		},
		{
			name: "set metadata clears history",
			op:   "set-metadata",
			args: []string{"gc-session", sessionauthority.MetadataTransitions},
		},
		{
			name:  "create request receipt",
			op:    "create",
			input: `{"title":"forged","metadata":{"gc.session_request.v1.r1":"{}"}}`,
		},
		{
			name: "set request receipt",
			op:   "set-metadata",
			args: []string{"gc-session", beadmeta.SessionRequestReceiptPrefix + "r1"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := runBdStoreBridge(tt.op, tt.args, dir, "127.0.0.1", "3307", "root", strings.NewReader(tt.input), io.Discard)
			if err == nil || !strings.Contains(err.Error(), "controller-owned session") {
				t.Fatalf("runBdStoreBridge error = %v, want controller-owned metadata refusal", err)
			}
		})
	}

	t.Setenv(sessionauthority.HostTrustFileEnv, "")
	err := validateBdStoreBridgeAuthorityMetadata(
		map[string]string{"template_overrides": "{}"},
		map[string]string{sessionauthority.MetadataTransitions: `[{"outcome":"accepted"}]`},
	)
	if err == nil || !strings.Contains(err.Error(), "controller-owned session authority metadata") {
		t.Fatalf("protected session without ambient trust error = %v, want authority metadata refusal", err)
	}
}

func writeFakeBdBridgeScript(t *testing.T, binDir, envFile, argsFile string) {
	t.Helper()
	path := filepath.Join(binDir, "bd")
	script := `#!/bin/sh
set -eu
printf 'BEADS_DIR=%s
GC_BIN=%s
GC_DOLT_HOST=%s
GC_DOLT_PORT=%s
GC_DOLT_USER=%s
GC_DOLT_PASSWORD=%s
BEADS_DOLT_SERVER_HOST=%s
BEADS_DOLT_SERVER_PORT=%s
BEADS_DOLT_SERVER_USER=%s
BEADS_DOLT_PASSWORD=%s
BEADS_DOLT_SERVER_DATABASE=%s
BEADS_CREDENTIALS_FILE=%s
GC_BEADS=%s
GC_BEADS_BACKEND=%s
BEADS_BACKEND=%s
GC_BEADS_PREFIX=%s
BD_EXPORT_AUTO=%s
' \
  "${BEADS_DIR:-}" "${GC_BIN:-}" "${GC_DOLT_HOST:-}" "${GC_DOLT_PORT:-}" "${GC_DOLT_USER:-}" "${GC_DOLT_PASSWORD:-}" \
  "${BEADS_DOLT_SERVER_HOST:-}" "${BEADS_DOLT_SERVER_PORT:-}" "${BEADS_DOLT_SERVER_USER:-}" "${BEADS_DOLT_PASSWORD:-}" \
  "${BEADS_DOLT_SERVER_DATABASE:-}" "${BEADS_CREDENTIALS_FILE:-}" "${GC_BEADS:-}" "${GC_BEADS_BACKEND:-}" "${BEADS_BACKEND:-}" "${GC_BEADS_PREFIX:-}" \
  "${BD_EXPORT_AUTO:-}" > "` + envFile + `"
printf '%s
' "$*" > "` + argsFile + `"
if [ "${1:-}" = "--dolt-auto-commit" ]; then
  shift 2
fi
case "${1:-}" in
  create)
    cat <<'JSON'
{"id":"BD-1","title":"captured","status":"open","issue_type":"task","created_at":"2026-02-27T10:00:00Z"}
JSON
    ;;
  show)
    cat <<'JSON'
[{"id":"BD-1","title":"captured","status":"open","issue_type":"task","created_at":"2026-02-27T10:00:00Z"}]
JSON
    ;;
  list)
    cat <<'JSON'
[{"id":"BD-1","title":"captured","status":"open","issue_type":"message","assignee":"mayor","created_at":"2026-02-27T10:00:00Z"}]
JSON
    ;;
  update)
    exit 0
    ;;
  dep)
    if [ "${2:-}" = "list" ]; then
      cat <<'JSON'
[{"id":"BD-2","dependency_type":"blocks"}]
JSON
      exit 0
    fi
    exit 2
    ;;
  *)
    exit 2
    ;;
esac
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestBdStoreBridgeCreateCmdProjectsCanonicalEnvAndClearsAmbientAuthority(t *testing.T) {
	scopeDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(scopeDir, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	binDir := t.TempDir()
	envFile := filepath.Join(t.TempDir(), "bridge.env")
	argsFile := filepath.Join(t.TempDir(), "bridge.args")
	writeFakeBdBridgeScript(t, binDir, envFile, argsFile)
	gcBin := filepath.Join(t.TempDir(), "gc")
	if err := os.WriteFile(gcBin, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	oldResolve := resolveInvokingExecutable
	resolveInvokingExecutable = func() (string, error) { return gcBin, nil }
	t.Cleanup(func() { resolveInvokingExecutable = oldResolve })
	wantGCBin, err := filepath.EvalSymlinks(gcBin)
	if err != nil {
		t.Fatal(err)
	}

	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GC_BIN", "/tmp/stale-gc")
	t.Setenv("BEADS_DOLT_SERVER_DATABASE", "wrong-db")
	t.Setenv("BEADS_CREDENTIALS_FILE", "/tmp/stale-creds")
	t.Setenv("GC_BEADS", "ambient-bd")
	t.Setenv("GC_BEADS_PREFIX", "ambient-prefix")
	t.Setenv("GC_DOLT_PASSWORD", "secret")
	var stdout, stderr bytes.Buffer

	withTestStdin(t, `{"title":"captured","type":"task","labels":["triage"]}`+"\n", func() {
		code := run([]string{
			"bd-store-bridge",
			"--dir", scopeDir,
			"--host", "db.example.internal",
			"--port", "3317",
			"--user", "root",
			"create",
		}, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("run() = %d, stderr = %s", code, stderr.String())
		}
	})

	var bead map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &bead); err != nil {
		t.Fatalf("stdout JSON: %v\n%s", err, stdout.String())
	}
	if bead["id"] != "BD-1" || bead["type"] != "task" {
		t.Fatalf("unexpected bead payload: %#v", bead)
	}

	envText, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatalf("ReadFile(env): %v", err)
	}
	envMap := readExecCaptureEnv(t, envFile)
	if got := envMap["BEADS_DIR"]; got != filepath.Join(scopeDir, ".beads") {
		t.Fatalf("BEADS_DIR = %q, want %q", got, filepath.Join(scopeDir, ".beads"))
	}
	if got := envMap["GC_BIN"]; got != wantGCBin {
		t.Fatalf("GC_BIN = %q, want canonical %q (ambient value must not leak)", got, wantGCBin)
	}
	if got := envMap["GC_DOLT_HOST"]; got != "db.example.internal" {
		t.Fatalf("GC_DOLT_HOST = %q, want db.example.internal", got)
	}
	if got := envMap["GC_DOLT_PORT"]; got != "3317" {
		t.Fatalf("GC_DOLT_PORT = %q, want 3317", got)
	}
	if got := envMap["BEADS_DOLT_SERVER_DATABASE"]; got != "" {
		t.Fatalf("BEADS_DOLT_SERVER_DATABASE = %q, want empty after sanitization\n%s", got, string(envText))
	}
	if got := envMap["BEADS_CREDENTIALS_FILE"]; got != "" {
		t.Fatalf("BEADS_CREDENTIALS_FILE = %q, want empty after sanitization\n%s", got, string(envText))
	}
	if got := envMap["GC_BEADS"]; got != "" {
		t.Fatalf("GC_BEADS = %q, want empty after sanitization\n%s", got, string(envText))
	}
	if got := envMap["GC_BEADS_PREFIX"]; got != "" {
		t.Fatalf("GC_BEADS_PREFIX = %q, want empty after sanitization\n%s", got, string(envText))
	}
	if got := envMap["BD_EXPORT_AUTO"]; got != "false" {
		t.Fatalf("BD_EXPORT_AUTO = %q, want false to suppress bridge auto-export\n%s", got, string(envText))
	}

	argsText, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("ReadFile(args): %v", err)
	}
	for _, want := range []string{"create", "--json", "captured", "-t", "task", "--labels", "triage"} {
		if !strings.Contains(string(argsText), want) {
			t.Fatalf("bd args missing %q: %s", want, string(argsText))
		}
	}
}

func TestBdStoreBridgeGetCmdReturnsBead(t *testing.T) {
	scopeDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(scopeDir, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	binDir := t.TempDir()
	envFile := filepath.Join(t.TempDir(), "bridge.env")
	argsFile := filepath.Join(t.TempDir(), "bridge.args")
	writeFakeBdBridgeScript(t, binDir, envFile, argsFile)

	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	var stdout, stderr bytes.Buffer
	code := run([]string{
		"bd-store-bridge",
		"--dir", scopeDir,
		"--host", "db.example.internal",
		"--port", "3317",
		"get",
		"BD-1",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run() = %d, stderr = %s", code, stderr.String())
	}

	var bead bdStoreBridgeBead
	if err := json.Unmarshal(stdout.Bytes(), &bead); err != nil {
		t.Fatalf("stdout JSON: %v\n%s", err, stdout.String())
	}
	if bead.ID != "BD-1" || bead.Title != "captured" || bead.Type != "task" {
		t.Fatalf("unexpected bead payload: %#v", bead)
	}

	argsText, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("ReadFile(args): %v", err)
	}
	if got := strings.TrimSpace(string(argsText)); got != "show --json BD-1" {
		t.Fatalf("get args = %q, want %q", got, "show --json BD-1")
	}
}

func TestBdStoreBridgeDoltliteClearsDoltServerEnv(t *testing.T) {
	scopeDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(scopeDir, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scopeDir, ".beads", "metadata.json"), []byte(`{"backend":"doltlite","database":"doltlite"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	binDir := t.TempDir()
	envFile := filepath.Join(t.TempDir(), "bridge.env")
	argsFile := filepath.Join(t.TempDir(), "bridge.args")
	writeFakeBdBridgeScript(t, binDir, envFile, argsFile)

	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GC_BEADS_BACKEND", "doltlite")
	var stdout, stderr bytes.Buffer
	withTestStdin(t, `{"title":"captured","type":"task"}`+"\n", func() {
		code := run([]string{
			"bd-store-bridge",
			"--dir", scopeDir,
			"--host", "db.example.internal",
			"--port", "3317",
			"--user", "root",
			"create",
		}, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("run() = %d, stderr = %s", code, stderr.String())
		}
	})

	envMap := readExecCaptureEnv(t, envFile)
	if got := envMap["GC_BEADS_BACKEND"]; got != "doltlite" {
		t.Fatalf("GC_BEADS_BACKEND = %q, want doltlite", got)
	}
	if got := envMap["BEADS_BACKEND"]; got != "doltlite" {
		t.Fatalf("BEADS_BACKEND = %q, want doltlite", got)
	}
	for _, key := range []string{"GC_DOLT_HOST", "GC_DOLT_PORT", "BEADS_DOLT_SERVER_HOST", "BEADS_DOLT_SERVER_PORT", "BEADS_DOLT_AUTO_START"} {
		if got := envMap[key]; got != "" {
			t.Fatalf("%s = %q, want empty for doltlite bridge", key, got)
		}
	}
}

func TestBdStoreBridgeDepListCmdReturnsJSON(t *testing.T) {
	scopeDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(scopeDir, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	binDir := t.TempDir()
	envFile := filepath.Join(t.TempDir(), "bridge.env")
	argsFile := filepath.Join(t.TempDir(), "bridge.args")
	writeFakeBdBridgeScript(t, binDir, envFile, argsFile)

	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	var stdout, stderr bytes.Buffer
	code := run([]string{
		"bd-store-bridge",
		"--dir", scopeDir,
		"--host", "db.example.internal",
		"--port", "3317",
		"dep-list",
		"BD-1",
		"up",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run() = %d, stderr = %s", code, stderr.String())
	}
	if got := strings.TrimSpace(stdout.String()); got != `[{"issue_id":"BD-2","depends_on_id":"BD-1","type":"blocks"}]` {
		t.Fatalf("stdout = %q", got)
	}
	argsText, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("ReadFile(args): %v", err)
	}
	if !strings.Contains(string(argsText), "dep list BD-1 --json --direction=up") {
		t.Fatalf("dep-list args = %q", string(argsText))
	}
}

func TestBdStoreBridgeUpdateCommandPassesType(t *testing.T) {
	scopeDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(scopeDir, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	binDir := t.TempDir()
	envFile := filepath.Join(t.TempDir(), "bridge.env")
	argsFile := filepath.Join(t.TempDir(), "bridge.args")
	writeFakeBdBridgeScript(t, binDir, envFile, argsFile)

	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	var stdout, stderr bytes.Buffer
	withTestStdin(t, `{"type":"bug"}`+"\n", func() {
		code := run([]string{
			"bd-store-bridge",
			"--dir", scopeDir,
			"--host", "db.example.internal",
			"--port", "3317",
			"update",
			"BD-1",
		}, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("run() = %d, stderr = %s", code, stderr.String())
		}
	})
	argsText, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("ReadFile(args): %v", err)
	}
	for _, want := range []string{"update", "--json", "BD-1", "--type", "bug"} {
		if !strings.Contains(string(argsText), want) {
			t.Fatalf("update args missing %q: %s", want, string(argsText))
		}
	}
}

func TestBdStoreBridgeRefusesGenericMutationsOfEnrolledWork(t *testing.T) {
	for _, tc := range []struct {
		name  string
		op    string
		args  []string
		stdin string
	}{
		{name: "update", op: "update", args: []string{"BD-1"}, stdin: `{"title":"Changed"}`},
		{name: "close", op: "close", args: []string{"BD-1"}},
		{name: "set-metadata", op: "set-metadata", args: []string{"BD-1", "gc.session_id"}, stdin: "replacement"},
		{name: "delete", op: "delete", args: []string{"BD-1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scopeDir := t.TempDir()
			if err := os.MkdirAll(filepath.Join(scopeDir, ".beads"), 0o755); err != nil {
				t.Fatal(err)
			}
			binDir := t.TempDir()
			argsFile := filepath.Join(t.TempDir(), "bridge.args")
			script := `#!/bin/sh
set -eu
printf '%s\n' "$*" >> '` + argsFile + `'
case "$*" in
  *"show --json BD-1"*)
    cat <<'JSON'
[{"id":"BD-1","title":"Protected","status":"open","issue_type":"task","metadata":{"gc.lifecycle.admission_receipt.v1":"persisted"}}]
JSON
    ;;
esac
exit 0
`
			if err := os.WriteFile(filepath.Join(binDir, "bd"), []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
			var stdout bytes.Buffer
			err := runBdStoreBridge(tc.op, tc.args, scopeDir, "db.example.internal", "3317", "root", strings.NewReader(tc.stdin), &stdout)
			if err == nil || !strings.Contains(err.Error(), "generic mutation lacks current session, claim, and row-revision proof") {
				t.Fatalf("runBdStoreBridge() error = %v, want fail-closed enrolled-work error", err)
			}
			argsText, readErr := os.ReadFile(argsFile)
			if readErr != nil {
				t.Fatalf("ReadFile(args): %v", readErr)
			}
			if got := strings.TrimSpace(string(argsText)); got != "show --json BD-1" {
				t.Fatalf("bd calls = %q, want only the preflight read", got)
			}
		})
	}
}

func TestBdStoreBridgeListCommandForwardsFilters(t *testing.T) {
	scopeDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(scopeDir, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	binDir := t.TempDir()
	envFile := filepath.Join(t.TempDir(), "bridge.env")
	argsFile := filepath.Join(t.TempDir(), "bridge.args")
	writeFakeBdBridgeScript(t, binDir, envFile, argsFile)

	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	var stdout, stderr bytes.Buffer
	code := run([]string{
		"bd-store-bridge",
		"--dir", scopeDir,
		"--host", "db.example.internal",
		"--port", "3317",
		"list",
		"--status=open",
		"--assignee=mayor",
		"--type=message",
		"--limit=7",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run() = %d, stderr = %s", code, stderr.String())
	}
	if got := strings.TrimSpace(stdout.String()); !strings.Contains(got, `"id":"BD-1"`) {
		t.Fatalf("stdout = %q", got)
	}
	argsText, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("ReadFile(args): %v", err)
	}
	// TierIssues + a single assignee has no Go-side-only residual filter, so
	// BdStore pushes the caller's real limit straight to bd list (gc-i4j3y).
	for _, want := range []string{"list", "--json", "--status=open", "--assignee=mayor", "--type=message", "--include-infra", "--include-gates", "--limit", "7"} {
		if !strings.Contains(string(argsText), want) {
			t.Fatalf("list args missing %q: %s", want, string(argsText))
		}
	}
}
