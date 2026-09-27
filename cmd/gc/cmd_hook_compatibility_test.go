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
