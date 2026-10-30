package sessionauthority

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"path/filepath"
	"strings"
	"time"
)

// NativeBindingSchemaV1 identifies a host-signed native capture attestation.
const NativeBindingSchemaV1 = "gc.native-binding.v1"

const nativeBindingSigningDomain = "gascity.native-binding.v1\n"

// NativeBindingTarget binds a fresh capture to one authoritative session row,
// native conversation, workspace and effective launch policy. Callers derive
// this target from trusted current state, never from the received token.
type NativeBindingTarget struct {
	City                  string `json:"city"`
	SessionID             string `json:"session_id"`
	SessionRevision       int64  `json:"session_revision"`
	Provider              string `json:"provider"`
	Transport             string `json:"transport"`
	SessionKey            string `json:"session_key"`
	Generation            uint64 `json:"generation"`
	InstanceToken         string `json:"instance_token"`
	ContinuationEpoch     uint64 `json:"continuation_epoch"`
	WorkDir               string `json:"work_dir"`
	EffectiveConfigSHA256 string `json:"effective_config_sha256"`
	PermissionMode        string `json:"permission_mode"`
	CaptureSource         string `json:"capture_source"`
}

// NativeBindingClaims is the complete signed capture statement. Only an
// already trusted host principal may issue it, after authenticating the native
// capture channel. This package does not authenticate an env/stdin hook, inspect
// a model conversation, mint credentials, persist a key, or admit executable work.
type NativeBindingClaims struct {
	SchemaVersion   string              `json:"schema_version"`
	BindingID       string              `json:"binding_id"`
	AuthorizationID string              `json:"authorization_id"`
	KeyID           string              `json:"kid"`
	Issuer          string              `json:"iss"`
	Subject         string              `json:"sub"`
	Target          NativeBindingTarget `json:"target"`
	IssuedAt        int64               `json:"iat"`
	ExpiresAt       int64               `json:"exp"`
}

// NativeBindingReceipt records the verified statement, not a task or runtime
// launch permit. Exact replay of a fresh statement is allowed; exclusive launch
// consumption and revocation require a separate provider-enforced capability.
type NativeBindingReceipt struct {
	BindingID       string
	AuthorizationID string
	Target          NativeBindingTarget
	Principal       Principal
	CapturedAt      time.Time
	ExpiresAt       time.Time
}

// NativeBindingSigningBytes returns domain-separated canonical capture bytes.
// A signing host must verify its native capture channel before signing them.
func NativeBindingSigningBytes(c NativeBindingClaims) ([]byte, error) {
	if !validNativeBindingClaims(c) {
		return nil, ErrMalformed
	}
	raw, err := json.Marshal(c)
	if err != nil {
		return nil, err
	}
	return append([]byte(nativeBindingSigningDomain), raw...), nil
}

// VerifyNativeBinding requires a fresh exact capture and a still-trusted signed
// worker launch authorization from the same host principal. Unlike a stored
// launch authorization, the capture expires at use time. Callers reload host
// trust for each decision and must not cache this receipt as admission proof.
func (v *Verifier) VerifyNativeBinding(token string, want NativeBindingTarget, auth Authorization) (NativeBindingReceipt, error) {
	empty := NativeBindingReceipt{}
	if v == nil {
		return empty, ErrUnavailable
	}
	c, sig, err := parseNativeBinding(token)
	if err != nil {
		return empty, err
	}
	pub, ok := v.keys[c.KeyID]
	if !ok {
		return empty, ErrUnauthorized
	}
	signing, err := NativeBindingSigningBytes(c)
	if err != nil || !ed25519.Verify(pub, signing, sig) {
		return empty, ErrBadSignature
	}
	principal := Principal{KeyID: c.KeyID, Issuer: c.Issuer, Subject: c.Subject}
	allowed := v.authorities[authorityID{c.KeyID, c.Issuer, c.Subject}]
	if _, ok := allowed[ProfileWorker]; !ok || principal != auth.Principal {
		return empty, ErrUnauthorized
	}
	if c.Target != want || c.AuthorizationID != auth.Claims.AuthorizationID {
		return empty, ErrTargetMismatch
	}
	if err := v.VerifyStored(auth, Expectation{
		City: want.City, SessionID: want.SessionID, Generation: want.Generation,
		EffectiveConfigSHA256: want.EffectiveConfigSHA256, FromProfile: auth.Claims.FromProfile,
		ToProfile: ProfileWorker, PermissionMode: want.PermissionMode,
	}); err != nil {
		return empty, err
	}
	now := v.now().UTC()
	if now.Before(time.Unix(c.IssuedAt, 0)) || !now.Before(time.Unix(c.ExpiresAt, 0)) {
		return empty, ErrExpired
	}
	return NativeBindingReceipt{
		BindingID: c.BindingID, AuthorizationID: c.AuthorizationID, Target: c.Target,
		Principal: principal, CapturedAt: time.Unix(c.IssuedAt, 0).UTC(), ExpiresAt: time.Unix(c.ExpiresAt, 0).UTC(),
	}, nil
}

func parseNativeBinding(token string) (NativeBindingClaims, []byte, error) {
	if len(token) == 0 || len(token) > maxTokenBytes {
		return NativeBindingClaims{}, nil, ErrMalformed
	}
	payload, signature, ok := strings.Cut(token, ".")
	if !ok || payload == "" || signature == "" {
		return NativeBindingClaims{}, nil, ErrMalformed
	}
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil || base64.RawURLEncoding.EncodeToString(raw) != payload {
		return NativeBindingClaims{}, nil, ErrMalformed
	}
	sig, err := base64.RawURLEncoding.DecodeString(signature)
	if err != nil || len(sig) != ed25519.SignatureSize || base64.RawURLEncoding.EncodeToString(sig) != signature {
		return NativeBindingClaims{}, nil, ErrMalformed
	}
	var c NativeBindingClaims
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil || ensureEOF(dec) != nil || !validNativeBindingClaims(c) {
		return NativeBindingClaims{}, nil, ErrMalformed
	}
	return c, sig, nil
}

func validNativeBindingClaims(c NativeBindingClaims) bool {
	if c.SchemaVersion != NativeBindingSchemaV1 || c.IssuedAt <= 0 || c.ExpiresAt <= c.IssuedAt || c.ExpiresAt-c.IssuedAt > int64(maxGrantTTL/time.Second) {
		return false
	}
	t := c.Target
	for _, value := range []string{c.BindingID, c.AuthorizationID, c.KeyID, c.Issuer, c.Subject, t.City, t.SessionID, t.Provider, t.Transport, t.InstanceToken, t.CaptureSource, t.PermissionMode} {
		if !validAtom(value) || strings.TrimSpace(value) != value {
			return false
		}
	}
	if t.SessionRevision == 0 || t.Generation == 0 || t.ContinuationEpoch == 0 || !validSHA(t.EffectiveConfigSHA256) ||
		!filepath.IsAbs(t.WorkDir) || filepath.Clean(t.WorkDir) != t.WorkDir || strings.ContainsAny(t.WorkDir, "\x00\r\n\t") ||
		t.SessionKey == "" || len(t.SessionKey) > 512 || strings.HasPrefix(t.SessionKey, "-") {
		return false
	}
	for _, r := range t.SessionKey {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', strings.ContainsRune("-_./:", r):
		default:
			return false
		}
	}
	return true
}
