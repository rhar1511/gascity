package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/storeref"
	"github.com/gastownhall/gascity/internal/worklifecycle"
)

func TestLifecycleClaimProviderRechecksCanonicalReadiness(t *testing.T) {
	for _, tc := range []struct {
		name    string
		prepare func(t *testing.T, setup lifecycleAdmissionTransitionSetup, store *lifecycleClaimReadinessTestStore)
	}{
		{
			name: "dispatch hold",
			prepare: func(_ *testing.T, _ lifecycleAdmissionTransitionSetup, store *lifecycleClaimReadinessTestStore) {
				store.overlay = func(current beads.Bead) beads.Bead {
					current.Labels = append(current.Labels, beadmeta.DispatchHoldLabels[0])
					return current
				}
			},
		},
		{
			name: "deferred source",
			prepare: func(_ *testing.T, _ lifecycleAdmissionTransitionSetup, store *lifecycleClaimReadinessTestStore) {
				deferUntil := time.Now().UTC().Add(time.Hour)
				store.overlay = func(current beads.Bead) beads.Bead {
					current.DeferUntil = &deferUntil
					return current
				}
			},
		},
		{
			name: "blocked dependency",
			prepare: func(t *testing.T, setup lifecycleAdmissionTransitionSetup, _ *lifecycleClaimReadinessTestStore) {
				seedLifecycleClaimBlocker(t, setup)
			},
		},
		{
			name: "dependency blocks after proof and before live readiness read",
			prepare: func(t *testing.T, setup lifecycleAdmissionTransitionSetup, store *lifecycleClaimReadinessTestStore) {
				store.beforeReady = func() error {
					seedLifecycleClaimBlocker(t, setup)
					return nil
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setup, state, request := newLifecycleClaimProviderFixture(t)
			readinessStore := &lifecycleClaimReadinessTestStore{lifecycleAdmissionTransitionTestStore: setup.store}
			tc.prepare(t, setup, readinessStore)
			setup.rigStores["pilot"] = readinessStore
			state.beadStores = setup.rigStores
			request.WorkStore = readinessStore

			beforeRequests := len(setup.store.patchRequests)
			beforeReceipts := len(setup.store.patchReceipts)
			beforeGenericWrites := setup.store.sourceGenericWrites
			if _, err := state.ClaimLifecycleWork(context.Background(), request); err == nil {
				t.Fatal("provider claimed work that was no longer canonically ready")
			} else if !errors.Is(err, worklifecycle.ErrTransitionChainStale) {
				t.Fatalf("claim error = %v, want a stale-readiness refusal", err)
			}
			if len(setup.store.patchRequests) != beforeRequests || len(setup.store.patchReceipts) != beforeReceipts || setup.store.sourceGenericWrites != beforeGenericWrites {
				t.Fatalf("readiness refusal wrote source state: patches=%d/%d receipts=%d/%d generic=%d/%d",
					len(setup.store.patchRequests), beforeRequests, len(setup.store.patchReceipts), beforeReceipts,
					setup.store.sourceGenericWrites, beforeGenericWrites)
			}
			current, err := setup.store.Get(request.Work.ID)
			if err != nil {
				t.Fatal(err)
			}
			if current.Status != "open" || current.Assignee != "" {
				t.Fatalf("readiness refusal changed source owner/status to %q/%q", current.Status, current.Assignee)
			}
			if tc.name == "dependency blocks after proof and before live readiness read" && !readinessStore.readinessRaceRan {
				t.Fatal("test did not change dependency readiness at the final live-read boundary")
			}
		})
	}
}

func TestLifecycleClaimProviderClaimsReadyWorkAndReplaysExactClaim(t *testing.T) {
	setup, state, request := newLifecycleClaimProviderFixture(t)
	beforeRequests := len(setup.store.patchRequests)
	claimed, err := state.ClaimLifecycleWork(context.Background(), request)
	if err != nil {
		t.Fatalf("claim currently ready admitted work: %v", err)
	}
	if claimed.ClaimGeneration != "1" || claimed.ReceiptID == "" || claimed.Replayed || claimed.Recovered {
		t.Fatalf("claim result = %+v, want a fresh generation-1 transition", claimed)
	}
	current, err := setup.store.Get(request.Work.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Status != "in_progress" || current.Assignee != request.Actor || len(setup.store.patchRequests) != beforeRequests+1 {
		t.Fatalf("source after claim = %q/%q with %d transition writes, want claimed once", current.Status, current.Assignee, len(setup.store.patchRequests)-beforeRequests)
	}

	replayed, err := state.ClaimLifecycleWork(context.Background(), request)
	if err != nil {
		t.Fatalf("replay exact claim after a lost response: %v", err)
	}
	if !replayed.Replayed || replayed.ReceiptID != claimed.ReceiptID || replayed.ClaimGeneration != claimed.ClaimGeneration || len(setup.store.patchRequests) != beforeRequests+1 {
		t.Fatalf("claim replay = %+v with %d transition writes, want the original receipt without another write", replayed, len(setup.store.patchRequests)-beforeRequests)
	}
}

type lifecycleClaimReadinessTestStore struct {
	*lifecycleAdmissionTransitionTestStore
	overlay          func(beads.Bead) beads.Bead
	beforeReady      func() error
	readinessRaceRan bool
}

func (s *lifecycleClaimReadinessTestStore) Get(id string) (beads.Bead, error) {
	current, err := s.lifecycleAdmissionTransitionTestStore.Get(id)
	if err == nil && s.overlay != nil {
		current = s.overlay(current)
	}
	return current, err
}

func (s *lifecycleClaimReadinessTestStore) Ready(queries ...beads.ReadyQuery) ([]beads.Bead, error) {
	if s.beforeReady != nil {
		beforeReady := s.beforeReady
		s.beforeReady = nil
		s.readinessRaceRan = true
		if err := beforeReady(); err != nil {
			return nil, err
		}
	}
	return s.lifecycleAdmissionTransitionTestStore.Ready(queries...)
}

func newLifecycleClaimProviderFixture(t *testing.T) (lifecycleAdmissionTransitionSetup, *controllerState, api.LifecycleClaimTransitionRequest) {
	t.Helper()
	setup := newLifecycleAdmissionTransitionSetup(t)
	var stderr strings.Builder
	reconcileLifecycleAdmissionWithPermitResolver("pilot", setup.fixture.cityPath, setup.fixture.cfg, setup.cityStore, setup.rigStores, nil, &stderr, setup.permitResolver)
	current, err := setup.store.Get(setup.source.ID)
	if err != nil {
		t.Fatalf("read admitted lifecycle source: %v", err)
	}
	marker, ok := lifecycleMaterializationFor(current)
	if !ok || marker.State != "attached" || strings.TrimSpace(current.Metadata[beadmeta.LifecycleTransitionHeadMetadataKey]) == "" {
		t.Fatalf("lifecycle source was not attached before claim test: marker=%+v stderr=%s", marker, stderr.String())
	}
	state := &controllerState{
		cfg: setup.fixture.cfg, cityName: "pilot", cityPath: setup.fixture.cityPath,
		cityBeadStore: setup.cityStore, beadStores: setup.rigStores,
		beadsPermitResolver: setup.permitResolver, graphStoreGeneration: 1,
	}
	info := session.Info{ID: "claim-session", Generation: "1", InstanceToken: "instance-token"}
	actor := session.AssigneeIdentifier(info)
	request := api.LifecycleClaimTransitionRequest{
		Work: current, WorkStore: setup.store, WorkStoreRef: storeref.RigRef("pilot"),
		Scope:   worklifecycle.ScopeForStore("pilot", string(storeref.RigRef("pilot"))),
		Session: info, Actor: actor, ExpectedRevision: current.Revision,
		ExpectedTransitionHead: current.Metadata[beadmeta.LifecycleTransitionHeadMetadataKey],
	}
	return setup, state, request
}

func seedLifecycleClaimBlocker(t *testing.T, setup lifecycleAdmissionTransitionSetup) {
	t.Helper()
	blocker, err := setup.store.MemStore.Create(beads.Bead{
		ID: "pilot-rig-claim-blocker", Title: "open dependency", Type: "task", Status: "open",
	})
	if err != nil {
		t.Fatalf("create open claim blocker: %v", err)
	}
	if err := setup.store.MemStore.DepAdd(setup.source.ID, blocker.ID, "blocks"); err != nil {
		t.Fatalf("block admitted source: %v", err)
	}
}
