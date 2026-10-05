package session

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"testing"
	"time"

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
				RequestID       string `json:"request_id"`
				SessionID       string `json:"session_id"`
				Generation      int    `json:"generation"`
				Instruction     string `json:"instruction"`
				AcknowledgeWith string `json:"acknowledge_with"`
				Message         string `json:"message"`
			}
			if err := json.Unmarshal([]byte(call.Message), &envelope); err != nil {
				t.Fatalf("decode tracked request envelope: %v", err)
			}
			if envelope.RequestID != "tracked-1" || envelope.SessionID != info.ID || envelope.Generation != gen || envelope.Message != "report progress" {
				t.Fatalf("delivery envelope = %+v", envelope)
			}
			if envelope.Instruction == "" || envelope.AcknowledgeWith != `gc session request ack "tracked-1"` {
				t.Fatalf("delivery lacks an exact acknowledgement instruction: %+v", envelope)
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

func TestSessionRequestDeliveryUsesTargetCapturedAtAcceptance(t *testing.T) {
	backing := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManagerWithOptions(backing, sp)
	info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Command: "claude", WorkDir: t.TempDir(), Provider: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	front := NewStore(beads.SessionStore{Store: backing})
	if _, err := front.AcceptRequest(info.ID, "immutable-target", 1, "report progress", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := backing.SetMetadata(info.ID, "session_name", "retargeted-runtime"); err != nil {
		t.Fatal(err)
	}

	receipt, err := mgr.SubmitRequest(context.Background(), info.ID, "immutable-target", 1, "report progress")
	if err != nil || receipt.Delivery != RequestDeliveryAccepted {
		t.Fatalf("submit = %+v, %v", receipt, err)
	}
	if got := sp.CountCalls("Nudge", info.SessionName); got != 1 {
		t.Fatalf("accepted target deliveries = %d, want 1", got)
	}
	if got := sp.CountCalls("Nudge", "retargeted-runtime"); got != 0 {
		t.Fatalf("retargeted runtime deliveries = %d, want 0", got)
	}
}

func TestSessionRequestDeliveryRejectsGenerationReplacementAfterAcceptance(t *testing.T) {
	backing := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := NewManagerWithOptions(backing, sp)
	info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Command: "claude", WorkDir: t.TempDir(), Provider: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	front := NewStore(beads.SessionStore{Store: backing})
	if _, err := front.AcceptRequest(info.ID, "generation-target", 1, "report progress", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := backing.SetMetadataBatch(info.ID, map[string]string{"generation": "2", "instance_token": NewInstanceToken()}); err != nil {
		t.Fatal(err)
	}

	if _, err := mgr.SubmitRequest(context.Background(), info.ID, "generation-target", 1, "report progress"); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("replacement generation submit = %v, want request conflict", err)
	}
	receipt, err := front.GetRequest(info.ID, "generation-target")
	if err != nil || receipt.Delivery != RequestDeliveryPending || receipt.DeliveryAttemptedAt != nil {
		t.Fatalf("replacement generation receipt = %+v, %v", receipt, err)
	}
	if got := sp.CountCalls("Nudge", info.SessionName); got != 0 {
		t.Fatalf("replacement generation deliveries = %d, want 0", got)
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
