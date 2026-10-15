package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/worklifecycle"
	"github.com/gastownhall/gascity/internal/worktree"
)

// PublishLifecycleWorkspace publishes independently verified native creation
// through the existing host-authorized transition chain. It never provisions a
// directory, transfers ownership, starts a session, or writes generic metadata.
func (cs *controllerState) PublishLifecycleWorkspace(ctx context.Context, request api.LifecycleWorkspaceTransitionRequest) (api.LifecycleWorkspaceResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	empty := api.LifecycleWorkspaceResult{}
	body := request.Submission
	if err := ctx.Err(); err != nil {
		return empty, err
	}
	if request.WorkStore == nil || request.Work.ID != body.WorkID || body.Validate() != nil {
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
	if err != nil || sourceRef != body.SourceStoreRef {
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
	if selected.store == nil {
		return empty, worklifecycle.ErrTransitionChainStale
	}
	// The declared repository must be the exact configured rig repository. A
	// valid worktree of some other repository cannot authorize this source.
	configuredRepo := ""
	for _, rig := range cfg.Rigs {
		if "rig:"+rig.Name != sourceRef {
			continue
		}
		configuredRepo = rig.Path
		if !filepath.IsAbs(configuredRepo) {
			configuredRepo = filepath.Join(cityPath, configuredRepo)
		}
		configuredRepo = filepath.Clean(configuredRepo)
		break
	}
	if configuredRepo == "" || configuredRepo != body.Workspace.RepoDir {
		return empty, fmt.Errorf("workspace repository differs from registered rig: %w", worklifecycle.ErrTransitionChainEvidence)
	}
	w := body.Workspace
	spec := worktree.Spec{
		RepoDir: configuredRepo, Root: w.Root, Path: w.Path, Branch: w.Branch,
		Base: w.BaseRef, BaseSHA: w.BaseSHA, BeadID: w.SourceID, StoreRef: sourceRef, Creator: w.Creator,
		Owner: w.Owner, Generation: w.Generation, Lifecycle: w.Lifecycle, AttemptID: w.AttemptID,
	}
	transition, err := buildLifecycleTransitionContextWithWorkspace(cityName, cityPath, cfg, cityStore, rigStores, legs, selected, body.WorkID, scope, resolver, authority, newLifecycleNativeWorkspaceVerifier(spec))
	if err != nil {
		return empty, err
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return empty, err
	}
	operation := lifecycleTransitionOperationID("workspace-publication", body.WorkID, scope, transition.admissionDigest, string(encoded))
	receiptID, err := worklifecycle.TransitionReceiptID(body.WorkID, scope, worklifecycle.TransitionStepWorkspacePublication, operation)
	if err != nil {
		return empty, err
	}
	reader, ok := beads.RevisionTransitionPatchReceiptReaderFor(selected.store)
	if !ok || reader == nil {
		return empty, worklifecycle.ErrTransitionChainUnavailable
	}
	_, replay, err := reader.ReadRevisionTransitionPatchReceipt(receiptID)
	if err != nil {
		return empty, err
	}
	apply := func() (api.LifecycleWorkspaceResult, error) {
		if err := ctx.Err(); err != nil {
			return empty, err
		}
		if !replay {
			current, err := beads.HandlesFor(selected.store).Live.Get(body.WorkID)
			if err != nil {
				return empty, err
			}
			if current.Status != "open" || current.Assignee != "" || !lifecycleCompletionMutable(current) ||
				current.Revision != body.ExpectedRevision || current.Metadata[beadmeta.LifecycleTransitionHeadMetadataKey] != body.ExpectedTransitionHead {
				return empty, worklifecycle.ErrTransitionChainStale
			}
		}
		result, err := transition.chain.Apply(worklifecycle.TransitionRequest{
			IssueID: body.WorkID, Step: worklifecycle.TransitionStepWorkspacePublication,
			OperationID: operation, PriorReceiptID: body.ExpectedTransitionHead, Evidence: transition.evidence, Patch: w.PublicationPatch(),
		})
		if err != nil {
			return empty, err
		}
		if result.Receipt.ExpectedVersion != body.ExpectedRevision || result.Receipt.ReceiptID != receiptID {
			return empty, worklifecycle.ErrTransitionChainReceipt
		}
		return api.LifecycleWorkspaceResult{WorkID: body.WorkID, ReceiptID: receiptID, Revision: result.Receipt.ToVersion, Replayed: result.Replayed || result.Recovered}, nil
	}
	// Historical exact retries validate immutable receipts; they never recreate
	// a cleaned workspace or claim that it is still available for dispatch.
	if replay {
		return apply()
	}
	if transition.source.Revision != body.ExpectedRevision || transition.head.ReceiptID != body.ExpectedTransitionHead || transition.head.Step != worklifecycle.TransitionStepAttachedMaterialization {
		return empty, worklifecycle.ErrTransitionChainStale
	}
	if strings.TrimSpace(w.Owner) != transition.admission.Route {
		return empty, worklifecycle.ErrTransitionChainEvidence
	}
	var result api.LifecycleWorkspaceResult
	err = worktree.WithVerified(spec, func(worktree.Report) error {
		var applyErr error
		result, applyErr = apply()
		return applyErr
	})
	return result, err
}

var _ api.LifecycleWorkspaceTransitionProvider = (*controllerState)(nil)
