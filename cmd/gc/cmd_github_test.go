package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/api"
)

func centralPRTestQueue() api.PRActionQueue {
	now := time.Now().UTC()
	return api.PRActionQueue{
		Availability: api.PRActionAvailabilityReady, PolicyState: api.PRActionSourceReady,
		PolicyVersion: "policy-v1", ObservedAt: now, FreshUntil: now.Add(time.Minute),
		Sources: []api.PRActionSource{{Monitor: "central", Owner: "example", Repo: "project", Rig: "project", State: api.PRActionSourceReady}},
		Items:   []api.PRActionQueueItem{{Monitor: "central", Owner: "example", Repo: "project", PullRequest: 7, Title: "Repair", HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), PolicyVersion: "policy-v1", ObservedAt: now, FreshUntil: now.Add(time.Minute), Actions: []api.PRActionOption{{Action: api.PRActionPrepare, Available: true, Reason: "server-approved repair"}}}},
	}
}

func useCentralPRTestServer(t *testing.T, handler http.HandlerFunc) string {
	t.Helper()
	cityPath := writeBeadsTestCity(t)
	t.Setenv("GC_NO_API", "")
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	oldAlive, oldSupervisor := apiRouteControllerAliveHook, apiRouteSupervisorClientHook
	t.Cleanup(func() { apiRouteControllerAliveHook, apiRouteSupervisorClientHook = oldAlive, oldSupervisor })
	apiRouteControllerAliveHook = func(string) int { return 1 }
	apiRouteSupervisorClientHook = func(string) *api.Client { return api.NewCityScopedClient(srv.URL, "test-city") }
	return cityPath
}

func centralPRActionReceipt(request api.PRActionRequest) api.PRActionResult {
	outcome := api.PRActionOutcomePrepared
	workID := request.WorkID
	if workID == "" {
		workID = "work-1"
	}
	if request.Action == api.PRActionQueueReview {
		outcome = api.PRActionOutcomeReviewQueued
	}
	return api.PRActionResult{ID: "action-1", Action: request.Action, Status: api.PRActionStatusVerified, Outcome: outcome, IdempotencyKey: request.IdempotencyKey, Monitor: request.Monitor, Owner: request.Owner, Repo: request.Repo, PullRequest: request.PullRequest, WorkID: workID, AttemptID: request.AttemptID, HeadSHA: request.HeadSHA, BaseSHA: request.BaseSHA, PolicyVersion: request.PolicyVersion, ActorKeyID: "worker-key", CreatedAt: time.Now().UTC(), VerifiedAt: time.Now().UTC()}
}

func TestGitHubPRBackfillPrepareUsesExactServerVerdictAndStableKey(t *testing.T) {
	queue := centralPRTestQueue()
	var keys []string
	cityPath := useCentralPRTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet && r.URL.Path == "/v0/city/test-city/pr-actions/queue" {
			_ = json.NewEncoder(w).Encode(queue)
			return
		}
		if r.Method != http.MethodPost || r.URL.Path != "/v0/city/test-city/pr-actions" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(400)
			return
		}
		var wire map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&wire); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		if _, found := wire["idempotency_key"]; found {
			t.Error("idempotency key sent in JSON body")
		}
		encoded, _ := json.Marshal(wire)
		var request api.PRActionRequest
		if err := json.Unmarshal(encoded, &request); err != nil {
			t.Fatal(err)
		}
		request.IdempotencyKey = r.Header.Get("Idempotency-Key")
		if request.HeadSHA != strings.Repeat("a", 40) || request.BaseSHA != strings.Repeat("b", 40) || request.PolicyVersion != "policy-v1" || request.Owner != "example" || request.Repo != "project" || request.PullRequest != 7 || request.Monitor != "central" || request.Action != api.PRActionPrepare {
			t.Errorf("wrong exact request: %+v", request)
		}
		if request.IdempotencyKey == "" || r.Header.Get("X-GC-Request") == "" {
			t.Error("missing idempotency or CSRF header")
		}
		keys = append(keys, request.IdempotencyKey)
		_ = json.NewEncoder(w).Encode(centralPRActionReceipt(request))
	})
	for range 2 {
		var out, errOut bytes.Buffer
		if code := run([]string{"--city", cityPath, "github", "pr", "backfill", "--create-repair-beads", "--json"}, &out, &errOut); code != 0 {
			t.Fatalf("exit=%d stderr=%s", code, &errOut)
		}
		validateJSONResultSchema(t, []string{"github", "pr", "backfill"}, out.Bytes())
		if !strings.Contains(out.String(), "work_prepared") || strings.Contains(out.String(), "dispatched") {
			t.Fatalf("wrong outcome: %s", &out)
		}
	}
	if len(keys) != 2 || keys[0] != keys[1] {
		t.Fatalf("retry keys=%v", keys)
	}
}

func TestGitHubPRBackfillUnavailableSourcesDoNotPrepare(t *testing.T) {
	queue := centralPRTestQueue()
	queue.Availability = api.PRActionAvailabilityPartial
	queue.Sources = append(queue.Sources, api.PRActionSource{Monitor: "missing", State: api.PRActionSourceUnavailable, Detail: "forge unavailable"})
	writes := 0
	cityPath := useCentralPRTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writes++
			w.WriteHeader(500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(queue)
	})
	var out, errOut bytes.Buffer
	if code := run([]string{"--city", cityPath, "github", "pr", "backfill", "--create-repair-beads"}, &out, &errOut); code == 0 {
		t.Fatal("partial source allowed repair submission")
	}
	if writes != 0 {
		t.Fatalf("writes=%d", writes)
	}
	out.Reset()
	errOut.Reset()
	if code := run([]string{"--city", cityPath, "github", "pr", "backfill", "--json"}, &out, &errOut); code != 0 || !strings.Contains(out.String(), "forge unavailable") {
		t.Fatalf("partial read lost source state: exit=%d stdout=%s stderr=%s", code, &out, &errOut)
	}
}

func TestGitHubPRBackfillServerErrorsNeverUseLocalPolicy(t *testing.T) {
	for _, status := range []int{404, 409, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			requests := 0
			cityPath := useCentralPRTestServer(t, func(w http.ResponseWriter, _ *http.Request) {
				requests++
				w.Header().Set("Content-Type", "application/problem+json")
				w.WriteHeader(status)
				_, _ = fmt.Fprintf(w, `{"status":%d,"detail":"central authority unavailable"}`, status)
			})
			var out, errOut bytes.Buffer
			if code := run([]string{"--city", cityPath, "github", "pr", "backfill", "--create-repair-beads"}, &out, &errOut); code == 0 {
				t.Fatal("server error reported success")
			}
			if requests != 1 {
				t.Fatalf("requests=%d; want one read, no retry or mutation", requests)
			}
			if !strings.Contains(errOut.String(), "central") {
				t.Fatalf("missing server error: %s", &errOut)
			}
		})
	}
}

func TestGitHubPRActionPreservesExactAttemptAndRejectsUnknownOrMismatchedReceipts(t *testing.T) {
	for _, variant := range []string{"verified", "unknown", "wrong-base", "stale"} {
		t.Run(variant, func(t *testing.T) {
			requests := 0
			cityPath := useCentralPRTestServer(t, func(w http.ResponseWriter, r *http.Request) {
				requests++
				if r.Method != http.MethodPost || r.URL.Path != "/v0/city/test-city/pr-actions" {
					t.Error("action unexpectedly queried or changed its supplied revision")
					w.WriteHeader(400)
					return
				}
				var request api.PRActionRequest
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Error(err)
					w.WriteHeader(400)
					return
				}
				request.IdempotencyKey = r.Header.Get("Idempotency-Key")
				if request.WorkID != "work-7" || request.AttemptID != "attempt-2" || request.IdempotencyKey != "explicit-request-7" {
					t.Errorf("identity changed: %+v", request)
				}
				if variant == "stale" {
					w.Header().Set("Content-Type", "application/problem+json")
					w.WriteHeader(409)
					_, _ = fmt.Fprint(w, `{"status":409,"detail":"stale revision"}`)
					return
				}
				receipt := centralPRActionReceipt(request)
				if variant == "unknown" {
					receipt.Status = api.PRActionStatusUnknown
				}
				if variant == "wrong-base" {
					receipt.BaseSHA = strings.Repeat("c", 40)
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(receipt)
			})
			args := []string{"--city", cityPath, "github", "pr", "action", "queue_review", "--repo", "example/project", "--monitor", "central", "--pr", "7", "--head-sha", strings.Repeat("a", 40), "--base-sha", strings.Repeat("b", 40), "--policy-version", "policy-v1", "--idempotency-key", "explicit-request-7", "--work-id", "work-7", "--attempt-id", "attempt-2"}
			var out, errOut bytes.Buffer
			code := run(args, &out, &errOut)
			if variant == "verified" {
				if code != 0 {
					t.Fatalf("exit=%d stderr=%s", code, &errOut)
				}
				validateJSONResultSchema(t, []string{"github", "pr", "action"}, out.Bytes())
			} else {
				if code == 0 {
					t.Fatalf("%s receipt reported success", variant)
				}
				if errOut.Len() == 0 {
					t.Fatalf("%s rejection has no diagnostic", variant)
				}
			}
			if requests != 1 {
				t.Fatalf("requests=%d; action must not silently retry", requests)
			}
		})
	}
}

func TestGitHubPRActionsRespectAPIDisableAndMergeDeferral(t *testing.T) {
	requests := 0
	cityPath := useCentralPRTestServer(t, func(w http.ResponseWriter, _ *http.Request) { requests++; w.WriteHeader(500) })
	t.Setenv("GC_NO_API", "1")
	var out, errOut bytes.Buffer
	if code := run([]string{"--city", cityPath, "github", "pr", "backfill", "--create-repair-beads"}, &out, &errOut); code == 0 {
		t.Fatal("API disable allowed local repair")
	}
	t.Setenv("GC_NO_API", "")
	out.Reset()
	errOut.Reset()
	cmd := newGitHubPRActionCmd(&out, &errOut)
	cmd.SetArgs([]string{"merge", "--repo", "example/project", "--monitor", "central", "--pr", "7", "--head-sha", strings.Repeat("a", 40), "--base-sha", strings.Repeat("b", 40), "--policy-version", "policy-v1", "--idempotency-key", "explicit-request-7"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("GitHub merge enabled")
	}
	if requests != 0 {
		t.Fatalf("requests=%d; unavailable actions must not send requests", requests)
	}
}
