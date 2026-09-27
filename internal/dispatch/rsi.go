package dispatch

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/reviewquorum"
	"github.com/gastownhall/gascity/internal/rsipolicy"
	"github.com/gastownhall/gascity/internal/storybench"
)

const (
	rsiCandidateEvidenceMissing    = "rsi_candidate_evidence_missing"
	rsiCandidateEvidenceAmbiguous  = "rsi_candidate_evidence_ambiguous"
	rsiCandidateEvidenceMalformed  = "rsi_candidate_evidence_malformed"
	rsiJudgeEvidenceMalformed      = "rsi_judge_evidence_malformed"
	rsiBenchmarkEvidenceAmbiguous  = "rsi_benchmark_evidence_ambiguous"
	rsiBenchmarkEvidenceMalformed  = "rsi_benchmark_evidence_malformed"
	rsiBenchmarkRequirementInvalid = "rsi_benchmark_requirement_invalid"
	rsiBenchmarkSuiteInvalid       = "rsi_benchmark_suite_invalid"
	rsiBenchmarkEvidenceInvalid    = "rsi_benchmark_evidence_invalid"
)

// processRSIPromotionGate evaluates the durable candidate and independent judge
// outputs attached to an RSI gate. The gate is the only writer of the policy
// decision; worker beads remain evidence records and cannot self-promote.
func processRSIPromotionGate(store beads.Store, bead beads.Bead, _ ProcessOptions) (ControlResult, error) {
	deps, err := store.DepList(bead.ID, "down")
	if err != nil {
		return ControlResult{}, fmt.Errorf("%s: listing RSI evidence dependencies: %w", bead.ID, err)
	}

	candidateCount := 0
	var candidate rsipolicy.CandidateEvidence
	var benchmark *storybench.Evidence
	var lanes []reviewquorum.LaneOutput
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
				return closeRSIPromotionGate(store, bead, rsipolicy.Decision{
					Reason:  rsiCandidateEvidenceAmbiguous,
					Reasons: []string{rsiCandidateEvidenceAmbiguous},
				}, beadmeta.OutcomeFail, "rsi-reject")
			}
			if err := decodeRSIJSON(dependency, &candidate); err != nil {
				return closeRSIPromotionGate(store, bead, rsipolicy.Decision{
					Reason:  rsiCandidateEvidenceMalformed,
					Reasons: []string{rsiCandidateEvidenceMalformed},
				}, beadmeta.OutcomeFail, "rsi-reject")
			}
		case beadmeta.RSIRoleJudge:
			var lane reviewquorum.LaneOutput
			if err := decodeRSIJSON(dependency, &lane); err != nil {
				return closeRSIPromotionGate(store, bead, rsipolicy.Decision{
					Reason:  rsiJudgeEvidenceMalformed,
					Reasons: []string{rsiJudgeEvidenceMalformed},
				}, beadmeta.OutcomeFail, "rsi-reject")
			}
			lanes = append(lanes, lane)
		case beadmeta.RSIRoleBenchmark:
			if benchmark != nil {
				return closeRSIPromotionGate(store, bead, rsipolicy.Decision{
					Reason: rsiBenchmarkEvidenceAmbiguous, Reasons: []string{rsiBenchmarkEvidenceAmbiguous},
				}, beadmeta.OutcomeFail, "rsi-reject")
			}
			var evidence storybench.Evidence
			if err := decodeRSIJSON(dependency, &evidence); err != nil {
				return closeRSIPromotionGate(store, bead, rsipolicy.Decision{
					Reason: rsiBenchmarkEvidenceMalformed, Reasons: []string{rsiBenchmarkEvidenceMalformed},
				}, beadmeta.OutcomeFail, "rsi-reject")
			}
			benchmark = &evidence
		}
	}

	if candidateCount == 0 {
		return closeRSIPromotionGate(store, bead, rsipolicy.Decision{
			Reason:  rsiCandidateEvidenceMissing,
			Reasons: []string{rsiCandidateEvidenceMissing},
		}, beadmeta.OutcomeFail, "rsi-reject")
	}

	input := candidate.Input(lanes)
	requirement := strings.TrimSpace(bead.Metadata[beadmeta.RSIStoryRequiredMetadataKey])
	if requirement != "" && requirement != "true" && requirement != "false" {
		return closeRSIPromotionGate(store, bead, rsipolicy.Decision{
			Reason: rsiBenchmarkRequirementInvalid, Reasons: []string{rsiBenchmarkRequirementInvalid},
		}, beadmeta.OutcomeFail, "rsi-reject")
	}
	input.StoryBenchmarkRequired = requirement == "true"
	if input.StoryBenchmarkRequired {
		suitePath := strings.TrimSpace(bead.Metadata[beadmeta.RSISuitePathMetadataKey])
		expectedHash := strings.TrimSpace(bead.Metadata[beadmeta.RSISuiteHashMetadataKey])
		expectedParent := strings.TrimSpace(bead.Metadata[beadmeta.RSIParentBundleMetadataKey])
		if !filepath.IsAbs(suitePath) || expectedHash == "" || expectedParent == "" ||
			input.Current.ID != expectedParent || input.Current.EvalSuiteHash != expectedHash ||
			input.Candidate.EvalSuiteHash != expectedHash {
			return closeRSIPromotionGate(store, bead, rsipolicy.Decision{
				Reason: rsiBenchmarkSuiteInvalid, Reasons: []string{rsiBenchmarkSuiteInvalid},
			}, beadmeta.OutcomeFail, "rsi-reject")
		}
		rawSuite, err := os.ReadFile(suitePath)
		if err != nil || storybench.SuiteHash(rawSuite) != expectedHash {
			return closeRSIPromotionGate(store, bead, rsipolicy.Decision{
				Reason: rsiBenchmarkSuiteInvalid, Reasons: []string{rsiBenchmarkSuiteInvalid},
			}, beadmeta.OutcomeFail, "rsi-reject")
		}
		if benchmark != nil {
			result, err := storybench.Evaluate(rawSuite, benchmark.Baseline, benchmark.Candidate, candidate.Improver)
			if err != nil {
				return closeRSIPromotionGate(store, bead, rsipolicy.Decision{
					Reason: rsiBenchmarkEvidenceInvalid, Reasons: []string{rsiBenchmarkEvidenceInvalid},
				}, beadmeta.OutcomeFail, "rsi-reject")
			}
			input.StoryBenchmark = &result
		}
	} else if benchmark != nil {
		return closeRSIPromotionGate(store, bead, rsipolicy.Decision{
			Reason: rsiBenchmarkRequirementInvalid, Reasons: []string{rsiBenchmarkRequirementInvalid},
		}, beadmeta.OutcomeFail, "rsi-reject")
	}
	// The gate metadata is the authoritative authority class. Candidate workers
	// may report it for evidence, but they must not be able to downgrade a
	// human-approval boundary in their own payload.
	if authorityClass := strings.TrimSpace(bead.Metadata[beadmeta.RSIAuthorityClassMetadataKey]); authorityClass != "" {
		input.AuthorityClass = authorityClass
	}
	decision := rsipolicy.Evaluate(input)
	if decision.ManualApprovalRequired && onlyRSIHumanApprovalReason(decision) {
		return closeRSIPromotionGate(store, bead, decision, beadmeta.OutcomePass, "rsi-human-approval")
	}
	if decision.Promote {
		return closeRSIPromotionGate(store, bead, decision, beadmeta.OutcomePass, "rsi-promote")
	}
	return closeRSIPromotionGate(store, bead, decision, beadmeta.OutcomePass, "rsi-reject")
}

func onlyRSIHumanApprovalReason(decision rsipolicy.Decision) bool {
	return len(decision.Reasons) == 1 && decision.Reasons[0] == rsipolicy.ReasonHumanApprovalRequired
}

func decodeRSIJSON[T any](bead beads.Bead, dst *T) error {
	raw := strings.TrimSpace(bead.Metadata[beadmeta.OutputJSONMetadataKey])
	if raw == "" {
		return fmt.Errorf("%s: %s is empty", bead.ID, beadmeta.OutputJSONMetadataKey)
	}
	if err := json.Unmarshal([]byte(raw), dst); err != nil {
		return fmt.Errorf("%s: decode %s: %w", bead.ID, beadmeta.OutputJSONMetadataKey, err)
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
