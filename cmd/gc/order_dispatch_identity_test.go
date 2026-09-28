package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/qualification"
)

func TestOrderDispatchIdentityRegistryConcurrentAddRemove(t *testing.T) {
	const generation = "execution-1"
	registry := newOrderDispatchIdentityRegistryWithGeneration(generation)
	const workers = 8
	const iterations = 64
	var wg sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		worker := worker
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				runID := fmt.Sprintf("run-%d-%d", worker, i)
				lease := registry.begin("daily", runID, "formula_root")
				if !lease.registered {
					t.Errorf("begin(%q) did not register", runID)
					return
				}
				lease.bindWork("root-" + runID)
				snapshot := registry.SnapshotForGeneration(generation)
				if snapshot.Availability.Status == qualification.StatusAvailable && snapshot.StartFence != snapshot.EndFence {
					t.Errorf("available snapshot has unequal fences: %+v", snapshot)
				}
				lease.complete()
			}
		}()
	}
	wg.Wait()

	snapshot := registry.SnapshotForGeneration(generation)
	if snapshot.Availability.Status != qualification.StatusAvailable {
		t.Fatalf("final snapshot unavailable: %+v", snapshot)
	}
	if len(snapshot.Identities) != 0 {
		t.Fatalf("final snapshot retained %d identities, want none", len(snapshot.Identities))
	}
	if snapshot.StartFence != snapshot.EndFence {
		t.Fatalf("stable snapshot fences differ: %+v", snapshot)
	}
}

func TestOrderDispatchIdentityRegistryRejectsDuplicateIdentity(t *testing.T) {
	registry := newOrderDispatchIdentityRegistryWithGeneration("execution-duplicate")
	first := registry.begin("daily", "run-1", "formula_root")
	if !first.registered {
		t.Fatal("first dispatch identity did not register")
	}
	first.bindWork("root-1")
	duplicate := registry.begin("daily", "run-1", "formula_root")
	if duplicate.registered {
		t.Fatal("duplicate dispatch identity registered")
	}

	snapshot := registry.SnapshotForGeneration("execution-duplicate")
	if snapshot.Availability.Status != qualification.StatusUnavailable || snapshot.Availability.Reason != "duplicate_dispatch_identity" {
		t.Fatalf("duplicate identity snapshot = %+v, want unavailable duplicate reason", snapshot)
	}
	first.complete()
	if snapshot = registry.SnapshotForGeneration("execution-duplicate"); len(snapshot.Identities) != 0 {
		t.Fatalf("completed duplicate fixture retained identities: %+v", snapshot.Identities)
	}
	if snapshot.Availability.Status != qualification.StatusUnavailable {
		t.Fatalf("duplicate evidence was lost after completion: %+v", snapshot)
	}
}

func TestOrderDispatchIdentitySnapshotSortsAndMarksUnknownWork(t *testing.T) {
	const generation = "execution-sorted"
	registry := newOrderDispatchIdentityRegistryWithGeneration(generation)
	zulu := registry.begin("z-order", "run-z", "formula_root")
	zulu.bindWork("root-z")
	alpha := registry.begin("a-order", "run-a", "formula_root")
	alpha.bindWork("root-a")
	snapshot := registry.SnapshotForGeneration(generation)
	if snapshot.Availability.Status != qualification.StatusAvailable {
		t.Fatalf("known work identities unavailable: %+v", snapshot)
	}
	if len(snapshot.Identities) != 2 || snapshot.Identities[0].ScopedOrder != "a-order" || snapshot.Identities[1].ScopedOrder != "z-order" {
		t.Fatalf("identities are not deterministically ordered: %+v", snapshot.Identities)
	}
	zulu.complete()
	alpha.complete()

	execLease := registry.begin("script-order", "run-exec", "exec")
	execSnapshot := registry.SnapshotForGeneration(generation)
	if execSnapshot.Availability.Status != qualification.StatusUnavailable || len(execSnapshot.Identities) != 1 {
		t.Fatalf("exec work identity should be explicitly unavailable: %+v", execSnapshot)
	}
	if execSnapshot.Identities[0].WorkIdentityAvailability.Status != qualification.StatusUnavailable || execSnapshot.Identities[0].WorkIdentityAvailability.Reason != "exec_has_no_canonical_work_identity" {
		t.Fatalf("exec work identity availability = %+v", execSnapshot.Identities[0].WorkIdentityAvailability)
	}
	execLease.complete()
}

func TestOrderDispatchIdentityRegistryFencesRestartGeneration(t *testing.T) {
	oldRegistry := newOrderDispatchIdentityRegistryWithGeneration("execution-old")
	oldLease := oldRegistry.begin("daily", "run-1", "formula_root")
	oldLease.bindWork("root-1")

	newRegistry := newOrderDispatchIdentityRegistryWithGeneration("execution-new")
	newLease := newRegistry.begin("daily", "run-1", "formula_root")
	newLease.bindWork("root-2")
	if oldRegistry.generation == newRegistry.generation {
		t.Fatal("restart reused the controller execution generation")
	}

	mismatch := newRegistry.SnapshotForGeneration("execution-old")
	if mismatch.Availability.Status != qualification.StatusUnavailable || mismatch.Availability.Reason != "controller_execution_generation_mismatch" {
		t.Fatalf("old generation snapshot = %+v, want generation mismatch", mismatch)
	}
	newRegistry.complete(oldLease)
	if len(newRegistry.SnapshotForGeneration("execution-new").Identities) != 1 {
		t.Fatal("stale completion removed an identity from the new execution")
	}
	newLease.complete()
}

func TestOrderDispatchIdentitySnapshotDetectsConcurrentMutation(t *testing.T) {
	const generation = "execution-race"
	registry := newOrderDispatchIdentityRegistryWithGeneration(generation)
	first := registry.begin("daily", "run-1", "formula_root")
	first.bindWork("root-1")
	mutate := func() {
		second := registry.begin("daily", "run-2", "formula_root")
		second.bindWork("root-2")
		second.complete()
	}

	snapshot := registry.snapshotForGeneration(generation, mutate)
	if snapshot.Availability.Status != qualification.StatusUnavailable || snapshot.Availability.Reason != "in_flight_identity_snapshot_raced" {
		t.Fatalf("raced snapshot = %+v, want explicit snapshot-raced status", snapshot)
	}
	if snapshot.StartFence == snapshot.EndFence {
		t.Fatalf("raced snapshot has matching fences: %+v", snapshot)
	}
	first.complete()
}

func TestOrderDispatchIdentitySerializationOmitsSensitiveInputs(t *testing.T) {
	registry := newOrderDispatchIdentityRegistryWithGeneration("execution-private")
	lease := registry.begin("nightly", "run-1", "formula_root")
	lease.bindWork("root-1")
	snapshot := registry.SnapshotForGeneration("execution-private")
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatalf("marshal identity snapshot: %v", err)
	}
	serialized := string(encoded)
	for _, forbidden := range []string{"command", "environment", "source_path", "city_path", "/tmp/private", "SECRET_VALUE"} {
		if strings.Contains(serialized, forbidden) {
			t.Fatalf("identity snapshot leaked %q: %s", forbidden, serialized)
		}
	}
	for _, field := range []string{"scoped_order", "run_id", "work_id", "execution_generation", "start_fence", "end_fence"} {
		if !strings.Contains(serialized, `"`+field+`"`) {
			t.Errorf("identity snapshot omitted %q: %s", field, serialized)
		}
	}
	lease.complete()
}
