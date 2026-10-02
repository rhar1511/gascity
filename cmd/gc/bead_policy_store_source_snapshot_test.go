package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

type beadPolicySourceSnapshotStore struct {
	beads.Store
	bead beads.Bead
}

func (s *beadPolicySourceSnapshotStore) DecisionFrontierSourceSnapshot(id string) (beads.Bead, error) {
	bead := s.bead
	bead.ID = id
	return bead, nil
}

func (s *beadPolicySourceSnapshotStore) DecisionFrontierSourceReaderHandle() (beads.DecisionFrontierSourceReader, bool) {
	return s, s != nil
}

func TestBeadPolicyStorePreservesDecisionFrontierSourceReader(t *testing.T) {
	backing := &beadPolicySourceSnapshotStore{Store: beads.NewMemStore(), bead: beads.Bead{Title: "policy backing snapshot"}}
	wrapped := wrapStoreWithBeadPolicies(backing, nil)
	reader, ok := beads.DecisionFrontierSourceReaderFor(wrapped)
	if !ok || reader == nil {
		t.Fatal("bead policy wrapper hid the source-snapshot reader")
	}
	bead, err := reader.DecisionFrontierSourceSnapshot("source-1")
	if err != nil || bead.ID != "source-1" || bead.Title != "policy backing snapshot" {
		t.Fatalf("policy source snapshot = %+v, %v", bead, err)
	}
}

func TestBeadPolicyStoreDoesNotInventDecisionFrontierSourceReader(t *testing.T) {
	wrapped := wrapStoreWithBeadPolicies(beads.Store(beadPolicyPlainStore{Store: beads.NewMemStore()}), nil)
	if reader, ok := beads.DecisionFrontierSourceReaderFor(wrapped); ok || reader != nil {
		t.Fatal("bead policy wrapper invented a source-snapshot reader")
	}
}

func TestBeadPolicyStorePreservesOnlyBackingDecisionFrontierWriter(t *testing.T) {
	backing := beads.NewMemStore()
	backing.HonorExplicitIDs = true
	wrapped := wrapStoreWithBeadPolicies(backing, nil)
	if _, ok := beads.DecisionFrontierRecordWriterFor(wrapped); !ok {
		t.Fatal("bead policy wrapper hid the backing's writable decision-frontier role")
	}

	incomplete := &beadPolicyPlainStore{Store: beads.NewMemStore()}
	wrapped = wrapStoreWithBeadPolicies(incomplete, nil)
	if writer, ok := beads.DecisionFrontierRecordWriterFor(wrapped); ok || writer != nil {
		t.Fatalf("bead policy wrapper invented a decision-frontier writer: (%T, %t)", writer, ok)
	}
}

type beadPolicyPlainStore struct{ beads.Store }
