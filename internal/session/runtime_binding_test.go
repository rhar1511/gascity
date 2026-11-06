package session

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/rollout/gate"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/routing"
)

func TestCreateSessionRuntimeBackendRoundTrip(t *testing.T) {
	for _, deferred := range []bool{false, true} {
		t.Run(map[bool]string{false: "started", true: "deferred"}[deferred], func(t *testing.T) {
			store := beads.NewMemStore()
			local, remote := runtime.NewFake(), runtime.NewFake()
			fingerprint := strings.Repeat("a", 64)
			sp, err := routing.New("local", map[string]runtime.Provider{"local": local, "remote@" + fingerprint: remote})
			if err != nil {
				t.Fatal(err)
			}
			manager := NewManagerWithOptions(store, sp)
			created, err := manager.CreateSession(context.Background(), CreateOptions{ExplicitName: "selected-runtime", Template: "helper", Command: "coding-provider", Provider: "coding-provider", WorkDir: t.TempDir(), RuntimeBackend: "remote", RuntimeFingerprint: fingerprint, BeadOnly: deferred})
			if err != nil {
				t.Fatal(err)
			}
			reloaded, err := manager.PersistedStore().Get(created.ID)
			if err != nil {
				t.Fatal(err)
			}
			if created.RuntimeBackend != "remote" || reloaded.RuntimeBackend != "remote" || reloaded.RuntimeFingerprint != fingerprint {
				t.Fatalf("runtime identity lost: created=%#v reloaded=%#v", created, reloaded)
			}
			if local.CountCalls("Start", created.SessionName) != 0 || remote.CountCalls("Start", created.SessionName) != map[bool]int{true: 0, false: 1}[deferred] {
				t.Fatal("creation contacted the wrong backend")
			}
		})
	}
}

func TestRuntimeIdentityFencesDeadRuntimeClosure(t *testing.T) {
	expected := Info{ID: "old-corpse", SessionNameMetadata: "exact", RuntimeBackend: "capsule", RuntimeFingerprint: strings.Repeat("a", 64)}
	changed := expected
	changed.RuntimeBackend = "local"
	if deadRuntimeRowUnchanged(expected, changed, time.Now()) {
		t.Fatal("corpse proof survived changed backend")
	}
	changed = expected
	changed.RuntimeFingerprint = strings.Repeat("b", 64)
	if deadRuntimeRowUnchanged(expected, changed, time.Now()) {
		t.Fatal("corpse proof survived changed declaration")
	}
}

func TestRuntimeIdentityFencesPendingLeaseAndUnsupportedDialogs(t *testing.T) {
	expected := Info{InstanceToken: "same", Generation: "1", RuntimeBackend: "capsule", RuntimeFingerprint: strings.Repeat("a", 64), PendingCreateClaim: true, MetadataState: "creating"}
	changed := expected
	changed.RuntimeFingerprint = strings.Repeat("b", 64)
	a, b := LeaseFromInfo(expected), LeaseFromInfo(changed)
	if a.SameIdentity(b) || a.CanRollback(b) || a.CommitVerdict(b) == LeaseCommit || a.CommitVerdict(b) == LeaseDiscardStopRuntime {
		t.Fatal("runtime drift authorized commit, rollback or cleanup")
	}
	backend := runtime.NewFake()
	router, err := routing.New("local", map[string]runtime.Provider{"local": struct{ runtime.Provider }{backend}})
	if err != nil {
		t.Fatal(err)
	}
	if err := router.Bind("core-only", "local"); err != nil {
		t.Fatal(err)
	}
	m := NewManagerWithOptions(beads.NewMemStore(), router)
	if m.dismissKnownDialogsLocked(context.Background(), "core-only", time.Second) {
		t.Fatal("unsupported owner dialog check counted as verification")
	}
}

func TestRuntimeObservationReconstructsPersistedBackend(t *testing.T) {
	store := beads.NewMemStore()
	local, remote := runtime.NewFake(), runtime.NewFake()
	fingerprint := strings.Repeat("c", 64)
	makeRouter := func(key string) *routing.Provider {
		p, err := routing.New("local", map[string]runtime.Provider{"local": local, key: remote})
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	manager := NewManagerWithOptions(store, makeRouter("remote@"+fingerprint))
	created, err := manager.CreateSession(context.Background(), CreateOptions{ExplicitName: "retained-runtime", Template: "helper", Provider: "coding-provider", Command: "coding-provider", WorkDir: t.TempDir(), RuntimeBackend: "remote", RuntimeFingerprint: fingerprint})
	if err != nil {
		t.Fatal(err)
	}
	current, err := manager.PersistedStore().Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	observer := NewManagerWithOptions(store, makeRouter("remote@"+fingerprint))
	observation, err := observer.ObserveRuntimeForInfo(current, nil)
	if err != nil || !observation.Running {
		t.Fatalf("persisted route not reconstructed: %#v %v", observation, err)
	}
	if observer.EnrichInfo(current).State != StateActive {
		t.Fatal("live persisted route downgraded")
	}
	unknown := NewManagerWithOptions(store, makeRouter("remote@"+strings.Repeat("d", 64)))
	if _, err := unknown.ObserveRuntimeForInfo(current, nil); err == nil {
		t.Fatal("changed declaration treated as absence")
	}
	if unknown.EnrichInfo(current).State != StateActive {
		t.Fatal("unknown route downgraded lifecycle state")
	}
}

func TestRuntimeRefusalCannotRestoreChangedBackendCustody(t *testing.T) {
	base := Info{ID: "session", State: StateAsleep, RuntimeBackend: "remote", RuntimeFingerprint: strings.Repeat("e", 64), Generation: "1", InstanceToken: "exact-token"}
	if !RuntimeStartRefusalRestoreAllowed(base, base) {
		t.Fatal("unchanged dormant custody refused")
	}
	changed := base
	changed.RuntimeBackend = "local"
	if RuntimeStartRefusalRestoreAllowed(base, changed) {
		t.Fatal("changed backend retained stale refusal authority")
	}
	changed = base
	changed.RuntimeFingerprint = strings.Repeat("f", 64)
	if RuntimeStartRefusalRestoreAllowed(base, changed) {
		t.Fatal("changed declaration retained stale refusal authority")
	}
}

func TestCreateSessionRejectsUnresolvedOrForgedRuntimeBeforeMutation(t *testing.T) {
	fingerprint := strings.Repeat("b", 64)
	tests := []CreateOptions{
		{RuntimeBackend: "remote"},
		{RuntimeFingerprint: fingerprint},
		{RuntimeBackend: "remote", RuntimeFingerprint: "invalid"},
		{RuntimeBackend: "unknown", RuntimeFingerprint: fingerprint},
		{RuntimeBackend: "remote", RuntimeFingerprint: fingerprint, ExtraMeta: map[string]string{"gc.runtime_backend": "local"}},
	}
	for _, spec := range tests {
		store := beads.NewMemStore()
		backend := runtime.NewFake()
		sp, err := routing.New("local", map[string]runtime.Provider{"local": backend, "remote@" + fingerprint: backend})
		if err != nil {
			t.Fatal(err)
		}
		manager := NewManagerWithOptions(store, sp)
		spec.Command = "coding-provider"
		spec.Template = "helper"
		spec.BeadOnly = true
		if _, err := manager.CreateSession(context.Background(), spec); err == nil {
			t.Fatalf("invalid runtime spec accepted: %#v", spec)
		}
		infos, err := manager.PersistedStore().List("", "")
		if err != nil {
			t.Fatal(err)
		}
		if len(infos) != 0 || len(backend.SnapshotCalls()) != 0 {
			t.Fatalf("refused creation had effects: sessions=%d calls=%v", len(infos), backend.SnapshotCalls())
		}
	}
}

func TestRuntimeResolverAppliesOnlyToNewSessions(t *testing.T) {
	fingerprint := strings.Repeat("a", 64)
	local, remote := runtime.NewFake(), runtime.NewFake()
	sp, err := routing.New("local", map[string]runtime.Provider{"local": local, "remote@" + fingerprint: remote})
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	manager := NewManagerWithOptions(beads.NewMemStore(), sp, WithRuntimeBackendResolver(func(_ CreateOptions) (string, string, error) { calls++; return "remote", fingerprint, nil }))
	created, err := manager.CreateSession(context.Background(), CreateOptions{Template: "helper", Provider: "coding-provider", Command: "coding-provider", WorkDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if created.RuntimeBackend != "remote" {
		t.Fatal("new session did not capture runtime selection")
	}
	if _, err := manager.Get(created.ID); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("runtime resolver called %d times; existing sessions must use their own identity", calls)
	}
}

func TestRuntimeCompletedLeasePreservesChangedCustody(t *testing.T) {
	for _, patch := range []map[string]string{
		{"gc.runtime_backend": "local"},
		{"gc.runtime_fingerprint": strings.Repeat("b", 64)},
		{"session_key": "other-native-key"},
		{"work_dir": "/other/worktree"},
		{"session_name": "other-runtime-name"},
	} {
		t.Run(fmt.Sprint(patch), func(t *testing.T) {
			b := sessionBeadFixture("typed-failure", "open", map[string]string{
				"state": "creating", "pending_create_claim": "true", "generation": "2", "instance_token": "token-2",
				"last_woke_at": "2026-11-05T00:00:00Z", "session_name": "exact-runtime", "session_key": "exact-native-key", "work_dir": "/exact/worktree",
				"gc.runtime_backend": "capsule", "gc.runtime_fingerprint": strings.Repeat("a", 64),
			})
			mem := beads.NewMemStoreFrom(1, []beads.Bead{b}, nil)
			opened, err := beads.OpenStoreAtForCity(context.Background(), beads.StoreOpenOptions{ScopeRoot: t.TempDir(), Provider: "file", ConditionalWrites: gate.Require, OpenFileStore: func() (beads.Store, error) { return mem, nil }})
			if err != nil {
				t.Fatal(err)
			}
			front := NewStore(beads.SessionStore{Store: opened.Store})
			expected, err := front.Get(b.ID)
			if err != nil {
				t.Fatal(err)
			}
			if err := mem.SetMetadataBatch(b.ID, patch); err != nil {
				t.Fatal(err)
			}
			applied, err := front.ReleaseCompletedPendingStartLease(expected)
			if err != nil || applied {
				t.Fatalf("changed custody release=(%v,%v)", applied, err)
			}
			after, err := mem.Get(b.ID)
			if err != nil {
				t.Fatal(err)
			}
			if after.Metadata["last_woke_at"] != expected.LastWokeAt {
				t.Fatal("changed custody lost its lease")
			}
		})
	}
}

func TestRuntimeMissingPersistedPairRefusesCachedBinding(t *testing.T) {
	backend := runtime.NewFake()
	sp, err := routing.New("local", map[string]runtime.Provider{"local": backend})
	if err != nil {
		t.Fatal(err)
	}
	if err := sp.Bind("exact-key", "local"); err != nil {
		t.Fatal(err)
	}
	if err := BindRuntimeInfo(sp, Info{SessionName: "exact-key"}); err == nil {
		t.Fatal("cached binding supplied missing durable ownership")
	}
	if len(backend.SnapshotCalls()) != 0 {
		t.Fatal("missing ownership contacted backend")
	}
}

func TestRuntimeRetiredManagerCannotCreate(t *testing.T) {
	mem := beads.NewMemStore()
	backend := runtime.NewFake()
	fingerprint := strings.Repeat("a", 64)
	sp, err := routing.New("local", map[string]runtime.Provider{"local": backend, "capsule@" + fingerprint: backend})
	if err != nil {
		t.Fatal(err)
	}
	manager := NewManagerWithOptions(mem, sp)
	if err := sp.WithRoutingReplacement(func() (bool, error) { return true, nil }); err != nil {
		t.Fatal(err)
	}
	_, err = manager.CreateSession(context.Background(), CreateOptions{BeadOnly: true, Template: "helper", RuntimeBackend: "capsule", RuntimeFingerprint: fingerprint})
	if err == nil {
		t.Fatal("retired manager created a row")
	}
	infos, err := manager.PersistedStore().List("", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(infos) != 0 || len(backend.SnapshotCalls()) != 0 {
		t.Fatal("retired creator mutated store or runtime")
	}
}
