package protectedmutation

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

type mutableSource struct {
	bundle Bundle
	loads  int
	err    error
}

func (s *mutableSource) Load(context.Context) (Bundle, error) {
	s.loads++
	return s.bundle, s.err
}

type grantFixture struct {
	now               time.Time
	grantPrivate      ed25519.PrivateKey
	revocationPrivate ed25519.PrivateKey
	source            *mutableSource
	verifier          *Verifier
	claims            Claims
	want              Expectation
}

func newGrantFixture(t *testing.T) grantFixture {
	t.Helper()
	grantPublic, grantPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	revocationPublic, revocationPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	claims := Claims{
		SchemaVersion: SchemaVersionV1,
		Purpose:       GrantKeyPurpose,
		KeyID:         "protected-mutation-key",
		Issuer:        "host-controller-authority",
		Audience:      "beads-protected-mutation",
		Workspace:     "workspace-a",
		Operation:     "record.transition",
		ResourceIDs:   []string{"record:question-2", "record:map-1"},
		RequestDigest: strings.Repeat("a", 64),
		ReplayID:      "018f5f3d-4e43-7d12-8d7a-000000000001",
		IssuedAt:      now.Add(-time.Minute).Format(time.RFC3339Nano),
		ExpiresAt:     now.Add(time.Minute).Format(time.RFC3339Nano),
	}
	want := Expectation{
		Audience:      claims.Audience,
		Workspace:     claims.Workspace,
		Operation:     claims.Operation,
		ResourceIDs:   []string{"record:map-1", "record:question-2"},
		RequestDigest: claims.RequestDigest,
	}
	revocations := Revocations{
		SchemaVersion:    RevocationsSchemaVersionV1,
		Purpose:          RevocationKeyPurpose,
		KeyID:            "protected-mutation-revocation-key",
		Issuer:           "host-controller-authority",
		Audience:         claims.Audience,
		Workspace:        claims.Workspace,
		IssuedAt:         now.Add(-time.Minute).Format(time.RFC3339Nano),
		ExpiresAt:        now.Add(time.Hour).Format(time.RFC3339Nano),
		RevokedReplayIDs: []string{},
	}
	source := &mutableSource{bundle: Bundle{
		Keys: []TrustedKey{
			{KeyID: claims.KeyID, Issuer: claims.Issuer, Purpose: GrantKeyPurpose, PublicKey: base64.StdEncoding.EncodeToString(grantPublic)},
			{KeyID: revocations.KeyID, Issuer: revocations.Issuer, Purpose: RevocationKeyPurpose, PublicKey: base64.StdEncoding.EncodeToString(revocationPublic)},
		},
		Revocations: signRevocations(t, revocations, revocationPrivate),
	}}
	verifier, err := NewVerifier(source, Options{
		Audience:         claims.Audience,
		MaxGrantAge:      5 * time.Minute,
		MaxRevocationAge: 2 * time.Hour,
		Now:              func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	return grantFixture{
		now: now, grantPrivate: grantPrivate, revocationPrivate: revocationPrivate,
		source: source, verifier: verifier, claims: claims, want: want,
	}
}

func TestVerifierBindsExactGenericMutationAndCanonicalResources(t *testing.T) {
	fixture := newGrantFixture(t)
	verified, err := fixture.verifier.Verify(context.Background(), tokenForClaims(t, fixture.claims, fixture.grantPrivate), fixture.want)
	if err != nil {
		t.Fatal(err)
	}
	if fixture.source.loads != 1 {
		t.Fatalf("authority loads = %d, want 1", fixture.source.loads)
	}
	if got, want := verified.ResourceIDs, []string{"record:map-1", "record:question-2"}; !equalStrings(got, want) {
		t.Fatalf("verified resources = %v, want %v", got, want)
	}
	if !verified.Valid() {
		t.Fatal("verified result is not sealed")
	}
	if verified.KeyID != fixture.claims.KeyID || verified.Issuer != fixture.claims.Issuer ||
		verified.Audience != fixture.claims.Audience || verified.Workspace != fixture.claims.Workspace ||
		verified.Operation != fixture.claims.Operation || verified.RequestDigest != fixture.claims.RequestDigest ||
		verified.ReplayID != fixture.claims.ReplayID {
		t.Fatalf("verified grant lost signed bindings: %#v", verified)
	}
	verified.ResourceIDs[0] = "record:tampered"
	if verified.Valid() {
		t.Fatal("mutated verified result remained valid")
	}
}

func TestVerifierRejectsWrongPurposeAudienceScopeAndDigest(t *testing.T) {
	fixture := newGrantFixture(t)
	t.Run("wrong claim purpose", func(t *testing.T) {
		changed := fixture.claims
		changed.Purpose = "compatibility_release"
		token := tokenForRawClaims(t, changed, fixture.grantPrivate, GrantSigningDomain)
		if _, err := fixture.verifier.Verify(context.Background(), token, fixture.want); !errors.Is(err, ErrPurpose) {
			t.Fatalf("Verify wrong purpose = %v, want ErrPurpose", err)
		}
	})
	t.Run("wrong audience", func(t *testing.T) {
		changed := fixture.claims
		changed.Audience = "other-audience"
		token := tokenForClaims(t, changed, fixture.grantPrivate)
		if _, err := fixture.verifier.Verify(context.Background(), token, fixture.want); !errors.Is(err, ErrAudience) {
			t.Fatalf("Verify wrong audience = %v, want ErrAudience", err)
		}
	})
	t.Run("wrong issuer", func(t *testing.T) {
		changed := fixture.claims
		changed.Issuer = "other-host-authority"
		token := tokenForClaims(t, changed, fixture.grantPrivate)
		if _, err := fixture.verifier.Verify(context.Background(), token, fixture.want); !errors.Is(err, ErrAuthority) {
			t.Fatalf("Verify wrong issuer = %v, want ErrAuthority", err)
		}
	})

	token := tokenForClaims(t, fixture.claims, fixture.grantPrivate)
	cases := map[string]struct {
		change func(*Expectation)
		want   error
	}{
		"workspace": {change: func(w *Expectation) { w.Workspace = "workspace-b" }, want: ErrScope},
		"operation": {change: func(w *Expectation) { w.Operation = "record.delete" }, want: ErrScope},
		"resources": {change: func(w *Expectation) { w.ResourceIDs = []string{"record:map-1"} }, want: ErrScope},
		"digest":    {change: func(w *Expectation) { w.RequestDigest = strings.Repeat("b", 64) }, want: ErrDigest},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			changed := fixture.want
			tc.change(&changed)
			if _, err := fixture.verifier.Verify(context.Background(), token, changed); !errors.Is(err, tc.want) {
				t.Fatalf("Verify changed %s = %v, want %v", name, err, tc.want)
			}
		})
	}
}

func TestVerifierRejectsExpiredAndNotYetValidGrants(t *testing.T) {
	fixture := newGrantFixture(t)
	t.Run("expired", func(t *testing.T) {
		changed := fixture.claims
		changed.IssuedAt = fixture.now.Add(-2 * time.Minute).Format(time.RFC3339Nano)
		changed.ExpiresAt = fixture.now.Format(time.RFC3339Nano)
		if _, err := fixture.verifier.Verify(context.Background(), tokenForClaims(t, changed, fixture.grantPrivate), fixture.want); !errors.Is(err, ErrExpired) {
			t.Fatalf("Verify expired = %v, want ErrExpired", err)
		}
	})
	t.Run("not yet valid", func(t *testing.T) {
		changed := fixture.claims
		changed.IssuedAt = fixture.now.Add(time.Second).Format(time.RFC3339Nano)
		changed.ExpiresAt = fixture.now.Add(time.Minute).Format(time.RFC3339Nano)
		if _, err := fixture.verifier.Verify(context.Background(), tokenForClaims(t, changed, fixture.grantPrivate), fixture.want); !errors.Is(err, ErrNotYetValid) {
			t.Fatalf("Verify future grant = %v, want ErrNotYetValid", err)
		}
	})
}

func TestVerifierReloadsSignedRevocationsAndRejectsRevokedReplayID(t *testing.T) {
	fixture := newGrantFixture(t)
	token := tokenForClaims(t, fixture.claims, fixture.grantPrivate)
	if _, err := fixture.verifier.Verify(context.Background(), token, fixture.want); err != nil {
		t.Fatalf("Verify before revocation: %v", err)
	}
	revocations := Revocations{
		SchemaVersion:    RevocationsSchemaVersionV1,
		Purpose:          RevocationKeyPurpose,
		KeyID:            "protected-mutation-revocation-key",
		Issuer:           fixture.claims.Issuer,
		Audience:         fixture.claims.Audience,
		Workspace:        fixture.claims.Workspace,
		IssuedAt:         fixture.now.Format(time.RFC3339Nano),
		ExpiresAt:        fixture.now.Add(time.Hour).Format(time.RFC3339Nano),
		RevokedReplayIDs: []string{fixture.claims.ReplayID},
	}
	fixture.source.bundle.Revocations = signRevocations(t, revocations, fixture.revocationPrivate)
	if _, err := fixture.verifier.Verify(context.Background(), token, fixture.want); !errors.Is(err, ErrRevoked) {
		t.Fatalf("Verify after revocation = %v, want ErrRevoked", err)
	}
	if fixture.source.loads != 2 {
		t.Fatalf("authority loads = %d, want reload for each verification", fixture.source.loads)
	}
}

func TestVerifierRejectsDuplicateResourcesAndMalformedReplayID(t *testing.T) {
	fixture := newGrantFixture(t)
	cases := map[string]func(*Claims){
		"duplicate resource": func(c *Claims) { c.ResourceIDs = []string{"record:map-1", "record:map-1"} },
		"empty resource":     func(c *Claims) { c.ResourceIDs = []string{"record:map-1", ""} },
		"malformed replay":   func(c *Claims) { c.ReplayID = "bad replay id" },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			changed := fixture.claims
			change(&changed)
			if _, err := GrantSigningBytes(changed); !errors.Is(err, ErrMalformed) {
				t.Fatalf("GrantSigningBytes = %v, want ErrMalformed", err)
			}
			token := tokenForRawClaims(t, changed, fixture.grantPrivate, GrantSigningDomain)
			if _, err := fixture.verifier.Verify(context.Background(), token, fixture.want); !errors.Is(err, ErrMalformed) {
				t.Fatalf("Verify = %v, want ErrMalformed", err)
			}
		})
	}
}

func TestVerifierRequiresCanonicalSignedResourceOrder(t *testing.T) {
	fixture := newGrantFixture(t)
	if fixture.claims.ResourceIDs[0] < fixture.claims.ResourceIDs[1] {
		t.Fatal("fixture must start in noncanonical resource order")
	}
	token := tokenForRawClaims(t, fixture.claims, fixture.grantPrivate, GrantSigningDomain)
	if _, err := fixture.verifier.Verify(context.Background(), token, fixture.want); !errors.Is(err, ErrMalformed) {
		t.Fatalf("Verify signed noncanonical resource order = %v, want ErrMalformed", err)
	}

	want := fixture.want
	want.ResourceIDs = []string{"record:map-1", "record:map-1"}
	if _, err := fixture.verifier.Verify(context.Background(), tokenForClaims(t, fixture.claims, fixture.grantPrivate), want); !errors.Is(err, ErrMalformed) {
		t.Fatalf("Verify duplicate expected resources = %v, want ErrMalformed", err)
	}
}

func TestVerifierRejectsForeignSigningDomainAndWrongKeyPurpose(t *testing.T) {
	fixture := newGrantFixture(t)
	payload, err := canonicalClaims(fixture.claims)
	if err != nil {
		t.Fatal(err)
	}
	foreign := tokenForRawPayload(payload, ed25519.Sign(fixture.grantPrivate, append([]byte("gascity.compatibility-release.v1\n"), payload...)))
	if _, err := fixture.verifier.Verify(context.Background(), foreign, fixture.want); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("Verify foreign domain = %v, want ErrBadSignature", err)
	}

	fixture.source.bundle.Keys[0].Purpose = RevocationKeyPurpose
	if _, err := fixture.verifier.Verify(context.Background(), tokenForClaims(t, fixture.claims, fixture.grantPrivate), fixture.want); !errors.Is(err, ErrPurpose) {
		t.Fatalf("Verify wrong key purpose = %v, want ErrPurpose", err)
	}
}

func TestVerifierRejectsKeyMaterialReuseAcrossPurposes(t *testing.T) {
	fixture := newGrantFixture(t)
	fixture.source.bundle.Keys[1].PublicKey = fixture.source.bundle.Keys[0].PublicKey
	if _, err := fixture.verifier.Verify(context.Background(), tokenForClaims(t, fixture.claims, fixture.grantPrivate), fixture.want); !errors.Is(err, ErrMalformed) {
		t.Fatalf("Verify with reused grant and revocation key = %v, want ErrMalformed", err)
	}
}

func TestVerifierFailsClosedForInvalidSignedRevocationState(t *testing.T) {
	fixture := newGrantFixture(t)
	token := tokenForClaims(t, fixture.claims, fixture.grantPrivate)
	t.Run("bad signature", func(t *testing.T) {
		changed := *fixture.source
		changed.bundle = fixture.source.bundle
		changed.bundle.Revocations.Signature = strings.Repeat("A", 86)
		verifier, err := NewVerifier(&changed, fixture.verifier.options)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := verifier.Verify(context.Background(), token, fixture.want); !errors.Is(err, ErrBadSignature) {
			t.Fatalf("Verify with invalid revocation signature = %v, want ErrBadSignature", err)
		}
	})
	t.Run("wrong workspace", func(t *testing.T) {
		changed := *fixture.source
		changed.bundle = fixture.source.bundle
		var list Revocations
		if err := json.Unmarshal(changed.bundle.Revocations.Payload, &list); err != nil {
			t.Fatal(err)
		}
		list.Workspace = "workspace-b"
		changed.bundle.Revocations = signRevocations(t, list, fixture.revocationPrivate)
		verifier, err := NewVerifier(&changed, fixture.verifier.options)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := verifier.Verify(context.Background(), token, fixture.want); !errors.Is(err, ErrScope) {
			t.Fatalf("Verify with foreign revocation scope = %v, want ErrScope", err)
		}
	})
	t.Run("not yet valid", func(t *testing.T) {
		changed := *fixture.source
		changed.bundle = fixture.source.bundle
		var list Revocations
		if err := json.Unmarshal(changed.bundle.Revocations.Payload, &list); err != nil {
			t.Fatal(err)
		}
		list.IssuedAt = fixture.now.Add(time.Second).Format(time.RFC3339Nano)
		list.ExpiresAt = fixture.now.Add(time.Hour).Format(time.RFC3339Nano)
		changed.bundle.Revocations = signRevocations(t, list, fixture.revocationPrivate)
		verifier, err := NewVerifier(&changed, fixture.verifier.options)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := verifier.Verify(context.Background(), token, fixture.want); !errors.Is(err, ErrNotYetValid) {
			t.Fatalf("Verify with future revocations = %v, want ErrNotYetValid", err)
		}
	})
}

func TestVerifierRejectsIncompleteExpectationsAndUnavailableAuthority(t *testing.T) {
	fixture := newGrantFixture(t)
	if _, err := fixture.verifier.Verify(context.Background(), tokenForClaims(t, fixture.claims, fixture.grantPrivate), Expectation{}); !errors.Is(err, ErrMalformed) {
		t.Fatalf("Verify incomplete expectation = %v, want ErrMalformed", err)
	}
	fixture.source.err = errors.New("host authority unavailable")
	if _, err := fixture.verifier.Verify(context.Background(), tokenForClaims(t, fixture.claims, fixture.grantPrivate), fixture.want); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("Verify unavailable source = %v, want ErrUnavailable", err)
	}
}

func TestVerifiedGrantSealPreservesExactStringBytes(t *testing.T) {
	fixture := newGrantFixture(t)
	fixture.claims.ResourceIDs = []string{"record:\ufffd"}
	fixture.want.ResourceIDs = append([]string(nil), fixture.claims.ResourceIDs...)
	verified, err := fixture.verifier.Verify(context.Background(), tokenForClaims(t, fixture.claims, fixture.grantPrivate), fixture.want)
	if err != nil {
		t.Fatal(err)
	}
	if !verified.Valid() {
		t.Fatal("fresh verified grant is not valid")
	}
	verified.ResourceIDs[0] = "record:\xff"
	if verified.Valid() {
		t.Fatal("grant remained valid after byte-distinct invalid UTF-8 mutation")
	}
}

func tokenForClaims(t *testing.T, claims Claims, private ed25519.PrivateKey) string {
	t.Helper()
	signed, err := GrantSigningBytes(claims)
	if err != nil {
		t.Fatal(err)
	}
	payload := signed[len(GrantSigningDomain):]
	return tokenForRawPayload(payload, ed25519.Sign(private, signed))
}

func tokenForRawClaims(t *testing.T, claims Claims, private ed25519.PrivateKey, domain string) string {
	t.Helper()
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	return tokenForRawPayload(payload, ed25519.Sign(private, append([]byte(domain), payload...)))
}

func tokenForRawPayload(payload, signature []byte) string {
	return base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(signature)
}

func signRevocations(t *testing.T, list Revocations, private ed25519.PrivateKey) SignedRevocations {
	t.Helper()
	signed, err := RevocationSigningBytes(list)
	if err != nil {
		t.Fatal(err)
	}
	payload := append([]byte(nil), signed[len(RevocationSigningDomain):]...)
	return SignedRevocations{Payload: payload, Signature: base64.RawURLEncoding.EncodeToString(ed25519.Sign(private, signed))}
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
