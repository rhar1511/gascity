package decisionfrontier

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

type ambiguousTransitionStore struct {
	beads.Store
	writer   beads.RevisionTransitionWriter
	failNext bool
}

func (s *ambiguousTransitionStore) StableCreateIDResolveTarget() beads.Store { return s.Store }

func (s *ambiguousTransitionStore) ConditionalWriterHandle() (beads.ConditionalWriter, bool) {
	return beads.ConditionalWriterFor(s.Store)
}

func (s *ambiguousTransitionStore) RevisionTransitionWriterHandle() (beads.RevisionTransitionWriter, bool) {
	return s, true
}

func (s *ambiguousTransitionStore) DecisionFrontierRecordWriterHandle() (beads.DecisionFrontierRecordWriter, bool) {
	return beads.DecisionFrontierRecordWriterFor(s.Store)
}

func (s *ambiguousTransitionStore) DecisionFrontierSourceReaderHandle() (beads.DecisionFrontierSourceReader, bool) {
	return beads.DecisionFrontierSourceReaderFor(s.Store)
}

func (s *ambiguousTransitionStore) RevisionTransitionReceiptReaderHandle() (beads.RevisionTransitionReceiptReader, bool) {
	return beads.RevisionTransitionReceiptReaderFor(s.Store)
}

func (s *ambiguousTransitionStore) CompareAndSetMetadataKeyWithReceipt(id, key, expected, next string, revision int64, receipt beads.RevisionTransitionReceipt) (beads.Bead, bool, error) {
	bead, won, err := s.writer.CompareAndSetMetadataKeyWithReceipt(id, key, expected, next, revision, receipt)
	if err == nil && won && s.failNext {
		s.failNext = false
		return beads.Bead{}, false, errors.New("response lost after transition committed")
	}
	return bead, won, err
}

type offsetRevisionStore struct {
	beads.Store
	transition beads.RevisionTransitionWriter
}

type failingRecordCreateStore struct {
	beads.Store
	writer               beads.DecisionFrontierRecordWriter
	failID               string
	failDependencyIssue  string
	failDependencyTarget string
	requiredBeforeLink   []string
	failed               bool
	failedDependency     bool
	allRecordsBeforeLink bool
}

func (s *failingRecordCreateStore) StableCreateIDResolveTarget() beads.Store { return s.Store }

func (s *failingRecordCreateStore) ConditionalWriterHandle() (beads.ConditionalWriter, bool) {
	return beads.ConditionalWriterFor(s.Store)
}

func (s *failingRecordCreateStore) RevisionTransitionWriterHandle() (beads.RevisionTransitionWriter, bool) {
	return beads.RevisionTransitionWriterFor(s.Store)
}

func (s *failingRecordCreateStore) DecisionFrontierRecordWriterHandle() (beads.DecisionFrontierRecordWriter, bool) {
	return s, true
}

func (s *failingRecordCreateStore) DecisionFrontierSourceReaderHandle() (beads.DecisionFrontierSourceReader, bool) {
	return beads.DecisionFrontierSourceReaderFor(s.Store)
}

func (s *failingRecordCreateStore) RevisionTransitionReceiptReaderHandle() (beads.RevisionTransitionReceiptReader, bool) {
	return beads.RevisionTransitionReceiptReaderFor(s.Store)
}

func (s *failingRecordCreateStore) CreateDecisionFrontierRecord(b beads.Bead) (beads.Bead, error) {
	if b.ID == s.failID && !s.failed {
		s.failed = true
		return beads.Bead{}, errors.New("injected controller-record create failure")
	}
	return s.writer.CreateDecisionFrontierRecord(b)
}

func (s *failingRecordCreateStore) CompareAndSetDecisionFrontierRecordMetadataKey(id, key, expected, next string) (bool, error) {
	return s.writer.CompareAndSetDecisionFrontierRecordMetadataKey(id, key, expected, next)
}

func (s *failingRecordCreateStore) EnsureDecisionFrontierLink(issueID, dependsOnID, depType string) error {
	allPresent := true
	for _, id := range s.requiredBeforeLink {
		if _, err := s.Get(id); err != nil {
			allPresent = false
			break
		}
	}
	if len(s.requiredBeforeLink) > 0 && allPresent {
		s.allRecordsBeforeLink = true
	}
	if err := s.writer.EnsureDecisionFrontierLink(issueID, dependsOnID, depType); err != nil {
		return err
	}
	if !s.failedDependency && issueID == s.failDependencyIssue && dependsOnID == s.failDependencyTarget {
		s.failedDependency = true
		return errors.New("injected dependency response loss after commit")
	}
	return nil
}

func (s *offsetRevisionStore) StableCreateIDResolveTarget() beads.Store { return s.Store }

func (s *offsetRevisionStore) ConditionalWriterHandle() (beads.ConditionalWriter, bool) {
	return beads.ConditionalWriterFor(s.Store)
}

func (s *offsetRevisionStore) RevisionTransitionWriterHandle() (beads.RevisionTransitionWriter, bool) {
	return s, true
}

func (s *offsetRevisionStore) DecisionFrontierRecordWriterHandle() (beads.DecisionFrontierRecordWriter, bool) {
	return beads.DecisionFrontierRecordWriterFor(s.Store)
}

func (s *offsetRevisionStore) DecisionFrontierSourceReaderHandle() (beads.DecisionFrontierSourceReader, bool) {
	return s, true
}

func (s *offsetRevisionStore) RevisionTransitionReceiptReaderHandle() (beads.RevisionTransitionReceiptReader, bool) {
	return s, true
}

func (s *offsetRevisionStore) DecisionFrontierSourceSnapshot(id string) (beads.Bead, error) {
	reader, ok := beads.DecisionFrontierSourceReaderFor(s.Store)
	if !ok || reader == nil {
		return beads.Bead{}, beads.ErrConditionalWriteUnsupported
	}
	bead, err := reader.DecisionFrontierSourceSnapshot(id)
	if err != nil {
		return beads.Bead{}, err
	}
	return toOffsetToken(bead)
}

func (s *offsetRevisionStore) DecisionFrontierRevisionTransitionReceipt(issueID, receiptID string) (beads.RevisionTransitionReceipt, bool, error) {
	reader, ok := beads.RevisionTransitionReceiptReaderFor(s.Store)
	if !ok || reader == nil {
		return beads.RevisionTransitionReceipt{}, false, beads.ErrConditionalWriteUnsupported
	}
	receipt, found, err := reader.DecisionFrontierRevisionTransitionReceipt(issueID, receiptID)
	if err != nil || !found {
		return receipt, found, err
	}
	receipt.FromRevision, err = toOffsetRevision(receipt.FromRevision)
	if err != nil {
		return beads.RevisionTransitionReceipt{}, false, err
	}
	receipt.ToRevision, err = toOffsetRevision(receipt.ToRevision)
	if err != nil {
		return beads.RevisionTransitionReceipt{}, false, err
	}
	return receipt, true, nil
}

func (s *offsetRevisionStore) Get(id string) (beads.Bead, error) {
	bead, err := s.Store.Get(id)
	if err != nil {
		return beads.Bead{}, err
	}
	offset, err := toOffsetToken(bead)
	return offset, err
}

func (s *offsetRevisionStore) CompareAndSetMetadataKeyWithReceipt(id, key, expected, next string, revision int64, receipt beads.RevisionTransitionReceipt) (beads.Bead, bool, error) {
	rawRevision, err := fromOffsetToken(revision)
	if err != nil {
		return beads.Bead{}, false, err
	}
	receipt.FromRevision, err = fromOffsetToken(receipt.FromRevision)
	if err != nil {
		return beads.Bead{}, false, err
	}
	if receipt.Operation == "reserve" {
		next, err = rewriteHoldRevision(next, fromOffsetTokenString)
	} else {
		expected, err = rewriteHoldRevision(expected, fromOffsetTokenString)
	}
	if err != nil {
		return beads.Bead{}, false, err
	}
	bead, won, err := s.transition.CompareAndSetMetadataKeyWithReceipt(id, key, expected, next, rawRevision, receipt)
	if err != nil || !won {
		return beads.Bead{}, won, err
	}
	offset, err := toOffsetToken(bead)
	return offset, true, err
}

func fromOffsetTokenString(value string) (string, error) {
	token, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return "", err
	}
	raw, err := fromOffsetToken(token)
	if err != nil {
		return "", err
	}
	return strconv.FormatInt(raw, 10), nil
}

func fromOffsetToken(token int64) (int64, error) {
	if token < 22 || (token-5)%17 != 0 {
		return 0, fmt.Errorf("invalid offset revision token %d", token)
	}
	return (token - 5) / 17, nil
}

func toOffsetToken(bead beads.Bead) (beads.Bead, error) {
	if bead.Revision > 0 {
		bead.Revision = bead.Revision*17 + 5
	}
	if marker := bead.Metadata[beadmeta.DecisionFrontierHoldMetadataKey]; marker != "" {
		updated, err := rewriteHoldRevision(marker, func(value string) (string, error) {
			raw, err := strconv.ParseInt(value, 10, 64)
			if err != nil || raw <= 0 {
				return "", fmt.Errorf("invalid raw revision %q", value)
			}
			return strconv.FormatInt(raw*17+5, 10), nil
		})
		if err != nil {
			return beads.Bead{}, err
		}
		bead.Metadata[beadmeta.DecisionFrontierHoldMetadataKey] = updated
	}
	if raw := bead.Metadata[beadmeta.DecisionFrontierRevisionReceiptsMetadataKey]; raw != "" {
		updated, err := rewriteReceiptTokens(raw, func(value int64) (int64, error) {
			return value*17 + 5, nil
		})
		if err != nil {
			return beads.Bead{}, err
		}
		bead.Metadata[beadmeta.DecisionFrontierRevisionReceiptsMetadataKey] = updated
	}
	return bead, nil
}

func toOffsetRevision(revision int64) (int64, error) {
	if revision <= 0 {
		return 0, fmt.Errorf("invalid raw revision token %d", revision)
	}
	return revision*17 + 5, nil
}

func rewriteHoldRevision(raw string, transform func(string) (string, error)) (string, error) {
	var hold sourceHold
	if err := json.Unmarshal([]byte(raw), &hold); err != nil {
		return "", err
	}
	updated, err := transform(hold.WorkRevision)
	if err != nil {
		return "", err
	}
	hold.WorkRevision = updated
	data, err := json.Marshal(hold)
	return string(data), err
}

func rewriteReceiptTokens(raw string, transform func(int64) (int64, error)) (string, error) {
	var receipts []beads.RevisionTransitionReceipt
	if err := json.Unmarshal([]byte(raw), &receipts); err != nil {
		return "", err
	}
	for i := range receipts {
		var err error
		receipts[i].FromRevision, err = transform(receipts[i].FromRevision)
		if err != nil {
			return "", err
		}
		receipts[i].ToRevision, err = transform(receipts[i].ToRevision)
		if err != nil {
			return "", err
		}
	}
	data, err := json.Marshal(receipts)
	return string(data), err
}

func TestServiceUsesReturnedRevisionTokensInsteadOfIncrementing(t *testing.T) {
	base := beads.NewMemStore()
	base.HonorExplicitIDs = true
	work, err := base.Create(beads.Bead{ID: "wrk-opaque-revision", Type: "task", Title: "Opaque revision"})
	if err != nil {
		t.Fatal(err)
	}
	transition, ok := beads.RevisionTransitionWriterFor(base)
	if !ok {
		t.Fatal("MemStore did not expose revision transition capability")
	}
	store := &offsetRevisionStore{Store: base, transition: transition}
	wrappedWork, err := store.Get(work.ID)
	if err != nil {
		t.Fatal(err)
	}
	revision, _ := WorkRevision(wrappedWork)
	frontier, err := (Service{}).Ensure(context.Background(), store, testCityScope(), work.ID, revision,
		Proposal{Questions: []Question{{ID: "q", Title: "Question", Prompt: "Choose."}}})
	if err != nil {
		t.Fatal(err)
	}
	held, err := store.Get(work.ID)
	if err != nil {
		t.Fatal(err)
	}
	reserved := transitionReceiptForTest(t, held, reservationReceiptID(testCityScope(), frontier.MapID))
	if reserved.FromRevision != wrappedWork.Revision || reserved.ToRevision == reserved.FromRevision+1 {
		t.Fatalf("reservation did not use the opaque backend token: start=%d receipt=%+v", wrappedWork.Revision, reserved)
	}
	verifier := verifierFunc(func(_ context.Context, challenge AnswerChallenge, submission AnswerSubmission) (VerifiedAnswer, error) {
		return VerifiedAnswer{
			CityRef: challenge.CityRef, StoreRef: challenge.StoreRef, KeyID: "key", Issuer: "issuer", Subject: "human",
			WorkID: challenge.WorkID, WorkRevision: challenge.WorkRevision, WorkDigest: challenge.WorkDigest,
			MapID: challenge.MapID, TicketID: challenge.TicketID, QuestionID: challenge.QuestionID, QuestionVersion: challenge.QuestionVersion,
			AnswerDigest: AnswerDigest(submission.Resolution, submission.Text), Resolution: submission.Resolution,
		}, nil
	})
	question := frontier.OpenQuestions[0]
	resolved, err := (Service{Verifier: verifier}).Answer(context.Background(), store, testCityScope(), work.ID, AnswerSubmission{
		TicketID: question.TicketID, WorkRevision: revision, QuestionVersion: question.Version,
		Resolution: ResolutionAnswered, Text: "Proceed", Proof: "proof",
	})
	if err != nil {
		t.Fatal(err)
	}
	releasedWork, err := store.Get(work.ID)
	if err != nil {
		t.Fatal(err)
	}
	released := transitionReceiptForTest(t, releasedWork, releaseReceiptID(testCityScope(), frontier.MapID))
	if released.FromRevision != reserved.ToRevision || released.ToRevision == reserved.FromRevision+2 || resolved.State != StateResolved {
		t.Fatalf("release did not follow actual opaque reservation token: reserve=%+v release=%+v frontier=%+v", reserved, released, resolved)
	}
}

func TestWorkRevisionPreservesCanonicalSignedBackendToken(t *testing.T) {
	const token int64 = -868924464739205321
	got, err := WorkRevision(beads.Bead{ID: "wrk-signed-token", Revision: token})
	if err != nil || got != strconv.FormatInt(token, 10) {
		t.Fatalf("WorkRevision signed token = %q, %v; want %d", got, err, token)
	}
	parsed, err := canonicalWorkRevision(got)
	if err != nil || parsed != token {
		t.Fatalf("canonicalWorkRevision(%q) = %d, %v; want %d", got, parsed, err, token)
	}
	for _, invalid := range []string{"0", "-0", "+1", "01", "-01", "9223372036854775808", "-9223372036854775809"} {
		if _, err := canonicalWorkRevision(invalid); !errors.Is(err, ErrInvalid) {
			t.Errorf("canonicalWorkRevision(%q) error = %v, want ErrInvalid", invalid, err)
		}
	}
}

func TestTransitionReceiptMatchesPreservesSignedBackendTokens(t *testing.T) {
	scope := testCityScope()
	const from int64 = -868924464739205321
	const to int64 = -868924464739205278
	receipt := beads.RevisionTransitionReceipt{
		ID:           reservationReceiptID(scope, "map-signed-token"),
		CityRef:      scope.CityRef,
		StoreRef:     scope.StoreRef,
		WorkID:       "wrk-signed-token",
		MapID:        "map-signed-token",
		Operation:    "reserve",
		FromRevision: from,
		ToRevision:   to,
	}
	if !receiptMatches(receipt, scope, receipt.WorkID, receipt.MapID, receipt.Operation, from) {
		t.Fatalf("receipt with distinct signed backend tokens was rejected: %+v", receipt)
	}
	for name, mutate := range map[string]func(*beads.RevisionTransitionReceipt, *int64){
		"zero from token": func(r *beads.RevisionTransitionReceipt, from *int64) {
			r.FromRevision = 0
			*from = 0
		},
		"zero to token": func(r *beads.RevisionTransitionReceipt, _ *int64) {
			r.ToRevision = 0
		},
		"unchanged revision token": func(r *beads.RevisionTransitionReceipt, from *int64) {
			r.ToRevision = r.FromRevision
			*from = r.FromRevision
		},
	} {
		t.Run(name, func(t *testing.T) {
			candidate := receipt
			candidateFrom := from
			mutate(&candidate, &candidateFrom)
			if receiptMatches(candidate, scope, candidate.WorkID, candidate.MapID, candidate.Operation, candidateFrom) {
				t.Fatalf("invalid transition receipt was accepted: %+v", candidate)
			}
		})
	}
}

func TestEnsureRepairsPartialImmutableRecordsAfterStoreReopen(t *testing.T) {
	proposal := Proposal{Questions: []Question{
		{ID: "first", Title: "First", Prompt: "Choose the first option."},
		{ID: "second", Title: "Second", Prompt: "Choose the second option.", DependsOn: []string{"first"}},
	}}
	questions, _, err := normalizeProposal(proposal)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name        string
		failOrdinal int
	}{
		{name: "after map", failOrdinal: 0},
		{name: "after first ticket", failOrdinal: 1},
		{name: "before prompt", failOrdinal: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			opened, err := beads.OpenSQLiteStore(dir)
			if err != nil {
				t.Fatal(err)
			}
			store := opened.(*beads.SQLiteStore)
			work, err := store.Create(beads.Bead{Type: "task", Title: "Recover partial decision map"})
			if err != nil {
				t.Fatal(err)
			}
			if work.Revision == 0 {
				if err := store.Update(work.ID, beads.UpdateOpts{Title: stringPtrForTest("revisioned decision source")}); err != nil {
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
			scope := testCityScope()
			mapID := frontierMapID(scope, work.ID, revision)
			ticketIDs := ticketIDsFor(scope, mapID, questions)
			failIDs := []string{ticketIDs[0], ticketIDs[1], frontierPromptID(scope, mapID)}
			baseWriter, ok := beads.DecisionFrontierRecordWriterFor(store)
			if !ok {
				t.Fatal("SQLite store lacks decision record writer")
			}
			faultStore := &failingRecordCreateStore{Store: store, writer: baseWriter, failID: failIDs[tc.failOrdinal]}
			if _, err := (Service{}).Ensure(context.Background(), faultStore, scope, work.ID, revision, proposal); err == nil {
				t.Fatal("Ensure succeeded despite injected record-create failure")
			}
			if !faultStore.failed {
				t.Fatal("injected record-create failure was not reached")
			}
			if _, err := store.Get(mapID); err != nil {
				t.Fatalf("map record was not durably created before failure: %v", err)
			}
			held, err := store.Get(work.ID)
			if err != nil || held.Metadata[beadmeta.DecisionFrontierHoldMetadataKey] == "" {
				t.Fatalf("source was not held during partial creation: bead=%+v err=%v", held, err)
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
			frontier, err := (Service{}).Ensure(context.Background(), reopenedStore, scope, work.ID, revision, proposal)
			if err != nil {
				t.Fatalf("retry after store reopen did not repair partial records: %v", err)
			}
			if frontier.MapID != mapID || len(frontier.Questions) != len(questions) || len(frontier.OpenQuestions) != 1 || frontier.OpenQuestions[0].ID != "first" {
				t.Fatalf("repaired frontier = %+v", frontier)
			}
			rows, err := reopenedStore.List(beads.ListQuery{Type: "gate", AllowScan: true, IncludeClosed: true})
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != 4 {
				t.Fatalf("repaired record count = %d, want map + two tickets + prompt", len(rows))
			}
		})
	}
}

func TestEnsureRepairsCommittedDependencyAfterStoreReopen(t *testing.T) {
	dir := t.TempDir()
	opened, err := beads.OpenSQLiteStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	store := opened.(*beads.SQLiteStore)
	t.Cleanup(func() { _ = store.CloseStore() })
	work, err := store.Create(beads.Bead{Type: "task", Title: "Recover partial decision dependency"})
	if err != nil {
		t.Fatal(err)
	}
	description := "revisioned source"
	if err := store.Update(work.ID, beads.UpdateOpts{Description: &description}); err != nil {
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
	scope := testCityScope()
	proposal := Proposal{Questions: []Question{{ID: "q", Title: "Question", Prompt: "Choose."}}}
	mapID := frontierMapID(scope, work.ID, revision)
	ticketID := ticketIDsFor(scope, mapID, []Question{{ID: "q"}})[0]
	baseWriter, ok := beads.DecisionFrontierRecordWriterFor(store)
	if !ok {
		t.Fatal("SQLite store lacks decision record writer")
	}
	faultStore := &failingRecordCreateStore{
		Store: store, writer: baseWriter,
		failDependencyIssue: ticketID, failDependencyTarget: mapID,
		requiredBeforeLink: []string{mapID, ticketID, frontierPromptID(scope, mapID)},
	}
	if _, err := (Service{}).Ensure(context.Background(), faultStore, scope, work.ID, revision, proposal); err == nil {
		t.Fatal("Ensure succeeded despite injected dependency response loss")
	}
	if !faultStore.failedDependency {
		t.Fatal("dependency response loss was not injected")
	}
	if !faultStore.allRecordsBeforeLink {
		t.Fatal("the controller attempted a decision-frontier link before creating every immutable record")
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
	frontier, err := (Service{}).Ensure(context.Background(), reopenedStore, scope, work.ID, revision, proposal)
	if err != nil {
		t.Fatalf("retry after dependency commit and store reopen: %v", err)
	}
	if frontier.MapID != mapID || len(frontier.OpenQuestions) != 1 || frontier.OpenQuestions[0].TicketID != ticketID {
		t.Fatalf("replayed frontier changed identity or question: %+v", frontier)
	}
	dependencies, err := reopenedStore.DepList(ticketID, "down")
	if err != nil {
		t.Fatal(err)
	}
	if len(dependencies) != 1 || dependencies[0].DependsOnID != mapID || dependencies[0].Type != "relates-to" {
		t.Fatalf("ticket-to-map dependencies after retry = %+v, want exactly one relates-to edge", dependencies)
	}
}

func stringPtrForTest(value string) *string { return &value }

func TestSQLiteFrontierRecoversCommittedReceiptAfterRestart(t *testing.T) {
	dir := t.TempDir()
	store, err := beads.OpenSQLiteStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	closer := store.(interface{ CloseStore() error })
	defer func() {
		if err := closer.CloseStore(); err != nil {
			t.Errorf("close initial SQLite frontier store: %v", err)
		}
	}()
	work, err := store.Create(beads.Bead{ID: "wrk-sqlite-frontier", Type: "task", Title: "Durable source"})
	if err != nil {
		t.Fatal(err)
	}
	description := "source revision established"
	if err := store.Update(work.ID, beads.UpdateOpts{Description: &description}); err != nil {
		t.Fatal(err)
	}
	work, err = store.Get(work.ID)
	if err != nil {
		t.Fatal(err)
	}
	revision, err := WorkRevision(work)
	if err != nil {
		t.Fatalf("SQLite source revision = %d: %v", work.Revision, err)
	}
	transition, ok := beads.RevisionTransitionWriterFor(store)
	if !ok {
		t.Fatal("SQLiteStore did not expose revision transition capability")
	}
	faultStore := &ambiguousTransitionStore{Store: store, writer: transition, failNext: true}
	proposal := Proposal{Questions: []Question{{ID: "q", Title: "Question", Prompt: "Choose."}}}
	frontier, err := (Service{}).Ensure(context.Background(), faultStore, testCityScope(), work.ID, revision, proposal)
	if err != nil {
		t.Fatalf("Ensure should recover a committed transition with lost response: %v", err)
	}
	if faultStore.failNext {
		t.Fatal("test did not inject the commit-then-error boundary")
	}
	held, err := store.Get(work.ID)
	if err != nil {
		t.Fatal(err)
	}
	reserved := transitionReceiptForTest(t, held, reservationReceiptID(testCityScope(), frontier.MapID))
	if err := closer.CloseStore(); err != nil {
		t.Fatal(err)
	}

	reopened, err := beads.OpenSQLiteStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	reopenedCloser := reopened.(interface{ CloseStore() error })
	defer func() {
		if err := reopenedCloser.CloseStore(); err != nil {
			t.Errorf("close reopened SQLite frontier store: %v", err)
		}
	}()
	replayed, err := (Service{}).Ensure(context.Background(), reopened, testCityScope(), work.ID, revision, proposal)
	if err != nil {
		t.Fatalf("Ensure after store restart: %v", err)
	}
	if replayed.MapID != frontier.MapID || replayed.Questions[0].TicketID != frontier.Questions[0].TicketID {
		t.Fatalf("restart changed frontier identity: first=%+v replay=%+v", frontier, replayed)
	}
	question := replayed.OpenQuestions[0]
	verifier := verifierFunc(func(_ context.Context, challenge AnswerChallenge, submission AnswerSubmission) (VerifiedAnswer, error) {
		return VerifiedAnswer{
			CityRef: challenge.CityRef, StoreRef: challenge.StoreRef, KeyID: "key", Issuer: "issuer", Subject: "human",
			WorkID: challenge.WorkID, WorkRevision: challenge.WorkRevision, WorkDigest: challenge.WorkDigest,
			MapID: challenge.MapID, TicketID: challenge.TicketID, QuestionID: challenge.QuestionID, QuestionVersion: challenge.QuestionVersion,
			AnswerDigest: AnswerDigest(submission.Resolution, submission.Text), Resolution: submission.Resolution,
		}, nil
	})
	if _, err := (Service{Verifier: verifier}).Answer(context.Background(), reopened, testCityScope(), work.ID, AnswerSubmission{
		TicketID: question.TicketID, WorkRevision: revision, QuestionVersion: question.Version,
		Resolution: ResolutionAnswered, Text: "Proceed after restart", Proof: "signed-proof",
	}); err != nil {
		t.Fatalf("Answer after store restart: %v", err)
	}
	if err := reopenedCloser.CloseStore(); err != nil {
		t.Fatal(err)
	}

	finalStore, err := beads.OpenSQLiteStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	finalCloser := finalStore.(interface{ CloseStore() error })
	defer func() {
		if err := finalCloser.CloseStore(); err != nil {
			t.Errorf("close final SQLite frontier store: %v", err)
		}
	}()
	read, err := (Service{}).Read(context.Background(), finalStore, testCityScope(), work.ID, revision)
	if err != nil {
		t.Fatalf("Read after second store restart: %v", err)
	}
	if read.State != StateResolved {
		t.Fatalf("persisted frontier state = %q, want resolved", read.State)
	}
	current, err := finalStore.Get(work.ID)
	if err != nil {
		t.Fatal(err)
	}
	released := transitionReceiptForTest(t, current, releaseReceiptID(testCityScope(), frontier.MapID))
	if released.FromRevision != reserved.ToRevision || current.Revision != released.ToRevision || beads.HasDecisionFrontierHold(current) {
		t.Fatalf("durable release receipt mismatch: current=%+v reserved=%+v released=%+v", current, reserved, released)
	}
}
