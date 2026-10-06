package worklifecycle

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/config"
)

const recoveryEscalationRequestDomain = "gascity.lifecycle.recovery-escalation-request.v1\n"

// RecoveryEscalationRequest authorizes one durable human-escalation record for
// an exhausted lifecycle recovery budget. Its revision and target are signed.
type RecoveryEscalationRequest struct {
	Version          int    `json:"version"`
	RequestID        string `json:"request_id"`
	Scope            string `json:"scope"`
	WorkItemID       string `json:"work_item_id"`
	ExpectedRevision int64  `json:"expected_revision"`
	Target           string `json:"target"`
	IssuedAt         string `json:"issued_at"`
	ExpiresAt        string `json:"expires_at"`
	AuthorizedBy     string `json:"authorized_by"`
	Signature        string `json:"signature"`
}

// RecoveryEscalationSigningBytes returns the domain-separated canonical
// payload. The detached signature is excluded.
func RecoveryEscalationSigningBytes(request RecoveryEscalationRequest) ([]byte, error) {
	request.Signature = ""
	encoded, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	return append([]byte(recoveryEscalationRequestDomain), encoded...), nil
}

// SignRecoveryEscalationRequest signs an exhaustion escalation request with
// its separately held Ed25519 key.
func SignRecoveryEscalationRequest(request RecoveryEscalationRequest, privateKey ed25519.PrivateKey) (RecoveryEscalationRequest, error) {
	if len(privateKey) != ed25519.PrivateKeySize {
		return RecoveryEscalationRequest{}, fmt.Errorf("recovery escalation key must be an Ed25519 private key: %w", ErrRecoveryEscalationRequestInvalid)
	}
	payload, err := RecoveryEscalationSigningBytes(request)
	if err != nil {
		return RecoveryEscalationRequest{}, fmt.Errorf("encode recovery escalation request: %w", err)
	}
	request.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(privateKey, payload))
	return request, nil
}

// VerifyRecoveryEscalationRequest checks the signer, exhaustion action,
// configured recipient, exact source binding, and current authorization window.
func VerifyRecoveryEscalationRequest(request RecoveryEscalationRequest, cfg config.LifecycleConfig, now time.Time) (string, error) {
	return verifyRecoveryEscalationRequest(request, cfg, now, true)
}

// VerifyRecoveryEscalationRequestProof verifies the signed request structure
// without checking freshness. Use it only when reading an already persisted
// transition receipt for replay or ancestry validation.
func VerifyRecoveryEscalationRequestProof(request RecoveryEscalationRequest, cfg config.LifecycleConfig) (string, error) {
	return verifyRecoveryEscalationRequest(request, cfg, time.Time{}, false)
}

func verifyRecoveryEscalationRequest(request RecoveryEscalationRequest, cfg config.LifecycleConfig, now time.Time, requireCurrentWindow bool) (string, error) {
	if !cfg.AdmissionEnabled || !cfg.RecoveryEnabled || (requireCurrentWindow && now.IsZero()) {
		return "", fmt.Errorf("lifecycle recovery escalation is disabled or time is unavailable: %w", ErrRecoveryEscalationRequestInvalid)
	}
	if request.Version != 1 || !validSessionRequestID(request.RequestID) ||
		!validRecoveryToken(request.Scope, 500) || !validRecoveryToken(request.WorkItemID, 200) ||
		request.ExpectedRevision == 0 || !validRecoveryToken(request.Target, 500) ||
		request.Target != cfg.EscalationTarget || !validRecoveryToken(request.AuthorizedBy, 200) {
		return "", ErrRecoveryEscalationRequestInvalid
	}
	authority, ok := cfg.RecoveryAuthorities[request.AuthorizedBy]
	if !ok || !containsExact(authority.Actions, config.LifecycleRecoveryActionEscalate) || !containsExact(authority.Scopes, request.Scope) {
		return "", fmt.Errorf("recovery authority does not grant escalation in this scope: %w", ErrRecoveryEscalationRequestInvalid)
	}
	issuedAt, errIssued := time.Parse(time.RFC3339Nano, request.IssuedAt)
	expiresAt, errExpires := time.Parse(time.RFC3339Nano, request.ExpiresAt)
	if errIssued != nil || errExpires != nil || !issuedAt.Before(expiresAt) ||
		issuedAt.UTC().Format(time.RFC3339Nano) != request.IssuedAt || expiresAt.UTC().Format(time.RFC3339Nano) != request.ExpiresAt ||
		(requireCurrentWindow && (now.Before(issuedAt) || !now.Before(expiresAt))) {
		return "", fmt.Errorf("recovery escalation validity window is invalid or expired: %w", ErrRecoveryEscalationRequestInvalid)
	}
	publicKey, err := base64.StdEncoding.DecodeString(strings.TrimSpace(authority.PublicKey))
	if err != nil || len(publicKey) != ed25519.PublicKeySize {
		return "", fmt.Errorf("configured recovery authority key is invalid: %w", ErrRecoveryEscalationRequestInvalid)
	}
	signature, err := base64.StdEncoding.DecodeString(request.Signature)
	if err != nil || len(signature) != ed25519.SignatureSize {
		return "", fmt.Errorf("recovery escalation signature is invalid: %w", ErrRecoveryEscalationRequestInvalid)
	}
	payload, err := RecoveryEscalationSigningBytes(request)
	if err != nil || !ed25519.Verify(ed25519.PublicKey(publicKey), payload, signature) {
		return "", fmt.Errorf("recovery escalation signature is not trusted: %w", ErrRecoveryEscalationRequestInvalid)
	}
	digest, err := RecoveryEscalationRequestDigest(request)
	if err != nil {
		return "", errors.Join(ErrRecoveryEscalationRequestInvalid, err)
	}
	return digest, nil
}

// RecoveryEscalationRequestDigest binds replay identity to every request field,
// including its detached signature.
func RecoveryEscalationRequestDigest(request RecoveryEscalationRequest) (string, error) {
	encoded, err := json.Marshal(request)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func recoveryEscalationID(request RecoveryEscalationRequest) (string, error) {
	digest, err := RecoveryEscalationRequestDigest(request)
	if err != nil {
		return "", err
	}
	material := recoveryEscalationRequestDomain + request.Scope + "\x00" + request.WorkItemID + "\x00" + request.RequestID + "\x00" + digest
	derived := sha256.Sum256([]byte(material))
	return hex.EncodeToString(derived[:16]), nil
}

// BuildRecoveryEscalationPatch constructs the exact append-only recovery-state
// patch for an authorized escalation request. Apply verifies the request
// signature and currentness before issuing a protected Beads permit.
func BuildRecoveryEscalationPatch(rawState string, request RecoveryEscalationRequest) (SourceWorkPatch, error) {
	if rawState == "" || !validRecoveryToken(request.WorkItemID, 200) || !validRecoveryToken(request.Scope, 500) ||
		!validSessionRequestID(request.RequestID) || !validRecoveryToken(request.Target, 500) {
		return SourceWorkPatch{}, ErrRecoveryEscalationRequestInvalid
	}
	state, err := decodeCanonicalRecoveryState(rawState, request.WorkItemID, request.Scope)
	if err != nil {
		return SourceWorkPatch{}, err
	}
	if len(state.Attempts) != MaxRecoveryAttempts || state.Escalation != nil {
		return SourceWorkPatch{}, fmt.Errorf("recovery escalation requires exactly %d attempts and no prior escalation: %w", MaxRecoveryAttempts, ErrRecoveryEscalationRequestInvalid)
	}
	issuedAt, err := time.Parse(time.RFC3339Nano, request.IssuedAt)
	if err != nil || issuedAt.UTC().Format(time.RFC3339Nano) != request.IssuedAt {
		return SourceWorkPatch{}, ErrRecoveryEscalationRequestInvalid
	}
	next := cloneRecoveryState(state)
	escalationID, err := recoveryEscalationID(request)
	if err != nil {
		return SourceWorkPatch{}, err
	}
	next.Escalation = &RecoveryEscalation{
		ID: escalationID, Target: request.Target, RequestedAt: request.IssuedAt,
		DedupKey: recoveryEscalationDedupKey(request.Scope, request.WorkItemID, escalationID), Request: &request,
	}
	encoded, err := json.Marshal(next)
	if err != nil {
		return SourceWorkPatch{}, err
	}
	expected := rawState
	return SourceWorkPatch{Metadata: map[string]MetadataStringPatch{
		beadmeta.LifecycleRecoveryStateMetadataKey: {Expected: &expected, Value: string(encoded)},
	}}, nil
}
