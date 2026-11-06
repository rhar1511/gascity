package main

import (
	"fmt"
	"os"
	"sort"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/routing"
	"github.com/gastownhall/gascity/internal/session"
)

func configuredRuntimeBindingForCreate(cfg *config.City, sp runtime.Provider, template, named, transport string) (config.RuntimeBinding, error) {
	binder, strict := runtime.AsBackendBindingProvider(sp)
	if !strict {
		return config.RuntimeBinding{}, nil
	}
	base := sessionProviderContextForCity(cfg, "", os.Getenv("GC_SESSION")).providerName
	binding, err := config.ResolveSessionRuntimeBinding(cfg, base, template, named, transport)
	if err != nil {
		return config.RuntimeBinding{}, err
	}
	key, err := session.RuntimeBackendKey(binding.Name, binding.Fingerprint)
	if err != nil {
		return config.RuntimeBinding{}, err
	}
	if err := binder.ValidateBackend(key); err != nil {
		return config.RuntimeBinding{}, err
	}
	return binding, nil
}

// prospectiveRuntimeNameAvailable observes discovery without inventing a route.
// Unknown or conflicting discovery is uncertainty, not free capacity.
func prospectiveRuntimeNameAvailable(sp runtime.Provider, name string, binding config.RuntimeBinding) error {
	key, err := session.RuntimeBackendKey(binding.Name, binding.Fingerprint)
	if err != nil {
		return err
	}
	checker, ok := sp.(interface{ CheckProspectiveName(string, string) error })
	if !ok {
		return fmt.Errorf("%w: selected runtime availability check unavailable", runtime.ErrRuntimeUnavailable)
	}
	return checker.CheckProspectiveName(name, key)
}

func requiresSessionRuntimeRouting(cfg *config.City, snapshot *sessionBeadSnapshot) bool {
	if config.HasSessionRuntimeSelections(cfg) {
		return true
	}
	for _, info := range snapshot.OpenInfos() {
		if info.RuntimeBackend != "" || info.RuntimeFingerprint != "" {
			return true
		}
	}
	return false
}

func (cr *CityRuntime) runtimeOwnershipSnapshot() (*sessionBeadSnapshot, error) {
	store := cr.sessionsBeadStore()
	if store.Store == nil {
		return nil, fmt.Errorf("runtime ownership store unavailable")
	}
	infos, err := session.NewStore(store).ListRuntimeOwnershipInfos()
	if err != nil {
		return nil, err
	}
	return newSessionBeadSnapshotFromInfos(infos), nil
}

// rebuildSessionRuntimeRouting validates the entire registry and every retained
// owner before exposing a candidate. Existing declarations keep their provider
// instances. Discovery and current templates never supply missing provenance.
func rebuildSessionRuntimeRouting(ctx sessionProviderContext, snapshot *sessionBeadSnapshot, previous runtime.Provider) (runtime.Provider, error) {
	if err := snapshot.LoadError(); err != nil {
		return nil, fmt.Errorf("runtime ownership snapshot: %w", err)
	}
	if ctx.cfg == nil {
		return nil, fmt.Errorf("runtime routing requires city configuration")
	}
	baseName := ctx.providerName
	if baseName == "" {
		baseName = "tmux"
	}
	bindings := make(map[string]config.RuntimeBinding)
	add := func(binding config.RuntimeBinding, err error) error {
		if err != nil {
			return err
		}
		bindings[binding.Name] = binding
		return nil
	}
	if err := add(config.RuntimeBindingForName(ctx.cfg, baseName)); err != nil {
		return nil, err
	}
	for _, a := range ctx.cfg.Agents {
		if err := add(config.ResolveSessionRuntimeBinding(ctx.cfg, baseName, a.QualifiedName(), "", agentSessionCreateTransport(ctx.cfg, a))); err != nil {
			return nil, err
		}
	}
	for _, named := range ctx.cfg.NamedSessions {
		a := config.FindAgent(ctx.cfg, named.TemplateQualifiedName())
		if a == nil {
			return nil, fmt.Errorf("runtime routing: missing template %q", named.TemplateQualifiedName())
		}
		if err := add(config.ResolveSessionRuntimeBinding(ctx.cfg, baseName, a.QualifiedName(), named.QualifiedName(), agentSessionCreateTransport(ctx.cfg, *a))); err != nil {
			return nil, err
		}
	}
	// ACP may also be chosen by a coding-provider override on session creation.
	if hasACPProviderTargets(ctx.cfg) {
		if err := add(config.RuntimeBindingForName(ctx.cfg, "acp")); err != nil {
			return nil, err
		}
	}
	for _, info := range snapshot.OpenInfos() {
		if _, err := session.RuntimeBackendKey(info.RuntimeBackend, info.RuntimeFingerprint); err != nil {
			return nil, fmt.Errorf("runtime routing: session %q has no proven backend identity: %w", info.ID, err)
		}
		binding, err := config.RuntimeBindingForName(ctx.cfg, info.RuntimeBackend)
		if err != nil {
			return nil, err
		}
		if binding.Fingerprint != info.RuntimeFingerprint {
			return nil, fmt.Errorf("runtime routing: declaration changed for session %q backend %q", info.ID, info.RuntimeBackend)
		}
		bindings[binding.Name] = binding
	}
	registry, err := runtimeRegistryForCity(ctx.cfg)
	if err != nil {
		return nil, err
	}
	registry = registry.Clone()
	registry.SetFallback(nil)
	for name := range bindings {
		if !registry.CanResolve(name) {
			return nil, fmt.Errorf("runtime routing: unknown runtime %q", name)
		}
	}
	backends := make(map[string]runtime.Provider, len(bindings))
	names := make([]string, 0, len(bindings))
	for name := range bindings {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		binding := bindings[name]
		key, err := session.RuntimeBackendKey(name, binding.Fingerprint)
		if err != nil {
			return nil, err
		}
		if prior, ok := previous.(interface {
			BackendForKey(string) (runtime.Provider, bool)
		}); ok {
			if backend, exists := prior.BackendForKey(key); exists {
				backends[key] = backend
				continue
			}
		}
		backend, err := registry.New(name, ctx.sc, ctx.cityName, ctx.cityPath)
		if err != nil {
			return nil, err
		}
		backends[key] = backend
	}
	baseKey, _ := session.RuntimeBackendKey(baseName, bindings[baseName].Fingerprint)
	provider, err := routing.New(baseKey, backends)
	if err != nil {
		return nil, err
	}
	for _, info := range snapshot.OpenInfos() {
		key, _ := session.RuntimeBackendKey(info.RuntimeBackend, info.RuntimeFingerprint)
		if err := provider.Bind(info.SessionName, key); err != nil {
			return nil, err
		}
	}
	return provider, nil
}
