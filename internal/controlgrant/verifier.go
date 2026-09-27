// Package controlgrant verifies operation-specific grants for centrally
// executed workflow controls. It does not execute controls, load trust from
// city/pack state, or persist invocation and effect reservations.
package controlgrant

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode"
)

const (
	// SchemaVersionV1 identifies the signed workflow-control grant payload.
	SchemaVersionV1 = "gc.workflow-control-grant.v1"
	// SigningDomain separates this grant from all other signature purposes.
	SigningDomain = "gascity.workflow-control-grant.v1\n"
	maxTokenBytes = 16 << 10
)

// Kind is a workflow-control operation with a separately granted authority.
type Kind string

const (
	// KindRetry authorizes bounded retry controls.
	KindRetry Kind = "retry"
	// KindRalph authorizes bounded Ralph controls.
	KindRalph Kind = "ralph"
	// KindFanout authorizes bounded fanout controls.
	KindFanout Kind = "fanout"
	// KindDrain authorizes bounded drain controls.
	KindDrain Kind = "drain"
)

// EffectUnit names the side effects charged by a grant's effect allowance.
// The unit is derived from Kind and must agree with the signed claim.
type EffectUnit string

const (
	// EffectRetryAttempts counts retry attempts.
	EffectRetryAttempts EffectUnit = "retry_attempts"
	// EffectRalphAttempts counts Ralph attempts.
	EffectRalphAttempts EffectUnit = "ralph_attempts"
	// EffectFanoutChildren counts fanout children.
	EffectFanoutChildren EffectUnit = "fanout_children"
	// EffectDrainMembers counts drain members.
	EffectDrainMembers EffectUnit = "drain_members"
)

var (
	// ErrUnavailable means no dedicated control-grant authority is configured.
	ErrUnavailable = errors.New("workflow control grant authority unavailable")
	// ErrMalformed means a grant or trust configuration fails strict validation.
	ErrMalformed = errors.New("workflow control grant malformed")
	// ErrUnknownKey means a grant names no trusted control-grant key.
	ErrUnknownKey = errors.New("workflow control grant signing key is unknown")
	// ErrBadSignature means the grant was not signed for the control-grant purpose.
	ErrBadSignature = errors.New("workflow control grant signature is invalid")
	// ErrUnauthorized means the trusted signer cannot authorize this operation kind.
	ErrUnauthorized = errors.New("workflow control grant authority does not allow this operation")
	// ErrTargetMismatch means the grant differs from the server-resolved target.
	ErrTargetMismatch = errors.New("workflow control grant does not match the server-resolved target")
	// ErrExpired means the grant is outside its signed validity window or trusted maximum age.
	ErrExpired = errors.New("workflow control grant is outside its valid time window")
)

// TrustedKey is a control-grant-only Ed25519 verification key. The verifier
// accepts signatures over SigningDomain-prefixed canonical payloads, so a key
// used by city-write, recovery, PR, or compatibility trust cannot authenticate
// those other formats as a control grant.
type TrustedKey struct {
	KeyID     string `json:"key_id"`
	PublicKey string `json:"public_key"`
}

// Authority binds a dedicated signer identity to exact operation kinds.
type Authority struct {
	KeyID   string `json:"key_id"`
	Issuer  string `json:"issuer"`
	Subject string `json:"subject"`
	Kinds   []Kind `json:"kinds"`
}

// TrustConfig is supplied only by a trusted server-startup path. It is not a
// city, pack, provider, worker, or request configuration type.
type TrustConfig struct {
	Keys        []TrustedKey `json:"keys"`
	Authorities []Authority  `json:"authorities"`
}

// Claims is the complete signed authority. The two budget values are separate:
// InvocationLimit counts control invocations, while EffectLimit counts the
// kind-specific EffectUnit. Q45 requires durable reservation before effects;
// this verifier only authenticates those limits.
type Claims struct {
	SchemaVersion       string     `json:"schema_version"`
	GrantID             string     `json:"grant_id"`
	TokenID             string     `json:"jti"`
	KeyID               string     `json:"kid"`
	Issuer              string     `json:"iss"`
	Subject             string     `json:"sub"`
	Kind                Kind       `json:"kind"`
	City                string     `json:"city"`
	StoreRef            string     `json:"store_ref"`
	ControlID           string     `json:"control_id"`
	ControlRevision     int64      `json:"control_revision"`
	WorkID              string     `json:"work_id"`
	WorkRevision        int64      `json:"work_revision"`
	Owner               string     `json:"owner"`
	ExecutionGeneration uint64     `json:"execution_generation"`
	IdempotencyKey      string     `json:"idempotency_key"`
	InvocationLimit     uint64     `json:"invocation_limit"`
	EffectLimit         uint64     `json:"effect_limit"`
	EffectUnit          EffectUnit `json:"effect_unit"`
	IssuedAt            int64      `json:"iat"`
	ExpiresAt           int64      `json:"exp"`
}

// Expectation is assembled from the canonical server-resolved state and the
// typed request. Clients do not provide an Expectation or any of its target
// fields as an authority decision.
type Expectation struct {
	Kind                Kind
	City                string
	StoreRef            string
	ControlID           string
	ControlRevision     int64
	WorkID              string
	WorkRevision        int64
	Owner               string
	ExecutionGeneration uint64
	IdempotencyKey      string
}

// Budget contains authenticated limits. Reservation and consumption are the
// executor's responsibility and must be durable before any control effect.
type Budget struct {
	InvocationLimit uint64
	EffectLimit     uint64
	EffectUnit      EffectUnit
}

// Principal identifies the trusted signing authority represented by a grant.
type Principal struct {
	KeyID   string
	Issuer  string
	Subject string
}

// VerifiedGrant is returned only after signature, authority, lifetime, budget,
// and exact-target validation succeed.
type VerifiedGrant struct {
	Claims    Claims
	Principal Principal
	Budget    Budget
}

type authorityIdentity struct {
	keyID   string
	issuer  string
	subject string
}

// Verifier holds an immutable, dedicated control-grant trust snapshot.
type Verifier struct {
	keys        map[string]ed25519.PublicKey
	authorities map[authorityIdentity]map[Kind]struct{}
	maxGrantAge time.Duration
	now         func() time.Time
}

// NewVerifier validates and copies trusted keys and operation authorities.
// maxGrantAge must come from trusted server startup policy; there is no default.
func NewVerifier(cfg TrustConfig, maxGrantAge time.Duration, now func() time.Time) (*Verifier, error) {
	if len(cfg.Keys) == 0 || len(cfg.Authorities) == 0 {
		return nil, ErrUnavailable
	}
	if maxGrantAge <= 0 {
		return nil, fmt.Errorf("maximum control-grant age must be positive: %w", ErrMalformed)
	}
	keys := make(map[string]ed25519.PublicKey, len(cfg.Keys))
	for _, entry := range cfg.Keys {
		if !validAtom(entry.KeyID) {
			return nil, fmt.Errorf("control-grant key ID is required: %w", ErrMalformed)
		}
		if _, duplicate := keys[entry.KeyID]; duplicate {
			return nil, fmt.Errorf("duplicate control-grant key ID %q: %w", entry.KeyID, ErrMalformed)
		}
		raw, err := base64.StdEncoding.DecodeString(entry.PublicKey)
		if err != nil || len(raw) != ed25519.PublicKeySize || base64.StdEncoding.EncodeToString(raw) != entry.PublicKey {
			return nil, fmt.Errorf("control-grant key %q must be canonical standard-base64 Ed25519 public key: %w", entry.KeyID, ErrMalformed)
		}
		public := ed25519.PublicKey(append([]byte(nil), raw...))
		for priorID, prior := range keys {
			if bytes.Equal(public, prior) {
				return nil, fmt.Errorf("control-grant keys %q and %q duplicate public key material: %w", priorID, entry.KeyID, ErrMalformed)
			}
		}
		keys[entry.KeyID] = public
	}

	authorities := make(map[authorityIdentity]map[Kind]struct{}, len(cfg.Authorities))
	for _, entry := range cfg.Authorities {
		identity := authorityIdentity{keyID: entry.KeyID, issuer: entry.Issuer, subject: entry.Subject}
		if _, known := keys[identity.keyID]; !known || !validAtom(identity.issuer) || !validAtom(identity.subject) || len(entry.Kinds) == 0 {
			return nil, fmt.Errorf("control-grant authority needs a known key, issuer, subject, and operation kind: %w", ErrMalformed)
		}
		if _, duplicate := authorities[identity]; duplicate {
			return nil, fmt.Errorf("duplicate control-grant authority %q/%q/%q: %w", identity.keyID, identity.issuer, identity.subject, ErrMalformed)
		}
		kinds := make(map[Kind]struct{}, len(entry.Kinds))
		for _, kind := range entry.Kinds {
			if _, ok := effectUnitFor(kind); !ok {
				return nil, fmt.Errorf("unknown control-grant operation kind %q: %w", kind, ErrMalformed)
			}
			if _, duplicate := kinds[kind]; duplicate {
				return nil, fmt.Errorf("duplicate control-grant operation kind %q: %w", kind, ErrMalformed)
			}
			kinds[kind] = struct{}{}
		}
		authorities[identity] = kinds
	}
	if now == nil {
		now = time.Now
	}
	return &Verifier{keys: keys, authorities: authorities, maxGrantAge: maxGrantAge, now: now}, nil
}

// ResolveVerifier leaves control-grant authority unavailable when no trusted
// server startup document is supplied. Callers must not obtain raw from city,
// pack, provider, worker, or request data.
func ResolveVerifier(raw string, maxGrantAge time.Duration, now func() time.Time) (*Verifier, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var cfg TrustConfig
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("decode control-grant trust: %w: %w", ErrMalformed, err)
	}
	if err := ensureJSONEOF(dec); err != nil {
		return nil, fmt.Errorf("decode control-grant trust: %w: %w", ErrMalformed, err)
	}
	return NewVerifier(cfg, maxGrantAge, now)
}

// SigningBytes returns the domain-separated canonical claims a dedicated
// control-grant issuer signs. The other authorization domains must not reuse
// this purpose prefix or key source.
func SigningBytes(claims Claims) ([]byte, error) {
	if err := validateClaimsShape(claims); err != nil {
		return nil, err
	}
	canonical, err := json.Marshal(claims)
	if err != nil {
		return nil, fmt.Errorf("encode control-grant claims: %w", err)
	}
	return append([]byte(SigningDomain), canonical...), nil
}

// Verify validates one compact raw-URL-base64(payload).raw-URL-base64(sig)
// grant against the exact server-derived target.
func (v *Verifier) Verify(token string, want Expectation) (VerifiedGrant, error) {
	if v == nil {
		return VerifiedGrant{}, ErrUnavailable
	}
	if err := validateExpectation(want); err != nil {
		return VerifiedGrant{}, err
	}
	claims, payload, signature, err := decodeToken(token)
	if err != nil {
		return VerifiedGrant{}, err
	}
	if err := validateClaimsShape(claims); err != nil {
		return VerifiedGrant{}, err
	}
	public, known := v.keys[claims.KeyID]
	if !known {
		return VerifiedGrant{}, ErrUnknownKey
	}
	signed := append([]byte(SigningDomain), payload...)
	if !ed25519.Verify(public, signed, signature) {
		return VerifiedGrant{}, ErrBadSignature
	}
	issued := time.Unix(claims.IssuedAt, 0)
	expires := time.Unix(claims.ExpiresAt, 0)
	now := v.now()
	if now.Before(issued) || !now.Before(expires) || expires.Sub(issued) > v.maxGrantAge {
		return VerifiedGrant{}, ErrExpired
	}
	identity := authorityIdentity{keyID: claims.KeyID, issuer: claims.Issuer, subject: claims.Subject}
	kinds, allowed := v.authorities[identity]
	if !allowed {
		return VerifiedGrant{}, ErrUnauthorized
	}
	if _, allowed := kinds[claims.Kind]; !allowed {
		return VerifiedGrant{}, ErrUnauthorized
	}
	if !matchesExpectation(claims, want) {
		return VerifiedGrant{}, ErrTargetMismatch
	}
	unit, _ := effectUnitFor(claims.Kind)
	return VerifiedGrant{
		Claims:    claims,
		Principal: Principal{KeyID: claims.KeyID, Issuer: claims.Issuer, Subject: claims.Subject},
		Budget:    Budget{InvocationLimit: claims.InvocationLimit, EffectLimit: claims.EffectLimit, EffectUnit: unit},
	}, nil
}

func decodeToken(token string) (Claims, []byte, []byte, error) {
	if token == "" || len(token) > maxTokenBytes || strings.TrimSpace(token) != token {
		return Claims{}, nil, nil, ErrMalformed
	}
	parts := strings.Split(token, ".")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return Claims{}, nil, nil, ErrMalformed
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil || base64.RawURLEncoding.EncodeToString(payload) != parts[0] || len(payload) > maxTokenBytes {
		return Claims{}, nil, nil, ErrMalformed
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || len(signature) != ed25519.SignatureSize || base64.RawURLEncoding.EncodeToString(signature) != parts[1] {
		return Claims{}, nil, nil, ErrMalformed
	}
	var claims Claims
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&claims); err != nil {
		return Claims{}, nil, nil, fmt.Errorf("decode control-grant claims: %w: %w", ErrMalformed, err)
	}
	if err := ensureJSONEOF(dec); err != nil {
		return Claims{}, nil, nil, fmt.Errorf("decode control-grant claims: %w: %w", ErrMalformed, err)
	}
	canonical, err := json.Marshal(claims)
	if err != nil || !bytes.Equal(canonical, payload) {
		return Claims{}, nil, nil, ErrMalformed
	}
	return claims, payload, signature, nil
}

func ensureJSONEOF(dec *json.Decoder) error {
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("unexpected trailing JSON value")
		}
		return err
	}
	return nil
}

func validateClaimsShape(claims Claims) error {
	if claims.SchemaVersion != SchemaVersionV1 || !validAtom(claims.GrantID) || !validAtom(claims.TokenID) ||
		!validAtom(claims.KeyID) || !validAtom(claims.Issuer) || !validAtom(claims.Subject) ||
		!validKind(claims.Kind) || !validAtom(claims.City) || !validAtom(claims.StoreRef) ||
		!validAtom(claims.ControlID) || claims.ControlRevision == 0 || !validAtom(claims.WorkID) || claims.WorkRevision == 0 ||
		!validAtom(claims.Owner) || claims.ExecutionGeneration == 0 || !validIdempotencyKey(claims.IdempotencyKey) ||
		claims.InvocationLimit == 0 || claims.EffectLimit == 0 || claims.IssuedAt <= 0 || claims.ExpiresAt <= claims.IssuedAt {
		return ErrMalformed
	}
	unit, ok := effectUnitFor(claims.Kind)
	if !ok || claims.EffectUnit != unit {
		return ErrMalformed
	}
	return nil
}

func validateExpectation(want Expectation) error {
	if !validKind(want.Kind) || !validAtom(want.City) || !validAtom(want.StoreRef) || !validAtom(want.ControlID) ||
		want.ControlRevision == 0 || !validAtom(want.WorkID) || want.WorkRevision == 0 || !validAtom(want.Owner) ||
		want.ExecutionGeneration == 0 || !validIdempotencyKey(want.IdempotencyKey) {
		return fmt.Errorf("incomplete server-derived control-grant expectation: %w", ErrMalformed)
	}
	return nil
}

func matchesExpectation(claims Claims, want Expectation) bool {
	return claims.Kind == want.Kind && claims.City == want.City && claims.StoreRef == want.StoreRef &&
		claims.ControlID == want.ControlID && claims.ControlRevision == want.ControlRevision &&
		claims.WorkID == want.WorkID && claims.WorkRevision == want.WorkRevision && claims.Owner == want.Owner &&
		claims.ExecutionGeneration == want.ExecutionGeneration && claims.IdempotencyKey == want.IdempotencyKey
}

func effectUnitFor(kind Kind) (EffectUnit, bool) {
	switch kind {
	case KindRetry:
		return EffectRetryAttempts, true
	case KindRalph:
		return EffectRalphAttempts, true
	case KindFanout:
		return EffectFanoutChildren, true
	case KindDrain:
		return EffectDrainMembers, true
	default:
		return "", false
	}
}

func validKind(kind Kind) bool {
	_, ok := effectUnitFor(kind)
	return ok
}

func validAtom(value string) bool {
	if value == "" || strings.TrimSpace(value) != value {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func validIdempotencyKey(value string) bool {
	if value == "" || len(value) > 200 {
		return false
	}
	for i, r := range value {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case i > 0:
			switch r {
			case '.', '_', ':', '-':
			default:
				return false
			}
		default:
			return false
		}
	}
	return true
}
