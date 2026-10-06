package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

func TestHookClaimFormulaCompatibilityDenialPreventsClaim(t *testing.T) {
	candidate := beads.Bead{
		ID:       "work-1",
		Status:   "open",
		Metadata: beads.StringMap{beadmeta.RoutedToMetadataKey: "worker", beadmeta.FormulaSourceMetadataKey: "/packs/required/formulas/work.formula.toml"},
	}
	raw, err := json.Marshal([]beads.Bead{candidate})
	if err != nil {
		t.Fatal(err)
	}
	claimCalls := 0
	var stdout, stderr bytes.Buffer
	check := func(_ context.Context, current beads.Bead) (formulaActionCandidate, error) {
		if current.ID != candidate.ID {
			t.Fatalf("checked bead id = %q, want %q", current.ID, candidate.ID)
		}
		return formulaActionCandidate{Bead: current}, errors.New("release decision unavailable")
	}
	opts := hookClaimOptions{
		Assignee:           "worker",
		IdentityCandidates: []string{"worker"},
		RouteTargets:       []string{"worker"},
		CheckFormulaAction: check,
	}
	ops := hookClaimOps{
		Runner: func(string, string) (string, error) { return string(raw), nil },
		Claim: func(_ context.Context, _ string, _ []string, _, _ string) (beads.Bead, bool, error) {
			claimCalls++
			return candidate, true, nil
		},
	}
	result := tryHookClaim("gc ready", ".", &opts, &ops, &stdout, &stderr)
	if !result.terminal || result.code == 0 {
		t.Fatalf("claim result = %+v, want terminal refusal", result)
	}
	if claimCalls != 0 {
		t.Fatalf("claim writes = %d, want none", claimCalls)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout exposed candidate: %q", stdout.String())
	}
}

func TestHookClaimCustomQueryUsesCanonicalFormulaBeforeClaim(t *testing.T) {
	cityPath := t.TempDir()
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
	formulaPath := filepath.Join(cityPath, "packs", "required", "formulas", "work.toml")
	write(formulaPath, "formula = \"work\"\n")
	cfg, _, err := config.LoadWithIncludesOptions(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"), config.LoadOptions{CaptureQualificationInputs: true})
	if err != nil {
		t.Fatalf("load compatibility config: %v", err)
	}
	if !cfg.HasRequiredCompatibilityPacks() {
		t.Fatal("test config did not capture the required pack declaration")
	}
	store := beads.NewMemStore()
	canonical, err := store.Create(beads.Bead{Title: "required formula work", Status: "open", Metadata: beads.StringMap{
		beadmeta.FormulaSourceMetadataKey: formulaPath,
	}})
	if err != nil {
		t.Fatalf("create canonical bead: %v", err)
	}
	previousFactory := openStoreFactoryForCity
	openStoreFactoryForCity = func(_ context.Context, opts beads.StoreOpenOptions) (beads.StoreOpenResult, error) {
		if filepath.Clean(opts.ScopeRoot) != filepath.Clean(cityPath) {
			t.Fatalf("unexpected store scope %q", opts.ScopeRoot)
		}
		return beads.StoreOpenResult{Store: store}, nil
	}
	t.Cleanup(func() { openStoreFactoryForCity = previousFactory })

	// This is the actual work-query projection: the exact source store survives
	// as a locator, while canonical formula provenance and approval metadata
	// are omitted. SourceStoreRef is transient on beads.Bead, so model the wire
	// field explicitly instead of marshaling the store-only value.
	queryRow := beads.Bead{
		ID: canonical.ID, Status: "open", SourceStoreRef: "city:city-alpha",
		Metadata: beads.StringMap{beadmeta.RoutedToMetadataKey: "worker"},
	}
	raw, err := json.Marshal([]struct {
		beads.Bead
		SourceStoreRef string `json:"source_store_ref"`
	}{{Bead: queryRow, SourceStoreRef: queryRow.SourceStoreRef}})
	if err != nil {
		t.Fatal(err)
	}
	claimCalls := 0
	var stdout, stderr bytes.Buffer
	opts := hookClaimOptions{
		Assignee:           "worker",
		IdentityCandidates: []string{"worker"},
		RouteTargets:       []string{"worker"},
		CheckFormulaAction: controllerFormulaActionCandidateCheck(cityPath, cfg, true),
	}
	ops := hookClaimOps{
		Runner: func(string, string) (string, error) { return string(raw), nil },
		Claim: func(_ context.Context, _ string, _ []string, _, _ string) (beads.Bead, bool, error) {
			claimCalls++
			return queryRow, true, nil
		},
	}
	result := tryHookClaim("gc ready", ".", &opts, &ops, &stdout, &stderr)
	if !result.terminal || result.code == 0 {
		t.Fatalf("claim result = %+v, want terminal compatibility refusal", result)
	}
	if claimCalls != 0 {
		t.Fatalf("claim writes = %d, want none", claimCalls)
	}
	if !strings.Contains(stderr.String(), "incomplete compatibility approval metadata") {
		t.Fatalf("stderr = %q, want canonical approval refusal", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout exposed candidate: %q", stdout.String())
	}
}

func newSignedFormulaActionCandidate(t *testing.T) (*beads.MemStore, formulaActionCandidate, hookClaimOptions) {
	t.Helper()

	mem := beads.NewMemStore()
	created, err := mem.Create(beads.Bead{Title: "formula action", Status: "open"})
	if err != nil {
		t.Fatal(err)
	}
	signed := &lifecycleSignedRevisionStore{MemStore: mem}
	current, err := signed.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Revision >= 0 {
		t.Fatalf("signed revision = %d, want negative token", current.Revision)
	}
	check := func(_ context.Context, candidate beads.Bead) (formulaActionCandidate, error) {
		fresh, err := signed.Get(candidate.ID)
		if err != nil {
			return formulaActionCandidate{}, err
		}
		fresh.SourceStoreRef = candidate.SourceStoreRef
		return formulaActionCandidate{Store: signed, Bead: fresh, Required: true}, nil
	}
	state := formulaActionCandidate{Store: signed, Bead: current, Required: true}
	opts := hookClaimOptions{
		IdentityCandidates: []string{"worker"},
		CheckFormulaAction: check,
	}
	return mem, state, opts
}

func TestConditionalFormulaActionClaimUsesSignedRevisionCAS(t *testing.T) {
	t.Run("negative revision is an exact CAS token", func(t *testing.T) {
		mem, state, opts := newSignedFormulaActionCandidate(t)
		if state.Bead.Revision != -1 {
			t.Fatalf("initial signed revision = %d, want -1", state.Bead.Revision)
		}

		claimed, ok, err := conditionalFormulaActionClaim(context.Background(), state, "worker", false, opts)
		if err != nil {
			t.Fatalf("conditional claim: %v", err)
		}
		if !ok || claimed.ID != state.Bead.ID || claimed.Revision != -2 {
			t.Fatalf("claim result = %+v, ok=%v; want claimed revision -2", claimed, ok)
		}
		stored, err := mem.Get(state.Bead.ID)
		if err != nil {
			t.Fatal(err)
		}
		if stored.Status != "in_progress" || stored.Assignee != "worker" || stored.Revision != 2 {
			t.Fatalf("stored bead = %+v, want in_progress/worker at revision 2", stored)
		}
	})

	t.Run("stale negative revision fails CAS without claiming", func(t *testing.T) {
		mem, state, opts := newSignedFormulaActionCandidate(t)
		staleToken := state.Bead.Revision
		title := "concurrent edit"
		if err := mem.Update(state.Bead.ID, beads.UpdateOpts{Title: &title}); err != nil {
			t.Fatal(err)
		}

		claimed, ok, err := conditionalFormulaActionClaim(context.Background(), state, "worker", false, opts)
		var precondition *beads.PreconditionFailedError
		if !errors.As(err, &precondition) {
			t.Fatalf("conditional claim error = %v, want stale-token precondition failure", err)
		}
		if precondition.Expected != -staleToken || precondition.Current != -staleToken+1 {
			t.Fatalf("precondition = %+v, want expected %d and current %d", precondition, -staleToken, -staleToken+1)
		}
		if ok || claimed.ID != "" {
			t.Fatalf("stale claim result = %+v, ok=%v; want no claim", claimed, ok)
		}
		stored, getErr := mem.Get(state.Bead.ID)
		if getErr != nil {
			t.Fatal(getErr)
		}
		if stored.Status != "open" || stored.Assignee != "" || stored.Revision != -staleToken+1 {
			t.Fatalf("stale claim changed bead: %+v", stored)
		}
	})

	t.Run("zero revision remains unavailable", func(t *testing.T) {
		mem, state, opts := newSignedFormulaActionCandidate(t)
		state.Bead.Revision = 0

		claimed, ok, err := conditionalFormulaActionClaim(context.Background(), state, "worker", false, opts)
		if err != nil || ok || claimed.ID != "" {
			t.Fatalf("zero-token claim = %+v, ok=%v, err=%v; want unavailable without error", claimed, ok, err)
		}
		stored, getErr := mem.Get(state.Bead.ID)
		if getErr != nil {
			t.Fatal(getErr)
		}
		if stored.Status != "open" || stored.Assignee != "" || stored.Revision != 1 {
			t.Fatalf("zero-token claim changed bead: %+v", stored)
		}
	})
}

func TestConditionalFormulaActionAssignUsesSignedRevisionCAS(t *testing.T) {
	t.Run("negative revision is an exact CAS token", func(t *testing.T) {
		mem, state, opts := newSignedFormulaActionCandidate(t)
		if state.Bead.Revision != -1 {
			t.Fatalf("initial signed revision = %d, want -1", state.Bead.Revision)
		}

		if err := conditionalFormulaActionAssign(context.Background(), state, "worker", opts); err != nil {
			t.Fatalf("conditional assignment: %v", err)
		}
		stored, err := mem.Get(state.Bead.ID)
		if err != nil {
			t.Fatal(err)
		}
		if stored.Status != "open" || stored.Assignee != "worker" || stored.Revision != 2 {
			t.Fatalf("stored bead = %+v, want open/worker at revision 2", stored)
		}
	})

	t.Run("stale negative revision fails CAS without assignment", func(t *testing.T) {
		mem, state, opts := newSignedFormulaActionCandidate(t)
		staleToken := state.Bead.Revision
		title := "concurrent edit"
		if err := mem.Update(state.Bead.ID, beads.UpdateOpts{Title: &title}); err != nil {
			t.Fatal(err)
		}

		err := conditionalFormulaActionAssign(context.Background(), state, "worker", opts)
		var precondition *beads.PreconditionFailedError
		if !errors.As(err, &precondition) {
			t.Fatalf("conditional assignment error = %v, want stale-token precondition failure", err)
		}
		if precondition.Expected != -staleToken || precondition.Current != -staleToken+1 {
			t.Fatalf("precondition = %+v, want expected %d and current %d", precondition, -staleToken, -staleToken+1)
		}
		stored, getErr := mem.Get(state.Bead.ID)
		if getErr != nil {
			t.Fatal(getErr)
		}
		if stored.Status != "open" || stored.Assignee != "" || stored.Revision != -staleToken+1 {
			t.Fatalf("stale assignment changed bead: %+v", stored)
		}
	})

	t.Run("zero revision remains unavailable", func(t *testing.T) {
		mem, state, opts := newSignedFormulaActionCandidate(t)
		state.Bead.Revision = 0

		if err := conditionalFormulaActionAssign(context.Background(), state, "worker", opts); !errors.Is(err, beads.ErrConditionalWriteUnsupported) {
			t.Fatalf("zero-token assignment error = %v, want conditional write unavailable", err)
		}
		stored, getErr := mem.Get(state.Bead.ID)
		if getErr != nil {
			t.Fatal(getErr)
		}
		if stored.Status != "open" || stored.Assignee != "" || stored.Revision != 1 {
			t.Fatalf("zero-token assignment changed bead: %+v", stored)
		}
	})
}
