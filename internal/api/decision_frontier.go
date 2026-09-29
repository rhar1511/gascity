package api

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"github.com/gastownhall/gascity/internal/api/apierr"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/decisionfrontier"
	"github.com/gastownhall/gascity/internal/storeref"
)

// DecisionFrontierServiceProvider is an optional, controller-owned extension
// to State. Its verifier and delivery ports must be built from trusted runtime
// configuration; request bodies cannot select either port or its authority.
// Without this provider reads/ensures may use the fail-closed domain service,
// while answer verification and external delivery remain unavailable.
type DecisionFrontierServiceProvider interface {
	DecisionFrontierService() decisionfrontier.Service
}

// DecisionFrontierReadInput reads the exact question map for a source revision.
// City and store scope are resolved from the controller's current routing.
type DecisionFrontierReadInput struct {
	CityScope
	ID           string `path:"id" doc:"Source work bead ID."`
	WorkRevision string `query:"work_revision" required:"true" doc:"Exact source work revision token."`
}

// DecisionFrontierEnsureInput submits a proposed question map for one exact
// source revision. The caller cannot provide a city, store or answer authority.
type DecisionFrontierEnsureInput struct {
	CityScope
	ID             string `path:"id" doc:"Source work bead ID."`
	IdempotencyKey string `header:"Idempotency-Key" required:"true" doc:"Stable key for retries of this exact proposal."`
	Body           DecisionFrontierEnsureRequest
}

// DecisionFrontierEnsureRequest is the exact idempotency body for one proposal.
type DecisionFrontierEnsureRequest struct {
	WorkRevision string                    `json:"work_revision" minLength:"1" doc:"Exact source work revision token."`
	Proposal     decisionfrontier.Proposal `json:"proposal"`
}

// DecisionFrontierAnswerInput submits a signed human-answer envelope for one
// exact ticket version. The verifier supplies the authenticated subject.
type DecisionFrontierAnswerInput struct {
	CityScope
	ID             string `path:"id" doc:"Source work bead ID."`
	IdempotencyKey string `header:"Idempotency-Key" required:"true" doc:"Stable key for retries of this exact answer."`
	Body           decisionfrontier.AnswerSubmission
}

func (sm *SupervisorMux) registerCityDecisionFrontierRoutes() {
	cityGet(sm, "/bead/{id}/decision-frontier", (*Server).humaHandleDecisionFrontierRead,
		func(op *huma.Operation) {
			op.OperationID = "get-decision-frontier"
			op.Summary = "Read a decision frontier"
			op.Description = "Reads the question map and answer/delivery stages bound to the exact source work revision. City and physical store scope come from controller routing."
		},
		errorStatuses(http.StatusBadRequest, http.StatusNotFound, http.StatusConflict, http.StatusServiceUnavailable))
	cityPost(sm, "/bead/{id}/decision-frontier", (*Server).humaHandleDecisionFrontierEnsure,
		func(op *huma.Operation) {
			op.OperationID = "ensure-decision-frontier"
			op.Summary = "Ensure a decision frontier"
			op.Description = "Persists an idempotent question map and holds its exact source work revision. A proposal is not an answer. Prompt delivery occurs only when a separately configured trusted provider is available."
		},
		errorStatuses(http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity, http.StatusServiceUnavailable))
	cityPost(sm, "/bead/{id}/decision-frontier/answers", (*Server).humaHandleDecisionFrontierAnswer,
		func(op *huma.Operation) {
			op.OperationID = "answer-decision-frontier"
			op.Summary = "Record a verified human decision"
			op.Description = "Submits a proof envelope for a separately trusted human-answer verifier. Subject and authorization come from that verifier, never from the caller body; absent verifier configuration returns unavailable."
		},
		errorStatuses(http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity, http.StatusServiceUnavailable))
}

func (s *Server) humaHandleDecisionFrontierRead(ctx context.Context, input *DecisionFrontierReadInput) (*IndexOutput[decisionfrontier.Frontier], error) {
	store, scope, err := s.decisionFrontierSource(input.ID)
	if err != nil {
		return nil, err
	}
	frontier, err := s.decisionFrontierService().Read(ctx, store, scope, input.ID, input.WorkRevision)
	if err != nil {
		return nil, decisionFrontierAPIError(err)
	}
	return &IndexOutput[decisionfrontier.Frontier]{Index: s.latestIndex(), Body: frontier}, nil
}

func (s *Server) humaHandleDecisionFrontierEnsure(ctx context.Context, input *DecisionFrontierEnsureInput) (*IndexOutput[decisionfrontier.Frontier], error) {
	if strings.TrimSpace(input.IdempotencyKey) == "" {
		return nil, apierr.InvalidRequest.Msg("Idempotency-Key header is required")
	}
	store, scope, err := s.decisionFrontierSource(input.ID)
	if err != nil {
		return nil, err
	}
	service := s.decisionFrontierService()
	body := struct {
		CityRef  string                        `json:"city_ref"`
		StoreRef string                        `json:"store_ref"`
		ID       string                        `json:"id"`
		Request  DecisionFrontierEnsureRequest `json:"request"`
	}{CityRef: scope.CityRef, StoreRef: scope.StoreRef, ID: input.ID, Request: input.Body}
	_, err = withIdempotency(s.idem, "/v0/city/"+s.state.CityName()+"/bead/decision-frontier", input.IdempotencyKey, body,
		func() (bool, error) {
			_, ensureErr := service.Ensure(ctx, store, scope, input.ID, input.Body.WorkRevision, input.Body.Proposal)
			if ensureErr != nil {
				return false, decisionFrontierAPIError(ensureErr)
			}
			return true, nil
		})
	if err != nil {
		return nil, err
	}
	// Replay reads the authoritative durable frontier again. The cache records
	// that the request ran, not a stale snapshot of later ticket progress.
	frontier, err := service.Read(ctx, store, scope, input.ID, input.Body.WorkRevision)
	if err != nil {
		return nil, decisionFrontierAPIError(err)
	}
	return &IndexOutput[decisionfrontier.Frontier]{Index: s.latestIndex(), Body: frontier}, nil
}

func (s *Server) humaHandleDecisionFrontierAnswer(ctx context.Context, input *DecisionFrontierAnswerInput) (*IndexOutput[decisionfrontier.Frontier], error) {
	if strings.TrimSpace(input.IdempotencyKey) == "" {
		return nil, apierr.InvalidRequest.Msg("Idempotency-Key header is required")
	}
	store, scope, err := s.decisionFrontierSource(input.ID)
	if err != nil {
		return nil, err
	}
	service := s.decisionFrontierService()
	body := struct {
		CityRef  string                            `json:"city_ref"`
		StoreRef string                            `json:"store_ref"`
		ID       string                            `json:"id"`
		Request  decisionfrontier.AnswerSubmission `json:"request"`
	}{CityRef: scope.CityRef, StoreRef: scope.StoreRef, ID: input.ID, Request: input.Body}
	_, err = withIdempotency(s.idem, "/v0/city/"+s.state.CityName()+"/bead/decision-frontier/answers", input.IdempotencyKey, body,
		func() (bool, error) {
			_, answerErr := service.Answer(ctx, store, scope, input.ID, input.Body)
			if answerErr != nil {
				return false, decisionFrontierAPIError(answerErr)
			}
			return true, nil
		})
	if err != nil {
		return nil, err
	}
	frontier, err := service.Read(ctx, store, scope, input.ID, input.Body.WorkRevision)
	if err != nil {
		return nil, decisionFrontierAPIError(err)
	}
	return &IndexOutput[decisionfrontier.Frontier]{Index: s.latestIndex(), Body: frontier}, nil
}

func (s *Server) decisionFrontierService() decisionfrontier.Service {
	service := decisionfrontier.Service{}
	if provider, ok := s.state.(DecisionFrontierServiceProvider); ok {
		service = provider.DecisionFrontierService()
	}
	if !decisionAnswerVerifierAvailable(service.Verifier) && s.decisionAnswerVerifier != nil {
		service.Verifier = s.decisionAnswerVerifier
	}
	return service
}

func decisionAnswerVerifierAvailable(verifier decisionfrontier.AnswerVerifier) bool {
	if verifier == nil {
		return false
	}
	value := reflect.ValueOf(verifier)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return !value.IsNil()
	default:
		return true
	}
}

// decisionFrontierSource binds the path ID to its controller-resolved physical
// work store. Class bindings and graph/session/control beads cannot be used as
// source work for a human decision frontier.
func (s *Server) decisionFrontierSource(id string) (beads.Store, decisionfrontier.Scope, error) {
	store, work, ref, err := s.resolveBeadOwnerRef(id)
	if err != nil {
		return nil, decisionfrontier.Scope{}, err
	}
	if coordclass.Classify(work) != coordclass.ClassWork || work.Metadata[beadmeta.DecisionFrontierRecordMetadataKey] != "" {
		return nil, decisionfrontier.Scope{}, apierr.InvalidRequest.Msg("decision frontier requires source work from a city or rig work store")
	}
	cityRef := "city:" + s.state.CityName()
	storeRef := ""
	switch {
	case ref == storeref.WorkRef:
		storeRef = cityRef
	case strings.HasPrefix(string(ref), "rig:") && strings.TrimSpace(strings.TrimPrefix(string(ref), "rig:")) != "":
		storeRef = string(ref)
	default:
		return nil, decisionfrontier.Scope{}, apierr.ServiceUnavailable.Msg("decision frontier source store has no supported physical reference")
	}
	return store, decisionfrontier.Scope{CityRef: cityRef, StoreRef: storeRef}, nil
}

func decisionFrontierAPIError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, beads.ErrNotFound):
		return apierr.BeadNotFound.Msg("decision frontier source or record was not found")
	case errors.Is(err, decisionfrontier.ErrUnauthorized):
		return apierr.Forbidden.Msg("decision answer is not authorized")
	case errors.Is(err, decisionfrontier.ErrInvalid):
		return apierr.InvalidRequest.Msg("decision answer or question map is invalid")
	case errors.Is(err, decisionfrontier.ErrStale), errors.Is(err, decisionfrontier.ErrConflict), errors.Is(err, decisionfrontier.ErrAnswerInProgress),
		errors.Is(err, beads.ErrDecisionFrontierMutationBlocked):
		return apierr.ConflictWrongState.Msg("decision frontier or source work changed; read the current frontier before retrying")
	case errors.Is(err, decisionfrontier.ErrUnavailable), errors.Is(err, decisionfrontier.ErrAnswerVerifierUnavailable), errors.Is(err, decisionfrontier.ErrPromptDeliveryUnavailable),
		errors.Is(err, beads.ErrConditionalWriteUnsupported):
		return apierr.ServiceUnavailable.Msg("decision frontier authority or storage capability is unavailable")
	default:
		var precondition *beads.PreconditionFailedError
		if errors.As(err, &precondition) {
			return apierr.ConflictConcurrentModify.Msg("decision frontier source changed during the operation")
		}
		return apierr.Internal.Msg("decision frontier operation failed")
	}
}
