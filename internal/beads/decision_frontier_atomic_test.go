package beads

import (
	"errors"
	"testing"
)

func TestMemoryFrontierTargetTransactionOwnsBackendLockAndRollsBack(t *testing.T) {
	store := NewMemStore()
	store.HonorExplicitIDs = true
	if _, err := store.Create(Bead{ID: "session", Type: "session", Title: "Review", Metadata: map[string]string{"generation": "7", "configured_named_session": "true", "configured_named_identity": "review-desk"}}); err != nil {
		t.Fatal(err)
	}
	failure := errors.New("injected transaction failure")
	err := store.EnsureDecisionFrontierTargetBound(DecisionFrontierTargetBinding{SessionID: "session", Generation: 7, TargetName: "review-desk"}, func(view Store) error {
		if store.mu.TryLock() {
			store.mu.Unlock()
			t.Fatal("creation is not serialized with backend target writers")
		}
		if _, err := view.Create(Bead{ID: "candidate", Type: "task", Title: "Candidate"}); err != nil {
			t.Fatal(err)
		}
		return failure
	})
	if !errors.Is(err, failure) {
		t.Fatalf("transaction error: %v", err)
	}
	if _, err := store.Get("candidate"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("failed transaction committed: %v", err)
	}
	if err := store.SetMetadata("session", "generation", "8"); err != nil {
		t.Fatal(err)
	}
	called := false
	err = store.EnsureDecisionFrontierTargetBound(DecisionFrontierTargetBinding{SessionID: "session", Generation: 7, TargetName: "review-desk"}, func(Store) error { called = true; return nil })
	if !errors.Is(err, ErrDecisionFrontierLinkConflict) || called {
		t.Fatalf("stale target reached creation: called=%v err=%v", called, err)
	}
	if backend, available := DecisionFrontierAtomicBackendFor(&SQLiteStore{}); available || backend != nil {
		t.Fatal("row-CAS-only backend advertised atomic source-flow contract")
	}
}
