package api

import (
	"fmt"
	"reflect"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worker"
)

func (s *Server) workerFactory(store beads.Store) (*worker.Factory, error) {
	cfg, sp := s.state.SessionRuntimeSnapshot()
	return s.workerFactoryForRuntimeSnapshot(store, cfg, sp)
}

func (s *Server) workerFactoryForRuntimeSnapshot(store beads.Store, cfg *config.City, sp runtime.Provider) (*worker.Factory, error) {
	var resolveTransport func(template, provider string) string
	if cfg != nil {
		resolveTransport = func(template, provider string) string {
			return configuredSessionTransport(cfg, template, provider)
		}
	}
	resolver := s.runtimeBackendResolverForSnapshot(cfg, sp)
	resolveRuntime := s.resolveWorkerSessionRuntimeWithMetadata
	if _, strict := runtime.AsBackendBindingProvider(sp); strict {
		resolveRuntime = func(info session.Info, kind string, metadata map[string]string) (*worker.ResolvedRuntime, error) {
			if !s.runtimeSnapshotCurrent(cfg, sp) {
				return nil, fmt.Errorf("%w: captured runtime configuration was replaced", runtime.ErrRuntimeUnavailable)
			}
			resolved, err := s.resolveWorkerSessionRuntimeWithMetadata(info, kind, metadata)
			if err != nil {
				return nil, err
			}
			if !s.runtimeSnapshotCurrent(cfg, sp) {
				return nil, fmt.Errorf("%w: runtime configuration changed during resolution", runtime.ErrRuntimeUnavailable)
			}
			return resolved, nil
		}
	}
	paths := s.sessionLogSearchPaths
	if paths == nil {
		if cfg == nil {
			paths = worker.DefaultSearchPaths()
		} else {
			paths = worker.MergeSearchPaths(cfg.Daemon.ObservePaths)
		}
	}
	return worker.NewFactory(worker.FactoryConfig{
		Store:                 store,
		Provider:              sp,
		CityPath:              s.state.CityPath(),
		SearchPaths:           paths,
		Recorder:              s.state.EventProvider(),
		UsageSink:             s.state.UsageSink(),
		ResolveTransport:      resolveTransport,
		ResolveRuntimeBackend: resolver,
		ResolveSessionRuntime: resolveRuntime,
		Pricing:               cfg.PricingRegistry(),
		ActivityMemo:          s.activityMemo,
	})
}

func (s *Server) workerSessionCatalog(store beads.Store) (*worker.SessionCatalog, error) {
	factory, err := s.workerFactory(store)
	if err != nil {
		return nil, err
	}
	return factory.Catalog()
}

// runtimeSnapshotCurrent rejects materialization that crossed a publication.
// Configuration snapshots are immutable; provider identity remains exact too.
func (s *Server) runtimeSnapshotCurrent(cfg *config.City, sp runtime.Provider) bool {
	currentCfg, currentSP := s.state.SessionRuntimeSnapshot()
	a, b := reflect.ValueOf(sp), reflect.ValueOf(currentSP)
	return currentCfg == cfg && a.IsValid() && b.IsValid() && a.Comparable() && b.Comparable() && a.Equal(b)
}

func (s *Server) runtimeBackendResolverForSnapshot(cfg *config.City, sp runtime.Provider) session.RuntimeBackendResolver {
	resolver := session.RuntimeBackendResolverForConfig(cfg, effectiveRuntimeBase(cfg), sp)
	if resolver == nil {
		return nil
	}
	return func(spec session.CreateOptions) (string, string, error) {
		if !s.runtimeSnapshotCurrent(cfg, sp) {
			return "", "", fmt.Errorf("%w: configuration changed; retry with the current runtime registry", runtime.ErrRuntimeUnavailable)
		}
		return resolver(spec)
	}
}

func (s *Server) newResolvedWorkerSessionHandleForRuntimeSnapshot(store beads.Store, resolved worker.ResolvedSessionConfig, cfg *config.City, sp runtime.Provider) (worker.Handle, error) {
	factory, err := s.workerFactoryForRuntimeSnapshot(store, cfg, sp)
	if err != nil {
		return nil, err
	}
	return factory.SessionForResolvedRuntime(resolved)
}
