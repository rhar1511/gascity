package routing

import (
	"context"

	"github.com/gastownhall/gascity/internal/runtime"
)

// Exec preserves the selected backend's command result. Unsupported execution
// never reaches another backend, even if it supports the requested operation.
func (p *Provider) Exec(ctx context.Context, name string, argv []string) ([]byte, int, error) {
	b, err := p.route(name)
	if err != nil {
		return nil, 0, err
	}
	if executor, ok := b.(runtime.ExecProvider); ok {
		return executor.Exec(ctx, name, argv)
	}
	return nil, 0, runtime.ErrExecUnsupported
}
