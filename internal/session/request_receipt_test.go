package session

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

func requestReceiptFixture(t *testing.T) (*beads.MemStore, *Store, time.Time) {
	t.Helper()
	backing := &beads.MemStore{IDPrefix: "gc", HonorExplicitIDs: true}
	_, err := backing.Create(beads.Bead{
		ID: "gc-session", Type: "session", Labels: []string{LabelSession},
		Metadata: map[string]string{"generation": "2", "instance_token": "execution-token", "state": "active"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return backing, NewStore(beads.SessionStore{Store: backing}), time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
}

func TestSessionRequestStagesAndReplay(t *testing.T) {
	backing, store, now := requestReceiptFixture(t)
	accepted, err := store.AcceptRequest("gc-session", "request-1", 2, "report progress", now)
	if err != nil {
		t.Fatal(err)
	}
	if !accepted.NewlyAccepted || accepted.RequestID != "request-1" || accepted.SessionID != "gc-session" || accepted.Generation != 2 || accepted.Delivery != RequestDeliveryPending || accepted.AcknowledgedAt != nil || accepted.Effect != "unverified" {
		t.Fatalf("acceptance invented later evidence: %+v", accepted)
	}
	if _, err := store.RecordRequestDelivery("gc-session", "request-1", 2, RequestDeliveryAccepted, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	read, err := NewStore(beads.SessionStore{Store: backing}).GetRequest("gc-session", "request-1")
	if err != nil || read.AcknowledgedAt != nil || read.Effect != "unverified" {
		t.Fatalf("provider acceptance became acknowledgement: %+v, %v", read, err)
	}
	ack, err := store.AcknowledgeRequest("gc-session", "request-1", 2, "execution-token", now.Add(2*time.Second))
	if err != nil || ack.AcknowledgedAt == nil || ack.Effect != "unverified" {
		t.Fatalf("acknowledge: %+v, %v", ack, err)
	}
	before, _ := backing.Get("gc-session")
	replay, err := store.AcknowledgeRequest("gc-session", "request-1", 2, "execution-token", now.Add(3*time.Second))
	after, _ := backing.Get("gc-session")
	if err != nil || replay.AcknowledgedAt == nil || !replay.AcknowledgedAt.Equal(*ack.AcknowledgedAt) || before.Revision != after.Revision {
		t.Fatalf("acknowledgement replay mutated evidence: %+v, %v", replay, err)
	}
	if _, err := store.AcceptRequest("gc-session", "request-1", 2, "different request", now); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("request-ID reuse accepted: %v", err)
	}
}

func TestSessionRequestConcurrentAcceptanceHasOneDeliveryOwner(t *testing.T) {
	_, store, now := requestReceiptFixture(t)
	var wg sync.WaitGroup
	results := make(chan RequestAcceptance, 2)
	errorsFound := make(chan error, 2)
	for range 2 {
		wg.Go(func() {
			receipt, err := store.AcceptRequest("gc-session", "request-1", 2, "report progress", now)
			results <- receipt
			errorsFound <- err
		})
	}
	wg.Wait()
	close(results)
	close(errorsFound)
	owners := 0
	for result := range results {
		if result.NewlyAccepted {
			owners++
		}
	}
	for err := range errorsFound {
		if err != nil {
			t.Fatal(err)
		}
	}
	if owners != 1 {
		t.Fatalf("delivery owners = %d, want 1", owners)
	}
}

func TestSessionRequestWithoutConditionalWritesRefusesAcceptance(t *testing.T) {
	backing, _, now := requestReceiptFixture(t)
	before, err := backing.Get("gc-session")
	if err != nil {
		t.Fatal(err)
	}
	store := NewStore(beads.SessionStore{Store: struct{ beads.Store }{backing}})
	_, err = store.AcceptRequest("gc-session", "request-1", 2, "report progress", now)
	if !errors.Is(err, beads.ErrConditionalWriteUnsupported) {
		t.Fatalf("acceptance without a fence = %v", err)
	}
	after, err := backing.Get("gc-session")
	if err != nil || before.Revision != after.Revision {
		t.Fatalf("unsupported write mutated state: %v", err)
	}
}

type receiptGenerationRace struct {
	*beads.MemStore
	change func()
}

func (s *receiptGenerationRace) UpdateIfMatch(id string, revision int64, opts beads.UpdateOpts) error {
	if s.change != nil {
		change := s.change
		s.change = nil
		change()
	}
	return s.MemStore.UpdateIfMatch(id, revision, opts)
}

func TestSessionRequestGenerationChangeBetweenReadAndWriteRejectsAck(t *testing.T) {
	backing, store, now := requestReceiptFixture(t)
	if _, err := store.AcceptRequest("gc-session", "request-1", 2, "report progress", now); err != nil {
		t.Fatal(err)
	}
	race := &receiptGenerationRace{MemStore: backing, change: func() {
		if err := backing.SetMetadataBatch("gc-session", map[string]string{"generation": "3", "instance_token": "replacement"}); err != nil {
			t.Fatal(err)
		}
	}}
	_, err := NewStore(beads.SessionStore{Store: race}).AcknowledgeRequest("gc-session", "request-1", 2, "execution-token", now.Add(time.Second))
	if !errors.Is(err, ErrRequestAcknowledgementRejected) {
		t.Fatalf("generation race error = %v", err)
	}
	receipt, err := store.GetRequest("gc-session", "request-1")
	if err != nil || receipt.AcknowledgedAt != nil {
		t.Fatalf("stale acknowledgement persisted: %+v, %v", receipt, err)
	}
}

func TestSessionRequestSurvivesSQLiteReopen(t *testing.T) {
	dir := t.TempDir()
	backing, err := beads.OpenSQLiteStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	row, err := backing.Create(beads.Bead{Type: "session", Labels: []string{LabelSession}, Metadata: map[string]string{"generation": "2", "instance_token": "execution-token", "state": "active"}})
	if err != nil {
		t.Fatal(err)
	}
	// The start commit supplies the first usable revision on counter-backed
	// stores; a newly created row may legitimately have revision zero.
	if err := backing.SetMetadata(row.ID, "creation_complete_at", "2026-09-26T11:59:00Z"); err != nil {
		t.Fatal(err)
	}
	store := NewStore(beads.SessionStore{Store: backing})
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	if _, err := store.AcceptRequest(row.ID, "request-1", 2, "report progress", now); err != nil {
		t.Fatal(err)
	}
	if err := backing.(interface{ CloseStore() error }).CloseStore(); err != nil {
		t.Fatal(err)
	}
	reopened, err := beads.OpenSQLiteStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.(interface{ CloseStore() error }).CloseStore() })
	receipt, err := NewStore(beads.SessionStore{Store: reopened}).GetRequest(row.ID, "request-1")
	if err != nil || receipt.Generation != 2 || receipt.MessageDigest != requestDigest("report progress") || receipt.AcknowledgedAt != nil {
		t.Fatalf("reopened receipt = %+v, %v", receipt, err)
	}
}

func TestSessionRequestRejectsUnrelatedOrStaleAcknowledgements(t *testing.T) {
	for _, tc := range []struct {
		name, requestID, token string
		generation             int
		advance                bool
	}{
		{name: "wrong token", requestID: "request-1", generation: 2, token: "other-token"},
		{name: "wrong request", requestID: "request-2", generation: 2, token: "execution-token"},
		{name: "wrong generation", requestID: "request-1", generation: 3, token: "execution-token"},
		{name: "retired execution", requestID: "request-1", generation: 2, token: "execution-token", advance: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backing, store, now := requestReceiptFixture(t)
			if _, err := store.AcceptRequest("gc-session", "request-1", 2, "report progress", now); err != nil {
				t.Fatal(err)
			}
			if tc.advance {
				if err := backing.SetMetadataBatch("gc-session", map[string]string{"generation": "3", "instance_token": "new-token"}); err != nil {
					t.Fatal(err)
				}
			}
			before, _ := backing.Get("gc-session")
			if _, err := store.AcknowledgeRequest("gc-session", tc.requestID, tc.generation, tc.token, now.Add(time.Second)); err == nil {
				t.Fatal("unrelated execution acknowledged request")
			}
			after, _ := backing.Get("gc-session")
			if before.Revision != after.Revision {
				t.Fatal("rejected acknowledgement changed durable state")
			}
		})
	}
}

func TestSessionRequestListKeepsGenerationsSeparateAndRejectsCorruption(t *testing.T) {
	backing, store, now := requestReceiptFixture(t)
	for _, id := range []string{"z", "a"} {
		if _, err := store.AcceptRequest("gc-session", id, 2, id, now); err != nil {
			t.Fatal(err)
		}
	}
	if err := backing.Update("gc-session", beads.UpdateOpts{Metadata: map[string]string{"generation": "3", "instance_token": "next-token"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AcceptRequest("gc-session", "next", 3, "next", now); err != nil {
		t.Fatal(err)
	}
	got, err := store.ListRequests("gc-session", 2)
	if err != nil || len(got) != 2 || got[0].RequestID != "a" || got[1].RequestID != "z" {
		t.Fatalf("historical generation = %+v, %v", got, err)
	}
	if err := backing.Update("gc-session", beads.UpdateOpts{Metadata: map[string]string{requestReceiptPrefix + "corrupt": "{}"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ListRequests("gc-session", 2); err == nil {
		t.Fatal("corrupt evidence presented as complete list")
	}
}

func TestSessionRequestHistoryPreventsSessionDeletion(t *testing.T) {
	backing, store, now := requestReceiptFixture(t)
	if _, err := store.AcceptRequest("gc-session", "retained", 2, "report progress", now); err != nil {
		t.Fatal(err)
	}
	if err := backing.Close("gc-session"); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteClosedSession("gc-session"); !errors.Is(err, ErrRequestEvidenceRetained) {
		t.Fatalf("delete erased request history: %v", err)
	}
	if _, err := store.GetRequest("gc-session", "retained"); err != nil {
		t.Fatalf("receipt lost: %v", err)
	}
}
