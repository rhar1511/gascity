package worklifecycle

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

const (
	// MaxRecoveryAttempts is the fixed per-work-item automatic intervention
	// budget. It is persisted on the bead and cannot be raised by candidate or
	// caller data.
	MaxRecoveryAttempts = 2

	recoveryStateVersion   = 1
	maxRecoveryCASAttempts = 64
	recoveryDedupKeyPrefix = "gc.lifecycle.recovery_escalation.v1:"
)

var (
	// ErrRecoveryStateInvalid means persisted recovery metadata is malformed or inconsistent.
	ErrRecoveryStateInvalid = errors.New("recovery state is invalid")
	// ErrRecoveryCASContention means metadata CAS did not converge within the retry bound.
	ErrRecoveryCASContention = errors.New("recovery state CAS contention exceeded retry bound")
	// ErrRecoveryEscalationTargetConflict means an existing request names another recipient.
	ErrRecoveryEscalationTargetConflict = errors.New("recovery escalation target conflicts with persisted request")
	// ErrRecoveryScopeMismatch means persisted state belongs to another store scope.
	ErrRecoveryScopeMismatch = errors.New("recovery state scope does not match work item scope")
)

// RecoveryAttempt is a durable reservation made before an automatic recovery
// side effect. Reserving consumes budget even if that side effect later has an
// unknown outcome. IDs are cryptographically random 128-bit values and are
// generated once per call, then reused while that call retries a metadata CAS.
// A later call after an ambiguous error gets a new ID; if the first CAS
// committed, that uncertain attempt remains consumed rather than being silently
// reused. Random ID collisions are possible in theory but negligible at this
// size.
type RecoveryAttempt struct {
	ID               string `json:"id"`
	ReservedAt       string `json:"reserved_at"`
	RequestID        string `json:"request_id,omitempty"`
	RequestDigest    string `json:"request_digest,omitempty"`
	ExpectedRevision int64  `json:"expected_revision,omitempty"`
}

// RecoveryEscalation is a durable request for human attention. It does not
// assert that mail was sent or delivered. The stable DedupKey lets a caller use
// mail.DedupSender when it publishes the request; delivery remains a separate
// outcome.
type RecoveryEscalation struct {
	ID          string `json:"id"`
	Target      string `json:"target"`
	RequestedAt string `json:"requested_at"`
	DedupKey    string `json:"dedup_key"`
}

// RecoveryState is versioned JSON stored under
// beadmeta.LifecycleRecoveryStateMetadataKey on the work item's bead. The
// metadata row scopes state to the backing store, while Scope and WorkItemID
// reject replay across canonical store bindings or work items. Attempts and
// escalation are append-only.
type RecoveryState struct {
	Version    int                 `json:"version"`
	WorkItemID string              `json:"work_item_id"`
	Scope      string              `json:"scope"`
	Attempts   []RecoveryAttempt   `json:"attempts"`
	Escalation *RecoveryEscalation `json:"escalation,omitempty"`
}

// ReserveRecoveryAttempt durably reserves one of two automatic recovery
// attempts before the caller performs any side effect. Reads and rejected
// reservations do not mutate metadata. The caller must perform the effect only
// when reserved is true. A process restart or an unknown effect outcome does
// not restore the budget.
//
// The state is scoped by the bead row in store, beadID, and the caller's
// canonical ScopeForStore value. The explicit scope prevents replay when the
// same bead ID appears in another city or binding. There is no process-local
// counter or fallback write. Stores without metadata CAS return
// beads.ErrConditionalWriteUnsupported and no effect may follow. If this call
// returns an ambiguous CAS error, the caller must not perform an effect; a
// retry is a new reservation and may consume the other budget slot if the
// original reservation reached storage.
func ReserveRecoveryAttempt(store beads.Store, beadID, scope string) (state RecoveryState, reserved bool, err error) {
	if err := validateRecoveryIdentity(store, beadID, scope); err != nil {
		return RecoveryState{}, false, err
	}
	attemptID, err := newRecoveryID()
	if err != nil {
		return RecoveryState{}, false, fmt.Errorf("create recovery attempt ID: %w", err)
	}

	for range maxRecoveryCASAttempts {
		bead, state, raw, err := readRecoveryStateBead(store, beadID, scope)
		if err != nil {
			return RecoveryState{}, false, err
		}
		if len(state.Attempts) >= MaxRecoveryAttempts {
			return state, false, nil
		}

		next := cloneRecoveryState(state)
		next.Attempts = append(next.Attempts, RecoveryAttempt{
			ID:         attemptID,
			ReservedAt: time.Now().UTC().Format(time.RFC3339Nano),
		})
		encoded, err := json.Marshal(next)
		if err != nil {
			return state, false, fmt.Errorf("encode recovery state: %w", err)
		}

		err = beads.UpdateLifecycleRecoveryStateIfMatch(store, beadID, bead.Revision, raw, string(encoded))
		if err != nil {
			if beads.IsPreconditionFailed(err) {
				// Another writer changed the row or state. Re-read it and claim a
				// remaining slot only if the durable count is still below the fixed
				// limit.
				continue
			}
			return state, false, fmt.Errorf("reserve recovery attempt for %q: %w", beadID, err)
		}
		return next, true, nil
	}

	return RecoveryState{}, false, ErrRecoveryCASContention
}

// RequestRecoveryEscalation writes one immutable, deduplicated escalation
// request after both automatic attempts have been reserved. The scope must be
// the same canonical ScopeForStore value used for reservations. Calling it
// before exhaustion is a read-only refusal. Replays with the same target return
// the existing request and newlyRequested=false; a changed target is rejected
// so the request cannot be silently redirected.
//
// This function persists only the request. A controller caller publishes or
// notifies from the returned state and reports that delivery separately.
func RequestRecoveryEscalation(store beads.Store, beadID, scope, target string) (state RecoveryState, newlyRequested bool, err error) {
	if err := validateRecoveryIdentity(store, beadID, scope); err != nil {
		return RecoveryState{}, false, err
	}
	target = strings.TrimSpace(target)
	if target == "" {
		return RecoveryState{}, false, errors.New("recovery escalation target is required")
	}
	requestID, err := newRecoveryID()
	if err != nil {
		return RecoveryState{}, false, fmt.Errorf("create recovery escalation ID: %w", err)
	}

	for range maxRecoveryCASAttempts {
		bead, state, raw, err := readRecoveryStateBead(store, beadID, scope)
		if err != nil {
			return RecoveryState{}, false, err
		}
		if len(state.Attempts) < MaxRecoveryAttempts {
			return state, false, nil
		}
		if state.Escalation != nil {
			if state.Escalation.Target != target {
				return state, false, fmt.Errorf("escalation for %q: %w", beadID, ErrRecoveryEscalationTargetConflict)
			}
			return state, false, nil
		}

		next := cloneRecoveryState(state)
		next.Escalation = &RecoveryEscalation{
			ID:          requestID,
			Target:      target,
			RequestedAt: time.Now().UTC().Format(time.RFC3339Nano),
			DedupKey:    recoveryEscalationDedupKey(scope, beadID, requestID),
		}
		encoded, err := json.Marshal(next)
		if err != nil {
			return state, false, fmt.Errorf("encode recovery escalation: %w", err)
		}

		err = beads.UpdateLifecycleRecoveryStateIfMatch(store, beadID, bead.Revision, raw, string(encoded))
		if err != nil {
			if beads.IsPreconditionFailed(err) {
				continue
			}
			return state, false, fmt.Errorf("request recovery escalation for %q: %w", beadID, err)
		}
		return next, true, nil
	}

	return RecoveryState{}, false, ErrRecoveryCASContention
}

// ReserveRecoveryRequestAttempt binds one intervention reservation to the
// signed work revision. Exact request replay returns the persisted reservation
// without authorizing another side effect. A changed request digest conflicts.
// Only the invocation that wins UpdateIfMatch may submit the nudge; a later
// replay must inspect the session request receipt and preserve unknown outcomes.
func ReserveRecoveryRequestAttempt(store beads.Store, request RecoveryRequest, digest string) (state RecoveryState, reserved bool, err error) {
	state, reserved, _, err = ReserveRecoveryRequestAttemptWithFence(store, request, digest)
	return state, reserved, err
}

// ReserveRecoveryRequestAttemptWithFence is ReserveRecoveryRequestAttempt and,
// for the unique successful CAS caller, also returns the exact revision read
// back after the reservation. Revisions are opaque; callers must not infer this
// value by incrementing ExpectedRevision. Replays and uncertain writes return
// reserved=false and no revision authority.
func ReserveRecoveryRequestAttemptWithFence(store beads.Store, request RecoveryRequest, digest string) (state RecoveryState, reserved bool, reservedRevision int64, err error) {
	if err := validateRecoveryIdentity(store, request.WorkItemID, request.Scope); err != nil {
		return RecoveryState{}, false, 0, err
	}
	if !validRecoveryToken(request.RequestID, 200) || !validDigest(digest) || request.ExpectedRevision <= 0 {
		return RecoveryState{}, false, 0, ErrRecoveryRequestInvalid
	}
	current, state, raw, err := readRecoveryStateBead(store, request.WorkItemID, request.Scope)
	if err != nil {
		return RecoveryState{}, false, 0, err
	}
	if _, ok, err := recoveryAttemptForRequest(state, request.RequestID, digest); err != nil {
		return state, false, 0, err
	} else if ok {
		return state, false, 0, nil
	}
	if current.Revision != request.ExpectedRevision {
		return state, false, 0, ErrRecoveryWorkStale
	}
	if err := validateRecoveryTargetRowFromBead(current, request); err != nil {
		return state, false, 0, err
	}
	if len(state.Attempts) >= MaxRecoveryAttempts {
		return state, false, 0, nil
	}
	_, ok := beads.ConditionalWriterFor(store)
	if !ok {
		return state, false, 0, beads.ErrConditionalWriteUnsupported
	}
	attemptID, err := newRecoveryID()
	if err != nil {
		return state, false, 0, fmt.Errorf("create recovery attempt ID: %w", err)
	}
	next := cloneRecoveryState(state)
	expectedAttempt := RecoveryAttempt{
		ID: attemptID, ReservedAt: time.Now().UTC().Format(time.RFC3339Nano),
		RequestID: request.RequestID, RequestDigest: digest, ExpectedRevision: request.ExpectedRevision,
	}
	next.Attempts = append(next.Attempts, expectedAttempt)
	encoded, err := json.Marshal(next)
	if err != nil {
		return state, false, 0, fmt.Errorf("encode recovery state: %w", err)
	}
	writeErr := beads.UpdateLifecycleRecoveryStateIfMatch(store, request.WorkItemID, request.ExpectedRevision, raw, string(encoded))
	readbackBead, readback, readbackRaw, readErr := readRecoveryStateBead(store, request.WorkItemID, request.Scope)
	if readErr == nil {
		gotAttempt, found, findErr := recoveryAttemptForRequest(readback, request.RequestID, digest)
		if findErr != nil {
			return readback, false, 0, findErr
		}
		if writeErr == nil {
			if !found || readbackBead.ID != request.WorkItemID || readbackRaw != string(encoded) || gotAttempt != expectedAttempt {
				return readback, false, 0, fmt.Errorf("conditional recovery reservation readback differs from the winning write: %w", ErrRecoveryStateInvalid)
			}
			return readback, true, readbackBead.Revision, nil
		}
		if found {
			// A maybe-committed or raced write is durable but its caller did not
			// prove it won. Preserve the reservation and expose no side effect.
			return readback, false, 0, nil
		}
	}
	if writeErr != nil {
		if beads.IsPreconditionFailed(writeErr) {
			return state, false, 0, ErrRecoveryWorkStale
		}
		return state, false, 0, errors.Join(writeErr, readErr)
	}
	if readErr != nil {
		return next, false, 0, fmt.Errorf("verify recovery reservation readback: %w", readErr)
	}
	// An unrelated row mutation may have committed after the reservation. The
	// exact state readback above is required before the caller can submit.
	return readback, false, 0, ErrRecoveryStateInvalid
}

// RecoveryRequestAttempt reports whether the exact signed request has already
// consumed a durable reservation. It is read-only and does not authorize an
// effect. It supports receipt inspection after expiry or controller restart.
func RecoveryRequestAttempt(store beads.Store, request RecoveryRequest, digest string) (RecoveryState, RecoveryAttempt, bool, error) {
	if err := validateRecoveryIdentity(store, request.WorkItemID, request.Scope); err != nil {
		return RecoveryState{}, RecoveryAttempt{}, false, err
	}
	if !validRecoveryToken(request.RequestID, 200) || !validDigest(digest) {
		return RecoveryState{}, RecoveryAttempt{}, false, ErrRecoveryRequestInvalid
	}
	state, _, err := readRecoveryState(store, request.WorkItemID, request.Scope)
	if err != nil {
		return RecoveryState{}, RecoveryAttempt{}, false, err
	}
	attempt, found, err := recoveryAttemptForRequest(state, request.RequestID, digest)
	return state, attempt, found, err
}

// ValidateRecoveryTargetTuple verifies the live owner/session tuple and holds
// without comparing the revision. Use only after a successful reservation
// whose exact readback revision is separately checked.
func ValidateRecoveryTargetTuple(work beads.Bead, request RecoveryRequest) error {
	if work.ID != request.WorkItemID || work.Status != "in_progress" || work.Assignee != request.Owner ||
		work.Metadata[beadmeta.ClaimGenerationMetadataKey] != request.ClaimGeneration ||
		work.Metadata[beadmeta.SessionIDMetadataKey] != request.SessionID || hasLifecycleHold(work) {
		return ErrRecoveryWorkStale
	}
	return nil
}

func recoveryAttemptForRequest(state RecoveryState, requestID, digest string) (RecoveryAttempt, bool, error) {
	for _, attempt := range state.Attempts {
		if attempt.RequestID != requestID {
			continue
		}
		if attempt.RequestDigest != digest {
			return RecoveryAttempt{}, false, ErrRecoveryRequestConflict
		}
		return attempt, true, nil
	}
	return RecoveryAttempt{}, false, nil
}

func validateRecoveryTargetRowFromBead(work beads.Bead, request RecoveryRequest) error {
	if work.ID != request.WorkItemID || work.Revision != request.ExpectedRevision || work.Status != "in_progress" ||
		work.Assignee != request.Owner || work.Metadata[beadmeta.ClaimGenerationMetadataKey] != request.ClaimGeneration ||
		work.Metadata[beadmeta.SessionIDMetadataKey] != request.SessionID || hasLifecycleHold(work) {
		return ErrRecoveryWorkStale
	}
	return nil
}

func validDigest(digest string) bool {
	if len(digest) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(digest)
	return err == nil
}

func validateRecoveryIdentity(store beads.Store, beadID, scope string) error {
	if store == nil {
		return errors.New("recovery store is required")
	}
	if strings.TrimSpace(beadID) == "" || strings.TrimSpace(beadID) != beadID {
		return errors.New("recovery bead ID must be non-empty and trimmed")
	}
	if strings.TrimSpace(scope) == "" || strings.TrimSpace(scope) != scope {
		return errors.New("recovery scope must be non-empty and trimmed")
	}
	return nil
}

func readRecoveryState(store beads.Store, beadID, scope string) (RecoveryState, string, error) {
	_, state, raw, err := readRecoveryStateBead(store, beadID, scope)
	return state, raw, err
}

func readRecoveryStateBead(store beads.Store, beadID, scope string) (beads.Bead, RecoveryState, string, error) {
	bead, err := store.Get(beadID)
	if err != nil {
		return beads.Bead{}, RecoveryState{}, "", fmt.Errorf("read recovery state for %q: %w", beadID, err)
	}
	raw := bead.Metadata[beadmeta.LifecycleRecoveryStateMetadataKey]
	if raw == "" {
		return bead, RecoveryState{
			Version:    recoveryStateVersion,
			WorkItemID: beadID,
			Scope:      scope,
			Attempts:   []RecoveryAttempt{},
		}, raw, nil
	}
	state, err := decodeRecoveryState(raw, beadID, scope)
	if err != nil {
		return bead, RecoveryState{}, raw, err
	}
	return bead, state, raw, nil
}

func decodeRecoveryState(raw, beadID, scope string) (RecoveryState, error) {
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	var state RecoveryState
	if err := decoder.Decode(&state); err != nil {
		return RecoveryState{}, fmt.Errorf("decode recovery state for %q: %w: %w", beadID, ErrRecoveryStateInvalid, err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return RecoveryState{}, fmt.Errorf("decode recovery state for %q: %w: trailing JSON data", beadID, ErrRecoveryStateInvalid)
	}
	if err := validateRecoveryState(state, beadID, scope); err != nil {
		return RecoveryState{}, err
	}
	return state, nil
}

func validateRecoveryState(state RecoveryState, beadID, scope string) error {
	if state.Version != recoveryStateVersion || state.WorkItemID != beadID || state.Attempts == nil || len(state.Attempts) > MaxRecoveryAttempts {
		return fmt.Errorf("recovery state for %q: %w: version, identity, or attempt count is invalid", beadID, ErrRecoveryStateInvalid)
	}
	if state.Scope != scope {
		return fmt.Errorf("recovery state for %q has scope %q, want %q: %w", beadID, state.Scope, scope, ErrRecoveryScopeMismatch)
	}
	seen := make(map[string]struct{}, len(state.Attempts))
	seenRequests := make(map[string]string, len(state.Attempts))
	for _, attempt := range state.Attempts {
		if !validRecoveryID(attempt.ID) {
			return fmt.Errorf("recovery state for %q: %w: attempt ID is invalid", beadID, ErrRecoveryStateInvalid)
		}
		if _, ok := seen[attempt.ID]; ok {
			return fmt.Errorf("recovery state for %q: %w: duplicate attempt ID", beadID, ErrRecoveryStateInvalid)
		}
		seen[attempt.ID] = struct{}{}
		if _, err := time.Parse(time.RFC3339Nano, attempt.ReservedAt); err != nil {
			return fmt.Errorf("recovery state for %q: %w: attempt timestamp is invalid", beadID, ErrRecoveryStateInvalid)
		}
		if attempt.RequestID != "" || attempt.RequestDigest != "" || attempt.ExpectedRevision != 0 {
			if !validRecoveryToken(attempt.RequestID, 200) || !validDigest(attempt.RequestDigest) || attempt.ExpectedRevision <= 0 {
				return fmt.Errorf("recovery state for %q: %w: request-bound attempt is incomplete", beadID, ErrRecoveryStateInvalid)
			}
			if previous, duplicate := seenRequests[attempt.RequestID]; duplicate {
				if previous != attempt.RequestDigest {
					return fmt.Errorf("recovery state for %q: %w: request ID has conflicting digests", beadID, ErrRecoveryStateInvalid)
				}
				return fmt.Errorf("recovery state for %q: %w: duplicate request ID", beadID, ErrRecoveryStateInvalid)
			}
			seenRequests[attempt.RequestID] = attempt.RequestDigest
		}
	}
	if state.Escalation == nil {
		return nil
	}
	if len(state.Attempts) != MaxRecoveryAttempts || !validRecoveryID(state.Escalation.ID) || strings.TrimSpace(state.Escalation.Target) == "" || state.Escalation.Target != strings.TrimSpace(state.Escalation.Target) || state.Escalation.DedupKey != recoveryEscalationDedupKey(scope, beadID, state.Escalation.ID) {
		return fmt.Errorf("recovery state for %q: %w: escalation is inconsistent", beadID, ErrRecoveryStateInvalid)
	}
	if _, ok := seen[state.Escalation.ID]; ok {
		return fmt.Errorf("recovery state for %q: %w: escalation ID duplicates attempt ID", beadID, ErrRecoveryStateInvalid)
	}
	if _, err := time.Parse(time.RFC3339Nano, state.Escalation.RequestedAt); err != nil {
		return fmt.Errorf("recovery state for %q: %w: escalation timestamp is invalid", beadID, ErrRecoveryStateInvalid)
	}
	return nil
}

func cloneRecoveryState(state RecoveryState) RecoveryState {
	state.Attempts = append([]RecoveryAttempt(nil), state.Attempts...)
	if state.Escalation != nil {
		escalation := *state.Escalation
		state.Escalation = &escalation
	}
	return state
}

func newRecoveryID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}

func recoveryEscalationDedupKey(scope, beadID, requestID string) string {
	material := scope + "\x00" + beadID + "\x00" + requestID
	digest := sha256.Sum256([]byte(material))
	return recoveryDedupKeyPrefix + hex.EncodeToString(digest[:])
}

func validRecoveryID(id string) bool {
	if len(id) != 32 {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}
