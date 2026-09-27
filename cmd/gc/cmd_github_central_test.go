package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/api"
)

func TestGitHubPRBackfillUsesCentralQueueWithoutLocalMonitorPolicy(t *testing.T) {
	cityPath := writeBeadsTestCity(t)
	t.Setenv("GC_NO_API", "")
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method != http.MethodGet || r.URL.Path != "/v0/city/test-city/pr-actions/queue" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"availability":"ready","policy_state":"ready","policy_version":"signed-policy","observed_at":"2026-09-27T00:00:00Z","fresh_until":"2026-09-27T00:00:30Z","sources":[{"monitor":"central","owner":"example","repo":"project","rig":"project","state":"ready"}],"items":[{"monitor":"central","owner":"example","repo":"project","pull_request":7,"title":"Repair","base_ref_name":"main","head_sha":%q,"base_sha":%q,"merge_state":"BLOCKED","is_draft":false,"policy_version":"signed-policy","observed_at":"2026-09-27T00:00:00Z","fresh_until":"2026-09-27T00:00:30Z","work_records":[],"evidence_state":"missing","attempt_evidence":[],"actions":[{"action":"prepare","available":true,"requires_human_approval":false,"reason":"server decision"}]}]}`, strings.Repeat("a", 40), strings.Repeat("b", 40))
	}))
	defer srv.Close()
	oldAlive, oldSupervisor := apiRouteControllerAliveHook, apiRouteSupervisorClientHook
	t.Cleanup(func() { apiRouteControllerAliveHook, apiRouteSupervisorClientHook = oldAlive, oldSupervisor })
	// The local configuration has no monitor policy or standalone API port.
	apiRouteControllerAliveHook = func(string) int { return 1 }
	apiRouteSupervisorClientHook = func(string) *api.Client { return api.NewCityScopedClient(srv.URL, "test-city") }
	var out, errOut bytes.Buffer
	if code := run([]string{"--city", cityPath, "github", "pr", "backfill", "--json"}, &out, &errOut); code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, &errOut)
	}
	if requests != 1 || !strings.Contains(out.String(), "server decision") || !strings.Contains(out.String(), "signed-policy") {
		t.Fatalf("requests=%d output=%s; want unmodified central verdict", requests, &out)
	}
}

func TestGitHubPRActionRemoteTargetNeedsNoLocalCity(t *testing.T) {
	configureIsolatedRuntimeEnv(t)
	t.Setenv("GC_CITY", "")
	t.Setenv("GC_NO_API", "")
	t.Setenv("GC_CITY_URL_TOKEN", "private-test-bearer")
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Path != "/v0/city/remote-city/pr-actions" || r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer private-test-bearer" {
			t.Error("action did not use the resolved authenticated remote target")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var request api.PRActionRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		request.IdempotencyKey = r.Header.Get("Idempotency-Key")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(centralPRActionReceipt(request))
	}))
	defer server.Close()
	args := []string{"--city-url", server.URL, "--city-name", "remote-city", "github", "pr", "action", "prepare", "--repo", "example/project", "--monitor", "central", "--pr", "7", "--head-sha", strings.Repeat("a", 40), "--base-sha", strings.Repeat("b", 40), "--policy-version", "policy-v1", "--idempotency-key", "remote-request-7"}
	var out, errOut bytes.Buffer
	if code := run(args, &out, &errOut); code != 0 {
		t.Fatalf("remote command failed: exit=%d stderr=%s", code, &errOut)
	}
	if requests != 1 || !strings.Contains(out.String(), "work_prepared") {
		t.Fatalf("requests=%d output=%s", requests, &out)
	}
	if strings.Contains(out.String()+errOut.String(), "private-test-bearer") {
		t.Fatal("transport credential exposed in command output")
	}
}

func TestGitHubPRBackfillRemoteTargetOmitsURLCredentials(t *testing.T) {
	configureIsolatedRuntimeEnv(t)
	t.Setenv("GC_CITY", "")
	t.Setenv("GC_NO_API", "")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(api.PRActionQueue{
			Availability: api.PRActionAvailabilityReady, PolicyState: api.PRActionSourceReady,
			Sources: []api.PRActionSource{}, Items: []api.PRActionQueueItem{},
		})
	}))
	defer server.Close()
	remote, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	remote.User = url.UserPassword("private-url-user", "private-url-password")
	remote.RawQuery = "token=private-query-token"
	remote.Fragment = "private-fragment"
	var out, errOut bytes.Buffer
	if code := run([]string{"--city-url", remote.String(), "--city-name", "remote-city", "github", "pr", "backfill", "--json"}, &out, &errOut); code != 0 {
		t.Fatalf("remote backfill failed with exit %d", code)
	}
	for _, secret := range []string{"private-url-user", "private-url-password", "private-query-token", "private-fragment"} {
		if strings.Contains(out.String()+errOut.String(), secret) {
			t.Fatal("configured URL credentials or private URL components leaked in command output")
		}
	}
	var result githubPRBackfillResult
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Target != server.URL+"/v0/city/remote-city" {
		t.Fatal("remote target did not retain its public server and city identity")
	}
}
