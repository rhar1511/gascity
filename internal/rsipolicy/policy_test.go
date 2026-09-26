package rsipolicy

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/reviewquorum"
)

func passingInput() Input {
	workUnits := []string{"work-01", "work-02", "work-03", "work-04", "work-05", "work-06", "work-07", "work-08", "work-09", "work-10"}
	return Input{
		Objective:      "reduce discovery latency without changing behavior",
		AuthorityClass: "optimization",
		Current: Bundle{
			ID:             "bundle-001",
			TypesCommit:    strings.Repeat("1", 40),
			AICommit:       strings.Repeat("2", 40),
			FrontendCommit: strings.Repeat("3", 40),
			InktreeCommit:  strings.Repeat("4", 40),
			EvalSuiteHash:  strings.Repeat("a", 64),
		},
		Candidate: Bundle{
			ID:             "bundle-002",
			TypesCommit:    strings.Repeat("1", 40),
			AICommit:       strings.Repeat("5", 40),
			FrontendCommit: strings.Repeat("3", 40),
			InktreeCommit:  strings.Repeat("4", 40),
			EvalSuiteHash:  strings.Repeat("a", 64),
			Parent:         "bundle-001",
		},
		Baseline: Metrics{
			Score:           0.80,
			SafetyScore:     0.96,
			LatencyMS:       100,
			CostUSD:         1.00,
			DependencyCount: 3,
		},
		CandidateMetrics: Metrics{
			Score:           0.90,
			SafetyScore:     0.97,
			LatencyMS:       110,
			CostUSD:         1.10,
			DependencyCount: 3,
		},
		Limits: Limits{
			MinSafetyScore:  0.95,
			MaxLatencyMS:    200,
			MaxCostUSD:      2.00,
			MaxDependencies: 4,
		},
		Review: Review{
			Improver:        "jules",
			ImproverSession: "session-improver",
			Judges:          []string{"correctness", "performance"},
			Executions: []JudgeExecutionIdentity{
				{LaneID: "correctness", ActorID: "judge-a", SessionID: "session-a"},
				{LaneID: "performance", ActorID: "judge-b", SessionID: "session-b"},
			},
			Summary: reviewquorum.Summary{
				Subject: "candidate:bundle-002",
				BaseRef: "bundle-001",
				Verdict: reviewquorum.VerdictPass,
				Lanes: []reviewquorum.LaneOutput{
					{LaneID: "correctness", Verdict: reviewquorum.VerdictPass},
					{LaneID: "performance", Verdict: reviewquorum.VerdictPass},
				},
			},
		},
		Attempts:    1,
		MaxAttempts: 3,
		WorkAccounting: WorkAccounting{
			AcceptanceSetSHA256:    HashAcceptanceUnitIDs(workUnits),
			AcceptanceUnitIDs:      workUnits,
			BaselineUsefulUnitIDs:  []string{"work-01", "work-02", "work-03", "work-04", "work-05", "work-06", "work-07", "work-08"},
			CandidateUsefulUnitIDs: []string{"work-01", "work-02", "work-03", "work-04", "work-05", "work-06", "work-07", "work-08", "work-09"},
			BaselineWorkerTime:     WorkerTime{ImplementationHours: 8, RecoveryHours: 1, EvaluationHours: 1},
			CandidateWorkerTime:    WorkerTime{ImplementationHours: 7, RecoveryHours: 1, EvaluationHours: 2},
		},
		HumanApprovalVerified: true,
	}
}

func TestEvaluatePromotesCandidateWhenAllGatesPass(t *testing.T) {
	decision := Evaluate(passingInput())
	if !decision.Promote {
		t.Fatalf("Promote = false, reasons: %v", decision.Reasons)
	}
	if decision.Reason != ReasonPromoted {
		t.Fatalf("Reason = %q, want %q", decision.Reason, ReasonPromoted)
	}
	if decision.RollbackBundleID != "bundle-001" {
		t.Fatalf("RollbackBundleID = %q, want bundle-001", decision.RollbackBundleID)
	}
}

func TestEvaluateRejectsRegressionAndHardLimitViolations(t *testing.T) {
	input := passingInput()
	input.CandidateMetrics.Score = input.Baseline.Score
	input.CandidateMetrics.SafetyScore = 0.90
	input.CandidateMetrics.LatencyMS = 250
	input.CandidateMetrics.CostUSD = 2.50
	input.CandidateMetrics.DependencyCount = 5

	decision := Evaluate(input)
	if decision.Promote {
		t.Fatal("Promote = true, want false")
	}
	for _, reason := range []string{
		ReasonScoreNotImproved,
		ReasonSafetyLimitExceeded,
		ReasonLatencyLimitExceeded,
		ReasonCostLimitExceeded,
		ReasonDependencyLimitExceeded,
	} {
		if !containsReason(decision.Reasons, reason) {
			t.Errorf("reasons %v do not contain %q", decision.Reasons, reason)
		}
	}
}

func TestEvaluateRequiresLineageAndIndependentJudges(t *testing.T) {
	input := passingInput()
	input.Candidate.Parent = "different-bundle"
	input.Review.Judges = []string{"jules", "performance"}
	input.Review.Summary.Lanes = input.Review.Summary.Lanes[:1]

	decision := Evaluate(input)
	if decision.Promote {
		t.Fatal("Promote = true, want false")
	}
	for _, reason := range []string{
		ReasonParentMismatch,
		ReasonImproverJudgerOverlap,
		ReasonInsufficientJudges,
	} {
		if !containsReason(decision.Reasons, reason) {
			t.Errorf("reasons %v do not contain %q", decision.Reasons, reason)
		}
	}
}

func TestEvaluateRejectsChangedEvaluationSuiteAndBudgetExhaustion(t *testing.T) {
	input := passingInput()
	input.Candidate.EvalSuiteHash = "suite-v2"
	input.Attempts = input.MaxAttempts

	decision := Evaluate(input)
	if decision.Promote {
		t.Fatal("Promote = true, want false")
	}
	for _, reason := range []string{ReasonEvalSuiteChanged, ReasonAttemptBudgetExhausted} {
		if !containsReason(decision.Reasons, reason) {
			t.Errorf("reasons %v do not contain %q", decision.Reasons, reason)
		}
	}
}

func TestEvaluateRejectsInvalidNumbersAndReviewFailure(t *testing.T) {
	input := passingInput()
	input.CandidateMetrics.Score = -1
	input.CandidateMetrics.LatencyMS = -1
	input.Review.Summary.Verdict = reviewquorum.VerdictFail

	decision := Evaluate(input)
	if decision.Promote {
		t.Fatal("Promote = true, want false")
	}
	if !containsReason(decision.Reasons, ReasonInvalidMetrics) {
		t.Errorf("reasons %v do not contain %q", decision.Reasons, ReasonInvalidMetrics)
	}
	if !containsReason(decision.Reasons, ReasonReviewFailed) {
		t.Errorf("reasons %v do not contain %q", decision.Reasons, ReasonReviewFailed)
	}
}

func containsReason(reasons []string, want string) bool {
	for _, reason := range reasons {
		if strings.HasPrefix(reason, want) {
			return true
		}
	}
	return false
}

func TestReviewFinalizedSummaryUsesGoQuorumFinalizer(t *testing.T) {
	input := passingInput()
	input.Review.Summary = reviewquorum.Summary{}
	input.Review.Subject = "candidate:bundle-002"
	input.Review.BaseRef = "bundle-001"
	input.Review.LaneOutputs = []reviewquorum.LaneOutput{
		{
			LaneID:       "correctness",
			Provider:     "judge-a",
			Model:        "model-a",
			Verdict:      reviewquorum.VerdictPass,
			FailureClass: reviewquorum.FailureClassNone,
			ReadOnlyEnforcement: reviewquorum.ReadOnlyEnforcement{
				Observed: true, Enabled: true, Passed: true,
				BaselineCommand: "git status --porcelain=v1 -z",
				AfterCommand:    "git status --porcelain=v1 -z",
			},
		},
		{
			LaneID:       "performance",
			Provider:     "judge-b",
			Model:        "model-b",
			Verdict:      reviewquorum.VerdictPass,
			FailureClass: reviewquorum.FailureClassNone,
			ReadOnlyEnforcement: reviewquorum.ReadOnlyEnforcement{
				Observed: true, Enabled: true, Passed: true,
				BaselineCommand: "git status --porcelain=v1 -z",
				AfterCommand:    "git status --porcelain=v1 -z",
			},
		},
	}

	summary := input.Review.FinalizedSummary()
	if summary.Verdict != reviewquorum.VerdictPass {
		t.Fatalf("Verdict = %q, want %q; summary=%+v", summary.Verdict, reviewquorum.VerdictPass, summary)
	}
	if summary.Subject != input.Review.Subject || summary.BaseRef != input.Review.BaseRef {
		t.Fatalf("identity = %q/%q, want %q/%q", summary.Subject, summary.BaseRef, input.Review.Subject, input.Review.BaseRef)
	}
	if !Evaluate(input).Promote {
		t.Fatalf("Evaluate() rejected finalized passing lanes: %+v", Evaluate(input))
	}
}

func TestEvaluateRequiresHumanApprovalForSensitiveAuthority(t *testing.T) {
	input := passingInput()
	input.AuthorityClass = "safety_policy"
	input.HumanApprovalVerified = false

	decision := Evaluate(input)
	if decision.Promote {
		t.Fatal("Promote = true, want false")
	}
	if !decision.ManualApprovalRequired {
		t.Fatal("ManualApprovalRequired = false, want true")
	}
	if !containsReason(decision.Reasons, ReasonHumanApprovalRequired) {
		t.Errorf("reasons %v do not contain %q", decision.Reasons, ReasonHumanApprovalRequired)
	}
}

func TestEvaluateRejectsUnknownAuthorityClassAndMissingHumanApproval(t *testing.T) {
	input := passingInput()
	input.AuthorityClass = "safety-policy"
	input.HumanApprovalVerified = false

	decision := Evaluate(input)
	if decision.Promote {
		t.Fatal("Promote = true, want false for unknown authority and no human approval")
	}
	for _, reason := range []string{ReasonUnknownAuthorityClass, ReasonHumanApprovalRequired} {
		if !containsReason(decision.Reasons, reason) {
			t.Errorf("reasons %v do not contain %q", decision.Reasons, reason)
		}
	}
}

func TestEvaluateRequiresControllerVerifiedHumanApproval(t *testing.T) {
	input := passingInput()
	input.HumanApprovalVerified = false
	decision := Evaluate(input)
	if decision.Promote {
		t.Fatal("policy authorized promotion without controller-verified human approval")
	}
	if !decision.ManualApprovalRequired {
		t.Fatal("ManualApprovalRequired = false, want true")
	}
}

func TestEvaluateRequiresJudgeIDsToMatchFinalizedLanes(t *testing.T) {
	input := passingInput()
	input.Review.Judges = []string{"correctness", "missing-lane"}

	decision := Evaluate(input)
	if decision.Promote {
		t.Fatal("Promote = true, want false")
	}
	if !containsReason(decision.Reasons, ReasonJudgeLaneMismatch) {
		t.Errorf("reasons %v do not contain %q", decision.Reasons, ReasonJudgeLaneMismatch)
	}
}

func TestCandidateProposalContainsOnlyCandidateBundle(t *testing.T) {
	input := passingInput()
	raw, err := json.Marshal(CandidateProposal{Candidate: input.Candidate})
	if err != nil {
		t.Fatalf("marshal proposal: %v", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatalf("decode proposal: %v", err)
	}
	if len(fields) != 1 || fields["candidate"] == nil {
		t.Fatalf("candidate proposal fields = %v, want only candidate bundle", fields)
	}
}

func TestTrustedMeasurementsRequireCompleteJSONFields(t *testing.T) {
	for _, test := range []struct {
		name string
		raw  string
		dst  any
	}{
		{name: "metrics", raw: `{"score":1}`, dst: &Metrics{}},
		{name: "limits", raw: `{"min_safety_score":0.9}`, dst: &Limits{}},
		{name: "worker time", raw: `{"implementation_hours":1}`, dst: &WorkerTime{}},
		{name: "work accounting", raw: `{"acceptance_set_sha256":"x"}`, dst: &WorkAccounting{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := json.Unmarshal([]byte(test.raw), test.dst); err == nil {
				t.Fatal("incomplete trusted data decoded without error")
			}
		})
	}
}

func TestEvaluateRejectsInvalidLimitsAndDuplicateAcceptanceUnits(t *testing.T) {
	input := passingInput()
	input.Limits.MaxCostUSD = 0
	input.WorkAccounting.AcceptanceUnitIDs = append(input.WorkAccounting.AcceptanceUnitIDs, input.WorkAccounting.AcceptanceUnitIDs[0])
	input.WorkAccounting.AcceptanceSetSHA256 = HashAcceptanceUnitIDs(input.WorkAccounting.AcceptanceUnitIDs)

	decision := Evaluate(input)
	if decision.Promote {
		t.Fatal("Promote = true, want false for invalid limits and duplicate acceptance units")
	}
	for _, reason := range []string{ReasonInvalidLimits, ReasonWorkAccountingInvalid} {
		if !containsReason(decision.Reasons, reason) {
			t.Errorf("reasons %v do not contain %q", decision.Reasons, reason)
		}
	}
}
