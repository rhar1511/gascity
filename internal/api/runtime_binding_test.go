package api

import (
	"context"
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/routing"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worker"
)

type atomicRuntimeSnapshotState struct {
	*fakeState
	reads     int
	snapshots int
}

func (s *atomicRuntimeSnapshotState) Config() *config.City { s.reads++; return nil }
func (s *atomicRuntimeSnapshotState) SessionProvider() runtime.Provider {
	s.reads++
	return runtime.NewFake()
}

func (s *atomicRuntimeSnapshotState) SessionRuntimeSnapshot() (*config.City, runtime.Provider) {
	s.snapshots++
	return s.cfg, s.fakeState.SessionProvider()
}

func TestAPICreationCapturesAtomicRuntimeSnapshot(t *testing.T) {
	for _, factory := range []bool{false, true} {
		t.Run(map[bool]string{false: "manager", true: "worker"}[factory], func(t *testing.T) {
			fs := newSessionFakeState(t)
			fs.cfg.Agents[0].Runtime = "capsule"
			fs.cfg.Runtimes = map[string]config.DiscoveredRuntime{"capsule": {Name: "capsule", Command: "/test/exec"}}
			binding, err := config.RuntimeBindingForName(fs.cfg, "capsule")
			if err != nil {
				t.Fatal(err)
			}
			key, _ := session.RuntimeBackendKey(binding.Name, binding.Fingerprint)
			fs.sessionProvider, err = routing.New("local", map[string]runtime.Provider{"local": runtime.NewFake(), key: runtime.NewFake()})
			if err != nil {
				t.Fatal(err)
			}
			state := &atomicRuntimeSnapshotState{fakeState: fs}
			srv := &Server{state: state}
			var info session.Info
			if !factory {
				info, err = srv.sessionManager(fs.cityBeadStore).CreateSession(context.Background(), session.CreateOptions{BeadOnly: true, Template: fs.cfg.Agents[0].QualifiedName(), Provider: "claude", WorkDir: t.TempDir()})
			} else {
				f, e := srv.workerFactory(fs.cityBeadStore)
				if e != nil {
					t.Fatal(e)
				}
				h, e := f.Session(worker.SessionSpec{Template: fs.cfg.Agents[0].QualifiedName(), Provider: "claude", WorkDir: t.TempDir()})
				if e != nil {
					t.Fatal(e)
				}
				info, err = h.Create(context.Background(), worker.CreateModeDeferred)
			}
			if err != nil || info.RuntimeBackend != binding.Name || state.reads != 0 || state.snapshots != 2 {
				t.Fatalf("runtime=%s err=%v separate reads=%d snapshots=%d", info.RuntimeBackend, err, state.reads, state.snapshots)
			}
		})
	}
}

func TestAPICreationPersistsConfiguredRuntime(t *testing.T) {
	for _, factory := range []bool{false, true} {
		t.Run(map[bool]string{false: "manager", true: "worker"}[factory], func(t *testing.T) {
			fs := newSessionFakeState(t)
			fs.cfg.Agents[0].Runtime = "capsule"
			fs.cfg.Runtimes = map[string]config.DiscoveredRuntime{"capsule": {Name: "capsule", Command: "/test/exec"}}
			binding, err := config.RuntimeBindingForName(fs.cfg, "capsule")
			if err != nil {
				t.Fatal(err)
			}
			key, _ := session.RuntimeBackendKey(binding.Name, binding.Fingerprint)
			local, remote := runtime.NewFake(), runtime.NewFake()
			fs.sessionProvider, err = routing.New("local", map[string]runtime.Provider{"local": local, key: remote})
			if err != nil {
				t.Fatal(err)
			}
			srv := New(fs)
			var info session.Info
			if !factory {
				info, err = srv.sessionManager(fs.cityBeadStore).CreateSession(context.Background(), session.CreateOptions{BeadOnly: true, Template: fs.cfg.Agents[0].QualifiedName(), Provider: "claude", WorkDir: t.TempDir()})
			} else {
				f, factoryErr := srv.workerFactory(fs.cityBeadStore)
				if factoryErr != nil {
					t.Fatal(factoryErr)
				}
				h, factoryErr := f.Session(worker.SessionSpec{Template: fs.cfg.Agents[0].QualifiedName(), Provider: "claude", WorkDir: t.TempDir()})
				if factoryErr != nil {
					t.Fatal(factoryErr)
				}
				info, err = h.Create(context.Background(), worker.CreateModeDeferred)
			}
			if err != nil || info.RuntimeBackend != binding.Name || info.RuntimeFingerprint != binding.Fingerprint {
				t.Fatalf("binding=%#v err=%v", info, err)
			}
		})
	}
}

func TestAPICapturedFactoryRefusesNewPublication(t *testing.T) {
	fs := newSessionFakeState(t)
	fs.cfg.Agents[0].Runtime = "capsule"
	fs.cfg.Runtimes = map[string]config.DiscoveredRuntime{"capsule": {Name: "capsule", Command: "/test/exec"}}
	binding, err := config.RuntimeBindingForName(fs.cfg, "capsule")
	if err != nil {
		t.Fatal(err)
	}
	key, _ := session.RuntimeBackendKey(binding.Name, binding.Fingerprint)
	fs.sessionProvider, err = routing.New("local", map[string]runtime.Provider{"local": runtime.NewFake(), key: runtime.NewFake()})
	if err != nil {
		t.Fatal(err)
	}
	srv := New(fs)
	factory, err := srv.workerFactory(fs.cityBeadStore)
	if err != nil {
		t.Fatal(err)
	}
	template := fs.cfg.Agents[0].QualifiedName()
	newer := *fs.cfg
	fs.cfg = &newer
	for _, typed := range []bool{false, true} {
		spec := worker.SessionSpec{Template: template, Provider: "claude", WorkDir: t.TempDir()}
		if typed {
			spec.RuntimeBackend = binding.Name
			spec.RuntimeFingerprint = binding.Fingerprint
		}
		handle, err := factory.Session(spec)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := handle.Create(context.Background(), worker.CreateModeDeferred); !errors.Is(err, runtime.ErrRuntimeUnavailable) {
			t.Fatalf("stale creation did not refuse: %v", err)
		}
	}
	info := session.Info{ID: "existing", Template: template, SessionName: "exact-native", RuntimeBackend: binding.Name, RuntimeFingerprint: binding.Fingerprint}
	if _, err := factory.SessionByRecord(info, session.PersistedResponse{}); !errors.Is(err, runtime.ErrRuntimeUnavailable) {
		t.Fatalf("stale wake resolver did not refuse: %v", err)
	}
	rows, err := session.NewStore(fs.SessionsBeadStore()).List("", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatal("captured factory created after publication")
	}
}
