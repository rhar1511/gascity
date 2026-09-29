//go:build !linux

// Package beadspermitbroker provides a fail-closed stub where Linux pidfds
// and Unix peer credentials are unavailable.
package beadspermitbroker

import (
	"context"
	"errors"
)

var errUnsupported = errors.New("protected Beads authority broker requires Linux pidfds")

type Config struct {
	AuthorityDirectory  string
	PrivateKeyDirectory string
	ControllerGID       uint32
}

type LaunchConfig struct {
	AuthorityDirectory        string
	PrivateKeyDirectory       string
	ControllerEnvironmentFile string
	GCExecutable              string
	ControllerPath            string
	ControllerUID             uint32
	ControllerGID             uint32
}

type Broker struct{}

func New(Config) (*Broker, error) { return nil, errUnsupported }

func (*Broker) RegisterLaunch(int, uint32, string, []string) error { return errUnsupported }

func (*Broker) Serve(context.Context) error { return errUnsupported }

func (*Broker) Close() error { return nil }

func LaunchSupervisor(LaunchConfig) int { return 1 }

func ExecChild() int { return 1 }
