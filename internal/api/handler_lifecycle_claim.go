package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"github.com/gastownhall/gascity/internal/api/apierr"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/storeref"
	"github.com/gastownhall/gascity/internal/worklifecycle"
)

// LifecycleClaimSubmitRequest is the keyless hook request for an admitted
// work claim. The hook supplies its observed source revision/head and its
// managed runtime incarnation; the controller derives the actor, generation,
// exact evidence, and protected-mutation permit.
type LifecycleClaimSubmitRequest struct {
	WorkID                 string `json:"work_id" minLength:"1" maxLength:"200" doc:"Exact admitted work bead ID."`
	SourceStoreRef         string `json:"source_store_ref" maxLength:"200" doc:"Canonical store reference emitted by gc ready: city:<city_name> for the city work store or rig:<rig_name> for a rig store."`
	ExpectedRevision       int64  `json:"expected_revision" minimum:"1" doc:"Source revision observed by the claim candidate query."`
	ExpectedTransitionHead string `json:"expected_transition_head" minLength:"1" maxLength:"200" doc:"Lifecycle transition head observed by the claim candidate query."`
	SessionID              string `json:"session_id" minLength:"1" maxLength:"200" doc:"Managed session bead ID."`
	InstanceToken          string `json:"instance_token" minLength:"1" maxLength:"512" doc:"Current managed runtime instance token."`
	RuntimeEpoch           string `json:"runtime_epoch" minLength:"1" maxLength:"32" doc:"Canonical positive managed runtime epoch."`
}

// LifecycleClaimSubmitInput is the city-scoped Q54 claim request.
type LifecycleClaimSubmitInput struct {
	CityScope
	Body LifecycleClaimSubmitRequest
}

// LifecycleClaimSubmitOutput reports the durable claim transition and the
// reciprocal session claim stamp that was verified before success returned.
type LifecycleClaimSubmitOutput struct {
	Body struct {
		WorkID          string `json:"work_id"`
		Actor           string `json:"actor"`
		ClaimGeneration string `json:"claim_generation"`
		ReceiptID       string `json:"receipt_id"`
		Replayed        bool   `json:"replayed"`
	}
}

// LifecycleClaimTransitionRequest is the trusted in-process request passed to
// the controller-owned transition-chain provider after API authorization and
// authoritative store/session reads.
type LifecycleClaimTransitionRequest struct {
	Work                   beads.Bead
	WorkStore              beads.Store
	WorkStoreRef           storeref.StoreRef
	Scope                  string
	Session                session.Info
	Actor                  string
	ExpectedRevision       int64
	ExpectedTransitionHead string
}

// LifecycleClaimTransitionResult is the exact Q54 transition receipt needed
// to stamp the reciprocal session claim and return a stable result.
type LifecycleClaimTransitionResult struct {
	ClaimGeneration string
	ReceiptID       string
	Replayed        bool
	Recovered       bool
}

// LifecycleClaimTransitionProvider is an optional controller-state extension.
// Its implementation owns exact admission/policy verification, CurrentHead,
// and the atomic typed ClaimIdentity transition. Host permit capabilities stay
// entirely behind that provider and are never part of the hook request.
type LifecycleClaimTransitionProvider interface {
	ClaimLifecycleWork(context.Context, LifecycleClaimTransitionRequest) (LifecycleClaimTransitionResult, error)
}

func registerLifecycleClaimRoutes(sm *SupervisorMux) {
	cityRegister(sm, huma.Operation{
		OperationID:   "claim-admitted-lifecycle-work",
		Method:        http.MethodPost,
		Path:          "/lifecycle/claims",
		Summary:       "Claim admitted work through its durable lifecycle transition",
		Description:   "Authenticates the current managed session incarnation, atomically appends a Q54 ClaimIdentity transition for exact admitted work, then stamps and verifies the reciprocal session claim.",
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusServiceUnavailable},
	}, (*Server).humaHandleLifecycleClaimSubmit)
}

func (s *Server) humaHandleLifecycleClaimSubmit(ctx context.Context, input *LifecycleClaimSubmitInput) (*LifecycleClaimSubmitOutput, error) {
	body := input.Body
	if !validLifecycleClaimSubmitRequest(body) {
		return nil, apierr.InvalidRequest.Msg("lifecycle claim requires an exact work/store snapshot and managed session incarnation")
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
			return nil, apierr.BeadNotFound.Msg("lifecycle claim work item not found")
		}
		return nil, apierr.ServiceUnavailable.Msg("authoritative work store read failed")
	}
	work := owner.Bead
	if !owner.Read {
		work, err = owner.Store.Get(body.WorkID)
		if err != nil {
			if errors.Is(err, beads.ErrNotFound) {
				return nil, apierr.BeadNotFound.Msg("lifecycle claim work item not found")
			}
			return nil, apierr.ServiceUnavailable.Msg("authoritative work store read failed")
		}
	}
	if owner.Store == nil || work.ID != body.WorkID {
		return nil, apierr.ConflictWrongState.Msg("claim candidate no longer resolves to its exact authoritative work store")
	}
	scope, _, err := lifecycleRequestStoreIdentity(s.state.CityName(), cfg, owner.Ref)
	expectedSourceRef, sourceRefErr := lifecycleClaimRequestSourceStoreRef(s.state.CityName(), owner.Ref)
	if err != nil || sourceRefErr != nil {
		return nil, apierr.ServiceUnavailable.Msg("authoritative lifecycle work scope is unavailable")
	}
	if body.SourceStoreRef != expectedSourceRef {
		return nil, apierr.ConflictWrongState.Msg("claim candidate no longer resolves to its exact authoritative work store")
	}
	if work.Metadata[beadmeta.LifecycleAdmissionReceiptV2MetadataKey] == "" || work.Metadata[beadmeta.LifecycleAdmissionReceiptMetadataKey] != "" {
		return nil, apierr.ConflictWrongState.Msg("work item is not enrolled with an exact v2 lifecycle admission")
	}
	sessions := s.state.SessionsBeadStore()
	if sessions.Store == nil {
		return nil, apierr.ServiceUnavailable.Msg("authoritative session store unavailable")
	}
	sessionStore := session.NewStore(sessions)
	info, err := sessionStore.Get(body.SessionID)
	if err != nil {
		if errors.Is(err, beads.ErrNotFound) {
			return nil, apierr.ConflictWrongState.Msg("managed claim session no longer exists")
		}
		return nil, apierr.ServiceUnavailable.Msg("authoritative session read failed")
	}
	if !lifecycleClaimSessionMatches(info, body) {
		return nil, apierr.ConflictWrongState.Msg("managed session ID, token, epoch, or lifecycle state changed")
	}
	actor := session.AssigneeIdentifier(info)
	if actor == "" {
		return nil, apierr.ConflictWrongState.Msg("managed session has no authoritative work identity")
	}
	provider, ok := s.state.(LifecycleClaimTransitionProvider)
	if !ok {
		return nil, apierr.ServiceUnavailable.Msg("controller lifecycle claim transition provider is unavailable")
	}
	transition, err := provider.ClaimLifecycleWork(ctx, LifecycleClaimTransitionRequest{
		Work: work, WorkStore: owner.Store, WorkStoreRef: owner.Ref, Scope: scope,
		Session: info, Actor: actor, ExpectedRevision: body.ExpectedRevision,
		ExpectedTransitionHead: body.ExpectedTransitionHead,
	})
	if err != nil {
		return nil, lifecycleClaimTransitionError(err)
	}
	if !canonicalLifecycleClaimInteger(transition.ClaimGeneration) || strings.TrimSpace(transition.ReceiptID) == "" {
		return nil, apierr.ServiceUnavailable.Msg("controller returned an invalid durable claim result")
	}
	if err := stampLifecycleClaimReciprocal(sessionStore, info, body, work.ID, transition.ClaimGeneration); err != nil {
		if errors.Is(err, errLifecycleClaimSessionChanged) {
			return nil, apierr.ConflictWrongState.Msg("managed session incarnation changed before reciprocal claim stamp")
		}
		return nil, apierr.ServiceUnavailable.Msg("reciprocal session claim could not be stamped and verified")
	}
	out := &LifecycleClaimSubmitOutput{}
	out.Body.WorkID = work.ID
	out.Body.Actor = actor
	out.Body.ClaimGeneration = transition.ClaimGeneration
	out.Body.ReceiptID = transition.ReceiptID
	out.Body.Replayed = transition.Replayed || transition.Recovered
	return out, nil
}

func validLifecycleClaimSubmitRequest(request LifecycleClaimSubmitRequest) bool {
	return strings.TrimSpace(request.WorkID) != "" && strings.TrimSpace(request.WorkID) == request.WorkID &&
		strings.TrimSpace(request.SourceStoreRef) == request.SourceStoreRef &&
		request.ExpectedRevision > 0 && strings.TrimSpace(request.ExpectedTransitionHead) != "" && strings.TrimSpace(request.ExpectedTransitionHead) == request.ExpectedTransitionHead &&
		strings.TrimSpace(request.SessionID) != "" && strings.TrimSpace(request.SessionID) == request.SessionID &&
		strings.TrimSpace(request.InstanceToken) != "" && strings.TrimSpace(request.InstanceToken) == request.InstanceToken &&
		canonicalLifecycleClaimInteger(request.RuntimeEpoch)
}

func lifecycleClaimSessionMatches(info session.Info, request LifecycleClaimSubmitRequest) bool {
	if !session.IsSessionBeadOrRepairableInfo(info) || info.ID != request.SessionID || info.Closed ||
		info.InstanceToken == "" || info.InstanceToken != request.InstanceToken || info.Generation != request.RuntimeEpoch ||
		!canonicalLifecycleClaimInteger(info.Generation) {
		return false
	}
	base := session.ProjectLifecycle(session.LifecycleInputFromInfo(info)).BaseState
	switch base {
	case session.BaseStateNone, session.BaseStateStartPending, session.BaseStateCreating, session.BaseStateActive:
		return true
	default:
		return false
	}
}

var errLifecycleClaimSessionChanged = errors.New("lifecycle claim session incarnation changed")

func stampLifecycleClaimReciprocal(store *session.Store, expected session.Info, request LifecycleClaimSubmitRequest, workID, generation string) error {
	return session.WithSessionMutationLock(expected.ID, func() error {
		current, err := store.Get(expected.ID)
		if err != nil || !lifecycleClaimSessionMatches(current, request) ||
			current.InstanceToken != expected.InstanceToken || current.Generation != expected.Generation {
			return errors.Join(errLifecycleClaimSessionChanged, err)
		}
		if _, err := store.SetCurrentClaimForGeneration(current.ID, workID, generation); err != nil {
			return err
		}
		claimID, claimGeneration, err := store.CurrentClaim(current.ID)
		if err != nil || claimID != workID || claimGeneration != generation {
			return errors.Join(errors.New("reciprocal session claim readback differs from transition"), err)
		}
		readback, err := store.Get(current.ID)
		if err != nil || !lifecycleClaimSessionMatches(readback, request) ||
			readback.InstanceToken != expected.InstanceToken || readback.Generation != expected.Generation {
			return errors.Join(errLifecycleClaimSessionChanged, err)
		}
		return nil
	})
}

func canonicalLifecycleClaimInteger(raw string) bool {
	value, err := strconv.ParseInt(raw, 10, 64)
	return err == nil && value > 0 && strconv.FormatInt(value, 10) == raw
}

func lifecycleClaimRequestSourceStoreRef(city string, ref storeref.StoreRef) (string, error) {
	city = strings.TrimSpace(city)
	if city == "" {
		return "", errors.New("city identity is unavailable")
	}
	switch {
	case ref == storeref.WorkRef:
		return "city:" + city, nil
	case strings.HasPrefix(string(ref), "rig:"):
		return string(ref), nil
	default:
		return "", errors.New("unsupported lifecycle work store reference")
	}
}

func lifecycleClaimTransitionError(err error) error {
	switch {
	case errors.Is(err, worklifecycle.ErrTransitionChainStale), errors.Is(err, worklifecycle.ErrTransitionChainEvidence),
		errors.Is(err, worklifecycle.ErrTransitionChainReceipt), errors.Is(err, worklifecycle.ErrTransitionChainInvalid):
		return apierr.ConflictWrongState.Msg("admitted work or its current lifecycle transition head changed")
	case errors.Is(err, worklifecycle.ErrTransitionChainUnavailable), errors.Is(err, worklifecycle.ErrTransitionChainPermit):
		return apierr.ServiceUnavailable.Msg("durable lifecycle claim transition is unavailable")
	default:
		return apierr.ServiceUnavailable.Msg("durable lifecycle claim transition failed")
	}
}
