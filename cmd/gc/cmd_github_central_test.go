package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/api"
)

func TestGitHubPRBackfillUsesCentralQueueWithoutLocalMonitorPolicy(t *testing.T) {
	queue := centralPRTestQueue()
	queue.PolicyVersion = "signed-policy"
	queue.Items[0].PolicyVersion = "signed-policy"
	queue.Items[0].Actions[0].Reason = "server decision"
	client := &fakeGitHubPRActionClient{queue: queue}
	cityPath := useCentralPRTestClient(t, client)
	var out, errOut bytes.Buffer
	if code := run([]string{"--city", cityPath, "github", "pr", "backfill", "--json"}, &out, &errOut); code != 0 {
		t.Fatalf("exit=%d stderr=%s", code, &errOut)
	}
	if client.queueCalls != 1 || !strings.Contains(out.String(), "server decision") || !strings.Contains(out.String(), "signed-policy") {
		t.Fatalf("queue calls=%d output=%s; want unmodified central verdict", client.queueCalls, &out)
	}
}

func TestGitHubPRActionRemoteTargetNeedsNoLocalCity(t *testing.T) {
	configureIsolatedRuntimeEnv(t)
	t.Setenv("GC_CITY", "")
	t.Setenv("GC_NO_API", "")
	t.Setenv("GC_CITY_URL_TOKEN", "private-test-bearer")
	client := &fakeGitHubPRActionClient{}
	var resolved *remoteTarget
	previous := githubPRActionRemoteClientBuilder
	t.Cleanup(func() { githubPRActionRemoteClientBuilder = previous })
	githubPRActionRemoteClientBuilder = func(target *remoteTarget) (githubPRActionAPI, error) {
		resolved = target
		return client, nil
	}
	args := []string{"--city-url", "https://city.example.test", "--city-name", "remote-city", "github", "pr", "action", "prepare", "--repo", "example/project", "--monitor", "central", "--pr", "7", "--head-sha", strings.Repeat("a", 40), "--base-sha", strings.Repeat("b", 40), "--policy-version", "policy-v1", "--idempotency-key", "remote-request-7"}
	var out, errOut bytes.Buffer
	if code := run(args, &out, &errOut); code != 0 {
		t.Fatalf("remote command failed: exit=%d stderr=%s", code, &errOut)
	}
	if resolved == nil || resolved.BaseURL != "https://city.example.test" || resolved.CityName != "remote-city" || resolved.Token != "private-test-bearer" {
		t.Fatalf("remote target = %+v; want sanitized endpoint, city and credential source", resolved)
	}
	if len(client.actionRequests) != 1 || !strings.Contains(out.String(), "work_prepared") {
		t.Fatalf("requests=%d output=%s", len(client.actionRequests), &out)
	}
	if strings.Contains(out.String()+errOut.String(), "private-test-bearer") {
		t.Fatal("transport credential exposed in command output")
	}
}

func TestGitHubPRBackfillRemoteTargetOmitsURLCredentials(t *testing.T) {
	configureIsolatedRuntimeEnv(t)
	t.Setenv("GC_CITY", "")
	t.Setenv("GC_NO_API", "")
	client := &fakeGitHubPRActionClient{queue: api.PRActionQueue{
		Availability: api.PRActionAvailabilityReady,
		PolicyState:  api.PRActionSourceReady,
		Sources:      []api.PRActionSource{},
		Items:        []api.PRActionQueueItem{},
	}}
	var resolved *remoteTarget
	previous := githubPRActionRemoteClientBuilder
	t.Cleanup(func() { githubPRActionRemoteClientBuilder = previous })
	githubPRActionRemoteClientBuilder = func(target *remoteTarget) (githubPRActionAPI, error) {
		resolved = target
		return client, nil
	}
	remoteURL := "https://private-url-user:private-url-password@city.example.test?token=private-query-token#private-fragment"
	var out, errOut bytes.Buffer
	if code := run([]string{"--city-url", remoteURL, "--city-name", "remote-city", "github", "pr", "backfill", "--json"}, &out, &errOut); code != 0 {
		t.Fatalf("remote backfill failed with exit %d: %s", code, &errOut)
	}
	for _, secret := range []string{"private-url-user", "private-url-password", "private-query-token", "private-fragment"} {
		if strings.Contains(out.String()+errOut.String(), secret) {
			t.Fatalf("private URL component %q leaked in command output", secret)
		}
	}
	var result githubPRBackfillResult
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if resolved == nil || resolved.BaseURL != "https://city.example.test" || result.Target != "https://city.example.test/v0/city/remote-city" {
		t.Fatalf("resolved=%+v target=%q; private URL components should be omitted", resolved, result.Target)
	}
}
