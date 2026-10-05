package api

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"github.com/gastownhall/gascity/internal/api/apierr"
)

func (sm *SupervisorMux) registerCityPRActionRoutes() {
	cityGet(sm, "/pr-actions/queue", (*Server).humaHandlePRActionQueue,
		errorStatuses(http.StatusNotFound, http.StatusServiceUnavailable))
	cityRegister(sm, huma.Operation{
		OperationID:   "execute-pr-action",
		Method:        http.MethodPost,
		Path:          "/pr-actions",
		Summary:       "Execute a revision-bound pull-request action",
		Description:   "Prepares durable repair work, queues an exact evidenced revision for review, or merges it after separate exact human approval. The controller rechecks forge state and records an idempotent action before any effect.",
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusUnprocessableEntity, http.StatusServiceUnavailable},
	}, (*Server).humaHandlePRActionExecute)
}

// PRActionQueueInput scopes a queue read to the selected city.
type PRActionQueueInput struct {
	CityScope
}

// PRActionQueueOutput contains the authoritative PR queue response.
type PRActionQueueOutput struct {
	Body PRActionQueue
}

// PRActionExecuteInput is the typed request for a revision-bound central PR
// action. The idempotency key is accepted only as a header, so a JSON body
// cannot silently select or override the durable request identity.
type PRActionExecuteInput struct {
	CityScope
	IdempotencyKey string `header:"Idempotency-Key" required:"true" doc:"Stable key for this exact action request."`
	Body           PRActionExecuteBody
}

// PRActionExecuteBody is the stable JSON body clients submit with
// POST /v0/city/{cityName}/pr-actions. The idempotency key is deliberately a
// request header and is not part of this body.
type PRActionExecuteBody struct {
	Monitor       string       `json:"monitor" minLength:"1" doc:"Configured PR monitor name."`
	Owner         string       `json:"owner" minLength:"1" doc:"Repository owner from the server queue."`
	Repo          string       `json:"repo" minLength:"1" doc:"Repository name from the server queue."`
	PullRequest   int          `json:"pull_request" minimum:"1"`
	Action        PRActionKind `json:"action" enum:"prepare,queue_review,merge" doc:"Requested action from the current server queue."`
	WorkID        string       `json:"work_id,omitempty" doc:"Exact durable repair-work bead when the action needs one."`
	AttemptID     string       `json:"attempt_id,omitempty" doc:"Exact immutable attempt ID from the server queue."`
	HeadSHA       string       `json:"head_sha" minLength:"40" maxLength:"40" doc:"Exact candidate commit SHA from the current queue item."`
	BaseSHA       string       `json:"base_sha" minLength:"40" maxLength:"40" doc:"Exact base commit SHA from the current queue item."`
	PolicyVersion string       `json:"policy_version" minLength:"1" doc:"Policy version copied from the current queue."`
	HumanGrant    string       `json:"human_grant,omitempty" doc:"Separate authority-signed exact merge approval; required for merge."`
}

// PRActionExecuteOutput contains the durable result of one central PR action.
type PRActionExecuteOutput struct {
	Body PRActionResult
}

func (s *Server) humaHandlePRActionQueue(ctx context.Context, _ *PRActionQueueInput) (*PRActionQueueOutput, error) {
	queue, err := s.prActions().Queue(ctx)
	if err != nil {
		return nil, apierr.ServiceUnavailable.Msg("PR action queue sources are unavailable")
	}
	return &PRActionQueueOutput{Body: queue}, nil
}

func (s *Server) humaHandlePRActionExecute(ctx context.Context, input *PRActionExecuteInput) (*PRActionExecuteOutput, error) {
	cityWrite, ok := verifiedCityWritePrincipal(ctx)
	if !ok || cityWrite.City != s.state.CityName() {
		return nil, apierr.Forbidden.Msg("PR actions require a verified city-write grant")
	}
	if strings.TrimSpace(input.IdempotencyKey) == "" {
		return nil, apierr.InvalidRequest.Msg("Idempotency-Key header is required")
	}
	request := PRActionRequest{
		Monitor: input.Body.Monitor, Owner: input.Body.Owner, Repo: input.Body.Repo,
		PullRequest: input.Body.PullRequest, Action: input.Body.Action,
		WorkID: input.Body.WorkID, AttemptID: input.Body.AttemptID,
		HeadSHA: input.Body.HeadSHA, BaseSHA: input.Body.BaseSHA,
		PolicyVersion: input.Body.PolicyVersion, IdempotencyKey: input.IdempotencyKey,
	}
	actor := PRActionActor{CityWrite: cityWrite}
	if request.Action == PRActionMerge {
		if s.prHumanVerifier == nil || strings.TrimSpace(input.Body.HumanGrant) == "" {
			return nil, apierr.Forbidden.Msg("merge requires a separately authorized human approval grant")
		}
		principal, err := s.prHumanVerifier.Verify(input.Body.HumanGrant, PRHumanGrantExpectation{
			City: cityWrite.City, Scope: PRActionScopeMerge,
			Owner: request.Owner, Repo: request.Repo, PullRequest: request.PullRequest,
			HeadSHA: request.HeadSHA, BaseSHA: request.BaseSHA, PolicyVersion: request.PolicyVersion,
			WorkID: request.WorkID, IdempotencyKey: request.IdempotencyKey,
		})
		if err != nil {
			return nil, apierr.Forbidden.Msg("human approval grant does not authorize this exact merge")
		}
		actor.Human = &principal
	} else if input.Body.HumanGrant != "" {
		return nil, apierr.InvalidRequest.Msg("human_grant is only accepted for merge actions")
	}
	var actionErr error
	result, err := withIdempotency(s.idem, "/v0/city/"+s.state.CityName()+"/pr-actions", input.IdempotencyKey,
		struct {
			Request   PRActionRequest            `json:"request"`
			CityWrite VerifiedCityWritePrincipal `json:"city_write"`
			Human     *VerifiedPRPrincipal       `json:"human,omitempty"`
		}{Request: request, CityWrite: cityWrite, Human: actor.Human},
		func() (PRActionResult, error) {
			result, execErr := s.prActions().Execute(ctx, request, actor)
			actionErr = execErr
			return result, execErr
		})
	if err != nil {
		if actionErr == nil {
			// The endpoint cache owns same-process replay/conflict errors. Domain
			// errors from Execute are mapped below; preserve the cache's typed 409/422.
			return nil, err
		}
		err = actionErr
		switch {
		case errors.Is(err, ErrPRActionUnauthorized), errors.Is(err, ErrPRActionHumanRequired):
			return nil, apierr.Forbidden.Msg("PR action authorization failed")
		case errors.Is(err, ErrPRActionStale), errors.Is(err, ErrPRActionEvidenceMissing):
			return nil, apierr.ConflictWrongState.Msg("PR action target or evidence no longer matches the server queue")
		case errors.Is(err, ErrPRActionConflict):
			return nil, apierr.IdempotencyMismatch.Msg("Idempotency-Key was already used for a different PR action")
		case errors.Is(err, ErrPRActionUnavailable), errors.Is(err, ErrPRActionOutcomeUnknown):
			return nil, apierr.ServiceUnavailable.Msg("PR action outcome could not be verified")
		default:
			return nil, apierr.Internal.Msg("PR action could not be completed")
		}
	}
	return &PRActionExecuteOutput{Body: result}, nil
}

func (s *Server) prActions() *PRActionService {
	s.prActionServiceMu.Lock()
	defer s.prActionServiceMu.Unlock()
	if s.prActionService != nil {
		return s.prActionService
	}
	forge := s.prActionForge
	if forge == nil {
		token := strings.TrimSpace(os.Getenv("GH_TOKEN"))
		if token == "" {
			token = strings.TrimSpace(os.Getenv("GITHUB_TOKEN"))
		}
		if token != "" {
			forge = NewGitHubPRActionForge(token)
		}
	}
	evidence := s.prActionEvidence
	if evidence == nil {
		if provider, ok := s.state.(PRActionAttemptEvidenceProvider); ok {
			evidence = provider.PRActionAttemptEvidenceReader()
		}
	}
	policy, _ := ResolvePRActionPolicy(os.Getenv(PRActionPolicyEnv), s.state.CityName(), s.prHumanVerifier)
	s.prActionService = NewPRActionService(PRActionServiceOptions{
		State: s.state, Forge: forge, Evidence: evidence, HumanVerifier: s.prHumanVerifier, Policy: policy,
	})
	return s.prActionService
}
