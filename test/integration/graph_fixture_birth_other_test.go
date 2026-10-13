//go:build integration && !darwin

package integration

import "github.com/gastownhall/gascity/internal/beads/proxyendpoint"

func graphFixtureProcessBirth(pid int) (string, error) {
	return proxyendpoint.DefaultProcessTable().Birth(pid)
}
