package worklifecycle

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

const (
	descendantAnchorKind = "lifecycle_descendant_anchor_v1"
	descendantClaimKind  = "lifecycle_descendant_claim_identity_v1"
)

// DescendantBinding is returned only by the trusted controller after verifying
// the current signed parent, its transition chain, and exact workflow membership.
// It deliberately excludes the mutable parent head: that head is reverified on
// every call, while this immutable binding survives legitimate parent progress.
type DescendantBinding struct {
	AdmissionDigest string `json:"admission_digest"`
	LineageDigest   string `json:"lineage_digest"`
	ParentID        string `json:"parent_id"`
	Scope           string `json:"scope"`
	StoreRef        string `json:"store_ref"`
	WorkflowID      string `json:"workflow_id"`
}

// DescendantClaimConfig supplies controller authority, never worker metadata
// as authority. Verify must check current parent policy, membership, session
// route, holds and (for open work) readiness on each invocation.
type DescendantClaimConfig struct {
	Store        beads.Store
	Scope        string
	Actor        string
	AnchorActor  string
	PermitIssuer beads.ControllerProtectedMutationPermitIssuer
	Verify       func(beads.Bead) (DescendantBinding, error)
}

// ClaimDescendantIdentity anchors a generated step with its own protected Q43
// receipt before an atomic Q54 claim. Partial anchor commits and lost claim
// responses are recoverable without repeating either mutation. This initial
// claim path refuses already released or otherwise advanced descendants.
func ClaimDescendantIdentity(cfg DescendantClaimConfig, request ClaimIdentityRequest) (ClaimIdentityResult, error) {
	reader, sourceOK := beads.DecisionFrontierSourceReaderFor(cfg.Store)
	anchorWriter, anchorOK := beads.ControllerMetadataTransitionWriterFor(cfg.Store)
	anchorReader, receiptOK := beads.ControllerMetadataTransitionReceiptReaderFor(cfg.Store)
	patchWriter, patchOK := beads.RevisionTransitionPatchWriterFor(cfg.Store)
	patchReader, patchReadOK := beads.RevisionTransitionPatchReceiptReaderFor(cfg.Store)
	if !sourceOK || !anchorOK || !receiptOK || !patchOK || !patchReadOK || reader == nil || anchorWriter == nil || anchorReader == nil || patchWriter == nil || patchReader == nil || cfg.PermitIssuer == nil || cfg.Verify == nil {
		return ClaimIdentityResult{}, ErrTransitionChainUnavailable
	}
	if !validTransitionText(cfg.Scope, 255) || !validTransitionText(cfg.Actor, 255) || !validTransitionText(cfg.AnchorActor, 255) || !validTransitionText(request.IssueID, 200) || request.ExpectedRevision == 0 {
		return ClaimIdentityResult{}, ErrTransitionChainInvalid
	}
	operationID, err := ClaimIdentityOperationID(request.SessionID, request.InstanceToken, request.RuntimeEpoch, "1")
	if err != nil {
		return ClaimIdentityResult{}, err
	}
	current, err := reader.DecisionFrontierSourceSnapshot(request.IssueID)
	if err != nil {
		return ClaimIdentityResult{}, err
	}
	binding, err := cfg.Verify(current)
	if err != nil {
		return ClaimIdentityResult{}, fmt.Errorf("verify descendant before anchor: %w", err)
	}
	if current.ID != request.IssueID || current.Revision == 0 || binding.Scope != cfg.Scope || binding.ParentID == current.ID ||
		!validTransitionText(binding.ParentID, 200) || !validTransitionText(binding.WorkflowID, 200) || binding.StoreRef == "" ||
		!validAdmissionDigest(binding.AdmissionDigest) || !validLowercaseDigest(binding.LineageDigest) {
		return ClaimIdentityResult{}, ErrTransitionChainEvidence
	}
	payload, err := json.Marshal(binding)
	if err != nil {
		return ClaimIdentityResult{}, err
	}
	identity := sha256.Sum256(append([]byte("gascity.lifecycle.descendant-anchor.v1\n"+current.ID+"\n"), payload...))
	anchorID := "gc-descendant-anchor-v1-" + hex.EncodeToString(identity[:])
	value, _ := json.Marshal(anchorID)
	rawValue := json.RawMessage(value)
	anchorRequest := beads.ControllerMetadataTransitionRequest{
		ReceiptID: anchorID, Scope: cfg.Scope, Kind: descendantAnchorKind, Actor: cfg.AnchorActor,
		ExpectedVersion: request.ExpectedRevision, Key: beadmeta.LifecycleTransitionHeadMetadataKey,
		Value: &rawValue, Payload: payload,
	}
	anchor, found, err := anchorReader.ControllerMetadataTransitionReceipt(current.ID, anchorID)
	if err != nil {
		return ClaimIdentityResult{}, err
	}
	if !found {
		if current.Revision != request.ExpectedRevision || request.ExpectedTransitionHead != "" || current.Metadata[beadmeta.LifecycleTransitionHeadMetadataKey] != "" || !unclaimedDescendant(current) {
			return ClaimIdentityResult{}, fmt.Errorf("descendant anchor requires an unchanged unclaimed step: %w", ErrTransitionChainStale)
		}
		if _, present := current.Metadata[beadmeta.LifecycleTransitionHeadMetadataKey]; present {
			return ClaimIdentityResult{}, ErrTransitionChainEvidence
		}
		digest, digestErr := beads.ControllerMetadataTransitionProtectedMutationDigest(current.ID, anchorRequest)
		if digestErr != nil {
			return ClaimIdentityResult{}, digestErr
		}
		permit, permitErr := cfg.PermitIssuer.IssueProtectedMutation(beads.ControllerProtectedMutationRequest{
			Operation: beads.ControllerMetadataTransitionProtectedMutationOperation, ResourceIDs: []string{current.ID}, RequestDigest: digest,
		}, anchorID)
		if permitErr != nil || !validTransitionPermit(permit) {
			return ClaimIdentityResult{}, errors.Join(ErrTransitionChainPermit, permitErr)
		}
		anchorRequest.ProtectedPermit = permit
		result, writeErr := anchorWriter.TransitionMetadata(current.ID, anchorRequest)
		anchor, found, err = anchorReader.ControllerMetadataTransitionReceipt(current.ID, anchorID)
		if err != nil || !found {
			return ClaimIdentityResult{}, errors.Join(ErrTransitionChainReceipt, writeErr, err)
		}
		if writeErr == nil && (!result.Applied || result.Receipt == nil || !sameDescendantAnchor(*result.Receipt, anchorRequest, current.ID)) {
			return ClaimIdentityResult{}, ErrTransitionChainReceipt
		}
	}
	// Existing anchors bind either the original pre-anchor request or a fresh
	// observation. Never replace their original revision with the caller's value.
	anchorRequest.ExpectedVersion = anchor.ExpectedVersion
	if !sameDescendantAnchor(anchor, anchorRequest, current.ID) {
		return ClaimIdentityResult{}, ErrTransitionChainReceipt
	}
	current, err = reader.DecisionFrontierSourceSnapshot(request.IssueID)
	if err != nil {
		return ClaimIdentityResult{}, err
	}
	verifiedBinding, err := cfg.Verify(current)
	if err != nil || verifiedBinding != binding {
		return ClaimIdentityResult{}, errors.Join(ErrTransitionChainEvidence, err)
	}
	claimID, err := transitionReceiptID(current.ID, cfg.Scope, TransitionStepClaimIdentity, operationID)
	if err != nil {
		return ClaimIdentityResult{}, err
	}
	priorDigest, err := controllerMetadataTransitionReceiptDigest(anchor)
	if err != nil {
		return ClaimIdentityResult{}, err
	}
	claimRequest := beads.RevisionTransitionPatchRequest{
		ReceiptID: claimID, Scope: cfg.Scope, Kind: descendantClaimKind, Actor: cfg.Actor,
		ExpectedVersion: anchor.ToVersion, PriorReceiptID: anchorID, PriorReceiptDigest: priorDigest,
	}
	receipt, replay, err := patchReader.ReadRevisionTransitionPatchReceipt(claimID)
	if err != nil {
		return ClaimIdentityResult{}, err
	}
	originalObservation := request.ExpectedRevision == anchor.ExpectedVersion && request.ExpectedTransitionHead == ""
	anchorObservation := request.ExpectedRevision == anchor.ToVersion && request.ExpectedTransitionHead == anchorID
	if replay {
		currentObservation := request.ExpectedRevision == current.Revision && request.ExpectedTransitionHead == claimID
		if !originalObservation && !anchorObservation && !currentObservation {
			return ClaimIdentityResult{}, fmt.Errorf("descendant replay does not bind the anchor or current claim: %w", ErrTransitionChainStale)
		}
		callerPatch, patchErr := sourcePatchFromCanonical(receipt.Patch)
		if patchErr != nil {
			return ClaimIdentityResult{}, patchErr
		}
		callerPatch, patchErr = stripTransitionHeadPatch(callerPatch)
		if patchErr != nil || validateTransitionStepPatch(TransitionStepClaimIdentity, callerPatch) != nil ||
			callerPatch.Metadata[beadmeta.SessionIDMetadataKey].Value != request.SessionID ||
			callerPatch.Metadata[beadmeta.ClaimGenerationMetadataKey].Value != "1" ||
			callerPatch.Assignee == nil || callerPatch.Assignee.Value != cfg.Actor {
			return ClaimIdentityResult{}, ErrTransitionChainReceipt
		}
		effective, patchErr := transitionPatchWithHead(callerPatch, TransitionStepClaimIdentity, anchorID, claimID)
		if patchErr != nil {
			return ClaimIdentityResult{}, patchErr
		}
		claimRequest.Patch, err = transitionPatch(effective)
		if err != nil {
			return ClaimIdentityResult{}, err
		}
	} else {
		if !originalObservation && !anchorObservation || current.Revision != anchor.ToVersion || current.Metadata[beadmeta.LifecycleTransitionHeadMetadataKey] != anchorID || !unclaimedDescendant(current) {
			return ClaimIdentityResult{}, fmt.Errorf("descendant claim requires its unchanged unclaimed anchor: %w", ErrTransitionChainStale)
		}
		patch := SourceWorkPatch{
			Status: &StringTransition{Expected: "open", Value: "in_progress"}, Assignee: &StringTransition{Expected: "", Value: cfg.Actor},
			Metadata: map[string]MetadataStringPatch{
				beadmeta.ClaimGenerationMetadataKey: {Value: "1"}, beadmeta.ClaimedAtMetadataKey: {Value: time.Now().UTC().Format(time.RFC3339)}, beadmeta.SessionIDMetadataKey: {Value: request.SessionID},
			},
		}
		for _, optional := range []struct{ key, value string }{{beadmeta.SessionNameMetadataKey, request.SessionName}, {beadmeta.WorkDirMetadataKey, request.WorkDir}, {beadmeta.WorkBranchMetadataKey, request.WorkBranch}} {
			if optional.value == "" {
				continue
			}
			change := MetadataStringPatch{Value: optional.value}
			if previous, present := current.Metadata[optional.key]; present {
				change.Expected = &previous
			}
			patch.Metadata[optional.key] = change
		}
		if err := validateTransitionStepPatch(TransitionStepClaimIdentity, patch); err != nil {
			return ClaimIdentityResult{}, err
		}
		effective, patchErr := transitionPatchWithHead(patch, TransitionStepClaimIdentity, anchorID, claimID)
		if patchErr != nil {
			return ClaimIdentityResult{}, patchErr
		}
		claimRequest.Patch, err = transitionPatch(effective)
		if err != nil {
			return ClaimIdentityResult{}, err
		}
	}
	recovered := false
	if !replay {
		digest, digestErr := beads.RevisionTransitionPatchProtectedMutationDigest(current.ID, claimRequest)
		if digestErr != nil {
			return ClaimIdentityResult{}, digestErr
		}
		permit, permitErr := cfg.PermitIssuer.IssueProtectedMutation(beads.ControllerProtectedMutationRequest{
			Operation: beads.RevisionTransitionPatchProtectedMutationOperation, ResourceIDs: []string{current.ID}, RequestDigest: digest,
		}, claimID)
		if permitErr != nil || !validTransitionPermit(permit) {
			return ClaimIdentityResult{}, errors.Join(ErrTransitionChainPermit, permitErr)
		}
		claimRequest.ProtectedPermit = permit
		result, writeErr := patchWriter.TransitionPatch(current.ID, claimRequest)
		receipt, found, err = patchReader.ReadRevisionTransitionPatchReceipt(claimID)
		if err != nil || !found {
			return ClaimIdentityResult{}, errors.Join(ErrTransitionChainReceipt, writeErr, err)
		}
		if writeErr == nil {
			if !result.Applied || result.Receipt == nil {
				return ClaimIdentityResult{}, ErrTransitionChainReceipt
			}
			if _, _, resultErr := verifyTransitionPatchReceipt(*result.Receipt, current.ID, claimRequest); resultErr != nil {
				return ClaimIdentityResult{}, resultErr
			}
		}
		recovered = writeErr != nil
	}
	if _, _, err := verifyTransitionPatchReceipt(receipt, current.ID, claimRequest); err != nil {
		return ClaimIdentityResult{}, err
	}
	committed, err := reader.DecisionFrontierSourceSnapshot(current.ID)
	if err != nil {
		return ClaimIdentityResult{}, err
	}
	verifiedBinding, err = cfg.Verify(committed)
	if err != nil || verifiedBinding != binding {
		return ClaimIdentityResult{}, errors.Join(ErrTransitionChainEvidence, err)
	}
	if committed.ID != current.ID || committed.Revision != receipt.ToVersion || committed.Status != "in_progress" || committed.Assignee != cfg.Actor ||
		committed.Metadata[beadmeta.LifecycleTransitionHeadMetadataKey] != claimID {
		return ClaimIdentityResult{}, fmt.Errorf("descendant claim readback differs from receipt: %w", ErrTransitionChainStale)
	}
	for _, change := range receipt.Patch.Metadata {
		var want string
		if change.Value == nil || json.Unmarshal(*change.Value, &want) != nil || committed.Metadata[change.Key] != want {
			return ClaimIdentityResult{}, ErrTransitionChainReceipt
		}
	}
	return ClaimIdentityResult{Receipt: receipt, ClaimGeneration: "1", Replayed: replay, Recovered: recovered}, nil
}

func unclaimedDescendant(row beads.Bead) bool {
	if row.Status != "open" || row.Assignee != "" {
		return false
	}
	for _, key := range []string{beadmeta.ClaimGenerationMetadataKey, beadmeta.SessionIDMetadataKey, beadmeta.ClaimedAtMetadataKey} {
		if _, exists := row.Metadata[key]; exists {
			return false
		}
	}
	return true
}

func sameDescendantAnchor(receipt beads.ControllerMetadataTransitionReceipt, request beads.ControllerMetadataTransitionRequest, issueID string) bool {
	return receipt.ReceiptID == request.ReceiptID && receipt.IssueID == issueID && receipt.Scope == request.Scope && receipt.Kind == request.Kind && receipt.Actor == request.Actor &&
		receipt.ExpectedVersion != 0 && receipt.ExpectedVersion == request.ExpectedVersion && receipt.ToVersion != 0 && receipt.ToVersion != receipt.ExpectedVersion &&
		receipt.Key == request.Key && len(receipt.Expected) == 0 && request.Value != nil && bytes.Equal(receipt.Value, *request.Value) && bytes.Equal(receipt.Payload, request.Payload)
}
