package acceptancehelpers

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBuildGCUsesOverrideBinary(t *testing.T) {
	t.Helper()

	tmpDir := t.TempDir()
	override := filepath.Join(tmpDir, "gc-external")
	if err := os.WriteFile(override, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write override binary: %v", err)
	}

	t.Setenv("GC_ACCEPTANCE_GC_BIN", override)

	got := BuildGC(t.TempDir())
	if got != override {
		t.Fatalf("BuildGC() = %q, want %q", got, override)
	}
}

func TestRunGCUsesExactBinaryOverridePath(t *testing.T) {
	t.Helper()

	tmpDir := t.TempDir()
	gcHome := filepath.Join(tmpDir, "home")
	runtimeDir := filepath.Join(tmpDir, "runtime")
	if err := os.MkdirAll(gcHome, 0o755); err != nil {
		t.Fatalf("mkdir gcHome: %v", err)
	}
	if err := os.MkdirAll(runtimeDir, 0o755); err != nil {
		t.Fatalf("mkdir runtimeDir: %v", err)
	}

	override := filepath.Join(tmpDir, "custom-binary-name")
	script := "#!/bin/sh\necho override:$0 $*\n"
	if err := os.WriteFile(override, []byte(script), 0o755); err != nil {
		t.Fatalf("write override binary: %v", err)
	}

	env := NewEnv(override, gcHome, runtimeDir)
	out, err := RunGC(env, "", "version")
	if err != nil {
		t.Fatalf("RunGC: %v\n%s", err, out)
	}
	if !strings.Contains(out, "override:"+override+" version") {
		t.Fatalf("RunGC() output = %q, want override invocation for %q", out, override)
	}
}

func TestFindBDSelectsOwnerModuleRunfile(t *testing.T) {
	for _, repo := range []string{"gazelle++go_deps+com_github_rhar1511_beads", "com_github_rhar1511_beads"} {
		t.Run(repo, func(t *testing.T) {
			runfiles := t.TempDir()
			write := func(rel string) string {
				t.Helper()
				path := filepath.Join(runfiles, rel)
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("fixture executable; never invoked"), 0o755); err != nil {
					t.Fatal(err)
				}
				return path
			}
			owner := write(filepath.Join(repo, "cmd", "bd", "bd_", "bd"))
			write(filepath.Join("+http_archive+bd_bin_v1_3_1", "bd"))
			ambient := write(filepath.Join("ambient", "bd"))
			t.Setenv("GC_ACCEPTANCE_BD_BIN", "")
			t.Setenv("TEST_SRCDIR", runfiles)
			t.Setenv("RUNFILES_DIR", runfiles)
			t.Setenv("PATH", filepath.Dir(ambient))
			if got := FindBD(); got != owner {
				t.Fatalf("FindBD()=%q, want owner module executable %q before stale archive/PATH", got, owner)
			}
			// The explicit caller-selected executable remains authoritative.
			t.Setenv("GC_ACCEPTANCE_BD_BIN", ambient)
			if got := FindBD(); got != ambient {
				t.Fatalf("explicit FindBD()=%q, want %q", got, ambient)
			}
		})
	}
}

func TestFindBDRejectsMissingOwnerModuleRunfile(t *testing.T) {
	for _, ambientAvailable := range []bool{true, false} {
		name := "without PATH bd"
		if ambientAvailable {
			name = "with stale archive and ambient bd"
		}
		t.Run(name, func(t *testing.T) {
			runfiles := t.TempDir()
			staleArchive := filepath.Join(runfiles, "+http_archive+bd_bin_v1_3_1", "bd")
			if err := os.MkdirAll(filepath.Dir(staleArchive), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(staleArchive, []byte("stale upstream fixture; never invoked"), 0o755); err != nil {
				t.Fatal(err)
			}
			ambientDir := t.TempDir()
			if ambientAvailable {
				if err := os.WriteFile(filepath.Join(ambientDir, "bd"), []byte("ambient fixture; never invoked"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("GC_ACCEPTANCE_BD_BIN", "")
			t.Setenv("TEST_SRCDIR", runfiles)
			t.Setenv("RUNFILES_DIR", runfiles)
			t.Setenv("PATH", ambientDir)
			defer func() {
				failure := recover()
				message, ok := failure.(string)
				if !ok || !strings.Contains(message, "Bazel owner-module bd runfile is missing") || !strings.Contains(message, "@com_github_rhar1511_beads//cmd/bd:bd") {
					t.Errorf("FindBD missing-owner failure=%v, want explicit declared-owner tooling failure", failure)
				}
			}()
			got := FindBD()
			t.Errorf("FindBD returned %q with missing declared owner; neither ambient bd nor unavailable/skip is permitted under Bazel", got)
		})
	}
}

func TestFindBDUsesPathOutsideBazel(t *testing.T) {
	ambDir := t.TempDir()
	ambient := filepath.Join(ambDir, "bd")
	if err := os.WriteFile(ambient, []byte("ambient fixture; never invoked"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_ACCEPTANCE_BD_BIN", "")
	t.Setenv("TEST_SRCDIR", "")
	t.Setenv("RUNFILES_DIR", "")
	t.Setenv("PATH", ambDir)
	if got := FindBD(); got != ambient {
		t.Fatalf("ordinary FindBD()=%q, want unchanged PATH executable %q", got, ambient)
	}
	if err := os.Remove(ambient); err != nil {
		t.Fatal(err)
	}
	if got := FindBD(); got != "" {
		t.Fatalf("ordinary unavailable FindBD()=%q, want empty", got)
	}
}
