package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/qualification"
)

func TestFormulaActionCheckerUsesCanonicalSourceAndRootStore(t *testing.T) {
	cityPath := t.TempDir()
	rigPath := filepath.Join(t.TempDir(), "worker")
	write := func(path, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(cityPath, "city.toml"), "[workspace]\nname = \"city-alpha\"\n")
	write(filepath.Join(cityPath, "pack.toml"), "[pack]\nname = \"city-root\"\nschema = 2\nincludes = [\"packs/required\"]\n")
	write(filepath.Join(cityPath, "packs", "required", "pack.toml"), "[pack]\nname = \"trusted-pack\"\nschema = 2\nrequires_gc = \">=0.14.0\"\n")
	formulaPath := filepath.Join(cityPath, "packs", "required", "formulas", "review.toml")
	write(formulaPath, "formula = \"review\"\n")
	cfg, _, err := config.LoadWithIncludesOptions(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"), config.LoadOptions{CaptureQualificationInputs: true})
	if err != nil {
		t.Fatalf("load compatibility config: %v", err)
	}
	cfg.Rigs = []config.Rig{{Name: "worker", Path: rigPath}}
	if !cfg.HasRequiredCompatibilityPacks() {
		t.Fatal("test config did not capture the required pack declaration")
	}

	cityStore := beads.NewMemStore()
	root, err := cityStore.Create(beads.Bead{Title: "formula root", Type: "workflow", Metadata: beads.StringMap{
		"gc.formula_source": formulaPath,
	}})
	if err != nil {
		t.Fatalf("create root: %v", err)
	}
	rigStore := beads.NewMemStore()
	child, err := rigStore.Create(beads.Bead{Title: "formula step", Metadata: beads.StringMap{
		"gc.root_bead_id":   root.ID,
		"gc.root_store_ref": "city:city-alpha",
	}})
	if err != nil {
		t.Fatalf("create child: %v", err)
	}
	ordinary, err := cityStore.Create(beads.Bead{Title: "ordinary routed work"})
	if err != nil {
		t.Fatalf("create ordinary bead: %v", err)
	}

	previousFactory := openStoreFactoryForCity
	openStoreFactoryForCity = func(_ context.Context, opts beads.StoreOpenOptions) (beads.StoreOpenResult, error) {
		switch filepath.Clean(opts.ScopeRoot) {
		case filepath.Clean(cityPath):
			return beads.StoreOpenResult{Store: cityStore}, nil
		case filepath.Clean(rigPath):
			return beads.StoreOpenResult{Store: rigStore}, nil
		default:
			t.Fatalf("unexpected store scope %q", opts.ScopeRoot)
			return beads.StoreOpenResult{}, nil
		}
	}
	t.Cleanup(func() { openStoreFactoryForCity = previousFactory })

	// An ordinary row remains ordinary even in a city that contains a required
	// pack. The checker resolves it canonically instead of relying on the query's
	// metadata projection.
	_, err = checkControllerFormulaAction(context.Background(), cityPath, cfg, beads.Bead{
		ID: ordinary.ID, Revision: -1, SourceStoreRef: "city:city-alpha",
	})
	if err == nil || !strings.Contains(err.Error(), "changed after discovery") {
		t.Fatalf("checking a negative revision mismatch = %v; want an exact mismatch refusal", err)
	}
	state, err := checkControllerFormulaAction(context.Background(), cityPath, cfg, beads.Bead{
		ID: ordinary.ID, SourceStoreRef: "city:city-alpha",
	})
	if err != nil {
		t.Fatalf("checking canonical ordinary bead: %v", err)
	}
	if state.Required || state.Bead.ID != ordinary.ID {
		t.Fatalf("ordinary formula action result = %+v, want canonical unguarded bead", state)
	}

	// The work-query projection intentionally omits every formula/auth marker.
	// The canonical child carries only root lineage, and that root is in another
	// store. Source resolution must find the required formula and refuse it for
	// missing per-bead approval metadata before a legacy claim can proceed.
	state, err = checkControllerFormulaAction(context.Background(), cityPath, cfg, beads.Bead{
		ID: child.ID, SourceStoreRef: "rig:worker",
	})
	if err == nil || !strings.Contains(err.Error(), "incomplete compatibility approval metadata") {
		t.Fatalf("checking metadata-stripped required candidate = %+v, %v; want approval refusal", state, err)
	}

	// Production custom-query discovery must pass the locator to the canonical
	// lookup even when its selected columns omit formula and authorization
	// metadata. Only the exact store and ID are needed to find canonical source
	// provenance; query metadata must not be a second authority source.
	checkCandidate := controllerFormulaActionCandidateCheck(cityPath, cfg, true)
	state, err = checkCandidate(context.Background(), beads.Bead{
		ID: ordinary.ID, SourceStoreRef: "city:city-alpha",
	})
	if err != nil || state.Required || state.Bead.ID != ordinary.ID {
		t.Fatalf("checking ordinary custom-query candidate = %+v, %v; want canonical ordinary bead", state, err)
	}
	state, err = checkCandidate(context.Background(), beads.Bead{
		ID: child.ID, SourceStoreRef: "rig:worker",
	})
	if err == nil || !strings.Contains(err.Error(), "incomplete compatibility approval metadata") {
		t.Fatalf("checking required custom-query candidate = %+v, %v; want canonical approval refusal", state, err)
	}
	state, err = checkCandidate(context.Background(), beads.Bead{ID: child.ID})
	if err == nil || !errors.Is(err, qualification.ErrUnavailable) || !strings.Contains(err.Error(), "no exact store reference") {
		t.Fatalf("checking custom-query candidate without store identity = %+v, %v; want unavailable", state, err)
	}
	state, err = checkCandidate(context.Background(), beads.Bead{
		ID: child.ID, SourceStoreRef: "rig:not-configured",
	})
	if err == nil || !errors.Is(err, qualification.ErrUnavailable) {
		t.Fatalf("checking custom-query candidate with unresolved store identity = %+v, %v; want unavailable", state, err)
	}
}
