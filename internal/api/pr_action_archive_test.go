package api

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/attemptevidence"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/testutil"
)

func TestPRActionServerReadsSealedAttemptAfterBranchMovement(t *testing.T) {
	fx := newPRActionFixture(t, false)
	path := t.TempDir()
	open := func() *beads.SQLiteStore {
		t.Helper()
		opened, err := beads.OpenSQLiteStore(path)
		if err != nil {
			t.Fatal(err)
		}
		store := opened.(*beads.SQLiteStore)
		t.Cleanup(func() { _ = store.CloseStore() })
		return store
	}
	store := open()
	fx.state.stores["myrig"] = store
	repo, _ := testutil.InitGitRepo(t)
	base := testutil.RunGit(t, repo, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(repo, "result.txt"), []byte("reviewed candidate\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	testutil.RunGit(t, repo, "add", "result.txt")
	testutil.RunGit(t, repo, "commit", "-m", "candidate")
	head := testutil.RunGit(t, repo, "rev-parse", "HEAD")
	work, err := store.Create(beads.Bead{Title: "repair", Type: "task", Labels: []string{"github", "repair", "pr-monitor"}, Metadata: beads.StringMap{
		"source": "github-pr-monitor", "github.owner": "acme", "github.repo": "widget", "github.pr": "12",
		"github.head_sha": head, "github.base_sha": base, "github.monitor": "pilot", "gc.routed_to": "myrig/worker",
	}})
	if err != nil {
		t.Fatal(err)
	}
	spec := attemptevidence.CaptureSpec{
		Identity: attemptevidence.Identity{Kind: attemptevidence.KindRetry, OwnerBeadID: work.ID, ExecutionBeadID: "execution-one"},
		StoreRef: "rig:myrig", WorkDir: repo, BaseSHA: base,
	}
	first, err := attemptevidence.Capture(context.Background(), store, spec)
	if err != nil {
		t.Fatal(err)
	}
	// A later clean commit and separate execution must not replace the exact
	// archive used to evaluate the still-current forge revision.
	testutil.RunGit(t, repo, "commit", "--allow-empty", "-m", "later attempt")
	spec.Identity.ExecutionBeadID = "execution-two"
	second, err := attemptevidence.Capture(context.Background(), store, spec)
	if err != nil {
		t.Fatal(err)
	}
	if first.AttemptID == second.AttemptID || first.CandidateSHA == second.CandidateSHA {
		t.Fatal("fixture must preserve two distinct executions and candidate commits")
	}
	fx.forge.pullRequests[0].BaseSHA = base
	fx.forge.pullRequests[0].HeadSHA = head
	server := New(fx.state)
	reader := server.prActions().evidence
	if reader == nil {
		t.Fatal("server has no archive reader composed for central PR decisions")
	}
	fx.service.evidence = reader
	queue, err := fx.service.Queue(context.Background())
	if err != nil || len(queue.Items) != 1 {
		t.Fatalf("queue=%+v err=%v", queue, err)
	}
	item := queue.Items[0]
	if item.EvidenceState != PRActionEvidenceVerified || !item.HasAction(PRActionQueueReview) || len(item.AttemptEvidence) != 1 {
		t.Fatalf("exact archived evidence did not permit review: %+v", item)
	}
	ref := item.AttemptEvidence[0]
	if ref.AttemptID != first.AttemptID || ref.BaseSHA != base || ref.CandidateSHA != head || ref.DiffSHA256 != first.Diff.SHA256 {
		t.Fatalf("queue used a different attempt: %+v", ref)
	}
	// A fresh store handle and deleted owner cannot erase the historical read.
	if err := store.Delete(work.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.CloseStore(); err != nil {
		t.Fatal(err)
	}
	store = open()
	got, err := reader.Read(store, work.ID, first.AttemptID)
	if err != nil || got.CandidateSHA != head || got.DiffSHA256 != first.Diff.SHA256 {
		t.Fatalf("historical read=%+v err=%v", got, err)
	}
	if _, err := reader.References(store, "rig:other", work.ID); err == nil {
		t.Fatal("copied archive was relabeled as belonging to another store scope")
	}
	if _, err := reader.Read(store, work.ID, "missing-attempt"); !errors.Is(err, ErrPRActionEvidenceMissing) {
		t.Fatalf("missing attempt error=%v", err)
	}
}
