package routing

import (
	"fmt"

	"github.com/gastownhall/gascity/internal/runtime"
)

// ObserveLivenessWithError preserves uncertainty from the selected backend.
// Missing bindings are observation failures, never evidence of absence.
func (p *Provider) ObserveLivenessWithError(name string, processNames []string) (runtime.Liveness, error) {
	b, err := p.route(name)
	if err != nil {
		return runtime.Liveness{}, fmt.Errorf("%w: %w", runtime.ErrRuntimeUnavailable, err)
	}
	return runtime.ObserveLivenessWithError(b, name, processNames)
}

// ObserveLiveness observes only the bound backend. Callers that may mutate
// lifecycle state must use ObserveLivenessWithError to preserve uncertainty.
func (p *Provider) ObserveLiveness(name string, processNames []string) runtime.Liveness {
	observation, _ := p.ObserveLivenessWithError(name, processNames)
	return observation
}

// IsDeadRuntimeSession accepts positive corpse evidence only from the owner.
func (p *Provider) IsDeadRuntimeSession(name string) (bool, error) {
	b, err := p.route(name)
	if err != nil {
		return false, fmt.Errorf("%w: %w", runtime.ErrRuntimeUnavailable, err)
	}
	if q, ok := b.(runtime.DeadRuntimeSessionChecker); ok {
		return q.IsDeadRuntimeSession(name)
	}
	return false, nil
}

// SnapshotIdle preserves an owner's point-in-time idle observation.
func (p *Provider) SnapshotIdle(name string) (bool, error) {
	b, err := p.route(name)
	if err != nil {
		return false, err
	}
	if q, ok := b.(runtime.IdleSnapshotProvider); ok {
		return q.SnapshotIdle(name)
	}
	return false, runtime.ErrInteractionUnsupported
}

// GetAllEnvironment reads environment only from the bound backend.
func (p *Provider) GetAllEnvironment(name string) (map[string]string, error) {
	b, err := p.route(name)
	if err != nil {
		return nil, err
	}
	if q, ok := b.(runtime.EnvironmentBatchProvider); ok {
		return q.GetAllEnvironment(name)
	}
	return nil, fmt.Errorf("runtime routing: bound backend does not support environment snapshots")
}

// SleepCapability returns the selected backend's policy capability; missing
// bindings and unsupported backends cannot authorize sleep.
func (p *Provider) SleepCapability(name string) runtime.SessionSleepCapability {
	b, err := p.route(name)
	if err != nil {
		return runtime.SessionSleepCapabilityDisabled
	}
	if q, ok := b.(runtime.SleepCapabilityProvider); ok {
		return q.SleepCapability(name)
	}
	return runtime.SessionSleepCapabilityDisabled
}
