package session

import (
	"encoding/json"
	"errors"
	"strings"
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

func requestTranscriptEvidenceFixture(sessionID, requestID string, generation int) RequestTranscriptEvidence {
	streamID := requestDigest("opaque-transcript-stream")
	generationID := requestDigest("opaque-transcript-generation")
	return RequestTranscriptEvidence{
		Status:             RequestTranscriptEvidenceAvailable,
		ToolStatus:         RequestTranscriptEvidenceAvailable,
		TranscriptStreamID: streamID, TranscriptGenerationID: generationID,
		References: []RequestTranscriptReference{
			{SessionID: sessionID, Generation: generation, RequestID: requestID, TranscriptStreamID: streamID, TranscriptGenerationID: generationID, EntryID: "entry-user", Kind: RequestTranscriptEntryReference},
			{SessionID: sessionID, Generation: generation, RequestID: requestID, TranscriptStreamID: streamID, TranscriptGenerationID: generationID, EntryID: "entry-assistant", Kind: RequestTranscriptToolUseReference, ToolID: "tool-1"},
			{SessionID: sessionID, Generation: generation, RequestID: requestID, TranscriptStreamID: streamID, TranscriptGenerationID: generationID, EntryID: "entry-result", Kind: RequestTranscriptToolResultReference, ToolID: "tool-1"},
		},
	}
}

func TestRequestTranscriptEvidenceMatchesOnlyCanonicalGeneratedEnvelope(t *testing.T) {
	receipt := RequestReceipt{
		RequestID: "request-envelope", SessionID: "gc-session", Generation: 2,
		MessageDigest: requestDigest("say hello"),
	}
	canonical, err := marshalTrackedRequestEnvelope(receipt.RequestID, receipt.SessionID, receipt.Generation, "say hello")
	if err != nil {
		t.Fatal(err)
	}
	if !RequestEnvelopeMatchesReceipt(string(canonical), receipt) {
		t.Fatal("canonical tracked envelope did not match its receipt")
	}
	for name, text := range map[string]string{
		"surrounding prose":       "copied envelope: " + string(canonical),
		"extra field":             strings.TrimSuffix(string(canonical), "}") + `,"extra":"ignored"}`,
		"noncanonical whitespace": " " + string(canonical),
		"changed message":         strings.Replace(string(canonical), "say hello", "say goodbye", 1),
		"wrong execution":         strings.Replace(string(canonical), `"generation":2`, `"generation":3`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			if RequestEnvelopeMatchesReceipt(text, receipt) {
				t.Fatal("noncanonical or mismatched text matched the receipt")
			}
		})
	}
}

func TestSessionRequestTranscriptEvidenceIsBoundAppendOnlyAndReplaySafe(t *testing.T) {
	backing, store, now := requestReceiptFixture(t)
	if _, err := store.AcceptRequest("gc-session", "request-transcript", 2, "report progress", now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RecordRequestDelivery("gc-session", "request-transcript", 2, RequestDeliveryAccepted, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	evidence := requestTranscriptEvidenceFixture("gc-session", "request-transcript", 2)
	first, err := store.RecordRequestTranscriptEvidence("gc-session", "request-transcript", 2, evidence, now.Add(2*time.Second))
	if err != nil || first.Ledger == nil || first.Ledger.TranscriptEvidence == nil || first.Ledger.TranscriptEvidence.Status != RequestTranscriptEvidenceAvailable || len(first.Ledger.TranscriptEvidence.References) != 3 {
		t.Fatalf("recorded transcript evidence = %+v, %v", first.Ledger, err)
	}
	if got := first.Ledger.Events[len(first.Ledger.Events)-1]; got.Kind != RequestEventTranscriptEvidence || got.TranscriptEvidence == nil {
		t.Fatalf("last event = %+v, want transcript evidence", got)
	}
	before, _ := backing.Get("gc-session")
	replayed, err := store.RecordRequestTranscriptEvidence("gc-session", "request-transcript", 2, evidence, now.Add(5*time.Second))
	after, _ := backing.Get("gc-session")
	if err != nil || before.Revision != after.Revision || replayed.Ledger == nil || replayed.Ledger.Digest != first.Ledger.Digest {
		t.Fatalf("evidence replay changed the ledger: revisions=%d/%d receipt=%+v err=%v", before.Revision, after.Revision, replayed.Ledger, err)
	}

	changed := requestTranscriptEvidenceFixture("gc-session", "request-transcript", 2)
	changed.References[1].EntryID = "different-tool-entry"
	before, _ = backing.Get("gc-session")
	if _, err := store.RecordRequestTranscriptEvidence("gc-session", "request-transcript", 2, changed, now.Add(6*time.Second)); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("changed evidence for one transcript generation accepted: %v", err)
	}
	after, _ = backing.Get("gc-session")
	if before.Revision != after.Revision {
		t.Fatal("conflicting transcript observation changed the session row")
	}
}

func TestSessionRequestTranscriptEvidenceRejectsInvalidAndStaleBindingsWithoutWrite(t *testing.T) {
	backing, store, now := requestReceiptFixture(t)
	if _, err := store.AcceptRequest("gc-session", "request-transcript", 2, "report progress", now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RecordRequestDelivery("gc-session", "request-transcript", 2, RequestDeliveryAccepted, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	valid := requestTranscriptEvidenceFixture("gc-session", "request-transcript", 2)
	invalid := []RequestTranscriptEvidence{}
	wrongRequest := requestTranscriptEvidenceFixture("gc-session", "other-request", 2)
	invalid = append(invalid, wrongRequest)
	wrongGeneration := requestTranscriptEvidenceFixture("gc-session", "request-transcript", 3)
	invalid = append(invalid, wrongGeneration)
	unavailableWithReferences := unavailableRequestTranscriptEvidence(RequestTranscriptNoExactEnvelope)
	unavailableWithReferences.TranscriptStreamID = valid.TranscriptStreamID
	unavailableWithReferences.TranscriptGenerationID = valid.TranscriptGenerationID
	unavailableWithReferences.References = append([]RequestTranscriptReference(nil), valid.References...)
	invalid = append(invalid, *unavailableWithReferences)
	toolUnavailableWithReferences := valid
	toolUnavailableWithReferences.ToolStatus = RequestTranscriptEvidenceUnavailable
	toolUnavailableWithReferences.ToolUnavailableReason = RequestTranscriptNoToolLineage
	invalid = append(invalid, toolUnavailableWithReferences)
	for index, evidence := range invalid {
		before, _ := backing.Get("gc-session")
		if _, err := store.RecordRequestTranscriptEvidence("gc-session", "request-transcript", 2, evidence, now.Add(time.Duration(index+2)*time.Second)); !errors.Is(err, ErrRequestConflict) {
			t.Fatalf("invalid transcript binding %d accepted: %v", index, err)
		}
		after, _ := backing.Get("gc-session")
		if before.Revision != after.Revision {
			t.Fatalf("invalid transcript binding %d changed the session row", index)
		}
	}
	if err := backing.Update("gc-session", beads.UpdateOpts{Metadata: map[string]string{"generation": "3", "instance_token": "replacement"}}); err != nil {
		t.Fatal(err)
	}
	before, _ := backing.Get("gc-session")
	if _, err := store.RecordRequestTranscriptEvidence("gc-session", "request-transcript", 2, valid, now.Add(time.Minute)); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("stale transcript generation accepted: %v", err)
	}
	after, _ := backing.Get("gc-session")
	if before.Revision != after.Revision {
		t.Fatal("stale transcript observation changed the session row")
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

func TestSessionRequestLedgerStagesReplayAndAttemptReference(t *testing.T) {
	backing, store, now := requestReceiptFixture(t)
	reference, err := NewRequestAttemptReference("city:test", "gc-work", "ae-"+strings.Repeat("a", 64), "42")
	if err != nil {
		t.Fatal(err)
	}
	attribution, err := AvailableRequestAttemptAttribution(reference)
	if err != nil {
		t.Fatal(err)
	}
	accepted, err := store.AcceptRequestWithAttribution("gc-session", "request-ledger", 2, "report progress", attribution, now)
	if err != nil || !accepted.NewlyAccepted || accepted.Ledger == nil || accepted.Ledger.Status != RequestLedgerAvailable {
		t.Fatalf("acceptance projection = %+v, %v", accepted, err)
	}
	if len(accepted.Ledger.Events) != 2 || accepted.Ledger.Events[0].Kind != RequestEventAccepted || accepted.Ledger.Events[1].Kind != RequestEventEffectUnverified {
		t.Fatalf("initial events = %+v", accepted.Ledger.Events)
	}
	if got := accepted.Ledger.AttemptAttribution; got == nil || got.Reference == nil || *got.Reference != reference {
		t.Fatalf("attempt reference was not passed through exactly: %+v", got)
	}
	before, _ := backing.Get("gc-session")
	replay, err := store.AcceptRequest("gc-session", "request-ledger", 2, "report progress", now.Add(time.Second))
	after, _ := backing.Get("gc-session")
	if err != nil || replay.NewlyAccepted || before.Revision != after.Revision || replay.Ledger == nil || replay.Ledger.Digest != accepted.Ledger.Digest {
		t.Fatalf("stable replay changed ledger: receipt=%+v err=%v revisions=%d/%d", replay, err, before.Revision, after.Revision)
	}
	if replay.Ledger.AttemptAttribution == nil || replay.Ledger.AttemptAttribution.Reference == nil || *replay.Ledger.AttemptAttribution.Reference != reference {
		t.Fatalf("replay lost exact reference: %+v", replay.Ledger.AttemptAttribution)
	}
	otherReference, err := NewRequestAttemptReference("rig:sample", "gc-work", "ae-"+strings.Repeat("c", 64), "43")
	if err != nil {
		t.Fatal(err)
	}
	otherAttribution, err := AvailableRequestAttemptAttribution(otherReference)
	if err != nil {
		t.Fatal(err)
	}
	before, _ = backing.Get("gc-session")
	changedAdapterReplay, err := store.AcceptRequestWithAttribution("gc-session", "request-ledger", 2, "report progress", otherAttribution, now.Add(1500*time.Millisecond))
	after, _ = backing.Get("gc-session")
	if err != nil || before.Revision != after.Revision || changedAdapterReplay.Ledger == nil || changedAdapterReplay.Ledger.AttemptAttribution == nil || changedAdapterReplay.Ledger.AttemptAttribution.Reference == nil || *changedAdapterReplay.Ledger.AttemptAttribution.Reference != reference {
		t.Fatalf("replay rewrote original attribution: %+v, %v", changedAdapterReplay.Ledger, err)
	}
	delivered, err := store.RecordRequestDelivery("gc-session", "request-ledger", 2, RequestDeliveryAccepted, now.Add(2*time.Second))
	if err != nil || delivered.Ledger == nil || len(delivered.Ledger.Events) != 4 {
		t.Fatalf("delivery ledger = %+v, %v", delivered.Ledger, err)
	}
	if delivered.Ledger.Events[2].Kind != RequestEventDeliveryAttempt || delivered.Ledger.Events[3].Kind != RequestEventProviderResult || delivered.AcknowledgedAt != nil || delivered.Effect != "unverified" {
		t.Fatalf("provider result collapsed stages: %+v", delivered)
	}
	acknowledged, err := store.AcknowledgeRequest("gc-session", "request-ledger", 2, "execution-token", now.Add(3*time.Second))
	if err != nil || acknowledged.Ledger == nil || len(acknowledged.Ledger.Events) != 5 || acknowledged.Ledger.Events[4].Kind != RequestEventAcknowledged {
		t.Fatalf("acknowledgement ledger = %+v, %v", acknowledged.Ledger, err)
	}
	for _, event := range acknowledged.Ledger.Events {
		if event.Kind == "effect_verified" {
			t.Fatal("ledger invented verified effect evidence")
		}
		if event.SessionID != "gc-session" || event.Generation != 2 || event.RequestID != "request-ledger" || event.MessageDigest != requestDigest("report progress") {
			t.Fatalf("event lost exact request binding: %+v", event)
		}
	}
}

func TestSessionRequestLedgerUnavailableAndLegacyReceipts(t *testing.T) {
	backing, store, now := requestReceiptFixture(t)
	accepted, err := store.AcceptRequest("gc-session", "request-unattributed", 2, "report progress", now)
	if err != nil || accepted.Ledger == nil || accepted.Ledger.Status != RequestLedgerAvailable {
		t.Fatalf("default acceptance = %+v, %v", accepted, err)
	}
	if got := accepted.Ledger.AttemptAttribution; got == nil || got.Status != attributionUnavailable || got.Reason != string(RequestAttemptAdapterUnavailable) || got.Reference != nil {
		t.Fatalf("missing adapter was not explicit: %+v", got)
	}
	if _, err := NewRequestAttemptReference("city:test", "gc-work", "not-canonical", "42"); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("invalid attempt reference accepted: %v", err)
	}
	for _, invalid := range [][4]string{
		{"", "gc-work", "ae-" + strings.Repeat("a", 64), "42"},
		{"city:test", "", "ae-" + strings.Repeat("a", 64), "42"},
		{"city:test", "gc-work", "ae-" + strings.Repeat("a", 64), "042"},
		{"unknown:test", "gc-work", "ae-" + strings.Repeat("a", 64), "42"},
	} {
		if _, err := NewRequestAttemptReference(invalid[0], invalid[1], invalid[2], invalid[3]); !errors.Is(err, ErrRequestConflict) {
			t.Fatalf("invalid locator accepted (%q,%q,%q,%q): %v", invalid[0], invalid[1], invalid[2], invalid[3], err)
		}
	}
	if _, err := store.AcceptRequestWithAttribution("gc-session", "bad-ref", 2, "report progress", RequestAttemptAttribution{Status: attributionAvailable, Reference: &RequestAttemptReference{AttemptID: "bad"}}, now); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("invalid attribution mutated acceptance: %v", err)
	}
	if err := backing.Update("gc-session", beads.UpdateOpts{Metadata: map[string]string{"generation": "3", "instance_token": "replacement"}}); err != nil {
		t.Fatal(err)
	}
	staleReference, err := NewRequestAttemptReference("city:test", "gc-work", "ae-"+strings.Repeat("d", 64), "44")
	if err != nil {
		t.Fatal(err)
	}
	staleAttribution, err := AvailableRequestAttemptAttribution(staleReference)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := backing.Get("gc-session")
	if _, err := store.AcceptRequestWithAttribution("gc-session", "stale-ref", 2, "report progress", staleAttribution, now); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("stale generation accepted: %v", err)
	}
	after, _ := backing.Get("gc-session")
	if before.Revision != after.Revision {
		t.Fatal("stale attempt reference changed the session row")
	}

	legacy := storedRequestReceipt{
		Version: 1,
		RequestReceipt: RequestReceipt{
			RequestID: "legacy", SessionID: "gc-session", Generation: 2,
			MessageDigest: requestDigest("legacy message"), AcceptedAt: now,
			Delivery: RequestDeliveryPending, Effect: "unverified",
		},
		ExecutionTokenDigest: requestDigest("execution-token"),
	}
	raw, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := backing.Update("gc-session", beads.UpdateOpts{Metadata: map[string]string{requestReceiptPrefix + "legacy": string(raw)}}); err != nil {
		t.Fatal(err)
	}
	read, err := store.GetRequest("gc-session", "legacy")
	if err != nil || read.Ledger == nil || read.Ledger.Status != RequestLedgerUnavailable || read.Ledger.UnavailableReason != requestLedgerLegacyReason || len(read.Ledger.Events) != 0 {
		t.Fatalf("legacy receipt invented an event history: %+v, %v", read, err)
	}
	projection, err := store.ListRequestLedger("gc-session")
	if err != nil || projection.Status != RequestLedgerUnavailable || projection.UnavailableReason != requestLedgerLegacyReason || projection.Digest == "" || len(projection.Requests) != 2 {
		t.Fatalf("session ledger projection = %+v, %v", projection, err)
	}
}

func TestSessionRequestLedgerRejectsCorruptHistoryWithoutPartialProjection(t *testing.T) {
	backing, store, now := requestReceiptFixture(t)
	if _, err := store.AcceptRequest("gc-session", "corrupt-ledger", 2, "report progress", now); err != nil {
		t.Fatal(err)
	}
	b, _ := backing.Get("gc-session")
	var record storedRequestReceipt
	if err := json.Unmarshal([]byte(b.Metadata[requestReceiptPrefix+"corrupt-ledger"]), &record); err != nil {
		t.Fatal(err)
	}
	record.Events[1].Sequence = 9
	raw, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := backing.Update("gc-session", beads.UpdateOpts{Metadata: map[string]string{requestReceiptPrefix + "corrupt-ledger": string(raw)}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetRequest("gc-session", "corrupt-ledger"); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("corrupt event sequence read as valid: %v", err)
	}
	projection, err := store.ListRequestLedger("gc-session")
	if !errors.Is(err, ErrRequestConflict) || projection.Status != RequestLedgerUnavailable || projection.UnavailableReason != "invalid_receipt" || len(projection.Requests) != 0 {
		t.Fatalf("corruption produced a partial ledger: %+v, %v", projection, err)
	}
}

func TestSessionRequestTranscriptEvidenceCorruptionFailsClosed(t *testing.T) {
	backing, store, now := requestReceiptFixture(t)
	if _, err := store.AcceptRequest("gc-session", "corrupt-transcript", 2, "report progress", now); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RecordRequestDelivery("gc-session", "corrupt-transcript", 2, RequestDeliveryAccepted, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RecordRequestTranscriptEvidence("gc-session", "corrupt-transcript", 2, requestTranscriptEvidenceFixture("gc-session", "corrupt-transcript", 2), now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	bead, _ := backing.Get("gc-session")
	var record storedRequestReceipt
	if err := json.Unmarshal([]byte(bead.Metadata[requestReceiptPrefix+"corrupt-transcript"]), &record); err != nil {
		t.Fatal(err)
	}
	record.Events[len(record.Events)-1].TranscriptEvidence.References[0].RequestID = "wrong-request"
	raw, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := backing.Update("gc-session", beads.UpdateOpts{Metadata: map[string]string{requestReceiptPrefix + "corrupt-transcript": string(raw)}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetRequest("gc-session", "corrupt-transcript"); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("corrupt transcript reference read as valid: %v", err)
	}
	projection, err := store.ListRequestLedger("gc-session")
	if !errors.Is(err, ErrRequestConflict) || projection.Status != RequestLedgerUnavailable || projection.UnavailableReason != "invalid_receipt" || len(projection.Requests) != 0 {
		t.Fatalf("corruption produced a partial transcript ledger: %+v, %v", projection, err)
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
	accepted, err := store.AcceptRequest(row.ID, "request-1", 2, "report progress", now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.RecordRequestDelivery(row.ID, "request-1", 2, RequestDeliveryQueued, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RecordRequestTranscriptEvidence(row.ID, "request-1", 2, requestTranscriptEvidenceFixture(row.ID, "request-1", 2), now.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	beforeReopen, err := store.GetRequest(row.ID, "request-1")
	if err != nil || accepted.Ledger == nil || beforeReopen.Ledger == nil || beforeReopen.Ledger.Status != RequestLedgerAvailable || beforeReopen.Ledger.TranscriptEvidence == nil || beforeReopen.Ledger.TranscriptEvidence.Status != RequestTranscriptEvidenceAvailable {
		t.Fatalf("pre-reopen ledger = %+v, %v", beforeReopen.Ledger, err)
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
	if err != nil || receipt.Generation != 2 || receipt.MessageDigest != requestDigest("report progress") || receipt.AcknowledgedAt != nil || receipt.Ledger == nil || receipt.Ledger.Digest != beforeReopen.Ledger.Digest || len(receipt.Ledger.Events) != len(beforeReopen.Ledger.Events) || receipt.Ledger.TranscriptEvidence == nil || !sameRequestTranscriptEvidence(*receipt.Ledger.TranscriptEvidence, *beforeReopen.Ledger.TranscriptEvidence) {
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
