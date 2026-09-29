package session

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/attemptevidence"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
)

func TestSessionRequestDeliveryIsDurableAndNeverReplayed(t *testing.T) {
	backing := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManagerWithOptions(backing, sp)
	info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Command: "claude", WorkDir: t.TempDir(), Provider: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	front := NewStore(beads.SessionStore{Store: backing})
	persisted, err := front.Get(info.ID)
	if err != nil {
		t.Fatal(err)
	}
	gen, _ := strconv.Atoi(persisted.Generation)
	// The API persists acceptance before scheduling delivery in the background.
	if _, err := front.AcceptRequest(info.ID, "tracked-1", gen, "report progress", time.Now()); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		receipt, err := mgr.SubmitRequest(context.Background(), info.ID, "tracked-1", gen, "report progress")
		if err != nil || receipt.Delivery != RequestDeliveryAccepted || receipt.AcknowledgedAt != nil {
			t.Fatalf("submit = %+v, %v", receipt, err)
		}
	}
	count := 0
	for _, call := range sp.SnapshotCalls() {
		if call.Method == "Nudge" || call.Method == "NudgeNow" {
			count++
			var envelope struct {
				RequestID            string `json:"request_id"`
				SessionID            string `json:"session_id"`
				Generation           int    `json:"generation"`
				Message              string `json:"message"`
				AcknowledgeWith      string `json:"acknowledge_with"`
				AcknowledgeWhen      string `json:"acknowledge_when"`
				AcknowledgementMeans string `json:"acknowledgement_means"`
			}
			if err := json.Unmarshal([]byte(call.Message), &envelope); err != nil {
				t.Fatalf("decode tracked delivery envelope: %v; message=%q", err, call.Message)
			}
			if envelope.RequestID != "tracked-1" || envelope.SessionID != info.ID ||
				envelope.Generation != gen || envelope.Message != "report progress" ||
				envelope.AcknowledgeWith != "gc session request ack -- tracked-1" ||
				envelope.AcknowledgeWhen != "after reading this request and before acting on it" ||
				envelope.AcknowledgementMeans != "receipt_only; this does not verify completion or effect" {
				t.Fatalf("delivery did not explain exact receipt acknowledgement: %+v", envelope)
			}
		}
	}
	if count != 1 {
		t.Fatalf("provider deliveries = %d", count)
	}
	if _, err := mgr.SubmitRequest(context.Background(), info.ID, "wrong-generation", gen+1, "report progress"); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("wrong generation: %v", err)
	}
}

func TestSessionRequestAcknowledgementCommandHandlesLeadingHyphens(t *testing.T) {
	backing := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManagerWithOptions(backing, sp)
	info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Command: "claude", WorkDir: t.TempDir(), Provider: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := mgr.SubmitRequest(context.Background(), info.ID, "-request-123", 1, "report progress")
	if err != nil || receipt.Delivery != RequestDeliveryAccepted {
		t.Fatalf("submit = %+v, %v", receipt, err)
	}
	var envelope struct {
		AcknowledgeWith string `json:"acknowledge_with"`
	}
	for _, call := range sp.SnapshotCalls() {
		if call.Method != "Nudge" && call.Method != "NudgeNow" {
			continue
		}
		if err := json.Unmarshal([]byte(call.Message), &envelope); err != nil {
			t.Fatalf("decode delivery envelope: %v", err)
		}
		break
	}
	if envelope.AcknowledgeWith != "gc session request ack -- -request-123" {
		t.Fatalf("acknowledgement command = %q, want leading-hyphen-safe command", envelope.AcknowledgeWith)
	}
}

func TestSessionRequestDeliveryDoesNotWakeOrRestart(t *testing.T) {
	backing := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManagerWithOptions(backing, sp)
	info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Command: "claude", WorkDir: t.TempDir(), Provider: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	if err := mgr.Suspend(info.ID); err != nil {
		t.Fatal(err)
	}
	before := sp.CountCalls("Start", info.SessionName)
	if _, err := mgr.SubmitRequest(context.Background(), info.ID, "suspended", 1, "report progress"); err == nil {
		t.Fatal("suspended execution accepted tracked delivery")
	}
	if after := sp.CountCalls("Start", info.SessionName); after != before {
		t.Fatal("tracked request woke a new execution")
	}
}

func TestSessionRequestUncertainProviderOutcomeIsNeverRetried(t *testing.T) {
	backing := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManagerWithOptions(backing, sp)
	info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Command: "claude", WorkDir: t.TempDir(), Provider: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	sp.NudgeErrors = map[string]error{info.SessionName: errors.New("connection lost after send")}
	receipt, err := mgr.SubmitRequest(context.Background(), info.ID, "uncertain", 1, "report progress")
	if err == nil || receipt.Delivery != RequestDeliveryUnknown || receipt.DeliveryAttemptedAt == nil {
		t.Fatalf("uncertain receipt = %+v, %v", receipt, err)
	}
	delete(sp.NudgeErrors, info.SessionName)
	receipt, err = mgr.SubmitRequest(context.Background(), info.ID, "uncertain", 1, "report progress")
	if err != nil || receipt.Delivery != RequestDeliveryUnknown {
		t.Fatalf("replay = %+v,%v", receipt, err)
	}
	if got := sp.CountCalls("Nudge", info.SessionName); got != 1 {
		t.Fatalf("provider calls = %d", got)
	}
}

func TestSessionRequestDeliveryForAttemptPreservesAttributionAndSendReservation(t *testing.T) {
	for _, uncertain := range []bool{false, true} {
		name := "accepted"
		if uncertain {
			name = "unknown"
		}
		t.Run(name, func(t *testing.T) {
			mgr, front, sp, info, binding, generation := requestDeliveryAttemptFixture(t)
			if uncertain {
				sp.NudgeErrors = map[string]error{info.SessionName: errors.New("connection lost after send")}
			}
			original := binding
			for attempt := range 2 {
				receipt, err := mgr.SubmitRequestForAttempt(context.Background(), info.ID, "bound-request", generation, "report progress", binding)
				if (err != nil) != (uncertain && attempt == 0) {
					t.Fatalf("submit %d error = %v", attempt, err)
				}
				wantDelivery := RequestDeliveryAccepted
				if uncertain {
					wantDelivery = RequestDeliveryUnknown
				}
				if receipt.Attempt == nil || *receipt.Attempt != original || receipt.Delivery != wantDelivery || receipt.AcknowledgedAt != nil || receipt.Effect != "unverified" {
					t.Fatalf("submit %d rewrote attribution or invented evidence: %+v", attempt, receipt)
				}
				delete(sp.NudgeErrors, info.SessionName)
				binding.WorkRevision = "8"
			}
			if got := sp.CountCalls("Nudge", info.SessionName); got != 1 {
				t.Fatalf("provider calls = %d, want one reserved send", got)
			}
			if _, err := front.SetCurrentClaim(info.ID, "different-work"); err != nil {
				t.Fatal(err)
			}
			if _, err := mgr.SubmitRequestForAttempt(context.Background(), info.ID, "bound-request", generation, "report progress", binding); !errors.Is(err, ErrRequestConflict) {
				t.Fatalf("old work claim accepted: %v", err)
			}
			if got := sp.CountCalls("Nudge", info.SessionName); got != 1 {
				t.Fatalf("changed claim caused another provider call: %d", got)
			}
		})
	}
}

func TestSessionRequestDeliveryForAttemptCannotRetrofitLegacyAcceptance(t *testing.T) {
	mgr, front, sp, info, binding, generation := requestDeliveryAttemptFixture(t)
	if _, err := front.AcceptRequest(info.ID, "legacy-request", generation, "report progress", time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.SubmitRequestForAttempt(context.Background(), info.ID, "legacy-request", generation, "report progress", binding); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("legacy request was rebound: %v", err)
	}
	if got := sp.CountCalls("Nudge", info.SessionName); got != 0 {
		t.Fatalf("legacy rebind reached provider: %d calls", got)
	}
	receipt, err := front.GetRequest(info.ID, "legacy-request")
	if err != nil || receipt.Attempt != nil || receipt.Delivery != RequestDeliveryPending {
		t.Fatalf("legacy acceptance changed: %+v, %v", receipt, err)
	}
}

func TestSessionRecoveryRequestRejectsDifferentRevisionInsertedAfterPreflight(t *testing.T) {
	mgr, front, sp, info, binding, generation := requestDeliveryAttemptFixture(t)
	if _, err := front.GetRequest(info.ID, "recovery-revision-race"); !errors.Is(err, ErrRequestNotFound) {
		t.Fatalf("preflight receipt = %v, want not found", err)
	}
	intervening := binding
	intervening.WorkRevision = "8"
	if _, err := front.AcceptRequestForAttempt(info.ID, "recovery-revision-race", generation, "report progress", intervening, time.Now()); err != nil {
		t.Fatalf("insert intervening pending receipt: %v", err)
	}

	if _, err := mgr.SubmitRequestForAttemptExact(context.Background(), info.ID, "recovery-revision-race", generation, "report progress", binding); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("exact submit accepted receipt with a different original revision: %v", err)
	}
	if got := sp.CountCalls("Nudge", info.SessionName); got != 0 {
		t.Fatalf("provider sends=%d after revision race, want 0", got)
	}
	receipt, err := front.GetRequest(info.ID, "recovery-revision-race")
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Delivery != RequestDeliveryPending || receipt.Attempt == nil || *receipt.Attempt != intervening || receipt.DeliveryAttemptedAt != nil {
		t.Fatalf("intervening receipt changed despite exact-binding refusal: %+v", receipt)
	}
}

func requestDeliveryAttemptFixture(t *testing.T) (*Manager, *Store, *runtime.Fake, Info, RequestAttemptBinding, int) {
	t.Helper()
	backing := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManagerWithOptions(backing, sp)
	info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Command: "claude", WorkDir: t.TempDir(), Provider: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	front := NewStore(beads.SessionStore{Store: backing})
	persisted, err := front.Get(info.ID)
	if err != nil {
		t.Fatal(err)
	}
	generation, err := strconv.Atoi(persisted.Generation)
	if err != nil {
		t.Fatal(err)
	}
	binding := requestAttemptFixture(t, "work-one", "claim-one")
	binding.Identity.SessionID = info.ID
	binding.Identity.SessionGeneration = persisted.Generation
	binding.AttemptID, err = attemptevidence.AttemptID(binding.Identity)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := front.SetCurrentClaim(info.ID, binding.Identity.ExecutionBeadID); err != nil {
		t.Fatal(err)
	}
	return mgr, front, sp, info, binding, generation
}
