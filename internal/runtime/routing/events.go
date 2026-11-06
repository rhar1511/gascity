package routing

import (
	"context"

	"github.com/gastownhall/gascity/internal/runtime"
)

// SubscribeSessionEvents merges each backend's level-triggered hints. Events
// do not bind ownership; consumers must re-observe through the exact route.
func (p *Provider) SubscribeSessionEvents(ctx context.Context) (<-chan runtime.SessionEvent, error) {
	var sources []runtime.SessionEventSource
	for _, key := range p.keys {
		sources = append(sources, runtime.SessionEventSource{Name: key, Provider: p.backends[key]})
	}
	return runtime.SubscribeSessionEventSources(ctx, sources...)
}
