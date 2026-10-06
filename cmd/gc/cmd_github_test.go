package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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

type fakeGitHubPRActionClient struct {
	queue          api.PRActionQueue
	queueErr       error
	queueCalls     int
	actionRequests []api.PRActionRequest
	execute        func(api.PRActionRequest) (api.PRActionResult, error)
}

func (c *fakeGitHubPRActionClient) GetPRActionQueue(context.Context) (api.PRActionQueue, error) {
	c.queueCalls++
	return c.queue, c.queueErr
}

func (c *fakeGitHubPRActionClient) ExecutePRAction(_ context.Context, request api.PRActionRequest) (api.PRActionResult, error) {
	c.actionRequests = append(c.actionRequests, request)
	if c.execute != nil {
		return c.execute(request)
	}
	return centralPRActionReceipt(request), nil
}

func useCentralPRTestClient(t *testing.T, client githubPRActionAPI) string {
	t.Helper()
	return useCentralPRTestClientAt(t, writeBeadsTestCity(t), client)
}

func useCentralPRTestClientAt(t *testing.T, cityPath string, client githubPRActionAPI) string {
	t.Helper()
	t.Setenv("GC_NO_API", "")
	previous := githubPRActionClientForCommand
	t.Cleanup(func() { githubPRActionClientForCommand = previous })
	githubPRActionClientForCommand = func() (string, githubPRActionAPI, error) {
		return cityPath, client, nil
	}
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
	client := &fakeGitHubPRActionClient{queue: centralPRTestQueue()}
	cityPath := useCentralPRTestClient(t, client)
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
	if client.queueCalls != 2 || len(client.actionRequests) != 2 {
		t.Fatalf("queue calls/actions = %d/%d, want 2/2", client.queueCalls, len(client.actionRequests))
	}
	first, second := client.actionRequests[0], client.actionRequests[1]
	if first.HeadSHA != strings.Repeat("a", 40) || first.BaseSHA != strings.Repeat("b", 40) || first.PolicyVersion != "policy-v1" || first.Owner != "example" || first.Repo != "project" || first.PullRequest != 7 || first.Monitor != "central" || first.Action != api.PRActionPrepare {
		t.Fatalf("wrong exact request: %+v", first)
	}
	if first.IdempotencyKey == "" || first.IdempotencyKey != second.IdempotencyKey {
		t.Fatalf("retry keys = %q / %q; want same stable key", first.IdempotencyKey, second.IdempotencyKey)
	}
}

func TestGitHubPRBackfillUnavailableSourcesDoNotPrepare(t *testing.T) {
	queue := centralPRTestQueue()
	queue.Availability = api.PRActionAvailabilityPartial
	queue.Sources = append(queue.Sources, api.PRActionSource{Monitor: "missing", State: api.PRActionSourceUnavailable, Detail: "forge unavailable"})
	client := &fakeGitHubPRActionClient{queue: queue}
	cityPath := useCentralPRTestClient(t, client)
	var out, errOut bytes.Buffer
	if code := run([]string{"--city", cityPath, "github", "pr", "backfill", "--create-repair-beads"}, &out, &errOut); code == 0 {
		t.Fatal("partial source allowed repair submission")
	}
	if len(client.actionRequests) != 0 {
		t.Fatalf("actions=%d", len(client.actionRequests))
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
			client := &fakeGitHubPRActionClient{queueErr: fmt.Errorf("HTTP %d: central authority unavailable", status)}
			cityPath := useCentralPRTestClient(t, client)
			var out, errOut bytes.Buffer
			if code := run([]string{"--city", cityPath, "github", "pr", "backfill", "--create-repair-beads"}, &out, &errOut); code == 0 {
				t.Fatal("server error reported success")
			}
			if client.queueCalls != 1 || len(client.actionRequests) != 0 {
				t.Fatalf("queue calls/actions = %d/%d; want one read and no mutation", client.queueCalls, len(client.actionRequests))
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
			client := &fakeGitHubPRActionClient{execute: func(request api.PRActionRequest) (api.PRActionResult, error) {
				if request.WorkID != "work-7" || request.AttemptID != "attempt-2" || request.IdempotencyKey != "explicit-request-7" {
					t.Errorf("identity changed: %+v", request)
				}
				if variant == "stale" {
					return api.PRActionResult{}, errors.New("stale revision")
				}
				receipt := centralPRActionReceipt(request)
				if variant == "unknown" {
					receipt.Status = api.PRActionStatusUnknown
				}
				if variant == "wrong-base" {
					receipt.BaseSHA = strings.Repeat("c", 40)
				}
				return receipt, nil
			}}
			cityPath := useCentralPRTestClient(t, client)
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
			if len(client.actionRequests) != 1 {
				t.Fatalf("actions=%d; action must not silently retry", len(client.actionRequests))
			}
		})
	}
}

func TestGitHubPRActionsRespectAPIDisableAndMergeDeferral(t *testing.T) {
	cityPath := writeBeadsTestCity(t)
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
}
