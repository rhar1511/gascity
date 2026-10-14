package main

import (
	"context"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// Production bd list omits revision; authenticated Get/sourceSnapshot retain it.
// Keep the real CachingStore boundary instead of giving every read the live row.
type lifecycleRevisionlessListStore struct {
	*lifecycleAdmissionTransitionTestStore
}

func (s *lifecycleRevisionlessListStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	rows, err := s.lifecycleAdmissionTransitionTestStore.List(query)
	for i := range rows {
		rows[i].Revision = 0
	}
	return rows, err
}

func TestLifecycleCoreGraphClaimWithRevisionlessCache(t *testing.T) {
	setup, state, request := newLifecycleCoreGraphClaimProviderFixture(t)
	backing := &lifecycleRevisionlessListStore{setup.store}
	cache := beads.NewCachingStore(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatal(err)
	}
	cached, err := cache.Get(setup.source.ID)
	if err != nil || cached.Revision != 0 {
		t.Fatalf("fixture must reproduce revisionless list cache: revision=%d, err=%v", cached.Revision, err)
	}
	live, err := beads.HandlesFor(cache).Live.Get(setup.source.ID)
	if err != nil || live.Revision == 0 {
		t.Fatalf("live source must retain exact revision: revision=%d, err=%v", live.Revision, err)
	}
	setup.rigStores["pilot"] = cache
	state.beadStores = setup.rigStores
	request.WorkStore = cache
	before := len(setup.store.patchRequests)
	claim, err := state.ClaimLifecycleWork(context.Background(), request)
	if err != nil {
		t.Fatalf("claim real graph step through revisionless list cache: %v", err)
	}
	if claim.ClaimGeneration != "1" || claim.ReceiptID == "" || len(setup.store.patchRequests) != before+1 {
		t.Fatalf("unexpected claim: %+v", claim)
	}
	replay, err := state.ClaimLifecycleWork(context.Background(), request)
	if err != nil || !replay.Replayed || replay.ReceiptID != claim.ReceiptID || len(setup.store.patchRequests) != before+1 {
		t.Fatalf("exact claim retry: %+v, %v", replay, err)
	}
	step, err := setup.store.Get(request.Work.ID)
	if err != nil {
		t.Fatal(err)
	}
	source, err := setup.store.Get(setup.source.ID)
	if err != nil {
		t.Fatal(err)
	}
	request.ExecutionWork, request.ExecutionClaimGeneration = step, claim.ClaimGeneration
	request.Work, request.ExpectedRevision, request.ExpectedTransitionHead = source, source.Revision, source.Metadata[beadmeta.LifecycleTransitionHeadMetadataKey]
	sourceClaim, err := state.ClaimLifecycleWork(context.Background(), request)
	if err != nil {
		t.Fatalf("begin source through cache: %v", err)
	}
	writes := len(setup.store.patchRequests)
	repeated, err := state.ClaimLifecycleWork(context.Background(), request)
	if err != nil || !repeated.Replayed || repeated.ReceiptID != sourceClaim.ReceiptID || len(setup.store.patchRequests) != writes {
		t.Fatalf("exact source begin replay: %+v %v", repeated, err)
	}
}

func TestLifecycleAdmissionReplayWithRevisionlessCache(t *testing.T) {
	setup, _, _ := newLifecycleCoreGraphClaimProviderFixture(t)
	cache := beads.NewCachingStore(&lifecycleRevisionlessListStore{setup.store}, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatal(err)
	}
	setup.rigStores["pilot"] = cache
	before := len(setup.store.patchRequests)
	var stderr strings.Builder
	reconcileLifecycleAdmissionWithPermitResolver("pilot", setup.fixture.cityPath, setup.fixture.cfg, setup.cityStore, setup.rigStores, nil, &stderr, setup.permitResolver)
	if stderr.Len() != 0 || len(setup.store.patchRequests) != before {
		t.Fatalf("exact admission replay with cached list must neither refuse nor rewrite: %s", stderr.String())
	}
}

func TestLifecycleCompletionWithRevisionlessCache(t *testing.T) {
	setup, state, request := newLifecycleCompletionProviderFixture(t)
	cache := beads.NewCachingStore(&lifecycleRevisionlessListStore{setup.store}, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatal(err)
	}
	setup.rigStores["pilot"] = cache
	state.beadStores = setup.rigStores
	request.WorkStore = cache
	result, err := state.CompleteLifecycleWork(context.Background(), request)
	if err != nil {
		t.Fatalf("acceptance through cache: %v", err)
	}
	current, err := setup.store.Get(request.Work.ID)
	if err != nil || current.Status != "closed" || current.Revision != result.Revision {
		t.Fatalf("exact source close was not returned: %+v %v", result, err)
	}
	before := len(setup.store.patchRequests)
	replay, err := state.CompleteLifecycleWork(context.Background(), request)
	if err != nil || !replay.Replayed || replay.ReceiptID != result.ReceiptID || len(setup.store.patchRequests) != before {
		t.Fatalf("exact acceptance replay: %+v %v", replay, err)
	}
}
