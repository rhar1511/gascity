package beads

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
)

func TestRevisionTransitionReceiptReaderFindsExactLocalReceipt(t *testing.T) {
	for _, tc := range decisionFrontierStores(t) {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if err := tc.close(); err != nil {
					t.Errorf("close decision-frontier test store: %v", err)
				}
			}()
			work, err := tc.store.Create(Bead{Title: "receipt reader source"})
			if err != nil {
				t.Fatal(err)
			}
			held, _, _ := reserveGuardTestSource(t, tc.store, work.ID)
			var expected []RevisionTransitionReceipt
			if err := json.Unmarshal([]byte(held.Metadata[beadmeta.DecisionFrontierRevisionReceiptsMetadataKey]), &expected); err != nil || len(expected) != 1 {
				t.Fatalf("decode test source receipt: receipts=%+v err=%v", expected, err)
			}

			reader, ok := RevisionTransitionReceiptReaderFor(tc.store)
			if !ok || reader == nil {
				t.Fatal("store did not expose its exact source receipt reader")
			}
			actual, found, err := reader.DecisionFrontierRevisionTransitionReceipt(work.ID, expected[0].ID)
			if err != nil || !found || actual != expected[0] {
				t.Fatalf("exact receipt = %+v, found=%v err=%v; want %+v", actual, found, err, expected[0])
			}
			if _, found, err := reader.DecisionFrontierRevisionTransitionReceipt(work.ID, "missing-receipt"); err != nil || found {
				t.Fatalf("missing receipt found=%v err=%v, want absent without error", found, err)
			}
			if _, _, err := reader.DecisionFrontierRevisionTransitionReceipt("another-source", expected[0].ID); !errors.Is(err, ErrNotFound) {
				t.Fatalf("missing source receipt read error = %v, want not found", err)
			}
			if _, _, err := decisionFrontierRevisionTransitionReceipt(held, "another-source", expected[0].ID); !errors.Is(err, ErrDecisionFrontierTransitionReceiptCorrupt) {
				t.Fatalf("wrong source receipt projection error = %v, want corrupt receipt", err)
			}
		})
	}
}
