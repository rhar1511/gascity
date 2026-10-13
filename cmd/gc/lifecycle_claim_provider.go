package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/agentutil"
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
	return cs.transitionLifecycleWork(ctx, request, nil)
}

// transitionLifecycleWork shares exact route and descendant authority checks.
// Completion is an internal intent, never a field accepted by the claim API.
func (cs *controllerState) transitionLifecycleWork(ctx context.Context, request api.LifecycleClaimTransitionRequest, completion *api.LifecycleExecutionCompletionIntent) (api.LifecycleClaimTransitionResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return api.LifecycleClaimTransitionResult{}, err
	}
	if strings.TrimSpace(request.Work.ID) == "" || request.WorkStore == nil || strings.TrimSpace(request.Session.ID) != request.Session.ID ||
		request.Session.ID == "" || request.Actor == "" || request.ExpectedRevision == 0 ||
		strings.TrimSpace(request.ExpectedTransitionHead) != request.ExpectedTransitionHead {
		return api.LifecycleClaimTransitionResult{}, worklifecycle.ErrTransitionChainInvalid
	}

	if request.ExecutionWork.ID != "" || request.ExecutionClaimGeneration != "" {
		if err := cs.verifyLifecycleSourceExecution(ctx, request); err != nil {
			return api.LifecycleClaimTransitionResult{}, err
		}
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
	observed, err := beads.HandlesFor(selected.store).Live.Get(request.Work.ID)
	if err != nil {
		return api.LifecycleClaimTransitionResult{}, err
	}
	if lineage, ok := lifecycleMaterializationFor(observed); ok && lineage.SourceID != observed.ID &&
		observed.Metadata[beadmeta.LifecycleAdmissionReceiptV2MetadataKey] == "" {
		resolveStore := func(ref string) (beads.Store, error) {
			for _, leg := range legs {
				if leg.ref == ref {
					return leg.store, nil
				}
			}
			if ref == "city:"+cityName {
				return cityStore, nil
			}
			return nil, worklifecycle.ErrTransitionChainUnavailable
		}
		opts := hookClaimOptions{
			Lifecycle: cfg.Lifecycle, LifecycleCity: cfg, TrustedLifecycleScope: true,
			IdentityCandidates: []string{request.Actor}, ResolveLifecycleStore: resolveStore,
			VerifyLifecycleTransitionHead: newLifecycleClaimTransitionHeadVerifier(cityPath, cfg, resolveStore),
		}
		verify := func(row beads.Bead) (worklifecycle.DescendantBinding, error) {
			if err := ctx.Err(); err != nil {
				return worklifecycle.DescendantBinding{}, err
			}
			marker, valid := lifecycleMaterializationFor(row)
			row.SourceStoreRef, row.LifecycleScope = selected.ref, scope
			if !valid || row.ID != request.Work.ID || marker.Scope != scope || marker.SourceStoreRef != selected.ref || marker.WorkflowStoreRef != selected.ref ||
				row.Metadata[beadmeta.LifecycleAdmissionReceiptMetadataKey] != "" || row.Metadata[beadmeta.LifecycleAdmissionReceiptV2MetadataKey] != "" ||
				!lifecycleLineageAdmissionMatches(cfg, row, marker) || !lifecycleLineageCurrent(row, marker, opts) {
				return worklifecycle.DescendantBinding{}, worklifecycle.ErrTransitionChainEvidence
			}
			target := findAgentByTemplate(cfg, agentutil.NormalizePoolRouteTarget(cfg, marker.Route))
			if target == nil {
				return worklifecycle.DescendantBinding{}, worklifecycle.ErrTransitionChainEvidence
			}
			if err := verifyLifecycleClaimSessionRoute(cfg, request.Session, lifecycleAdmissionPolicy{target: *target}, marker.Route); err != nil {
				return worklifecycle.DescendantBinding{}, err
			}
			parent, err := selected.store.Get(marker.SourceID)
			if err != nil {
				return worklifecycle.DescendantBinding{}, err
			}
			admission, err := worklifecycle.VerifyAdmissionReceiptV2(parent, cfg.Lifecycle, scope)
			if err != nil {
				return worklifecycle.DescendantBinding{}, err
			}
			policy, err := buildLifecycleAdmissionPolicy(parent, admission, scope, cityName, cityPath, cfg, cityStore, rigStores, selected, legs, sling.SlingRunner(shellSlingRunner), authority)
			if err != nil {
				return worklifecycle.DescendantBinding{}, err
			}
			if policy.projection.FormulaSchemaVersion == beadmeta.FormulaContractGraphV2 {
				if err := verifyLifecycleGraphWorkerStep(ctx, selected.store, parent, row, policy, cfg); err != nil {
					return worklifecycle.DescendantBinding{}, fmt.Errorf("verify compiled graph worker step: %w", errorsJoinTransitionEvidence(err))
				}
			}
			attached, valid := lifecycleMaterializationFor(parent)
			if !valid || attached.WorkflowID == "" || row.ID == attached.WorkflowID {
				return worklifecycle.DescendantBinding{}, worklifecycle.ErrTransitionChainEvidence
			}
			if row.Status == "open" && row.Assignee == "" {
				if err := verifyLifecycleClaimReadiness(selected.store, row.ID, row.Revision, row.Metadata[beadmeta.LifecycleTransitionHeadMetadataKey]); err != nil {
					return worklifecycle.DescendantBinding{}, fmt.Errorf("verify descendant readiness: %w", err)
				}
			} else if (row.Status != "in_progress" && (completion == nil || row.Status != "closed")) || row.Assignee != request.Actor || row.Metadata[beadmeta.SessionIDMetadataKey] != request.Session.ID {
				return worklifecycle.DescendantBinding{}, worklifecycle.ErrTransitionChainStale
			}
			digest := sha256.Sum256([]byte(row.Metadata[beadmeta.LifecycleMaterializationMetadataKey]))
			return worklifecycle.DescendantBinding{
				AdmissionDigest: marker.Contract, LineageDigest: hex.EncodeToString(digest[:]),
				ParentID: marker.SourceID, Scope: scope, StoreRef: selected.ref, WorkflowID: attached.WorkflowID,
			}, nil
		}
		descendantConfig := worklifecycle.DescendantClaimConfig{
			Store: selected.store, Scope: scope, Actor: request.Actor, AnchorActor: permitPolicy.Actor, PermitIssuer: permitIssuer, Verify: verify,
		}
		claimRequest := worklifecycle.ClaimIdentityRequest{
			IssueID: observed.ID, SessionID: request.Session.ID, InstanceToken: request.Session.InstanceToken, RuntimeEpoch: request.Session.Generation,
			ExpectedRevision: request.ExpectedRevision, ExpectedTransitionHead: request.ExpectedTransitionHead, SessionName: request.Session.SessionNameMetadata, WorkDir: request.Session.WorkDir,
		}
		var claimed worklifecycle.ClaimIdentityResult
		if completion != nil {
			claimed, err = worklifecycle.CompleteDescendantExecution(descendantConfig, worklifecycle.DescendantCompletionRequest{
				ClaimIdentityRequest: claimRequest, ClaimGeneration: completion.ClaimGeneration, Outcome: completion.Outcome,
			})
		} else {
			claimed, err = worklifecycle.ClaimDescendantIdentity(descendantConfig, claimRequest)
		}
		if err != nil {
			return api.LifecycleClaimTransitionResult{}, err
		}
		return api.LifecycleClaimTransitionResult{ClaimGeneration: claimed.ClaimGeneration, ReceiptID: claimed.Receipt.ReceiptID, Revision: claimed.Receipt.ToVersion, Replayed: claimed.Replayed, Recovered: claimed.Recovered}, nil
	}
	if completion != nil {
		return api.LifecycleClaimTransitionResult{}, worklifecycle.ErrTransitionChainEvidence
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
	admissionReceipt, err := worklifecycle.VerifyAdmissionReceiptV2(current, cfg.Lifecycle, scope)
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
	if err := verifyLifecycleClaimSessionRoute(cfg, request.Session, resolvedPolicy, admissionReceipt.Route); err != nil {
		return api.LifecycleClaimTransitionResult{}, err
	}
	if current.Status == "open" && current.Assignee == "" &&
		(request.ExpectedRevision != head.ToVersion || request.ExpectedTransitionHead != head.ReceiptID) {
		return api.LifecycleClaimTransitionResult{}, worklifecycle.ErrTransitionChainStale
	}
	if current.Status == "open" && current.Assignee == "" {
		if err := verifyLifecycleClaimReadiness(selected.store, current.ID, head.ToVersion, head.ReceiptID); err != nil {
			return api.LifecycleClaimTransitionResult{}, err
		}
	}
	if err := ctx.Err(); err != nil {
		return api.LifecycleClaimTransitionResult{}, err
	}
	if request.ExecutionWork.ID != "" {
		if err := verifyLifecycleSourceExecutionSnapshot(request); err != nil {
			return api.LifecycleClaimTransitionResult{}, err
		}
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
	if request.ExecutionWork.ID != "" {
		if err := verifyLifecycleSourceExecutionSnapshot(request); err != nil {
			return api.LifecycleClaimTransitionResult{}, err
		}
	}
	return api.LifecycleClaimTransitionResult{
		ClaimGeneration: claimed.ClaimGeneration, ReceiptID: claimed.Receipt.ReceiptID,
		Replayed: claimed.Replayed, Recovered: claimed.Recovered,
	}, nil
}

// verifyLifecycleClaimSessionRoute binds the authenticated managed session to
// the exact route in the current signed admission policy. Session template
// resolution uses the canonical Info helper so concrete pool slots collapse to
// their configured base agent before route comparison.
func verifyLifecycleClaimSessionRoute(cfg *config.City, info session.Info, policy lifecycleAdmissionPolicy, receiptRoute string) error {
	sessionAgent := sessionAgentConfigInfo(cfg, info)
	if sessionAgent == nil {
		return fmt.Errorf("managed claim session has no unambiguous configured agent route: %w", worklifecycle.ErrTransitionChainEvidence)
	}
	sessionRoute := agentutil.RoutedToIdentity(sessionAgent)
	policyRoute := agentutil.RoutedToIdentity(&policy.target)
	if receiptRoute == "" || policyRoute == "" || policyRoute != receiptRoute || sessionRoute != policyRoute {
		return fmt.Errorf("managed claim session route %q does not match signed admission route %q: %w", sessionRoute, receiptRoute, worklifecycle.ErrTransitionChainEvidence)
	}
	return nil
}

// verifyLifecycleClaimReadiness re-reads the exact source after its transition
// head is verified and immediately before ClaimIdentity. The hook's projection
// is only a candidate locator: a fresh claim requires the current source to be
// open, unassigned, outside all deferrals and dispatch holds, and present in the
// authoritative live ready set. Dependency readiness is whatever that final
// live read observes; it is not an atomic fence on independent dependency rows.
func verifyLifecycleClaimReadiness(store beads.Store, workID string, expectedRevision int64, expectedHead string) error {
	if store == nil || strings.TrimSpace(workID) == "" || expectedRevision == 0 {
		return worklifecycle.ErrTransitionChainUnavailable
	}
	live := beads.HandlesFor(store).Live
	current, err := live.Get(workID)
	if err != nil {
		return fmt.Errorf("read current lifecycle claim source: %w", errorsJoinTransitionEvidence(err))
	}
	if current.ID != workID || current.Revision != expectedRevision ||
		current.Metadata[beadmeta.LifecycleTransitionHeadMetadataKey] != expectedHead ||
		current.Status != "open" || current.Assignee != "" {
		return fmt.Errorf("claim snapshot changed before readiness read: %w", worklifecycle.ErrTransitionChainStale)
	}
	now := time.Now()
	if (current.IsBlocked != nil && *current.IsBlocked) || lifecycleRowHeld(current) ||
		beads.IsDeferred(current, now) || hookCandidateBudgetDeferred(current, now) {
		return fmt.Errorf("claim snapshot is blocked, held, or deferred: %w", worklifecycle.ErrTransitionChainStale)
	}
	ready, err := live.Ready(beads.ReadyQuery{TierMode: beads.FederatedReadTier})
	if err != nil {
		return fmt.Errorf("read current lifecycle claim ready set: %w", errorsJoinTransitionEvidence(err))
	}
	for _, row := range ready {
		if row.ID == workID {
			return nil
		}
	}
	return fmt.Errorf("claim snapshot is absent from live ready set: %w", worklifecycle.ErrTransitionChainStale)
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

// verifyLifecycleSourceExecution proves that this is the authenticated worker's
// already claimed descendant of the exact source. Replaying an existing child
// claim verifies its Q43 anchor, Q54 receipt, signed parent policy, compiled
// membership, and runtime incarnation without writing another child transition.
// It runs before taking the source route lease to avoid recursive RWMutex reads
// across a waiting route publisher.
func (cs *controllerState) verifyLifecycleSourceExecution(ctx context.Context, request api.LifecycleClaimTransitionRequest) error {
	step := request.ExecutionWork
	if step.ID == "" || step.ID == request.Work.ID || !canonicalLifecycleClaimProviderEpoch(request.ExecutionClaimGeneration) ||
		request.Work.Metadata[beadmeta.LifecycleAdmissionReceiptV2MetadataKey] == "" ||
		step.Metadata[beadmeta.LifecycleAdmissionReceiptV2MetadataKey] != "" || step.Metadata[beadmeta.LifecycleAdmissionReceiptMetadataKey] != "" {
		return worklifecycle.ErrTransitionChainEvidence
	}
	if err := verifyLifecycleSourceExecutionSnapshot(request); err != nil {
		return err
	}
	lineage, ok := lifecycleMaterializationFor(step)
	if !ok || lineage.SourceID != request.Work.ID || lineage.SourceStoreRef != lineage.WorkflowStoreRef {
		return worklifecycle.ErrTransitionChainEvidence
	}
	childRequest := request
	childRequest.Work = step
	childRequest.ExpectedRevision = step.Revision
	childRequest.ExpectedTransitionHead = step.Metadata[beadmeta.LifecycleTransitionHeadMetadataKey]
	childRequest.ExecutionWork = beads.Bead{}
	childRequest.ExecutionClaimGeneration = ""
	result, err := cs.ClaimLifecycleWork(ctx, childRequest)
	if err != nil {
		return err
	}
	if !result.Replayed || result.Recovered || result.ClaimGeneration != request.ExecutionClaimGeneration || result.ReceiptID != childRequest.ExpectedTransitionHead {
		return worklifecycle.ErrTransitionChainEvidence
	}
	return verifyLifecycleSourceExecutionSnapshot(request)
}

// This is a live observation of a separately owned row, not an atomic cross-row
// fence. A concurrent external step/session mutation can leave a committed
// source claim with an error response; retries must preserve the source's
// original observation and revalidate both ownership proofs.
func verifyLifecycleSourceExecutionSnapshot(request api.LifecycleClaimTransitionRequest) error {
	step := request.ExecutionWork
	current, err := beads.HandlesFor(request.WorkStore).Live.Get(step.ID)
	if err != nil {
		return worklifecycle.ErrTransitionChainStale
	}
	if current.ID != step.ID || current.Revision == 0 || current.Revision != step.Revision || current.Status != "in_progress" ||
		current.Assignee != request.Actor || current.Metadata[beadmeta.SessionIDMetadataKey] != request.Session.ID ||
		current.Metadata[beadmeta.ClaimGenerationMetadataKey] != request.ExecutionClaimGeneration ||
		current.Metadata[beadmeta.LifecycleTransitionHeadMetadataKey] == "" ||
		current.Metadata[beadmeta.LifecycleTransitionHeadMetadataKey] != step.Metadata[beadmeta.LifecycleTransitionHeadMetadataKey] ||
		current.Metadata[beadmeta.LifecycleMaterializationMetadataKey] != step.Metadata[beadmeta.LifecycleMaterializationMetadataKey] {
		return worklifecycle.ErrTransitionChainStale
	}
	return nil
}

// CompleteLifecycleExecution finishes only an authenticated descendant claim.
func (cs *controllerState) CompleteLifecycleExecution(ctx context.Context, request api.LifecycleExecutionCompletionTransitionRequest) (api.LifecycleCompletionResult, error) {
	result, err := cs.transitionLifecycleWork(ctx, request.Claim, &request.Intent)
	if err != nil {
		return api.LifecycleCompletionResult{}, err
	}
	return api.LifecycleCompletionResult{WorkID: request.Claim.Work.ID, ReceiptID: result.ReceiptID, Revision: result.Revision, Replayed: result.Replayed || result.Recovered}, nil
}
