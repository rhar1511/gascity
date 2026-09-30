package worklifecycle

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

func TestValidateEnrolledMutationBlocksGenericWrites(t *testing.T) {
	current := beads.Bead{
		ID: "work-1",
		Metadata: map[string]string{
			beadmeta.LifecycleAdmissionReceiptMetadataKey: "persisted admission evidence",
		},
	}
	title := "changed by a generic caller"
	assignee := "replacement-worker"
	status := "closed"
	metadata := map[string]string{"gc.session_id": "replacement-session"}

	for name, opts := range map[string]beads.UpdateOpts{
		"ordinary field": {Title: &title},
		"assignment":     {Assignee: &assignee},
		"status":         {Status: &status},
		"owner metadata": {Metadata: metadata},
	} {
		t.Run(name, func(t *testing.T) {
			if err := ValidateEnrolledMutation(current, opts); !errors.Is(err, ErrEnrolledWorkMutationBlocked) {
				t.Fatalf("ValidateEnrolledMutation() error = %v, want %v", err, ErrEnrolledWorkMutationBlocked)
			}
		})
	}
}

func TestValidateEnrolledMutationLeavesLegacyWorkWritable(t *testing.T) {
	current := beads.Bead{ID: "legacy-work"}
	title := "updated"
	if err := ValidateEnrolledMutation(current, beads.UpdateOpts{Title: &title}); err != nil {
		t.Fatalf("ValidateEnrolledMutation() error = %v, want nil", err)
	}
}

func TestValidateGenericMutationBlocksLifecycleRecoveryIntent(t *testing.T) {
	current := beads.Bead{
		ID: "work-1",
		Metadata: map[string]string{
			beadmeta.LifecycleRecoveryIntentMetadataKey: "pending request",
		},
	}
	if err := ValidateGenericMutation(current); !errors.Is(err, ErrEnrolledWorkMutationBlocked) {
		t.Fatalf("ValidateGenericMutation() error = %v, want %v", err, ErrEnrolledWorkMutationBlocked)
	}
}

func lifecycleKeys(t *testing.T) (ed25519.PrivateKey, ed25519.PrivateKey, ed25519.PrivateKey, config.LifecycleConfig) {
	t.Helper()
	_, admissionPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	_, acceptancePrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	_, legacyPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	admissionPublic := admissionPrivate.Public().(ed25519.PublicKey)
	acceptancePublic := acceptancePrivate.Public().(ed25519.PublicKey)
	return admissionPrivate, acceptancePrivate, legacyPrivate, config.LifecycleConfig{
		AdmissionEnabled: true,
		AdmissionAuthorities: map[string]string{
			"triage-v1": base64.StdEncoding.EncodeToString(legacyPrivate.Public().(ed25519.PublicKey)),
		},
		AdmissionV2PrimaryAuthority: "triage",
		AdmissionV2Authorities: map[string]string{
			"triage": base64.StdEncoding.EncodeToString(admissionPublic),
		},
		AcceptanceAuthorities: map[string]string{
			"reviewer": base64.StdEncoding.EncodeToString(acceptancePublic),
		},
		CompletionReceiptMaxAge: "168h",
		CompletionClockSkew:     "5m",
	}
}

func admittedBead(t *testing.T, admissionPrivate ed25519.PrivateKey) (beads.Bead, AdmissionReceiptV2) {
	t.Helper()
	receipt := AdmissionReceiptV2{
		Version:              2,
		WorkItemID:           "work-1",
		Scope:                "rig:pilot",
		Route:                "rig/coder",
		ExpectedWorkRevision: 17,
		Workflow:             "mol-polecat-work",
		RoutingPolicyDigest:  strings.Repeat("a", 64),
		MergeStrategy:        "mr",
		Deliverable:          "commit implementing the change",
		Verification:         "go test ./internal/example",
		AcceptanceAuthority:  "reviewer",
		AdmittedBy:           "triage",
	}
	encoded, err := SignAdmissionReceiptV2(receipt, admissionPrivate)
	if err != nil {
		t.Fatal(err)
	}
	bead := beads.Bead{
		ID:     "work-1",
		Status: "open",
		Labels: []string{AdmissionIntentLabel},
		Metadata: map[string]string{
			beadmeta.LifecycleAdmissionReceiptV2MetadataKey: encoded,
		},
	}
	return bead, receipt
}

func TestEvaluateAdmissionVerifiesSignatureAndWorkBinding(t *testing.T) {
	admissionPrivate, _, _, cfg := lifecycleKeys(t)
	bead, _ := admittedBead(t, admissionPrivate)
	decision := EvaluateAdmission(bead, cfg, "rig:pilot")
	if !decision.Requested || decision.Admitted || decision.Receipt.WorkItemID != "" || !strings.Contains(decision.Reason, "attachment and current route-policy proof") {
		t.Fatalf("EvaluateAdmission() = %+v, want fail-closed proof requirement", decision)
	}
	verified, err := VerifyAdmissionReceiptV2(bead, cfg, "rig:pilot")
	if err != nil || verified.Route != "rig/coder" {
		t.Fatalf("VerifyAdmissionReceiptV2() = %+v, %v, want trusted signed envelope", verified, err)
	}

	t.Run("tampered contract", func(t *testing.T) {
		changed := bead
		changed.Metadata = map[string]string{}
		for k, v := range bead.Metadata {
			changed.Metadata[k] = v
		}
		changed.Metadata[beadmeta.LifecycleAdmissionReceiptV2MetadataKey] = strings.Replace(
			changed.Metadata[beadmeta.LifecycleAdmissionReceiptV2MetadataKey], "go test ./internal/example", "echo pass", 1)
		if got := EvaluateAdmission(changed, cfg, "rig:pilot"); got.Admitted || !got.Requested {
			t.Fatalf("tampered receipt decision = %+v, want requested but blocked", got)
		}
		if _, err := VerifyAdmissionReceiptV2(changed, cfg, "rig:pilot"); err == nil {
			t.Fatal("tampered signed envelope verified")
		}
	})

	t.Run("different work item", func(t *testing.T) {
		changed := bead
		changed.ID = "other-work"
		if got := EvaluateAdmission(changed, cfg, "rig:pilot"); got.Admitted {
			t.Fatalf("receipt for another item was admitted: %+v", got)
		}
		if _, err := VerifyAdmissionReceiptV2(changed, cfg, "rig:pilot"); err == nil {
			t.Fatal("envelope for another work item verified")
		}
	})

	t.Run("unknown signer", func(t *testing.T) {
		changed := bead
		changed.Metadata = map[string]string{}
		for k, v := range bead.Metadata {
			changed.Metadata[k] = v
		}
		changed.Metadata[beadmeta.LifecycleAdmissionReceiptV2MetadataKey] = strings.Replace(
			changed.Metadata[beadmeta.LifecycleAdmissionReceiptV2MetadataKey], `"admitted_by":"triage"`, `"admitted_by":"worker"`, 1)
		if got := EvaluateAdmission(changed, cfg, "rig:pilot"); got.Admitted {
			t.Fatalf("worker-authored identity was trusted: %+v", got)
		}
		if _, err := VerifyAdmissionReceiptV2(changed, cfg, "rig:pilot"); err == nil {
			t.Fatal("unknown admission signer verified")
		}
	})
}

func TestAdmissionV2VerifierRejectsDelegatedOrAliasedSigners(t *testing.T) {
	private, _, _, cfg := lifecycleKeys(t)
	bead, _ := admittedBead(t, private)

	sharedPurposeKey := cfg
	sharedPurposeKey.AcceptanceAuthorities = map[string]string{"reviewer": cfg.AdmissionV2Authorities["triage"]}
	if _, err := VerifyAdmissionReceiptV2(bead, sharedPurposeKey, "rig:pilot"); err == nil {
		t.Fatal("directly constructed config reused the v2 admission key for acceptance")
	}

	sharedRecoveryKey := cfg
	sharedRecoveryKey.RecoveryAuthorities = map[string]config.LifecycleRecoveryAuthority{
		"operator": {
			PublicKey: cfg.AdmissionV2Authorities["triage"],
			Actions:   []string{"nudge"},
			Scopes:    []string{"rig:pilot"},
		},
	}
	if _, err := VerifyAdmissionReceiptV2(bead, sharedRecoveryKey, "rig:pilot"); err == nil {
		t.Fatal("directly constructed config reused the v2 admission key for recovery")
	}

	cfg.AdmissionV2Authorities["delegate"] = cfg.AdmissionV2Authorities["triage"]
	if _, err := VerifyAdmissionReceiptV2(bead, cfg, "rig:pilot"); err == nil {
		t.Fatal("directly constructed multi-entry config authorized an undelegated signer alias")
	}

	cfg.AdmissionV2Authorities = map[string]string{"delegate": cfg.AdmissionV2Authorities["triage"]}
	if _, err := VerifyAdmissionReceiptV2(bead, cfg, "rig:pilot"); err == nil {
		t.Fatal("receipt from a non-primary signer was accepted")
	}
}

func TestEvaluateAdmissionRequiresCompleteV2AndRejectsLegacyOrConflictingEvidence(t *testing.T) {
	admissionPrivate, _, _, cfg := lifecycleKeys(t)
	bead, receipt := admittedBead(t, admissionPrivate)

	t.Run("missing reviewed revision", func(t *testing.T) {
		changed := receipt
		changed.ExpectedWorkRevision = 0
		encoded, err := SignAdmissionReceiptV2(changed, admissionPrivate)
		if err != nil {
			t.Fatal(err)
		}
		bead.Metadata[beadmeta.LifecycleAdmissionReceiptV2MetadataKey] = encoded
		if got := EvaluateAdmission(bead, cfg, "rig:pilot"); got.Admitted || !got.Requested {
			t.Fatalf("zero reviewed revision decision = %+v, want requested but held", got)
		}
		if _, err := VerifyAdmissionReceiptV2(bead, cfg, "rig:pilot"); err == nil {
			t.Fatal("zero reviewed revision verified")
		}
	})

	t.Run("missing policy digest", func(t *testing.T) {
		changed := receipt
		changed.RoutingPolicyDigest = ""
		encoded, err := SignAdmissionReceiptV2(changed, admissionPrivate)
		if err != nil {
			t.Fatal(err)
		}
		bead.Metadata[beadmeta.LifecycleAdmissionReceiptV2MetadataKey] = encoded
		if got := EvaluateAdmission(bead, cfg, "rig:pilot"); got.Admitted || !got.Requested {
			t.Fatalf("missing policy digest decision = %+v, want requested but held", got)
		}
		if _, err := VerifyAdmissionReceiptV2(bead, cfg, "rig:pilot"); err == nil {
			t.Fatal("missing policy digest verified")
		}
	})

	t.Run("uppercase digest", func(t *testing.T) {
		changed := receipt
		changed.RoutingPolicyDigest = strings.Repeat("A", 64)
		encoded, err := SignAdmissionReceiptV2(changed, admissionPrivate)
		if err != nil {
			t.Fatal(err)
		}
		bead.Metadata[beadmeta.LifecycleAdmissionReceiptV2MetadataKey] = encoded
		if got := EvaluateAdmission(bead, cfg, "rig:pilot"); got.Admitted || !got.Requested {
			t.Fatalf("uppercase policy digest decision = %+v, want requested but held", got)
		}
		if _, err := VerifyAdmissionReceiptV2(bead, cfg, "rig:pilot"); err == nil {
			t.Fatal("uppercase policy digest verified")
		}
	})

	t.Run("noncanonical target whitespace", func(t *testing.T) {
		changed := receipt
		changed.Route = "pilot/ worker"
		encoded, err := SignAdmissionReceiptV2(changed, admissionPrivate)
		if err != nil {
			t.Fatal(err)
		}
		bead.Metadata[beadmeta.LifecycleAdmissionReceiptV2MetadataKey] = encoded
		if got := EvaluateAdmission(bead, cfg, "rig:pilot"); got.Admitted || !got.Requested {
			t.Fatalf("whitespace target decision = %+v, want requested but held", got)
		}
		if _, err := VerifyAdmissionReceiptV2(bead, cfg, "rig:pilot"); err == nil {
			t.Fatal("noncanonical target verified")
		}
	})

	t.Run("duplicate JSON member", func(t *testing.T) {
		changed := bead
		changed.Metadata = map[string]string{}
		for key, value := range bead.Metadata {
			changed.Metadata[key] = value
		}
		encoded := changed.Metadata[beadmeta.LifecycleAdmissionReceiptV2MetadataKey]
		changed.Metadata[beadmeta.LifecycleAdmissionReceiptV2MetadataKey] = strings.Replace(encoded, `{"version":2,`, `{"version":2,"version":2,`, 1)
		if got := EvaluateAdmission(changed, cfg, "rig:pilot"); got.Admitted || !got.Requested {
			t.Fatalf("duplicate-member receipt decision = %+v, want requested but held", got)
		}
		if _, err := VerifyAdmissionReceiptV2(changed, cfg, "rig:pilot"); err == nil {
			t.Fatal("duplicate-member receipt verified")
		}
	})

	t.Run("noncanonical field order", func(t *testing.T) {
		var fields map[string]any
		if err := json.Unmarshal([]byte(bead.Metadata[beadmeta.LifecycleAdmissionReceiptV2MetadataKey]), &fields); err != nil {
			t.Fatal(err)
		}
		reordered, err := json.Marshal(fields)
		if err != nil {
			t.Fatal(err)
		}
		changed := bead
		changed.Metadata = map[string]string{}
		for key, value := range bead.Metadata {
			changed.Metadata[key] = value
		}
		changed.Metadata[beadmeta.LifecycleAdmissionReceiptV2MetadataKey] = string(reordered)
		if got := EvaluateAdmission(changed, cfg, "rig:pilot"); got.Admitted || !got.Requested {
			t.Fatalf("noncanonical receipt decision = %+v, want requested but held", got)
		}
		if _, err := VerifyAdmissionReceiptV2(changed, cfg, "rig:pilot"); err == nil {
			t.Fatal("noncanonical receipt verified")
		}
	})

	t.Run("negative revision remains opaque", func(t *testing.T) {
		changed := receipt
		changed.ExpectedWorkRevision = -17
		encoded, err := SignAdmissionReceiptV2(changed, admissionPrivate)
		if err != nil {
			t.Fatal(err)
		}
		bead.Metadata[beadmeta.LifecycleAdmissionReceiptV2MetadataKey] = encoded
		got := EvaluateAdmission(bead, cfg, "rig:pilot")
		verified, err := VerifyAdmissionReceiptV2(bead, cfg, "rig:pilot")
		if err != nil || verified.ExpectedWorkRevision != -17 {
			t.Fatalf("negative reviewed revision envelope = %+v, %v, want exact nonzero opaque token", verified, err)
		}
		if got.Admitted || !got.Requested {
			t.Fatalf("negative reviewed revision decision = %+v, want proof hold", got)
		}
	})

	t.Run("v1 only", func(t *testing.T) {
		legacy := beads.Bead{ID: bead.ID, Labels: []string{AdmissionIntentLabel}, Metadata: map[string]string{
			beadmeta.LifecycleAdmissionReceiptMetadataKey: `{"version":1,"work_item_id":"work-1","scope":"rig:pilot","route":"rig/coder"}`,
		}}
		if got := EvaluateAdmission(legacy, cfg, "rig:pilot"); !got.Requested || got.Admitted {
			t.Fatalf("v1-only decision = %+v, want requested but held", got)
		}
		if _, err := VerifyAdmissionReceiptV2(legacy, cfg, "rig:pilot"); err == nil {
			t.Fatal("historical v1 receipt verified as v2")
		}
	})

	t.Run("v1 and v2 conflict", func(t *testing.T) {
		changed := bead
		changed.Metadata = map[string]string{}
		for key, value := range bead.Metadata {
			changed.Metadata[key] = value
		}
		changed.Metadata[beadmeta.LifecycleAdmissionReceiptMetadataKey] = `{"version":1}`
		if got := EvaluateAdmission(changed, cfg, "rig:pilot"); got.Admitted || !got.Requested {
			t.Fatalf("conflicting receipt decision = %+v, want requested but held", got)
		}
		if _, err := VerifyAdmissionReceiptV2(changed, cfg, "rig:pilot"); err == nil {
			t.Fatal("conflicting v1/v2 receipts verified")
		}
	})
}

func TestAdmissionV1SignerParserAndDigestRemainHistorical(t *testing.T) {
	_, _, private, cfg := lifecycleKeys(t)
	receipt := AdmissionReceipt{
		Version: 1, WorkItemID: "legacy-1", Scope: "rig:pilot", Route: "rig/coder",
		Workflow: "mol-polecat-work", MergeStrategy: "mr", Deliverable: "commit",
		Verification: "test run", AcceptanceAuthority: "reviewer", AdmittedBy: "triage",
	}
	encoded, err := SignAdmissionReceipt(receipt, private)
	if err != nil {
		t.Fatal(err)
	}
	var parsed AdmissionReceipt
	if err := decodeStrict(encoded, &parsed); err != nil || parsed.Version != 1 || parsed.WorkItemID != receipt.WorkItemID {
		t.Fatalf("decode historical v1 receipt = %+v, %v", parsed, err)
	}
	payloadV1, err := AdmissionSigningBytes(parsed)
	if err != nil {
		t.Fatal(err)
	}
	payloadV2, err := AdmissionV2SigningBytes(AdmissionReceiptV2{
		Version: 2, WorkItemID: receipt.WorkItemID, Scope: receipt.Scope,
	})
	if err != nil {
		t.Fatal(err)
	}
	signature, err := base64.StdEncoding.DecodeString(parsed.Signature)
	if err != nil || !ed25519.Verify(private.Public().(ed25519.PublicKey), payloadV1, signature) ||
		ed25519.Verify(private.Public().(ed25519.PublicKey), payloadV2, signature) {
		t.Fatal("historical v1 signature did not remain bound to its v1 domain")
	}
	digest, err := AdmissionDigest(parsed)
	if err != nil || digest == "" {
		t.Fatalf("historical v1 digest = %q, %v", digest, err)
	}
	bead := beads.Bead{ID: receipt.WorkItemID, Labels: []string{AdmissionIntentLabel}, Metadata: map[string]string{
		beadmeta.LifecycleAdmissionReceiptMetadataKey: encoded,
	}}
	if got := EvaluateAdmission(bead, cfg, receipt.Scope); got.Admitted || !got.Requested {
		t.Fatalf("historical v1 admission decision = %+v, want durable hold", got)
	}
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
	admissionPrivate, _, _, cfg := lifecycleKeys(t)
	bead, _ := admittedBead(t, admissionPrivate)
	bead.Labels = nil // The verified receipt is durable enrollment; removing a label cannot bypass it.
	if got := EvaluateAdmission(bead, cfg, "rig:pilot"); !got.Requested || got.Admitted {
		t.Fatalf("label removal disabled proof hold: %+v", got)
	}
	if got := EvaluateAdmission(bead, cfg, "rig:other"); got.Admitted {
		t.Fatalf("receipt replayed across store scope: %+v", got)
	}
	if _, err := VerifyAdmissionReceiptV2(bead, cfg, "rig:pilot"); err != nil {
		t.Fatalf("label removal invalidated the exact signed envelope: %v", err)
	}
	if _, err := VerifyAdmissionReceiptV2(bead, cfg, "rig:other"); err == nil {
		t.Fatal("receipt replayed across store scope")
	}
}

func TestEvaluateCompletionRequiresTrustedAcceptanceBoundToContract(t *testing.T) {
	admissionPrivate, acceptancePrivate, _, cfg := lifecycleKeys(t)
	bead, admission := admittedBead(t, admissionPrivate)
	digest, err := AdmissionDigestV2(admission)
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
		AcceptedAt:      time.Now().UTC().Format(time.RFC3339Nano),
	}
	encoded, err := SignCompletionReceipt(completion, acceptancePrivate)
	if err != nil {
		t.Fatal(err)
	}
	bead.Metadata[beadmeta.LifecycleCompletionReceiptMetadataKey] = encoded
	if got := EvaluateCompletion(bead, cfg, "rig:pilot"); got.Accepted || !strings.Contains(got.Reason, "verified admission contract") {
		t.Fatalf("completion decision = %+v, want attachment/policy proof hold", got)
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

func TestEvaluateCompletionRequiresFreshnessPolicyAndHonorsItsBounds(t *testing.T) {
	admissionPrivate, acceptancePrivate, _, cfg := lifecycleKeys(t)
	bead, admission := admittedBead(t, admissionPrivate)
	digest, err := AdmissionDigestV2(admission)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name        string
		offset      time.Duration
		policyUnset bool
		wantAccept  bool
	}{
		{name: "fresh envelope still lacks attachment proof", offset: -2 * time.Hour},
		{name: "future within skew still lacks attachment proof", offset: time.Minute},
		{name: "future beyond skew", offset: 6 * time.Minute},
		{name: "expired", offset: -168*time.Hour - time.Second},
		{name: "unset freshness is disabled", offset: -time.Hour, policyUnset: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			configured := cfg
			if tc.policyUnset {
				configured.CompletionReceiptMaxAge = ""
				configured.CompletionClockSkew = ""
			}
			completion := CompletionReceipt{
				Version: 1, WorkItemID: bead.ID, Scope: "rig:pilot", AdmissionDigest: digest,
				DeliverableRef: "git:commit:abc123", VerificationRef: "test-run:run-456",
				AcceptedBy: "reviewer", AcceptedAt: now.Add(tc.offset).Format(time.RFC3339Nano),
			}
			encoded, err := SignCompletionReceipt(completion, acceptancePrivate)
			if err != nil {
				t.Fatal(err)
			}
			bead.Metadata[beadmeta.LifecycleCompletionReceiptMetadataKey] = encoded
			decision := EvaluateCompletionAt(bead, configured, "rig:pilot", now)
			if decision.Accepted != tc.wantAccept {
				t.Fatalf("completion decision = %+v, want accepted=%v", decision, tc.wantAccept)
			}
			if !strings.Contains(decision.Reason, "verified admission contract") {
				t.Fatalf("completion decision reason = %q, want fail-closed admission proof reason", decision.Reason)
			}
		})
	}
}
