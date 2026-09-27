package api

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/githubmonitor"
)

func TestPRActionQueueJoinsFreshForgeStateWorkAndImmutableEvidence(t *testing.T) {
	fx := newPRActionFixture(t, true)
	queue, err := fx.service.Queue(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if queue.Availability != PRActionAvailabilityReady || len(queue.Items) != 1 {
		t.Fatalf("queue = %+v", queue)
	}
	item := queue.Items[0]
	if item.HeadSHA != fx.pullRequest.HeadSHA || item.BaseSHA != fx.pullRequest.BaseSHA || item.PolicyVersion != fx.policy.Version {
		t.Fatalf("queue item did not bind current forge revisions/policy: %+v", item)
	}
	if len(item.WorkRecords) != 1 || item.WorkRecords[0].ID != fx.work.ID {
		t.Fatalf("work records = %+v, want server-loaded work %s", item.WorkRecords, fx.work.ID)
	}
	if !item.WorkRecords[0].CurrentRevision || item.WorkRecords[0].BaseSHA != item.BaseSHA {
		t.Fatalf("work record did not bind the current base/head pair: %+v", item.WorkRecords[0])
	}
	if item.EvidenceState != PRActionEvidenceVerified || len(item.AttemptEvidence) != 1 {
		t.Fatalf("attempt evidence = %+v state=%s", item.AttemptEvidence, item.EvidenceState)
	}
	if !item.HasAction(PRActionQueueReview) || !item.HasAction(PRActionMerge) {
		t.Fatalf("verified exact evidence should allow review and human-gated merge: %+v", item.Actions)
	}
	if !item.ActionRequiresHumanApproval(PRActionMerge) {
		t.Fatalf("merge action did not require explicit human approval: %+v", item.Actions)
	}
}

func TestPRActionQueueShowsExactDurableReviewSubmission(t *testing.T) {
	fx := newPRActionFixture(t, true)
	request := fx.actionRequest(PRActionQueueReview)
	request.IdempotencyKey = "review-receipt-roundtrip-1"
	submitted, err := fx.service.Execute(context.Background(), request, fx.workerActor())
	if err != nil {
		t.Fatal(err)
	}
	freshService := NewPRActionService(PRActionServiceOptions{
		State: fx.state, Forge: fx.forge, Evidence: fx.evidence, HumanVerifier: fx.verifier,
		Policy: fx.policy, Now: func() time.Time { return fx.now },
	})
	queue, err := freshService.Queue(context.Background())
	if err != nil || len(queue.Items) != 1 {
		t.Fatalf("fresh service queue = %+v err=%v", queue, err)
	}
	item := queue.Items[0]
	if !item.HasAction(PRActionQueueReview) {
		t.Fatal("an exact review submission incorrectly replaced current eligibility")
	}
	if len(item.ActionReceipts) != 1 {
		t.Fatalf("action receipts = %+v, want the persisted review submission", item.ActionReceipts)
	}
	receipt := item.ActionReceipts[0]
	if receipt.ID != submitted.ID || receipt.Action != PRActionQueueReview || receipt.Status != PRActionStatusVerified || receipt.Outcome != PRActionOutcomeReviewQueued || receipt.WorkID != request.WorkID || receipt.AttemptID != request.AttemptID || receipt.HeadSHA != request.HeadSHA || receipt.BaseSHA != request.BaseSHA || receipt.PolicyVersion != request.PolicyVersion {
		t.Fatalf("fresh queue did not expose exact review receipt: %+v", receipt)
	}
}

func TestPRActionQueueDoesNotTreatSameHeadOnOldBaseAsCurrentWork(t *testing.T) {
	fx := newPRActionFixture(t, true)
	fx.forge.pullRequests[0].MergeStateStatus = "BEHIND"
	if err := fx.store.SetMetadata(fx.work.ID, "github.base_sha", strings.Repeat("c", 40)); err != nil {
		t.Fatal(err)
	}
	queue, err := fx.service.Queue(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	item := queue.Items[0]
	if item.WorkRecords[0].CurrentRevision || item.WorkRecords[0].BaseSHA == item.BaseSHA {
		t.Fatalf("work from a prior base was marked current: %+v", item.WorkRecords[0])
	}
	if !item.HasAction(PRActionPrepare) {
		t.Fatalf("changed base should require fresh repair work: %+v", item.Actions)
	}
}

func TestPRActionQueueRequiresExactConfiguredBaseBranchCase(t *testing.T) {
	fx := newPRActionFixture(t, true)
	fx.policy.Monitors[0].BaseBranches = []string{"Release"}
	fx.forge.pullRequests[0].BaseRefName = "release"
	queue, err := fx.service.Queue(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(queue.Items) != 0 {
		t.Fatalf("case-different git ref was authorized by base allowlist: %+v", queue.Items)
	}
}

func TestPRActionFreshQueueMustRetainTheSelectedAttempt(t *testing.T) {
	fx := newPRActionFixture(t, true)
	request := fx.actionRequest(PRActionQueueReview)
	selected := fx.evidence.refs[0]
	other := selected
	other.AttemptID = "ae-another-attempt"
	fx.evidence.refsOnCall = 2
	fx.evidence.nextRefs = []PRActionAttemptReference{other}
	fx.evidence.records[other.AttemptID] = fx.evidence.records[selected.AttemptID]
	result, err := fx.service.Execute(context.Background(), request, fx.workerActor())
	if !errors.Is(err, ErrPRActionEvidenceMissing) {
		t.Fatalf("fresh selected-attempt error = %v, want evidence missing", err)
	}
	if result.Status != PRActionStatusRejected {
		t.Fatalf("lost selected attempt was not recorded as rejected: %+v", result)
	}
}

func TestPRMergeRequiredCheckNamesAreCaseSensitive(t *testing.T) {
	pr := githubmonitor.PullRequest{
		MergeStateStatus: "CLEAN",
		Checks:           []githubmonitor.Check{{Name: "ci", Status: "COMPLETED", Conclusion: "SUCCESS"}},
	}
	if prMergeReady(pr, []string{"CI"}) {
		t.Fatal("a differently cased check name satisfied the signed required-check policy")
	}
}

func TestPRActionQueueKeepsUnavailableSourcesDistinctAndFailsClosed(t *testing.T) {
	fx := newPRActionFixture(t, false)
	fx.state.stores["myrig"] = nil
	queue, err := fx.service.Queue(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if queue.Availability != PRActionAvailabilityUnknown || len(queue.Sources) != 1 || queue.Sources[0].State != PRActionSourceUnavailable {
		t.Fatalf("unavailable bead store was flattened into an empty queue: %+v", queue)
	}
	if len(queue.Items) != 0 {
		t.Fatalf("unavailable work source exposed actionable items: %+v", queue.Items)
	}

	fx = newPRActionFixture(t, true)
	fx.evidence.readErr = errors.New("attempt archive unavailable")
	queue, err = fx.service.Queue(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(queue.Items) != 1 || queue.Items[0].EvidenceState != PRActionEvidenceUnavailable {
		t.Fatalf("unavailable evidence must remain explicit: %+v", queue)
	}
	if queue.Items[0].HasAction(PRActionQueueReview) || queue.Items[0].HasAction(PRActionMerge) {
		t.Fatalf("unavailable evidence allowed revision-bound action: %+v", queue.Items[0].Actions)
	}
}

func TestPRActionPolicyCannotBeExpandedByCityConfigAndRequiresNamedChecks(t *testing.T) {
	fx := newPRActionFixture(t, true)
	fx.state.cfg.GitHub.PRMonitors = []config.GitHubPRMonitor{{
		Name: "worker-added", Owner: "attacker", Repo: "repo", BaseBranches: []string{"main"}, Rig: "myrig",
	}}
	queue, err := fx.service.Queue(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(queue.Items) != 1 || queue.Items[0].Owner != "acme" || queue.Items[0].Repo != "widget" || queue.Items[0].PolicyVersion != fx.policy.Version {
		t.Fatalf("candidate city config changed the signed host policy queue: %+v", queue)
	}

	fx.forge.pullRequests[0].Checks = nil
	queue, err = fx.service.Queue(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if queue.Items[0].HasAction(PRActionMerge) {
		t.Fatalf("policy with required check ci allowed merge when checks were absent: %+v", queue.Items[0].Actions)
	}
}

func TestPRActionRevalidatesRevisionAndPolicyBeforeCreatingAction(t *testing.T) {
	fx := newPRActionFixture(t, true)
	request := fx.actionRequest(PRActionQueueReview)
	fx.forge.pullRequests[0].BaseSHA = strings.Repeat("c", 40)
	if _, err := fx.service.Execute(context.Background(), request, fx.workerActor()); !errors.Is(err, ErrPRActionStale) {
		t.Fatalf("changed base error = %v, want stale action", err)
	}
	if got := fx.actionRecordCount(); got != 0 {
		t.Fatalf("stale action persisted %d action records before validation", got)
	}

	fx = newPRActionFixture(t, true)
	request = fx.actionRequest(PRActionQueueReview)
	request.PolicyVersion = "candidate-selected-policy"
	if _, err := fx.service.Execute(context.Background(), request, fx.workerActor()); !errors.Is(err, ErrPRActionStale) {
		t.Fatalf("changed policy error = %v, want stale action", err)
	}
}

func TestPRActionQueueReviewIsDurableAndIdempotent(t *testing.T) {
	fx := newPRActionFixture(t, true)
	request := fx.actionRequest(PRActionQueueReview)
	first, err := fx.service.Execute(context.Background(), request, fx.workerActor())
	if err != nil {
		t.Fatal(err)
	}
	second, err := fx.service.Execute(context.Background(), request, fx.workerActor())
	if err != nil {
		t.Fatal(err)
	}
	if first.ID == "" || first.ID != second.ID || first.Status != PRActionStatusVerified || second.Status != PRActionStatusVerified {
		t.Fatalf("action replay = first %+v second %+v", first, second)
	}
	if got := fx.actionRecordCount(); got != 1 {
		t.Fatalf("durable action record count = %d, want one", got)
	}
	ready, err := fx.store.Ready()
	if err != nil {
		t.Fatal(err)
	}
	for _, bead := range ready {
		if bead.Metadata[prActionSourceMetadataKey] == prActionRecordSource {
			t.Fatalf("durable action receipt %s became a Ready candidate: %+v", bead.ID, bead)
		}
	}
	if len(fx.forge.mergeCalls) != 0 {
		t.Fatalf("review queueing unexpectedly called merge: %+v", fx.forge.mergeCalls)
	}
}

func TestPRActionConcurrentRetriesShareOneDurableOutcome(t *testing.T) {
	fx := newPRActionFixture(t, true)
	request := fx.actionRequest(PRActionQueueReview)
	const callers = 8
	results := make([]PRActionResult, callers)
	errs := make([]error, callers)
	var wg sync.WaitGroup
	for i := range callers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = fx.service.Execute(context.Background(), request, fx.workerActor())
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("caller %d: %v", i, err)
		}
		if results[i].ID == "" || results[i].ID != results[0].ID || results[i].Status != PRActionStatusVerified {
			t.Fatalf("caller %d observed a different action result: %+v versus %+v", i, results[i], results[0])
		}
	}
	if got := fx.actionRecordCount(); got != 1 {
		t.Fatalf("concurrent retries created %d durable action records, want one", got)
	}
}

func TestPRActionDistinctServicesShareDurableIdempotencyAdmission(t *testing.T) {
	fx := newPRActionFixture(t, true)
	request := fx.actionRequest(PRActionQueueReview)
	fx.forge.listBarrier = newPRActionListBarrier(2)
	second := NewPRActionService(PRActionServiceOptions{
		State: fx.state, Forge: fx.forge, Evidence: fx.evidence, HumanVerifier: fx.verifier, Policy: fx.policy,
		Now: func() time.Time { return fx.now },
	})
	services := []*PRActionService{fx.service, second}
	results := make([]PRActionResult, len(services))
	errs := make([]error, len(services))
	var wg sync.WaitGroup
	for i, service := range services {
		wg.Add(1)
		go func(i int, service *PRActionService) {
			defer wg.Done()
			results[i], errs[i] = service.Execute(context.Background(), request, fx.workerActor())
		}(i, service)
	}
	wg.Wait()
	winners := 0
	var winnerID string
	for i, err := range errs {
		if err == nil {
			winners++
			if results[i].Status != PRActionStatusVerified || results[i].ID == "" {
				t.Fatalf("service %d returned unverified result: %+v", i, results[i])
			}
			if winnerID != "" && results[i].ID != winnerID {
				t.Fatalf("services returned different durable action IDs: %q and %q", winnerID, results[i].ID)
			}
			winnerID = results[i].ID
			continue
		}
		if !errors.Is(err, ErrPRActionInProgress) {
			t.Fatalf("service %d error = %v, want pending durable claim", i, err)
		}
	}
	if winners == 0 {
		t.Fatalf("no service obtained the durable action result: results=%+v errors=%v", results, errs)
	}
	if got := fx.actionRecordCount(); got != 1 {
		t.Fatalf("distinct services created %d action records for one key, want one", got)
	}
}

func TestPRActionUnavailableWithoutStableCreateAndConditionalCAS(t *testing.T) {
	fx := newPRActionFixture(t, true)
	fx.state.stores["myrig"].(*beads.MemStore).HonorExplicitIDs = false
	queue, err := fx.service.Queue(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if queue.Availability != PRActionAvailabilityUnknown || queue.Sources[0].State != PRActionSourceUnavailable || len(queue.Items) != 0 {
		t.Fatalf("store without unique durable admission was exposed as actionable: %+v", queue)
	}
	if _, err := fx.service.Execute(context.Background(), fx.actionRequest(PRActionQueueReview), fx.workerActor()); !errors.Is(err, ErrPRActionUnavailable) {
		t.Fatalf("Execute error = %v, want fail-closed unavailable", err)
	}
	if got := fx.actionRecordCount(); got != 0 {
		t.Fatalf("unsupported store wrote %d action records", got)
	}
}

func TestPRActionPreparePersistsWorkTransitionBeforeReportingSuccess(t *testing.T) {
	fx := newPRActionFixture(t, false)
	fx.forge.pullRequests[0].MergeStateStatus = "BLOCKED"
	request := fx.actionRequest(PRActionPrepare)
	request.WorkID = ""
	first, err := fx.service.Execute(context.Background(), request, fx.workerActor())
	if err != nil {
		t.Fatal(err)
	}
	second, err := fx.service.Execute(context.Background(), request, fx.workerActor())
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != second.ID || first.Status != PRActionStatusVerified || first.WorkID == "" || first.WorkID != second.WorkID {
		t.Fatalf("prepare replay = first %+v second %+v", first, second)
	}
	works, err := fx.store.List(beads.ListQuery{Metadata: map[string]string{
		"source": "github-pr-monitor", "github.owner": request.Owner, "github.repo": request.Repo,
		"github.pr": "12", "github.head_sha": request.HeadSHA, "github.base_sha": request.BaseSHA,
	}, IncludeClosed: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(works) != 1 {
		t.Fatalf("prepared work records = %d, want one durable transition", len(works))
	}
	prepared, err := fx.store.Get(first.WorkID)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Type != "task" || !hasPRActionExternalHold(prepared.Labels) || prepared.Metadata[prActionRouteProposalKey] != "myrig/worker" || prepared.Metadata[beadmeta.RoutedToMetadataKey] != "" {
		t.Fatalf("prepared task escaped triage hold or gained a serving route: %+v", prepared)
	}
	ready, err := fx.store.Ready()
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range ready {
		if candidate.ID == prepared.ID {
			t.Fatalf("held PR triage candidate appeared in Ready: %+v", candidate)
		}
	}
}

func TestPRActionExecuteKeepsLedgerAndPreparedWorkOnMonitorRig(t *testing.T) {
	fx := newPRActionFixture(t, false)
	fx.forge.pullRequests[0].MergeStateStatus = "BEHIND"
	rigStore := fx.store
	cityStore := beads.NewMemStore()
	graphStore := beads.NewMemStore()
	sessionStore := beads.NewMemStore()
	orderStore := beads.NewMemStore()
	nudgeStore := beads.NewMemStore()
	fx.state.cityBeadStore = cityStore
	fx.state.graphBeadStore = graphStore
	fx.state.sessionsBeadStore = sessionStore
	fx.state.ordersBeadStore = orderStore
	fx.state.nudgesBeadStore = nudgeStore

	request := fx.actionRequest(PRActionPrepare)
	request.WorkID = ""
	result, err := fx.service.Execute(context.Background(), request, fx.workerActor())
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != PRActionStatusVerified || result.ID == "" || result.WorkID == "" {
		t.Fatalf("prepare result = %+v, want verified action and work records", result)
	}

	ledger, err := rigStore.Get(result.ID)
	if err != nil {
		t.Fatalf("action ledger is not in selected rig store: %v", err)
	}
	if ledger.Type != "gate" || ledger.Metadata[prActionSourceMetadataKey] != prActionRecordSource || coordclass.Classify(ledger) != coordclass.ClassWork {
		t.Fatalf("action ledger is not work-class by construction: %+v class=%s", ledger, coordclass.Classify(ledger))
	}
	work, err := rigStore.Get(result.WorkID)
	if err != nil {
		t.Fatalf("prepared work is not in selected rig store: %v", err)
	}
	if work.Type != "task" || !hasPRActionExternalHold(work.Labels) || coordclass.Classify(work) != coordclass.ClassWork {
		t.Fatalf("prepared work is not held work-class triage: %+v class=%s", work, coordclass.Classify(work))
	}

	for name, store := range map[string]beads.Store{
		"city": cityStore, "graph": graphStore, "sessions": sessionStore, "orders": orderStore, "nudges": nudgeStore,
	} {
		for _, id := range []string{result.ID, result.WorkID} {
			if _, err := store.Get(id); err == nil {
				t.Errorf("%s store also contains monitor-rig record %s", name, id)
			}
		}
	}
}

func TestPRActionPrepareDifferentIdempotencyKeysShareOneSemanticWorkRecord(t *testing.T) {
	fx := newPRActionFixture(t, false)
	fx.forge.pullRequests[0].MergeStateStatus = "BEHIND"
	actor := fx.workerActor()
	requests := []PRActionRequest{fx.actionRequest(PRActionPrepare), fx.actionRequest(PRActionPrepare)}
	requests[0].IdempotencyKey = "prepare-target-alpha"
	requests[1].IdempotencyKey = "prepare-target-bravo"
	for _, request := range requests {
		fingerprint, err := prActionFingerprint(request, actor)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := createPRActionIntent(fx.store, request, actor, fingerprint, fx.now, nil); err != nil {
			t.Fatal(err)
		}
	}
	queue, err := fx.service.Queue(context.Background())
	if err != nil || len(queue.Items) != 1 {
		t.Fatalf("queue before concurrent prepares = %+v err=%v", queue, err)
	}
	monitor, ok := findPRActionMonitor(fx.policy.Monitors, requests[0].Monitor)
	if !ok {
		t.Fatal("test policy lost its monitor")
	}
	store := &prRepairCreateBarrierStore{Store: fx.store, target: fx.store, barrier: newPRRepairCreateBarrier(2)}
	if !beads.StableCreateIDFor(store) {
		t.Fatal("repair store wrapper did not preserve stable-ID capability")
	}
	results := make([]beads.Bead, 2)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := range requests {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = ensurePRRepairWork(store, monitor.githubMonitor(), queue.Items[0])
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("prepare intent %q: %v", requests[i].IdempotencyKey, err)
		}
	}
	if results[0].ID == "" || results[0].ID != results[1].ID || results[0].ID != prRepairWorkBeadID(queue.Items[0]) {
		t.Fatalf("distinct idempotency keys created different semantic work: %+v", results)
	}
	works, err := fx.store.List(beads.ListQuery{Metadata: map[string]string{
		"source": "github-pr-monitor", "github.owner": requests[0].Owner, "github.repo": requests[0].Repo,
		"github.pr": "12", "github.head_sha": requests[0].HeadSHA, "github.base_sha": requests[0].BaseSHA,
	}, IncludeClosed: true})
	if err != nil || len(works) != 1 {
		t.Fatalf("semantic target work records = %d, err=%v; want one", len(works), err)
	}
	if got := fx.actionRecordCount(); got != 2 {
		t.Fatalf("per-request durable action intents = %d, want two", got)
	}
}

type prRepairCreateBarrierStore struct {
	beads.Store
	target  beads.Store
	barrier *prRepairCreateBarrier
}

func (s *prRepairCreateBarrierStore) StableCreateIDResolveTarget() beads.Store { return s.target }

func (s *prRepairCreateBarrierStore) Create(b beads.Bead) (beads.Bead, error) {
	if strings.HasPrefix(b.ID, "gc-pr-repair-") {
		s.barrier.arrive()
	}
	return s.Store.Create(b)
}

type prRepairCreateBarrier struct {
	mu      sync.Mutex
	arrived int
	target  int
	release chan struct{}
}

func newPRRepairCreateBarrier(target int) *prRepairCreateBarrier {
	return &prRepairCreateBarrier{target: target, release: make(chan struct{})}
}

func (b *prRepairCreateBarrier) arrive() {
	b.mu.Lock()
	if b.arrived >= b.target {
		b.mu.Unlock()
		return
	}
	b.arrived++
	if b.arrived == b.target {
		close(b.release)
	}
	release := b.release
	b.mu.Unlock()
	<-release
}

func TestPRActionPrepareResumesPendingIntentAfterDurableWorkWasCreated(t *testing.T) {
	fx := newPRActionFixture(t, false)
	fx.forge.pullRequests[0].MergeStateStatus = "BLOCKED"
	request := fx.actionRequest(PRActionPrepare)
	request.WorkID = ""
	actor := fx.workerActor()
	fingerprint, err := prActionFingerprint(request, actor)
	if err != nil {
		t.Fatal(err)
	}
	intent, err := createPRActionIntent(fx.store, request, actor, fingerprint, fx.now, nil)
	if err != nil {
		t.Fatal(err)
	}
	queue, err := fx.service.Queue(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	item := queue.Items[0]
	monitor, ok := findPRActionMonitor(fx.policy.Monitors, request.Monitor)
	if !ok {
		t.Fatal("test policy lost the repair monitor")
	}
	work, err := ensurePRRepairWork(fx.store, monitor.githubMonitor(), item)
	if err != nil {
		t.Fatal(err)
	}
	result, err := fx.service.Execute(context.Background(), request, actor)
	if err != nil {
		t.Fatal(err)
	}
	if result.ID != intent.ID || result.Status != PRActionStatusVerified || result.Outcome != PRActionOutcomePrepared || result.WorkID != work.ID {
		t.Fatalf("prepare retry did not recover its durable transition: intent=%+v work=%s result=%+v", intent, work.ID, result)
	}
	works, err := fx.store.List(beads.ListQuery{Metadata: map[string]string{
		"source": "github-pr-monitor", "github.owner": request.Owner, "github.repo": request.Repo,
		"github.pr": "12", "github.head_sha": request.HeadSHA, "github.base_sha": request.BaseSHA,
	}, IncludeClosed: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(works) != 1 {
		t.Fatalf("pending prepare replay left %d work records, want one", len(works))
	}
}

func TestPRActionMergeRequiresExactHumanAuthorityAndVerifiesEffect(t *testing.T) {
	fx := newPRActionFixture(t, true)
	request := fx.actionRequest(PRActionMerge)
	if _, err := fx.service.Execute(context.Background(), request, fx.workerActor()); !errors.Is(err, ErrPRActionHumanRequired) {
		t.Fatalf("worker-only merge error = %v, want human approval required", err)
	}
	if len(fx.forge.mergeCalls) != 0 {
		t.Fatal("worker-only merge reached forge")
	}

	request.IdempotencyKey = "merge-human-approval-1"
	principal := fx.humanPrincipal(request)
	actor := fx.workerActor()
	actor.Human = &principal
	first, err := fx.service.Execute(context.Background(), request, actor)
	if err != nil {
		t.Fatal(err)
	}
	second, err := fx.service.Execute(context.Background(), request, actor)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID == "" || first.ID != second.ID || first.Status != PRActionStatusVerified || first.Outcome != PRActionOutcomeMerged {
		t.Fatalf("verified merge result = first %+v second %+v", first, second)
	}
	if len(fx.forge.mergeCalls) != 1 {
		t.Fatalf("merge calls = %d, want one exact-revision effect", len(fx.forge.mergeCalls))
	}
}

func TestPRActionMergeMarksUnverifiedEffectUnknownAndNeverClaimsSuccess(t *testing.T) {
	fx := newPRActionFixture(t, true)
	request := fx.actionRequest(PRActionMerge)
	request.IdempotencyKey = "merge-unknown-1"
	principal := fx.humanPrincipal(request)
	actor := fx.workerActor()
	actor.Human = &principal
	fx.forge.mergeErr = errors.New("connection lost after request")
	fx.forge.readErr = errors.New("GitHub unavailable")
	result, err := fx.service.Execute(context.Background(), request, actor)
	if !errors.Is(err, ErrPRActionOutcomeUnknown) {
		t.Fatalf("unknown merge result error = %v, want explicit unknown", err)
	}
	if result.ID == "" || result.Status != PRActionStatusUnknown {
		t.Fatalf("unknown outcome was not durably recorded: %+v", result)
	}
	retry, retryErr := fx.service.Execute(context.Background(), request, actor)
	if !errors.Is(retryErr, ErrPRActionOutcomeUnknown) || retry.ID != result.ID {
		t.Fatalf("unknown replay = result %+v error %v, want same durable unknown", retry, retryErr)
	}
	if len(fx.forge.mergeCalls) != 1 {
		t.Fatalf("unknown merge replay repeated the effect: calls=%d", len(fx.forge.mergeCalls))
	}
}

func TestPRActionMergeReservesNonRetryableStateBeforeForgeCall(t *testing.T) {
	fx := newPRActionFixture(t, true)
	fx.service.now = func() time.Time { return fx.now }
	request := fx.actionRequest(PRActionMerge)
	request.IdempotencyKey = "merge-interrupted-1"
	actor := fx.workerActor()
	principal := fx.humanPrincipal(request)
	actor.Human = &principal
	fx.forge.beforeMerge = func() { panic("simulated interruption before forge mutation") }

	func() {
		defer func() {
			if recovered := recover(); recovered == nil {
				t.Fatal("expected simulated process interruption before forge mutation")
			}
		}()
		_, _ = fx.service.Execute(context.Background(), request, actor)
	}()

	result, found, err := findPRActionRecord(fx.store, request.IdempotencyKey)
	if err != nil || !found || result.Status != PRActionStatusUnknown {
		t.Fatalf("pre-forge merge reservation = result %+v found=%v err=%v", result, found, err)
	}
	if len(fx.forge.mergeCalls) != 1 || fx.forge.pullRequests[0].IsMerged {
		t.Fatalf("interrupted merge crossed the adapter more than once or changed forge state: calls=%v pr=%+v", fx.forge.mergeCalls, fx.forge.pullRequests[0])
	}

	// Once the durable claim lease has expired, an exact retry still reads the
	// non-retryable reservation and never submits a second forge mutation.
	fx.now = fx.now.Add(25 * time.Hour)
	retry, retryErr := fx.service.Execute(context.Background(), request, actor)
	if !errors.Is(retryErr, ErrPRActionOutcomeUnknown) || retry.ID != result.ID || retry.Status != PRActionStatusUnknown {
		t.Fatalf("expired-claim replay = result %+v error %v, want same durable unknown", retry, retryErr)
	}
	if len(fx.forge.mergeCalls) != 1 || fx.forge.pullRequests[0].IsMerged {
		t.Fatalf("expired-claim replay repeated or applied merge: calls=%v pr=%+v", fx.forge.mergeCalls, fx.forge.pullRequests[0])
	}
}

func TestPRActionMergeClaimRejectsStalePendingReadAfterUnknownReservation(t *testing.T) {
	fx := newPRActionFixture(t, true)
	fx.service.now = func() time.Time { return fx.now }
	request := fx.actionRequest(PRActionMerge)
	request.IdempotencyKey = "merge-stale-reader-claim-1"
	actor := fx.workerActor()
	principal := fx.humanPrincipal(request)
	actor.Human = &principal
	fingerprint, err := prActionFingerprint(request, actor)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := createPRActionIntent(fx.store, request, actor, fingerprint, fx.now, nil); err != nil {
		t.Fatal(err)
	}
	blockedStore := &prActionStaleSnapshotStore{
		Store: fx.store, target: fx.store, idempotencyKey: request.IdempotencyKey,
		read: make(chan struct{}), release: make(chan struct{}),
	}
	stateB := *fx.state
	stateB.stores = map[string]beads.Store{"myrig": blockedStore}
	serviceB := NewPRActionService(PRActionServiceOptions{
		State: &stateB, Forge: fx.forge, Evidence: fx.evidence, HumanVerifier: fx.verifier,
		Policy: fx.policy, Now: func() time.Time { return fx.now },
	})
	resultB := make(chan PRActionResult, 1)
	errB := make(chan error, 1)
	go func() {
		result, executeErr := serviceB.Execute(context.Background(), request, actor)
		resultB <- result
		errB <- executeErr
	}()
	select {
	case <-blockedStore.read:
	case result := <-resultB:
		err := <-errB
		t.Fatalf("second service returned before capturing the stale pending snapshot: result=%+v err=%v", result, err)
	case <-time.After(5 * time.Second):
		t.Fatal("second service did not capture its pending action snapshot (and did not return early)")
	}

	fx.forge.mergeErr = errors.New("connection lost after submission")
	fx.forge.readErr = errors.New("forge read unavailable")
	resultA, errA := fx.service.Execute(context.Background(), request, actor)
	if !errors.Is(errA, ErrPRActionOutcomeUnknown) || resultA.Status != PRActionStatusUnknown {
		t.Fatalf("first service merge result = %+v err=%v, want durable unknown", resultA, errA)
	}
	if len(fx.forge.mergeCalls) != 1 {
		t.Fatalf("first service merge calls = %d, want one", len(fx.forge.mergeCalls))
	}
	fx.now = fx.now.Add(25 * time.Hour)
	close(blockedStore.release)
	result, executeErr := <-resultB, <-errB
	if !errors.Is(executeErr, ErrPRActionOutcomeUnknown) || result.ID != resultA.ID || result.Status != PRActionStatusUnknown {
		t.Fatalf("stale second service result = %+v err=%v, want same unknown reservation", result, executeErr)
	}
	if len(fx.forge.mergeCalls) != 1 {
		t.Fatalf("stale pending snapshot repeated the merge: calls=%d", len(fx.forge.mergeCalls))
	}
}

type prActionStaleSnapshotStore struct {
	beads.Store
	target          beads.Store
	idempotencyKey  string
	read            chan struct{}
	release         chan struct{}
	blockPendingGet sync.Once
}

func (s *prActionStaleSnapshotStore) ConditionalWritesResolveTarget() beads.Store {
	return s.target
}

func (s *prActionStaleSnapshotStore) ConditionalWriterHandle() (beads.ConditionalWriter, bool) {
	return beads.ConditionalWriterFor(s.target)
}

func (s *prActionStaleSnapshotStore) StableCreateIDResolveTarget() beads.Store {
	return s.target
}

func (s *prActionStaleSnapshotStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	rows, err := s.Store.List(query)
	if query.Metadata[prActionIdempotencyMetadataKey] == s.idempotencyKey {
		s.blockPendingGet.Do(func() {
			close(s.read)
			<-s.release
		})
	}
	return rows, err
}

func TestPRActionMergeDoesNotClaimSuccessAfterBaseChangesBeforeReadback(t *testing.T) {
	fx := newPRActionFixture(t, true)
	request := fx.actionRequest(PRActionMerge)
	request.IdempotencyKey = "merge-base-readback-1"
	actor := fx.workerActor()
	principal := fx.humanPrincipal(request)
	actor.Human = &principal
	fx.forge.afterMerge = func() {
		fx.forge.pullRequests[0].BaseSHA = strings.Repeat("c", 40)
	}
	result, err := fx.service.Execute(context.Background(), request, actor)
	if !errors.Is(err, ErrPRActionOutcomeUnknown) {
		t.Fatalf("base changed before readback error = %v, want unknown", err)
	}
	if result.Status != PRActionStatusUnknown || result.Outcome == PRActionOutcomeMerged {
		t.Fatalf("base movement was reported as verified merge: %+v", result)
	}
}

func TestPRActionMergeRejectsBaseMovementAtAtomicForgeBoundary(t *testing.T) {
	fx := newPRActionFixture(t, true)
	request := fx.actionRequest(PRActionMerge)
	request.IdempotencyKey = "merge-base-race-1"
	actor := fx.workerActor()
	principal := fx.humanPrincipal(request)
	actor.Human = &principal
	fx.forge.beforeMerge = func() {
		fx.forge.pullRequests[0].BaseSHA = strings.Repeat("c", 40)
	}
	result, err := fx.service.Execute(context.Background(), request, actor)
	if !errors.Is(err, ErrPRActionStale) {
		t.Fatalf("base race error = %v, want stale", err)
	}
	if result.Status != PRActionStatusRejected || result.Outcome == PRActionOutcomeMerged || fx.forge.pullRequests[0].IsMerged {
		t.Fatalf("base movement produced a merge outcome: result=%+v pr=%+v", result, fx.forge.pullRequests[0])
	}
}

type prActionFixture struct {
	t           *testing.T
	state       *fakeState
	store       beads.Store
	pullRequest githubmonitor.PullRequest
	work        beads.Bead
	forge       *fakePRActionForge
	evidence    *fakePRActionEvidence
	service     *PRActionService
	verifier    *PRHumanGrantVerifier
	policy      *TrustedPRActionPolicy
	priv        ed25519.PrivateKey
	now         time.Time
}

func newPRActionFixture(t *testing.T, withWork bool) *prActionFixture {
	t.Helper()
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	state := newFakeState(t)
	state.stores["myrig"].(*beads.MemStore).HonorExplicitIDs = true
	state.cfg.GitHub.PRMonitors = []config.GitHubPRMonitor{{
		Name: "pilot", Owner: "acme", Repo: "widget", BaseBranches: []string{"main"}, Rig: "myrig", RepairRoute: "myrig/worker",
	}}
	store := state.stores["myrig"]
	baseSHA := strings.Repeat("b", 40)
	headSHA := strings.Repeat("a", 40)
	pullRequest := githubmonitor.PullRequest{
		Number: 12, Title: "fix: make it safe", URL: "https://github.com/acme/widget/pull/12",
		BaseRefName: "main", BaseSHA: baseSHA, HeadRefName: "fix", HeadSHA: headSHA,
		State: "OPEN", MergeStateStatus: "CLEAN",
		Checks: []githubmonitor.Check{{Name: "ci", Status: "COMPLETED", Conclusion: "SUCCESS"}},
	}
	var work beads.Bead
	evidence := &fakePRActionEvidence{}
	if withWork {
		var err error
		work, err = store.Create(beads.Bead{
			Title: "repair current revision", Type: "task", Labels: []string{"github", "repair", "pr-monitor"},
			Metadata: beads.StringMap{
				"source": "github-pr-monitor", "github.owner": "acme", "github.repo": "widget",
				"github.pr": "12", "github.head_sha": headSHA, "github.base_sha": baseSHA, "github.monitor": "pilot",
				"gc.routed_to": "myrig/worker",
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		ref := PRActionAttemptReference{
			StoreRef: "rig:myrig", WorkID: work.ID, AttemptID: "ae-attempt-1",
			BaseSHA: baseSHA, CandidateSHA: headSHA, DiffSHA256: strings.Repeat("d", 64),
			DiffSource: PRActionDiffCandidateCommit, WorkingTreeStatus: PRActionWorkingTreeClean,
		}
		evidence.refs = []PRActionAttemptReference{ref}
		evidence.records = map[string]PRActionAttemptEvidence{
			ref.AttemptID: {
				OwnerBeadID: work.ID, ExecutionBeadID: "gc-attempt-9", BaseSHA: baseSHA,
				CandidateSHA: headSHA, DiffSHA256: ref.DiffSHA256,
				DiffSource: ref.DiffSource, WorkingTreeStatus: ref.WorkingTreeStatus,
			},
		}
	}
	forge := &fakePRActionForge{pullRequests: []githubmonitor.PullRequest{pullRequest}}
	humanPub, humanPriv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	humanVerifier, err := NewPRHumanGrantVerifier(PRHumanTrustConfig{
		Keys:        []PRHumanGrantKey{{KeyID: "human", PublicKey: base64.StdEncoding.EncodeToString(humanPub)}},
		Authorities: []PRHumanAuthority{{KeyID: "human", Issuer: "governance", Subject: "ricky", Scopes: []string{PRActionScopeMerge}}},
	}, nil, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	policy := &TrustedPRActionPolicy{
		City: state.cityName, Version: "pr-actions-test-v1",
		Monitors: []PRActionPolicyMonitor{{
			Name: "pilot", Owner: "acme", Repo: "widget", Rig: "myrig", BaseBranches: []string{"main"},
			RepairRoute: "myrig/worker", RequiredChecks: []string{"ci"},
		}},
	}
	service := NewPRActionService(PRActionServiceOptions{
		State: state, Forge: forge, Evidence: evidence, HumanVerifier: humanVerifier, Policy: policy, Now: func() time.Time { return now },
	})
	return &prActionFixture{t: t, state: state, store: store, pullRequest: pullRequest, work: work, forge: forge, evidence: evidence, service: service, verifier: humanVerifier, policy: policy, priv: humanPriv, now: now}
}

func (fx *prActionFixture) actionRequest(action PRActionKind) PRActionRequest {
	request := PRActionRequest{
		Monitor: "pilot", Owner: "acme", Repo: "widget", PullRequest: 12, Action: action,
		HeadSHA: fx.pullRequest.HeadSHA, BaseSHA: fx.pullRequest.BaseSHA,
		PolicyVersion: fx.policy.Version, IdempotencyKey: "action-key-1234",
	}
	if action != PRActionPrepare {
		request.WorkID = fx.work.ID
		if len(fx.evidence.refs) > 0 {
			request.AttemptID = fx.evidence.refs[0].AttemptID
		}
	}
	return request
}

func (fx *prActionFixture) workerActor() PRActionActor {
	return PRActionActor{CityWrite: VerifiedCityWritePrincipal{KeyID: "worker-kid", City: fx.state.cityName}}
}

func (fx *prActionFixture) humanPrincipal(request PRActionRequest) VerifiedPRPrincipal {
	claims := PRHumanGrantClaims{
		KeyID: "human", Issuer: "governance", Subject: "ricky", Scope: PRActionScopeMerge,
		City: fx.state.cityName, Owner: request.Owner, Repo: request.Repo, PullRequest: request.PullRequest,
		HeadSHA: request.HeadSHA, BaseSHA: request.BaseSHA, PolicyVersion: request.PolicyVersion,
		WorkID: request.WorkID, IdempotencyKey: request.IdempotencyKey,
		IssuedAt: fx.now.Add(-time.Second).Unix(), ExpiresAt: fx.now.Add(time.Minute).Unix(), TokenID: "approval-1",
	}
	principal, err := fx.verifier.Verify(signPRHumanGrant(fx.t, fx.priv, claims), PRHumanGrantExpectation{
		City: claims.City, Scope: claims.Scope, Owner: claims.Owner, Repo: claims.Repo, PullRequest: claims.PullRequest,
		HeadSHA: claims.HeadSHA, BaseSHA: claims.BaseSHA, PolicyVersion: claims.PolicyVersion,
		WorkID: claims.WorkID, IdempotencyKey: claims.IdempotencyKey,
	})
	if err != nil {
		panic(err)
	}
	return principal
}

func (fx *prActionFixture) actionRecordCount() int {
	records, err := fx.store.List(beads.ListQuery{Metadata: map[string]string{"gc.pr_action.source": "api"}, IncludeClosed: true})
	if err != nil {
		panic(err)
	}
	return len(records)
}

type fakePRActionForge struct {
	pullRequests []githubmonitor.PullRequest
	mergeCalls   []string
	mergeErr     error
	readErr      error
	beforeMerge  func()
	afterMerge   func()
	listBarrier  *prActionListBarrier
}

func (f *fakePRActionForge) ListOpenPullRequests(context.Context, string, string) ([]githubmonitor.PullRequest, error) {
	if f.listBarrier != nil {
		f.listBarrier.arrive()
	}
	return append([]githubmonitor.PullRequest(nil), f.pullRequests...), nil
}

type prActionListBarrier struct {
	mu      sync.Mutex
	arrived int
	target  int
	release chan struct{}
}

func newPRActionListBarrier(target int) *prActionListBarrier {
	return &prActionListBarrier{target: target, release: make(chan struct{})}
}

func (b *prActionListBarrier) arrive() {
	b.mu.Lock()
	if b.arrived >= b.target {
		b.mu.Unlock()
		return
	}
	b.arrived++
	if b.arrived == b.target {
		close(b.release)
	}
	release := b.release
	b.mu.Unlock()
	<-release
}

func (f *fakePRActionForge) SupportsAtomicBaseBoundMerge() bool { return true }

func (f *fakePRActionForge) MergePullRequest(_ context.Context, owner, repo string, number int, headSHA, baseSHA string, requiredChecks []string) (PRActionMergeReceipt, error) {
	f.mergeCalls = append(f.mergeCalls, fmt.Sprintf("%s/%s#%d@%s:%s", owner, repo, number, headSHA, baseSHA))
	if f.beforeMerge != nil {
		f.beforeMerge()
	}
	if f.mergeErr != nil {
		return PRActionMergeReceipt{}, f.mergeErr
	}
	for i := range f.pullRequests {
		if f.pullRequests[i].Number != number {
			continue
		}
		if f.pullRequests[i].HeadSHA != headSHA || f.pullRequests[i].BaseSHA != baseSHA || !prMergeReady(f.pullRequests[i], requiredChecks) {
			return PRActionMergeReceipt{}, ErrPRActionStale
		}
		f.pullRequests[i].IsMerged = true
		f.pullRequests[i].State = "MERGED"
		f.pullRequests[i].MergeCommitSHA = strings.Repeat("e", 40)
		if f.afterMerge != nil {
			f.afterMerge()
		}
		return PRActionMergeReceipt{HeadSHA: headSHA, BaseSHA: baseSHA, MergeCommitSHA: f.pullRequests[i].MergeCommitSHA}, nil
	}
	return PRActionMergeReceipt{}, ErrPRActionStale
}

func (f *fakePRActionForge) ReadPullRequest(_ context.Context, _ string, _ string, number int) (githubmonitor.PullRequest, error) {
	if f.readErr != nil {
		return githubmonitor.PullRequest{}, f.readErr
	}
	for _, pr := range f.pullRequests {
		if pr.Number == number {
			return pr, nil
		}
	}
	return githubmonitor.PullRequest{}, fmt.Errorf("PR %d not found", number)
}

type fakePRActionEvidence struct {
	refs       []PRActionAttemptReference
	nextRefs   []PRActionAttemptReference
	refsCalls  int
	refsOnCall int
	records    map[string]PRActionAttemptEvidence
	readErr    error
	refsErr    error
}

func (f *fakePRActionEvidence) References(_ beads.Store, storeRef, workID string) ([]PRActionAttemptReference, error) {
	f.refsCalls++
	if f.refsOnCall > 0 && f.refsCalls >= f.refsOnCall {
		f.refs = append([]PRActionAttemptReference(nil), f.nextRefs...)
	}
	if f.refsErr != nil {
		return nil, f.refsErr
	}
	var out []PRActionAttemptReference
	for _, ref := range f.refs {
		if ref.StoreRef == storeRef && ref.WorkID == workID {
			out = append(out, ref)
		}
	}
	return out, nil
}

func (f *fakePRActionEvidence) Read(_ beads.Store, workID, attemptID string) (PRActionAttemptEvidence, error) {
	if f.readErr != nil {
		return PRActionAttemptEvidence{}, f.readErr
	}
	for _, ref := range f.refs {
		if ref.WorkID == workID && ref.AttemptID == attemptID {
			return f.records[attemptID], nil
		}
	}
	return PRActionAttemptEvidence{}, ErrPRActionEvidenceMissing
}
