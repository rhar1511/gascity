package routing

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

type stopSpy struct {
	*runtime.Fake
	stops []string
	err   error
}

func TestUnboundSessionRefusesWithoutDefaultProbe(t *testing.T) {
	local := &stopSpy{Fake: runtime.NewFake()}
	p, err := New("local", map[string]runtime.Provider{"local": local})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Stop("unknown"); err == nil {
		t.Fatal("unbound Stop succeeded")
	}
	if err := p.Start(context.Background(), "unknown", runtime.Config{}); err == nil {
		t.Fatal("unbound Start succeeded")
	}
	if _, err := p.ObserveLivenessWithError("unknown", nil); !errors.Is(err, runtime.ErrRuntimeUnavailable) {
		t.Fatalf("unbound observation = %v", err)
	}
	if len(local.stops) != 0 {
		t.Fatalf("default backend contacted: %v", local.stops)
	}
}

func TestBindingCannotMigrateAfterSuccessfulStop(t *testing.T) {
	local := &stopSpy{Fake: runtime.NewFake()}
	remote := &stopSpy{Fake: runtime.NewFake()}
	registry := map[string]runtime.Provider{"local": local, "remote": remote}
	p, err := New("local", registry)
	if err != nil {
		t.Fatal(err)
	}
	registry["remote"] = local
	if err := p.Bind("session", "missing"); err == nil {
		t.Fatal("unknown backend accepted")
	}
	if err := p.Bind("session", "remote"); err != nil {
		t.Fatal(err)
	}
	if err := p.Bind("session", "remote"); err != nil {
		t.Fatal(err)
	}
	if err := p.Stop("session"); err != nil {
		t.Fatal(err)
	}
	if err := p.Bind("session", "local"); err == nil {
		t.Fatal("binding migrated after Stop")
	}
	if err := p.Stop("session"); err != nil {
		t.Fatal(err)
	}
	if len(local.stops) != 0 || len(remote.stops) != 2 {
		t.Fatalf("stop calls local=%v remote=%v", local.stops, remote.stops)
	}
}

func TestConcurrentBindingHasOneOwner(t *testing.T) {
	p, err := New("local", map[string]runtime.Provider{"local": runtime.NewFake(), "remote": runtime.NewFake()})
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	var workers sync.WaitGroup
	for _, key := range []string{"local", "remote"} {
		workers.Add(1)
		go func() { defer workers.Done(); <-start; results <- p.Bind("session", key) }()
	}
	close(start)
	workers.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("successful conflicting bindings=%d", successes)
	}
}

func TestStartPreservesTypedErrorsAndHostConfiguration(t *testing.T) {
	for _, failure := range []error{&runtime.CapacityError{ExitCode: 75, Source: runtime.CapacitySourceExitStatus, Err: runtime.ErrSessionDiedDuringStartup}, runtime.ErrSessionExists, context.Canceled} {
		t.Run(failure.Error(), func(t *testing.T) {
			local, remote := runtime.NewFake(), runtime.NewFake()
			remote.StartErrors["session"] = failure
			p, err := New("local", map[string]runtime.Provider{"local": local, "remote": remote})
			if err != nil {
				t.Fatal(err)
			}
			if err := p.Bind("session", "remote"); err != nil {
				t.Fatal(err)
			}
			cfg := runtime.Config{WorkDir: "/host/verified-worktree", Env: map[string]string{"GC_SESSION_ID": "session", "HOST_SENTINEL": "private"}, Command: "runner --resume exact-key"}
			if err := p.Start(context.Background(), "session", cfg); !errors.Is(err, failure) {
				t.Fatalf("Start=%v, want %v", err, failure)
			}
			calls := remote.SnapshotCalls()
			if len(calls) != 1 || !reflect.DeepEqual(calls[0].Config, cfg) || len(local.SnapshotCalls()) != 0 {
				t.Fatalf("start configuration/calls changed: local=%v remote=%v", local.SnapshotCalls(), calls)
			}
		})
	}
}

func TestEveryCoreOperationStaysOnItsBoundBackend(t *testing.T) {
	tests := []struct {
		name   string
		invoke func(runtime.Provider)
	}{
		{"interrupt", func(p runtime.Provider) { _ = p.Interrupt("session") }},
		{"running", func(p runtime.Provider) { _ = p.IsRunning("session") }},
		{"attached", func(p runtime.Provider) { _ = p.IsAttached("session") }},
		{"attach", func(p runtime.Provider) { _ = p.Attach("session") }},
		{"process", func(p runtime.Provider) { _ = p.ProcessAlive("session", nil) }},
		{"nudge", func(p runtime.Provider) { _ = p.Nudge("session", nil) }},
		{"set-meta", func(p runtime.Provider) { _ = p.SetMeta("session", "key", "value") }},
		{"get-meta", func(p runtime.Provider) { _, _ = p.GetMeta("session", "key") }},
		{"remove-meta", func(p runtime.Provider) { _ = p.RemoveMeta("session", "key") }},
		{"peek", func(p runtime.Provider) { _, _ = p.Peek("session", 3) }},
		{"activity", func(p runtime.Provider) { _, _ = p.GetLastActivity("session") }},
		{"scrollback", func(p runtime.Provider) { _ = p.ClearScrollback("session") }},
		{"copy", func(p runtime.Provider) { _ = p.CopyTo("session", "/host/input", "input") }},
		{"keys", func(p runtime.Provider) { _ = p.SendKeys("session", "Enter") }},
		{"live", func(p runtime.Provider) { _ = p.RunLive("session", runtime.Config{}) }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			local, remote := runtime.NewFake(), runtime.NewFake()
			if err := local.Start(context.Background(), "session", runtime.Config{}); err != nil {
				t.Fatal(err)
			}
			if err := remote.Start(context.Background(), "session", runtime.Config{}); err != nil {
				t.Fatal(err)
			}
			p, err := New("local", map[string]runtime.Provider{"local": local, "remote": remote})
			if err != nil {
				t.Fatal(err)
			}
			if err := p.Bind("session", "remote"); err != nil {
				t.Fatal(err)
			}
			test.invoke(p)
			if len(local.SnapshotCalls()) != 1 || len(remote.SnapshotCalls()) <= 1 {
				t.Fatalf("cross-backend/missing operation local=%v remote=%v", local.SnapshotCalls(), remote.SnapshotCalls())
			}
		})
	}
}

func TestDiscoveryRetainsAttributionWithoutAssigningOwnership(t *testing.T) {
	local, remote := runtime.NewFake(), runtime.NewFake()
	ctx := context.Background()
	if err := local.Start(ctx, "duplicate", runtime.Config{}); err != nil {
		t.Fatal(err)
	}
	if err := remote.Start(ctx, "duplicate", runtime.Config{}); err != nil {
		t.Fatal(err)
	}
	p, err := New("local", map[string]runtime.Provider{"local": local, "remote": remote})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Bind("duplicate", "remote"); err != nil {
		t.Fatal(err)
	}
	listings := p.ListRunningByBackend("")
	if len(listings) != 2 || listings[0].Label != "local" || listings[1].Label != "remote" || listings[0].Err == nil || listings[1].Err == nil {
		t.Fatalf("ambiguous discovery = %#v", listings)
	}
	if _, err := p.ListRunning(""); err == nil {
		t.Fatal("ambiguous union reported complete")
	}
	if err := p.Bind("duplicate", "local"); err == nil {
		t.Fatal("discovery changed ownership")
	}
}

func (s *stopSpy) Stop(name string) error {
	s.stops = append(s.stops, name)
	return s.err
}

func TestStopNeverFallsBackToAnotherBackend(t *testing.T) {
	failure := errors.New("selected runtime unavailable")
	local := &stopSpy{Fake: runtime.NewFake()}
	remote := &stopSpy{Fake: runtime.NewFake(), err: failure}
	p, err := New("local", map[string]runtime.Provider{"local": local, "remote": remote})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Bind("session", "remote"); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := p.Stop("session"); !errors.Is(err, failure) {
			t.Fatalf("Stop = %v, want selected backend failure", err)
		}
	}
	if len(local.stops) != 0 || len(remote.stops) != 2 {
		t.Fatalf("stop calls local=%v remote=%v", local.stops, remote.stops)
	}
}

type coreOnly struct{ runtime.Provider }

type eventSource struct {
	runtime.Provider
	events chan runtime.SessionEvent
}

func (s *eventSource) SubscribeSessionEvents(context.Context) (<-chan runtime.SessionEvent, error) {
	return s.events, nil
}

func TestEventsCoverAllThreeBackends(t *testing.T) {
	backends := make(map[string]runtime.Provider)
	sources := make(map[string]*eventSource)
	for _, key := range []string{"local", "acp", "exec"} {
		s := &eventSource{Provider: runtime.NewFake(), events: make(chan runtime.SessionEvent, 1)}
		sources[key] = s
		backends[key] = s
	}
	p, err := New("local", backends)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, err := p.SubscribeSessionEvents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for key, source := range sources {
		source.events <- runtime.SessionEvent{Kind: runtime.SessionEventResync, Ref: key}
	}
	seen := map[string]bool{}
	timeout := time.After(time.Second)
	for len(seen) < 3 {
		select {
		case event, ok := <-stream:
			if !ok {
				t.Fatal("event stream closed early")
			}
			seen[event.Ref] = true
		case <-timeout:
			t.Fatalf("missing backend events: %v", seen)
		}
	}
	for key := range sources {
		if !seen[key] {
			t.Fatalf("missing backend %q", key)
		}
	}
}

type observationSpy struct {
	*runtime.Fake
	snapshots, environments, corpses int
}

func (s *observationSpy) SnapshotIdle(string) (bool, error) { s.snapshots++; return true, nil }
func (s *observationSpy) GetAllEnvironment(string) (map[string]string, error) {
	s.environments++
	return map[string]string{"BOUND": "selected"}, nil
}
func (s *observationSpy) IsDeadRuntimeSession(string) (bool, error) { s.corpses++; return true, nil }

func TestOptionalObservationsCannotReachASibling(t *testing.T) {
	local := &observationSpy{Fake: runtime.NewFake()}
	remote := &observationSpy{Fake: runtime.NewFake()}
	local.SleepCapabilityValue = runtime.SessionSleepCapabilityDisabled
	p, err := New("local", map[string]runtime.Provider{"local": local, "remote": remote})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Bind("session", "remote"); err != nil {
		t.Fatal(err)
	}
	idle, err := p.SnapshotIdle("session")
	if err != nil || !idle {
		t.Fatalf("idle=%v,%v", idle, err)
	}
	env, err := p.GetAllEnvironment("session")
	if err != nil || env["BOUND"] != "selected" {
		t.Fatalf("environment=%v,%v", env, err)
	}
	dead, err := p.IsDeadRuntimeSession("session")
	if err != nil || !dead {
		t.Fatalf("dead=%v,%v", dead, err)
	}
	if p.SleepCapability("session") != runtime.SessionSleepCapabilityFull || p.SleepCapability("unknown") != runtime.SessionSleepCapabilityDisabled {
		t.Fatal("sleep capability did not preserve ownership")
	}
	if local.snapshots+local.environments+local.corpses != 0 || remote.snapshots != 1 || remote.environments != 1 || remote.corpses != 1 {
		t.Fatalf("observations local=%#v remote=%#v", local, remote)
	}
}

func TestOptionalOperationsStayOnTheirOwner(t *testing.T) {
	tests := []struct {
		name   string
		invoke func(runtime.Provider) error
	}{
		{"idle", func(p runtime.Provider) error {
			q, ok := p.(runtime.IdleWaitProvider)
			if !ok {
				return errors.New("idle seam hidden")
			}
			return q.WaitForIdle(context.Background(), "session", time.Second)
		}},
		{"relaunch", func(p runtime.Provider) error {
			q, ok := p.(runtime.RelaunchProvider)
			if !ok {
				return errors.New("relaunch seam hidden")
			}
			return q.Relaunch(context.Background(), "session", runtime.Config{})
		}},
		{"dialog", func(p runtime.Provider) error {
			q, ok := p.(runtime.DialogProvider)
			if !ok {
				return errors.New("dialog seam hidden")
			}
			return q.DismissKnownDialogs(context.Background(), "session", time.Second)
		}},
		{"reset", func(p runtime.Provider) error {
			q, ok := p.(runtime.InterruptedTurnResetProvider)
			if !ok {
				return errors.New("reset seam hidden")
			}
			return q.ResetInterruptedTurn(context.Background(), "session")
		}},
		{"interrupt-boundary", func(p runtime.Provider) error {
			q, ok := p.(runtime.InterruptBoundaryWaitProvider)
			if !ok {
				return errors.New("boundary seam hidden")
			}
			return q.WaitForInterruptBoundary(context.Background(), "session", time.Time{}, time.Second)
		}},
		{"immediate-nudge", func(p runtime.Provider) error {
			q, ok := p.(runtime.ImmediateNudgeProvider)
			if !ok {
				return errors.New("nudge seam hidden")
			}
			return q.NudgeNow("session", nil)
		}},
		{"pending", func(p runtime.Provider) error {
			q, ok := p.(runtime.InteractionProvider)
			if !ok {
				return errors.New("interaction seam hidden")
			}
			_, err := q.Pending("session")
			return err
		}},
		{"respond", func(p runtime.Provider) error {
			q, ok := p.(runtime.InteractionProvider)
			if !ok {
				return errors.New("interaction seam hidden")
			}
			return q.Respond("session", runtime.InteractionResponse{})
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			local, remote := runtime.NewFake(), runtime.NewFake()
			remote.WaitForIdleErrors["session"] = nil
			remote.PendingInteractions["session"] = &runtime.PendingInteraction{RequestID: "request"}
			p, err := New("local", map[string]runtime.Provider{"local": local, "remote": remote})
			if err != nil {
				t.Fatal(err)
			}
			if err := p.Bind("session", "remote"); err != nil {
				t.Fatal(err)
			}
			if err := remote.Start(context.Background(), "session", runtime.Config{}); err != nil {
				t.Fatal(err)
			}
			before := len(remote.SnapshotCalls())
			if err := test.invoke(p); err != nil {
				t.Fatal(err)
			}
			if len(local.SnapshotCalls()) != 0 || len(remote.SnapshotCalls()) <= before {
				t.Fatalf("owner-only dispatch failed: local=%v remote=%v", local.SnapshotCalls(), remote.SnapshotCalls())
			}
		})
	}
}

func TestExecPreservesSelectedResultAndNeverFallsBack(t *testing.T) {
	local, remote := runtime.NewFake(), runtime.NewFake()
	failure := errors.New("remote execution refused")
	remote.ExecResults = map[string]runtime.FakeExecResult{"session": {Output: "remote result", Code: 13, Err: failure}}
	p, err := New("local", map[string]runtime.Provider{"local": local, "remote": remote, "plain": coreOnly{remote}})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Bind("session", "remote"); err != nil {
		t.Fatal(err)
	}
	out, code, err := p.Exec(context.Background(), "session", []string{"tool", "literal argument"})
	if string(out) != "remote result" || code != 13 || !errors.Is(err, failure) {
		t.Fatalf("Exec = %q, %d, %v", out, code, err)
	}
	if len(local.SnapshotCalls()) != 0 {
		t.Fatalf("sibling contacted: %v", local.SnapshotCalls())
	}
	if err := p.Bind("plain-session", "plain"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := p.Exec(context.Background(), "plain-session", nil); !errors.Is(err, runtime.ErrExecUnsupported) {
		t.Fatalf("unsupported Exec = %v", err)
	}
	if local.CountCalls("Exec", "plain-session") != 0 || remote.CountCalls("Exec", "plain-session") != 0 {
		t.Fatal("unsupported Exec fell back")
	}
}
