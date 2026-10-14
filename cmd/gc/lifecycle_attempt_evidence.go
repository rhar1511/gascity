package main

import (
	"context"
	"fmt"
	"maps"
	"strings"

	"github.com/gastownhall/gascity/internal/attemptevidence"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worklifecycle"
)

// lifecycleAttemptCaptureStore keeps private reads and payload storage on their
// existing transports. Only the owner's first-write index is redirected into
// the controller's protected transition; no generic enrolled write is allowed.
type lifecycleAttemptCaptureStore struct {
	beads.Store
	ownerID string
	seal    func(key, value string) error
}

func (s *lifecycleAttemptCaptureStore) PrivatePayloadValueTransportTarget() beads.Store {
	return s.Store
}

func (s *lifecycleAttemptCaptureStore) PrivateEvidenceArchiveReaderHandle() (beads.PrivateEvidenceArchiveReader, bool) {
	return beads.PrivateEvidenceArchiveReaderFor(s.Store)
}

func (s *lifecycleAttemptCaptureStore) ReadPrivateEvidenceMetadataKey(id, key string) (string, bool, error) {
	return beads.ReadPrivateEvidenceMetadataKey(s.Store, id, key)
}

func (s *lifecycleAttemptCaptureStore) CompareAndSetPrivateEvidenceMetadataKey(id, key, expected, next string) (bool, error) {
	if id != s.ownerID || expected != "" || !strings.HasPrefix(key, beadmeta.AttemptEvidenceIndexPrefix) || s.seal == nil {
		return false, worklifecycle.ErrTransitionChainEvidence
	}
	if err := s.seal(key, next); err != nil {
		return false, err
	}
	return true, nil
}

// captureLifecycleExecutionAttempt supplies snapshot/archive I/O to the domain
// completion operation. The domain owns the separately bound Q54 evidence write.
func captureLifecycleExecutionAttempt(ctx context.Context, cityPath string, cfg *config.City, cityStore beads.Store, leg classStoreCandidate, owner beads.Bead, expected session.Info, outcome string, seal func(key, value string) error) error {
	verifySession := func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		current, err := session.NewStore(beads.SessionStore{Store: cliSessionStore(cityStore, cfg, cityPath)}).Get(expected.ID)
		if err != nil || current.ID != owner.Metadata[beadmeta.SessionIDMetadataKey] || current.Generation != expected.Generation ||
			current.InstanceToken != expected.InstanceToken || session.AssigneeIdentifier(current) != owner.Assignee {
			return fmt.Errorf("execution capture has no exact persisted session incarnation: %w", worklifecycle.ErrTransitionChainEvidence)
		}
		return nil
	}
	if err := verifySession(); err != nil {
		return err
	}
	if !rootStoreRefMatchesCandidate(owner.Metadata[beadmeta.RootStoreRefMetadataKey], leg.ref) {
		return worklifecycle.ErrTransitionChainEvidence
	}
	// Generated steps can omit the root-store metadata. Their verified route
	// supplies the owner for repository resolution without mutating the row.
	owner.Metadata = maps.Clone(owner.Metadata)
	owner.Metadata[beadmeta.RootStoreRefMetadataKey] = leg.ref
	dirs := workRecordRepoDirs{cityPath: cityPath, legacy: cityPath, rigs: func() []config.Rig { return cfg.Rigs }}
	spec, err := workbenchAttemptCaptureSpec(owner, dirs, cityPath, cfg, expected.Generation)
	if err != nil {
		return err
	}
	spec.Outcome = outcome
	store := &lifecycleAttemptCaptureStore{
		Store: leg.store, ownerID: owner.ID,
		seal: func(key, value string) error {
			if err := verifySession(); err != nil {
				return err
			}
			return seal(key, value)
		},
	}
	_, err = attemptevidence.Capture(ctx, store, spec)
	return err
}

// applyLifecycleCompletionBudget seals capture in the same protected patch as
// the acceptance budget, then repairs/verifies its archive before close. A
// committed budget is replayed from its immutable capture, never a new snapshot.
func applyLifecycleCompletionBudget(ctx context.Context, cityPath string, cfg *config.City, cityStore beads.Store, leg classStoreCandidate, scope string, transition lifecycleTransitionContext, request worklifecycle.TransitionRequest) (worklifecycle.TransitionResult, error) {
	empty := worklifecycle.TransitionResult{}
	if transition.source.Status == "closed" {
		return transition.chain.Apply(request)
	}
	prospective := transition.source
	prospective.Metadata = maps.Clone(prospective.Metadata)
	if !rootStoreRefMatchesCandidate(prospective.Metadata[beadmeta.RootStoreRefMetadataKey], leg.ref) {
		return empty, worklifecycle.ErrTransitionChainEvidence
	}
	// The verified route owns this source even when its optional store marker
	// is absent. Supply capture provenance locally; do not write inferred metadata.
	prospective.Metadata[beadmeta.RootStoreRefMetadataKey] = leg.ref
	request.Patch.Metadata = maps.Clone(request.Patch.Metadata)
	for key, change := range request.Patch.Metadata {
		prospective.Metadata[key] = change.Value
	}
	var spec attemptevidence.CaptureSpec
	var indexKey string
	for key := range request.Patch.Metadata {
		if strings.HasPrefix(key, beadmeta.AttemptEvidenceIndexPrefix) {
			if indexKey != "" {
				return empty, worklifecycle.ErrTransitionChainEvidence
			}
			indexKey = key
		}
	}
	if indexKey != "" {
		// The context already proved this budget at the current source revision.
		// Require its actual private index before calling Capture, which can also
		// find archive-only evidence and otherwise fall back to a fresh snapshot.
		value, present, err := beads.ReadPrivateEvidenceMetadataKey(leg.store, prospective.ID, indexKey)
		if err != nil || !present || value != request.Patch.Metadata[indexKey].Value {
			return empty, fmt.Errorf("completion budget has no exact sealed attempt index: %w", worklifecycle.ErrTransitionChainEvidence)
		}
		sealed, err := attemptevidence.DecodeCompact([]byte(value))
		if err != nil {
			return empty, worklifecycle.ErrTransitionChainEvidence
		}
		spec = attemptevidence.CaptureSpec{Identity: sealed.Identity, StoreRef: leg.ref}
	} else {
		if transition.source.Metadata[beadmeta.LifecycleCompletionBudgetMetadataKey] != "" {
			return empty, fmt.Errorf("pending historical completion budget has no sealed attempt; preserving claim: %w", worklifecycle.ErrTransitionChainEvidence)
		}
		info, err := lifecycleCompletionCaptureSession(cityPath, cfg, cityStore, leg.store, transition.source, scope, transition.head.ReceiptID)
		if err != nil {
			return empty, err
		}
		dirs := workRecordRepoDirs{cityPath: cityPath, legacy: cityPath, rigs: func() []config.Rig { return cfg.Rigs }}
		spec, err = workbenchAttemptCaptureSpec(prospective, dirs, cityPath, cfg, info.Generation)
		if err != nil {
			return empty, err
		}
	}
	store := &lifecycleAttemptCaptureStore{Store: leg.store, ownerID: prospective.ID}
	var committed *worklifecycle.TransitionResult
	store.seal = func(key, value string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		// A runtime may have restarted while Git was being captured. Recheck
		// the claim incarnation immediately before consuming the budget.
		info, err := lifecycleCompletionCaptureSession(cityPath, cfg, cityStore, leg.store, transition.source, scope, transition.head.ReceiptID)
		if err != nil || info.Generation != spec.Identity.SessionGeneration {
			return fmt.Errorf("session changed during attempt capture: %w", worklifecycle.ErrTransitionChainEvidence)
		}
		patch, err := worklifecycle.BuildAttemptEvidencePatch(prospective, leg.ref, key, value)
		if err != nil {
			return err
		}
		maps.Copy(request.Patch.Metadata, patch)
		result, err := transition.chain.Apply(request)
		if err == nil {
			committed = &result
		}
		return err
	}
	capture, err := attemptevidence.Capture(ctx, store, spec)
	if err != nil {
		return empty, fmt.Errorf("capture protected completion attempt: %w", err)
	}
	// Capture can return an existing archive without invoking the seal callback.
	// Read the protected index/reference back instead of assuming it committed.
	key := attemptevidence.OwnerIndexKey(capture.AttemptID)
	value, present, err := beads.ReadPrivateEvidenceMetadataKey(store, prospective.ID, key)
	if err != nil || !present || value == "" {
		return empty, fmt.Errorf("captured attempt has no protected owner index: %w", worklifecycle.ErrTransitionChainEvidence)
	}
	patch, err := worklifecycle.BuildAttemptEvidencePatch(prospective, leg.ref, key, value)
	if err != nil {
		return empty, err
	}
	current, err := leg.store.Get(prospective.ID)
	if err != nil || current.Metadata[beadmeta.AttemptEvidenceReferenceMetadataKey] != patch[beadmeta.AttemptEvidenceReferenceMetadataKey].Value ||
		current.Metadata[beadmeta.LifecycleCompletionBudgetMetadataKey] != request.Patch.Metadata[beadmeta.LifecycleCompletionBudgetMetadataKey].Value {
		return empty, fmt.Errorf("capture has no matching protected reference and budget: %w", worklifecycle.ErrTransitionChainEvidence)
	}
	maps.Copy(request.Patch.Metadata, patch)
	if err := ctx.Err(); err != nil {
		return empty, err
	}
	verified, err := transition.chain.Apply(request)
	if err == nil && committed != nil {
		// The final readback is a replay used to verify capture, not a second
		// caller request. Preserve the actual first write's result.
		return *committed, nil
	}
	return verified, err
}

// lifecycleCompletionCaptureSession binds a fresh snapshot to the incarnation
// that owns the immutable claim, rather than a replacement runtime sharing its
// session ID. The transition context has already verified the current chain.
func lifecycleCompletionCaptureSession(cityPath string, cfg *config.City, cityStore, workStore beads.Store, source beads.Bead, scope, headID string) (session.Info, error) {
	info, err := session.NewStore(beads.SessionStore{Store: cliSessionStore(cityStore, cfg, cityPath)}).Get(source.Metadata[beadmeta.SessionIDMetadataKey])
	if err != nil {
		return session.Info{}, fmt.Errorf("read claimed session before attempt capture: %w", err)
	}
	op, err := worklifecycle.ClaimIdentityOperationID(info.ID, info.InstanceToken, info.Generation, source.Metadata[beadmeta.ClaimGenerationMetadataKey])
	if err != nil || source.Assignee != session.AssigneeIdentifier(info) {
		return session.Info{}, fmt.Errorf("attempt capture has no exact session incarnation: %w", worklifecycle.ErrTransitionChainEvidence)
	}
	claimID, err := worklifecycle.TransitionReceiptID(source.ID, scope, worklifecycle.TransitionStepClaimIdentity, op)
	if err != nil {
		return session.Info{}, err
	}
	reader, ok := beads.RevisionTransitionPatchReceiptReaderFor(workStore)
	if !ok || reader == nil {
		return session.Info{}, worklifecycle.ErrTransitionChainUnavailable
	}
	seen := make(map[string]bool)
	for headID != "" && !seen[headID] {
		seen[headID] = true
		receipt, found, err := reader.ReadRevisionTransitionPatchReceipt(headID)
		if err != nil || !found || receipt.ReceiptID != headID || receipt.IssueID != source.ID || receipt.Scope != scope {
			break
		}
		if headID == claimID && receipt.Kind == "lifecycle_source_claim_identity_v1" && receipt.Actor == source.Assignee {
			return info, nil
		}
		headID = receipt.PriorReceiptID
	}
	return session.Info{}, fmt.Errorf("attempt capture session differs from the immutable claim: %w", worklifecycle.ErrTransitionChainEvidence)
}
