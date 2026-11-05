// Package routing dispatches session operations through explicitly bound runtime
// identities. Discovery never assigns ownership and a missing route never falls
// back to a different backend.
package routing

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/gastownhall/gascity/internal/runtime"
)

// Provider retains immutable backend instances and exact session bindings.
// Keys are opaque identities supplied by the composition root; they may include
// a declaration digest. Bindings remain valid after Stop so repeated cleanup
// cannot reach a different backend.
type Provider struct {
	backends   map[string]runtime.Provider
	keys       []string
	mu         sync.RWMutex
	bindings   map[string]string
	creationMu sync.RWMutex
	retired    bool
}

var (
	_ runtime.Provider                  = (*Provider)(nil)
	_ runtime.BackendListingProvider    = (*Provider)(nil)
	_ runtime.ListingAttestation        = (*Provider)(nil)
	_ runtime.LivenessObserverWithError = (*Provider)(nil)
)

// New copies the backend registry and orders the default backend first for
// discovery. Even default-backed sessions require an explicit Bind.
func New(defaultKey string, backends map[string]runtime.Provider) (*Provider, error) {
	if defaultKey == "" || backends[defaultKey] == nil {
		return nil, fmt.Errorf("runtime routing: default backend %q is missing", defaultKey)
	}
	p := &Provider{backends: make(map[string]runtime.Provider, len(backends)), bindings: make(map[string]string)}
	for key, backend := range backends {
		if key == "" || strings.TrimSpace(key) != key || backend == nil {
			return nil, fmt.Errorf("runtime routing: invalid backend %q", key)
		}
		p.backends[key] = backend
		if key != defaultKey {
			p.keys = append(p.keys, key)
		}
	}
	sort.Strings(p.keys)
	p.keys = append([]string{defaultKey}, p.keys...)
	return p, nil
}

// Bind associates a runtime session name with its persisted backend identity.
// Repeating the same binding is idempotent; migration and unknown keys refuse.
func (p *Provider) Bind(name, key string) error {
	if name == "" || strings.TrimSpace(name) != name || p.backends[key] == nil {
		return fmt.Errorf("runtime routing: invalid binding %q to %q", name, key)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if prior, exists := p.bindings[name]; exists && prior != key {
		return fmt.Errorf("runtime routing: session %q is already bound to %q", name, prior)
	}
	p.bindings[name] = key
	return nil
}

// ValidateBackend checks a resolved identity before a caller creates a session.
func (p *Provider) ValidateBackend(key string) error {
	if p.backends[key] == nil {
		return fmt.Errorf("runtime routing: unknown backend %q", key)
	}
	return nil
}

// WithBackendCreation serializes a creator captured under this registry with
// its publication lifetime. Existing-owner operations remain valid when retired.
func (p *Provider) WithBackendCreation(create func() error) error {
	p.creationMu.RLock()
	defer p.creationMu.RUnlock()
	if p.retired {
		return fmt.Errorf("%w: runtime registry replaced; resolve current configuration", runtime.ErrRuntimeUnavailable)
	}
	return create()
}

// WithRoutingReplacement waits for existing creators, then publishes while new
// creators are fenced. A rejected candidate leaves this registry usable.
func (p *Provider) WithRoutingReplacement(publish func() (bool, error)) error {
	p.creationMu.Lock()
	defer p.creationMu.Unlock()
	if p.retired {
		return fmt.Errorf("%w: runtime registry already replaced", runtime.ErrRuntimeUnavailable)
	}
	accepted, err := publish()
	if accepted && err == nil {
		p.retired = true
	}
	return err
}

// BackendForKey returns an immutable registry entry for composition-time reuse.
// It does not infer a session's owner or grant permission to operate on it.
func (p *Provider) BackendForKey(key string) (runtime.Provider, bool) {
	backend, ok := p.backends[key]
	return backend, ok
}

// SupportsTransport reports aggregate creation capability. Exact binding and
// configuration validation still select the only backend a session may use.
func (p *Provider) SupportsTransport(transport string) bool {
	if transport != "acp" {
		return true
	}
	for _, backend := range p.backends {
		if capability, ok := backend.(runtime.TransportCapabilityProvider); ok && capability.SupportsTransport(transport) {
			return true
		}
	}
	return false
}

// BoundBackend returns the immutable identity of an already-bound session.
// It does not consult configuration or infer ownership from discovery.
func (p *Provider) BoundBackend(name string) (string, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	key, exists := p.bindings[name]
	if !exists {
		return "", fmt.Errorf("runtime routing: session %q has no backend binding", name)
	}
	return key, nil
}

func (p *Provider) route(name string) (runtime.Provider, error) {
	p.mu.RLock()
	key, bound := p.bindings[name]
	p.mu.RUnlock()
	if !bound {
		return nil, fmt.Errorf("runtime routing: session %q has no backend binding", name)
	}
	return p.backends[key], nil
}

// ListRunningByBackend retains backend attribution and reports unknown,
// conflicting and duplicate runtime names as degraded observations. It never
// turns discovery into a binding or hides usable partial results.
func (p *Provider) ListRunningByBackend(prefix string) []runtime.BackendListing {
	p.mu.RLock()
	bindings := make(map[string]string, len(p.bindings))
	for name, key := range p.bindings {
		bindings[name] = key
	}
	p.mu.RUnlock()
	listings := make([]runtime.BackendListing, 0, len(p.keys))
	seen := make(map[string]string)
	for _, key := range p.keys {
		backend := p.backends[key]
		names, err := backend.ListRunning(prefix)
		for _, name := range names {
			if owner, exists := bindings[name]; !exists || owner != key {
				err = errors.Join(err, fmt.Errorf("runtime routing: discovered session %q on %q without matching ownership", name, key))
			}
			if prior, duplicate := seen[name]; duplicate {
				err = errors.Join(err, fmt.Errorf("runtime routing: duplicate session %q on %q and %q", name, prior, key))
			}
			seen[name] = key
		}
		listings = append(listings, runtime.BackendListing{Label: key, Provider: backend, Names: names, Err: err})
	}
	return listings
}

// ListRunning returns the attributed union, preserving partial-list errors.
func (p *Provider) ListRunning(prefix string) ([]string, error) {
	return runtime.MergeBackendListings(p.ListRunningByBackend(prefix))
}

// ListRunningComplete requires every backend to attest its listing.
func (p *Provider) ListRunningComplete() bool {
	for _, key := range p.keys {
		if !runtime.ListRunningAttested(p.backends[key]) {
			return false
		}
	}
	return true
}

// Capabilities intersects abilities and unions the claim-backstop requirement.
func (p *Provider) Capabilities() runtime.ProviderCapabilities {
	c := p.backends[p.keys[0]].Capabilities()
	for _, key := range p.keys[1:] {
		next := p.backends[key].Capabilities()
		c.CanReportAttachment = c.CanReportAttachment && next.CanReportAttachment
		c.CanReportActivity = c.CanReportActivity && next.CanReportActivity
		c.CanStream = c.CanStream && next.CanStream
		c.CanAttachTTY = c.CanAttachTTY && next.CanAttachTTY
		c.NeedsClaimBackstop = c.NeedsClaimBackstop || next.NeedsClaimBackstop
	}
	return c
}

// CheckProspectiveName checks a new owner's selected namespace without binding
// it. Global discovery remains conservative for reaping and quiescence; missing
// sibling inventories cannot retarget or block an independently selected owner.
func (p *Provider) CheckProspectiveName(name, key string) error {
	if name == "" || strings.TrimSpace(name) != name {
		return fmt.Errorf("runtime routing: invalid prospective name %q", name)
	}
	if err := p.ValidateBackend(key); err != nil {
		return err
	}
	observed, err := runtime.ObserveLivenessWithError(p.backends[key], name, nil)
	if err != nil {
		return fmt.Errorf("%w: prospective runtime %q: %w", runtime.ErrRuntimeUnavailable, name, err)
	}
	if observed.Running {
		return fmt.Errorf("%w: runtime %q", runtime.ErrSessionExists, name)
	}
	return nil
}
