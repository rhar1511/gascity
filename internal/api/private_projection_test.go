package api

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
)

func TestPrivateProjectionEventAndWorkflowWire(t *testing.T) {
	b := beads.Bead{ID: "answer", Description: `{"Proof":"SECRET"}`, Title: "SECRET", Metadata: map[string]string{beadmeta.DecisionFrontierRecordMetadataKey: "decision-frontier/answer/v1", "custom": "SECRET"}}
	payload, err := beads.EncodeBeadEventPayload(b)
	if err != nil {
		t.Fatal(err)
	}
	e := events.Event{Type: events.BeadUpdated, Seq: 1, Message: "SECRET", Payload: payload}
	list, ok := toWireEvent(e)
	if !ok {
		t.Fatal("event list projection failed")
	}
	stream, err := wireEventFrom(e, nil)
	if err != nil {
		t.Fatal(err)
	}
	tagged, err := wireTaggedEventFrom(events.TaggedEvent{Event: e, City: "city"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, view := range []any{list, stream, tagged, workflowBeadResponseFromBead(b)} {
		encoded, err := json.Marshal(view)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(encoded), "SECRET") {
			t.Fatalf("wire leaked: %s", encoded)
		}
	}
	if _, visible := publicAttemptEvidenceBead(b); visible {
		t.Fatal("generic API exposed answer row")
	}
	if b.Metadata["custom"] != "SECRET" || string(e.Payload) != string(payload) {
		t.Fatal("wire projection mutated authority")
	}
}
