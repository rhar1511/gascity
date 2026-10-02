package worklifecycle

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

func TestEvaluateAdmissionWithProofRequiresCurrentAttachmentAndPolicy(t *testing.T) {
	bead, cfg, inputs := proofAwareAdmissionFixture(t)

	t.Run("positive exact attachment and policy", func(t *testing.T) {
		got := EvaluateAdmissionWithProof(bead, cfg, inputs.PolicyProjection.SourceScope, inputs)
		if !got.Requested || !got.Admitted || got.Receipt.WorkItemID != bead.ID {
			t.Fatalf("EvaluateAdmissionWithProof() = %+v, want current proof admitted", got)
		}
	})

	t.Run("opaque revision token may decrease", func(t *testing.T) {
		current := bead
		current.Revision = 5
		changed := inputs
		attachment := *inputs.Attachment
		attachment.ToRevision = current.Revision
		changed.Attachment = &attachment
		got := EvaluateAdmissionWithProof(current, cfg, inputs.PolicyProjection.SourceScope, changed)
		if !got.Requested || !got.Admitted {
			t.Fatalf("lower distinct current revision token decision = %+v, want admitted", got)
		}
	})

	t.Run("missing attachment", func(t *testing.T) {
		changed := inputs
		changed.Attachment = nil
		if got := EvaluateAdmissionWithProof(bead, cfg, inputs.PolicyProjection.SourceScope, changed); !got.Requested || got.Admitted {
			t.Fatalf("missing attachment decision = %+v, want fail-closed hold", got)
		}
	})

	t.Run("stale attachment revision", func(t *testing.T) {
		changed := inputs
		attachment := *inputs.Attachment
		attachment.ToRevision = bead.Revision - 1
		changed.Attachment = &attachment
		if got := EvaluateAdmissionWithProof(bead, cfg, inputs.PolicyProjection.SourceScope, changed); !got.Requested || got.Admitted {
			t.Fatalf("stale attachment decision = %+v, want fail-closed hold", got)
		}
	})

	t.Run("wrong attachment identity", func(t *testing.T) {
		changed := inputs
		attachment := *inputs.Attachment
		attachment.ReceiptID = "another-q43-transition"
		changed.Attachment = &attachment
		if got := EvaluateAdmissionWithProof(bead, cfg, inputs.PolicyProjection.SourceScope, changed); !got.Requested || got.Admitted {
			t.Fatalf("wrong attachment decision = %+v, want fail-closed hold", got)
		}
	})

	t.Run("mismatched current policy", func(t *testing.T) {
		changed := inputs
		projection := *inputs.PolicyProjection
		projection.Target.MinActiveSessions++
		changed.PolicyProjection = &projection
		if got := EvaluateAdmissionWithProof(bead, cfg, inputs.PolicyProjection.SourceScope, changed); !got.Requested || got.Admitted {
			t.Fatalf("mismatched policy decision = %+v, want fail-closed hold", got)
		}
	})

	t.Run("historical v1 evidence", func(t *testing.T) {
		changed := bead
		changed.Metadata = cloneStringMap(bead.Metadata)
		delete(changed.Metadata, beadmeta.LifecycleAdmissionReceiptV2MetadataKey)
		changed.Metadata[beadmeta.LifecycleAdmissionReceiptMetadataKey] = `{"version":1}`
		if got := EvaluateAdmissionWithProof(changed, cfg, inputs.PolicyProjection.SourceScope, inputs); !got.Requested || got.Admitted {
			t.Fatalf("v1 decision = %+v, want historical hold", got)
		}
	})

	t.Run("conflicting v1 and v2 evidence", func(t *testing.T) {
		changed := bead
		changed.Metadata = cloneStringMap(bead.Metadata)
		changed.Metadata[beadmeta.LifecycleAdmissionReceiptMetadataKey] = `{"version":1}`
		if got := EvaluateAdmissionWithProof(changed, cfg, inputs.PolicyProjection.SourceScope, inputs); !got.Requested || got.Admitted {
			t.Fatalf("conflicting receipt decision = %+v, want fail-closed hold", got)
		}
	})

	t.Run("signed v2 receipt alone still holds", func(t *testing.T) {
		got := EvaluateAdmission(bead, cfg, inputs.PolicyProjection.SourceScope)
		if !got.Requested || got.Admitted || !strings.Contains(got.Reason, "attachment and current route-policy proof") {
			t.Fatalf("EvaluateAdmission() = %+v, want default fail-closed hold", got)
		}
	})
}

func TestEvaluateAdmissionWithTransitionProofRequiresVerifiedQ54Head(t *testing.T) {
	bead, cfg, inputs := proofAwareAdmissionFixture(t)
	bead.Revision++
	receipt, err := VerifyAdmissionReceiptV2(bead, cfg, inputs.PolicyProjection.SourceScope)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := AdmissionDigestV2(receipt)
	if err != nil {
		t.Fatal(err)
	}
	encoded := bead.Metadata[beadmeta.LifecycleAdmissionReceiptV2MetadataKey]
	marker := transitionMaterialization{
		Version: 1, State: "reserved", Scope: receipt.Scope, Contract: digest, Route: receipt.Route,
		Workflow: receipt.Workflow, MergeStrategy: receipt.MergeStrategy, Token: "reservation-token",
		SourceID: bead.ID, SourceStoreRef: "rig:pilot", WorkflowStoreRef: "rig:pilot", AdmissionReceipt: encoded,
	}
	markerBytes, err := json.Marshal(marker)
	if err != nil {
		t.Fatal(err)
	}
	bead.Metadata[beadmeta.LifecycleMaterializationMetadataKey] = string(markerBytes)
	verifiedHead := TransitionHead{ReceiptID: "q54-reservation", ToVersion: bead.Revision, Step: TransitionStepReservation, verified: true}

	t.Run("verified reservation head accepts historical Q43", func(t *testing.T) {
		got := EvaluateAdmissionWithTransitionProof(bead, cfg, receipt.Scope, inputs, verifiedHead)
		if !got.Requested || !got.Admitted {
			t.Fatalf("transition-aware decision = %+v, want exact verified reservation admitted", got)
		}
	})

	t.Run("ordinary proof gate still rejects historical Q43", func(t *testing.T) {
		got := EvaluateAdmissionWithProof(bead, cfg, receipt.Scope, inputs)
		if !got.Requested || got.Admitted {
			t.Fatalf("ordinary proof gate decision = %+v, want fail-closed hold", got)
		}
	})

	for _, tc := range []struct {
		name string
		head TransitionHead
	}{
		{name: "unverified head", head: TransitionHead{ReceiptID: verifiedHead.ReceiptID, ToVersion: bead.Revision, Step: verifiedHead.Step}},
		{name: "wrong step", head: TransitionHead{ReceiptID: verifiedHead.ReceiptID, ToVersion: bead.Revision, Step: TransitionStepClaimIdentity, verified: true}},
		{name: "stale revision", head: TransitionHead{ReceiptID: verifiedHead.ReceiptID, ToVersion: bead.Revision - 1, Step: verifiedHead.Step, verified: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := EvaluateAdmissionWithTransitionProof(bead, cfg, receipt.Scope, inputs, tc.head)
			if !got.Requested || got.Admitted {
				t.Fatalf("transition-aware decision = %+v, want fail-closed hold", got)
			}
		})
	}

	t.Run("verified post-attachment Q54 head retains admission", func(t *testing.T) {
		attached := bead
		attached.Metadata = cloneStringMap(bead.Metadata)
		marker.State = "attached"
		marker.WorkflowID = "workflow-1"
		encodedMarker, err := json.Marshal(marker)
		if err != nil {
			t.Fatal(err)
		}
		attached.Metadata[beadmeta.LifecycleMaterializationMetadataKey] = string(encodedMarker)
		attached.Metadata[beadmeta.RoutedToMetadataKey] = receipt.Route
		attached.Metadata[beadmeta.MoleculeIDMetadataKey] = marker.WorkflowID
		attached.Metadata[beadmeta.MergeStrategyMetadataKey] = receipt.MergeStrategy
		laterHead := TransitionHead{ReceiptID: "q54-claim", ToVersion: attached.Revision, Step: TransitionStepClaimIdentity, verified: true}
		got := EvaluateAdmissionWithTransitionProof(attached, cfg, receipt.Scope, inputs, laterHead)
		if !got.Requested || !got.Admitted {
			t.Fatalf("transition-aware decision = %+v, want attached source with a later verified Q54 head admitted", got)
		}
	})

	t.Run("reservation marker must match the verified step", func(t *testing.T) {
		changed := bead
		changed.Metadata = cloneStringMap(bead.Metadata)
		marker.State = "attached"
		marker.WorkflowID = "workflow-1"
		changedMarker, err := json.Marshal(marker)
		if err != nil {
			t.Fatal(err)
		}
		changed.Metadata[beadmeta.LifecycleMaterializationMetadataKey] = string(changedMarker)
		got := EvaluateAdmissionWithTransitionProof(changed, cfg, receipt.Scope, inputs, verifiedHead)
		if !got.Requested || got.Admitted {
			t.Fatalf("mismatched marker/head decision = %+v, want fail-closed hold", got)
		}
	})
}

func proofAwareAdmissionFixture(t *testing.T) (beads.Bead, config.LifecycleConfig, AdmissionProofInputsV2) {
	t.Helper()
	admissionPrivate, _, _, cfg := lifecycleKeys(t)
	projection := validAdmissionPolicyProjectionV2(t)
	policyDigest, err := DigestAdmissionPolicyV2(projection)
	if err != nil {
		t.Fatal(err)
	}
	receipt := AdmissionReceiptV2{
		Version: 2, WorkItemID: "work-1", Scope: projection.SourceScope,
		ExpectedWorkRevision: 17, Route: projection.Target.Identity, Workflow: projection.Workflow,
		RoutingPolicyDigest: policyDigest, MergeStrategy: projection.MergeStrategy,
		Deliverable: "reviewed patch", Verification: "acceptance tests",
		AcceptanceAuthority: "reviewer", AdmittedBy: "triage",
	}
	encoded, err := SignAdmissionReceiptV2(receipt, admissionPrivate)
	if err != nil {
		t.Fatal(err)
	}
	bead := beads.Bead{
		ID: "work-1", Revision: 18, Status: "open", Labels: []string{AdmissionIntentLabel},
		Metadata: map[string]string{beadmeta.LifecycleAdmissionReceiptV2MetadataKey: encoded},
	}
	digest, err := AdmissionDigestV2(receipt)
	if err != nil {
		t.Fatal(err)
	}
	request, err := admissionAttachmentRequest(receipt, encoded, digest)
	if err != nil {
		t.Fatal(err)
	}
	return bead, cfg, AdmissionProofInputsV2{
		Attachment: &AdmissionAttachmentProof{
			SchemaVersion: 1, ReceiptID: request.ReceiptID, WorkItemID: bead.ID,
			Scope: receipt.Scope, ReceiptDigest: digest,
			FromRevision: receipt.ExpectedWorkRevision, ToRevision: bead.Revision,
		},
		PolicyProjection: &projection,
	}
}
