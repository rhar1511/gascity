package main

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"golang.org/x/sys/unix"
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

func TestHostBeadsPermitResolverLoadsDistinctExactScopes(t *testing.T) {
	requireHostBeadsPermitOwnerSupport(t)
	directory := t.TempDir()
	firstEntry, firstKey := hostBeadsPermitTestEntry(t, "alpha", "city:alpha", "key-alpha", "alpha-key.pem")
	secondEntry, secondKey := hostBeadsPermitTestEntry(t, "beta", "rig:workers", "key-beta", "beta-key.pem")
	_ = firstKey
	_ = secondKey
	writeHostBeadsPermitAuthority(t, directory, []hostBeadsPermitAuthorityEntry{firstEntry, secondEntry})

	resolver, err := loadHostBeadsPermitResolver(directory)
	if err != nil {
		t.Fatalf("loadHostBeadsPermitResolver: %v", err)
	}
	t.Cleanup(func() { _ = resolver.close() })

	firstIssuer, firstPolicy, ok := resolver.resolve("alpha", "city:alpha")
	if !ok || firstIssuer == nil {
		t.Fatal("first exact scope did not resolve")
	}
	if firstPolicy.CityName != "alpha" || firstPolicy.StoreRef != "city:alpha" || firstPolicy.Actor != firstEntry.Actor ||
		firstPolicy.ProtectionClass != firstEntry.ProtectionClass || firstPolicy.KeyID != "key-alpha" {
		t.Fatalf("first trusted policy = %+v", firstPolicy)
	}
	secondIssuer, secondPolicy, ok := resolver.resolve("beta", "rig:workers")
	if !ok || secondIssuer == nil || firstIssuer == secondIssuer {
		t.Fatal("second exact scope did not resolve to its distinct issuer")
	}
	if secondPolicy.CityName != "beta" || secondPolicy.StoreRef != "rig:workers" || secondPolicy.KeyID != "key-beta" {
		t.Fatalf("second trusted policy = %+v", secondPolicy)
	}
	if _, _, ok := resolver.resolve("alpha", "rig:workers"); ok {
		t.Fatal("resolver accepted a cross-scope lookup")
	}
	if _, _, ok := resolver.resolve(" alpha", "city:alpha"); ok {
		t.Fatal("resolver normalized non-exact city input")
	}

	firstPolicy.Actor = "caller-mutated-copy"
	_, policyAgain, ok := resolver.resolve("alpha", "city:alpha")
	if !ok || policyAgain.Actor != firstEntry.Actor {
		t.Fatal("caller mutation changed the resolver's trusted policy")
	}
	token, err := firstIssuer.IssueProtectedMutation(beads.ControllerProtectedMutationRequest{
		Operation: "issue.revision_transition", ResourceIDs: []string{"record-1"},
		RequestDigest: strings.Repeat("0", 64),
	}, "replay-alpha-0001")
	if err != nil || token == "" {
		t.Fatalf("loaded issuer did not sign a protected mutation: token empty=%t, err=%v", token == "", err)
	}
	payloadPart, _, found := strings.Cut(token, ".")
	if !found {
		t.Fatal("loaded issuer returned a malformed permit token")
	}
	payload, err := base64.RawURLEncoding.DecodeString(payloadPart)
	if err != nil {
		t.Fatalf("decode loaded issuer claims: %v", err)
	}
	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil {
		t.Fatalf("decode loaded issuer claims JSON: %v", err)
	}
	for claim, want := range map[string]string{
		"audience": firstEntry.Audience, "project_id": firstEntry.ProjectID,
		"database": firstEntry.Database, "key_id": firstEntry.KeyID, "issuer": firstEntry.Issuer,
	} {
		if got, _ := claims[claim].(string); got != want {
			t.Fatalf("permit claim %s = %q, want %q", claim, got, want)
		}
	}
}

func TestHostBeadsPermitResolverCloseDestroysSigner(t *testing.T) {
	requireHostBeadsPermitOwnerSupport(t)
	directory := t.TempDir()
	entry, key := hostBeadsPermitTestEntry(t, "alpha", "city:alpha", "key-alpha", "signing-key.pem")
	_ = key
	writeHostBeadsPermitAuthority(t, directory, []hostBeadsPermitAuthorityEntry{entry})
	resolver, err := loadHostBeadsPermitResolver(directory)
	if err != nil {
		t.Fatalf("loadHostBeadsPermitResolver: %v", err)
	}
	binding := resolver.bindings[hostBeadsPermitScope{cityName: entry.CityName, storeRef: entry.StoreRef}]
	privateKeyBytes := binding.signer.privateKey
	if len(privateKeyBytes) != ed25519.PrivateKeySize {
		t.Fatalf("loaded signer private-key length = %d", len(privateKeyBytes))
	}
	if err := resolver.close(); err != nil {
		t.Fatalf("resolver.close: %v", err)
	}
	for index, value := range privateKeyBytes {
		if value != 0 {
			t.Fatalf("private-key byte %d was not cleared", index)
		}
	}
	if _, err := binding.signer.Sign(rand.Reader, []byte("after-close"), crypto.Hash(0)); err == nil {
		t.Fatal("destroyed signer still signed")
	}
}

func requireHostBeadsPermitOwnerSupport(t *testing.T) {
	t.Helper()
	info, err := os.Stat(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if !hostBeadsPermitOwnerIsTrusted(info) {
		t.Skip("this platform cannot verify authority-file owner UIDs and fails closed")
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
	t.Setenv(hostBeadsPermitAuthorityDirEnv, hostBeadsPermitTestDirectory(t))
	if resolver, err := loadHostBeadsPermitResolverFromEnv(); err == nil || resolver != nil {
		t.Fatalf("failed process isolation = (%v, %v), want an error", resolver, err)
	}
}

func TestHostBeadsPermitResolverRejectsUnsafeDirectoryAndFiles(t *testing.T) {
	t.Run("group writable directory", func(t *testing.T) {
		directory := hostBeadsPermitTestDirectory(t)
		if err := os.Chmod(directory, 0o770); err != nil {
			t.Fatal(err)
		}
		if _, err := loadHostBeadsPermitResolver(directory); err == nil {
			t.Fatal("group-writable directory was accepted")
		}
	})

	t.Run("world writable authority file", func(t *testing.T) {
		directory := hostBeadsPermitTestDirectory(t)
		if err := os.Chmod(filepath.Join(directory, hostBeadsPermitAuthorityFile), 0o666); err != nil {
			t.Fatal(err)
		}
		if _, err := loadHostBeadsPermitResolver(directory); err == nil {
			t.Fatal("world-writable authority file was accepted")
		}
	})

	t.Run("symlinked authority directory", func(t *testing.T) {
		realDirectory := hostBeadsPermitTestDirectory(t)
		link := filepath.Join(t.TempDir(), "authority-link")
		if err := os.Symlink(realDirectory, link); err != nil {
			t.Fatal(err)
		}
		if _, err := loadHostBeadsPermitResolver(link); err == nil {
			t.Fatal("symlinked authority directory was accepted")
		}
	})

	t.Run("named key file descriptor", func(t *testing.T) {
		directory := t.TempDir()
		entry, key := hostBeadsPermitTestEntry(t, "alpha", "city:alpha", "key-alpha", "")
		path := filepath.Join(t.TempDir(), "named-key.pem")
		if err := os.WriteFile(path, key, 0o600); err != nil {
			t.Fatal(err)
		}
		fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC, 0)
		if err != nil {
			t.Fatal(err)
		}
		entry.privateKeyFD = fd
		writeHostBeadsPermitDocument(t, directory, []hostBeadsPermitAuthorityEntry{entry})
		if _, err := loadHostBeadsPermitResolver(directory); err == nil {
			t.Fatal("named private-key file descriptor was accepted")
		}
	})

	t.Run("unsealed memfd", func(t *testing.T) {
		directory := t.TempDir()
		entry, key := hostBeadsPermitTestEntry(t, "alpha", "city:alpha", "key-alpha", "")
		replaceHostBeadsPermitTestKey(t, &entry, key, false)
		writeHostBeadsPermitDocument(t, directory, []hostBeadsPermitAuthorityEntry{entry})
		if _, err := loadHostBeadsPermitResolver(directory); err == nil {
			t.Fatal("unsealed private-key memfd was accepted")
		}
	})

	t.Run("oversized sealed memfd", func(t *testing.T) {
		directory := t.TempDir()
		entry, _ := hostBeadsPermitTestEntry(t, "alpha", "city:alpha", "key-alpha", "")
		replaceHostBeadsPermitTestKey(t, &entry, []byte(strings.Repeat("x", maxHostBeadsPermitKeyBytes+1)), true)
		writeHostBeadsPermitDocument(t, directory, []hostBeadsPermitAuthorityEntry{entry})
		if _, err := loadHostBeadsPermitResolver(directory); err == nil {
			t.Fatal("oversized private key was accepted")
		}
	})

	t.Run("oversized authority document", func(t *testing.T) {
		directory := t.TempDir()
		if err := os.WriteFile(filepath.Join(directory, hostBeadsPermitAuthorityFile), []byte(strings.Repeat(" ", maxHostBeadsPermitAuthorityBytes+1)), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := loadHostBeadsPermitResolver(directory); err == nil {
			t.Fatal("oversized authority document was accepted")
		}
	})
}

func TestHostBeadsPermitResolverRejectsAuthorityDirectoryReplacement(t *testing.T) {
	requireHostBeadsPermitOwnerSupport(t)
	directory := hostBeadsPermitTestDirectory(t)
	source, err := newHostBeadsPermitAuthoritySource(directory)
	if err != nil {
		t.Fatalf("newHostBeadsPermitAuthoritySource: %v", err)
	}
	t.Cleanup(func() { _ = source.close() })
	backup := directory + ".old"
	if err := os.Rename(directory, backup); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := source.load(); err == nil {
		t.Fatal("replaced authority directory was accepted")
	}
}

func TestHostBeadsPermitRetainedIssuerRejectsAuthorityDirectoryReplacement(t *testing.T) {
	requireHostBeadsPermitOwnerSupport(t)
	directory := hostBeadsPermitTestDirectory(t)
	resolver, err := loadHostBeadsPermitResolver(directory)
	if err != nil {
		t.Fatalf("loadHostBeadsPermitResolver: %v", err)
	}
	t.Cleanup(func() { _ = resolver.close() })
	issuer, _, ok := resolver.resolve("alpha", "city:alpha")
	if !ok || issuer == nil {
		t.Fatal("configured issuer did not resolve before authority replacement")
	}
	request := beads.ControllerProtectedMutationRequest{
		Operation: "issue.revision_transition", ResourceIDs: []string{"record-1"},
		RequestDigest: strings.Repeat("0", 64),
	}
	if _, err := issuer.IssueProtectedMutation(request, "replay-before-replacement"); err != nil {
		t.Fatalf("issue before authority replacement: %v", err)
	}
	backup := directory + ".old"
	if err := os.Rename(directory, backup); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := issuer.IssueProtectedMutation(request, "replay-after-replacement"); err == nil {
		t.Fatal("retained issuer signed after its authority directory was replaced")
	}
}

func TestHostBeadsPermitRetainedIssuerRejectsAuthorityDocumentChange(t *testing.T) {
	requireHostBeadsPermitOwnerSupport(t)
	directory := hostBeadsPermitTestDirectory(t)
	resolver, err := loadHostBeadsPermitResolver(directory)
	if err != nil {
		t.Fatalf("loadHostBeadsPermitResolver: %v", err)
	}
	t.Cleanup(func() { _ = resolver.close() })
	issuer, _, ok := resolver.resolve("alpha", "city:alpha")
	if !ok || issuer == nil {
		t.Fatal("configured issuer did not resolve before authority change")
	}
	request := beads.ControllerProtectedMutationRequest{
		Operation: "issue.revision_transition", ResourceIDs: []string{"record-1"},
		RequestDigest: strings.Repeat("0", 64),
	}
	if _, err := issuer.IssueProtectedMutation(request, "replay-before-change"); err != nil {
		t.Fatalf("issue before authority change: %v", err)
	}
	path := filepath.Join(directory, hostBeadsPermitAuthorityFile)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := issuer.IssueProtectedMutation(request, "replay-after-change"); err == nil {
		t.Fatal("retained issuer signed after its authority document changed")
	}
}

func TestHostBeadsPermitResolverRejectsDuplicateScopesAndKeys(t *testing.T) {
	first, firstKey := hostBeadsPermitTestEntry(t, "alpha", "city:alpha", "key-alpha", "first.pem")
	second, _ := hostBeadsPermitTestEntry(t, "beta", "rig:workers", "key-beta", "second.pem")

	tests := []struct {
		name    string
		entries []hostBeadsPermitAuthorityEntry
	}{
		{name: "duplicate scope", entries: []hostBeadsPermitAuthorityEntry{first, first}},
		{name: "duplicate key identifier", entries: []hostBeadsPermitAuthorityEntry{first, func() hostBeadsPermitAuthorityEntry { second.KeyID = first.KeyID; return second }()}},
		{name: "duplicate public key", entries: []hostBeadsPermitAuthorityEntry{first, func() hostBeadsPermitAuthorityEntry {
			second.KeyID = "key-other"
			replaceHostBeadsPermitTestKey(t, &second, firstKey, true)
			return second
		}()}},
		{name: "reused key handle", entries: []hostBeadsPermitAuthorityEntry{first, func() hostBeadsPermitAuthorityEntry { second.PrivateKeyHandle = first.PrivateKeyHandle; return second }()}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			directory := t.TempDir()
			writeHostBeadsPermitAuthority(t, directory, test.entries)
			if _, err := loadHostBeadsPermitResolver(directory); err == nil {
				t.Fatal("duplicate authority was accepted")
			}
		})
	}
}

func TestHostBeadsPermitResolverRejectsNonStrictDocumentsAndUnsafePaths(t *testing.T) {
	t.Run("unknown field", func(t *testing.T) {
		directory := hostBeadsPermitTestDirectory(t)
		path := filepath.Join(directory, hostBeadsPermitAuthorityFile)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		data = []byte(strings.Replace(string(data), `"entries":`, `"future":true,"entries":`, 1))
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := loadHostBeadsPermitResolver(directory); err == nil {
			t.Fatal("unknown JSON field was accepted")
		}
	})

	t.Run("duplicate JSON field", func(t *testing.T) {
		directory := t.TempDir()
		data := []byte(`{"schema_version":1,"schema_version":1,"entries":[]}`)
		if err := os.WriteFile(filepath.Join(directory, hostBeadsPermitAuthorityFile), data, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := loadHostBeadsPermitResolver(directory); err == nil {
			t.Fatal("duplicate JSON field was accepted")
		}
	})

	t.Run("standard descriptor", func(t *testing.T) {
		directory := t.TempDir()
		entry, _ := hostBeadsPermitTestEntry(t, "alpha", "city:alpha", "key-alpha", "")
		entry.privateKeyFD = 2
		entry.PrivateKeyHandle = ""
		writeHostBeadsPermitDocument(t, directory, []hostBeadsPermitAuthorityEntry{entry})
		if _, err := loadHostBeadsPermitResolver(directory); err == nil {
			t.Fatal("standard private-key descriptor was accepted")
		}
	})

	t.Run("city store reference mismatch", func(t *testing.T) {
		directory := t.TempDir()
		entry, _ := hostBeadsPermitTestEntry(t, "alpha", "city:other", "key-alpha", "")
		writeHostBeadsPermitAuthority(t, directory, []hostBeadsPermitAuthorityEntry{entry})
		if _, err := loadHostBeadsPermitResolver(directory); err == nil {
			t.Fatal("city store reference for another city was accepted")
		}
	})

	t.Run("wrong private key type", func(t *testing.T) {
		directory := t.TempDir()
		entry, _ := hostBeadsPermitTestEntry(t, "alpha", "city:alpha", "key-alpha", "signing-key.pem")
		wrongKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		wrongPKCS8, err := x509.MarshalPKCS8PrivateKey(wrongKey)
		if err != nil {
			t.Fatal(err)
		}
		key := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: wrongPKCS8})
		replaceHostBeadsPermitTestKey(t, &entry, key, true)
		writeHostBeadsPermitAuthority(t, directory, []hostBeadsPermitAuthorityEntry{entry})
		if _, err := loadHostBeadsPermitResolver(directory); err == nil {
			t.Fatal("non-Ed25519 private key was accepted")
		}
	})
}

func TestHostBeadsPermitResolverErrorsDoNotExposePrivateKeyBytes(t *testing.T) {
	directory := t.TempDir()
	entry, _ := hostBeadsPermitTestEntry(t, "alpha", "city:alpha", "key-alpha", "signing-key.pem")
	secret := []byte("never-print-this-private-key-material")
	replaceHostBeadsPermitTestKey(t, &entry, secret, true)
	writeHostBeadsPermitAuthority(t, directory, []hostBeadsPermitAuthorityEntry{entry})
	_, err := loadHostBeadsPermitResolver(directory)
	if err == nil {
		t.Fatal("invalid private key was accepted")
	}
	if strings.Contains(err.Error(), string(secret)) {
		t.Fatalf("error exposed key bytes: %v", err)
	}
}

func hostBeadsPermitTestDirectory(t *testing.T) string {
	t.Helper()
	directory := t.TempDir()
	entry, _ := hostBeadsPermitTestEntry(t, "alpha", "city:alpha", "key-alpha", "")
	writeHostBeadsPermitAuthority(t, directory, []hostBeadsPermitAuthorityEntry{entry})
	return directory
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
	replaceHostBeadsPermitTestKey(t, &entry, encoded, true)
	return entry, encoded
}

func writeHostBeadsPermitAuthority(t *testing.T, directory string, entries []hostBeadsPermitAuthorityEntry) {
	t.Helper()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	writeHostBeadsPermitDocument(t, directory, entries)
}

func writeHostBeadsPermitDocument(t *testing.T, directory string, entries []hostBeadsPermitAuthorityEntry) {
	t.Helper()
	placeholder, err := os.CreateTemp("/tmp", "gc-key-broker-*.sock")
	if err != nil {
		t.Fatal(err)
	}
	brokerSocket := placeholder.Name()
	if err := placeholder.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(brokerSocket); err != nil {
		t.Fatal(err)
	}
	document := hostBeadsPermitAuthorityDocument{SchemaVersion: hostBeadsPermitAuthoritySchemaV1, KeyBrokerSocket: brokerSocket, Entries: entries}
	data, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, hostBeadsPermitAuthorityFile), data, 0o600); err != nil {
		t.Fatal(err)
	}
	startHostBeadsPermitTestBroker(t, brokerSocket, data, entries)
}
