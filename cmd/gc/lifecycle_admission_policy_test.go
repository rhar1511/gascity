package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/featureflags"
	"github.com/gastownhall/gascity/internal/formula"
	"github.com/gastownhall/gascity/internal/rollout"
	"github.com/gastownhall/gascity/internal/sling"
	"github.com/gastownhall/gascity/internal/worklifecycle"
)

func TestBuildLifecycleAdmissionPolicyUsesCurrentExactRouteAndCompilerClosure(t *testing.T) {
	fixture := newLifecycleAdmissionPolicyFixture(t, "formula = \"review\"\nversion = 1\n\n[[steps]]\nid = \"work\"\ntitle = \"Review work\"\n")
	got, err := fixture.build(t, fixture.receipt)
	if err != nil {
		t.Fatalf("buildLifecycleAdmissionPolicy: %v", err)
	}
	projection := got.projection
	if projection.Target.Identity != fixture.receipt.Route || projection.Workflow != fixture.receipt.Workflow {
		t.Fatalf("projection target/workflow = %q/%q, want exact %q/%q", projection.Target.Identity, projection.Workflow, fixture.receipt.Route, fixture.receipt.Workflow)
	}
	if projection.SourceScope != fixture.receipt.Scope || projection.MergeStrategy != fixture.receipt.MergeStrategy {
		t.Fatalf("projection scope/merge = %q/%q, want %q/%q", projection.SourceScope, projection.MergeStrategy, fixture.receipt.Scope, fixture.receipt.MergeStrategy)
	}
	if projection.FormulaSourceCount != 1 || len(projection.FormulaSources) != 1 || projection.FormulaSources[0].LogicalID != "review" || len(projection.FormulaSources[0].SHA256) != 64 {
		t.Fatalf("formula source closure = %+v count=%d, want stable review identity and hash", projection.FormulaSources, projection.FormulaSourceCount)
	}
	if projection.ExternalAssetCount != 0 || !projection.ExternalAssetClosureComplete || projection.CheckPathCount != 0 || !projection.CheckMappingsComplete {
		t.Fatalf("asset/check closure = assets=%+v checks=%+v, want complete empty closure", projection.ExternalAssets, projection.CheckClosures)
	}
	if projection.StorePlacement.SourceStoreRef != "city:pilot" || projection.StorePlacement.GraphStoreRef == "" || projection.StorePlacement.WorkflowStoreRef != "city:pilot" {
		t.Fatalf("store placement = %+v, want current source and graph refs", projection.StorePlacement)
	}
	if projection.FormulaCompilerCapability == "" || projection.FormulaCompilerImplementationVersion != formula.FormulaCompilerImplementationVersion || projection.FormulaProvenanceSchemaVersion != formula.CompileProvenanceSchemaVersion || projection.FormulaSchemaVersion != "formula.v1" {
		t.Fatalf("compiler/schema facts = %+v, want current compiler and formula.v1", projection)
	}
	if _, err := worklifecycle.DigestAdmissionPolicyV2(projection); err != nil {
		t.Fatalf("DigestAdmissionPolicyV2: %v", err)
	}
}

func TestBuildLifecycleAdmissionPolicyFailsClosedOnUnavailableEvidence(t *testing.T) {
	tests := []struct {
		name        string
		formula     string
		prepare     func(*testing.T, *lifecycleAdmissionPolicyFixture)
		wantMessage string
	}{
		{
			name: "description and check asset gaps",
			formula: `formula = "review"
version = 1

[requires]
formula_compiler = ">=2.0.0"

[[steps]]
id = "work"
title = "Review work"
description_file = "prompt.md"

[steps.check]
max_attempts = 2

[steps.check.check]
mode = "exec"
path = "scripts/check.sh"
`,
			prepare: func(t *testing.T, fixture *lifecycleAdmissionPolicyFixture) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(fixture.formulaDir, "prompt.md"), []byte("external description\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.MkdirAll(filepath.Join(fixture.formulaDir, "scripts"), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(fixture.formulaDir, "scripts", "check.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			wantMessage: "provenance is incomplete",
		},
		{
			name:    "exact route does not normalize aliases",
			formula: "formula = \"review\"\nversion = 1\n\n[[steps]]\nid = \"work\"\ntitle = \"Review work\"\n",
			prepare: func(_ *testing.T, fixture *lifecycleAdmissionPolicyFixture) {
				fixture.receipt.Route = "worker"
			},
			wantMessage: "target rig",
		},
		{
			name:        "graph workflow generated convoy",
			formula:     "formula = \"review\"\nversion = 2\ncontract = \"graph.v2\"\n\n[[steps]]\nid = \"work\"\ntitle = \"Review work\"\ntype = \"task\"\n",
			wantMessage: "generated input convoy",
		},
		{
			name:    "corrupt runtime suspension evidence",
			formula: "formula = \"review\"\nversion = 1\n\n[[steps]]\nid = \"work\"\ntitle = \"Review work\"\n",
			prepare: func(t *testing.T, fixture *lifecycleAdmissionPolicyFixture) {
				t.Helper()
				statePath := filepath.Join(fixture.cityPath, ".gc", "runtime", "suspension-state.json")
				if err := os.MkdirAll(filepath.Dir(statePath), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(statePath, []byte("{broken"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			wantMessage: "runtime suspension evidence",
		},
		{
			name:    "global FormulaV2 mode mismatch",
			formula: "formula = \"review\"\nversion = 1\n\n[[steps]]\nid = \"work\"\ntitle = \"Review work\"\n",
			prepare: func(_ *testing.T, fixture *lifecycleAdmissionPolicyFixture) {
				fixture.cfg.Daemon.FormulaV2 = boolPtr(false)
			},
			wantMessage: "global FormulaV2 mode",
		},
		{
			name:    "live branch invocation fallback",
			formula: "formula = \"mol-polecat-work\"\nversion = 1\n\n[[steps]]\nid = \"work\"\ntitle = \"Review work\"\n",
			prepare: func(t *testing.T, fixture *lifecycleAdmissionPolicyFixture) {
				t.Helper()
				formulaName := "mol-polecat-work"
				fixture.cfg.Agents[0].DefaultSlingFormula = &formulaName
				fixture.receipt.Workflow = formulaName
				if err := os.WriteFile(filepath.Join(fixture.formulaDir, formulaName+".toml"), []byte("formula = \"mol-polecat-work\"\nversion = 1\n\n[[steps]]\nid = \"work\"\ntitle = \"Review work\"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
			wantMessage: "base_branch would require",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newLifecycleAdmissionPolicyFixture(t, test.formula)
			if test.prepare != nil {
				test.prepare(t, fixture)
			}
			_, err := fixture.build(t, fixture.receipt)
			if err == nil || !strings.Contains(err.Error(), test.wantMessage) {
				t.Fatalf("buildLifecycleAdmissionPolicy error = %v, want message containing %q", err, test.wantMessage)
			}
		})
	}
}

func TestAdmissionFormulaSourceClosureIgnoresSliceOrderAndRejectsAmbiguity(t *testing.T) {
	recipe := []formula.SourceIdentity{
		{FormulaName: "review", ContentSHA256: strings.Repeat("a", 64)},
		{FormulaName: "parent", ContentSHA256: strings.Repeat("b", 64)},
	}
	provenance := []formula.SourceIdentity{recipe[1], recipe[0]}
	if _, err := admissionFormulaSourceClosure(recipe, provenance); err != nil {
		t.Fatalf("admissionFormulaSourceClosure(reordered): %v", err)
	}
	provenance[0].ContentSHA256 = strings.Repeat("c", 64)
	if _, err := admissionFormulaSourceClosure(recipe, provenance); err == nil {
		t.Fatal("admissionFormulaSourceClosure accepted a hash mismatch")
	}
	if _, err := admissionFormulaSourceClosure(append(recipe, recipe[0]), append(provenance, recipe[0])); err == nil {
		t.Fatal("admissionFormulaSourceClosure accepted a duplicate logical identity")
	}
}

type lifecycleAdmissionPolicyFixture struct {
	cfg        *config.City
	cityPath   string
	formulaDir string
	store      beads.Store
	leg        classStoreCandidate
	receipt    worklifecycle.AdmissionReceiptV2
}

func newLifecycleAdmissionPolicyFixture(t *testing.T, formulaText string) *lifecycleAdmissionPolicyFixture {
	t.Helper()
	t.Setenv("GC_FORMULA_REF", "")
	cityPath := t.TempDir()
	formulaDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(formulaDir, "review.toml"), []byte(formulaText), 0o600); err != nil {
		t.Fatal(err)
	}
	workflow := "review"
	minSessions := 1
	cfg := &config.City{
		Workspace:     config.Workspace{Name: "pilot"},
		FormulaLayers: config.FormulaLayers{City: []string{formulaDir}},
		Rigs:          []config.Rig{{Name: "pilot", Path: "pilot", Prefix: "pilot-rig"}},
		Agents: []config.Agent{{
			Name: "worker", Dir: "pilot", MinActiveSessions: &minSessions,
			DefaultSlingFormula: &workflow,
		}},
		Lifecycle: config.LifecycleConfig{AdmissionEnabled: true},
		Daemon:    config.DaemonConfig{FormulaV2: boolPtr(true)},
	}
	previous := featureflags.Snapshot()
	formulaV2 := rollout.ForTest(rollout.WithFormulaV2(cfg.Daemon.FormulaV2Enabled())).FormulaV2()
	featureflags.Apply(featureflags.Flags{FormulaV2: formulaV2, GraphApply: formulaV2})
	t.Cleanup(func() { featureflags.Apply(previous) })
	store := beads.NewMemStore()
	work, err := store.Create(beads.Bead{ID: "work-1", Title: "review", Type: "task", Status: "open", Metadata: map[string]string{beadmeta.RootStoreRefMetadataKey: "city:pilot"}})
	if err != nil {
		t.Fatal(err)
	}
	leg := classStoreCandidate{store: store, ref: "city:pilot"}
	scope := worklifecycle.ScopeForStore("pilot", "city:pilot")
	return &lifecycleAdmissionPolicyFixture{
		cfg: cfg, cityPath: cityPath, formulaDir: formulaDir, store: store, leg: leg,
		receipt: worklifecycle.AdmissionReceiptV2{
			Version: 2, WorkItemID: work.ID, Scope: scope, ExpectedWorkRevision: work.Revision,
			Route: "pilot/worker", Workflow: workflow, RoutingPolicyDigest: strings.Repeat("a", 64),
			MergeStrategy: "mr", Deliverable: "reviewed patch", Verification: "tests",
			AcceptanceAuthority: "reviewer", AdmittedBy: "triage",
		},
	}
}

func (f *lifecycleAdmissionPolicyFixture) build(t *testing.T, receipt worklifecycle.AdmissionReceiptV2) (lifecycleAdmissionPolicy, error) {
	t.Helper()
	bead, err := f.store.Get(receipt.WorkItemID)
	if err != nil {
		t.Fatal(err)
	}
	return buildLifecycleAdmissionPolicy(bead, receipt, receipt.Scope, "pilot", f.cityPath, f.cfg, f.store, nil, f.leg, []classStoreCandidate{f.leg}, sling.SlingRunner(shellSlingRunner), nil)
}
