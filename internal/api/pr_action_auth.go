package api

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

// PRActionScopePrepare and the related constants name the actions that may be
// granted by a trusted human authority.
const (
	// PRHumanTrustEnv is read only by the trusted supervisor process. The
	// corresponding private keys must be held by the named human grant service,
	// never by workers or by the city-write grant minter.
	PRHumanTrustEnv = "GC_PR_HUMAN_TRUST"

	PRActionScopePrepare     = "pr.prepare"
	PRActionScopeReview      = "pr.review"
	PRActionScopeMerge       = "pr.merge"
	PRActionScopePolicyWrite = "pr.policy.write"

	maxPRHumanGrantBytes = 16 << 10
	maxPRHumanGrantTTL   = 2 * time.Minute
	prHumanGrantSkew     = 30 * time.Second
)

// ErrPRHumanGrantMalformed and the related errors describe rejected human
// grants and unavailable human authority.
var (
	ErrPRHumanGrantMalformed       = errors.New("PR human grant is malformed")
	ErrPRHumanGrantUnknownKey      = errors.New("PR human grant signing key is not trusted")
	ErrPRHumanGrantBadSignature    = errors.New("PR human grant signature is invalid")
	ErrPRHumanGrantUnknownSubject  = errors.New("PR human grant subject is not authorized for this signing key")
	ErrPRHumanGrantScope           = errors.New("PR human grant does not authorize this action")
	ErrPRHumanGrantTarget          = errors.New("PR human grant target does not match the requested action")
	ErrPRHumanGrantExpired         = errors.New("PR human grant is expired or not yet valid")
	ErrPRHumanGrantUnavailable     = errors.New("PR human grant authority is not configured")
	ErrPRHumanGrantSharedWorkerKey = errors.New("PR human grant key must be separate from city-write keys")
)

// PRHumanTrustConfig is operator-owned trust material for signed, exact-action
// human grants. Public keys and subject/scope bindings are separate entries on
// purpose: a signature proves which key signed, while Authorities says which
// exact issuer+subject pair that key may represent and for which actions.
type PRHumanTrustConfig struct {
	Keys        []PRHumanGrantKey  `json:"keys"`
	Authorities []PRHumanAuthority `json:"authorities"`
}

// PRHumanGrantKey binds a stable key ID to a trusted Ed25519 public key.
type PRHumanGrantKey struct {
	KeyID     string `json:"key_id"`
	PublicKey string `json:"public_key"`
}

// PRHumanAuthority binds one exact signer identity to permitted action scopes.
type PRHumanAuthority struct {
	KeyID   string   `json:"key_id"`
	Issuer  string   `json:"issuer"`
	Subject string   `json:"subject"`
	Scopes  []string `json:"scopes"`
}

// PRHumanGrantClaims is the signed wire payload. Every field that can affect a
// privileged PR action is bound by the human signature, including its exact
// revision, the frozen policy version, and the durable idempotency key.
type PRHumanGrantClaims struct {
	KeyID          string `json:"kid"`
	Issuer         string `json:"iss"`
	Subject        string `json:"sub"`
	Scope          string `json:"scope"`
	City           string `json:"city"`
	Owner          string `json:"owner"`
	Repo           string `json:"repo"`
	PullRequest    int    `json:"pr"`
	HeadSHA        string `json:"head_sha"`
	BaseSHA        string `json:"base_sha"`
	PolicyVersion  string `json:"policy_version"`
	WorkID         string `json:"work_id"`
	IdempotencyKey string `json:"idempotency_key"`
	IssuedAt       int64  `json:"iat"`
	ExpiresAt      int64  `json:"exp"`
	TokenID        string `json:"jti"`
}

// PRHumanGrantExpectation is assembled from server-resolved city state and
// the typed request. A grant is valid only for this entire tuple.
type PRHumanGrantExpectation struct {
	City           string
	Scope          string
	Owner          string
	Repo           string
	PullRequest    int
	HeadSHA        string
	BaseSHA        string
	PolicyVersion  string
	WorkID         string
	IdempotencyKey string
}

// VerifiedPRPrincipal is created only after signature, authority, scope,
// expiry, and target checks pass. Callers must authorize with this value, never
// with a client-supplied role or subject field.
type VerifiedPRPrincipal struct {
	KeyID   string
	Issuer  string
	Subject string
	Scopes  []string
}

// Allows reports whether this verified principal has scope.
func (p VerifiedPRPrincipal) Allows(scope string) bool {
	for _, got := range p.Scopes {
		if got == scope {
			return true
		}
	}
	return false
}

type prHumanAuthorityKey struct {
	keyID   string
	issuer  string
	subject string
}

// PRHumanGrantVerifier accepts keys only from the dedicated human trust
// configuration. The constructor rejects any key id or public key also used
// by the city-write verifier, so a worker-capable city-write signer cannot
// acquire human authority merely by adding a "sub" claim.
type PRHumanGrantVerifier struct {
	keys        map[string]ed25519.PublicKey
	authorities map[prHumanAuthorityKey]map[string]struct{}
	now         func() time.Time
}

// NewPRHumanGrantVerifier validates and copies trusted keys and exact subject
// bindings. workerKeys must be the active city-write verifier key set; pass nil
// only when the server has no city-write verifier configured.
func NewPRHumanGrantVerifier(cfg PRHumanTrustConfig, workerKeys map[string]ed25519.PublicKey, now func() time.Time) (*PRHumanGrantVerifier, error) {
	if len(cfg.Keys) == 0 || len(cfg.Authorities) == 0 {
		return nil, ErrPRHumanGrantUnavailable
	}
	keys := make(map[string]ed25519.PublicKey, len(cfg.Keys))
	for _, entry := range cfg.Keys {
		kid := strings.TrimSpace(entry.KeyID)
		if kid == "" {
			return nil, fmt.Errorf("%w: key_id is required", ErrPRHumanGrantMalformed)
		}
		if _, exists := keys[kid]; exists {
			return nil, fmt.Errorf("%w: duplicate key_id %q", ErrPRHumanGrantMalformed, kid)
		}
		raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(entry.PublicKey))
		if err != nil || len(raw) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("%w: key %q must contain a standard-base64 ed25519 public key", ErrPRHumanGrantMalformed, kid)
		}
		pub := ed25519.PublicKey(append([]byte(nil), raw...))
		for workerID, workerKey := range workerKeys {
			if kid == workerID || bytes.Equal(pub, workerKey) {
				return nil, fmt.Errorf("%w: key %q overlaps city-write key %q", ErrPRHumanGrantSharedWorkerKey, kid, workerID)
			}
		}
		keys[kid] = pub
	}

	authorities := make(map[prHumanAuthorityKey]map[string]struct{}, len(cfg.Authorities))
	knownScopes := map[string]struct{}{
		PRActionScopePrepare: {}, PRActionScopeReview: {}, PRActionScopeMerge: {}, PRActionScopePolicyWrite: {},
	}
	for _, entry := range cfg.Authorities {
		binding := prHumanAuthorityKey{
			keyID: strings.TrimSpace(entry.KeyID), issuer: strings.TrimSpace(entry.Issuer), subject: strings.TrimSpace(entry.Subject),
		}
		if _, ok := keys[binding.keyID]; !ok || binding.issuer == "" || binding.subject == "" || len(entry.Scopes) == 0 {
			return nil, fmt.Errorf("%w: each authority needs a known key_id, exact issuer, exact subject, and at least one scope", ErrPRHumanGrantMalformed)
		}
		if _, duplicate := authorities[binding]; duplicate {
			return nil, fmt.Errorf("%w: duplicate authority binding for %q/%q/%q", ErrPRHumanGrantMalformed, binding.keyID, binding.issuer, binding.subject)
		}
		scopes := make(map[string]struct{}, len(entry.Scopes))
		for _, rawScope := range entry.Scopes {
			scope := strings.TrimSpace(rawScope)
			if _, ok := knownScopes[scope]; !ok {
				return nil, fmt.Errorf("%w: unknown scope %q", ErrPRHumanGrantMalformed, rawScope)
			}
			if _, duplicate := scopes[scope]; duplicate {
				return nil, fmt.Errorf("%w: duplicate scope %q", ErrPRHumanGrantMalformed, scope)
			}
			scopes[scope] = struct{}{}
		}
		authorities[binding] = scopes
	}
	if now == nil {
		now = time.Now
	}
	return &PRHumanGrantVerifier{keys: keys, authorities: authorities, now: now}, nil
}

// ResolvePRHumanGrantVerifier parses the root-controlled trust document. An
// absent document leaves the feature disabled; malformed configured trust is a
// startup error. The source of rawConfig must be outside candidate-writable
// city state (for example a supervisor environment file managed by the host).
func ResolvePRHumanGrantVerifier(rawConfig string, workerKeys map[string]ed25519.PublicKey, now func() time.Time) (*PRHumanGrantVerifier, error) {
	rawConfig = strings.TrimSpace(rawConfig)
	if rawConfig == "" {
		return nil, nil
	}
	var cfg PRHumanTrustConfig
	dec := json.NewDecoder(strings.NewReader(rawConfig))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return nil, fmt.Errorf("%s: decode trust configuration: %w", PRHumanTrustEnv, err)
	}
	if err := ensureJSONEOF(dec); err != nil {
		return nil, fmt.Errorf("%s: decode trust configuration: %w", PRHumanTrustEnv, err)
	}
	return NewPRHumanGrantVerifier(cfg, workerKeys, now)
}

// Verify returns an authenticated, exact-subject principal only when the
// signed grant names a configured key/issuer/subject binding with the required
// scope and matches every expected action field.
func (v *PRHumanGrantVerifier) Verify(token string, want PRHumanGrantExpectation) (VerifiedPRPrincipal, error) {
	if v == nil {
		return VerifiedPRPrincipal{}, ErrPRHumanGrantUnavailable
	}
	if len(token) > maxPRHumanGrantBytes {
		return VerifiedPRPrincipal{}, ErrPRHumanGrantMalformed
	}
	payload, sig, ok := strings.Cut(strings.TrimSpace(token), ".")
	if !ok || payload == "" || sig == "" {
		return VerifiedPRPrincipal{}, ErrPRHumanGrantMalformed
	}
	decoded, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return VerifiedPRPrincipal{}, fmt.Errorf("%w: payload encoding", ErrPRHumanGrantMalformed)
	}
	signature, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil || len(signature) != ed25519.SignatureSize {
		return VerifiedPRPrincipal{}, fmt.Errorf("%w: signature encoding", ErrPRHumanGrantMalformed)
	}
	// Select the verification key from the trusted human keyring before parsing
	// the rest of the payload. A valid city-write token carrying human-looking
	// extra claims therefore fails as an unknown human key.
	var keyIDClaim struct {
		KeyID string `json:"kid"`
	}
	if err := json.Unmarshal(decoded, &keyIDClaim); err != nil || keyIDClaim.KeyID == "" {
		return VerifiedPRPrincipal{}, ErrPRHumanGrantMalformed
	}
	pub, known := v.keys[keyIDClaim.KeyID]
	if !known {
		return VerifiedPRPrincipal{}, ErrPRHumanGrantUnknownKey
	}
	if !ed25519.Verify(pub, decoded, signature) {
		return VerifiedPRPrincipal{}, ErrPRHumanGrantBadSignature
	}
	var claims PRHumanGrantClaims
	dec := json.NewDecoder(bytes.NewReader(decoded))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&claims); err != nil {
		return VerifiedPRPrincipal{}, fmt.Errorf("%w: %w", ErrPRHumanGrantMalformed, err)
	}
	if err := ensureJSONEOF(dec); err != nil {
		return VerifiedPRPrincipal{}, fmt.Errorf("%w: %w", ErrPRHumanGrantMalformed, err)
	}
	if claims.KeyID != keyIDClaim.KeyID || claims.Issuer == "" || claims.Subject == "" || claims.TokenID == "" {
		return VerifiedPRPrincipal{}, ErrPRHumanGrantMalformed
	}
	binding := prHumanAuthorityKey{keyID: claims.KeyID, issuer: claims.Issuer, subject: claims.Subject}
	scopes, allowed := v.authorities[binding]
	if !allowed {
		return VerifiedPRPrincipal{}, ErrPRHumanGrantUnknownSubject
	}
	if _, ok := scopes[claims.Scope]; !ok || claims.Scope != want.Scope {
		return VerifiedPRPrincipal{}, ErrPRHumanGrantScope
	}
	if err := validatePRHumanGrantTimes(claims, v.now()); err != nil {
		return VerifiedPRPrincipal{}, err
	}
	if want.City == "" || want.Scope == "" || want.Owner == "" || want.Repo == "" || want.PullRequest <= 0 || want.HeadSHA == "" || want.BaseSHA == "" || want.PolicyVersion == "" || want.WorkID == "" || want.IdempotencyKey == "" {
		return VerifiedPRPrincipal{}, fmt.Errorf("%w: incomplete server expectation", ErrPRHumanGrantTarget)
	}
	if claims.City != want.City || claims.Scope != want.Scope || claims.Owner != want.Owner || claims.Repo != want.Repo || claims.PullRequest != want.PullRequest || claims.HeadSHA != want.HeadSHA || claims.BaseSHA != want.BaseSHA || claims.PolicyVersion != want.PolicyVersion || claims.WorkID != want.WorkID || claims.IdempotencyKey != want.IdempotencyKey {
		return VerifiedPRPrincipal{}, ErrPRHumanGrantTarget
	}
	principalScopes := make([]string, 0, len(scopes))
	for scope := range scopes {
		principalScopes = append(principalScopes, scope)
	}
	sort.Strings(principalScopes)
	return VerifiedPRPrincipal{KeyID: claims.KeyID, Issuer: claims.Issuer, Subject: claims.Subject, Scopes: principalScopes}, nil
}

// Authorize checks the complete verified identity tuple again at the policy
// boundary. A subject by itself is never an authority key.
func (v *PRHumanGrantVerifier) Authorize(principal VerifiedPRPrincipal, scope string) error {
	if v == nil {
		return ErrPRHumanGrantUnavailable
	}
	binding := prHumanAuthorityKey{
		keyID: strings.TrimSpace(principal.KeyID), issuer: strings.TrimSpace(principal.Issuer),
		subject: strings.TrimSpace(principal.Subject),
	}
	scopes, ok := v.authorities[binding]
	if !ok {
		return ErrPRHumanGrantUnknownSubject
	}
	if _, ok := scopes[scope]; !ok {
		return ErrPRHumanGrantScope
	}
	return nil
}

func validatePRHumanGrantTimes(claims PRHumanGrantClaims, now time.Time) error {
	if claims.IssuedAt <= 0 || claims.ExpiresAt <= claims.IssuedAt || claims.TokenID == "" {
		return ErrPRHumanGrantMalformed
	}
	iat := time.Unix(claims.IssuedAt, 0)
	exp := time.Unix(claims.ExpiresAt, 0)
	if exp.Sub(iat) > maxPRHumanGrantTTL || now.After(exp.Add(prHumanGrantSkew)) || now.Add(prHumanGrantSkew).Before(iat) {
		return ErrPRHumanGrantExpired
	}
	return nil
}

func ensureJSONEOF(dec *json.Decoder) error {
	var extra any
	if err := dec.Decode(&extra); err == nil {
		return errors.New("trailing JSON value")
	} else if !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}
