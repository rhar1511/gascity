package routing

import (
	"context"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// Start delegates only to the bound backend.
func (p *Provider) Start(ctx context.Context, name string, cfg runtime.Config) error {
	b, err := p.route(name)
	if err != nil {
		return err
	}
	return b.Start(ctx, name, cfg)
}

// Stop delegates only to the bound backend.
func (p *Provider) Stop(name string) error {
	b, err := p.route(name)
	if err != nil {
		return err
	}
	return b.Stop(name)
}

// Interrupt delegates only to the bound backend.
func (p *Provider) Interrupt(name string) error {
	b, err := p.route(name)
	if err != nil {
		return err
	}
	return b.Interrupt(name)
}

// IsRunning delegates only to the bound backend.
func (p *Provider) IsRunning(name string) bool {
	b, err := p.route(name)
	if err != nil {
		return false
	}
	return b.IsRunning(name)
}

// IsAttached delegates only to the bound backend.
func (p *Provider) IsAttached(name string) bool {
	b, err := p.route(name)
	if err != nil {
		return false
	}
	return b.IsAttached(name)
}

// Attach delegates only to the bound backend.
func (p *Provider) Attach(name string) error {
	b, err := p.route(name)
	if err != nil {
		return err
	}
	return b.Attach(name)
}

// ProcessAlive delegates only to the bound backend.
func (p *Provider) ProcessAlive(name string, processNames []string) bool {
	b, err := p.route(name)
	if err != nil {
		return false
	}
	return b.ProcessAlive(name, processNames)
}

// Nudge delegates only to the bound backend.
func (p *Provider) Nudge(name string, content []runtime.ContentBlock) error {
	b, err := p.route(name)
	if err != nil {
		return err
	}
	return b.Nudge(name, content)
}

// SetMeta delegates only to the bound backend.
func (p *Provider) SetMeta(name, key, value string) error {
	b, err := p.route(name)
	if err != nil {
		return err
	}
	return b.SetMeta(name, key, value)
}

// GetMeta delegates only to the bound backend.
func (p *Provider) GetMeta(name, key string) (string, error) {
	b, err := p.route(name)
	if err != nil {
		return "", err
	}
	return b.GetMeta(name, key)
}

// RemoveMeta delegates only to the bound backend.
func (p *Provider) RemoveMeta(name, key string) error {
	b, err := p.route(name)
	if err != nil {
		return err
	}
	return b.RemoveMeta(name, key)
}

// Peek delegates only to the bound backend.
func (p *Provider) Peek(name string, lines int) (string, error) {
	b, err := p.route(name)
	if err != nil {
		return "", err
	}
	return b.Peek(name, lines)
}

// GetLastActivity delegates only to the bound backend.
func (p *Provider) GetLastActivity(name string) (time.Time, error) {
	b, err := p.route(name)
	if err != nil {
		return time.Time{}, err
	}
	return b.GetLastActivity(name)
}

// ClearScrollback delegates only to the bound backend.
func (p *Provider) ClearScrollback(name string) error {
	b, err := p.route(name)
	if err != nil {
		return err
	}
	return b.ClearScrollback(name)
}

// CopyTo delegates only to the bound backend.
func (p *Provider) CopyTo(name, src, relDst string) error {
	b, err := p.route(name)
	if err != nil {
		return err
	}
	return b.CopyTo(name, src, relDst)
}

// SendKeys delegates only to the bound backend.
func (p *Provider) SendKeys(name string, keys ...string) error {
	b, err := p.route(name)
	if err != nil {
		return err
	}
	return b.SendKeys(name, keys...)
}

// RunLive delegates only to the bound backend.
func (p *Provider) RunLive(name string, cfg runtime.Config) error {
	b, err := p.route(name)
	if err != nil {
		return err
	}
	return b.RunLive(name, cfg)
}
