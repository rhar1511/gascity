package workbench

import "testing"

func TestNewAttemptInspectionKeepsHistoricalArtifactsUnavailable(t *testing.T) {
	got := NewAttemptInspection("bead-1", "session-1", true)
	if got.BeadID != "bead-1" || got.SessionID != "session-1" {
		t.Fatalf("identity = (%q, %q), want (bead-1, session-1)", got.BeadID, got.SessionID)
	}
	if got.Association.State != ArtifactAvailable {
		t.Fatalf("association = %+v, want recorded link", got.Association)
	}
	if got.Diff.State != ArtifactUnavailable || got.Diff.Reason != "historical_diff_not_recorded" || got.Diff.Text != "" {
		t.Fatalf("diff = %+v, want explicit unavailable without text", got.Diff)
	}
	if got.PullRequest.State != ArtifactUnavailable || got.PullRequest.Reason != "attempt_pr_state_not_recorded" || got.PullRequest.URL != "" {
		t.Fatalf("pull request = %+v, want explicit unavailable without URL", got.PullRequest)
	}
}

func TestNewAttemptInspectionReportsMissingAttemptLink(t *testing.T) {
	got := NewAttemptInspection("bead-1", "session-1", false)
	if got.Association.State != ArtifactUnavailable || got.Association.Reason != "attempt_session_link_not_recorded" {
		t.Fatalf("association = %+v, want explicit unavailable link", got.Association)
	}
}
