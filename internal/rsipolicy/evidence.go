package rsipolicy

import "github.com/gastownhall/gascity/internal/reviewquorum"

// CandidateEvidence is the durable candidate-worker payload consumed by the
// RSI promotion control. It contains proposal facts and measurements, while
// judge lanes remain separate child outputs.
type CandidateEvidence struct {
	Objective        string   `json:"objective"`
	Current          Bundle   `json:"current"`
	Candidate        Bundle   `json:"candidate"`
	Baseline         Metrics  `json:"baseline"`
	CandidateMetrics Metrics  `json:"candidate_metrics"`
	Limits           Limits   `json:"limits"`
	AuthorityClass   string   `json:"authority_class"`
	Attempts         int      `json:"attempts"`
	MaxAttempts      int      `json:"max_attempts"`
	Improver         string   `json:"improver"`
	Judges           []string `json:"judges"`
	ReviewSubject    string   `json:"review_subject"`
	ReviewBaseRef    string   `json:"review_base_ref"`
}

// Input builds the policy input from candidate evidence and independently
// persisted judge lane outputs.
func (e CandidateEvidence) Input(lanes []reviewquorum.LaneOutput) Input {
	return Input{
		Objective:        e.Objective,
		Current:          e.Current,
		Candidate:        e.Candidate,
		Baseline:         e.Baseline,
		CandidateMetrics: e.CandidateMetrics,
		Limits:           e.Limits,
		AuthorityClass:   e.AuthorityClass,
		Attempts:         e.Attempts,
		MaxAttempts:      e.MaxAttempts,
		Review: Review{
			Improver:    e.Improver,
			Judges:      append([]string(nil), e.Judges...),
			Subject:     e.ReviewSubject,
			BaseRef:     e.ReviewBaseRef,
			LaneOutputs: append([]reviewquorum.LaneOutput(nil), lanes...),
		},
	}
}
