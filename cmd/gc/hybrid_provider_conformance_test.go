package main

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/runtimetest"
	sessiontmux "github.com/gastownhall/gascity/internal/runtime/tmux"
)

func TestHybridLocalConformance(t *testing.T) {
	runtimetest.RunProviderTests(t, func(_ *testing.T) (runtime.Provider, error) {
		return newHybridProvider(config.SessionConfig{}, "test-city", "", hybridBackendFactories{
			local:  func(sessiontmux.Config) runtime.Provider { return runtime.NewFake() },
			remote: func() (runtime.Provider, error) { return runtime.NewFake(), nil },
		})
	})
}

func TestHybridRemoteConformance(t *testing.T) {
	runtimetest.RunProviderTests(t, func(_ *testing.T) (runtime.Provider, error) {
		return newHybridProvider(config.SessionConfig{RemoteMatch: "conformance"}, "test-city", "", hybridBackendFactories{
			local:  func(sessiontmux.Config) runtime.Provider { return runtime.NewFake() },
			remote: func() (runtime.Provider, error) { return runtime.NewFake(), nil },
		})
	})
}

func TestHybridFactoryPreservesConfigurationAndRouting(t *testing.T) {
	for _, tc := range []struct {
		name, pattern, override, session string
		remote                           bool
	}{
		{"empty local", "", "", "remote-task", false},
		{"configured remote", "remote", "", "remote-task", true},
		{"environment remote", "local", "remote", "remote-task", true},
		{"environment replaces config", "remote", "other", "remote-task", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GC_HYBRID_REMOTE_MATCH", tc.override)
			local, remote := runtime.NewFake(), runtime.NewFake()
			var received sessiontmux.Config
			cfg := config.SessionConfig{RemoteMatch: tc.pattern}
			p, err := newHybridProvider(cfg, "test-city", "/tmp/hybrid-fixture", hybridBackendFactories{
				local:  func(cfg sessiontmux.Config) runtime.Provider { received = cfg; return local },
				remote: func() (runtime.Provider, error) { return remote, nil },
			})
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(received, tmuxConfigFromSession(cfg, "test-city", "/tmp/hybrid-fixture")) {
				t.Fatal("local constructor lost session/city configuration")
			}
			if err := p.Start(context.Background(), tc.session, runtime.Config{}); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = p.Stop(tc.session) })
			if remote.IsRunning(tc.session) != tc.remote || local.IsRunning(tc.session) == tc.remote {
				t.Fatal("session did not reach exactly the selected backend")
			}
			if err := p.Stop(tc.session); err != nil {
				t.Fatal(err)
			}
			if remote.IsRunning(tc.session) || local.IsRunning(tc.session) {
				t.Fatal("owned session remained after Stop")
			}
		})
	}
}

func TestHybridFactoryRetainsRemoteConstructionError(t *testing.T) {
	want := errors.New("remote construction failed")
	p, err := newHybridProvider(config.SessionConfig{}, "test-city", "", hybridBackendFactories{
		local:  func(sessiontmux.Config) runtime.Provider { return runtime.NewFake() },
		remote: func() (runtime.Provider, error) { return nil, want },
	})
	if p != nil || !errors.Is(err, want) || !strings.Contains(err.Error(), "hybrid: k8s backend:") {
		t.Fatalf("construction result = %v, %v", p, err)
	}
}
