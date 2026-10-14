package main

import (
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/worklifecycle"
	"github.com/gastownhall/gascity/internal/worktree"
)

func TestLifecycleWorkspaceVerifierRequiresExactNativeCreation(t *testing.T) {
	repo, base := worktreeTestRepo(t)
	root := t.TempDir()
	spec := worktree.Spec{
		RepoDir: repo, Root: root, Path: filepath.Join(root, "source"),
		Branch: "work/source", Base: base, BeadID: "source-1", StoreRef: "rig:compute",
		Creator: "gc-worktree", Owner: "compute/worker", Generation: "1", Lifecycle: worktree.LifecycleActive,
	}
	report, err := worktree.Ensure(spec)
	if err != nil {
		t.Fatal(err)
	}
	spec.BaseSHA, spec.AttemptID = report.Provenance.BaseSHA, report.Provenance.AttemptID
	w := lifecycleSourceWorkspace(spec)
	source := beads.Bead{ID: spec.BeadID}
	admission := worklifecycle.AdmissionReceiptV2{WorkItemID: source.ID, Route: spec.Owner}
	policy := worklifecycle.AdmissionPolicyProjectionV2{}
	policy.Target.Identity = spec.Owner
	policy.StorePlacement.SourceStoreRef = spec.StoreRef
	verifier := newLifecycleNativeWorkspaceVerifier(spec)
	if err := verifier.VerifyNativeWorkspace(source, admission, policy, w); err != nil {
		t.Fatal(err)
	}
	for name, alter := range map[string]func(*worklifecycle.SourceWorkspace){
		"execution step substituted": func(w *worklifecycle.SourceWorkspace) { w.SourceID = "step-1" },
		"foreign store":              func(w *worklifecycle.SourceWorkspace) { w.StoreRef = "rig:other" },
		"new attempt":                func(w *worklifecycle.SourceWorkspace) { w.AttemptID = "another-attempt" },
		"new generation":             func(w *worklifecycle.SourceWorkspace) { w.Generation = "2" },
		"different repository":       func(w *worklifecycle.SourceWorkspace) { w.RepoDir = root },
		"different path":             func(w *worklifecycle.SourceWorkspace) { w.Path = repo },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := w
			alter(&candidate)
			if err := verifier.VerifyNativeWorkspace(source, admission, policy, candidate); err == nil {
				t.Fatal("different ownership accepted")
			}
		})
	}
	// Even matching caller fields cannot substitute a fabricated creation attempt.
	stale := spec
	stale.AttemptID = "fabricated-attempt"
	if err := newLifecycleNativeWorkspaceVerifier(stale).VerifyNativeWorkspace(source, admission, policy, lifecycleSourceWorkspace(stale)); err == nil {
		t.Fatal("matching fabricated tuple accepted without matching native attempt")
	}
	// Ensure is never performed by the publication verifier.
	absent := spec
	absent.Path = filepath.Join(root, "not-created")
	if err := newLifecycleNativeWorkspaceVerifier(absent).VerifyNativeWorkspace(source, admission, policy, lifecycleSourceWorkspace(absent)); err == nil {
		t.Fatal("uncreated workspace accepted")
	}
}
