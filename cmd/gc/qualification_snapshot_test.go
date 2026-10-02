package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/qualification"
)

func TestResolveRigPathsAndRefreshQualificationCapturesResolvedPath(t *testing.T) {
	cityRoot := t.TempDir()
	t.Setenv("GC_HOME", filepath.Join(cityRoot, "isolated-gc-home"))
	rigRoot := filepath.Join(cityRoot, "rig")
	if err := os.Mkdir(rigRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	cityPath := filepath.Join(cityRoot, "city.toml")
	cityTOML := "[workspace]\nname = \"qualification-path-test\"\n\n[[rigs]]\nname = \"alpha\"\npath = \"rig\"\nprefix = \"al\"\n"
	if err := os.WriteFile(cityPath, []byte(cityTOML), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, prov, err := config.LoadWithIncludesOptions(fsys.OSFS{}, cityPath, config.LoadOptions{CaptureQualificationInputs: true})
	if err != nil {
		t.Fatalf("LoadWithIncludesOptions: %v", err)
	}
	if len(cfg.Rigs) != 1 || cfg.Rigs[0].Path != "rig" {
		t.Fatalf("loaded rig path = %#v, want relative path before runtime normalization", cfg.Rigs)
	}
	before := config.RefreshQualificationSnapshot(cfg, prov)
	if before.Status != qualification.StatusAvailable {
		t.Fatalf("unresolved-path snapshot = %#v, want available loader snapshot", before)
	}

	resolveRigPathsAndRefreshQualification(cityRoot, cfg, prov)
	after := cfg.QualificationSnapshot()
	if cfg.Rigs[0].Path != rigRoot {
		t.Fatalf("published rig path = %q, want %q", cfg.Rigs[0].Path, rigRoot)
	}
	if after.Status != qualification.StatusAvailable {
		t.Fatalf("resolved-path snapshot = %#v, want available", after)
	}
	if after.EffectiveConfigSHA256 == before.EffectiveConfigSHA256 {
		t.Fatal("effective config digest did not change after relative rig path normalization")
	}

	effective := struct {
		Config                  *config.City `json:"config"`
		ResolvedWorkspaceName   string       `json:"resolved_workspace_name,omitempty"`
		ResolvedWorkspacePrefix string       `json:"resolved_workspace_prefix,omitempty"`
	}{
		Config:                  cfg,
		ResolvedWorkspaceName:   cfg.ResolvedWorkspaceName,
		ResolvedWorkspacePrefix: cfg.ResolvedWorkspacePrefix,
	}
	want, err := qualification.NewSnapshot(effective, prov.QualificationInputs())
	if err != nil {
		t.Fatalf("NewSnapshot(final config): %v", err)
	}
	if after.EffectiveConfigSHA256 != want.EffectiveConfigSHA256 || after.EffectiveConfigIdentitySHA256 != want.EffectiveConfigIdentitySHA256 {
		t.Fatalf("snapshot = %#v, want identity for final resolved config %#v", after, want)
	}
}
