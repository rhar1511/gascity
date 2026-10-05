package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/herdr"
	"github.com/gastownhall/gascity/internal/runtime/herdr/herdrtest"
)

// TestSessionEventPumpLiveHerdr proves the event→poke chain against a real
// herdr binary: the pump subscribes through the provider's stream, and an
// agent's natural process exit lands a reconcile poke without any polling.
// Opt-in: see herdrtest.RequireLive.
func TestSessionEventPumpLiveHerdr(t *testing.T) {
	herdrtest.RequireLive(t)

	// Unique per run: herdr persists session state across server restarts, so a
	// fixed name inherits a prior run's leftovers.
	session := fmt.Sprintf("gctest-pump-live-%d", time.Now().UnixNano())
	p := herdr.New(session, t.TempDir(), t.TempDir(), 0, 0)
	_ = p.TeardownServer() // clear any leftover server from a crashed prior run
	t.Cleanup(func() { _ = p.TeardownServer() })
	if err := p.ConfigureServer(); err != nil {
		t.Fatalf("ConfigureServer: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// The agent blocks on a flag file rather than a fixed sleep, so the exit
	// happens when this test asks for it. A timed exit races the quiet-drain
	// below, which would consume the very poke the assertion then waits for,
	// and the failure reads as "the pump never poked".
	const agentName = "evt-pump-live"
	work := t.TempDir()
	exitFlag := filepath.Join(work, "exit-now")
	cfg := runtime.Config{
		WorkDir: work,
		Command: fmt.Sprintf("/bin/sh -c 'while [ ! -e %s ]; do sleep 0.2; done'", exitFlag),
	}
	if err := p.Start(ctx, agentName, cfg); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = p.Stop(agentName) })

	// Raw-command panes are absent from herdr's agent registry. The event
	// stream must attribute this exit through Gas City's persisted pane binding.

	pokeCh := make(chan struct{}, 1)
	pump := newSessionEventPump(ctx, pokeCh, &bytes.Buffer{}, "live")
	// Park resync pokes outside the test window so the only poke observed
	// below is the attributed process-exit one.
	pump.resyncDelay = time.Minute
	pump.restart(p)
	if !pump.streaming() {
		t.Fatal("pump not streaming against live herdr")
	}

	// Drain startup noise (leading resync, resubscribe cycles for the new
	// agent pane) until the pokes go quiet, then the process exit must poke.
	quietUntil := time.Now().Add(time.Second)
	for time.Now().Before(quietUntil) {
		select {
		case <-pokeCh:
			quietUntil = time.Now().Add(time.Second)
		case <-time.After(100 * time.Millisecond):
		}
	}

	if err := os.WriteFile(exitFlag, nil, 0o600); err != nil {
		t.Fatalf("signaling the agent to exit: %v", err)
	}
	start := time.Now()
	select {
	case <-pokeCh:
		t.Logf("process exit → reconcile poke in %v", time.Since(start).Round(time.Millisecond))
	case <-time.After(15 * time.Second):
		t.Fatal("no reconcile poke after the agent process exited")
	}
}
