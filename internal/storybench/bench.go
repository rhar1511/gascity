// Package storybench scores independently captured agent action traces against
// a frozen, product-owned user-story suite. It makes no clinical efficacy claim.
package storybench

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

// Stable benchmark veto reasons used by the RSI policy and audit output.
const (
	ReasonCriticalFailure = "critical_case_failed"
	ReasonRegression      = "story_regression"
	ReasonNoImprovement   = "no_story_improvement"
)

// Case describes observable agent actions. Action names are supplied by the
// product suite, not hard-coded in Gas City's policy engine.
type Case struct {
	ID               string   `json:"id"`
	StoryID          string   `json:"story_id"`
	Objective        string   `json:"objective"`
	Prompt           string   `json:"prompt"`
	Critical         bool     `json:"critical"`
	RequiredActions  []string `json:"required_actions"`
	ForbiddenActions []string `json:"forbidden_actions"`
	MaxActions       int      `json:"max_actions"`
	MaxDurationMS    int64    `json:"max_duration_ms,omitempty"`
}

// Suite is a versioned collection of frozen user-story cases.
type Suite struct {
	Version int    `json:"version"`
	ID      string `json:"id"`
	Cases   []Case `json:"cases"`
}

// Run is produced from a harness-captured trace, not a candidate worker's
// self-reported score. The caller must enforce CapturedBy provenance.
type Run struct {
	SuiteSHA256 string  `json:"suite_sha256"`
	BundleID    string  `json:"bundle_id"`
	CapturedBy  string  `json:"captured_by"`
	Cases       []Trace `json:"cases"`
}

// Trace records observed action tags and elapsed time for one case.
type Trace struct {
	CaseID    string   `json:"case_id"`
	Actions   []string `json:"actions"`
	ElapsedMS int64    `json:"elapsed_ms,omitempty"`
}

// Evidence is the durable independent-benchmark bead payload. The promotion
// gate recomputes Result from these traces and its own pinned suite file.
type Evidence struct {
	Baseline  Run `json:"baseline"`
	Candidate Run `json:"candidate"`
}

// CaseResult links a baseline/candidate comparison back to a user story.
type CaseResult struct {
	CaseID              string   `json:"case_id"`
	StoryID             string   `json:"story_id"`
	Critical            bool     `json:"critical"`
	BaselinePassed      bool     `json:"baseline_passed"`
	CandidatePassed     bool     `json:"candidate_passed"`
	CandidateViolations []string `json:"candidate_violations,omitempty"`
}

// Result is the deterministic benchmark artifact for an RSI gate. Ineligible
// results cannot be made eligible by a higher aggregate quality score.
type Result struct {
	SuiteID             string       `json:"suite_id"`
	SuiteHash           string       `json:"suite_hash"`
	BaselineBundleID    string       `json:"baseline_bundle_id"`
	CandidateBundleID   string       `json:"candidate_bundle_id"`
	BaselineCapturedBy  string       `json:"baseline_captured_by"`
	CandidateCapturedBy string       `json:"candidate_captured_by"`
	BaselinePassed      int          `json:"baseline_passed"`
	CandidatePassed     int          `json:"candidate_passed"`
	CriticalFailures    int          `json:"critical_failures"`
	Regressions         int          `json:"regressions"`
	Eligible            bool         `json:"eligible"`
	Reasons             []string     `json:"reasons,omitempty"`
	Cases               []CaseResult `json:"cases"`
}

// SuiteHash returns the SHA-256 of the exact frozen suite bytes.
func SuiteHash(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// Evaluate compares the same frozen cases on the accepted and candidate
// bundles. Invalid or incomplete evidence is an error rather than a score.
func Evaluate(rawSuite []byte, baseline, candidate Run, improver string) (Result, error) {
	var suite Suite
	if err := json.Unmarshal(rawSuite, &suite); err != nil {
		return Result{}, fmt.Errorf("decode story suite: %w", err)
	}
	if err := validateSuite(suite); err != nil {
		return Result{}, err
	}
	hash := SuiteHash(rawSuite)
	baseTraces, err := validateRun(suite, hash, baseline, improver)
	if err != nil {
		return Result{}, fmt.Errorf("baseline: %w", err)
	}
	candidateTraces, err := validateRun(suite, hash, candidate, improver)
	if err != nil {
		return Result{}, fmt.Errorf("candidate: %w", err)
	}
	if baseline.BundleID == candidate.BundleID {
		return Result{}, fmt.Errorf("baseline and candidate bundle IDs are identical")
	}
	result := Result{
		SuiteID: suite.ID, SuiteHash: hash, BaselineBundleID: baseline.BundleID,
		CandidateBundleID: candidate.BundleID, BaselineCapturedBy: baseline.CapturedBy,
		CandidateCapturedBy: candidate.CapturedBy, Cases: make([]CaseResult, 0, len(suite.Cases)),
	}
	for _, c := range suite.Cases {
		baseViolations := scoreCase(c, baseTraces[c.ID])
		candidateViolations := scoreCase(c, candidateTraces[c.ID])
		basePassed, candidatePassed := len(baseViolations) == 0, len(candidateViolations) == 0
		if basePassed {
			result.BaselinePassed++
		}
		if candidatePassed {
			result.CandidatePassed++
		}
		if c.Critical && !candidatePassed {
			result.CriticalFailures++
		}
		if basePassed && !candidatePassed {
			result.Regressions++
		}
		result.Cases = append(result.Cases, CaseResult{
			CaseID: c.ID, StoryID: c.StoryID, Critical: c.Critical,
			BaselinePassed: basePassed, CandidatePassed: candidatePassed,
			CandidateViolations: candidateViolations,
		})
	}
	if result.CriticalFailures > 0 {
		result.Reasons = append(result.Reasons, ReasonCriticalFailure)
	}
	if result.Regressions > 0 {
		result.Reasons = append(result.Reasons, ReasonRegression)
	}
	if result.CandidatePassed <= result.BaselinePassed {
		result.Reasons = append(result.Reasons, ReasonNoImprovement)
	}
	result.Eligible = len(result.Reasons) == 0
	return result, nil
}

func validateSuite(s Suite) error {
	if s.Version != 1 || strings.TrimSpace(s.ID) == "" || len(s.Cases) == 0 {
		return fmt.Errorf("story suite requires version 1, id, and cases")
	}
	seen := make(map[string]bool, len(s.Cases))
	for _, c := range s.Cases {
		if strings.TrimSpace(c.ID) == "" || strings.TrimSpace(c.StoryID) == "" ||
			strings.TrimSpace(c.Objective) == "" || strings.TrimSpace(c.Prompt) == "" ||
			c.MaxActions <= 0 || c.MaxDurationMS < 0 {
			return fmt.Errorf("case %q requires id, story_id, objective, prompt, and positive max_actions", c.ID)
		}
		if seen[c.ID] {
			return fmt.Errorf("duplicate case ID %q", c.ID)
		}
		seen[c.ID] = true
		if len(c.RequiredActions) == 0 && len(c.ForbiddenActions) == 0 {
			return fmt.Errorf("case %q has no assertions", c.ID)
		}
		patterns := make(map[string]bool)
		for _, action := range c.RequiredActions {
			if strings.TrimSpace(action) == "" || patterns[action] {
				return fmt.Errorf("case %q has invalid required action", c.ID)
			}
			patterns[action] = true
		}
		for _, action := range c.ForbiddenActions {
			if strings.TrimSpace(action) == "" || patterns[action] {
				return fmt.Errorf("case %q has conflicting or duplicate action", c.ID)
			}
			patterns[action] = true
		}
	}
	return nil
}

func validateRun(s Suite, hash string, run Run, improver string) (map[string]Trace, error) {
	if run.SuiteSHA256 != hash {
		return nil, fmt.Errorf("suite hash mismatch")
	}
	if strings.TrimSpace(run.BundleID) == "" {
		return nil, fmt.Errorf("bundle ID missing")
	}
	if strings.TrimSpace(run.CapturedBy) == "" || strings.TrimSpace(run.CapturedBy) == strings.TrimSpace(improver) {
		return nil, fmt.Errorf("trace capturer missing or same as improver")
	}
	allowed := make(map[string]bool, len(s.Cases))
	for _, c := range s.Cases {
		allowed[c.ID] = true
	}
	traces := make(map[string]Trace, len(run.Cases))
	for _, trace := range run.Cases {
		if !allowed[trace.CaseID] {
			return nil, fmt.Errorf("unknown case %q", trace.CaseID)
		}
		if _, exists := traces[trace.CaseID]; exists {
			return nil, fmt.Errorf("duplicate case %q", trace.CaseID)
		}
		for _, action := range trace.Actions {
			if strings.TrimSpace(action) == "" {
				return nil, fmt.Errorf("empty action in %q", trace.CaseID)
			}
		}
		if trace.ElapsedMS < 0 {
			return nil, fmt.Errorf("negative elapsed_ms in %q", trace.CaseID)
		}
		traces[trace.CaseID] = trace
	}
	if len(traces) != len(s.Cases) {
		return nil, fmt.Errorf("incomplete case coverage: got %d, want %d", len(traces), len(s.Cases))
	}
	return traces, nil
}

func scoreCase(c Case, trace Trace) []string {
	seen := make(map[string]bool, len(trace.Actions))
	for _, action := range trace.Actions {
		seen[action] = true
	}
	violations := make([]string, 0)
	for _, action := range c.RequiredActions {
		if !seen[action] {
			violations = append(violations, "missing:"+action)
		}
	}
	for _, action := range c.ForbiddenActions {
		if seen[action] {
			violations = append(violations, "forbidden:"+action)
		}
	}
	if len(trace.Actions) > c.MaxActions {
		violations = append(violations, "action_budget_exceeded")
	}
	if c.MaxDurationMS > 0 {
		if trace.ElapsedMS == 0 {
			violations = append(violations, "duration_missing")
		} else if trace.ElapsedMS > c.MaxDurationMS {
			violations = append(violations, "duration_exceeded")
		}
	}
	return violations
}
