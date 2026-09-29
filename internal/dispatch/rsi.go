package dispatch

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/rsipolicy"
)

const (
	rsiCandidateEvidenceMissing     = "rsi_candidate_evidence_missing"
	rsiCandidateEvidenceAmbiguous   = "rsi_candidate_evidence_ambiguous"
	rsiCandidateEvidenceMalformed   = "rsi_candidate_evidence_malformed"
	rsiJudgeEvidenceMalformed       = "rsi_judge_evidence_malformed"
	rsiEvidenceRoleUnknown          = "rsi_evidence_role_unknown"
	rsiTrustedEvaluationUnavailable = "rsi_trusted_evaluation_unavailable"
)

// processRSIPromotionGate evaluates only controller-resolved trusted policy
// evidence. Durable worker outputs identify the proposed candidate and judge
// reports; no worker-supplied policy, limits, metrics, attempts, or authority
// fields are used to build the promotion input.
func processRSIPromotionGate(store beads.Store, bead beads.Bead, opts ProcessOptions) (ControlResult, error) {
	deps, err := store.DepList(bead.ID, "down")
	if err != nil {
		return ControlResult{}, fmt.Errorf("%s: listing RSI evidence dependencies: %w", bead.ID, err)
	}

	var candidate rsipolicy.CandidateRecord
	var candidateCount int
	var judges []rsipolicy.JudgeRecord
	for _, dep := range deps {
		if dep.Type != "blocks" && dep.Type != "" {
			continue
		}
		dependency, getErr := store.Get(dep.DependsOnID)
		if getErr != nil {
			return ControlResult{}, fmt.Errorf("%s: loading RSI evidence bead %s: %w", bead.ID, dep.DependsOnID, getErr)
		}
		switch strings.TrimSpace(dependency.Metadata[beadmeta.RSIRoleMetadataKey]) {
		case beadmeta.RSIRoleImprover:
			candidateCount++
			if candidateCount > 1 {
				return closeRSIPromotionGate(store, bead, rsiReject(rsiCandidateEvidenceAmbiguous), beadmeta.OutcomeFail, "rsi-reject")
			}
			actual, resolveErr := resolveRSIWorkerExecution(store, dependency)
			if resolveErr != nil || dependency.Metadata[beadmeta.OutputJSONMetadataKey] != actual.Metadata[beadmeta.OutputJSONMetadataKey] {
				return closeRSIPromotionGate(store, bead, rsiReject(rsiTrustedEvaluationUnavailable), beadmeta.OutcomeFail, "rsi-reject")
			}
			rawOutput := actual.Metadata[beadmeta.OutputJSONMetadataKey]
			var proposal rsipolicy.CandidateProposal
			if err := decodeRSIOutput(rawOutput, &proposal); err != nil {
				return closeRSIPromotionGate(store, bead, rsiReject(rsiCandidateEvidenceMalformed), beadmeta.OutcomeFail, "rsi-reject")
			}
			candidate = rsipolicy.CandidateRecord{
				BeadID:        actual.ID,
				ControlBeadID: dependency.ID,
				Attempt:       beadmeta.RetryAttemptNumber(actual.Metadata),
				MaxAttempts:   retryMaxAttempts(dependency),
				ActorID:       strings.TrimSpace(actual.Assignee),
				SessionID:     strings.TrimSpace(actual.Metadata[beadmeta.SessionIDMetadataKey]),
				Status:        strings.TrimSpace(actual.Status),
				Outcome:       strings.TrimSpace(actual.Metadata[beadmeta.OutcomeMetadataKey]),
				RawOutput:     rawOutput,
				Proposal:      proposal,
			}
		case beadmeta.RSIRoleJudge:
			actual, resolveErr := resolveRSIWorkerExecution(store, dependency)
			if resolveErr != nil || dependency.Metadata[beadmeta.OutputJSONMetadataKey] != actual.Metadata[beadmeta.OutputJSONMetadataKey] {
				return closeRSIPromotionGate(store, bead, rsiReject(rsiTrustedEvaluationUnavailable), beadmeta.OutcomeFail, "rsi-reject")
			}
			rawOutput := actual.Metadata[beadmeta.OutputJSONMetadataKey]
			var lane rsipolicy.JudgeRecord
			if err := decodeRSIOutput(rawOutput, &lane.Lane); err != nil {
				return closeRSIPromotionGate(store, bead, rsiReject(rsiJudgeEvidenceMalformed), beadmeta.OutcomeFail, "rsi-reject")
			}
			lane.BeadID = actual.ID
			lane.ControlBeadID = dependency.ID
			lane.ActorID = strings.TrimSpace(actual.Assignee)
			lane.SessionID = strings.TrimSpace(actual.Metadata[beadmeta.SessionIDMetadataKey])
			lane.Status = strings.TrimSpace(actual.Status)
			lane.Outcome = strings.TrimSpace(actual.Metadata[beadmeta.OutcomeMetadataKey])
			lane.RawOutput = rawOutput
			judges = append(judges, lane)
		default:
			return closeRSIPromotionGate(store, bead, rsiReject(rsiEvidenceRoleUnknown), beadmeta.OutcomeFail, "rsi-reject")
		}
	}

	if candidateCount == 0 {
		return closeRSIPromotionGate(store, bead, rsiReject(rsiCandidateEvidenceMissing), beadmeta.OutcomeFail, "rsi-reject")
	}
	if opts.ResolveRSIEvaluation == nil {
		return closeRSIPromotionGate(store, bead, rsiReject(rsiTrustedEvaluationUnavailable), beadmeta.OutcomeFail, "rsi-reject")
	}
	trusted, err := opts.ResolveRSIEvaluation(opts.Context, rsipolicy.ResolveRequest{Candidate: candidate, Judges: judges})
	if err != nil {
		if errors.Is(err, rsipolicy.ErrTrustedEvaluationPending) {
			return ControlResult{}, fmt.Errorf("%w: %w", ErrControlPending, err)
		}
		return closeRSIPromotionGate(store, bead, rsiReject(rsiTrustedEvaluationUnavailable), beadmeta.OutcomeFail, "rsi-reject")
	}
	trusted.Input.HumanApprovalVerified = trusted.HumanApprovalVerified
	decision := rsipolicy.Evaluate(trusted.Input)
	if decision.ManualApprovalRequired && onlyRSIHumanApprovalReason(decision) {
		return ControlResult{}, fmt.Errorf("%w: human signature required for this evaluation", ErrControlPending)
	}
	if decision.Promote {
		return closeRSIPromotionGate(store, bead, decision, beadmeta.OutcomePass, "rsi-promote")
	}
	return closeRSIPromotionGate(store, bead, decision, beadmeta.OutcomePass, "rsi-reject")
}

// resolveRSIWorkerExecution follows a retry control to the exact successful
// attempt that supplied its output. Retry controls intentionally do not copy
// assignment/session metadata from workers, so those values must come from the
// closed attempt bead rather than the logical control bead.
func resolveRSIWorkerExecution(store beads.Store, logical beads.Bead) (beads.Bead, error) {
	if logical.Status != "closed" || logical.Metadata[beadmeta.OutcomeMetadataKey] != beadmeta.OutcomePass {
		return beads.Bead{}, fmt.Errorf("logical RSI worker %s is not closed with pass", logical.ID)
	}
	actual := logical
	if logical.Metadata[beadmeta.KindMetadataKey] == beadmeta.KindRetry {
		attempt, err := findLatestAttempt(store, logical)
		if err != nil {
			return beads.Bead{}, err
		}
		if attempt.ID == "" || attempt.Status != "closed" || attempt.Metadata[beadmeta.OutcomeMetadataKey] != beadmeta.OutcomePass {
			return beads.Bead{}, fmt.Errorf("RSI retry control %s has no passing closed attempt", logical.ID)
		}
		actual = attempt
		closedBy, parseErr := strconv.Atoi(strings.TrimSpace(logical.Metadata[beadmeta.ClosedByAttemptMetadataKey]))
		if parseErr != nil || closedBy < 1 || beadmeta.RetryAttemptNumber(actual.Metadata) != closedBy {
			return beads.Bead{}, fmt.Errorf("RSI retry control %s does not bind its successful execution to gc.closed_by_attempt", logical.ID)
		}
	}
	if actual.Metadata[beadmeta.RSIRoleMetadataKey] != logical.Metadata[beadmeta.RSIRoleMetadataKey] {
		return beads.Bead{}, fmt.Errorf("RSI retry execution %s does not carry the logical worker role", actual.ID)
	}
	if strings.TrimSpace(actual.Assignee) == "" || strings.TrimSpace(actual.Metadata[beadmeta.SessionIDMetadataKey]) == "" {
		return beads.Bead{}, fmt.Errorf("RSI worker execution %s lacks assigned actor or session", actual.ID)
	}
	return actual, nil
}

func retryMaxAttempts(control beads.Bead) int {
	maxAttempts, _ := strconv.Atoi(strings.TrimSpace(control.Metadata[beadmeta.MaxAttemptsMetadataKey]))
	return maxAttempts
}

func rsiReject(reason string) rsipolicy.Decision {
	return rsipolicy.Decision{Reason: reason, Reasons: []string{reason}}
}

func onlyRSIHumanApprovalReason(decision rsipolicy.Decision) bool {
	return len(decision.Reasons) == 1 && decision.Reasons[0] == rsipolicy.ReasonHumanApprovalRequired
}

func decodeRSIOutput[T any](raw string, dst *T) error {
	if strings.TrimSpace(raw) == "" {
		return fmt.Errorf("%s is empty", beadmeta.OutputJSONMetadataKey)
	}
	if err := json.Unmarshal([]byte(raw), dst); err != nil {
		return fmt.Errorf("decode %s: %w", beadmeta.OutputJSONMetadataKey, err)
	}
	return nil
}

func closeRSIPromotionGate(store beads.Store, bead beads.Bead, decision rsipolicy.Decision, outcome, action string) (ControlResult, error) {
	output, err := json.Marshal(decision)
	if err != nil {
		return ControlResult{}, fmt.Errorf("%s: marshal RSI decision: %w", bead.ID, err)
	}
	metadata := map[string]string{
		beadmeta.OutputJSONMetadataKey:        string(output),
		beadmeta.OutcomeMetadataKey:           outcome,
		beadmeta.RSIPromoteMetadataKey:        fmt.Sprintf("%t", decision.Promote),
		beadmeta.RSIReasonMetadataKey:         decision.Reason,
		beadmeta.RSIManualApprovalMetadataKey: fmt.Sprintf("%t", decision.ManualApprovalRequired),
	}
	if err := updateMetadataAndClose(store, bead.ID, metadata); err != nil {
		return ControlResult{}, fmt.Errorf("%s: closing RSI promotion gate: %w", bead.ID, err)
	}
	return ControlResult{Processed: true, Action: action}, nil
}
