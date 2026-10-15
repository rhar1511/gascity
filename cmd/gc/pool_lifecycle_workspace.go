package main

import (
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worktree"
)

// Publish through the session front door, with a single revision-fenced cluster
// update. Never repoint an existing native conversation or legacy worktree.
// A supported ownership transfer must be explicit, outside ordinary dispatch.
func bindPoolLifecycleWorkspace(bp *agentBuildParams, cfgAgent *config.Agent, qualifiedName string, info session.Info, request SessionRequest) (session.Info, error) {
	bound := info
	err := session.WithSessionMutationLock(info.ID, func() error {
		spec, protected, verifyErr := poolLifecycleSourceWorkspace(bp, cfgAgent, qualifiedName, request)
		if verifyErr != nil || !protected || spec == nil {
			return fmt.Errorf("protected source workspace unavailable: %w", verifyErr)
		}
		// Keep session-before-worktree ordering. Native custody lasts through
		// the conditional session write; a changed source path cannot redirect
		// that write onto a workspace outside the lock held here.
		return worktree.WithVerified(*spec, func(worktree.Report) error {
			writer, _, err := beads.ResolveConditionalWriter(bp.beadStore)
			if err != nil || writer == nil {
				return fmt.Errorf("protected workspace binding requires conditional session writes: %w", err)
			}
			var decisionErr error
			alreadyBound := false
			applied, err := sessionFrontDoor(bp.beadStore).UpdateMetadataAuthoritativeFenced(info.ID, 1, func(current session.Info, persisted session.PersistedResponse) session.MetadataPatch {
				if current.Closed || !reflect.DeepEqual(current, info) ||
					(request.SessionBeadID != "" && request.SessionBeadID != current.ID) ||
					(current.Template != "" && current.Template != cfgAgent.QualifiedName()) {
					decisionErr = fmt.Errorf("session identity, incarnation, or binding changed")
					return nil
				}
				path, ownership, verifyErr := verifiedPoolTriggerWorkspace(bp, cfgAgent, qualifiedName, request)
				if verifyErr != nil || ownership == nil || path != spec.Path || !reflect.DeepEqual(ownership, poolLifecycleWorkspaceMetadata(spec)) {
					decisionErr = fmt.Errorf("source workspace proof changed: %w", verifyErr)
					return nil
				}
				if (current.WorkDir != "" && current.WorkDir != path) ||
					(current.WorkDirCanonical != "" && current.WorkDirCanonical != path) {
					decisionErr = fmt.Errorf("existing session workspace requires exact-owner recovery")
					return nil
				}
				patch := computePoolTriggerBindingPatch(current, request, path)
				for key, value := range ownership {
					old := persisted.Metadata[key]
					if old != "" && old != value {
						decisionErr = fmt.Errorf("existing session workspace ownership differs at %s", key)
						return nil
					}
					if old != value {
						patch[key] = value
					}
				}
				alreadyBound = len(patch) == 0
				bound = current.ApplyPatch(patch)
				return patch
			})
			if decisionErr != nil {
				return decisionErr
			}
			if err != nil {
				return err
			}
			if !applied && !alreadyBound {
				return fmt.Errorf("session revision changed before workspace publication")
			}
			return nil
		})
	})
	if err != nil {
		return info, fmt.Errorf("%w: %w", errPoolTriggerWorktreeEvidence, err)
	}
	return bound, nil
}

const (
	poolWorkspaceSourceIDKey = beadmeta.WorktreeSourceBeadMetadataKey
	poolWorkspaceStoreKey    = beadmeta.WorktreeSourceStoreMetadataKey
	poolWorkspaceAttemptKey  = beadmeta.WorktreeAttemptMetadataKey
)

// verifiedPoolTriggerWorkspace keeps execution identity on the demand step,
// while a protected descendant uses its admitted source's native ownership.
// Planning metadata is not authority: resolve and verify again at publication.
// This function never creates a worktree or writes to a work bead.
func verifiedPoolTriggerWorkspace(bp *agentBuildParams, cfgAgent *config.Agent, qualifiedName string, request SessionRequest) (string, map[string]string, error) {
	spec, protected, err := poolLifecycleSourceWorkspace(bp, cfgAgent, qualifiedName, request)
	if err != nil {
		return "", nil, fmt.Errorf("%w: %w", errPoolTriggerWorktreeEvidence, err)
	}
	if !protected {
		path, err := verifiedLegacyPoolTriggerWorkDir(bp, cfgAgent, qualifiedName, request)
		return path, nil, err
	}
	if bp.realizeProbe != nil {
		bp.realizeProbe.worktreeVerifies.Add(1)
	}
	report, err := worktree.Verify(*spec)
	if err != nil {
		return "", nil, fmt.Errorf("%w: source workspace: %w", errPoolTriggerWorktreeEvidence, err)
	}
	// Verify checks owner/generation, but AttemptID is otherwise a cleanup-only
	// field. Binding must also reject replacement at the same path/generation.
	if report.Provenance == nil || report.Provenance.AttemptID != spec.AttemptID {
		return "", nil, fmt.Errorf("%w: source workspace attempt changed", errPoolTriggerWorktreeEvidence)
	}
	return report.Path, poolLifecycleWorkspaceMetadata(spec), nil
}

func poolLifecycleWorkspaceMetadata(spec *worktree.Spec) map[string]string {
	return map[string]string{
		poolWorkspaceSourceIDKey: spec.BeadID, poolWorkspaceStoreKey: spec.StoreRef,
		poolWorkspaceAttemptKey:          spec.AttemptID,
		beadmeta.WorktreeRepoMetadataKey: spec.RepoDir, beadmeta.WorktreeRootMetadataKey: spec.Root,
		beadmeta.WorkBranchMetadataKey: spec.Branch, beadmeta.WorktreeBaseRefMetadataKey: spec.Base,
		beadmeta.WorktreeBaseSHAMetadataKey: spec.BaseSHA, beadmeta.WorktreeCreatorMetadataKey: spec.Creator,
		beadmeta.WorktreeOwnerMetadataKey: spec.Owner, beadmeta.WorktreeGenerationMetadataKey: spec.Generation,
		beadmeta.WorktreeLifecycleMetadataKey: spec.Lifecycle,
	}
}

func poolLifecycleSourceWorkspace(bp *agentBuildParams, cfgAgent *config.Agent, qualifiedName string, request SessionRequest) (*worktree.Spec, bool, error) {
	if bp == nil || bp.city == nil || !bp.city.Lifecycle.AdmissionEnabled || strings.TrimSpace(request.WorkBeadID) == "" {
		return nil, false, nil
	}
	if cfgAgent == nil || bp.beadStore == nil {
		return nil, true, fmt.Errorf("protected workspace requires controller store and route context")
	}
	legs, err := routedWorkStoreCandidates(bp.cityPath, bp.city, bp.beadStore, bp.sessionCensusRigStores,
		buildSuspendedRigPathsForCity(bp.city, bp.cityPath))
	if err != nil {
		return nil, true, err
	}
	// Canonicalize only the known local city alias. Do not collapse two city
	// identities or accept arbitrary bare rig names as protected authority.
	storeRef := strings.TrimSpace(request.WorkStoreRef)
	if storeRef == "city" {
		storeRef = "city:" + bp.cityName
	}
	resolve := func(ref string) (beads.Store, error) {
		for _, leg := range legs {
			if leg.ref == ref && leg.store != nil {
				return leg.store, nil
			}
		}
		return nil, fmt.Errorf("workspace store %q is not a current routed store", ref)
	}
	store, err := resolve(storeRef)
	if err != nil {
		return nil, true, err
	}
	row, err := beads.HandlesFor(store).Live.Get(request.WorkBeadID)
	if err != nil || row.ID != request.WorkBeadID {
		return nil, true, fmt.Errorf("workspace trigger %q unavailable", request.WorkBeadID)
	}
	if !lifecycleProtectedWork(row, bp.city) {
		return nil, false, nil
	}
	if (row.Status != "open" && row.Status != "in_progress") || lifecycleRowHeld(row) || beads.IsDeferred(row, time.Now()) ||
		(row.Assignee != "" && row.Assignee != qualifiedName) {
		return nil, true, fmt.Errorf("workspace trigger is terminal, held, deferred, or owned by another actor")
	}
	lineage, ok := lifecycleMaterializationFor(row)
	scope := lifecycleScopeForRef(bp.cityName, bp.city, storeRef)
	row.SourceStoreRef, row.LifecycleScope = storeRef, scope
	if !ok || lineage.SourceID == "" || lineage.SourceID == row.ID || lineage.Scope != scope ||
		lineage.SourceStoreRef != storeRef || lineage.WorkflowStoreRef != storeRef ||
		(row.Metadata[beadmeta.RootStoreRefMetadataKey] != "" && row.Metadata[beadmeta.RootStoreRefMetadataKey] != storeRef) ||
		row.Metadata[beadmeta.LifecycleAdmissionReceiptMetadataKey] != "" ||
		row.Metadata[beadmeta.LifecycleAdmissionReceiptV2MetadataKey] != "" ||
		!hookLifecycleRouteMatches(bp.city, lineage.Route, []string{cfgAgent.QualifiedName()}) {
		return nil, true, fmt.Errorf("workspace trigger has no exact same-store descendant route")
	}
	source, err := beads.HandlesFor(store).Live.Get(lineage.SourceID)
	if err != nil || source.ID != lineage.SourceID {
		return nil, true, fmt.Errorf("workspace source unavailable")
	}
	opts := hookClaimOptions{
		Lifecycle: bp.city.Lifecycle, LifecycleCity: bp.city, TrustedLifecycleScope: true,
		IdentityCandidates: []string{qualifiedName}, RouteTargets: []string{cfgAgent.QualifiedName()},
		ResolveLifecycleStore:         resolve,
		VerifyLifecycleTransitionHead: newLifecycleClaimTransitionHeadVerifier(bp.cityPath, bp.city, resolve),
	}
	if !lifecycleLineageAdmissionMatches(bp.city, row, lineage) || !lifecycleLineageCurrent(row, lineage, opts) {
		return nil, true, fmt.Errorf("workspace source admission or descendant lineage is not current")
	}
	// Do not combine ownership from one revision with admission from another.
	current, err := beads.HandlesFor(store).Live.Get(source.ID)
	trigger, triggerErr := beads.HandlesFor(store).Live.Get(row.ID)
	if err != nil || triggerErr != nil || current.Revision != source.Revision || trigger.Revision != row.Revision ||
		current.Metadata[beadmeta.LifecycleTransitionHeadMetadataKey] != source.Metadata[beadmeta.LifecycleTransitionHeadMetadataKey] {
		return nil, true, fmt.Errorf("workspace source or trigger changed during verification")
	}
	spec, err := worktreeSpecForBead(current, storeRef)
	if err != nil {
		return nil, true, err
	}
	if spec == nil || spec.StoreRef != storeRef || spec.BeadID != lineage.SourceID {
		return nil, true, fmt.Errorf("admitted source has no complete published native workspace ownership")
	}
	spec.AttemptID = current.Metadata[poolWorkspaceAttemptKey]
	workspace := lifecycleSourceWorkspace(*spec)
	if workspace.Validate() != nil {
		return nil, true, fmt.Errorf("admitted source has incomplete native workspace custody")
	}
	for key, patch := range workspace.PublicationPatch().Metadata {
		if current.Metadata[key] != patch.Value {
			return nil, true, fmt.Errorf("admitted source custody differs at %s", key)
		}
	}
	configuredRepo := ""
	for _, rig := range bp.city.Rigs {
		if "rig:"+rig.Name == storeRef {
			configuredRepo = rig.Path
			break
		}
	}
	if configuredRepo != "" && !filepath.IsAbs(configuredRepo) {
		configuredRepo = filepath.Join(bp.cityPath, configuredRepo)
	}
	if configuredRepo == "" || filepath.Clean(configuredRepo) != spec.RepoDir || spec.Owner != cfgAgent.QualifiedName() {
		return nil, true, fmt.Errorf("source workspace repository or owner differs from admitted route")
	}
	return spec, true, nil
}
