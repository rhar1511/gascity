// Package sessionauthority verifies and records controller-authorized session
// launch-profile transitions. Trust is supplied only by a host-owned file; city,
// pack, session, label, and prompt data cannot add authorities.
package sessionauthority

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	// SchemaVersionV1 identifies the first signed session-authority grant schema.
	SchemaVersionV1 = "gc.session-authority-grant.v1"
	// SigningDomain separates session-authority signatures from other grants.
	SigningDomain = "gascity.session-authority-grant.v1\n"

	// HostTrustFileEnv names the controller-only trust-file setting.
	HostTrustFileEnv = "GC_SESSION_AUTHORITY_TRUST_FILE"

	// ProfileDesign is the default authority profile for an unprofiled session.
	ProfileDesign Profile = "design"
	// ProfileRouter identifies the generic routing authority profile.
	ProfileRouter Profile = "router"
	// ProfileWorker identifies the generic work execution authority profile.
	ProfileWorker Profile = "worker"
	// ProfileOperator identifies the generic operator authority profile.
	ProfileOperator Profile = "operator"

	// MetadataProfile stores the current authority profile.
	MetadataProfile = "gc.authority_profile"
	// MetadataAuthorization stores the accepted signed authorization.
	MetadataAuthorization = "gc.authority_authorization.v1"
	// MetadataTransitions stores the bounded transition audit history.
	MetadataTransitions = "gc.authority_transitions.v1"
	// MetadataTemplateOverrides stores provider launch-option overrides.
	MetadataTemplateOverrides = "template_overrides"
	// MetadataPermissionModeOption stores the flattened permission-mode override.
	MetadataPermissionModeOption = "opt_permission_mode"

	maxTrustBytes  = 1 << 20
	maxTokenBytes  = 16 << 10
	maxTransitions = 128
	maxGrantTTL    = 5 * time.Minute
)

var (
	// ErrUnavailable reports missing trust or required authority state.
	ErrUnavailable = errors.New("session authority unavailable")
	// ErrMalformed reports malformed trust, grants, or persisted proof.
	ErrMalformed = errors.New("session authority grant malformed")
	// ErrBadSignature reports a signature that does not verify.
	ErrBadSignature = errors.New("session authority grant signature invalid")
	// ErrUnauthorized reports a signer, profile, or authorization that is not trusted.
	ErrUnauthorized = errors.New("session authority grant unauthorized")
	// ErrTargetMismatch reports a grant that does not match the resolved target.
	ErrTargetMismatch = errors.New("session authority grant target mismatch")
	// ErrExpired reports a grant accepted outside its validity window.
	ErrExpired = errors.New("session authority grant expired")
	// ErrReplay reports reuse of an authorization after its accepted state changed.
	ErrReplay = errors.New("session authority grant replayed")
)

// Profile is a provider-independent controller authority profile.
type Profile string

// Valid reports whether p is a recognized generic authority profile.
func (p Profile) Valid() bool {
	switch p {
	case ProfileDesign, ProfileRouter, ProfileWorker, ProfileOperator:
		return true
	default:
		return false
	}
}

// Claims is the complete signed transition scope. PermissionMode is the exact
// provider schema value the profile authorizes for the next launch.
type Claims struct {
	SchemaVersion         string  `json:"schema_version"`
	AuthorizationID       string  `json:"authorization_id"`
	TokenID               string  `json:"jti"`
	KeyID                 string  `json:"kid"`
	Issuer                string  `json:"iss"`
	Subject               string  `json:"sub"`
	City                  string  `json:"city"`
	SessionID             string  `json:"session_id"`
	Generation            uint64  `json:"generation"`
	EffectiveConfigSHA256 string  `json:"effective_config_sha256"`
	FromProfile           Profile `json:"from_profile"`
	ToProfile             Profile `json:"to_profile"`
	PermissionMode        string  `json:"permission_mode"`
	IssuedAt              int64   `json:"iat"`
	ExpiresAt             int64   `json:"exp"`
}

// TrustedKey names an Ed25519 public key accepted by the host.
type TrustedKey struct {
	KeyID     string `json:"key_id"`
	PublicKey string `json:"public_key"`
}

// Authority grants one signer permission to authorize selected profiles.
type Authority struct {
	KeyID    string    `json:"key_id"`
	Issuer   string    `json:"issuer"`
	Subject  string    `json:"subject"`
	Profiles []Profile `json:"profiles"`
}

// TrustConfig is the strict host-owned session-authority trust document.
type TrustConfig struct {
	Keys                    []TrustedKey `json:"keys"`
	Authorities             []Authority  `json:"authorities"`
	RevokedAuthorizationIDs []string     `json:"revoked_authorization_ids,omitempty"`
}

// Expectation contains only server-resolved facts.
type Expectation struct {
	City                  string
	SessionID             string
	Generation            uint64
	EffectiveConfigSHA256 string
	FromProfile           Profile
	ToProfile             Profile
	PermissionMode        string
}

// Principal is the trusted signer identity copied into durable audit records.
type Principal struct {
	KeyID   string `json:"key_id"`
	Issuer  string `json:"issuer"`
	Subject string `json:"subject"`
}

// Authorization is the durable proof used again at launch. The compact token
// is public signed material, not a private credential.
type Authorization struct {
	Claims      Claims    `json:"claims"`
	Principal   Principal `json:"principal"`
	Token       string    `json:"token"`
	AcceptedAt  string    `json:"accepted_at"`
	ScopeSHA256 string    `json:"scope_sha256"`
}

// TransitionRecord is the append-only audit fact stored on the session bead.
// Denials intentionally omit the token and principal; accepted records point
// at the separately stored authorization proof.
type TransitionRecord struct {
	AttemptedAt           string    `json:"attempted_at"`
	Outcome               string    `json:"outcome"`
	Reason                string    `json:"reason"`
	SessionID             string    `json:"session_id"`
	Generation            uint64    `json:"generation"`
	EffectiveConfigSHA256 string    `json:"effective_config_sha256"`
	FromProfile           Profile   `json:"from_profile"`
	ToProfile             Profile   `json:"to_profile"`
	PermissionMode        string    `json:"permission_mode"`
	AuthorizationID       string    `json:"authorization_id,omitempty"`
	Principal             Principal `json:"principal,omitempty"`
}

type authorityID struct{ keyID, issuer, subject string }

// Verifier validates signed transitions against one host trust snapshot.
type Verifier struct {
	keys        map[string]ed25519.PublicKey
	authorities map[authorityID]map[Profile]struct{}
	revoked     map[string]struct{}
	now         func() time.Time
}

// NewVerifier validates cfg and constructs a verifier over an immutable copy.
func NewVerifier(cfg TrustConfig, now func() time.Time) (*Verifier, error) {
	if len(cfg.Keys) == 0 || len(cfg.Authorities) == 0 {
		return nil, ErrUnavailable
	}
	keys := make(map[string]ed25519.PublicKey, len(cfg.Keys))
	for _, entry := range cfg.Keys {
		if !validAtom(entry.KeyID) {
			return nil, ErrMalformed
		}
		raw, err := base64.StdEncoding.DecodeString(entry.PublicKey)
		if err != nil || len(raw) != ed25519.PublicKeySize || base64.StdEncoding.EncodeToString(raw) != entry.PublicKey {
			return nil, ErrMalformed
		}
		if _, duplicate := keys[entry.KeyID]; duplicate {
			return nil, ErrMalformed
		}
		for _, prior := range keys {
			if bytes.Equal(prior, raw) {
				return nil, ErrMalformed
			}
		}
		keys[entry.KeyID] = append(ed25519.PublicKey(nil), raw...)
	}
	authorities := make(map[authorityID]map[Profile]struct{}, len(cfg.Authorities))
	for _, entry := range cfg.Authorities {
		id := authorityID{entry.KeyID, entry.Issuer, entry.Subject}
		if _, ok := keys[id.keyID]; !ok || !validAtom(id.issuer) || !validAtom(id.subject) || len(entry.Profiles) == 0 {
			return nil, ErrMalformed
		}
		if _, duplicate := authorities[id]; duplicate {
			return nil, ErrMalformed
		}
		allowed := make(map[Profile]struct{}, len(entry.Profiles))
		for _, profile := range entry.Profiles {
			if !profile.Valid() {
				return nil, ErrMalformed
			}
			allowed[profile] = struct{}{}
		}
		authorities[id] = allowed
	}
	revoked := make(map[string]struct{}, len(cfg.RevokedAuthorizationIDs))
	for _, id := range cfg.RevokedAuthorizationIDs {
		if !validAtom(id) {
			return nil, ErrMalformed
		}
		if _, duplicate := revoked[id]; duplicate {
			return nil, ErrMalformed
		}
		revoked[id] = struct{}{}
	}
	if now == nil {
		now = time.Now
	}
	return &Verifier{keys: keys, authorities: authorities, revoked: revoked, now: now}, nil
}

// LoadHostVerifier reloads the host trust file for every decision, so removal
// of an authority takes effect before a later transition or launch.
func LoadHostVerifier() (*Verifier, error) {
	path := strings.TrimSpace(os.Getenv(HostTrustFileEnv))
	if path == "" {
		return nil, ErrUnavailable
	}
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("session authority trust path must be absolute: %w", ErrMalformed)
	}
	linkInfo, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("inspect session authority trust: %w: %w", ErrUnavailable, err)
	}
	if linkInfo.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("session authority trust must not be a symlink: %w", ErrMalformed)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open session authority trust: %w: %w", ErrUnavailable, err)
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil || !os.SameFile(linkInfo, info) {
		return nil, fmt.Errorf("session authority trust changed while opening: %w", ErrUnavailable)
	}
	if !info.Mode().IsRegular() || info.Mode()&0o022 != 0 {
		return nil, fmt.Errorf("session authority trust must be a non-writable regular file: %w", ErrMalformed)
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxTrustBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read session authority trust: %w: %w", ErrUnavailable, err)
	}
	if len(raw) > maxTrustBytes {
		return nil, fmt.Errorf("session authority trust file exceeds size limit: %w", ErrMalformed)
	}
	var cfg TrustConfig
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("decode session authority trust: %w: %w", ErrMalformed, err)
	}
	if err := ensureEOF(dec); err != nil {
		return nil, err
	}
	return NewVerifier(cfg, time.Now)
}

// EnforcementEnabled reports whether trusted startup configured the authority
// boundary. It supports a staged rollout: existing installations retain their
// prior provider-option behavior until the host explicitly installs trust.
func EnforcementEnabled() bool {
	return strings.TrimSpace(os.Getenv(HostTrustFileEnv)) != ""
}

// ProtectsMetadataMutation reports whether a generic bead writer must refuse
// key. Authority proof and history are always controller-owned. Launch options
// become controller-owned when host enforcement is enabled or the existing
// session already carries authority state, so disabling enforcement cannot
// erase the proof needed to explain an earlier protected launch.
func ProtectsMetadataMutation(key string, current map[string]string) bool {
	switch strings.TrimSpace(key) {
	case MetadataProfile, MetadataAuthorization, MetadataTransitions:
		return true
	case MetadataTemplateOverrides, MetadataPermissionModeOption:
		return EnforcementEnabled() || HasAuthorityMetadata(current)
	default:
		return false
	}
}

// HasAuthorityMetadata reports whether persisted session metadata carries any
// non-empty authority profile, authorization, or transition history.
func HasAuthorityMetadata(metadata map[string]string) bool {
	for _, key := range []string{MetadataProfile, MetadataAuthorization, MetadataTransitions} {
		if strings.TrimSpace(metadata[key]) != "" {
			return true
		}
	}
	return false
}

// SigningBytes returns the domain-separated canonical bytes for claims.
func SigningBytes(claims Claims) ([]byte, error) {
	if err := validateClaims(claims); err != nil {
		return nil, err
	}
	b, err := json.Marshal(claims)
	if err != nil {
		return nil, err
	}
	return append([]byte(SigningDomain), b...), nil
}

// Verify validates a new token against the exact controller-resolved scope.
func (v *Verifier) Verify(token string, want Expectation) (Authorization, error) {
	claims, sig, err := parseToken(token)
	if err != nil {
		return Authorization{}, err
	}
	if err := v.verifyClaims(claims, sig, want, true, time.Time{}); err != nil {
		return Authorization{}, err
	}
	accepted := v.now().UTC()
	scopeSHA, _ := expectationSHA(want)
	return Authorization{
		Claims:    claims,
		Principal: Principal{KeyID: claims.KeyID, Issuer: claims.Issuer, Subject: claims.Subject},
		Token:     token, AcceptedAt: accepted.Format(time.RFC3339Nano), ScopeSHA256: scopeSHA,
	}, nil
}

// VerifyStored rechecks signature, trusted authority, and the exact current
// launch scope. Grant expiry applies to the acceptance time, not every later
// launch; otherwise a valid durable profile would silently expire.
func (v *Verifier) VerifyStored(auth Authorization, want Expectation) error {
	accepted, err := time.Parse(time.RFC3339Nano, auth.AcceptedAt)
	if err != nil {
		return ErrMalformed
	}
	claims, sig, err := parseToken(auth.Token)
	if err != nil || claims != auth.Claims {
		return ErrMalformed
	}
	if auth.Principal != (Principal{KeyID: claims.KeyID, Issuer: claims.Issuer, Subject: claims.Subject}) {
		return ErrMalformed
	}
	scopeSHA, err := expectationSHA(want)
	if err != nil || auth.ScopeSHA256 != scopeSHA {
		return ErrTargetMismatch
	}
	return v.verifyClaims(claims, sig, want, false, accepted)
}

// EncodeAuthorization serializes an accepted authorization for durable storage.
func EncodeAuthorization(auth Authorization) (string, error) {
	if _, _, err := parseToken(auth.Token); err != nil {
		return "", err
	}
	b, err := json.Marshal(auth)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// DecodeAuthorization strictly decodes a stored authorization.
func DecodeAuthorization(raw string) (Authorization, error) {
	var auth Authorization
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&auth); err != nil || ensureEOF(dec) != nil {
		return Authorization{}, ErrMalformed
	}
	if _, _, err := parseToken(auth.Token); err != nil {
		return Authorization{}, err
	}
	return auth, nil
}

// AppendTransition returns a canonical bounded history. It never drops an old
// record; a full ledger fails closed and requires operator archival.
func AppendTransition(raw string, record TransitionRecord) (string, error) {
	records, err := decodeTransitions(raw)
	if err != nil {
		return "", err
	}
	if len(records) >= maxTransitions {
		return "", fmt.Errorf("session authority transition ledger is full: %w", ErrUnavailable)
	}
	if record.Outcome != "accepted" && record.Outcome != "denied" {
		return "", ErrMalformed
	}
	record.AttemptedAt = strings.TrimSpace(record.AttemptedAt)
	if _, err := time.Parse(time.RFC3339Nano, record.AttemptedAt); err != nil || strings.TrimSpace(record.Reason) == "" ||
		strings.TrimSpace(record.SessionID) == "" || record.Generation == 0 || !record.FromProfile.Valid() || strings.TrimSpace(record.PermissionMode) == "" {
		return "", ErrMalformed
	}
	if record.Outcome == "accepted" && (!validSHA(record.EffectiveConfigSHA256) || !record.ToProfile.Valid() || strings.TrimSpace(record.AuthorizationID) == "") {
		return "", ErrMalformed
	}
	records = append(records, record)
	b, err := json.Marshal(records)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// AuthorizationIDSeen reports whether the audit history already names an ID.
func AuthorizationIDSeen(raw, authorizationID string) (bool, error) {
	records, err := decodeTransitions(raw)
	if err != nil {
		return false, err
	}
	for _, record := range records {
		if record.AuthorizationID == authorizationID && authorizationID != "" {
			return true, nil
		}
	}
	return false, nil
}

func decodeTransitions(raw string) ([]TransitionRecord, error) {
	var records []TransitionRecord
	if strings.TrimSpace(raw) == "" {
		return records, nil
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&records); err != nil || ensureEOF(dec) != nil {
		return nil, ErrMalformed
	}
	return records, nil
}

// ProfileFromMetadata returns the stored valid profile or the design default.
func ProfileFromMetadata(metadata map[string]string) Profile {
	profile := Profile(strings.TrimSpace(metadata[MetadataProfile]))
	if profile.Valid() {
		return profile
	}
	return ProfileDesign
}

// VerifyLaunch reloads host trust and checks that a stored permission-mode
// override is backed by an accepted authorization for the exact session,
// generation, effective configuration, and current profile. A session with no
// authority-controlled permission mode remains a legacy-compatible launch.
func VerifyLaunch(city, sessionID string, generation uint64, effectiveConfigSHA256 string, profile Profile, permissionMode, rawAuthorization string, previouslyProtected bool) error {
	permissionMode = strings.TrimSpace(permissionMode)
	rawAuthorization = strings.TrimSpace(rawAuthorization)
	profileRaw := strings.TrimSpace(string(profile))
	protected := EnforcementEnabled() || previouslyProtected || rawAuthorization != "" || profileRaw != ""
	if permissionMode == "" && rawAuthorization == "" && profileRaw == "" && !previouslyProtected {
		return nil
	}
	if !protected {
		return nil
	}
	if permissionMode == "" || rawAuthorization == "" || !profile.Valid() {
		return ErrUnavailable
	}
	auth, err := DecodeAuthorization(rawAuthorization)
	if err != nil {
		return err
	}
	verifier, err := LoadHostVerifier()
	if err != nil {
		return err
	}
	want := Expectation{
		City: strings.TrimSpace(city), SessionID: strings.TrimSpace(sessionID), Generation: generation,
		EffectiveConfigSHA256: strings.ToLower(strings.TrimSpace(effectiveConfigSHA256)),
		FromProfile:           auth.Claims.FromProfile, ToProfile: profile, PermissionMode: permissionMode,
	}
	return verifier.VerifyStored(auth, want)
}

func (v *Verifier) verifyClaims(claims Claims, sig []byte, want Expectation, checkNow bool, accepted time.Time) error {
	if v == nil {
		return ErrUnavailable
	}
	pub, ok := v.keys[claims.KeyID]
	if !ok {
		return ErrUnauthorized
	}
	signing, err := SigningBytes(claims)
	if err != nil || !ed25519.Verify(pub, signing, sig) {
		return ErrBadSignature
	}
	allowed, ok := v.authorities[authorityID{claims.KeyID, claims.Issuer, claims.Subject}]
	if !ok {
		return ErrUnauthorized
	}
	if _, ok := allowed[claims.ToProfile]; !ok {
		return ErrUnauthorized
	}
	if _, revoked := v.revoked[claims.AuthorizationID]; revoked {
		return ErrUnauthorized
	}
	if !matches(claims, want) {
		return ErrTargetMismatch
	}
	when := accepted
	if checkNow {
		when = v.now().UTC()
	}
	if when.Before(time.Unix(claims.IssuedAt, 0)) || !when.Before(time.Unix(claims.ExpiresAt, 0)) {
		return ErrExpired
	}
	return nil
}

func matches(c Claims, w Expectation) bool {
	return c.City == strings.TrimSpace(w.City) && c.SessionID == strings.TrimSpace(w.SessionID) &&
		c.Generation == w.Generation && c.EffectiveConfigSHA256 == strings.ToLower(strings.TrimSpace(w.EffectiveConfigSHA256)) &&
		c.FromProfile == w.FromProfile && c.ToProfile == w.ToProfile && c.PermissionMode == strings.TrimSpace(w.PermissionMode)
}

func validateClaims(c Claims) error {
	if c.SchemaVersion != SchemaVersionV1 || !validAtom(c.AuthorizationID) || !validAtom(c.TokenID) ||
		!validAtom(c.KeyID) || !validAtom(c.Issuer) || !validAtom(c.Subject) || strings.TrimSpace(c.City) == "" ||
		strings.TrimSpace(c.SessionID) == "" || c.Generation == 0 || !validSHA(c.EffectiveConfigSHA256) ||
		!c.FromProfile.Valid() || !c.ToProfile.Valid() || strings.TrimSpace(c.PermissionMode) == "" || c.IssuedAt <= 0 || c.ExpiresAt <= c.IssuedAt ||
		time.Duration(c.ExpiresAt-c.IssuedAt)*time.Second > maxGrantTTL {
		return ErrMalformed
	}
	return nil
}

func parseToken(token string) (Claims, []byte, error) {
	if len(token) == 0 || len(token) > maxTokenBytes {
		return Claims{}, nil, ErrMalformed
	}
	p, s, ok := strings.Cut(token, ".")
	if !ok || p == "" || s == "" {
		return Claims{}, nil, ErrMalformed
	}
	payload, err := base64.RawURLEncoding.DecodeString(p)
	if err != nil {
		return Claims{}, nil, ErrMalformed
	}
	sig, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return Claims{}, nil, ErrMalformed
	}
	var claims Claims
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&claims); err != nil || ensureEOF(dec) != nil || validateClaims(claims) != nil {
		return Claims{}, nil, ErrMalformed
	}
	return claims, sig, nil
}

func expectationSHA(w Expectation) (string, error) {
	if strings.TrimSpace(w.City) == "" || strings.TrimSpace(w.SessionID) == "" || w.Generation == 0 ||
		!validSHA(w.EffectiveConfigSHA256) || !w.FromProfile.Valid() || !w.ToProfile.Valid() || strings.TrimSpace(w.PermissionMode) == "" {
		return "", ErrMalformed
	}
	b, _ := json.Marshal(w)
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:]), nil
}

func validSHA(s string) bool {
	if len(s) != 64 || s != strings.ToLower(s) {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

func validAtom(s string) bool {
	s = strings.TrimSpace(s)
	return s != "" && len(s) <= 256 && !strings.ContainsAny(s, "\x00\r\n\t")
}

func ensureEOF(dec *json.Decoder) error {
	var extra any
	if err := dec.Decode(&extra); err == nil {
		return ErrMalformed
	} else if !errors.Is(err, io.EOF) {
		return ErrMalformed
	}
	return nil
}
