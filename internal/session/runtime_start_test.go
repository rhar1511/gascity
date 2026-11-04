package session

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	runtimepkg "github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/testutil"
)

type blockedRuntimeStart struct {
	*runtimepkg.Fake
	entered chan string
	release chan struct{}
	once    sync.Once
	calls   atomic.Int32
	result  error
}

type runtimeStartReadFaultStore struct {
	*beads.MemStore
	unavailable atomic.Bool
}

func (s *runtimeStartReadFaultStore) Get(id string) (beads.Bead, error) {
	if s.unavailable.Load() {
		return beads.Bead{}, errors.New("temporary authoritative read failure")
	}
	return s.MemStore.Get(id)
}

func newBlockedRuntimeStart(t *testing.T) *blockedRuntimeStart {
	t.Helper()
	p := &blockedRuntimeStart{Fake: runtimepkg.NewFake(), entered: make(chan string, 4), release: make(chan struct{})}
	t.Cleanup(p.allow)
	return p
}

func (p *blockedRuntimeStart) allow() { p.once.Do(func() { close(p.release) }) }

func (p *blockedRuntimeStart) Start(ctx context.Context, name string, cfg runtimepkg.Config) error {
	p.calls.Add(1)
	p.entered <- name
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-p.release:
		if p.result != nil {
			return p.result
		}
		return p.Fake.Start(ctx, name, cfg)
	}
}

func beginBlockedRuntimeStart(t *testing.T, m *Manager, id string) <-chan error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testutil.GoroutineRaceTimeout)
	result := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		result <- m.StartRuntimeOnly(ctx, id, "test-worker", runtimepkg.Config{})
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(goroutineHangBudget):
			t.Error("runtime start goroutine survived cancellation")
		}
	})
	return result
}

func awaitRuntimeStart(t *testing.T, p *blockedRuntimeStart) string {
	t.Helper()
	select {
	case name := <-p.entered:
		return name
	case <-time.After(goroutineHangBudget):
		t.Fatal("provider Start was not reached")
		return ""
	}
}

func awaitRuntimeResult(t *testing.T, result <-chan error) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(goroutineHangBudget):
		t.Fatal("runtime start did not finish")
		return nil
	}
}

func TestRuntimeOnlyStartSerializesAcrossManagers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		_, raw, expected := workClaimSessionFixture(t)
		p := newBlockedRuntimeStart(t)
		newManager := func() *Manager {
			return NewManagerWithOptions(raw, p, WithStaleKeyDetectionWaiter(immediateStaleKeyDetectionWaiter))
		}
		first := beginBlockedRuntimeStart(t, newManager(), expected.ID)
		awaitRuntimeStart(t, p)
		second := beginBlockedRuntimeStart(t, newManager(), expected.ID)
		synctest.Wait()
		if p.calls.Load() != 1 {
			t.Fatal("duplicate provider start while original start is pending")
		}
		p.allow()
		if err := awaitRuntimeResult(t, first); err != nil {
			t.Fatal(err)
		}
		if err := awaitRuntimeResult(t, second); err != nil {
			t.Fatal(err)
		}
		if p.calls.Load() != 1 {
			t.Fatal("second manager started a duplicate runtime after the first became ready")
		}
	})
}

func TestRuntimeOnlyStartsForDifferentSessionsRemainParallel(t *testing.T) {
	_, raw, expected := workClaimSessionFixture(t)
	other, err := raw.Create(sessionBeadFixture("other-start", "open", map[string]string{
		"state": "active", "generation": "1", "instance_token": "other-token", "work_dir": "/other",
	}))
	if err != nil {
		t.Fatal(err)
	}
	p := newBlockedRuntimeStart(t)
	m := NewManagerWithOptions(raw, p, WithStaleKeyDetectionWaiter(immediateStaleKeyDetectionWaiter))
	first := beginBlockedRuntimeStart(t, m, expected.ID)
	second := beginBlockedRuntimeStart(t, m, other.ID)
	if a, b := awaitRuntimeStart(t, p), awaitRuntimeStart(t, p); a == b {
		t.Fatal("independent sessions shared a runtime name")
	}
	p.allow()
	for _, result := range []<-chan error{first, second} {
		if err := awaitRuntimeResult(t, result); err != nil {
			t.Fatal(err)
		}
	}
}

func TestRuntimeOnlyStartDefersCorpseCleanupWhileMetadataLockIsAvailable(t *testing.T) {
	_, raw, expected := workClaimSessionFixture(t)
	p := newBlockedRuntimeStart(t)
	m := NewManagerWithOptions(raw, p, WithStaleKeyDetectionWaiter(immediateStaleKeyDetectionWaiter))
	result := beginBlockedRuntimeStart(t, m, expected.ID)
	awaitRuntimeStart(t, p)
	sessionMutationLocksMu.Lock()
	entry := sessionMutationLocks[expected.ID]
	sessionMutationLocksMu.Unlock()
	if entry == nil || !entry.mu.TryLock() {
		t.Fatal("provider wait still owns the metadata lock needed by startup claims")
	}
	entry.mu.Unlock()
	front := NewStore(beads.SessionStore{Store: &startFenceStore{Store: raw, check: func(string) {
		t.Fatal("corpse cleanup read a session with an outstanding runtime start")
	}}})
	cleaned, closed, err := front.CleanupDeadRuntime(expected, time.Now(), func(Info) (bool, error) {
		t.Fatal("corpse cleanup stopped a starting runtime")
		return true, nil
	})
	if cleaned || closed || err != nil {
		t.Fatalf("cleanup=%v close=%v error=%v", cleaned, closed, err)
	}
	p.allow()
	if err := awaitRuntimeResult(t, result); err != nil {
		t.Fatal(err)
	}
}

func TestRuntimeOnlyStartRejectsChangedExecutionWithoutRetryOrReplacementMutation(t *testing.T) {
	for key, value := range map[string]string{
		"instance_token": "new-token", "session_key": "replacement-thread", "work_dir": "/replacement",
		"state": "suspended", "held_until": "2126-01-01T00:00:00Z", "continuation_reset_pending": "true",
	} {
		t.Run(key, func(t *testing.T) {
			_, raw, expected := workClaimSessionFixture(t)
			p := newBlockedRuntimeStart(t)
			p.result = runtimepkg.ErrSessionDiedDuringStartup
			m := NewManagerWithOptions(raw, p, WithStaleKeyDetectionWaiter(immediateStaleKeyDetectionWaiter))
			result := beginBlockedRuntimeStart(t, m, expected.ID)
			name := awaitRuntimeStart(t, p)
			if err := WithSessionMutationLock(expected.ID, func() error {
				return raw.SetMetadata(expected.ID, key, value)
			}); err != nil {
				t.Fatal(err)
			}
			// Model a newer externally attributed runtime at the same name. The
			// failed old attempt must neither stop it nor clear its conversation.
			if err := p.Fake.Start(context.Background(), name, runtimepkg.Config{Command: "replacement-owner"}); err != nil {
				t.Fatal(err)
			}
			p.allow()
			err := awaitRuntimeResult(t, result)
			if !errors.Is(err, ErrRuntimeStartSuperseded) || errors.Is(err, runtimepkg.ErrSessionDiedDuringStartup) {
				t.Fatalf("stale startup verdict=%v", err)
			}
			current, err := raw.Get(expected.ID)
			if err != nil || current.Metadata[key] != value || p.calls.Load() != 1 {
				t.Fatalf("replacement changed or old attempt retried: %v, calls=%d", err, p.calls.Load())
			}
			if !p.IsRunning(name) || p.Fake.LastStartConfig(name).Command != "replacement-owner" {
				t.Fatal("old startup disturbed the replacement runtime")
			}
		})
	}
}

func TestRuntimeOnlyStartAllowsSameTokenGenerationHealing(t *testing.T) {
	_, raw, expected := workClaimSessionFixture(t)
	p := newBlockedRuntimeStart(t)
	m := NewManagerWithOptions(raw, p, WithStaleKeyDetectionWaiter(immediateStaleKeyDetectionWaiter))
	result := beginBlockedRuntimeStart(t, m, expected.ID)
	awaitRuntimeStart(t, p)
	if err := WithSessionMutationLock(expected.ID, func() error { return raw.SetMetadata(expected.ID, "generation", "3") }); err != nil {
		t.Fatal(err)
	}
	p.allow()
	if err := awaitRuntimeResult(t, result); err != nil {
		t.Fatalf("matching-token generation healing rejected: %v", err)
	}
}

func TestRuntimeOnlyStartCancellationReleasesReservation(t *testing.T) {
	_, raw, expected := workClaimSessionFixture(t)
	p := newBlockedRuntimeStart(t)
	m := NewManagerWithOptions(raw, p, WithStaleKeyDetectionWaiter(immediateStaleKeyDetectionWaiter))
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() { result <- m.StartRuntimeOnly(ctx, expected.ID, "test-worker", runtimepkg.Config{}) }()
	awaitRuntimeStart(t, p)
	cancel()
	if err := awaitRuntimeResult(t, result); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation=%v", err)
	}
	reserved, err := tryWithRuntimeStart(expected.ID, func() error { return nil })
	if !reserved || err != nil {
		t.Fatalf("canceled startup retained its reservation: %v %v", reserved, err)
	}
}

func TestRuntimeOnlyStartPreservesPendingCreateHealing(t *testing.T) {
	_, raw, expected := workClaimSessionFixture(t)
	if err := raw.SetMetadataBatch(expected.ID, map[string]string{"state": "creating", "pending_create_claim": "true"}); err != nil {
		t.Fatal(err)
	}
	p := newBlockedRuntimeStart(t)
	m := NewManagerWithOptions(raw, p, WithStaleKeyDetectionWaiter(immediateStaleKeyDetectionWaiter))
	result := beginBlockedRuntimeStart(t, m, expected.ID)
	awaitRuntimeStart(t, p)
	if err := WithSessionMutationLock(expected.ID, func() error {
		return raw.SetMetadataBatch(expected.ID, map[string]string{"state": "awake", "pending_create_claim": "", "generation": "3"})
	}); err != nil {
		t.Fatal(err)
	}
	p.allow()
	if err := awaitRuntimeResult(t, result); err != nil {
		t.Fatal(err)
	}
	current, err := raw.Get(expected.ID)
	if err != nil || current.Metadata["state"] != "awake" || current.Metadata["pending_create_claim"] != "" || current.Metadata["generation"] != "3" {
		t.Fatal("runtime-only bridge overwrote its caller's lifecycle progress")
	}
}

func TestRuntimeOnlyStartRefusesClosedOrRolledBackCreate(t *testing.T) {
	for _, closeRow := range []bool{false, true} {
		t.Run(map[bool]string{false: "claim-cleared", true: "closed"}[closeRow], func(t *testing.T) {
			_, raw, expected := workClaimSessionFixture(t)
			if err := raw.SetMetadataBatch(expected.ID, map[string]string{"state": "creating", "pending_create_claim": "true"}); err != nil {
				t.Fatal(err)
			}
			p := newBlockedRuntimeStart(t)
			p.result = runtimepkg.ErrSessionDiedDuringStartup
			m := NewManagerWithOptions(raw, p, WithStaleKeyDetectionWaiter(immediateStaleKeyDetectionWaiter))
			result := beginBlockedRuntimeStart(t, m, expected.ID)
			awaitRuntimeStart(t, p)
			if err := WithSessionMutationLock(expected.ID, func() error {
				if closeRow {
					return raw.Close(expected.ID)
				}
				return raw.SetMetadata(expected.ID, "pending_create_claim", "")
			}); err != nil {
				t.Fatal(err)
			}
			p.allow()
			if err := awaitRuntimeResult(t, result); !errors.Is(err, ErrRuntimeStartSuperseded) {
				t.Fatalf("rolled-back startup=%v", err)
			}
			current, err := raw.Get(expected.ID)
			if err != nil || current.Metadata["session_key"] != expected.SessionKey || p.calls.Load() != 1 {
				t.Fatal("stale startup retried or erased the retained native identity")
			}
		})
	}
}

func TestRuntimeOnlyStartPreservesFirstNativeKeyPublication(t *testing.T) {
	_, raw, expected := workClaimSessionFixture(t)
	if err := raw.SetMetadata(expected.ID, "session_key", ""); err != nil {
		t.Fatal(err)
	}
	p := newBlockedRuntimeStart(t)
	m := NewManagerWithOptions(raw, p, WithStaleKeyDetectionWaiter(immediateStaleKeyDetectionWaiter))
	result := beginBlockedRuntimeStart(t, m, expected.ID)
	awaitRuntimeStart(t, p)
	if err := WithSessionMutationLock(expected.ID, func() error { return raw.SetMetadata(expected.ID, "session_key", "discovered-thread") }); err != nil {
		t.Fatal(err)
	}
	p.allow()
	if err := awaitRuntimeResult(t, result); err != nil {
		t.Fatal(err)
	}
	current, err := raw.Get(expected.ID)
	if err != nil || current.Metadata["session_key"] != "discovered-thread" || p.calls.Load() != 1 {
		t.Fatal("first native identity was lost or startup repeated")
	}
}

func TestRuntimeOnlyStartRechecksAuthoritativeRowAfterProviderIO(t *testing.T) {
	_, raw, expected := workClaimSessionFixture(t)
	cache := beads.NewCachingStoreForTest(raw, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatal(err)
	}
	p := newBlockedRuntimeStart(t)
	m := NewManagerWithOptions(cache, p, WithStaleKeyDetectionWaiter(immediateStaleKeyDetectionWaiter))
	result := beginBlockedRuntimeStart(t, m, expected.ID)
	awaitRuntimeStart(t, p)
	if err := WithSessionMutationLock(expected.ID, func() error { return raw.SetMetadata(expected.ID, "instance_token", "authoritative-replacement") }); err != nil {
		t.Fatal(err)
	}
	cached, err := cache.Get(expected.ID)
	if err != nil || cached.Metadata["instance_token"] != expected.InstanceToken {
		t.Fatal("fixture did not retain the stale cached session snapshot")
	}
	p.allow()
	if err := awaitRuntimeResult(t, result); !errors.Is(err, ErrRuntimeStartSuperseded) {
		t.Fatalf("stale cached startup was accepted: %v", err)
	}
	current, err := raw.Get(expected.ID)
	if err != nil || current.Metadata["instance_token"] != "authoritative-replacement" {
		t.Fatal("replacement incarnation changed")
	}
}

func TestRuntimeOnlyStartUnavailableReadDoesNotProveSupersession(t *testing.T) {
	_, raw, expected := workClaimSessionFixture(t)
	store := &runtimeStartReadFaultStore{MemStore: raw}
	p := newBlockedRuntimeStart(t)
	m := NewManagerWithOptions(store, p, WithStaleKeyDetectionWaiter(immediateStaleKeyDetectionWaiter))
	result := beginBlockedRuntimeStart(t, m, expected.ID)
	awaitRuntimeStart(t, p)
	store.unavailable.Store(true)
	p.allow()
	err := awaitRuntimeResult(t, result)
	if !errors.Is(err, runtimepkg.ErrRuntimeUnavailable) || errors.Is(err, ErrRuntimeStartSuperseded) {
		t.Fatalf("read failure was treated as a proved stale execution: %v", err)
	}
	current, err := raw.Get(expected.ID)
	if err != nil || current.Metadata["session_key"] != expected.SessionKey || p.calls.Load() != 1 {
		t.Fatal("unavailable identity read changed native identity or retried startup")
	}
}

func TestRuntimeOnlySupersededCapacityKeepsRefusalWithoutStaleKeyRecovery(t *testing.T) {
	_, raw, expected := workClaimSessionFixture(t)
	p := newBlockedRuntimeStart(t)
	p.result = &runtimepkg.CapacityError{ExitCode: 75, Source: "exit-status", Err: runtimepkg.ErrSessionDiedDuringStartup}
	m := NewManagerWithOptions(raw, p, WithStaleKeyDetectionWaiter(immediateStaleKeyDetectionWaiter))
	result := beginBlockedRuntimeStart(t, m, expected.ID)
	awaitRuntimeStart(t, p)
	if err := WithSessionMutationLock(expected.ID, func() error { return raw.SetMetadata(expected.ID, "sleep_intent", "wait-hold") }); err != nil {
		t.Fatal(err)
	}
	p.allow()
	err := awaitRuntimeResult(t, result)
	if !errors.Is(err, ErrRuntimeStartSuperseded) || !runtimepkg.IsProviderCapacity(err) || errors.Is(err, runtimepkg.ErrSessionDiedDuringStartup) {
		t.Fatalf("superseded capacity verdict must retain refusal without authorizing fresh-key recovery: %v", err)
	}
	if p.calls.Load() != 1 {
		t.Fatalf("provider called %d times", p.calls.Load())
	}
}

func TestRuntimeProviderStartAcceptanceRequiresCompletedStart(t *testing.T) {
	for _, tc := range []struct {
		name        string
		genericIO   bool
		preChanged  bool
		unavailable bool
		cancel      bool
		providerErr error
		wantCalls   int
		wantProof   bool
	}{
		{name: "completed Start superseded", wantCalls: 1, wantProof: true},
		{name: "generic observation superseded", genericIO: true, wantCalls: 1},
		{name: "pre-call supersession", preChanged: true},
		{name: "provider error", providerErr: errors.New("provider failed"), wantCalls: 1},
		{name: "capacity refusal", providerErr: &runtimepkg.CapacityError{Err: runtimepkg.ErrSessionDiedDuringStartup, ExitCode: 75, Source: "exit-status"}, wantCalls: 1},
		{name: "unavailable post-call read", unavailable: true, wantCalls: 1},
		{name: "canceled nil-returning provider", cancel: true, wantCalls: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, raw, expected := workClaimSessionFixture(t)
			store := &runtimeStartReadFaultStore{MemStore: raw}
			m := NewManagerWithOptions(store, runtimepkg.NewFake())
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			lock := acquireSessionMutationLock(expected.ID)
			defer releaseSessionMutationLock(expected.ID, lock)
			ctx = context.WithValue(ctx, runtimeStartGuardKey{}, &runtimeStartGuard{id: expected.ID, lock: lock})
			if tc.preChanged {
				if err := raw.SetMetadata(expected.ID, "instance_token", "replacement"); err != nil {
					t.Fatal(err)
				}
			}
			calls := 0
			effect := func() error {
				calls++
				if err := raw.SetMetadata(expected.ID, "instance_token", "replacement"); err != nil {
					return err
				}
				store.unavailable.Store(tc.unavailable)
				if tc.cancel {
					cancel()
				}
				return tc.providerErr
			}
			var err error
			if tc.genericIO {
				err = m.runtimeStartIO(ctx, expected, effect)
			} else {
				err = m.runtimeProviderStartIO(ctx, expected, effect)
			}
			store.unavailable.Store(false)
			if calls != tc.wantCalls || RuntimeStartProviderAccepted(err) != tc.wantProof {
				t.Fatalf("calls=%d accepted=%v error=%v, want calls=%d accepted=%v", calls, RuntimeStartProviderAccepted(err), err, tc.wantCalls, tc.wantProof)
			}
			if tc.unavailable {
				if !errors.Is(err, runtimepkg.ErrRuntimeUnavailable) || errors.Is(err, ErrRuntimeStartSuperseded) {
					t.Fatalf("unavailable read became acceptance or supersession: %v", err)
				}
			} else if !errors.Is(err, ErrRuntimeStartSuperseded) || errors.Is(err, runtimepkg.ErrSessionDiedDuringStartup) {
				t.Fatalf("supersession lost its fence or retained stale-key recovery: %v", err)
			}
			if runtimepkg.IsProviderCapacity(tc.providerErr) && !runtimepkg.IsProviderCapacity(err) {
				t.Fatalf("capacity refusal lost precedence: %v", err)
			}
		})
	}
}

// Strict continuation arrived on merged main after this startup repair was
// developed. It must share runtime exclusion while claims retain metadata access.
func TestStartContinuationWaitsForRuntimeOnlyStart(t *testing.T) {
	for _, cancelWaiter := range []bool{false, true} {
		name := "runtime-completes"
		if cancelWaiter {
			name = "waiter-canceled"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				raw, _, _, expected := continuationFixture(t)
				p := newBlockedRuntimeStart(t)
				m := NewManagerWithOptions(raw, p, WithStaleKeyDetectionWaiter(immediateStaleKeyDetectionWaiter))
				first := beginBlockedRuntimeStart(t, m, expected.SessionID)
				awaitRuntimeStart(t, p)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				var prepared atomic.Bool
				second := make(chan error, 1)
				go func() {
					second <- m.StartContinuation(ctx, expected, func(info Info) (ContinuationLaunch, error) {
						prepared.Store(true)
						return prepareContinuationFixture(info)
					})
				}()
				t.Cleanup(func() {
					cancel()
					p.allow()
				})
				synctest.Wait()
				if prepared.Load() || p.calls.Load() != 1 {
					t.Errorf("continuation overlapped pending runtime start: prepared=%v starts=%d", prepared.Load(), p.calls.Load())
				}
				if cancelWaiter {
					cancel()
					if err := awaitRuntimeResult(t, second); !errors.Is(err, context.Canceled) {
						t.Errorf("waiting continuation = %v, want cancellation", err)
					}
				}
				p.allow()
				if err := awaitRuntimeResult(t, first); err != nil {
					t.Fatal(err)
				}
				if !cancelWaiter {
					if err := awaitRuntimeResult(t, second); !errors.Is(err, ErrContinuationRuntime) {
						t.Errorf("continuation after first start = %v, want live-runtime refusal", err)
					}
				}
				if prepared.Load() || p.calls.Load() != 1 {
					t.Errorf("continuation reached provider preparation: prepared=%v starts=%d", prepared.Load(), p.calls.Load())
				}
			})
		})
	}
}
