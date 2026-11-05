package session

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

func TestRuntimeOwnershipSnapshotRetainsLabelLostAndDamagedRows(t *testing.T) {
	store := beads.NewMemStore()
	for _, row := range []beads.Bead{
		{ID: "typed", Type: BeadType},
		{ID: "damaged", Type: "task", Labels: []string{LabelSession}},
		{ID: "both", Type: BeadType, Labels: []string{LabelSession}},
		{ID: "closed", Status: "closed", Type: BeadType},
		{ID: "ordinary", Type: "task"},
	} {
		created, err := store.Create(row)
		if err != nil {
			t.Fatal(err)
		}
		if row.Status == "closed" {
			if err := store.Close(created.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	infos, err := NewStore(beads.SessionStore{Store: store}).ListRuntimeOwnershipInfos()
	if err != nil || len(infos) != 3 {
		t.Fatalf("ownership rows=%v %v", infos, err)
	}
	if _, err := NewStore(beads.SessionStore{}).ListRuntimeOwnershipInfos(); err == nil {
		t.Fatal("nil authority treated as empty")
	}
}
