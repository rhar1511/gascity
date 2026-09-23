// Package rsipolicy defines the pure promotion gate for bounded,
// evidence-backed self-improvement cycles.
package rsipolicy

import (
	"math"
	"strings"

	"github.com/gastownhall/gascity/internal/reviewquorum"
)

// Stable reason codes returned by Evaluate.
const (
	ReasonPromoted                = "promoted"
	ReasonObjectiveMissing        = "objective_missing"
	ReasonBundleInvalid           = "bundle_invalid"
	ReasonParentMismatch          = "parent_bundle_mismatch"
	ReasonEvalSuiteChanged        = "evaluation_suite_changed"
	ReasonInvalidMetrics          = "invalid_metrics"
	ReasonScoreNotImproved        = "score_not_improved"
	ReasonSafetyLimitExceeded     = "safety_limit_exceeded"
	ReasonLatencyLimitExceeded    = "latency_limit_exceeded"
	ReasonCostLimitExceeded       = "cost_limit_exceeded"
	ReasonDependencyLimitExceeded = "dependency_limit_exceeded"
	ReasonInsufficientJudges      = "insufficient_independent_judges"
	ReasonImproverJudgerOverlap   = "improver_is_judge"
	ReasonReviewFailed            = "review_gate_failed"
	ReasonAttemptBudgetExhausted  = "attempt_budget_exhausted"
	ReasonAttemptBudgetInvalid    = "attempt_budget_invalid"
	ReasonJudgeLaneMismatch       = "judge_lane_mismatch"
	ReasonHumanApprovalRequired   = "human_approval_required"
)

// Bundle is an immutable revision bundle. Each field identifies the exact
// revision used for one rig or the evaluation suite. Parent points at the
// previously accepted bundle so rollback is always immediate and explicit.
type Bundle struct {
	ID             string `json:"id"`
	TypesCommit    string `json:"types"`
	AICommit       string `json:"ai"`
	FrontendCommit string `json:"frontend"`
	InktreeCommit  string `json:"inktree"`
	EvalSuiteHash  string `json:"evals"`
	Parent         string `json:"parent"`
}

// Metrics contains the measurements used by the monotonic promotion gate.
// Score and SafetyScore are higher-is-better. The remaining values are bounded
// by Limits.
type Metrics struct {
	Score           float64 `json:"score"`
	SafetyScore     float64 `json:"safety_score"`
	LatencyMS       float64 `json:"latency_ms"`
	CostUSD         float64 `json:"cost_usd"`
	DependencyCount int     `json:"dependency_count"`
}

// Limits are hard ceilings/floors for a candidate. A zero value means the
// corresponding limit is not configured, except MinSafetyScore where zero is
// a valid floor.
type Limits struct {
	MinSafetyScore  float64 `json:"min_safety_score"`
	MaxLatencyMS    float64 `json:"max_latency_ms"`
	MaxCostUSD      float64 `json:"max_cost_usd"`
	MaxDependencies int     `json:"max_dependencies"`
}

// Review identifies the independent judge lanes that evaluated the candidate.
// Improver is kept separate from Judges so a candidate-producing agent cannot
// approve its own change.
type Review struct {
	Improver    string                    `json:"improver"`
	Judges      []string                  `json:"judges"`
	Subject     string                    `json:"subject"`
	BaseRef     string                    `json:"base_ref"`
	LaneOutputs []reviewquorum.LaneOutput `json:"lane_outputs,omitempty"`
	Summary     reviewquorum.Summary      `json:"summary,omitempty"`
}

// FinalizedSummary returns the durable review quorum summary. When raw lane
// outputs are present, the Go finalizer owns synthesis so an agent cannot
// rewrite or self-approve the judge result.
func (review Review) FinalizedSummary() reviewquorum.Summary {
	if len(review.LaneOutputs) > 0 {
		return reviewquorum.Finalize(review.Subject, review.BaseRef, review.LaneOutputs)
	}
	return review.Summary
}

// Input is one bounded improvement attempt.
type Input struct {
	Objective        string  `json:"objective"`
	Current          Bundle  `json:"current"`
	Candidate        Bundle  `json:"candidate"`
	Baseline         Metrics `json:"baseline"`
	CandidateMetrics Metrics `json:"candidate_metrics"`
	Limits           Limits  `json:"limits"`
	Review           Review  `json:"review"`
	AuthorityClass   string  `json:"authority_class"`
	Attempts         int     `json:"attempts"`
	MaxAttempts      int     `json:"max_attempts"`
}

// Decision is the auditable result of Evaluate. Reasons are stable machine
// readable codes; the first reason is also exposed as Reason for compact
// consumers.
type Decision struct {
	Promote                bool     `json:"promote"`
	ManualApprovalRequired bool     `json:"manual_approval_required"`
	Reason                 string   `json:"reason"`
	Reasons                []string `json:"reasons"`
	RollbackBundleID       string   `json:"rollback_bundle_id"`
}

// Evaluate applies all promotion invariants without side effects. A candidate
// is promoted only when it strictly improves the baseline score, remains within
// every configured safety/resource limit, preserves evaluation-suite identity,
// has a valid parent, and passes independent review before its attempt budget
// is exhausted.
func Evaluate(in Input) Decision {
	decision := Decision{RollbackBundleID: strings.TrimSpace(in.Current.ID)}
	reasons := make([]string, 0, 8)
	add := func(reason string) {
		for _, existing := range reasons {
			if existing == reason {
				return
			}
		}
		reasons = append(reasons, reason)
	}

	if strings.TrimSpace(in.Objective) == "" {
		add(ReasonObjectiveMissing)
	}
	if in.MaxAttempts <= 0 || in.Attempts < 0 {
		add(ReasonAttemptBudgetInvalid)
	} else if in.Attempts >= in.MaxAttempts {
		add(ReasonAttemptBudgetExhausted)
	}

	if !validBundle(in.Current) || !validBundle(in.Candidate) {
		add(ReasonBundleInvalid)
	}
	if strings.TrimSpace(in.Candidate.Parent) != strings.TrimSpace(in.Current.ID) {
		add(ReasonParentMismatch)
	}
	if strings.TrimSpace(in.Current.EvalSuiteHash) == "" ||
		strings.TrimSpace(in.Candidate.EvalSuiteHash) != strings.TrimSpace(in.Current.EvalSuiteHash) {
		add(ReasonEvalSuiteChanged)
	}

	if !validMetrics(in.Baseline) || !validMetrics(in.CandidateMetrics) {
		add(ReasonInvalidMetrics)
	} else {
		if in.CandidateMetrics.Score <= in.Baseline.Score {
			add(ReasonScoreNotImproved)
		}
		if in.CandidateMetrics.SafetyScore < in.Limits.MinSafetyScore {
			add(ReasonSafetyLimitExceeded)
		}
		if in.Limits.MaxLatencyMS > 0 && in.CandidateMetrics.LatencyMS > in.Limits.MaxLatencyMS {
			add(ReasonLatencyLimitExceeded)
		}
		if in.Limits.MaxCostUSD > 0 && in.CandidateMetrics.CostUSD > in.Limits.MaxCostUSD {
			add(ReasonCostLimitExceeded)
		}
		if in.Limits.MaxDependencies > 0 && in.CandidateMetrics.DependencyCount > in.Limits.MaxDependencies {
			add(ReasonDependencyLimitExceeded)
		}
	}

	summary := in.Review.FinalizedSummary()
	judges := normalizedJudges(in.Review.Judges)
	if len(judges) < 2 || len(summary.Lanes) < 2 {
		add(ReasonInsufficientJudges)
	} else {
		laneIDs := make(map[string]struct{}, len(summary.Lanes))
		for _, lane := range summary.Lanes {
			laneIDs[strings.TrimSpace(lane.LaneID)] = struct{}{}
		}
		for _, judge := range judges {
			if _, ok := laneIDs[judge]; !ok {
				add(ReasonJudgeLaneMismatch)
				break
			}
		}
	}
	for _, judge := range judges {
		if judge == strings.TrimSpace(in.Review.Improver) {
			add(ReasonImproverJudgerOverlap)
			break
		}
	}
	if summary.Verdict != reviewquorum.VerdictPass &&
		summary.Verdict != reviewquorum.VerdictPassWithFindings {
		add(ReasonReviewFailed)
	}
	if requiresHumanApproval(in.AuthorityClass) {
		decision.ManualApprovalRequired = true
		add(ReasonHumanApprovalRequired)
	}

	if len(reasons) == 0 {
		decision.Promote = true
		decision.Reason = ReasonPromoted
		decision.Reasons = []string{ReasonPromoted}
		return decision
	}
	decision.Reason = reasons[0]
	decision.Reasons = reasons
	return decision
}

func validBundle(bundle Bundle) bool {
	fields := []string{
		bundle.ID,
		bundle.TypesCommit,
		bundle.AICommit,
		bundle.FrontendCommit,
		bundle.InktreeCommit,
		bundle.EvalSuiteHash,
	}
	for _, field := range fields {
		if strings.TrimSpace(field) == "" {
			return false
		}
	}
	return true
}

func validMetrics(metrics Metrics) bool {
	return !math.IsNaN(metrics.Score) && !math.IsInf(metrics.Score, 0) &&
		!math.IsNaN(metrics.SafetyScore) && !math.IsInf(metrics.SafetyScore, 0) &&
		!math.IsNaN(metrics.LatencyMS) && !math.IsInf(metrics.LatencyMS, 0) &&
		!math.IsNaN(metrics.CostUSD) && !math.IsInf(metrics.CostUSD, 0) &&
		metrics.Score >= 0 && metrics.SafetyScore >= 0 && metrics.LatencyMS >= 0 &&
		metrics.CostUSD >= 0 && metrics.DependencyCount >= 0
}

func normalizedJudges(judges []string) []string {
	seen := make(map[string]struct{}, len(judges))
	out := make([]string, 0, len(judges))
	for _, judge := range judges {
		judge = strings.TrimSpace(judge)
		if judge == "" {
			continue
		}
		if _, ok := seen[judge]; ok {
			continue
		}
		seen[judge] = struct{}{}
		out = append(out, judge)
	}
	return out
}

func requiresHumanApproval(authorityClass string) bool {
	switch strings.ToLower(strings.TrimSpace(authorityClass)) {
	case "evaluator", "safety_policy", "clinical", "deployment", "deployment_policy", "world_model":
		return true
	default:
		return false
	}
}
