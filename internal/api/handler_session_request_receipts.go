package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/gastownhall/gascity/internal/api/apierr"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worker"
)

// SessionRequestInput selects an exact durable session request.
type SessionRequestInput struct {
	CityScope
	ID        string `path:"id" doc:"Exact durable session ID."`
	RequestID string `path:"request_id" doc:"Exact request ID."`
}

// SessionRequestAcknowledgementInput binds acknowledgement to an execution credential.
type SessionRequestAcknowledgementInput struct {
	SessionRequestInput
	Token string `header:"X-GC-Session-Token" doc:"Credential of the intended session execution."`
	Body  struct {
		Generation int `json:"generation" minimum:"1" doc:"Intended session execution generation."`
	}
}

// SessionRequestOutput exposes the credential-free receipt.
type SessionRequestOutput struct {
	Body session.RequestReceipt
}

func registerSessionRequestRoutes(sm *SupervisorMux) {
	cityPost(sm, "/session/{id}/requests", (*Server).humaHandleSessionRequestSubmit, func(op *huma.Operation) { op.DefaultStatus = http.StatusAccepted }, errorStatuses(http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusServiceUnavailable))
	cityGet(sm, "/session/{id}/requests/{request_id}", (*Server).humaHandleSessionRequestGet, errorStatuses(http.StatusNotFound, http.StatusConflict, http.StatusServiceUnavailable))
	cityPost(sm, "/session/{id}/requests/{request_id}/ack", (*Server).humaHandleSessionRequestAcknowledge, errorStatuses(http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusServiceUnavailable))
}

func (s *Server) humaHandleSessionRequestGet(_ context.Context, input *SessionRequestInput) (*SessionRequestOutput, error) {
	receipt, err := session.NewStore(s.state.SessionsBeadStore()).GetRequest(input.ID, input.RequestID)
	if err != nil {
		return nil, sessionRequestError(err)
	}
	return &SessionRequestOutput{Body: receipt}, nil
}

func (s *Server) humaHandleSessionRequestAcknowledge(_ context.Context, input *SessionRequestAcknowledgementInput) (*SessionRequestOutput, error) {
	receipt, err := session.NewStore(s.state.SessionsBeadStore()).AcknowledgeRequest(input.ID, input.RequestID, input.Body.Generation, input.Token, time.Now())
	if err != nil {
		return nil, sessionRequestError(err)
	}
	return &SessionRequestOutput{Body: receipt}, nil
}

func sessionRequestError(err error) error {
	switch {
	case errors.Is(err, session.ErrRequestAcknowledgementRejected):
		return apierr.Forbidden.Msg("acknowledgement does not match the intended execution")
	case errors.Is(err, session.ErrRequestNotFound):
		return apierr.SessionRequestNotFound.Msg("session request not found")
	case errors.Is(err, session.ErrSessionNotFound), errors.Is(err, beads.ErrNotFound):
		return apierr.SessionNotFound.Msg("session not found")
	case errors.Is(err, session.ErrRequestConflict):
		return apierr.SessionConflict.Msg("session request identity, content, or stored evidence conflicts")
	default:
		return apierr.ServiceUnavailable.Msg("session request storage unavailable")
	}
}

// SessionRequestSubmitInput names the exact intended execution and request content.
type SessionRequestSubmitInput struct {
	CityScope
	ID   string `path:"id" doc:"Exact durable session ID."`
	Body struct {
		RequestID  string `json:"request_id" minLength:"1" maxLength:"200" doc:"Durable idempotency identity for this request."`
		Generation int    `json:"generation" minimum:"1" doc:"Exact intended execution generation."`
		Message    string `json:"message" minLength:"1" doc:"Message delivered with its request identity."`
	}
}

// Acceptance is persisted before background delivery. Repeated submissions
// share the same record; the session domain reserves provider delivery once.
func (s *Server) humaHandleSessionRequestSubmit(_ context.Context, input *SessionRequestSubmitInput) (*SessionRequestOutput, error) {
	store := s.state.SessionsBeadStore()
	if store.Store == nil {
		return nil, apierr.ServiceUnavailable.Msg("session request storage unavailable")
	}
	handle, err := s.workerHandleForSession(store.Store, input.ID)
	if err != nil {
		return nil, humaResolveError(err)
	}
	accepted, err := session.NewStore(store).AcceptRequest(input.ID, input.Body.RequestID, input.Body.Generation, input.Body.Message, time.Now())
	if err != nil {
		return nil, sessionRequestError(err)
	}
	requestID, generation, message := input.Body.RequestID, input.Body.Generation, input.Body.Message
	go func() {
		defer s.recoverAsRequestFailed(requestID, RequestOperationSessionSubmit)
		result, err := handle.Message(context.Background(), worker.MessageRequest{RequestID: requestID, Generation: generation, Text: message})
		if err != nil {
			s.emitSessionSubmitFailed(requestID, "tracked_submit_failed", err.Error())
			return
		}
		if result.Receipt == nil || result.Receipt.Delivery != session.RequestDeliveryAccepted {
			s.emitSessionSubmitFailed(requestID, "delivery_unknown", "provider delivery has not been established; inspect the durable receipt")
			return
		}
		// This event denotes provider submission only. The persisted receipt is
		// authoritative for session acknowledgement and effect evidence.
		s.emitSessionSubmitSucceeded(requestID, input.ID, false, string(session.SubmitIntentDefault))
	}()
	return &SessionRequestOutput{Body: accepted.RequestReceipt}, nil
}
