package sessionauthority

import (
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
)

func TestVerifierBindsExactTransitionAndRevalidatesStoredAuthorization(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	v, err := NewVerifier(TrustConfig{
		Keys:        []TrustedKey{{KeyID: "authority-key", PublicKey: base64.StdEncoding.EncodeToString(pub)}},
		Authorities: []Authority{{KeyID: "authority-key", Issuer: "ricky", Subject: "operator", Profiles: []Profile{ProfileWorker}}},
	}, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	want := Expectation{
		City: "pilot", SessionID: "gc-session", Generation: 3,
		EffectiveConfigSHA256: repeatedHex('a'), FromProfile: ProfileDesign,
		ToProfile: ProfileWorker, PermissionMode: "workspace-write",
	}
	claims := Claims{
		SchemaVersion: SchemaVersionV1, AuthorizationID: "auth-1", TokenID: "token-1",
		KeyID: "authority-key", Issuer: "ricky", Subject: "operator",
		City: want.City, SessionID: want.SessionID, Generation: want.Generation,
		EffectiveConfigSHA256: want.EffectiveConfigSHA256, FromProfile: want.FromProfile,
		ToProfile: want.ToProfile, PermissionMode: want.PermissionMode,
		IssuedAt: now.Add(-time.Minute).Unix(), ExpiresAt: now.Add(time.Minute).Unix(),
	}
	token := mintToken(t, priv, claims)
	auth, err := v.Verify(token, want)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	now = now.Add(24 * time.Hour)
	if err := v.VerifyStored(auth, want); err != nil {
		t.Fatalf("VerifyStored after grant expiry: %v", err)
	}
	stale := want
	stale.Generation++
	if err := v.VerifyStored(auth, stale); !errors.Is(err, ErrTargetMismatch) {
		t.Fatalf("VerifyStored stale generation = %v, want target mismatch", err)
	}
	stale = want
	stale.EffectiveConfigSHA256 = repeatedHex('b')
	if err := v.VerifyStored(auth, stale); !errors.Is(err, ErrTargetMismatch) {
		t.Fatalf("VerifyStored stale config = %v, want target mismatch", err)
	}
	revoked, err := NewVerifier(TrustConfig{
		Keys:                    []TrustedKey{{KeyID: "authority-key", PublicKey: base64.StdEncoding.EncodeToString(pub)}},
		Authorities:             []Authority{{KeyID: "authority-key", Issuer: "ricky", Subject: "operator", Profiles: []Profile{ProfileWorker}}},
		RevokedAuthorizationIDs: []string{"auth-1"},
	}, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	if err := revoked.VerifyStored(auth, want); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("VerifyStored revoked authorization = %v, want unauthorized", err)
	}
}

func TestVerifierRejectsUntrustedProfileAndTampering(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	now := time.Now().UTC()
	v, err := NewVerifier(TrustConfig{
		Keys:        []TrustedKey{{KeyID: "key", PublicKey: base64.StdEncoding.EncodeToString(pub)}},
		Authorities: []Authority{{KeyID: "key", Issuer: "issuer", Subject: "subject", Profiles: []Profile{ProfileDesign}}},
	}, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	want := Expectation{City: "city", SessionID: "s", Generation: 1, EffectiveConfigSHA256: repeatedHex('c'), FromProfile: ProfileDesign, ToProfile: ProfileOperator, PermissionMode: "danger"}
	claims := Claims{SchemaVersion: SchemaVersionV1, AuthorizationID: "a", TokenID: "t", KeyID: "key", Issuer: "issuer", Subject: "subject", City: "city", SessionID: "s", Generation: 1, EffectiveConfigSHA256: repeatedHex('c'), FromProfile: ProfileDesign, ToProfile: ProfileOperator, PermissionMode: "danger", IssuedAt: now.Add(-time.Minute).Unix(), ExpiresAt: now.Add(time.Minute).Unix()}
	if _, err := v.Verify(mintToken(t, priv, claims), want); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("operator profile = %v, want unauthorized", err)
	}
	claims.ToProfile = ProfileDesign
	want.ToProfile = ProfileDesign
	token := mintToken(t, priv, claims)
	payload, signature, _ := strings.Cut(token, ".")
	if signature[0] == 'A' {
		signature = "B" + signature[1:]
	} else {
		signature = "A" + signature[1:]
	}
	token = payload + "." + signature
	if _, err := v.Verify(token, want); !errors.Is(err, ErrBadSignature) && !errors.Is(err, ErrMalformed) {
		t.Fatalf("tampered token = %v, want signature rejection", err)
	}
}

func TestLoadHostVerifierAndVerifyLaunchFailClosed(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	now := time.Now().UTC()
	trust := TrustConfig{
		Keys:        []TrustedKey{{KeyID: "key", PublicKey: base64.StdEncoding.EncodeToString(pub)}},
		Authorities: []Authority{{KeyID: "key", Issuer: "issuer", Subject: "subject", Profiles: []Profile{ProfileRouter}}},
	}
	path := filepath.Join(t.TempDir(), "trust.json")
	raw, _ := json.Marshal(trust)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(HostTrustFileEnv, path)
	want := Expectation{City: "city", SessionID: "s", Generation: 2, EffectiveConfigSHA256: repeatedHex('d'), FromProfile: ProfileDesign, ToProfile: ProfileRouter, PermissionMode: "plan"}
	claims := Claims{SchemaVersion: SchemaVersionV1, AuthorizationID: "a", TokenID: "t", KeyID: "key", Issuer: "issuer", Subject: "subject", City: want.City, SessionID: want.SessionID, Generation: want.Generation, EffectiveConfigSHA256: want.EffectiveConfigSHA256, FromProfile: want.FromProfile, ToProfile: want.ToProfile, PermissionMode: want.PermissionMode, IssuedAt: now.Add(-time.Minute).Unix(), ExpiresAt: now.Add(time.Minute).Unix()}
	v, err := NewVerifier(trust, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	auth, err := v.Verify(mintToken(t, priv, claims), want)
	if err != nil {
		t.Fatal(err)
	}
	rawAuth, err := EncodeAuthorization(auth)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyLaunch("city", "s", 2, repeatedHex('d'), ProfileRouter, "plan", rawAuth, true); err != nil {
		t.Fatalf("VerifyLaunch: %v", err)
	}
	if err := os.WriteFile(path, []byte(`{"keys":[],"authorities":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := VerifyLaunch("city", "s", 2, repeatedHex('d'), ProfileRouter, "plan", rawAuth, true); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("VerifyLaunch after authority removal = %v, want unavailable", err)
	}
	if err := VerifyLaunch("city", "s", 2, repeatedHex('d'), "", "", "", true); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("VerifyLaunch after protected markers were cleared = %v, want unavailable", err)
	}
}

func TestAppendTransitionPreservesAcceptedAndDeniedHistory(t *testing.T) {
	base := TransitionRecord{AttemptedAt: time.Now().UTC().Format(time.RFC3339Nano), Reason: "rejected", SessionID: "s", Generation: 1, EffectiveConfigSHA256: repeatedHex('e'), FromProfile: ProfileDesign, ToProfile: ProfileWorker, PermissionMode: "write"}
	base.Outcome = "denied"
	raw, err := AppendTransition("", base)
	if err != nil {
		t.Fatal(err)
	}
	base.Outcome, base.Reason, base.AuthorizationID = "accepted", "authorized", "auth-1"
	raw, err = AppendTransition(raw, base)
	if err != nil {
		t.Fatal(err)
	}
	var got []TransitionRecord
	if err := json.Unmarshal([]byte(raw), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Outcome != "denied" || got[1].AuthorizationID != "auth-1" {
		t.Fatalf("history = %#v", got)
	}
}

func mintToken(t *testing.T, key ed25519.PrivateKey, claims Claims) string {
	t.Helper()
	signing, err := SigningBytes(claims)
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(claims)
	sig := ed25519.Sign(key, signing)
	return base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func repeatedHex(b byte) string {
	out := make([]byte, 64)
	for i := range out {
		out[i] = b
	}
	return string(out)
}
