package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/storeref"
	"github.com/gastownhall/gascity/internal/worklifecycle"
)

// VerifyLifecycleRecoveryAdmission proves the current Q43 attachment, signed
// route policy, and attached workflow for the exact store selected by the API.
// The API may persist a recovery intent only after this controller-owned proof
// succeeds.
func (cs *controllerState) VerifyLifecycleRecoveryAdmission(ctx context.Context, request api.LifecycleRecoveryAdmissionRequest) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if request.Work.ID == "" || request.WorkStore == nil || request.ExpectedRevision <= 0 ||
		request.Work.Revision != request.ExpectedRevision {
		return worklifecycle.ErrTransitionChainInvalid
	}

	cs.mu.RLock()
	cfg := cs.cfg
	cityName := cs.cityName
	cityPath := cs.cityPath
	cityStore := cs.cityBeadStore
	rigStores := make(map[string]beads.Store, len(cs.beadStores))
	for name, store := range cs.beadStores {
		rigStores[name] = store
	}
	permitResolver := cs.beadsPermitResolver
	authority := cs.compatibilityAuthority
	graphGeneration := cs.graphStoreGeneration
	routesClosed := cs.compatibilityRoutesClosed
	cs.mu.RUnlock()
	if cfg == nil || !cfg.Lifecycle.AdmissionEnabled || strings.TrimSpace(cityName) == "" ||
		strings.TrimSpace(cityPath) == "" || permitResolver == nil || routesClosed {
		return worklifecycle.ErrTransitionChainUnavailable
	}

	sourceStoreRef, err := lifecycleClaimSourceStoreRef(cityName, request.WorkStoreRef)
	if err != nil {
		return err
	}
	if sourceStoreRef != strings.TrimSpace(sourceStoreRef) || request.Scope != lifecycleScopeForRef(cityName, cfg, sourceStoreRef) {
		return worklifecycle.ErrTransitionChainEvidence
	}
	store := request.WorkStore
	if request.WorkStoreRef == storeref.WorkRef {
		if registered := registeredCityWorkStore(cityPath); registered != nil {
			store = registered
		} else {
			store = cityStore
		}
	}
	if store == nil || store != request.WorkStore {
		return fmt.Errorf("authoritative recovery store changed: %w", worklifecycle.ErrTransitionChainStale)
	}

	suspendedRigPaths := buildSuspendedRigPathsForCity(cfg, cityPath)
	legs, err := routedWorkStoreCandidates(cityPath, cfg, cityStore, rigStores, suspendedRigPaths)
	if err != nil {
		return fmt.Errorf("resolve routed lifecycle recovery stores: %w", err)
	}
	var selected classStoreCandidate
	for _, leg := range legs {
		if leg.ref == sourceStoreRef && leg.store == request.WorkStore {
			selected = leg
			break
		}
	}
	if selected.store == nil {
		return fmt.Errorf("recovery store is not the exact routed work store: %w", worklifecycle.ErrTransitionChainStale)
	}
	releaseRouteLease, err := cs.acquireLifecycleClaimRouteLease(cfg, graphGeneration)
	if err != nil {
		return err
	}
	defer releaseRouteLease()

	current, err := selected.store.Get(request.Work.ID)
	if err != nil || current.ID != request.Work.ID || current.Revision != request.ExpectedRevision ||
		!rootStoreRefMatchesCandidate(current.Metadata[beadmeta.RootStoreRefMetadataKey], selected.ref) ||
		current.Metadata[beadmeta.RootStoreRefMetadataKey] != request.Work.Metadata[beadmeta.RootStoreRefMetadataKey] {
		return fmt.Errorf("recovery source changed before admission proof: %w", worklifecycle.ErrTransitionChainStale)
	}
	transition, err := buildLifecycleTransitionContext(
		cityName, cityPath, cfg, cityStore, rigStores, legs, selected, request.Work.ID, request.Scope, permitResolver, authority,
	)
	if err != nil {
		return fmt.Errorf("verify exact Q43 attachment and current Q54 head: %w", err)
	}
	if transition.source.Revision != request.ExpectedRevision || transition.head.ToVersion != request.ExpectedRevision {
		return worklifecycle.ErrTransitionChainStale
	}
	if err := worklifecycle.ValidateRecoveryWorkEnvelopeEvidence(transition.source, cfg.Lifecycle, request.Scope); err != nil {
		return fmt.Errorf("verify attached recovery source envelope: %w", err)
	}
	storesByRef := make(map[string]beads.Store, len(legs))
	for _, leg := range legs {
		if leg.store != nil {
			storesByRef[leg.ref] = leg.store
		}
	}
	if !lifecycleRecoveryAttachedWorkflowMatches(transition.source, selected.ref, selected.store, storesByRef, cfg, request.Scope) {
		return fmt.Errorf("current attached workflow evidence is unavailable: %w", worklifecycle.ErrTransitionChainEvidence)
	}
	return ctx.Err()
}

var _ api.LifecycleRecoveryAdmissionVerifier = (*controllerState)(nil)
