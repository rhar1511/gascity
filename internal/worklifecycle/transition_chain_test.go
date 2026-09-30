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
		{step: TransitionStepReservation, operation: "reserve-1", patch: func(bead beads.Bead) SourceWorkPatch {
			return SourceWorkPatch{Metadata: map[string]MetadataStringPatch{
				beadmeta.LifecycleMaterializationMetadataKey: {Value: fixture.materializationValue("reserved", "")},
			}}
		}},
		{step: TransitionStepAttachedMaterialization, operation: "materialize-1", patch: func(bead beads.Bead) SourceWorkPatch {
			return fixture.attachedMaterializationPatch(bead)
		}},
		{step: TransitionStepClaimIdentity, operation: "claim-1", patch: func(bead beads.Bead) SourceWorkPatch {
			return claimIdentityTestPatch()
		}},
		{step: TransitionStepRecoveryBudget, operation: "recovery-1", patch: func(bead beads.Bead) SourceWorkPatch {
			return metadataTransition(bead, beadmeta.LifecycleRecoveryStateMetadataKey, `{"version":1,"attempts":[{"id":"00000000000000000000000000000001"}]}`)
		}},
		{step: TransitionStepCompletionBudget, operation: "completion-1", patch: func(bead beads.Bead) SourceWorkPatch {
			return metadataTransition(bead, beadmeta.LifecycleCompletionBudgetMetadataKey, `{"version":1,"reserved":true}`)
		}},
		{step: TransitionStepClose, operation: "close-1", patch: func(bead beads.Bead) SourceWorkPatch {
			patch := metadataTransition(bead, beadmeta.LifecycleCompletionReceiptMetadataKey, `{"version":1,"accepted":true}`)
			patch.Status = &StringTransition{Expected: "in_progress", Value: "closed"}
			return patch
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
		got, err := fixture.chain.Apply(request)
		if err != nil {
			t.Fatalf("Apply(%s): %v", tc.step, err)
		}
		if got.Receipt.ReceiptID == "" || got.Receipt.Kind != transitionStepKind(tc.step) || got.Receipt.ExpectedVersion == got.Receipt.ToVersion || got.Receipt.ToVersion == 0 {
			t.Fatalf("Apply(%s) receipt = %+v, want deterministic kind and backend ToVersion", tc.step, got.Receipt)
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
	if previous.Kind != transitionStepKind(TransitionStepClose) || previous.PriorReceiptID != fixture.store.patchRequests[len(steps)-2].ReceiptID {
		t.Fatalf("close receipt = %+v, want direct parent from completion budget", previous)
	}
	closed, err := fixture.store.DecisionFrontierSourceSnapshot(fixture.work.ID)
	if err != nil || closed.Status != "closed" {
		t.Fatalf("closed source = %+v err %v, want closed", closed, err)
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

	t.Run("changed current policy proof", func(t *testing.T) {
		fixture := newTransitionChainFixture(t)
		request := fixture.firstReservationRequest()
		request.Evidence.Policy.FormulaSources[0].SHA256 = strings.Repeat("9", 64)
		if _, err := fixture.chain.Apply(request); !errors.Is(err, ErrTransitionChainEvidence) {
			t.Fatalf("changed policy error = %v, want evidence refusal", err)
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
	copy := SourceWorkPatch{Metadata: make(map[string]MetadataStringPatch, len(patch.Metadata))}
	for key, value := range patch.Metadata {
		if value.Expected != nil {
			expected := *value.Expected
			value.Expected = &expected
		}
		copy.Metadata[key] = value
	}
	if patch.Status != nil {
		value := *patch.Status
		copy.Status = &value
	}
	if patch.Assignee != nil {
		value := *patch.Assignee
		copy.Assignee = &value
	}
	return copy
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

type transitionChainFixture struct {
	store      *transitionChainTestStore
	permits    *transitionChainPermitIssuer
	chain      *TransitionChain
	work       beads.Bead
	policy     AdmissionPolicyProjectionV2
	cfg        config.LifecycleConfig
	attachment AdmissionAttachmentProof
	evidence   TransitionEvidence
}

func newTransitionChainFixture(t *testing.T) *transitionChainFixture {
	t.Helper()
	policy := validAdmissionPolicyProjectionV2(t)
	scope := policy.SourceScope
	admissionPublic, admissionPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	acceptancePublic, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.LifecycleConfig{
		AdmissionEnabled:            true,
		AdmissionV2PrimaryAuthority: "triage",
		AdmissionV2Authorities:      map[string]string{"triage": base64.StdEncoding.EncodeToString(admissionPublic)},
		AcceptanceAuthorities:       map[string]string{"reviewer": base64.StdEncoding.EncodeToString(acceptancePublic)},
	}
	policyDigest, err := DigestAdmissionPolicyV2(policy)
	if err != nil {
		t.Fatal(err)
	}
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
		policy: policy, cfg: cfg, attachment: attachment,
		evidence: TransitionEvidence{Attachment: attachment, Policy: policy},
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
	}
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
	copy := source
	copy.Metadata = make(map[string]string, len(source.Metadata))
	for key, value := range source.Metadata {
		copy.Metadata[key] = value
	}
	copy.Labels = append([]string(nil), source.Labels...)
	return copy
}

func cloneTransitionPatchRequest(request beads.RevisionTransitionPatchRequest) beads.RevisionTransitionPatchRequest {
	copy := request
	copy.Patch.Metadata = append([]beads.RevisionTransitionMetadataPatch(nil), request.Patch.Metadata...)
	for index := range copy.Patch.Metadata {
		copy.Patch.Metadata[index].Expected = cloneRawMessagePointer(request.Patch.Metadata[index].Expected)
		copy.Patch.Metadata[index].Value = cloneRawMessagePointer(request.Patch.Metadata[index].Value)
	}
	if request.Patch.Status != nil {
		value := *request.Patch.Status
		copy.Patch.Status = &value
	}
	if request.Patch.Assignee != nil {
		value := *request.Patch.Assignee
		copy.Patch.Assignee = &value
	}
	if request.Patch.Labels != nil {
		value := *request.Patch.Labels
		value.Expected = append([]string(nil), value.Expected...)
		value.Value = append([]string(nil), value.Value...)
		copy.Patch.Labels = &value
	}
	return copy
}

func cloneRawMessagePointer(raw *json.RawMessage) *json.RawMessage {
	if raw == nil {
		return nil
	}
	copy := append(json.RawMessage(nil), (*raw)...)
	return &copy
}
