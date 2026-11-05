package main

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/routing"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/session/sessiontest"
)

// Model a provider that created an attributable runtime, while a concurrent
// authoritative writer changed its known conversation during readiness.
type supersededPreparedStartProvider struct {
	*runtime.Fake
	backing beads.Store
	id      string
	patch   map[string]string
}

func (p *supersededPreparedStartProvider) Start(ctx context.Context, name string, cfg runtime.Config) error {
	if err := p.Fake.Start(ctx, name, cfg); err != nil {
		return err
	}
	for _, key := range []string{"GC_SESSION_ID", "GC_INSTANCE_TOKEN", "GC_RUNTIME_EPOCH"} {
		if err := p.SetMeta(name, key, cfg.Env[key]); err != nil {
			return err
		}
	}
	return sessionpkg.WithSessionMutationLock(p.id, func() error {
		if err := p.backing.SetMetadata(p.id, "session_key", "replacement-native"); err != nil {
			return err
		}
		if len(p.patch) > 0 {
			return p.backing.SetMetadataBatch(p.id, p.patch)
		}
		return nil
	})
}

func TestRuntimeBackendDriftRetainsSupersededOrCommandDriftAttempt(t *testing.T) {
	for _, superseded := range []bool{false, true} {
		t.Run(map[bool]string{false: "command", true: "native-superseded"}[superseded], func(t *testing.T) {
			backing := beads.NewMemStore()
			fingerprint := strings.Repeat("a", 64)
			row, err := backing.Create(beads.Bead{Type: sessionpkg.BeadType, Labels: []string{sessionpkg.LabelSession}, Metadata: creatingMeta(map[string]string{"session_name": "worker", "template": "worker", "generation": "2", "continuation_epoch": "1", "instance_token": "original-token", "session_key": "original-native", "pending_create_claim": "true", "command": "worker", "gc.runtime_backend": "capsule", "gc.runtime_fingerprint": fingerprint})})
			if err != nil {
				t.Fatal(err)
			}
			cache := beads.NewCachingStoreForTest(backing, nil)
			if err := cache.Prime(context.Background()); err != nil {
				t.Fatal(err)
			}
			p := &supersededPreparedStartProvider{Fake: runtime.NewFake(), backing: backing, id: row.ID, patch: map[string]string{"gc.runtime_fingerprint": strings.Repeat("b", 64)}}
			sp, err := routing.New("local", map[string]runtime.Provider{"local": runtime.NewFake(), "capsule@" + fingerprint: p})
			if err != nil {
				t.Fatal(err)
			}
			if err := sp.Bind("worker", "capsule@"+fingerprint); err != nil {
				t.Fatal(err)
			}
			item := preparedStart{candidate: startCandidate{info: sessiontest.SeedBead(t, row), tp: TemplateParams{Command: "worker", SessionName: "worker", TemplateName: "worker"}}, cfg: runtime.Config{Command: "worker"}}
			var result startResult
			if superseded {
				result = runPreparedStartCandidate(context.Background(), item, t.TempDir(), sp, cache, &config.City{Agents: []config.Agent{{Name: "worker"}}}, 10*time.Second, func(context.Context, string) bool { return false }, func(context.Context, string) error { return nil }, nil)
				if !errors.Is(result.err, sessionpkg.ErrRuntimeStartSuperseded) {
					t.Fatalf("missing supersession: %v", result.err)
				}
			} else {
				if err := backing.SetMetadataBatch(row.ID, map[string]string{"gc.runtime_fingerprint": strings.Repeat("b", 64), "command": "replacement"}); err != nil {
					t.Fatal(err)
				}
				result = startResult{prepared: item, provider: sp}
			}
			current, err := backing.Get(row.ID)
			if err != nil {
				t.Fatal(err)
			}
			_, verdict := refreshAsyncStartResult(result, cache, ioDiscard{})
			if verdict.commit || verdict.cleanupRuntime || verdict.releaseInFlight || verdict.rollbackPendingCreate {
				t.Fatalf("route drift authorized effects: %+v", verdict)
			}
			if commitAsyncStartResultWithContext(context.Background(), result, sp, cache, clock.Real{}, events.Discard, 0, ioDiscard{}, ioDiscard{}, nil) {
				t.Fatal("stale attempt committed")
			}
			after, err := backing.Get(row.ID)
			if err != nil || after.Revision != current.Revision || !reflect.DeepEqual(after.Metadata, current.Metadata) || p.CountCalls("Stop", "worker") != 0 {
				t.Fatal("runtime drift changed current row or stopped original backend")
			}
		})
	}
}

func TestPreparedRuntimeOnlyStartPreservesSupersededVerdict(t *testing.T) {
	backing := beads.NewMemStore()
	row, err := backing.Create(beads.Bead{
		ID: "gc-worker", Type: sessionpkg.BeadType, Labels: []string{sessionpkg.LabelSession}, Title: "worker",
		Metadata: creatingMeta(map[string]string{
			"session_name": "worker", "template": "worker", "generation": "2",
			"continuation_epoch": "1", "instance_token": "original-token",
			"session_key": "original-native", "pending_create_claim": "true",
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	cache := beads.NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatal(err)
	}
	p := &supersededPreparedStartProvider{Fake: runtime.NewFake(), backing: backing, id: row.ID}
	item := preparedStart{
		candidate: startCandidate{info: sessiontest.SeedBead(t, row), tp: TemplateParams{
			Command: "worker", SessionName: "worker", TemplateName: "worker",
		}},
		cfg: runtime.Config{Command: "worker"},
	}
	result := runPreparedStartCandidate(context.Background(), item, t.TempDir(), p, cache,
		&config.City{Agents: []config.Agent{{Name: "worker"}}}, 10*time.Second,
		func(context.Context, string) bool { return false },
		func(context.Context, string) error { return nil }, nil)
	if !errors.Is(result.err, sessionpkg.ErrRuntimeStartSuperseded) {
		t.Fatalf("controller erased the proven stale-start verdict: error=%v outcome=%s", result.err, result.outcome)
	}
	if !runningSessionMatchesPendingCreateInfo(item.candidate.info, "worker", p) {
		t.Fatal("fixture did not reach the matching-runtime convergence condition")
	}
	_, verdict := refreshAsyncStartResult(result, cache, ioDiscard{})
	if verdict.commit || verdict.releaseInFlight || verdict.rollbackPendingCreate || !verdict.cleanupRuntime {
		t.Fatalf("stale start was allowed to mutate the current row: %+v", verdict)
	}
	current, err := backing.Get(row.ID)
	if err != nil || current.Metadata["session_key"] != "replacement-native" || current.Metadata["pending_create_claim"] != "true" {
		t.Fatal("controller changed replacement identity or pending-create ownership")
	}
	if commitAsyncStartResultWithContext(context.Background(), result, p, cache, clock.Real{}, events.Discard, 0, ioDiscard{}, ioDiscard{}, nil) {
		t.Fatal("superseded attempt committed")
	}
	if p.IsRunning("worker") {
		t.Fatal("the old attributable runtime was not cleaned up")
	}
	// A replay arriving after another execution owns the name must not stop
	// that execution, and neither discard may write the current session row.
	if err := p.Fake.Start(context.Background(), "worker", runtime.Config{}); err != nil {
		t.Fatal(err)
	}
	for key, value := range map[string]string{"GC_SESSION_ID": row.ID, "GC_INSTANCE_TOKEN": "foreign-token", "GC_RUNTIME_EPOCH": "3"} {
		if err := p.SetMeta("worker", key, value); err != nil {
			t.Fatal(err)
		}
	}
	if commitAsyncStartResultWithContext(context.Background(), result, p, cache, clock.Real{}, events.Discard, 0, ioDiscard{}, ioDiscard{}, nil) || !p.IsRunning("worker") {
		t.Fatal("stale cleanup replay committed or stopped the foreign runtime")
	}
	after, err := backing.Get(row.ID)
	if err != nil || after.Revision != current.Revision || after.Status != current.Status || !reflect.DeepEqual(after.Metadata, current.Metadata) {
		t.Fatal("stale cleanup changed replacement session metadata or failure budgets")
	}
}

// Keep the cached projection readable while the authoritative backend is unavailable.
type unavailableStartStore struct {
	*beads.MemStore
	unavailable atomic.Bool
}

func (s *unavailableStartStore) Get(id string) (beads.Bead, error) {
	if s.unavailable.Load() {
		return beads.Bead{}, errors.New("backend temporarily unavailable")
	}
	return s.MemStore.Get(id)
}

func TestPreparedRuntimeOnlyStartDefersInitialUnavailableRead(t *testing.T) {
	backing := &unavailableStartStore{MemStore: beads.NewMemStore()}
	row, err := backing.Create(beads.Bead{
		ID: "gc-unavailable-worker", Type: sessionpkg.BeadType, Labels: []string{sessionpkg.LabelSession}, Title: "worker",
		Metadata: creatingMeta(map[string]string{
			"session_name": "worker", "template": "worker", "generation": "2", "continuation_epoch": "1",
			"instance_token": "original-token", "session_key": "original-native", "pending_create_claim": "true", "wake_attempts": "2",
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	cache := beads.NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatal(err)
	}
	p := runtime.NewFake()
	item := preparedStart{candidate: startCandidate{info: sessiontest.SeedBead(t, row), tp: TemplateParams{Command: "worker", SessionName: "worker", TemplateName: "worker"}}, cfg: runtime.Config{Command: "worker"}}
	backing.unavailable.Store(true)
	result := runPreparedStartCandidate(context.Background(), item, t.TempDir(), p, cache,
		&config.City{Agents: []config.Agent{{Name: "worker"}}}, 10*time.Second,
		func(context.Context, string) bool { return false }, func(context.Context, string) error { return nil }, nil)
	if result.outcome != TraceOutcomeDeferred || result.err != nil || result.rollbackPending {
		t.Fatalf("unavailable initial read spent startup budget: outcome=%s err=%v rollback=%v", result.outcome, result.err, result.rollbackPending)
	}
	if p.CountCalls("Start", "worker") != 0 {
		t.Fatal("provider started without authoritative identity")
	}
	backing.unavailable.Store(false)
	commitAsyncStartResultWithContext(context.Background(), result, p, cache, clock.Real{}, events.Discard, 0, ioDiscard{}, ioDiscard{}, nil)
	current, err := backing.Get(row.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Status != row.Status {
		t.Fatal("unavailable read closed pending creation")
	}
	before, after := row.Metadata, current.Metadata
	// Deferral may release its own in-flight lease, but no other metadata.
	delete(before, "last_woke_at")
	delete(after, "last_woke_at")
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("unavailable read changed ownership or failure budgets: before=%v after=%v", before, after)
	}
}

// Capture the authoritative row at the provider's refusal, so a later stale
// completion cannot disguise a write to a closed execution as cleanup.
type snapshotRefusingStartProvider struct {
	runtime.Provider
	store beads.Store
	id    string
	after beads.Bead
}

func (p *snapshotRefusingStartProvider) Start(ctx context.Context, name string, cfg runtime.Config) error {
	err := p.Provider.Start(ctx, name, cfg)
	var readErr error
	p.after, readErr = p.store.Get(p.id)
	if readErr != nil {
		return readErr
	}
	return err
}

func TestSupersededSynchronousStartDoesNotWriteClosedRow(t *testing.T) {
	e := newCapacityEnv(t, true, "s")
	c := asleepSession(t, e, nil)
	p := &snapshotRefusingStartProvider{
		Provider: &closingRefuser{Fake: e.sp, store: e.store, id: c.info.ID},
		store:    e.store, id: c.info.ID,
	}
	executePlannedStartsTraced(context.Background(), []startCandidate{c}, e.cfg, e.desiredState, p, e.store, "", "", e.clk, e.rec, 0, &e.log, &e.log, e.trace, e.startOptions...)
	got := e.bead(t, c.info.ID)
	if !reflect.DeepEqual(got, p.after) {
		t.Fatalf("closed row changed after superseded completion: before=%+v after=%+v", p.after, got)
	}
}

func TestSupersededCapacityRefusalRestoresPreWakeWithoutSpendingBudget(t *testing.T) {
	for _, async := range []bool{false, true} {
		name := "sync"
		if async {
			name = "async"
		}
		t.Run(name, func(t *testing.T) {
			e := newCapacityEnv(t, true, "s")
			c := asleepSession(t, e, nil)
			before := e.bead(t, c.info.ID)
			p := &midStartWriter{Fake: e.sp, store: e.store, id: c.info.ID, patch: map[string]string{"wait_hold": "true", "sleep_intent": "wait-hold"}}
			if async {
				e.startAsyncWith(t, p, c)
			} else {
				executePlannedStartsTraced(context.Background(), []startCandidate{c}, e.cfg, e.desiredState, p, e.store, "", "", e.clk, e.rec, 0, &e.log, &e.log, e.trace, e.startOptions...)
			}
			got := e.bead(t, c.info.ID)
			if got.Metadata["state"] != before.Metadata["state"] || got.Metadata["last_woke_at"] != "" || got.Metadata["wait_hold"] != "true" || got.Metadata["sleep_intent"] != "wait-hold" {
				t.Fatalf("refused launch did not restore PreWake while preserving hold: %+v", got.Metadata)
			}
			for _, key := range []string{"wake_attempts", "startup_failure_kind"} {
				if got.Metadata[key] != before.Metadata[key] {
					t.Errorf("%s changed after capacity refusal", key)
				}
			}
		})
	}
}

func TestSupersededCapacityRefusalDoesNotRestoreChangedCustody(t *testing.T) {
	for key, value := range map[string]string{
		"session_key": "replacement-native", "work_dir": "/replacement-worktree", "alias": "replacement-owner",
		"continuation_epoch": "17", "provider": "replacement-provider", "state": "active",
	} {
		for _, async := range []bool{false, true} {
			mode := "sync"
			if async {
				mode = "async"
			}
			t.Run(key+"/"+mode, func(t *testing.T) {
				e := newCapacityEnv(t, true, "s")
				c := asleepSession(t, e, nil)
				p := &snapshotRefusingStartProvider{
					Provider: &midStartWriter{Fake: e.sp, store: e.store, id: c.info.ID, patch: map[string]string{key: value}},
					store:    e.store, id: c.info.ID,
				}
				if async {
					e.startAsyncWith(t, p, c)
				} else {
					executePlannedStartsTraced(context.Background(), []startCandidate{c}, e.cfg, e.desiredState, p, e.store, "", "", e.clk, e.rec, 0, &e.log, &e.log, e.trace, e.startOptions...)
				}
				got := e.bead(t, c.info.ID)
				if !reflect.DeepEqual(got, p.after) {
					t.Fatalf("changed execution was mutated: before=%+v after=%+v", p.after, got)
				}
			})
		}
	}
}

func TestCapacityRestoreKeepsLaunchCustodyAcrossAsyncRefresh(t *testing.T) {
	for key, value := range map[string]string{"session_key": "replacement-native", "work_dir": "/replacement-worktree", "alias": "replacement-owner", "continuation_epoch": "17"} {
		t.Run(key, func(t *testing.T) {
			e := newCapacityEnv(t, true, "s")
			c := asleepSession(t, e, nil)
			result := refusedCapacityStartForRestore(t, e, c)
			if err := e.store.SetMetadata(c.info.ID, key, value); err != nil {
				t.Fatal(err)
			}
			before := e.bead(t, c.info.ID)
			refreshed, verdict := refreshAsyncStartResult(result, e.store, &e.log)
			if !verdict.commit {
				t.Fatalf("did not exercise the capacity restore commit: %+v", verdict)
			}
			commitStartResultTraced(refreshed, sessionFrontDoor(e.store), e.clk, e.rec, 0, &e.log, &e.log, e.trace)
			got := e.bead(t, c.info.ID)
			if !reflect.DeepEqual(got, before) {
				t.Fatalf("async refresh replaced the launch custody fence: before=%+v after=%+v", before, got)
			}
		})
	}
}
