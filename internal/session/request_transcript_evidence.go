package session

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// RequestTranscriptEvidenceStatus says whether the transcript references are
// grounded in an exact request envelope and provider IDs.
type RequestTranscriptEvidenceStatus string

// Transcript-evidence statuses distinguish grounded references from explicit
// unavailability.
const (
	RequestTranscriptEvidenceAvailable   RequestTranscriptEvidenceStatus = "available"
	RequestTranscriptEvidenceUnavailable RequestTranscriptEvidenceStatus = "unavailable"
)

// RequestTranscriptEvidenceReason identifies why references could not be
// established from the full provider transcript.
type RequestTranscriptEvidenceReason string

// Transcript-evidence reasons preserve why exact provider references could not
// be established.
const (
	RequestTranscriptNotObserved           RequestTranscriptEvidenceReason = "not_observed"
	RequestTranscriptUnavailable           RequestTranscriptEvidenceReason = "transcript_unavailable"
	RequestTranscriptIncompleteView        RequestTranscriptEvidenceReason = "incomplete_view"
	RequestTranscriptDegraded              RequestTranscriptEvidenceReason = "transcript_degraded"
	RequestTranscriptBranched              RequestTranscriptEvidenceReason = "branched_transcript"
	RequestTranscriptNoExactEnvelope       RequestTranscriptEvidenceReason = "no_exact_envelope"
	RequestTranscriptAmbiguousEnvelope     RequestTranscriptEvidenceReason = "ambiguous_envelope"
	RequestTranscriptUnstableEntryID       RequestTranscriptEvidenceReason = "unstable_entry_id"
	RequestTranscriptStreamMismatch        RequestTranscriptEvidenceReason = "stream_mismatch"
	RequestTranscriptGenerationUnavailable RequestTranscriptEvidenceReason = "generation_unavailable"
	RequestTranscriptNoToolLineage         RequestTranscriptEvidenceReason = "no_exact_tool_lineage"
	RequestTranscriptAmbiguousToolLineage  RequestTranscriptEvidenceReason = "ambiguous_tool_lineage"
	RequestTranscriptMissingToolID         RequestTranscriptEvidenceReason = "missing_tool_id"
)

// RequestTranscriptReferenceKind identifies a stable provider transcript or
// tool-event identity without copying provider payloads.
type RequestTranscriptReferenceKind string

// Transcript-reference kinds distinguish provider entries and tool lineage.
const (
	RequestTranscriptEntryReference      RequestTranscriptReferenceKind = "transcript_entry"
	RequestTranscriptToolUseReference    RequestTranscriptReferenceKind = "tool_use"
	RequestTranscriptToolResultReference RequestTranscriptReferenceKind = "tool_result"
)

// RequestTranscriptReference binds one provider identity to the exact durable
// request and the transcript stream generation where it was observed.
type RequestTranscriptReference struct {
	SessionID              string                         `json:"session_id"`
	Generation             int                            `json:"generation"`
	RequestID              string                         `json:"request_id"`
	TranscriptStreamID     string                         `json:"transcript_stream_id"`
	TranscriptGenerationID string                         `json:"transcript_generation_id"`
	EntryID                string                         `json:"entry_id"`
	Kind                   RequestTranscriptReferenceKind `json:"kind"`
	ToolID                 string                         `json:"tool_id,omitempty"`
}

// RequestTranscriptEvidence is one append-only observation for the exact
// transcript stream/generation. References contain IDs only; unavailable
// states remain explicit when the adapter cannot establish them safely.
type RequestTranscriptEvidence struct {
	Status                 RequestTranscriptEvidenceStatus `json:"status"`
	UnavailableReason      RequestTranscriptEvidenceReason `json:"unavailable_reason,omitempty"`
	ToolStatus             RequestTranscriptEvidenceStatus `json:"tool_status"`
	ToolUnavailableReason  RequestTranscriptEvidenceReason `json:"tool_unavailable_reason,omitempty"`
	TranscriptStreamID     string                          `json:"transcript_stream_id,omitempty"`
	TranscriptGenerationID string                          `json:"transcript_generation_id,omitempty"`
	References             []RequestTranscriptReference    `json:"references,omitempty"`
}

type trackedRequestEnvelope struct {
	RequestID            string `json:"request_id"`
	SessionID            string `json:"session_id"`
	Generation           int    `json:"generation"`
	Instruction          string `json:"instruction"`
	AcknowledgeWith      string `json:"acknowledge_with"`
	Message              string `json:"message"`
	AcknowledgeWhen      string `json:"acknowledge_when,omitempty"`
	AcknowledgementMeans string `json:"acknowledgement_means,omitempty"`
}

const (
	trackedRequestEnvelopeInstruction  = "Acknowledge receipt before acting by running the command in acknowledge_with."
	trackedRequestAcknowledgeWhen      = "after reading this request and before acting on it"
	trackedRequestAcknowledgementMeans = "receipt_only; this does not verify completion or effect"
)

func marshalTrackedRequestEnvelope(requestID, sessionID string, generation int, message string) ([]byte, error) {
	return json.Marshal(trackedRequestEnvelope{
		RequestID:            requestID,
		SessionID:            sessionID,
		Generation:           generation,
		Instruction:          trackedRequestEnvelopeInstruction,
		AcknowledgeWith:      "gc session request ack -- " + requestID,
		Message:              message,
		AcknowledgeWhen:      trackedRequestAcknowledgeWhen,
		AcknowledgementMeans: trackedRequestAcknowledgementMeans,
	})
}

// RequestEnvelopeMatchesReceipt accepts only the canonical envelope generated
// by tracked delivery and bound to this exact persisted receipt.
func RequestEnvelopeMatchesReceipt(text string, receipt RequestReceipt) bool {
	if receipt.SessionID == "" || receipt.RequestID == "" || receipt.Generation <= 0 || !validRequestDigest(receipt.MessageDigest) {
		return false
	}
	var envelope trackedRequestEnvelope
	if err := json.Unmarshal([]byte(text), &envelope); err != nil {
		return false
	}
	acknowledgeWith := "gc session request ack -- " + receipt.RequestID
	if envelope.AcknowledgeWhen == "" && envelope.AcknowledgementMeans == "" {
		acknowledgeWith = fmt.Sprintf("gc session request ack %q", receipt.RequestID)
	} else if envelope.AcknowledgeWhen != trackedRequestAcknowledgeWhen || envelope.AcknowledgementMeans != trackedRequestAcknowledgementMeans {
		return false
	}
	if envelope.RequestID != receipt.RequestID || envelope.SessionID != receipt.SessionID || envelope.Generation != receipt.Generation ||
		envelope.Instruction != trackedRequestEnvelopeInstruction || envelope.AcknowledgeWith != acknowledgeWith ||
		requestDigest(envelope.Message) != receipt.MessageDigest {
		return false
	}
	canonical, err := json.Marshal(envelope)
	return err == nil && bytes.Equal(canonical, []byte(text))
}

// RecordRequestTranscriptEvidence appends trusted worker-derived references to
// the exact current execution's receipt. Callers must derive evidence from an
// unpaged provider history and may not accept reference fields from a request
// body. Existing observations are immutable; exact replays are no-ops.
func (s *Store) RecordRequestTranscriptEvidence(sessionID, requestID string, generation int, evidence RequestTranscriptEvidence, now time.Time) (RequestReceipt, error) {
	if generation <= 0 || now.IsZero() {
		return RequestReceipt{}, ErrRequestConflict
	}
	return s.mutateRequestReceipt(sessionID, requestID, func(b beads.Bead, record *storedRequestReceipt) (bool, error) {
		if record.Version == 0 {
			return false, ErrRequestNotFound
		}
		if record.Version != 2 || record.Generation != generation || infoFromPersistedBead(b).Generation != fmt.Sprintf("%d", generation) || record.DeliveryAttemptedAt == nil {
			return false, ErrRequestConflict
		}
		canonical, err := canonicalRequestTranscriptEvidence(evidence, sessionID, requestID, generation)
		if err != nil {
			return false, err
		}
		for _, event := range record.Events {
			if event.Kind != RequestEventTranscriptEvidence || event.TranscriptEvidence == nil ||
				event.TranscriptEvidence.TranscriptStreamID != canonical.TranscriptStreamID ||
				event.TranscriptEvidence.TranscriptGenerationID != canonical.TranscriptGenerationID {
				continue
			}
			if sameRequestTranscriptEvidence(*event.TranscriptEvidence, canonical) {
				return false, nil
			}
			return false, ErrRequestConflict
		}
		event := RequestEvent{
			Sequence:           len(record.Events) + 1,
			Kind:               RequestEventTranscriptEvidence,
			SessionID:          record.SessionID,
			Generation:         record.Generation,
			RequestID:          record.RequestID,
			MessageDigest:      record.MessageDigest,
			At:                 now.UTC(),
			TranscriptEvidence: &canonical,
		}
		record.Events = append(record.Events, event)
		return true, nil
	})
}

func canonicalRequestTranscriptEvidence(evidence RequestTranscriptEvidence, sessionID, requestID string, generation int) (RequestTranscriptEvidence, error) {
	if !validRequestTranscriptEvidenceStatus(evidence.Status, evidence.UnavailableReason) ||
		!validRequestTranscriptEvidenceStatus(evidence.ToolStatus, evidence.ToolUnavailableReason) {
		return RequestTranscriptEvidence{}, ErrRequestConflict
	}
	if (evidence.TranscriptStreamID == "") != (evidence.TranscriptGenerationID == "") ||
		(evidence.TranscriptStreamID != "" && (!validRequestDigest(evidence.TranscriptStreamID) || !validRequestDigest(evidence.TranscriptGenerationID))) {
		return RequestTranscriptEvidence{}, ErrRequestConflict
	}
	canonical := evidence
	canonical.References = append([]RequestTranscriptReference(nil), evidence.References...)
	for index := range canonical.References {
		reference := &canonical.References[index]
		if reference.SessionID != sessionID || reference.RequestID != requestID || reference.Generation != generation ||
			reference.TranscriptStreamID != evidence.TranscriptStreamID || reference.TranscriptGenerationID != evidence.TranscriptGenerationID ||
			!validRequestTranscriptReference(*reference) {
			return RequestTranscriptEvidence{}, ErrRequestConflict
		}
	}
	sort.Slice(canonical.References, func(i, j int) bool {
		left, right := canonical.References[i], canonical.References[j]
		if left.Kind != right.Kind {
			return left.Kind < right.Kind
		}
		if left.EntryID != right.EntryID {
			return left.EntryID < right.EntryID
		}
		return left.ToolID < right.ToolID
	})
	for index := 1; index < len(canonical.References); index++ {
		if sameRequestTranscriptReference(canonical.References[index-1], canonical.References[index]) {
			return RequestTranscriptEvidence{}, ErrRequestConflict
		}
	}
	entryRefs, toolRefs := 0, 0
	for _, reference := range canonical.References {
		switch reference.Kind {
		case RequestTranscriptEntryReference:
			entryRefs++
		case RequestTranscriptToolUseReference, RequestTranscriptToolResultReference:
			toolRefs++
		}
	}
	if canonical.Status == RequestTranscriptEvidenceAvailable && entryRefs != 1 ||
		canonical.Status == RequestTranscriptEvidenceUnavailable && (entryRefs != 0 || toolRefs != 0) ||
		canonical.ToolStatus == RequestTranscriptEvidenceUnavailable && toolRefs != 0 {
		return RequestTranscriptEvidence{}, ErrRequestConflict
	}
	if canonical.Status == RequestTranscriptEvidenceAvailable && canonical.TranscriptStreamID == "" {
		return RequestTranscriptEvidence{}, ErrRequestConflict
	}
	if canonical.Status == RequestTranscriptEvidenceUnavailable && canonical.ToolStatus == RequestTranscriptEvidenceAvailable {
		return RequestTranscriptEvidence{}, ErrRequestConflict
	}
	if canonical.ToolStatus == RequestTranscriptEvidenceUnavailable && canonical.ToolUnavailableReason == "" {
		return RequestTranscriptEvidence{}, ErrRequestConflict
	}
	return canonical, nil
}

func validRequestTranscriptEvidenceStatus(status RequestTranscriptEvidenceStatus, reason RequestTranscriptEvidenceReason) bool {
	if status == RequestTranscriptEvidenceAvailable {
		return reason == ""
	}
	if status != RequestTranscriptEvidenceUnavailable {
		return false
	}
	switch reason {
	case RequestTranscriptNotObserved,
		RequestTranscriptUnavailable,
		RequestTranscriptIncompleteView,
		RequestTranscriptDegraded,
		RequestTranscriptBranched,
		RequestTranscriptNoExactEnvelope,
		RequestTranscriptAmbiguousEnvelope,
		RequestTranscriptUnstableEntryID,
		RequestTranscriptStreamMismatch,
		RequestTranscriptGenerationUnavailable,
		RequestTranscriptNoToolLineage,
		RequestTranscriptAmbiguousToolLineage,
		RequestTranscriptMissingToolID:
		return true
	default:
		return false
	}
}

func unavailableRequestTranscriptEvidence(reason RequestTranscriptEvidenceReason) *RequestTranscriptEvidence {
	return &RequestTranscriptEvidence{
		Status:                RequestTranscriptEvidenceUnavailable,
		UnavailableReason:     reason,
		ToolStatus:            RequestTranscriptEvidenceUnavailable,
		ToolUnavailableReason: reason,
	}
}

func validRequestTranscriptReference(reference RequestTranscriptReference) bool {
	if reference.SessionID == "" || reference.RequestID == "" || reference.Generation <= 0 ||
		!validRequestDigest(reference.TranscriptStreamID) || !validRequestDigest(reference.TranscriptGenerationID) ||
		strings.TrimSpace(reference.EntryID) == "" || strings.TrimSpace(reference.EntryID) != reference.EntryID || strings.ContainsRune(reference.EntryID, '\x00') {
		return false
	}
	switch reference.Kind {
	case RequestTranscriptEntryReference:
		return reference.ToolID == ""
	case RequestTranscriptToolUseReference, RequestTranscriptToolResultReference:
		return strings.TrimSpace(reference.ToolID) != "" && strings.TrimSpace(reference.ToolID) == reference.ToolID && !strings.ContainsRune(reference.ToolID, '\x00')
	default:
		return false
	}
}

func sameRequestTranscriptReference(left, right RequestTranscriptReference) bool {
	return left.SessionID == right.SessionID && left.Generation == right.Generation && left.RequestID == right.RequestID &&
		left.TranscriptStreamID == right.TranscriptStreamID && left.TranscriptGenerationID == right.TranscriptGenerationID &&
		left.EntryID == right.EntryID && left.Kind == right.Kind && left.ToolID == right.ToolID
}

func sameRequestTranscriptEvidence(left, right RequestTranscriptEvidence) bool {
	if left.Status != right.Status || left.UnavailableReason != right.UnavailableReason || left.ToolStatus != right.ToolStatus ||
		left.ToolUnavailableReason != right.ToolUnavailableReason || left.TranscriptStreamID != right.TranscriptStreamID ||
		left.TranscriptGenerationID != right.TranscriptGenerationID || len(left.References) != len(right.References) {
		return false
	}
	for index := range left.References {
		if !sameRequestTranscriptReference(left.References[index], right.References[index]) {
			return false
		}
	}
	return true
}

func cloneRequestTranscriptEvidence(evidence *RequestTranscriptEvidence) *RequestTranscriptEvidence {
	if evidence == nil {
		return nil
	}
	cloned := *evidence
	cloned.References = append([]RequestTranscriptReference(nil), evidence.References...)
	return &cloned
}
