// Package worklifecycle verifies controller lifecycle evidence carried through
// bead metadata. Receipt signer identity is never trusted on its own: the
// signature must verify against a public key configured by the city operator.
package worklifecycle

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
	"strings"
	"time"
	"unicode"

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
	admissionV1Domain = "gascity.lifecycle.admission.v1\n"
	admissionV2Domain = "gascity.lifecycle.admission.v2\n"
	completionDomain  = "gascity.lifecycle.completion.v1\n"
)

// AdmissionReceipt is the retained v1 admission envelope. It remains
// decodable for historical evidence but cannot authorize new materialization.
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

// AdmissionReceiptV2 binds explicit admission intent to the exact reviewed
// source revision, acceptance contract, canonical rig-qualified target, and
// a versioned digest of the admission-relevant route/formula policy closure.
// Signature is Ed25519 over AdmissionV2SigningBytes.
type AdmissionReceiptV2 struct {
	Version              int    `json:"version"`
	WorkItemID           string `json:"work_item_id"`
	Scope                string `json:"scope"`
	ExpectedWorkRevision int64  `json:"expected_work_revision"`
	Route                string `json:"route"`
	Workflow             string `json:"workflow"`
	RoutingPolicyDigest  string `json:"routing_policy_digest"`
	MergeStrategy        string `json:"merge_strategy"`
	Deliverable          string `json:"deliverable"`
	Verification         string `json:"verification"`
	AcceptanceAuthority  string `json:"acceptance_authority"`
	AdmittedBy           string `json:"admitted_by"`
	Signature            string `json:"signature"`
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
// whether the controller can authorize it. Receipt is populated only when all
// required store-backed attachment and current-policy checks have succeeded.
type AdmissionDecision struct {
	Requested bool
	Admitted  bool
	Reason    string
	Receipt   AdmissionReceiptV2
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
	return signingBytes(admissionV1Domain, receipt)
}

// SignAdmissionReceipt signs a historical v1 receipt for compatibility and
// migration tests. It cannot authorize current lifecycle admission.
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

// AdmissionV2SigningBytes returns the domain-separated v2 payload with the
// detached signature excluded.
func AdmissionV2SigningBytes(receipt AdmissionReceiptV2) ([]byte, error) {
	receipt.Signature = ""
	return signingBytes(admissionV2Domain, receipt)
}

// SignAdmissionReceiptV2 signs a v2 receipt. The returned canonical JSON is
// stored under gc.lifecycle.admission_receipt.v2. The v2 signing key must be
// dedicated to admission and kept outside city config and bead metadata.
func SignAdmissionReceiptV2(receipt AdmissionReceiptV2, privateKey ed25519.PrivateKey) (string, error) {
	if len(privateKey) != ed25519.PrivateKeySize {
		return "", errors.New("admission v2 signing key must be an Ed25519 private key")
	}
	payload, err := AdmissionV2SigningBytes(receipt)
	if err != nil {
		return "", err
	}
	receipt.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(privateKey, payload))
	encoded, err := json.Marshal(receipt)
	if err != nil {
		return "", fmt.Errorf("encode admission v2 receipt: %w", err)
	}
	return string(encoded), nil
}

// EvaluateAdmission recognizes explicit lifecycle intent or durable admission
// evidence. V1 remains a historical hold. A valid v2 signature alone also
// remains held until the caller integrates exact Q43 attachment and current
// route-policy proof; this method has no store capabilities to verify those.
func EvaluateAdmission(bead beads.Bead, cfg config.LifecycleConfig, scope string) AdmissionDecision {
	v1Metadata := bead.Metadata[beadmeta.LifecycleAdmissionReceiptMetadataKey]
	v2Metadata := bead.Metadata[beadmeta.LifecycleAdmissionReceiptV2MetadataKey]
	if !cfg.AdmissionEnabled || (!hasLabel(bead, AdmissionIntentLabel) && v1Metadata == "" && v2Metadata == "") {
		return AdmissionDecision{}
	}
	decision := AdmissionDecision{Requested: true, Reason: "v2 admission receipt is missing"}
	if v1Metadata != "" && v2Metadata != "" {
		decision.Reason = "conflicting v1 and v2 admission receipts are present"
		return decision
	}
	if v2Metadata == "" {
		if v1Metadata != "" {
			var legacy AdmissionReceipt
			if err := decodeStrict(v1Metadata, &legacy); err == nil {
				decision.Reason = "v1 admission evidence is historical and requires explicit v2 re-admission"
			} else {
				decision.Reason = "historical v1 admission receipt is invalid"
			}
		}
		return decision
	}
	_, err := VerifyAdmissionReceiptV2(bead, cfg, scope)
	if err != nil {
		decision.Reason = err.Error()
		return decision
	}
	decision.Reason = "v2 admission requires verified attachment and current route-policy proof"
	return decision
}

// VerifyAdmissionReceiptV2 verifies only the canonical signed envelope and its
// authority/configuration binding. It deliberately does not report admission:
// callers must separately prove the atomic Q43 attachment and recompute the
// current route-policy projection before any materialization or worker claim.
func VerifyAdmissionReceiptV2(bead beads.Bead, cfg config.LifecycleConfig, scope string) (AdmissionReceiptV2, error) {
	v1Metadata := bead.Metadata[beadmeta.LifecycleAdmissionReceiptMetadataKey]
	v2Metadata := bead.Metadata[beadmeta.LifecycleAdmissionReceiptV2MetadataKey]
	if !cfg.AdmissionEnabled {
		return AdmissionReceiptV2{}, errors.New("v2 admission is disabled")
	}
	if v1Metadata != "" || v2Metadata == "" {
		return AdmissionReceiptV2{}, errors.New("v2 admission receipt is missing or conflicts with historical v1 evidence")
	}
	var receipt AdmissionReceiptV2
	if err := decodeStrict(v2Metadata, &receipt); err != nil {
		return AdmissionReceiptV2{}, fmt.Errorf("v2 admission receipt is invalid: %w", err)
	}
	canonical, err := json.Marshal(receipt)
	if err != nil || !bytes.Equal(canonical, []byte(v2Metadata)) {
		return AdmissionReceiptV2{}, errors.New("v2 admission receipt is not canonical JSON")
	}
	if receipt.Version != 2 || receipt.WorkItemID != bead.ID || receipt.Scope == "" || receipt.Scope != scope || receipt.ExpectedWorkRevision == 0 {
		return AdmissionReceiptV2{}, errors.New("v2 admission receipt does not identify this work item, nonzero reviewed revision, store scope, and version")
	}
	if !canonicalRigQualifiedTarget(receipt.Route) || strings.TrimSpace(receipt.Workflow) == "" ||
		!validLowerSHA256(receipt.RoutingPolicyDigest) || strings.TrimSpace(receipt.Deliverable) == "" || strings.TrimSpace(receipt.Verification) == "" {
		return AdmissionReceiptV2{}, errors.New("v2 admission receipt must name a canonical rig-qualified target, workflow, policy digest, deliverable, and verification method")
	}
	switch receipt.MergeStrategy {
	case "direct", "mr", "local":
	default:
		return AdmissionReceiptV2{}, errors.New("v2 admission receipt must name a supported merge strategy (direct, mr, or local)")
	}
	if _, ok := cfg.AcceptanceAuthorities[receipt.AcceptanceAuthority]; !ok {
		return AdmissionReceiptV2{}, errors.New("v2 admission receipt names an untrusted acceptance authority")
	}
	if cfg.AdmissionV2PrimaryAuthority == "" || len(cfg.AdmissionV2Authorities) != 1 || receipt.AdmittedBy != cfg.AdmissionV2PrimaryAuthority ||
		!admissionV2KeyPurposeSeparated(cfg, receipt.AdmittedBy) {
		return AdmissionReceiptV2{}, errors.New("v2 admission requires the sole explicitly configured, purpose-separated primary signer; delegates are unavailable")
	}
	if !verifyReceiptSignature(receipt, receipt.Signature, receipt.AdmittedBy, cfg.AdmissionV2Authorities) {
		return AdmissionReceiptV2{}, errors.New("v2 admission signature is not trusted")
	}
	return receipt, nil
}

func admissionV2KeyPurposeSeparated(cfg config.LifecycleConfig, identity string) bool {
	encoded := strings.TrimSpace(cfg.AdmissionV2Authorities[identity])
	publicKey, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(publicKey) != ed25519.PublicKeySize {
		return false
	}
	for _, authorities := range []map[string]string{cfg.AdmissionAuthorities, cfg.AcceptanceAuthorities} {
		for _, value := range authorities {
			other, err := base64.StdEncoding.DecodeString(strings.TrimSpace(value))
			if err == nil && bytes.Equal(publicKey, other) {
				return false
			}
		}
	}
	for _, authority := range cfg.RecoveryAuthorities {
		other, err := base64.StdEncoding.DecodeString(strings.TrimSpace(authority.PublicKey))
		if err == nil && bytes.Equal(publicKey, other) {
			return false
		}
	}
	return true
}

func canonicalRigQualifiedTarget(value string) bool {
	if strings.Count(value, "/") != 1 || strings.IndexFunc(value, unicode.IsSpace) >= 0 {
		return false
	}
	dir, name := config.ParseQualifiedName(value)
	return strings.TrimSpace(dir) != "" && strings.TrimSpace(name) != ""
}

func validLowerSHA256(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && hex.EncodeToString(decoded) == value
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

// AdmissionDigestV2 returns the stable digest of the signed v2 admission
// payload, excluding its detached signature.
func AdmissionDigestV2(receipt AdmissionReceiptV2) (string, error) {
	payload, err := AdmissionV2SigningBytes(receipt)
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
	digest, err := AdmissionDigestV2(admission.Receipt)
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
	case AdmissionReceiptV2:
		payload, err = AdmissionV2SigningBytes(value)
	case CompletionReceipt:
		payload, err = CompletionSigningBytes(value)
	default:
		return false
	}
	return err == nil && ed25519.Verify(ed25519.PublicKey(publicKey), payload, signature)
}

// CanonicalAdmissionReceiptV2 verifies that a v2 receipt uses its exact
// canonical JSON representation. The detached signature is included here.
func CanonicalAdmissionReceiptV2(encoded string) (string, error) {
	var receipt AdmissionReceiptV2
	if err := decodeStrict(encoded, &receipt); err != nil {
		return "", err
	}
	canonical, err := json.Marshal(receipt)
	if err != nil {
		return "", err
	}
	if !bytes.Equal([]byte(encoded), canonical) {
		return "", errors.New("admission v2 receipt JSON is not canonical")
	}
	return string(canonical), nil
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
