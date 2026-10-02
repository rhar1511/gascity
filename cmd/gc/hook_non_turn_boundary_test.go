package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
)

// installNonTurnDemandProbe puts a fake `bd` on PATH that serves ONE routed,
// unassigned demand row to the routed-pool tier and records every invocation.
//
// It answers `[]` to every other tier so the generated query reaches the routed
// tier without dragging the blocked_by enrichment (which shells `bd show` and
// jq) into the fixture. The recorded argv is the assertion surface: a claim is a
// `bd update <id> --claim`, so its presence or absence in the log is direct
// evidence of whether a mutation ran, which an exit code alone cannot give.
func installNonTurnDemandProbe(t *testing.T, cityDir, sessionID string) (argvLog string) {
	t.Helper()
	store, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatal(err)
	}
	seeded, err := store.Get(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(struct {
		beads.Bead
		Revision int64 `json:"revision"`
	}{Bead: seeded, Revision: seeded.Revision})
	if err != nil {
		t.Fatal(err)
	}
	fakeBin := t.TempDir()
	stateDir := t.TempDir()
	argvLog = filepath.Join(stateDir, "bd-argv.log")
	if err := os.WriteFile(filepath.Join(stateDir, "session.json"), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stateDir, "owner"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
printf '%s\n' "$*" >> "$BD_ARGV_LOG"
owner=$(cat "$BD_PROBE_STATE/owner")
status=open; revision=1
[ -n "$owner" ] && status=in_progress && revision=2
render_demand() {
  printf '[{"id":"demand-1","status":"%s","assignee":"%s","revision":%s,"issue_type":"task","source_store_ref":"city:test-city","metadata":{"gc.routed_to":"worker"}}]' "$status" "$owner" "$revision"
}
case "$1" in
show)
  if [ "$3" = "demand-1" ]; then
    render_demand
  elif [ "$3" = "$BD_PROBE_SESSION" ]; then
    printf '['; cat "$BD_PROBE_STATE/session.json"; printf ']'
  else
    printf '[]'
  fi
  exit 0 ;;
ready)
  for argument in "$@"; do
    case "$argument" in --assignee=*) printf '[]'; exit 0 ;; esac
  done
  if [ "$status" = "open" ]; then render_demand; else printf '[]'; fi
  exit 0 ;;
update)
  if [ "$2" = "demand-1" ]; then
    for argument in "$@"; do
      if [ "$argument" = "--claim" ]; then
        if [ -n "$owner" ] && [ "$owner" != "$BEADS_ACTOR" ]; then exit 13; fi
        printf '%s' "$BEADS_ACTOR" > "$BD_PROBE_STATE/owner"
        owner="$BEADS_ACTOR"; status=in_progress; revision=2
        render_demand
        exit 0
      fi
    done
  fi
  ;;
esac
printf '[]'
`
	if err := os.WriteFile(filepath.Join(fakeBin, "bd"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("BD_ARGV_LOG", argvLog)
	t.Setenv("BD_PROBE_STATE", stateDir)
	t.Setenv("BD_PROBE_SESSION", sessionID)
	// The same fake bd serves query projections, exact authoritative reads and
	// the claim write. The seeded session remains available for identity checks.
	t.Setenv("GC_BEADS", "bd")
	return argvLog
}

// nonTurnProbeClaimed reports whether the fake bd was ever asked to run a claim
// mutation.
func nonTurnProbeClaimed(t *testing.T, argvLog string) bool {
	t.Helper()
	raw, err := os.ReadFile(argvLog)
	if err != nil {
		if os.IsNotExist(err) {
			return false
		}
		t.Fatalf("reading bd argv log: %v", err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.Contains(line, "update") && strings.Contains(line, "--claim") {
			return true
		}
	}
	return false
}

// nonTurnStepStartedCount counts execution.step_started records in the city's
// event log.
func nonTurnStepStartedCount(t *testing.T, cityDir string) int {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(cityDir, ".gc", "events.jsonl"))
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatalf("reading events.jsonl: %v", err)
	}
	count := 0
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.Contains(line, `"execution.step_started"`) {
			count++
		}
	}
	return count
}

// TestNonTurnHookInvocationCannotMintExecution is the cross-boundary shape test:
// it drives the real CLI entry point the way each provider callback lane does —
// process environment markers, not an injected ops seam — against a city that
// HAS claimable routed demand, and proves nothing was minted.
//
// The unit fences prove the predicate; this proves the wiring. A fence that
// exists in cmd_hook_claim.go but is never reached from cmdHookWithOptions would
// pass every unit test and change nothing on the fleet.
func TestNonTurnHookInvocationCannotMintExecution(t *testing.T) {
	for _, marker := range []struct{ key, value string }{
		{"GC_HOOK_CALLBACK_LANE", "1"},
		{"GC_MANAGED_SESSION_HOOK", "1"},
		{"GC_HOOK_EVENT_NAME", "SessionStart"},
		{"GC_HOOK_EVENT_NAME", "UserPromptSubmit"},
	} {
		t.Run(marker.key+"="+marker.value, func(t *testing.T) {
			clearGCEnv(t)
			disableManagedDoltRecoveryForTest(t)
			t.Setenv("GC_BEADS", "file")
			cityDir := writeFenceTestCity(t)
			sessionID := newFenceSessionBead(t, cityDir, session.StateActive, "live-token")
			argvLog := installNonTurnDemandProbe(t, cityDir, sessionID)
			setFenceClaimEnv(t, cityDir, sessionID, "live-token")
			t.Setenv(marker.key, marker.value)

			var stdout, stderr bytes.Buffer
			code := cmdHookWithOptions(nil, hookCommandOptions{Claim: true, DrainAck: true, JSON: true}, &stdout, &stderr)

			if code != 0 {
				t.Fatalf("code = %d, want 0; stdout=%q stderr=%s", code, stdout.String(), stderr.String())
			}
			if nonTurnProbeClaimed(t, argvLog) {
				t.Fatalf("a %s callback lane ran a claim mutation; bd argv log:\n%s", marker.key, readFileForTest(t, argvLog))
			}
			// The refusal must also be CHEAP. A provider callback's whole budget
			// is 15s (defaultHookRunTimeout) while the work query alone is
			// bounded at 150s (hookWorkQueryTimeout), so a fence that refuses only
			// after the query is a fence whose answer the provider never sees.
			if readFileForTest(t, argvLog) != "<absent>" {
				t.Fatalf("a %s callback lane ran the work query before refusing; bd argv log:\n%s", marker.key, readFileForTest(t, argvLog))
			}
			if got := nonTurnStepStartedCount(t, cityDir); got != 0 {
				t.Fatalf("execution.step_started count = %d, want 0 from a non-turn invocation", got)
			}
			var result hookClaimJSONResult
			if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &result); err != nil {
				t.Fatalf("stdout is not a JSON drain result: %v\n%s", err, stdout.String())
			}
			if result.Action != "drain" || result.Reason != hookClaimReasonNonTurnContext {
				t.Fatalf("result = %+v, want action=drain reason=%s", result, hookClaimReasonNonTurnContext)
			}
			if result.DrainAcknowledged {
				t.Fatal("a callback lane acknowledged the session's drain on the session's behalf")
			}
		})
	}
}

// TestTurnHookInvocationMintsTheClaim is the differently-failing control for the
// cross-boundary test: the identical city, the identical demand, invoked as a
// real turn. It must claim.
//
// Without it, deleting the whole claim path would make the test above pass.
func TestTurnHookInvocationMintsTheClaim(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)
	t.Setenv("GC_BEADS", "file")
	cityDir := writeFenceTestCity(t)
	sessionID := newFenceSessionBead(t, cityDir, session.StateActive, "live-token")
	argvLog := installNonTurnDemandProbe(t, cityDir, sessionID)
	setFenceClaimEnv(t, cityDir, sessionID, "live-token")

	var stdout, stderr bytes.Buffer
	code := cmdHookWithOptions(nil, hookCommandOptions{Claim: true, JSON: true}, &stdout, &stderr)

	if !nonTurnProbeClaimed(t, argvLog) {
		t.Fatalf("a real turn did not reach the claim mutation; stdout=%q stderr=%s\nbd argv log:\n%s",
			stdout.String(), stderr.String(), readFileForTest(t, argvLog))
	}
	var result hookClaimJSONResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("claim result: %v; stdout=%q stderr=%s", err, stdout.String(), stderr.String())
	}
	if code != 0 || result.Action != "work" || result.BeadID != "demand-1" {
		t.Fatalf("claim result = %+v, code=%d; stderr=%s", result, code, stderr.String())
	}
	if owner := strings.TrimSpace(readFileForTest(t, filepath.Join(os.Getenv("BD_PROBE_STATE"), "owner"))); owner == "" || owner != result.Assignee {
		t.Fatalf("persisted claim owner = %q, want result assignee %q", owner, result.Assignee)
	}
}

func readFileForTest(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		return "<absent>"
	}
	return string(raw)
}
