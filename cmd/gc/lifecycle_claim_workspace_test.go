package main

import (
	"context"
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worklifecycle"
)

func lifecycleClaimPublishedWorkspaceFixture(t *testing.T, stepType ...string) (lifecycleAdmissionTransitionSetup, *controllerState, api.LifecycleClaimTransitionRequest) {
	t.Helper()
	setup, state, publication := newLifecycleWorkspaceProviderFixture(t, stepType...)
	if _, err := state.PublishLifecycleWorkspace(context.Background(), publication); err != nil {
		t.Fatal(err)
	}
	source, err := setup.store.Get(publication.Work.ID)
	if err != nil {
		t.Fatal(err)
	}
	metadata := map[string]string{"template": "pilot/worker", "agent_name": "pilot/worker", "generation": "1", "instance_token": "workspace-claim", "session_key": "original-thread"}
	for k, p := range publication.Submission.Workspace.PublicationPatch().Metadata {
		metadata[k] = p.Value
	}
	info, err := sessionFrontDoor(setup.cityStore).CreateSessionInfo(session.CreateSpec{Title: "workspace worker", AgentName: "pilot/worker", Metadata: metadata})
	if err != nil {
		t.Fatal(err)
	}
	return setup, state, api.LifecycleClaimTransitionRequest{Work: source, WorkStore: setup.store, WorkStoreRef: publication.WorkStoreRef, Scope: publication.Scope, Session: info, Actor: session.AssigneeIdentifier(info), ExpectedRevision: source.Revision, ExpectedTransitionHead: source.Metadata[beadmeta.LifecycleTransitionHeadMetadataKey]}
}

func TestLifecycleClaimWorkspaceRequiresExactSessionCustodyBeforeProtectedWrite(t *testing.T) {
	for _, key := range []string{beadmeta.WorktreeSourceBeadMetadataKey, beadmeta.WorktreeSourceStoreMetadataKey, beadmeta.WorktreeAttemptMetadataKey, beadmeta.WorktreeOwnerMetadataKey, "session_key"} {
		t.Run(key, func(t *testing.T) {
			setup, state, request := lifecycleClaimPublishedWorkspaceFixture(t)
			if err := setup.cityStore.SetMetadata(request.Session.ID, key, "changed"); err != nil {
				t.Fatal(err)
			}
			before := len(setup.store.patchRequests)
			if _, err := state.ClaimLifecycleWork(context.Background(), request); err == nil {
				t.Fatal("changed session custody accepted")
			}
			if len(setup.store.patchRequests) != before {
				t.Fatal("refusal wrote protected work")
			}
		})
	}
}

func TestLifecycleClaimWorkspaceSourceAndDescendantUseOriginalNativeCreation(t *testing.T) {
	for _, descendant := range []bool{false, true} {
		t.Run(map[bool]string{false: "source", true: "step"}[descendant], func(t *testing.T) {
			setup, state, request := lifecycleClaimPublishedWorkspaceFixture(t, "bug")
			if descendant {
				marker, ok := lifecycleMaterializationFor(request.Work)
				if !ok {
					t.Fatal("missing lineage")
				}
				request.Work = lifecycleAdmissionV1StepCandidate(t, setup, marker)
				request.ExpectedRevision = request.Work.Revision
				request.ExpectedTransitionHead = request.Work.Metadata[beadmeta.LifecycleTransitionHeadMetadataKey]
			}
			first, err := state.ClaimLifecycleWork(context.Background(), request)
			if err != nil {
				t.Fatal(err)
			}
			second, err := state.ClaimLifecycleWork(context.Background(), request)
			if err != nil || !second.Replayed || first.ReceiptID != second.ReceiptID {
				t.Fatalf("exact retry: %+v %v", second, err)
			}
		})
	}
}

func TestLifecycleClaimWorkspaceRejectsDriftAfterDurableTransitionWithoutRollback(t *testing.T) {
	setup, state, request := lifecycleClaimPublishedWorkspaceFixture(t)
	setup.store.afterNextPatch = func(string) {
		if err := setup.cityStore.SetMetadata(request.Session.ID, beadmeta.WorktreeAttemptMetadataKey, "replacement"); err != nil {
			t.Fatal(err)
		}
	}
	before := len(setup.store.patchRequests)
	if _, err := state.ClaimLifecycleWork(context.Background(), request); err == nil {
		t.Fatal("post-commit custody drift reported success")
	}
	current, err := beads.HandlesFor(setup.store).Live.Get(request.Work.ID)
	if err != nil || current.Status != "in_progress" || len(setup.store.patchRequests) != before+1 {
		t.Fatalf("durable claim lost: %+v %v", current, err)
	}
}

// Default formula children are infrastructure steps, excluded by normal Ready.
// Publishing native custody must not turn one into eligible work.
func TestLifecycleClaimWorkspaceDoesNotMakeInfrastructureStepReady(t *testing.T) {
	setup, state, request := lifecycleClaimPublishedWorkspaceFixture(t)
	marker, _ := lifecycleMaterializationFor(request.Work)
	request.Work = lifecycleAdmissionV1StepCandidate(t, setup, marker)
	if request.Work.Type != "step" {
		t.Fatalf("expected infrastructure step, got %q", request.Work.Type)
	}
	request.ExpectedRevision = request.Work.Revision
	request.ExpectedTransitionHead = request.Work.Metadata[beadmeta.LifecycleTransitionHeadMetadataKey]
	before := len(setup.store.patchRequests)
	if _, err := state.ClaimLifecycleWork(context.Background(), request); !errors.Is(err, worklifecycle.ErrTransitionChainStale) {
		t.Fatalf("unready infrastructure claim: %v", err)
	}
	if len(setup.store.patchRequests) != before {
		t.Fatal("unready step caused a protected write")
	}
}
