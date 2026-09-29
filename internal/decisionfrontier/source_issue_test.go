package decisionfrontier

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

func TestNormalizeProposalAcceptsCanonicalSourceIssueReferences(t *testing.T) {
	for _, ref := range []SourceIssueRef{
		{
			TrackerKind: "github", Repository: "github.com/rhar1511/gascity", IssueID: "48",
			CanonicalURL: "https://github.com/rhar1511/gascity/issues/48",
		},
		{
			TrackerKind: "forgejo", Repository: "git.example.org/team/project", IssueID: "7",
			CanonicalURL: "https://git.example.org/team/project/issues/7",
		},
	} {
		proposal := sourceIssueProposal(ref)
		if _, _, err := normalizeProposal(proposal); err != nil {
			t.Errorf("normalize canonical %s source issue: %v", ref.TrackerKind, err)
		}
	}

	if _, _, err := normalizeProposal(Proposal{Questions: []Question{{ID: "q", Title: "Question", Prompt: "Choose."}}}); err != nil {
		t.Fatalf("normalize proposal without optional source issue: %v", err)
	}
}

func TestNormalizeProposalRejectsInvalidSourceIssueReferences(t *testing.T) {
	valid := SourceIssueRef{
		TrackerKind: "github", Repository: "github.com/rhar1511/gascity", IssueID: "48",
		CanonicalURL: "https://github.com/rhar1511/gascity/issues/48",
	}
	otherURL := valid
	otherURL.CanonicalURL = "https://github.com/rhar1511/gascity/issues/49"
	otherRepository := valid
	otherRepository.Repository = "github.com/rhar1511/other"
	whitespace := valid
	whitespace.IssueID = " 48"
	upperTracker := valid
	upperTracker.TrackerKind = "GitHub"
	leadingZero := valid
	leadingZero.IssueID = "048"
	trailingSlash := valid
	trailingSlash.CanonicalURL += "/"
	query := valid
	query.CanonicalURL += "?source=planning"
	fragment := valid
	fragment.CanonicalURL += "#issue"
	insecure := valid
	insecure.CanonicalURL = "http://github.com/rhar1511/gascity/issues/48"
	unknownTracker := valid
	unknownTracker.TrackerKind = "gitlab"
	partial := valid
	partial.CanonicalURL = ""

	cases := []struct {
		name string
		ref  *SourceIssueRef
	}{
		{name: "unknown tracker", ref: &unknownTracker},
		{name: "partial reference", ref: &partial},
		{name: "repository does not match URL", ref: &otherRepository},
		{name: "issue ID does not match URL", ref: &otherURL},
		{name: "untrimmed identifier", ref: &whitespace},
		{name: "noncanonical tracker kind", ref: &upperTracker},
		{name: "leading zero issue ID", ref: &leadingZero},
		{name: "trailing URL slash", ref: &trailingSlash},
		{name: "URL query", ref: &query},
		{name: "URL fragment", ref: &fragment},
		{name: "non-HTTPS URL", ref: &insecure},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := normalizeProposal(sourceIssueProposal(*tc.ref)); !errors.Is(err, ErrInvalid) {
				t.Fatalf("normalize invalid source issue = %v, want ErrInvalid", err)
			}
		})
	}

	for _, key := range []string{"source_issue", "Source-Issue", "source.issue", "issue", "issue_url", "source issue reference"} {
		t.Run("reserved source link key "+key, func(t *testing.T) {
			proposal := sourceIssueProposal(valid)
			proposal.SourceLinks = map[string]string{key: valid.CanonicalURL}
			if _, _, err := normalizeProposal(proposal); !errors.Is(err, ErrInvalid) {
				t.Fatalf("normalize proposal with issue alias %q = %v, want ErrInvalid", key, err)
			}
		})
	}
}

func TestSourceIssueChangesProposalDigestAndExistingEnsureConflicts(t *testing.T) {
	firstRef := SourceIssueRef{
		TrackerKind: "github", Repository: "github.com/rhar1511/gascity", IssueID: "48",
		CanonicalURL: "https://github.com/rhar1511/gascity/issues/48",
	}
	secondRef := SourceIssueRef{
		TrackerKind: "github", Repository: "github.com/rhar1511/gascity", IssueID: "49",
		CanonicalURL: "https://github.com/rhar1511/gascity/issues/49",
	}
	firstQuestions, firstDigest, err := normalizeProposal(sourceIssueProposal(firstRef))
	if err != nil {
		t.Fatal(err)
	}
	secondQuestions, secondDigest, err := normalizeProposal(sourceIssueProposal(secondRef))
	if err != nil {
		t.Fatal(err)
	}
	if firstDigest == secondDigest || len(firstQuestions) != len(secondQuestions) {
		t.Fatalf("source issue change did not change proposal digest: %s / %s", firstDigest, secondDigest)
	}

	store := beads.NewMemStore()
	store.HonorExplicitIDs = true
	work, err := store.Create(beads.Bead{ID: "wrk-source-issue-digest", Type: "task", Title: "Choose"})
	if err != nil {
		t.Fatal(err)
	}
	revision, err := WorkRevision(work)
	if err != nil {
		t.Fatal(err)
	}
	service := Service{}
	first, err := service.Ensure(context.Background(), store, testCityScope(), work.ID, revision, sourceIssueProposal(firstRef))
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := service.Ensure(context.Background(), store, testCityScope(), work.ID, revision, sourceIssueProposal(firstRef))
	if err != nil || first.MapID != replayed.MapID || replayed.SourceIssue == nil || *replayed.SourceIssue != firstRef {
		t.Fatalf("exact source issue replay = %+v, %v; first=%+v", replayed, err, first)
	}
	if _, err := service.Ensure(context.Background(), store, testCityScope(), work.ID, revision, sourceIssueProposal(secondRef)); !errors.Is(err, ErrConflict) {
		t.Fatalf("Ensure with changed source issue = %v, want ErrConflict", err)
	}
}

func TestSourceIssuePersistsThroughReadReplayDeliveryAndRestart(t *testing.T) {
	ref := SourceIssueRef{
		TrackerKind: "forgejo", Repository: "git.example.org/team/project", IssueID: "7",
		CanonicalURL: "https://git.example.org/team/project/issues/7",
	}
	proposal := sourceIssueProposal(ref)
	proposal.SourceLinks = map[string]string{"design": "https://docs.example.org/design"}
	dir := t.TempDir()
	opened, err := beads.OpenSQLiteStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	store := opened.(*beads.SQLiteStore)
	closed := false
	defer func() {
		if !closed {
			_ = store.CloseStore()
		}
	}()
	work, err := store.Create(beads.Bead{Type: "task", Title: "Choose from a source issue"})
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
	delivery := &sourceIssueDeliveryFake{}
	service := Service{Delivery: delivery}
	first, err := service.Ensure(context.Background(), store, testCityScope(), work.ID, revision, proposal)
	if err != nil {
		t.Fatal(err)
	}
	assertSourceIssue(t, first.SourceIssue, ref)
	assertSourceIssue(t, first.Questions[0].SourceIssue, ref)
	if len(delivery.requests) != 1 {
		t.Fatalf("prompt delivery requests = %d, want 1", len(delivery.requests))
	}
	assertSourceIssue(t, delivery.requests[0].SourceIssue, ref)

	var mapDoc mapRecord
	mapBead, err := store.Get(first.MapID)
	if err != nil {
		t.Fatalf("read map source issue: %v", err)
	}
	if err := json.Unmarshal([]byte(mapBead.Description), &mapDoc); err != nil {
		t.Fatalf("decode map source issue: %v", err)
	}
	assertSourceIssue(t, mapDoc.SourceIssue, ref)
	var promptDoc promptRecord
	promptBead, err := store.Get(first.Prompt.ID)
	if err != nil {
		t.Fatalf("read prompt source issue: %v", err)
	}
	if err := json.Unmarshal([]byte(promptBead.Description), &promptDoc); err != nil {
		t.Fatalf("decode prompt source issue: %v", err)
	}
	assertSourceIssue(t, promptDoc.SourceIssue, ref)
	var ticketDoc ticketRecord
	ticketBead, err := store.Get(first.Questions[0].TicketID)
	if err != nil {
		t.Fatalf("read ticket source issue: %v", err)
	}
	if err := json.Unmarshal([]byte(ticketBead.Description), &ticketDoc); err != nil {
		t.Fatalf("decode ticket source issue: %v", err)
	}
	assertSourceIssue(t, ticketDoc.SourceIssue, ref)

	read, err := service.Read(context.Background(), store, testCityScope(), work.ID, revision)
	if err != nil {
		t.Fatal(err)
	}
	assertSourceIssue(t, read.SourceIssue, ref)
	replayed, err := service.Ensure(context.Background(), store, testCityScope(), work.ID, revision, proposal)
	if err != nil || replayed.MapID != first.MapID {
		t.Fatalf("Ensure replay after Read = %+v, %v", replayed, err)
	}
	assertSourceIssue(t, replayed.SourceIssue, ref)
	if err := store.CloseStore(); err != nil {
		t.Fatal(err)
	}
	closed = true

	reopened, err := beads.OpenSQLiteStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	reopenedStore := reopened.(*beads.SQLiteStore)
	t.Cleanup(func() { _ = reopenedStore.CloseStore() })
	afterRestart, err := service.Read(context.Background(), reopenedStore, testCityScope(), work.ID, revision)
	if err != nil {
		t.Fatal(err)
	}
	assertSourceIssue(t, afterRestart.SourceIssue, ref)
	assertSourceIssue(t, afterRestart.Questions[0].SourceIssue, ref)
	replayAfterRestart, err := service.Ensure(context.Background(), reopenedStore, testCityScope(), work.ID, revision, proposal)
	if err != nil || replayAfterRestart.MapID != first.MapID {
		t.Fatalf("Ensure replay after restart = %+v, %v", replayAfterRestart, err)
	}
	assertSourceIssue(t, replayAfterRestart.SourceIssue, ref)
}

func TestReadRejectsSourceIssueAndSourceLinkRecordTampering(t *testing.T) {
	ref := SourceIssueRef{
		TrackerKind: "github", Repository: "github.com/rhar1511/gascity", IssueID: "48",
		CanonicalURL: "https://github.com/rhar1511/gascity/issues/48",
	}
	other := SourceIssueRef{
		TrackerKind: "github", Repository: "github.com/rhar1511/gascity", IssueID: "49",
		CanonicalURL: "https://github.com/rhar1511/gascity/issues/49",
	}
	for _, tc := range []struct {
		name   string
		target string
		mutate func(string) (string, error)
	}{
		{
			name:   "map typed source issue",
			target: "map",
			mutate: func(body string) (string, error) {
				var doc mapRecord
				if err := json.Unmarshal([]byte(body), &doc); err != nil {
					return "", err
				}
				doc.SourceIssue = &other
				encoded, err := json.Marshal(doc)
				return string(encoded), err
			},
		},
		{
			name:   "ticket typed source issue",
			target: "ticket",
			mutate: func(body string) (string, error) {
				var doc ticketRecord
				if err := json.Unmarshal([]byte(body), &doc); err != nil {
					return "", err
				}
				doc.SourceIssue = &other
				encoded, err := json.Marshal(doc)
				return string(encoded), err
			},
		},
		{
			name:   "prompt typed source issue",
			target: "prompt",
			mutate: func(body string) (string, error) {
				var doc promptRecord
				if err := json.Unmarshal([]byte(body), &doc); err != nil {
					return "", err
				}
				doc.SourceIssue = &other
				encoded, err := json.Marshal(doc)
				return string(encoded), err
			},
		},
		{
			name:   "map generic source link",
			target: "map",
			mutate: func(body string) (string, error) {
				var doc mapRecord
				if err := json.Unmarshal([]byte(body), &doc); err != nil {
					return "", err
				}
				doc.SourceLinks["design"] = "https://docs.example.org/changed"
				encoded, err := json.Marshal(doc)
				return string(encoded), err
			},
		},
		{
			name:   "prompt generic source link",
			target: "prompt",
			mutate: func(body string) (string, error) {
				var doc promptRecord
				if err := json.Unmarshal([]byte(body), &doc); err != nil {
					return "", err
				}
				doc.SourceLinks["design"] = "https://docs.example.org/changed"
				encoded, err := json.Marshal(doc)
				return string(encoded), err
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := beads.NewMemStore()
			store.HonorExplicitIDs = true
			work, err := store.Create(beads.Bead{ID: "wrk-source-issue-tamper", Type: "task", Title: "Choose"})
			if err != nil {
				t.Fatal(err)
			}
			revision, err := WorkRevision(work)
			if err != nil {
				t.Fatal(err)
			}
			proposal := sourceIssueProposal(ref)
			proposal.SourceLinks = map[string]string{"design": "https://docs.example.org/design"}
			frontier, err := (Service{}).Ensure(context.Background(), store, testCityScope(), work.ID, revision, proposal)
			if err != nil {
				t.Fatal(err)
			}
			id := frontier.MapID
			switch tc.target {
			case "ticket":
				id = frontier.Questions[0].TicketID
			case "prompt":
				id = frontier.Prompt.ID
			}
			tampered := &sourceIssueTamperReadStore{Store: store, recordID: id, mutate: tc.mutate}
			if _, err := (Service{}).Read(context.Background(), tampered, testCityScope(), work.ID, revision); !errors.Is(err, ErrConflict) {
				t.Fatalf("Read after tampering = %v, want ErrConflict", err)
			}
		})
	}
}

type sourceIssueDeliveryFake struct {
	requests []PromptRequest
}

type sourceIssueTamperReadStore struct {
	beads.Store
	recordID string
	mutate   func(string) (string, error)
}

func (s *sourceIssueTamperReadStore) Get(id string) (beads.Bead, error) {
	bead, err := s.Store.Get(id)
	if err != nil || id != s.recordID {
		return bead, err
	}
	bead.Description, err = s.mutate(bead.Description)
	return bead, err
}

func (s *sourceIssueTamperReadStore) DecisionFrontierSourceReaderHandle() (beads.DecisionFrontierSourceReader, bool) {
	return beads.DecisionFrontierSourceReaderFor(s.Store)
}

func (s *sourceIssueTamperReadStore) StableCreateIDResolveTarget() beads.Store { return s.Store }

func (s *sourceIssueTamperReadStore) ConditionalWriterHandle() (beads.ConditionalWriter, bool) {
	return beads.ConditionalWriterFor(s.Store)
}

func (s *sourceIssueTamperReadStore) RevisionTransitionWriterHandle() (beads.RevisionTransitionWriter, bool) {
	return beads.RevisionTransitionWriterFor(s.Store)
}

func (s *sourceIssueTamperReadStore) DecisionFrontierRecordWriterHandle() (beads.DecisionFrontierRecordWriter, bool) {
	return beads.DecisionFrontierRecordWriterFor(s.Store)
}

func (s *sourceIssueTamperReadStore) RevisionTransitionReceiptReaderHandle() (beads.RevisionTransitionReceiptReader, bool) {
	return beads.RevisionTransitionReceiptReaderFor(s.Store)
}

func (f *sourceIssueDeliveryFake) ResolveDecisionPrompt(_ context.Context, request PromptRequest) (PromptBinding, error) {
	return PromptBinding{SessionID: "source-issue-session", ExecutionGeneration: 1, RequestID: request.ID}, nil
}

func (f *sourceIssueDeliveryFake) DeliverDecisionPrompt(_ context.Context, request PromptRequest, _ PromptBinding) (PromptResult, error) {
	request.SourceIssue = cloneSourceIssueRef(request.SourceIssue)
	request.SourceLinks = cloneStringMap(request.SourceLinks)
	f.requests = append(f.requests, request)
	return PromptResult{Status: "accepted"}, nil
}

func (f *sourceIssueDeliveryFake) ReconcileDecisionPrompt(context.Context, PromptRequest, PromptBinding) (PromptResult, error) {
	return PromptResult{DefinitivelyAbsent: true}, nil
}

func sourceIssueProposal(ref SourceIssueRef) Proposal {
	return Proposal{
		Questions:   []Question{{ID: "q", Title: "Question", Prompt: "Choose."}},
		SourceIssue: &ref,
	}
}

func assertSourceIssue(t *testing.T, got *SourceIssueRef, want SourceIssueRef) {
	t.Helper()
	if got == nil || *got != want {
		t.Fatalf("source issue = %+v, want %+v", got, want)
	}
}
