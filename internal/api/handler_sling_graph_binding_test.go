package api

import (
	"context"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/compatibility"
	"github.com/gastownhall/gascity/internal/config"
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

func (*compatibilityAuthorityMarker) Resolve(context.Context, qualification.CompatibilityScope) (qualification.CompatibilityPolicy, error) {
	return qualification.CompatibilityPolicy{}, nil
}

func (*compatibilityAuthorityMarker) Authorize(context.Context, qualification.CompatibilityRequest) (qualification.ActionAuthorization, error) {
	return qualification.ActionAuthorization{}, nil
}

func (*compatibilityAuthorityMarker) Verify(context.Context, qualification.CompatibilityRequest, qualification.ActionAuthorization) error {
	return nil
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
	_, _, actionGate := server.graphFormulaCompatibility()
	gate, ok := actionGate.(compatibility.MaterializationGate)
	if !ok {
		t.Fatalf("graphFormulaCompatibility() gate type = %T, want compatibility.MaterializationGate", actionGate)
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
