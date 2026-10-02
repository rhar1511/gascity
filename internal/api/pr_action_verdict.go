package api

import "time"

// PRActionPolicyVerdict preserves the exact policy permission and evidence
// observed for one action. It is historical evidence, not permission to perform
// another action under the current policy.
type PRActionPolicyVerdict struct {
	PolicyVersion  string                    `json:"policy_version"`
	Monitor        string                    `json:"monitor"`
	Owner          string                    `json:"owner"`
	Repo           string                    `json:"repo"`
	PullRequest    int                       `json:"pull_request"`
	StoreRef       string                    `json:"store_ref"`
	HeadSHA        string                    `json:"head_sha"`
	BaseSHA        string                    `json:"base_sha"`
	ObservedAt     time.Time                 `json:"observed_at"`
	FreshUntil     time.Time                 `json:"fresh_until"`
	EvidenceState  string                    `json:"evidence_state"`
	Action         PRActionOption            `json:"action"`
	RequiredChecks []string                  `json:"required_checks"`
	Attempt        *PRActionAttemptReference `json:"attempt,omitempty"`
}

func capturePRActionVerdict(item PRActionQueueItem, request PRActionRequest, monitor PRActionPolicyMonitor) *PRActionPolicyVerdict {
	verdict := &PRActionPolicyVerdict{
		PolicyVersion: item.PolicyVersion, Monitor: item.Monitor, Owner: item.Owner,
		Repo: item.Repo, PullRequest: item.PullRequest, StoreRef: "rig:" + monitor.Rig,
		HeadSHA: item.HeadSHA, BaseSHA: item.BaseSHA,
		ObservedAt: item.ObservedAt, FreshUntil: item.FreshUntil, EvidenceState: item.EvidenceState,
		RequiredChecks: append([]string{}, monitor.RequiredChecks...),
		Action:         PRActionOption{Action: request.Action, Reason: "action not offered by the observed queue"},
	}
	for _, option := range item.Actions {
		if option.Action == request.Action {
			verdict.Action = option
			break
		}
	}
	for _, ref := range item.AttemptEvidence {
		if ref.WorkID == request.WorkID && ref.AttemptID == request.AttemptID && ref.BaseSHA == request.BaseSHA && ref.CandidateSHA == request.HeadSHA {
			matchedRef := ref
			verdict.Attempt = &matchedRef
			break
		}
	}
	return verdict
}
