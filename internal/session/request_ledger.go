package session

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

// RequestAttemptUnavailableReason explains why no canonical attempt reference
// was available when the request was accepted.
type RequestAttemptUnavailableReason string

// Request-attribution unavailable reasons preserve why no canonical attempt
// reference could be recorded.
const (
	RequestAttemptAdapterUnavailable RequestAttemptUnavailableReason = "adapter_unavailable"
	RequestAttemptBindingUnavailable RequestAttemptUnavailableReason = "binding_unavailable"
	RequestAttemptAdapterFailed      RequestAttemptUnavailableReason = "adapter_error"
)

// NewRequestAttemptReference accepts the canonical locator and reviewed work
// revision from the existing .15 binding. It stores a reference only; attempt
// identity and evidence continue to live in the .15 authority.
func NewRequestAttemptReference(storeRef, workID, attemptID, workRevision string) (RequestAttemptReference, error) {
	reference := RequestAttemptReference{StoreRef: storeRef, WorkID: workID, AttemptID: attemptID, WorkRevision: workRevision}
	if !validRequestAttemptReference(reference) {
		return RequestAttemptReference{}, ErrRequestConflict
	}
	return reference, nil
}

// AvailableRequestAttemptAttribution creates a typed exact-reference value.
func AvailableRequestAttemptAttribution(reference RequestAttemptReference) (RequestAttemptAttribution, error) {
	if !validRequestAttemptReference(reference) {
		return RequestAttemptAttribution{}, ErrRequestConflict
	}
	return RequestAttemptAttribution{
		Status:    attributionAvailable,
		Reference: &reference,
	}, nil
}

func validRequestAttemptReference(reference RequestAttemptReference) bool {
	storeRef := strings.TrimSpace(reference.StoreRef)
	if storeRef == "" || storeRef != reference.StoreRef || reference.WorkID == "" || strings.TrimSpace(reference.WorkID) != reference.WorkID ||
		!canonicalAttemptIDPattern.MatchString(reference.AttemptID) {
		return false
	}
	storeParts := strings.SplitN(storeRef, ":", 2)
	if len(storeParts) != 2 || (storeParts[0] != "city" && storeParts[0] != "rig") || strings.TrimSpace(storeParts[1]) == "" {
		return false
	}
	revision, err := strconv.ParseInt(reference.WorkRevision, 10, 64)
	return err == nil && revision != 0 && strconv.FormatInt(revision, 10) == reference.WorkRevision
}

// UnavailableRequestAttemptAttribution records an explicit reason without
// introducing a placeholder reference or guessed attempt identity.
func UnavailableRequestAttemptAttribution(reason RequestAttemptUnavailableReason) RequestAttemptAttribution {
	return RequestAttemptAttribution{Status: attributionUnavailable, Reason: string(reason)}
}

func validRequestAttemptAttribution(attribution RequestAttemptAttribution) bool {
	switch attribution.Status {
	case attributionAvailable:
		return attribution.Reason == "" && attribution.Reference != nil && validRequestAttemptReference(*attribution.Reference)
	case attributionUnavailable:
		switch RequestAttemptUnavailableReason(attribution.Reason) {
		case RequestAttemptAdapterUnavailable, RequestAttemptBindingUnavailable, RequestAttemptAdapterFailed:
			return attribution.Reference == nil
		}
	}
	return false
}

func unavailableRequestLedger(reason string) *RequestLedger {
	return &RequestLedger{
		Status:             RequestLedgerUnavailable,
		UnavailableReason:  reason,
		TranscriptEvidence: unavailableRequestTranscriptEvidence(RequestTranscriptNotObserved),
	}
}

func appendRequestEvent(record *storedRequestReceipt, kind RequestEventKind, at time.Time, delivery RequestDelivery, attribution *RequestAttemptAttribution) error {
	if record == nil || record.Version != 2 || at.IsZero() {
		return ErrRequestConflict
	}
	event := RequestEvent{
		Sequence:      len(record.Events) + 1,
		Kind:          kind,
		SessionID:     record.SessionID,
		Generation:    record.Generation,
		RequestID:     record.RequestID,
		MessageDigest: record.MessageDigest,
		At:            at.UTC(),
		Delivery:      delivery,
	}
	if attribution != nil {
		if !validRequestAttemptAttribution(*attribution) {
			return ErrRequestConflict
		}
		event.Attribution = cloneRequestAttemptAttribution(attribution)
	}
	record.Events = append(record.Events, event)
	return nil
}

func foldRequestEvents(events []RequestEvent) (*RequestLedger, RequestReceipt, error) {
	if len(events) < 2 {
		return nil, RequestReceipt{}, ErrRequestConflict
	}
	first := events[0]
	if first.Kind != RequestEventAccepted || first.Attribution == nil || !validRequestAttemptAttribution(*first.Attribution) || first.Delivery != RequestDeliveryPending {
		return nil, RequestReceipt{}, ErrRequestConflict
	}
	projected := RequestReceipt{
		RequestID: first.RequestID, SessionID: first.SessionID, Generation: first.Generation,
		MessageDigest: first.MessageDigest, AcceptedAt: first.At.UTC(),
		Delivery: RequestDeliveryPending, Effect: "unverified",
	}
	if projected.SessionID == "" || projected.Generation <= 0 || projected.RequestID == "" || !validRequestDigest(projected.MessageDigest) {
		return nil, RequestReceipt{}, ErrRequestConflict
	}
	ledger := &RequestLedger{
		Status:             RequestLedgerAvailable,
		TranscriptEvidence: unavailableRequestTranscriptEvidence(RequestTranscriptNotObserved),
	}
	ledger.AttemptAttribution = cloneRequestAttemptAttribution(first.Attribution)
	effectSeen, attemptSeen, resultSeen, acknowledgementSeen := false, false, false, false
	transcriptObservations := make(map[string]bool)
	for idx, event := range events {
		if event.Sequence != idx+1 || event.SessionID != projected.SessionID || event.Generation != projected.Generation ||
			event.RequestID != projected.RequestID || event.MessageDigest != projected.MessageDigest || event.At.IsZero() {
			return nil, RequestReceipt{}, ErrRequestConflict
		}
		if idx > 0 && event.Attribution != nil {
			return nil, RequestReceipt{}, ErrRequestConflict
		}
		switch event.Kind {
		case RequestEventAccepted:
			if idx != 0 || event.TranscriptEvidence != nil {
				return nil, RequestReceipt{}, ErrRequestConflict
			}
		case RequestEventEffectUnverified:
			if idx != 1 || effectSeen || event.Delivery != "" || event.TranscriptEvidence != nil {
				return nil, RequestReceipt{}, ErrRequestConflict
			}
			effectSeen = true
		case RequestEventDeliveryAttempt:
			if !effectSeen || attemptSeen || resultSeen || event.Delivery != "" || event.TranscriptEvidence != nil {
				return nil, RequestReceipt{}, ErrRequestConflict
			}
			attemptSeen = true
			stamp := event.At.UTC()
			projected.DeliveryAttemptedAt = &stamp
			projected.Delivery = RequestDeliveryUnknown
		case RequestEventProviderResult:
			if !effectSeen || resultSeen || !attemptSeen || event.TranscriptEvidence != nil || (event.Delivery != RequestDeliveryAccepted && event.Delivery != RequestDeliveryQueued && event.Delivery != RequestDeliveryUnknown) {
				return nil, RequestReceipt{}, ErrRequestConflict
			}
			resultSeen = true
			projected.Delivery = event.Delivery
			stamp := event.At.UTC()
			projected.ProviderResultAt = &stamp
		case RequestEventAcknowledged:
			if !effectSeen || acknowledgementSeen || event.Delivery != "" || event.TranscriptEvidence != nil {
				return nil, RequestReceipt{}, ErrRequestConflict
			}
			acknowledgementSeen = true
			stamp := event.At.UTC()
			projected.AcknowledgedAt = &stamp
		case RequestEventTranscriptEvidence:
			if !effectSeen || !attemptSeen || event.Delivery != "" || event.TranscriptEvidence == nil {
				return nil, RequestReceipt{}, ErrRequestConflict
			}
			canonical, err := canonicalRequestTranscriptEvidence(*event.TranscriptEvidence, projected.SessionID, projected.RequestID, projected.Generation)
			if err != nil || !sameRequestTranscriptEvidence(*event.TranscriptEvidence, canonical) {
				return nil, RequestReceipt{}, ErrRequestConflict
			}
			observationKey := canonical.TranscriptStreamID + ":" + canonical.TranscriptGenerationID
			if transcriptObservations[observationKey] {
				return nil, RequestReceipt{}, ErrRequestConflict
			}
			transcriptObservations[observationKey] = true
			ledger.TranscriptEvidence = cloneRequestTranscriptEvidence(&canonical)
		default:
			return nil, RequestReceipt{}, ErrRequestConflict
		}
	}
	if !effectSeen {
		return nil, RequestReceipt{}, ErrRequestConflict
	}
	encoded, err := json.Marshal(events)
	if err != nil {
		return nil, RequestReceipt{}, err
	}
	ledger.Digest = requestDigest(string(encoded))
	ledger.Events = cloneRequestEvents(events)
	return ledger, projected, nil
}

func sameRequestReceiptProjection(stored, projected RequestReceipt) bool {
	return stored.RequestID == projected.RequestID && stored.SessionID == projected.SessionID &&
		stored.Generation == projected.Generation && stored.MessageDigest == projected.MessageDigest &&
		stored.AcceptedAt.Equal(projected.AcceptedAt) && stored.Delivery == projected.Delivery &&
		sameRequestTime(stored.DeliveryAttemptedAt, projected.DeliveryAttemptedAt) &&
		sameRequestTime(stored.ProviderResultAt, projected.ProviderResultAt) &&
		sameRequestTime(stored.AcknowledgedAt, projected.AcknowledgedAt) && stored.Effect == projected.Effect &&
		((stored.Attempt == nil && projected.Attempt == nil) || sameRequestAttemptExact(stored.Attempt, projected.Attempt))
}

func sameRequestTime(left, right *time.Time) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return left.Equal(*right)
}

func cloneRequestAttemptAttribution(value *RequestAttemptAttribution) *RequestAttemptAttribution {
	if value == nil {
		return nil
	}
	cloned := *value
	if value.Reference != nil {
		reference := *value.Reference
		cloned.Reference = &reference
	}
	return &cloned
}

func cloneRequestEvents(events []RequestEvent) []RequestEvent {
	if len(events) == 0 {
		return nil
	}
	cloned := make([]RequestEvent, len(events))
	for idx, event := range events {
		cloned[idx] = event
		cloned[idx].At = event.At.UTC()
		cloned[idx].Attribution = cloneRequestAttemptAttribution(event.Attribution)
		cloned[idx].TranscriptEvidence = cloneRequestTranscriptEvidence(event.TranscriptEvidence)
	}
	return cloned
}
