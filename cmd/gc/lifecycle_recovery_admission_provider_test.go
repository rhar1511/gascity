package main

import (
	"context"
	"io"
	"testing"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/storeref"
)

func TestLifecycleRecoveryAdmissionProviderRequiresQ43AndCurrentPolicy(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mutate  func(*testing.T, *lifecycleAdmissionTransitionSetup)
		wantErr bool
	}{
		{name: "exact attachment and current policy", wantErr: false},
		{
			name: "missing Q43 receipt",
			mutate: func(t *testing.T, setup *lifecycleAdmissionTransitionSetup) {
				t.Helper()
				setup.store.mu.Lock()
				setup.store.q43Receipts = map[string]beads.ControllerMetadataTransitionReceipt{}
				setup.store.mu.Unlock()
			},
			wantErr: true,
		},
		{
			name: "changed current formula policy",
			mutate: func(t *testing.T, setup *lifecycleAdmissionTransitionSetup) {
				t.Helper()
				workflow := "different-review"
				setup.fixture.cfg.Agents[0].DefaultSlingFormula = &workflow
			},
			wantErr: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setup := newLifecycleAdmissionTransitionSetup(t)
			reconcileLifecycleAdmissionWithPermitResolver(
				"pilot", setup.fixture.cityPath, setup.fixture.cfg, setup.cityStore, setup.rigStores, nil,
				io.Discard, setup.permitResolver,
			)
			work, err := setup.store.Get(setup.source.ID)
			if err != nil {
				t.Fatal(err)
			}
			marker, attached := lifecycleMaterializationFor(work)
			if !attached || marker.State != "attached" {
				t.Fatalf("test fixture did not materialize an attached workflow: %+v", marker)
			}
			if tc.mutate != nil {
				tc.mutate(t, &setup)
			}
			cs := &controllerState{
				cfg: setup.fixture.cfg, cityName: "pilot", cityPath: setup.fixture.cityPath,
				cityBeadStore: setup.cityStore, beadStores: setup.rigStores,
				beadsPermitResolver: setup.permitResolver, graphStoreGeneration: 1,
			}
			request := api.LifecycleRecoveryAdmissionRequest{
				Work: work, WorkStore: setup.store, WorkStoreRef: storeref.StoreRef("rig:pilot"),
				Scope: setup.receipt.Scope, ExpectedRevision: work.Revision,
			}
			err = cs.VerifyLifecycleRecoveryAdmission(context.Background(), request)
			if (err != nil) != tc.wantErr {
				t.Fatalf("VerifyLifecycleRecoveryAdmission() error = %v, wantErr %t", err, tc.wantErr)
			}
		})
	}
}
