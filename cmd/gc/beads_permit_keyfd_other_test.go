//go:build !linux

package main

import (
	"crypto/sha256"
	"testing"
)

func TestHostBeadsPermitNonLinuxSigningAuthorityFailsClosed(t *testing.T) {
	if _, err := receiveHostBeadsPermitPrivateKeyFD("unused", [sha256.Size]byte{}, "unused"); err == nil {
		t.Fatal("unsupported broker accepted")
	}
	if _, err := readHostBeadsPermitPrivateKeyFD(-1, 1024); err == nil {
		t.Fatal("unsupported sealed descriptor accepted")
	}
	if err := lockDownHostBeadsPermitProcessMemory(); err == nil {
		t.Fatal("unsupported process isolation accepted")
	}
}

// Probe the unavailable facility before skipping its positive fixtures.
// Ordinary in-memory controller fixtures retain their existing assertions.
func replaceHostBeadsPermitTestKey(t *testing.T, _ *hostBeadsPermitAuthorityEntry, _ []byte, _ bool) {
	t.Helper()
	if err := lockDownHostBeadsPermitProcessMemory(); err != nil {
		t.Skipf("sealed signing authority is unavailable: %v", err)
	}
	t.Error("unexpected non-Linux signing authority support")
}

func startHostBeadsPermitTestBroker(t *testing.T, _ string, _ []byte, _ []hostBeadsPermitAuthorityEntry) {
	t.Helper()
	if _, err := receiveHostBeadsPermitPrivateKeyFD("unused", [sha256.Size]byte{}, "unused"); err != nil {
		t.Skipf("signing authority broker is unavailable: %v", err)
	}
	t.Error("unexpected non-Linux signing authority support")
}
