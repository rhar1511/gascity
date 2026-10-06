package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/fsys"
)

func TestCanonicalScopeLocalConfigKeepsPortableAndMetadata(t *testing.T) {
	scope := t.TempDir()
	beadsDir := filepath.Join(scope, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	const portable = "# Portable team settings.\nissue-prefix: inktree\nsync.remote: git+https://github.com/rhar1511/inktree.git\n"
	const metadata = `{"backend":"dolt","database":"dolt","dolt_database":"inktree","dolt_mode":"server","project_id":"fixture-project"}`
	for name, data := range map[string]string{"config.yaml": portable, "config.local.yaml": "local-extension: keep\n", "metadata.json": metadata} {
		if err := os.WriteFile(filepath.Join(beadsDir, name), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	state := contract.ConfigState{IssuePrefix: "inktree", DoltHost: "127.0.0.1", DoltPort: "3309", DoltMode: "server", EndpointOrigin: contract.EndpointOriginInheritedCity, EndpointStatus: contract.EndpointStatusVerified}
	fs := fsys.OSFS{}
	for attempt := 0; attempt < 2; attempt++ {
		if err := ensureCanonicalScopeConfigState(fs, scope, state); err != nil {
			t.Fatal(err)
		}
		for name, want := range map[string]string{"config.yaml": portable, "metadata.json": metadata} {
			got, err := os.ReadFile(filepath.Join(beadsDir, name))
			if err != nil || !bytes.Equal(got, []byte(want)) {
				t.Fatalf("attempt=%d changed %s: err=%v\n%s", attempt, name, err, got)
			}
		}
		cfg, ok, err := contract.ReadConfigState(fs, filepath.Join(beadsDir, "config.yaml"))
		if err != nil || !ok || cfg.DoltHost != "127.0.0.1" || cfg.DoltPort != "3309" || cfg.DoltMode != "server" || cfg.EndpointStatus != contract.EndpointStatusVerified {
			t.Fatalf("runtime endpoint=%+v present=%v err=%v", cfg, ok, err)
		}
	}
}
