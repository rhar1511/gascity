package session

import (
	"context"
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/rollout/gate"
)

func workClaimSessionFixture(t *testing.T) (*Store, *beads.MemStore, Info) {
	t.Helper()
	b := sessionBeadFixture("claim-session", "open", map[string]string{
		"state": "active", "generation": "2", "instance_token": "token-2",
		"work_dir": "/native/source", "gc.work_dir": "/native/source",
		"session_key": "original-thread", "provider": "test", "template": "rig/worker",
	})
	b.Revision = 1
	raw := beads.NewMemStoreFrom(1, []beads.Bead{b}, nil)
	_, err := beads.OpenStoreAtForCity(context.Background(), beads.StoreOpenOptions{
		ScopeRoot: t.TempDir(), Provider: "file", ConditionalWrites: gate.Require,
		OpenFileStore: func() (beads.Store, error) { return raw, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	store := NewStore(beads.SessionStore{Store: raw})
	info, err := store.Get(b.ID)
	if err != nil {
		t.Fatal(err)
	}
	return store, raw, info
}

func assertWorkClaimLockHeld(t *testing.T, id string) {
	t.Helper()
	sessionMutationLocksMu.Lock()
	entry := sessionMutationLocks[id]
	sessionMutationLocksMu.Unlock()
	if entry == nil {
		t.Fatal("claim session lock absent")
	}
	if entry.mu.TryLock() {
		entry.mu.Unlock()
		t.Fatal("claim session lock released before operation completed")
	}
}

func TestCommitWorkClaimHoldsSessionLockThroughTransitionAndReciprocalStamp(t *testing.T) {
	_, raw, expected := workClaimSessionFixture(t)
	wrapped := &workClaimFenceStore{MemStore: raw, check: func(id string) { assertWorkClaimLockHeld(t, id) }}
	store := NewStore(beads.SessionStore{Store: wrapped})
	err := store.CommitWorkClaim(expected, func(current Info, _ PersistedResponse) (string, string, error) {
		assertWorkClaimLockHeld(t, expected.ID)
		if current.SessionKey != expected.SessionKey || current.WorkDir != expected.WorkDir {
			t.Fatal("identity changed")
		}
		return "step-1", "3", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	id, generation, err := NewStore(beads.SessionStore{Store: raw}).CurrentClaim(expected.ID)
	if err != nil || id != "step-1" || generation != "3" {
		t.Fatalf("claim=%s/%s error=%v", id, generation, err)
	}
	if wrapped.writes != 1 {
		t.Fatalf("reciprocal writes=%d, want one", wrapped.writes)
	}
}

func TestCommitWorkClaimRejectsChangedIdentityBeforeTransition(t *testing.T) {
	for key, value := range map[string]string{
		"instance_token": "new-token", "generation": "3", "work_dir": "/other", "gc.work_dir": "/other",
		"session_key": "replacement-thread", "provider": "other", "template": "foreign/worker", "alias": "foreign",
		"state": "suspended",
	} {
		t.Run(key, func(t *testing.T) {
			store, raw, expected := workClaimSessionFixture(t)
			if err := raw.SetMetadata(expected.ID, key, value); err != nil {
				t.Fatal(err)
			}
			called := false
			err := store.CommitWorkClaim(expected, func(Info, PersistedResponse) (string, string, error) { called = true; return "step", "1", nil })
			if !errors.Is(err, ErrWorkClaimSessionChanged) || called {
				t.Fatalf("error=%v called=%v", err, called)
			}
			id, _, err := store.CurrentClaim(expected.ID)
			if err != nil || id != "" {
				t.Fatalf("unexpected reciprocal claim %q: %v", id, err)
			}
		})
	}
}

func TestCommitWorkClaimRefusesExternalChangeAfterTransitionBeforeStamp(t *testing.T) {
	store, raw, expected := workClaimSessionFixture(t)
	err := store.CommitWorkClaim(expected, func(Info, PersistedResponse) (string, string, error) {
		// Simulate a writer outside this process-local lock after the source
		// transition committed. That durable transition cannot be rolled back.
		if err := raw.SetMetadata(expected.ID, "session_key", "replacement-thread"); err != nil {
			t.Fatal(err)
		}
		return "step", "4", nil
	})
	if !errors.Is(err, ErrWorkClaimSessionChanged) {
		t.Fatalf("error=%v", err)
	}
	id, _, err := store.CurrentClaim(expected.ID)
	if err != nil || id != "" {
		t.Fatalf("stamped changed session %q: %v", id, err)
	}
}

func TestCommitWorkClaimLostResponsePreservesExactRetry(t *testing.T) {
	store, _, expected := workClaimSessionFixture(t)
	lost := errors.New("committed transition response lost")
	if err := store.CommitWorkClaim(expected, func(Info, PersistedResponse) (string, string, error) { return "", "", lost }); !errors.Is(err, lost) {
		t.Fatal(err)
	}
	id, _, err := store.CurrentClaim(expected.ID)
	if err != nil || id != "" {
		t.Fatalf("lost response stamped %q: %v", id, err)
	}
	for range 2 {
		if err := store.CommitWorkClaim(expected, func(Info, PersistedResponse) (string, string, error) { return "step", "4", nil }); err != nil {
			t.Fatal(err)
		}
	}
	id, generation, err := store.CurrentClaim(expected.ID)
	if err != nil || id != "step" || generation != "4" {
		t.Fatalf("retry=%s/%s: %v", id, generation, err)
	}
}

func TestCommitWorkClaimInvalidReceiptDoesNotStamp(t *testing.T) {
	for _, generation := range []string{"", "0", "-1", "01"} {
		t.Run(generation, func(t *testing.T) {
			store, _, expected := workClaimSessionFixture(t)
			if err := store.CommitWorkClaim(expected, func(Info, PersistedResponse) (string, string, error) { return "step", generation, nil }); err == nil {
				t.Fatal("invalid generation accepted")
			}
			id, _, err := store.CurrentClaim(expected.ID)
			if err != nil || id != "" {
				t.Fatalf("invalid receipt stamped %q: %v", id, err)
			}
		})
	}
}

type workClaimFenceStore struct {
	*beads.MemStore
	check       func(string)
	writes      int
	beforeWrite func()
}

func (s *workClaimFenceStore) Get(id string) (beads.Bead, error) {
	s.check(id)
	return s.MemStore.Get(id)
}

func (s *workClaimFenceStore) UpdateIfMatch(id string, revision int64, opts beads.UpdateOpts) error {
	s.check(id)
	if s.beforeWrite != nil {
		s.beforeWrite()
	}
	s.writes++
	return s.MemStore.UpdateIfMatch(id, revision, opts)
}

func TestCommitWorkClaimConditionalRaceDoesNotStampReplacement(t *testing.T) {
	_, raw, expected := workClaimSessionFixture(t)
	wrapped := &workClaimFenceStore{MemStore: raw, check: func(id string) { assertWorkClaimLockHeld(t, id) }}
	wrapped.beforeWrite = func() {
		if err := raw.SetMetadata(expected.ID, "gc.worktree_attempt_id", "replacement-attempt"); err != nil {
			t.Fatal(err)
		}
	}
	store := NewStore(beads.SessionStore{Store: wrapped})
	err := store.CommitWorkClaim(expected, func(Info, PersistedResponse) (string, string, error) { return "step", "4", nil })
	if !errors.Is(err, ErrWorkClaimSessionChanged) {
		t.Fatalf("race error=%v", err)
	}
	id, _, err := NewStore(beads.SessionStore{Store: raw}).CurrentClaim(expected.ID)
	if err != nil || id != "" {
		t.Fatalf("raced stamp=%q error=%v", id, err)
	}
}

type workClaimUnfencedStore struct{ beads.Store }

func TestCommitWorkClaimRequiresConditionalWriterBeforeTransition(t *testing.T) {
	_, raw, expected := workClaimSessionFixture(t)
	store := NewStore(beads.SessionStore{Store: &workClaimUnfencedStore{Store: raw}})
	called := false
	err := store.CommitWorkClaim(expected, func(Info, PersistedResponse) (string, string, error) { called = true; return "step", "4", nil })
	if !errors.Is(err, beads.ErrConditionalWriteUnsupported) || called {
		t.Fatalf("error=%v transition=%v", err, called)
	}
}

func TestCommitWorkClaimRefusesFactoryDisabledPolicyBeforeTransition(t *testing.T) {
	_, raw, expected := workClaimSessionFixture(t)
	_, err := beads.OpenStoreAtForCity(context.Background(), beads.StoreOpenOptions{
		ScopeRoot: t.TempDir(), Provider: "file", ConditionalWrites: gate.Off,
		OpenFileStore: func() (beads.Store, error) { return raw, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	called := false
	err = NewStore(beads.SessionStore{Store: raw}).CommitWorkClaim(expected, func(Info, PersistedResponse) (string, string, error) {
		called = true
		return "step", "4", nil
	})
	if !errors.Is(err, beads.ErrConditionalWriteUnsupported) || called {
		t.Fatalf("disabled policy error=%v transition=%v", err, called)
	}
}

func TestReadWorkClaimSessionUsesAuthoritativeIdentityAndCustody(t *testing.T) {
	store, raw, expected := workClaimSessionFixture(t)
	if _, _, err := store.ReadWorkClaimSession(expected); err != nil {
		t.Fatal(err)
	}
	if err := raw.SetMetadata(expected.ID, "session_key", "changed-native-thread"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.ReadWorkClaimSession(expected); !errors.Is(err, ErrWorkClaimSessionChanged) {
		t.Fatalf("changed native identity: %v", err)
	}
}
