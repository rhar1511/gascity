package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gastownhall/gascity/internal/worklifecycle"
)

type lifecycleWorkspaceTestState struct {
	*fakeState
	publish func(context.Context, LifecycleWorkspaceTransitionRequest) (LifecycleWorkspaceResult, error)
}

func (s *lifecycleWorkspaceTestState) PublishLifecycleWorkspace(ctx context.Context, r LifecycleWorkspaceTransitionRequest) (LifecycleWorkspaceResult, error) {
	return s.publish(ctx, r)
}

func TestLifecycleWorkspaceAPIResolvesExactOwnerAndRefusesGenericFields(t *testing.T) {
	for _, scenario := range []string{"exact", "foreign store", "step substituted", "metadata injection", "zero revision", "missing provider authority"} {
		t.Run(scenario, func(t *testing.T) {
			original, store, _, work := newLifecycleClaimTestStateWithRevision(t, -7)
			state := &lifecycleWorkspaceTestState{fakeState: original.fakeState}
			calls := 0
			state.publish = func(_ context.Context, r LifecycleWorkspaceTransitionRequest) (LifecycleWorkspaceResult, error) {
				calls++
				if r.Work.ID != work.ID || r.WorkStore != store || r.Submission.ExpectedRevision != -7 || r.Submission.ExpectedTransitionHead != "head-1" {
					t.Fatalf("publication lost exact source snapshot: %+v", r)
				}
				if scenario == "missing provider authority" {
					return LifecycleWorkspaceResult{}, worklifecycle.ErrTransitionChainPermit
				}
				return LifecycleWorkspaceResult{WorkID: work.ID, ReceiptID: "workspace-1", Revision: -9}, nil
			}
			body := map[string]any{
				"work_id": work.ID, "source_store_ref": "city:test-city", "expected_revision": -7, "expected_transition_head": "head-1",
				"workspace": map[string]any{"source_id": work.ID, "store_ref": "city:test-city", "path": "/tmp/source", "repo_dir": "/tmp/repo", "root": "/tmp", "branch": "work/source", "base_ref": "main", "base_sha": "1111111111111111111111111111111111111111", "creator": "gc-worktree", "owner": "worker", "generation": "1", "lifecycle": "active", "attempt_id": "native-attempt"},
			}
			want := http.StatusOK
			switch scenario {
			case "foreign store":
				body["source_store_ref"] = "rig:other"
				body["workspace"].(map[string]any)["store_ref"] = "rig:other"
				want = http.StatusConflict
			case "step substituted":
				body["workspace"].(map[string]any)["source_id"] = "different-step"
				want = http.StatusBadRequest
			case "metadata injection":
				body["metadata"] = map[string]string{"gc.work_dir": "/tmp/forged"}
				want = http.StatusUnprocessableEntity
			case "zero revision":
				body["expected_revision"] = 0
				want = http.StatusBadRequest
			case "missing provider authority":
				want = http.StatusServiceUnavailable
			}
			raw, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			response := httptest.NewRecorder()
			newTestCityHandler(t, state).ServeHTTP(response, newPostRequest(cityURL(state, "/lifecycle/workspaces"), bytes.NewReader(raw)))
			if response.Code != want {
				t.Fatalf("HTTP %d want %d: %s", response.Code, want, response.Body.String())
			}
			wantCalls := 0
			if scenario == "exact" || scenario == "missing provider authority" {
				wantCalls = 1
			}
			if calls != wantCalls {
				t.Fatalf("provider calls %d want %d", calls, wantCalls)
			}
		})
	}
}
