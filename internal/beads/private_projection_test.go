package beads

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
)

func TestPrivatePresentationPreservesAuthoritativeRecord(t *testing.T) {
	for _, marker := range []string{beadmeta.DecisionFrontierRecordMetadataKey, beadmeta.AttemptEvidenceArchivePayloadMetadataKey, beadmeta.AttemptEvidencePayloadDataMetadataKey} {
		value := "SECRET"
		if marker == beadmeta.DecisionFrontierRecordMetadataKey {
			value = "decision-frontier/answer/v1"
		}
		raw := Bead{ID: "answer", Title: "SECRET", Description: `{"Proof":"SECRET"}`, Metadata: map[string]string{marker: value, "custom": "SECRET"}, Labels: []string{"SECRET"}}
		view := PublicBead(raw)
		data, err := json.Marshal(view)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "SECRET") || view.ID != raw.ID {
			t.Fatalf("private projection: %s", data)
		}
		if raw.Description != `{"Proof":"SECRET"}` || raw.Metadata["custom"] != "SECRET" {
			t.Fatal("projection modified authority")
		}
		if !IsPrivatePresentationRecord(raw) {
			t.Fatal("private marker unrecognized")
		}
	}
	raw := Bead{Description: "public", Metadata: map[string]string{"custom": "keep", beadmeta.StdoutMetadataKey: "SECRET", beadmeta.SessionRequestReceiptPrefix + "one": "SECRET"}}
	view := PublicBead(raw)
	if view.Description != "public" || view.Metadata["custom"] != "keep" || len(view.Metadata) != 1 {
		t.Fatalf("public projection: %#v", view)
	}
	view.Metadata["custom"] = "changed"
	if raw.Metadata["custom"] != "keep" || raw.Metadata[beadmeta.StdoutMetadataKey] != "SECRET" {
		t.Fatal("metadata aliases authority")
	}
	rows := []Bead{raw}
	projected := PublicBeads(rows)
	projected[0].Description = "changed"
	projected[0].Metadata["custom"] = "changed"
	if rows[0].Description != "public" || rows[0].Metadata["custom"] != "keep" {
		t.Fatal("list projection aliases authoritative records")
	}
	if PublicBeads(nil) != nil {
		t.Fatal("nil list changed to non-nil")
	}
}

func TestPrivateEventPresentationKeepsRawDecoder(t *testing.T) {
	raw := Bead{ID: "answer", Title: "SECRET", Description: `{"Proof":"SECRET"}`, Metadata: map[string]string{beadmeta.DecisionFrontierRecordMetadataKey: "decision-frontier/answer/v1"}}
	payload, err := EncodeBeadEventPayload(raw)
	if err != nil {
		t.Fatal(err)
	}
	projected, message, err := PublicBeadEvent("bead.updated", payload, "SECRET")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(projected), "SECRET") || message != "" {
		t.Fatalf("event leaked: %s %q", projected, message)
	}
	decoded, ok := DecodeBeadEventPayload(payload)
	if !ok || decoded.Description != raw.Description {
		t.Fatal("raw decoder lost verification evidence")
	}
	if _, _, err := PublicBeadEvent("bead.updated", []byte(`{"broken":true}`), "SECRET"); err == nil {
		t.Fatal("malformed bead event was forwarded")
	}
}
