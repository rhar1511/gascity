package api

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/citywriteauth"
)

func TestPRHumanGrantVerifierBindsPrincipalAndAction(t *testing.T) {
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := NewPRHumanGrantVerifier(PRHumanTrustConfig{
		Keys: []PRHumanGrantKey{{KeyID: "root-review", PublicKey: base64.StdEncoding.EncodeToString(pub)}},
		Authorities: []PRHumanAuthority{{
			KeyID: "root-review", Issuer: "city-governance", Subject: "ricky@example.test",
			Scopes: []string{PRActionScopeMerge},
		}},
	}, nil, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	want := PRHumanGrantExpectation{
		City: "pilot", Scope: PRActionScopeMerge, Owner: "acme", Repo: "widget", PullRequest: 12,
		HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40),
		PolicyVersion: "policy-test-1", WorkID: "gc-44", IdempotencyKey: "merge-12-a1",
	}
	claims := PRHumanGrantClaims{
		KeyID: "root-review", Issuer: "city-governance", Subject: "ricky@example.test",
		City: want.City, Scope: want.Scope, Owner: want.Owner, Repo: want.Repo,
		PullRequest: want.PullRequest, HeadSHA: want.HeadSHA, BaseSHA: want.BaseSHA,
		PolicyVersion: want.PolicyVersion, WorkID: want.WorkID, IdempotencyKey: want.IdempotencyKey,
		IssuedAt: now.Add(-time.Second).Unix(), ExpiresAt: now.Add(time.Minute).Unix(), TokenID: "grant-1",
	}
	principal, err := verifier.Verify(signPRHumanGrant(t, priv, claims), want)
	if err != nil {
		t.Fatal(err)
	}
	if principal.KeyID != "root-review" || principal.Issuer != "city-governance" || principal.Subject != "ricky@example.test" {
		t.Fatalf("verified principal = %+v", principal)
	}
	if !principal.Allows(PRActionScopeMerge) || principal.Allows(PRActionScopePolicyWrite) {
		t.Fatalf("principal scopes were not applied exactly: %+v", principal.Scopes)
	}
	if err := verifier.Authorize(principal, PRActionScopeMerge); err != nil {
		t.Fatalf("exact key/issuer/subject authority was denied: %v", err)
	}
	forgedPrincipal := principal
	forgedPrincipal.KeyID = "worker-kid"
	if err := verifier.Authorize(forgedPrincipal, PRActionScopeMerge); !errors.Is(err, ErrPRHumanGrantUnknownSubject) {
		t.Fatalf("subject detached from its signing key error = %v, want unknown authority", err)
	}

	wrongRevision := want
	wrongRevision.HeadSHA = strings.Repeat("c", 40)
	if _, err := verifier.Verify(signPRHumanGrant(t, priv, claims), wrongRevision); !errors.Is(err, ErrPRHumanGrantTarget) {
		t.Fatalf("wrong revision error = %v, want target mismatch", err)
	}
}

func TestPRHumanGrantVerifierRejectsWorkerWriteKeyForgingHumanSubject(t *testing.T) {
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	workerPub, workerPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	humanPub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	writeVerifier, err := citywriteauth.New(citywriteauth.Options{
		Aud: "gc-city-write.v2", Keys: map[string]ed25519.PublicKey{"worker-kid": workerPub},
		MaxTTL: 2 * time.Minute, Skew: 30 * time.Second, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	prVerifier, err := NewPRHumanGrantVerifier(PRHumanTrustConfig{
		Keys: []PRHumanGrantKey{{KeyID: "human-kid", PublicKey: base64.StdEncoding.EncodeToString(humanPub)}},
		Authorities: []PRHumanAuthority{{
			KeyID: "human-kid", Issuer: "city-governance", Subject: "ricky@example.test",
			Scopes: []string{PRActionScopeMerge},
		}},
	}, map[string]ed25519.PublicKey{"worker-kid": workerPub}, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}

	// The city-write verifier accepts this signed request grant. Its extra sub
	// claim is still just worker-authored data; that key is absent from the
	// separate human trust set and cannot mint a human principal.
	body := []byte(`{"action":"merge"}`)
	expect := citywriteauth.Expect{City: "pilot", ReqDigest: citywriteauth.ReqDigest("POST", "/v0/city/pilot/pr-actions", "", body)}
	writeClaims := struct {
		citywriteauth.Grant
		Issuer  string `json:"iss"`
		Subject string `json:"sub"`
	}{
		Grant: citywriteauth.Grant{
			Kid: "worker-kid", Aud: "gc-city-write.v2", City: "pilot", Epoch: 1,
			IAT: now.Add(-time.Second).Unix(), Exp: now.Add(time.Minute).Unix(), JTI: "worker-grant",
			Req: expect.ReqDigest,
		},
		Issuer: "worker-service", Subject: "ricky@example.test",
	}
	encoded, err := json.Marshal(writeClaims)
	if err != nil {
		t.Fatal(err)
	}
	token := signRawPRToken(workerPriv, encoded)
	if _, err := writeVerifier.Verify(token, expect); err != nil {
		t.Fatalf("ordinary city-write grant should be valid: %v", err)
	}
	if _, err := prVerifier.Verify(token, PRHumanGrantExpectation{
		City: "pilot", Scope: PRActionScopeMerge, Owner: "acme", Repo: "widget", PullRequest: 12,
		HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), PolicyVersion: "policy-test-1",
		WorkID: "gc-44", IdempotencyKey: "merge-12-a1",
	}); !errors.Is(err, ErrPRHumanGrantUnknownKey) {
		t.Fatalf("forged human subject error = %v, want unknown human key", err)
	}
}

func TestPRHumanGrantVerifierRejectsSharedWorkerSigningKeyAndMissingTrust(t *testing.T) {
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	trust := PRHumanTrustConfig{
		Keys:        []PRHumanGrantKey{{KeyID: "review", PublicKey: base64.StdEncoding.EncodeToString(pub)}},
		Authorities: []PRHumanAuthority{{KeyID: "review", Issuer: "root", Subject: "ricky", Scopes: []string{PRActionScopeMerge}}},
	}
	if _, err := NewPRHumanGrantVerifier(trust, map[string]ed25519.PublicKey{"worker": pub}, nil); !errors.Is(err, ErrPRHumanGrantSharedWorkerKey) {
		t.Fatalf("overlapping key error = %v, want shared worker key", err)
	}
	if v, err := ResolvePRHumanGrantVerifier("", nil, nil); err != nil || v != nil {
		t.Fatalf("empty trust config = (%v, %v), want (nil, nil)", v, err)
	}
}

func signPRHumanGrant(t *testing.T, priv ed25519.PrivateKey, claims PRHumanGrantClaims) string {
	t.Helper()
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	return signRawPRToken(priv, payload)
}

func signRawPRToken(priv ed25519.PrivateKey, payload []byte) string {
	return base64.RawURLEncoding.EncodeToString(payload) + "." +
		base64.RawURLEncoding.EncodeToString(ed25519.Sign(priv, payload))
}
