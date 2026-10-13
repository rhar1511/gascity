package worklifecycle

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

const descendantCompletionKind = "lifecycle_descendant_completion_v1"

// DescendantCompletionRequest finishes one already claimed execution step.
// It cannot accept or close the separately admitted source.
type DescendantCompletionRequest struct {
	ClaimIdentityRequest
	ClaimGeneration string
	Outcome         string
}

// CompleteDescendantExecution verifies the immutable anchor and incarnation-
// bound claim before appending a protected terminal patch. Verify must also
// accept a closed step for exact replay, without relaxing parent authority.
func CompleteDescendantExecution(cfg DescendantClaimConfig, request DescendantCompletionRequest) (ClaimIdentityResult, error) {
	reader, sourceOK := beads.DecisionFrontierSourceReaderFor(cfg.Store)
	anchors, anchorOK := beads.ControllerMetadataTransitionReceiptReaderFor(cfg.Store)
	patches, patchOK := beads.RevisionTransitionPatchReceiptReaderFor(cfg.Store)
	writer, writeOK := beads.RevisionTransitionPatchWriterFor(cfg.Store)
	if !sourceOK || !anchorOK || !patchOK || !writeOK || reader == nil || anchors == nil || patches == nil || writer == nil || cfg.PermitIssuer == nil || cfg.Verify == nil {
		return ClaimIdentityResult{}, ErrTransitionChainUnavailable
	}
	if !validTransitionText(cfg.Scope, 255) || !validTransitionText(cfg.Actor, 255) || !validTransitionText(cfg.AnchorActor, 255) ||
		!validTransitionText(request.IssueID, 200) || request.ExpectedRevision == 0 || request.ExpectedTransitionHead == "" || request.ClaimGeneration != "1" ||
		(request.Outcome != "pass" && request.Outcome != "fail" && request.Outcome != "skipped") {
		return ClaimIdentityResult{}, fmt.Errorf("invalid completion identity or outcome: %w", ErrTransitionChainInvalid)
	}
	op, err := ClaimIdentityOperationID(request.SessionID, request.InstanceToken, request.RuntimeEpoch, request.ClaimGeneration)
	if err != nil {
		return ClaimIdentityResult{}, err
	}
	current, err := reader.DecisionFrontierSourceSnapshot(request.IssueID)
	if err != nil {
		return ClaimIdentityResult{}, err
	}
	binding, err := cfg.Verify(current)
	if err != nil {
		return ClaimIdentityResult{}, fmt.Errorf("verify descendant before completion: %w", err)
	}
	if current.ID != request.IssueID || current.Revision == 0 || binding.Scope != cfg.Scope || binding.ParentID == current.ID ||
		!validTransitionText(binding.ParentID, 200) || !validTransitionText(binding.WorkflowID, 200) || binding.StoreRef == "" ||
		!validAdmissionDigest(binding.AdmissionDigest) || !validLowercaseDigest(binding.LineageDigest) {
		return ClaimIdentityResult{}, fmt.Errorf("invalid completion binding: %w", ErrTransitionChainEvidence)
	}
	payload, err := json.Marshal(binding)
	if err != nil {
		return ClaimIdentityResult{}, err
	}
	identity := sha256.Sum256(append([]byte("gascity.lifecycle.descendant-anchor.v1\n"+current.ID+"\n"), payload...))
	anchorID := "gc-descendant-anchor-v1-" + hex.EncodeToString(identity[:])
	anchor, found, err := anchors.ControllerMetadataTransitionReceipt(current.ID, anchorID)
	if err != nil || !found {
		return ClaimIdentityResult{}, errors.Join(ErrTransitionChainReceipt, err)
	}
	value, _ := json.Marshal(anchorID)
	rawValue := json.RawMessage(value)
	if !sameDescendantAnchor(anchor, beads.ControllerMetadataTransitionRequest{
		ReceiptID: anchorID, Scope: cfg.Scope, Kind: descendantAnchorKind, Actor: cfg.AnchorActor,
		ExpectedVersion: anchor.ExpectedVersion, Key: beadmeta.LifecycleTransitionHeadMetadataKey, Value: &rawValue, Payload: payload,
	}, current.ID) {
		return ClaimIdentityResult{}, fmt.Errorf("completion anchor differs from signed workflow binding: %w", ErrTransitionChainReceipt)
	}
	anchorDigest, err := controllerMetadataTransitionReceiptDigest(anchor)
	if err != nil {
		return ClaimIdentityResult{}, err
	}
	claimID, err := transitionReceiptID(current.ID, cfg.Scope, TransitionStepClaimIdentity, op)
	if err != nil {
		return ClaimIdentityResult{}, err
	}
	claim, found, err := patches.ReadRevisionTransitionPatchReceipt(claimID)
	if err != nil || !found {
		return ClaimIdentityResult{}, errors.Join(ErrTransitionChainReceipt, err)
	}
	claimPatch, err := sourcePatchFromCanonical(claim.Patch)
	if err == nil {
		claimPatch, err = stripTransitionHeadPatch(claimPatch)
	}
	if err != nil || validateTransitionStepPatch(TransitionStepClaimIdentity, claimPatch) != nil ||
		claimPatch.Assignee == nil || claimPatch.Assignee.Value != cfg.Actor ||
		claimPatch.Metadata[beadmeta.SessionIDMetadataKey].Value != request.SessionID ||
		claimPatch.Metadata[beadmeta.ClaimGenerationMetadataKey].Value != request.ClaimGeneration {
		return ClaimIdentityResult{}, fmt.Errorf("completion claim patch does not bind this session and generation: %w", ErrTransitionChainReceipt)
	}
	effective, err := transitionPatchWithHead(claimPatch, TransitionStepClaimIdentity, anchorID, claimID)
	if err != nil {
		return ClaimIdentityResult{}, err
	}
	canonicalClaim, err := transitionPatch(effective)
	if err != nil {
		return ClaimIdentityResult{}, err
	}
	_, claimDigest, err := verifyTransitionPatchReceipt(claim, current.ID, beads.RevisionTransitionPatchRequest{
		ReceiptID: claimID, Scope: cfg.Scope, Kind: descendantClaimKind, Actor: cfg.Actor,
		ExpectedVersion: anchor.ToVersion, PriorReceiptID: anchorID, PriorReceiptDigest: anchorDigest, Patch: canonicalClaim,
	})
	if err != nil {
		return ClaimIdentityResult{}, err
	}
	// The deterministic completion identity excludes outcome: a changed outcome
	// must conflict with the original receipt, never create a second completion.
	completionID, err := transitionReceiptID(current.ID, cfg.Scope, TransitionStep("descendant_completion"), op)
	if err != nil {
		return ClaimIdentityResult{}, err
	}
	patch, err := transitionPatchWithHead(SourceWorkPatch{
		Status:   &StringTransition{Expected: "in_progress", Value: "closed"},
		Metadata: map[string]MetadataStringPatch{beadmeta.OutcomeMetadataKey: {Value: request.Outcome}},
	}, TransitionStep("descendant_completion"), claimID, completionID)
	if err != nil {
		return ClaimIdentityResult{}, err
	}
	canonicalCompletion, err := transitionPatch(patch)
	if err != nil {
		return ClaimIdentityResult{}, err
	}
	write := beads.RevisionTransitionPatchRequest{
		ReceiptID: completionID, Scope: cfg.Scope, Kind: descendantCompletionKind, Actor: cfg.Actor,
		ExpectedVersion: claim.ToVersion, PriorReceiptID: claimID, PriorReceiptDigest: claimDigest, Patch: canonicalCompletion,
	}
	receipt, replay, err := patches.ReadRevisionTransitionPatchReceipt(completionID)
	if err != nil {
		return ClaimIdentityResult{}, err
	}
	if request.ExpectedRevision != claim.ToVersion || request.ExpectedTransitionHead != claimID {
		if !replay || request.ExpectedRevision != receipt.ToVersion || request.ExpectedTransitionHead != completionID {
			return ClaimIdentityResult{}, fmt.Errorf("completion observation is neither claim nor exact terminal receipt: %w", ErrTransitionChainStale)
		}
	}
	if current.Assignee != cfg.Actor || current.Metadata[beadmeta.SessionIDMetadataKey] != request.SessionID || current.Metadata[beadmeta.ClaimGenerationMetadataKey] != request.ClaimGeneration {
		return ClaimIdentityResult{}, fmt.Errorf("completion owner or generation changed: %w", ErrTransitionChainStale)
	}
	for key, change := range claimPatch.Metadata {
		if current.Metadata[key] != change.Value {
			return ClaimIdentityResult{}, fmt.Errorf("completion claim metadata changed: %w", ErrTransitionChainStale)
		}
	}
	recovered := false
	if !replay {
		if current.Status != "in_progress" || current.Revision != claim.ToVersion || current.Metadata[beadmeta.LifecycleTransitionHeadMetadataKey] != claimID {
			return ClaimIdentityResult{}, fmt.Errorf("completion requires the unchanged in-progress claim head: %w", ErrTransitionChainStale)
		}
		if _, exists := current.Metadata[beadmeta.OutcomeMetadataKey]; exists {
			return ClaimIdentityResult{}, fmt.Errorf("completion outcome is already present: %w", ErrTransitionChainStale)
		}
		digest, err := beads.RevisionTransitionPatchProtectedMutationDigest(current.ID, write)
		if err != nil {
			return ClaimIdentityResult{}, err
		}
		permit, err := cfg.PermitIssuer.IssueProtectedMutation(beads.ControllerProtectedMutationRequest{
			Operation: beads.RevisionTransitionPatchProtectedMutationOperation, ResourceIDs: []string{current.ID}, RequestDigest: digest,
		}, completionID)
		if err != nil || !validTransitionPermit(permit) {
			return ClaimIdentityResult{}, errors.Join(ErrTransitionChainPermit, err)
		}
		freshBinding, err := cfg.Verify(current)
		if err != nil || freshBinding != binding {
			return ClaimIdentityResult{}, fmt.Errorf("verify descendant completion binding: %w", errors.Join(ErrTransitionChainEvidence, err))
		}
		write.ProtectedPermit = permit
		result, writeErr := writer.TransitionPatch(current.ID, write)
		receipt, found, err = patches.ReadRevisionTransitionPatchReceipt(completionID)
		if err != nil || !found {
			return ClaimIdentityResult{}, errors.Join(ErrTransitionChainReceipt, writeErr, err)
		}
		if writeErr == nil {
			if !result.Applied || result.Receipt == nil {
				return ClaimIdentityResult{}, fmt.Errorf("completion write omitted its receipt: %w", ErrTransitionChainReceipt)
			}
			if _, _, err := verifyTransitionPatchReceipt(*result.Receipt, current.ID, write); err != nil {
				return ClaimIdentityResult{}, err
			}
		}
		recovered = writeErr != nil
	}
	if _, _, err := verifyTransitionPatchReceipt(receipt, current.ID, write); err != nil {
		return ClaimIdentityResult{}, err
	}
	committed, err := reader.DecisionFrontierSourceSnapshot(current.ID)
	if err != nil {
		return ClaimIdentityResult{}, err
	}
	freshBinding, err := cfg.Verify(committed)
	if err != nil || freshBinding != binding {
		return ClaimIdentityResult{}, fmt.Errorf("verify descendant completion binding: %w", errors.Join(ErrTransitionChainEvidence, err))
	}
	if committed.ID != current.ID || committed.Revision != receipt.ToVersion || committed.Status != "closed" || committed.Assignee != cfg.Actor ||
		committed.Metadata[beadmeta.LifecycleTransitionHeadMetadataKey] != completionID || committed.Metadata[beadmeta.OutcomeMetadataKey] != request.Outcome {
		return ClaimIdentityResult{}, fmt.Errorf("completion readback differs from terminal receipt: %w", ErrTransitionChainStale)
	}
	for key, change := range claimPatch.Metadata {
		if committed.Metadata[key] != change.Value {
			return ClaimIdentityResult{}, fmt.Errorf("completion readback changed claim metadata: %w", ErrTransitionChainStale)
		}
	}
	return ClaimIdentityResult{Receipt: receipt, ClaimGeneration: request.ClaimGeneration, Replayed: replay, Recovered: recovered}, nil
}
