package session

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/attemptevidence"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

func TestSessionRequestAttemptRevisionPreservesIntegerPrecision(t *testing.T) {
	_, front, now := requestReceiptFixture(t)
	if _, err := front.SetCurrentClaimForGeneration("gc-session", "gc-work-1", "claim-1"); err != nil {
		t.Fatal(err)
	}
	binding := requestAttemptFixture(t, "gc-work-1", "claim-1")
	for index, revision := range []string{"-1", "-9223372036854775808", "9223372036854775807"} {
		requestID := []string{"negative-one", "minimum-revision", "maximum-revision"}[index]
		binding.WorkRevision = revision
		if _, err := front.AcceptRequestForAttempt("gc-session", requestID, 2, "report", binding, now); err != nil {
			t.Fatalf("AcceptRequestForAttempt(revision=%q): %v", revision, err)
		}
		receipt, err := front.GetRequest("gc-session", requestID)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(receipt)
		if err != nil {
			t.Fatal(err)
		}
		var decoded struct {
			Attempt struct {
				WorkRevision string `json:"work_revision"`
			} `json:"attempt"`
		}
		if err := json.Unmarshal(raw, &decoded); err != nil || decoded.Attempt.WorkRevision != revision {
			t.Fatalf("opaque revision %q was not preserved: %s, %v", revision, raw, err)
		}
	}
	for _, revision := range []string{
		"0", "-0", "007", "-01", "+1", " 1", "1 ", "1.0", "1e2",
		"9223372036854775808", "-9223372036854775809",
	} {
		binding.WorkRevision = revision
		if _, err := front.AcceptRequestForAttempt("gc-session", "invalid-"+revision, 2, "report", binding, now); !errors.Is(err, ErrRequestConflict) {
			t.Fatalf("malformed or out-of-range revision %q accepted: %v", revision, err)
		}
	}
}

func requestAttemptFixture(t *testing.T, workID, claim string) RequestAttemptBinding {
	t.Helper()
	identity := attemptevidence.Identity{
		Kind: attemptevidence.KindWorkbench, OwnerBeadID: workID,
		ExecutionBeadID: workID, SessionID: "gc-session", SessionGeneration: "2", ClaimGeneration: claim,
	}
	attemptID, err := attemptevidence.AttemptID(identity)
	if err != nil {
		t.Fatal(err)
	}
	return RequestAttemptBinding{StoreRef: "city:test", AttemptID: attemptID, WorkRevision: "7", Identity: identity}
}

func TestSessionRequestAttemptBindingSeparatesWorkWithinOneGeneration(t *testing.T) {
	backing, store, now := requestReceiptFixture(t)
	first := requestAttemptFixture(t, "gc-work-1", "claim-1")
	second := requestAttemptFixture(t, "gc-work-2", "claim-2")
	for index, binding := range []RequestAttemptBinding{first, second} {
		if _, err := store.SetCurrentClaimForGeneration("gc-session", binding.Identity.ExecutionBeadID, binding.Identity.ClaimGeneration); err != nil {
			t.Fatal(err)
		}
		id := []string{"request-one", "request-two"}[index]
		accepted, err := store.AcceptRequestForAttempt("gc-session", id, 2, "report progress", binding, now)
		if err != nil || accepted.Attempt == nil || *accepted.Attempt != binding {
			t.Fatalf("acceptance=%+v error=%v", accepted, err)
		}
		if _, err := store.RecordRequestDelivery("gc-session", id, 2, RequestDeliveryAccepted, now); err != nil {
			t.Fatal(err)
		}
		if _, err := store.AcknowledgeRequest("gc-session", id, 2, "execution-token", now); err != nil {
			t.Fatal(err)
		}
	}
	read, err := NewStore(beads.SessionStore{Store: backing}).GetRequest("gc-session", "request-one")
	if err != nil || read.Attempt == nil || *read.Attempt != first || read.AcknowledgedAt == nil || read.Effect != "unverified" {
		t.Fatalf("historical receipt changed after reassignment: %+v, %v", read, err)
	}
	if _, err := store.AcceptRequestForAttempt("gc-session", "request-one", 2, "report progress", second, now); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("request ID rebound to a second attempt: %v", err)
	}
}

func TestSessionRequestAttemptBindingRequiresExactReciprocalClaim(t *testing.T) {
	backing, front, now := requestReceiptFixture(t)
	binding := requestAttemptFixture(t, "gc-work-1", "claim-1")
	if _, err := front.AcceptRequestForAttempt("gc-session", "without-claim", 2, "report", binding, now); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("missing claim accepted: %v", err)
	}
	if _, err := front.SetCurrentClaimForGeneration("gc-session", "gc-work-1", "claim-1"); err != nil {
		t.Fatal(err)
	}
	raced := &receiptGenerationRace{MemStore: backing, change: func() {
		if err := backing.SetMetadata("gc-session", beadmeta.CurrentClaimBeadIDMetadataKey, "gc-work-2"); err != nil {
			t.Fatal(err)
		}
	}}
	front = NewStore(beads.SessionStore{Store: raced})
	if _, err := front.AcceptRequestForAttempt("gc-session", "claim-race", 2, "report", binding, now); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("changed reciprocal claim accepted: %v", err)
	}
	if _, err := front.GetRequest("gc-session", "claim-race"); !errors.Is(err, ErrRequestNotFound) {
		t.Fatalf("raced receipt persisted: %v", err)
	}
}

func TestSessionRequestAttemptBindingRejectsSameBeadRenewedBeforeAcceptanceWrite(t *testing.T) {
	backing, front, now := requestReceiptFixture(t)
	binding := requestAttemptFixture(t, "gc-work-1", "claim-1")
	if _, err := front.SetCurrentClaimForGeneration("gc-session", "gc-work-1", "claim-1"); err != nil {
		t.Fatal(err)
	}
	raced := &receiptGenerationRace{MemStore: backing, change: func() {
		if err := backing.SetMetadata("gc-session", beadmeta.CurrentClaimGenerationMetadataKey, "claim-2"); err != nil {
			t.Fatal(err)
		}
	}}
	front = NewStore(beads.SessionStore{Store: raced})
	if _, err := front.AcceptRequestForAttempt("gc-session", "renewed-claim-race", 2, "report", binding, now); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("same-bead renewed claim accepted: %v", err)
	}
	if _, err := front.GetRequest("gc-session", "renewed-claim-race"); !errors.Is(err, ErrRequestNotFound) {
		t.Fatalf("raced receipt persisted: %v", err)
	}
}

func TestSessionRequestAttemptBindingIsNotRetrofittedOrChangedByReplay(t *testing.T) {
	_, front, now := requestReceiptFixture(t)
	binding := requestAttemptFixture(t, "gc-work-1", "claim-1")
	if _, err := front.SetCurrentClaimForGeneration("gc-session", "gc-work-1", "claim-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := front.AcceptRequest("gc-session", "legacy-request", 2, "report", now); err != nil {
		t.Fatal(err)
	}
	if _, err := front.AcceptRequestForAttempt("gc-session", "legacy-request", 2, "report", binding, now); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("legacy identity retrofitted: %v", err)
	}
	if _, err := front.AcceptRequestForAttempt("gc-session", "bound-request", 2, "report", binding, now); err != nil {
		t.Fatal(err)
	}
	binding.WorkRevision = "8"
	replayed, err := front.AcceptRequestForAttempt("gc-session", "bound-request", 2, "report", binding, now)
	if err != nil || replayed.NewlyAccepted || replayed.Attempt == nil || replayed.Attempt.WorkRevision != "7" {
		t.Fatalf("replay rewrote observed revision: %+v %v", replayed, err)
	}
	plain, err := front.AcceptRequest("gc-session", "bound-request", 2, "report", now)
	if err != nil || plain.Attempt == nil || plain.Attempt.WorkRevision != "7" {
		t.Fatalf("delivery stripped attempt binding: %+v %v", plain, err)
	}
}

func TestSessionRequestBoundReplayRejectsClearedClaim(t *testing.T) {
	backing, front, now := requestReceiptFixture(t)
	binding := requestAttemptFixture(t, "gc-work-1", "claim-1")
	if _, err := front.SetCurrentClaimForGeneration("gc-session", "gc-work-1", "claim-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := front.AcceptRequestForAttempt("gc-session", "bound-cleared", 2, "report", binding, now); err != nil {
		t.Fatal(err)
	}
	if err := backing.SetMetadataBatch("gc-session", map[string]string{
		beadmeta.CurrentClaimBeadIDMetadataKey: "", beadmeta.CurrentClaimGenerationMetadataKey: "",
	}); err != nil {
		t.Fatal(err)
	}
	before, err := backing.Get("gc-session")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := front.AcceptRequest("gc-session", "bound-cleared", 2, "report", now); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("generic replay bypassed retained attempt binding: %v", err)
	}
	after, err := backing.Get("gc-session")
	if err != nil || after.Revision != before.Revision {
		t.Fatalf("rejected replay changed the session row: %+v, %v", after, err)
	}
	if historical, err := front.GetRequest("gc-session", "bound-cleared"); err != nil || historical.Attempt == nil || *historical.Attempt != binding {
		t.Fatalf("claim clearing lost historical attribution: %+v, %v", historical, err)
	}
}
