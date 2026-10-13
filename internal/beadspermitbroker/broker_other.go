//go:build !linux

// Package beadspermitbroker provides a fail-closed stub where Linux pidfds
// and Unix peer credentials are unavailable.
package beadspermitbroker

import (
	"context"
	"errors"
)

var errUnsupported = errors.New("protected Beads authority broker requires Linux pidfds")

// Config describes the broker authority and controller identity.
type Config struct {
	AuthorityDirectory  string
	PrivateKeyDirectory string
	ControllerGID       uint32
}

// LaunchConfig describes the controller launch requested by the host.
type LaunchConfig struct {
	AuthorityDirectory        string
	PrivateKeyDirectory       string
	ControllerEnvironmentFile string
	GCExecutable              string
	ControllerPath            string
	ControllerUID             uint32
	ControllerGID             uint32
}

// Broker is unavailable on platforms without Linux pidfds.
type Broker struct{}

// New refuses broker construction on unsupported platforms.
func New(Config) (*Broker, error) { return nil, errUnsupported }

// RegisterLaunch refuses registration without Linux process identity checks.
func (*Broker) RegisterLaunch(int, uint32, string, []string) error { return errUnsupported }

// Serve refuses to serve an unsupported broker.
func (*Broker) Serve(context.Context) error { return errUnsupported }

// Close has no resources to release on unsupported platforms.
func (*Broker) Close() error { return nil }

// LaunchSupervisor refuses controller startup on unsupported platforms.
func LaunchSupervisor(LaunchConfig) int { return 1 }

// ExecChild refuses the private launch protocol on unsupported platforms.
func ExecChild() int { return 1 }
