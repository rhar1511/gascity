package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
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
	request := fx.actionRequest(PRActionQueueReview)
	request.WorkID, request.AttemptID = work.ID, first.AttemptID
	request.HeadSHA, request.BaseSHA = head, base
	firstAction, err := fx.service.Execute(context.Background(), request, fx.workerActor())
	if err != nil {
		t.Fatal(err)
	}
	// The same work has a separate later action, which must not appear in the
	// first attempt's historical read.
	if err := store.SetMetadata(work.ID, "github.head_sha", second.CandidateSHA); err != nil {
		t.Fatal(err)
	}
	fx.forge.pullRequests[0].HeadSHA = second.CandidateSHA
	request.AttemptID, request.HeadSHA = second.AttemptID, second.CandidateSHA
	request.IdempotencyKey += "-later"
	if _, err := fx.service.Execute(context.Background(), request, fx.workerActor()); err != nil {
		t.Fatal(err)
	}
	// A fresh store handle and deleted owner cannot erase the historical read.
	if err := store.Delete(work.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.CloseStore(); err != nil {
		t.Fatal(err)
	}
	store = open()
	fx.state.stores["myrig"] = store
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
	// History is served from durable records even when the PR and current
	// policy no longer exist. Authorization still uses the sealed scope.
	fx.forge.pullRequests = nil
	fx.service.policy = nil
	server.attemptEvidenceReadAuthorizer = attemptEvidenceAuthorizerFunc(func(_ context.Context, req attemptevidence.ReadAuthorizationRequest) error {
		if req.AttemptID != first.AttemptID || req.Scope != first.Permission {
			return ErrAttemptEvidenceReadDenied
		}
		return nil
	})
	w := httptest.NewRecorder()
	path = cityURL(fx.state, "/bead/"+work.ID+"/attempt-evidence/"+first.AttemptID)
	newTestCityHandlerWith(t, fx.state, server).ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("historical get=%d %s", w.Code, w.Body.String())
	}
	var response struct {
		attemptevidence.Evidence
		Related struct {
			Actions struct {
				Status  string `json:"status"`
				Records []struct {
					Receipt         PRActionResult        `json:"receipt"`
					AdmissionPolicy attemptevidence.Facet `json:"admission_policy"`
				} `json:"records"`
			} `json:"actions"`
			Acknowledgements attemptevidence.Facet `json:"acknowledgements"`
		} `json:"related_records"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.AttemptID != first.AttemptID || response.Diff.SHA256 != first.Diff.SHA256 {
		t.Fatal("related record composition replaced the immutable archive")
	}
	if response.Related.Actions.Status != attemptevidence.StatusAvailable || len(response.Related.Actions.Records) != 1 {
		t.Fatalf("historical action records missing or include another attempt: %+v", response.Related)
	}
	record := response.Related.Actions.Records[0]
	if record.Receipt.ID != firstAction.ID || record.Receipt.AttemptID != first.AttemptID || record.AdmissionPolicy.Status != attemptevidence.StatusAvailable || record.Receipt.AdmissionVerdict == nil {
		t.Fatalf("historical action lost its receipt or verdict: %+v", record)
	}
	if response.Related.Acknowledgements.Status != attemptevidence.StatusUnavailable {
		t.Fatal("session acknowledgements were inferred without work/attempt attribution")
	}
}
