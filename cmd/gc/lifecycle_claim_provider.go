package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/sling"
	"github.com/gastownhall/gascity/internal/storeref"
	"github.com/gastownhall/gascity/internal/worklifecycle"
)

// ClaimLifecycleWork is the controller-owned Q54 provider for the API claim
// endpoint. The request carries only a session incarnation and observed source
// head; all policy, workflow evidence, protected write authority, and actor
// identity are resolved from controller state here.
func (cs *controllerState) ClaimLifecycleWork(ctx context.Context, request api.LifecycleClaimTransitionRequest) (api.LifecycleClaimTransitionResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return api.LifecycleClaimTransitionResult{}, err
	}
	if strings.TrimSpace(request.Work.ID) == "" || request.WorkStore == nil || strings.TrimSpace(request.Session.ID) != request.Session.ID ||
		request.Session.ID == "" || request.Actor == "" || request.ExpectedRevision <= 0 ||
		strings.TrimSpace(request.ExpectedTransitionHead) != request.ExpectedTransitionHead || request.ExpectedTransitionHead == "" {
		return api.LifecycleClaimTransitionResult{}, worklifecycle.ErrTransitionChainInvalid
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
		return api.LifecycleClaimTransitionResult{}, worklifecycle.ErrTransitionChainUnavailable
	}
	if session.AssigneeIdentifier(request.Session) != request.Actor ||
		!canonicalLifecycleClaimProviderEpoch(request.Session.Generation) || request.Session.InstanceToken == "" {
		return api.LifecycleClaimTransitionResult{}, worklifecycle.ErrTransitionChainInvalid
	}

	// The API by-id axis spells the residual city work store "work". Lifecycle
	// scopes and host permits use the controller's canonical scoped ref instead.
	sourceStoreRef, err := lifecycleClaimSourceStoreRef(cityName, request.WorkStoreRef)
	if err != nil {
		return api.LifecycleClaimTransitionResult{}, err
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
		return api.LifecycleClaimTransitionResult{}, fmt.Errorf("authoritative lifecycle store changed: %w", worklifecycle.ErrTransitionChainStale)
	}

	suspendedRigPaths := buildSuspendedRigPathsForCity(cfg, cityPath)
	legs, err := routedWorkStoreCandidates(cityPath, cfg, cityStore, rigStores, suspendedRigPaths)
	if err != nil {
		return api.LifecycleClaimTransitionResult{}, fmt.Errorf("resolve routed lifecycle work stores: %w", err)
	}
	var selected classStoreCandidate
	selectedFound := false
	for _, leg := range legs {
		if leg.ref == sourceStoreRef && leg.store == request.WorkStore {
			selected = leg
			selectedFound = true
			break
		}
	}
	if !selectedFound {
		return api.LifecycleClaimTransitionResult{}, fmt.Errorf("claim store is not the exact routed lifecycle store: %w", worklifecycle.ErrTransitionChainStale)
	}
	releaseRouteLease, err := cs.acquireLifecycleClaimRouteLease(cfg, graphGeneration)
	if err != nil {
		return api.LifecycleClaimTransitionResult{}, err
	}
	defer releaseRouteLease()
	if !rootStoreRefMatchesCandidate(request.Work.Metadata[beadmeta.RootStoreRefMetadataKey], selected.ref) {
		return api.LifecycleClaimTransitionResult{}, fmt.Errorf("claim source store provenance differs from its selected route: %w", worklifecycle.ErrTransitionChainEvidence)
	}

	scope := lifecycleScopeForRef(cityName, cfg, selected.ref)
	permitIssuer, permitPolicy, authorized := permitResolver.resolve(cityName, selected.ref)
	if !authorized || permitIssuer == nil || strings.TrimSpace(permitPolicy.Actor) == "" {
		return api.LifecycleClaimTransitionResult{}, worklifecycle.ErrTransitionChainPermit
	}
	attachmentAdapter, err := worklifecycle.NewAdmissionAttachmentAdapter(selected.store)
	if err != nil {
		return api.LifecycleClaimTransitionResult{}, err
	}
	current, attachmentProof, err := attachmentAdapter.VerifyForTransition(request.Work.ID, cfg.Lifecycle, scope)
	if err != nil {
		return api.LifecycleClaimTransitionResult{}, fmt.Errorf("verify exact Q43 lifecycle attachment: %w", errorsJoinTransitionEvidence(err))
	}
	if current.ID != request.Work.ID || current.Metadata[beadmeta.RootStoreRefMetadataKey] != request.Work.Metadata[beadmeta.RootStoreRefMetadataKey] ||
		current.Metadata[beadmeta.LifecycleAdmissionReceiptMetadataKey] != "" || current.Metadata[beadmeta.LifecycleAdmissionReceiptV2MetadataKey] == "" {
		return api.LifecycleClaimTransitionResult{}, worklifecycle.ErrTransitionChainEvidence
	}
	_, err = worklifecycle.VerifyAdmissionReceiptV2(current, cfg.Lifecycle, scope)
	if err != nil {
		return api.LifecycleClaimTransitionResult{}, fmt.Errorf("verify current v2 lifecycle admission: %w", errorsJoinTransitionEvidence(err))
	}

	runner := sling.SlingRunner(shellSlingRunner)
	var resolvedPolicy lifecycleAdmissionPolicy
	var resolvedPolicyRevision int64
	policyResolver := worklifecycle.CurrentAdmissionPolicyResolverFunc(func(source beads.Bead, receipt worklifecycle.AdmissionReceiptV2) (worklifecycle.AdmissionPolicyProjectionV2, error) {
		if source.ID != request.Work.ID || source.Metadata[beadmeta.RootStoreRefMetadataKey] != request.Work.Metadata[beadmeta.RootStoreRefMetadataKey] {
			return worklifecycle.AdmissionPolicyProjectionV2{}, fmt.Errorf("claim policy source identity changed")
		}
		policy, resolveErr := buildLifecycleAdmissionPolicy(source, receipt, scope, cityName, cityPath, cfg, cityStore, rigStores, selected, legs, runner, authority)
		if resolveErr != nil {
			return worklifecycle.AdmissionPolicyProjectionV2{}, resolveErr
		}
		resolvedPolicy = policy
		resolvedPolicyRevision = source.Revision
		return policy.projection, nil
	})
	workflowVerifier := worklifecycle.AttachedWorkflowEvidenceVerifierFunc(func(source beads.Bead, receipt worklifecycle.AdmissionReceiptV2, policy worklifecycle.AdmissionPolicyProjectionV2, evidence worklifecycle.AttachedWorkflowEvidence) error {
		if resolvedPolicyRevision != source.Revision || resolvedPolicy.deps.GraphStore == nil {
			return fmt.Errorf("current admission policy did not resolve the exact attached workflow store")
		}
		return verifyLifecycleAttachedWorkflow(selected.store, resolvedPolicy.deps.GraphStore, source, receipt, policy, evidence)
	})
	// The protected receipt actor is the authenticated session actor for this
	// step. The host-issued permit remains the authority for the exact source
	// scope, and the session actor is derived above from the persisted session.
	chain, err := newLifecycleAdmissionTransitionChain(selected.store, cfg.Lifecycle, scope, request.Actor, permitIssuer, policyResolver, workflowVerifier)
	if err != nil {
		return api.LifecycleClaimTransitionResult{}, err
	}
	// Capture and validate the current head explicitly at the provider boundary.
	// ClaimIdentity repeats this check immediately before applying the typed patch.
	evidence := worklifecycle.TransitionEvidence{Attachment: attachmentProof}
	head, err := chain.CurrentHead(current.ID, evidence)
	if err != nil {
		return api.LifecycleClaimTransitionResult{}, err
	}
	if head.ToVersion != current.Revision {
		return api.LifecycleClaimTransitionResult{}, worklifecycle.ErrTransitionChainStale
	}
	if current.Status == "open" && current.Assignee == "" &&
		(request.ExpectedRevision != head.ToVersion || request.ExpectedTransitionHead != head.ReceiptID) {
		return api.LifecycleClaimTransitionResult{}, worklifecycle.ErrTransitionChainStale
	}
	if err := ctx.Err(); err != nil {
		return api.LifecycleClaimTransitionResult{}, err
	}
	claimed, err := chain.ClaimIdentity(worklifecycle.ClaimIdentityRequest{
		IssueID: current.ID, SessionID: request.Session.ID,
		InstanceToken: request.Session.InstanceToken, RuntimeEpoch: request.Session.Generation,
		ExpectedRevision: request.ExpectedRevision, ExpectedTransitionHead: request.ExpectedTransitionHead,
		SessionName: request.Session.SessionNameMetadata, WorkDir: request.Session.WorkDir,
		Evidence: evidence,
	})
	if err != nil {
		return api.LifecycleClaimTransitionResult{}, err
	}
	return api.LifecycleClaimTransitionResult{
		ClaimGeneration: claimed.ClaimGeneration, ReceiptID: claimed.Receipt.ReceiptID,
		Replayed: claimed.Replayed, Recovered: claimed.Recovered,
	}, nil
}

func lifecycleClaimSourceStoreRef(cityName string, apiRef storeref.StoreRef) (string, error) {
	switch {
	case apiRef == storeref.WorkRef:
		return "city:" + cityName, nil
	case strings.TrimSpace(string(apiRef)) != "":
		return string(apiRef), nil
	default:
		return "", worklifecycle.ErrTransitionChainInvalid
	}
}

// acquireLifecycleClaimRouteLease keeps the exact stores and current policy
// snapshot used by this Q54 proof alive until the transition completes. Route
// publication and shutdown take the write side of compatibilityActionMu and
// advance graphStoreGeneration, so a claim that raced either change fails
// closed before reading or writing through a retired store handle.
func (cs *controllerState) acquireLifecycleClaimRouteLease(cfg *config.City, generation uint64) (func(), error) {
	cs.compatibilityActionMu.RLock()
	cs.mu.RLock()
	valid := !cs.compatibilityRoutesClosed && cs.cfg == cfg && cs.graphStoreGeneration == generation
	cs.mu.RUnlock()
	if !valid {
		cs.compatibilityActionMu.RUnlock()
		return nil, worklifecycle.ErrTransitionChainUnavailable
	}
	return cs.compatibilityActionMu.RUnlock, nil
}

func canonicalLifecycleClaimProviderEpoch(raw string) bool {
	if raw == "" || strings.TrimSpace(raw) != raw {
		return false
	}
	epoch, err := strconv.ParseInt(raw, 10, 64)
	return err == nil && epoch > 0 && strconv.FormatInt(epoch, 10) == raw
}

func errorsJoinTransitionEvidence(err error) error {
	return fmt.Errorf("%w: %w", worklifecycle.ErrTransitionChainEvidence, err)
}

var _ api.LifecycleClaimTransitionProvider = (*controllerState)(nil)
