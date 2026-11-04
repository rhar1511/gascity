package sessionauthority

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func nativeBindingFixture(t *testing.T) (*Verifier, ed25519.PrivateKey, NativeBindingClaims, Authorization) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 28, 7, 0, 0, 0, time.UTC)
	v, err := NewVerifier(TrustConfig{
		Keys:        []TrustedKey{{KeyID: "host", PublicKey: base64.StdEncoding.EncodeToString(pub)}},
		Authorities: []Authority{{KeyID: "host", Issuer: "host", Subject: "native-capture", Profiles: []Profile{ProfileWorker}}},
	}, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	want := Expectation{
		City: "reconstructed", SessionID: "session-1", Generation: 2,
		EffectiveConfigSHA256: repeatedHex('a'), FromProfile: ProfileDesign, ToProfile: ProfileWorker, PermissionMode: "workspace-write",
	}
	grant := Claims{
		SchemaVersion: SchemaVersionV1, AuthorizationID: "authorization-1", TokenID: "grant-1", KeyID: "host", Issuer: "host", Subject: "native-capture",
		City: want.City, SessionID: want.SessionID, Generation: want.Generation, EffectiveConfigSHA256: want.EffectiveConfigSHA256,
		FromProfile: want.FromProfile, ToProfile: want.ToProfile, PermissionMode: want.PermissionMode,
		IssuedAt: now.Add(-time.Minute).Unix(), ExpiresAt: now.Add(time.Minute).Unix(),
	}
	auth, err := v.Verify(mintToken(t, priv, grant), want)
	if err != nil {
		t.Fatal(err)
	}
	claims := NativeBindingClaims{
		SchemaVersion: NativeBindingSchemaV1, BindingID: "binding-1", AuthorizationID: grant.AuthorizationID,
		KeyID: grant.KeyID, Issuer: grant.Issuer, Subject: grant.Subject,
		Target: NativeBindingTarget{
			City: want.City, SessionID: want.SessionID, SessionRevision: -71, Generation: want.Generation,
			EffectiveConfigSHA256: want.EffectiveConfigSHA256, PermissionMode: want.PermissionMode, Provider: "codex", Transport: "tmux", SessionKey: "11111111-2222-4333-8444-555555555555",
			InstanceToken: "incarnation-2", ContinuationEpoch: 3, WorkDir: "/canonical/worktree", CaptureSource: "native.session-start",
		},
		IssuedAt: now.Add(-time.Second).Unix(), ExpiresAt: now.Add(time.Minute).Unix(),
	}
	return v, priv, claims, auth
}

func mintNativeBinding(t *testing.T, key ed25519.PrivateKey, c NativeBindingClaims) string {
	t.Helper()
	signing, err := NativeBindingSigningBytes(c)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(raw) + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, signing))
}

func TestNativeBindingVerifiesExactHostSignedSnapshot(t *testing.T) {
	v, priv, c, auth := nativeBindingFixture(t)
	receipt, err := v.VerifyNativeBinding(mintNativeBinding(t, priv, c), c.Target, auth)
	if err != nil || receipt.Target != c.Target || receipt.BindingID != c.BindingID || receipt.Principal != auth.Principal {
		t.Fatalf("receipt=%+v error=%v", receipt, err)
	}
}

func TestNativeBindingRejectsEveryChangedBinding(t *testing.T) {
	cases := []struct {
		name   string
		change func(*NativeBindingTarget)
	}{
		{"city", func(x *NativeBindingTarget) { x.City = "other" }},
		{"session", func(x *NativeBindingTarget) { x.SessionID = "other" }},
		{"revision", func(x *NativeBindingTarget) { x.SessionRevision-- }},
		{"provider", func(x *NativeBindingTarget) { x.Provider = "other" }},
		{"transport", func(x *NativeBindingTarget) { x.Transport = "acp" }},
		{"native-key", func(x *NativeBindingTarget) { x.SessionKey = "other-thread" }},
		{"generation", func(x *NativeBindingTarget) { x.Generation++ }},
		{"incarnation", func(x *NativeBindingTarget) { x.InstanceToken = "other" }},
		{"epoch", func(x *NativeBindingTarget) { x.ContinuationEpoch++ }},
		{"workspace", func(x *NativeBindingTarget) { x.WorkDir = "/other/worktree" }},
		{"config", func(x *NativeBindingTarget) { x.EffectiveConfigSHA256 = repeatedHex('b') }},
		{"permission", func(x *NativeBindingTarget) { x.PermissionMode = "other" }},
		{"capture-source", func(x *NativeBindingTarget) { x.CaptureSource = "stdin-correlation" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v, priv, c, auth := nativeBindingFixture(t)
			want := c.Target
			tc.change(&want)
			if _, err := v.VerifyNativeBinding(mintNativeBinding(t, priv, c), want, auth); !errors.Is(err, ErrTargetMismatch) {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestNativeBindingRejectsMissingTrustBadSignatureAndLaunchProof(t *testing.T) {
	v, priv, c, auth := nativeBindingFixture(t)
	token := mintNativeBinding(t, priv, c)
	var missing *Verifier
	if _, err := missing.VerifyNativeBinding(token, c.Target, auth); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("nil verifier: %v", err)
	}
	_, foreign, _ := ed25519.GenerateKey(rand.Reader)
	if _, err := v.VerifyNativeBinding(mintNativeBinding(t, foreign, c), c.Target, auth); !errors.Is(err, ErrBadSignature) {
		t.Fatalf("bad signature: %v", err)
	}
	if _, err := v.VerifyNativeBinding(token, c.Target, Authorization{}); err == nil {
		t.Fatal("missing launch proof accepted")
	}
	changed := auth
	changed.Claims.AuthorizationID = "other"
	if _, err := v.VerifyNativeBinding(token, c.Target, changed); err == nil {
		t.Fatal("different authorization accepted")
	}
	v.revoked[c.AuthorizationID] = struct{}{}
	if _, err := v.VerifyNativeBinding(token, c.Target, auth); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("revoked launch authorization: %v", err)
	}
}

func TestNativeBindingExpiresEvenWhenLaunchAuthorizationIsDurable(t *testing.T) {
	v, priv, c, auth := nativeBindingFixture(t)
	token := mintNativeBinding(t, priv, c)
	v.now = func() time.Time { return time.Unix(c.ExpiresAt, 0) }
	if _, err := v.VerifyNativeBinding(token, c.Target, auth); !errors.Is(err, ErrExpired) {
		t.Fatalf("expired capture: %v", err)
	}
	v.now = func() time.Time { return time.Unix(c.IssuedAt-1, 0) }
	if _, err := v.VerifyNativeBinding(token, c.Target, auth); !errors.Is(err, ErrExpired) {
		t.Fatalf("future capture: %v", err)
	}
}

func TestNativeBindingRejectsMalformedScopeAndUntrustedAttribution(t *testing.T) {
	v, priv, c, auth := nativeBindingFixture(t)
	for _, token := range []string{"", `{"session_key":"correlated-app-thread"}`, strings.Repeat("a", maxTokenBytes+1)} {
		if _, err := v.VerifyNativeBinding(token, c.Target, auth); !errors.Is(err, ErrMalformed) {
			t.Fatalf("malformed token: %v", err)
		}
	}
	for _, change := range []func(*NativeBindingClaims){
		func(x *NativeBindingClaims) { x.SchemaVersion = SchemaVersionV1 },
		func(x *NativeBindingClaims) { x.Target.WorkDir = "relative" },
		func(x *NativeBindingClaims) { x.Target.WorkDir = "/canonical/../worktree" },
		func(x *NativeBindingClaims) { x.Target.SessionKey = "--last" },
		func(x *NativeBindingClaims) { x.Target.Generation = 0 },
		func(x *NativeBindingClaims) { x.Target.ContinuationEpoch = 0 },
		func(x *NativeBindingClaims) { x.Target.SessionRevision = 0 },
		func(x *NativeBindingClaims) { x.Target.CaptureSource = "" },
		func(x *NativeBindingClaims) { x.Target.InstanceToken = " token " },
		func(x *NativeBindingClaims) { x.ExpiresAt = x.IssuedAt + 301 },
	} {
		bad := c
		change(&bad)
		if _, err := NativeBindingSigningBytes(bad); !errors.Is(err, ErrMalformed) {
			t.Fatalf("malformed claims accepted: %+v, %v", bad, err)
		}
	}
	c.Subject = "untrusted-subject"
	if _, err := v.VerifyNativeBinding(mintNativeBinding(t, priv, c), c.Target, auth); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("untrusted subject: %v", err)
	}
}
