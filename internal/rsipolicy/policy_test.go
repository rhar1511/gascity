package rsipolicy

import (
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/reviewquorum"
	"github.com/gastownhall/gascity/internal/storybench"
)

func passingInput() Input {
	return Input{
		Objective: "reduce discovery latency without changing behavior",
		Current: Bundle{
			ID:             "bundle-001",
			TypesCommit:    "types-commit",
			AICommit:       "ai-commit",
			FrontendCommit: "frontend-commit",
			InktreeCommit:  "inktree-commit",
			EvalSuiteHash:  "suite-v1",
		},
		Candidate: Bundle{
			ID:             "bundle-002",
			TypesCommit:    "types-commit",
			AICommit:       "ai-commit-candidate",
			FrontendCommit: "frontend-commit",
			InktreeCommit:  "inktree-commit",
			EvalSuiteHash:  "suite-v1",
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
			Improver: "jules",
			Judges:   []string{"correctness", "performance"},
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

func TestEvaluateStoryBenchmarkCannotBeBypassedByAggregateScore(t *testing.T) {
	input := passingInput()
	input.StoryBenchmarkRequired = true
	decision := Evaluate(input)
	if decision.Promote || !containsReason(decision.Reasons, ReasonStoryBenchmarkMissing) {
		t.Fatalf("missing benchmark promoted: %+v", decision)
	}
	input.StoryBenchmark = &storybench.Result{
		SuiteID:   "inktree-story-agency-v1",
		SuiteHash: input.Current.EvalSuiteHash, BaselineBundleID: input.Current.ID,
		CandidateBundleID: input.Candidate.ID, Eligible: true,
		BaselineCapturedBy: "independent-harness", CandidateCapturedBy: "independent-harness",
		BaselinePassed: 0, CandidatePassed: 1,
		Cases: []storybench.CaseResult{{CaseID: "night-end", StoryID: "INK-001", CandidatePassed: true}},
	}
	if decision := Evaluate(input); !decision.Promote {
		t.Fatalf("passing benchmark rejected: %+v", decision)
	}
	input.StoryBenchmark.Cases[0].CandidatePassed = false
	if decision := Evaluate(input); decision.Promote || !containsReason(decision.Reasons, ReasonStoryBenchmarkFailed) {
		t.Fatalf("inconsistent benchmark promoted: %+v", decision)
	}
}

func TestEvaluateRejectsMismatchedOrSelfJudgedStoryEvidence(t *testing.T) {
	input := passingInput()
	input.StoryBenchmarkRequired = true
	passing := storybench.Result{
		SuiteID: "suite-v1", SuiteHash: input.Current.EvalSuiteHash,
		BaselineBundleID: input.Current.ID, CandidateBundleID: input.Candidate.ID,
		BaselineCapturedBy: "harness", CandidateCapturedBy: "harness", Eligible: true,
		BaselinePassed: 0, CandidatePassed: 1,
		Cases: []storybench.CaseResult{{CaseID: "one", StoryID: "INK-001", CandidatePassed: true}},
	}
	for name, mutate := range map[string]func(*storybench.Result){
		"suite":        func(r *storybench.Result) { r.SuiteHash = "changed" },
		"bundle":       func(r *storybench.Result) { r.CandidateBundleID = "other" },
		"self judge":   func(r *storybench.Result) { r.CandidateCapturedBy = input.Review.Improver },
		"critical":     func(r *storybench.Result) { r.CriticalFailures = 1 },
		"partial case": func(r *storybench.Result) { r.Cases = nil },
	} {
		t.Run(name, func(t *testing.T) {
			result := passing
			mutate(&result)
			input.StoryBenchmark = &result
			decision := Evaluate(input)
			if decision.Promote || !containsReason(decision.Reasons, ReasonStoryBenchmarkFailed) {
				t.Fatalf("invalid story evidence promoted: %+v", decision)
			}
		})
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

func TestCandidateEvidenceBuildsPolicyInputWithoutMergingJudgeData(t *testing.T) {
	input := passingInput()
	evidence := CandidateEvidence{
		Objective:        input.Objective,
		Current:          input.Current,
		Candidate:        input.Candidate,
		Baseline:         input.Baseline,
		CandidateMetrics: input.CandidateMetrics,
		Limits:           input.Limits,
		AuthorityClass:   input.AuthorityClass,
		Attempts:         input.Attempts,
		MaxAttempts:      input.MaxAttempts,
		Improver:         input.Review.Improver,
		Judges:           input.Review.Judges,
		ReviewSubject:    "candidate:bundle-002",
		ReviewBaseRef:    "bundle-001",
	}
	got := evidence.Input(input.Review.Summary.Lanes)
	if got.Candidate.ID != input.Candidate.ID || got.Current.ID != input.Current.ID {
		t.Fatalf("bundle lineage = %q/%q, want %q/%q", got.Current.ID, got.Candidate.ID, input.Current.ID, input.Candidate.ID)
	}
	if got.Review.Improver != input.Review.Improver || len(got.Review.LaneOutputs) != 2 {
		t.Fatalf("review input = %+v, want improver and two lanes", got.Review)
	}
}
