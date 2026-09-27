package worklifecycle

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

func lifecycleKeys(t *testing.T) (ed25519.PrivateKey, ed25519.PrivateKey, config.LifecycleConfig) {
	t.Helper()
	_, admissionPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	_, acceptancePrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	admissionPublic := admissionPrivate.Public().(ed25519.PublicKey)
	acceptancePublic := acceptancePrivate.Public().(ed25519.PublicKey)
	return admissionPrivate, acceptancePrivate, config.LifecycleConfig{
		AdmissionEnabled: true,
		AdmissionAuthorities: map[string]string{
			"triage": base64.StdEncoding.EncodeToString(admissionPublic),
		},
		AcceptanceAuthorities: map[string]string{
			"reviewer": base64.StdEncoding.EncodeToString(acceptancePublic),
		},
	}
}

func admittedBead(t *testing.T, admissionPrivate ed25519.PrivateKey) (beads.Bead, AdmissionReceipt) {
	t.Helper()
	receipt := AdmissionReceipt{
		Version:             1,
		WorkItemID:          "work-1",
		Scope:               "rig:pilot",
		Route:               "rig/coder",
		Workflow:            "mol-polecat-work",
		MergeStrategy:       "mr",
		Deliverable:         "commit implementing the change",
		Verification:        "go test ./internal/example",
		AcceptanceAuthority: "reviewer",
		AdmittedBy:          "triage",
	}
	encoded, err := SignAdmissionReceipt(receipt, admissionPrivate)
	if err != nil {
		t.Fatal(err)
	}
	bead := beads.Bead{
		ID:     "work-1",
		Status: "open",
		Labels: []string{AdmissionIntentLabel},
		Metadata: map[string]string{
			beadmeta.LifecycleAdmissionReceiptMetadataKey: encoded,
		},
	}
	return bead, receipt
}

func TestEvaluateAdmissionVerifiesSignatureAndWorkBinding(t *testing.T) {
	admissionPrivate, _, cfg := lifecycleKeys(t)
	bead, _ := admittedBead(t, admissionPrivate)
	decision := EvaluateAdmission(bead, cfg, "rig:pilot")
	if !decision.Requested || !decision.Admitted || decision.Receipt.Route != "rig/coder" {
		t.Fatalf("EvaluateAdmission() = %+v, want verified admitted receipt", decision)
	}

	t.Run("tampered contract", func(t *testing.T) {
		changed := bead
		changed.Metadata = map[string]string{}
		for k, v := range bead.Metadata {
			changed.Metadata[k] = v
		}
		changed.Metadata[beadmeta.LifecycleAdmissionReceiptMetadataKey] = strings.Replace(
			changed.Metadata[beadmeta.LifecycleAdmissionReceiptMetadataKey], "go test ./internal/example", "echo pass", 1)
		if got := EvaluateAdmission(changed, cfg, "rig:pilot"); got.Admitted || !got.Requested {
			t.Fatalf("tampered receipt decision = %+v, want requested but blocked", got)
		}
	})

	t.Run("different work item", func(t *testing.T) {
		changed := bead
		changed.ID = "other-work"
		if got := EvaluateAdmission(changed, cfg, "rig:pilot"); got.Admitted {
			t.Fatalf("receipt for another item was admitted: %+v", got)
		}
	})

	t.Run("unknown signer", func(t *testing.T) {
		changed := bead
		changed.Metadata = map[string]string{}
		for k, v := range bead.Metadata {
			changed.Metadata[k] = v
		}
		changed.Metadata[beadmeta.LifecycleAdmissionReceiptMetadataKey] = strings.Replace(
			changed.Metadata[beadmeta.LifecycleAdmissionReceiptMetadataKey], `"admitted_by":"triage"`, `"admitted_by":"worker"`, 1)
		if got := EvaluateAdmission(changed, cfg, "rig:pilot"); got.Admitted {
			t.Fatalf("worker-authored identity was trusted: %+v", got)
		}
	})
}

func TestEvaluateAdmissionLeavesUnmanagedWorkAlone(t *testing.T) {
	bead := beads.Bead{ID: "work-legacy", Status: "open"}
	if got := EvaluateAdmission(bead, config.LifecycleConfig{AdmissionEnabled: true}, "city:test"); got.Requested {
		t.Fatalf("work without explicit admission intent was intercepted: %+v", got)
	}
	bead.Labels = []string{"READY-FOR-AGENT"}
	if got := EvaluateAdmission(bead, config.LifecycleConfig{}, "city:test"); got.Requested {
		t.Fatalf("disabled admission gate was active: %+v", got)
	}
}

func TestEvaluateAdmissionReceiptKeepsEnrollmentAndBindsStoreScope(t *testing.T) {
	admissionPrivate, _, cfg := lifecycleKeys(t)
	bead, _ := admittedBead(t, admissionPrivate)
	bead.Labels = nil // The verified receipt is durable enrollment; removing a label cannot bypass it.
	if got := EvaluateAdmission(bead, cfg, "rig:pilot"); !got.Requested || !got.Admitted {
		t.Fatalf("label removal disabled receipt enforcement: %+v", got)
	}
	if got := EvaluateAdmission(bead, cfg, "rig:other"); got.Admitted {
		t.Fatalf("receipt replayed across store scope: %+v", got)
	}
}

func TestEvaluateCompletionRequiresTrustedAcceptanceBoundToContract(t *testing.T) {
	admissionPrivate, acceptancePrivate, cfg := lifecycleKeys(t)
	bead, admission := admittedBead(t, admissionPrivate)
	digest, err := AdmissionDigest(admission)
	if err != nil {
		t.Fatal(err)
	}
	completion := CompletionReceipt{
		Version:         1,
		WorkItemID:      bead.ID,
		Scope:           "rig:pilot",
		AdmissionDigest: digest,
		DeliverableRef:  "git:commit:abc123",
		VerificationRef: "test-run:run-456",
		AcceptedBy:      "reviewer",
		AcceptedAt:      "2026-09-26T10:00:00Z",
	}
	encoded, err := SignCompletionReceipt(completion, acceptancePrivate)
	if err != nil {
		t.Fatal(err)
	}
	bead.Metadata[beadmeta.LifecycleCompletionReceiptMetadataKey] = encoded
	if got := EvaluateCompletion(bead, cfg, "rig:pilot"); !got.Accepted {
		t.Fatalf("completion decision = %+v, want accepted", got)
	}

	badDigest := bead
	badDigest.Metadata = map[string]string{}
	for k, v := range bead.Metadata {
		badDigest.Metadata[k] = v
	}
	badDigest.Metadata[beadmeta.LifecycleCompletionReceiptMetadataKey] = strings.Replace(encoded, digest, "wrong-digest", 1)
	if got := EvaluateCompletion(badDigest, cfg, "rig:pilot"); got.Accepted {
		t.Fatalf("completion receipt for another contract was accepted: %+v", got)
	}

	t.Run("different trusted acceptance authority", func(t *testing.T) {
		_, otherAcceptancePrivate, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		cfg.AcceptanceAuthorities["other-reviewer"] = base64.StdEncoding.EncodeToString(otherAcceptancePrivate.Public().(ed25519.PublicKey))
		other := completion
		other.AcceptedBy = "other-reviewer"
		encoded, err := SignCompletionReceipt(other, otherAcceptancePrivate)
		if err != nil {
			t.Fatal(err)
		}
		changed := bead
		changed.Metadata = map[string]string{}
		for k, v := range bead.Metadata {
			changed.Metadata[k] = v
		}
		changed.Metadata[beadmeta.LifecycleCompletionReceiptMetadataKey] = encoded
		if got := EvaluateCompletion(changed, cfg, "rig:pilot"); got.Accepted {
			t.Fatalf("second authority overrode the admitted acceptance authority: %+v", got)
		}
	})
}
