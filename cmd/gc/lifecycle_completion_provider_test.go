package main

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/worklifecycle"
	"github.com/gastownhall/gascity/internal/workrecord"
)

func newLifecycleCompletionProviderFixture(t *testing.T) (lifecycleAdmissionTransitionSetup, *controllerState, api.LifecycleCompletionTransitionRequest) {
	t.Helper()
	setup, state, claim := newLifecycleClaimProviderFixture(t)
	setup.fixture.cfg.Lifecycle.CompletionReceiptMaxAge = "24h"
	setup.fixture.cfg.Lifecycle.CompletionClockSkew = "2m"
	if _, err := state.ClaimLifecycleWork(context.Background(), claim); err != nil {
		t.Fatal(err)
	}
	work, err := setup.store.Get(setup.source.ID)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := worklifecycle.AdmissionDigestV2(setup.receipt)
	if err != nil {
		t.Fatal(err)
	}
	signed, err := worklifecycle.SignCompletionReceipt(worklifecycle.CompletionReceipt{
		Version: 1, WorkItemID: work.ID, Scope: claim.Scope, AdmissionDigest: digest,
		DeliverableRef: "commit:reviewed", VerificationRef: "checks:passed", AcceptedBy: "reviewer",
		AcceptedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}, setup.acceptancePrivate)
	if err != nil {
		t.Fatal(err)
	}
	return setup, state, api.LifecycleCompletionTransitionRequest{
		Work: work, WorkStore: setup.store, WorkStoreRef: claim.WorkStoreRef, Scope: claim.Scope,
		Submission: api.LifecycleCompletionSubmitRequest{
			WorkID: work.ID, SourceStoreRef: "rig:pilot", ExpectedRevision: work.Revision,
			ExpectedTransitionHead: work.Metadata[beadmeta.LifecycleTransitionHeadMetadataKey], CompletionReceipt: signed,
		},
	}
}

func TestLifecycleCompletionProviderCommitsAndRecoversExactAcceptance(t *testing.T) {
	for _, mode := range []string{"legacy", "normal", "lost budget response", "failed close"} {
		t.Run(mode, func(t *testing.T) {
			setup, state, request := newLifecycleCompletionProviderFixture(t)
			wantBranch := request.Work.Metadata[beadmeta.WorkBranchMetadataKey]
			wantOutcome := "no-op"
			if mode != "legacy" {
				var receipt worklifecycle.CompletionReceipt
				if err := json.Unmarshal([]byte(request.Submission.CompletionReceipt), &receipt); err != nil {
					t.Fatal(err)
				}
				receipt.WorkRecord = &workrecord.Record{Outcome: "no-op", Verification: "existing behavior verified"}
				if mode == "lost budget response" || mode == "failed close" {
					wantBranch, wantOutcome = "reviewed-branch", "shipped"
					receipt.WorkRecord.Outcome = wantOutcome
					receipt.WorkRecord.Commit = strings.Repeat("a", 40)
					receipt.WorkRecord.Branch = wantBranch
				}
				signed, err := worklifecycle.SignCompletionReceipt(receipt, setup.acceptancePrivate)
				if err != nil {
					t.Fatal(err)
				}
				request.Submission.CompletionReceipt = signed
			}
			provider, ok := any(state).(api.LifecycleCompletionTransitionProvider)
			if !ok {
				t.Fatal("running controller has no protected completion intake")
			}
			if mode == "lost budget response" {
				setup.store.loseNextPatchKind = lifecycleCompletionBudgetKind
			}
			if mode == "failed close" {
				setup.store.failNextPatchKind = lifecycleCloseKind
			}
			result, err := provider.CompleteLifecycleWork(context.Background(), request)
			if mode == "failed close" {
				if err == nil {
					t.Fatal("injected close failure was not surfaced")
				}
				pending, getErr := setup.store.Get(request.Work.ID)
				if getErr != nil || pending.Status != "in_progress" || typedLifecycleReceiptCount(setup.store, lifecycleCompletionBudgetKind) != 1 {
					t.Fatal("failed close lost committed acceptance budget")
				}
				result, err = provider.CompleteLifecycleWork(context.Background(), request)
			}
			if err != nil {
				t.Fatalf("protected completion: %v", err)
			}
			closed, err := setup.store.Get(request.Work.ID)
			if err != nil || closed.Status != "closed" || closed.Revision != result.Revision || result.WorkID != closed.ID || result.ReceiptID != closed.Metadata[beadmeta.LifecycleTransitionHeadMetadataKey] || closed.Metadata[beadmeta.LifecycleCompletionReceiptMetadataKey] != request.Submission.CompletionReceipt {
				t.Fatalf("completion did not commit exact accepted source: result=%+v err=%v", result, err)
			}
			if mode != "legacy" && (closed.Metadata[beadmeta.WorkOutcomeMetadataKey] != wantOutcome || closed.Metadata[beadmeta.WorkVerificationMetadataKey] != "existing behavior verified") {
				t.Fatal("completion lost the signed work record")
			}
			if closed.Metadata[beadmeta.WorkBranchMetadataKey] != wantBranch || wantOutcome == "shipped" && closed.Metadata[beadmeta.WorkCommitMetadataKey] != strings.Repeat("a", 40) {
				t.Fatal("completion did not preserve the accepted branch and commit")
			}
			writes := len(setup.store.patchRequests)
			// An already-closed retry must remain read-only even if policy now
			// enforces records or its mutable Git references no longer resolve.
			t.Setenv(workrecord.EnforceEnvVar, "true")
			replay, err := provider.CompleteLifecycleWork(context.Background(), request)
			after, getErr := setup.store.Get(request.Work.ID)
			if err != nil || getErr != nil || !replay.Replayed || replay.ReceiptID != result.ReceiptID || !reflect.DeepEqual(closed, after) || len(setup.store.patchRequests) != writes {
				t.Fatalf("completion retry changed committed work: %+v err=%v", replay, err)
			}
			if setup.store.sourceGenericWrites != 0 || typedLifecycleReceiptCount(setup.store, lifecycleCompletionBudgetKind) != 1 || typedLifecycleReceiptCount(setup.store, lifecycleCloseKind) != 1 {
				t.Fatal("completion did not use exactly one protected budget and close")
			}
		})
	}
}

func TestLifecycleCompletionProviderRefusesInvalidIntakeBeforeWrites(t *testing.T) {
	for _, name := range []string{"wrong revision", "wrong head", "wrong scope", "wrong store ref", "wrong store handle", "malformed receipt", "wrong work", "wrong admission", "expired acceptance", "forged acceptance", "forged work record", "closed routes", "canceled context"} {
		t.Run(name, func(t *testing.T) {
			setup, state, request := newLifecycleCompletionProviderFixture(t)
			ctx := context.Background()
			switch name {
			case "wrong revision":
				request.Submission.ExpectedRevision++
			case "wrong head":
				request.Submission.ExpectedTransitionHead = "other-head"
			case "wrong scope":
				request.Scope = "city:other/rig:pilot"
			case "wrong store ref":
				request.Submission.SourceStoreRef = "rig:other"
			case "wrong store handle":
				request.WorkStore = beads.NewMemStore()
			case "malformed receipt":
				request.Submission.CompletionReceipt = "invalid"
			case "closed routes":
				state.compatibilityRoutesClosed = true
			case "canceled context":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			default:
				var receipt worklifecycle.CompletionReceipt
				if err := json.Unmarshal([]byte(request.Submission.CompletionReceipt), &receipt); err != nil {
					t.Fatal(err)
				}
				switch name {
				case "wrong work":
					receipt.WorkItemID = "other-work"
				case "wrong admission":
					receipt.AdmissionDigest = strings.Repeat("x", 43)
				case "expired acceptance":
					receipt.AcceptedAt = time.Now().Add(-48 * time.Hour).UTC().Format(time.RFC3339Nano)
				case "forged acceptance":
					receipt.DeliverableRef = "commit:unreviewed"
				case "forged work record":
					receipt.WorkRecord = &workrecord.Record{Outcome: "no-op"}
				}
				if name == "forged acceptance" || name == "forged work record" {
					raw, err := json.Marshal(receipt)
					if err != nil {
						t.Fatal(err)
					}
					request.Submission.CompletionReceipt = string(raw)
				} else {
					receipt.Signature = ""
					encoded, err := worklifecycle.SignCompletionReceipt(receipt, setup.acceptancePrivate)
					if err != nil {
						t.Fatal(err)
					}
					request.Submission.CompletionReceipt = encoded
				}
			}
			before, err := setup.store.Get(setup.source.ID)
			if err != nil {
				t.Fatal(err)
			}
			writes := len(setup.store.patchRequests)
			if _, err := state.CompleteLifecycleWork(ctx, request); err == nil {
				t.Fatal("invalid completion intake succeeded")
			}
			after, err := setup.store.Get(setup.source.ID)
			if err != nil || !reflect.DeepEqual(before, after) || len(setup.store.patchRequests) != writes || setup.store.sourceGenericWrites != 0 {
				t.Fatal("invalid completion intake mutated work")
			}
		})
	}
}

func TestLifecycleCompletionProviderHonorsWorkRecordEnforcement(t *testing.T) {
	for _, tc := range []struct {
		name    string
		enforce string
		record  bool
		refuse  bool
	}{
		{name: "warn-only legacy", enforce: "false"},
		{name: "enforced missing record", enforce: "true", refuse: true},
		{name: "enforced no-op", enforce: "true", record: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(workrecord.EnforceEnvVar, tc.enforce)
			setup, state, request := newLifecycleCompletionProviderFixture(t)
			if tc.record {
				var receipt worklifecycle.CompletionReceipt
				if err := json.Unmarshal([]byte(request.Submission.CompletionReceipt), &receipt); err != nil {
					t.Fatal(err)
				}
				receipt.WorkRecord = &workrecord.Record{Outcome: "no-op"}
				encoded, err := worklifecycle.SignCompletionReceipt(receipt, setup.acceptancePrivate)
				if err != nil {
					t.Fatal(err)
				}
				request.Submission.CompletionReceipt = encoded
			}
			before, err := setup.store.Get(request.Work.ID)
			if err != nil {
				t.Fatal(err)
			}
			writes := len(setup.store.patchRequests)
			_, err = state.CompleteLifecycleWork(context.Background(), request)
			after, readErr := setup.store.Get(request.Work.ID)
			if tc.refuse {
				if !errors.Is(err, worklifecycle.ErrTransitionChainEvidence) || readErr != nil || !reflect.DeepEqual(before, after) || len(setup.store.patchRequests) != writes {
					t.Fatalf("work-record enforcement did not refuse before lifecycle writes: %v", err)
				}
			} else if err != nil || readErr != nil || after.Status != "closed" {
				t.Fatalf("permitted work record did not close: %v, %v", err, readErr)
			}
		})
	}
}
