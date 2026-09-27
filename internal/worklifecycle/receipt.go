// Package worklifecycle verifies controller lifecycle evidence carried through
// bead metadata. Receipt signer identity is never trusted on its own: the
// signature must verify against a public key configured by the city operator.
package worklifecycle

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// AdmissionIntentLabel explicitly enrolls a bead in controller lifecycle admission.
const AdmissionIntentLabel = "ready-for-agent"

// ErrEnrolledWorkMutationBlocked reports an attempted ordinary mutation of
// controller-enrolled work that requires verified lifecycle evidence.
var ErrEnrolledWorkMutationBlocked = beads.ErrLifecycleMutationBlocked

const (
	admissionDomain  = "gascity.lifecycle.admission.v1\n"
	completionDomain = "gascity.lifecycle.completion.v1\n"
)

// AdmissionReceipt binds explicit admission intent, the work item, its route,
// acceptance authority, and the minimum usable acceptance contract. Signature
// is Ed25519 over AdmissionSigningBytes.
type AdmissionReceipt struct {
	Version             int    `json:"version"`
	WorkItemID          string `json:"work_item_id"`
	Scope               string `json:"scope"`
	Route               string `json:"route"`
	Workflow            string `json:"workflow"`
	MergeStrategy       string `json:"merge_strategy"`
	Deliverable         string `json:"deliverable"`
	Verification        string `json:"verification"`
	AcceptanceAuthority string `json:"acceptance_authority"`
	AdmittedBy          string `json:"admitted_by"`
	Signature           string `json:"signature"`
}

// CompletionReceipt records acceptance of the exact admission contract and
// points to the delivered artifact and verification evidence. A trusted
// acceptance authority signs the record after checking those references.
type CompletionReceipt struct {
	Version         int    `json:"version"`
	WorkItemID      string `json:"work_item_id"`
	Scope           string `json:"scope"`
	AdmissionDigest string `json:"admission_digest"`
	DeliverableRef  string `json:"deliverable_ref"`
	VerificationRef string `json:"verification_ref"`
	AcceptedBy      string `json:"accepted_by"`
	AcceptedAt      string `json:"accepted_at"`
	Signature       string `json:"signature"`
}

// AdmissionDecision reports whether a bead requests lifecycle admission and
// whether its signed contract is trusted for the supplied store scope.
type AdmissionDecision struct {
	Requested bool
	Admitted  bool
	Reason    string
	Receipt   AdmissionReceipt
}

// CompletionDecision reports whether a signed completion receipt satisfies
// the bead's admission contract and configured acceptance authority.
type CompletionDecision struct {
	Accepted bool
	Reason   string
	Receipt  CompletionReceipt
}

// AdmissionSigningBytes returns the domain-separated canonical payload signed
// by an admission authority. The receipt signature is excluded.
func AdmissionSigningBytes(receipt AdmissionReceipt) ([]byte, error) {
	receipt.Signature = ""
	return signingBytes(admissionDomain, receipt)
}

// SignAdmissionReceipt signs a receipt and returns its JSON representation for
// the gc.lifecycle.admission_receipt.v1 metadata key. Private keys should be
// held by the trusted admission tool, never in city.toml or bead metadata.
func SignAdmissionReceipt(receipt AdmissionReceipt, privateKey ed25519.PrivateKey) (string, error) {
	if len(privateKey) != ed25519.PrivateKeySize {
		return "", errors.New("admission signing key must be an Ed25519 private key")
	}
	payload, err := AdmissionSigningBytes(receipt)
	if err != nil {
		return "", err
	}
	receipt.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(privateKey, payload))
	encoded, err := json.Marshal(receipt)
	if err != nil {
		return "", fmt.Errorf("encode admission receipt: %w", err)
	}
	return string(encoded), nil
}

// EvaluateAdmission recognizes lifecycle admission only when the intent label
// is present. With the gate disabled or no explicit intent, existing workflows
// retain their current eligibility path. Once intent is present, malformed,
// absent, stale, or untrusted evidence blocks only this item.
func EvaluateAdmission(bead beads.Bead, cfg config.LifecycleConfig, scope string) AdmissionDecision {
	receiptMetadata := strings.TrimSpace(bead.Metadata[beadmeta.LifecycleAdmissionReceiptMetadataKey])
	if !cfg.AdmissionEnabled || (!hasLabel(bead, AdmissionIntentLabel) && receiptMetadata == "") {
		return AdmissionDecision{}
	}
	decision := AdmissionDecision{Requested: true, Reason: "admission receipt is missing"}
	var receipt AdmissionReceipt
	if err := decodeStrict(bead.Metadata[beadmeta.LifecycleAdmissionReceiptMetadataKey], &receipt); err != nil {
		decision.Reason = "admission receipt is invalid: " + err.Error()
		return decision
	}
	if receipt.Version != 1 || strings.TrimSpace(receipt.WorkItemID) != bead.ID || strings.TrimSpace(receipt.Scope) == "" || strings.TrimSpace(receipt.Scope) != strings.TrimSpace(scope) {
		decision.Reason = "admission receipt does not identify this work item, store scope, and version"
		return decision
	}
	if strings.TrimSpace(receipt.Route) == "" || strings.TrimSpace(receipt.Workflow) == "" || strings.TrimSpace(receipt.Deliverable) == "" || strings.TrimSpace(receipt.Verification) == "" {
		decision.Reason = "admission receipt must name a route, workflow, deliverable, and verification method"
		return decision
	}
	switch receipt.MergeStrategy {
	case "direct", "mr", "local":
	default:
		decision.Reason = "admission receipt must name a supported merge strategy (direct, mr, or local)"
		return decision
	}
	if _, ok := cfg.AcceptanceAuthorities[receipt.AcceptanceAuthority]; !ok {
		decision.Reason = "admission receipt names an untrusted acceptance authority"
		return decision
	}
	if !verifyReceiptSignature(receipt, receipt.Signature, receipt.AdmittedBy, cfg.AdmissionAuthorities) {
		decision.Reason = "admission receipt signature is not trusted"
		return decision
	}
	decision.Admitted = true
	decision.Reason = "verified"
	decision.Receipt = receipt
	return decision
}

// CompletionSigningBytes returns the domain-separated canonical payload signed
// by an acceptance authority. The receipt signature is excluded.
func CompletionSigningBytes(receipt CompletionReceipt) ([]byte, error) {
	receipt.Signature = ""
	return signingBytes(completionDomain, receipt)
}

// SignCompletionReceipt signs an acceptance record. The caller must first
// verify the artifact and verification references against the admission
// contract; the controller independently checks signer, work ID, and contract
// binding before reconciling the bead.
func SignCompletionReceipt(receipt CompletionReceipt, privateKey ed25519.PrivateKey) (string, error) {
	if len(privateKey) != ed25519.PrivateKeySize {
		return "", errors.New("completion signing key must be an Ed25519 private key")
	}
	payload, err := CompletionSigningBytes(receipt)
	if err != nil {
		return "", err
	}
	receipt.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(privateKey, payload))
	encoded, err := json.Marshal(receipt)
	if err != nil {
		return "", fmt.Errorf("encode completion receipt: %w", err)
	}
	return string(encoded), nil
}

// AdmissionDigest returns a stable digest of the signed admission payload,
// excluding its detached signature. Completion receipts bind to this digest.
func AdmissionDigest(receipt AdmissionReceipt) (string, error) {
	payload, err := AdmissionSigningBytes(receipt)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(payload)
	return base64.RawURLEncoding.EncodeToString(digest[:]), nil
}

// EvaluateCompletion accepts a completion receipt only when the work item has
// a valid admission receipt and the configured acceptance authority signs a
// non-empty deliverable/verification record bound to that exact contract.
func EvaluateCompletion(bead beads.Bead, cfg config.LifecycleConfig, scope string) CompletionDecision {
	return EvaluateCompletionAt(bead, cfg, scope, time.Now())
}

// EvaluateCompletionAt accepts completion only within the explicitly
// configured freshness window. The injected clock keeps policy boundary tests
// deterministic; callers should use EvaluateCompletion for live work.
func EvaluateCompletionAt(bead beads.Bead, cfg config.LifecycleConfig, scope string, now time.Time) CompletionDecision {
	admission := EvaluateAdmission(bead, cfg, scope)
	if !admission.Requested || !admission.Admitted {
		return CompletionDecision{Reason: "work item has no verified admission contract"}
	}
	decision := CompletionDecision{Reason: "completion receipt is missing"}
	var receipt CompletionReceipt
	if err := decodeStrict(bead.Metadata[beadmeta.LifecycleCompletionReceiptMetadataKey], &receipt); err != nil {
		decision.Reason = "completion receipt is invalid: " + err.Error()
		return decision
	}
	if receipt.Version != 1 || strings.TrimSpace(receipt.WorkItemID) != bead.ID || strings.TrimSpace(receipt.Scope) == "" || strings.TrimSpace(receipt.Scope) != strings.TrimSpace(scope) {
		decision.Reason = "completion receipt does not identify this work item, store scope, and version"
		return decision
	}
	if strings.TrimSpace(receipt.DeliverableRef) == "" || strings.TrimSpace(receipt.VerificationRef) == "" {
		decision.Reason = "completion receipt must reference the delivered artifact and verification evidence"
		return decision
	}
	acceptedAt, err := time.Parse(time.RFC3339Nano, receipt.AcceptedAt)
	if err != nil || acceptedAt.IsZero() {
		decision.Reason = "completion receipt has an invalid acceptance time"
		return decision
	}
	maxAge, clockSkew, configured := completionFreshnessPolicy(cfg)
	if !configured {
		decision.Reason = "completion receipt freshness policy is not configured"
		return decision
	}
	if now.IsZero() || acceptedAt.After(now.Add(clockSkew)) {
		decision.Reason = "completion receipt acceptance time is too far in the future"
		return decision
	}
	if acceptedAt.Before(now.Add(-maxAge)) {
		decision.Reason = "completion receipt is older than the configured freshness window"
		return decision
	}
	digest, err := AdmissionDigest(admission.Receipt)
	if err != nil || receipt.AdmissionDigest != digest {
		decision.Reason = "completion receipt does not bind to the current admission contract"
		return decision
	}
	if receipt.AcceptedBy != admission.Receipt.AcceptanceAuthority {
		decision.Reason = "completion signer differs from the acceptance authority named at admission"
		return decision
	}
	if !verifyReceiptSignature(receipt, receipt.Signature, receipt.AcceptedBy, cfg.AcceptanceAuthorities) {
		decision.Reason = "completion receipt signature is not trusted"
		return decision
	}
	decision.Accepted = true
	decision.Reason = "verified"
	decision.Receipt = receipt
	return decision
}

func completionFreshnessPolicy(cfg config.LifecycleConfig) (maxAge, clockSkew time.Duration, configured bool) {
	maxAgeText := strings.TrimSpace(cfg.CompletionReceiptMaxAge)
	skewText := strings.TrimSpace(cfg.CompletionClockSkew)
	if maxAgeText == "" || skewText == "" {
		return 0, 0, false
	}
	maxAge, err := time.ParseDuration(maxAgeText)
	if err != nil || maxAge <= 0 {
		return 0, 0, false
	}
	clockSkew, err = time.ParseDuration(skewText)
	if err != nil || clockSkew < 0 {
		return 0, 0, false
	}
	return maxAge, clockSkew, true
}

// HasDurableEnrollment reports controller-owned lifecycle evidence persisted
// on the row. It deliberately ignores the removable intent label: once a
// signed receipt, materialization, completion receipt, or recovery state exists,
// ordinary label edits cannot return the record to legacy mutation paths.
func HasDurableEnrollment(bead beads.Bead) bool {
	return beads.HasLifecycleEvidence(bead)
}

// ValidateEnrolledMutation prevents generic close/reopen/status and metadata
// writes from bypassing completion or erasing the lifecycle record before the
// next controller reconciliation. A changed signed contract must use a
// separately authorized fresh attempt; blank values cannot silently opt
// existing work back into legacy behavior.
func ValidateEnrolledMutation(current beads.Bead, opts beads.UpdateOpts) error {
	return beads.ValidateLifecycleMutation(current, opts)
}

func hasLabel(bead beads.Bead, wanted string) bool {
	for _, label := range bead.Labels {
		if strings.EqualFold(strings.TrimSpace(label), wanted) {
			return true
		}
	}
	return false
}

func decodeStrict(encoded string, target any) error {
	if strings.TrimSpace(encoded) == "" {
		return errors.New("metadata value is empty")
	}
	decoder := json.NewDecoder(strings.NewReader(encoded))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("metadata contains trailing JSON")
		}
		return err
	}
	return nil
}

func signingBytes(domain string, payload any) ([]byte, error) {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("encode signed lifecycle payload: %w", err)
	}
	return append([]byte(domain), encoded...), nil
}

func verifyReceiptSignature(receipt any, encodedSignature, identity string, authorities map[string]string) bool {
	encodedKey, ok := authorities[identity]
	if !ok || strings.TrimSpace(identity) == "" {
		return false
	}
	publicKey, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encodedKey))
	if err != nil || len(publicKey) != ed25519.PublicKeySize {
		return false
	}
	signature, err := base64.StdEncoding.DecodeString(encodedSignature)
	if err != nil || len(signature) != ed25519.SignatureSize {
		return false
	}
	var payload []byte
	switch value := receipt.(type) {
	case AdmissionReceipt:
		payload, err = AdmissionSigningBytes(value)
	case CompletionReceipt:
		payload, err = CompletionSigningBytes(value)
	default:
		return false
	}
	return err == nil && ed25519.Verify(ed25519.PublicKey(publicKey), payload, signature)
}

// CanonicalAdmissionReceipt reports whether the metadata JSON is the exact
// canonical encoding produced by SignAdmissionReceipt. It is useful for tools
// that compare or transport receipts without introducing alternate encodings.
func CanonicalAdmissionReceipt(encoded string) (string, error) {
	var receipt AdmissionReceipt
	if err := decodeStrict(encoded, &receipt); err != nil {
		return "", err
	}
	canonical, err := json.Marshal(receipt)
	if err != nil {
		return "", err
	}
	if !bytes.Equal(bytes.TrimSpace([]byte(encoded)), canonical) {
		return "", errors.New("admission receipt JSON is not canonical")
	}
	return string(canonical), nil
}
