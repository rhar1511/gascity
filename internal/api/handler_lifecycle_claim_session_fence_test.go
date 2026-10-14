package api

import (
	"context"
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/session"
)

func TestLifecycleClaimSessionFenceRejectsChangedSnapshotBeforeProvider(t *testing.T) {
	state, store, info, work := newLifecycleClaimTestState(t)
	called := false
	state.claim = func(context.Context, LifecycleClaimTransitionRequest) (LifecycleClaimTransitionResult, error) {
		called = true
		return LifecycleClaimTransitionResult{ClaimGeneration: "1", ReceiptID: "receipt"}, nil
	}
	if err := store.SetMetadata(info.ID, "session_key", "replacement-thread"); err != nil {
		t.Fatal(err)
	}
	_, err := claimLifecycleWorkKeepingSession(context.Background(), session.NewStore(state.SessionsBeadStore()),
		lifecycleClaimInput(info, work).Body, LifecycleClaimTransitionRequest{Session: info, Actor: session.AssigneeIdentifier(info), Work: work}, state)
	if !errors.Is(err, session.ErrWorkClaimSessionChanged) || called {
		t.Fatalf("error=%v provider called=%v", err, called)
	}
}

func TestLifecycleClaimSessionFenceRefusesExternalChangeBeforeReciprocalWrite(t *testing.T) {
	state, store, info, work := newLifecycleClaimTestState(t)
	committed := false
	state.claim = func(context.Context, LifecycleClaimTransitionRequest) (LifecycleClaimTransitionResult, error) {
		committed = true // A protected transition already committed in its work store.
		if err := store.SetMetadata(info.ID, "gc.worktree_attempt_id", "changed-attempt"); err != nil {
			t.Fatal(err)
		}
		return LifecycleClaimTransitionResult{ClaimGeneration: "1", ReceiptID: "receipt"}, nil
	}
	if _, err := (&Server{state: state}).humaHandleLifecycleClaimSubmit(context.Background(), lifecycleClaimInput(info, work)); err == nil {
		t.Fatal("session custody change reported claim success")
	}
	claim, _, err := session.NewStore(state.SessionsBeadStore()).CurrentClaim(info.ID)
	if err != nil || claim != "" || !committed {
		t.Fatalf("claim=%q committed=%v error=%v", claim, committed, err)
	}
}

func TestLifecycleClaimSessionFenceInvalidReceiptCannotStamp(t *testing.T) {
	state, _, info, work := newLifecycleClaimTestState(t)
	state.claim = func(context.Context, LifecycleClaimTransitionRequest) (LifecycleClaimTransitionResult, error) {
		return LifecycleClaimTransitionResult{ClaimGeneration: "1"}, nil
	}
	if _, err := (&Server{state: state}).humaHandleLifecycleClaimSubmit(context.Background(), lifecycleClaimInput(info, work)); err == nil {
		t.Fatal("invalid provider receipt accepted")
	}
	claim, _, err := session.NewStore(state.SessionsBeadStore()).CurrentClaim(info.ID)
	if err != nil || claim != "" {
		t.Fatalf("invalid receipt stamped %q: %v", claim, err)
	}
}
