package worklifecycle

import (
	"errors"
	"fmt"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// AdmissionProofInputsV2 contains exact Q43 attachment evidence and the
// authoritative policy projection recomputed from current configuration.
// Callers must obtain Attachment through AdmissionAttachmentAdapter.
type AdmissionProofInputsV2 struct {
	Attachment       *AdmissionAttachmentProof
	PolicyProjection *AdmissionPolicyProjectionV2
}

// EvaluateAdmissionWithProof is the proof-aware admission gate. EvaluateAdmission
// remains fail-closed for callers that do not have store-backed Q43 evidence.
func EvaluateAdmissionWithProof(bead beads.Bead, cfg config.LifecycleConfig, scope string, inputs AdmissionProofInputsV2) AdmissionDecision {
	return evaluateAdmissionWithProof(bead, cfg, scope, inputs, TransitionHead{}, false)
}

// EvaluateAdmissionWithTransitionProof accepts an advanced source revision
// only when CurrentHead verified the exact Q43 receipt and complete Q54 chain,
// and the current source marker still matches the reservation or attachment
// contract. The private verification bit prevents callers from manufacturing
// this authorization from public field values.
func EvaluateAdmissionWithTransitionProof(
	bead beads.Bead,
	cfg config.LifecycleConfig,
	scope string,
	inputs AdmissionProofInputsV2,
	head TransitionHead,
) AdmissionDecision {
	return evaluateAdmissionWithProof(bead, cfg, scope, inputs, head, true)
}

func evaluateAdmissionWithProof(
	bead beads.Bead,
	cfg config.LifecycleConfig,
	scope string,
	inputs AdmissionProofInputsV2,
	head TransitionHead,
	allowVerifiedTransition bool,
) AdmissionDecision {
	decision := EvaluateAdmission(bead, cfg, scope)
	if !decision.Requested {
		return decision
	}
	receipt, err := VerifyAdmissionReceiptV2(bead, cfg, scope)
	if err != nil {
		decision.Reason = err.Error()
		return decision
	}
	var attachmentErr error
	if allowVerifiedTransition && inputs.Attachment != nil && inputs.Attachment.ToRevision != bead.Revision {
		attachmentErr = verifyAdvancedAdmissionProofAttachment(bead, receipt, inputs.Attachment, head)
	} else {
		attachmentErr = verifyAdmissionProofAttachment(bead, receipt, inputs.Attachment)
	}
	if attachmentErr != nil {
		decision.Reason = attachmentErr.Error()
		return decision
	}
	if err := verifyAdmissionCurrentPolicy(receipt, scope, inputs.PolicyProjection); err != nil {
		decision.Reason = err.Error()
		return decision
	}
	return AdmissionDecision{
		Requested: true,
		Admitted:  true,
		Reason:    "v2 admission receipt has current Q43 attachment and route-policy proof",
		Receipt:   receipt,
	}
}

func verifyAdvancedAdmissionProofAttachment(bead beads.Bead, receipt AdmissionReceiptV2, proof *AdmissionAttachmentProof, head TransitionHead) error {
	if proof == nil || proof.SchemaVersion != 1 || proof.WorkItemID != bead.ID || proof.Scope != receipt.Scope ||
		proof.FromRevision != receipt.ExpectedWorkRevision || proof.ToRevision == 0 || proof.ToRevision == proof.FromRevision ||
		bead.Revision <= proof.ToRevision {
		return fmt.Errorf("%w: historical Q43 proof does not identify an older revision of this receipt", ErrAdmissionAttachmentInvalid)
	}
	digest, err := AdmissionDigestV2(receipt)
	if err != nil {
		return fmt.Errorf("%w: receipt digest could not be recomputed: %v", ErrAdmissionAttachmentInvalid, err)
	}
	encoded := bead.Metadata[beadmeta.LifecycleAdmissionReceiptV2MetadataKey]
	request, err := admissionAttachmentRequest(receipt, encoded, digest)
	if err != nil || proof.ReceiptID != request.ReceiptID || proof.ReceiptDigest != digest {
		return fmt.Errorf("%w: Q43 proof does not identify the exact signed receipt", ErrAdmissionAttachmentInvalid)
	}
	if !head.verified || head.FromAttachment || head.ReceiptID == "" || head.ToVersion != bead.Revision ||
		!validTransitionStep(head.Step) {
		return fmt.Errorf("%w: advanced source revision has no verified Q54 transition head", ErrAdmissionAttachmentInvalid)
	}
	marker, err := parseTransitionMaterialization(bead.Metadata[beadmeta.LifecycleMaterializationMetadataKey])
	if err != nil || !sameTransitionMaterializationContract(marker, transitionMaterialization{
		Version: 1, Scope: proof.Scope, Contract: digest, Route: receipt.Route, Workflow: receipt.Workflow,
		MergeStrategy: receipt.MergeStrategy, Token: marker.Token, SourceID: bead.ID,
		SourceStoreRef: marker.SourceStoreRef, WorkflowStoreRef: marker.WorkflowStoreRef,
		AdmissionReceipt: encoded,
	}) {
		return fmt.Errorf("%w: source materialization marker differs from the exact signed admission", ErrAdmissionAttachmentInvalid)
	}
	switch head.Step {
	case TransitionStepReservation:
		_, hasMerge := bead.Metadata[beadmeta.MergeStrategyMetadataKey]
		if marker.State != "reserved" || marker.WorkflowID != "" ||
			bead.Metadata[beadmeta.RoutedToMetadataKey] != "" || bead.Metadata[beadmeta.ExecutionRoutedToMetadataKey] != "" ||
			bead.Metadata[beadmeta.MoleculeIDMetadataKey] != "" || bead.Metadata[beadmeta.WorkflowIDMetadataKey] != "" ||
			bead.Metadata[beadmeta.LegacyWorkflowIDMetadataKey] != "" || hasMerge {
			return fmt.Errorf("%w: reservation head does not match the held source marker", ErrAdmissionAttachmentInvalid)
		}
	default:
		if marker.State != "attached" || marker.WorkflowID == "" ||
			bead.Metadata[beadmeta.RoutedToMetadataKey] != receipt.Route ||
			bead.Metadata[beadmeta.MoleculeIDMetadataKey] != marker.WorkflowID ||
			bead.Metadata[beadmeta.WorkflowIDMetadataKey] != "" || bead.Metadata[beadmeta.LegacyWorkflowIDMetadataKey] != "" ||
			bead.Metadata[beadmeta.ExecutionRoutedToMetadataKey] != "" ||
			bead.Metadata[beadmeta.MergeStrategyMetadataKey] != receipt.MergeStrategy {
			return fmt.Errorf("%w: materialization head does not match the attached source marker", ErrAdmissionAttachmentInvalid)
		}
	}
	return nil
}

func verifyAdmissionProofAttachment(bead beads.Bead, receipt AdmissionReceiptV2, proof *AdmissionAttachmentProof) error {
	if proof == nil {
		return errors.Join(ErrAdmissionAttachmentUnavailable, errors.New("current Q43 proof was not supplied"))
	}
	if proof.SchemaVersion != 1 || proof.WorkItemID != bead.ID || proof.Scope != receipt.Scope ||
		proof.FromRevision != receipt.ExpectedWorkRevision || proof.ToRevision != bead.Revision ||
		proof.ToRevision == 0 || proof.ToRevision == proof.FromRevision {
		return fmt.Errorf("%w: Q43 proof does not identify this receipt at the current work revision", ErrAdmissionAttachmentInvalid)
	}
	digest, err := AdmissionDigestV2(receipt)
	if err != nil {
		return fmt.Errorf("%w: receipt digest could not be recomputed: %v", ErrAdmissionAttachmentInvalid, err)
	}
	encoded := bead.Metadata[beadmeta.LifecycleAdmissionReceiptV2MetadataKey]
	request, err := admissionAttachmentRequest(receipt, encoded, digest)
	if err != nil || proof.ReceiptID != request.ReceiptID || proof.ReceiptDigest != digest {
		return fmt.Errorf("%w: Q43 proof does not identify the exact signed receipt", ErrAdmissionAttachmentInvalid)
	}
	return nil
}

func verifyAdmissionCurrentPolicy(receipt AdmissionReceiptV2, scope string, projection *AdmissionPolicyProjectionV2) error {
	if projection == nil {
		return errors.New("current admission policy projection is unavailable")
	}
	if projection.SourceScope != scope || projection.Target.Identity != receipt.Route ||
		projection.Workflow != receipt.Workflow || projection.MergeStrategy != receipt.MergeStrategy {
		return errors.New("current admission policy projection does not match the signed route, workflow, merge strategy, and scope")
	}
	digest, err := DigestAdmissionPolicyV2(*projection)
	if err != nil {
		return fmt.Errorf("current admission policy projection is invalid: %w", err)
	}
	if digest != receipt.RoutingPolicyDigest {
		return errors.New("current admission policy digest does not match the signed receipt")
	}
	return nil
}
