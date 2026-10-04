// Package selectorattestation verifies host-signed selector observations and
// human reviews. It authenticates evidence bindings only; it cannot start a
// trial, authorize retirement, disable an order, or mutate controller state.
package selectorattestation

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
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
	// ObservationSchemaVersionV1 identifies the signed selector observation.
	ObservationSchemaVersionV1 = "gc.selector-observation.v1"
	// ReviewSchemaVersionV1 identifies the signed selector human review.
	ReviewSchemaVersionV1 = "gc.selector-human-review.v1"
	// RevocationsSchemaVersionV1 identifies the signed revocation snapshot.
	RevocationsSchemaVersionV1 = "gc.selector-attestation-revocations.v1"

	// ObservationKeyPurpose is dedicated to selector evidence collectors.
	ObservationKeyPurpose = "selector_observation"
	// ReviewKeyPurpose is dedicated to named human reviewers.
	ReviewKeyPurpose = "selector_human_review"
	// RevocationKeyPurpose is dedicated to selector-attestation revocations.
	RevocationKeyPurpose = "selector_attestation_revocation"

	// ObservationSigningDomain separates observation signatures from every
	// other authority.
	ObservationSigningDomain = "gascity.selector-observation.v1\n"
	// ReviewSigningDomain separates review signatures from every other
	// authority.
	ReviewSigningDomain = "gascity.selector-human-review.v1\n"
	// RevocationSigningDomain separates selector-attestation revocations from
	// every other authority.
	RevocationSigningDomain = "gascity.selector-attestation-revocations.v1\n"

	// DecisionApproved records an affirmative human review.
	DecisionApproved = "approved"
	// DecisionRejected records a negative human review.
	DecisionRejected = "rejected"

	maxRecordPayloadBytes     = 32 << 10
	maxRecordTokenBytes       = 64 << 10
	maxRevocationBytes        = 4 << 20
	maxTrustedKeys            = 64
	maxRevokedRecordIDs       = 65536
	verifiedObservationDomain = "gascity.verified-selector-observation.v1\n"
	verifiedReviewDomain      = "gascity.verified-selector-human-review.v1\n"
)

var (
	// ErrUnavailable means the current complete authority could not be loaded.
	ErrUnavailable = errors.New("selector attestation authority unavailable")
	// ErrMalformed means input is invalid, non-strict, or noncanonical.
	ErrMalformed = errors.New("selector attestation material is malformed")
	// ErrAmbiguous means trusted input repeats an identity or key material.
	ErrAmbiguous = errors.New("selector attestation material is ambiguous")
	// ErrUnknownKey means the signed key identifier is not trusted.
	ErrUnknownKey = errors.New("selector attestation signing key is unknown")
	// ErrBadSignature means the signature fails its dedicated domain check.
	ErrBadSignature = errors.New("selector attestation signature is invalid")
	// ErrPurpose means a claim or key belongs to another signing purpose.
	ErrPurpose = errors.New("selector attestation signing purpose mismatch")
	// ErrAuthority means a key does not own the signed issuer or subject.
	ErrAuthority = errors.New("selector attestation issuer or subject mismatch")
	// ErrAudience means signed material has another configured audience.
	ErrAudience = errors.New("selector attestation audience mismatch")
	// ErrBinding means signed evidence differs from the caller's expectation.
	ErrBinding = errors.New("selector attestation evidence binding mismatch")
	// ErrExpired means signed material is expired or exceeds its maximum age.
	ErrExpired = errors.New("selector attestation material expired")
	// ErrNotYetValid means signed material starts in the future.
	ErrNotYetValid = errors.New("selector attestation material is not yet valid")
	// ErrRevoked means the current snapshot revokes the signed record ID.
	ErrRevoked = errors.New("selector attestation record is revoked")
	// ErrRevocationSnapshot means the signed revocation payload differs from
	// the exact snapshot pinned by trusted startup policy.
	ErrRevocationSnapshot = errors.New("selector attestation revocation snapshot mismatch")
)

// TrustedKey is one dedicated host-authorized Ed25519 key. Observation,
// review, and revocation key material must remain distinct.
type TrustedKey struct {
	KeyID     string `json:"key_id"`
	Issuer    string `json:"issuer"`
	Subject   string `json:"subject,omitempty"`
	Purpose   string `json:"purpose"`
	PublicKey string `json:"public_key"`
}

// ObservationClaims binds one collector signature to exact retained evidence.
// Completeness and coverage are explicit signed results interpreted elsewhere.
type ObservationClaims struct {
	SchemaVersion                 string `json:"schema_version"`
	Purpose                       string `json:"purpose"`
	RecordID                      string `json:"record_id"`
	KeyID                         string `json:"key_id"`
	Issuer                        string `json:"issuer"`
	CollectorIdentity             string `json:"collector_identity"`
	Audience                      string `json:"audience"`
	Workspace                     string `json:"workspace"`
	CandidateManifestSHA256       string `json:"candidate_manifest_sha256"`
	SelectorSnapshotSHA256        string `json:"selector_snapshot_sha256"`
	ObservationLedgerSHA256       string `json:"observation_ledger_sha256"`
	SequenceStart                 uint64 `json:"sequence_start"`
	SequenceEnd                   uint64 `json:"sequence_end"`
	SequenceCompletenessResult    string `json:"sequence_completeness_result"`
	CoverageResult                string `json:"coverage_result"`
	RuntimeIdentitySHA256         string `json:"runtime_identity_sha256"`
	BuildIdentitySHA256           string `json:"build_identity_sha256"`
	ConfigIdentitySHA256          string `json:"config_identity_sha256"`
	OrderInventorySHA256          string `json:"order_inventory_sha256"`
	ExternalWriterInventorySHA256 string `json:"external_writer_inventory_sha256"`
	InFlightResolutionSHA256      string `json:"in_flight_resolution_sha256"`
	ObservationStartedAt          string `json:"observation_started_at"`
	ObservationEndedAt            string `json:"observation_ended_at"`
	IssuedAt                      string `json:"issued_at"`
	ExpiresAt                     string `json:"expires_at"`
}

// ReviewClaims binds one human decision to the exact candidate and signed
// observation record reviewed.
type ReviewClaims struct {
	SchemaVersion           string `json:"schema_version"`
	Purpose                 string `json:"purpose"`
	RecordID                string `json:"record_id"`
	KeyID                   string `json:"key_id"`
	Issuer                  string `json:"issuer"`
	ReviewerIdentity        string `json:"reviewer_identity"`
	Audience                string `json:"audience"`
	Workspace               string `json:"workspace"`
	CandidateManifestSHA256 string `json:"candidate_manifest_sha256"`
	ObservationRecordSHA256 string `json:"observation_record_sha256"`
	Decision                string `json:"decision"`
	ReviewedDeltaSHA256     string `json:"reviewed_delta_sha256"`
	ReviewedAt              string `json:"reviewed_at"`
	IssuedAt                string `json:"issued_at"`
	ExpiresAt               string `json:"expires_at"`
}

// Revocations is one signed current snapshot for an audience and workspace.
type Revocations struct {
	SchemaVersion    string   `json:"schema_version"`
	Purpose          string   `json:"purpose"`
	KeyID            string   `json:"key_id"`
	Issuer           string   `json:"issuer"`
	Audience         string   `json:"audience"`
	Workspace        string   `json:"workspace"`
	IssuedAt         string   `json:"issued_at"`
	ExpiresAt        string   `json:"expires_at"`
	RevokedRecordIDs []string `json:"revoked_record_ids"`
}

// SignedRevocations carries canonical revocation payload bytes and their
// raw-URL-base64 Ed25519 signature.
type SignedRevocations struct {
	Payload   json.RawMessage `json:"payload"`
	Signature string          `json:"signature"`
}

// Bundle is one current host-authority snapshot.
type Bundle struct {
	Keys        []TrustedKey
	Revocations SignedRevocations
}

// Source supplies the current host-authority snapshot. Verification reloads it
// on every call so expiry, key removal, and any unpinned change fail closed.
type Source interface {
	Load(context.Context) (Bundle, error)
}

// Options is trusted controller policy and must not come from request data.
// RevocationPayloadSHA256 pins the exact current canonical signed payload. A
// rotation requires trusted startup policy to move this pin; replacing the
// host file alone fails closed, including after process restart.
type Options struct {
	Audience                string
	RevocationPayloadSHA256 string
	MaxRecordAge            time.Duration
	MaxRevocationAge        time.Duration
	ClockSkew               time.Duration
	Now                     func() time.Time
}

// ObservationExpectation contains the exact evidence facts the caller expects.
type ObservationExpectation struct {
	RecordID                      string
	CollectorIdentity             string
	Audience                      string
	Workspace                     string
	CandidateManifestSHA256       string
	SelectorSnapshotSHA256        string
	ObservationLedgerSHA256       string
	SequenceStart                 uint64
	SequenceEnd                   uint64
	SequenceCompletenessResult    string
	CoverageResult                string
	RuntimeIdentitySHA256         string
	BuildIdentitySHA256           string
	ConfigIdentitySHA256          string
	OrderInventorySHA256          string
	ExternalWriterInventorySHA256 string
	InFlightResolutionSHA256      string
	ObservationStartedAt          time.Time
	ObservationEndedAt            time.Time
}

// ReviewExpectation contains the exact human decision the caller expects.
type ReviewExpectation struct {
	RecordID                string
	ReviewerIdentity        string
	Audience                string
	Workspace               string
	CandidateManifestSHA256 string
	ObservationRecordSHA256 string
	Decision                string
	ReviewedDeltaSHA256     string
	ReviewedAt              time.Time
}

// VerifiedObservation is the sealed result of authenticating an observation.
type VerifiedObservation struct {
	RecordID                      string
	KeyID                         string
	Issuer                        string
	CollectorIdentity             string
	Audience                      string
	Workspace                     string
	CandidateManifestSHA256       string
	SelectorSnapshotSHA256        string
	ObservationLedgerSHA256       string
	SignedRecordSHA256            string
	SequenceStart                 uint64
	SequenceEnd                   uint64
	SequenceCompletenessResult    string
	CoverageResult                string
	RuntimeIdentitySHA256         string
	BuildIdentitySHA256           string
	ConfigIdentitySHA256          string
	OrderInventorySHA256          string
	ExternalWriterInventorySHA256 string
	InFlightResolutionSHA256      string
	ObservationStartedAt          time.Time
	ObservationEndedAt            time.Time
	IssuedAt                      time.Time
	ExpiresAt                     time.Time
	seal                          [sha256.Size]byte
}

// Valid reports whether the value still exactly matches verifier output.
func (v VerifiedObservation) Valid() bool {
	if v.seal == ([sha256.Size]byte{}) {
		return false
	}
	expected := sealObservation(v)
	return subtle.ConstantTimeCompare(v.seal[:], expected[:]) == 1
}

// VerifiedReview is the sealed result of authenticating a human review.
type VerifiedReview struct {
	RecordID                string
	KeyID                   string
	Issuer                  string
	ReviewerIdentity        string
	Audience                string
	Workspace               string
	CandidateManifestSHA256 string
	ObservationRecordSHA256 string
	SignedRecordSHA256      string
	Decision                string
	ReviewedDeltaSHA256     string
	ReviewedAt              time.Time
	IssuedAt                time.Time
	ExpiresAt               time.Time
	seal                    [sha256.Size]byte
}

// Valid reports whether the value still exactly matches verifier output.
func (v VerifiedReview) Valid() bool {
	if v.seal == ([sha256.Size]byte{}) {
		return false
	}
	expected := sealReview(v)
	return subtle.ConstantTimeCompare(v.seal[:], expected[:]) == 1
}

// Verifier authenticates records without collecting evidence or taking action.
type Verifier struct {
	source  Source
	options Options
}

// NewVerifier validates trusted policy and requires a concrete authority source.
func NewVerifier(source Source, options Options) (*Verifier, error) {
	if source == nil {
		return nil, ErrUnavailable
	}
	if !validAtom(options.Audience, 256) || !validSHA256(options.RevocationPayloadSHA256) ||
		options.MaxRecordAge <= 0 || options.MaxRevocationAge <= 0 || options.ClockSkew < 0 {
		return nil, ErrMalformed
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	return &Verifier{source: source, options: options}, nil
}

// ObservationSigningBytes returns the observation domain plus canonical claims.
func ObservationSigningBytes(claims ObservationClaims) ([]byte, error) {
	payload, _, _, err := canonicalObservationClaims(claims)
	if err != nil {
		return nil, err
	}
	return append([]byte(ObservationSigningDomain), payload...), nil
}

// ReviewSigningBytes returns the review domain plus canonical claims.
func ReviewSigningBytes(claims ReviewClaims) ([]byte, error) {
	payload, _, err := canonicalReviewClaims(claims)
	if err != nil {
		return nil, err
	}
	return append([]byte(ReviewSigningDomain), payload...), nil
}

// RevocationSigningBytes returns the revocation domain plus canonical claims.
func RevocationSigningBytes(claims Revocations) ([]byte, error) {
	payload, err := canonicalRevocations(claims)
	if err != nil {
		return nil, err
	}
	return append([]byte(RevocationSigningDomain), payload...), nil
}

// VerifyObservation authenticates a compact token against exact evidence facts.
func (v *Verifier) VerifyObservation(ctx context.Context, token string, want ObservationExpectation) (VerifiedObservation, error) {
	if v == nil || v.source == nil {
		return VerifiedObservation{}, ErrUnavailable
	}
	if err := validateObservationExpectation(want, v.options.Audience); err != nil {
		return VerifiedObservation{}, err
	}
	claims, payload, signature, observationStart, observationEnd, err := decodeObservationToken(token)
	if err != nil {
		return VerifiedObservation{}, err
	}
	keys, revoked, now, err := v.currentAuthority(ctx, want.Audience, want.Workspace)
	if err != nil {
		return VerifiedObservation{}, err
	}
	key, err := authenticateKey(keys, claims.KeyID, claims.Issuer, claims.CollectorIdentity, ObservationKeyPurpose)
	if err != nil {
		return VerifiedObservation{}, err
	}
	if !ed25519.Verify(key.public, append([]byte(ObservationSigningDomain), payload...), signature) {
		return VerifiedObservation{}, ErrBadSignature
	}
	issued, expires, err := validateFreshness(claims.IssuedAt, claims.ExpiresAt, v.options.MaxRecordAge, v.options.ClockSkew, now)
	if err != nil {
		return VerifiedObservation{}, err
	}
	if claims.Audience != v.options.Audience || claims.Audience != want.Audience {
		return VerifiedObservation{}, ErrAudience
	}
	if !matchesObservation(claims, observationStart, observationEnd, want) {
		return VerifiedObservation{}, ErrBinding
	}
	if _, found := revoked[claims.RecordID]; found {
		return VerifiedObservation{}, ErrRevoked
	}
	verified := VerifiedObservation{
		RecordID: claims.RecordID, KeyID: claims.KeyID, Issuer: claims.Issuer,
		CollectorIdentity: claims.CollectorIdentity, Audience: claims.Audience, Workspace: claims.Workspace,
		CandidateManifestSHA256: claims.CandidateManifestSHA256, SelectorSnapshotSHA256: claims.SelectorSnapshotSHA256,
		ObservationLedgerSHA256: claims.ObservationLedgerSHA256, SignedRecordSHA256: sha256String(token),
		SequenceStart: claims.SequenceStart, SequenceEnd: claims.SequenceEnd,
		SequenceCompletenessResult: claims.SequenceCompletenessResult, CoverageResult: claims.CoverageResult,
		RuntimeIdentitySHA256: claims.RuntimeIdentitySHA256, BuildIdentitySHA256: claims.BuildIdentitySHA256,
		ConfigIdentitySHA256: claims.ConfigIdentitySHA256, OrderInventorySHA256: claims.OrderInventorySHA256,
		ExternalWriterInventorySHA256: claims.ExternalWriterInventorySHA256,
		InFlightResolutionSHA256:      claims.InFlightResolutionSHA256,
		ObservationStartedAt:          observationStart, ObservationEndedAt: observationEnd,
		IssuedAt: issued, ExpiresAt: expires,
	}
	verified.seal = sealObservation(verified)
	return verified, nil
}

// VerifyReview authenticates a compact token against an exact human decision.
func (v *Verifier) VerifyReview(ctx context.Context, token string, want ReviewExpectation) (VerifiedReview, error) {
	if v == nil || v.source == nil {
		return VerifiedReview{}, ErrUnavailable
	}
	if err := validateReviewExpectation(want, v.options.Audience); err != nil {
		return VerifiedReview{}, err
	}
	claims, payload, signature, reviewedAt, err := decodeReviewToken(token)
	if err != nil {
		return VerifiedReview{}, err
	}
	keys, revoked, now, err := v.currentAuthority(ctx, want.Audience, want.Workspace)
	if err != nil {
		return VerifiedReview{}, err
	}
	key, err := authenticateKey(keys, claims.KeyID, claims.Issuer, claims.ReviewerIdentity, ReviewKeyPurpose)
	if err != nil {
		return VerifiedReview{}, err
	}
	if !ed25519.Verify(key.public, append([]byte(ReviewSigningDomain), payload...), signature) {
		return VerifiedReview{}, ErrBadSignature
	}
	issued, expires, err := validateFreshness(claims.IssuedAt, claims.ExpiresAt, v.options.MaxRecordAge, v.options.ClockSkew, now)
	if err != nil {
		return VerifiedReview{}, err
	}
	if claims.Audience != v.options.Audience || claims.Audience != want.Audience {
		return VerifiedReview{}, ErrAudience
	}
	if !matchesReview(claims, reviewedAt, want) {
		return VerifiedReview{}, ErrBinding
	}
	if _, found := revoked[claims.RecordID]; found {
		return VerifiedReview{}, ErrRevoked
	}
	verified := VerifiedReview{
		RecordID: claims.RecordID, KeyID: claims.KeyID, Issuer: claims.Issuer,
		ReviewerIdentity: claims.ReviewerIdentity, Audience: claims.Audience, Workspace: claims.Workspace,
		CandidateManifestSHA256: claims.CandidateManifestSHA256,
		ObservationRecordSHA256: claims.ObservationRecordSHA256, SignedRecordSHA256: sha256String(token),
		Decision: claims.Decision, ReviewedDeltaSHA256: claims.ReviewedDeltaSHA256,
		ReviewedAt: reviewedAt, IssuedAt: issued, ExpiresAt: expires,
	}
	verified.seal = sealReview(verified)
	return verified, nil
}

type verifiedKey struct {
	issuer  string
	subject string
	purpose string
	public  ed25519.PublicKey
}

func (v *Verifier) currentAuthority(ctx context.Context, audience, workspace string) (map[string]verifiedKey, map[string]struct{}, time.Time, error) {
	bundle, err := v.source.Load(ctx)
	if err != nil {
		return nil, nil, time.Time{}, fmt.Errorf("load selector attestation authority: %w: %s", ErrUnavailable, err.Error())
	}
	keys, err := validateKeys(bundle.Keys)
	if err != nil {
		return nil, nil, time.Time{}, err
	}
	now := v.options.Now()
	revoked, err := verifyRevocations(bundle.Revocations, keys, audience, workspace, v.options, now)
	if err != nil {
		return nil, nil, time.Time{}, err
	}
	return keys, revoked, now, nil
}

func validateKeys(entries []TrustedKey) (map[string]verifiedKey, error) {
	if len(entries) == 0 || len(entries) > maxTrustedKeys {
		return nil, ErrUnavailable
	}
	keys := make(map[string]verifiedKey, len(entries))
	material := make(map[string]string, len(entries))
	for _, entry := range entries {
		if !validAtom(entry.KeyID, 200) || !validAtom(entry.Issuer, 256) || !validKeyPurpose(entry.Purpose) {
			return nil, ErrMalformed
		}
		if entry.Purpose == RevocationKeyPurpose {
			if entry.Subject != "" {
				return nil, ErrMalformed
			}
		} else if !validAtom(entry.Subject, 256) {
			return nil, ErrMalformed
		}
		if _, exists := keys[entry.KeyID]; exists {
			return nil, ErrAmbiguous
		}
		raw, err := base64.StdEncoding.DecodeString(entry.PublicKey)
		if err != nil || len(raw) != ed25519.PublicKeySize || base64.StdEncoding.EncodeToString(raw) != entry.PublicKey {
			return nil, ErrMalformed
		}
		fingerprint := sha256String(string(raw))
		if _, exists := material[fingerprint]; exists {
			return nil, ErrAmbiguous
		}
		material[fingerprint] = entry.KeyID
		keys[entry.KeyID] = verifiedKey{
			issuer: entry.Issuer, subject: entry.Subject, purpose: entry.Purpose,
			public: append(ed25519.PublicKey(nil), raw...),
		}
	}
	return keys, nil
}

func authenticateKey(keys map[string]verifiedKey, keyID, issuer, subject, purpose string) (verifiedKey, error) {
	key, found := keys[keyID]
	if !found {
		return verifiedKey{}, ErrUnknownKey
	}
	if key.purpose != purpose {
		return verifiedKey{}, ErrPurpose
	}
	if key.issuer != issuer || key.subject != subject {
		return verifiedKey{}, ErrAuthority
	}
	return key, nil
}

func verifyRevocations(envelope SignedRevocations, keys map[string]verifiedKey, audience, workspace string, options Options, now time.Time) (map[string]struct{}, error) {
	payload := append(json.RawMessage(nil), envelope.Payload...)
	if len(payload) == 0 || len(payload) > maxRevocationBytes {
		return nil, ErrUnavailable
	}
	var claims Revocations
	if err := decodeStrictJSON(payload, &claims); err != nil {
		return nil, err
	}
	canonical, err := canonicalRevocations(claims)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(canonical, payload) {
		return nil, ErrMalformed
	}
	if sha256Bytes(payload) != options.RevocationPayloadSHA256 {
		return nil, ErrRevocationSnapshot
	}
	key, err := authenticateKey(keys, claims.KeyID, claims.Issuer, "", RevocationKeyPurpose)
	if err != nil {
		return nil, err
	}
	signature, err := decodeSignature(envelope.Signature)
	if err != nil {
		return nil, err
	}
	if !ed25519.Verify(key.public, append([]byte(RevocationSigningDomain), payload...), signature) {
		return nil, ErrBadSignature
	}
	if claims.Audience != options.Audience || claims.Audience != audience {
		return nil, ErrAudience
	}
	if claims.Workspace != workspace {
		return nil, ErrBinding
	}
	if _, _, err := validateFreshness(claims.IssuedAt, claims.ExpiresAt, options.MaxRevocationAge, options.ClockSkew, now); err != nil {
		return nil, err
	}
	revoked := make(map[string]struct{}, len(claims.RevokedRecordIDs))
	for _, recordID := range claims.RevokedRecordIDs {
		revoked[recordID] = struct{}{}
	}
	return revoked, nil
}

func decodeObservationToken(token string) (ObservationClaims, []byte, []byte, time.Time, time.Time, error) {
	payload, signature, err := splitToken(token)
	if err != nil {
		return ObservationClaims{}, nil, nil, time.Time{}, time.Time{}, err
	}
	var claims ObservationClaims
	if err := decodeStrictJSON(payload, &claims); err != nil {
		return ObservationClaims{}, nil, nil, time.Time{}, time.Time{}, err
	}
	canonical, start, end, err := canonicalObservationClaims(claims)
	if err != nil {
		return ObservationClaims{}, nil, nil, time.Time{}, time.Time{}, err
	}
	if !bytes.Equal(canonical, payload) {
		return ObservationClaims{}, nil, nil, time.Time{}, time.Time{}, ErrMalformed
	}
	return claims, payload, signature, start, end, nil
}

func decodeReviewToken(token string) (ReviewClaims, []byte, []byte, time.Time, error) {
	payload, signature, err := splitToken(token)
	if err != nil {
		return ReviewClaims{}, nil, nil, time.Time{}, err
	}
	var claims ReviewClaims
	if err := decodeStrictJSON(payload, &claims); err != nil {
		return ReviewClaims{}, nil, nil, time.Time{}, err
	}
	canonical, reviewedAt, err := canonicalReviewClaims(claims)
	if err != nil {
		return ReviewClaims{}, nil, nil, time.Time{}, err
	}
	if !bytes.Equal(canonical, payload) {
		return ReviewClaims{}, nil, nil, time.Time{}, ErrMalformed
	}
	return claims, payload, signature, reviewedAt, nil
}

func splitToken(token string) ([]byte, []byte, error) {
	if token == "" || len(token) > maxRecordTokenBytes || strings.TrimSpace(token) != token {
		return nil, nil, ErrMalformed
	}
	parts := strings.Split(token, ".")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return nil, nil, ErrMalformed
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil || base64.RawURLEncoding.EncodeToString(payload) != parts[0] || len(payload) > maxRecordPayloadBytes {
		return nil, nil, ErrMalformed
	}
	signature, err := decodeSignature(parts[1])
	if err != nil {
		return nil, nil, err
	}
	return payload, signature, nil
}

func decodeSignature(encoded string) ([]byte, error) {
	signature, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || len(signature) != ed25519.SignatureSize || base64.RawURLEncoding.EncodeToString(signature) != encoded {
		return nil, ErrMalformed
	}
	return signature, nil
}

func canonicalObservationClaims(claims ObservationClaims) ([]byte, time.Time, time.Time, error) {
	if claims.SchemaVersion != ObservationSchemaVersionV1 {
		return nil, time.Time{}, time.Time{}, ErrMalformed
	}
	if claims.Purpose != ObservationKeyPurpose {
		return nil, time.Time{}, time.Time{}, ErrPurpose
	}
	if !validAtom(claims.RecordID, 200) || !validAtom(claims.KeyID, 200) || !validAtom(claims.Issuer, 256) ||
		!validAtom(claims.CollectorIdentity, 256) || !validAtom(claims.Audience, 256) || !validAtom(claims.Workspace, 512) ||
		!validAtom(claims.SequenceCompletenessResult, 256) || !validAtom(claims.CoverageResult, 256) ||
		claims.SequenceEnd < claims.SequenceStart || !validObservationDigests(claims) {
		return nil, time.Time{}, time.Time{}, ErrMalformed
	}
	start, err := parseCanonicalTime(claims.ObservationStartedAt)
	if err != nil {
		return nil, time.Time{}, time.Time{}, err
	}
	end, err := parseCanonicalTime(claims.ObservationEndedAt)
	if err != nil || !end.After(start) {
		return nil, time.Time{}, time.Time{}, ErrMalformed
	}
	issued, err := parseCanonicalTime(claims.IssuedAt)
	if err != nil || issued.Before(end) {
		return nil, time.Time{}, time.Time{}, ErrMalformed
	}
	expires, err := parseCanonicalTime(claims.ExpiresAt)
	if err != nil || !expires.After(issued) {
		return nil, time.Time{}, time.Time{}, ErrMalformed
	}
	payload, err := json.Marshal(claims)
	if err != nil || len(payload) > maxRecordPayloadBytes {
		return nil, time.Time{}, time.Time{}, ErrMalformed
	}
	return payload, start, end, nil
}

func canonicalReviewClaims(claims ReviewClaims) ([]byte, time.Time, error) {
	if claims.SchemaVersion != ReviewSchemaVersionV1 {
		return nil, time.Time{}, ErrMalformed
	}
	if claims.Purpose != ReviewKeyPurpose {
		return nil, time.Time{}, ErrPurpose
	}
	if !validAtom(claims.RecordID, 200) || !validAtom(claims.KeyID, 200) || !validAtom(claims.Issuer, 256) ||
		!validAtom(claims.ReviewerIdentity, 256) || !validAtom(claims.Audience, 256) || !validAtom(claims.Workspace, 512) ||
		!validDecision(claims.Decision) || !validSHA256(claims.CandidateManifestSHA256) ||
		!validSHA256(claims.ObservationRecordSHA256) || !validSHA256(claims.ReviewedDeltaSHA256) {
		return nil, time.Time{}, ErrMalformed
	}
	reviewedAt, err := parseCanonicalTime(claims.ReviewedAt)
	if err != nil {
		return nil, time.Time{}, err
	}
	issued, err := parseCanonicalTime(claims.IssuedAt)
	if err != nil || issued.Before(reviewedAt) {
		return nil, time.Time{}, ErrMalformed
	}
	expires, err := parseCanonicalTime(claims.ExpiresAt)
	if err != nil || !expires.After(issued) {
		return nil, time.Time{}, ErrMalformed
	}
	payload, err := json.Marshal(claims)
	if err != nil || len(payload) > maxRecordPayloadBytes {
		return nil, time.Time{}, ErrMalformed
	}
	return payload, reviewedAt, nil
}

func canonicalRevocations(claims Revocations) ([]byte, error) {
	if claims.SchemaVersion != RevocationsSchemaVersionV1 {
		return nil, ErrMalformed
	}
	if claims.Purpose != RevocationKeyPurpose {
		return nil, ErrPurpose
	}
	if !validAtom(claims.KeyID, 200) || !validAtom(claims.Issuer, 256) || !validAtom(claims.Audience, 256) ||
		!validAtom(claims.Workspace, 512) {
		return nil, ErrMalformed
	}
	ids := append([]string{}, claims.RevokedRecordIDs...)
	if len(ids) > maxRevokedRecordIDs {
		return nil, ErrMalformed
	}
	for _, id := range ids {
		if !validAtom(id, 200) {
			return nil, ErrMalformed
		}
	}
	sort.Strings(ids)
	for i := 1; i < len(ids); i++ {
		if ids[i] == ids[i-1] {
			return nil, ErrAmbiguous
		}
	}
	issued, err := parseCanonicalTime(claims.IssuedAt)
	if err != nil {
		return nil, err
	}
	expires, err := parseCanonicalTime(claims.ExpiresAt)
	if err != nil || !expires.After(issued) {
		return nil, ErrMalformed
	}
	claims.RevokedRecordIDs = ids
	payload, err := json.Marshal(claims)
	if err != nil || len(payload) > maxRevocationBytes {
		return nil, ErrMalformed
	}
	return payload, nil
}

func validateObservationExpectation(w ObservationExpectation, audience string) error {
	if !validAtom(w.RecordID, 200) || !validAtom(w.CollectorIdentity, 256) || !validAtom(w.Audience, 256) ||
		!validAtom(w.Workspace, 512) || !validAtom(w.SequenceCompletenessResult, 256) || !validAtom(w.CoverageResult, 256) ||
		w.SequenceEnd < w.SequenceStart || !validObservationExpectationDigests(w) ||
		w.ObservationStartedAt.IsZero() || !w.ObservationEndedAt.After(w.ObservationStartedAt) {
		return ErrMalformed
	}
	if w.Audience != audience {
		return ErrAudience
	}
	return nil
}

func validateReviewExpectation(w ReviewExpectation, audience string) error {
	if !validAtom(w.RecordID, 200) || !validAtom(w.ReviewerIdentity, 256) || !validAtom(w.Audience, 256) ||
		!validAtom(w.Workspace, 512) || !validDecision(w.Decision) || !validSHA256(w.CandidateManifestSHA256) ||
		!validSHA256(w.ObservationRecordSHA256) || !validSHA256(w.ReviewedDeltaSHA256) || w.ReviewedAt.IsZero() {
		return ErrMalformed
	}
	if w.Audience != audience {
		return ErrAudience
	}
	return nil
}

func matchesObservation(c ObservationClaims, start, end time.Time, w ObservationExpectation) bool {
	return c.RecordID == w.RecordID && c.CollectorIdentity == w.CollectorIdentity && c.Workspace == w.Workspace &&
		c.CandidateManifestSHA256 == w.CandidateManifestSHA256 && c.SelectorSnapshotSHA256 == w.SelectorSnapshotSHA256 &&
		c.ObservationLedgerSHA256 == w.ObservationLedgerSHA256 && c.SequenceStart == w.SequenceStart && c.SequenceEnd == w.SequenceEnd &&
		c.SequenceCompletenessResult == w.SequenceCompletenessResult && c.CoverageResult == w.CoverageResult &&
		c.RuntimeIdentitySHA256 == w.RuntimeIdentitySHA256 && c.BuildIdentitySHA256 == w.BuildIdentitySHA256 &&
		c.ConfigIdentitySHA256 == w.ConfigIdentitySHA256 && c.OrderInventorySHA256 == w.OrderInventorySHA256 &&
		c.ExternalWriterInventorySHA256 == w.ExternalWriterInventorySHA256 && c.InFlightResolutionSHA256 == w.InFlightResolutionSHA256 &&
		start.Equal(w.ObservationStartedAt) && end.Equal(w.ObservationEndedAt)
}

func matchesReview(c ReviewClaims, reviewedAt time.Time, w ReviewExpectation) bool {
	return c.RecordID == w.RecordID && c.ReviewerIdentity == w.ReviewerIdentity && c.Workspace == w.Workspace &&
		c.CandidateManifestSHA256 == w.CandidateManifestSHA256 && c.ObservationRecordSHA256 == w.ObservationRecordSHA256 &&
		c.Decision == w.Decision && c.ReviewedDeltaSHA256 == w.ReviewedDeltaSHA256 && reviewedAt.Equal(w.ReviewedAt)
}

func validObservationDigests(c ObservationClaims) bool {
	return validSHA256(c.CandidateManifestSHA256) && validSHA256(c.SelectorSnapshotSHA256) &&
		validSHA256(c.ObservationLedgerSHA256) && validSHA256(c.RuntimeIdentitySHA256) &&
		validSHA256(c.BuildIdentitySHA256) && validSHA256(c.ConfigIdentitySHA256) &&
		validSHA256(c.OrderInventorySHA256) && validSHA256(c.ExternalWriterInventorySHA256) &&
		validSHA256(c.InFlightResolutionSHA256)
}

func validObservationExpectationDigests(w ObservationExpectation) bool {
	return validSHA256(w.CandidateManifestSHA256) && validSHA256(w.SelectorSnapshotSHA256) &&
		validSHA256(w.ObservationLedgerSHA256) && validSHA256(w.RuntimeIdentitySHA256) &&
		validSHA256(w.BuildIdentitySHA256) && validSHA256(w.ConfigIdentitySHA256) &&
		validSHA256(w.OrderInventorySHA256) && validSHA256(w.ExternalWriterInventorySHA256) &&
		validSHA256(w.InFlightResolutionSHA256)
}

func validKeyPurpose(purpose string) bool {
	return purpose == ObservationKeyPurpose || purpose == ReviewKeyPurpose || purpose == RevocationKeyPurpose
}

func validDecision(decision string) bool {
	return decision == DecisionApproved || decision == DecisionRejected
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

func sha256String(value string) string {
	return sha256Bytes([]byte(value))
}

func sha256Bytes(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}

func sealObservation(v VerifiedObservation) [sha256.Size]byte {
	v.seal = [sha256.Size]byte{}
	payload, _ := json.Marshal(v)
	return sha256.Sum256(append([]byte(verifiedObservationDomain), payload...))
}

func sealReview(v VerifiedReview) [sha256.Size]byte {
	v.seal = [sha256.Size]byte{}
	payload, _ := json.Marshal(v)
	return sha256.Sum256(append([]byte(verifiedReviewDomain), payload...))
}
