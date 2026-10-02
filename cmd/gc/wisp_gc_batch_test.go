package main

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// batchGCStore advertises beads.BatchDeleter so tests can prove protected
// workflow purge deliberately bypasses that unfenced capability.
type batchGCStore struct {
	*gcTestStore
	batchCalls [][]string
	depRemoves int
}

//nolint:unparam // error return satisfies beads.BatchDeleter; the test spy never fails.
func (s *batchGCStore) DeleteBatch(ids []string) error {
	s.batchCalls = append(s.batchCalls, append([]string(nil), ids...))
	for _, id := range ids {
		_ = s.Delete(id)
	}
	return nil
}

var _ beads.BatchDeleter = (*batchGCStore)(nil)

func (s *batchGCStore) DepRemove(issueID, dependsOnID string) error {
	s.depRemoves++
	return s.gcTestStore.DepRemove(issueID, dependsOnID)
}

func TestWispGCClosureUsesRevisionFencedDeletes(t *testing.T) {
	now := time.Now()
	base := newGCStore([]beads.Bead{
		makeGCBead("mol-1", now.Add(-2*time.Hour), "closed", "molecule"),
		{
			ID:        "mol-1.1",
			Status:    "open",
			Type:      "task",
			CreatedAt: now.Add(-2 * time.Hour),
			ParentID:  "mol-1",
		},
		{
			ID:        "mol-1.2",
			Status:    "open",
			Type:      "task",
			CreatedAt: now.Add(-2 * time.Hour),
			ParentID:  "mol-1.1",
		},
	})
	if err := base.DepAdd("mol-1.1", "mol-1", "parent-child"); err != nil {
		t.Fatalf("DepAdd(mol-1.1->mol-1): %v", err)
	}
	if err := base.DepAdd("mol-1.2", "mol-1.1", "parent-child"); err != nil {
		t.Fatalf("DepAdd(mol-1.2->mol-1.1): %v", err)
	}
	store := &batchGCStore{gcTestStore: base}

	wg := newWispGC(5*time.Minute, time.Hour, 0)
	purged, err := wg.runGC(beads.GraphStore{Store: store}, beads.MailStore{Store: store}, now)
	if err != nil {
		t.Fatalf("runGC: %v", err)
	}
	if purged != 1 {
		t.Fatalf("purged = %d, want 1 root purge accounting", purged)
	}

	if len(store.batchCalls) != 0 {
		t.Fatalf("batch calls = %v, want none because batch delete has no per-row revision fence", store.batchCalls)
	}
	if store.depRemoves != 0 {
		t.Fatalf("dependency removals = %d, want checked deletion to own atomic cleanup", store.depRemoves)
	}
	assertDeletedIDs(t, base.deletedIDs, "mol-1", "mol-1.1", "mol-1.2")
	for _, id := range []string{"mol-1", "mol-1.1", "mol-1.2"} {
		for _, direction := range []string{"down", "up"} {
			deps, err := base.DepList(id, direction)
			if err != nil || len(deps) != 0 {
				t.Fatalf("DepList(%s, %s) after checked deletion = %v, %v", id, direction, deps, err)
			}
		}
	}
}

// The production controller rewraps the store in beadPolicyStore. This pins
// that its conditional-writer handle remains reachable while batch delete is
// deliberately bypassed.
func TestDeleteWorkflowBeadsBatchUsesConditionalWriterThroughPolicyWrapper(t *testing.T) {
	now := time.Now()
	base := newGCStore([]beads.Bead{
		makeGCBead("mol-1", now, "closed", "molecule"),
		makeGCBead("mol-1.1", now, "closed", "task"),
	})
	batchStore := &batchGCStore{gcTestStore: base}

	wrapped := wrapStoreWithBeadPolicies(batchStore, nil)
	if _, ok := wrapped.(beads.BatchDeleter); !ok {
		t.Fatalf("policy-wrapped store does not expose beads.BatchDeleter")
	}

	if err := deleteWorkflowBeadsBatch(wrapped, []string{"mol-1", "mol-1.1"}); err != nil {
		t.Fatalf("deleteWorkflowBeadsBatch through policy wrapper: %v", err)
	}

	if len(batchStore.batchCalls) != 0 {
		t.Fatalf("batch calls = %v, want revision-fenced per-row deletion", batchStore.batchCalls)
	}
	assertDeletedIDs(t, base.deletedIDs, "mol-1", "mol-1.1")
}

// A policy-wrapped backing without BatchDeleter uses the same fenced path.
func TestDeleteWorkflowBeadsBatchFallsBackThroughPolicyWrapperWithoutBatchDeleter(t *testing.T) {
	now := time.Now()
	base := newGCStore([]beads.Bead{
		makeGCBead("mol-1", now, "closed", "molecule"),
		makeGCBead("mol-1.1", now, "closed", "task"),
	})

	// base (plain gcTestStore over MemStore) does not implement beads.BatchDeleter.
	wrapped := wrapStoreWithBeadPolicies(base, nil)

	if err := deleteWorkflowBeadsBatch(wrapped, []string{"mol-1", "mol-1.1"}); err != nil {
		t.Fatalf("deleteWorkflowBeadsBatch fallback through policy wrapper: %v", err)
	}
	assertDeletedIDs(t, base.deletedIDs, "mol-1", "mol-1.1")
}
