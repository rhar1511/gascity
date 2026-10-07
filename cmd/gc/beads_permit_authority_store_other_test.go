//go:build !linux

package main

import (
	"crypto/sha256"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// controllerPermitTestResolver exercises portable controller contracts with a
// test-owned signing fixture. Production broker and sealed descriptor support
// remain Linux-only. Root/document availability and signature checks stay real.
func controllerPermitTestResolver(t *testing.T, entry hostBeadsPermitAuthorityEntry, key []byte) *hostBeadsPermitResolver {
	t.Helper()
	if err := validateHostBeadsPermitEntry(entry); err != nil {
		t.Fatalf("validate test authority entry: %v", err)
	}
	directory, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve test authority directory: %v", err)
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	document := hostBeadsPermitAuthorityDocument{
		SchemaVersion:   hostBeadsPermitAuthoritySchemaV1,
		KeyBrokerSocket: filepath.Join(directory, "broker.sock"),
		Entries:         []hostBeadsPermitAuthorityEntry{entry},
	}
	data, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, hostBeadsPermitAuthorityFile), data, 0o600); err != nil {
		t.Fatal(err)
	}
	source, err := newHostBeadsPermitAuthoritySource(directory)
	if err != nil {
		t.Fatalf("open test authority source: %v", err)
	}
	source.documentDigest = sha256.Sum256(data)
	source.documentPinned = true
	var signer *hostBeadsPermitSigner
	loaded := false
	defer func() {
		if !loaded {
			if signer != nil {
				signer.destroy()
			}
			_ = source.close()
		}
	}()
	if err := source.verifyAuthorityAvailable(); err != nil {
		t.Fatalf("verify test authority availability: %v", err)
	}
	signer, _, err = parseHostBeadsPermitPrivateKey(key)
	if err != nil {
		t.Fatalf("parse test signing key: %v", err)
	}
	signer.authorityAvailable = source.verifyAuthorityAvailable
	issuer, err := beads.NewControllerBeadsPermitIssuer(beads.ControllerBeadsPermitIssuerConfig{
		Audience: entry.Audience, ProjectID: entry.ProjectID, Database: entry.Database,
		KeyID: entry.KeyID, Issuer: entry.Issuer,
		Lifetime: time.Duration(entry.PermitLifetimeSeconds) * time.Second, Signer: signer,
	})
	if err != nil {
		t.Fatalf("create test authority issuer: %v", err)
	}
	policy := hostBeadsPermitPolicy{
		CityName: entry.CityName, StoreRef: entry.StoreRef,
		Audience: entry.Audience, ProjectID: entry.ProjectID, Database: entry.Database,
		KeyID: entry.KeyID, Issuer: entry.Issuer, Actor: entry.Actor,
		ProtectionClass: entry.ProtectionClass,
		Lifetime:        time.Duration(entry.PermitLifetimeSeconds) * time.Second,
	}
	resolver := &hostBeadsPermitResolver{
		source: source,
		bindings: map[hostBeadsPermitScope]hostBeadsPermitBinding{
			{cityName: entry.CityName, storeRef: entry.StoreRef}: {issuer: issuer, signer: signer, policy: policy},
		},
	}
	t.Cleanup(func() { _ = resolver.close() })
	loaded = true
	return resolver
}

func TestHostBeadsPermitPlatformRefusesNativeAuthority(t *testing.T) {
	if err := lockDownHostBeadsPermitProcessMemory(); err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("non-Linux process isolation error = %v, want unsupported", err)
	}
	if fd, err := receiveHostBeadsPermitPrivateKeyFD("unused-test-socket", [sha256.Size]byte{}, "test-key"); fd != -1 || err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("non-Linux broker = (%d, %v), want unsupported", fd, err)
	}
	if data, err := readHostBeadsPermitPrivateKeyFD(-1, 1); data != nil || err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("non-Linux sealed key data/error = (%v, %v), want unsupported", data, err)
	}
}

func TestControllerPermitFixturePinsAuthorityDocument(t *testing.T) {
	entry, key := hostBeadsPermitTestEntry(t, "alpha", "city:alpha", "test-key", "")
	resolver := controllerPermitTestResolver(t, entry, key)
	issuer, _, ok := resolver.resolve(entry.CityName, entry.StoreRef)
	if !ok || issuer == nil {
		t.Fatal("portable controller fixture did not resolve exact authority")
	}
	request := beads.ControllerProtectedMutationRequest{
		Operation: "issue.revision_transition", ResourceIDs: []string{"test-record"},
		RequestDigest: strings.Repeat("0", 64),
	}
	if _, err := issuer.IssueProtectedMutation(request, "test-before-document-change"); err != nil {
		t.Fatalf("portable fixture could not sign before change: %v", err)
	}
	path := filepath.Join(resolver.source.directory, hostBeadsPermitAuthorityFile)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := issuer.IssueProtectedMutation(request, "test-after-document-change"); err == nil {
		t.Fatal("portable fixture signed after its pinned authority document changed")
	}
}
