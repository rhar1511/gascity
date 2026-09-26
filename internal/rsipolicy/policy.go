// Package rsipolicy defines the pure promotion gate for bounded,
// evidence-backed self-improvement cycles.
package rsipolicy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"sort"
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
	ReasonInvalidLimits           = "invalid_limits"
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
	ReasonJudgeExecutionInvalid   = "judge_execution_identity_invalid"
	ReasonWorkAccountingInvalid   = "work_accounting_invalid"
	ReasonUnknownAuthorityClass   = "unknown_authority_class"
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

// UnmarshalJSON requires every measurement field to be present so omitted
// metrics cannot be confused with measured zero values.
func (m *Metrics) UnmarshalJSON(data []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	if err := requireJSONKeys(fields, "score", "safety_score", "latency_ms", "cost_usd", "dependency_count"); err != nil {
		return err
	}
	type plain Metrics
	var decoded plain
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*m = Metrics(decoded)
	return nil
}

// WorkerTime accounts for all time spent producing, recovering, and evaluating
// a candidate. Values are worker-hours and must be non-negative.
type WorkerTime struct {
	ImplementationHours float64 `json:"implementation_hours"`
	RecoveryHours       float64 `json:"recovery_hours"`
	EvaluationHours     float64 `json:"evaluation_hours"`
}

// UnmarshalJSON requires all worker-time fields to be present.
func (t *WorkerTime) UnmarshalJSON(data []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	if err := requireJSONKeys(fields, "implementation_hours", "recovery_hours", "evaluation_hours"); err != nil {
		return err
	}
	type plain WorkerTime
	var decoded plain
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*t = WorkerTime(decoded)
	return nil
}

// TotalHours returns the sum of implementation, recovery, and evaluation work.
func (t WorkerTime) TotalHours() float64 {
	return t.ImplementationHours + t.RecoveryHours + t.EvaluationHours
}

// WorkAccounting freezes the acceptance set and records independently verified
// useful completions and total worker time for both sides of one evaluation.
// Score is useful completions per worker-hour derived from these fields.
type WorkAccounting struct {
	AcceptanceSetSHA256    string     `json:"acceptance_set_sha256"`
	AcceptanceUnitIDs      []string   `json:"acceptance_unit_ids"`
	BaselineUsefulUnitIDs  []string   `json:"baseline_useful_unit_ids"`
	CandidateUsefulUnitIDs []string   `json:"candidate_useful_unit_ids"`
	BaselineWorkerTime     WorkerTime `json:"baseline_worker_time"`
	CandidateWorkerTime    WorkerTime `json:"candidate_worker_time"`
}

// UnmarshalJSON requires the acceptance set and both side-specific ledgers.
func (work *WorkAccounting) UnmarshalJSON(data []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	if err := requireJSONKeys(fields, "acceptance_set_sha256", "acceptance_unit_ids", "baseline_useful_unit_ids", "candidate_useful_unit_ids", "baseline_worker_time", "candidate_worker_time"); err != nil {
		return err
	}
	type plain WorkAccounting
	var decoded plain
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*work = WorkAccounting(decoded)
	if work.AcceptanceUnitIDs == nil || work.BaselineUsefulUnitIDs == nil || work.CandidateUsefulUnitIDs == nil {
		return fmt.Errorf("work accounting completion lists must be present arrays")
	}
	return nil
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

// UnmarshalJSON requires every safety and resource limit to be present.
func (limits *Limits) UnmarshalJSON(data []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	if err := requireJSONKeys(fields, "min_safety_score", "max_latency_ms", "max_cost_usd", "max_dependencies"); err != nil {
		return err
	}
	type plain Limits
	var decoded plain
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*limits = Limits(decoded)
	return nil
}

// Review identifies the independent judge lanes that evaluated the candidate.
// Improver is kept separate from Judges so a candidate-producing agent cannot
// approve its own change.
type Review struct {
	Improver        string                    `json:"improver"`
	ImproverSession string                    `json:"improver_session,omitempty"`
	Judges          []string                  `json:"judges"`
	Executions      []JudgeExecutionIdentity  `json:"executions,omitempty"`
	Subject         string                    `json:"subject"`
	BaseRef         string                    `json:"base_ref"`
	LaneOutputs     []reviewquorum.LaneOutput `json:"lane_outputs,omitempty"`
	Summary         reviewquorum.Summary      `json:"summary,omitempty"`
}

// JudgeExecutionIdentity is copied from trusted controller evidence after it
// is checked against the actual assigned judge bead. Lane names alone are not
// proof of separate actors or sessions.
type JudgeExecutionIdentity struct {
	LaneID    string `json:"lane_id"`
	ActorID   string `json:"actor_id"`
	SessionID string `json:"session_id"`
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
	Objective             string         `json:"objective"`
	Current               Bundle         `json:"current"`
	Candidate             Bundle         `json:"candidate"`
	Baseline              Metrics        `json:"baseline"`
	CandidateMetrics      Metrics        `json:"candidate_metrics"`
	Limits                Limits         `json:"limits"`
	WorkAccounting        WorkAccounting `json:"work_accounting"`
	Review                Review         `json:"review"`
	AuthorityClass        string         `json:"authority_class"`
	Attempts              int            `json:"attempts"`
	MaxAttempts           int            `json:"max_attempts"`
	HumanApprovalVerified bool           `json:"human_approval_verified"`
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
	if !validLimits(in.Limits) {
		add(ReasonInvalidLimits)
	}
	if !validWorkAccounting(in.WorkAccounting) ||
		!metricMatchesWork(in.Baseline.Score, len(in.WorkAccounting.BaselineUsefulUnitIDs), in.WorkAccounting.BaselineWorkerTime.TotalHours()) ||
		!metricMatchesWork(in.CandidateMetrics.Score, len(in.WorkAccounting.CandidateUsefulUnitIDs), in.WorkAccounting.CandidateWorkerTime.TotalHours()) {
		add(ReasonWorkAccountingInvalid)
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
	if !validJudgeExecutions(in.Review) {
		add(ReasonJudgeExecutionInvalid)
	}
	if summary.Verdict != reviewquorum.VerdictPass &&
		summary.Verdict != reviewquorum.VerdictPassWithFindings {
		add(ReasonReviewFailed)
	}
	if !knownAuthorityClass(in.AuthorityClass) {
		add(ReasonUnknownAuthorityClass)
	}
	if !in.HumanApprovalVerified {
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
	return strings.TrimSpace(bundle.ID) != "" && validRevision(bundle.TypesCommit) && validRevision(bundle.AICommit) &&
		validRevision(bundle.FrontendCommit) && validRevision(bundle.InktreeCommit) && validSHA256(bundle.EvalSuiteHash)
}

func validRevision(revision string) bool {
	decoded, err := hex.DecodeString(strings.TrimSpace(revision))
	return err == nil && (len(decoded) == 20 || len(decoded) == 32) && strings.TrimSpace(revision) == strings.ToLower(strings.TrimSpace(revision))
}

func validMetrics(metrics Metrics) bool {
	return !math.IsNaN(metrics.Score) && !math.IsInf(metrics.Score, 0) &&
		!math.IsNaN(metrics.SafetyScore) && !math.IsInf(metrics.SafetyScore, 0) &&
		!math.IsNaN(metrics.LatencyMS) && !math.IsInf(metrics.LatencyMS, 0) &&
		!math.IsNaN(metrics.CostUSD) && !math.IsInf(metrics.CostUSD, 0) &&
		metrics.Score >= 0 && metrics.SafetyScore >= 0 && metrics.SafetyScore <= 1 && metrics.LatencyMS > 0 &&
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

func knownAuthorityClass(authorityClass string) bool {
	switch strings.ToLower(strings.TrimSpace(authorityClass)) {
	case "formatting", "administrative", "optimization", "workflow":
		return true
	case "evaluator", "safety_policy", "clinical", "deployment", "deployment_policy", "world_model":
		return true
	default:
		return false
	}
}

func validWorkAccounting(work WorkAccounting) bool {
	decoded, err := hex.DecodeString(strings.TrimSpace(work.AcceptanceSetSHA256))
	if err != nil || len(decoded) != 32 || len(work.AcceptanceUnitIDs) == 0 || len(work.AcceptanceUnitIDs) > 10000 {
		return false
	}
	if HashAcceptanceUnitIDs(work.AcceptanceUnitIDs) != strings.ToLower(work.AcceptanceSetSHA256) {
		return false
	}
	known := make(map[string]struct{}, len(work.AcceptanceUnitIDs))
	for _, id := range work.AcceptanceUnitIDs {
		if strings.TrimSpace(id) == "" || strings.TrimSpace(id) != id || strings.ContainsAny(id, "\x00\r\n") {
			return false
		}
		if _, exists := known[id]; exists {
			return false
		}
		known[id] = struct{}{}
	}
	if !validUsefulUnitIDs(work.BaselineUsefulUnitIDs, known) || !validUsefulUnitIDs(work.CandidateUsefulUnitIDs, known) {
		return false
	}
	return validWorkerTime(work.BaselineWorkerTime) && validWorkerTime(work.CandidateWorkerTime) &&
		work.BaselineWorkerTime.TotalHours() > 0 && work.CandidateWorkerTime.TotalHours() > 0
}

func validUsefulUnitIDs(ids []string, known map[string]struct{}) bool {
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		if _, ok := known[id]; !ok {
			return false
		}
		if _, duplicate := seen[id]; duplicate {
			return false
		}
		seen[id] = struct{}{}
	}
	return true
}

// HashAcceptanceUnitIDs returns the SHA-256 digest of sorted, unique unit IDs
// separated by NUL bytes. This gives the evaluation a stable frozen set while
// per-run useful-completion lists remain separately auditable.
func HashAcceptanceUnitIDs(ids []string) string {
	copyIDs := append([]string(nil), ids...)
	sort.Strings(copyIDs)
	digest := sha256.Sum256([]byte(strings.Join(copyIDs, "\x00")))
	return hex.EncodeToString(digest[:])
}

func validLimits(limits Limits) bool {
	return !math.IsNaN(limits.MinSafetyScore) && !math.IsInf(limits.MinSafetyScore, 0) &&
		!math.IsNaN(limits.MaxLatencyMS) && !math.IsInf(limits.MaxLatencyMS, 0) &&
		!math.IsNaN(limits.MaxCostUSD) && !math.IsInf(limits.MaxCostUSD, 0) &&
		limits.MinSafetyScore > 0 && limits.MinSafetyScore <= 1 &&
		limits.MaxLatencyMS > 0 && limits.MaxCostUSD > 0 && limits.MaxDependencies > 0
}

func validWorkerTime(workerTime WorkerTime) bool {
	values := []float64{workerTime.ImplementationHours, workerTime.RecoveryHours, workerTime.EvaluationHours}
	for _, value := range values {
		if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
			return false
		}
	}
	return !math.IsInf(workerTime.TotalHours(), 0)
}

func metricMatchesWork(score float64, completions int, workerHours float64) bool {
	if workerHours <= 0 || math.IsNaN(score) || math.IsInf(score, 0) {
		return false
	}
	want := float64(completions) / workerHours
	tolerance := math.Max(1e-9, math.Abs(want)*1e-6)
	return math.Abs(score-want) <= tolerance
}

func validJudgeExecutions(review Review) bool {
	if len(review.Executions) != len(review.Judges) || strings.TrimSpace(review.Improver) == "" || strings.TrimSpace(review.ImproverSession) == "" {
		return false
	}
	actors := map[string]struct{}{strings.TrimSpace(review.Improver): {}}
	sessions := map[string]struct{}{strings.TrimSpace(review.ImproverSession): {}}
	lanes := make(map[string]struct{}, len(review.Executions))
	for _, execution := range review.Executions {
		lane := strings.TrimSpace(execution.LaneID)
		actor := strings.TrimSpace(execution.ActorID)
		session := strings.TrimSpace(execution.SessionID)
		if lane == "" || actor == "" || session == "" {
			return false
		}
		if _, ok := lanes[lane]; ok {
			return false
		}
		if _, ok := actors[actor]; ok {
			return false
		}
		if _, ok := sessions[session]; ok {
			return false
		}
		lanes[lane] = struct{}{}
		actors[actor] = struct{}{}
		sessions[session] = struct{}{}
	}
	for _, judge := range normalizedJudges(review.Judges) {
		if _, ok := lanes[judge]; !ok {
			return false
		}
	}
	return len(lanes) == len(review.Judges)
}

func requireJSONKeys(fields map[string]json.RawMessage, required ...string) error {
	if len(fields) != len(required) {
		return fmt.Errorf("object has %d fields, want exactly %d", len(fields), len(required))
	}
	for _, key := range required {
		if _, ok := fields[key]; !ok {
			return fmt.Errorf("required field %q is missing", key)
		}
	}
	return nil
}
