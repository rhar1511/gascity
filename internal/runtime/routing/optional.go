package routing

import (
	"context"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// WaitForIdle preserves the selected backend's optional capability.
func (p *Provider) WaitForIdle(ctx context.Context, name string, timeout time.Duration) error {
	b, err := p.route(name)
	if err != nil {
		return err
	}
	if q, ok := b.(runtime.IdleWaitProvider); ok {
		return q.WaitForIdle(ctx, name, timeout)
	}
	return runtime.ErrInteractionUnsupported
}

// Relaunch preserves the selected backend's optional capability.
func (p *Provider) Relaunch(ctx context.Context, name string, cfg runtime.Config) error {
	b, err := p.route(name)
	if err != nil {
		return err
	}
	if q, ok := b.(runtime.RelaunchProvider); ok {
		return q.Relaunch(ctx, name, cfg)
	}
	return runtime.ErrRelaunchUnsupported
}

// DismissKnownDialogs preserves the selected backend's optional capability.
func (p *Provider) DismissKnownDialogs(ctx context.Context, name string, timeout time.Duration) error {
	b, err := p.route(name)
	if err != nil {
		return err
	}
	if q, ok := b.(runtime.DialogProvider); ok {
		return q.DismissKnownDialogs(ctx, name, timeout)
	}
	return runtime.ErrInteractionUnsupported
}

// ResetInterruptedTurn preserves the selected backend's optional capability.
func (p *Provider) ResetInterruptedTurn(ctx context.Context, name string) error {
	b, err := p.route(name)
	if err != nil {
		return err
	}
	if q, ok := b.(runtime.InterruptedTurnResetProvider); ok {
		return q.ResetInterruptedTurn(ctx, name)
	}
	return runtime.ErrInteractionUnsupported
}

// WaitForInterruptBoundary preserves the selected backend's optional capability.
func (p *Provider) WaitForInterruptBoundary(ctx context.Context, name string, since time.Time, timeout time.Duration) error {
	b, err := p.route(name)
	if err != nil {
		return err
	}
	if q, ok := b.(runtime.InterruptBoundaryWaitProvider); ok {
		return q.WaitForInterruptBoundary(ctx, name, since, timeout)
	}
	return runtime.ErrInteractionUnsupported
}

// NudgeNow preserves the selected backend's optional capability.
func (p *Provider) NudgeNow(name string, content []runtime.ContentBlock) error {
	b, err := p.route(name)
	if err != nil {
		return err
	}
	if q, ok := b.(runtime.ImmediateNudgeProvider); ok {
		return q.NudgeNow(name, content)
	}
	return b.Nudge(name, content)
}

// Pending preserves the selected backend's optional capability.
func (p *Provider) Pending(name string) (*runtime.PendingInteraction, error) {
	b, err := p.route(name)
	if err != nil {
		return nil, err
	}
	if q, ok := b.(runtime.InteractionProvider); ok {
		return q.Pending(name)
	}
	return nil, runtime.ErrInteractionUnsupported
}

// Respond preserves the selected backend's optional capability.
func (p *Provider) Respond(name string, response runtime.InteractionResponse) error {
	b, err := p.route(name)
	if err != nil {
		return err
	}
	if q, ok := b.(runtime.InteractionProvider); ok {
		return q.Respond(name, response)
	}
	return runtime.ErrInteractionUnsupported
}
