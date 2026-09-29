package decisionfrontier

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

func testCityScope() Scope { return Scope{CityRef: "city:test", StoreRef: "city:test"} }

type sourceFenceTestStore struct {
	name  string
	store beads.Store
	close func() error
}

func sourceFenceTestStores(t *testing.T) []sourceFenceTestStore {
	t.Helper()
	mem := beads.NewMemStore()
	mem.HonorExplicitIDs = true
	opened, err := beads.OpenSQLiteStore(t.TempDir())
	if err != nil {
		t.Fatalf("OpenSQLiteStore: %v", err)
	}
	sqlite := opened.(*beads.SQLiteStore)
	return []sourceFenceTestStore{
		{name: "mem", store: mem, close: func() error { return nil }},
		{name: "sqlite", store: sqlite, close: sqlite.CloseStore},
	}
}

func transitionReceiptForTest(t *testing.T, work beads.Bead, id string) beads.RevisionTransitionReceipt {
	t.Helper()
	var receipts []beads.RevisionTransitionReceipt
	if err := json.Unmarshal([]byte(work.Metadata[beadmeta.DecisionFrontierRevisionReceiptsMetadataKey]), &receipts); err != nil {
		t.Fatalf("decode transition receipts: %v", err)
	}
	for _, receipt := range receipts {
		if receipt.ID == id {
			return receipt
		}
	}
	t.Fatalf("transition receipt %q not found in %+v", id, receipts)
	return beads.RevisionTransitionReceipt{}
}

func TestEnsurePersistsAndReplaysHeldFrontier(t *testing.T) {
	store := beads.NewMemStore()
	store.HonorExplicitIDs = true
	work, err := store.Create(beads.Bead{
		ID:          "wrk-frontier",
		Type:        "task",
		Title:       "Choose a direction",
		Description: "Exact source work",
		Labels:      []string{beadmeta.HoldExternalLabel},
	})
	if err != nil {
		t.Fatal(err)
	}
	proposal := Proposal{Questions: []Question{
		{ID: "scope", Title: "Which scope?", Prompt: "Choose the target scope.", Recommendations: []string{"pilot"}},
		{ID: "rollout", Title: "How to roll out?", Prompt: "Choose rollout after scope.", DependsOn: []string{"scope"}},
	}}
	revision, err := WorkRevision(work)
	if err != nil {
		t.Fatal(err)
	}

	service := Service{}
	first, err := service.Ensure(context.Background(), store, testCityScope(), work.ID, revision, proposal)
	if err != nil {
		t.Fatal(err)
	}
	if first.State != StatePending || first.WorkRevision != revision || len(first.Questions) != 2 {
		t.Fatalf("first frontier = %+v", first)
	}
	if len(first.OpenQuestions) != 1 || first.OpenQuestions[0].ID != "scope" {
		t.Fatalf("open question frontier = %+v, want only independent scope question", first.OpenQuestions)
	}
	current, err := store.Get(work.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Metadata[beadmeta.DecisionFrontierHoldMetadataKey] == "" {
		t.Fatal("source work was not durably held")
	}
	reserved := transitionReceiptForTest(t, current, reservationReceiptID(testCityScope(), first.MapID))
	if current.Revision != reserved.ToRevision || reserved.FromRevision != work.Revision ||
		reserved.CityRef != first.CityRef || reserved.StoreRef != first.StoreRef {
		t.Fatalf("reservation receipt does not bind exact source transition: work=%+v receipt=%+v", current, reserved)
	}
	if !containsLabel(current.Labels, beadmeta.HoldExternalLabel) {
		t.Fatalf("pre-existing external hold was lost: %v", current.Labels)
	}
	ready, err := store.Ready()
	if err != nil {
		t.Fatal(err)
	}
	if containsBead(ready, work.ID) {
		t.Fatal("source work remained ready while decision frontier is pending")
	}
	for _, question := range first.Questions {
		b, getErr := store.Get(question.TicketID)
		if getErr != nil {
			t.Fatal(getErr)
		}
		if b.Type != "gate" {
			t.Fatalf("ticket %s type = %q, want non-runnable gate", b.ID, b.Type)
		}
	}

	second, err := service.Ensure(context.Background(), store, testCityScope(), work.ID, revision, proposal)
	if err != nil {
		t.Fatal(err)
	}
	if second.MapID != first.MapID || len(second.Questions) != len(first.Questions) {
		t.Fatalf("replay changed frontier identity: first=%+v second=%+v", first, second)
	}
	rows, err := store.List(beads.ListQuery{Type: "gate", AllowScan: true, IncludeClosed: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 4 { // one map, two tickets, one durable prompt intent
		t.Fatalf("gate rows = %d, want exactly 4: %+v", len(rows), rows)
	}
}

func TestPersistedDependencyChangeInvalidatesPreReservationSnapshot(t *testing.T) {
	for _, tc := range sourceFenceTestStores(t) {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if err := tc.close(); err != nil {
					t.Errorf("close source-fence test store: %v", err)
				}
			}()
			store := tc.store
			work, err := store.Create(beads.Bead{Title: "source before graph edit"})
			if err != nil {
				t.Fatal(err)
			}
			if work.Revision == 0 {
				title := "source revisioned before graph edit"
				if err := store.Update(work.ID, beads.UpdateOpts{Title: &title}); err != nil {
					t.Fatal(err)
				}
			}
			target, err := store.Create(beads.Bead{Title: "new dependency target"})
			if err != nil {
				t.Fatal(err)
			}
			reader, ok := beads.DecisionFrontierSourceReaderFor(store)
			if !ok || reader == nil {
				t.Fatal("direct supported store lacks source snapshot reader")
			}
			before, err := reader.DecisionFrontierSourceSnapshot(work.ID)
			if err != nil {
				t.Fatal(err)
			}
			oldRevision, err := WorkRevision(before)
			if err != nil {
				t.Fatal(err)
			}
			oldDigest, err := WorkDigest(before)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.DepAdd(work.ID, target.ID, "blocks"); err != nil {
				t.Fatal(err)
			}
			after, err := reader.DecisionFrontierSourceSnapshot(work.ID)
			if err != nil {
				t.Fatal(err)
			}
			newDigest, err := WorkDigest(after)
			if err != nil {
				t.Fatal(err)
			}
			if after.Revision == before.Revision || oldDigest == newDigest || len(after.Dependencies) != 1 {
				t.Fatalf("persisted edge did not change fenced source snapshot: before=%+v/%s after=%+v/%s", before, oldDigest, after, newDigest)
			}
			_, err = (Service{}).Ensure(context.Background(), store, testCityScope(), work.ID, oldRevision,
				Proposal{Questions: []Question{{ID: "q", Title: "Question", Prompt: "Choose."}}})
			if !errors.Is(err, ErrStale) {
				t.Fatalf("Ensure after edge change = %v, want stale", err)
			}
			current, err := store.Get(work.ID)
			if err != nil {
				t.Fatal(err)
			}
			if beads.HasDecisionFrontierHold(current) {
				t.Fatalf("stale pre-reservation graph edit left source held: %+v", current)
			}
			mapID := frontierMapID(testCityScope(), work.ID, oldRevision)
			if _, err := store.Get(mapID); !errors.Is(err, beads.ErrNotFound) {
				t.Fatalf("stale Ensure created map %s: %v", mapID, err)
			}
		})
	}
}

func TestHeldFrontierRejectsConcurrentDependencyMutationAndStillReleases(t *testing.T) {
	for _, tc := range sourceFenceTestStores(t) {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if err := tc.close(); err != nil {
					t.Errorf("close source-fence test store: %v", err)
				}
			}()
			store := tc.store
			work, err := store.Create(beads.Bead{Title: "held source"})
			if err != nil {
				t.Fatal(err)
			}
			if work.Revision == 0 {
				title := "revisioned held source"
				if err := store.Update(work.ID, beads.UpdateOpts{Title: &title}); err != nil {
					t.Fatal(err)
				}
			}
			target, err := store.Create(beads.Bead{Title: "dependency target"})
			if err != nil {
				t.Fatal(err)
			}
			extraTarget, err := store.Create(beads.Bead{Title: "second dependency target"})
			if err != nil {
				t.Fatal(err)
			}
			if err := store.DepAdd(work.ID, target.ID, "blocks"); err != nil {
				t.Fatalf("seed source edge before hold: %v", err)
			}
			unrelated, err := store.Create(beads.Bead{Title: "unrelated source"})
			if err != nil {
				t.Fatal(err)
			}
			revisioned, ok := beads.DecisionFrontierSourceReaderFor(store)
			if !ok || revisioned == nil {
				t.Fatal("source snapshot reader unavailable")
			}
			initial, err := revisioned.DecisionFrontierSourceSnapshot(work.ID)
			if err != nil {
				t.Fatal(err)
			}
			workRevision, err := WorkRevision(initial)
			if err != nil {
				t.Fatal(err)
			}
			proposal := Proposal{Questions: []Question{{ID: "q", Title: "Question", Prompt: "Choose."}}}
			service := Service{}
			front, err := service.Ensure(context.Background(), store, testCityScope(), work.ID, workRevision, proposal)
			if err != nil {
				t.Fatal(err)
			}
			held, err := revisioned.DecisionFrontierSourceSnapshot(work.ID)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.DepAdd(work.ID, extraTarget.ID, "blocks"); !errors.Is(err, beads.ErrDecisionFrontierMutationBlocked) {
				t.Fatalf("concurrent edge addition during hold = %v, want blocked", err)
			}
			if err := store.DepRemove(work.ID, target.ID); !errors.Is(err, beads.ErrDecisionFrontierMutationBlocked) {
				t.Fatalf("concurrent edge removal during hold = %v, want blocked", err)
			}
			beforeUnrelated, err := store.Get(unrelated.ID)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.DepAdd(unrelated.ID, target.ID, "blocks"); err != nil {
				t.Fatalf("unrelated source edge edit during hold: %v", err)
			}
			afterUnrelated, err := store.Get(unrelated.ID)
			if err != nil {
				t.Fatal(err)
			}
			if afterUnrelated.Revision != beforeUnrelated.Revision+1 {
				t.Fatalf("unrelated source revision = %d, want %d", afterUnrelated.Revision, beforeUnrelated.Revision+1)
			}
			stillHeld, err := revisioned.DecisionFrontierSourceSnapshot(work.ID)
			if err != nil {
				t.Fatal(err)
			}
			stillHeldDigest, err := WorkDigest(stillHeld)
			if err != nil {
				t.Fatal(err)
			}
			initialDigest, err := WorkDigest(initial)
			if err != nil {
				t.Fatal(err)
			}
			if stillHeld.Revision != held.Revision || stillHeldDigest != initialDigest || !beads.HasDecisionFrontierHold(stillHeld) {
				t.Fatalf("refused held-source edits or unrelated edit changed source: initial=%+v held=%+v current=%+v digest=%s/%s", initial, held, stillHeld, initialDigest, stillHeldDigest)
			}
			question := questionByID(t, front, "q")
			service.Verifier = verifierFunc(func(_ context.Context, challenge AnswerChallenge, submission AnswerSubmission) (VerifiedAnswer, error) {
				return VerifiedAnswer{
					CityRef: challenge.CityRef, StoreRef: challenge.StoreRef, WorkDigest: challenge.WorkDigest,
					KeyID: "test-key", Issuer: "test-authority", Subject: "test-human", WorkID: challenge.WorkID,
					WorkRevision: challenge.WorkRevision, MapID: challenge.MapID, TicketID: challenge.TicketID,
					QuestionID:      challenge.QuestionID,
					QuestionVersion: challenge.QuestionVersion, AnswerDigest: AnswerDigest(submission.Resolution, submission.Text),
					Resolution: submission.Resolution,
				}, nil
			})
			resolved, err := service.Answer(context.Background(), store, testCityScope(), work.ID, AnswerSubmission{
				TicketID: question.TicketID, WorkRevision: workRevision, QuestionVersion: question.Version,
				Resolution: ResolutionAnswered, Text: "Proceed with the selected option.", Proof: "signed test envelope",
			})
			if err != nil {
				t.Fatalf("Answer/release after blocked graph edit: %v", err)
			}
			if resolved.State != StateResolved {
				t.Fatalf("frontier state after answer = %q, want resolved", resolved.State)
			}
			released, err := revisioned.DecisionFrontierSourceSnapshot(work.ID)
			if err != nil {
				t.Fatal(err)
			}
			if beads.HasDecisionFrontierHold(released) || released.Metadata[beadmeta.DecisionFrontierRevisionReceiptsMetadataKey] == "" {
				t.Fatalf("source not released with durable receipt: %+v", released)
			}
		})
	}
}

func TestAnswerRefusesOutOfBandGraphChangeBeforeRelease(t *testing.T) {
	dir := t.TempDir()
	opened, err := beads.OpenSQLiteStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	store := opened.(*beads.SQLiteStore)
	defer func() {
		if err := store.CloseStore(); err != nil {
			t.Errorf("close SQLite decision-frontier store: %v", err)
		}
	}()
	target, err := store.Create(beads.Bead{Title: "target"})
	if err != nil {
		t.Fatal(err)
	}
	work, err := store.Create(beads.Bead{Title: "source"})
	if err != nil {
		t.Fatal(err)
	}
	if work.Revision == 0 {
		title := "revisioned source"
		if err := store.Update(work.ID, beads.UpdateOpts{Title: &title}); err != nil {
			t.Fatal(err)
		}
	}
	reader, ok := beads.DecisionFrontierSourceReaderFor(store)
	if !ok {
		t.Fatal("SQLite source snapshot reader unavailable")
	}
	work, err = reader.DecisionFrontierSourceSnapshot(work.ID)
	if err != nil {
		t.Fatal(err)
	}
	revision, err := WorkRevision(work)
	if err != nil {
		t.Fatal(err)
	}
	verifier := verifierFunc(func(_ context.Context, challenge AnswerChallenge, submission AnswerSubmission) (VerifiedAnswer, error) {
		return VerifiedAnswer{
			CityRef: challenge.CityRef, StoreRef: challenge.StoreRef, WorkDigest: challenge.WorkDigest,
			KeyID: "test-key", Issuer: "test-authority", Subject: "test-human", WorkID: challenge.WorkID,
			WorkRevision: challenge.WorkRevision, MapID: challenge.MapID, TicketID: challenge.TicketID,
			QuestionID:      challenge.QuestionID,
			QuestionVersion: challenge.QuestionVersion, AnswerDigest: AnswerDigest(submission.Resolution, submission.Text),
			Resolution: submission.Resolution,
		}, nil
	})
	service := Service{Verifier: verifier}
	front, err := service.Ensure(context.Background(), store, testCityScope(), work.ID, revision,
		Proposal{Questions: []Question{{ID: "q", Title: "Question", Prompt: "Choose."}}})
	if err != nil {
		t.Fatal(err)
	}
	held, err := reader.DecisionFrontierSourceSnapshot(work.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate an unsupported backend writer that changes the dependency table
	// without honoring the source revision/hold fence. The controller must see
	// the authoritative edge and refuse to release from its stale digest.
	db, err := sql.Open("sqlite", filepath.Join(dir, "beads.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO deps(issue_id, depends_on_id, dep_type) VALUES(?,?,?)`, work.ID, target.ID, "blocks"); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	question := questionByID(t, front, "q")
	_, err = service.Answer(context.Background(), store, testCityScope(), work.ID, AnswerSubmission{
		TicketID: question.TicketID, WorkRevision: revision, QuestionVersion: question.Version,
		Resolution: ResolutionAnswered, Text: "Proceed with the selected option.", Proof: "signed test envelope",
	})
	if !errors.Is(err, ErrStale) {
		t.Fatalf("Answer after out-of-band graph change = %v, want stale", err)
	}
	current, err := reader.DecisionFrontierSourceSnapshot(work.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !beads.HasDecisionFrontierHold(current) || current.Revision != held.Revision {
		t.Fatalf("stale answer changed source hold or revision: held=%+v current=%+v", held, current)
	}
	var receipts []beads.RevisionTransitionReceipt
	if err := json.Unmarshal([]byte(current.Metadata[beadmeta.DecisionFrontierRevisionReceiptsMetadataKey]), &receipts); err != nil {
		t.Fatal(err)
	}
	for _, receipt := range receipts {
		if receipt.ID == releaseReceiptID(testCityScope(), front.MapID) {
			t.Fatal("stale answer wrote a release receipt")
		}
	}
}

func TestAnswerRequiresConfiguredVerifierAndResumesOnlyExactFrontier(t *testing.T) {
	store := beads.NewMemStore()
	store.HonorExplicitIDs = true
	work, err := store.Create(beads.Bead{ID: "wrk-answer", Type: "task", Title: "Choose safely", Labels: []string{beadmeta.HoldExternalLabel}})
	if err != nil {
		t.Fatal(err)
	}
	revision, err := WorkRevision(work)
	if err != nil {
		t.Fatal(err)
	}
	proposal := Proposal{Questions: []Question{
		{ID: "a", Title: "First", Prompt: "First decision"},
		{ID: "b", Title: "Second", Prompt: "Second decision", DependsOn: []string{"a"}},
	}}
	service := Service{}
	front, err := service.Ensure(context.Background(), store, testCityScope(), work.ID, revision, proposal)
	if err != nil {
		t.Fatal(err)
	}
	first := questionByID(t, front, "a")
	_, err = service.Answer(context.Background(), store, testCityScope(), work.ID, AnswerSubmission{
		TicketID: first.TicketID, WorkRevision: revision, QuestionVersion: first.Version,
		Resolution: ResolutionAnswered, Text: "Keep the pilot scope.", Proof: "signed-envelope",
	})
	if !errors.Is(err, ErrAnswerVerifierUnavailable) {
		t.Fatalf("Answer without a configured trusted verifier: %v, want unavailable", err)
	}
	stillHeld, err := store.Get(work.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stillHeld.Metadata[beadmeta.DecisionFrontierHoldMetadataKey] == "" {
		t.Fatal("unverified answer released dependent work")
	}

	service.Verifier = verifierFunc(func(_ context.Context, challenge AnswerChallenge, submission AnswerSubmission) (VerifiedAnswer, error) {
		return VerifiedAnswer{
			CityRef: challenge.CityRef, StoreRef: challenge.StoreRef, WorkDigest: challenge.WorkDigest,
			KeyID: "human-key", Issuer: "human-authority", Subject: "authorized-human",
			WorkID: challenge.WorkID, WorkRevision: challenge.WorkRevision,
			MapID: challenge.MapID, TicketID: challenge.TicketID,
			QuestionID:      challenge.QuestionID,
			QuestionVersion: challenge.QuestionVersion, AnswerDigest: AnswerDigest(submission.Resolution, submission.Text),
			Resolution: submission.Resolution,
		}, nil
	})
	resolved, err := service.Answer(context.Background(), store, testCityScope(), work.ID, AnswerSubmission{
		TicketID: first.TicketID, WorkRevision: revision, QuestionVersion: first.Version,
		Resolution: ResolutionAnswered, Text: "Keep the pilot scope.", Proof: "signed-envelope",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(resolved.OpenQuestions) != 1 || resolved.OpenQuestions[0].ID != "b" {
		t.Fatalf("dependent question did not become current: %+v", resolved.OpenQuestions)
	}
	if resolved.State != StatePending {
		t.Fatalf("state after only first answer = %q, want pending", resolved.State)
	}

	second := questionByID(t, resolved, "b")
	completed, err := service.Answer(context.Background(), store, testCityScope(), work.ID, AnswerSubmission{
		TicketID: second.TicketID, WorkRevision: revision, QuestionVersion: second.Version,
		Resolution: ResolutionAnswered, Text: "Review after the pilot.", Proof: "signed-envelope-2",
	})
	if err != nil {
		t.Fatal(err)
	}
	if completed.State != StateResolved || len(completed.OpenQuestions) != 0 {
		t.Fatalf("completed frontier = %+v", completed)
	}
	current, err := store.Get(work.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Metadata[beadmeta.DecisionFrontierHoldMetadataKey] != "" {
		t.Fatalf("resolved frontier left controller hold: %q", current.Metadata[beadmeta.DecisionFrontierHoldMetadataKey])
	}
	if !containsLabel(current.Labels, beadmeta.HoldExternalLabel) {
		t.Fatalf("resolution removed unrelated hold: %v", current.Labels)
	}
	released := transitionReceiptForTest(t, current, releaseReceiptID(testCityScope(), front.MapID))
	if current.Revision != released.ToRevision || released.FromRevision <= work.Revision ||
		released.CityRef != front.CityRef || released.StoreRef != front.StoreRef {
		t.Fatalf("release receipt does not bind exact source transition: work=%+v receipt=%+v", current, released)
	}
	ready, err := store.Ready()
	if err != nil {
		t.Fatal(err)
	}
	if containsBead(ready, work.ID) {
		t.Fatal("resolution cleared a pre-existing external hold")
	}
}

func TestConcurrentEnsureConvergesOnOneFrontier(t *testing.T) {
	store := beads.NewMemStore()
	store.HonorExplicitIDs = true
	work, err := store.Create(beads.Bead{ID: "wrk-concurrent", Type: "task", Title: "Choose"})
	if err != nil {
		t.Fatal(err)
	}
	revision, err := WorkRevision(work)
	if err != nil {
		t.Fatal(err)
	}
	proposal := Proposal{Questions: []Question{{ID: "q", Title: "Question", Prompt: "Choose."}}}
	services := []Service{{}, {}}
	results := make([]Frontier, len(services))
	errs := make([]error, len(services))
	var wg sync.WaitGroup
	for i := range services {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = services[i].Ensure(context.Background(), store, testCityScope(), work.ID, revision, proposal)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("Ensure[%d]: %v", i, err)
		}
	}
	if results[0].MapID != results[1].MapID || results[0].Questions[0].TicketID != results[1].Questions[0].TicketID {
		t.Fatalf("concurrent calls diverged: %+v / %+v", results[0], results[1])
	}
	rows, err := store.List(beads.ListQuery{Type: "gate", AllowScan: true, IncludeClosed: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("gate rows = %d, want map + ticket + prompt intent", len(rows))
	}
}

func TestFrontierIdentityIsScopedToCityAndAuthoritativeStore(t *testing.T) {
	newStore := func() (*beads.MemStore, beads.Bead) {
		t.Helper()
		store := beads.NewMemStore()
		store.HonorExplicitIDs = true
		work, err := store.Create(beads.Bead{ID: "same-work-id", Type: "task", Title: "Same content"})
		if err != nil {
			t.Fatal(err)
		}
		return store, work
	}
	alphaStore, alphaWork := newStore()
	betaStore, betaWork := newStore()
	alphaScope := Scope{CityRef: "city:alpha", StoreRef: "rig:ai"}
	betaScope := Scope{CityRef: "city:beta", StoreRef: "rig:ai"}
	alphaRevision, _ := WorkRevision(alphaWork)
	betaRevision, _ := WorkRevision(betaWork)
	proposal := Proposal{Questions: []Question{{ID: "scope", Title: "Scope", Prompt: "Choose."}}}
	alpha, err := (Service{}).Ensure(context.Background(), alphaStore, alphaScope, alphaWork.ID, alphaRevision, proposal)
	if err != nil {
		t.Fatal(err)
	}
	beta, err := (Service{}).Ensure(context.Background(), betaStore, betaScope, betaWork.ID, betaRevision, proposal)
	if err != nil {
		t.Fatal(err)
	}
	if alpha.MapID == beta.MapID || alpha.Questions[0].TicketID == beta.Questions[0].TicketID || alpha.Prompt.ID == beta.Prompt.ID {
		t.Fatalf("identical work under same rig name in different cities shared frontier identities: alpha=%+v beta=%+v", alpha, beta)
	}
	if _, err := (Service{}).Read(context.Background(), alphaStore, betaScope, alphaWork.ID, alphaRevision); !errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("cross-city read replay error = %v, want no map in the other city scope", err)
	}
	if _, err := (Service{}).Read(context.Background(), betaStore, alphaScope, betaWork.ID, betaRevision); !errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("reverse cross-city read replay error = %v, want no map in the other city scope", err)
	}
}

func TestAnswerVerifierAndTicketReplayAreBoundToOwningCity(t *testing.T) {
	newStore := func() (*beads.MemStore, beads.Bead) {
		t.Helper()
		store := beads.NewMemStore()
		store.HonorExplicitIDs = true
		work, err := store.Create(beads.Bead{ID: "same-work-id", Type: "task", Title: "Same content"})
		if err != nil {
			t.Fatal(err)
		}
		return store, work
	}
	alphaStore, alphaWork := newStore()
	betaStore, betaWork := newStore()
	alphaScope := Scope{CityRef: "city:alpha", StoreRef: "rig:ai"}
	betaScope := Scope{CityRef: "city:beta", StoreRef: "rig:ai"}
	alphaRevision, _ := WorkRevision(alphaWork)
	betaRevision, _ := WorkRevision(betaWork)
	proposal := Proposal{Questions: []Question{{ID: "scope", Title: "Scope", Prompt: "Choose."}}}
	alpha, err := (Service{}).Ensure(context.Background(), alphaStore, alphaScope, alphaWork.ID, alphaRevision, proposal)
	if err != nil {
		t.Fatal(err)
	}
	beta, err := (Service{}).Ensure(context.Background(), betaStore, betaScope, betaWork.ID, betaRevision, proposal)
	if err != nil {
		t.Fatal(err)
	}
	question := alpha.OpenQuestions[0]
	submission := AnswerSubmission{
		TicketID: question.TicketID, WorkRevision: alphaRevision, QuestionVersion: question.Version,
		Resolution: ResolutionAnswered, Text: "Proceed", Proof: "signed",
	}
	wrongCity := Service{Verifier: verifierFunc(func(_ context.Context, challenge AnswerChallenge, _ AnswerSubmission) (VerifiedAnswer, error) {
		return VerifiedAnswer{
			CityRef: betaScope.CityRef, StoreRef: challenge.StoreRef,
			KeyID: "human-key", Issuer: "human-issuer", Subject: "authorized-human",
			WorkID: challenge.WorkID, WorkRevision: challenge.WorkRevision, WorkDigest: challenge.WorkDigest,
			MapID: challenge.MapID, TicketID: challenge.TicketID, QuestionVersion: challenge.QuestionVersion,
			QuestionID:   challenge.QuestionID,
			AnswerDigest: challenge.AnswerDigest, Resolution: challenge.Resolution,
		}, nil
	})}
	if _, err := wrongCity.Answer(context.Background(), alphaStore, alphaScope, alphaWork.ID, submission); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("answer signed for another city = %v, want unauthorized", err)
	}
	current, err := alphaStore.Get(alphaWork.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !beads.HasDecisionFrontierHold(current) {
		t.Fatal("wrong-city verifier response released the source hold")
	}
	validVerifier := verifierFunc(func(_ context.Context, challenge AnswerChallenge, sub AnswerSubmission) (VerifiedAnswer, error) {
		return VerifiedAnswer{
			CityRef: challenge.CityRef, StoreRef: challenge.StoreRef,
			KeyID: "human-key", Issuer: "human-issuer", Subject: "authorized-human",
			WorkID: challenge.WorkID, WorkRevision: challenge.WorkRevision, WorkDigest: challenge.WorkDigest,
			MapID: challenge.MapID, TicketID: challenge.TicketID, QuestionVersion: challenge.QuestionVersion,
			QuestionID:   challenge.QuestionID,
			AnswerDigest: AnswerDigest(sub.Resolution, sub.Text), Resolution: sub.Resolution,
		}, nil
	})
	if _, err := (Service{Verifier: validVerifier}).Answer(context.Background(), betaStore, betaScope, betaWork.ID, submission); !errors.Is(err, ErrInvalid) {
		t.Fatalf("ticket replayed into another city = %v, want invalid ticket", err)
	}
	if alpha.MapID == beta.MapID || alpha.Questions[0].TicketID == beta.Questions[0].TicketID {
		t.Fatalf("city-specific ticket identity missing: alpha=%+v beta=%+v", alpha, beta)
	}
}

func TestOutOfBandRevisionInvalidatesFrontierEvenWhenDigestReturns(t *testing.T) {
	dir := t.TempDir()
	store, err := beads.OpenSQLiteStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := store.(*beads.SQLiteStore).CloseStore(); err != nil {
			t.Errorf("close SQLite store: %v", err)
		}
	}()
	work, err := store.Create(beads.Bead{
		ID: "wrk-noop", Type: "task", Title: "Exact source",
		Metadata: beads.StringMap{"gc.probe": ""},
	})
	if err != nil {
		t.Fatal(err)
	}
	// A new SQLite row starts with revision zero. Give the source a durable
	// nonzero revision before asking the frontier service to reserve it.
	title := "Revisioned exact source"
	if err := store.Update(work.ID, beads.UpdateOpts{Title: &title}); err != nil {
		t.Fatal(err)
	}
	work, err = store.Get(work.ID)
	if err != nil {
		t.Fatal(err)
	}
	revision, err := WorkRevision(work)
	if err != nil {
		t.Fatal(err)
	}
	frontier, err := (Service{}).Ensure(context.Background(), store, testCityScope(), work.ID, revision,
		Proposal{Questions: []Question{{ID: "q", Title: "Question", Prompt: "Choose."}}})
	if err != nil {
		t.Fatal(err)
	}
	reserved, err := store.Get(work.ID)
	if err != nil {
		t.Fatal(err)
	}
	initialDigest, err := WorkDigest(reserved)
	if err != nil {
		t.Fatal(err)
	}
	// Generic writers must reject changes under the frontier hold. Simulate
	// an out-of-band backend revision change without changing source content
	// to verify that the reader checks the exact token as well as its digest.
	db, err := sql.Open("sqlite", filepath.Join(dir, "beads.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Errorf("close out-of-band SQLite connection: %v", err)
		}
	}()
	result, err := db.Exec(`UPDATE beads SET revision = revision + 2 WHERE id = ?`, work.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rows, err := result.RowsAffected(); err != nil || rows != 1 {
		t.Fatalf("out-of-band revision change affected %d rows, err %v", rows, err)
	}
	after, err := store.Get(work.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Revision == reserved.Revision {
		t.Fatal("out-of-band write did not change the source revision")
	}
	afterDigest, err := WorkDigest(after)
	if err != nil {
		t.Fatal(err)
	}
	if afterDigest != initialDigest {
		t.Fatalf("test setup did not restore identical source content: before %s after %s", initialDigest, afterDigest)
	}
	if _, err := (Service{}).Read(context.Background(), store, testCityScope(), work.ID, revision); !errors.Is(err, ErrStale) {
		t.Fatalf("read after same-content intervening revisions = %v, want stale", err)
	}
	if frontier.WorkDigest != initialDigest {
		t.Fatalf("frontier digest = %s, source digest = %s", frontier.WorkDigest, initialDigest)
	}
}

func TestChangedStatusOwnershipAndHoldsInvalidateOriginalRevision(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*testing.T, *beads.MemStore, string)
	}{
		{name: "status", mutate: func(t *testing.T, store *beads.MemStore, id string) {
			status := "in_progress"
			if err := store.Update(id, beads.UpdateOpts{Status: &status}); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "assignee", mutate: func(t *testing.T, store *beads.MemStore, id string) {
			assignee := "worker-a"
			if err := store.Update(id, beads.UpdateOpts{Assignee: &assignee}); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "hold added", mutate: func(t *testing.T, store *beads.MemStore, id string) {
			if err := store.Update(id, beads.UpdateOpts{Labels: []string{beadmeta.HoldMayorLabel}}); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "hold removed", mutate: func(t *testing.T, store *beads.MemStore, id string) {
			if err := store.Update(id, beads.UpdateOpts{RemoveLabels: []string{beadmeta.HoldExternalLabel}}); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "closed then reopened", mutate: func(t *testing.T, store *beads.MemStore, id string) {
			closed, open := "closed", "open"
			if err := store.Update(id, beads.UpdateOpts{Status: &closed}); err != nil {
				t.Fatal(err)
			}
			if err := store.Update(id, beads.UpdateOpts{Status: &open}); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := beads.NewMemStore()
			store.HonorExplicitIDs = true
			labels := []string(nil)
			if tc.name == "hold removed" {
				labels = []string{beadmeta.HoldExternalLabel}
			}
			work, err := store.Create(beads.Bead{ID: "wrk-change", Type: "task", Title: "Exact source", Labels: labels})
			if err != nil {
				t.Fatal(err)
			}
			revision, err := WorkRevision(work)
			if err != nil {
				t.Fatal(err)
			}
			beforeDigest, err := WorkDigest(work)
			if err != nil {
				t.Fatal(err)
			}
			tc.mutate(t, store, work.ID)
			changed, err := store.Get(work.ID)
			if err != nil {
				t.Fatal(err)
			}
			changedDigest, err := WorkDigest(changed)
			if err != nil {
				t.Fatal(err)
			}
			if changed.Revision == work.Revision || changedDigest == beforeDigest {
				t.Fatalf("source change did not alter exact revision/digest: before=%+v/%s after=%+v/%s", work, beforeDigest, changed, changedDigest)
			}
			_, err = (Service{}).Ensure(context.Background(), store, testCityScope(), work.ID, revision,
				Proposal{Questions: []Question{{ID: "q", Title: "Question", Prompt: "Choose."}}})
			if !errors.Is(err, ErrStale) {
				t.Fatalf("Ensure after source %s change = %v, want stale", tc.name, err)
			}
		})
	}
}

func TestOrdinaryMutationCannotChangeHeldFrontierSource(t *testing.T) {
	store := beads.NewMemStore()
	store.HonorExplicitIDs = true
	work, err := store.Create(beads.Bead{ID: "wrk-protected", Type: "task", Status: "in_progress", Title: "Protected source"})
	if err != nil {
		t.Fatal(err)
	}
	status := "in_progress"
	if err := store.Update(work.ID, beads.UpdateOpts{Status: &status}); err != nil {
		t.Fatal(err)
	}
	work, err = store.Get(work.ID)
	if err != nil {
		t.Fatal(err)
	}
	revision, _ := WorkRevision(work)
	if _, err := (Service{}).Ensure(context.Background(), store, testCityScope(), work.ID, revision,
		Proposal{Questions: []Question{{ID: "q", Title: "Question", Prompt: "Choose."}}}); err != nil {
		t.Fatal(err)
	}
	closed, assignee := "closed", "worker-a"
	for _, update := range []beads.UpdateOpts{
		{Status: &closed},
		{Assignee: &assignee},
		{Labels: []string{beadmeta.HoldExternalLabel}},
		{Metadata: map[string]string{"gc.unrelated": "changed"}},
	} {
		if err := store.Update(work.ID, update); !errors.Is(err, beads.ErrDecisionFrontierMutationBlocked) {
			t.Fatalf("ordinary held-source update %+v = %v, want protected mutation rejection", update, err)
		}
	}
	writer, ok := beads.ConditionalWriterFor(store)
	if !ok {
		t.Fatal("MemStore did not expose conditional writer")
	}
	current, err := store.Get(work.ID)
	if err != nil {
		t.Fatal(err)
	}
	reserved := transitionReceiptForTest(t, current, reservationReceiptID(testCityScope(), frontierMapID(testCityScope(), work.ID, revision)))
	if current.Revision != reserved.ToRevision || !beads.HasDecisionFrontierHold(current) {
		t.Fatalf("failed ordinary mutations changed the reservation: original=%+v current=%+v", work, current)
	}
	for key, expected := range map[string]string{
		beadmeta.DecisionFrontierHoldMetadataKey:             current.Metadata[beadmeta.DecisionFrontierHoldMetadataKey],
		beadmeta.DecisionFrontierRevisionReceiptsMetadataKey: current.Metadata[beadmeta.DecisionFrontierRevisionReceiptsMetadataKey],
	} {
		if _, err := writer.CompareAndSetMetadataKey(work.ID, key, expected, ""); !errors.Is(err, beads.ErrDecisionFrontierMutationBlocked) {
			t.Fatalf("ordinary conditional write to protected source key %q = %v, want blocked", key, err)
		}
	}
	if err := store.Close(work.ID); !errors.Is(err, beads.ErrDecisionFrontierMutationBlocked) {
		t.Fatalf("ordinary close of held source = %v, want blocked", err)
	}
	if err := store.Reopen(work.ID); !errors.Is(err, beads.ErrDecisionFrontierMutationBlocked) {
		t.Fatalf("ordinary reopen of held source = %v, want blocked", err)
	}
	if err := writer.DeleteIfMatch(work.ID, current.Revision); !errors.Is(err, beads.ErrDecisionFrontierRecordProtected) {
		t.Fatalf("ordinary deletion of held source = %v, want protected", err)
	}
}

func TestDecisionFrontierRecordsRejectOrdinaryMutationAndDeletion(t *testing.T) {
	store := beads.NewMemStore()
	store.HonorExplicitIDs = true
	work, err := store.Create(beads.Bead{ID: "wrk-protect-records", Type: "task", Title: "Protected records"})
	if err != nil {
		t.Fatal(err)
	}
	revision, _ := WorkRevision(work)
	frontier, err := (Service{}).Ensure(context.Background(), store, testCityScope(), work.ID, revision,
		Proposal{Questions: []Question{{ID: "q", Title: "Question", Prompt: "Choose."}}})
	if err != nil {
		t.Fatal(err)
	}
	writer, ok := beads.ConditionalWriterFor(store)
	if !ok {
		t.Fatal("MemStore did not expose conditional writer")
	}
	forgedTitle := "forged"
	for _, id := range []string{frontier.MapID, frontier.Questions[0].TicketID, frontier.Prompt.ID} {
		if err := store.Update(id, beads.UpdateOpts{Title: &forgedTitle}); !errors.Is(err, beads.ErrDecisionFrontierMutationBlocked) {
			t.Errorf("ordinary update of record %s = %v, want blocked", id, err)
		}
		if err := store.Close(id); !errors.Is(err, beads.ErrDecisionFrontierMutationBlocked) {
			t.Errorf("ordinary close of record %s = %v, want blocked", id, err)
		}
		b, err := store.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		if err := writer.DeleteIfMatch(id, b.Revision); !errors.Is(err, beads.ErrDecisionFrontierRecordProtected) {
			t.Errorf("ordinary deletion of record %s = %v, want protected", id, err)
		}
	}
}

func TestEnsureFailsClosedBeforeCreatingAnythingWithoutRequiredStoreCapabilities(t *testing.T) {
	for _, tc := range []struct {
		name  string
		store *beads.MemStore
	}{
		{name: "no stable IDs", store: beads.NewMemStore()},
		{name: "conditional writes disabled", store: func() *beads.MemStore {
			s := beads.NewMemStore()
			s.HonorExplicitIDs = true
			s.DisableConditionalWrites = true
			return s
		}()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			work, err := tc.store.Create(beads.Bead{ID: "wrk-unavailable", Type: "task", Title: "No capability"})
			if err != nil {
				t.Fatal(err)
			}
			revision, err := WorkRevision(work)
			if err != nil {
				t.Fatal(err)
			}
			_, err = (Service{}).Ensure(context.Background(), tc.store, testCityScope(), work.ID, revision,
				Proposal{Questions: []Question{{ID: "q", Title: "Question", Prompt: "Choose."}}})
			if !errors.Is(err, ErrUnavailable) && !errors.Is(err, beads.ErrConditionalWriteUnsupported) {
				t.Fatalf("Ensure error = %v, want unavailable", err)
			}
			current, getErr := tc.store.Get(work.ID)
			if getErr != nil {
				t.Fatal(getErr)
			}
			if current.Metadata[beadmeta.DecisionFrontierHoldMetadataKey] != "" {
				t.Fatalf("unsupported store left a source hold: %+v", current.Metadata)
			}
			rows, listErr := tc.store.List(beads.ListQuery{AllowScan: true, IncludeClosed: true})
			if listErr != nil {
				t.Fatal(listErr)
			}
			if len(rows) != 1 {
				t.Fatalf("unsupported store created dependent records: %d rows", len(rows))
			}
		})
	}
}

func TestEnsureRejectsStaleRevisionAndCyclicQuestionGraph(t *testing.T) {
	store := beads.NewMemStore()
	store.HonorExplicitIDs = true
	work, err := store.Create(beads.Bead{ID: "wrk-stale", Type: "task", Title: "Initial"})
	if err != nil {
		t.Fatal(err)
	}
	revision, err := WorkRevision(work)
	if err != nil {
		t.Fatal(err)
	}
	updatedTitle := "Changed before the frontier"
	if err := store.Update(work.ID, beads.UpdateOpts{Title: &updatedTitle}); err != nil {
		t.Fatal(err)
	}
	_, err = (Service{}).Ensure(context.Background(), store, testCityScope(), work.ID, revision,
		Proposal{Questions: []Question{{ID: "q", Title: "Question", Prompt: "Choose."}}})
	if !errors.Is(err, ErrStale) {
		t.Fatalf("Ensure stale source error = %v, want stale", err)
	}

	current, err := store.Get(work.ID)
	if err != nil {
		t.Fatal(err)
	}
	currentRevision, err := WorkRevision(current)
	if err != nil {
		t.Fatal(err)
	}
	_, err = (Service{}).Ensure(context.Background(), store, testCityScope(), work.ID, currentRevision,
		Proposal{Questions: []Question{
			{ID: "a", Title: "A", Prompt: "A?", DependsOn: []string{"b"}},
			{ID: "b", Title: "B", Prompt: "B?", DependsOn: []string{"a"}},
		}})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("Ensure cyclic question error = %v, want invalid", err)
	}
}

func TestAnswerVerifierMustBindExactQuestionAndUnresolvedAnswerKeepsWorkHeld(t *testing.T) {
	store := beads.NewMemStore()
	store.HonorExplicitIDs = true
	work, err := store.Create(beads.Bead{ID: "wrk-verify", Type: "task", Title: "Choose"})
	if err != nil {
		t.Fatal(err)
	}
	revision, _ := WorkRevision(work)
	front, err := (Service{}).Ensure(context.Background(), store, testCityScope(), work.ID, revision,
		Proposal{Questions: []Question{{ID: "q", Title: "Question", Prompt: "Choose."}}})
	if err != nil {
		t.Fatal(err)
	}
	question := front.OpenQuestions[0]
	submission := AnswerSubmission{
		TicketID: question.TicketID, WorkRevision: revision, QuestionVersion: question.Version,
		Resolution: ResolutionDeclined, Text: "Declined after review", Proof: "signed",
	}
	service := Service{Verifier: verifierFunc(func(_ context.Context, challenge AnswerChallenge, _ AnswerSubmission) (VerifiedAnswer, error) {
		return VerifiedAnswer{
			CityRef: challenge.CityRef, StoreRef: challenge.StoreRef, WorkDigest: challenge.WorkDigest,
			KeyID: "human-key", Issuer: "human-authority", Subject: "worker-forgery",
			WorkID: challenge.WorkID, WorkRevision: challenge.WorkRevision, MapID: challenge.MapID,
			TicketID: challenge.TicketID, QuestionID: challenge.QuestionID + "-changed", QuestionVersion: challenge.QuestionVersion,
			AnswerDigest: challenge.AnswerDigest, Resolution: challenge.Resolution,
		}, nil
	})}
	if _, err := service.Answer(context.Background(), store, testCityScope(), work.ID, submission); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("answer verifier response for a different question ID = %v, want unauthorized", err)
	}
	stillHeld, err := store.Get(work.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !beads.HasDecisionFrontierHold(stillHeld) {
		t.Fatal("question-ID mismatch released source work")
	}
	service.Verifier = verifierFunc(func(_ context.Context, challenge AnswerChallenge, _ AnswerSubmission) (VerifiedAnswer, error) {
		return VerifiedAnswer{
			CityRef: challenge.CityRef, StoreRef: challenge.StoreRef, WorkDigest: challenge.WorkDigest,
			KeyID: "human-key", Issuer: "human-authority", Subject: "worker-forgery",
			WorkID: challenge.WorkID, WorkRevision: challenge.WorkRevision, MapID: challenge.MapID,
			TicketID: challenge.TicketID, QuestionID: challenge.QuestionID, QuestionVersion: challenge.QuestionVersion,
			AnswerDigest: challenge.AnswerDigest, Resolution: challenge.Resolution,
		}, nil
	})
	resolved, err := service.Answer(context.Background(), store, testCityScope(), work.ID, submission)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.State != StatePending || len(resolved.OpenQuestions) != 1 || resolved.OpenQuestions[0].Answer == nil ||
		resolved.OpenQuestions[0].Answer.Resolution != ResolutionDeclined {
		t.Fatalf("declined answer should be recorded without resolving: %+v", resolved)
	}
	current, err := store.Get(work.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Metadata[beadmeta.DecisionFrontierHoldMetadataKey] == "" {
		t.Fatal("declined answer released source work")
	}
}

func TestExactAnswerReplayReturnsPersistedFrontierWithoutAddingRows(t *testing.T) {
	store := beads.NewMemStore()
	store.HonorExplicitIDs = true
	work, err := store.Create(beads.Bead{ID: "wrk-replay-answer", Type: "task", Title: "Choose"})
	if err != nil {
		t.Fatal(err)
	}
	revision, _ := WorkRevision(work)
	service := Service{Verifier: verifierFunc(func(_ context.Context, challenge AnswerChallenge, _ AnswerSubmission) (VerifiedAnswer, error) {
		return VerifiedAnswer{
			CityRef: challenge.CityRef, StoreRef: challenge.StoreRef, WorkDigest: challenge.WorkDigest,
			KeyID: "human-key", Issuer: "authority", Subject: "ricky",
			WorkID: challenge.WorkID, WorkRevision: challenge.WorkRevision, MapID: challenge.MapID,
			TicketID: challenge.TicketID, QuestionID: challenge.QuestionID, QuestionVersion: challenge.QuestionVersion,
			AnswerDigest: challenge.AnswerDigest, Resolution: challenge.Resolution,
		}, nil
	})}
	front, err := (Service{}).Ensure(context.Background(), store, testCityScope(), work.ID, revision,
		Proposal{Questions: []Question{{ID: "q", Title: "Question", Prompt: "Choose."}}})
	if err != nil {
		t.Fatal(err)
	}
	question := front.OpenQuestions[0]
	submission := AnswerSubmission{
		TicketID: question.TicketID, WorkRevision: revision, QuestionVersion: question.Version,
		Resolution: ResolutionAnswered, Text: "Proceed within the accepted scope", Proof: "proof",
	}
	if _, err := service.Answer(context.Background(), store, testCityScope(), work.ID, submission); err != nil {
		t.Fatal(err)
	}
	replayed, err := service.Answer(context.Background(), store, testCityScope(), work.ID, submission)
	if err != nil {
		t.Fatal(err)
	}
	if replayed.State != StateResolved || len(replayed.Questions) != 1 || replayed.Questions[0].Answer == nil {
		t.Fatalf("replay did not return exact answer: %+v", replayed)
	}
	rows, err := store.List(beads.ListQuery{Type: "gate", AllowScan: true, IncludeClosed: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 4 { // map, one question, prompt, one immutable answer
		t.Fatalf("gate rows after replay = %d, want 4", len(rows))
	}
}

func TestPromptDeliveryReconcilesUnknownBeforeRetryingEffect(t *testing.T) {
	store := beads.NewMemStore()
	store.HonorExplicitIDs = true
	work, err := store.Create(beads.Bead{ID: "wrk-prompt", Type: "task", Title: "Choose"})
	if err != nil {
		t.Fatal(err)
	}
	revision, _ := WorkRevision(work)
	delivery := &promptDeliveryFake{binding: PromptBinding{SessionID: "session-selected", ExecutionGeneration: 7}, deliverErr: fmt.Errorf("connection dropped"), reconcile: PromptResult{Status: "absent", DefinitivelyAbsent: true}}
	service := Service{Delivery: delivery}
	proposal := Proposal{Questions: []Question{{ID: "q", Title: "Question", Prompt: "Choose."}}}
	if _, err := service.Ensure(context.Background(), store, testCityScope(), work.ID, revision, proposal); err == nil {
		t.Fatal("initial ambiguous delivery should surface its uncertainty")
	}
	if delivery.deliveries != 1 {
		t.Fatalf("deliveries = %d, want first attempt", delivery.deliveries)
	}
	frontier, err := service.Ensure(context.Background(), store, testCityScope(), work.ID, revision, proposal)
	if err != nil {
		t.Fatal(err)
	}
	if delivery.reconciliations != 1 || delivery.deliveries != 2 {
		t.Fatalf("delivery sequence = %d sends, %d reconciliations; want reconcile before one retry", delivery.deliveries, delivery.reconciliations)
	}
	if delivery.resolutions != 1 {
		t.Fatalf("binding resolutions = %d, want one across duplicate Ensure", delivery.resolutions)
	}
	if delivery.deliveredBinding != delivery.reconciledBinding || delivery.deliveredRequest.ID == "" || delivery.deliveredBinding.RequestID != delivery.deliveredRequest.ID {
		t.Fatalf("retry changed exact prompt binding: delivered=%+v reconciled=%+v request=%+v", delivery.deliveredBinding, delivery.reconciledBinding, delivery.deliveredRequest)
	}
	if !promptRequestsEqual(delivery.deliveredRequest, delivery.reconciledRequest) {
		t.Fatalf("retry changed prompt payload:\n delivered=%+v\n reconciled=%+v", delivery.deliveredRequest, delivery.reconciledRequest)
	}
	if frontier.Prompt.Status != "accepted" {
		t.Fatalf("prompt status = %q, want accepted", frontier.Prompt.Status)
	}
}

func TestPromptBindingAndPresentationSurviveRestartAndReplay(t *testing.T) {
	dir := t.TempDir()
	opened, err := beads.OpenSQLiteStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	store := opened.(*beads.SQLiteStore)
	var activeStore beads.Store = store
	work, err := store.Create(beads.Bead{Type: "task", Title: "Choose a deployment", Description: "Revisioned source"})
	if err != nil {
		t.Fatal(err)
	}
	if work.Revision == 0 {
		description := "source revision established"
		if err := store.Update(work.ID, beads.UpdateOpts{Description: &description}); err != nil {
			t.Fatal(err)
		}
		work, err = store.Get(work.ID)
		if err != nil {
			t.Fatal(err)
		}
	}
	revision, err := WorkRevision(work)
	if err != nil {
		t.Fatal(err)
	}
	issue := &SourceIssueRef{TrackerKind: "github", Repository: "github.com/example/gascity", IssueID: "17", CanonicalURL: "https://github.com/example/gascity/issues/17"}
	proposal := Proposal{
		Questions: []Question{
			{ID: "scope", Title: "Pilot scope", Prompt: "Choose the pilot scope.", Recommendations: []string{"one rig"}, SourceLinks: []string{"https://example.test/notes"}},
			{ID: "rollout", Title: "Rollout", Prompt: "Choose the rollout after scope.", DependsOn: []string{"scope"}},
		},
		SourceLinks: map[string]string{"design": "https://example.test/design"}, SourceIssue: issue,
	}
	scope := testCityScope()
	mapID := frontierMapID(scope, work.ID, revision)
	promptID := frontierPromptID(scope, mapID)
	delivery := &promptDeliveryFake{
		binding:    PromptBinding{SessionID: "session-a", ExecutionGeneration: 4},
		deliverErr: fmt.Errorf("connection dropped after submit"),
		reconcile:  PromptResult{Status: "delivered"},
		beforeDelivery: func(request PromptRequest, _ PromptBinding) error {
			for _, id := range []string{mapID, promptID, frontierQuestionID(scope, mapID, "scope"), frontierQuestionID(scope, mapID, "rollout")} {
				if _, err := activeStore.Get(id); err != nil {
					return fmt.Errorf("record %s missing before delivery: %w", id, err)
				}
			}
			held, err := activeStore.Get(work.ID)
			if err != nil || held.Metadata[beadmeta.DecisionFrontierHoldMetadataKey] == "" {
				return fmt.Errorf("source hold missing before delivery: bead=%+v err=%v", held, err)
			}
			if request.WorkDigest == "" || len(request.Questions) != 2 {
				return fmt.Errorf("delivery request is incomplete: %+v", request)
			}
			return nil
		},
	}
	service := Service{Delivery: delivery}
	if _, err := service.Ensure(context.Background(), store, scope, work.ID, revision, proposal); !errors.Is(err, ErrPromptDeliveryUnavailable) {
		t.Fatalf("first uncertain delivery error = %v, want ErrPromptDeliveryUnavailable", err)
	}
	if delivery.resolutions != 1 {
		t.Fatalf("binding resolver calls = %d, want 1", delivery.resolutions)
	}
	if delivery.deliveredBinding.RequestID != promptID {
		t.Fatalf("resolved tracked request ID = %q, want %q", delivery.deliveredBinding.RequestID, promptID)
	}

	mapBead, err := store.Get(mapID)
	if err != nil {
		t.Fatal(err)
	}
	var persistedMap mapRecord
	if err := json.Unmarshal([]byte(mapBead.Description), &persistedMap); err != nil {
		t.Fatal(err)
	}
	if persistedMap.PromptBinding == nil || *persistedMap.PromptBinding != delivery.deliveredBinding {
		t.Fatalf("map binding = %+v, want %+v", persistedMap.PromptBinding, delivery.deliveredBinding)
	}
	promptBead, err := store.Get(promptID)
	if err != nil {
		t.Fatal(err)
	}
	var persistedPrompt promptRecord
	if err := json.Unmarshal([]byte(promptBead.Description), &persistedPrompt); err != nil {
		t.Fatal(err)
	}
	if persistedPrompt.PromptBinding == nil || *persistedPrompt.PromptBinding != delivery.deliveredBinding || len(persistedPrompt.Questions) != 2 {
		t.Fatalf("persisted prompt binding/presentation = %+v, %+v", persistedPrompt.PromptBinding, persistedPrompt.Questions)
	}
	wantPresentation := []PromptQuestionPresentation{
		{ID: "scope", TicketID: frontierQuestionID(scope, mapID, "scope"), Version: persistedPrompt.Questions[0].Version,
			Title: "Pilot scope", Prompt: "Choose the pilot scope.", Recommendations: []string{"one rig"}, SourceLinks: []string{"https://example.test/notes"}, SourceIssue: issue},
		{ID: "rollout", TicketID: frontierQuestionID(scope, mapID, "rollout"), Version: persistedPrompt.Questions[1].Version,
			Title: "Rollout", Prompt: "Choose the rollout after scope.", DependsOn: []string{"scope"}, SourceIssue: issue},
	}
	gotJSON, _ := json.Marshal(persistedPrompt.Questions)
	wantJSON, _ := json.Marshal(wantPresentation)
	if !slices.Equal(gotJSON, wantJSON) || persistedPrompt.WorkDigest != persistedMap.WorkDigest || !maps.Equal(persistedPrompt.SourceLinks, proposal.SourceLinks) || !sourceIssueRefsEqual(persistedPrompt.SourceIssue, issue) {
		t.Fatalf("persisted prompt content does not match immutable selected presentation:\n got %s\nwant %s", gotJSON, wantJSON)
	}
	if persistedPrompt.PresentationVersion != promptPresentationVersion || persistedPrompt.MessageDigest == "" || persistedPrompt.MessageDigest != delivery.deliveredRequest.MessageDigest || delivery.deliveredRequest.MessageDigest != promptMessageDigest(delivery.deliveredRequest) {
		t.Fatalf("prompt presentation identity = version %d digest %q; request=%+v", persistedPrompt.PresentationVersion, persistedPrompt.MessageDigest, delivery.deliveredRequest)
	}
	if err := store.CloseStore(); err != nil {
		t.Fatal(err)
	}

	reopened, err := beads.OpenSQLiteStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	reopenedStore := reopened.(*beads.SQLiteStore)
	t.Cleanup(func() { _ = reopenedStore.CloseStore() })
	activeStore = reopenedStore
	delivery.binding = PromptBinding{SessionID: "session-changed", ExecutionGeneration: 99}
	delivery.deliverErr = nil
	frontier, err := (Service{Delivery: delivery}).Ensure(context.Background(), reopenedStore, scope, work.ID, revision, proposal)
	if err != nil {
		t.Fatal(err)
	}
	if delivery.resolutions != 1 || delivery.reconciliations != 1 || delivery.reconciledBinding != delivery.deliveredBinding {
		t.Fatalf("restart re-resolved or changed binding: resolutions=%d reconciliations=%d delivered=%+v reconciled=%+v", delivery.resolutions, delivery.reconciliations, delivery.deliveredBinding, delivery.reconciledBinding)
	}
	if !promptRequestsEqual(delivery.deliveredRequest, delivery.reconciledRequest) {
		t.Fatalf("restart changed persisted prompt request:\n delivered=%+v\n reconciled=%+v", delivery.deliveredRequest, delivery.reconciledRequest)
	}
	if frontier.Prompt.Status != "delivered" {
		t.Fatalf("prompt status after reconciliation = %q, want delivered", frontier.Prompt.Status)
	}
}

func TestMalformedPromptBindingFailsBeforeProtectiveWrites(t *testing.T) {
	for name, binding := range map[string]PromptBinding{
		"missing session":        {ExecutionGeneration: 1},
		"nonpositive generation": {SessionID: "session", ExecutionGeneration: 0},
		"wrong tracked request":  {SessionID: "session", ExecutionGeneration: 1, RequestID: "another-request"},
	} {
		t.Run(name, func(t *testing.T) {
			store := beads.NewMemStore()
			store.HonorExplicitIDs = true
			work, err := store.Create(beads.Bead{ID: "wrk-invalid-binding", Type: "task", Title: "Choose"})
			if err != nil {
				t.Fatal(err)
			}
			revision, _ := WorkRevision(work)
			delivery := &promptDeliveryFake{binding: binding}
			if _, err := (Service{Delivery: delivery}).Ensure(context.Background(), store, testCityScope(), work.ID, revision,
				Proposal{Questions: []Question{{ID: "q", Title: "Question", Prompt: "Choose."}}}); err == nil {
				t.Fatal("Ensure succeeded with malformed or wrong binding")
			}
			rows, err := store.List(beads.ListQuery{Type: "gate", AllowScan: true, IncludeClosed: true})
			if err != nil {
				t.Fatal(err)
			}
			current, err := store.Get(work.ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != 0 || current.Metadata[beadmeta.DecisionFrontierHoldMetadataKey] != "" || delivery.deliveries != 0 {
				t.Fatalf("invalid binding caused writes/effects: records=%d hold=%q deliveries=%d", len(rows), current.Metadata[beadmeta.DecisionFrontierHoldMetadataKey], delivery.deliveries)
			}
		})
	}
}

func TestConcurrentEnsureAdoptsPersistedPromptBinding(t *testing.T) {
	store := beads.NewMemStore()
	store.HonorExplicitIDs = true
	work, err := store.Create(beads.Bead{ID: "wrk-concurrent-prompt-binding", Type: "task", Title: "Choose"})
	if err != nil {
		t.Fatal(err)
	}
	revision, _ := WorkRevision(work)
	proposal := Proposal{Questions: []Question{{ID: "q", Title: "Question", Prompt: "Choose."}}}
	ready := make(chan struct{}, 2)
	release := make(chan struct{})
	deliveries := []*promptDeliveryFake{
		{binding: PromptBinding{SessionID: "session-first", ExecutionGeneration: 1}, reconcile: PromptResult{Status: "delivered"}},
		{binding: PromptBinding{SessionID: "session-second", ExecutionGeneration: 2}, reconcile: PromptResult{Status: "delivered"}},
	}
	for _, delivery := range deliveries {
		delivery.resolvePrompt = func(_ context.Context, request PromptRequest) (PromptBinding, error) {
			ready <- struct{}{}
			<-release
			binding := delivery.binding
			binding.RequestID = request.ID
			return binding, nil
		}
	}
	type ensureResult struct {
		index int
		err   error
	}
	results := make(chan ensureResult, len(deliveries))
	for i, delivery := range deliveries {
		go func(index int, delivery *promptDeliveryFake) {
			_, err := (Service{Delivery: delivery}).Ensure(context.Background(), store, testCityScope(), work.ID, revision, proposal)
			results <- ensureResult{index: index, err: err}
		}(i, delivery)
	}
	<-ready
	<-ready
	close(release)
	var succeeded bool
	for range deliveries {
		result := <-results
		if result.err == nil {
			succeeded = true
		} else if !errors.Is(result.err, ErrConflict) && !errors.Is(result.err, ErrPromptDeliveryUnavailable) {
			t.Errorf("concurrent Ensure %d error = %v", result.index, result.err)
		}
	}
	if !succeeded {
		t.Fatal("both concurrent Ensure calls failed")
	}
	mapID := frontierMapID(testCityScope(), work.ID, revision)
	mapBead, err := store.Get(mapID)
	if err != nil {
		t.Fatal(err)
	}
	var persisted mapRecord
	if err := json.Unmarshal([]byte(mapBead.Description), &persisted); err != nil {
		t.Fatal(err)
	}
	if persisted.PromptBinding == nil || !validPromptBinding(*persisted.PromptBinding, frontierPromptID(testCityScope(), mapID)) {
		t.Fatalf("persisted concurrent winner binding = %+v", persisted.PromptBinding)
	}
	var observed int
	for i, delivery := range deliveries {
		if delivery.resolutions != 1 {
			t.Errorf("resolver %d calls = %d, want one per concurrent caller", i, delivery.resolutions)
		}
		if delivery.deliveries > 0 {
			observed++
			if delivery.deliveredBinding != *persisted.PromptBinding {
				t.Errorf("delivery %d used losing binding %+v, persisted winner is %+v", i, delivery.deliveredBinding, *persisted.PromptBinding)
			}
		}
		if delivery.reconciliations > 0 {
			observed++
			if delivery.reconciledBinding != *persisted.PromptBinding {
				t.Errorf("reconciliation %d used losing binding %+v, persisted winner is %+v", i, delivery.reconciledBinding, *persisted.PromptBinding)
			}
		}
	}
	if observed == 0 {
		t.Fatal("concurrent Ensure did not reach delivery or reconciliation")
	}
}

func TestExistingUnboundMapAndChangedPromptBindingFailClosed(t *testing.T) {
	t.Run("unbound map cannot be enabled later", func(t *testing.T) {
		store := beads.NewMemStore()
		store.HonorExplicitIDs = true
		work, err := store.Create(beads.Bead{ID: "wrk-unbound-prompt", Type: "task", Title: "Choose"})
		if err != nil {
			t.Fatal(err)
		}
		revision, _ := WorkRevision(work)
		proposal := Proposal{Questions: []Question{{ID: "q", Title: "Question", Prompt: "Choose."}}}
		first, err := (Service{}).Ensure(context.Background(), store, testCityScope(), work.ID, revision, proposal)
		if err != nil {
			t.Fatal(err)
		}
		before, err := store.List(beads.ListQuery{Type: "gate", AllowScan: true, IncludeClosed: true})
		if err != nil {
			t.Fatal(err)
		}
		delivery := &promptDeliveryFake{binding: PromptBinding{SessionID: "late-session", ExecutionGeneration: 1}}
		if _, err := (Service{Delivery: delivery}).Ensure(context.Background(), store, testCityScope(), work.ID, revision, proposal); !errors.Is(err, ErrConflict) {
			t.Fatalf("enabling delivery for unbound map error = %v, want ErrConflict", err)
		}
		after, err := store.List(beads.ListQuery{Type: "gate", AllowScan: true, IncludeClosed: true})
		if err != nil {
			t.Fatal(err)
		}
		if len(after) != len(before) || delivery.resolutions != 0 || delivery.deliveries != 0 || delivery.reconciliations != 0 {
			t.Fatalf("unbound delivery enablement wrote or caused effects: before=%d after=%d fake=%+v", len(before), len(after), delivery)
		}
		replayed, err := (Service{}).Read(context.Background(), store, testCityScope(), work.ID, revision)
		if err != nil || replayed.MapID != first.MapID {
			t.Fatalf("unconfigured map changed after rejected enablement: frontier=%+v err=%v", replayed, err)
		}
	})

	t.Run("changed persisted binding fails before effects", func(t *testing.T) {
		store := beads.NewMemStore()
		store.HonorExplicitIDs = true
		work, err := store.Create(beads.Bead{ID: "wrk-changed-binding", Type: "task", Title: "Choose"})
		if err != nil {
			t.Fatal(err)
		}
		revision, _ := WorkRevision(work)
		proposal := Proposal{Questions: []Question{{ID: "q", Title: "Question", Prompt: "Choose."}}}
		delivery := &promptDeliveryFake{binding: PromptBinding{SessionID: "bound-session", ExecutionGeneration: 3}}
		frontier, err := (Service{Delivery: delivery}).Ensure(context.Background(), store, testCityScope(), work.ID, revision, proposal)
		if err != nil {
			t.Fatal(err)
		}
		prompt, err := store.Get(frontier.Prompt.ID)
		if err != nil {
			t.Fatal(err)
		}
		var doc promptRecord
		if err := json.Unmarshal([]byte(prompt.Description), &doc); err != nil {
			t.Fatal(err)
		}
		if doc.PromptBinding == nil {
			t.Fatal("prompt record omitted execution binding")
		}
		before, err := store.List(beads.ListQuery{Type: "gate", AllowScan: true, IncludeClosed: true})
		if err != nil {
			t.Fatal(err)
		}
		calls := delivery.deliveries + delivery.reconciliations
		tampered := &sourceIssueTamperReadStore{
			Store: store, recordID: prompt.ID,
			mutate: func(raw string) (string, error) {
				var stored promptRecord
				if err := json.Unmarshal([]byte(raw), &stored); err != nil {
					return "", err
				}
				stored.PromptBinding.SessionID = "changed-session"
				body, err := json.Marshal(stored)
				return string(body), err
			},
		}
		if _, err := (Service{Delivery: delivery}).Ensure(context.Background(), tampered, testCityScope(), work.ID, revision, proposal); !errors.Is(err, ErrConflict) {
			t.Fatalf("changed persisted binding error = %v, want ErrConflict", err)
		}
		after, err := store.List(beads.ListQuery{Type: "gate", AllowScan: true, IncludeClosed: true})
		if err != nil {
			t.Fatal(err)
		}
		if len(after) != len(before) || delivery.deliveries+delivery.reconciliations != calls || delivery.resolutions != 1 {
			t.Fatalf("changed binding caused writes/effects: before=%d after=%d fake=%+v", len(before), len(after), delivery)
		}
	})
}

func TestUnknownPromptReceiptDoesNotRetryDelivery(t *testing.T) {
	store := beads.NewMemStore()
	store.HonorExplicitIDs = true
	work, err := store.Create(beads.Bead{ID: "wrk-unknown-receipt", Type: "task", Title: "Choose"})
	if err != nil {
		t.Fatal(err)
	}
	revision, _ := WorkRevision(work)
	delivery := &promptDeliveryFake{
		binding:    PromptBinding{SessionID: "receipt-session", ExecutionGeneration: 2},
		deliverErr: fmt.Errorf("connection dropped"),
		reconcile:  PromptResult{Status: "unknown", DefinitivelyAbsent: true},
	}
	service := Service{Delivery: delivery}
	proposal := Proposal{Questions: []Question{{ID: "q", Title: "Question", Prompt: "Choose."}}}
	if _, err := service.Ensure(context.Background(), store, testCityScope(), work.ID, revision, proposal); !errors.Is(err, ErrPromptDeliveryUnavailable) {
		t.Fatalf("initial ambiguous delivery error = %v", err)
	}
	if _, err := service.Ensure(context.Background(), store, testCityScope(), work.ID, revision, proposal); !errors.Is(err, ErrPromptDeliveryUnavailable) {
		t.Fatalf("unknown receipt error = %v, want ErrPromptDeliveryUnavailable", err)
	}
	if delivery.deliveries != 1 || delivery.reconciliations != 1 {
		t.Fatalf("unknown receipt retried external delivery: sends=%d reconciliations=%d", delivery.deliveries, delivery.reconciliations)
	}
}

type verifierFunc func(context.Context, AnswerChallenge, AnswerSubmission) (VerifiedAnswer, error)

func (f verifierFunc) VerifyDecisionAnswer(ctx context.Context, challenge AnswerChallenge, submission AnswerSubmission) (VerifiedAnswer, error) {
	return f(ctx, challenge, submission)
}

type promptDeliveryFake struct {
	deliveries        int
	reconciliations   int
	resolutions       int
	deliverErr        error
	deliverResult     PromptResult
	reconcile         PromptResult
	binding           PromptBinding
	deliveredRequest  PromptRequest
	deliveredBinding  PromptBinding
	reconciledRequest PromptRequest
	reconciledBinding PromptBinding
	beforeDelivery    func(PromptRequest, PromptBinding) error
	resolvePrompt     func(context.Context, PromptRequest) (PromptBinding, error)
}

func (f *promptDeliveryFake) ResolveDecisionPrompt(ctx context.Context, request PromptRequest) (PromptBinding, error) {
	f.resolutions++
	if f.resolvePrompt != nil {
		return f.resolvePrompt(ctx, request)
	}
	binding := f.binding
	if binding.RequestID == "" {
		binding.RequestID = request.ID
	}
	return binding, nil
}

func (f *promptDeliveryFake) DeliverDecisionPrompt(_ context.Context, request PromptRequest, binding PromptBinding) (PromptResult, error) {
	f.deliveries++
	f.deliveredRequest = clonePromptRequest(request)
	f.deliveredBinding = binding
	if f.beforeDelivery != nil {
		if err := f.beforeDelivery(request, binding); err != nil {
			return PromptResult{}, err
		}
	}
	if f.deliverErr != nil {
		err := f.deliverErr
		f.deliverErr = nil
		return PromptResult{}, err
	}
	if f.deliverResult.Status == "" {
		return PromptResult{Status: "accepted"}, nil
	}
	return f.deliverResult, nil
}

func (f *promptDeliveryFake) ReconcileDecisionPrompt(_ context.Context, request PromptRequest, binding PromptBinding) (PromptResult, error) {
	f.reconciliations++
	f.reconciledRequest = clonePromptRequest(request)
	f.reconciledBinding = binding
	if f.beforeDelivery != nil {
		if err := f.beforeDelivery(request, binding); err != nil {
			return PromptResult{}, err
		}
	}
	return f.reconcile, nil
}

func promptRequestsEqual(left, right PromptRequest) bool {
	leftJSON, _ := json.Marshal(left)
	rightJSON, _ := json.Marshal(right)
	return slices.Equal(leftJSON, rightJSON)
}

func questionByID(t *testing.T, frontier Frontier, id string) QuestionView {
	t.Helper()
	for _, question := range frontier.Questions {
		if question.ID == id {
			return question
		}
	}
	t.Fatalf("question %q is absent from %+v", id, frontier.Questions)
	return QuestionView{}
}

func containsLabel(labels []string, want string) bool {
	for _, label := range labels {
		if label == want {
			return true
		}
	}
	return false
}

func containsBead(beads []beads.Bead, id string) bool {
	for _, bead := range beads {
		if bead.ID == id {
			return true
		}
	}
	return false
}
