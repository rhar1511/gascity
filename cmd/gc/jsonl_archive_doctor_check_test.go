package main

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func jsonUnmarshalString(s string, v interface{}) error {
	return json.Unmarshal([]byte(s), v)
}

type stubArchiveEnv struct {
	vars map[string]string
}

func (s stubArchiveEnv) get(key string) string { return s.vars[key] }

func initBareArchiveRepo(t *testing.T, dir string, withOrigin bool) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if out, err := exec.Command("git", "-C", dir, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	if withOrigin {
		if out, err := exec.Command("git", "-C", dir, "remote", "add", "origin", "https://example.invalid/archive.git").CombinedOutput(); err != nil {
			t.Fatalf("git remote add: %v\n%s", err, out)
		}
	}
}

func writeArchiveState(t *testing.T, stateDir, body string) {
	t.Helper()
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	path := filepath.Join(stateDir, "jsonl-export-state.json")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}

func runJsonlArchiveCheck(t *testing.T, cityPath string, env map[string]string) *doctor.CheckResult {
	t.Helper()
	check := newJsonlArchiveDoctorCheck(cityPath)
	check.getenv = stubArchiveEnv{vars: env}.get
	return check.Run(&doctor.CheckContext{CityPath: cityPath})
}

func TestJsonlArchiveDoctorCheck_NoStateNoArchive(t *testing.T) {
	cityDir := t.TempDir()
	result := runJsonlArchiveCheck(t, cityDir, map[string]string{})
	if result.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want OK; result=%#v", result.Status, result)
	}
	if !strings.Contains(result.Message, "no archive activity yet") {
		t.Fatalf("message = %q", result.Message)
	}
}

func TestJsonlArchiveDoctorCheck_MalformedArchiveRepo(t *testing.T) {
	cityDir := t.TempDir()
	archiveDir := filepath.Join(cityDir, "archive")
	if err := os.MkdirAll(archiveDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	// No .git subdir → malformed per the check.
	env := map[string]string{"GC_JSONL_ARCHIVE_REPO": archiveDir}
	result := runJsonlArchiveCheck(t, cityDir, env)
	if result.Status != doctor.StatusError {
		t.Fatalf("status = %v, want Error; result=%#v", result.Status, result)
	}
	if !strings.Contains(result.Message, "malformed (no .git)") {
		t.Fatalf("message = %q", result.Message)
	}
	if result.FixHint == "" {
		t.Fatalf("expected a fix hint on malformed archive")
	}

	// An existing but incomplete .git reaches the real Git child rather than
	// the missing-.git guard. Preserve the blocking result and capture only
	// safe error metadata. This fixture is not proof of the hosted cause.
	if err := os.Mkdir(filepath.Join(archiveDir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	result = runJsonlArchiveCheck(t, cityDir, env)
	if result.Status != doctor.StatusError {
		t.Fatalf("incomplete repo status = %v, want Error", result.Status)
	}
	for _, want := range []string{"querying archive remotes: exit status 128;", "captured_stderr_bytes=", "captured_stderr_sha256=", "post_error_snapshot={git=directory HEAD=missing config=missing objects=missing refs=missing}"} {
		if !strings.Contains(result.Message, want) {
			t.Errorf("message = %q, missing %q", result.Message, want)
		}
	}
	if strings.Contains(result.Message, archiveDir) || strings.Contains(result.Message, "fatal:") {
		t.Fatalf("message exposes raw Git diagnostic: %q", result.Message)
	}
	if !strings.Contains(result.FixHint, "remote -v") {
		t.Fatalf("original remote-query hint was lost: %q", result.FixHint)
	}
}

func TestJsonlArchiveDoctorCheck_LocalOnlyWarning(t *testing.T) {
	cityDir := t.TempDir()
	stateDir := t.TempDir()
	archiveDir := filepath.Join(cityDir, "archive")
	initBareArchiveRepo(t, archiveDir, false)

	env := map[string]string{
		"GC_PACK_STATE_DIR":     stateDir,
		"GC_JSONL_ARCHIVE_REPO": archiveDir,
	}
	result := runJsonlArchiveCheck(t, cityDir, env)
	if result.Status != doctor.StatusWarning {
		t.Fatalf("status = %v, want Warning; result=%#v", result.Status, result)
	}
	if !strings.Contains(result.Message, "local-only mode") {
		t.Fatalf("message = %q", result.Message)
	}
	if !strings.Contains(result.FixHint, "remote add origin") {
		t.Fatalf("fix hint = %q", result.FixHint)
	}
}

// TestJsonlArchiveDoctorCheck_ArchiveHasOriginIgnoresPoisonedGitEnv proves the
// archive remote query resolves from the archive repo passed via -C even when
// git-locating environment variables point at an unrelated repository. Running
// gc doctor inside a pre-commit hook or nested worktree exports
// GIT_DIR/GIT_WORK_TREE for the parent repo; without git.SanitizedEnv() the
// leaked GIT_DIR redirects `git -C <archive> remote -v` to the poisoned repo, so
// the check would report push-mode health for the wrong repository.
func TestJsonlArchiveDoctorCheck_ArchiveHasOriginIgnoresPoisonedGitEnv(t *testing.T) {
	cityDir := t.TempDir()
	archiveDir := filepath.Join(cityDir, "archive")
	// Archive repo has an origin remote → push mode.
	initBareArchiveRepo(t, archiveDir, true)

	// Unrelated repo with no origin whose git-locating env vars would make
	// archiveHasOrigin read its (empty) remote list if inherited.
	poison := t.TempDir()
	initBareArchiveRepo(t, poison, false)
	t.Setenv("GIT_DIR", filepath.Join(poison, ".git"))
	t.Setenv("GIT_WORK_TREE", poison)
	t.Setenv("GIT_INDEX_FILE", filepath.Join(poison, ".git", "index"))

	// Real git path (no runGit stub) exercises cmd.Env = git.SanitizedEnv().
	check := newJsonlArchiveDoctorCheck(cityDir)
	hasOrigin, err := check.archiveHasOrigin(archiveDir)
	if err != nil {
		t.Fatalf("archiveHasOrigin with poisoned git env: %v", err)
	}
	if !hasOrigin {
		t.Fatalf("archiveHasOrigin = false, want true (must query archive repo via -C, not poisoned GIT_DIR)")
	}
}

func TestJsonlArchiveDoctorCheck_PushModeHealthyWithTimestamp(t *testing.T) {
	cityDir := t.TempDir()
	stateDir := t.TempDir()
	archiveDir := filepath.Join(cityDir, "archive")
	initBareArchiveRepo(t, archiveDir, true)
	writeArchiveState(t, stateDir, `{"consecutive_push_failures":0,"last_push_at":"2026-05-11T12:00:00Z"}`)

	env := map[string]string{
		"GC_PACK_STATE_DIR":     stateDir,
		"GC_JSONL_ARCHIVE_REPO": archiveDir,
	}
	result := runJsonlArchiveCheck(t, cityDir, env)
	if result.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want OK; result=%#v", result.Status, result)
	}
	if !strings.Contains(result.Message, "last successful push 2026-05-11T12:00:00Z") {
		t.Fatalf("message = %q", result.Message)
	}
}

func TestJsonlArchiveDoctorCheck_PushModeHealthyNoTimestamp(t *testing.T) {
	cityDir := t.TempDir()
	stateDir := t.TempDir()
	archiveDir := filepath.Join(cityDir, "archive")
	initBareArchiveRepo(t, archiveDir, true)
	writeArchiveState(t, stateDir, `{}`)

	env := map[string]string{
		"GC_PACK_STATE_DIR":     stateDir,
		"GC_JSONL_ARCHIVE_REPO": archiveDir,
	}
	result := runJsonlArchiveCheck(t, cityDir, env)
	if result.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want OK; result=%#v", result.Status, result)
	}
	if !strings.Contains(result.Message, "no pushes attempted yet") {
		t.Fatalf("message = %q", result.Message)
	}
}

func TestJsonlArchiveDoctorCheck_PushModeFailuresSurfaceStderr(t *testing.T) {
	cityDir := t.TempDir()
	stateDir := t.TempDir()
	archiveDir := filepath.Join(cityDir, "archive")
	initBareArchiveRepo(t, archiveDir, true)
	writeArchiveState(t, stateDir, `{"consecutive_push_failures":3,"last_push_stderr":"fatal: remote rejected"}`)

	env := map[string]string{
		"GC_PACK_STATE_DIR":     stateDir,
		"GC_JSONL_ARCHIVE_REPO": archiveDir,
	}
	result := runJsonlArchiveCheck(t, cityDir, env)
	if result.Status != doctor.StatusError {
		t.Fatalf("status = %v, want Error; result=%#v", result.Status, result)
	}
	if !strings.Contains(result.Message, "3 consecutive push failure") {
		t.Fatalf("message = %q (missing failure count)", result.Message)
	}
	if !strings.Contains(result.Message, "fatal: remote rejected") {
		t.Fatalf("message = %q (missing stderr)", result.Message)
	}
	if !strings.Contains(result.FixHint, "verify credentials") {
		t.Fatalf("fix hint = %q", result.FixHint)
	}
}

func TestJsonlArchiveDoctorCheck_PushModeFailuresWithoutStderr(t *testing.T) {
	cityDir := t.TempDir()
	stateDir := t.TempDir()
	archiveDir := filepath.Join(cityDir, "archive")
	initBareArchiveRepo(t, archiveDir, true)
	writeArchiveState(t, stateDir, `{"consecutive_push_failures":2}`)

	env := map[string]string{
		"GC_PACK_STATE_DIR":     stateDir,
		"GC_JSONL_ARCHIVE_REPO": archiveDir,
	}
	result := runJsonlArchiveCheck(t, cityDir, env)
	if result.Status != doctor.StatusError {
		t.Fatalf("status = %v, want Error; result=%#v", result.Status, result)
	}
	if !strings.Contains(result.Message, "2 consecutive push failure") {
		t.Fatalf("message = %q", result.Message)
	}
	if strings.Contains(result.Message, "Last stderr") {
		t.Fatalf("message %q should not include stderr when stderr is empty", result.Message)
	}
}

func TestJsonlArchiveDoctorCheck_MalformedStateTreatedAsEmpty(t *testing.T) {
	cityDir := t.TempDir()
	stateDir := t.TempDir()
	archiveDir := filepath.Join(cityDir, "archive")
	initBareArchiveRepo(t, archiveDir, true)
	writeArchiveState(t, stateDir, `not-json`)

	env := map[string]string{
		"GC_PACK_STATE_DIR":     stateDir,
		"GC_JSONL_ARCHIVE_REPO": archiveDir,
	}
	result := runJsonlArchiveCheck(t, cityDir, env)
	if result.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want OK (malformed state must not escalate); result=%#v", result.Status, result)
	}
	if !strings.Contains(result.Message, "no pushes attempted yet") {
		t.Fatalf("message = %q", result.Message)
	}
}

func TestJsonlArchiveDoctorCheck_StateInLegacyLocation(t *testing.T) {
	cityDir := t.TempDir()
	archiveDir := filepath.Join(cityDir, "archive")
	initBareArchiveRepo(t, archiveDir, true)

	// Legacy state path: <cityPath>/.gc/jsonl-export-state.json
	legacyDir := filepath.Join(cityDir, ".gc")
	if err := os.MkdirAll(legacyDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	legacyPath := filepath.Join(legacyDir, "jsonl-export-state.json")
	if err := os.WriteFile(legacyPath, []byte(`{"last_push_at":"2026-05-11T00:00:00Z"}`), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	env := map[string]string{
		"GC_JSONL_ARCHIVE_REPO": archiveDir,
	}
	result := runJsonlArchiveCheck(t, cityDir, env)
	if result.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want OK; result=%#v", result.Status, result)
	}
	if !strings.Contains(result.Message, "2026-05-11T00:00:00Z") {
		t.Fatalf("message = %q (legacy state path ignored?)", result.Message)
	}
}

func TestJsonlArchiveDoctorCheck_StateInCoreRuntimePackLocation(t *testing.T) {
	cityDir := t.TempDir()
	archiveDir := filepath.Join(cityDir, "archive")
	initBareArchiveRepo(t, archiveDir, true)
	writeArchiveState(t, filepath.Join(cityDir, ".gc", "runtime", "packs", "core"), `{"last_push_at":"2026-06-09T00:00:00Z"}`)

	env := map[string]string{
		"GC_JSONL_ARCHIVE_REPO": archiveDir,
	}
	result := runJsonlArchiveCheck(t, cityDir, env)
	if result.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want OK; result=%#v", result.Status, result)
	}
	if !strings.Contains(result.Message, "2026-06-09T00:00:00Z") {
		t.Fatalf("message = %q (core state path ignored?)", result.Message)
	}
}

func TestJsonlArchiveDoctorCheck_StateInLegacyMaintenancePackLocation(t *testing.T) {
	cityDir := t.TempDir()
	archiveDir := filepath.Join(cityDir, "archive")
	initBareArchiveRepo(t, archiveDir, true)
	writeArchiveState(t, filepath.Join(cityDir, ".gc", "runtime", "packs", "maintenance"), `{"last_push_at":"2026-05-30T00:00:00Z"}`)

	env := map[string]string{
		"GC_JSONL_ARCHIVE_REPO": archiveDir,
	}
	result := runJsonlArchiveCheck(t, cityDir, env)
	if result.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want OK; result=%#v", result.Status, result)
	}
	if !strings.Contains(result.Message, "2026-05-30T00:00:00Z") {
		t.Fatalf("message = %q (legacy maintenance state path ignored?)", result.Message)
	}
}

func TestJsonlArchiveDoctorCheck_StateEnvOverridesRuntimePackLocations(t *testing.T) {
	cityDir := t.TempDir()
	stateDir := t.TempDir()
	archiveDir := filepath.Join(cityDir, "archive")
	initBareArchiveRepo(t, archiveDir, true)
	writeArchiveState(t, stateDir, `{"last_push_at":"2026-06-09T12:00:00Z"}`)
	writeArchiveState(t, filepath.Join(cityDir, ".gc", "runtime", "packs", "core"), `{"last_push_at":"2026-06-09T00:00:00Z"}`)

	env := map[string]string{
		"GC_PACK_STATE_DIR":     stateDir,
		"GC_JSONL_ARCHIVE_REPO": archiveDir,
	}
	result := runJsonlArchiveCheck(t, cityDir, env)
	if result.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want OK; result=%#v", result.Status, result)
	}
	if !strings.Contains(result.Message, "2026-06-09T12:00:00Z") {
		t.Fatalf("message = %q (GC_PACK_STATE_DIR did not win?)", result.Message)
	}
}

func TestJsonlArchiveDoctorCheck_ArchiveRepoRuntimePackPrecedence(t *testing.T) {
	for _, tt := range []struct {
		name              string
		coreExists        bool
		maintenanceExists bool
		legacyExists      bool
		want              string
	}{
		{
			name:       "core wins",
			coreExists: true,
			want:       "core",
		},
		{
			name:              "maintenance fallback",
			maintenanceExists: true,
			legacyExists:      true,
			want:              "maintenance",
		},
		{
			name:         "pre-pack fallback",
			legacyExists: true,
			want:         "legacy",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cityDir := t.TempDir()
			runtimeDir := filepath.Join(cityDir, ".gc", "runtime")
			paths := map[string]string{
				"core":        filepath.Join(runtimeDir, "packs", "core", "jsonl-archive"),
				"maintenance": filepath.Join(runtimeDir, "packs", "maintenance", "jsonl-archive"),
				"legacy":      filepath.Join(cityDir, ".gc", "jsonl-archive"),
			}
			if tt.coreExists {
				initBareArchiveRepo(t, paths["core"], true)
			}
			if tt.maintenanceExists {
				initBareArchiveRepo(t, paths["maintenance"], true)
			}
			if tt.legacyExists {
				initBareArchiveRepo(t, paths["legacy"], true)
			}

			check := newJsonlArchiveDoctorCheck(cityDir)
			check.getenv = stubArchiveEnv{vars: map[string]string{
				"GC_CITY_RUNTIME_DIR": runtimeDir,
			}}.get
			if got := check.resolveArchiveRepo(); got != paths[tt.want] {
				t.Fatalf("resolveArchiveRepo() = %q, want %q", got, paths[tt.want])
			}
		})
	}
}

func TestDoDoctorRegistersJsonlArchiveCheck(t *testing.T) {
	clearGCEnv(t)
	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeCityToml(t, cityDir, `[workspace]
name = "demo"

[beads]
provider = "file"
`)
	writePackToml(t, cityDir, `[pack]
name = "demo"
schema = 1
`)
	archiveDir := filepath.Join(cityDir, "archive")
	initBareArchiveRepo(t, archiveDir, false)
	t.Setenv("GC_JSONL_ARCHIVE_REPO", archiveDir)

	prevCityFlag := cityFlag
	prevCityDoltCheck := newDoctorDoltServerCheck
	prevRigDoltCheck := newDoctorRigDoltServerCheck
	t.Cleanup(func() {
		cityFlag = prevCityFlag
		newDoctorDoltServerCheck = prevCityDoltCheck
		newDoctorRigDoltServerCheck = prevRigDoltCheck
	})
	cityFlag = cityDir
	newDoctorDoltServerCheck = func(cityPath string, _ bool) *doctor.DoltServerCheck {
		return doctor.NewDoltServerCheck(cityPath, true)
	}
	newDoctorRigDoltServerCheck = func(cityPath string, rig config.Rig, _ bool) *doctor.RigDoltServerCheck {
		return doctor.NewRigDoltServerCheck(cityPath, rig, true)
	}

	var stdout, stderr strings.Builder
	_ = doDoctor(doctorOpts{Verbose: true}, &stdout, &stderr)
	out := stdout.String() + stderr.String()
	if !strings.Contains(out, "jsonl-archive") {
		t.Fatalf("doctor output missing jsonl-archive check:\n%s", out)
	}
	if !strings.Contains(out, "local-only mode") {
		t.Fatalf("doctor output missing local-only warning:\n%s", out)
	}
}

func TestDoDoctorJSONOutputIncludesArchiveCheck(t *testing.T) {
	clearGCEnv(t)
	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeCityToml(t, cityDir, `[workspace]
name = "demo"

[beads]
provider = "file"
`)
	writePackToml(t, cityDir, `[pack]
name = "demo"
schema = 1
`)
	archiveDir := filepath.Join(cityDir, "archive")
	initBareArchiveRepo(t, archiveDir, false)
	t.Setenv("GC_JSONL_ARCHIVE_REPO", archiveDir)

	prevCityFlag := cityFlag
	prevCityDoltCheck := newDoctorDoltServerCheck
	prevRigDoltCheck := newDoctorRigDoltServerCheck
	t.Cleanup(func() {
		cityFlag = prevCityFlag
		newDoctorDoltServerCheck = prevCityDoltCheck
		newDoctorRigDoltServerCheck = prevRigDoltCheck
	})
	cityFlag = cityDir
	newDoctorDoltServerCheck = func(cityPath string, _ bool) *doctor.DoltServerCheck {
		return doctor.NewDoltServerCheck(cityPath, true)
	}
	newDoctorRigDoltServerCheck = func(cityPath string, rig config.Rig, _ bool) *doctor.RigDoltServerCheck {
		return doctor.NewRigDoltServerCheck(cityPath, rig, true)
	}

	var stdout, stderr strings.Builder
	_ = doDoctor(doctorOpts{JSON: true}, &stdout, &stderr)

	out := stdout.String()
	if !strings.HasPrefix(strings.TrimSpace(out), "{") {
		t.Fatalf("expected JSON object, got:\n%s", out)
	}
	var decoded struct {
		Passed  int
		Warned  int
		Failed  int
		Results []struct {
			Name    string
			Status  string
			Message string
			FixHint string `json:"fix_hint"`
		}
	}
	if err := jsonUnmarshalString(out, &decoded); err != nil {
		t.Fatalf("unmarshal doctor JSON: %v\n%s", err, out)
	}
	var found *struct {
		Name    string
		Status  string
		Message string
		FixHint string `json:"fix_hint"`
	}
	for i := range decoded.Results {
		if decoded.Results[i].Name == "jsonl-archive" {
			found = &decoded.Results[i]
			break
		}
	}
	if found == nil {
		t.Fatalf("jsonl-archive check missing from JSON output:\n%s", out)
	}
	if found.Status != "warning" {
		t.Fatalf("jsonl-archive status = %q, want \"warning\"", found.Status)
	}
	if !strings.Contains(found.Message, "local-only mode") {
		t.Fatalf("jsonl-archive message = %q", found.Message)
	}
	if found.FixHint == "" {
		t.Fatalf("expected fix_hint in JSON result")
	}
}

func TestFormatArchivePushFailureMessage(t *testing.T) {
	tests := []struct {
		count  int
		stderr string
		want   string
	}{
		{1, "", "push mode, 1 consecutive push failure(s)"},
		{5, "  ", "push mode, 5 consecutive push failure(s)"},
		{2, "fatal: remote rejected", "push mode, 2 consecutive push failure(s). Last stderr: fatal: remote rejected"},
	}
	for _, tt := range tests {
		got := formatArchivePushFailureMessage(tt.count, tt.stderr)
		if got != tt.want {
			t.Errorf("formatArchivePushFailureMessage(%d, %q) = %q, want %q", tt.count, tt.stderr, got, tt.want)
		}
	}
}

// Stubbed errors verify telemetry safety; only the existing real-Git cases
// exercise the subprocess, and neither establishes the hosted failure's cause.
func TestArchiveGitFailureDiagnosticSafeLabels(t *testing.T) {
	archiveDir := t.TempDir()
	secret := "https://private-user:private-token@example.invalid/private-repo"
	for _, tt := range []struct{ stderr, class string }{
		{"fatal: not a git repository: " + secret, "not_repository"},
		{"fatal: detected dubious ownership in repository at " + secret, "dubious_ownership"},
		{"fatal: bad config line 1 in file " + secret, "invalid_config"},
		{"fatal: unable to read config file " + secret, "config_read"},
		{secret + ": Permission denied", "permission_denied"},
		{"unclassified error " + secret, "unknown"},
		{"", "unknown"},
	} {
		t.Run(tt.class+fmt.Sprint(len(tt.stderr)), func(t *testing.T) {
			err := fmt.Errorf("sensitive wrapper %s: %w", secret, &exec.ExitError{Stderr: []byte(tt.stderr)})
			got := archiveGitFailureDiagnostic(archiveDir, err)
			want := fmt.Sprintf("exit status -1; stderr_class=%s captured_stderr_bytes=%d captured_stderr_sha256=%x post_error_snapshot={git=missing HEAD=not_inspected config=not_inspected objects=not_inspected refs=not_inspected}", tt.class, len(tt.stderr), sha256.Sum256([]byte(tt.stderr)))
			assert.Equal(t, want, got)
			for _, forbidden := range []string{secret, archiveDir, "sensitive wrapper", "fatal:"} {
				assert.NotContains(t, got, forbidden, "diagnostic exposes sensitive input")
			}
		})
	}
	got := archiveGitFailureDiagnostic(archiveDir, errors.New(secret))
	assert.True(t, strings.HasPrefix(got, "exit status -1; stderr_class=unknown captured_stderr_bytes=0 "), "non-exit error was not safely classified: %q", got)
	assert.NotContains(t, got, secret)
}

func TestArchiveGitFailureDiagnosticPostErrorSnapshot(t *testing.T) {
	archiveDir := t.TempDir()
	gitDir := filepath.Join(archiveDir, ".git")
	require.NoError(t, os.MkdirAll(filepath.Join(gitDir, "objects"), 0o755))
	secret := "private-token-and-config-contents"
	for _, name := range []string{"HEAD", "config"} {
		require.NoError(t, os.WriteFile(filepath.Join(gitDir, name), []byte(secret), 0o600))
	}
	got := archiveGitFailureDiagnostic(archiveDir, errors.New(secret))
	assert.Contains(t, got, "post_error_snapshot={git=directory HEAD=regular config=regular objects=directory refs=missing}")
	assert.NotContains(t, got, secret)
	// A worktree gitfile is metadata only; its content is never followed.
	gitFileArchive := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(gitFileArchive, ".git"), []byte("gitdir: "+gitDir), 0o600))
	got = archiveGitFailureDiagnostic(gitFileArchive, errors.New(secret))
	assert.Contains(t, got, "post_error_snapshot={git=regular HEAD=not_inspected config=not_inspected objects=not_inspected refs=not_inspected}")
	assert.NotContains(t, got, gitDir)
	linkedArchive := t.TempDir()
	require.NoError(t, os.Symlink(gitDir, filepath.Join(linkedArchive, ".git")))
	got = archiveGitFailureDiagnostic(linkedArchive, errors.New(secret))
	assert.Contains(t, got, "post_error_snapshot={git=symlink HEAD=not_inspected config=not_inspected objects=not_inspected refs=not_inspected}")
	assert.NotContains(t, got, gitDir)
}
