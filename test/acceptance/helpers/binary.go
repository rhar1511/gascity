package acceptancehelpers

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/bazeltest"
)

// BuildGC compiles the gc binary to dir and returns its path.
// Panics on failure — intended for TestMain.
func BuildGC(dir string) string {
	if override := strings.TrimSpace(os.Getenv("GC_ACCEPTANCE_GC_BIN")); override != "" {
		bin, err := filepath.Abs(override)
		if err != nil {
			panic("acceptance: resolving GC_ACCEPTANCE_GC_BIN: " + err.Error())
		}
		info, err := os.Stat(bin)
		if err != nil {
			panic("acceptance: GC_ACCEPTANCE_GC_BIN: " + err.Error())
		}
		if info.IsDir() {
			panic("acceptance: GC_ACCEPTANCE_GC_BIN points to a directory: " + bin)
		}
		return bin
	}

	bin := filepath.Join(dir, "gc")
	// Under bazel the pre-built gc binary ships in runfiles (declared as
	// a data dep); use it instead of shelling out to `go build`.
	for _, rf := range []string{os.Getenv("RUNFILES_DIR"), os.Getenv("TEST_SRCDIR")} {
		if rf == "" {
			continue
		}
		if _, err := os.Stat(filepath.Join(rf, "_main", "cmd", "gc", "gc_", "gc")); err == nil {
			return filepath.Join(rf, "_main", "cmd", "gc", "gc_", "gc")
		}
	}
	cmd := exec.Command("go", "build", "-o", bin, "./cmd/gc")
	cmd.Dir = FindModuleRoot()
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		panic("acceptance: building gc binary: " + err.Error() + "\n" + string(out))
	}
	return bin
}

// FindModuleRoot walks up from cwd to find go.mod.
func FindModuleRoot() string {
	if root := bazeltest.OverrideRoot(); root != "" {
		return root
	}
	dir, err := os.Getwd()
	if err != nil {
		panic("acceptance: getting cwd: " + err.Error())
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			panic("acceptance: go.mod not found")
		}
		dir = parent
	}
}

// FindBD returns the selected bd binary, or empty string if unavailable outside
// Bazel. A missing declared owner-module runfile under Bazel is a tooling error.
func FindBD() string {
	if override := strings.TrimSpace(os.Getenv("GC_ACCEPTANCE_BD_BIN")); override != "" {
		if bin, err := filepath.Abs(override); err == nil {
			if info, statErr := os.Stat(bin); statErr == nil && !info.IsDir() {
				return bin
			}
		}
	}
	// Under Bazel, bd is built from the owner module supplying the linked
	// schema and shipped as a data dependency. Prefer its runfile over PATH;
	// the independent upstream archive has a different compatibility role.
	if bazeltest.IsBazel() {
		for _, rf := range []string{os.Getenv("RUNFILES_DIR"), os.Getenv("TEST_SRCDIR")} {
			if rf == "" {
				continue
			}
			for _, repo := range []string{"gazelle++go_deps+com_github_rhar1511_beads", "com_github_rhar1511_beads"} {
				if bin := filepath.Join(rf, repo, "cmd", "bd", "bd_", "bd"); statBinary(bin) {
					return bin
				}
			}
		}
		panic("acceptance: Bazel owner-module bd runfile is missing; expected @com_github_rhar1511_beads//cmd/bd:bd (cmd/bd/bd_/bd) under RUNFILES_DIR or TEST_SRCDIR; rebuild the declared acceptance data dependency")
	}
	p, err := exec.LookPath("bd")
	if err != nil {
		return ""
	}
	return p
}

func statBinary(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// RequireBD skips t if bd is not available.
func RequireBD(t *testing.T) string {
	t.Helper()
	p := FindBD()
	if p == "" {
		t.Skip("bd not available")
	}
	return p
}
