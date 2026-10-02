package prwatchdog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

const watchdogWorkflowFile = "pr-evidence-watchdog.yml"

func loadWatchdogWorkflow(t *testing.T) map[string]any {
	t.Helper()
	path := filepath.Join("..", "..", ".github", "workflows", watchdogWorkflowFile)
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(body, &doc); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return doc
}

func TestWatchdogWorkflow_TriggerSetIsExact(t *testing.T) {
	doc := loadWatchdogWorkflow(t)
	on, ok := doc["on"].(map[string]any)
	if !ok {
		t.Fatalf("workflow 'on' must be a mapping, got %T", doc["on"])
	}
	if len(on) != 1 {
		t.Fatalf("workflow must trigger on pull_request_target only, got triggers: %v", on)
	}
	prTarget, ok := on["pull_request_target"].(map[string]any)
	if !ok {
		t.Fatalf("workflow must trigger on pull_request_target, got: %v", on)
	}
	types, ok := prTarget["types"].([]any)
	if !ok {
		t.Fatalf("pull_request_target must declare explicit types, got: %v", prTarget["types"])
	}
	want := []string{"opened", "reopened", "synchronize", "ready_for_review"}
	if len(types) != len(want) {
		t.Fatalf("pull_request_target.types = %v, want exactly %v", types, want)
	}
	for i, w := range want {
		if types[i] != w {
			t.Fatalf("pull_request_target.types[%d] = %v, want %q", i, types[i], w)
		}
	}
}

func TestWatchdogWorkflow_MinimumReadOnlyPermissions(t *testing.T) {
	doc := loadWatchdogWorkflow(t)
	perms, ok := doc["permissions"].(map[string]any)
	if !ok {
		t.Fatalf("workflow must declare explicit top-level permissions, got: %v", doc["permissions"])
	}
	want := map[string]string{
		"checks":        "read",
		"pull-requests": "read",
		"contents":      "read",
	}
	if len(perms) != len(want) {
		t.Fatalf("permissions = %v, want exactly %v", perms, want)
	}
	for key, level := range want {
		got, ok := perms[key].(string)
		if !ok || got != level {
			t.Fatalf("permissions[%q] = %v, want %q", key, perms[key], level)
		}
	}
}

func TestWatchdogWorkflow_NoWriteOrSecretsScopes(t *testing.T) {
	doc := loadWatchdogWorkflow(t)
	body, err := yaml.Marshal(doc)
	if err != nil {
		t.Fatalf("re-marshal workflow: %v", err)
	}
	text := string(body)
	if strings.Contains(text, "secrets") {
		t.Fatalf("workflow must not reference secrets:\n%s", text)
	}
	if strings.Contains(text, ": write") {
		t.Fatalf("workflow must not grant any write permission:\n%s", text)
	}
}

func TestWatchdogWorkflow_RequiredCheckJobExists(t *testing.T) {
	doc := loadWatchdogWorkflow(t)
	jobs, ok := doc["jobs"].(map[string]any)
	if !ok {
		t.Fatalf("workflow jobs must be a mapping")
	}
	var found map[string]any
	for _, raw := range jobs {
		job, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if job["name"] == RequiredCheckName {
			found = job
			break
		}
	}
	if found == nil {
		t.Fatalf("no job has name %q (the required check name)", RequiredCheckName)
	}

	timeout, ok := found["timeout-minutes"].(int)
	if !ok || timeout < int(ObservationDeadline.Minutes())+10 || timeout > 140 {
		t.Fatalf("job %q timeout-minutes = %v, want at least 10 minutes beyond the %dm observation deadline and <= 140", RequiredCheckName, found["timeout-minutes"], int(ObservationDeadline.Minutes()))
	}
}

func TestEvidenceDeadlineExceedsLongestRequiredSuiteBudget(t *testing.T) {
	const longestRequiredSuite = 90 * time.Minute
	if ObservationDeadline <= longestRequiredSuite {
		t.Fatalf("observation deadline = %s, want longer than required-suite budget %s", ObservationDeadline, longestRequiredSuite)
	}
}

func TestCIWorkflowRestSmokeShardsHaveSetupAndExecutionBudget(t *testing.T) {
	path := filepath.Join("..", "..", ".github", "workflows", "ci.yml")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(body, &doc); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	jobs, ok := doc["jobs"].(map[string]any)
	if !ok {
		t.Fatalf("workflow jobs must be a mapping")
	}
	integration, ok := jobs["integration-shards"].(map[string]any)
	if !ok {
		t.Fatalf("integration job must exist")
	}
	strategy, ok := integration["strategy"].(map[string]any)
	if !ok {
		t.Fatalf("integration strategy must exist")
	}
	matrix, ok := strategy["matrix"].(map[string]any)
	if !ok {
		t.Fatalf("integration matrix must exist")
	}
	include, ok := matrix["include"].([]any)
	if !ok {
		t.Fatalf("integration matrix include must be a list")
	}
	found := map[string]bool{}
	for _, raw := range include {
		shard, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		name, _ := shard["shard_name"].(string)
		if name != "rest-smoke-1-of-2" && name != "rest-smoke-2-of-2" {
			continue
		}
		found[name] = true
		timeout, ok := shard["timeout_minutes"].(int)
		if !ok || timeout < 30 {
			t.Fatalf("%s timeout_minutes = %v, want >= 30 to cover setup plus execution", name, shard["timeout_minutes"])
		}
	}
	for _, name := range []string{"rest-smoke-1-of-2", "rest-smoke-2-of-2"} {
		if !found[name] {
			t.Fatalf("integration matrix is missing %s", name)
		}
	}
}

func TestWatchdogWorkflow_ConcurrencyKeyedByPRCancelsObsoleteHead(t *testing.T) {
	doc := loadWatchdogWorkflow(t)
	concurrency, ok := doc["concurrency"].(map[string]any)
	if !ok {
		t.Fatalf("workflow must declare top-level concurrency, got: %v", doc["concurrency"])
	}
	group, ok := concurrency["group"].(string)
	if !ok || !strings.Contains(group, "pull_request.number") {
		t.Fatalf("concurrency.group = %v, want it to key on the PR number", concurrency["group"])
	}
	if strings.Contains(group, "head") {
		t.Fatalf("concurrency.group = %q must not key on head SHA, or a new push would never cancel the obsolete-head observation", group)
	}
	cancel, ok := concurrency["cancel-in-progress"].(bool)
	if !ok || !cancel {
		t.Fatalf("concurrency.cancel-in-progress = %v, want true", concurrency["cancel-in-progress"])
	}
}

func TestWatchdogWorkflow_CheckoutIsTrustedBaseOnlyOrAbsent(t *testing.T) {
	doc := loadWatchdogWorkflow(t)
	jobs, _ := doc["jobs"].(map[string]any)
	for jobName, raw := range jobs {
		job, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		steps, ok := job["steps"].([]any)
		if !ok {
			continue
		}
		for _, rawStep := range steps {
			step, ok := rawStep.(map[string]any)
			if !ok {
				continue
			}
			uses, _ := step["uses"].(string)
			if !strings.HasPrefix(uses, "actions/checkout@") {
				continue
			}
			with, _ := step["with"].(map[string]any)
			ref, _ := with["ref"].(string)
			if ref != "${{ github.event.pull_request.base.sha }}" {
				t.Fatalf("job %q checkout must pin ref to the PR base SHA only, got ref=%q", jobName, ref)
			}
			persist, hasPersist := with["persist-credentials"].(bool)
			if !hasPersist || persist {
				t.Fatalf("job %q checkout must set persist-credentials: false, got %v", jobName, with["persist-credentials"])
			}
		}
	}
}

func TestWatchdogWorkflow_InvokesTheWatchdogProgram(t *testing.T) {
	doc := loadWatchdogWorkflow(t)
	body, err := yaml.Marshal(doc)
	if err != nil {
		t.Fatalf("re-marshal workflow: %v", err)
	}
	text := string(body)
	if !strings.Contains(text, "scripts/prwatchdog/cmd/watchdog") {
		t.Fatalf("expected the workflow to invoke the watchdog program at scripts/prwatchdog/cmd/watchdog, got:\n%s", text)
	}
}

func TestWatchdogWorkflow_UsesExplicitPRHeadSHANotBareGithubSHA(t *testing.T) {
	doc := loadWatchdogWorkflow(t)
	body, err := yaml.Marshal(doc)
	if err != nil {
		t.Fatalf("re-marshal workflow: %v", err)
	}
	text := string(body)
	if !strings.Contains(text, "github.event.pull_request.head.sha") {
		t.Fatalf("workflow must read the PR head SHA explicitly from github.event.pull_request.head.sha for API lookups, got:\n%s", text)
	}
	// Under pull_request_target, github.sha resolves to the BASE branch
	// commit, not the PR head -- using it here would silently evaluate the
	// wrong commit's evidence. This is precisely the class of bug this
	// watchdog exists to catch (observed on fork PR #4967).
	if strings.Contains(text, "${{ github.sha }}") {
		t.Fatalf("workflow must not use the bare github.sha context (resolves to the base ref under pull_request_target, not the PR head):\n%s", text)
	}
}
