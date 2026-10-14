package worklifecycle

import (
	"errors"
	"fmt"
	"path/filepath"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// SourceWorkspace is the complete native ownership returned by worktree
// creation. Publication does not create or adopt a directory: a trusted native
// verifier must independently verify every field before the protected write.
type SourceWorkspace struct {
	SourceID   string `json:"source_id"`
	StoreRef   string `json:"store_ref"`
	Path       string `json:"path"`
	RepoDir    string `json:"repo_dir"`
	Root       string `json:"root"`
	Branch     string `json:"branch"`
	BaseRef    string `json:"base_ref"`
	BaseSHA    string `json:"base_sha"`
	Creator    string `json:"creator"`
	Owner      string `json:"owner"`
	Generation string `json:"generation"`
	Lifecycle  string `json:"lifecycle"`
	AttemptID  string `json:"attempt_id"`
}

// Validate checks the complete canonical tuple without asserting provenance.
func (w SourceWorkspace) Validate() error {
	_, err := sourceWorkspaceFromPatch(w.PublicationPatch())
	return err
}

// PublicationPatch compares every custody field against absence. Existing
// ownership requires a separate exact-owner recovery, never ordinary adoption.
func (w SourceWorkspace) PublicationPatch() SourceWorkPatch {
	values := map[string]string{
		beadmeta.WorktreeSourceBeadMetadataKey: w.SourceID, beadmeta.WorktreeSourceStoreMetadataKey: w.StoreRef,
		beadmeta.WorkDirMetadataKey: w.Path, beadmeta.LegacyWorkDirMetadataKey: w.Path,
		beadmeta.WorktreeRepoMetadataKey: w.RepoDir, beadmeta.WorktreeRootMetadataKey: w.Root,
		beadmeta.WorkBranchMetadataKey: w.Branch, beadmeta.WorktreeBaseRefMetadataKey: w.BaseRef,
		beadmeta.WorktreeBaseSHAMetadataKey: w.BaseSHA, beadmeta.WorktreeCreatorMetadataKey: w.Creator,
		beadmeta.WorktreeOwnerMetadataKey: w.Owner, beadmeta.WorktreeGenerationMetadataKey: w.Generation,
		beadmeta.WorktreeLifecycleMetadataKey: w.Lifecycle, beadmeta.WorktreeAttemptMetadataKey: w.AttemptID,
	}
	patch := SourceWorkPatch{Metadata: make(map[string]MetadataStringPatch, len(values))}
	for key, value := range values {
		patch.Metadata[key] = MetadataStringPatch{Value: value}
	}
	return patch
}

// NativeWorkspaceVerifier resolves genuine native provenance against trusted
// controller configuration. It must verify the exact source, store, configured
// repository, owner, generation, path and creation attempt without writing any
// provenance. A caller-supplied tuple alone is not evidence.
type NativeWorkspaceVerifier interface {
	VerifyNativeWorkspace(beads.Bead, AdmissionReceiptV2, AdmissionPolicyProjectionV2, SourceWorkspace) error
}

// NativeWorkspaceVerifierFunc adapts the trusted controller verification seam.
type NativeWorkspaceVerifierFunc func(beads.Bead, AdmissionReceiptV2, AdmissionPolicyProjectionV2, SourceWorkspace) error

// VerifyNativeWorkspace verifies native ownership through f.
func (f NativeWorkspaceVerifierFunc) VerifyNativeWorkspace(source beads.Bead, admission AdmissionReceiptV2, policy AdmissionPolicyProjectionV2, workspace SourceWorkspace) error {
	return f(source, admission, policy, workspace)
}

func sourceWorkspaceFromPatch(p SourceWorkPatch) (SourceWorkspace, error) {
	value := func(key string) string { return p.Metadata[key].Value }
	w := SourceWorkspace{
		SourceID: value(beadmeta.WorktreeSourceBeadMetadataKey), StoreRef: value(beadmeta.WorktreeSourceStoreMetadataKey),
		Path: value(beadmeta.WorkDirMetadataKey), RepoDir: value(beadmeta.WorktreeRepoMetadataKey), Root: value(beadmeta.WorktreeRootMetadataKey),
		Branch: value(beadmeta.WorkBranchMetadataKey), BaseRef: value(beadmeta.WorktreeBaseRefMetadataKey), BaseSHA: value(beadmeta.WorktreeBaseSHAMetadataKey),
		Creator: value(beadmeta.WorktreeCreatorMetadataKey), Owner: value(beadmeta.WorktreeOwnerMetadataKey), Generation: value(beadmeta.WorktreeGenerationMetadataKey),
		Lifecycle: value(beadmeta.WorktreeLifecycleMetadataKey), AttemptID: value(beadmeta.WorktreeAttemptMetadataKey),
	}
	canonical := w.PublicationPatch()
	if p.Status != nil || p.Assignee != nil || len(p.Metadata) != len(canonical.Metadata) {
		return SourceWorkspace{}, fmt.Errorf("workspace publication requires exactly the complete custody tuple: %w", ErrTransitionChainInvalid)
	}
	for key, expected := range canonical.Metadata {
		actual, present := p.Metadata[key]
		if !present || actual.Expected != nil || !validCanonicalText(actual.Value) || actual.Value != expected.Value {
			return SourceWorkspace{}, fmt.Errorf("workspace custody %q must be canonical, complete and previously absent: %w", key, ErrTransitionChainInvalid)
		}
	}
	for _, path := range []string{w.Path, w.RepoDir, w.Root} {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path {
			return SourceWorkspace{}, fmt.Errorf("workspace path is not absolute and canonical: %w", ErrTransitionChainInvalid)
		}
	}
	return w, nil
}

func validateWorkspaceAdmission(patch SourceWorkPatch, issueID string, evidence verifiedTransitionEvidence) error {
	w, err := sourceWorkspaceFromPatch(patch)
	if err != nil {
		return err
	}
	if w.SourceID != issueID || w.SourceID != evidence.Admission.WorkItemID ||
		w.StoreRef != evidence.Policy.StorePlacement.SourceStoreRef || w.Owner != evidence.Policy.Target.Identity {
		return fmt.Errorf("workspace source, store or owner differs from admitted route: %w", ErrTransitionChainEvidence)
	}
	return nil
}

func (c *TransitionChain) verifyFreshWorkspace(source beads.Bead, evidence verifiedTransitionEvidence, patch SourceWorkPatch) error {
	if c.workspaceVerifier == nil {
		return fmt.Errorf("native workspace verifier unavailable: %w", ErrTransitionChainUnavailable)
	}
	if source.Status != "open" || source.Assignee != "" {
		return fmt.Errorf("workspace publication requires open unassigned source: %w", ErrTransitionChainStale)
	}
	w, err := sourceWorkspaceFromPatch(patch)
	if err != nil {
		return err
	}
	if err := c.workspaceVerifier.VerifyNativeWorkspace(source, evidence.Admission, evidence.Policy, w); err != nil {
		return fmt.Errorf("verify native workspace: %w", errors.Join(ErrTransitionChainEvidence, err))
	}
	return nil
}

func validateWorkspaceClaim(patch SourceWorkPatch, parent transitionParent) error {
	if parent.Step != TransitionStepWorkspacePublication {
		return nil
	}
	// Omission preserves the revision-fenced published value. A canonical
	// protected patch cannot contain a no-op assertion, and ordinary claims
	// must never change either part of this already published workspace.
	for _, key := range []string{beadmeta.WorkDirMetadataKey, beadmeta.WorkBranchMetadataKey} {
		if _, present := patch.Metadata[key]; present {
			return fmt.Errorf("claim must preserve published workspace %q: %w", key, ErrTransitionChainEvidence)
		}
	}
	return nil
}
