package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/compatibility"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/formula"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/molecule"
	"github.com/gastownhall/gascity/internal/qualification"
	"github.com/gastownhall/gascity/internal/sourceworkflow"
)

type compatibilityIdentityState struct {
	*fakeState
	identity CompatibilityRuntimeIdentity
}

func (s *compatibilityIdentityState) CompatibilityRuntimeIdentity() (CompatibilityRuntimeIdentity, error) {
	return s.identity, nil
}

type compatibilityAuthorityMarker struct{ marker string }

func (a *compatibilityAuthorityMarker) Resolve(_ context.Context, scope qualification.CompatibilityScope) (qualification.CompatibilityPolicy, error) {
	scopeSHA, err := qualification.CompatibilityScopeIdentitySHA(scope)
	if err != nil {
		return qualification.CompatibilityPolicy{}, err
	}
	return qualification.CompatibilityPolicy{
		Status:               qualification.StatusAvailable,
		ScopeSHA256:          scopeSHA,
		PolicyReference:      "test-policy-" + a.marker,
		PolicyVersion:        "test-v1",
		RequiredCapabilities: []string{"test.graph.route"},
	}, nil
}

func (*compatibilityAuthorityMarker) Authorize(_ context.Context, request qualification.CompatibilityRequest) (qualification.ActionAuthorization, error) {
	return qualification.ActionAuthorization{
		Status:          qualification.StatusAuthorized,
		PolicyReference: request.Policy.PolicyReference,
		PolicyVersion:   request.Policy.PolicyVersion,
		RequestSHA256:   request.RequestSHA256,
		RecordID:        "test-record",
		Reference:       "test-reference",
	}, nil
}

func (*compatibilityAuthorityMarker) Verify(_ context.Context, request qualification.CompatibilityRequest, authorization qualification.ActionAuthorization) error {
	if authorization.Status != qualification.StatusAuthorized || authorization.RequestSHA256 != request.RequestSHA256 ||
		authorization.PolicyReference != request.Policy.PolicyReference || authorization.PolicyVersion != request.Policy.PolicyVersion {
		return qualification.ErrUnavailable
	}
	return nil
}

type compatibilityRouteProver struct{}

func (compatibilityRouteProver) Prove(_ context.Context, scope qualification.CompatibilityScope, policy qualification.CompatibilityPolicy, capability string) (qualification.CapabilityProof, error) {
	scopeSHA, err := qualification.CompatibilityScopeIdentitySHA(scope)
	if err != nil || policy.Status != qualification.StatusAvailable || policy.ScopeSHA256 != scopeSHA || capability != "test.graph.route" {
		return qualification.CapabilityProof{}, qualification.ErrUnavailable
	}
	return qualification.CapabilityProof{
		Status:          qualification.StatusAvailable,
		Capability:      capability,
		ScopeSHA256:     scopeSHA,
		PolicyReference: policy.PolicyReference,
		PolicyVersion:   policy.PolicyVersion,
		EvidenceSHA256:  strings.Repeat("c", 64),
	}, nil
}

// TestGraphFormulaCompatibilityReadsOnePublishedIdentityPerCheck proves that
// the API callback carries the host authority in the same generation as the
// config, snapshot, and build used by later materialization checks.
func TestGraphFormulaCompatibilityReadsOnePublishedIdentityPerCheck(t *testing.T) {
	base := newFakeState(t)
	base.cityBeadStore = beads.NewMemStore()
	firstConfig := base.cfg
	secondConfig := &config.City{Workspace: config.Workspace{Name: "reloaded-city"}}
	firstAuthority := &compatibilityAuthorityMarker{marker: "first"}
	secondAuthority := &compatibilityAuthorityMarker{marker: "second"}
	state := &compatibilityIdentityState{
		fakeState: base,
		identity: CompatibilityRuntimeIdentity{
			Config: firstConfig,
			Snapshot: qualification.Snapshot{
				EffectiveConfigIdentitySHA256: "first-config-generation",
			},
			Build:     qualification.BuildIdentity{BuildID: "first-build-generation"},
			Authority: firstAuthority,
		},
	}
	server := &Server{state: state}
	route := server.graphFormulaCompatibility()
	gate, ok := route.gate.(compatibility.MaterializationGate)
	if !ok {
		t.Fatalf("graphFormulaCompatibility() gate type = %T, want compatibility.MaterializationGate", route.gate)
	}
	if gate.Gate.Authority != firstAuthority {
		t.Fatal("initial gate did not capture the authority from the published identity")
	}

	state.identity = CompatibilityRuntimeIdentity{
		Config: secondConfig,
		Snapshot: qualification.Snapshot{
			EffectiveConfigIdentitySHA256: "second-config-generation",
		},
		Build:     qualification.BuildIdentity{BuildID: "second-build-generation"},
		Authority: secondAuthority,
	}
	cfg, snapshot, build, authority, err := gate.Current()
	if err != nil {
		t.Fatalf("Current() error = %v", err)
	}
	if cfg != secondConfig || snapshot.EffectiveConfigIdentitySHA256 != "second-config-generation" ||
		build.BuildID != "second-build-generation" || authority != secondAuthority {
		t.Fatalf("Current() mixed or stale generation: cfg=%p snapshot=%+v build=%+v authority=%p", cfg, snapshot, build, authority)
	}
}

// TestGraphFormulaCompatibilityRejectsStoreReplacementWithSameStoreRef pins
// the actual store route across materialization. Replacing a graph handle does
// not necessarily change its class StoreRef, so a ref-only comparison cannot
// detect a stale captured route.
func TestGraphFormulaCompatibilityRejectsStoreReplacementWithSameStoreRef(t *testing.T) {
	cfg, recipe, snapshot := requiredGraphCompatibilityFixture(t)
	stateBase := newFakeState(t)
	stateBase.cityName = cfg.Workspace.Name
	stateBase.cityPath = t.TempDir()
	stateBase.cityBeadStore = beads.NewMemStore()
	oldStore := beads.NewMemStore()
	newStore := beads.NewMemStore()
	stateBase.graphBeadStore = oldStore
	authority := &compatibilityAuthorityMarker{marker: "route"}
	state := &compatibilityIdentityState{
		fakeState: stateBase,
		identity: CompatibilityRuntimeIdentity{
			Config:    cfg,
			Snapshot:  snapshot,
			Build:     routeTestBuildIdentity(),
			Authority: authority,
		},
	}
	server := &Server{state: state}
	route := server.graphFormulaCompatibility()
	capturedStoreRef := route.storeRef
	gate, ok := route.gate.(compatibility.MaterializationGate)
	if !ok {
		t.Fatalf("graphFormulaCompatibility() gate type = %T, want compatibility.MaterializationGate", route.gate)
	}
	gate.Gate.ProverForStore = func(beads.Store, string) qualification.CapabilityProver {
		return compatibilityRouteProver{}
	}
	if capturedStoreRef == "" {
		t.Fatal("graphFormulaCompatibility() returned an empty initial StoreRef")
	}
	if authorization, err := gate.AuthorizeRecipe(context.Background(), recipe, oldStore); err != nil || !authorization.Required {
		t.Fatalf("same-route authorization = %+v, %v; want a successful required authorization", authorization, err)
	}

	stateBase.graphBeadStore = newStore
	currentStoreRef := server.graphFormulaCompatibility().storeRef
	if currentStoreRef != capturedStoreRef {
		t.Fatalf("test replacement changed StoreRef from %q to %q; want to exercise same-ref handle replacement", capturedStoreRef, currentStoreRef)
	}
	_, err := molecule.Instantiate(context.Background(), newStore, recipe, molecule.Options{
		ActionGate:        gate,
		RequireActionGate: true,
	})
	if err == nil {
		t.Fatal("materialization succeeded after the captured graph store handle was replaced")
	}
	for label, store := range map[string]beads.Store{"old graph store": oldStore, "replacement graph store": newStore} {
		open, listErr := store.ListOpen()
		if listErr != nil {
			t.Fatalf("ListOpen(%s): %v", label, listErr)
		}
		if len(open) != 0 {
			t.Errorf("%s received %d beads after stale-route refusal: %#v", label, len(open), open)
		}
	}
}

func TestGraphFormulaCompatibilityRequiresPublishedRouteForRequiredPacks(t *testing.T) {
	cfg, _, snapshot := requiredGraphCompatibilityFixture(t)
	if !cfg.HasRequiredCompatibilityPacks() {
		t.Fatal("test fixture does not contain a required compatibility pack")
	}
	base := newFakeState(t)
	base.cityName = cfg.Workspace.Name
	base.cityPath = t.TempDir()
	base.cityBeadStore = beads.NewMemStore()
	base.graphBeadStore = beads.NewMemStore()
	state := &compatibilityIdentityState{
		fakeState: base,
		identity: CompatibilityRuntimeIdentity{
			Config:    cfg,
			Snapshot:  snapshot,
			Build:     routeTestBuildIdentity(),
			Authority: &compatibilityAuthorityMarker{marker: "unpublished-route"},
		},
	}
	route := (&Server{state: state}).graphFormulaCompatibility()
	if route.acquireLease == nil {
		t.Fatal("required-pack route without a published route identity exposed no refusal callback")
	}
	if _, err := route.acquireLease(); !errors.Is(err, qualification.ErrUnavailable) {
		t.Fatalf("required-pack route acquisition error = %v, want unavailable", err)
	}
}

func TestExecSlingLegacyStateSupportsPlainAndOrdinaryFormulaActions(t *testing.T) {
	t.Run("plain bead", func(t *testing.T) {
		server, state := newLegacyExecSlingServer(t)
		store := state.stores["myrig"]
		bead, err := store.Create(beads.Bead{Title: "plain work", Type: "task"})
		if err != nil {
			t.Fatalf("Create(plain work): %v", err)
		}

		resp, status, code, message, _ := server.execSling(context.Background(), slingBody{
			Target: "myrig/worker",
			Bead:   bead.ID,
		}, "")
		if status != http.StatusOK || code != "" || resp == nil {
			t.Fatalf("execSling(plain) = (%+v, %d, %q, %q), want successful response", resp, status, code, message)
		}
		routed, err := store.Get(bead.ID)
		if err != nil {
			t.Fatalf("Get(plain work): %v", err)
		}
		if got := routed.Metadata["gc.routed_to"]; got != "myrig/worker" {
			t.Fatalf("plain bead gc.routed_to = %q, want myrig/worker", got)
		}
	})

	t.Run("ordinary formula", func(t *testing.T) {
		server, state := newLegacyExecSlingServer(t)
		formulaDir := t.TempDir()
		state.cfg.FormulaLayers.City = []string{formulaDir}
		writeTestFormula(t, formulaDir, "ordinary-review", `
formula = "ordinary-review"

[[steps]]
id = "review"
title = "Review the work"
`)

		resp, status, code, message, _ := server.execSling(context.Background(), slingBody{
			Target:  "myrig/worker",
			Formula: "ordinary-review",
		}, "")
		if status != http.StatusOK || code != "" || resp == nil {
			t.Fatalf("execSling(formula) = (%+v, %d, %q, %q), want successful response", resp, status, code, message)
		}
		if resp.RootBeadID == "" || resp.Formula != "ordinary-review" {
			t.Fatalf("execSling(formula) response = %+v, want a materialized ordinary formula root", resp)
		}
	})
}

func newLegacyExecSlingServer(t *testing.T) (*Server, *fakeMutatorState) {
	t.Helper()
	state := newFakeMutatorState(t)
	state.cfg.Rigs[0].Prefix = "gc"
	state.cityBeadStore = state.stores["myrig"]
	return New(state), state
}

func requiredGraphCompatibilityFixture(t *testing.T) (*config.City, *formula.Recipe, qualification.Snapshot) {
	t.Helper()
	cityDir := t.TempDir()
	t.Setenv("GC_HOME", filepath.Join(t.TempDir(), "gc-home"))
	write := func(path, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(cityDir, "city.toml"), "[workspace]\nname = \"city-route\"\n")
	write(filepath.Join(cityDir, "pack.toml"), "[pack]\nname = \"city-root\"\nschema = 2\nincludes = [\"packs/required\"]\n")
	write(filepath.Join(cityDir, "packs", "required", "pack.toml"), "[pack]\nname = \"route-pack\"\nschema = 2\nrequires_gc = \">=0.14.0\"\n")
	formulaPath := filepath.Join(cityDir, "packs", "required", "formulas", "review.toml")
	formulaBody := "formula = \"review\"\n"
	write(formulaPath, formulaBody)
	cfg, provenance, err := config.LoadWithIncludesOptions(fsys.OSFS{}, filepath.Join(cityDir, "city.toml"), config.LoadOptions{CaptureQualificationInputs: true})
	if err != nil {
		t.Fatalf("load required graph compatibility config: %v", err)
	}
	recipe := &formula.Recipe{
		Name:          "review",
		FormulaSource: formulaPath,
		ContentHash:   compatibilityRouteDigest([]byte(formulaBody)),
		FormulaSources: []formula.SourceIdentity{{
			Path:          formulaPath,
			ContentSHA256: compatibilityRouteDigest([]byte(formulaBody)),
		}},
		Steps: []formula.RecipeStep{{ID: "review.root", Title: "review item", Type: "task", IsRoot: true}},
	}
	return cfg, recipe, config.RefreshQualificationSnapshot(cfg, provenance)
}

func routeTestBuildIdentity() qualification.BuildIdentity {
	return qualification.BuildIdentity{
		Status:         qualification.StatusAvailable,
		SourceRevision: strings.Repeat("1", 40),
		BuildID:        strings.Repeat("1", 40),
		Version:        "0.14.1",
		ArtifactStatus: qualification.StatusAvailable,
		ArtifactSHA256: strings.Repeat("b", 64),
	}
}

func compatibilityRouteDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// TestSourceWorkflowStoresLeadsWithRelocatedGraphStore pins the leg order and
// the strict policy the split city depends on, at the enumerator itself.
func TestSourceWorkflowStoresLeadsWithRelocatedGraphStore(t *testing.T) {
	state := newFakeState(t)
	state.cityName = "bright-lights"
	state.cityBeadStore = beads.NewMemStore()
	graph := beads.NewMemStore()
	state.graphBeadStore = graph
	state.stores = map[string]beads.Store{"alpha": beads.NewMemStore()}
	s := &Server{state: state}

	stores := s.sourceWorkflowStores()
	if len(stores) != 3 {
		t.Fatalf("sourceWorkflowStores() returned %d entries, want graph + city + rig", len(stores))
	}
	if stores[0].Store != graph {
		t.Fatalf("stores[0].Store = %p, want the relocated graph store %p (graph-first)", stores[0].Store, graph)
	}
	if stores[0].StoreRef != sourceworkflow.GraphStoreRef("bright-lights") {
		t.Fatalf("stores[0].StoreRef = %q, want %q", stores[0].StoreRef, sourceworkflow.GraphStoreRef("bright-lights"))
	}
	if !stores[0].Strict {
		t.Fatal("the graph leg is not strict; a fault on the store that holds the answer would degrade to a warning")
	}
	if stores[1].StoreRef != "city:bright-lights" || stores[2].StoreRef != "rig:alpha" {
		t.Fatalf("work legs = %q, %q; want city:bright-lights then rig:alpha", stores[1].StoreRef, stores[2].StoreRef)
	}
	for _, info := range stores[1:] {
		if info.Strict {
			t.Fatalf("work leg %q is strict; only the selected source store and the graph binding are", info.StoreRef)
		}
	}
}

// TestSourceWorkflowStoresOmitsGraphLegOnSingleStoreCity is decision (4): where
// the graph class is not relocated the graph store IS the work store, so adding
// it would scan one store twice. The single-store enumeration stays
// byte-identical to what it was before the graph leg existed.
func TestSourceWorkflowStoresOmitsGraphLegOnSingleStoreCity(t *testing.T) {
	state := newFakeState(t)
	state.cityName = "bright-lights"
	state.cityBeadStore = beads.NewMemStore()
	state.stores = map[string]beads.Store{"alpha": beads.NewMemStore()}
	s := &Server{state: state}

	if state.GraphBeadStore().Store != state.CityBeadStore() {
		t.Fatal("fixture is not a single-store city")
	}
	stores := s.sourceWorkflowStores()
	if len(stores) != 2 {
		t.Fatalf("sourceWorkflowStores() returned %d entries, want exactly the city and rig work legs", len(stores))
	}
	for _, info := range stores {
		if strings.HasPrefix(info.StoreRef, sourceworkflow.GraphStoreRefPrefix+":") {
			t.Fatalf("single-store city enumerated a graph leg %q; the work store would be scanned twice", info.StoreRef)
		}
		if info.Strict {
			t.Fatalf("work leg %q is strict on a single-store city", info.StoreRef)
		}
	}
}
