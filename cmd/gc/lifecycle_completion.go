package main

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/worklifecycle"
)

// reconcileLifecycleCompletions aligns work records with signed acceptance.
// The acceptance authority verifies the referenced artifact and validation;
// the controller verifies the signature, contract, scope, and current row.
// Reconciliation consumes the same durable intervention budget as recovery.
func reconcileLifecycleCompletions(cityName, cityPath string, cfg *config.City, store beads.Store, rigStores map[string]beads.Store, suspendedRigPaths map[string]bool, stderr io.Writer) {
	if cfg == nil || !cfg.Lifecycle.AdmissionEnabled || !cfg.Lifecycle.RecoveryEnabled || strings.TrimSpace(cfg.Lifecycle.EscalationTarget) == "" {
		return
	}
	legs, err := routedWorkStoreCandidates(cityPath, cfg, store, rigStores, suspendedRigPaths, censusRefScoped)
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
				if err := reconcileLifecycleCompletion(leg.store, row.ID, scope, cfg.Lifecycle); err != nil {
					fmt.Fprintf(stderr, "lifecycle completion: %s in %s: %v\n", row.ID, leg.ref, err) //nolint:errcheck
				}
			}
		}
	}
}

func reconcileLifecycleCompletion(store beads.Store, id, scope string, cfg config.LifecycleConfig) error {
	current, err := beads.HandlesFor(store).Live.Get(id)
	if err != nil {
		return fmt.Errorf("read current work: %w", err)
	}
	if !lifecycleCompletionMutable(current) {
		return nil
	}
	if decision := worklifecycle.EvaluateCompletion(current, cfg, scope); !decision.Accepted {
		return fmt.Errorf("acceptance remains unverified: %s", decision.Reason)
	}
	writer, supported := beads.ConditionalWriterFor(store)
	if !supported || current.Revision == 0 || !beads.InspectConditionalWrites(store).Capable {
		return fmt.Errorf("conditional completion unavailable; work remains held")
	}
	_, reserved, err := worklifecycle.ReserveRecoveryAttempt(store, id, scope)
	if err != nil {
		return fmt.Errorf("reserve completion intervention: %w", err)
	}
	if !reserved {
		_, _, err := worklifecycle.RequestRecoveryEscalation(store, id, scope, cfg.EscalationTarget)
		if err != nil {
			return fmt.Errorf("record exhausted completion escalation: %w", err)
		}
		return fmt.Errorf("intervention budget exhausted; durable escalation awaits delivery to %s", cfg.EscalationTarget)
	}
	// Reserving changes the revision. Re-read all authority and hold evidence
	// before taking a new revision-bound transition; never close an old snapshot.
	current, err = beads.HandlesFor(store).Live.Get(id)
	if err != nil {
		return fmt.Errorf("verify reserved work: %w", err)
	}
	if !lifecycleCompletionMutable(current) {
		return nil
	}
	if decision := worklifecycle.EvaluateCompletion(current, cfg, scope); !decision.Accepted {
		return fmt.Errorf("acceptance changed after reservation: %s", decision.Reason)
	}
	if err := writer.CloseIfMatch(id, current.Revision); err != nil {
		return fmt.Errorf("conditional completion attempt: %w", err)
	}
	verified, err := beads.HandlesFor(store).Live.Get(id)
	if err != nil {
		return fmt.Errorf("completion outcome remains unknown: %w", err)
	}
	if verified.Status != "closed" || !worklifecycle.EvaluateCompletion(verified, cfg, scope).Accepted {
		return fmt.Errorf("completion effect was not verified")
	}
	return nil
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
