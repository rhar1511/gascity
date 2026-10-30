package herdr

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// Model a raw preparation process that becomes a detected agent only after
// multiple observations. The fake never contacts a real terminal server.
func newStartupWrapperReplay(t *testing.T, mode string) (*Provider, string) {
	t.Helper()
	f := newFakeHerdr(t, "unused")
	dir := filepath.Dir(f.bin)
	script := `#!/bin/sh
shift 2
printf '%s\n' "$1 $2" >> '__DIR__/calls'
case "$1 $2" in
 'agent list') printf '{"result":{"agents":[]}}';;
 'workspace list') if [ -f '__DIR__/never' ];then sleep 0.2;fi;printf '{"result":{"workspaces":[]}}';;
 'workspace create') printf '{"result":{"workspace":{"workspace_id":"wBoot"},"tab":{"tab_id":"wBoot:t1"},"root_pane":{"pane_id":"wBoot:p1"}}}';;
 'tab list') printf '{"result":{"tabs":[]}}';;
 'pane run')
  case "$4" in
   'exec /bin/sh -c '*) : > '__DIR__/booted';;
   *) : > '__DIR__/premature-fallback';;
  esac
  printf '{"result":{}}';;
 'pane process-info')
  if [ -f '__DIR__/booted' ];then printf '{"result":{"process_info":{"shell_pid":4242,"foreground_processes":[{"pid":4242,"name":"python3"}]}}}'
  else printf '{"result":{"process_info":{"shell_pid":4242,"foreground_processes":[]}}}';fi;;
 'agent get'|'agent wait'|'agent prompt')
  count=0
  if [ -f '__DIR__/booted' ];then
   if [ -f '__DIR__/polls' ];then read -r count < '__DIR__/polls';fi
   count=$((count+1));printf '%s\n' "$count" > '__DIR__/polls'
  fi
  threshold=6
  if [ -f '__DIR__/never' ];then threshold=1000000;if [ -f '__DIR__/booted' ] && [ "$2" = 'get' ];then : > '__DIR__/detection-probed';fi;fi
  if [ -f '__DIR__/blocked' ] || [ -f '__DIR__/disappeared' ];then threshold=0;fi
  if [ ! -f '__DIR__/booted' ] || [ "$count" -lt "$threshold" ];then printf '{"error":{"code":"agent_not_found","message":"wrapper still preparing; no detected TUI"}}'
  elif [ "$2" = 'get' ];then
   status=idle;if [ -f '__DIR__/blocked' ];then status=blocked;fi
   printf '{"result":{"agent":{"agent":"codex","pane_id":"wBoot:p1","agent_status":"%s"}}}' "$status"
  elif [ "$2" = 'prompt' ];then
   if [ -f '__DIR__/disappeared' ];then printf '{"error":{"code":"agent_not_found","message":"detected agent exited"}}'
   elif [ -f '__DIR__/blocked' ];then printf '{"error":{"code":"agent_prompt_stalled","message":"dialog is blocked"}}'
   else : > '__DIR__/delivered';printf '{"result":{}}';fi
  elif [ -f '__DIR__/blocked' ];then printf '{"error":{"code":"timeout","message":"dialog is blocked"}}'
  else printf '{"result":{}}';fi;;
 *) printf '{"result":{}}';;
 esac
`
	script = strings.ReplaceAll(script, "__DIR__", dir)
	if err := os.WriteFile(f.bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if mode != "delayed" {
		if err := os.WriteFile(filepath.Join(dir, mode), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return newFakeStartProvider(t, f), dir
}

func TestStartupRawWrapperWaitsForDetectedAgent(t *testing.T) {
	p, dir := newStartupWrapperReplay(t, "delayed")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cfg := runtime.Config{Command: "/fixture/protected-worker-wrapper", WorkDir: t.TempDir(), ProcessNames: []string{"codex"}, Nudge: "Write the synthetic canary report."}
	if err := p.start(ctx, "diag-worker", cfg); err != nil {
		t.Fatalf("startup: %v", err)
	}
	calls, _ := os.ReadFile(filepath.Join(dir, "calls"))
	if _, err := os.Stat(filepath.Join(dir, "delivered")); err != nil {
		marker, _ := p.GetMeta("diag-worker", metaStartupUnconfirmed)
		t.Fatalf("first turn never reached detected agent; startup returned success; unconfirmed=%q calls=\n%s", marker, calls)
	}
	if _, err := os.Stat(filepath.Join(dir, "premature-fallback")); err == nil {
		t.Fatal("first turn pasted into preparation wrapper before an agent appeared")
	}
}

func TestStartupRawWrapperNeverDetectedWithholdsInput(t *testing.T) {
	p, dir := newStartupWrapperReplay(t, "never")
	// Cancel only once the wrapper has launched and detection was attempted.
	// A wall-clock deadline starting before placement can expire before the
	// condition this test is meant to exercise, especially on a loaded host.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		tick := time.NewTicker(5 * time.Millisecond)
		defer tick.Stop()
		for {
			if _, err := os.Stat(filepath.Join(dir, "detection-probed")); err == nil {
				cancel()
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
		}
	}()
	t.Cleanup(func() { cancel(); <-watchDone })
	cfg := runtime.Config{Command: "/fixture/protected-worker-wrapper", WorkDir: t.TempDir(), ProcessNames: []string{"codex"}, Nudge: "Synthetic first turn."}
	if err := p.start(ctx, "diag-worker", cfg); err != nil {
		t.Fatalf("startup ended before wrapped-agent detection: %v", err)
	}
	if ctx.Err() != context.Canceled {
		t.Fatalf("detection probe did not trigger cancellation: %v", ctx.Err())
	}
	if _, err := os.Stat(filepath.Join(dir, "detection-probed")); err != nil {
		t.Fatalf("wrapper never reached detection probe: %v", err)
	}
	calls, _ := os.ReadFile(filepath.Join(dir, "calls"))
	if strings.Contains(string(calls), "pane send-keys") {
		t.Fatalf("keystroke sent before an agent appeared: %s", calls)
	}
	if _, err := os.Stat(filepath.Join(dir, "premature-fallback")); err == nil {
		t.Fatal("first turn pasted into a wrapper that never became an agent")
	}
	if marker, _ := p.GetMeta("diag-worker", metaStartupUnconfirmed); marker == "" {
		t.Fatal("unconfirmed startup has no durable marker")
	}
}

func TestStartupRawWrapperBlockedAgentWithholdsEnter(t *testing.T) {
	p, dir := newStartupWrapperReplay(t, "blocked")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cfg := runtime.Config{Command: "/fixture/protected-worker-wrapper", WorkDir: t.TempDir(), ProcessNames: []string{"codex"}, Nudge: "Synthetic first turn."}
	if err := p.start(ctx, "diag-worker", cfg); err != nil {
		t.Fatalf("startup: %v", err)
	}
	calls, _ := os.ReadFile(filepath.Join(dir, "calls"))
	if strings.Contains(string(calls), "pane send-keys") {
		t.Fatalf("blocked dialog received an Enter: %s", calls)
	}
	if _, err := os.Stat(filepath.Join(dir, "delivered")); err == nil {
		t.Fatal("blocked dialog received a first turn")
	}
	if marker, _ := p.GetMeta("diag-worker", metaStartupUnconfirmed); !strings.Contains(marker, "blocked") {
		t.Fatalf("missing blocked-startup marker: %q", marker)
	}
}

func TestStartupRawShellRetainsPasteFallback(t *testing.T) {
	p, dir := newStartupWrapperReplay(t, "never")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cfg := runtime.Config{Command: "/fixture/plain-shell", WorkDir: t.TempDir(), ProcessNames: []string{"bash"}, Nudge: "echo shell-ready"}
	if err := p.start(ctx, "diag-worker", cfg); err != nil {
		t.Fatalf("startup: %v", err)
	}
	calls, _ := os.ReadFile(filepath.Join(dir, "calls"))
	if _, err := os.Stat(filepath.Join(dir, "premature-fallback")); err != nil {
		t.Fatal("real shell lost its paste fallback")
	}
	if !strings.Contains(string(calls), "pane send-keys") {
		t.Fatalf("real shell input was not submitted: %s", calls)
	}
}

func TestStartupRawWrapperDisappearsBeforePromptWithholdsInput(t *testing.T) {
	p, dir := newStartupWrapperReplay(t, "disappeared")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cfg := runtime.Config{Command: "/fixture/protected-worker-wrapper", WorkDir: t.TempDir(), ProcessNames: []string{"codex"}, Nudge: "Synthetic first turn."}
	if err := p.start(ctx, "diag-worker", cfg); err != nil {
		t.Fatalf("startup: %v", err)
	}
	calls, _ := os.ReadFile(filepath.Join(dir, "calls"))
	if strings.Contains(string(calls), "pane send-keys") {
		t.Fatalf("keystroke sent after detected child exited: %s", calls)
	}
	if _, err := os.Stat(filepath.Join(dir, "premature-fallback")); err == nil {
		t.Fatal("first turn pasted into wrapper after detected child exited")
	}
	if marker, _ := p.GetMeta("diag-worker", metaStartupUnconfirmed); marker == "" {
		t.Fatal("missing unconfirmed marker after detected child exited")
	}
}
