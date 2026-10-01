package beads

import (
	"encoding/json"
	"fmt"
	"maps"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
)

// IsPrivatePresentationRecord identifies records excluded from generic views.
// Marker presence protects incomplete attempt records as well as complete ones.
func IsPrivatePresentationRecord(b Bead) bool {
	return IsProtectedAttemptEvidenceRecord(b) ||
		b.Metadata[beadmeta.DecisionFrontierRecordMetadataKey] == "decision-frontier/answer/v1"
}

// IsPrivatePresentationMetadataKey identifies private fields on public owners.
func IsPrivatePresentationMetadataKey(key string) bool {
	return key == beadmeta.AttemptEvidenceArchiveAttemptIDMetadataKey ||
		key == beadmeta.AttemptEvidenceArchiveOwnerIDMetadataKey ||
		key == beadmeta.AttemptEvidenceArchivePayloadMetadataKey ||
		key == beadmeta.AttemptEvidenceArchiveDigestMetadataKey ||
		key == beadmeta.AttemptEvidencePayloadDataMetadataKey ||
		key == beadmeta.AttemptEvidencePayloadDigestMetadataKey ||
		key == beadmeta.AttemptEvidenceReferenceMetadataKey ||
		key == beadmeta.StdoutMetadataKey || key == beadmeta.StderrMetadataKey || key == beadmeta.OutputJSONMetadataKey ||
		strings.HasPrefix(key, beadmeta.AttemptEvidenceIndexPrefix) ||
		strings.HasPrefix(key, beadmeta.SessionRequestReceiptPrefix)
}

// PublicBead returns a presentation copy, never an authoritative read. Private
// records become identity-only stubs; generic APIs may instead omit them using
// IsPrivatePresentationRecord. Store reads and verification must retain raw data.
func PublicBead(b Bead) Bead {
	if IsPrivatePresentationRecord(b) {
		return Bead{ID: b.ID, Title: "[private record]"}
	}
	b.Metadata = maps.Clone(b.Metadata)
	for key := range b.Metadata {
		if IsPrivatePresentationMetadataKey(key) {
			delete(b.Metadata, key)
		}
	}
	return b
}

// PublicBeads projects a list without altering the source slice or its records.
func PublicBeads(rows []Bead) []Bead {
	if rows == nil {
		return nil
	}
	out := make([]Bead, len(rows))
	for i, b := range rows {
		out[i] = PublicBead(b)
	}
	return out
}

// PublicBeadEvent projects a bead snapshot and its free-form message at an
// outbound boundary. Historical events are treated exactly like current ones.
// The decoder and durable event bytes remain authoritative and unchanged.
func PublicBeadEvent(eventType string, payload json.RawMessage, message string) (json.RawMessage, string, error) {
	switch eventType {
	case "bead.created", "bead.updated", "bead.closed", "bead.deleted":
	default:
		return payload, message, nil
	}
	if len(payload) == 0 || string(payload) == "null" {
		return payload, message, nil
	}
	b, ok := DecodeBeadEventPayload(payload)
	if !ok {
		return nil, "", fmt.Errorf("invalid %s bead snapshot", eventType)
	}
	if IsPrivatePresentationRecord(b) {
		message = ""
	}
	data, err := EncodeBeadEventPayload(PublicBead(b))
	return data, message, err
}
