package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

func TestLoadedPackCompatibilityBindsRequiredFormulaToCapturedManifestAndRoot(t *testing.T) {
	cityDir := t.TempDir()
	writeFixture := func(path, contents string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeFixture(filepath.Join(cityDir, "city.toml"), "[workspace]\nname = \"city-alpha\"\n")
	writeFixture(filepath.Join(cityDir, "pack.toml"), "[pack]\nname = \"city-root\"\nschema = 2\nincludes = [\"packs/required\"]\n")
	writeFixture(filepath.Join(cityDir, "packs", "required", "pack.toml"), "[pack]\nname = \"trusted-pack\"\nschema = 2\nrequires_gc = \">=0.14.0\"\n")
	formulaPath := filepath.Join(cityDir, "packs", "required", "formulas", "review.toml")
	writeFixture(formulaPath, "formula = \"review\"\n")

	cfg, _, err := LoadWithIncludesOptions(fsys.OSFS{}, filepath.Join(cityDir, "city.toml"), LoadOptions{CaptureQualificationInputs: true})
	if err != nil {
		t.Fatalf("LoadWithIncludesOptions: %v", err)
	}
	packs, required, err := cfg.RequiredCompatibilityPacks([]string{formulaPath})
	if err != nil {
		t.Fatalf("RequiredCompatibilityPacks: %v", err)
	}
	if !required || len(packs) != 1 {
		t.Fatalf("required=%v packs=%#v; want one required pack", required, packs)
	}
	pack := packs[0]
	if pack.Name != "trusted-pack" || pack.RequiresGC != ">=0.14.0" || pack.PinStatus != "content" || pack.RootID == "" || pack.ManifestSHA256 == "" || pack.SourceSubpathSHA256 == "" {
		t.Fatalf("pack binding = %#v; want exact manifest/root binding", pack)
	}
	if pack.ManifestSHA256 != qualificationDigest([]byte("[pack]\nname = \"trusted-pack\"\nschema = 2\nrequires_gc = \">=0.14.0\"\n")) {
		t.Fatalf("manifest digest = %q; want digest of exact parsed bytes", pack.ManifestSHA256)
	}
}

func TestRequiredCompatibilityPacksFailsClosedWithoutCapturedRootBinding(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, "pack"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname=\"city\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "pack.toml"), []byte("[pack]\nname=\"required\"\nschema=2\nrequires_gc=\">=0.14.0\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	formulaPath := filepath.Join(cityDir, "formulas", "review.toml")
	if err := os.MkdirAll(filepath.Dir(formulaPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(formulaPath, []byte("formula=\"review\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, _, err := LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityDir, "city.toml"))
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	if _, required, err := cfg.RequiredCompatibilityPacks([]string{formulaPath}); err == nil || !required {
		t.Fatalf("RequiredCompatibilityPacks without loader capture = required %v, err %v; want required+unavailable", required, err)
	}
}

func TestHasRequiredCompatibilityPacksUsesOnlyCapturedDeclarations(t *testing.T) {
	cfg := &City{}
	if cfg.HasRequiredCompatibilityPacks() {
		t.Fatal("empty city reports a required compatibility pack")
	}
	cfg.packCompatibilityBindings = []PackCompatibilityBinding{{Name: "ordinary"}}
	if cfg.HasRequiredCompatibilityPacks() {
		t.Fatal("pack without requires_gc reports a required compatibility pack")
	}
	cfg.packCompatibilityBindings = append(cfg.packCompatibilityBindings, PackCompatibilityBinding{Name: "gated", RequiresGC: ">=0.14.0"})
	if !cfg.HasRequiredCompatibilityPacks() {
		t.Fatal("captured requires_gc declaration was not reported")
	}
}

func TestOrdinaryFormulaDoesNotRequireCompatibilityAuthority(t *testing.T) {
	cityDir := t.TempDir()
	formulaPath := filepath.Join(cityDir, "formulas", "ordinary.toml")
	if err := os.MkdirAll(filepath.Dir(formulaPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(formulaPath, []byte("formula=\"ordinary\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := &City{}
	if packs, required, err := cfg.RequiredCompatibilityPacks([]string{formulaPath}); err != nil || required || len(packs) != 0 {
		t.Fatalf("ordinary formula result = %#v, required=%v, err=%v; want no gate", packs, required, err)
	}
}

func TestRequiresGCIsOnlyMinimumVersionAndRejectsInvalidOrOldBuild(t *testing.T) {
	pack := PackCompatibilityBinding{Name: "generic-pack", RequiresGC: ">=0.14.0"}
	if err := pack.CheckControllerVersion("0.14.2"); err != nil {
		t.Fatalf("CheckControllerVersion(newer): %v", err)
	}
	if err := pack.CheckControllerVersion("0.13.9"); err == nil {
		t.Fatal("CheckControllerVersion(old) succeeded, want minimum-version failure")
	}
	if err := pack.CheckControllerVersion("unknown"); err == nil {
		t.Fatal("CheckControllerVersion(unknown) succeeded, want unavailable")
	}
	pack.RequiresGC = "not-a-constraint"
	if err := pack.CheckControllerVersion("0.14.2"); err == nil {
		t.Fatal("CheckControllerVersion(invalid constraint) succeeded, want unavailable")
	}
}

func qualificationDigest(data []byte) string { return digestBytes(data) }
