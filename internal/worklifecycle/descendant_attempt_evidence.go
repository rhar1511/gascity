package worklifecycle

import (
	"errors"
	"fmt"
	"maps"
	"strings"

	"github.com/gastownhall/gascity/internal/attemptevidence"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

const descendantAttemptEvidenceKind = "lifecycle_descendant_attempt_evidence_v1"

// DescendantCompletionConfig requires capture to finish before terminal close.
// CaptureAttempt owns snapshot/archive I/O. Its seal callback is the only path
// that may write the protected owner index and reference. A nil callback can
// verify an already-closed receipt but cannot finish an in-progress execution.
type DescendantCompletionConfig struct {
	DescendantClaimConfig
	CaptureAttempt func(owner beads.Bead, seal func(key, value string) error) error
}

type descendantCompletionPredecessor struct {
	receipt   beads.RevisionTransitionPatchReceipt
	digest    string
	current   beads.Bead
	replayed  bool
	recovered bool
}

func prepareDescendantCompletionEvidence(cfg DescendantCompletionConfig, request DescendantCompletionRequest, current beads.Bead, binding DescendantBinding, claim beads.RevisionTransitionPatchReceipt, claimDigest, operation string, completion beads.RevisionTransitionPatchReceipt, completed bool) (descendantCompletionPredecessor, error) {
	empty := descendantCompletionPredecessor{}
	reader, _ := beads.DecisionFrontierSourceReaderFor(cfg.Store)
	patches, _ := beads.RevisionTransitionPatchReceiptReaderFor(cfg.Store)
	writer, _ := beads.RevisionTransitionPatchWriterFor(cfg.Store)
	evidenceID, err := transitionReceiptID(current.ID, cfg.Scope, TransitionStep("descendant_attempt_evidence"), operation)
	if err != nil {
		return empty, err
	}
	sealed, found, err := patches.ReadRevisionTransitionPatchReceipt(evidenceID)
	if err != nil {
		return empty, err
	}
	claimObservation := request.ExpectedRevision == claim.ToVersion && request.ExpectedTransitionHead == claim.ReceiptID
	terminalObservation := completed && current.Status == "closed" && request.ExpectedRevision == completion.ToVersion && request.ExpectedTransitionHead == completion.ReceiptID
	if completed && completion.PriorReceiptID == claim.ReceiptID {
		// Historical terminal receipts remain exact, read-only replays. There
		// is no pending legacy evidence phase to mistake for a completed archive.
		if current.Status != "closed" || found || (!claimObservation && !terminalObservation) {
			return empty, ErrTransitionChainStale
		}
		return descendantCompletionPredecessor{receipt: claim, digest: claimDigest, current: current}, nil
	}
	if completed && current.Status != "closed" || !completed && current.Status != "in_progress" {
		return empty, ErrTransitionChainStale
	}
	verifySealed := func(receipt beads.RevisionTransitionPatchReceipt) (string, map[string]MetadataStringPatch, error) {
		patch, err := sourcePatchFromCanonical(receipt.Patch)
		if err == nil {
			patch, err = stripTransitionHeadPatch(patch)
		}
		if err != nil || patch.Status != nil || patch.Assignee != nil || len(patch.Metadata) != 2 {
			return "", nil, ErrTransitionChainReceipt
		}
		var key string
		for name := range patch.Metadata {
			if strings.HasPrefix(name, beadmeta.AttemptEvidenceIndexPrefix) {
				key = name
			}
		}
		wanted, err := descendantAttemptPatch(current, binding.StoreRef, request, key, patch.Metadata[key].Value)
		if err != nil {
			return "", nil, err
		}
		for name, change := range wanted {
			actual, exists := patch.Metadata[name]
			if !exists || actual.Expected != nil || actual.Value != change.Value {
				return "", nil, ErrTransitionChainReceipt
			}
		}
		write, err := descendantAttemptWrite(cfg, evidenceID, claim, claimDigest, wanted)
		if err != nil {
			return "", nil, err
		}
		_, digest, err := verifyTransitionPatchReceipt(receipt, current.ID, write)
		return digest, wanted, err
	}
	if found {
		_, patch, err := verifySealed(sealed)
		if err != nil {
			return empty, err
		}
		for key, change := range patch {
			if current.Metadata[key] != change.Value {
				return empty, fmt.Errorf("sealed descendant evidence is missing or changed: %w", ErrTransitionChainStale)
			}
		}
	}
	evidenceObservation := found && request.ExpectedRevision == sealed.ToVersion && request.ExpectedTransitionHead == evidenceID
	if !claimObservation && !evidenceObservation && !terminalObservation {
		return empty, fmt.Errorf("completion observation is neither claim, evidence nor terminal receipt: %w", ErrTransitionChainStale)
	}
	recovered := false
	if !completed {
		expected := claim
		if found {
			expected = sealed
		}
		if current.Revision != expected.ToVersion || current.Metadata[beadmeta.LifecycleTransitionHeadMetadataKey] != expected.ReceiptID {
			return empty, ErrTransitionChainStale
		}
		if _, exists := current.Metadata[beadmeta.OutcomeMetadataKey]; exists {
			return empty, ErrTransitionChainStale
		}
		if cfg.CaptureAttempt == nil {
			return empty, fmt.Errorf("descendant completion requires attempt capture: %w", ErrTransitionChainUnavailable)
		}
		if err := cfg.CaptureAttempt(current, func(key, value string) error {
			patch, err := descendantAttemptPatch(current, binding.StoreRef, request, key, value)
			if err != nil {
				return err
			}
			write, err := descendantAttemptWrite(cfg, evidenceID, claim, claimDigest, patch)
			if err != nil {
				return err
			}
			digest, err := beads.RevisionTransitionPatchProtectedMutationDigest(current.ID, write)
			if err != nil {
				return err
			}
			permit, err := cfg.PermitIssuer.IssueProtectedMutation(beads.ControllerProtectedMutationRequest{
				Operation: beads.RevisionTransitionPatchProtectedMutationOperation, ResourceIDs: []string{current.ID}, RequestDigest: digest,
			}, evidenceID)
			if err != nil || !validTransitionPermit(permit) {
				return errors.Join(ErrTransitionChainPermit, err)
			}
			freshBinding, err := cfg.Verify(current)
			if err != nil || freshBinding != binding {
				return errors.Join(ErrTransitionChainEvidence, err)
			}
			write.ProtectedPermit = permit
			result, writeErr := writer.TransitionPatch(current.ID, write)
			receipt, exists, err := patches.ReadRevisionTransitionPatchReceipt(evidenceID)
			if err != nil || !exists {
				return errors.Join(ErrTransitionChainReceipt, writeErr, err)
			}
			if writeErr == nil {
				if !result.Applied || result.Receipt == nil {
					return ErrTransitionChainReceipt
				}
				if _, _, err := verifyTransitionPatchReceipt(*result.Receipt, current.ID, write); err != nil {
					return err
				}
			}
			if _, _, err := verifyTransitionPatchReceipt(receipt, current.ID, write); err != nil {
				return err
			}
			recovered = writeErr != nil
			return nil
		}); err != nil {
			return empty, fmt.Errorf("capture descendant before completion: %w", err)
		}
	}
	// Capture may have repaired an existing archive without invoking seal.
	// Require the independently read protected receipt and owner index anyway.
	sealed, exists, err := patches.ReadRevisionTransitionPatchReceipt(evidenceID)
	if err != nil || !exists {
		return empty, errors.Join(ErrTransitionChainReceipt, err)
	}
	digest, patch, err := verifySealed(sealed)
	if err != nil {
		return empty, err
	}
	fresh, err := reader.DecisionFrontierSourceSnapshot(current.ID)
	if err != nil {
		return empty, err
	}
	for key, change := range patch {
		if fresh.Metadata[key] != change.Value {
			return empty, ErrTransitionChainStale
		}
	}
	if !completed && (fresh.Status != "in_progress" || fresh.Revision != sealed.ToVersion || fresh.Metadata[beadmeta.LifecycleTransitionHeadMetadataKey] != evidenceID) {
		return empty, ErrTransitionChainStale
	}
	return descendantCompletionPredecessor{receipt: sealed, digest: digest, current: fresh, replayed: found, recovered: recovered}, nil
}

func descendantAttemptPatch(current beads.Bead, storeRef string, request DescendantCompletionRequest, key, value string) (map[string]MetadataStringPatch, error) {
	capture, err := attemptevidence.DecodeCompact([]byte(value))
	if err != nil || capture.Identity.SessionGeneration != request.RuntimeEpoch {
		return nil, ErrTransitionChainEvidence
	}
	// Generated-step outcomes use gc.outcome, while source work records use
	// gc.work_outcome. Bind capture to the requested step outcome without
	// introducing a source work-record field on the descendant.
	prospective := current
	prospective.Metadata = maps.Clone(current.Metadata)
	prospective.Metadata[beadmeta.WorkOutcomeMetadataKey] = request.Outcome
	return BuildAttemptEvidencePatch(prospective, storeRef, key, value)
}

func descendantAttemptWrite(cfg DescendantCompletionConfig, evidenceID string, claim beads.RevisionTransitionPatchReceipt, claimDigest string, metadata map[string]MetadataStringPatch) (beads.RevisionTransitionPatchRequest, error) {
	patch, err := transitionPatchWithHead(SourceWorkPatch{Metadata: metadata}, TransitionStep("descendant_attempt_evidence"), claim.ReceiptID, evidenceID)
	if err != nil {
		return beads.RevisionTransitionPatchRequest{}, err
	}
	canonical, err := transitionPatch(patch)
	return beads.RevisionTransitionPatchRequest{
		ReceiptID: evidenceID, Scope: cfg.Scope, Kind: descendantAttemptEvidenceKind, Actor: cfg.Actor,
		ExpectedVersion: claim.ToVersion, PriorReceiptID: claim.ReceiptID, PriorReceiptDigest: claimDigest, Patch: canonical,
	}, err
}
