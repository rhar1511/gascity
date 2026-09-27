package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/qualification"
)

func TestQualificationLoaderCapturesCityAndPackInputs(t *testing.T) {
	cityDir := t.TempDir()
	t.Setenv("GC_HOME", filepath.Join(cityDir, "isolated-gc-home"))
	cityPath := filepath.Join(cityDir, "city.toml")
	packDir := filepath.Join(cityDir, "packs", "local")
	writeQualificationInput(t, cityPath, `[workspace]
name = "qualification-test"
`)
	writeQualificationInput(t, filepath.Join(packDir, "pack.toml"), `[pack]
name = "local"
schema = 2
`)
	assetPath := filepath.Join(packDir, "assets", "policy.txt")
	writeQualificationInput(t, assetPath, "policy revision one\n")

	first := loadQualificationTestCity(t, cityPath, packDir)
	if first.Status != qualification.StatusAvailable {
		t.Fatalf("initial snapshot = %#v, want available", first)
	}
	if len(first.InputRoots) != 2 || first.InputRoots[0].ID != "city" || !strings.HasPrefix(first.InputRoots[1].ID, "pack:") {
		t.Fatalf("input roots = %#v, want city and local pack roots", first.InputRoots)
	}
	for _, root := range first.InputRoots {
		if len(root.ResolvedPathSHA256) != 64 || len(root.InputsSHA256) != 64 || root.InputCount == 0 {
			t.Fatalf("root lacks canonical hashes/input count: %#v", root)
		}
	}

	encoded, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), cityDir) || strings.Contains(string(encoded), "policy revision one") {
		t.Fatalf("qualification snapshot disclosed a path or input bytes: %s", encoded)
	}

	if err := os.WriteFile(assetPath, []byte("policy revision two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	second := loadQualificationTestCity(t, cityPath, packDir)
	if second.EffectiveConfigInputClosureSHA256 == first.EffectiveConfigInputClosureSHA256 {
		t.Fatal("pack asset change did not change loaded input closure digest")
	}
	if second.EffectiveConfigIdentitySHA256 == first.EffectiveConfigIdentitySHA256 {
		t.Fatal("pack asset change did not change effective config identity")
	}
}

func TestQualificationLoaderMarksExternalFragmentUnavailable(t *testing.T) {
	root := t.TempDir()
	t.Setenv("GC_HOME", filepath.Join(root, "isolated-gc-home"))
	cityDir := filepath.Join(root, "city")
	externalDir := filepath.Join(root, "shared")
	if err := os.MkdirAll(cityDir, 0o755); err != nil {
		t.Fatal(err)
	}
	fragmentPath := filepath.Join(externalDir, "fragment.toml")
	writeQualificationInput(t, filepath.Join(cityDir, "city.toml"), `[workspace]
name = "qualification-test"
`)
	writeQualificationInput(t, fragmentPath, "# external loader input\n")

	_, provenance, err := LoadWithIncludesOptions(
		fsys.OSFS{}, filepath.Join(cityDir, "city.toml"),
		LoadOptions{CaptureQualificationInputs: true},
		filepath.Join("..", "shared", "fragment.toml"),
	)
	if err != nil {
		t.Fatalf("LoadWithIncludesOptions: %v", err)
	}
	closure := provenance.QualificationInputs()
	if closure.Status != qualification.StatusUnavailable {
		t.Fatalf("closure = %#v, want unavailable for an external fragment", closure)
	}
	foundExternal := false
	for _, root := range closure.Roots {
		if root.Kind == "external" {
			foundExternal = true
			if root.UnavailableReason == "" || root.ResolvedPathSHA256 == "" || root.InputsSHA256 == "" {
				t.Fatalf("external root lacks explicit unavailable identity: %#v", root)
			}
		}
	}
	if !foundExternal {
		t.Fatalf("external input not represented: %#v", closure.Roots)
	}
}

func TestQualificationLoaderFailsClosedOnUnclassifiedPackState(t *testing.T) {
	cityDir := t.TempDir()
	t.Setenv("GC_HOME", filepath.Join(cityDir, "isolated-gc-home"))
	cityPath := filepath.Join(cityDir, "city.toml")
	packDir := filepath.Join(cityDir, "packs", "local")
	writeQualificationInput(t, cityPath, `[workspace]
name = "qualification-test"
`)
	writeQualificationInput(t, filepath.Join(packDir, "pack.toml"), `[pack]
name = "local"
schema = 2
`)
	writeQualificationInput(t, filepath.Join(packDir, "state", "private.json"), `{"ignored_by_loader":true}`)

	snapshot := loadQualificationTestCity(t, cityPath, packDir)
	if snapshot.Status != qualification.StatusUnavailable || snapshot.Reason != "pack_input_directory_unclassified" {
		t.Fatalf("snapshot = %#v, want unavailable for unclassified pack state", snapshot)
	}
}

func TestCanonicalQualificationPathResolvesAliases(t *testing.T) {
	root := t.TempDir()
	realDir := filepath.Join(root, "real")
	alias := filepath.Join(root, "alias")
	if err := os.Mkdir(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realDir, alias); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	resolvedReal := canonicalQualificationPath(realDir)
	resolvedAlias := canonicalQualificationPath(alias)
	if resolvedReal == "" || resolvedAlias != resolvedReal {
		t.Fatalf("canonical paths: real=%q alias=%q, want the same resolved absolute path", resolvedReal, resolvedAlias)
	}
	if digest := qualificationPathDigest(resolvedAlias); len(digest) != 64 {
		t.Fatalf("canonical path digest = %q, want SHA-256 hex", digest)
	}
}

func TestQualificationCaptureRejectsConflictingReadsAndPackAliases(t *testing.T) {
	root := t.TempDir()
	packA := filepath.Join(root, "pack-a")
	packB := filepath.Join(root, "pack-b")
	if err := os.MkdirAll(packA, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(packB, 0o755); err != nil {
		t.Fatal(err)
	}
	inputPath := filepath.Join(root, "input.toml")
	writeQualificationInput(t, inputPath, "first")

	capture := newQualificationCapture(fsys.OSFS{}, root)
	capture.recordRead(inputPath, []byte("first"))
	capture.recordRead(inputPath, []byte("second"))
	const lockedPin = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	capture.recordPackRoot(packA, "https://example.test/pack.git", lockedPin, "locked")
	// Simulate the same stable source/pin resolving to a different root. The
	// identity collision must remain visible and disqualify the closure.
	capture.recordPackRoot(packB, "https://example.test/pack.git", lockedPin, "locked")
	closure := capture.closure()
	if closure.Status != qualification.StatusUnavailable {
		t.Fatalf("closure = %#v, want unavailable after conflicting reads/root identity", closure)
	}
	if closure.Reason != "duplicate_root_id" && closure.Reason != "input_changed_during_load" {
		t.Fatalf("closure reason = %q, want an explicit conflict reason", closure.Reason)
	}
	if len(closure.Roots) < 3 {
		t.Fatalf("roots = %#v, want distinct city and conflicting pack roots", closure.Roots)
	}
}

func TestQualificationCaptureRejectsInvalidLockedPackPin(t *testing.T) {
	cityRoot := t.TempDir()
	packRoot := filepath.Join(cityRoot, "pack")
	writeQualificationInput(t, filepath.Join(packRoot, "pack.toml"), "[pack]\nname='test'\n")
	capture := newQualificationCapture(fsys.OSFS{}, cityRoot)
	capture.recordPackRoot(packRoot, "https://example.test/pack.git", "abc123", "locked")

	closure := capture.closure()
	if closure.Status != qualification.StatusUnavailable || closure.Reason != "pack_pin_unbound" {
		t.Fatalf("closure = %#v, want unavailable with invalid locked pin", closure)
	}
}

func loadQualificationTestCity(t *testing.T, cityPath string, packIncludes ...string) qualification.Snapshot {
	t.Helper()
	cfg, provenance, err := LoadWithIncludesOptions(
		fsys.OSFS{}, cityPath, LoadOptions{CaptureQualificationInputs: true}, packIncludes...,
	)
	if err != nil {
		t.Fatalf("LoadWithIncludesOptions: %v", err)
	}
	return RefreshQualificationSnapshot(cfg, provenance)
}

func writeQualificationInput(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}
