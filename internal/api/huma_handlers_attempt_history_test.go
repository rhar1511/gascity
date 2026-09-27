package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/testutil"
)

func TestAttemptHistoryDoesNotUseCurrentBeadDiffForPriorSession(t *testing.T) {
	state := newFakeState(t)
	store := beads.NewMemStore()
	state.cityBeadStore = store
	state.stores["myrig"] = store

	currentWorktree, _ := testutil.InitGitRepo(t)
	currentFile := filepath.Join(currentWorktree, "current.txt")
	if err := os.WriteFile(currentFile, []byte("baseline\n"), 0o644); err != nil {
		t.Fatalf("write current worktree baseline: %v", err)
	}
	testutil.RunGit(t, currentWorktree, "add", "current.txt")
	testutil.RunGit(t, currentWorktree, "commit", "-m", "baseline")
	if err := os.WriteFile(currentFile, []byte("current-only-change\n"), 0o644); err != nil {
		t.Fatalf("write current worktree change: %v", err)
	}
	bead, err := store.Create(beads.Bead{
		Title: "work",
		Type:  "task",
		Metadata: beads.StringMap{
			beadmeta.WorkDirMetadataKey:   currentWorktree,
			beadmeta.SessionIDMetadataKey: "current-session",
		},
	})
	if err != nil {
		t.Fatalf("create work bead: %v", err)
	}
	prior, err := store.Create(beads.Bead{
		Title:  "prior session",
		Type:   session.BeadType,
		Labels: []string{session.LabelSession},
		Metadata: beads.StringMap{
			"session_name": "worker-prior",
			"work_dir":     "/gone/prior-worktree",
		},
	})
	if err != nil {
		t.Fatalf("create prior session bead: %v", err)
	}

	handler := newTestCityHandler(t, state)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet,
		cityURL(state, path.Join("/bead", bead.ID, "attempts", prior.ID, "history")), nil)
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("history status = %d, want %d; body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
	}

	var response struct {
		BeadID    string `json:"bead_id"`
		SessionID string `json:"session_id"`
		Diff      struct {
			State  string `json:"state"`
			Reason string `json:"reason"`
			Text   string `json:"text"`
		} `json:"diff"`
		PullRequest struct {
			State  string `json:"state"`
			Reason string `json:"reason"`
			URL    string `json:"url"`
		} `json:"pull_request"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode history response: %v; body=%s", err, recorder.Body.String())
	}
	if response.BeadID != bead.ID || response.SessionID != prior.ID {
		t.Fatalf("history identity = (%q, %q), want (%q, %q)", response.BeadID, response.SessionID, bead.ID, prior.ID)
	}
	if response.Diff.State != "unavailable" || response.Diff.Reason != "historical_diff_not_recorded" {
		t.Fatalf("historical diff = %+v, want explicit not-recorded state", response.Diff)
	}
	if response.Diff.Text != "" || strings.Contains(recorder.Body.String(), "current-only-change") {
		t.Fatalf("history response leaked current worktree content: %s", recorder.Body.String())
	}
	if response.PullRequest.State != "unavailable" || response.PullRequest.Reason != "attempt_pr_state_not_recorded" || response.PullRequest.URL != "" {
		t.Fatalf("historical pull request = %+v, want explicit not-recorded state", response.PullRequest)
	}
}
