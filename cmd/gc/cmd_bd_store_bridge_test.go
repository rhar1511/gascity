package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
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

func TestBdStoreBridgeGenericReadProjectionRedactsExecutionCredentials(t *testing.T) {
	source := beads.Bead{ID: "BD-session", Metadata: map[string]string{
		beadmeta.SessionInstanceTokenMetadataKey: "bridge-secret",
		"generation":                             "7",
	}}
	for name, got := range map[string][]bdStoreBridgeBead{
		"get":           {bridgeBead(source)},
		"list_variants": bridgeBeads([]beads.Bead{source}),
	} {
		if len(got) != 1 {
			t.Fatalf("%s projection length = %d", name, len(got))
		}
		if _, ok := got[0].Metadata[beadmeta.SessionInstanceTokenMetadataKey]; ok {
			t.Fatalf("%s projection leaked execution credential: %#v", name, got[0].Metadata)
		}
		if got[0].Metadata["generation"] != "7" {
			t.Fatalf("%s projection lost safe metadata: %#v", name, got[0].Metadata)
		}
	}
	if source.Metadata[beadmeta.SessionInstanceTokenMetadataKey] != "bridge-secret" {
		t.Fatalf("bridge projection mutated source metadata: %#v", source.Metadata)
	}
	private := bridgeReadBead(source, true)
	if private.Metadata[beadmeta.SessionInstanceTokenMetadataKey] != "bridge-secret" {
		t.Fatalf("private exec-store projection lost lifecycle credential: %#v", private.Metadata)
	}
}

func TestBdStoreBridgePrivateReadRequiresExplicitInternalMode(t *testing.T) {
	t.Setenv("GC_BD_STORE_BRIDGE_PRIVATE", "")
	err := runBdStoreBridge("private-get", []string{"BD-session"}, t.TempDir(), "127.0.0.1", "3306", "root", strings.NewReader(""), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "internal exec-store mode") {
		t.Fatalf("private read without marker = %v, want refusal", err)
	}
}

func TestBdStoreBridgeDeleteRequiresMatchingRevision(t *testing.T) {
	store := &beads.MemStore{HonorExplicitIDs: true}
	created, err := store.Create(beads.Bead{ID: "BD-1", Title: "closed", Status: "closed"})
	if err != nil {
		t.Fatal(err)
	}
	description := "concurrent"
	if err := store.Update(created.ID, beads.UpdateOpts{Description: &description}); err != nil {
		t.Fatal(err)
	}
	if err := deleteBdStoreBridge(store, []string{created.ID, strconv.FormatInt(created.Revision, 10)}); err == nil {
		t.Fatal("stale bridge delete succeeded")
	} else {
		var stale *beads.PreconditionFailedError
		if !errors.As(err, &stale) {
			t.Fatalf("stale bridge delete = %v, want precondition failure", err)
		}
	}
	if _, err := store.Get(created.ID); err != nil {
		t.Fatalf("stale bridge delete removed row: %v", err)
	}
	if err := deleteBdStoreBridge(store, []string{created.ID}); err == nil || !strings.Contains(err.Error(), "usage: delete") {
		t.Fatalf("revision-less bridge delete = %v, want usage refusal", err)
	}
}

func TestBdStoreBridgeDeleteRetainsRequestEvidence(t *testing.T) {
	store := &beads.MemStore{HonorExplicitIDs: true}
	created, err := store.Create(beads.Bead{ID: "BD-session", Title: "closed", Type: session.BeadType, Status: "closed", Metadata: map[string]string{
		beadmeta.SessionRequestReceiptPrefix + "evidence": "{}",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := deleteBdStoreBridge(store, []string{created.ID, strconv.FormatInt(created.Revision, 10)}); !errors.Is(err, session.ErrRequestEvidenceRetained) {
		t.Fatalf("deleteBdStoreBridge = %v, want retained-evidence refusal", err)
	}
	if _, err := store.Get(created.ID); err != nil {
		t.Fatalf("evidence-bearing row was deleted: %v", err)
	}
}

func TestBdStoreBridgeRejectsProtocolOwnedMetadata(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range []struct {
		name  string
		op    string
		args  []string
		stdin string
	}{
		{"create receipt", "create", nil, `{"title":"forged","metadata":{"` + beadmeta.SessionRequestReceiptPrefix + `forged":"{}"}}`},
		{"update fence", "update", []string{"BD-1"}, `{"metadata":{"` + beadmeta.SessionRequestPurgeFenceMetadataKey + `":"forged"}}`},
		{"set receipt", "set-metadata", []string{"BD-1", beadmeta.SessionRequestReceiptPrefix + "forged"}, `{}`},
		{"set fence", "set-metadata", []string{"BD-1", beadmeta.SessionRequestPurgeFenceMetadataKey}, `forged`},
		{"set credential", "set-metadata", []string{"BD-1", beadmeta.SessionInstanceTokenMetadataKey}, `forged`},
		{"set session name", "set-metadata", []string{"BD-1", "session_name"}, `retargeted`},
		{"set generation", "set-metadata", []string{"BD-1", "generation"}, `2`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := runBdStoreBridge(tc.op, tc.args, dir, "127.0.0.1", "1", "root", strings.NewReader(tc.stdin), io.Discard)
			if !errors.Is(err, session.ErrRequestConflict) {
				t.Fatalf("runBdStoreBridge = %v, want protected-metadata conflict", err)
			}
		})
	}
}

func TestBdStoreBridgeRejectsTypeChangeWithRequestEvidence(t *testing.T) {
	store := &beads.MemStore{HonorExplicitIDs: true}
	b, err := store.Create(beads.Bead{ID: "BD-history", Title: "history", Type: "task", Metadata: map[string]string{
		beadmeta.SessionRequestReceiptPrefix + "history": `{}`,
	}})
	if err != nil {
		t.Fatal(err)
	}
	nextType := "bug"
	if err := updateBdStoreBridge(store, b.ID, beads.UpdateOpts{Type: &nextType}); !errors.Is(err, session.ErrRequestConflict) {
		t.Fatalf("type change = %v, want request conflict", err)
	}
	got, err := store.Get(b.ID)
	if err != nil || got.Type != "task" || !session.HasRequestEvidence(got) {
		t.Fatalf("historical evidence row changed: %+v, %v", got, err)
	}
}

func TestBdStoreBridgeRejectsSessionLabelRemovalWithRequestEvidence(t *testing.T) {
	store := &beads.MemStore{HonorExplicitIDs: true}
	b, err := store.Create(beads.Bead{
		ID: "BD-history-label", Title: "history", Type: session.BeadType, Labels: []string{session.LabelSession, "worker"},
		Metadata: map[string]string{beadmeta.SessionRequestReceiptPrefix + "history": `{}`},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := updateBdStoreBridge(store, b.ID, beads.UpdateOpts{RemoveLabels: []string{session.LabelSession}}); !errors.Is(err, session.ErrRequestConflict) {
		t.Fatalf("session label removal = %v, want request conflict", err)
	}
	got, err := store.Get(b.ID)
	if err != nil || !slices.Contains(got.Labels, session.LabelSession) || !session.HasRequestEvidence(got) {
		t.Fatalf("historical session identity changed: %+v, %v", got, err)
	}
}

func TestBdStoreBridgeLabelRemovalLosesRaceToRequestEvidence(t *testing.T) {
	base := &beads.MemStore{HonorExplicitIDs: true}
	store := &fenceBeforeCLIUpdateStore{MemStore: base}
	b, err := store.Create(beads.Bead{
		ID: "BD-label-race", Title: "session", Type: session.BeadType, Labels: []string{session.LabelSession},
	})
	if err != nil {
		t.Fatal(err)
	}
	store.beforeUpdate = func() {
		if err := base.SetMetadata(b.ID, beadmeta.SessionRequestReceiptPrefix+"racing", `{}`); err != nil {
			t.Fatalf("install racing receipt = %v", err)
		}
	}
	if err := updateBdStoreBridge(store, b.ID, beads.UpdateOpts{RemoveLabels: []string{session.LabelSession}}); !beads.IsPreconditionFailed(err) {
		t.Fatalf("label removal across racing receipt = %v, want revision conflict", err)
	}
	got, err := base.Get(b.ID)
	if err != nil || !slices.Contains(got.Labels, session.LabelSession) || !session.HasRequestEvidence(got) {
		t.Fatalf("racing receipt lost session identity: %+v, %v", got, err)
	}
}

func TestBdStoreBridgeParentUpdatesLoseRevisionRace(t *testing.T) {
	for _, tc := range []struct {
		name     string
		combined bool
	}{
		{name: "parent_only"},
		{name: "combined", combined: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := &beads.MemStore{HonorExplicitIDs: true}
			store := &fenceBeforeCLIUpdateStore{MemStore: base}
			b, err := store.Create(beads.Bead{ID: "BD-parent-race", Title: "before", Type: "task"})
			if err != nil {
				t.Fatal(err)
			}
			store.beforeUpdate = func() {
				if err := base.SetMetadata(b.ID, "racing", "winner"); err != nil {
					t.Fatalf("install racing update: %v", err)
				}
			}
			parent := "BD-new-parent"
			opts := beads.UpdateOpts{ParentID: &parent}
			if tc.combined {
				title := "after"
				opts.Title = &title
			}
			if err := updateBdStoreBridge(store, b.ID, opts); !beads.IsPreconditionFailed(err) {
				t.Fatalf("racing parent update = %v, want precondition failure", err)
			}
			got, err := base.Get(b.ID)
			if err != nil || got.ParentID != "" || got.Title != "before" || got.Metadata["racing"] != "winner" {
				t.Fatalf("racing parent update partially committed: %+v, %v", got, err)
			}
		})
	}
}

func TestBdStoreBridgeFailsClosedWhenPurgeFenceRacesUpdate(t *testing.T) {
	base := &beads.MemStore{HonorExplicitIDs: true}
	store := &fenceBeforeCLIUpdateStore{MemStore: base}
	b, err := store.Create(beads.Bead{ID: "BD-race", Title: "before", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	store.beforeUpdate = func() {
		if ok, err := base.CompareAndSetMetadataKey(b.ID, beadmeta.SessionRequestPurgeFenceMetadataKey, "", "purge-owner"); err != nil || !ok {
			t.Fatalf("install racing fence = (%v, %v)", ok, err)
		}
	}
	after := "after"
	if err := updateBdStoreBridge(store, b.ID, beads.UpdateOpts{Title: &after}); err == nil {
		t.Fatal("bridge update crossed racing purge fence")
	}
	got, err := base.Get(b.ID)
	if err != nil || got.Title != "before" || !session.IsRequestPurgeFenced(got) {
		t.Fatalf("racing bridge update result: %+v, %v", got, err)
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
if [ "${2:-}" = "--help" ]; then
  printf '%s\n' '      --if-revision int'
  exit 0
fi
case "${1:-}" in
  create)
    cat <<'JSON'
{"id":"BD-1","title":"captured","status":"open","issue_type":"task","created_at":"2026-02-27T10:00:00Z"}
JSON
    ;;
  show)
    cat <<'JSON'
    [{"id":"BD-1","title":"captured","status":"open","issue_type":"task","revision":1,"created_at":"2026-02-27T10:00:00Z"}]
JSON
    ;;
  list)
    cat <<'JSON'
[{"id":"BD-1","title":"captured","status":"open","issue_type":"message","assignee":"mayor","created_at":"2026-02-27T10:00:00Z"}]
JSON
    ;;
  update)
    cat <<'JSON'
{"id":"BD-1","title":"captured","status":"open","issue_type":"bug","revision":2,"created_at":"2026-02-27T10:00:00Z"}
JSON
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
