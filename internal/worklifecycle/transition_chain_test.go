package worklifecycle

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

func TestTransitionChainAppliesEveryLifecycleStepWithExactPermitBinding(t *testing.T) {
	fixture := newTransitionChainFixture(t)
	steps := []struct {
		step      TransitionStep
		operation string
		patch     func(beads.Bead) SourceWorkPatch
	}{
		{step: TransitionStepReservation, operation: "reserve-1", patch: func(_ beads.Bead) SourceWorkPatch {
			return SourceWorkPatch{Metadata: map[string]MetadataStringPatch{
				beadmeta.LifecycleMaterializationMetadataKey: {Value: fixture.materializationValue("reserved", "")},
			}}
		}},
		{step: TransitionStepAttachedMaterialization, operation: "materialize-1", patch: func(bead beads.Bead) SourceWorkPatch {
			return fixture.attachedMaterializationPatch(bead)
		}},
		{step: TransitionStepClaimIdentity, operation: "claim-1", patch: func(_ beads.Bead) SourceWorkPatch {
			return claimIdentityTestPatch()
		}},
		{step: TransitionStepRecoveryBudget, operation: "recovery-1", patch: func(_ beads.Bead) SourceWorkPatch {
			return SourceWorkPatch{}
		}},
		{step: TransitionStepCompletionBudget, operation: "completion-1", patch: func(_ beads.Bead) SourceWorkPatch {
			return SourceWorkPatch{}
		}},
		{step: TransitionStepClose, operation: "close-1", patch: func(_ beads.Bead) SourceWorkPatch {
			return SourceWorkPatch{Status: &StringTransition{Expected: "in_progress", Value: "closed"}}
		}},
	}

	parentID := fixture.attachment.ReceiptID
	var previous beads.RevisionTransitionPatchReceipt
	for _, tc := range steps {
		current, err := fixture.store.DecisionFrontierSourceSnapshot(fixture.work.ID)
		if err != nil {
			t.Fatal(err)
		}
		request := TransitionRequest{
			IssueID: fixture.work.ID, Step: tc.step, OperationID: tc.operation, PriorReceiptID: parentID,
			Evidence: fixture.evidence, Patch: tc.patch(current),
		}
		switch tc.step {
		case TransitionStepRecoveryBudget:
			request = fixture.recoveryBudgetRequest(t, previous, tc.operation)
		case TransitionStepCompletionBudget:
			request = fixture.completionBudgetRequest(t, previous, fixture.completionReceiptValue(t, nil))
		case TransitionStepClose:
			request.CompletionReceipt = fixture.completionReceiptValue(t, nil)
		}
		got, err := fixture.chain.Apply(request)
		if err != nil {
			t.Fatalf("Apply(%s): %v", tc.step, err)
		}
		if got.Receipt.ReceiptID == "" || got.Receipt.Kind != transitionStepKind(tc.step) || got.Receipt.ExpectedVersion == got.Receipt.ToVersion || got.Receipt.ToVersion == 0 {
			t.Fatalf("Apply(%s) receipt = %+v, want deterministic kind and backend ToVersion", tc.step, got.Receipt)
		}
		assertTransitionHeadPatch(t, got.Receipt, tc.step, previous.ReceiptID)
		head, err := fixture.chain.CurrentHead(fixture.work.ID, fixture.evidence)
		currentHeadRow, readErr := fixture.store.DecisionFrontierSourceSnapshot(fixture.work.ID)
		if err != nil || readErr != nil || head.ReceiptID != got.Receipt.ReceiptID || head.ToVersion != currentHeadRow.Revision || currentHeadRow.Metadata[beadmeta.LifecycleTransitionHeadMetadataKey] != got.Receipt.ReceiptID {
			t.Fatalf("current head after %s = %+v row=%+v err=%v readErr=%v", tc.step, head, currentHeadRow, err, readErr)
		}
		if got.Replayed || got.Recovered {
			t.Fatalf("Apply(%s) flags = replayed %t recovered %t, want fresh transition", tc.step, got.Replayed, got.Recovered)
		}
		if tc.step == TransitionStepReservation {
			requestDigest, err := beads.RevisionTransitionPatchProtectedMutationDigest(fixture.work.ID, fixture.store.patchRequests[0])
			if err != nil {
				t.Fatal(err)
			}
			if len(fixture.permits.requests) != 1 {
				t.Fatalf("permit requests = %d, want one", len(fixture.permits.requests))
			}
			permitRequest := fixture.permits.requests[0]
			if permitRequest.request.Operation != beads.RevisionTransitionPatchProtectedMutationOperation ||
				!reflect.DeepEqual(permitRequest.request.ResourceIDs, []string{fixture.work.ID}) ||
				permitRequest.request.RequestDigest != requestDigest || permitRequest.replayID != got.Receipt.ReceiptID {
				t.Fatalf("permit request = %+v replay %q, want exact operation/resource/digest/receipt binding", permitRequest.request, permitRequest.replayID)
			}
		}
		parentID = got.Receipt.ReceiptID
		previous = got.Receipt
	}
	if len(fixture.store.patchRequests) != len(steps) {
		t.Fatalf("patch calls = %d, want %d", len(fixture.store.patchRequests), len(steps))
	}
	if fixture.policyResolver.calls != len(steps)*2 || len(fixture.workflowVerifier.calls) != 1 {
		t.Fatalf("proof calls: policy=%d workflow=%d, want policy for every apply/head read and one workflow verification", fixture.policyResolver.calls, len(fixture.workflowVerifier.calls))
	}
	if previous.Kind != transitionStepKind(TransitionStepClose) || previous.PriorReceiptID != fixture.store.patchRequests[len(steps)-2].ReceiptID {
		t.Fatalf("close receipt = %+v, want direct parent from completion budget", previous)
	}
	closed, err := fixture.store.DecisionFrontierSourceSnapshot(fixture.work.ID)
	if err != nil || closed.Status != "closed" {
		t.Fatalf("closed source = %+v err %v, want closed", closed, err)
	}
	beforeReplayWrites := len(fixture.store.patchRequests)
	replay, err := fixture.chain.Apply(fixture.firstReservationRequest())
	if err != nil || !replay.Replayed || len(fixture.store.patchRequests) != beforeReplayWrites ||
		closed.Metadata[beadmeta.LifecycleTransitionHeadMetadataKey] != previous.ReceiptID {
		t.Fatalf("historical reservation replay after later steps = %+v err %v writes=%d->%d", replay, err, beforeReplayWrites, len(fixture.store.patchRequests))
	}
}

func assertTransitionHeadPatch(t *testing.T, receipt beads.RevisionTransitionPatchReceipt, step TransitionStep, parentID string) {
	t.Helper()
	for _, change := range receipt.Patch.Metadata {
		if change.Key != beadmeta.LifecycleTransitionHeadMetadataKey {
			continue
		}
		var value string
		if change.Value == nil || json.Unmarshal(*change.Value, &value) != nil || value != receipt.ReceiptID {
			t.Fatalf("%s head value = %s, want its receipt ID %q", step, change.Value, receipt.ReceiptID)
		}
		if step == TransitionStepReservation {
			if change.Expected != nil {
				t.Fatalf("reservation head expected = %s, want absent", change.Expected)
			}
		} else {
			var expected string
			if change.Expected == nil || json.Unmarshal(*change.Expected, &expected) != nil || expected != parentID {
				t.Fatalf("%s head expected = %v, want parent %q", step, change.Expected, parentID)
			}
		}
		return
	}
	t.Fatalf("%s receipt omits service-owned transition head", step)
}

func TestTransitionChainCurrentHeadAndMutationCAS(t *testing.T) {
	t.Run("attachment is the initial head and restart discovers latest patch", func(t *testing.T) {
		fixture := newTransitionChainFixture(t)
		initial, err := fixture.chain.CurrentHead(fixture.work.ID, fixture.evidence)
		if err != nil || !initial.FromAttachment || initial.ReceiptID != fixture.attachment.ReceiptID || initial.ToVersion != fixture.attachment.ToRevision {
			t.Fatalf("initial head = %+v err %v, want Q43 attachment", initial, err)
		}
		reservation := applyTestReservation(t, fixture)
		claim := applyTestClaim(t, fixture, applyTestAttachment(t, fixture, reservation))
		restarted, err := NewTransitionChain(fixture.chainConfig())
		if err != nil {
			t.Fatal(err)
		}
		head, err := restarted.CurrentHead(fixture.work.ID, fixture.evidence)
		if err != nil || head.FromAttachment || head.ReceiptID != claim.Receipt.ReceiptID || head.ToVersion != claim.Receipt.ToVersion {
			t.Fatalf("restarted current head = %+v err %v, want latest claim receipt", head, err)
		}
	})

	t.Run("caller cannot choose service-owned head", func(t *testing.T) {
		fixture := newTransitionChainFixture(t)
		request := fixture.firstReservationRequest()
		request.Patch.Metadata[beadmeta.LifecycleTransitionHeadMetadataKey] = MetadataStringPatch{Value: "forged"}
		if _, err := fixture.chain.Apply(request); !errors.Is(err, ErrTransitionChainInvalid) {
			t.Fatalf("caller-supplied transition head error = %v, want invalid request", err)
		}
	})

	for _, tc := range []struct {
		name string
		set  func(beads.Bead) string
	}{
		{name: "missing", set: func(source beads.Bead) string {
			delete(source.Metadata, beadmeta.LifecycleTransitionHeadMetadataKey)
			return ""
		}},
		{name: "forged", set: func(source beads.Bead) string {
			source.Metadata[beadmeta.LifecycleTransitionHeadMetadataKey] = "forged-receipt"
			return "forged-receipt"
		}},
		{name: "old receipt", set: func(source beads.Bead) string {
			source.Metadata[beadmeta.LifecycleTransitionHeadMetadataKey] = ""
			return ""
		}},
	} {
		t.Run("reject "+tc.name+" head", func(t *testing.T) {
			fixture := newTransitionChainFixture(t)
			reservation := applyTestReservation(t, fixture)
			attached := applyTestAttachment(t, fixture, reservation)
			claim := applyTestClaim(t, fixture, attached)
			fixture.store.mu.Lock()
			if tc.name == "old receipt" {
				fixture.store.source.Metadata[beadmeta.LifecycleTransitionHeadMetadataKey] = attached.Receipt.ReceiptID
			} else {
				_ = tc.set(fixture.store.source)
			}
			fixture.store.mu.Unlock()
			if _, err := fixture.chain.CurrentHead(fixture.work.ID, fixture.evidence); !errors.Is(err, ErrTransitionChainStale) && !errors.Is(err, ErrTransitionChainReceipt) {
				t.Fatalf("CurrentHead with %s source head = %v, want stale/receipt refusal", tc.name, err)
			}
			request := fixture.recoveryBudgetRequest(t, claim.Receipt, "recovery-request-1")
			beforeWrites, beforePermits := len(fixture.store.patchRequests), len(fixture.permits.requests)
			if _, err := fixture.chain.Apply(request); !errors.Is(err, ErrTransitionChainStale) {
				t.Fatalf("Apply with %s source head = %v, want stale refusal", tc.name, err)
			}
			if len(fixture.store.patchRequests) != beforeWrites || len(fixture.permits.requests) != beforePermits {
				t.Fatalf("bad source head reached write/permit: %d->%d, %d->%d", beforeWrites, len(fixture.store.patchRequests), beforePermits, len(fixture.permits.requests))
			}
		})
	}
}

func TestTransitionChainRequiresExactAttachedAndClaimMetadataSets(t *testing.T) {
	fixture := newTransitionChainFixture(t)
	reservation := fixture.firstReservationRequest()
	reserved, err := fixture.chain.Apply(reservation)
	if err != nil {
		t.Fatal(err)
	}
	current, err := fixture.store.DecisionFrontierSourceSnapshot(fixture.work.ID)
	if err != nil {
		t.Fatal(err)
	}
	attached := TransitionRequest{
		IssueID: fixture.work.ID, Step: TransitionStepAttachedMaterialization, OperationID: "materialize-1",
		PriorReceiptID: reserved.Receipt.ReceiptID, Evidence: fixture.evidence,
		Patch: fixture.attachedMaterializationPatch(current),
	}
	t.Run("attached materialization omits route key", func(t *testing.T) {
		request := attached
		request.Patch = cloneSourceWorkPatch(attached.Patch)
		delete(request.Patch.Metadata, beadmeta.ExecutionRoutedToMetadataKey)
		if _, err := fixture.chain.Apply(request); !errors.Is(err, ErrTransitionChainInvalid) {
			t.Fatalf("Apply without route key = %v, want invalid patch", err)
		}
	})
	t.Run("attached materialization has extra key", func(t *testing.T) {
		request := attached
		request.Patch = cloneSourceWorkPatch(attached.Patch)
		request.Patch.Metadata[beadmeta.OutcomeMetadataKey] = MetadataStringPatch{Value: "extra"}
		if _, err := fixture.chain.Apply(request); !errors.Is(err, ErrTransitionChainInvalid) {
			t.Fatalf("Apply with extra key = %v, want invalid patch", err)
		}
	})
	attachedResult, err := fixture.chain.Apply(attached)
	if err != nil {
		t.Fatalf("Apply exact attached materialization: %v", err)
	}
	claim := TransitionRequest{
		IssueID: fixture.work.ID, Step: TransitionStepClaimIdentity, OperationID: "claim-1",
		PriorReceiptID: attachedResult.Receipt.ReceiptID, Evidence: fixture.evidence, Patch: claimIdentityTestPatch(),
	}
	t.Run("claim identity omits claimed_at", func(t *testing.T) {
		request := claim
		request.Patch = cloneSourceWorkPatch(claim.Patch)
		delete(request.Patch.Metadata, beadmeta.ClaimedAtMetadataKey)
		if _, err := fixture.chain.Apply(request); !errors.Is(err, ErrTransitionChainInvalid) {
			t.Fatalf("Apply without claimed_at = %v, want invalid patch", err)
		}
	})
	t.Run("claim identity has extra key", func(t *testing.T) {
		request := claim
		request.Patch = cloneSourceWorkPatch(claim.Patch)
		request.Patch.Metadata[beadmeta.OutcomeMetadataKey] = MetadataStringPatch{Value: "extra"}
		if _, err := fixture.chain.Apply(request); !errors.Is(err, ErrTransitionChainInvalid) {
			t.Fatalf("Apply with extra key = %v, want invalid patch", err)
		}
	})
	if _, err := fixture.chain.Apply(claim); err != nil {
		t.Fatalf("Apply exact claim identity: %v", err)
	}
}

func TestTransitionChainRejectsSkippedLifecyclePredecessors(t *testing.T) {
	t.Run("claim requires attached materialization", func(t *testing.T) {
		fixture := newTransitionChainFixture(t)
		request := TransitionRequest{
			IssueID: fixture.work.ID, Step: TransitionStepClaimIdentity, OperationID: "claim-1",
			PriorReceiptID: fixture.attachment.ReceiptID, Evidence: fixture.evidence, Patch: claimIdentityTestPatch(),
		}
		if _, err := fixture.chain.Apply(request); !errors.Is(err, ErrTransitionChainInvalid) {
			t.Fatalf("claim directly from Q43 = %v, want predecessor refusal", err)
		}
		if len(fixture.store.patchRequests) != 0 {
			t.Fatalf("skipped claim reached writer %d times, want zero", len(fixture.store.patchRequests))
		}
	})

	t.Run("recovery requires claim or recovery", func(t *testing.T) {
		fixture := newTransitionChainFixture(t)
		reservation := applyTestReservation(t, fixture)
		current, err := fixture.store.DecisionFrontierSourceSnapshot(fixture.work.ID)
		if err != nil {
			t.Fatal(err)
		}
		request := TransitionRequest{
			IssueID: fixture.work.ID, Step: TransitionStepRecoveryBudget, OperationID: "recovery-1",
			PriorReceiptID: reservation.Receipt.ReceiptID, Evidence: fixture.evidence,
			Patch: metadataTransition(current, beadmeta.LifecycleRecoveryStateMetadataKey, `{"version":1,"attempts":[]}`),
		}
		if _, err := fixture.chain.Apply(request); !errors.Is(err, ErrTransitionChainInvalid) {
			t.Fatalf("recovery directly from reservation = %v, want predecessor refusal", err)
		}
	})

	t.Run("completion budget requires claim or recovery", func(t *testing.T) {
		fixture := newTransitionChainFixture(t)
		attached := applyTestAttachment(t, fixture, applyTestReservation(t, fixture))
		request := TransitionRequest{
			IssueID: fixture.work.ID, Step: TransitionStepCompletionBudget, OperationID: "completion-1",
			PriorReceiptID: attached.Receipt.ReceiptID, Evidence: fixture.evidence,
			Patch: metadataTransition(beads.Bead{}, beadmeta.LifecycleCompletionBudgetMetadataKey, `{"version":1,"reserved":true}`),
		}
		if _, err := fixture.chain.Apply(request); !errors.Is(err, ErrTransitionChainInvalid) {
			t.Fatalf("completion budget directly from attachment = %v, want predecessor refusal", err)
		}
	})

	t.Run("close requires completion budget", func(t *testing.T) {
		fixture := newTransitionChainFixture(t)
		claim := applyTestClaim(t, fixture, applyTestAttachment(t, fixture, applyTestReservation(t, fixture)))
		request := TransitionRequest{
			IssueID: fixture.work.ID, Step: TransitionStepClose, OperationID: "close-1",
			PriorReceiptID: claim.Receipt.ReceiptID, Evidence: fixture.evidence,
			Patch: SourceWorkPatch{Status: &StringTransition{Expected: "in_progress", Value: "closed"}},
		}
		if _, err := fixture.chain.Apply(request); !errors.Is(err, ErrTransitionChainInvalid) {
			t.Fatalf("close directly from claim = %v, want predecessor refusal", err)
		}
	})
}

func TestTransitionChainAllowsRepeatedRecoveryAndExactCompletionReplayOnly(t *testing.T) {
	claimOnly := newTransitionChainFixture(t)
	claimOnlyReceipt := applyTestClaim(t, claimOnly, applyTestAttachment(t, claimOnly, applyTestReservation(t, claimOnly)))
	applyTestCompletionBudget(t, claimOnly, claimOnlyReceipt)

	fixture := newTransitionChainFixture(t)
	claim := applyTestClaim(t, fixture, applyTestAttachment(t, fixture, applyTestReservation(t, fixture)))
	recoveryOne := fixture.recoveryBudgetRequest(t, claim.Receipt, "recovery-request-1")
	recoveryResult, err := fixture.chain.Apply(recoveryOne)
	if err != nil {
		t.Fatalf("first recovery budget: %v", err)
	}
	recoveryTwo := fixture.recoveryBudgetRequest(t, recoveryResult.Receipt, "recovery-request-2")
	recoveryTwoResult, err := fixture.chain.Apply(recoveryTwo)
	if err != nil {
		t.Fatalf("repeated recovery budget: %v", err)
	}

	completion := TransitionRequest{
		IssueID: fixture.work.ID, Step: TransitionStepCompletionBudget, OperationID: "completion-1",
		PriorReceiptID: recoveryResult.Receipt.ReceiptID, Evidence: fixture.evidence,
	}
	// Completion may branch from the latest recovery receipt, so use the second
	// recovery receipt as its direct parent.
	completion.PriorReceiptID = recoveryTwo.PriorReceiptID
	completion = fixture.completionBudgetRequest(t, recoveryTwoResult.Receipt, fixture.completionReceiptValue(t, nil))
	completionResult, err := fixture.chain.Apply(completion)
	if err != nil {
		t.Fatalf("completion budget after recovery: %v", err)
	}
	// A durable exact receipt remains replayable after acceptance freshness
	// expires; freshness is required for a new close transition.
	fixture.now = fixture.now.Add(200 * time.Hour)
	replay, err := fixture.chain.Apply(completion)
	if err != nil || !replay.Replayed || replay.Receipt.ReceiptID != completionResult.Receipt.ReceiptID {
		t.Fatalf("exact completion replay = %+v err %v, want same receipt replay", replay, err)
	}
	current, err := fixture.store.DecisionFrontierSourceSnapshot(fixture.work.ID)
	if err != nil {
		t.Fatal(err)
	}
	priorCompletion := current.Metadata[beadmeta.LifecycleCompletionBudgetMetadataKey]
	completionRetry := completion
	completionRetry.OperationID = "completion-2"
	completionRetry.PriorReceiptID = completionResult.Receipt.ReceiptID
	completionRetry.Patch = SourceWorkPatch{Metadata: map[string]MetadataStringPatch{
		beadmeta.LifecycleCompletionBudgetMetadataKey: {Expected: &priorCompletion, Value: `{"version":1,"reserved":false}`},
	}}
	if _, err := fixture.chain.Apply(completionRetry); !errors.Is(err, ErrTransitionChainInvalid) {
		t.Fatalf("new completion budget step after completion = %v, want predecessor refusal", err)
	}
}

func TestTransitionChainRecoveryBudgetIsRequestBoundAndAppendOnly(t *testing.T) {
	t.Run("valid first and second append replay exactly", func(t *testing.T) {
		fixture := newTransitionChainFixture(t)
		claim := applyTestClaim(t, fixture, applyTestAttachment(t, fixture, applyTestReservation(t, fixture)))
		firstRequest := fixture.recoveryBudgetRequest(t, claim.Receipt, "recovery-request-1")
		first, err := fixture.chain.Apply(firstRequest)
		if err != nil {
			t.Fatalf("first recovery append: %v", err)
		}
		replay, err := fixture.chain.Apply(firstRequest)
		if err != nil || !replay.Replayed || replay.Receipt.ReceiptID != first.Receipt.ReceiptID {
			t.Fatalf("first recovery replay = %+v, err %v", replay, err)
		}
		secondRequest := fixture.recoveryBudgetRequest(t, first.Receipt, "recovery-request-2")
		second, err := fixture.chain.Apply(secondRequest)
		if err != nil {
			t.Fatalf("second recovery append: %v", err)
		}
		state, err := decodeRecoveryState(
			fixture.store.source.Metadata[beadmeta.LifecycleRecoveryStateMetadataKey], fixture.work.ID, fixture.policy.SourceScope,
		)
		if err != nil || len(state.Attempts) != MaxRecoveryAttempts || second.Receipt.PriorReceiptID != first.Receipt.ReceiptID {
			t.Fatalf("final recovery state = %+v err %v; receipt %+v", state, err, second.Receipt)
		}
	})

	cases := []struct {
		name   string
		mutate func(*testing.T, *transitionChainFixture, *TransitionRequest)
	}{
		{name: "reset", mutate: func(t *testing.T, _ *transitionChainFixture, request *TransitionRequest) {
			rewriteRecoveryRequestState(t, request, func(state *RecoveryState) { state.Attempts = []RecoveryAttempt{} })
		}},
		{name: "malformed state", mutate: func(_ *testing.T, _ *transitionChainFixture, request *TransitionRequest) {
			change := request.Patch.Metadata[beadmeta.LifecycleRecoveryStateMetadataKey]
			change.Value = `{"version":1,"attempts":[`
			request.Patch.Metadata[beadmeta.LifecycleRecoveryStateMetadataKey] = change
		}},
		{name: "more than two attempts", mutate: func(t *testing.T, fixture *transitionChainFixture, request *TransitionRequest) {
			rewriteRecoveryRequestState(t, request, func(state *RecoveryState) {
				state.Attempts = append(state.Attempts, RecoveryAttempt{
					ID: "00000000000000000000000000000003", ReservedAt: fixture.now.Format(time.RFC3339Nano),
					RequestID: "recovery-request-3", RequestDigest: strings.Repeat("3", 64), ExpectedRevision: request.RecoveryRequest.ExpectedRevision,
				})
			})
		}},
		{name: "changed prior attempt", mutate: func(t *testing.T, _ *transitionChainFixture, request *TransitionRequest) {
			rewriteRecoveryRequestState(t, request, func(state *RecoveryState) {
				state.Attempts[0].ID = "ffffffffffffffffffffffffffffffff"
			})
		}},
		{name: "duplicate request", mutate: func(t *testing.T, _ *transitionChainFixture, request *TransitionRequest) {
			rewriteRecoveryRequestState(t, request, func(state *RecoveryState) {
				state.Attempts[1].RequestID = state.Attempts[0].RequestID
				state.Attempts[1].RequestDigest = state.Attempts[0].RequestDigest
			})
		}},
		{name: "unbound new attempt", mutate: func(t *testing.T, _ *transitionChainFixture, request *TransitionRequest) {
			rewriteRecoveryRequestState(t, request, func(state *RecoveryState) {
				last := &state.Attempts[len(state.Attempts)-1]
				last.RequestID, last.RequestDigest, last.ExpectedRevision = "", "", 0
			})
		}},
		{name: "request digest mismatch", mutate: func(t *testing.T, _ *transitionChainFixture, request *TransitionRequest) {
			rewriteRecoveryRequestState(t, request, func(state *RecoveryState) {
				state.Attempts[len(state.Attempts)-1].RequestDigest = strings.Repeat("9", 64)
			})
		}},
		{name: "request ID differs from operation", mutate: func(_ *testing.T, _ *transitionChainFixture, request *TransitionRequest) {
			request.OperationID = "different-operation"
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newTransitionChainFixture(t)
			claim := applyTestClaim(t, fixture, applyTestAttachment(t, fixture, applyTestReservation(t, fixture)))
			firstRequest := fixture.recoveryBudgetRequest(t, claim.Receipt, "recovery-request-1")
			first, err := fixture.chain.Apply(firstRequest)
			if err != nil {
				t.Fatalf("prepare first recovery: %v", err)
			}
			request := fixture.recoveryBudgetRequest(t, first.Receipt, "recovery-request-2")
			tc.mutate(t, fixture, &request)
			beforeWrites, beforePermits := len(fixture.store.patchRequests), len(fixture.permits.requests)
			if _, err := fixture.chain.Apply(request); !errors.Is(err, ErrTransitionChainInvalid) && !errors.Is(err, ErrTransitionChainEvidence) {
				t.Fatalf("invalid recovery append error = %v, want invalid or evidence refusal", err)
			}
			if len(fixture.store.patchRequests) != beforeWrites || len(fixture.permits.requests) != beforePermits {
				t.Fatalf("invalid recovery append reached write/permit: %d->%d, %d->%d", beforeWrites, len(fixture.store.patchRequests), beforePermits, len(fixture.permits.requests))
			}
		})
	}
}

func TestTransitionChainPersistsRecoveryEscalationAndReplaysAfterRestart(t *testing.T) {
	fixture := newTransitionChainFixture(t)
	claim := applyTestClaim(t, fixture, applyTestAttachment(t, fixture, applyTestReservation(t, fixture)))
	first, err := fixture.chain.Apply(fixture.recoveryBudgetRequest(t, claim.Receipt, "recovery-request-1"))
	if err != nil {
		t.Fatalf("first recovery budget: %v", err)
	}
	second, err := fixture.chain.Apply(fixture.recoveryBudgetRequest(t, first.Receipt, "recovery-request-2"))
	if err != nil {
		t.Fatalf("second recovery budget: %v", err)
	}

	escalation := fixture.recoveryEscalationRequest(t, second.Receipt, "escalation-request-1")
	result, err := fixture.chain.Apply(escalation)
	if err != nil {
		t.Fatalf("persist authorized recovery escalation: %v", err)
	}
	if result.Receipt.Kind != transitionStepKind(TransitionStepRecoveryEscalation) || result.Receipt.PriorReceiptID != second.Receipt.ReceiptID {
		t.Fatalf("escalation receipt = %+v, want a direct recovery-budget child", result.Receipt)
	}
	state, err := decodeCanonicalRecoveryState(
		fixture.store.source.Metadata[beadmeta.LifecycleRecoveryStateMetadataKey], fixture.work.ID, fixture.policy.SourceScope,
	)
	if err != nil || state.Escalation == nil || state.Escalation.Request == nil || *state.Escalation.Request != *escalation.RecoveryEscalationRequest {
		t.Fatalf("persisted escalation = %+v err=%v, want exact signed request", state.Escalation, err)
	}
	wantEscalationID, err := recoveryEscalationID(*escalation.RecoveryEscalationRequest)
	if err != nil {
		t.Fatal(err)
	}
	if state.Escalation.ID != wantEscalationID ||
		state.Escalation.Target != fixture.cfg.EscalationTarget ||
		state.Escalation.DedupKey != recoveryEscalationDedupKey(fixture.policy.SourceScope, fixture.work.ID, state.Escalation.ID) {
		t.Fatalf("escalation identity is not derived from its authorization: %+v", state.Escalation)
	}
	if len(fixture.permits.requests) == 0 || fixture.permits.requests[len(fixture.permits.requests)-1].replayID != result.Receipt.ReceiptID {
		t.Fatalf("escalation permit is not bound to its exact receipt: %+v", fixture.permits.requests)
	}
	escalationPatchRequest := fixture.store.patchRequests[len(fixture.store.patchRequests)-1]
	escalationPermit := fixture.permits.requests[len(fixture.permits.requests)-1]
	escalationDigest, err := beads.RevisionTransitionPatchProtectedMutationDigest(fixture.work.ID, escalationPatchRequest)
	if err != nil {
		t.Fatal(err)
	}
	if escalationPermit.request.Operation != beads.RevisionTransitionPatchProtectedMutationOperation ||
		!reflect.DeepEqual(escalationPermit.request.ResourceIDs, []string{fixture.work.ID}) ||
		escalationPermit.request.RequestDigest != escalationDigest {
		t.Fatalf("escalation permit = %+v, want exact operation, source, and patch digest", escalationPermit.request)
	}

	completion := fixture.completionBudgetRequest(t, result.Receipt, fixture.completionReceiptValue(t, nil))
	completionResult, err := fixture.chain.Apply(completion)
	if err != nil {
		t.Fatalf("continue chain from recovery escalation to completion budget: %v", err)
	}

	restarted, err := NewTransitionChain(fixture.chainConfig())
	if err != nil {
		t.Fatal(err)
	}
	fixture.chain = restarted
	head, err := fixture.chain.CurrentHead(fixture.work.ID, fixture.evidence)
	if err != nil || head.ReceiptID != completionResult.Receipt.ReceiptID {
		t.Fatalf("current head after restart = %+v err=%v", head, err)
	}
	fixture.now = fixture.now.Add(2 * time.Hour)
	replay, err := fixture.chain.Apply(escalation)
	if err != nil || !replay.Replayed || replay.Receipt.ReceiptID != result.Receipt.ReceiptID {
		t.Fatalf("expired historical escalation replay = %+v err=%v, want exact receipt", replay, err)
	}
}

func TestTransitionChainRejectsInvalidRecoveryEscalation(t *testing.T) {
	t.Run("not exhausted", func(t *testing.T) {
		fixture := newTransitionChainFixture(t)
		claim := applyTestClaim(t, fixture, applyTestAttachment(t, fixture, applyTestReservation(t, fixture)))
		first, err := fixture.chain.Apply(fixture.recoveryBudgetRequest(t, claim.Receipt, "recovery-request-1"))
		if err != nil {
			t.Fatal(err)
		}
		request := fixture.rawRecoveryEscalationRequest(t, first.Receipt, "escalation-request-1")
		assertEscalationRejectedWithoutSideEffects(t, fixture, request)
	})

	for _, tc := range []struct {
		name   string
		mutate func(*testing.T, *transitionChainFixture, *TransitionRequest)
	}{
		{name: "modified signed target", mutate: func(_ *testing.T, _ *transitionChainFixture, request *TransitionRequest) {
			request.RecoveryEscalationRequest.Target = "different-recipient"
		}},
		{name: "signed target differs from configured recipient", mutate: func(t *testing.T, fixture *transitionChainFixture, request *TransitionRequest) {
			proof := *request.RecoveryEscalationRequest
			proof.Target = "different-recipient"
			var err error
			*request.RecoveryEscalationRequest, err = SignRecoveryEscalationRequest(proof, fixture.recoveryPrivate)
			if err != nil {
				t.Fatal(err)
			}
			current, err := fixture.store.DecisionFrontierSourceSnapshot(fixture.work.ID)
			if err != nil {
				t.Fatal(err)
			}
			request.Patch, err = BuildRecoveryEscalationPatch(current.Metadata[beadmeta.LifecycleRecoveryStateMetadataKey], *request.RecoveryEscalationRequest)
			if err != nil {
				t.Fatal(err)
			}
		}},
		{name: "expired authorization", mutate: func(_ *testing.T, fixture *transitionChainFixture, _ *TransitionRequest) {
			fixture.now = fixture.now.Add(2 * time.Hour)
		}},
		{name: "wrong signed parent revision", mutate: func(t *testing.T, fixture *transitionChainFixture, request *TransitionRequest) {
			proof := *request.RecoveryEscalationRequest
			proof.ExpectedRevision++
			var err error
			*request.RecoveryEscalationRequest, err = SignRecoveryEscalationRequest(proof, fixture.recoveryPrivate)
			if err != nil {
				t.Fatal(err)
			}
			current, err := fixture.store.DecisionFrontierSourceSnapshot(fixture.work.ID)
			if err != nil {
				t.Fatal(err)
			}
			request.Patch, err = BuildRecoveryEscalationPatch(current.Metadata[beadmeta.LifecycleRecoveryStateMetadataKey], *request.RecoveryEscalationRequest)
			if err != nil {
				t.Fatal(err)
			}
		}},
		{name: "changed previous recovery attempt", mutate: func(t *testing.T, _ *transitionChainFixture, request *TransitionRequest) {
			change := request.Patch.Metadata[beadmeta.LifecycleRecoveryStateMetadataKey]
			var state RecoveryState
			if err := json.Unmarshal([]byte(change.Value), &state); err != nil {
				t.Fatal(err)
			}
			state.Attempts[0].ID = "ffffffffffffffffffffffffffffffff"
			encoded, err := json.Marshal(state)
			if err != nil {
				t.Fatal(err)
			}
			change.Value = string(encoded)
			request.Patch.Metadata[beadmeta.LifecycleRecoveryStateMetadataKey] = change
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newTransitionChainFixture(t)
			claim := applyTestClaim(t, fixture, applyTestAttachment(t, fixture, applyTestReservation(t, fixture)))
			first, err := fixture.chain.Apply(fixture.recoveryBudgetRequest(t, claim.Receipt, "recovery-request-1"))
			if err != nil {
				t.Fatal(err)
			}
			second, err := fixture.chain.Apply(fixture.recoveryBudgetRequest(t, first.Receipt, "recovery-request-2"))
			if err != nil {
				t.Fatal(err)
			}
			request := fixture.recoveryEscalationRequest(t, second.Receipt, "escalation-request-1")
			tc.mutate(t, fixture, &request)
			assertEscalationRejectedWithoutSideEffects(t, fixture, request)
		})
	}

	t.Run("authority lacks escalation action", func(t *testing.T) {
		fixture := newTransitionChainFixture(t)
		claim := applyTestClaim(t, fixture, applyTestAttachment(t, fixture, applyTestReservation(t, fixture)))
		first, err := fixture.chain.Apply(fixture.recoveryBudgetRequest(t, claim.Receipt, "recovery-request-1"))
		if err != nil {
			t.Fatal(err)
		}
		second, err := fixture.chain.Apply(fixture.recoveryBudgetRequest(t, first.Receipt, "recovery-request-2"))
		if err != nil {
			t.Fatal(err)
		}
		request := fixture.recoveryEscalationRequest(t, second.Receipt, "escalation-request-1")
		authority := fixture.cfg.RecoveryAuthorities["recovery"]
		authority.Actions = []string{"nudge"}
		fixture.cfg.RecoveryAuthorities["recovery"] = authority
		chain, err := NewTransitionChain(fixture.chainConfig())
		if err != nil {
			t.Fatal(err)
		}
		fixture.chain = chain
		assertEscalationRejectedWithoutSideEffects(t, fixture, request)
	})
}

func assertEscalationRejectedWithoutSideEffects(t *testing.T, fixture *transitionChainFixture, request TransitionRequest) {
	t.Helper()
	beforeWrites, beforePermits := len(fixture.store.patchRequests), len(fixture.permits.requests)
	if _, err := fixture.chain.Apply(request); !errors.Is(err, ErrTransitionChainInvalid) &&
		!errors.Is(err, ErrTransitionChainEvidence) && !errors.Is(err, ErrTransitionChainStale) {
		t.Fatalf("invalid recovery escalation error = %v, want invalid/evidence/stale refusal", err)
	}
	if len(fixture.store.patchRequests) != beforeWrites || len(fixture.permits.requests) != beforePermits {
		t.Fatalf("invalid escalation reached patch/permit: %d->%d, %d->%d", beforeWrites, len(fixture.store.patchRequests), beforePermits, len(fixture.permits.requests))
	}
}

func rewriteRecoveryRequestState(t *testing.T, request *TransitionRequest, mutate func(*RecoveryState)) {
	t.Helper()
	change := request.Patch.Metadata[beadmeta.LifecycleRecoveryStateMetadataKey]
	var state RecoveryState
	if err := json.Unmarshal([]byte(change.Value), &state); err != nil {
		t.Fatal(err)
	}
	mutate(&state)
	encoded, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	change.Value = string(encoded)
	request.Patch.Metadata[beadmeta.LifecycleRecoveryStateMetadataKey] = change
}

func TestTransitionChainRequiresCanonicalBoundCompletionBudget(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*testing.T, *TransitionRequest)
	}{
		{name: "arbitrary JSON", mutate: func(_ *testing.T, request *TransitionRequest) {
			change := request.Patch.Metadata[beadmeta.LifecycleCompletionBudgetMetadataKey]
			change.Value = `{"version":1,"reserved":true}`
			request.Patch.Metadata[beadmeta.LifecycleCompletionBudgetMetadataKey] = change
		}},
		{name: "wrong work item", mutate: func(t *testing.T, request *TransitionRequest) {
			rewriteCompletionBudgetRequest(t, request, func(metadata *completionBudgetMetadata) { metadata.WorkItemID = "other-work" })
		}},
		{name: "wrong scope", mutate: func(t *testing.T, request *TransitionRequest) {
			rewriteCompletionBudgetRequest(t, request, func(metadata *completionBudgetMetadata) { metadata.Scope = "other-scope" })
		}},
		{name: "wrong admission digest", mutate: func(t *testing.T, request *TransitionRequest) {
			rewriteCompletionBudgetRequest(t, request, func(metadata *completionBudgetMetadata) {
				metadata.AdmissionDigest = base64.RawURLEncoding.EncodeToString(make([]byte, 32))
			})
		}},
		{name: "malformed completion digest", mutate: func(t *testing.T, request *TransitionRequest) {
			rewriteCompletionBudgetRequest(t, request, func(metadata *completionBudgetMetadata) { metadata.CompletionReceiptDigest = "invalid" })
		}},
		{name: "not consumed", mutate: func(t *testing.T, request *TransitionRequest) {
			rewriteCompletionBudgetRequest(t, request, func(metadata *completionBudgetMetadata) { metadata.Consumed = false })
		}},
		{name: "extra field", mutate: func(_ *testing.T, request *TransitionRequest) {
			change := request.Patch.Metadata[beadmeta.LifecycleCompletionBudgetMetadataKey]
			change.Value = strings.TrimSuffix(change.Value, "}") + `,"extra":true}`
			request.Patch.Metadata[beadmeta.LifecycleCompletionBudgetMetadataKey] = change
		}},
		{name: "expected value is caller supplied", mutate: func(_ *testing.T, request *TransitionRequest) {
			expected := "forged-prior-budget"
			change := request.Patch.Metadata[beadmeta.LifecycleCompletionBudgetMetadataKey]
			change.Expected = &expected
			request.Patch.Metadata[beadmeta.LifecycleCompletionBudgetMetadataKey] = change
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newTransitionChainFixture(t)
			claim := applyTestClaim(t, fixture, applyTestAttachment(t, fixture, applyTestReservation(t, fixture)))
			request := fixture.completionBudgetRequest(t, claim.Receipt, fixture.completionReceiptValue(t, nil))
			tc.mutate(t, &request)
			beforeWrites, beforePermits := len(fixture.store.patchRequests), len(fixture.permits.requests)
			if _, err := fixture.chain.Apply(request); !errors.Is(err, ErrTransitionChainInvalid) {
				t.Fatalf("malformed completion budget error = %v, want invalid transition", err)
			}
			if len(fixture.store.patchRequests) != beforeWrites || len(fixture.permits.requests) != beforePermits {
				t.Fatalf("invalid completion budget reached write/permit: %d->%d, %d->%d", beforeWrites, len(fixture.store.patchRequests), beforePermits, len(fixture.permits.requests))
			}
		})
	}

	t.Run("close receipt must match consumed digest", func(t *testing.T) {
		fixture := newTransitionChainFixture(t)
		claim := applyTestClaim(t, fixture, applyTestAttachment(t, fixture, applyTestReservation(t, fixture)))
		budgetReceipt := fixture.completionReceiptValue(t, nil)
		budget, err := fixture.chain.Apply(fixture.completionBudgetRequest(t, claim.Receipt, budgetReceipt))
		if err != nil {
			t.Fatal(err)
		}
		closeReceipt := fixture.completionReceiptValue(t, func(receipt *CompletionReceipt) { receipt.DeliverableRef = "artifact://different" })
		beforeWrites, beforePermits := len(fixture.store.patchRequests), len(fixture.permits.requests)
		if _, err := fixture.chain.Apply(closeTestRequest(fixture, budget, closeReceipt)); !errors.Is(err, ErrTransitionChainEvidence) {
			t.Fatalf("close with a different completion digest = %v, want evidence refusal", err)
		}
		if len(fixture.store.patchRequests) != beforeWrites || len(fixture.permits.requests) != beforePermits {
			t.Fatalf("mismatched completion receipt reached write/permit: %d->%d, %d->%d", beforeWrites, len(fixture.store.patchRequests), beforePermits, len(fixture.permits.requests))
		}
	})
}

func rewriteCompletionBudgetRequest(t *testing.T, request *TransitionRequest, mutate func(*completionBudgetMetadata)) {
	t.Helper()
	change := request.Patch.Metadata[beadmeta.LifecycleCompletionBudgetMetadataKey]
	var metadata completionBudgetMetadata
	if err := json.Unmarshal([]byte(change.Value), &metadata); err != nil {
		t.Fatal(err)
	}
	mutate(&metadata)
	encoded, err := json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	change.Value = string(encoded)
	request.Patch.Metadata[beadmeta.LifecycleCompletionBudgetMetadataKey] = change
}

func TestTransitionChainRecoversLostResponseAndReplaysAfterRestart(t *testing.T) {
	fixture := newTransitionChainFixture(t)
	fixture.store.loseNextPatchResponse = true
	request := fixture.firstReservationRequest()
	got, err := fixture.chain.Apply(request)
	if err != nil {
		t.Fatalf("Apply after committed lost response: %v", err)
	}
	if !got.Recovered || got.Replayed || len(fixture.store.patchRequests) != 1 {
		t.Fatalf("lost-response result = %+v; calls %d, want exact-receipt recovery", got, len(fixture.store.patchRequests))
	}

	restarted, err := NewTransitionChain(fixture.chainConfig())
	if err != nil {
		t.Fatal(err)
	}
	fixture.chain = restarted
	replay, err := fixture.chain.Apply(request)
	if err != nil {
		t.Fatalf("Apply after restart: %v", err)
	}
	if !replay.Replayed || replay.Recovered || replay.Receipt.ReceiptID != got.Receipt.ReceiptID || len(fixture.store.patchRequests) != 1 {
		t.Fatalf("restart replay = %+v; calls %d, want same immutable receipt without a write", replay, len(fixture.store.patchRequests))
	}
}

func TestTransitionChainDoesNotRecoverWithoutExactReceipt(t *testing.T) {
	fixture := newTransitionChainFixture(t)
	fixture.store.loseNextPatchWithoutCommit = true
	if got, err := fixture.chain.Apply(fixture.firstReservationRequest()); err == nil {
		t.Fatalf("Apply with lost response and no receipt = %+v, want fail closed", got)
	}
	if len(fixture.store.patchReceipts) != 0 || len(fixture.store.patchRequests) != 1 {
		t.Fatalf("receipts %d calls %d, want no committed receipt and one attempted write", len(fixture.store.patchReceipts), len(fixture.store.patchRequests))
	}
}

func TestTransitionChainRejectsConflictsRacesAndBrokenParentProofs(t *testing.T) {
	t.Run("same deterministic ID with changed patch", func(t *testing.T) {
		fixture := newTransitionChainFixture(t)
		request := fixture.firstReservationRequest()
		if _, err := fixture.chain.Apply(request); err != nil {
			t.Fatal(err)
		}
		changed := strings.Replace(fixture.materializationValue("reserved", ""), `"token":"reservation-token"`, `"token":"other-token"`, 1)
		request.Patch = SourceWorkPatch{Metadata: map[string]MetadataStringPatch{
			beadmeta.LifecycleMaterializationMetadataKey: {Value: changed},
		}}
		if _, err := fixture.chain.Apply(request); !errors.Is(err, ErrTransitionChainReceipt) {
			t.Fatalf("conflicting replay error = %v, want receipt conflict", err)
		}
		if len(fixture.store.patchRequests) != 1 {
			t.Fatalf("conflicting replay writes = %d, want one", len(fixture.store.patchRequests))
		}
	})

	t.Run("concurrent source revision change", func(t *testing.T) {
		fixture := newTransitionChainFixture(t)
		fixture.store.raceNextPatch = true
		if _, err := fixture.chain.Apply(fixture.firstReservationRequest()); !errors.Is(err, ErrTransitionChainStale) {
			t.Fatalf("race error = %v, want stale revision", err)
		}
		if len(fixture.store.patchReceipts) != 0 {
			t.Fatalf("race committed %d transition receipts, want zero", len(fixture.store.patchReceipts))
		}
	})

	t.Run("unknown parent receipt", func(t *testing.T) {
		fixture := newTransitionChainFixture(t)
		request := fixture.firstReservationRequest()
		request.PriorReceiptID = "q43-receipt-that-does-not-exist"
		if _, err := fixture.chain.Apply(request); !errors.Is(err, ErrTransitionChainReceipt) {
			t.Fatalf("unknown parent error = %v, want receipt refusal", err)
		}
	})

	t.Run("wrong stored parent digest", func(t *testing.T) {
		fixture := newTransitionChainFixture(t)
		first, err := fixture.chain.Apply(fixture.firstReservationRequest())
		if err != nil {
			t.Fatal(err)
		}
		parent := fixture.store.patchReceipts[first.Receipt.ReceiptID]
		parent.PriorReceiptDigest = strings.Repeat("1", 64)
		fixture.store.patchReceipts[parent.ReceiptID] = parent
		current, err := fixture.store.DecisionFrontierSourceSnapshot(fixture.work.ID)
		if err != nil {
			t.Fatal(err)
		}
		request := TransitionRequest{
			IssueID: fixture.work.ID, Step: TransitionStepAttachedMaterialization, OperationID: "materialize-1",
			PriorReceiptID: first.Receipt.ReceiptID, Evidence: fixture.evidence,
			Patch: fixture.attachedMaterializationPatch(current),
		}
		if _, err := fixture.chain.Apply(request); !errors.Is(err, ErrTransitionChainReceipt) {
			t.Fatalf("wrong stored parent digest error = %v, want receipt refusal", err)
		}
	})

	t.Run("wrong Q43 digest and ToVersion", func(t *testing.T) {
		for _, mutate := range []struct {
			name string
			fn   func(*beads.ControllerMetadataTransitionReceipt)
		}{
			{name: "wrong digest payload", fn: func(receipt *beads.ControllerMetadataTransitionReceipt) {
				receipt.Payload = json.RawMessage(`{"schema_version":1,"receipt_digest":"wrong"}`)
			}},
			{name: "wrong ToVersion", fn: func(receipt *beads.ControllerMetadataTransitionReceipt) { receipt.ToVersion++ }},
		} {
			t.Run(mutate.name, func(t *testing.T) {
				fixture := newTransitionChainFixture(t)
				actual := fixture.store.q43Receipts[fixture.attachment.ReceiptID]
				mutate.fn(&actual)
				fixture.store.q43Receipts[fixture.attachment.ReceiptID] = actual
				if _, err := fixture.chain.Apply(fixture.firstReservationRequest()); !errors.Is(err, ErrTransitionChainReceipt) {
					t.Fatalf("broken Q43 parent error = %v, want receipt refusal", err)
				}
			})
		}
	})

	t.Run("stale current policy resolver result", func(t *testing.T) {
		fixture := newTransitionChainFixture(t)
		request := fixture.firstReservationRequest()
		fixture.policyResolver.policy.FormulaSources = append([]AdmissionFormulaSourceV2(nil), fixture.policyResolver.policy.FormulaSources...)
		fixture.policyResolver.policy.FormulaSources[0].SHA256 = strings.Repeat("9", 64)
		if _, err := fixture.chain.Apply(request); !errors.Is(err, ErrTransitionChainEvidence) {
			t.Fatalf("stale resolver result = %v, want evidence refusal", err)
		}
		if len(fixture.store.patchRequests) != 0 {
			t.Fatalf("changed policy proof reached writer %d times, want zero", len(fixture.store.patchRequests))
		}
	})
}

func TestTransitionChainFailsClosedOnPermitFailureAndUnsupportedCapability(t *testing.T) {
	fixture := newTransitionChainFixture(t)
	fixture.permits.err = errors.New("permit refused")
	if _, err := fixture.chain.Apply(fixture.firstReservationRequest()); !errors.Is(err, ErrTransitionChainPermit) {
		t.Fatalf("permit failure = %v, want permit refusal", err)
	}
	if len(fixture.store.patchRequests) != 0 {
		t.Fatalf("permit failure reached patch writer %d times, want zero", len(fixture.store.patchRequests))
	}

	config := fixture.chainConfig()
	config.PatchWriter = nil
	if _, err := NewTransitionChain(config); !errors.Is(err, ErrTransitionChainUnavailable) {
		t.Fatalf("constructor with unsupported patch capability error = %v, want unavailable", err)
	}
	for _, mutate := range []struct {
		name string
		fn   func(*TransitionChainConfig)
	}{
		{name: "policy resolver", fn: func(config *TransitionChainConfig) { config.PolicyResolver = nil }},
		{name: "workflow evidence verifier", fn: func(config *TransitionChainConfig) { config.WorkflowEvidenceVerifier = nil }},
		{name: "clock", fn: func(config *TransitionChainConfig) { config.Now = nil }},
	} {
		t.Run("missing "+mutate.name, func(t *testing.T) {
			config := fixture.chainConfig()
			mutate.fn(&config)
			if _, err := NewTransitionChain(config); !errors.Is(err, ErrTransitionChainUnavailable) {
				t.Fatalf("constructor without %s error = %v, want unavailable", mutate.name, err)
			}
		})
	}
}

func TestTransitionChainRequiresCurrentPolicyAndWorkflowEvidenceCapabilities(t *testing.T) {
	t.Run("resolver error", func(t *testing.T) {
		fixture := newTransitionChainFixture(t)
		fixture.policyResolver.err = errors.New("current policy unavailable")
		if _, err := fixture.chain.Apply(fixture.firstReservationRequest()); !errors.Is(err, ErrTransitionChainEvidence) {
			t.Fatalf("Apply with resolver error = %v, want evidence refusal", err)
		}
		if len(fixture.store.patchRequests) != 0 {
			t.Fatalf("resolver failure reached writer %d times, want zero", len(fixture.store.patchRequests))
		}
	})

	t.Run("workflow verifier mismatch", func(t *testing.T) {
		fixture := newTransitionChainFixture(t)
		reservation := applyTestReservation(t, fixture)
		current, err := fixture.store.DecisionFrontierSourceSnapshot(fixture.work.ID)
		if err != nil {
			t.Fatal(err)
		}
		fixture.workflowVerifier.err = errors.New("workflow root missing or lineage differs")
		request := TransitionRequest{
			IssueID: fixture.work.ID, Step: TransitionStepAttachedMaterialization, OperationID: "materialize-1",
			PriorReceiptID: reservation.Receipt.ReceiptID, Evidence: fixture.evidence,
			Patch: fixture.attachedMaterializationPatch(current),
		}
		if _, err := fixture.chain.Apply(request); !errors.Is(err, ErrTransitionChainEvidence) {
			t.Fatalf("Apply with workflow mismatch = %v, want evidence refusal", err)
		}
		if len(fixture.workflowVerifier.calls) != 1 || len(fixture.store.patchRequests) != 1 || len(fixture.permits.requests) != 1 {
			t.Fatalf("failed workflow proof calls=%d patches=%d permits=%d; want one proof and no attachment write/permit", len(fixture.workflowVerifier.calls), len(fixture.store.patchRequests), len(fixture.permits.requests))
		}
	})
}

func TestTransitionChainRequiresFreshSignedCompletionReceiptForClose(t *testing.T) {
	_, wrongSigner, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name  string
		value func(*testing.T, *transitionChainFixture) string
	}{
		{name: "fake string", value: func(_ *testing.T, _ *transitionChainFixture) string { return `{"version":1,"accepted":true}` }},
		{name: "wrong work binding", value: func(t *testing.T, fixture *transitionChainFixture) string {
			return fixture.completionReceiptValue(t, func(receipt *CompletionReceipt) { receipt.WorkItemID = "other-work" })
		}},
		{name: "wrong scope binding", value: func(t *testing.T, fixture *transitionChainFixture) string {
			return fixture.completionReceiptValue(t, func(receipt *CompletionReceipt) { receipt.Scope = "other-scope" })
		}},
		{name: "wrong admission digest", value: func(t *testing.T, fixture *transitionChainFixture) string {
			return fixture.completionReceiptValue(t, func(receipt *CompletionReceipt) { receipt.AdmissionDigest = strings.Repeat("1", 43) })
		}},
		{name: "missing artifact reference", value: func(t *testing.T, fixture *transitionChainFixture) string {
			return fixture.completionReceiptValue(t, func(receipt *CompletionReceipt) { receipt.DeliverableRef = " " })
		}},
		{name: "untrusted signature", value: func(t *testing.T, fixture *transitionChainFixture) string {
			receipt := CompletionReceipt{
				Version: 1, WorkItemID: fixture.work.ID, Scope: fixture.policy.SourceScope,
				AdmissionDigest: fixture.attachment.ReceiptDigest,
				DeliverableRef:  "artifact://work-1/patch", VerificationRef: "checks://work-1/required",
				AcceptedBy: "reviewer", AcceptedAt: fixture.now.Format(time.RFC3339Nano),
			}
			encoded, err := SignCompletionReceipt(receipt, wrongSigner)
			if err != nil {
				t.Fatal(err)
			}
			return encoded
		}},
		{name: "stale acceptance", value: func(t *testing.T, fixture *transitionChainFixture) string {
			return fixture.completionReceiptValue(t, func(receipt *CompletionReceipt) {
				receipt.AcceptedAt = fixture.now.Add(-200 * time.Hour).Format(time.RFC3339Nano)
			})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newTransitionChainFixture(t)
			claim := applyTestClaim(t, fixture, applyTestAttachment(t, fixture, applyTestReservation(t, fixture)))
			completion := tc.value(t, fixture)
			budget, err := fixture.chain.Apply(fixture.completionBudgetRequest(t, claim.Receipt, completion))
			if err != nil {
				t.Fatalf("Apply completion budget for candidate receipt: %v", err)
			}
			beforeWrites := len(fixture.store.patchRequests)
			beforePermits := len(fixture.permits.requests)
			request := closeTestRequest(fixture, budget, completion)
			if _, err := fixture.chain.Apply(request); !errors.Is(err, ErrTransitionChainEvidence) {
				t.Fatalf("Apply with invalid completion receipt = %v, want evidence refusal", err)
			}
			if len(fixture.store.patchRequests) != beforeWrites || len(fixture.permits.requests) != beforePermits {
				t.Fatalf("invalid close reached patch/permit: writes %d->%d permits %d->%d", beforeWrites, len(fixture.store.patchRequests), beforePermits, len(fixture.permits.requests))
			}
		})
	}
}

func metadataTransition(_ beads.Bead, key, value string) SourceWorkPatch {
	return SourceWorkPatch{Metadata: map[string]MetadataStringPatch{key: {Value: value}}}
}

func claimIdentityTestPatch() SourceWorkPatch {
	return SourceWorkPatch{
		Metadata: map[string]MetadataStringPatch{
			beadmeta.ClaimGenerationMetadataKey: {Value: "1"},
			beadmeta.ClaimedAtMetadataKey:       {Value: "2026-09-30T12:00:00Z"},
			beadmeta.SessionIDMetadataKey:       {Value: "session-1"},
			beadmeta.SessionNameMetadataKey:     {Value: "worker-session"},
			beadmeta.WorkDirMetadataKey:         {Value: "/tmp/work"},
			beadmeta.WorkBranchMetadataKey:      {Value: "work-branch"},
		},
		Status:   &StringTransition{Expected: "open", Value: "in_progress"},
		Assignee: &StringTransition{Expected: "", Value: "worker-1"},
	}
}

func cloneSourceWorkPatch(patch SourceWorkPatch) SourceWorkPatch {
	cloned := SourceWorkPatch{Metadata: make(map[string]MetadataStringPatch, len(patch.Metadata))}
	for key, value := range patch.Metadata {
		if value.Expected != nil {
			expected := *value.Expected
			value.Expected = &expected
		}
		cloned.Metadata[key] = value
	}
	if patch.Status != nil {
		value := *patch.Status
		cloned.Status = &value
	}
	if patch.Assignee != nil {
		value := *patch.Assignee
		cloned.Assignee = &value
	}
	return cloned
}

func applyTestReservation(t *testing.T, fixture *transitionChainFixture) TransitionResult {
	t.Helper()
	result, err := fixture.chain.Apply(fixture.firstReservationRequest())
	if err != nil {
		t.Fatalf("apply reservation: %v", err)
	}
	return result
}

func applyTestAttachment(t *testing.T, fixture *transitionChainFixture, parent TransitionResult) TransitionResult {
	t.Helper()
	current, err := fixture.store.DecisionFrontierSourceSnapshot(fixture.work.ID)
	if err != nil {
		t.Fatal(err)
	}
	result, err := fixture.chain.Apply(TransitionRequest{
		IssueID: fixture.work.ID, Step: TransitionStepAttachedMaterialization, OperationID: "materialize-1",
		PriorReceiptID: parent.Receipt.ReceiptID, Evidence: fixture.evidence,
		Patch: fixture.attachedMaterializationPatch(current),
	})
	if err != nil {
		t.Fatalf("apply attached materialization: %v", err)
	}
	return result
}

func applyTestClaim(t *testing.T, fixture *transitionChainFixture, parent TransitionResult) TransitionResult {
	t.Helper()
	result, err := fixture.chain.Apply(TransitionRequest{
		IssueID: fixture.work.ID, Step: TransitionStepClaimIdentity, OperationID: "claim-1",
		PriorReceiptID: parent.Receipt.ReceiptID, Evidence: fixture.evidence, Patch: claimIdentityTestPatch(),
	})
	if err != nil {
		t.Fatalf("apply claim identity: %v", err)
	}
	return result
}

func applyTestCompletionBudget(t *testing.T, fixture *transitionChainFixture, parent TransitionResult) TransitionResult {
	t.Helper()
	request := fixture.completionBudgetRequest(t, parent.Receipt, fixture.completionReceiptValue(t, nil))
	result, err := fixture.chain.Apply(request)
	if err != nil {
		t.Fatalf("apply completion budget: %v", err)
	}
	return result
}

func (f *transitionChainFixture) recoveryBudgetRequest(t *testing.T, parent beads.RevisionTransitionPatchReceipt, requestID string) TransitionRequest {
	t.Helper()
	current, err := f.store.DecisionFrontierSourceSnapshot(f.work.ID)
	if err != nil {
		t.Fatal(err)
	}
	recoveryRequest, err := SignRecoveryRequest(RecoveryRequest{
		Version: 1, RequestID: requestID, Action: "nudge", Scope: f.policy.SourceScope, WorkItemID: f.work.ID,
		ExpectedRevision: parent.ToVersion, Owner: current.Assignee,
		ClaimGeneration: current.Metadata[beadmeta.ClaimGenerationMetadataKey],
		SessionID:       current.Metadata[beadmeta.SessionIDMetadataKey], SessionGeneration: "session-generation-1",
		Message: "authorized lifecycle recovery", IssuedAt: f.now.Add(-time.Minute).Format(time.RFC3339Nano),
		ExpiresAt: f.now.Add(time.Hour).Format(time.RFC3339Nano), AuthorizedBy: "recovery",
	}, f.recoveryPrivate)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := RecoveryRequestDigest(recoveryRequest)
	if err != nil {
		t.Fatal(err)
	}
	var previous RecoveryState
	previousRaw := current.Metadata[beadmeta.LifecycleRecoveryStateMetadataKey]
	if previousRaw == "" {
		previous = RecoveryState{Version: recoveryStateVersion, WorkItemID: f.work.ID, Scope: f.policy.SourceScope, Attempts: []RecoveryAttempt{}}
	} else {
		previous, err = decodeCanonicalRecoveryState(previousRaw, f.work.ID, f.policy.SourceScope)
		if err != nil {
			t.Fatal(err)
		}
	}
	next := cloneRecoveryState(previous)
	next.Attempts = append(next.Attempts, RecoveryAttempt{
		ID: fmt.Sprintf("%032x", len(previous.Attempts)+1), ReservedAt: f.now.UTC().Format(time.RFC3339Nano),
		RequestID: recoveryRequest.RequestID, RequestDigest: digest, ExpectedRevision: recoveryRequest.ExpectedRevision,
	})
	encoded, err := json.Marshal(next)
	if err != nil {
		t.Fatal(err)
	}
	change := MetadataStringPatch{Value: string(encoded)}
	if previousRaw != "" {
		change.Expected = &previousRaw
	}
	operationID, err := RecoveryBudgetOperationID(recoveryRequest.RequestID, digest)
	if err != nil {
		t.Fatal(err)
	}
	return TransitionRequest{
		IssueID: f.work.ID, Step: TransitionStepRecoveryBudget, OperationID: operationID,
		PriorReceiptID: parent.ReceiptID, Evidence: f.evidence, RecoveryRequest: &recoveryRequest,
		Patch: SourceWorkPatch{Metadata: map[string]MetadataStringPatch{beadmeta.LifecycleRecoveryStateMetadataKey: change}},
	}
}

func (f *transitionChainFixture) recoveryEscalationRequest(t *testing.T, parent beads.RevisionTransitionPatchReceipt, requestID string) TransitionRequest {
	t.Helper()
	current, err := f.store.DecisionFrontierSourceSnapshot(f.work.ID)
	if err != nil {
		t.Fatal(err)
	}
	proof, err := SignRecoveryEscalationRequest(RecoveryEscalationRequest{
		Version: 1, RequestID: requestID, Scope: f.policy.SourceScope, WorkItemID: f.work.ID,
		ExpectedRevision: parent.ToVersion, Target: f.cfg.EscalationTarget,
		IssuedAt: f.now.Add(-time.Minute).Format(time.RFC3339Nano), ExpiresAt: f.now.Add(time.Hour).Format(time.RFC3339Nano),
		AuthorizedBy: "recovery",
	}, f.recoveryPrivate)
	if err != nil {
		t.Fatal(err)
	}
	patch, err := BuildRecoveryEscalationPatch(current.Metadata[beadmeta.LifecycleRecoveryStateMetadataKey], proof)
	if err != nil {
		t.Fatal(err)
	}
	return TransitionRequest{
		IssueID: f.work.ID, Step: TransitionStepRecoveryEscalation, OperationID: proof.RequestID,
		PriorReceiptID: parent.ReceiptID, Evidence: f.evidence, RecoveryEscalationRequest: &proof, Patch: patch,
	}
}

func (f *transitionChainFixture) rawRecoveryEscalationRequest(t *testing.T, parent beads.RevisionTransitionPatchReceipt, requestID string) TransitionRequest {
	t.Helper()
	current, err := f.store.DecisionFrontierSourceSnapshot(f.work.ID)
	if err != nil {
		t.Fatal(err)
	}
	proof, err := SignRecoveryEscalationRequest(RecoveryEscalationRequest{
		Version: 1, RequestID: requestID, Scope: f.policy.SourceScope, WorkItemID: f.work.ID,
		ExpectedRevision: parent.ToVersion, Target: f.cfg.EscalationTarget,
		IssuedAt: f.now.Add(-time.Minute).Format(time.RFC3339Nano), ExpiresAt: f.now.Add(time.Hour).Format(time.RFC3339Nano),
		AuthorizedBy: "recovery",
	}, f.recoveryPrivate)
	if err != nil {
		t.Fatal(err)
	}
	priorRaw := current.Metadata[beadmeta.LifecycleRecoveryStateMetadataKey]
	state, err := decodeCanonicalRecoveryState(priorRaw, f.work.ID, f.policy.SourceScope)
	if err != nil {
		t.Fatal(err)
	}
	id, err := recoveryEscalationID(proof)
	if err != nil {
		t.Fatal(err)
	}
	state.Escalation = &RecoveryEscalation{
		ID: id, Target: proof.Target, RequestedAt: proof.IssuedAt,
		DedupKey: recoveryEscalationDedupKey(proof.Scope, proof.WorkItemID, id), Request: &proof,
	}
	nextRaw, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	return TransitionRequest{
		IssueID: f.work.ID, Step: TransitionStepRecoveryEscalation, OperationID: proof.RequestID,
		PriorReceiptID: parent.ReceiptID, Evidence: f.evidence, RecoveryEscalationRequest: &proof,
		Patch: SourceWorkPatch{Metadata: map[string]MetadataStringPatch{
			beadmeta.LifecycleRecoveryStateMetadataKey: {Expected: &priorRaw, Value: string(nextRaw)},
		}},
	}
}

func (f *transitionChainFixture) completionBudgetRequest(t *testing.T, parent beads.RevisionTransitionPatchReceipt, completionReceipt string) TransitionRequest {
	t.Helper()
	value, err := BuildCompletionBudgetMetadata(f.work.ID, f.policy.SourceScope, f.attachment.ReceiptDigest, completionReceipt)
	if err != nil {
		t.Fatal(err)
	}
	return TransitionRequest{
		IssueID: f.work.ID, Step: TransitionStepCompletionBudget, OperationID: "completion-budget-1",
		PriorReceiptID: parent.ReceiptID, Evidence: f.evidence,
		Patch: metadataTransition(beads.Bead{}, beadmeta.LifecycleCompletionBudgetMetadataKey, value),
	}
}

func closeTestRequest(fixture *transitionChainFixture, parent TransitionResult, completion string) TransitionRequest {
	return TransitionRequest{
		IssueID: fixture.work.ID, Step: TransitionStepClose, OperationID: "close-1",
		PriorReceiptID: parent.Receipt.ReceiptID, Evidence: fixture.evidence,
		CompletionReceipt: completion, Patch: SourceWorkPatch{Status: &StringTransition{Expected: "in_progress", Value: "closed"}},
	}
}

func (f *transitionChainFixture) firstReservationRequest() TransitionRequest {
	return TransitionRequest{
		IssueID: f.work.ID, Step: TransitionStepReservation, OperationID: "reserve-1", PriorReceiptID: f.attachment.ReceiptID,
		Evidence: f.evidence,
		Patch: SourceWorkPatch{Metadata: map[string]MetadataStringPatch{
			beadmeta.LifecycleMaterializationMetadataKey: {Value: f.materializationValue("reserved", "")},
		}},
	}
}

func (f *transitionChainFixture) attachedMaterializationPatch(bead beads.Bead) SourceWorkPatch {
	reserved := bead.Metadata[beadmeta.LifecycleMaterializationMetadataKey]
	return SourceWorkPatch{Metadata: map[string]MetadataStringPatch{
		beadmeta.LifecycleMaterializationMetadataKey: {Expected: &reserved, Value: f.materializationValue("attached", "workflow-1")},
		beadmeta.ExecutionRoutedToMetadataKey:        {Value: f.policy.Target.Identity},
		beadmeta.LegacyWorkflowIDMetadataKey:         {Value: "workflow-1"},
		beadmeta.MergeStrategyMetadataKey:            {Value: f.policy.MergeStrategy},
	}}
}

func (f *transitionChainFixture) materializationValue(state, workflowID string) string {
	current, _ := f.store.DecisionFrontierSourceSnapshot(f.work.ID)
	marker := transitionMaterialization{
		Version: 1, State: state, Scope: f.policy.SourceScope, Contract: f.attachment.ReceiptDigest,
		Route: f.policy.Target.Identity, Workflow: f.policy.Workflow, MergeStrategy: f.policy.MergeStrategy,
		Token: "reservation-token", WorkflowID: workflowID, SourceID: f.work.ID,
		SourceStoreRef: "source-store", WorkflowStoreRef: "workflow-store",
		AdmissionReceipt: current.Metadata[beadmeta.LifecycleAdmissionReceiptV2MetadataKey],
	}
	encoded, _ := json.Marshal(marker)
	return string(encoded)
}

func (f *transitionChainFixture) completionReceiptValue(t *testing.T, mutate func(*CompletionReceipt)) string {
	t.Helper()
	receipt := CompletionReceipt{
		Version: 1, WorkItemID: f.work.ID, Scope: f.policy.SourceScope,
		AdmissionDigest: f.attachment.ReceiptDigest,
		DeliverableRef:  "artifact://work-1/patch",
		VerificationRef: "checks://work-1/required",
		AcceptedBy:      "reviewer", AcceptedAt: f.now.Format(time.RFC3339Nano),
	}
	if mutate != nil {
		mutate(&receipt)
	}
	encoded, err := SignCompletionReceipt(receipt, f.acceptancePrivate)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

type transitionChainFixture struct {
	store             *transitionChainTestStore
	permits           *transitionChainPermitIssuer
	policyResolver    *transitionChainPolicyResolver
	workflowVerifier  *transitionChainWorkflowVerifier
	chain             *TransitionChain
	work              beads.Bead
	policy            AdmissionPolicyProjectionV2
	cfg               config.LifecycleConfig
	attachment        AdmissionAttachmentProof
	evidence          TransitionEvidence
	acceptancePrivate ed25519.PrivateKey
	recoveryPrivate   ed25519.PrivateKey
	now               time.Time
}

func newTransitionChainFixture(t *testing.T) *transitionChainFixture {
	t.Helper()
	policy := validAdmissionPolicyProjectionV2(t)
	scope := policy.SourceScope
	admissionPublic, admissionPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	acceptancePublic, acceptancePrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	recoveryPublic, recoveryPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.LifecycleConfig{
		AdmissionEnabled:            true,
		RecoveryEnabled:             true,
		AdmissionV2PrimaryAuthority: "triage",
		AdmissionV2Authorities:      map[string]string{"triage": base64.StdEncoding.EncodeToString(admissionPublic)},
		AcceptanceAuthorities:       map[string]string{"reviewer": base64.StdEncoding.EncodeToString(acceptancePublic)},
		RecoveryAuthorities: map[string]config.LifecycleRecoveryAuthority{"recovery": {
			PublicKey: base64.StdEncoding.EncodeToString(recoveryPublic), Actions: []string{"nudge", "escalate"}, Scopes: []string{scope},
		}},
		EscalationTarget:        "human",
		CompletionReceiptMaxAge: "168h",
		CompletionClockSkew:     "2m",
	}
	policyDigest, err := DigestAdmissionPolicyV2(policy)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.September, 30, 12, 0, 0, 0, time.UTC)
	work := beads.Bead{ID: "work-1", Type: "task", Status: "open", Revision: 41, Labels: []string{AdmissionIntentLabel}, Metadata: map[string]string{}}
	admission := AdmissionReceiptV2{
		Version: 2, WorkItemID: work.ID, Scope: scope, ExpectedWorkRevision: work.Revision,
		Route: policy.Target.Identity, Workflow: policy.Workflow, RoutingPolicyDigest: policyDigest,
		MergeStrategy: policy.MergeStrategy, Deliverable: "reviewed patch", Verification: "required checks",
		AcceptanceAuthority: "reviewer", AdmittedBy: "triage",
	}
	encoded, err := SignAdmissionReceiptV2(admission, admissionPrivate)
	if err != nil {
		t.Fatal(err)
	}
	admissionDigest, err := AdmissionDigestV2(admission)
	if err != nil {
		t.Fatal(err)
	}
	completionReceipt, err := SignCompletionReceipt(CompletionReceipt{
		Version: 1, WorkItemID: work.ID, Scope: scope, AdmissionDigest: admissionDigest,
		DeliverableRef: "artifact://work-1/patch", VerificationRef: "checks://work-1/required",
		AcceptedBy: "reviewer", AcceptedAt: now.Format(time.RFC3339Nano),
	}, acceptancePrivate)
	if err != nil {
		t.Fatal(err)
	}
	work.Metadata[beadmeta.LifecycleCompletionReceiptMetadataKey] = completionReceipt
	store := &transitionChainTestStore{
		source: work, q43Receipts: map[string]beads.ControllerMetadataTransitionReceipt{},
		patchReceipts: map[string]beads.RevisionTransitionPatchReceipt{}, nextRevision: 1000,
	}
	attachmentAdapter := &AdmissionAttachmentAdapter{writer: store, sourceReader: store, receiptReader: store}
	_, attachment, err := attachmentAdapter.Attach(encoded, cfg, scope)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	fixture := &transitionChainFixture{
		store: store, permits: &transitionChainPermitIssuer{token: "permit-token"}, work: work,
		policy: policy, cfg: cfg, attachment: attachment, acceptancePrivate: acceptancePrivate, recoveryPrivate: recoveryPrivate,
		now:              now,
		evidence:         TransitionEvidence{Attachment: attachment},
		policyResolver:   &transitionChainPolicyResolver{policy: policy},
		workflowVerifier: &transitionChainWorkflowVerifier{},
	}
	fixture.chain, err = NewTransitionChain(fixture.chainConfig())
	if err != nil {
		t.Fatal(err)
	}
	return fixture
}

func (f *transitionChainFixture) chainConfig() TransitionChainConfig {
	return TransitionChainConfig{
		Scope: f.policy.SourceScope, Actor: "controller", AdmissionConfig: f.cfg,
		PatchWriter: f.store, PatchReceiptReader: f.store, SourceReader: f.store,
		AttachmentReceiptReader: f.store, PermitIssuer: f.permits,
		PolicyResolver: f.policyResolver, WorkflowEvidenceVerifier: f.workflowVerifier,
		Now: func() time.Time { return f.now },
	}
}

type transitionChainPolicyResolver struct {
	policy AdmissionPolicyProjectionV2
	err    error
	calls  int
}

func (r *transitionChainPolicyResolver) CurrentAdmissionPolicy(source beads.Bead, admission AdmissionReceiptV2) (AdmissionPolicyProjectionV2, error) {
	r.calls++
	if r.err != nil {
		return AdmissionPolicyProjectionV2{}, r.err
	}
	if source.ID != admission.WorkItemID {
		return AdmissionPolicyProjectionV2{}, errors.New("policy resolver received mismatched source and admission")
	}
	return r.policy, nil
}

type transitionChainWorkflowVerifier struct {
	calls    []AttachedWorkflowEvidence
	err      error
	verified bool
}

func (v *transitionChainWorkflowVerifier) VerifyAttachedWorkflow(source beads.Bead, admission AdmissionReceiptV2, policy AdmissionPolicyProjectionV2, evidence AttachedWorkflowEvidence) error {
	v.calls = append(v.calls, evidence)
	if v.err != nil {
		return v.err
	}
	if source.ID != evidence.SourceID || admission.WorkItemID != evidence.SourceID ||
		policy.Target.Identity != evidence.Route || policy.Workflow != evidence.Workflow ||
		policy.MergeStrategy != evidence.MergeStrategy || evidence.WorkflowID == "" ||
		evidence.SourceStoreRef == "" || evidence.WorkflowStoreRef == "" || evidence.Token == "" {
		return errors.New("workflow lineage evidence does not match the current admission")
	}
	v.verified = true
	return nil
}

type transitionChainPermitCall struct {
	request  beads.ControllerProtectedMutationRequest
	replayID string
}

type transitionChainPermitIssuer struct {
	requests []transitionChainPermitCall
	token    string
	err      error
}

func (i *transitionChainPermitIssuer) IssueProtectedMutation(request beads.ControllerProtectedMutationRequest, replayID string) (string, error) {
	i.requests = append(i.requests, transitionChainPermitCall{request: request, replayID: replayID})
	return i.token, i.err
}

type transitionChainTestStore struct {
	mu                         sync.Mutex
	source                     beads.Bead
	q43Receipts                map[string]beads.ControllerMetadataTransitionReceipt
	patchReceipts              map[string]beads.RevisionTransitionPatchReceipt
	patchRequests              []beads.RevisionTransitionPatchRequest
	nextRevision               int64
	raceNextPatch              bool
	loseNextPatchResponse      bool
	loseNextPatchWithoutCommit bool
}

func (s *transitionChainTestStore) DecisionFrontierSourceSnapshot(id string) (beads.Bead, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id != s.source.ID {
		return beads.Bead{}, beads.ErrNotFound
	}
	return cloneTransitionTestBead(s.source), nil
}

func (s *transitionChainTestStore) ControllerMetadataTransitionReceipt(issueID, receiptID string) (beads.ControllerMetadataTransitionReceipt, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	receipt, ok := s.q43Receipts[receiptID]
	if !ok {
		return beads.ControllerMetadataTransitionReceipt{}, false, nil
	}
	if receipt.IssueID != issueID {
		return beads.ControllerMetadataTransitionReceipt{}, true, errors.New("Q43 receipt issue mismatch")
	}
	return receipt, true, nil
}

func (s *transitionChainTestStore) TransitionMetadata(issueID string, request beads.ControllerMetadataTransitionRequest) (beads.ControllerMetadataTransitionResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if prior, ok := s.q43Receipts[request.ReceiptID]; ok {
		return beads.ControllerMetadataTransitionResult{Applied: true, Replayed: true, Receipt: &prior}, nil
	}
	if issueID != s.source.ID || s.source.Revision != request.ExpectedVersion || request.Expected != nil || request.Value == nil {
		return beads.ControllerMetadataTransitionResult{}, errors.New("Q43 precondition failed")
	}
	var value string
	if err := json.Unmarshal(*request.Value, &value); err != nil {
		return beads.ControllerMetadataTransitionResult{}, err
	}
	if s.source.Metadata == nil {
		s.source.Metadata = map[string]string{}
	}
	s.source.Metadata[request.Key] = value
	s.source.Revision = 900
	receipt := beads.ControllerMetadataTransitionReceipt{
		ReceiptID: request.ReceiptID, IssueID: issueID, Scope: request.Scope, Kind: request.Kind, Actor: request.Actor,
		ExpectedVersion: request.ExpectedVersion, ToVersion: s.source.Revision, Key: request.Key,
		Value: append(json.RawMessage(nil), (*request.Value)...), Payload: append(json.RawMessage(nil), request.Payload...),
	}
	s.q43Receipts[request.ReceiptID] = receipt
	return beads.ControllerMetadataTransitionResult{Applied: true, Receipt: &receipt}, nil
}

func (s *transitionChainTestStore) TransitionPatch(issueID string, request beads.RevisionTransitionPatchRequest) (beads.RevisionTransitionPatchResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.patchRequests = append(s.patchRequests, cloneTransitionPatchRequest(request))
	if prior, ok := s.patchReceipts[request.ReceiptID]; ok {
		return beads.RevisionTransitionPatchResult{Applied: true, Replayed: true, Receipt: &prior}, nil
	}
	if s.loseNextPatchWithoutCommit {
		s.loseNextPatchWithoutCommit = false
		return beads.RevisionTransitionPatchResult{}, errors.New("lost response before commit")
	}
	if s.raceNextPatch {
		s.raceNextPatch = false
		s.source.Revision += 7
		return beads.RevisionTransitionPatchResult{}, beads.ErrRevisionTransitionPatchPrecondition
	}
	if issueID != s.source.ID || request.ExpectedVersion != s.source.Revision {
		return beads.RevisionTransitionPatchResult{}, beads.ErrRevisionTransitionPatchPrecondition
	}
	if err := applyTransitionPatchToTestBead(&s.source, request.Patch); err != nil {
		return beads.RevisionTransitionPatchResult{}, err
	}
	s.nextRevision += 137
	if s.nextRevision == request.ExpectedVersion {
		s.nextRevision++
	}
	s.source.Revision = s.nextRevision
	receipt := beads.RevisionTransitionPatchReceipt{
		ReceiptID: request.ReceiptID, IssueID: issueID, Scope: request.Scope, Kind: request.Kind, Actor: request.Actor,
		ExpectedVersion: request.ExpectedVersion, ToVersion: s.source.Revision, PriorReceiptID: request.PriorReceiptID,
		PriorReceiptDigest: request.PriorReceiptDigest, Patch: request.Patch,
	}
	s.patchReceipts[receipt.ReceiptID] = receipt
	if s.loseNextPatchResponse {
		s.loseNextPatchResponse = false
		return beads.RevisionTransitionPatchResult{}, errors.New("lost response after commit")
	}
	return beads.RevisionTransitionPatchResult{Applied: true, Receipt: &receipt}, nil
}

func (s *transitionChainTestStore) ReadRevisionTransitionPatchReceipt(receiptID string) (beads.RevisionTransitionPatchReceipt, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	receipt, ok := s.patchReceipts[receiptID]
	return receipt, ok, nil
}

func applyTransitionPatchToTestBead(source *beads.Bead, patch beads.RevisionTransitionIssuePatch) error {
	if source.Metadata == nil {
		source.Metadata = map[string]string{}
	}
	for _, change := range patch.Metadata {
		current, present := source.Metadata[change.Key]
		if change.Expected == nil {
			if present {
				return fmt.Errorf("metadata %s expected absent", change.Key)
			}
		} else {
			var expected string
			if err := json.Unmarshal(*change.Expected, &expected); err != nil || !present || current != expected {
				return fmt.Errorf("metadata %s expected mismatch", change.Key)
			}
		}
		var value string
		if change.Value == nil || json.Unmarshal(*change.Value, &value) != nil {
			return fmt.Errorf("metadata %s value is not a string", change.Key)
		}
		source.Metadata[change.Key] = value
	}
	if patch.Status != nil {
		if source.Status != patch.Status.Expected {
			return errors.New("status expected mismatch")
		}
		source.Status = patch.Status.Value
	}
	if patch.Assignee != nil {
		if source.Assignee != patch.Assignee.Expected {
			return errors.New("assignee expected mismatch")
		}
		source.Assignee = patch.Assignee.Value
	}
	if patch.Labels != nil {
		return errors.New("test store does not implement labels")
	}
	return nil
}

func cloneTransitionTestBead(source beads.Bead) beads.Bead {
	cloned := source
	cloned.Metadata = make(map[string]string, len(source.Metadata))
	for key, value := range source.Metadata {
		cloned.Metadata[key] = value
	}
	cloned.Labels = append([]string(nil), source.Labels...)
	return cloned
}

func cloneTransitionPatchRequest(request beads.RevisionTransitionPatchRequest) beads.RevisionTransitionPatchRequest {
	cloned := request
	cloned.Patch.Metadata = append([]beads.RevisionTransitionMetadataPatch(nil), request.Patch.Metadata...)
	for index := range cloned.Patch.Metadata {
		cloned.Patch.Metadata[index].Expected = cloneRawMessagePointer(request.Patch.Metadata[index].Expected)
		cloned.Patch.Metadata[index].Value = cloneRawMessagePointer(request.Patch.Metadata[index].Value)
	}
	if request.Patch.Status != nil {
		value := *request.Patch.Status
		cloned.Patch.Status = &value
	}
	if request.Patch.Assignee != nil {
		value := *request.Patch.Assignee
		cloned.Patch.Assignee = &value
	}
	if request.Patch.Labels != nil {
		value := *request.Patch.Labels
		value.Expected = append([]string(nil), value.Expected...)
		value.Value = append([]string(nil), value.Value...)
		cloned.Patch.Labels = &value
	}
	return cloned
}

func cloneRawMessagePointer(raw *json.RawMessage) *json.RawMessage {
	if raw == nil {
		return nil
	}
	cloned := append(json.RawMessage(nil), (*raw)...)
	return &cloned
}
