package beads

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
)

func TestBdStoreDecisionFrontierReceiptReaderBindsQ43EnvelopeAndPayload(t *testing.T) {
	const (
		issueID  = "gc/source-1"
		cityRef  = "city:receipt-test"
		storeRef = "rig:fixture"
	)
	mapID := DecisionFrontierMapRecordID(cityRef, storeRef, issueID, "9")
	receiptID := decisionFrontierStableID("frontier-transition", cityRef, storeRef, mapID, "reserve")
	marker, err := json.Marshal(struct {
		SchemaVersion int    `json:"schema_version"`
		CityRef       string `json:"city_ref"`
		StoreRef      string `json:"store_ref"`
		WorkID        string `json:"work_id"`
		MapID         string `json:"map_id"`
		WorkRevision  string `json:"work_revision"`
		WorkDigest    string `json:"work_digest"`
		ProposalHash  string `json:"proposal_hash"`
		ReservationID string `json:"reservation_id"`
	}{1, cityRef, storeRef, issueID, mapID, "9", "digest", "proposal", receiptID})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(RevisionTransitionReceipt{
		ID: receiptID, CityRef: cityRef, StoreRef: storeRef, WorkID: issueID, MapID: mapID,
		Operation: "reserve", FromRevision: 9,
	})
	if err != nil {
		t.Fatal(err)
	}
	payload, err = canonicalControllerTransitionJSON(payload)
	if err != nil {
		t.Fatal(err)
	}
	value, err := json.Marshal(string(marker))
	if err != nil {
		t.Fatal(err)
	}
	wire, err := json.Marshal(map[string]any{
		"receipt_id": receiptID, "issue_id": issueID, "scope": storeRef,
		"kind": controllerDecisionFrontierTransitionKind, "actor": privateEvidenceActor,
		"expected_version": "9", "to_version": "23", "key": beadmeta.DecisionFrontierHoldMetadataKey,
		"value": json.RawMessage(value), "payload": json.RawMessage(payload),
	})
	if err != nil {
		t.Fatal(err)
	}
	decodedWire, err := decodeControllerMetadataTransitionReceipt(wire)
	if err != nil {
		t.Fatalf("decode Q43 receipt fixture: %v", err)
	}
	if _, err := decodeDecisionFrontierControllerReceipt(decodedWire); err != nil {
		t.Fatalf("decode decision-frontier receipt fixture: %v", err)
	}
	transport := &controllerTransitionTestTransport{}
	transport.handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/v0/beads/context" {
			writeControllerTransitionTestContext(w, "project-a", "gc_fixture", []string{
				"issues.transitionMetadata", "issues.transitionReceipt.get", "project.enforce",
			})
			return
		}
		if r.Method == http.MethodGet && r.URL.EscapedPath() == controllerTransitionReceiptPath+url.PathEscape(receiptID) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(wire)
			return
		}
		http.NotFound(w, r)
	})
	store := controllerTransitionTestStore(t, transport)
	reader, ok := RevisionTransitionReceiptReaderFor(store)
	if !ok || reader == nil {
		t.Fatal("configured BdStore did not expose its exact receipt reader")
	}
	actual, found, err := reader.DecisionFrontierRevisionTransitionReceipt(issueID, receiptID)
	if err != nil || !found {
		t.Fatalf("receipt read = %+v, found=%v err=%v", actual, found, err)
	}
	want := RevisionTransitionReceipt{
		ID: receiptID, CityRef: cityRef, StoreRef: storeRef, WorkID: issueID, MapID: mapID,
		Operation: "reserve", FromRevision: 9, ToRevision: 23,
	}
	if actual != want {
		t.Fatalf("receipt = %+v, want %+v", actual, want)
	}
	if transport.contextCount != 1 || transport.receiptGetCount != 1 {
		t.Fatalf("requests: context=%d receipt-get=%d, want one each", transport.contextCount, transport.receiptGetCount)
	}
	if _, _, err := reader.DecisionFrontierRevisionTransitionReceipt("gc/other", receiptID); !errors.Is(err, ErrDecisionFrontierTransitionReceiptCorrupt) {
		t.Fatalf("wrong issue receipt error = %v, want corrupt receipt", err)
	}
}
