package workbench

// ArtifactState is the availability of an artifact recorded for one attempt.
type ArtifactState string

const (
	// ArtifactAvailable means Gas City has an authoritative artifact for the attempt.
	ArtifactAvailable ArtifactState = "available"
	// ArtifactUnavailable means Gas City has no authoritative artifact to show.
	ArtifactUnavailable ArtifactState = "unavailable"
)

// ArtifactAvailability explains whether a persisted attempt artifact exists.
type ArtifactAvailability struct {
	State  ArtifactState `json:"state"`
	Reason string        `json:"reason,omitempty"`
}

// HistoricalDiff is a snapshot owned by one execution attempt, not a live
// worktree read. Text is omitted unless an authoritative snapshot is recorded.
type HistoricalDiff struct {
	State     ArtifactState `json:"state"`
	Reason    string        `json:"reason,omitempty"`
	Text      string        `json:"text,omitempty"`
	Truncated bool          `json:"truncated,omitempty"`
	Binary    bool          `json:"binary,omitempty"`
	Bytes     int           `json:"bytes,omitempty"`
}

// HistoricalPullRequest is the PR state recorded against one execution attempt.
type HistoricalPullRequest struct {
	State  ArtifactState `json:"state"`
	Reason string        `json:"reason,omitempty"`
	URL    string        `json:"url,omitempty"`
	Status string        `json:"status,omitempty"`
}

// AttemptInspection is the attempt-scoped Workbench read model. Its fields
// describe persisted Gas City records only; a live worktree or a Bead's current
// metadata must never be substituted for historical artifacts.
type AttemptInspection struct {
	BeadID      string                `json:"bead_id"`
	SessionID   string                `json:"session_id"`
	Association ArtifactAvailability  `json:"association"`
	Diff        HistoricalDiff        `json:"diff"`
	PullRequest HistoricalPullRequest `json:"pull_request"`
}

// NewAttemptInspection projects the records available for one selected
// attempt. Current Gas City session and bead records do not persist a frozen
// historical diff or attempt-specific PR state, so those artifacts remain
// explicitly unavailable even when the session link itself is known.
func NewAttemptInspection(beadID, sessionID string, linked bool) AttemptInspection {
	association := ArtifactAvailability{
		State:  ArtifactUnavailable,
		Reason: "attempt_session_link_not_recorded",
	}
	if linked {
		association = ArtifactAvailability{State: ArtifactAvailable}
	}
	return AttemptInspection{
		BeadID:      beadID,
		SessionID:   sessionID,
		Association: association,
		Diff: HistoricalDiff{
			State:  ArtifactUnavailable,
			Reason: "historical_diff_not_recorded",
		},
		PullRequest: HistoricalPullRequest{
			State:  ArtifactUnavailable,
			Reason: "attempt_pr_state_not_recorded",
		},
	}
}
