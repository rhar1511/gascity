//go:build integration

package integration

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/test/toolhome"
)

// Every real bd this suite runs resolves to realBDBinary, directly or through
// the file-store shim (GC_INTEGRATION_REAL_BD). It must be the tool-home
// wrapper, or bd reads the operator's ~/.beads through the real HOME gc needs.
func TestIntegrationRealBdIsTheToolHomeWrapper(t *testing.T) {
	data, err := os.ReadFile(realBDBinary)
	if err != nil {
		t.Fatalf("read realBDBinary %s: %v", realBDBinary, err)
	}
	script := string(data)
	if !strings.HasPrefix(script, "#!/bin/sh\n") || !strings.Contains(script, "\nHOME=") || !strings.Contains(script, "\nexec ") {
		t.Fatalf("realBDBinary %s is not the tool-home wrapper:\n%s", realBDBinary, script)
	}
	if got := parseEnvList(integrationEnv())[integrationRealBDBinaryEnv]; got != realBDBinary {
		t.Fatalf("%s = %q, want the wrapper %q", integrationRealBDBinaryEnv, got, realBDBinary)
	}
}

// The suite's envs carry no host XDG directory or BEADS_*/BD_* value, and pin
// bd's shared-server mode off.
func TestIntegrationEnvCarriesNoHostBdState(t *testing.T) {
	for name, env := range map[string][]string{
		"integrationEnv":     integrationEnv(),
		"integrationEnvDolt": integrationEnvDolt(),
	} {
		vars := parseEnvList(env)
		for k, v := range vars {
			if k != "HOME" && toolhome.IsHomeVar(k) {
				t.Errorf("%s: host %s=%q reaches bd", name, k, v)
			}
		}
		if got := vars[toolhome.SharedServerConfigEnv]; got != "false" {
			t.Errorf("%s: %s = %q, want false", name, toolhome.SharedServerConfigEnv, got)
		}
		if _, ok := vars["BEADS_DOLT_SHARED_SERVER"]; ok {
			t.Errorf("%s: host BEADS_DOLT_SHARED_SERVER reaches bd", name)
		}
	}
}

// The Bazel integration CLI belongs to the linked module, even when an older
// archive and ambient executable are both available.
func TestBazelIntegrationBDSelectsDeclaredOwner(t *testing.T) {
	for _, key := range []string{"RUNFILES_DIR", "TEST_SRCDIR"} {
		for _, repo := range []string{"gazelle++go_deps+com_github_rhar1511_beads", "com_github_rhar1511_beads"} {
			t.Run(key+"/"+repo, func(t *testing.T) {
				t.Setenv("RUNFILES_DIR", "")
				t.Setenv("TEST_SRCDIR", "")
				root := t.TempDir()
				t.Setenv(key, root)
				want := filepath.Join(root, repo, "cmd", "bd", "bd_", "bd")
				if err := os.MkdirAll(filepath.Dir(want), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(want, []byte("owner executable fixture\n"), 0o755); err != nil {
					t.Fatal(err)
				}
				if got := bazelIntegrationBDBinary(); got != want {
					t.Fatalf("selected %q, want declared owner %q", got, want)
				}
			})
		}
	}
}

func TestBazelIntegrationBDMissingOwnerFailsClosed(t *testing.T) {
	for _, shape := range []string{"absent", "directory", "not executable"} {
		t.Run(shape, func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("RUNFILES_DIR", root)
			t.Setenv("TEST_SRCDIR", root)
			old := filepath.Join(root, "bd_bin_v1_3_1", "bd")
			if err := os.MkdirAll(filepath.Dir(old), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(old, []byte("old archive fixture\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", filepath.Dir(old))
			owner := filepath.Join(root, "gazelle++go_deps+com_github_rhar1511_beads", "cmd", "bd", "bd_", "bd")
			if shape != "absent" {
				if err := os.MkdirAll(filepath.Dir(owner), 0o755); err != nil {
					t.Fatal(err)
				}
				if shape == "directory" {
					if err := os.Mkdir(owner, 0o755); err != nil {
						t.Fatal(err)
					}
				} else if err := os.WriteFile(owner, []byte("not executable\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			defer func() {
				failure := recover()
				message, ok := failure.(string)
				if !ok || !strings.Contains(message, "@com_github_rhar1511_beads//cmd/bd:bd") {
					t.Fatalf("missing owner panic = %v, want useful declared-owner error", failure)
				}
			}()
			_ = bazelIntegrationBDBinary()
		})
	}
}
