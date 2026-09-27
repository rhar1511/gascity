package beads

import (
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
)

func TestRecoveryIntentIsExcludedAndImmutableThroughMemStore(t *testing.T) {
	store := &MemStore{HonorExplicitIDs: true}
	intent, err := store.Create(Bead{
		ID: "gc-lri-test", Title: "signed recovery request", Type: "lifecycle-intent",
		Labels: []string{"hold:external"}, Metadata: StringMap{
			"gc.lifecycle.recovery_intent.v1":        `{"request_id":"r1"}`,
			"gc.lifecycle.recovery_intent_digest.v1": "digest",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ready, err := store.Ready()
	if err != nil {
		t.Fatal(err)
	}
	if len(ready) != 0 {
		t.Fatalf("Ready() = %+v, want no recovery intent", ready)
	}
	if err := store.Update(intent.ID, UpdateOpts{Title: stringPointer("rewritten")}); !errors.Is(err, ErrLifecycleIntentImmutable) {
		t.Fatalf("Update error = %v, want immutable refusal", err)
	}
	if err := store.SetMetadata(intent.ID, "gc.lifecycle.recovery_intent.v1", ""); !errors.Is(err, ErrLifecycleIntentImmutable) {
		t.Fatalf("SetMetadata error = %v, want immutable refusal", err)
	}
	if err := store.Close(intent.ID); !errors.Is(err, ErrLifecycleIntentImmutable) {
		t.Fatalf("Close error = %v, want immutable refusal", err)
	}
	if err := store.Delete(intent.ID); !errors.Is(err, ErrLifecycleIntentImmutable) {
		t.Fatalf("Delete error = %v, want immutable refusal", err)
	}
	if err := store.UpdateIfMatch(intent.ID, intent.Revision, UpdateOpts{Title: stringPointer("rewritten")}); !errors.Is(err, ErrLifecycleIntentImmutable) {
		t.Fatalf("UpdateIfMatch error = %v, want immutable refusal", err)
	}
	if err := store.CloseIfMatch(intent.ID, intent.Revision); !errors.Is(err, ErrLifecycleIntentImmutable) {
		t.Fatalf("CloseIfMatch error = %v, want immutable refusal", err)
	}
	if err := store.DeleteIfMatch(intent.ID, intent.Revision); !errors.Is(err, ErrLifecycleIntentImmutable) {
		t.Fatalf("DeleteIfMatch error = %v, want immutable refusal", err)
	}
	if swapped, err := store.CompareAndSetMetadataKey(intent.ID, "gc.lifecycle.recovery_intent.v1", `{"request_id":"r1"}`, "{}"); swapped || !errors.Is(err, ErrLifecycleIntentImmutable) {
		t.Fatalf("CompareAndSetMetadataKey = (%v, %v), want immutable refusal", swapped, err)
	}
}

func TestRecoveryBudgetCannotBeResetByGenericUpdate(t *testing.T) {
	const workID = "gc-work-budget"
	initial := `{"version":1,"work_item_id":"gc-work-budget","scope":"city:pilot/city:pilot","attempts":[{"id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","reserved_at":"2026-09-27T12:00:00Z","request_id":"request-1","request_digest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","expected_revision":1}]}`
	reset := `{"version":1,"work_item_id":"gc-work-budget","scope":"city:pilot/city:pilot","attempts":[]}`
	mem := &MemStore{HonorExplicitIDs: true}
	work, err := createRecoveryBudgetFixture(t, mem, workID, initial)
	if err != nil {
		t.Fatal(err)
	}
	assertGenericRecoveryBudgetWritesBlocked(t, mem, work, initial, reset)

	dir := t.TempDir()
	sqlite, err := OpenSQLiteStore(dir, WithSQLiteStoreIDPrefix("gc"))
	if err != nil {
		t.Fatal(err)
	}
	sqliteWork, err := createRecoveryBudgetFixture(t, sqlite, workID, initial)
	if err != nil {
		t.Fatal(err)
	}
	assertGenericRecoveryBudgetWritesBlocked(t, sqlite, sqliteWork, initial, reset)
	if err := sqlite.(interface{ CloseStore() error }).CloseStore(); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenSQLiteStore(dir, WithSQLiteStoreIDPrefix("gc"))
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.(interface{ CloseStore() error }).CloseStore() //nolint:errcheck
	reopenedWork, err := reopened.Get(workID)
	if err != nil {
		t.Fatal(err)
	}
	if got := reopenedWork.Metadata[beadmeta.LifecycleRecoveryStateMetadataKey]; got != initial {
		t.Fatalf("reopened recovery state = %s, want persisted budget %s", got, initial)
	}
	assertGenericRecoveryBudgetWritesBlocked(t, reopened, reopenedWork, initial, reset)
}

func createRecoveryBudgetFixture(t *testing.T, store Store, id, recoveryState string) (Bead, error) {
	t.Helper()
	work, err := store.Create(Bead{
		ID: id, Title: "admitted work", Type: "task", Status: "in_progress",
		Metadata: StringMap{beadmeta.LifecycleRecoveryStateMetadataKey: recoveryState},
	})
	if err != nil {
		return Bead{}, err
	}
	if err := store.SetMetadata(id, "fixture.revision", "advanced"); err != nil {
		return Bead{}, err
	}
	return store.Get(work.ID)
}

func assertGenericRecoveryBudgetWritesBlocked(t *testing.T, store Store, work Bead, initial, reset string) {
	t.Helper()
	if err := store.Update(work.ID, UpdateOpts{Metadata: map[string]string{beadmeta.LifecycleRecoveryStateMetadataKey: reset}}); !errors.Is(err, ErrLifecycleMutationBlocked) {
		t.Fatalf("generic recovery-budget reset error = %v, want ErrLifecycleMutationBlocked", err)
	}
	if err := store.SetMetadata(work.ID, beadmeta.LifecycleRecoveryStateMetadataKey, reset); !errors.Is(err, ErrLifecycleMutationBlocked) {
		t.Fatalf("SetMetadata recovery-budget reset error = %v, want ErrLifecycleMutationBlocked", err)
	}
	writer, ok := ConditionalWriterFor(store)
	if !ok {
		t.Fatal("fixture store does not support conditional writes")
	}
	if err := writer.UpdateIfMatch(work.ID, work.Revision, UpdateOpts{Metadata: map[string]string{beadmeta.LifecycleRecoveryStateMetadataKey: reset}}); !errors.Is(err, ErrLifecycleMutationBlocked) {
		t.Fatalf("conditional recovery-budget reset error = %v, want ErrLifecycleMutationBlocked", err)
	}
	cas, ok := MetadataCASWriterFor(store)
	if !ok {
		t.Fatal("fixture store does not support metadata CAS")
	}
	if swapped, err := cas.CompareAndSetMetadataKey(work.ID, beadmeta.LifecycleRecoveryStateMetadataKey, initial, reset); swapped || !errors.Is(err, ErrLifecycleMutationBlocked) {
		t.Fatalf("metadata-CAS recovery-budget reset = (%v, %v), want lifecycle refusal", swapped, err)
	}
	got, err := store.Get(work.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Metadata[beadmeta.LifecycleRecoveryStateMetadataKey] != initial {
		t.Fatalf("generic writes changed recovery budget to %s, want %s", got.Metadata[beadmeta.LifecycleRecoveryStateMetadataKey], initial)
	}
}

func stringPointer(value string) *string { return &value }
