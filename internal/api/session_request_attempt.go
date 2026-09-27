package api

import (
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/api/apierr"
	"github.com/gastownhall/gascity/internal/attemptevidence"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/storeref"
)

func validateSessionRequestMetadata(metadata map[string]string) error {
	for key := range metadata {
		if strings.HasPrefix(key, beadmeta.SessionRequestReceiptPrefix) {
			return apierr.Forbidden.Msg("session request receipts require the tracked session protocol")
		}
	}
	return nil
}

// resolveSessionRequestAttempt observes the current authoritative work row.
// This observation is attribution, not permission to change or complete work.
// Session acceptance separately fences the reciprocal claim and generation.
func (s *Server) resolveSessionRequestAttempt(sessionID string, generation int) (*session.RequestAttemptBinding, error) {
	front := session.NewStore(s.state.SessionsBeadStore())
	info, err := front.Get(sessionID)
	if err != nil {
		return nil, err
	}
	if info.Closed || info.Generation != strconv.Itoa(generation) {
		return nil, session.ErrRequestConflict
	}
	claim, err := front.CurrentClaimBeadID(sessionID)
	if err != nil {
		return nil, err
	}
	if claim == "" {
		return nil, nil
	}
	plan, err := storeref.Plan(storeref.ByID{ID: claim, WorkAxis: apiWorkAxis{s}}, s.residencyTopology())
	if err != nil {
		return nil, err
	}
	owner, err := storeref.ResolveOwnerRow(plan, claim)
	if err != nil {
		return nil, err
	}
	work := owner.Bead
	if !owner.Read {
		work, err = owner.Store.Get(claim)
		if err != nil {
			return nil, err
		}
	}
	ref, ok := attemptEvidenceRefForLeg(storeref.Leg{Ref: owner.Ref}, s.state.CityName())
	if !ok || work.ID != claim || work.Revision <= 0 || work.Status != "in_progress" ||
		!slices.Contains(session.AssigneeIdentities(info), work.Assignee) ||
		strings.TrimSpace(work.Metadata[beadmeta.SessionIDMetadataKey]) != sessionID ||
		!attemptevidence.IsExecutionRecord(work) {
		return nil, session.ErrRequestConflict
	}
	identity := attemptevidence.Identity{
		Kind: attemptevidence.KindWorkbench, OwnerBeadID: work.ID, ExecutionBeadID: work.ID,
		SessionID: sessionID, SessionGeneration: info.Generation, ClaimGeneration: strings.TrimSpace(work.Metadata[beadmeta.ClaimGenerationMetadataKey]),
	}
	id, err := attemptevidence.AttemptID(identity)
	if err != nil {
		return nil, err
	}
	return &session.RequestAttemptBinding{StoreRef: ref, AttemptID: id, WorkRevision: strconv.FormatInt(work.Revision, 10), Identity: identity}, nil
}

func (s *Server) acceptAttributedSessionRequest(front *session.Store, sessionID, requestID string, generation int, message string, now time.Time) (session.RequestAcceptance, error) {
	prior, err := front.GetRequest(sessionID, requestID)
	if err != nil && !errors.Is(err, session.ErrRequestNotFound) {
		return session.RequestAcceptance{}, err
	}
	if err == nil && prior.Attempt == nil {
		// A legacy/general receipt retains its original lack of attribution.
		return front.AcceptRequest(sessionID, requestID, generation, message, now)
	}
	binding, err := s.resolveSessionRequestAttempt(sessionID, generation)
	if err != nil {
		return session.RequestAcceptance{}, err
	}
	if binding == nil {
		return front.AcceptRequest(sessionID, requestID, generation, message, now)
	}
	return front.AcceptRequestForAttempt(sessionID, requestID, generation, message, *binding, now)
}

// AttemptRequestRecords reports session receipts bound to the selected
// attempt. UnattributedRequests counts legacy/general requests in the same
// execution generation; these are never assigned to an attempt by inference.
type AttemptRequestRecords struct {
	Status               string                   `json:"status"`
	Reason               string                   `json:"reason,omitempty"`
	Records              []session.RequestReceipt `json:"records,omitempty"`
	UnattributedRequests int                      `json:"unattributed_requests,omitempty"`
}

func (s *Server) attemptAcknowledgements(evidence attemptevidence.Evidence) AttemptRequestRecords {
	result := AttemptRequestRecords{Status: attemptevidence.StatusUnavailable, Reason: "session_requests_lack_verified_work_attempt_attribution"}
	if evidence.Identity.Kind != attemptevidence.KindWorkbench {
		return result
	}
	gen, err := strconv.Atoi(evidence.Identity.SessionGeneration)
	if err != nil || gen <= 0 {
		return result
	}
	front := session.NewStore(s.state.SessionsBeadStore())
	rows, err := front.ListRequests(evidence.Identity.SessionID, gen)
	if err != nil {
		result.Reason = "session_request_ledger_unavailable"
		return result
	}
	for _, receipt := range rows {
		if receipt.Attempt == nil {
			result.UnattributedRequests++
			continue
		}
		binding := receipt.Attempt
		if binding.AttemptID != evidence.AttemptID {
			continue
		}
		if binding.StoreRef != evidence.StoreRef || binding.Identity != evidence.Identity {
			return AttemptRequestRecords{Status: attemptevidence.StatusUnavailable, Reason: "session_request_attempt_binding_conflict"}
		}
		result.Records = append(result.Records, receipt)
	}
	if len(result.Records) > 0 {
		result.Status = attemptevidence.StatusAvailable
		result.Reason = ""
		if result.UnattributedRequests > 0 {
			result.Reason = "additional_requests_lack_attempt_attribution"
		}
	} else if result.UnattributedRequests == 0 {
		result.Status = attemptevidence.StatusMissing
		result.Reason = "no_attributed_session_requests"
	}
	return result
}
