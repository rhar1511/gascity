package main

import (
	"fmt"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/worklifecycle"
	"github.com/gastownhall/gascity/internal/worktree"
)

func lifecycleSourceWorkspace(spec worktree.Spec) worklifecycle.SourceWorkspace {
	return worklifecycle.SourceWorkspace{
		SourceID: spec.BeadID, StoreRef: spec.StoreRef, Path: spec.Path, RepoDir: spec.RepoDir, Root: spec.Root,
		Branch: spec.Branch, BaseRef: spec.Base, BaseSHA: spec.BaseSHA, Creator: spec.Creator,
		Owner: spec.Owner, Generation: spec.Generation, Lifecycle: spec.Lifecycle, AttemptID: spec.AttemptID,
	}
}

// expected must come from the controller's exact source provisioning intent,
// with BaseSHA and AttemptID returned by native Ensure. Never construct that
// intent from execution-step work_dir or unowned session metadata.
func newLifecycleNativeWorkspaceVerifier(expected worktree.Spec) worklifecycle.NativeWorkspaceVerifier {
	return worklifecycle.NativeWorkspaceVerifierFunc(func(source beads.Bead, admission worklifecycle.AdmissionReceiptV2, policy worklifecycle.AdmissionPolicyProjectionV2, proposed worklifecycle.SourceWorkspace) error {
		if expected.DryRun || expected.AttemptID == "" || expected.BaseSHA == "" ||
			source.ID != expected.BeadID || admission.WorkItemID != source.ID ||
			admission.Route != expected.Owner || policy.Target.Identity != expected.Owner ||
			policy.StorePlacement.SourceStoreRef != expected.StoreRef || proposed != lifecycleSourceWorkspace(expected) {
			return fmt.Errorf("workspace publication differs from exact source provisioning intent")
		}
		report, err := worktree.Verify(expected)
		if err != nil {
			return err
		}
		if report.Provenance == nil || report.Provenance.AttemptID != expected.AttemptID || report.Path != expected.Path {
			return fmt.Errorf("native workspace creation attempt differs from publication intent")
		}
		return nil
	})
}
