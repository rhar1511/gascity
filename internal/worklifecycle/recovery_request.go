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
	"unicode/utf8"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

const recoveryRequestDomain = "gascity.lifecycle.recovery-request.v1\n"

var (
	// ErrRecoveryRequestInvalid means a signed recovery request is malformed,
	// outside its validity window, or not authorized by the configured signer.
	ErrRecoveryRequestInvalid = errors.New("recovery request is invalid")
	// ErrRecoveryRequestConflict means the same request ID was already used with
	// different signed content.
	ErrRecoveryRequestConflict = errors.New("recovery request identity or content conflicts")
	// ErrRecoveryWorkStale means current work or owner evidence no longer matches
	// the signed recovery request.
	ErrRecoveryWorkStale = errors.New("recovery request no longer matches current work ownership")
	// ErrRecoveryIntentUnavailable means the configured store cannot persist the
	// held recovery intent with the required conditional-write guarantees.
	ErrRecoveryIntentUnavailable = errors.New("durable recovery intent storage is unavailable")
)

// RecoveryRequest is a separately authorized, signed request to nudge one
// exact live execution. Revision and ownership fields are copied as opaque
// values from authoritative reads; the controller compares them for equality
// and never infers an adjacent generation or revision.
type RecoveryRequest struct {
	Version           int    `json:"version"`
	RequestID         string `json:"request_id"`
	Action            string `json:"action"`
	Scope             string `json:"scope"`
	WorkItemID        string `json:"work_item_id"`
	ExpectedRevision  int64  `json:"expected_revision"`
	Owner             string `json:"owner"`
	ClaimGeneration   string `json:"claim_generation"`
	SessionID         string `json:"session_id"`
	SessionGeneration string `json:"session_generation"`
	Message           string `json:"message"`
	IssuedAt          string `json:"issued_at"`
	ExpiresAt         string `json:"expires_at"`
	AuthorizedBy      string `json:"authorized_by"`
	Signature         string `json:"signature"`
}

// RecoveryRequestSigningBytes returns a domain-separated canonical JSON
// payload. The signature itself is excluded.
func RecoveryRequestSigningBytes(request RecoveryRequest) ([]byte, error) {
	request.Signature = ""
	payload, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	return append([]byte(recoveryRequestDomain), payload...), nil
}

// SignRecoveryRequest signs a request with a separately held Ed25519 key.
func SignRecoveryRequest(request RecoveryRequest, privateKey ed25519.PrivateKey) (RecoveryRequest, error) {
	if len(privateKey) != ed25519.PrivateKeySize {
		return RecoveryRequest{}, fmt.Errorf("recovery signing key must be an Ed25519 private key: %w", ErrRecoveryRequestInvalid)
	}
	payload, err := RecoveryRequestSigningBytes(request)
	if err != nil {
		return RecoveryRequest{}, fmt.Errorf("encode recovery request: %w", err)
	}
	request.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(privateKey, payload))
	return request, nil
}

// VerifyRecoveryRequest verifies the exact configured authority, action,
// store scope, signature, and request validity window. Expiry is caller chosen
// and signed; the controller does not invent a request deadline.
func VerifyRecoveryRequest(request RecoveryRequest, cfg config.LifecycleConfig, now time.Time) (string, error) {
	return verifyRecoveryRequest(request, cfg, now, true)
}

// VerifyRecoveryRequestProof verifies the configured signer and signed request
// structure without deciding whether its effect window is still open. It is
// only for observing a previously reserved request receipt: callers must never
// use it to reserve budget or perform a new action.
func VerifyRecoveryRequestProof(request RecoveryRequest, cfg config.LifecycleConfig) (string, error) {
	return verifyRecoveryRequest(request, cfg, time.Time{}, false)
}

func verifyRecoveryRequest(request RecoveryRequest, cfg config.LifecycleConfig, now time.Time, requireCurrentWindow bool) (string, error) {
	if !cfg.AdmissionEnabled || !cfg.RecoveryEnabled || (requireCurrentWindow && now.IsZero()) {
		return "", fmt.Errorf("lifecycle recovery is disabled or time is unavailable: %w", ErrRecoveryRequestInvalid)
	}
	if request.Version != 1 || request.Action != "nudge" || !validSessionRequestID(request.RequestID) ||
		!validRecoveryToken(request.Scope, 500) || !validRecoveryToken(request.WorkItemID, 200) ||
		request.ExpectedRevision <= 0 || !validRecoveryToken(request.Owner, 200) ||
		!validRecoveryToken(request.ClaimGeneration, 100) || !validRecoveryToken(request.SessionID, 200) ||
		!validRecoveryToken(request.SessionGeneration, 100) || strings.TrimSpace(request.Message) == "" ||
		len(request.Message) > 8192 || !utf8.ValidString(request.Message) ||
		!validRecoveryToken(request.AuthorizedBy, 200) {
		return "", ErrRecoveryRequestInvalid
	}
	authority, ok := cfg.RecoveryAuthorities[request.AuthorizedBy]
	if !ok || !containsExact(authority.Actions, request.Action) || !containsExact(authority.Scopes, request.Scope) {
		return "", fmt.Errorf("recovery authority does not grant this action and scope: %w", ErrRecoveryRequestInvalid)
	}
	issuedAt, errIssued := time.Parse(time.RFC3339Nano, request.IssuedAt)
	expiresAt, errExpires := time.Parse(time.RFC3339Nano, request.ExpiresAt)
	if errIssued != nil || errExpires != nil || !issuedAt.Before(expiresAt) ||
		(requireCurrentWindow && (now.Before(issuedAt) || !now.Before(expiresAt))) {
		return "", fmt.Errorf("recovery request validity window is invalid or expired: %w", ErrRecoveryRequestInvalid)
	}
	publicKey, err := base64.StdEncoding.DecodeString(strings.TrimSpace(authority.PublicKey))
	if err != nil || len(publicKey) != ed25519.PublicKeySize {
		return "", fmt.Errorf("configured recovery authority key is invalid: %w", ErrRecoveryRequestInvalid)
	}
	signature, err := base64.StdEncoding.DecodeString(request.Signature)
	if err != nil || len(signature) != ed25519.SignatureSize {
		return "", fmt.Errorf("recovery signature is invalid: %w", ErrRecoveryRequestInvalid)
	}
	payload, err := RecoveryRequestSigningBytes(request)
	if err != nil || !ed25519.Verify(ed25519.PublicKey(publicKey), payload, signature) {
		return "", fmt.Errorf("recovery signature is not trusted: %w", ErrRecoveryRequestInvalid)
	}
	return RecoveryRequestDigest(request)
}

func validSessionRequestID(id string) bool {
	if len(id) == 0 || len(id) > 200 {
		return false
	}
	for _, c := range id {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("-_.:", c) {
			continue
		}
		return false
	}
	return true
}

// RecoveryRequestDigest binds replay state to every signed field, including
// the detached signature. A repeated ID with a different signature or payload
// is a conflict instead of a new action.
func RecoveryRequestDigest(request RecoveryRequest) (string, error) {
	encoded, err := json.Marshal(request)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

// RecoveryIntent is the durable, controller-readable form of one authorized
// recovery request. It is stored as a separate held infrastructure bead.
type RecoveryIntent struct {
	Version int             `json:"version"`
	Digest  string          `json:"digest"`
	Request RecoveryRequest `json:"request"`
}

// PersistRecoveryIntent creates or reads back a stable-ID, immutable intent.
// Prefix must be the configured ID prefix for the exact authoritative store.
// The store must honor caller-supplied IDs; ambiguous Create outcomes are
// resolved only by an exact readback of this same deterministic ID.
func PersistRecoveryIntent(store beads.Store, prefix string, request RecoveryRequest, digest string) (beads.Bead, bool, error) {
	if store == nil || !beads.StableCreateIDFor(store) {
		return beads.Bead{}, false, ErrRecoveryIntentUnavailable
	}
	prefix = strings.TrimSpace(prefix)
	if prefix == "" || strings.ContainsAny(prefix, "-\n\r\t ") || digest == "" {
		return beads.Bead{}, false, ErrRecoveryIntentUnavailable
	}
	intentID := recoveryIntentID(prefix, request.Scope, request.WorkItemID, request.RequestID)
	intent := RecoveryIntent{Version: 1, Digest: digest, Request: request}
	raw, err := json.Marshal(intent)
	if err != nil {
		return beads.Bead{}, false, err
	}
	if existing, err := store.Get(intentID); err == nil {
		if !recoveryIntentMatches(existing, intentID, string(raw), digest) {
			return beads.Bead{}, false, ErrRecoveryRequestConflict
		}
		return existing, false, nil
	} else if !errors.Is(err, beads.ErrNotFound) {
		return beads.Bead{}, false, fmt.Errorf("read durable recovery intent: %w", err)
	}
	if err := validateRecoveryTargetRow(store, request); err != nil {
		return beads.Bead{}, false, err
	}
	created, createErr := store.Create(beads.Bead{
		ID: intentID, Title: "Authorized lifecycle recovery request", Type: "lifecycle-intent",
		Labels: []string{"hold:external"},
		Metadata: beads.StringMap{
			beadmeta.LifecycleRecoveryIntentMetadataKey:  string(raw),
			beadmeta.LifecycleRecoveryIntentDigestKey:    digest,
			beadmeta.LifecycleRecoveryIntentWorkItemKey:  request.WorkItemID,
			beadmeta.LifecycleRecoveryIntentScopeKey:     request.Scope,
			beadmeta.LifecycleRecoveryIntentRequestIDKey: request.RequestID,
		},
	})
	if createErr != nil {
		readback, readErr := store.Get(intentID)
		if readErr != nil || !recoveryIntentMatches(readback, intentID, string(raw), digest) {
			return beads.Bead{}, false, errors.Join(createErr, readErr)
		}
		return readback, false, nil
	}
	if created.ID != intentID || !recoveryIntentMatches(created, intentID, string(raw), digest) {
		return beads.Bead{}, false, fmt.Errorf("created recovery intent did not preserve its stable identity and content: %w", ErrRecoveryIntentUnavailable)
	}
	readback, err := store.Get(intentID)
	if err != nil || !recoveryIntentMatches(readback, intentID, string(raw), digest) {
		return beads.Bead{}, false, errors.Join(fmt.Errorf("recovery intent readback failed: %w", ErrRecoveryIntentUnavailable), err)
	}
	return readback, true, nil
}

// DecodeRecoveryIntent reads and validates the immutable payload carried by an
// intent bead. Callers must still verify its signature and revalidate the
// target before reserving or acting.
func DecodeRecoveryIntent(bead beads.Bead) (RecoveryIntent, error) {
	if bead.Type != "lifecycle-intent" || !beads.HasReadyExcludedLabel(bead) ||
		strings.TrimSpace(bead.Metadata[beadmeta.LifecycleRecoveryIntentMetadataKey]) == "" {
		return RecoveryIntent{}, ErrRecoveryRequestInvalid
	}
	var intent RecoveryIntent
	if err := json.Unmarshal([]byte(bead.Metadata[beadmeta.LifecycleRecoveryIntentMetadataKey]), &intent); err != nil {
		return RecoveryIntent{}, fmt.Errorf("decode durable recovery intent: %w", err)
	}
	if intent.Version != 1 || intent.Digest == "" || intent.Digest != bead.Metadata[beadmeta.LifecycleRecoveryIntentDigestKey] {
		return RecoveryIntent{}, ErrRecoveryRequestInvalid
	}
	digest, err := RecoveryRequestDigest(intent.Request)
	if err != nil || digest != intent.Digest || bead.Metadata[beadmeta.LifecycleRecoveryIntentWorkItemKey] != intent.Request.WorkItemID ||
		bead.Metadata[beadmeta.LifecycleRecoveryIntentScopeKey] != intent.Request.Scope ||
		bead.Metadata[beadmeta.LifecycleRecoveryIntentRequestIDKey] != intent.Request.RequestID {
		return RecoveryIntent{}, ErrRecoveryRequestInvalid
	}
	return intent, nil
}

// ValidateRecoveryWorkEvidence requires a direct, attached lifecycle source
// whose signed admission and workflow materialization still match. Descendant
// rows are never recovery targets under the initial nudge-only authority.
func ValidateRecoveryWorkEvidence(bead beads.Bead, cfg config.LifecycleConfig, scope string) error {
	decision := EvaluateAdmission(bead, cfg, scope)
	if !decision.Requested || !decision.Admitted {
		return fmt.Errorf("recovery target has no trusted admission: %w", ErrRecoveryWorkStale)
	}
	var marker struct {
		Version          int    `json:"version"`
		State            string `json:"state"`
		Scope            string `json:"scope"`
		Contract         string `json:"contract"`
		Route            string `json:"route"`
		Workflow         string `json:"workflow"`
		MergeStrategy    string `json:"merge_strategy"`
		Token            string `json:"token"`
		WorkflowID       string `json:"workflow_id"`
		SourceID         string `json:"source_id"`
		SourceStoreRef   string `json:"source_store_ref"`
		WorkflowStoreRef string `json:"workflow_store_ref"`
		AdmissionReceipt string `json:"admission_receipt"`
	}
	if err := decodeStrict(bead.Metadata[beadmeta.LifecycleMaterializationMetadataKey], &marker); err != nil {
		return fmt.Errorf("recovery target workflow materialization is invalid: %w", ErrRecoveryWorkStale)
	}
	digest, err := AdmissionDigest(decision.Receipt)
	if err != nil || marker.Version != 1 || marker.State != "attached" || marker.Scope != scope || marker.Contract != digest ||
		marker.Route == "" || marker.Workflow != decision.Receipt.Workflow || marker.MergeStrategy != decision.Receipt.MergeStrategy ||
		marker.Token == "" || marker.WorkflowID == "" || marker.SourceID != bead.ID || marker.SourceStoreRef == "" ||
		marker.WorkflowStoreRef == "" || marker.AdmissionReceipt != bead.Metadata[beadmeta.LifecycleAdmissionReceiptMetadataKey] {
		return fmt.Errorf("recovery target workflow is not the attached admitted source: %w", ErrRecoveryWorkStale)
	}
	return nil
}

func validateRecoveryTargetRow(store beads.Store, request RecoveryRequest) error {
	work, err := store.Get(request.WorkItemID)
	if err != nil {
		return fmt.Errorf("read requested work item: %w", err)
	}
	if work.Revision != request.ExpectedRevision || work.Status != "in_progress" || work.Assignee != request.Owner ||
		work.Metadata[beadmeta.ClaimGenerationMetadataKey] != request.ClaimGeneration ||
		work.Metadata[beadmeta.SessionIDMetadataKey] != request.SessionID || hasLifecycleHold(work) {
		return ErrRecoveryWorkStale
	}
	return nil
}

func recoveryIntentMatches(bead beads.Bead, id, raw, digest string) bool {
	return bead.ID == id && bead.Type == "lifecycle-intent" && beads.HasReadyExcludedLabel(bead) &&
		bead.Metadata[beadmeta.LifecycleRecoveryIntentMetadataKey] == raw &&
		bead.Metadata[beadmeta.LifecycleRecoveryIntentDigestKey] == digest
}

func recoveryIntentID(prefix, scope, workID, requestID string) string {
	digest := sha256.Sum256([]byte(scope + "\x00" + workID + "\x00" + requestID))
	return prefix + "-lri-" + hex.EncodeToString(digest[:16])
}

func validRecoveryToken(value string, limit int) bool {
	return value != "" && value == strings.TrimSpace(value) && len(value) <= limit &&
		!strings.ContainsAny(value, "\x00\r\n") && utf8.ValidString(value)
}

func containsExact(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func hasLifecycleHold(bead beads.Bead) bool {
	for _, label := range bead.Labels {
		if strings.HasPrefix(strings.TrimSpace(label), "hold:") || strings.EqualFold(strings.TrimSpace(label), "hold") {
			return true
		}
	}
	return strings.TrimSpace(bead.Metadata["gc.lifecycle.hold"]) != "" || strings.TrimSpace(bead.Metadata["gc.lifecycle.hold_reason"]) != ""
}
