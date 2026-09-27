package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

func TestPRActionRoutesReportUnavailableSourceAndRequireVerifiedWriter(t *testing.T) {
	t.Setenv("GH_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "")
	state := newFakeState(t)
	state.cfg.GitHub.PRMonitors = []config.GitHubPRMonitor{{Name: "pilot", Owner: "acme", Repo: "widget", Rig: "myrig", BaseBranches: []string{"main"}}}
	sm := NewSupervisorMux(&singleStateResolver{state: state}, nil, false, "test", "", time.Now()).WithAnyHostAllowed()

	get := httptest.NewRequest(http.MethodGet, "/v0/city/test-city/pr-actions/queue", nil)
	getRec := httptest.NewRecorder()
	sm.ServeHTTP(getRec, get)
	if getRec.Code != http.StatusOK {
		t.Fatalf("GET status = %d body=%s", getRec.Code, getRec.Body.String())
	}
	var queue PRActionQueue
	if err := json.Unmarshal(getRec.Body.Bytes(), &queue); err != nil {
		t.Fatalf("decode queue: %v; body=%s", err, getRec.Body.String())
	}
	if queue.Availability != PRActionAvailabilityUnknown || queue.PolicyState != PRActionSourceUnavailable {
		t.Fatalf("missing signed host policy was hidden as an empty queue: %+v", queue)
	}

	postBody := `{"monitor":"pilot","owner":"acme","repo":"widget","pull_request":12,"action":"merge","work_id":"gc-123","attempt_id":"ae-attempt-1","head_sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","base_sha":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","policy_version":"policy-test"}`
	post := httptest.NewRequest(http.MethodPost, "/v0/city/test-city/pr-actions", strings.NewReader(postBody))
	post.Header.Set(csrfHeaderName, "present")
	post.Header.Set("Idempotency-Key", "route-action-1234")
	postRec := httptest.NewRecorder()
	sm.ServeHTTP(postRec, post)
	if postRec.Code != http.StatusForbidden {
		t.Fatalf("unauthenticated action status = %d body=%s", postRec.Code, postRec.Body.String())
	}
	if got := listAllActionRecords(t, state); got != 0 {
		t.Fatalf("unauthenticated request wrote %d durable actions", got)
	}
}

func TestPRActionRoutesAppearInOpenAPISchema(t *testing.T) {
	sm := NewSupervisorMux(nil, nil, false, "test", "", time.Now())
	spec := sm.humaAPI.OpenAPI()
	if _, ok := spec.Paths["/v0/city/{cityName}/pr-actions/queue"]; !ok {
		t.Fatal("OpenAPI is missing the PR action queue route")
	}
	if _, ok := spec.Paths["/v0/city/{cityName}/pr-actions"]; !ok {
		t.Fatal("OpenAPI is missing the PR action execute route")
	}
}

func listAllActionRecords(t *testing.T, state *fakeState) int {
	t.Helper()
	rows, err := state.stores["myrig"].List(beads.ListQuery{Metadata: map[string]string{"gc.pr_action.source": "api"}, IncludeClosed: true})
	if err != nil {
		t.Fatal(err)
	}
	return len(rows)
}
