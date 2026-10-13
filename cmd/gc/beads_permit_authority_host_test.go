package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"io"
	"os"
	"strings"
	"testing"
)

func TestMainExitCodeRefusesConfiguredAuthorityWithoutProcessIsolation(t *testing.T) {
	previousLock := hostBeadsPermitLockProcessMemory
	hostBeadsPermitLockProcessMemory = func() error { return os.ErrPermission }
	t.Cleanup(func() { hostBeadsPermitLockProcessMemory = previousLock })
	t.Setenv(hostBeadsPermitAuthorityDirEnv, "/run/gc/protected-authority")
	var stderr bytes.Buffer
	if code := mainExitCode([]string{"version"}, io.Discard, &stderr); code != 1 {
		t.Fatalf("mainExitCode = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "protect configured Beads signing authority process") {
		t.Fatalf("stderr = %q, want process-isolation failure", stderr.String())
	}
}

func TestSupervisorStartRefusesConfiguredAuthorityWithoutRootBrokerLaunch(t *testing.T) {
	t.Setenv(hostBeadsPermitAuthorityDirEnv, "/run/gc/protected-authority")
	var stdout, stderr bytes.Buffer
	if code := doSupervisorStart(&stdout, &stderr); code != 1 {
		t.Fatalf("doSupervisorStart = %d, want 1", code)
	}
	if stdout.Len() != 0 || !strings.Contains(stderr.String(), "requires a root broker launcher") {
		t.Fatalf("stdout = %q, stderr = %q", stdout.String(), stderr.String())
	}
}

func TestHostBeadsPermitOwnerUIDTrust(t *testing.T) {
	tests := []struct {
		name        string
		ownerUID    uint64
		effectiveID int
		want        bool
	}{
		{name: "effective uid", ownerUID: 1001, effectiveID: 1001, want: true},
		{name: "root owner", ownerUID: 0, effectiveID: 1001, want: true},
		{name: "untrusted uid", ownerUID: 1002, effectiveID: 1001},
		{name: "unknown effective uid", ownerUID: 1001, effectiveID: -1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := hostBeadsPermitOwnerUIDIsTrusted(test.ownerUID, test.effectiveID); got != test.want {
				t.Fatalf("owner UID %d with effective UID %d trusted = %t, want %t", test.ownerUID, test.effectiveID, got, test.want)
			}
		})
	}
}

func TestHostBeadsPermitAuthorityPolicyRequiresRootOwner(t *testing.T) {
	if !hostBeadsPermitOwnerUIDIsRoot(0) {
		t.Fatal("root UID was not trusted for authority policy")
	}
	if hostBeadsPermitOwnerUIDIsRoot(uint64(os.Geteuid())) && os.Geteuid() != 0 {
		t.Fatal("non-root effective UID was trusted for authority policy")
	}
}

func TestHostBeadsPermitResolverEnvironmentSelection(t *testing.T) {
	previousLock := hostBeadsPermitLockProcessMemory
	hostBeadsPermitLockProcessMemory = func() error { return nil }
	t.Cleanup(func() { hostBeadsPermitLockProcessMemory = previousLock })
	// Getenv deliberately makes absent and empty-masked authority equivalent.
	// Setenv owns restoration without hand-written process-environment cleanup.
	t.Setenv(hostBeadsPermitAuthorityDirEnv, "")
	if resolver, err := loadHostBeadsPermitResolverFromEnv(); err != nil || resolver != nil {
		t.Fatalf("empty masked env = (%v, %v), want absent authority", resolver, err)
	}
	t.Setenv(hostBeadsPermitAuthorityDirEnv, "relative/authority")
	if resolver, err := loadHostBeadsPermitResolverFromEnv(); err == nil || resolver != nil {
		t.Fatalf("configured relative env = (%v, %v), want an error", resolver, err)
	}
	hostBeadsPermitLockProcessMemory = func() error { return os.ErrPermission }
	t.Setenv(hostBeadsPermitAuthorityDirEnv, t.TempDir())
	if resolver, err := loadHostBeadsPermitResolverFromEnv(); err == nil || resolver != nil || !strings.Contains(err.Error(), "protect configured Beads signing authority process") {
		t.Fatalf("failed process isolation = (%v, %v), want an error", resolver, err)
	}
}

func hostBeadsPermitTestEntry(t *testing.T, cityName, storeRef, keyID, _ string) (hostBeadsPermitAuthorityEntry, []byte) {
	t.Helper()
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pkcs8, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	encoded := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: pkcs8})
	entry := hostBeadsPermitAuthorityEntry{
		CityName: cityName, StoreRef: storeRef,
		Audience: "gascity-controller", ProjectID: "project-" + cityName, Database: "db-" + cityName,
		KeyID: keyID, Issuer: "controller-host", Actor: "controller-service",
		ProtectionClass: "protected-records", PermitLifetimeSeconds: 90,
		PrivateKeyHandle: keyID + "-handle",
	}
	return entry, encoded
}
