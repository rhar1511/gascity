package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/qualification"
	"github.com/gastownhall/gascity/internal/worklifecycle"
)

// reconcileLifecycleCompletions is retained for callers without controller
// permit authority. Those callers can observe completion evidence but cannot
// mutate an enrolled source.
//
//nolint:unparam // Direct callers use this fail-closed compatibility seam; the controller uses the resolver-aware entry point.
func reconcileLifecycleCompletions(cityName, cityPath string, cfg *config.City, store beads.Store, rigStores map[string]beads.Store, suspendedRigPaths map[string]bool, stderr io.Writer) {
	reconcileLifecycleCompletionsWithPermitResolver(cityName, cityPath, cfg, store, rigStores, suspendedRigPaths, stderr, nil, nil)
}

// reconcileLifecycleCompletionsWithPermitResolver applies Q54 completion
// transitions only from the controller executor that owns the host permit
// resolver. Recovery enablement is independent from signed acceptance.
func reconcileLifecycleCompletionsWithPermitResolver(
	cityName, cityPath string,
	cfg *config.City,
	store beads.Store,
	rigStores map[string]beads.Store,
	suspendedRigPaths map[string]bool,
	stderr io.Writer,
	permitResolver *hostBeadsPermitResolver,
	authority qualification.CompatibilityAuthority,
) {
	if cfg == nil || !cfg.Lifecycle.AdmissionEnabled {
		return
	}
	if stderr == nil {
		stderr = io.Discard
	}
	legs, err := routedWorkStoreCandidates(cityPath, cfg, store, rigStores, suspendedRigPaths)
	if err != nil {
		fmt.Fprintf(stderr, "lifecycle completion: resolving stores: %v\n", err) //nolint:errcheck
		return
	}
	seen := make(map[storeScopedBeadKey]bool)
	for _, leg := range legs {
		if leg.store == nil {
			continue
		}
		for _, status := range []string{"open", "in_progress"} {
			rows, err := beads.HandlesFor(leg.store).Live.List(beads.ListQuery{Status: status, Live: true})
			if err != nil {
				fmt.Fprintf(stderr, "lifecycle completion: listing %s in %s: %v\n", status, leg.ref, err) //nolint:errcheck
				continue
			}
			for _, row := range rows {
				key := storeScopedBeadKey{StoreRef: leg.ref, ID: row.ID}
				if seen[key] || !rootStoreRefMatchesCandidate(row.Metadata[beadmeta.RootStoreRefMetadataKey], leg.ref) {
					continue
				}
				seen[key] = true
				if strings.TrimSpace(row.Metadata[beadmeta.LifecycleCompletionReceiptMetadataKey]) == "" {
					continue
				}
				scope := lifecycleScopeForRef(cityName, cfg, leg.ref)
				if err := reconcileLifecycleCompletionForLeg(cityName, cityPath, cfg, store, rigStores, legs, leg, row.ID, scope, permitResolver, authority); err != nil {
					fmt.Fprintf(stderr, "lifecycle completion: %s in %s: %v\n", row.ID, leg.ref, err) //nolint:errcheck
				}
			}
		}
	}
}

// reconcileLifecycleCompletion preserves the read-only compatibility seam for
// direct callers. A verified receipt still needs the controller-owned source
// transition context before any close can be authorized.
func reconcileLifecycleCompletion(store beads.Store, id, scope string, cfg config.LifecycleConfig) error {
	current, err := beads.HandlesFor(store).Live.Get(id)
	if err != nil {
		return fmt.Errorf("read current work: %w", err)
	}
	if !lifecycleCompletionMutable(current) {
		return nil
	}
	if current.Status == "open" {
		return nil
	}
	if decision := worklifecycle.EvaluateCompletion(current, cfg, scope); !decision.Accepted {
		return fmt.Errorf("acceptance remains unverified: %s", decision.Reason)
	}
	return fmt.Errorf("controller transition authority is unavailable; accepted work remains in progress: %w", worklifecycle.ErrTransitionChainUnavailable)
}

func reconcileLifecycleCompletionForLeg(
	cityName, cityPath string,
	cfg *config.City,
	cityStore beads.Store,
	rigStores map[string]beads.Store,
	legs []classStoreCandidate,
	leg classStoreCandidate,
	id, scope string,
	permitResolver *hostBeadsPermitResolver,
	authority qualification.CompatibilityAuthority,
) error {
	if leg.store == nil || cfg == nil {
		return worklifecycle.ErrTransitionChainUnavailable
	}
	current, err := beads.HandlesFor(leg.store).Live.Get(id)
	if err != nil {
		return fmt.Errorf("read current work: %w", err)
	}
	if !lifecycleCompletionMutable(current) || current.Status == "open" {
		// Accepted work remains open until it has a controller-owned claim receipt.
		return nil
	}
	if current.Status != "in_progress" {
		return nil
	}
	transition, err := buildLifecycleTransitionContext(
		cityName, cityPath, cfg, cityStore, rigStores, legs, leg, id, scope, permitResolver, authority,
	)
	if err != nil {
		return fmt.Errorf("current admitted source evidence is unavailable: %w", err)
	}
	decision, err := transition.chain.VerifyCompletionReceipt(id, transition.evidence)
	if err != nil {
		return fmt.Errorf("verify signed completion receipt against current Q54 evidence: %w", err)
	}
	if !decision.Accepted {
		return fmt.Errorf("acceptance remains unverified: %s", decision.Reason)
	}
	completionReceipt := current.Metadata[beadmeta.LifecycleCompletionReceiptMetadataKey]
	if transition.source.Status != "in_progress" || transition.source.Metadata[beadmeta.LifecycleCompletionReceiptMetadataKey] != completionReceipt {
		return fmt.Errorf("completion source changed while rebuilding current evidence: %w", worklifecycle.ErrTransitionChainStale)
	}
	budgetValue, err := worklifecycle.BuildCompletionBudgetMetadata(id, scope, transition.admissionDigest, completionReceipt)
	if err != nil {
		return fmt.Errorf("build receipt-bound completion budget: %w", err)
	}
	budgetOperationID := lifecycleTransitionOperationID("completion-budget", id, scope, transition.admissionDigest, completionReceipt)
	budgetParentID := transition.head.ReceiptID
	if transition.source.Metadata[beadmeta.LifecycleCompletionBudgetMetadataKey] != "" {
		budgetReceipt, found, readErr := lifecycleHeadReceipt(leg.store, transition.head)
		if readErr != nil || !found {
			return fmt.Errorf("read consumed completion-budget receipt: %w", errors.Join(readErr, worklifecycle.ErrTransitionChainReceipt))
		}
		storedBudget, hasBudget := lifecyclePatchMetadataString(budgetReceipt, beadmeta.LifecycleCompletionBudgetMetadataKey)
		if budgetReceipt.Kind != "lifecycle_source_completion_budget_v1" || !hasBudget || storedBudget != budgetValue || budgetReceipt.PriorReceiptID == "" {
			return fmt.Errorf("current completion budget does not bind this exact receipt and head: %w", worklifecycle.ErrTransitionChainStale)
		}
		budgetParentID = budgetReceipt.PriorReceiptID
	}
	budgetResult, err := transition.chain.Apply(worklifecycle.TransitionRequest{
		IssueID: id, Step: worklifecycle.TransitionStepCompletionBudget,
		OperationID: budgetOperationID, PriorReceiptID: budgetParentID, Evidence: transition.evidence,
		Patch: worklifecycle.SourceWorkPatch{Metadata: map[string]worklifecycle.MetadataStringPatch{
			beadmeta.LifecycleCompletionBudgetMetadataKey: {Value: budgetValue},
		}},
	})
	if err != nil {
		return fmt.Errorf("apply receipt-bound completion budget: %w", err)
	}
	budgetHead, err := transition.chain.CurrentHead(id, transition.evidence)
	current, readErr := beads.HandlesFor(leg.store).Live.Get(id)
	if err != nil || readErr != nil || budgetResult.Receipt.ReceiptID == "" ||
		budgetHead.ReceiptID != budgetResult.Receipt.ReceiptID || budgetHead.ToVersion != current.Revision ||
		current.Status != "in_progress" || current.Metadata[beadmeta.LifecycleCompletionReceiptMetadataKey] != completionReceipt {
		return fmt.Errorf("completion budget has no exact current source head: %w", errors.Join(err, readErr, worklifecycle.ErrTransitionChainStale))
	}
	closeOperationID := lifecycleTransitionOperationID("close", id, scope, transition.admissionDigest, completionReceipt)
	closeResult, err := transition.chain.Apply(worklifecycle.TransitionRequest{
		IssueID: id, Step: worklifecycle.TransitionStepClose,
		OperationID: closeOperationID, PriorReceiptID: budgetResult.Receipt.ReceiptID, Evidence: transition.evidence,
		CompletionReceipt: completionReceipt,
		Patch: worklifecycle.SourceWorkPatch{
			Status: &worklifecycle.StringTransition{Expected: "in_progress", Value: "closed"},
		},
	})
	if err != nil {
		return fmt.Errorf("apply signed completion close: %w", err)
	}
	verified, err := beads.HandlesFor(leg.store).Live.Get(id)
	if err != nil {
		return fmt.Errorf("read completion close outcome: %w", err)
	}
	closedDecision, decisionErr := transition.chain.VerifyCompletionReceipt(id, transition.evidence)
	closeHead, headErr := transition.chain.CurrentHead(id, transition.evidence)
	if verified.ID != id || verified.Status != "closed" || decisionErr != nil || !closedDecision.Accepted ||
		verified.Metadata[beadmeta.LifecycleCompletionReceiptMetadataKey] != completionReceipt ||
		closeResult.Receipt.ReceiptID == "" || closeHead.ReceiptID != closeResult.Receipt.ReceiptID || closeHead.ToVersion != verified.Revision {
		return fmt.Errorf("signed completion close was not verified at its exact Q54 head: %w", errors.Join(decisionErr, headErr, worklifecycle.ErrTransitionChainReceipt))
	}
	return nil
}

func lifecycleHeadReceipt(store beads.Store, head worklifecycle.TransitionHead) (beads.RevisionTransitionPatchReceipt, bool, error) {
	reader, ok := beads.RevisionTransitionPatchReceiptReaderFor(store)
	if !ok || reader == nil || head.FromAttachment || head.ReceiptID == "" {
		return beads.RevisionTransitionPatchReceipt{}, false, worklifecycle.ErrTransitionChainUnavailable
	}
	receipt, found, err := reader.ReadRevisionTransitionPatchReceipt(head.ReceiptID)
	if err != nil || !found || receipt.ReceiptID != head.ReceiptID || receipt.ToVersion != head.ToVersion {
		return receipt, found, errors.Join(err, worklifecycle.ErrTransitionChainReceipt)
	}
	return receipt, true, nil
}

func lifecyclePatchMetadataString(receipt beads.RevisionTransitionPatchReceipt, key string) (string, bool) {
	for _, change := range receipt.Patch.Metadata {
		if change.Key != key || change.Value == nil {
			continue
		}
		var value string
		if err := json.Unmarshal(*change.Value, &value); err != nil {
			return "", false
		}
		return value, true
	}
	return "", false
}

func lifecycleCompletionMutable(row beads.Bead) bool {
	if row.Status != "open" && row.Status != "in_progress" {
		return false
	}
	if beads.IsDeferred(row, time.Now()) {
		return false
	}
	for _, label := range row.Labels {
		for _, hold := range beadmeta.DispatchHoldLabels {
			if strings.EqualFold(strings.TrimSpace(label), hold) {
				return false
			}
		}
	}
	return true
}
