package controlgrant

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

func controlGrantFixture(t *testing.T) (*Verifier, ed25519.PrivateKey, Claims, Expectation, time.Time) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1_800_000_000, 0).UTC()
	verifier, err := NewVerifier(TrustConfig{
		Keys: []TrustedKey{{KeyID: "control-key", PublicKey: base64.StdEncoding.EncodeToString(public)}},
		Authorities: []Authority{{
			KeyID: "control-key", Issuer: "host-control-authority", Subject: "role:workflow-operator",
			Kinds: []Kind{KindRetry, KindRalph, KindFanout, KindDrain},
		}},
	}, 2*time.Minute, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	claims := Claims{
		SchemaVersion: SchemaVersionV1, GrantID: "grant-001", TokenID: "token-001", KeyID: "control-key",
		Issuer: "host-control-authority", Subject: "role:workflow-operator", Kind: KindFanout,
		City: "city-a", StoreRef: "city:city-a", ControlID: "ga-control", ControlRevision: -7,
		WorkID: "ga-work", WorkRevision: 19, Owner: "agent-owner", ExecutionGeneration: 3,
		IdempotencyKey: "request-001", InvocationLimit: 2, EffectLimit: 5, EffectUnit: EffectFanoutChildren,
		IssuedAt: now.Add(-30 * time.Second).Unix(), ExpiresAt: now.Add(60 * time.Second).Unix(),
	}
	want := Expectation{
		Kind: claims.Kind, City: claims.City, StoreRef: claims.StoreRef, ControlID: claims.ControlID,
		ControlRevision: claims.ControlRevision, WorkID: claims.WorkID, WorkRevision: claims.WorkRevision,
		Owner: claims.Owner, ExecutionGeneration: claims.ExecutionGeneration, IdempotencyKey: claims.IdempotencyKey,
	}
	return verifier, private, claims, want, now
}

func tokenForControlGrant(t *testing.T, claims Claims, private ed25519.PrivateKey) string {
	t.Helper()
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	signingBytes, err := SigningBytes(claims)
	if err != nil {
		t.Fatal(err)
	}
	return tokenForRawPayload(payload, ed25519.Sign(private, signingBytes))
}

func tokenForRawPayload(payload, signature []byte) string {
	return base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(signature)
}

func TestVerifyBindsServerDerivedScopeAndReturnsBothBudgets(t *testing.T) {
	verifier, private, claims, want, _ := controlGrantFixture(t)
	verified, err := verifier.Verify(tokenForControlGrant(t, claims, private), want)
	if err != nil {
		t.Fatal(err)
	}
	if verified.Claims != claims {
		t.Fatalf("verified claims = %#v, want %#v", verified.Claims, claims)
	}
	if verified.Budget.InvocationLimit != 2 || verified.Budget.EffectLimit != 5 || verified.Budget.EffectUnit != EffectFanoutChildren {
		t.Fatalf("verified budget = %#v, want 2 invocations and 5 fanout children", verified.Budget)
	}
	if verified.Principal.KeyID != claims.KeyID || verified.Principal.Issuer != claims.Issuer || verified.Principal.Subject != claims.Subject {
		t.Fatalf("verified principal = %#v, want the signed authority identity", verified.Principal)
	}
}

func TestVerifyDoesNotConsumeGrantOrBudget(t *testing.T) {
	verifier, private, claims, want, _ := controlGrantFixture(t)
	token := tokenForControlGrant(t, claims, private)
	first, err := verifier.Verify(token, want)
	if err != nil {
		t.Fatal(err)
	}
	second, err := verifier.Verify(token, want)
	if err != nil {
		t.Fatalf("repeated Verify: %v", err)
	}
	if first != second {
		t.Fatalf("repeated Verify results differ: first=%#v second=%#v", first, second)
	}
	// Verify authenticates claims only. It does not consume either allowance or
	// prevent replay; an executor must persist its own reservation and receipt.
}

func TestVerifyRejectsEveryChangedServerDerivedScopeField(t *testing.T) {
	verifier, private, claims, want, _ := controlGrantFixture(t)
	token := tokenForControlGrant(t, claims, private)
	cases := map[string]func(*Expectation){
		"kind":                 func(w *Expectation) { w.Kind = KindDrain },
		"city":                 func(w *Expectation) { w.City = "city-b" },
		"store":                func(w *Expectation) { w.StoreRef = "rig:other" },
		"control id":           func(w *Expectation) { w.ControlID = "ga-other" },
		"control revision":     func(w *Expectation) { w.ControlRevision++ },
		"work id":              func(w *Expectation) { w.WorkID = "ga-other-work" },
		"work revision":        func(w *Expectation) { w.WorkRevision++ },
		"owner":                func(w *Expectation) { w.Owner = "agent-other" },
		"execution generation": func(w *Expectation) { w.ExecutionGeneration++ },
		"idempotency key":      func(w *Expectation) { w.IdempotencyKey = "request-other" },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			changed := want
			change(&changed)
			if _, err := verifier.Verify(token, changed); !errors.Is(err, ErrTargetMismatch) {
				t.Fatalf("Verify with changed %s = %v, want ErrTargetMismatch", name, err)
			}
		})
	}
}

func TestVerifyRejectsForeignSigningPurposesEvenWhenKeyIsTrusted(t *testing.T) {
	verifier, private, claims, want, _ := controlGrantFixture(t)
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	purposes := map[string][]byte{
		"city-write and PR grant raw-payload signatures": payload,
		"Q34 lifecycle admission":                        append([]byte("gascity.lifecycle.admission.v1\n"), payload...),
		"Q34 lifecycle completion":                       append([]byte("gascity.lifecycle.completion.v1\n"), payload...),
		"release approval":                               append([]byte("gascity.compatibility-release.v1\n"), payload...),
		"release revocation":                             append([]byte("gascity.compatibility-revocations.v1\n"), payload...),
	}
	for purpose, signedBytes := range purposes {
		t.Run(purpose, func(t *testing.T) {
			token := tokenForRawPayload(payload, ed25519.Sign(private, signedBytes))
			if _, err := verifier.Verify(token, want); !errors.Is(err, ErrBadSignature) {
				t.Fatalf("Verify foreign-purpose signature = %v, want ErrBadSignature", err)
			}
		})
	}
}

func TestVerifyRequiresCanonicalStrictJSONAndEncoding(t *testing.T) {
	verifier, private, claims, want, _ := controlGrantFixture(t)
	canonical, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	duplicateField := strings.Replace(string(canonical), `"grant_id":"grant-001"`, `"grant_id":"grant-001","grant_id":"grant-001"`, 1)
	unknownField := strings.TrimSuffix(string(canonical), "}") + `,"unexpected":true}`
	withWhitespace := " " + string(canonical)
	trailingJSON := string(canonical) + `{}`
	cases := map[string]string{
		"duplicate field":         duplicateField,
		"unknown field":           unknownField,
		"noncanonical whitespace": withWhitespace,
		"trailing JSON value":     trailingJSON,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			payload := []byte(raw)
			token := tokenForRawPayload(payload, ed25519.Sign(private, append([]byte(SigningDomain), payload...)))
			if _, err := verifier.Verify(token, want); !errors.Is(err, ErrMalformed) {
				t.Fatalf("Verify malformed signed payload = %v, want ErrMalformed", err)
			}
		})
	}
	valid := tokenForControlGrant(t, claims, private)
	if _, err := verifier.Verify(valid+"=", want); !errors.Is(err, ErrMalformed) {
		t.Fatalf("Verify padded token = %v, want ErrMalformed", err)
	}
}

func TestSigningAndVerificationRejectInvalidBudgetAndTargetShape(t *testing.T) {
	verifier, private, claims, want, _ := controlGrantFixture(t)
	cases := map[string]func(*Claims){
		"zero invocations":  func(c *Claims) { c.InvocationLimit = 0 },
		"zero effects":      func(c *Claims) { c.EffectLimit = 0 },
		"wrong effect unit": func(c *Claims) { c.EffectUnit = EffectDrainMembers },
		"unknown kind":      func(c *Claims) { c.Kind = "retry-eval" },
		"zero control revision": func(c *Claims) {
			c.ControlRevision = 0
		},
		"zero work revision": func(c *Claims) { c.WorkRevision = 0 },
		"zero generation":    func(c *Claims) { c.ExecutionGeneration = 0 },
		"empty owner":        func(c *Claims) { c.Owner = "" },
		"invalid time window": func(c *Claims) {
			c.ExpiresAt = c.IssuedAt
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			invalid := claims
			change(&invalid)
			if _, err := SigningBytes(invalid); !errors.Is(err, ErrMalformed) {
				t.Fatalf("SigningBytes invalid claim = %v, want ErrMalformed", err)
			}
			payload, err := json.Marshal(invalid)
			if err != nil {
				t.Fatal(err)
			}
			token := tokenForRawPayload(payload, ed25519.Sign(private, append([]byte(SigningDomain), payload...)))
			if _, err := verifier.Verify(token, want); !errors.Is(err, ErrMalformed) {
				t.Fatalf("Verify invalid signed claim = %v, want ErrMalformed", err)
			}
		})
	}
}

func TestVerifierRequiresConfiguredTrustAndEnforcesTrustedMaximumAge(t *testing.T) {
	verifier, private, claims, want, now := controlGrantFixture(t)
	nilVerifier, err := ResolveVerifier("", time.Minute, nil)
	if err != nil || nilVerifier != nil {
		t.Fatalf("ResolveVerifier(absent) = %v, %v, want nil, nil", nilVerifier, err)
	}
	if _, err := nilVerifier.Verify("", Expectation{}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("nil Verify = %v, want ErrUnavailable", err)
	}
	if _, err := NewVerifier(TrustConfig{}, time.Minute, nil); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("NewVerifier(empty trust) = %v, want ErrUnavailable", err)
	}
	public := private.Public().(ed25519.PublicKey)
	if _, err := NewVerifier(TrustConfig{
		Keys:        []TrustedKey{{KeyID: claims.KeyID, PublicKey: base64.StdEncoding.EncodeToString(public)}},
		Authorities: []Authority{{KeyID: claims.KeyID, Issuer: claims.Issuer, Subject: claims.Subject, Kinds: []Kind{claims.Kind}}},
	}, 0, nil); !errors.Is(err, ErrMalformed) {
		t.Fatalf("NewVerifier without trusted maximum age = %v, want ErrMalformed", err)
	}

	claims.IssuedAt = now.Add(-3 * time.Minute).Unix()
	claims.ExpiresAt = now.Add(time.Minute).Unix()
	if _, err := verifier.Verify(tokenForControlGrant(t, claims, private), want); !errors.Is(err, ErrExpired) {
		t.Fatalf("Verify grant older than trusted maximum age = %v, want ErrExpired", err)
	}
}

func TestVerifyRejectsExpiredAndNotYetValidGrant(t *testing.T) {
	verifier, private, claims, want, now := controlGrantFixture(t)
	for name, change := range map[string]func(*Claims){
		"expired": func(c *Claims) { c.ExpiresAt = now.Unix() },
		"not yet issued": func(c *Claims) {
			c.IssuedAt = now.Add(time.Second).Unix()
			c.ExpiresAt = now.Add(30 * time.Second).Unix()
		},
	} {
		t.Run(name, func(t *testing.T) {
			invalid := claims
			change(&invalid)
			if _, err := verifier.Verify(tokenForControlGrant(t, invalid, private), want); !errors.Is(err, ErrExpired) {
				t.Fatalf("Verify invalid grant time = %v, want ErrExpired", err)
			}
		})
	}
}

func TestVerifyRequiresTrustForTheSignedOperationKind(t *testing.T) {
	_, private, claims, _, now := controlGrantFixture(t)
	verifier, err := NewVerifier(TrustConfig{
		Keys: []TrustedKey{{KeyID: claims.KeyID, PublicKey: base64.StdEncoding.EncodeToString(private.Public().(ed25519.PublicKey))}},
		Authorities: []Authority{{
			KeyID: claims.KeyID, Issuer: claims.Issuer, Subject: claims.Subject, Kinds: []Kind{KindFanout},
		}},
	}, 2*time.Minute, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	claims.Kind = KindDrain
	claims.EffectUnit = EffectDrainMembers
	want := Expectation{
		Kind: claims.Kind, City: claims.City, StoreRef: claims.StoreRef, ControlID: claims.ControlID,
		ControlRevision: claims.ControlRevision, WorkID: claims.WorkID, WorkRevision: claims.WorkRevision,
		Owner: claims.Owner, ExecutionGeneration: claims.ExecutionGeneration, IdempotencyKey: claims.IdempotencyKey,
	}
	if _, err := verifier.Verify(tokenForControlGrant(t, claims, private), want); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("Verify unauthorized operation kind = %v, want ErrUnauthorized", err)
	}
}
