package main

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/worktree"
)

func newLifecycleWorkspaceProviderFixture(t *testing.T, stepType ...string) (lifecycleAdmissionTransitionSetup, *controllerState, api.LifecycleWorkspaceTransitionRequest) {
	t.Helper()
	repo, base := worktreeTestRepo(t)
	formula := "formula = \"review\"\nversion = 1\n\n[[steps]]\nid = \"work\"\ntitle = \"Review work\"\n"
	if len(stepType) != 0 {
		formula += "type = \"" + stepType[0] + "\"\n"
	}
	fixture := newLifecycleAdmissionPolicyFixture(t, formula)
	fixture.cfg.Rigs[0].Path = repo
	setup, state, claim := newLifecycleClaimProviderFixtureForSetup(t, newLifecycleAdmissionTransitionSetupWithFixture(t, fixture, false), 0)
	root := t.TempDir()
	spec := worktree.Spec{
		RepoDir: repo, Root: root, Path: filepath.Join(root, "source"), Branch: "work/source", Base: base,
		BeadID: setup.source.ID, StoreRef: "rig:pilot", Creator: "gc-worktree", Owner: "pilot/worker", Generation: "1", Lifecycle: worktree.LifecycleActive,
	}
	report, err := worktree.Ensure(spec)
	if err != nil {
		t.Fatal(err)
	}
	spec.BaseSHA, spec.AttemptID = report.Provenance.BaseSHA, report.Provenance.AttemptID
	return setup, state, api.LifecycleWorkspaceTransitionRequest{
		Work: claim.Work, WorkStore: claim.WorkStore, WorkStoreRef: claim.WorkStoreRef, Scope: claim.Scope,
		Submission: api.LifecycleWorkspaceSubmitRequest{
			WorkID: claim.Work.ID, SourceStoreRef: "rig:pilot",
			ExpectedRevision: claim.ExpectedRevision, ExpectedTransitionHead: claim.ExpectedTransitionHead, Workspace: lifecycleSourceWorkspace(spec),
		},
	}
}

func TestLifecycleWorkspaceProviderPublishesOnceWithoutClaimOrGenericWrites(t *testing.T) {
	setup, state, request := newLifecycleWorkspaceProviderFixture(t)
	before := setup.store.sourceGenericWrites
	first, err := state.PublishLifecycleWorkspace(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	current, err := setup.store.Get(request.Work.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Status != "open" || current.Assignee != "" || current.Metadata[beadmeta.WorktreeAttemptMetadataKey] != request.Submission.Workspace.AttemptID || setup.store.sourceGenericWrites != before {
		t.Fatal("publication changed owner or omitted native ownership")
	}
	patches := len(setup.store.patchRequests)
	second, err := state.PublishLifecycleWorkspace(context.Background(), request)
	if err != nil || !second.Replayed || first.ReceiptID != second.ReceiptID || len(setup.store.patchRequests) != patches {
		t.Fatalf("exact replay: %+v %v", second, err)
	}
}

func TestLifecycleWorkspaceProviderRefusesChangedIntentBeforeMutation(t *testing.T) {
	for _, scenario := range []string{"foreign repo", "wrong attempt", "wrong owner", "step identity", "stale revision", "stale head", "held source", "routes closed"} {
		t.Run(scenario, func(t *testing.T) {
			setup, state, request := newLifecycleWorkspaceProviderFixture(t)
			switch scenario {
			case "foreign repo":
				request.Submission.Workspace.RepoDir = t.TempDir()
			case "wrong attempt":
				request.Submission.Workspace.AttemptID = "forged"
			case "wrong owner":
				request.Submission.Workspace.Owner = "other/worker"
			case "step identity":
				request.Submission.Workspace.SourceID = "step"
			case "stale revision":
				request.Submission.ExpectedRevision++
			case "stale head":
				request.Submission.ExpectedTransitionHead = "old"
			case "held source":
				row := setup.store.rows[request.Work.ID]
				row.Labels = append(row.Labels, beadmeta.DispatchHoldLabels[0])
				setup.store.rows[row.ID] = row
			case "routes closed":
				state.compatibilityRoutesClosed = true
			}
			patches, generic := len(setup.store.patchRequests), setup.store.sourceGenericWrites
			if _, err := state.PublishLifecycleWorkspace(context.Background(), request); err == nil {
				t.Fatal("changed publication accepted")
			}
			if len(setup.store.patchRequests) != patches || setup.store.sourceGenericWrites != generic {
				t.Fatal("refusal mutated source")
			}
		})
	}
}
