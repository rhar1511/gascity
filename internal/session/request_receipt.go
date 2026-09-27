package session

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// RequestDelivery describes only the provider submission stage of a request.
type RequestDelivery string

const (
	// RequestDeliveryPending means the server accepted the request before a send attempt.
	RequestDeliveryPending RequestDelivery = "pending"
	// RequestDeliveryAccepted records provider acceptance without session acknowledgement.
	RequestDeliveryAccepted RequestDelivery = "accepted"
	// RequestDeliveryQueued records provider queueing without session acknowledgement.
	RequestDeliveryQueued RequestDelivery = "queued"
	// RequestDeliveryUnknown preserves an attempted send whose effect is uncertain.
	RequestDeliveryUnknown RequestDelivery = "unknown"
)

var (
	// ErrRequestConflict rejects conflicting identity, content, or stored evidence.
	ErrRequestConflict = errors.New("session request identity or content conflicts")
	// ErrRequestEvidenceRetained prevents deletion before receipt archival is established.
	ErrRequestEvidenceRetained = errors.New("session has retained request evidence; deletion requires a verified archive")
	// ErrRequestNotFound reports absence of the exact selected request.
	ErrRequestNotFound = errors.New("session request not found")
	// ErrRequestAcknowledgementRejected rejects proof from an unintended execution.
	ErrRequestAcknowledgementRejected = errors.New("session request acknowledgement does not match the intended execution")
)

const requestReceiptPrefix = beadmeta.SessionRequestReceiptPrefix

// RequestReceipt keeps acceptance, provider submission, session acknowledgement,
// and effect evidence separate. An acknowledgement never verifies an effect.
// Execution credentials are deliberately absent from this read model.
type RequestReceipt struct {
	RequestID           string                 `json:"request_id"`
	SessionID           string                 `json:"session_id"`
	Generation          int                    `json:"generation"`
	MessageDigest       string                 `json:"message_digest"`
	AcceptedAt          time.Time              `json:"accepted_at"`
	Delivery            RequestDelivery        `json:"delivery"`
	DeliveryAttemptedAt *time.Time             `json:"delivery_attempted_at,omitempty"`
	ProviderResultAt    *time.Time             `json:"provider_result_at,omitempty"`
	AcknowledgedAt      *time.Time             `json:"acknowledged_at,omitempty"`
	Effect              string                 `json:"effect"`
	Attempt             *RequestAttemptBinding `json:"attempt,omitempty"`
}

type storedRequestReceipt struct {
	Version int `json:"version"`
	RequestReceipt
	ExecutionTokenDigest string `json:"execution_token_digest"`
}

// RequestAcceptance reports whether this call created the durable request.
// Provider delivery needs the separate one-time reservation in SubmitRequest.
type RequestAcceptance struct {
	RequestReceipt
	NewlyAccepted bool `json:"-"`
}

// AcceptRequest durably records a request for the exact current session
// generation before provider delivery. Reusing the ID is idempotent only for
// the same generation and message. Conditional storage is mandatory: there is
// no legacy unconditional implementation of this protocol.
func (s *Store) AcceptRequest(sessionID, requestID string, generation int, message string, now time.Time) (RequestAcceptance, error) {
	return s.acceptRequest(sessionID, requestID, generation, message, nil, now)
}

func (s *Store) acceptRequest(sessionID, requestID string, generation int, message string, binding *RequestAttemptBinding, now time.Time) (RequestAcceptance, error) {
	if generation <= 0 || now.IsZero() || strings.TrimSpace(message) == "" {
		return RequestAcceptance{}, ErrRequestConflict
	}
	created := false
	receipt, err := s.mutateRequestReceipt(sessionID, requestID, func(b beads.Bead, record *storedRequestReceipt) (bool, error) {
		created = false
		info := infoFromPersistedBead(b)
		if info.Closed || info.Generation != strconv.Itoa(generation) || info.InstanceToken == "" {
			return false, ErrRequestConflict
		}
		digest := requestDigest(message)
		tokenDigest := requestDigest(info.InstanceToken)
		if record.Version != 0 {
			if record.Generation != generation || record.MessageDigest != digest || record.ExecutionTokenDigest != tokenDigest {
				return false, ErrRequestConflict
			}
			if (binding != nil && !sameRequestAttempt(record.Attempt, binding)) || !requestAttemptClaimMatches(b, record.Attempt) {
				return false, ErrRequestConflict
			}
			return false, nil
		}
		if !requestAttemptClaimMatches(b, binding) {
			return false, ErrRequestConflict
		}
		*record = storedRequestReceipt{
			Version: 1,
			RequestReceipt: RequestReceipt{
				RequestID: requestID, SessionID: sessionID, Generation: generation,
				MessageDigest: digest, AcceptedAt: now.UTC(),
				Delivery: RequestDeliveryPending, Effect: "unverified",
				Attempt: binding,
			},
			ExecutionTokenDigest: tokenDigest,
		}
		created = true
		return true, nil
	})
	return RequestAcceptance{RequestReceipt: receipt, NewlyAccepted: created && err == nil}, err
}

// GetRequest reads the selected request, including after session generation
// changes or closure. A missing record is never reconstructed from a newer one.
func (s *Store) GetRequest(sessionID, requestID string) (RequestReceipt, error) {
	if err := validateReceiptRequestID(requestID); err != nil {
		return RequestReceipt{}, err
	}
	b, err := s.requestReceiptBead(sessionID)
	if err != nil {
		return RequestReceipt{}, err
	}
	record, err := decodeRequestReceipt(b.Metadata[requestReceiptPrefix+requestID], sessionID, requestID)
	if err != nil {
		return RequestReceipt{}, err
	}
	if record.Version == 0 {
		return RequestReceipt{}, ErrRequestNotFound
	}
	return record.RequestReceipt, nil
}

// ListRequests returns immutable identities for one exact execution generation.
// A session generation can serve multiple work attempts; callers must establish
// explicit work/attempt attribution before joining these receipts to an attempt.
func (s *Store) ListRequests(sessionID string, generation int) ([]RequestReceipt, error) {
	if generation <= 0 {
		return nil, ErrRequestConflict
	}
	b, err := s.requestReceiptBead(sessionID)
	if err != nil {
		return nil, err
	}
	receipts := make([]RequestReceipt, 0)
	for key, raw := range b.Metadata {
		if !strings.HasPrefix(key, requestReceiptPrefix) {
			continue
		}
		id := strings.TrimPrefix(key, requestReceiptPrefix)
		if err := validateReceiptRequestID(id); err != nil {
			return nil, err
		}
		record, err := decodeRequestReceipt(raw, sessionID, id)
		if err != nil {
			return nil, err
		}
		if record.Version == 0 {
			return nil, ErrRequestConflict
		}
		if record.Generation == generation {
			receipts = append(receipts, record.RequestReceipt)
		}
	}
	sort.Slice(receipts, func(i, j int) bool { return receipts[i].RequestID < receipts[j].RequestID })
	return receipts, nil
}

// DeleteClosedSession preserves request history when a caller asks to delete
// a closed session. No archive/deletion policy is configured in this release.
// Direct backend writes remain outside this domain boundary.
func (s *Store) DeleteClosedSession(sessionID string) error {
	b, err := s.requestReceiptBead(sessionID)
	if err != nil {
		return err
	}
	if b.Status != "closed" {
		return ErrRequestConflict
	}
	for key := range b.Metadata {
		if strings.HasPrefix(key, requestReceiptPrefix) {
			return ErrRequestEvidenceRetained
		}
	}
	return s.store.Delete(sessionID)
}

// RecordRequestDelivery records a trusted provider caller's result. Even a
// successful send or queue operation leaves acknowledgement and effect unset.
// Unknown delivery outcomes are retained rather than being retried implicitly.
func (s *Store) RecordRequestDelivery(sessionID, requestID string, generation int, delivery RequestDelivery, now time.Time) (RequestReceipt, error) {
	if now.IsZero() || delivery != RequestDeliveryAccepted && delivery != RequestDeliveryQueued && delivery != RequestDeliveryUnknown {
		return RequestReceipt{}, ErrRequestConflict
	}
	return s.mutateRequestReceipt(sessionID, requestID, func(_ beads.Bead, record *storedRequestReceipt) (bool, error) {
		if record.Version == 0 {
			return false, ErrRequestNotFound
		}
		if record.Generation != generation {
			return false, ErrRequestConflict
		}
		if record.Delivery != RequestDeliveryPending && (record.Delivery != RequestDeliveryUnknown || record.ProviderResultAt != nil || record.DeliveryAttemptedAt == nil) {
			if record.Delivery != delivery {
				return false, ErrRequestConflict
			}
			return false, nil
		}
		record.Delivery = delivery
		stamp := now.UTC()
		record.ProviderResultAt = &stamp
		return true, nil
	})
}

// AcknowledgeRequest accepts a receipt only from the request's intended current
// execution. The caller supplies its incarnation credential; the stored public
// receipt never exposes it. Whole-row CAS rejects a generation change between
// checking identity and writing the receipt. Runtime credential isolation is
// required: read access to another execution's credentials grants its identity.
func (s *Store) AcknowledgeRequest(sessionID, requestID string, generation int, instanceToken string, now time.Time) (RequestReceipt, error) {
	if generation <= 0 || instanceToken == "" || now.IsZero() {
		return RequestReceipt{}, ErrRequestAcknowledgementRejected
	}
	return s.mutateRequestReceipt(sessionID, requestID, func(b beads.Bead, record *storedRequestReceipt) (bool, error) {
		if record.Version == 0 {
			return false, ErrRequestNotFound
		}
		info := infoFromPersistedBead(b)
		proof := requestDigest(instanceToken)
		if info.Closed || record.Generation != generation || info.Generation != strconv.Itoa(generation) ||
			subtle.ConstantTimeCompare([]byte(proof), []byte(record.ExecutionTokenDigest)) != 1 ||
			subtle.ConstantTimeCompare([]byte(proof), []byte(requestDigest(info.InstanceToken))) != 1 {
			return false, ErrRequestAcknowledgementRejected
		}
		if record.AcknowledgedAt != nil {
			return false, nil
		}
		stamp := now.UTC()
		record.AcknowledgedAt = &stamp
		return true, nil
	})
}

func (s *Store) requestReceiptBead(id string) (beads.Bead, error) {
	if s == nil || s.store.Store == nil {
		return beads.Bead{}, ErrSessionNotFound
	}
	b, err := beads.HandlesFor(s.store.Store).Live.Get(id)
	if err != nil {
		return beads.Bead{}, err
	}
	if b.ID == "" || !IsSessionBeadOrRepairable(b) {
		return beads.Bead{}, ErrSessionNotFound
	}
	return b, nil
}

func (s *Store) mutateRequestReceipt(sessionID, requestID string, change func(beads.Bead, *storedRequestReceipt) (bool, error)) (RequestReceipt, error) {
	if err := validateReceiptRequestID(requestID); err != nil {
		return RequestReceipt{}, err
	}
	if s == nil || s.store.Store == nil {
		return RequestReceipt{}, ErrSessionNotFound
	}
	writer, ok := beads.ConditionalWriterFor(s.store.Store)
	if !ok || !beads.InspectConditionalWrites(s.store.Store).Capable {
		return RequestReceipt{}, beads.ErrConditionalWriteUnsupported
	}
	for range 8 {
		b, err := s.requestReceiptBead(sessionID)
		if err != nil {
			return RequestReceipt{}, err
		}
		if b.Revision == 0 {
			return RequestReceipt{}, beads.ErrConditionalWriteUnsupported
		}
		record, err := decodeRequestReceipt(b.Metadata[requestReceiptPrefix+requestID], sessionID, requestID)
		if err != nil {
			return RequestReceipt{}, err
		}
		changed, err := change(b, &record)
		if err != nil || !changed {
			return record.RequestReceipt, err
		}
		raw, err := json.Marshal(record)
		if err != nil {
			return RequestReceipt{}, err
		}
		err = writer.UpdateIfMatch(sessionID, b.Revision, beads.UpdateOpts{Metadata: map[string]string{requestReceiptPrefix + requestID: string(raw)}})
		if err == nil {
			return record.RequestReceipt, nil
		}
		var stale *beads.PreconditionFailedError
		if !errors.As(err, &stale) {
			return RequestReceipt{}, err
		}
	}
	return RequestReceipt{}, fmt.Errorf("session request write contention: %w", ErrRequestConflict)
}

func decodeRequestReceipt(raw, sessionID, requestID string) (storedRequestReceipt, error) {
	var record storedRequestReceipt
	if raw == "" {
		return record, nil
	}
	if err := json.Unmarshal([]byte(raw), &record); err != nil {
		return record, fmt.Errorf("invalid stored session request: %w", err)
	}
	if record.Version != 1 || record.SessionID != sessionID || record.RequestID != requestID || record.Generation <= 0 || record.AcceptedAt.IsZero() || record.Effect != "unverified" || !validRequestDigest(record.MessageDigest) || !validRequestDigest(record.ExecutionTokenDigest) {
		return record, ErrRequestConflict
	}
	if record.Attempt != nil && !validRequestAttemptBinding(*record.Attempt, sessionID, record.Generation) {
		return record, ErrRequestConflict
	}
	switch record.Delivery {
	case RequestDeliveryPending:
		if record.ProviderResultAt != nil {
			return record, ErrRequestConflict
		}
	case RequestDeliveryUnknown:
		if record.DeliveryAttemptedAt == nil && record.ProviderResultAt == nil {
			return record, ErrRequestConflict
		}
	case RequestDeliveryAccepted, RequestDeliveryQueued:
		if record.ProviderResultAt == nil || record.ProviderResultAt.IsZero() {
			return record, ErrRequestConflict
		}
	default:
		return record, ErrRequestConflict
	}
	if record.AcknowledgedAt != nil && record.AcknowledgedAt.IsZero() {
		return record, ErrRequestConflict
	}
	return record, nil
}

func validateReceiptRequestID(id string) error {
	if len(id) == 0 || len(id) > 200 {
		return ErrRequestConflict
	}
	for _, c := range id {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("-_.:", c) {
			continue
		}
		return ErrRequestConflict
	}
	return nil
}

func requestDigest(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

func validRequestDigest(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
