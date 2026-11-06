package api

import (
	"context"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
)

func TestLifecycleClaimAuthenticatesCurrentIncarnationDespiteCachedPreviousEpoch(t *testing.T) {
	state, backing, previous, work := newLifecycleClaimTestState(t)
	cache := beads.NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatal(err)
	}
	state.sessionsBeadStore = cache
	state.cityBeadStore = cache
	if err := backing.SetMetadataBatch(previous.ID, map[string]string{
		"generation": "2", "instance_token": "new-incarnation-token",
	}); err != nil {
		t.Fatal(err)
	}
	current, err := session.NewStore(beads.SessionStore{Store: backing}).Get(previous.ID)
	if err != nil {
		t.Fatal(err)
	}
	called := false
	state.claim = func(_ context.Context, request LifecycleClaimTransitionRequest) (LifecycleClaimTransitionResult, error) {
		called = true
		if request.Session.Generation != current.Generation || request.Session.InstanceToken != current.InstanceToken {
			t.Fatal("provider received the cached previous incarnation")
		}
		return LifecycleClaimTransitionResult{ClaimGeneration: "1", ReceiptID: "current-claim"}, nil
	}
	if _, err := (&Server{state: state}).humaHandleLifecycleClaimSubmit(context.Background(), lifecycleClaimInput(current, work)); err != nil {
		t.Fatalf("current incarnation refused because cached previous epoch remains: %v", err)
	}
	if !called {
		t.Fatal("current incarnation did not reach the transition provider")
	}
	claim, generation, err := session.NewStore(beads.SessionStore{Store: backing}).CurrentClaim(current.ID)
	if err != nil || claim != work.ID || generation != "1" {
		t.Fatalf("live reciprocal claim = %q/%q, %v; want %q/1", claim, generation, err, work.ID)
	}
}

func TestLifecycleSourceClaimUsesAuthoritativeReciprocalClaim(t *testing.T) {
	for _, tc := range []struct {
		name              string
		cachedGeneration  string
		liveGeneration    string
		wantProvider      bool
		changeDuringClaim bool
	}{
		{name: "new execution claim", liveGeneration: "3", wantProvider: true},
		{name: "renewed execution claim", cachedGeneration: "2", liveGeneration: "3", wantProvider: true},
		{name: "superseded request", cachedGeneration: "3", liveGeneration: "4"},
		{name: "cleared execution claim", cachedGeneration: "3"},
		{name: "claim changes during source transition", cachedGeneration: "3", liveGeneration: "3", wantProvider: true, changeDuringClaim: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state, store, info, work := newLifecycleClaimTestState(t)
			step := beads.Bead{ID: "test-step", Revision: 9, Title: "execution step", Status: "in_progress", Assignee: session.AssigneeIdentifier(info), Metadata: map[string]string{
				beadmeta.SessionIDMetadataKey:                info.ID,
				beadmeta.ClaimGenerationMetadataKey:          "3",
				beadmeta.LifecycleTransitionHeadMetadataKey:  "step-claim-head",
				beadmeta.LifecycleMaterializationMetadataKey: "provider-verifies-lineage",
			}}
			row, err := store.Get(info.ID)
			if err != nil {
				t.Fatal(err)
			}
			row.Metadata[beadmeta.CurrentClaimBeadIDMetadataKey] = step.ID
			row.Metadata[beadmeta.CurrentClaimGenerationMetadataKey] = tc.cachedGeneration
			backing := beads.NewMemStoreFrom(1, []beads.Bead{work, step, row}, nil)
			cache := beads.NewCachingStoreForTest(backing, nil)
			if err := cache.Prime(context.Background()); err != nil {
				t.Fatal(err)
			}
			state.sessionsBeadStore, state.cityBeadStore = cache, cache
			liveClaimID := step.ID
			if tc.liveGeneration == "" {
				liveClaimID = ""
			}
			if err := backing.SetMetadataBatch(info.ID, map[string]string{
				beadmeta.CurrentClaimBeadIDMetadataKey:     liveClaimID,
				beadmeta.CurrentClaimGenerationMetadataKey: tc.liveGeneration,
			}); err != nil {
				t.Fatal(err)
			}
			called := false
			state.claim = func(_ context.Context, request LifecycleClaimTransitionRequest) (LifecycleClaimTransitionResult, error) {
				called = true
				if request.ExecutionWork.ID != step.ID || request.ExecutionClaimGeneration != "3" {
					t.Fatal("current execution claim did not reach controller")
				}
				if tc.changeDuringClaim {
					if err := backing.SetMetadataBatch(info.ID, map[string]string{beadmeta.CurrentClaimGenerationMetadataKey: "4"}); err != nil {
						t.Fatal(err)
					}
				}
				return LifecycleClaimTransitionResult{ClaimGeneration: "1", ReceiptID: "source-claim-head"}, nil
			}
			request := lifecycleClaimInput(info, work)
			request.Body.ExecutionClaim = &LifecycleExecutionClaim{WorkID: step.ID, ClaimGeneration: "3"}
			_, err = (&Server{state: state}).humaHandleLifecycleClaimSubmit(context.Background(), request)
			if called != tc.wantProvider || (err == nil) != (tc.wantProvider && !tc.changeDuringClaim) {
				t.Fatalf("source claim: provider=%v error=%v; want provider=%v", called, err, tc.wantProvider)
			}
			live := session.NewStore(beads.SessionStore{Store: backing})
			claimID, generation, err := live.CurrentClaim(info.ID)
			wantGeneration := tc.liveGeneration
			if tc.changeDuringClaim {
				wantGeneration = "4"
			}
			if err != nil || claimID != liveClaimID || generation != wantGeneration {
				t.Fatalf("source claim changed execution stamp: %q/%q error=%v", claimID, generation, err)
			}
		})
	}
}

func TestLifecycleClaimRejectsPreviousIncarnationDespiteMatchingCache(t *testing.T) {
	state, backing, previous, work := newLifecycleClaimTestState(t)
	cache := beads.NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatal(err)
	}
	state.sessionsBeadStore, state.cityBeadStore = cache, cache
	if err := backing.SetMetadataBatch(previous.ID, map[string]string{"generation": "2", "instance_token": "new-incarnation-token"}); err != nil {
		t.Fatal(err)
	}
	called := false
	state.claim = func(context.Context, LifecycleClaimTransitionRequest) (LifecycleClaimTransitionResult, error) {
		called = true
		return LifecycleClaimTransitionResult{}, nil
	}
	if _, err := (&Server{state: state}).humaHandleLifecycleClaimSubmit(context.Background(), lifecycleClaimInput(previous, work)); err == nil {
		t.Fatal("previous incarnation authenticated from stale cache")
	}
	if called {
		t.Fatal("stale incarnation reached transition provider")
	}
	claim, _, err := session.NewStore(beads.SessionStore{Store: backing}).CurrentClaim(previous.ID)
	if err != nil || claim != "" {
		t.Fatalf("stale request changed live reciprocal claim: %q, %v", claim, err)
	}
}
