//go:build linux

package main

import (
	"os"
	"testing"
)

func controllerPermitTestResolver(t *testing.T, entry hostBeadsPermitAuthorityEntry, key []byte) *hostBeadsPermitResolver {
	t.Helper()
	directory := t.TempDir()
	fixture := hostBeadsPermitBrokerFixture{hostBeadsPermitAuthorityEntry: entry}
	replaceHostBeadsPermitTestKey(t, &fixture, key, true)
	writeHostBeadsPermitAuthority(t, directory, []hostBeadsPermitBrokerFixture{fixture})
	resolver, err := loadHostBeadsPermitResolver(directory)
	if err != nil {
		t.Fatalf("load host permit resolver: %v", err)
	}
	t.Cleanup(func() { _ = resolver.close() })
	return resolver
}

func configureHostBeadsPermitBrokerTestOwnership() {
	hostBeadsPermitBrokerOwnerIsTrusted = hostBeadsPermitOwnerIsTrusted
	hostBeadsPermitBrokerUIDIsTrusted = func(ownerUID uint64) bool {
		return hostBeadsPermitOwnerUIDIsTrusted(ownerUID, os.Geteuid())
	}
}
