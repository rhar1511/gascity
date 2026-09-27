package session

import (
	"context"
	"errors"
	"strconv"
	"strings"
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
			if !strings.Contains(call.Message, "tracked-1") {
				t.Fatalf("delivery missing request identity: %+v", call)
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
