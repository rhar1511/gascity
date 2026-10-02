package compatibility

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/qualification"
)

func TestHostCompatibilityAuthorityBindsFullPolicyAndRevalidatesRevocation(t *testing.T) {
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	fixture := newHostAuthorityFixture(t, now)
	authority := NewHostCompatibilityAuthority(fixture.source, time.Hour, func() time.Time { return now })

	policy, err := authority.Resolve(context.Background(), fixture.scope)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if fixture.source.loads != 1 {
		t.Fatalf("Resolve source reads = %d, want one verified snapshot", fixture.source.loads)
	}
	if got, want := policy.RequiredCapabilities, []string{"cap.alpha", "cap.beta"}; !equalStrings(got, want) {
		t.Fatalf("required capabilities = %v, want full signed set %v", got, want)
	}
	request := fixture.request(t, policy)
	loadsBeforeAuthorize := fixture.source.loads
	authorization, err := authority.Authorize(context.Background(), request)
	if err != nil || authorization.Status != qualification.StatusAuthorized {
		t.Fatalf("Authorize = %#v, %v; want authorized", authorization, err)
	}
	if fixture.source.loads != loadsBeforeAuthorize+1 {
		t.Fatalf("Authorize source reads = %d, want one verified snapshot", fixture.source.loads-loadsBeforeAuthorize)
	}
	loadsBeforeVerify := fixture.source.loads
	if err := authority.Verify(context.Background(), request, authorization); err != nil {
		t.Fatalf("Verify current request: %v", err)
	}
	if fixture.source.loads != loadsBeforeVerify+1 {
		t.Fatalf("Verify source reads = %d, want one verified snapshot", fixture.source.loads-loadsBeforeVerify)
	}

	fixture.revoke(t, fixture.record.RecordID)
	if err := authority.Verify(context.Background(), request, authorization); !errors.Is(err, qualification.ErrUnavailable) {
		t.Fatalf("Verify revoked authorization = %v, want unavailable", err)
	}
}

func TestHostCompatibilityAuthorityRejectsWrongScopePurposeAndIncompleteProofs(t *testing.T) {
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	t.Run("wrong scope", func(t *testing.T) {
		fixture := newHostAuthorityFixture(t, now)
		changed := fixture.scope
		changed.CityID = "other-city"
		authority := NewHostCompatibilityAuthority(fixture.source, time.Hour, func() time.Time { return now })
		if _, err := authority.Resolve(context.Background(), changed); !errors.Is(err, qualification.ErrUnavailable) {
			t.Fatalf("Resolve wrong scope = %v, want unavailable", err)
		}
	})
	t.Run("wrong signing role", func(t *testing.T) {
		fixture := newHostAuthorityFixture(t, now)
		fixture.record.KeyID = "revocation-key"
		fixture.bundle.Records[0] = signHostRecord(t, fixture.record, fixture.revocationPrivate)
		fixture.source.bundle = fixture.bundle
		authority := NewHostCompatibilityAuthority(fixture.source, time.Hour, func() time.Time { return now })
		if _, err := authority.Resolve(context.Background(), fixture.scope); !errors.Is(err, qualification.ErrUnavailable) {
			t.Fatalf("Resolve wrong role = %v, want unavailable", err)
		}
	})
	t.Run("incomplete proof set", func(t *testing.T) {
		fixture := newHostAuthorityFixture(t, now)
		authority := NewHostCompatibilityAuthority(fixture.source, time.Hour, func() time.Time { return now })
		policy, err := authority.Resolve(context.Background(), fixture.scope)
		if err != nil {
			t.Fatal(err)
		}
		request := fixture.request(t, policy)
		request.Proofs = request.Proofs[:1]
		request.RequestSHA256, err = qualification.CompatibilityRequestIdentitySHA(request)
		if err == nil {
			t.Fatal("CompatibilityRequestIdentitySHA accepted incomplete proofs")
		}
		if _, err := authority.Authorize(context.Background(), request); !errors.Is(err, qualification.ErrUnavailable) {
			t.Fatalf("Authorize incomplete proofs = %v, want unavailable", err)
		}
	})
	t.Run("key material cannot cross signing purposes", func(t *testing.T) {
		fixture := newHostAuthorityFixture(t, now)
		fixture.bundle.Keys[1].PublicKey = fixture.bundle.Keys[0].PublicKey
		fixture.source.bundle = fixture.bundle
		authority := NewHostCompatibilityAuthority(fixture.source, time.Hour, func() time.Time { return now })
		if _, err := authority.Resolve(context.Background(), fixture.scope); !errors.Is(err, qualification.ErrUnavailable) {
			t.Fatalf("Resolve reused key material = %v, want unavailable", err)
		}
	})
}

func TestHostCompatibilityAuthorityRejectsRecomputedSubsetOfSignedPolicy(t *testing.T) {
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	fixture := newHostAuthorityFixture(t, now)
	authority := NewHostCompatibilityAuthority(fixture.source, time.Hour, func() time.Time { return now })
	policy, err := authority.Resolve(context.Background(), fixture.scope)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	request := fixture.request(t, policy)
	if len(request.Proofs) < 2 {
		t.Fatalf("fixture policy has %d proofs, want a multi-capability policy", len(request.Proofs))
	}

	// Model a caller that keeps its request internally consistent while trying
	// to authorize only a strict subset of the signed record's requirements.
	request.Policy.RequiredCapabilities = append([]string(nil), request.Policy.RequiredCapabilities[:1]...)
	request.Proofs = append([]qualification.CapabilityProof(nil), request.Proofs[:1]...)
	request.RequestSHA256, err = qualification.CompatibilityRequestIdentitySHA(request)
	if err != nil {
		t.Fatalf("CompatibilityRequestIdentitySHA for internally valid subset: %v", err)
	}
	if _, err := authority.Authorize(context.Background(), request); !errors.Is(err, qualification.ErrUnavailable) {
		t.Fatalf("Authorize recomputed strict-subset request = %v, want unavailable against full signed policy", err)
	}
}

func TestHostCompatibilityAuthorityRejectsExpiredRevocationSnapshot(t *testing.T) {
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	fixture := newHostAuthorityFixture(t, now)
	list := HostCompatibilityRevocationsPayload{
		SchemaVersion: HostCompatibilityRevocationsSchemaV1,
		KeyID:         "revocation-key",
		IssuedAt:      now.Add(-2 * time.Hour).Format(time.RFC3339Nano),
		ExpiresAt:     now.Add(-time.Hour).Format(time.RFC3339Nano),
	}
	fixture.bundle.Revocation = signHostRevocations(t, list, fixture.revocationPrivate)
	fixture.source.bundle = fixture.bundle
	authority := NewHostCompatibilityAuthority(fixture.source, time.Hour, func() time.Time { return now })
	if _, err := authority.Resolve(context.Background(), fixture.scope); !errors.Is(err, qualification.ErrUnavailable) {
		t.Fatalf("Resolve expired revocation snapshot = %v, want unavailable", err)
	}
}

func TestHostCompatibilityAuthorityRequiresTrustedMaximumRevocationAge(t *testing.T) {
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	fixture := newHostAuthorityFixture(t, now)
	if _, err := NewHostCompatibilityAuthority(fixture.source, 0, func() time.Time { return now }).Resolve(context.Background(), fixture.scope); !errors.Is(err, qualification.ErrUnavailable) {
		t.Fatalf("Resolve without trusted maximum age = %v, want unavailable", err)
	}

	list := HostCompatibilityRevocationsPayload{
		SchemaVersion: HostCompatibilityRevocationsSchemaV1,
		KeyID:         "revocation-key",
		IssuedAt:      now.Add(-2 * time.Hour).Format(time.RFC3339Nano),
		ExpiresAt:     now.Add(time.Hour).Format(time.RFC3339Nano),
	}
	fixture.bundle.Revocation = signHostRevocations(t, list, fixture.revocationPrivate)
	fixture.source.bundle = fixture.bundle
	authority := NewHostCompatibilityAuthority(fixture.source, time.Hour, func() time.Time { return now })
	if _, err := authority.Resolve(context.Background(), fixture.scope); !errors.Is(err, qualification.ErrUnavailable) {
		t.Fatalf("Resolve over-age signed snapshot = %v, want unavailable", err)
	}
}

func TestFileHostCompatibilityAuthoritySourceReadsProtectedFiles(t *testing.T) {
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	fixture := newHostAuthorityFixture(t, now)
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	writeHostAuthorityBundleFiles(t, directory, fixture.bundle)

	source, err := NewFileHostCompatibilityAuthoritySource(directory)
	if err != nil {
		t.Fatalf("NewFileHostCompatibilityAuthoritySource: %v", err)
	}
	t.Cleanup(func() {
		if err := source.Close(); err != nil {
			t.Errorf("close host compatibility source: %v", err)
		}
	})
	bundle, err := source.Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(bundle.Keys) != 2 || len(bundle.Records) != 1 || len(bundle.Revocation.Payload) == 0 {
		t.Fatalf("loaded incomplete trust bundle: %#v", bundle)
	}

	if _, err := NewFileHostCompatibilityAuthoritySource("relative/path"); !errors.Is(err, qualification.ErrUnavailable) {
		t.Fatalf("relative authority path error = %v, want unavailable", err)
	}
	if err := os.Chmod(filepath.Join(directory, HostCompatibilityKeyringFile), 0o666); err != nil {
		t.Fatal(err)
	}
	if _, err := source.Load(context.Background()); !errors.Is(err, qualification.ErrUnavailable) {
		t.Fatalf("Load writable keyring = %v, want unavailable", err)
	}
}

func TestFileHostCompatibilityAuthoritySourceRejectsReplacedRootAndSymlinkedFile(t *testing.T) {
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	fixture := newHostAuthorityFixture(t, now)
	t.Run("root path replacement", func(t *testing.T) {
		directory := filepath.Join(t.TempDir(), "authority")
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatal(err)
		}
		writeHostAuthorityBundleFiles(t, directory, fixture.bundle)
		source, err := NewFileHostCompatibilityAuthoritySource(directory)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := source.Close(); err != nil {
				t.Errorf("close host compatibility source: %v", err)
			}
		})

		oldDirectory := directory + ".old"
		if err := os.Rename(directory, oldDirectory); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatal(err)
		}
		// Even a byte-for-byte copy of the old trust directory must not silently
		// replace the root captured at trusted server startup.
		writeHostAuthorityBundleFiles(t, directory, fixture.bundle)
		if _, err := source.Load(context.Background()); !errors.Is(err, qualification.ErrUnavailable) {
			t.Fatalf("Load replaced root = %v, want unavailable", err)
		}
	})
	t.Run("file replaced by symlink", func(t *testing.T) {
		directory := filepath.Join(t.TempDir(), "authority")
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatal(err)
		}
		writeHostAuthorityBundleFiles(t, directory, fixture.bundle)
		source, err := NewFileHostCompatibilityAuthoritySource(directory)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := source.Close(); err != nil {
				t.Errorf("close host compatibility source: %v", err)
			}
		})

		keyringPath := filepath.Join(directory, HostCompatibilityKeyringFile)
		if err := os.Remove(keyringPath); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(HostCompatibilityRecordsFile, keyringPath); err != nil {
			t.Fatal(err)
		}
		if _, err := source.Load(context.Background()); !errors.Is(err, qualification.ErrUnavailable) {
			t.Fatalf("Load symlinked keyring = %v, want unavailable", err)
		}
	})
}

func TestFileHostCompatibilityAuthorityRevalidatesSignedRevocationReplacement(t *testing.T) {
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	fixture := newHostAuthorityFixture(t, now)
	directory := filepath.Join(t.TempDir(), "authority")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	writeHostAuthorityBundleFiles(t, directory, fixture.bundle)

	source, err := NewFileHostCompatibilityAuthoritySource(directory)
	if err != nil {
		t.Fatalf("NewFileHostCompatibilityAuthoritySource: %v", err)
	}
	t.Cleanup(func() {
		if err := source.Close(); err != nil {
			t.Errorf("close host compatibility source: %v", err)
		}
	})
	authority := NewHostCompatibilityAuthority(source, time.Hour, func() time.Time { return now })
	policy, err := authority.Resolve(context.Background(), fixture.scope)
	if err != nil {
		t.Fatalf("Resolve from host files: %v", err)
	}
	if !equalStrings(policy.RequiredCapabilities, []string{"cap.alpha", "cap.beta"}) {
		t.Fatalf("host-file policy capabilities = %v, want complete signed set", policy.RequiredCapabilities)
	}
	request := fixture.request(t, policy)
	decision, err := authority.Authorize(context.Background(), request)
	if err != nil || decision.Status != qualification.StatusAuthorized {
		t.Fatalf("Authorize from host files = %#v, %v; want authorized", decision, err)
	}
	if err := authority.Verify(context.Background(), request, decision); err != nil {
		t.Fatalf("Verify before revocation replacement: %v", err)
	}

	fixture.revoke(t, fixture.record.RecordID)
	data, err := json.Marshal(fixture.bundle.Revocation)
	if err != nil {
		t.Fatalf("marshal replacement revocations: %v", err)
	}
	if err := os.WriteFile(filepath.Join(directory, HostCompatibilityRevocationsFile), data, 0o600); err != nil {
		t.Fatalf("replace signed revocation snapshot: %v", err)
	}
	if err := authority.Verify(context.Background(), request, decision); !errors.Is(err, qualification.ErrUnavailable) {
		t.Fatalf("Verify after signed revocation replacement = %v, want unavailable", err)
	}
}

func TestHostCompatibilityAuthorityRejectsRevocationLifetimeBeyondMaximumAge(t *testing.T) {
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	fixture := newHostAuthorityFixture(t, now)
	issued := now.Add(-time.Minute)
	list := HostCompatibilityRevocationsPayload{
		SchemaVersion: HostCompatibilityRevocationsSchemaV1,
		KeyID:         "revocation-key",
		IssuedAt:      issued.Format(time.RFC3339Nano),
		ExpiresAt:     issued.Add(2 * time.Hour).Format(time.RFC3339Nano),
	}
	fixture.bundle.Revocation = signHostRevocations(t, list, fixture.revocationPrivate)
	fixture.source.bundle = fixture.bundle
	authority := NewHostCompatibilityAuthority(fixture.source, time.Hour, func() time.Time { return now })
	if _, err := authority.Resolve(context.Background(), fixture.scope); !errors.Is(err, qualification.ErrUnavailable) {
		t.Fatalf("Resolve revocation snapshot issued recently but valid for longer than max age = %v, want unavailable", err)
	}
}

type hostAuthorityFixture struct {
	source            *hostAuthoritySourceFixture
	bundle            HostCompatibilityAuthorityBundle
	scope             qualification.CompatibilityScope
	record            HostCompatibilityRecordPayload
	releasePrivate    ed25519.PrivateKey
	revocationPrivate ed25519.PrivateKey
}

type hostAuthoritySourceFixture struct {
	bundle HostCompatibilityAuthorityBundle
	loads  int
}

func (s *hostAuthoritySourceFixture) Load(ctx context.Context) (HostCompatibilityAuthorityBundle, error) {
	if err := ctx.Err(); err != nil {
		return HostCompatibilityAuthorityBundle{}, err
	}
	s.loads++
	return s.bundle, nil
}

func newHostAuthorityFixture(t *testing.T, now time.Time) *hostAuthorityFixture {
	t.Helper()
	_, releasePrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	_, revocationPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	scope := qualification.CompatibilityScope{
		SchemaVersion: qualification.SchemaVersion,
		CityID:        "city-alpha", ServerID: "server-7", StoreRef: "city:city-alpha",
		EffectiveConfigSHA256: strings.Repeat("1", 64),
		ReleaseRequestSHA256:  strings.Repeat("2", 64),
		Formula: qualification.CompatibilityFormula{
			Name: "review", ContentSHA256: strings.Repeat("3", 64),
			SourceSHA256: strings.Repeat("4", 64), CompiledSHA256: strings.Repeat("5", 64),
		},
		Packs: []qualification.CompatibilityPack{{
			Name: "pack-alpha", RootID: "pack:alpha", Pin: strings.Repeat("a", 40), PinStatus: "locked",
			ManifestSHA256: strings.Repeat("6", 64), SourceSubpathSHA256: strings.Repeat("7", 64), RequiresGC: ">=1.0.0",
		}},
	}
	scopeSHA, err := qualification.CompatibilityScopeIdentitySHA(scope)
	if err != nil {
		t.Fatal(err)
	}
	issued := now.Add(-time.Minute)
	record := HostCompatibilityRecordPayload{
		SchemaVersion: HostCompatibilityRecordSchemaV1,
		RecordID:      "release-approval-17", KeyID: "release-key", Status: "approved",
		IssuedAt: issued.Format(time.RFC3339Nano), ExpiresAt: now.Add(time.Hour).Format(time.RFC3339Nano),
		ScopeSHA256: scopeSHA, ReleaseRequestSHA256: scope.ReleaseRequestSHA256,
		PolicyReference: "record:release-approval-17", PolicyVersion: "policy-8",
		RequiredCapabilities: []string{"cap.beta", "cap.alpha"},
	}
	revocations := HostCompatibilityRevocationsPayload{
		SchemaVersion: HostCompatibilityRevocationsSchemaV1, KeyID: "revocation-key",
		IssuedAt: issued.Format(time.RFC3339Nano), ExpiresAt: now.Add(15 * time.Minute).Format(time.RFC3339Nano),
	}
	bundle := HostCompatibilityAuthorityBundle{
		Keys: []HostCompatibilityPublicKey{
			{KeyID: "release-key", Role: HostCompatibilityRecordRole, PublicKey: base64.StdEncoding.EncodeToString(releasePrivate.Public().(ed25519.PublicKey))},
			{KeyID: "revocation-key", Role: HostCompatibilityRevocationRole, PublicKey: base64.StdEncoding.EncodeToString(revocationPrivate.Public().(ed25519.PublicKey))},
		},
		Records:    []SignedHostCompatibilityRecord{signHostRecord(t, record, releasePrivate)},
		Revocation: signHostRevocations(t, revocations, revocationPrivate),
	}
	fixture := &hostAuthorityFixture{
		bundle: bundle, scope: scope, record: record,
		releasePrivate: releasePrivate, revocationPrivate: revocationPrivate,
	}
	fixture.source = &hostAuthoritySourceFixture{bundle: bundle}
	return fixture
}

func (f *hostAuthorityFixture) request(t *testing.T, policy qualification.CompatibilityPolicy) qualification.CompatibilityRequest {
	t.Helper()
	request := qualification.CompatibilityRequest{
		SchemaVersion: qualification.SchemaVersion,
		Scope:         f.scope,
		Policy:        policy,
	}
	request.ScopeSHA256, _ = qualification.CompatibilityScopeIdentitySHA(f.scope)
	for _, capability := range policy.RequiredCapabilities {
		request.Proofs = append(request.Proofs, qualification.CapabilityProof{
			Status: qualification.StatusAvailable, Capability: capability,
			ScopeSHA256: request.ScopeSHA256, PolicyReference: policy.PolicyReference,
			PolicyVersion: policy.PolicyVersion, EvidenceSHA256: strings.Repeat("8", 64),
		})
	}
	var err error
	request.RequestSHA256, err = qualification.CompatibilityRequestIdentitySHA(request)
	if err != nil {
		t.Fatal(err)
	}
	return request
}

func (f *hostAuthorityFixture) revoke(t *testing.T, recordID string) {
	t.Helper()
	list := HostCompatibilityRevocationsPayload{
		SchemaVersion:  HostCompatibilityRevocationsSchemaV1,
		KeyID:          "revocation-key",
		IssuedAt:       time.Date(2026, 9, 27, 11, 59, 0, 0, time.UTC).Format(time.RFC3339Nano),
		ExpiresAt:      time.Date(2026, 9, 27, 12, 15, 0, 0, time.UTC).Format(time.RFC3339Nano),
		RevokedRecords: []string{recordID},
	}
	f.bundle.Revocation = signHostRevocations(t, list, f.revocationPrivate)
	f.source.bundle = f.bundle
}

func signHostRecord(t *testing.T, record HostCompatibilityRecordPayload, private ed25519.PrivateKey) SignedHostCompatibilityRecord {
	t.Helper()
	canonical, err := HostCompatibilityRecordSigningBytes(record)
	if err != nil {
		t.Fatal(err)
	}
	return SignedHostCompatibilityRecord{
		Payload:   append(json.RawMessage(nil), canonical[len(compatibilityRecordSigningDomain)+1:]...),
		Signature: base64.RawURLEncoding.EncodeToString(ed25519.Sign(private, canonical)),
	}
}

func signHostRevocations(t *testing.T, list HostCompatibilityRevocationsPayload, private ed25519.PrivateKey) SignedHostCompatibilityRevocations {
	t.Helper()
	canonical, err := HostCompatibilityRevocationsSigningBytes(list)
	if err != nil {
		t.Fatal(err)
	}
	return SignedHostCompatibilityRevocations{
		Payload:   append(json.RawMessage(nil), canonical[len(compatibilityRevocationSigningDomain)+1:]...),
		Signature: base64.RawURLEncoding.EncodeToString(ed25519.Sign(private, canonical)),
	}
}

func writeHostAuthorityBundleFiles(t *testing.T, directory string, bundle HostCompatibilityAuthorityBundle) {
	t.Helper()
	writeJSON := func(name string, value any) {
		t.Helper()
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeJSON(HostCompatibilityKeyringFile, hostCompatibilityKeyringFile{
		SchemaVersion: HostCompatibilityAuthoritySchemaV1,
		Keys:          bundle.Keys,
	})
	writeJSON(HostCompatibilityRecordsFile, hostCompatibilityRecordsFile{
		SchemaVersion: HostCompatibilityAuthoritySchemaV1,
		Records:       bundle.Records,
	})
	writeJSON(HostCompatibilityRevocationsFile, bundle.Revocation)
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}
