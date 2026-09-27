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
	ID         string `json:"id"`
	ReservedAt string `json:"reserved_at"`
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
		state, raw, err := readRecoveryState(store, beadID, scope)
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

		outcome, err := beads.ApplyMetadataCAS(store, beadID, beadmeta.LifecycleRecoveryStateMetadataKey, raw, string(encoded))
		if err != nil {
			return state, false, fmt.Errorf("reserve recovery attempt for %q: %w", beadID, err)
		}
		switch outcome {
		case beads.MetadataCASSwapped, beads.MetadataCASAlreadyNext:
			return next, true, nil
		case beads.MetadataCASConflict:
			// Another caller changed the state. Re-read it and claim a remaining
			// slot only if the durable count is still below the fixed limit.
			continue
		default:
			return state, false, fmt.Errorf("reserve recovery attempt for %q: unexpected metadata CAS outcome %q", beadID, outcome)
		}
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
		state, raw, err := readRecoveryState(store, beadID, scope)
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

		outcome, err := beads.ApplyMetadataCAS(store, beadID, beadmeta.LifecycleRecoveryStateMetadataKey, raw, string(encoded))
		if err != nil {
			return state, false, fmt.Errorf("request recovery escalation for %q: %w", beadID, err)
		}
		switch outcome {
		case beads.MetadataCASSwapped:
			return next, true, nil
		case beads.MetadataCASAlreadyNext:
			// The same request is already durable. Callers should still observe
			// and publish state.Escalation when delivery is pending.
			return next, false, nil
		case beads.MetadataCASConflict:
			continue
		default:
			return state, false, fmt.Errorf("request recovery escalation for %q: unexpected metadata CAS outcome %q", beadID, outcome)
		}
	}

	return RecoveryState{}, false, ErrRecoveryCASContention
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
	bead, err := store.Get(beadID)
	if err != nil {
		return RecoveryState{}, "", fmt.Errorf("read recovery state for %q: %w", beadID, err)
	}
	raw := bead.Metadata[beadmeta.LifecycleRecoveryStateMetadataKey]
	if raw == "" {
		return RecoveryState{
			Version:    recoveryStateVersion,
			WorkItemID: beadID,
			Scope:      scope,
			Attempts:   []RecoveryAttempt{},
		}, raw, nil
	}
	state, err := decodeRecoveryState(raw, beadID, scope)
	if err != nil {
		return RecoveryState{}, raw, err
	}
	return state, raw, nil
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
