package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/attemptevidence"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/storeref"
)

// captureWorkbenchCloseTargets seals every persisted Workbench execution in a
// CLI close/update before bd can close the source bead. A known execution is
// never allowed to proceed when its session identity, source read, or durable
// private-payload write is unavailable.
func captureWorkbenchCloseTargets(ctx context.Context, store beads.Store, ids []string, preFetched map[string]beads.Bead, dirs workRecordRepoDirs, cityPath string, cfg *config.City) error {
	if store == nil {
		return errors.New("work store is unavailable")
	}
	for _, id := range ids {
		bead, ok := preFetched[id]
		if !ok {
			var err error
			bead, err = store.Get(id)
			if errors.Is(err, beads.ErrNotFound) {
				continue
			}
			if err != nil {
				return fmt.Errorf("read closing work bead %s before attempt capture: %w", id, err)
			}
		}
		if !attemptevidence.IsExecutionRecord(bead) {
			continue
		}
		if err := captureWorkbenchAttemptEvidence(ctx, store, bead, dirs, cityPath, cfg); err != nil {
			return fmt.Errorf("capture workbench execution %s: %w", bead.ID, err)
		}
	}
	return nil
}

func captureWorkbenchAttemptEvidence(ctx context.Context, store beads.Store, bead beads.Bead, dirs workRecordRepoDirs, cityPath string, cfg *config.City) error {
	return captureWorkbenchAttemptEvidenceFromStores(ctx, store, store, bead, dirs, cityPath, cfg)
}

func captureWorkbenchAttemptEvidenceFromStores(ctx context.Context, archiveStore, sessionReadStore beads.Store, bead beads.Bead, dirs workRecordRepoDirs, cityPath string, cfg *config.City) error {
	if !attemptevidence.IsExecutionRecord(bead) {
		return nil
	}
	sessionID := strings.TrimSpace(bead.Metadata[beadmeta.SessionIDMetadataKey])
	sessionStore := session.NewStore(beads.SessionStore{Store: cliSessionStore(sessionReadStore, cfg, cityPath)})
	info, err := sessionStore.Get(sessionID)
	if err != nil {
		return fmt.Errorf("read session %s: %w", sessionID, err)
	}
	if strings.TrimSpace(info.Generation) == "" {
		return fmt.Errorf("session %s has no persisted generation", sessionID)
	}
	repoDir := strings.TrimSpace(dirs.repoDirFor(bead))
	if cfg == nil {
		cfg = &config.City{}
		if dirs.rigs != nil {
			cfg.Rigs = dirs.rigs()
		}
	}
	storeRefRoot := repoDir
	if storeRefRoot == "" {
		storeRefRoot = dirs.legacy
	}
	storeRef, err := attemptEvidenceStoreRef(cityPath, storeRefRoot, cfg, bead)
	if err != nil {
		return err
	}
	workDir := firstNonBlankEvidenceValue(bead.Metadata[beadmeta.WorkDirMetadataKey], bead.Metadata[beadmeta.LegacyWorkDirMetadataKey])
	if workDir != "" && !filepath.IsAbs(workDir) {
		if repoDir == "" {
			return fmt.Errorf("relative worktree %q has no exact repository root", workDir)
		}
		workDir = filepath.Join(repoDir, workDir)
	}
	spec := attemptevidence.CaptureSpec{
		Identity: attemptevidence.Identity{
			Kind:              attemptevidence.KindWorkbench,
			OwnerBeadID:       bead.ID,
			ExecutionBeadID:   bead.ID,
			SessionID:         sessionID,
			SessionGeneration: strings.TrimSpace(info.Generation),
			ClaimGeneration:   strings.TrimSpace(bead.Metadata[beadmeta.ClaimGenerationMetadataKey]),
		},
		StoreRef: storeRef,
		Permission: attemptevidence.PermissionScope{
			StoreRef: storeRef, WorkID: bead.ID,
			RepositoryRoot: repoDir, WorkspaceRoot: workDir,
		},
		WorkDir: workDir,
		BaseSHA: strings.TrimSpace(bead.Metadata[beadmeta.WorktreeBaseSHAMetadataKey]),
		Outcome: strings.TrimSpace(bead.Metadata[beadmeta.WorkOutcomeMetadataKey]),
	}
	_, err = attemptevidence.Capture(ctx, archiveStore, spec)
	return err
}

func captureWorkbenchBeforeAssignmentRelease(ctx context.Context, cityPath string, cfg *config.City, archiveStore, sessionReadStore beads.Store, bead beads.Bead) error {
	if !attemptevidence.IsExecutionRecord(bead) {
		return nil
	}
	dirs := workRecordRepoDirs{
		cityPath: cityPath,
		legacy:   cityPath,
		rigs: func() []config.Rig {
			if cfg == nil {
				return nil
			}
			return cfg.Rigs
		},
	}
	if err := captureWorkbenchAttemptEvidenceFromStores(ctx, archiveStore, sessionReadStore, bead, dirs, cityPath, cfg); err != nil {
		return fmt.Errorf("capture execution %s before assignment release: %w", bead.ID, err)
	}
	return nil
}

// captureAssignedWorkbenchAttempts seals execution records before session
// retirement can release their assignments or prune their worktree. It scans
// the same exact store-leg plan used by session release, and any incomplete
// leg/list/capture result blocks the caller before it changes ownership.
func captureAssignedWorkbenchAttempts(ctx context.Context, cityPath string, cfg *config.City, store beads.Store, rigStores map[string]beads.Store, sessionBead beads.Bead) error {
	if store == nil {
		return errors.New("work store is unavailable")
	}
	if strings.TrimSpace(sessionBead.ID) == "" {
		return errors.New("session identity is unavailable")
	}
	return captureAssignedWorkbenchAttemptsForIdentifiers(ctx, cityPath, cfg, store, rigStores, sessionBead.ID, sessionAssignmentIdentifiersForConfig(sessionBead, cfg))
}

// captureSessionWorkBeforeClose rereads the current session bead and seals its
// assigned Workbench executions before closeBead can release those claims. A
// failed bead read or incomplete capture keeps the caller from closing the
// session, so the assignment and its checkout remain reachable for a retry.
func captureSessionWorkBeforeClose(ctx context.Context, cityPath string, cfg *config.City, store beads.Store, rigStores map[string]beads.Store, sessionID string) error {
	if store == nil {
		return errors.New("session store is unavailable")
	}
	sessionBead, err := store.Get(sessionID)
	if err != nil {
		return fmt.Errorf("read session %s before close capture: %w", sessionID, err)
	}
	if sessionBead.Status == "closed" {
		return nil
	}
	if err := captureAssignedWorkbenchAttempts(ctx, cityPath, cfg, store, rigStores, sessionBead); err != nil {
		return fmt.Errorf("capture assigned attempts for session %s: %w", sessionID, err)
	}
	return nil
}

func captureAssignedWorkbenchAttemptsForIdentifiers(ctx context.Context, cityPath string, cfg *config.City, store beads.Store, rigStores map[string]beads.Store, sessionID string, identifiers []string) error {
	if store == nil {
		return errors.New("work store is unavailable")
	}
	if strings.TrimSpace(sessionID) == "" {
		return errors.New("session identity is unavailable")
	}
	if strings.TrimSpace(cityPath) != "" && cfg == nil {
		return errors.New("city configuration is unavailable; cannot resolve every assigned-work store")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	plan, err := assignedWorkSweepPlan(cityPath, cfg, store, rigStores, identifiers)
	if err != nil {
		return fmt.Errorf("resolve assigned-work stores for session %s: %w", sessionID, err)
	}
	dirs := workRecordRepoDirs{
		cityPath: cityPath,
		legacy:   cityPath,
		rigs: func() []config.Rig {
			if cfg == nil {
				return nil
			}
			return cfg.Rigs
		},
	}
	seen := make(map[string]struct{})
	result, err := storeref.Walk(plan, func(leg storeref.Leg) (bool, error) {
		wa := workAssignmentForStore(beads.WorkStore{Store: leg.Store})
		for _, status := range []string{"open", "in_progress"} {
			for _, assignee := range identifiers {
				work, listErr := wa.OpenAssignedTo(assignee, status, beads.TierBoth, true)
				if listErr != nil {
					return false, fmt.Errorf("list %s work assigned to session %s via %q: %w", status, sessionID, assignee, listErr)
				}
				for _, item := range work {
					if session.IsSessionBeadOrRepairable(item) || !attemptevidence.IsExecutionRecord(item) {
						continue
					}
					key := string(leg.Ref) + "\x00" + item.ID
					if _, ok := seen[key]; ok {
						continue
					}
					seen[key] = struct{}{}
					if captureErr := captureWorkbenchAttemptEvidenceFromStores(ctx, leg.Store, store, item, dirs, cityPath, cfg); captureErr != nil {
						return false, fmt.Errorf("capture assigned execution %s before session %s retirement: %w", item.ID, sessionID, captureErr)
					}
				}
			}
		}
		return false, nil
	})
	if err != nil {
		return fmt.Errorf("scan assigned executions for session %s: %w", sessionID, err)
	}
	if result.Partial {
		return fmt.Errorf("scan assigned executions for session %s was incomplete", sessionID)
	}
	return nil
}

func captureControlAttemptEvidence(ctx context.Context, store beads.Store, cityPath, storePath string, cfg *config.City, control, attempt beads.Bead, attemptNum int, outcome string) error {
	kind := attemptevidence.KindRetry
	if control.Metadata[beadmeta.KindMetadataKey] == beadmeta.KindRalph {
		kind = attemptevidence.KindRalph
	}
	workDir := firstNonBlankEvidenceValue(
		attempt.Metadata[beadmeta.WorkDirMetadataKey], attempt.Metadata[beadmeta.LegacyWorkDirMetadataKey],
		control.Metadata[beadmeta.WorkDirMetadataKey], control.Metadata[beadmeta.LegacyWorkDirMetadataKey],
	)
	if workDir != "" && !filepath.IsAbs(workDir) {
		workDir = filepath.Join(resolveStoreScopeRoot(cityPath, storePath), workDir)
	}
	baseSHA := firstNonBlankEvidenceValue(
		attempt.Metadata[beadmeta.WorktreeBaseSHAMetadataKey],
		control.Metadata[beadmeta.WorktreeBaseSHAMetadataKey],
	)
	storeRef, err := attemptEvidenceStoreRef(cityPath, storePath, cfg, control)
	if err != nil {
		return err
	}
	identity := attemptevidence.Identity{
		Kind: kind, OwnerBeadID: control.ID, ExecutionBeadID: attempt.ID,
	}
	spec := attemptevidence.CaptureSpec{
		Identity: identity, StoreRef: storeRef, WorkDir: workDir,
		Permission: attemptevidence.PermissionScope{
			StoreRef: storeRef, WorkID: control.ID, WorkspaceRoot: workDir,
		},
		BaseSHA: baseSHA, Outcome: fmt.Sprintf("attempt_%d:%s", attemptNum, outcome),
	}
	if _, err := attemptevidence.Capture(ctx, store, spec); err != nil {
		return fmt.Errorf("capture %s control %s execution %s: %w", kind, control.ID, attempt.ID, err)
	}
	return nil
}

func attemptEvidenceStoreRef(cityPath, storePath string, cfg *config.City, control beads.Bead) (string, error) {
	if ref := strings.TrimSpace(control.Metadata[beadmeta.RootStoreRefMetadataKey]); ref != "" {
		return ref, nil
	}
	scopeRoot := resolveStoreScopeRoot(cityPath, storePath)
	if samePath(scopeRoot, cityPath) {
		cityName := config.EffectiveCityName(cfg, filepath.Base(cityPath))
		if strings.TrimSpace(cityName) == "" {
			return "", errors.New("cannot determine city scope for attempt evidence")
		}
		return "city:" + cityName, nil
	}
	if cfg != nil {
		for _, rig := range cfg.Rigs {
			if strings.TrimSpace(rig.Path) != "" && samePath(resolveStoreScopeRoot(cityPath, rig.Path), scopeRoot) {
				return "rig:" + rig.Name, nil
			}
		}
	}
	return "", fmt.Errorf("cannot resolve exact store reference for attempt evidence in %q", scopeRoot)
}

func firstNonBlankEvidenceValue(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}
