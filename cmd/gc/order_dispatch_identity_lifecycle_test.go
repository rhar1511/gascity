package main

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/orders"
	"github.com/gastownhall/gascity/internal/qualification"
)

type blockingOrderRunStore struct {
	beads.Store

	createEntered     chan struct{}
	createRelease     chan struct{}
	createErr         error
	createPanic       any
	closeEntered      chan struct{}
	closeRelease      chan struct{}
	createOnce        sync.Once
	closeOnce         sync.Once
	createReleaseOnce sync.Once
	closeReleaseOnce  sync.Once
}

func (store *blockingOrderRunStore) Create(bead beads.Bead) (beads.Bead, error) {
	if hasLabel(bead.Labels, labelOrderTracking) {
		store.createOnce.Do(func() { close(store.createEntered) })
		<-store.createRelease
		if store.createPanic != nil {
			panic(store.createPanic)
		}
		if store.createErr != nil {
			return beads.Bead{}, store.createErr
		}
	}
	return store.Store.Create(bead)
}

func (store *blockingOrderRunStore) CloseAll(ids []string, metadata map[string]string) (int, error) {
	if store.closeEntered != nil {
		store.closeOnce.Do(func() { close(store.closeEntered) })
		<-store.closeRelease
	}
	return store.Store.CloseAll(ids, metadata)
}

func (store *blockingOrderRunStore) releaseCreate() {
	store.createReleaseOnce.Do(func() { close(store.createRelease) })
}

func (store *blockingOrderRunStore) releaseClose() {
	if store.closeRelease != nil {
		store.closeReleaseOnce.Do(func() { close(store.closeRelease) })
	}
}

func (store *blockingOrderRunStore) release() {
	store.releaseCreate()
	store.releaseClose()
}

func recoverLaunchPanic(fn func()) (panicValue any) {
	defer func() { panicValue = recover() }()
	fn()
	return nil
}

func TestLaunchResolvedDispatchReservesIdentityBeforeCreateRun(t *testing.T) {
	cityPath, cfg, order := newExecOrderFixture(t)
	workStore := beads.NewMemStore()
	store := &blockingOrderRunStore{
		Store:         beads.NewMemStore(),
		createEntered: make(chan struct{}),
		createRelease: make(chan struct{}),
		closeEntered:  make(chan struct{}),
		closeRelease:  make(chan struct{}),
	}
	t.Cleanup(store.release)
	m := newSplitOrderDispatcher(t, cityPath, cfg, nil, workStore, store, events.Discard)
	registry := newOrderDispatchIdentityRegistryWithGeneration("execution-blocked-create")
	m.inflightIdentityRegistry = registry
	target := execStoreTarget{ScopeRoot: cityPath, ScopeKind: "city"}

	type launchResult struct {
		run orders.OrderRun
		err error
	}
	resultCh := make(chan launchResult, 1)
	go func() {
		run, err := m.launchResolvedDispatch(context.Background(), workStore, target, order, cityPath, nil, nil, nil)
		resultCh <- launchResult{run: run, err: err}
	}()

	waitCtx, waitCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer waitCancel()
	select {
	case <-store.createEntered:
	case <-waitCtx.Done():
		t.Fatal("CreateRun did not reach the blocked store Create")
	}
	blocked := registry.SnapshotForGeneration("execution-blocked-create")
	if blocked.Availability.Status != qualification.StatusUnavailable || blocked.Availability.Reason != "dispatch_identity_start_pending" || len(blocked.Identities) != 0 {
		t.Fatalf("snapshot while CreateRun is blocked = %+v, want unavailable pending start with no identities", blocked)
	}

	store.releaseCreate()
	var launched launchResult
	select {
	case launched = <-resultCh:
	case <-waitCtx.Done():
		t.Fatal("launchResolvedDispatch did not return after CreateRun was released")
	}
	if launched.err != nil {
		t.Fatalf("launchResolvedDispatch: %v", launched.err)
	}
	select {
	case <-store.closeEntered:
	case <-waitCtx.Done():
		t.Fatal("dispatch did not reach the tracking close barrier")
	}
	promoted := registry.SnapshotForGeneration("execution-blocked-create")
	if promoted.Availability.Status != qualification.StatusUnavailable || promoted.Availability.Reason != "exact_work_identity_unavailable" {
		t.Fatalf("snapshot after promotion = %+v, want exact run identity with unavailable exec work", promoted)
	}
	if len(promoted.Identities) != 1 || promoted.Identities[0].RunID != launched.run.ID || promoted.Identities[0].ScopedOrder != order.ScopedName() {
		t.Fatalf("snapshot after promotion = %+v, want the created order run", promoted.Identities)
	}

	store.releaseClose()
	drainCtx, drainCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer drainCancel()
	if !m.drain(drainCtx) {
		t.Fatal("dispatch did not drain after tracking close was released")
	}
	finished := registry.SnapshotForGeneration("execution-blocked-create")
	if finished.Availability.Status != qualification.StatusAvailable || len(finished.Identities) != 0 {
		t.Fatalf("snapshot after dispatch completion = %+v, want available empty", finished)
	}
}

func TestLaunchResolvedDispatchCancelsIdentityReservationOnCreateFailure(t *testing.T) {
	cityPath, cfg, order := newExecOrderFixture(t)
	wantErr := errors.New("tracking store unavailable")
	store := &blockingOrderRunStore{
		Store:         beads.NewMemStore(),
		createEntered: make(chan struct{}),
		createRelease: make(chan struct{}),
		createErr:     wantErr,
	}
	t.Cleanup(store.release)
	workStore := beads.NewMemStore()
	m := newSplitOrderDispatcher(t, cityPath, cfg, nil, workStore, store, events.Discard)
	registry := newOrderDispatchIdentityRegistryWithGeneration("execution-create-failure")
	m.inflightIdentityRegistry = registry
	target := execStoreTarget{ScopeRoot: cityPath, ScopeKind: "city"}

	type launchResult struct {
		run orders.OrderRun
		err error
	}
	resultCh := make(chan launchResult, 1)
	go func() {
		run, err := m.launchResolvedDispatch(context.Background(), workStore, target, order, cityPath, nil, nil, nil)
		resultCh <- launchResult{run: run, err: err}
	}()

	waitCtx, waitCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer waitCancel()
	select {
	case <-store.createEntered:
	case <-waitCtx.Done():
		t.Fatal("CreateRun did not reach the blocked store Create")
	}
	blocked := registry.SnapshotForGeneration("execution-create-failure")
	if blocked.Availability.Status != qualification.StatusUnavailable || blocked.Availability.Reason != "dispatch_identity_start_pending" {
		t.Fatalf("snapshot while CreateRun is blocked = %+v, want unavailable pending start", blocked)
	}
	store.releaseCreate()
	var launched launchResult
	select {
	case launched = <-resultCh:
	case <-waitCtx.Done():
		t.Fatal("launchResolvedDispatch did not return after CreateRun failure")
	}
	if !errors.Is(launched.err, wantErr) {
		t.Fatalf("launchResolvedDispatch error = %v, want wrapped %v", launched.err, wantErr)
	}
	afterFailure := registry.SnapshotForGeneration("execution-create-failure")
	if afterFailure.Availability.Status != qualification.StatusAvailable || len(afterFailure.Identities) != 0 {
		t.Fatalf("snapshot after CreateRun failure = %+v, want available empty after reservation cleanup", afterFailure)
	}
}

func TestLaunchResolvedDispatchCancelsIdentityReservationOnCreatePanic(t *testing.T) {
	cityPath, cfg, order := newExecOrderFixture(t)
	store := &blockingOrderRunStore{
		Store:         beads.NewMemStore(),
		createEntered: make(chan struct{}),
		createRelease: make(chan struct{}),
		createPanic:   "CreateRun panic",
	}
	store.releaseCreate()
	t.Cleanup(store.release)
	workStore := beads.NewMemStore()
	m := newSplitOrderDispatcher(t, cityPath, cfg, nil, workStore, store, events.Discard)
	registry := newOrderDispatchIdentityRegistryWithGeneration("execution-create-panic")
	m.inflightIdentityRegistry = registry
	target := execStoreTarget{ScopeRoot: cityPath, ScopeKind: "city"}

	gotPanic := recoverLaunchPanic(func() {
		_, _ = m.launchResolvedDispatch(context.Background(), workStore, target, order, cityPath, nil, nil, nil)
	})
	if gotPanic != "CreateRun panic" {
		t.Fatalf("launch panic = %v, want CreateRun panic to propagate", gotPanic)
	}
	snapshot := registry.SnapshotForGeneration("execution-create-panic")
	if snapshot.Availability.Status != qualification.StatusAvailable || len(snapshot.Identities) != 0 {
		t.Fatalf("snapshot after CreateRun panic = %+v, want available empty after reservation cleanup", snapshot)
	}
	if !m.drain(context.Background()) {
		t.Fatal("CreateRun panic leaked the dispatch in-flight count")
	}
}

func TestLaunchResolvedDispatchCancelsIdentityReservationOnOrderFrontDoorPanic(t *testing.T) {
	cityPath, cfg, order := newExecOrderFixture(t)
	workStore := beads.NewMemStore()
	orderStore := beads.NewMemStore()
	m := newSplitOrderDispatcher(t, cityPath, cfg, nil, workStore, orderStore, events.Discard)
	registry := newOrderDispatchIdentityRegistryWithGeneration("execution-front-door-panic")
	m.inflightIdentityRegistry = registry
	m.orderFrontDoorForFn = func(beads.Store) *orders.Store { panic("order front door panic") }
	target := execStoreTarget{ScopeRoot: cityPath, ScopeKind: "city"}

	gotPanic := recoverLaunchPanic(func() {
		_, _ = m.launchResolvedDispatch(context.Background(), workStore, target, order, cityPath, nil, nil, nil)
	})
	if gotPanic != "order front door panic" {
		t.Fatalf("launch panic = %v, want order front door panic to propagate", gotPanic)
	}
	snapshot := registry.SnapshotForGeneration("execution-front-door-panic")
	if snapshot.Availability.Status != qualification.StatusAvailable || len(snapshot.Identities) != 0 {
		t.Fatalf("snapshot after order front door panic = %+v, want available empty after reservation cleanup", snapshot)
	}
	if !m.drain(context.Background()) {
		t.Fatal("order front door panic leaked the dispatch in-flight count")
	}
}

func TestLaunchResolvedDispatchCompletesLeaseOnPreLaunchPanic(t *testing.T) {
	cityPath, cfg, order := newExecOrderFixture(t)
	workStore := beads.NewMemStore()
	orderStore := beads.NewMemStore()
	m := newSplitOrderDispatcher(t, cityPath, cfg, nil, workStore, orderStore, events.Discard)
	registry := newOrderDispatchIdentityRegistryWithGeneration("execution-pre-launch-panic")
	m.inflightIdentityRegistry = registry
	m.afterIdentityPromotion = func() { panic("pre-launch panic") }
	target := execStoreTarget{ScopeRoot: cityPath, ScopeKind: "city"}

	gotPanic := recoverLaunchPanic(func() {
		_, _ = m.launchResolvedDispatch(context.Background(), workStore, target, order, cityPath, nil, nil, nil)
	})
	if gotPanic != "pre-launch panic" {
		t.Fatalf("launch panic = %v, want pre-launch panic to propagate", gotPanic)
	}
	snapshot := registry.SnapshotForGeneration("execution-pre-launch-panic")
	if snapshot.Availability.Status != qualification.StatusAvailable || len(snapshot.Identities) != 0 {
		t.Fatalf("snapshot after pre-launch panic = %+v, want available empty after lease cleanup", snapshot)
	}
	if !m.drain(context.Background()) {
		t.Fatal("pre-launch panic leaked the dispatch in-flight count")
	}
}
