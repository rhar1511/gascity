//go:build linux

package main

import "testing"

func controllerPermitTestResolver(t *testing.T, entry hostBeadsPermitAuthorityEntry, key []byte) *hostBeadsPermitResolver {
	t.Helper()
	directory := t.TempDir()
	replaceHostBeadsPermitTestKey(t, &entry, key, true)
	writeHostBeadsPermitAuthority(t, directory, []hostBeadsPermitAuthorityEntry{entry})
	resolver, err := loadHostBeadsPermitResolver(directory)
	if err != nil {
		t.Fatalf("load host permit resolver: %v", err)
	}
	t.Cleanup(func() { _ = resolver.close() })
	return resolver
}
