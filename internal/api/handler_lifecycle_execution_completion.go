package api

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"github.com/gastownhall/gascity/internal/api/apierr"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/storeref"
	"github.com/gastownhall/gascity/internal/worklifecycle"
)

// LifecycleExecutionCompletionIntent finishes an execution, not source acceptance.
type LifecycleExecutionCompletionIntent struct {
	ClaimGeneration string `json:"claim_generation" minLength:"1" maxLength:"32"`
	Outcome         string `json:"outcome" enum:"pass,fail,skipped"`
}

// LifecycleExecutionCompletionRequest binds completion to an exact managed claim.
type LifecycleExecutionCompletionRequest struct {
	LifecycleExecutionCompletionIntent
	WorkID                 string `json:"work_id" minLength:"1" maxLength:"200"`
	SourceStoreRef         string `json:"source_store_ref" minLength:"1" maxLength:"200"`
	ExpectedRevision       int64  `json:"expected_revision" doc:"Opaque nonzero revision; compare only for equality."`
	ExpectedTransitionHead string `json:"expected_transition_head" minLength:"1" maxLength:"200"`
	SessionID              string `json:"session_id" minLength:"1" maxLength:"200"`
	InstanceToken          string `json:"instance_token" minLength:"1" maxLength:"512"`
	RuntimeEpoch           string `json:"runtime_epoch" minLength:"1" maxLength:"32"`
}

// LifecycleExecutionCompletionInput binds a typed completion to one city.
type LifecycleExecutionCompletionInput struct {
	CityScope
	Body LifecycleExecutionCompletionRequest
}

// LifecycleExecutionCompletionTransitionRequest contains authenticated ownership.
type LifecycleExecutionCompletionTransitionRequest struct {
	Claim  LifecycleClaimTransitionRequest
	Intent LifecycleExecutionCompletionIntent
}

// LifecycleExecutionCompletionProvider owns protected descendant completion.
type LifecycleExecutionCompletionProvider interface {
	CompleteLifecycleExecution(context.Context, LifecycleExecutionCompletionTransitionRequest) (LifecycleCompletionResult, error)
}

func registerLifecycleExecutionCompletionRoutes(sm *SupervisorMux) {
	cityRegister(sm, huma.Operation{
		OperationID: "complete-lifecycle-execution", Method: http.MethodPost, Path: "/lifecycle/executions/complete",
		Summary:       "Complete the current protected execution step",
		Description:   "Authenticates the managed incarnation and reciprocal current claim, verifies protected descendant authority and appends a terminal step receipt. Retains the current claim for retry and workflow lookup. Does not accept or close the admitted source.",
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusServiceUnavailable},
	}, (*Server).humaHandleLifecycleExecutionCompletion)
}

func (s *Server) humaHandleLifecycleExecutionCompletion(ctx context.Context, input *LifecycleExecutionCompletionInput) (*LifecycleCompletionSubmitOutput, error) {
	body := input.Body
	incarnation := LifecycleClaimSubmitRequest{
		WorkID: body.WorkID, SourceStoreRef: body.SourceStoreRef,
		ExpectedRevision: body.ExpectedRevision, ExpectedTransitionHead: body.ExpectedTransitionHead,
		SessionID: body.SessionID, InstanceToken: body.InstanceToken, RuntimeEpoch: body.RuntimeEpoch,
	}
	if !validLifecycleClaimSubmitRequest(incarnation) || body.ExpectedTransitionHead == "" || !canonicalLifecycleClaimInteger(body.ClaimGeneration) ||
		(body.Outcome != "pass" && body.Outcome != "fail" && body.Outcome != "skipped") {
		return nil, apierr.InvalidRequest.Msg("execution completion requires exact step, incarnation, claim and outcome")
	}
	cfg := s.state.Config()
	if cfg == nil || !cfg.Lifecycle.AdmissionEnabled {
		return nil, apierr.ServiceUnavailable.Msg("lifecycle admission configuration is unavailable")
	}
	plan, err := storeref.Plan(storeref.ByID{ID: body.WorkID, WorkAxis: apiWorkAxis{s}}, s.residencyTopology())
	if err != nil {
		return nil, apierr.ServiceUnavailable.Msg("authoritative work store resolution failed")
	}
	owner, err := storeref.ResolveOwnerRow(plan, body.WorkID)
	if err != nil {
		if errors.Is(err, beads.ErrNotFound) {
			return nil, apierr.BeadNotFound.Msg("execution completion work item not found")
		}
		return nil, apierr.ServiceUnavailable.Msg("authoritative work store read failed")
	}
	work := owner.Bead
	if !owner.Read {
		work, err = owner.Store.Get(body.WorkID)
		if err != nil {
			if errors.Is(err, beads.ErrNotFound) {
				return nil, apierr.BeadNotFound.Msg("execution completion work item not found")
			}
			return nil, apierr.ServiceUnavailable.Msg("authoritative work store read failed")
		}
	}
	if owner.Store == nil || work.ID != body.WorkID {
		return nil, apierr.ConflictWrongState.Msg("execution step no longer resolves to its exact authoritative work store")
	}
	scope, _, err := lifecycleRequestStoreIdentity(s.state.CityName(), cfg, owner.Ref)
	expectedSourceRef, sourceRefErr := lifecycleClaimRequestSourceStoreRef(s.state.CityName(), owner.Ref)
	if err != nil || sourceRefErr != nil {
		return nil, apierr.ServiceUnavailable.Msg("authoritative lifecycle work scope is unavailable")
	}
	if body.SourceStoreRef != expectedSourceRef {
		return nil, apierr.ConflictWrongState.Msg("execution step no longer resolves to its exact authoritative work store")
	}
	// Enrollment only locates the protected lane. The trusted provider verifies
	// direct admission or inherited workflow authority after session authentication.
	if !worklifecycle.HasDurableEnrollment(work) || work.Metadata[beadmeta.LifecycleAdmissionReceiptMetadataKey] != "" {
		return nil, apierr.ConflictWrongState.Msg("work item has no supported durable lifecycle enrollment")
	}

	if work.Metadata[beadmeta.LifecycleAdmissionReceiptV2MetadataKey] != "" {
		return nil, apierr.ConflictWrongState.Msg("worker execution completion cannot accept an admitted source")
	}
	sessions := s.state.SessionsBeadStore()
	if sessions.Store == nil {
		return nil, apierr.ServiceUnavailable.Msg("authoritative session store unavailable")
	}
	provider, ok := s.state.(LifecycleExecutionCompletionProvider)
	if !ok {
		return nil, apierr.ServiceUnavailable.Msg("controller execution completion provider unavailable")
	}
	sessionStore := session.NewStore(sessions)
	var result LifecycleCompletionResult
	err = session.WithSessionMutationLock(body.SessionID, func() error {
		var info session.Info
		verify := func() error {
			var err error
			info, err = sessionStore.Get(body.SessionID)
			if err != nil || !lifecycleClaimSessionMatches(info, incarnation) || session.AssigneeIdentifier(info) == "" {
				return worklifecycle.ErrTransitionChainStale
			}
			id, generation, err := sessionStore.CurrentClaim(body.SessionID)
			if err != nil || id != body.WorkID || generation != body.ClaimGeneration {
				return worklifecycle.ErrTransitionChainStale
			}
			return nil
		}
		if err := verify(); err != nil {
			return err
		}
		actor := session.AssigneeIdentifier(info)
		result, err = provider.CompleteLifecycleExecution(ctx, LifecycleExecutionCompletionTransitionRequest{
			Claim: LifecycleClaimTransitionRequest{
				Work: work, WorkStore: owner.Store, WorkStoreRef: owner.Ref, Scope: scope,
				Session: info, Actor: actor, ExpectedRevision: body.ExpectedRevision, ExpectedTransitionHead: body.ExpectedTransitionHead,
			}, Intent: body.LifecycleExecutionCompletionIntent,
		})
		if err != nil {
			return err
		}
		if err := verify(); err != nil {
			return err
		}
		if session.AssigneeIdentifier(info) != actor {
			return worklifecycle.ErrTransitionChainStale
		}
		return nil
	})
	if err != nil {
		return nil, lifecycleClaimTransitionError(err)
	}
	if result.WorkID != body.WorkID || strings.TrimSpace(result.ReceiptID) == "" || result.Revision == 0 {
		return nil, apierr.ServiceUnavailable.Msg("controller returned an invalid execution completion receipt")
	}
	return &LifecycleCompletionSubmitOutput{Body: result}, nil
}
