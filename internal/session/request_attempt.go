package session

import (
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/attemptevidence"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// RequestAttemptBinding records the work identity observed by the controller
// when it accepted a request. It does not grant continued ownership or prove
// that the request had an effect. WorkRevision is the original observation;
// retries for this identity preserve it instead of rewriting history.
type RequestAttemptBinding struct {
	StoreRef     string                   `json:"store_ref"`
	AttemptID    string                   `json:"attempt_id"`
	WorkRevision string                   `json:"work_revision"`
	Identity     attemptevidence.Identity `json:"identity"`
}

// AcceptRequestForAttempt records attribution supplied by trusted controller
// composition after verifying the authoritative work row and store. The
// session CAS additionally checks its generation and reciprocal work claim.
// Callers must never pass unverified client attribution to this method.
func (s *Store) AcceptRequestForAttempt(sessionID, requestID string, generation int, message string, binding RequestAttemptBinding, now time.Time) (RequestAcceptance, error) {
	if !validRequestAttemptBinding(binding, sessionID, generation) {
		return RequestAcceptance{}, ErrRequestConflict
	}
	return s.acceptRequest(sessionID, requestID, generation, message, &binding, now)
}

func validRequestAttemptBinding(binding RequestAttemptBinding, sessionID string, generation int) bool {
	revision, err := strconv.ParseInt(binding.WorkRevision, 10, 64)
	if err != nil || revision <= 0 || strconv.FormatInt(revision, 10) != binding.WorkRevision || binding.StoreRef != strings.TrimSpace(binding.StoreRef) ||
		(!strings.HasPrefix(binding.StoreRef, "city:") && !strings.HasPrefix(binding.StoreRef, "rig:")) ||
		strings.TrimSpace(strings.SplitN(binding.StoreRef, ":", 2)[1]) == "" ||
		binding.Identity.Kind != attemptevidence.KindWorkbench || binding.Identity.OwnerBeadID != binding.Identity.ExecutionBeadID ||
		binding.Identity.SessionID != sessionID || binding.Identity.SessionGeneration != strconv.Itoa(generation) {
		return false
	}
	id, err := attemptevidence.AttemptID(binding.Identity)
	return err == nil && id == binding.AttemptID
}

func sameRequestAttempt(left, right *RequestAttemptBinding) bool {
	return left != nil && right != nil && left.StoreRef == right.StoreRef &&
		left.AttemptID == right.AttemptID && left.Identity == right.Identity
}

func requestAttemptClaimMatches(current beads.Bead, binding *RequestAttemptBinding) bool {
	return binding == nil || strings.TrimSpace(current.Metadata[beadmeta.CurrentClaimBeadIDMetadataKey]) == binding.Identity.ExecutionBeadID
}
