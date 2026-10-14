package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/worklifecycle"
)

// CompleteLifecycleWork consumes separately signed acceptance at the caller's
// exact observed head. Only the controller owns the protected transition permit.
// A committed budget makes an interrupted close recoverable by the same request.
func (cs *controllerState) CompleteLifecycleWork(ctx context.Context, request api.LifecycleCompletionTransitionRequest) (api.LifecycleCompletionResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	empty := api.LifecycleCompletionResult{}
	if err := ctx.Err(); err != nil {
		return empty, err
	}
	body := request.Submission
	if request.WorkStore == nil || body.WorkID == "" || body.WorkID != request.Work.ID || strings.TrimSpace(body.WorkID) != body.WorkID ||
		body.ExpectedRevision == 0 || body.ExpectedTransitionHead == "" || strings.TrimSpace(body.ExpectedTransitionHead) != body.ExpectedTransitionHead ||
		body.CompletionReceipt == "" || len(body.CompletionReceipt) > 65536 {
		return empty, worklifecycle.ErrTransitionChainInvalid
	}
	cs.mu.RLock()
	cfg, cityName, cityPath := cs.cfg, cs.cityName, cs.cityPath
	cityStore := cs.cityBeadStore
	rigStores := make(map[string]beads.Store, len(cs.beadStores))
	for name, store := range cs.beadStores {
		rigStores[name] = store
	}
	resolver, authority := cs.beadsPermitResolver, cs.compatibilityAuthority
	generation, routesClosed := cs.graphStoreGeneration, cs.compatibilityRoutesClosed
	cs.mu.RUnlock()
	if cfg == nil || !cfg.Lifecycle.AdmissionEnabled || cityName == "" || cityPath == "" || resolver == nil || routesClosed {
		return empty, worklifecycle.ErrTransitionChainUnavailable
	}
	sourceRef, err := lifecycleClaimSourceStoreRef(cityName, request.WorkStoreRef)
	if err != nil || body.SourceStoreRef != sourceRef {
		return empty, errors.Join(worklifecycle.ErrTransitionChainStale, err)
	}
	scope := worklifecycle.ScopeForStore(cityName, sourceRef)
	if request.Scope != scope {
		return empty, worklifecycle.ErrTransitionChainEvidence
	}
	release, err := cs.acquireLifecycleClaimRouteLease(cfg, generation)
	if err != nil {
		return empty, err
	}
	defer release()
	legs, err := routedWorkStoreCandidates(cityPath, cfg, cityStore, rigStores, buildSuspendedRigPathsForCity(cfg, cityPath))
	if err != nil {
		return empty, err
	}
	var selected classStoreCandidate
	for _, leg := range legs {
		if leg.ref == sourceRef && leg.store == request.WorkStore {
			selected = leg
			break
		}
	}
	if selected.store == nil || !rootStoreRefMatchesCandidate(request.Work.Metadata[beadmeta.RootStoreRefMetadataKey], sourceRef) {
		return empty, worklifecycle.ErrTransitionChainStale
	}
	transition, err := buildLifecycleTransitionContext(cityName, cityPath, cfg, cityStore, rigStores, legs, selected, body.WorkID, scope, resolver, authority)
	if err != nil {
		return empty, err
	}
	current := transition.source
	budgetValue, err := worklifecycle.BuildCompletionBudgetMetadata(body.WorkID, scope, transition.admissionDigest, body.CompletionReceipt)
	if err != nil {
		return empty, err
	}
	existingBudget := current.Metadata[beadmeta.LifecycleCompletionBudgetMetadataKey] != ""
	budgetMetadata := map[string]worklifecycle.MetadataStringPatch{
		beadmeta.LifecycleCompletionBudgetMetadataKey:  {Value: budgetValue},
		beadmeta.LifecycleCompletionReceiptMetadataKey: {Value: body.CompletionReceipt},
	}
	switch {
	case existingBudget:
		// CurrentHead has already verified every immutable receipt. Bind this retry
		// to the original observation, including when the close has since committed.
		receipt, found, readErr := lifecycleHeadReceipt(selected.store, transition.head)
		if readErr != nil || !found {
			return empty, errors.Join(worklifecycle.ErrTransitionChainReceipt, readErr)
		}
		if transition.head.Step == worklifecycle.TransitionStepClose {
			reader, ok := beads.RevisionTransitionPatchReceiptReaderFor(selected.store)
			if !ok || reader == nil {
				return empty, worklifecycle.ErrTransitionChainUnavailable
			}
			receipt, found, readErr = reader.ReadRevisionTransitionPatchReceipt(receipt.PriorReceiptID)
			if readErr != nil || !found {
				return empty, errors.Join(worklifecycle.ErrTransitionChainReceipt, readErr)
			}
		}
		attached, hasAttachment := lifecyclePatchMetadataString(receipt, beadmeta.LifecycleCompletionReceiptMetadataKey)
		budget, hasBudget := lifecyclePatchMetadataString(receipt, beadmeta.LifecycleCompletionBudgetMetadataKey)
		if receipt.Kind != "lifecycle_source_completion_budget_v1" || receipt.IssueID != body.WorkID || receipt.Scope != scope ||
			receipt.ExpectedVersion != body.ExpectedRevision || receipt.PriorReceiptID != body.ExpectedTransitionHead ||
			!hasAttachment || attached != body.CompletionReceipt || !hasBudget || budget != budgetValue ||
			current.Metadata[beadmeta.LifecycleCompletionReceiptMetadataKey] != body.CompletionReceipt ||
			current.Metadata[beadmeta.LifecycleCompletionBudgetMetadataKey] != budgetValue {
			return empty, worklifecycle.ErrTransitionChainEvidence
		}
		budgetMetadata, err = lifecycleCompletionBudgetMetadata(receipt)
		if err != nil {
			return empty, err
		}
	case current.Status != "in_progress" || !lifecycleCompletionMutable(current) || current.Revision != body.ExpectedRevision ||
		transition.head.ReceiptID != body.ExpectedTransitionHead || current.Metadata[beadmeta.LifecycleCompletionReceiptMetadataKey] != "":
		return empty, worklifecycle.ErrTransitionChainStale
	default:
		var receipt worklifecycle.CompletionReceipt
		if err := json.Unmarshal([]byte(body.CompletionReceipt), &receipt); err != nil {
			return empty, errors.Join(worklifecycle.ErrTransitionChainEvidence, err)
		}
		// The chain verifies the canonical signature and every projected value
		// before committing this single protected budget transition.
		for key, value := range receipt.WorkRecord.Metadata() {
			change := worklifecycle.MetadataStringPatch{Value: value}
			if previous, present := current.Metadata[key]; present {
				change.Expected = &previous
			}
			budgetMetadata[key] = change
		}
	}
	// Holds and deferrals still prevent a pending completion from progressing.
	// A terminal exact replay is read-only and does not reopen or reassign work.
	if current.Status != "closed" && (current.Status != "in_progress" || !lifecycleCompletionMutable(current)) {
		return empty, worklifecycle.ErrTransitionChainStale
	}
	if current.Status != "closed" {
		if err := validateLifecycleCompletionWorkRecord(ctx, current, budgetMetadata, cityPath, cfg); err != nil {
			return empty, err
		}
	}
	if err := ctx.Err(); err != nil {
		return empty, err
	}
	budget, err := applyLifecycleCompletionBudget(ctx, cityPath, cfg, cityStore, selected, scope, transition, worklifecycle.TransitionRequest{
		IssueID: body.WorkID, Step: worklifecycle.TransitionStepCompletionBudget,
		OperationID:    lifecycleTransitionOperationID("completion-budget", body.WorkID, scope, transition.admissionDigest, body.CompletionReceipt),
		PriorReceiptID: body.ExpectedTransitionHead, Evidence: transition.evidence,
		Patch: worklifecycle.SourceWorkPatch{Metadata: budgetMetadata},
	})
	if err != nil {
		return empty, err
	}
	if err := ctx.Err(); err != nil {
		return empty, err
	}
	if err := reconcileLifecycleCompletionForLeg(ctx, cityName, cityPath, cfg, cityStore, rigStores, legs, selected, body.WorkID, scope, resolver, authority); err != nil {
		return empty, err
	}
	closed, err := beads.HandlesFor(selected.store).Live.Get(body.WorkID)
	if err != nil {
		return empty, err
	}
	head, err := transition.chain.CurrentHead(body.WorkID, transition.evidence)
	if err != nil || closed.Status != "closed" || closed.Revision != head.ToVersion || head.Step != worklifecycle.TransitionStepClose ||
		closed.Metadata[beadmeta.LifecycleCompletionReceiptMetadataKey] != body.CompletionReceipt {
		return empty, fmt.Errorf("completion has no verified terminal source head: %w", errors.Join(worklifecycle.ErrTransitionChainReceipt, err))
	}
	return api.LifecycleCompletionResult{
		WorkID: body.WorkID, ReceiptID: head.ReceiptID, Revision: head.ToVersion,
		Replayed: existingBudget || budget.Replayed || budget.Recovered,
	}, nil
}
