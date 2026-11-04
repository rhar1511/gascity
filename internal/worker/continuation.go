package worker

import (
	"context"

	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// ContinuationHandle is an opt-in capability. It neither creates sessions nor
// grants work admission; ordinary Handle implementations remain compatible.
type ContinuationHandle interface {
	StartContinuation(context.Context, sessionpkg.ContinuationExpectation, sessionpkg.ContinuationPrepare) error
}

// StartContinuation requires this handle to identify an existing session. It
// never resolves a template into a new record or uses first-start defaults.
func (h *SessionHandle) StartContinuation(ctx context.Context, expected sessionpkg.ContinuationExpectation, prepare sessionpkg.ContinuationPrepare) error {
	id := h.currentSessionID()
	if id == "" || id != expected.SessionID {
		return sessionpkg.ErrContinuationIdentity
	}
	return h.manager.StartContinuation(ctx, expected, prepare)
}

// StartContinuation refuses runtime-only handles, which have no durable native
// session binding or owner-authenticated continuation observation.
func (h *RuntimeHandle) StartContinuation(context.Context, sessionpkg.ContinuationExpectation, sessionpkg.ContinuationPrepare) error {
	return ErrOperationUnsupported
}

var (
	_ ContinuationHandle = (*SessionHandle)(nil)
	_ ContinuationHandle = (*RuntimeHandle)(nil)
)
