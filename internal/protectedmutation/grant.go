// Package protectedmutation verifies short-lived, host-authorized grants for
// exact generic mutations. It does not classify records, execute mutations,
// load city or pack policy, or consume replay identifiers.
package protectedmutation

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	// SchemaVersionV1 identifies the exact protected-mutation grant payload.
	SchemaVersionV1 = "gc.protected-mutation-grant.v1"
	// RevocationsSchemaVersionV1 identifies the signed revocation snapshot.
	RevocationsSchemaVersionV1 = "gc.protected-mutation-revocations.v1"

	// GrantKeyPurpose is dedicated to protected-mutation grants. Compatibility,
	// selector, city-write, and workflow-control keys cannot satisfy it.
	GrantKeyPurpose = "protected_mutation_grant"
	// RevocationKeyPurpose is dedicated to revoking protected-mutation grants.
	RevocationKeyPurpose = "protected_mutation_revocation"

	// GrantSigningDomain separates grant signatures from every other authority.
	GrantSigningDomain = "gascity.protected-mutation-grant.v1\n"
	// RevocationSigningDomain separates revocation signatures from grants and
	// every other authority.
	RevocationSigningDomain = "gascity.protected-mutation-revocations.v1\n"

	maxGrantPayloadBytes    = 32 << 10
	maxGrantTokenBytes      = 64 << 10
	maxRevocationBytes      = 4 << 20
	maxTrustedKeys          = 64
	maxResourceIDs          = 256
	maxRevokedReplayIDs     = 65536
	verifiedGrantSealDomain = "gascity.verified-protected-mutation-grant.v1\n"
)

var (
	// ErrUnavailable means the trusted host authority could not supply a
	// complete current keyring and signed revocation snapshot.
	ErrUnavailable = errors.New("protected mutation authority unavailable")
	// ErrMalformed means a grant, expectation, keyring, or revocation snapshot
	// is not strict and canonical.
	ErrMalformed = errors.New("protected mutation authority data is malformed")
	// ErrUnknownKey means no trusted key has the signed key identifier.
	ErrUnknownKey = errors.New("protected mutation signing key is unknown")
	// ErrBadSignature means a signature is invalid for its dedicated domain.
	ErrBadSignature = errors.New("protected mutation signature is invalid")
	// ErrPurpose means a claim or key is assigned to another signing purpose.
	ErrPurpose = errors.New("protected mutation signing purpose mismatch")
	// ErrAuthority means the signed issuer does not own the named trusted key.
	ErrAuthority = errors.New("protected mutation issuer or key mismatch")
	// ErrAudience means a grant or revocation snapshot has another audience.
	ErrAudience = errors.New("protected mutation audience mismatch")
	// ErrScope means the workspace, operation, or resources differ from the
	// exact server-derived expectation.
	ErrScope = errors.New("protected mutation scope mismatch")
	// ErrDigest means the grant is bound to another request.
	ErrDigest = errors.New("protected mutation request digest mismatch")
	// ErrExpired means a grant or revocation snapshot is expired or exceeds its
	// trusted maximum lifetime.
	ErrExpired = errors.New("protected mutation authority data expired")
	// ErrNotYetValid means a grant or revocation snapshot starts in the future.
	ErrNotYetValid = errors.New("protected mutation authority data is not yet valid")
	// ErrRevoked means the current signed snapshot revokes the grant replay ID.
	ErrRevoked = errors.New("protected mutation grant is revoked")
)

// TrustedKey is one host-authorized Ed25519 key with one signing purpose.
// Grant and revocation key material must be distinct.
type TrustedKey struct {
	KeyID     string `json:"key_id"`
	Issuer    string `json:"issuer"`
	Purpose   string `json:"purpose"`
	PublicKey string `json:"public_key"`
}

// Claims is the complete signed authorization for one exact generic mutation.
// ResourceIDs are set-like and serialized in sorted canonical order.
type Claims struct {
	SchemaVersion string   `json:"schema_version"`
	Purpose       string   `json:"purpose"`
	KeyID         string   `json:"key_id"`
	Issuer        string   `json:"issuer"`
	Audience      string   `json:"audience"`
	Workspace     string   `json:"workspace"`
	Operation     string   `json:"operation"`
	ResourceIDs   []string `json:"resource_ids"`
	RequestDigest string   `json:"request_digest"`
	ReplayID      string   `json:"replay_id"`
	IssuedAt      string   `json:"issued_at"`
	ExpiresAt     string   `json:"expires_at"`
}

// Revocations is a current signed snapshot for one audience and workspace.
// RevokedReplayIDs are serialized in sorted canonical order.
type Revocations struct {
	SchemaVersion    string   `json:"schema_version"`
	Purpose          string   `json:"purpose"`
	KeyID            string   `json:"key_id"`
	Issuer           string   `json:"issuer"`
	Audience         string   `json:"audience"`
	Workspace        string   `json:"workspace"`
	IssuedAt         string   `json:"issued_at"`
	ExpiresAt        string   `json:"expires_at"`
	RevokedReplayIDs []string `json:"revoked_replay_ids"`
}

// SignedRevocations carries canonical payload bytes and a raw-URL-base64
// Ed25519 signature over RevocationSigningDomain plus those bytes.
type SignedRevocations struct {
	Payload   json.RawMessage `json:"payload"`
	Signature string          `json:"signature"`
}

// Bundle is a current host-authority snapshot. Source implementations must not
// obtain it from city, pack, provider, worker, or mutation request data.
type Bundle struct {
	Keys        []TrustedKey
	Revocations SignedRevocations
}

// Source supplies an immutable current host-authority snapshot. Verify calls
// Load every time so key removal, expiry, and signed revocation take effect
// immediately.
type Source interface {
	Load(context.Context) (Bundle, error)
}

// Options is trusted controller startup policy. It is not request input.
type Options struct {
	Audience         string
	MaxGrantAge      time.Duration
	MaxRevocationAge time.Duration
	ClockSkew        time.Duration
	Now              func() time.Time
}

// Expectation contains only server-derived mutation facts. ResourceIDs may be
// supplied in any order; verification canonicalizes them before comparison.
type Expectation struct {
	Audience      string
	Workspace     string
	Operation     string
	ResourceIDs   []string
	RequestDigest string
}

// VerifiedGrant is the generic result of successful verification. ReplayID is
// an authenticated idempotency key for a later durable mutation boundary; this
// package deliberately does not consume it.
type VerifiedGrant struct {
	KeyID         string
	Issuer        string
	Audience      string
	Workspace     string
	Operation     string
	ResourceIDs   []string
	RequestDigest string
	ReplayID      string
	IssuedAt      time.Time
	ExpiresAt     time.Time
	seal          [sha256.Size]byte
}

// Valid reports whether the value still exactly matches a verifier-produced
// result. Consumers must check it before using a result as authority.
func (g VerifiedGrant) Valid() bool {
	if g.seal == ([sha256.Size]byte{}) {
		return false
	}
	expected := sealVerifiedGrant(g)
	return subtle.ConstantTimeCompare(g.seal[:], expected[:]) == 1
}

// Verifier authenticates grants against a current host-authority snapshot.
// It is verify-only and has no mutation or replay-storage capability.
type Verifier struct {
	source  Source
	options Options
}

// NewVerifier validates trusted controller policy. A concrete Source is
// required; there is no fallback authority.
func NewVerifier(source Source, options Options) (*Verifier, error) {
	if source == nil {
		return nil, ErrUnavailable
	}
	if !validAtom(options.Audience, 256) || options.MaxGrantAge <= 0 || options.MaxRevocationAge <= 0 || options.ClockSkew < 0 {
		return nil, ErrMalformed
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	return &Verifier{source: source, options: options}, nil
}

// GrantSigningBytes returns the dedicated domain plus canonical grant payload.
func GrantSigningBytes(claims Claims) ([]byte, error) {
	payload, err := canonicalClaims(claims)
	if err != nil {
		return nil, err
	}
	return append([]byte(GrantSigningDomain), payload...), nil
}

// RevocationSigningBytes returns the dedicated domain plus canonical signed
// revocation payload.
func RevocationSigningBytes(list Revocations) ([]byte, error) {
	payload, err := canonicalRevocations(list)
	if err != nil {
		return nil, err
	}
	return append([]byte(RevocationSigningDomain), payload...), nil
}

// Verify checks one compact raw-URL-base64(payload).raw-URL-base64(signature)
// token against an exact server-derived expectation and current revocations.
func (v *Verifier) Verify(ctx context.Context, token string, want Expectation) (VerifiedGrant, error) {
	if v == nil || v.source == nil {
		return VerifiedGrant{}, ErrUnavailable
	}
	wantResources, err := validateExpectation(want, v.options.Audience)
	if err != nil {
		return VerifiedGrant{}, err
	}
	claims, payload, signature, err := decodeGrantToken(token)
	if err != nil {
		return VerifiedGrant{}, err
	}
	bundle, err := v.source.Load(ctx)
	if err != nil {
		return VerifiedGrant{}, fmt.Errorf("load protected mutation authority: %w: %s", ErrUnavailable, err.Error())
	}
	keys, err := validateKeys(bundle.Keys)
	if err != nil {
		return VerifiedGrant{}, err
	}
	now := v.options.Now()
	revoked, err := verifyRevocations(bundle.Revocations, keys, want, v.options, now)
	if err != nil {
		return VerifiedGrant{}, err
	}
	if claims.Purpose != GrantKeyPurpose {
		return VerifiedGrant{}, ErrPurpose
	}
	key, ok := keys[claims.KeyID]
	if !ok {
		return VerifiedGrant{}, ErrUnknownKey
	}
	if key.purpose != GrantKeyPurpose {
		return VerifiedGrant{}, ErrPurpose
	}
	if key.issuer != claims.Issuer {
		return VerifiedGrant{}, ErrAuthority
	}
	if !ed25519.Verify(key.public, append([]byte(GrantSigningDomain), payload...), signature) {
		return VerifiedGrant{}, ErrBadSignature
	}
	issued, expires, err := validateFreshness(claims.IssuedAt, claims.ExpiresAt, v.options.MaxGrantAge, v.options.ClockSkew, now)
	if err != nil {
		return VerifiedGrant{}, err
	}
	if claims.Audience != v.options.Audience || claims.Audience != want.Audience {
		return VerifiedGrant{}, ErrAudience
	}
	if claims.Workspace != want.Workspace || claims.Operation != want.Operation || !equalStringSlices(claims.ResourceIDs, wantResources) {
		return VerifiedGrant{}, ErrScope
	}
	if claims.RequestDigest != want.RequestDigest {
		return VerifiedGrant{}, ErrDigest
	}
	if _, found := revoked[claims.ReplayID]; found {
		return VerifiedGrant{}, ErrRevoked
	}
	verified := VerifiedGrant{
		KeyID: claims.KeyID, Issuer: claims.Issuer, Audience: claims.Audience,
		Workspace: claims.Workspace, Operation: claims.Operation,
		ResourceIDs: append([]string(nil), claims.ResourceIDs...), RequestDigest: claims.RequestDigest,
		ReplayID: claims.ReplayID, IssuedAt: issued, ExpiresAt: expires,
	}
	verified.seal = sealVerifiedGrant(verified)
	return verified, nil
}

type verifiedKey struct {
	issuer  string
	purpose string
	public  ed25519.PublicKey
}

func validateKeys(entries []TrustedKey) (map[string]verifiedKey, error) {
	if len(entries) == 0 || len(entries) > maxTrustedKeys {
		return nil, ErrUnavailable
	}
	keys := make(map[string]verifiedKey, len(entries))
	material := make(map[string]string, len(entries))
	for _, entry := range entries {
		if !validAtom(entry.KeyID, 200) || !validAtom(entry.Issuer, 256) ||
			(entry.Purpose != GrantKeyPurpose && entry.Purpose != RevocationKeyPurpose) {
			return nil, ErrMalformed
		}
		if _, duplicate := keys[entry.KeyID]; duplicate {
			return nil, ErrMalformed
		}
		raw, err := base64.StdEncoding.DecodeString(entry.PublicKey)
		if err != nil || len(raw) != ed25519.PublicKeySize || base64.StdEncoding.EncodeToString(raw) != entry.PublicKey {
			return nil, ErrMalformed
		}
		digest := sha256.Sum256(raw)
		fingerprint := hex.EncodeToString(digest[:])
		if _, duplicate := material[fingerprint]; duplicate {
			return nil, ErrMalformed
		}
		material[fingerprint] = entry.KeyID
		keys[entry.KeyID] = verifiedKey{issuer: entry.Issuer, purpose: entry.Purpose, public: append(ed25519.PublicKey(nil), raw...)}
	}
	return keys, nil
}

func verifyRevocations(envelope SignedRevocations, keys map[string]verifiedKey, want Expectation, options Options, now time.Time) (map[string]struct{}, error) {
	envelope.Payload = append(json.RawMessage(nil), envelope.Payload...)
	if len(envelope.Payload) == 0 || len(envelope.Payload) > maxRevocationBytes {
		return nil, ErrUnavailable
	}
	var list Revocations
	if err := decodeStrictJSON(envelope.Payload, &list); err != nil {
		return nil, err
	}
	canonical, err := canonicalRevocations(list)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(canonical, envelope.Payload) {
		return nil, ErrMalformed
	}
	key, ok := keys[list.KeyID]
	if !ok {
		return nil, ErrUnknownKey
	}
	if list.Purpose != RevocationKeyPurpose || key.purpose != RevocationKeyPurpose {
		return nil, ErrPurpose
	}
	if key.issuer != list.Issuer {
		return nil, ErrAuthority
	}
	signature, err := base64.RawURLEncoding.DecodeString(envelope.Signature)
	if err != nil || len(signature) != ed25519.SignatureSize || base64.RawURLEncoding.EncodeToString(signature) != envelope.Signature {
		return nil, ErrMalformed
	}
	if !ed25519.Verify(key.public, append([]byte(RevocationSigningDomain), envelope.Payload...), signature) {
		return nil, ErrBadSignature
	}
	if list.Audience != options.Audience || list.Audience != want.Audience {
		return nil, ErrAudience
	}
	if list.Workspace != want.Workspace {
		return nil, ErrScope
	}
	if _, _, err := validateFreshness(list.IssuedAt, list.ExpiresAt, options.MaxRevocationAge, options.ClockSkew, now); err != nil {
		return nil, err
	}
	revoked := make(map[string]struct{}, len(list.RevokedReplayIDs))
	for _, replayID := range list.RevokedReplayIDs {
		revoked[replayID] = struct{}{}
	}
	return revoked, nil
}

func decodeGrantToken(token string) (Claims, []byte, []byte, error) {
	if token == "" || len(token) > maxGrantTokenBytes || strings.TrimSpace(token) != token {
		return Claims{}, nil, nil, ErrMalformed
	}
	parts := strings.Split(token, ".")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return Claims{}, nil, nil, ErrMalformed
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil || base64.RawURLEncoding.EncodeToString(payload) != parts[0] || len(payload) > maxGrantTokenBytes {
		return Claims{}, nil, nil, ErrMalformed
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || len(signature) != ed25519.SignatureSize || base64.RawURLEncoding.EncodeToString(signature) != parts[1] {
		return Claims{}, nil, nil, ErrMalformed
	}
	var claims Claims
	if err := decodeStrictJSON(payload, &claims); err != nil {
		return Claims{}, nil, nil, err
	}
	canonical, err := canonicalClaims(claims)
	if err != nil {
		return Claims{}, nil, nil, err
	}
	if !bytes.Equal(canonical, payload) {
		return Claims{}, nil, nil, ErrMalformed
	}
	return claims, payload, signature, nil
}

func canonicalClaims(claims Claims) ([]byte, error) {
	if claims.SchemaVersion != SchemaVersionV1 {
		return nil, ErrMalformed
	}
	if claims.Purpose != GrantKeyPurpose {
		return nil, ErrPurpose
	}
	if !validAtom(claims.KeyID, 200) || !validAtom(claims.Issuer, 256) || !validAtom(claims.Audience, 256) ||
		!validAtom(claims.Workspace, 512) || !validAtom(claims.Operation, 200) || !validSHA256(claims.RequestDigest) ||
		!validReplayID(claims.ReplayID) {
		return nil, ErrMalformed
	}
	resources, err := canonicalIDs(claims.ResourceIDs, maxResourceIDs, true)
	if err != nil {
		return nil, err
	}
	if _, err := parseCanonicalTime(claims.IssuedAt); err != nil {
		return nil, err
	}
	if _, err := parseCanonicalTime(claims.ExpiresAt); err != nil {
		return nil, err
	}
	claims.ResourceIDs = resources
	payload, err := json.Marshal(claims)
	if err != nil || len(payload) > maxGrantPayloadBytes {
		return nil, ErrMalformed
	}
	return payload, nil
}

func canonicalRevocations(list Revocations) ([]byte, error) {
	if list.SchemaVersion != RevocationsSchemaVersionV1 {
		return nil, ErrMalformed
	}
	if list.Purpose != RevocationKeyPurpose {
		return nil, ErrPurpose
	}
	if !validAtom(list.KeyID, 200) || !validAtom(list.Issuer, 256) || !validAtom(list.Audience, 256) || !validAtom(list.Workspace, 512) {
		return nil, ErrMalformed
	}
	ids, err := canonicalIDs(list.RevokedReplayIDs, maxRevokedReplayIDs, false)
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		if !validReplayID(id) {
			return nil, ErrMalformed
		}
	}
	if _, err := parseCanonicalTime(list.IssuedAt); err != nil {
		return nil, err
	}
	if _, err := parseCanonicalTime(list.ExpiresAt); err != nil {
		return nil, err
	}
	list.RevokedReplayIDs = ids
	payload, err := json.Marshal(list)
	if err != nil || len(payload) > maxRevocationBytes {
		return nil, ErrMalformed
	}
	return payload, nil
}

func validateExpectation(want Expectation, audience string) ([]string, error) {
	if !validAtom(want.Audience, 256) {
		return nil, ErrMalformed
	}
	if want.Audience != audience {
		return nil, ErrAudience
	}
	if !validAtom(want.Workspace, 512) || !validAtom(want.Operation, 200) || !validSHA256(want.RequestDigest) {
		return nil, ErrMalformed
	}
	return canonicalIDs(want.ResourceIDs, maxResourceIDs, true)
}

func canonicalIDs(values []string, limit int, requireNonEmpty bool) ([]string, error) {
	if len(values) > limit || (requireNonEmpty && len(values) == 0) {
		return nil, ErrMalformed
	}
	result := make([]string, len(values))
	copy(result, values)
	for _, value := range result {
		if !validAtom(value, 512) {
			return nil, ErrMalformed
		}
	}
	sort.Strings(result)
	for i := 1; i < len(result); i++ {
		if result[i] == result[i-1] {
			return nil, ErrMalformed
		}
	}
	return result, nil
}

func validateFreshness(issuedText, expiresText string, maxAge, skew time.Duration, now time.Time) (time.Time, time.Time, error) {
	issued, err := parseCanonicalTime(issuedText)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	expires, err := parseCanonicalTime(expiresText)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	if !expires.After(issued) || expires.Sub(issued) > maxAge || now.Sub(issued) > maxAge+skew {
		return time.Time{}, time.Time{}, ErrExpired
	}
	if now.Add(skew).Before(issued) {
		return time.Time{}, time.Time{}, ErrNotYetValid
	}
	if !now.Add(-skew).Before(expires) {
		return time.Time{}, time.Time{}, ErrExpired
	}
	return issued, expires, nil
}

func parseCanonicalTime(value string) (time.Time, error) {
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil || parsed.Format(time.RFC3339Nano) != value {
		return time.Time{}, ErrMalformed
	}
	return parsed, nil
}

func decodeStrictJSON(raw []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return fmt.Errorf("%w: %s", ErrMalformed, err.Error())
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return ErrMalformed
	}
	return nil
}

func validAtom(value string, maxBytes int) bool {
	if value == "" || len(value) > maxBytes || !utf8.ValidString(value) || strings.TrimSpace(value) != value {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func validSHA256(value string) bool {
	if len(value) != sha256.Size*2 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func validReplayID(value string) bool {
	if len(value) < 16 || len(value) > 200 {
		return false
	}
	for i, r := range value {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case i > 0 && (r == '.' || r == '_' || r == ':' || r == '-'):
		default:
			return false
		}
	}
	return true
}

func sealVerifiedGrant(grant VerifiedGrant) [sha256.Size]byte {
	hash := sha256.New()
	hash.Write([]byte(verifiedGrantSealDomain))
	writeSealField := func(value string) {
		var length [8]byte
		binary.BigEndian.PutUint64(length[:], uint64(len(value)))
		hash.Write(length[:])
		hash.Write([]byte(value))
	}
	writeSealField(grant.KeyID)
	writeSealField(grant.Issuer)
	writeSealField(grant.Audience)
	writeSealField(grant.Workspace)
	writeSealField(grant.Operation)
	var resourceCount [8]byte
	binary.BigEndian.PutUint64(resourceCount[:], uint64(len(grant.ResourceIDs)))
	hash.Write(resourceCount[:])
	for _, resourceID := range grant.ResourceIDs {
		writeSealField(resourceID)
	}
	writeSealField(grant.RequestDigest)
	writeSealField(grant.ReplayID)
	writeSealField(grant.IssuedAt.Format(time.RFC3339Nano))
	writeSealField(grant.ExpiresAt.Format(time.RFC3339Nano))
	var seal [sha256.Size]byte
	copy(seal[:], hash.Sum(nil))
	return seal
}

func equalStringSlices(left, right []string) bool {
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
