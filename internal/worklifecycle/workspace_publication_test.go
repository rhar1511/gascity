package worklifecycle

import (
	"errors"
	"maps"
	"reflect"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

func workspacePublicationFixture(t *testing.T) (*transitionChainFixture, TransitionRequest, SourceWorkspace) {
	t.Helper()
	f := newTransitionChainFixture(t)
	parent := applyTestAttachment(t, f, applyTestReservation(t, f))
	w := SourceWorkspace{
		SourceID: f.work.ID, StoreRef: f.policy.StorePlacement.SourceStoreRef,
		Path: "/tmp/work", RepoDir: "/tmp/repo", Root: "/tmp", Branch: "work-branch",
		BaseRef: "main", BaseSHA: "1111111111111111111111111111111111111111",
		Creator: "gc-worktree", Owner: f.policy.Target.Identity, Generation: "generation-1",
		Lifecycle: "active", AttemptID: "native-attempt-1",
	}
	return f, TransitionRequest{
		IssueID: f.work.ID, Step: TransitionStepWorkspacePublication, OperationID: "workspace-1",
		PriorReceiptID: parent.Receipt.ReceiptID, Evidence: f.evidence, Patch: w.PublicationPatch(),
	}, w
}

func TestWorkspacePublicationRequiresNativeProofAndPreservesExactRetry(t *testing.T) {
	f, request, workspace := workspacePublicationFixture(t)
	calls := 0
	f.chain.workspaceVerifier = NativeWorkspaceVerifierFunc(func(source beads.Bead, admission AdmissionReceiptV2, policy AdmissionPolicyProjectionV2, got SourceWorkspace) error {
		calls++
		if source.ID != workspace.SourceID || admission.WorkItemID != source.ID || policy.Target.Identity != workspace.Owner || got != workspace {
			t.Fatalf("native verifier received different source or ownership: %+v", got)
		}
		return nil
	})
	result, err := f.chain.Apply(request)
	if err != nil {
		t.Fatal(err)
	}
	for key, change := range request.Patch.Metadata {
		if f.store.source.Metadata[key] != change.Value {
			t.Errorf("published %s = %q, want %q", key, f.store.source.Metadata[key], change.Value)
		}
	}
	if f.store.source.Status != "open" || f.store.source.Assignee != "" {
		t.Fatal("workspace publication claimed the source")
	}
	// Historical receipt replay must not require a still-present filesystem.
	f.chain.workspaceVerifier = nil
	replay, err := f.chain.Apply(request)
	if err != nil || !replay.Replayed || replay.Receipt.ReceiptID != result.Receipt.ReceiptID || calls != 1 {
		t.Fatalf("exact replay = %+v, %v; verifier calls=%d", replay, err, calls)
	}
	if _, err := f.chain.CurrentHead(f.work.ID, f.evidence); err != nil {
		t.Fatalf("published workspace chain: %v", err)
	}
}

func TestWorkspacePublicationRefusesBeforePermitOrWrite(t *testing.T) {
	cases := map[string]func(*transitionChainFixture, *TransitionRequest){
		"missing native verifier": func(f *transitionChainFixture, _ *TransitionRequest) { f.chain.workspaceVerifier = nil },
		"native verification failure": func(f *transitionChainFixture, _ *TransitionRequest) {
			f.chain.workspaceVerifier = NativeWorkspaceVerifierFunc(func(beads.Bead, AdmissionReceiptV2, AdmissionPolicyProjectionV2, SourceWorkspace) error {
				return errors.New("provenance attempt differs")
			})
		},
		"missing attempt": func(_ *transitionChainFixture, r *TransitionRequest) {
			delete(r.Patch.Metadata, beadmeta.WorktreeAttemptMetadataKey)
		},
		"different source": func(_ *transitionChainFixture, r *TransitionRequest) {
			r.Patch.Metadata[beadmeta.WorktreeSourceBeadMetadataKey] = MetadataStringPatch{Value: "other"}
		},
		"different store": func(_ *transitionChainFixture, r *TransitionRequest) {
			r.Patch.Metadata[beadmeta.WorktreeSourceStoreMetadataKey] = MetadataStringPatch{Value: "rig:other"}
		},
		"different owner": func(_ *transitionChainFixture, r *TransitionRequest) {
			r.Patch.Metadata[beadmeta.WorktreeOwnerMetadataKey] = MetadataStringPatch{Value: "other/worker"}
		},
		"partial adoption": func(_ *transitionChainFixture, r *TransitionRequest) {
			old := "/tmp/old"
			r.Patch.Metadata[beadmeta.WorkDirMetadataKey] = MetadataStringPatch{Expected: &old, Value: "/tmp/work"}
		},
		"legacy path differs": func(_ *transitionChainFixture, r *TransitionRequest) {
			r.Patch.Metadata[beadmeta.LegacyWorkDirMetadataKey] = MetadataStringPatch{Value: "/tmp/other"}
		},
		"extra authority": func(_ *transitionChainFixture, r *TransitionRequest) {
			r.Patch.Metadata[beadmeta.SessionIDMetadataKey] = MetadataStringPatch{Value: "session"}
		},
		"changes status": func(_ *transitionChainFixture, r *TransitionRequest) {
			r.Patch.Status = &StringTransition{Expected: "open", Value: "in_progress"}
		},
		"preexisting custody": func(f *transitionChainFixture, _ *TransitionRequest) {
			f.store.source.Metadata[beadmeta.WorkDirMetadataKey] = "/tmp/existing"
		},
		"source already assigned": func(f *transitionChainFixture, _ *TransitionRequest) { f.store.source.Assignee = "other" },
		"source revision changed": func(f *transitionChainFixture, _ *TransitionRequest) { f.store.source.Revision++ },
	}
	for name, alter := range cases {
		t.Run(name, func(t *testing.T) {
			f, request, _ := workspacePublicationFixture(t)
			f.chain.workspaceVerifier = NativeWorkspaceVerifierFunc(func(beads.Bead, AdmissionReceiptV2, AdmissionPolicyProjectionV2, SourceWorkspace) error { return nil })
			alter(f, &request)
			permits, writes := len(f.permits.requests), len(f.store.patchRequests)
			before := maps.Clone(f.store.source.Metadata)
			if _, err := f.chain.Apply(request); err == nil {
				t.Fatal("unsafe workspace publication accepted")
			}
			if len(f.permits.requests) != permits || len(f.store.patchRequests) != writes || !reflect.DeepEqual(before, f.store.source.Metadata) {
				t.Fatal("refused publication issued a permit or changed source")
			}
		})
	}
}

func TestWorkspacePublicationClaimPreservesPublishedPath(t *testing.T) {
	for _, mismatch := range []bool{false, true} {
		t.Run(map[bool]string{false: "matching", true: "different path"}[mismatch], func(t *testing.T) {
			f, request, w := workspacePublicationFixture(t)
			f.chain.workspaceVerifier = NativeWorkspaceVerifierFunc(func(beads.Bead, AdmissionReceiptV2, AdmissionPolicyProjectionV2, SourceWorkspace) error { return nil })
			published, err := f.chain.Apply(request)
			if err != nil {
				t.Fatal(err)
			}
			path := w.Path
			if mismatch {
				path = "/tmp/replacement"
			}
			claim := ClaimIdentityRequest{
				IssueID: f.work.ID, SessionID: "session-1", InstanceToken: "instance-1", RuntimeEpoch: "1",
				ExpectedRevision: published.Receipt.ToVersion, ExpectedTransitionHead: published.Receipt.ReceiptID,
				WorkDir: path, WorkBranch: w.Branch, Evidence: f.evidence,
			}
			claimed, err := f.chain.ClaimIdentity(claim)

			if mismatch {
				if err == nil || f.store.source.Status != "open" {
					t.Fatal("claim replaced published workspace")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if f.store.source.Metadata[beadmeta.WorkDirMetadataKey] != w.Path {
				t.Fatal("claim lost path")
			}
			replay, replayErr := f.chain.ClaimIdentity(claim)
			if replayErr != nil || !replay.Replayed || replay.Receipt.ReceiptID != claimed.Receipt.ReceiptID {
				t.Fatalf("published claim retry: %+v %v", replay, replayErr)
			}
			if f.store.source.Metadata[beadmeta.WorkBranchMetadataKey] != w.Branch {
				t.Fatal("claim lost branch")
			}
			if _, err := f.chain.CurrentHead(f.work.ID, f.evidence); err != nil {
				t.Fatal(err)
			}
		})
	}
}
