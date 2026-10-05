package session

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

type requestKeyValidatingStore struct {
	*beads.MemStore
}

type requestDeleteInterleavingStore struct {
	*beads.MemStore
	beforeDelete func()
}

func (s *requestDeleteInterleavingStore) DeleteIfMatch(id string, expectedRevision int64) error {
	if s.beforeDelete != nil {
		before := s.beforeDelete
		s.beforeDelete = nil
		before()
	}
	return s.MemStore.DeleteIfMatch(id, expectedRevision)
}

func (s *requestKeyValidatingStore) UpdateIfMatch(id string, expectedRevision int64, opts beads.UpdateOpts) error {
	for key := range opts.Metadata {
		if err := beadmeta.ValidateKey(key); err != nil {
			return err
		}
	}
	return s.MemStore.UpdateIfMatch(id, expectedRevision, opts)
}

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

func reserveRequestAttempt(t *testing.T, store *Store, requestID string, now time.Time) {
	t.Helper()
	if _, err := store.mutateRequestReceipt("gc-session", requestID, func(_ beads.Bead, record *storedRequestReceipt) (bool, error) {
		stamp := now.UTC()
		record.DeliveryAttemptedAt = &stamp
		record.Delivery = RequestDeliveryUnknown
		return true, nil
	}); err != nil {
		t.Fatalf("reserve request attempt: %v", err)
	}
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
	if _, err := store.AcknowledgeRequest("gc-session", "request-1", 2, "execution-token", now.Add(time.Second)); !errors.Is(err, ErrRequestAcknowledgementRejected) {
		t.Fatalf("pre-delivery acknowledgement = %v, want rejection", err)
	}
	reserveRequestAttempt(t, store, "request-1", now.Add(time.Second))
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

func TestSessionRequestReplaySurvivesGenerationChange(t *testing.T) {
	backing, store, now := requestReceiptFixture(t)
	accepted, err := store.AcceptRequest("gc-session", "request-replay", 2, "report progress", now)
	if err != nil {
		t.Fatal(err)
	}
	if err := backing.SetMetadataBatch("gc-session", map[string]string{"generation": "3", "instance_token": "replacement-token"}); err != nil {
		t.Fatal(err)
	}

	replayed, err := store.AcceptRequest("gc-session", "request-replay", 2, "report progress", now.Add(time.Minute))
	if err != nil || replayed.NewlyAccepted || replayed.RequestReceipt != accepted.RequestReceipt {
		t.Fatalf("replay after generation change = %+v, %v; want original %+v", replayed, err, accepted.RequestReceipt)
	}
	for name, conflict := range map[string]struct {
		generation int
		message    string
	}{
		"generation": {generation: 3, message: "report progress"},
		"content":    {generation: 2, message: "different request"},
	} {
		if _, err := store.AcceptRequest("gc-session", "request-replay", conflict.generation, conflict.message, now.Add(2*time.Minute)); !errors.Is(err, ErrRequestConflict) {
			t.Fatalf("conflicting %s replay = %v, want request conflict", name, err)
		}
	}
}

func TestSessionRequestReplaySurvivesClosure(t *testing.T) {
	backing, store, now := requestReceiptFixture(t)
	accepted, err := store.AcceptRequest("gc-session", "request-closed-replay", 2, "report progress", now)
	if err != nil {
		t.Fatal(err)
	}
	if err := backing.Close("gc-session"); err != nil {
		t.Fatal(err)
	}

	replayed, err := store.AcceptRequest("gc-session", "request-closed-replay", 2, "report progress", now.Add(time.Minute))
	if err != nil || replayed.NewlyAccepted || replayed.RequestReceipt != accepted.RequestReceipt {
		t.Fatalf("replay after closure = %+v, %v; want original %+v", replayed, err, accepted.RequestReceipt)
	}
	if _, err := store.AcceptRequest("gc-session", "request-closed-replay", 2, "different request", now.Add(2*time.Minute)); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("conflicting replay after closure = %v, want request conflict", err)
	}
	if _, err := store.AcceptRequest("gc-session", "new-after-close", 2, "report progress", now.Add(2*time.Minute)); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("new acceptance after closure = %v, want request conflict", err)
	}
}

func TestSessionRequestReceiptNeverPersistsExecutionCredential(t *testing.T) {
	backing, store, now := requestReceiptFixture(t)
	if _, err := store.AcceptRequest("gc-session", "request-secret", 2, "report progress", now); err != nil {
		t.Fatal(err)
	}
	b, err := backing.Get("gc-session")
	if err != nil {
		t.Fatal(err)
	}
	key, err := requestReceiptMetadataKey("request-secret")
	if err != nil {
		t.Fatal(err)
	}
	raw := b.Metadata[key]
	if strings.Contains(raw, "execution-token") {
		t.Fatalf("receipt metadata leaked raw execution credential: %s", raw)
	}
	if !strings.Contains(raw, requestDigest("execution-token")) {
		t.Fatalf("receipt metadata lacks execution credential digest: %s", raw)
	}
	receipt, err := store.GetRequest("gc-session", "request-secret")
	if err != nil {
		t.Fatal(err)
	}
	public, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(public), "execution-token") || strings.Contains(string(public), "execution_token") {
		t.Fatalf("public receipt exposed execution credential material: %s", public)
	}
}

func TestSessionRequestAcceptanceEventRedactsCredentialWithoutMutatingRuntimeStorage(t *testing.T) {
	backing := &beads.MemStore{IDPrefix: "gc", HonorExplicitIDs: true}
	_, err := backing.Create(beads.Bead{
		ID: "gc-session", Type: BeadType, Labels: []string{LabelSession},
		Metadata: map[string]string{"generation": "2", "instance_token": "runtime-secret", "state": "active"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var eventPayload json.RawMessage
	cache := beads.NewCachingStoreForTest(backing, func(eventType, _ string, payload json.RawMessage) {
		if eventType == "bead.updated" {
			eventPayload = append(eventPayload[:0], payload...)
		}
	})
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatal(err)
	}
	store := NewStore(beads.SessionStore{Store: cache})
	if _, err := store.AcceptRequest("gc-session", "request-event", 2, "report progress", time.Now()); err != nil {
		t.Fatal(err)
	}
	if len(eventPayload) == 0 || !strings.Contains(string(eventPayload), beadmeta.SessionRequestReceiptPrefix) {
		t.Fatalf("acceptance event lacks receipt snapshot: %s", eventPayload)
	}
	if strings.Contains(string(eventPayload), "runtime-secret") || strings.Contains(string(eventPayload), `"instance_token"`) {
		t.Fatalf("acceptance event leaked execution credential: %s", eventPayload)
	}
	stored, err := backing.Get("gc-session")
	if err != nil || stored.Metadata[beadmeta.SessionInstanceTokenMetadataKey] != "runtime-secret" {
		t.Fatalf("runtime storage lost execution credential: %+v, %v", stored, err)
	}
}

func TestGuardGenericMutationRetainsSessionLabelWithRequestEvidence(t *testing.T) {
	b := beads.Bead{
		ID: "gc-session", Labels: []string{LabelSession, "worker"},
		Metadata: map[string]string{beadmeta.SessionRequestReceiptPrefix + "history": `{}`},
	}
	if err := GuardGenericMutation(b, beads.UpdateOpts{RemoveLabels: []string{LabelSession}}); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("remove session label = %v, want request conflict", err)
	}
	if err := GuardGenericMutation(b, beads.UpdateOpts{Labels: []string{LabelSession}, RemoveLabels: []string{LabelSession}}); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("add then remove session label = %v, want request conflict", err)
	}
	if err := GuardGenericMutation(b, beads.UpdateOpts{RemoveLabels: []string{"worker"}}); err != nil {
		t.Fatalf("remove unrelated label = %v", err)
	}
	damaged := b
	damaged.Labels = []string{"worker"}
	if err := GuardGenericMutation(damaged, beads.UpdateOpts{Labels: []string{"other"}}); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("label mutation that leaves historical row unidentified = %v, want conflict", err)
	}
	if err := GuardGenericMutation(damaged, beads.UpdateOpts{Labels: []string{LabelSession}}); err != nil {
		t.Fatalf("session identity repair = %v", err)
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
	reserveRequestAttempt(t, store, "request-1", now)
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
			reserveRequestAttempt(t, store, "request-1", now)
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
	corruptKey, err := requestReceiptMetadataKey("corrupt")
	if err != nil {
		t.Fatal(err)
	}
	if err := backing.Update("gc-session", beads.UpdateOpts{Metadata: map[string]string{corruptKey: "{}"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ListRequests("gc-session", 2); err == nil {
		t.Fatal("corrupt evidence presented as complete list")
	}
}

func TestSessionRequestMetadataKeysEncodePublicIDs(t *testing.T) {
	backing := &beads.MemStore{IDPrefix: "gc", HonorExplicitIDs: true}
	validating := &requestKeyValidatingStore{MemStore: backing}
	if _, err := validating.Create(beads.Bead{
		ID: "gc-session", Type: "session", Labels: []string{LabelSession},
		Metadata: map[string]string{"generation": "2", "instance_token": "execution-token", "state": "active"},
	}); err != nil {
		t.Fatal(err)
	}
	store := NewStore(beads.SessionStore{Store: validating})
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	ids := []string{"request-with-dash:scope", "request:with-dash-scope"}
	keys := make(map[string]bool)
	for _, id := range ids {
		accepted, err := store.AcceptRequest("gc-session", id, 2, "report "+id, now)
		if err != nil || accepted.RequestID != id {
			t.Fatalf("accept %q = %+v, %v", id, accepted, err)
		}
		key, err := requestReceiptMetadataKey(id)
		if err != nil {
			t.Fatal(err)
		}
		if err := beadmeta.ValidateKey(key); err != nil {
			t.Fatalf("encoded key %q rejected by production key-shape mirror: %v", key, err)
		}
		if keys[key] {
			t.Fatalf("request IDs collided at %q", key)
		}
		keys[key] = true
		replayed, err := store.AcceptRequest("gc-session", id, 2, "report "+id, now.Add(time.Minute))
		if err != nil || replayed.NewlyAccepted || replayed.AcceptedAt != accepted.AcceptedAt {
			t.Fatalf("replay %q = %+v, %v", id, replayed, err)
		}
		reserveRequestAttempt(t, store, id, now.Add(time.Second))
		if _, err := store.RecordRequestDelivery("gc-session", id, 2, RequestDeliveryAccepted, now.Add(time.Second)); err != nil {
			t.Fatal(err)
		}
		if _, err := store.AcknowledgeRequest("gc-session", id, 2, "execution-token", now.Add(2*time.Second)); err != nil {
			t.Fatal(err)
		}
		got, err := store.GetRequest("gc-session", id)
		if err != nil || got.RequestID != id || got.AcknowledgedAt == nil {
			t.Fatalf("lookup %q = %+v, %v", id, got, err)
		}
	}
	listed, err := store.ListRequests("gc-session", 2)
	if err != nil || len(listed) != len(ids) {
		t.Fatalf("list = %+v, %v", listed, err)
	}
	row, err := backing.Get("gc-session")
	if err != nil {
		t.Fatal(err)
	}
	for key := range row.Metadata {
		if strings.HasPrefix(key, requestReceiptPrefix) && !keys[key] {
			t.Fatalf("unexpected receipt key %q", key)
		}
	}
	if err := backing.Close("gc-session"); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteClosedSession("gc-session"); !errors.Is(err, ErrRequestEvidenceRetained) {
		t.Fatalf("encoded receipts did not guard deletion: %v", err)
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

func TestDeleteClosedSessionRejectsReopenAndAcceptanceInterleaving(t *testing.T) {
	backing, _, now := requestReceiptFixture(t)
	if err := backing.Close("gc-session"); err != nil {
		t.Fatal(err)
	}
	interleaved := &requestDeleteInterleavingStore{MemStore: backing}
	store := NewStore(beads.SessionStore{Store: interleaved})
	var reopenErr, acceptErr error
	var accepted RequestAcceptance
	interleaved.beforeDelete = func() {
		reopenErr = backing.Reopen("gc-session")
		accepted, acceptErr = store.AcceptRequest("gc-session", "won-delete-race", 2, "report progress", now)
	}

	err := store.DeleteClosedSession("gc-session")
	var stale *beads.PreconditionFailedError
	if !errors.As(err, &stale) {
		t.Fatalf("DeleteClosedSession error = %v, want revision precondition failure", err)
	}
	if reopenErr != nil {
		t.Fatalf("concurrent reopen: %v", reopenErr)
	}
	if acceptErr != nil || !accepted.NewlyAccepted {
		t.Fatalf("concurrent acceptance = %+v, %v", accepted, acceptErr)
	}
	row, err := backing.Get("gc-session")
	if err != nil {
		t.Fatalf("concurrently changed session was deleted: %v", err)
	}
	if row.Status == "closed" {
		t.Fatalf("concurrent reopen was lost: %+v", row)
	}
	if _, err := store.GetRequest("gc-session", "won-delete-race"); err != nil {
		t.Fatalf("concurrently accepted receipt was lost: %v", err)
	}
}

func TestDeleteClosedSessionRevisionFencedSuccessAndNotFound(t *testing.T) {
	backing, store, _ := requestReceiptFixture(t)
	if err := backing.Close("gc-session"); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteClosedSession("gc-session"); err != nil {
		t.Fatalf("DeleteClosedSession: %v", err)
	}
	if _, err := backing.Get("gc-session"); !errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("Get after delete = %v, want ErrNotFound", err)
	}
	if err := store.DeleteClosedSession("gc-session"); !errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("second DeleteClosedSession = %v, want ErrNotFound", err)
	}
}
