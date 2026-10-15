package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/git"
)

// jsonlArchiveState mirrors the keys the jsonl-export.sh script persists in
// its state file. Only the push-health fields are read here; other keys
// (pending spike alerts, etc.) are intentionally ignored so this check stays
// a narrow projection of "is the archive being backed up off-box?".
type jsonlArchiveState struct {
	ConsecutivePushFailures int    `json:"consecutive_push_failures,omitempty"`
	LastPushAt              string `json:"last_push_at,omitempty"`
	LastPushStderr          string `json:"last_push_stderr,omitempty"`
}

type jsonlArchiveDoctorCheck struct {
	cityPath string
	// env overrides for tests. nil means use os.Getenv.
	getenv func(string) string
	// git command override for tests. nil means use exec.Command("git", ...).
	runGit func(dir string, args ...string) ([]byte, error)
}

func newJsonlArchiveDoctorCheck(cityPath string) *jsonlArchiveDoctorCheck {
	return &jsonlArchiveDoctorCheck{cityPath: cityPath}
}

func (c *jsonlArchiveDoctorCheck) Name() string { return "jsonl-archive" }

func (c *jsonlArchiveDoctorCheck) CanFix() bool { return false }

func (c *jsonlArchiveDoctorCheck) Fix(_ *doctor.CheckContext) error { return nil }

func (c *jsonlArchiveDoctorCheck) env(key string) string {
	if c.getenv != nil {
		return c.getenv(key)
	}
	return os.Getenv(key)
}

func (c *jsonlArchiveDoctorCheck) git(dir string, args ...string) ([]byte, error) {
	if c.runGit != nil {
		return c.runGit(dir, args...)
	}
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	// Strip leaked GIT_DIR/GIT_WORK_TREE/GIT_INDEX_FILE (and related) variables
	// so the archive remote query resolves from the archive repo passed via -C,
	// not a parent repository inherited from a pre-commit hook or nested worktree
	// (which would report push-mode health for the wrong repository).
	cmd.Env = git.SanitizedEnv()
	return cmd.Output()
}

// resolveStateFile mirrors the precedence rules embedded in jsonl-export.sh:
//  1. $GC_PACK_STATE_DIR/jsonl-export-state.json
//  2. $GC_CITY_RUNTIME_DIR/packs/core/jsonl-export-state.json
//  3. <cityPath>/.gc/runtime/packs/core/jsonl-export-state.json
//
// When the primary path is absent, legacy maintenance-pack and pre-pack state
// files are returned if they exist.
func (c *jsonlArchiveDoctorCheck) resolveStateFile() string {
	base := c.env("GC_PACK_STATE_DIR")
	runtime := c.env("GC_CITY_RUNTIME_DIR")
	if runtime == "" {
		runtime = filepath.Join(c.cityPath, ".gc", "runtime")
	}
	if base == "" {
		base = filepath.Join(runtime, "packs", "core")
	}
	primary := filepath.Join(base, "jsonl-export-state.json")
	if _, err := os.Stat(primary); err == nil {
		return primary
	}
	legacyPack := filepath.Join(runtime, "packs", "maintenance", "jsonl-export-state.json")
	if _, err := os.Stat(legacyPack); err == nil {
		return legacyPack
	}
	legacy := filepath.Join(c.cityPath, ".gc", "jsonl-export-state.json")
	if _, err := os.Stat(legacy); err == nil {
		return legacy
	}
	return primary
}

// resolveArchiveRepo mirrors the precedence rules embedded in jsonl-export.sh
// for locating the archive repository. The script falls back from the
// primary location to legacy locations when the primary has no `.git`.
func (c *jsonlArchiveDoctorCheck) resolveArchiveRepo() string {
	if v := strings.TrimSpace(c.env("GC_JSONL_ARCHIVE_REPO")); v != "" {
		return v
	}
	base := c.env("GC_PACK_STATE_DIR")
	runtime := c.env("GC_CITY_RUNTIME_DIR")
	if runtime == "" {
		runtime = filepath.Join(c.cityPath, ".gc", "runtime")
	}
	if base == "" {
		base = filepath.Join(runtime, "packs", "core")
	}
	primary := filepath.Join(base, "jsonl-archive")
	if _, err := os.Stat(filepath.Join(primary, ".git")); err == nil {
		return primary
	}
	legacyPack := filepath.Join(runtime, "packs", "maintenance", "jsonl-archive")
	if _, err := os.Stat(filepath.Join(legacyPack, ".git")); err == nil {
		return legacyPack
	}
	legacy := filepath.Join(c.cityPath, ".gc", "jsonl-archive")
	if _, err := os.Stat(filepath.Join(legacy, ".git")); err == nil {
		return legacy
	}
	return primary
}

func (c *jsonlArchiveDoctorCheck) readState(path string) (jsonlArchiveState, bool, error) {
	var st jsonlArchiveState
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return st, false, nil
		}
		return st, false, err
	}
	if len(data) == 0 {
		return st, true, nil
	}
	// Tolerate malformed state: the script itself resets to `{}` on a bad
	// parse, so doctor should not promote that to ERROR.
	_ = json.Unmarshal(data, &st)
	return st, true, nil
}

func (c *jsonlArchiveDoctorCheck) archiveHasOrigin(archiveRepo string) (bool, error) {
	out, err := c.git(archiveRepo, "remote", "-v")
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) >= 2 && fields[0] == "origin" {
			return true, nil
		}
	}
	return false, nil
}

// archiveGitFailureDiagnostic projects only fixed labels from the original
// child error. Output retains bounded stderr on ExitError; neither its bytes
// nor arbitrary error text may enter doctor output. The filesystem observations
// are post-error snapshots, not an atomic view of the failing child.
func archiveGitFailureDiagnostic(archiveRepo string, err error) string {
	exitCode := -1
	var stderr []byte
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		exitCode = exitErr.ExitCode()
		stderr = exitErr.Stderr
	}
	class := "unknown"
	for _, candidate := range []struct{ text, class string }{
		{"fatal: not a git repository", "not_repository"},
		{"fatal: detected dubious ownership", "dubious_ownership"},
		{"fatal: bad config line", "invalid_config"},
		{"fatal: unable to read config file", "config_read"},
		{"Permission denied", "permission_denied"},
	} {
		if bytes.Contains(stderr, []byte(candidate.text)) {
			class = candidate.class
			break
		}
	}
	gitDir := filepath.Join(archiveRepo, ".git")
	gitKind := archiveGitPathKind(gitDir)
	snapshot := "git=" + gitKind
	for _, name := range []string{"HEAD", "config", "objects", "refs"} {
		kind := "not_inspected"
		// Do not follow a .git symlink or read a worktree gitfile's contents.
		if gitKind == "directory" {
			kind = archiveGitPathKind(filepath.Join(gitDir, name))
		}
		snapshot += " " + name + "=" + kind
	}
	return fmt.Sprintf("exit status %d; stderr_class=%s captured_stderr_bytes=%d captured_stderr_sha256=%x post_error_snapshot={%s}",
		exitCode, class, len(stderr), sha256.Sum256(stderr), snapshot)
}

func archiveGitPathKind(path string) string {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return "missing"
	}
	if err != nil {
		return "stat_error"
	}
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		return "symlink"
	case info.IsDir():
		return "directory"
	case info.Mode().IsRegular():
		return "regular"
	default:
		return "other"
	}
}

func (c *jsonlArchiveDoctorCheck) Run(_ *doctor.CheckContext) *doctor.CheckResult {
	r := &doctor.CheckResult{Name: c.Name()}

	archiveRepo := c.resolveArchiveRepo()
	stateFile := c.resolveStateFile()

	_, stateExists, stateErr := c.readState(stateFile)
	if stateErr != nil {
		r.Status = doctor.StatusError
		r.Message = fmt.Sprintf("reading %s: %v", stateFile, stateErr)
		return r
	}

	archiveDirStat, archiveDirErr := os.Stat(archiveRepo)
	archiveDirExists := archiveDirErr == nil && archiveDirStat.IsDir()

	if !stateExists && !archiveDirExists {
		r.Status = doctor.StatusOK
		r.Message = "no archive activity yet (script has not run)"
		return r
	}

	if archiveDirExists {
		if _, err := os.Stat(filepath.Join(archiveRepo, ".git")); err != nil {
			r.Status = doctor.StatusError
			r.Message = fmt.Sprintf("archive repo at %s is malformed (no .git)", archiveRepo)
			r.FixHint = fmt.Sprintf("remove or re-init %s so the next export run recreates it", archiveRepo)
			return r
		}
	}

	// Re-read the state so we can use the parsed values for push messaging.
	state, _, _ := c.readState(stateFile)

	// When there's no archive repo yet but state exists (rare), we still
	// report OK: the next run will create the repo.
	if !archiveDirExists {
		r.Status = doctor.StatusOK
		r.Message = "no archive activity yet (script has not run)"
		return r
	}

	hasOrigin, err := c.archiveHasOrigin(archiveRepo)
	if err != nil {
		r.Status = doctor.StatusError
		r.Message = "querying archive remotes: " + archiveGitFailureDiagnostic(archiveRepo, err)
		r.FixHint = fmt.Sprintf(`run "git -C %s remote -v" to diagnose`, archiveRepo)
		return r
	}

	if !hasOrigin {
		r.Status = doctor.StatusWarning
		r.Message = "local-only mode — commits stay on this host, off-box backup disabled"
		r.FixHint = fmt.Sprintf(`configure a remote with "git -C %s remote add origin <url>" to enable push mode`, archiveRepo)
		return r
	}

	if state.ConsecutivePushFailures > 0 {
		r.Status = doctor.StatusError
		r.Message = formatArchivePushFailureMessage(state.ConsecutivePushFailures, state.LastPushStderr)
		r.FixHint = fmt.Sprintf(`check "git -C %s remote -v", verify credentials, then let the next jsonl-export run retry`, archiveRepo)
		return r
	}

	r.Status = doctor.StatusOK
	if state.LastPushAt != "" {
		r.Message = fmt.Sprintf("push mode, last successful push %s", state.LastPushAt)
	} else {
		r.Message = "push mode, no pushes attempted yet"
	}
	return r
}

func formatArchivePushFailureMessage(count int, stderr string) string {
	base := fmt.Sprintf("push mode, %d consecutive push failure(s)", count)
	if strings.TrimSpace(stderr) == "" {
		return base
	}
	return fmt.Sprintf("%s. Last stderr: %s", base, stderr)
}
