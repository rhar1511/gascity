package worklifecycle

import (
	"errors"
	"fmt"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// AdmissionProofInputsV2 contains current Q43 attachment evidence and the
// authoritative policy projection recomputed from current configuration.
// Callers must obtain Attachment through AdmissionAttachmentAdapter.VerifyCurrent.
type AdmissionProofInputsV2 struct {
	Attachment       *AdmissionAttachmentProof
	PolicyProjection *AdmissionPolicyProjectionV2
}

// EvaluateAdmissionWithProof is the proof-aware admission gate. EvaluateAdmission
// remains fail-closed for callers that do not have store-backed Q43 evidence.
func EvaluateAdmissionWithProof(bead beads.Bead, cfg config.LifecycleConfig, scope string, inputs AdmissionProofInputsV2) AdmissionDecision {
	decision := EvaluateAdmission(bead, cfg, scope)
	if !decision.Requested {
		return decision
	}
	receipt, err := VerifyAdmissionReceiptV2(bead, cfg, scope)
	if err != nil {
		decision.Reason = err.Error()
		return decision
	}
	if err := verifyAdmissionProofAttachment(bead, receipt, inputs.Attachment); err != nil {
		decision.Reason = err.Error()
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
