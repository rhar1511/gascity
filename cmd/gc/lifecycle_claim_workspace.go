package main

import (
	"errors"
	"fmt"
	"path/filepath"
	"reflect"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/worklifecycle"
	"github.com/gastownhall/gascity/internal/worktree"
)

// Published custody is never inferred from the step's work_dir. Resolve the
// admitted source and retain its genuine native creation lock across the typed
// claim/completion, including exact retries. Existing non-workspace lifecycle
// work remains valid; any custody field on either row requires the full proof.
func withLifecycleClaimWorkspace(cfg *config.City, cityPath string, cityStore, store beads.Store, storeRef string, request api.LifecycleClaimTransitionRequest, observed beads.Bead, transition func() (api.LifecycleClaimTransitionResult, error)) (api.LifecycleClaimTransitionResult, error) {
	empty := api.LifecycleClaimTransitionResult{}
	sourceID := observed.ID
	if lineage, ok := lifecycleMaterializationFor(observed); ok && lineage.SourceID != observed.ID {
		if lineage.SourceID == "" || lineage.SourceStoreRef != storeRef || lineage.WorkflowStoreRef != storeRef {
			return empty, worklifecycle.ErrTransitionChainEvidence
		}
		sourceID = lineage.SourceID
	}
	source, err := beads.HandlesFor(store).Live.Get(sourceID)
	if err != nil {
		return empty, err
	}
	sessions := sessionFrontDoor(cityStore)
	_, persisted, err := sessions.ReadWorkClaimSession(request.Session)
	if err != nil {
		return empty, err
	}
	hasCustody := func(metadata map[string]string) bool {
		for key := range (worklifecycle.SourceWorkspace{}).PublicationPatch().Metadata {
			// Workdir/branch alone predate native custody publication.
			if key == beadmeta.WorkDirMetadataKey || key == beadmeta.LegacyWorkDirMetadataKey || key == beadmeta.WorkBranchMetadataKey {
				continue
			}
			if _, present := metadata[key]; present {
				return true
			}
		}
		return false
	}
	if !hasCustody(source.Metadata) && !hasCustody(observed.Metadata) && !hasCustody(persisted.Metadata) {
		return transition()
	}
	spec, err := worktreeSpecForBead(source, storeRef)
	if err != nil || spec == nil {
		return empty, errors.Join(worklifecycle.ErrTransitionChainEvidence, err)
	}
	spec.AttemptID = source.Metadata[beadmeta.WorktreeAttemptMetadataKey]
	workspace := lifecycleSourceWorkspace(*spec)
	if workspace.Validate() != nil || workspace.SourceID != sourceID || workspace.StoreRef != storeRef {
		return empty, worklifecycle.ErrTransitionChainEvidence
	}
	configuredRepo := ""
	for _, rig := range cfg.Rigs {
		if "rig:"+rig.Name == storeRef {
			configuredRepo = rig.Path
			break
		}
	}
	if configuredRepo != "" && !filepath.IsAbs(configuredRepo) {
		configuredRepo = filepath.Join(cityPath, configuredRepo)
	}
	if configuredRepo == "" || filepath.Clean(configuredRepo) != spec.RepoDir {
		return empty, worklifecycle.ErrTransitionChainEvidence
	}
	admission, err := worklifecycle.VerifyAdmissionReceiptV2(source, cfg.Lifecycle, request.Scope)
	if err != nil || admission.Route != spec.Owner {
		return empty, errors.Join(worklifecycle.ErrTransitionChainEvidence, err)
	}
	// Both source spellings and every protected custody field must agree. This
	// refuses incomplete legacy evidence instead of adopting it as native proof.
	verify := func() error {
		current, err := beads.HandlesFor(store).Live.Get(sourceID)
		if err != nil {
			return err
		}
		info, now, err := sessions.ReadWorkClaimSession(request.Session)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(now, persisted) || info.WorkDir != spec.Path || info.WorkDirCanonical != spec.Path {
			return worklifecycle.ErrTransitionChainStale
		}
		for key, patch := range workspace.PublicationPatch().Metadata {
			if current.Metadata[key] != patch.Value || now.Metadata[key] != patch.Value {
				return fmt.Errorf("workspace custody differs at %s: %w", key, worklifecycle.ErrTransitionChainEvidence)
			}
		}
		return nil
	}
	var result api.LifecycleClaimTransitionResult
	err = worktree.WithVerified(*spec, func(worktree.Report) error {
		if err := verify(); err != nil {
			return err
		}
		var err error
		result, err = transition()
		if err != nil {
			return err
		}
		// A post-commit refusal preserves the durable receipt for exact recovery;
		// it never rolls back work or reports changed custody as successful.
		if err := verify(); err != nil {
			return err
		}
		report, err := worktree.Verify(*spec)
		if err != nil {
			return err
		}
		if report.Provenance == nil || report.Provenance.AttemptID != spec.AttemptID {
			return worklifecycle.ErrTransitionChainStale
		}
		return nil
	})
	if err != nil {
		return empty, errors.Join(worklifecycle.ErrTransitionChainEvidence, err)
	}
	return result, nil
}
