package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worklifecycle"
)

const (
	lifecycleRecoveryBudgetKind   = "lifecycle_source_recovery_budget_v1"
	lifecycleCompletionBudgetKind = "lifecycle_source_completion_budget_v1"
	lifecycleCloseKind            = "lifecycle_source_close_v1"
)

type lifecycleQ54ClaimFixture struct {
	provider *runtime.Fake
	info     session.Info
	legs     []classStoreCandidate
	leg      classStoreCandidate
}

func prepareLifecycleQ54Claim(t *testing.T, setup lifecycleAdmissionTransitionSetup) lifecycleQ54ClaimFixture {
	t.Helper()
	var admissionLogs strings.Builder
	reconcileLifecycleAdmissionWithPermitResolver(
		"pilot", setup.fixture.cityPath, setup.fixture.cfg, setup.cityStore, setup.rigStores, nil,
		&admissionLogs, setup.permitResolver,
	)
	legs, err := routedWorkStoreCandidates(setup.fixture.cityPath, setup.fixture.cfg, setup.cityStore, setup.rigStores, nil)
	if err != nil {
		t.Fatalf("resolve routed work stores: %v", err)
	}
	var leg classStoreCandidate
	for _, candidate := range legs {
		if candidate.ref == "rig:pilot" {
			leg = candidate
			break
		}
	}
	if leg.store == nil {
		t.Fatalf("routed work stores omitted rig:pilot: %+v", legs)
	}
	transition, err := buildLifecycleTransitionContext(
		"pilot", setup.fixture.cityPath, setup.fixture.cfg, setup.cityStore, setup.rigStores, legs, leg,
		setup.source.ID, setup.receipt.Scope, setup.permitResolver, nil,
	)
	if err != nil {
		t.Fatalf("build current Q54 transition context after admission: %v; admission logs=%s", err, admissionLogs.String())
	}
	provider := runtime.NewFake()
	manager := session.NewManagerWithOptions(setup.store, provider)
	info, err := manager.CreateSession(context.Background(), session.CreateOptions{
		Template: "worker", Command: "claude", WorkDir: t.TempDir(), Provider: "claude",
	})
	if err != nil {
		t.Fatalf("create live recovery session: %v", err)
	}
	claimGeneration := "17"
	claimPatch := worklifecycle.SourceWorkPatch{
		Metadata: map[string]worklifecycle.MetadataStringPatch{
			beadmeta.ClaimGenerationMetadataKey: {Value: claimGeneration},
			beadmeta.ClaimedAtMetadataKey:       {Value: time.Now().UTC().Format(time.RFC3339)},
			beadmeta.SessionIDMetadataKey:       {Value: info.ID},
		},
		Status:   &worklifecycle.StringTransition{Expected: "open", Value: "in_progress"},
		Assignee: &worklifecycle.StringTransition{Expected: "", Value: info.ID},
	}
	_, err = transition.chain.Apply(worklifecycle.TransitionRequest{
		IssueID: setup.source.ID, Step: worklifecycle.TransitionStepClaimIdentity,
		OperationID: lifecycleTransitionOperationID("claim-identity", setup.source.ID, setup.receipt.Scope,
			transition.admissionDigest, info.ID+"-"+claimGeneration),
		PriorReceiptID: transition.head.ReceiptID, Evidence: transition.evidence, Patch: claimPatch,
	})
	if err != nil {
		t.Fatalf("apply typed Q54 claim: %v", err)
	}
	front := session.NewStore(beads.SessionStore{Store: setup.store})
	if _, err := front.SetCurrentClaimForGeneration(info.ID, setup.source.ID, claimGeneration); err != nil {
		t.Fatalf("set reciprocal session claim: %v", err)
	}
	return lifecycleQ54ClaimFixture{provider: provider, info: info, legs: legs, leg: leg}
}

func setLifecycleRecoveryAuthority(t *testing.T, setup lifecycleAdmissionTransitionSetup) ed25519.PrivateKey {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	setup.fixture.cfg.Lifecycle.RecoveryEnabled = true
	setup.fixture.cfg.Lifecycle.RecoveryAuthorities = map[string]config.LifecycleRecoveryAuthority{
		"recovery": {
			PublicKey: base64.StdEncoding.EncodeToString(publicKey),
			Actions:   []string{"nudge"}, Scopes: []string{setup.receipt.Scope},
		},
	}
	return privateKey
}

func lifecycleRecoveryRequest(t *testing.T, setup lifecycleAdmissionTransitionSetup, claim lifecycleQ54ClaimFixture, privateKey ed25519.PrivateKey, requestID string, revision int64) worklifecycle.RecoveryRequest {
	t.Helper()
	request, err := worklifecycle.SignRecoveryRequest(worklifecycle.RecoveryRequest{
		Version: 1, RequestID: requestID, Action: "nudge", Scope: setup.receipt.Scope,
		WorkItemID: setup.source.ID, ExpectedRevision: revision, Owner: claim.info.ID,
		ClaimGeneration: "17", SessionID: claim.info.ID, SessionGeneration: claim.info.Generation,
		Message:   "Please report current progress and the exact verification result.",
		IssuedAt:  time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano),
		ExpiresAt: time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano), AuthorizedBy: "recovery",
	}, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	return request
}

func persistLifecycleRecoveryIntent(t *testing.T, store beads.Store, request worklifecycle.RecoveryRequest) {
	t.Helper()
	digest, err := worklifecycle.RecoveryRequestDigest(request)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := worklifecycle.PersistRecoveryIntent(store, "pilotrig", request, digest); err != nil {
		t.Fatalf("persist exact signed recovery intent: %v", err)
	}
}

func runLifecycleRecovery(setup lifecycleAdmissionTransitionSetup, claim lifecycleQ54ClaimFixture, stderr *strings.Builder) {
	reconcileLifecycleRecoveryRequestsWithPermitResolver(
		context.Background(), "pilot", setup.fixture.cityPath, setup.fixture.cfg, setup.cityStore, setup.rigStores,
		beads.SessionStore{Store: setup.store}, claim.provider, stderr, setup.permitResolver, nil,
	)
}

func typedLifecyclePatchCount(store *lifecycleAdmissionTransitionTestStore, kind string) int {
	store.mu.Lock()
	defer store.mu.Unlock()
	count := 0
	for _, request := range store.patchRequests {
		if request.Kind == kind {
			count++
		}
	}
	return count
}

func typedLifecycleReceiptCount(store *lifecycleAdmissionTransitionTestStore, kind string) int {
	store.mu.Lock()
	defer store.mu.Unlock()
	count := 0
	for _, receipt := range store.patchReceipts {
		if receipt.Kind == kind {
			count++
		}
	}
	return count
}

func TestLifecycleRecoveryQ54ReservesExactRequestBeforeOneDeliveryAndReplaysReadOnly(t *testing.T) {
	setup := newLifecycleAdmissionTransitionSetup(t)
	claim := prepareLifecycleQ54Claim(t, setup)
	privateKey := setLifecycleRecoveryAuthority(t, setup)
	work, err := setup.store.Get(setup.source.ID)
	if err != nil {
		t.Fatal(err)
	}
	request := lifecycleRecoveryRequest(t, setup, claim, privateKey, "q54-recovery-one", work.Revision)
	persistLifecycleRecoveryIntent(t, setup.store, request)

	var logs strings.Builder
	runLifecycleRecovery(setup, claim, &logs)
	if got := claim.provider.CountCalls("Nudge", claim.info.SessionName); got != 1 {
		t.Fatalf("first exact recovery request sent %d nudges, want one; logs=%s", got, logs.String())
	}
	state, _, reserved, err := worklifecycle.RecoveryRequestAttempt(setup.store, request, mustRecoveryRequestDigest(t, request))
	if err != nil || !reserved || len(state.Attempts) != 1 {
		t.Fatalf("exact request reservation state=%+v reserved=%t err=%v", state, reserved, err)
	}
	if typedLifecyclePatchCount(setup.store, lifecycleRecoveryBudgetKind) != 1 || setup.store.sourceGenericWrites != 0 {
		t.Fatalf("typed recovery budgets=%d generic enrolled writes=%d, want one and zero", typedLifecyclePatchCount(setup.store, lifecycleRecoveryBudgetKind), setup.store.sourceGenericWrites)
	}
	before := len(setup.store.patchRequests)
	runLifecycleRecovery(setup, claim, &logs)
	if got := claim.provider.CountCalls("Nudge", claim.info.SessionName); got != 1 || len(setup.store.patchRequests) != before {
		t.Fatalf("exact replay sent %d nudges or wrote another patch: writes %d->%d", got, before, len(setup.store.patchRequests))
	}
}

func TestLifecycleRecoveryQ54LostResponseConsumesBudgetWithoutDelivery(t *testing.T) {
	setup := newLifecycleAdmissionTransitionSetup(t)
	claim := prepareLifecycleQ54Claim(t, setup)
	privateKey := setLifecycleRecoveryAuthority(t, setup)
	work, err := setup.store.Get(setup.source.ID)
	if err != nil {
		t.Fatal(err)
	}
	request := lifecycleRecoveryRequest(t, setup, claim, privateKey, "q54-recovery-lost", work.Revision)
	persistLifecycleRecoveryIntent(t, setup.store, request)
	setup.store.loseNextPatchKind = lifecycleRecoveryBudgetKind

	var logs strings.Builder
	runLifecycleRecovery(setup, claim, &logs)
	if got := claim.provider.CountCalls("Nudge", claim.info.SessionName); got != 0 {
		t.Fatalf("lost budget response authorized %d nudges", got)
	}
	if typedLifecyclePatchCount(setup.store, lifecycleRecoveryBudgetKind) != 1 || setup.store.sourceGenericWrites != 0 {
		t.Fatalf("lost response budget receipts=%d generic enrolled writes=%d", typedLifecyclePatchCount(setup.store, lifecycleRecoveryBudgetKind), setup.store.sourceGenericWrites)
	}
	runLifecycleRecovery(setup, claim, &logs)
	if got := claim.provider.CountCalls("Nudge", claim.info.SessionName); got != 0 {
		t.Fatalf("restart after recovered lost response sent %d nudges", got)
	}
}

func TestLifecycleRecoveryQ54RechecksWorkflowAtEffectBoundary(t *testing.T) {
	setup := newLifecycleAdmissionTransitionSetup(t)
	claim := prepareLifecycleQ54Claim(t, setup)
	privateKey := setLifecycleRecoveryAuthority(t, setup)
	work, err := setup.store.Get(setup.source.ID)
	if err != nil {
		t.Fatal(err)
	}
	marker, ok := lifecycleMaterializationFor(work)
	if !ok || marker.State != "attached" {
		t.Fatalf("recovery source has no attached workflow marker: %+v", marker)
	}
	workflow, err := setup.store.Get(marker.WorkflowID)
	if err != nil {
		t.Fatalf("read attached workflow root: %v", err)
	}
	request := lifecycleRecoveryRequest(t, setup, claim, privateKey, "q54-workflow-race", work.Revision)
	persistLifecycleRecoveryIntent(t, setup.store, request)
	setup.store.afterNextPatch = func(kind string) {
		if kind != lifecycleRecoveryBudgetKind {
			return
		}
		closed := "closed"
		if err := setup.store.MemStore.Update(workflow.ID, beads.UpdateOpts{Status: &closed}); err != nil {
			t.Errorf("close attached workflow after recovery reservation: %v", err)
		}
	}

	var logs strings.Builder
	runLifecycleRecovery(setup, claim, &logs)
	current, err := setup.store.Get(work.ID)
	if err != nil {
		t.Fatal(err)
	}
	workflow, err = setup.store.Get(workflow.ID)
	if err != nil {
		t.Fatal(err)
	}
	if workflow.Status != "closed" || current.Status != "in_progress" ||
		current.Revision == work.Revision || typedLifecyclePatchCount(setup.store, lifecycleRecoveryBudgetKind) != 1 {
		t.Fatalf("race fixture did not close workflow after consuming budget: work status/revision=%s/%d -> %d workflow=%s", current.Status, work.Revision, current.Revision, workflow.Status)
	}
	setup.store.mu.Lock()
	var budgetReceipt beads.RevisionTransitionPatchReceipt
	for _, receipt := range setup.store.patchReceipts {
		if receipt.Kind == lifecycleRecoveryBudgetKind {
			budgetReceipt = receipt
			break
		}
	}
	setup.store.mu.Unlock()
	if budgetReceipt.ReceiptID == "" || current.Metadata[beadmeta.LifecycleTransitionHeadMetadataKey] != budgetReceipt.ReceiptID ||
		current.Revision != budgetReceipt.ToVersion {
		t.Fatalf("workflow mutation changed the source Q54 head: source revision/head=%d/%s budget receipt=%+v", current.Revision,
			current.Metadata[beadmeta.LifecycleTransitionHeadMetadataKey], budgetReceipt)
	}
	if got := claim.provider.CountCalls("Nudge", claim.info.SessionName); got != 0 {
		t.Fatalf("recovery sent %d nudges after attached workflow ceased to be current", got)
	}
	if !strings.Contains(logs.String(), "lost a prerequisite before delivery") {
		t.Fatalf("controller did not report the final workflow fence: %s", logs.String())
	}
}

func TestLifecycleRecoveryQ54HoldsStaleRevisionAndTamperedHead(t *testing.T) {
	for _, tc := range []struct {
		name       string
		tamperHead bool
	}{
		{name: "stale signed revision"},
		{name: "tampered current head", tamperHead: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setup := newLifecycleAdmissionTransitionSetup(t)
			claim := prepareLifecycleQ54Claim(t, setup)
			privateKey := setLifecycleRecoveryAuthority(t, setup)
			work, err := setup.store.Get(setup.source.ID)
			if err != nil {
				t.Fatal(err)
			}
			revision := work.Revision
			if tc.tamperHead {
				work.Metadata[beadmeta.LifecycleTransitionHeadMetadataKey] = "q54-forged-head"
				setup.store.mu.Lock()
				setup.store.rows[work.ID] = work
				setup.store.mu.Unlock()
			} else {
				revision--
			}
			request := lifecycleRecoveryRequest(t, setup, claim, privateKey, "q54-recovery-stale", revision)
			if tc.tamperHead {
				persistLifecycleRecoveryIntent(t, setup.store, request)
			} else {
				seedLifecycleRecoveryIntent(t, setup.store, request)
			}
			before := len(setup.store.patchRequests)
			var logs strings.Builder
			runLifecycleRecovery(setup, claim, &logs)
			if got := claim.provider.CountCalls("Nudge", claim.info.SessionName); got != 0 || typedLifecyclePatchCount(setup.store, lifecycleRecoveryBudgetKind) != 0 {
				t.Fatalf("stale evidence sent %d nudges or reserved a budget", got)
			}
			if len(setup.store.patchRequests) != before || setup.store.sourceGenericWrites != 0 {
				t.Fatalf("stale evidence wrote patches %d->%d and generic=%d", before, len(setup.store.patchRequests), setup.store.sourceGenericWrites)
			}
		})
	}
}

func seedLifecycleRecoveryIntent(t *testing.T, store beads.Store, request worklifecycle.RecoveryRequest) {
	t.Helper()
	digest := mustRecoveryRequestDigest(t, request)
	intent := worklifecycle.RecoveryIntent{Version: 1, Digest: digest, Request: request}
	raw, err := json.Marshal(intent)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Create(beads.Bead{
		ID: "pilot-rig-stale-recovery-intent", Title: "Stale signed recovery request", Type: "lifecycle-intent",
		Labels: []string{"hold:external"}, Metadata: map[string]string{
			beadmeta.LifecycleRecoveryIntentMetadataKey:  string(raw),
			beadmeta.LifecycleRecoveryIntentDigestKey:    digest,
			beadmeta.LifecycleRecoveryIntentWorkItemKey:  request.WorkItemID,
			beadmeta.LifecycleRecoveryIntentScopeKey:     request.Scope,
			beadmeta.LifecycleRecoveryIntentRequestIDKey: request.RequestID,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
}

func mustRecoveryRequestDigest(t *testing.T, request worklifecycle.RecoveryRequest) string {
	t.Helper()
	digest, err := worklifecycle.RecoveryRequestDigest(request)
	if err != nil {
		t.Fatal(err)
	}
	return digest
}

func TestLifecycleRecoveryQ54ExhaustionDoesNotUseLegacyRecoveryCAS(t *testing.T) {
	setup := newLifecycleAdmissionTransitionSetup(t)
	claim := prepareLifecycleQ54Claim(t, setup)
	privateKey := setLifecycleRecoveryAuthority(t, setup)
	var logs strings.Builder
	for index := 1; index <= worklifecycle.MaxRecoveryAttempts; index++ {
		work, err := setup.store.Get(setup.source.ID)
		if err != nil {
			t.Fatal(err)
		}
		request := lifecycleRecoveryRequest(t, setup, claim, privateKey, "q54-exhaustion-"+strconv.Itoa(index), work.Revision)
		persistLifecycleRecoveryIntent(t, setup.store, request)
		runLifecycleRecovery(setup, claim, &logs)
	}
	work, err := setup.store.Get(setup.source.ID)
	if err != nil {
		t.Fatal(err)
	}
	var state worklifecycle.RecoveryState
	err = json.Unmarshal([]byte(work.Metadata[beadmeta.LifecycleRecoveryStateMetadataKey]), &state)
	if err != nil || state.WorkItemID != work.ID || state.Scope != setup.receipt.Scope || len(state.Attempts) != worklifecycle.MaxRecoveryAttempts {
		t.Fatalf("exhausted typed recovery state=%+v err=%v", state, err)
	}
	if !strings.Contains(logs.String(), "signed escalation authorization is unavailable") {
		t.Fatalf("exhausted admitted work was not held for signed escalation authority: %s", logs.String())
	}
	if got := typedLifecyclePatchCount(setup.store, lifecycleRecoveryBudgetKind); got != worklifecycle.MaxRecoveryAttempts || setup.store.sourceGenericWrites != 0 {
		t.Fatalf("Q54 recovery budget receipts=%d generic enrolled writes=%d", got, setup.store.sourceGenericWrites)
	}
}

func TestLifecycleCompletionQ54ClosesExactReceiptWhenRecoveryIsDisabled(t *testing.T) {
	setup := newLifecycleAdmissionTransitionSetupWithCompletion(t, true)
	claim := prepareLifecycleQ54Claim(t, setup)
	setup.fixture.cfg.Lifecycle.RecoveryEnabled = false
	var logs strings.Builder
	reconcileLifecycleCompletionsWithPermitResolver(
		"pilot", setup.fixture.cityPath, setup.fixture.cfg, setup.cityStore, setup.rigStores, nil,
		&logs, setup.permitResolver, nil,
	)
	closed, err := setup.store.Get(setup.source.ID)
	if err != nil {
		t.Fatal(err)
	}
	if closed.Status != "closed" || closed.Metadata[beadmeta.LifecycleCompletionReceiptMetadataKey] != setup.source.Metadata[beadmeta.LifecycleCompletionReceiptMetadataKey] {
		t.Fatalf("verified completion source=%+v; logs=%s", closed, logs.String())
	}
	if typedLifecyclePatchCount(setup.store, lifecycleCompletionBudgetKind) != 1 || typedLifecyclePatchCount(setup.store, lifecycleCloseKind) != 1 {
		t.Fatalf("completion budget=%d close=%d, want one each", typedLifecyclePatchCount(setup.store, lifecycleCompletionBudgetKind), typedLifecyclePatchCount(setup.store, lifecycleCloseKind))
	}
	if setup.store.sourceGenericWrites != 0 {
		t.Fatalf("completion used %d generic enrolled writes", setup.store.sourceGenericWrites)
	}
	if len(claim.legs) == 0 || claim.leg.store == nil {
		t.Fatal("completion fixture lost its exact routed source store")
	}
}

func TestLifecycleCompletionQ54HoldsAcceptedOpenWorkAndTamperedReceipt(t *testing.T) {
	t.Run("accepted but unclaimed open work remains open", func(t *testing.T) {
		setup := newLifecycleAdmissionTransitionSetupWithCompletion(t, true)
		var admissionLogs strings.Builder
		reconcileLifecycleAdmissionWithPermitResolver(
			"pilot", setup.fixture.cityPath, setup.fixture.cfg, setup.cityStore, setup.rigStores, nil,
			&admissionLogs, setup.permitResolver,
		)
		var logs strings.Builder
		reconcileLifecycleCompletionsWithPermitResolver(
			"pilot", setup.fixture.cityPath, setup.fixture.cfg, setup.cityStore, setup.rigStores, nil,
			&logs, setup.permitResolver, nil,
		)
		current, err := setup.store.Get(setup.source.ID)
		if err != nil {
			t.Fatal(err)
		}
		if current.Status != "open" || typedLifecyclePatchCount(setup.store, lifecycleCompletionBudgetKind) != 0 ||
			typedLifecyclePatchCount(setup.store, lifecycleCloseKind) != 0 || setup.store.sourceGenericWrites != 0 {
			t.Fatalf("accepted unclaimed work changed: status=%s completion budgets=%d closes=%d generic=%d", current.Status,
				typedLifecyclePatchCount(setup.store, lifecycleCompletionBudgetKind), typedLifecyclePatchCount(setup.store, lifecycleCloseKind), setup.store.sourceGenericWrites)
		}
	})

	t.Run("tampered receipt cannot consume budget or close", func(t *testing.T) {
		setup := newLifecycleAdmissionTransitionSetupWithCompletion(t, true)
		prepareLifecycleQ54Claim(t, setup)
		current, err := setup.store.Get(setup.source.ID)
		if err != nil {
			t.Fatal(err)
		}
		current.Metadata[beadmeta.LifecycleCompletionReceiptMetadataKey] = strings.Replace(
			current.Metadata[beadmeta.LifecycleCompletionReceiptMetadataKey], "commit:reviewed", "commit:forged", 1,
		)
		setup.store.mu.Lock()
		setup.store.rows[current.ID] = current
		setup.store.mu.Unlock()
		before := len(setup.store.patchRequests)
		var logs strings.Builder
		reconcileLifecycleCompletionsWithPermitResolver(
			"pilot", setup.fixture.cityPath, setup.fixture.cfg, setup.cityStore, setup.rigStores, nil,
			&logs, setup.permitResolver, nil,
		)
		if typedLifecyclePatchCount(setup.store, lifecycleCompletionBudgetKind) != 0 || typedLifecyclePatchCount(setup.store, lifecycleCloseKind) != 0 ||
			len(setup.store.patchRequests) != before || setup.store.sourceGenericWrites != 0 {
			t.Fatalf("tampered completion receipt reached a source transition: budget=%d close=%d patches=%d->%d generic=%d",
				typedLifecyclePatchCount(setup.store, lifecycleCompletionBudgetKind), typedLifecyclePatchCount(setup.store, lifecycleCloseKind),
				before, len(setup.store.patchRequests), setup.store.sourceGenericWrites)
		}
	})
}

func TestLifecycleCompletionQ54RestartsAfterBudgetAndRecoversLostCloseResponse(t *testing.T) {
	setup := newLifecycleAdmissionTransitionSetupWithCompletion(t, true)
	prepareLifecycleQ54Claim(t, setup)
	setup.store.failNextPatchKind = lifecycleCloseKind
	var logs strings.Builder
	reconcileLifecycleCompletionsWithPermitResolver(
		"pilot", setup.fixture.cityPath, setup.fixture.cfg, setup.cityStore, setup.rigStores, nil,
		&logs, setup.permitResolver, nil,
	)
	current, err := setup.store.Get(setup.source.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Status != "in_progress" || typedLifecycleReceiptCount(setup.store, lifecycleCompletionBudgetKind) != 1 ||
		typedLifecycleReceiptCount(setup.store, lifecycleCloseKind) != 0 {
		t.Fatalf("first pass should leave a consumed budget and in_progress source: status=%s budget=%d close=%d logs=%s",
			current.Status, typedLifecyclePatchCount(setup.store, lifecycleCompletionBudgetKind), typedLifecyclePatchCount(setup.store, lifecycleCloseKind), logs.String())
	}
	setup.store.loseNextPatchKind = lifecycleCloseKind
	reconcileLifecycleCompletionsWithPermitResolver(
		"pilot", setup.fixture.cityPath, setup.fixture.cfg, setup.cityStore, setup.rigStores, nil,
		&logs, setup.permitResolver, nil,
	)
	current, err = setup.store.Get(setup.source.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Status != "closed" || typedLifecycleReceiptCount(setup.store, lifecycleCompletionBudgetKind) != 1 ||
		typedLifecycleReceiptCount(setup.store, lifecycleCloseKind) != 1 ||
		typedLifecyclePatchCount(setup.store, lifecycleCompletionBudgetKind) != 1 ||
		typedLifecyclePatchCount(setup.store, lifecycleCloseKind) != 2 || setup.store.sourceGenericWrites != 0 {
		t.Fatalf("restart/lost-close recovery status=%s budgets=%d close calls=%d generic=%d logs=%s", current.Status,
			typedLifecycleReceiptCount(setup.store, lifecycleCompletionBudgetKind), typedLifecyclePatchCount(setup.store, lifecycleCloseKind),
			setup.store.sourceGenericWrites, logs.String())
	}
	if len(setup.store.patchReceipts) < 4 {
		t.Fatalf("restart did not retain typed admission, claim, budget, and close receipts: %d", len(setup.store.patchReceipts))
	}
}
