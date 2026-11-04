package worker

import (
	"context"
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

func TestStartContinuationUnknownWorkerNeverCreatesSession(t *testing.T) {
	handle, store, sp, _ := newTestSessionHandle(t, SessionSpec{Template: "probe", Command: "codex", Provider: "codex", WorkDir: t.TempDir()})
	err := handle.StartContinuation(context.Background(), sessionpkg.ContinuationExpectation{SessionID: "must-already-exist"}, nil)
	rows, listErr := sessionpkg.ListAllSessionBeads(store, beads.ListQuery{})
	if !errors.Is(err, sessionpkg.ErrContinuationIdentity) || listErr != nil || len(rows) != 0 || handle.currentSessionID() != "" {
		t.Fatalf("error=%v rows=%d session=%q runtime_calls=%v list_error=%v", err, len(rows), handle.currentSessionID(), sp.Calls, listErr)
	}
	for _, call := range sp.Calls {
		if call.Method == "Start" {
			t.Fatal("strict continuation attempted runtime start")
		}
	}
}

func TestStartContinuationRuntimeOnlyHandleIsUnsupported(t *testing.T) {
	handle := &RuntimeHandle{}
	if err := handle.StartContinuation(context.Background(), sessionpkg.ContinuationExpectation{}, nil); !errors.Is(err, ErrOperationUnsupported) {
		t.Fatalf("error=%v", err)
	}
}
