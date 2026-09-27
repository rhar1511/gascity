package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/gastownhall/gascity/internal/api/apierr"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/storeref"
	"github.com/gastownhall/gascity/internal/worklifecycle"
)

// LifecycleRecoverySubmitInput carries a signed, revision-bound nudge request.
type LifecycleRecoverySubmitInput struct {
	CityScope
	Body worklifecycle.RecoveryRequest
}

// LifecycleRecoverySubmitOutput reports durable request intake only.
type LifecycleRecoverySubmitOutput struct {
	Body struct {
		RequestID string `json:"request_id"`
		IntentID  string `json:"intent_id"`
		Status    string `json:"status"`
	}
}

func registerLifecycleRecoveryRoutes(sm *SupervisorMux) {
	cityRegister(sm, huma.Operation{
		OperationID:   "submit-lifecycle-recovery-request",
		Method:        http.MethodPost,
		Path:          "/lifecycle/recovery-requests",
		Summary:       "Persist an authorized lifecycle recovery request",
		Description:   "Verifies a separately signed, exact-scope nudge request and stores it as a held controller intent. The endpoint does not send a message or change work ownership.",
		DefaultStatus: http.StatusAccepted,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusServiceUnavailable},
	}, (*Server).humaHandleLifecycleRecoverySubmit)
}

func (s *Server) humaHandleLifecycleRecoverySubmit(_ context.Context, input *LifecycleRecoverySubmitInput) (*LifecycleRecoverySubmitOutput, error) {
	cfg := s.state.Config()
	if cfg == nil {
		return nil, apierr.ServiceUnavailable.Msg("lifecycle recovery configuration unavailable")
	}
	digest, err := worklifecycle.VerifyRecoveryRequest(input.Body, cfg.Lifecycle, time.Now())
	if err != nil {
		return nil, apierr.Forbidden.Msg("recovery request signature, authority, scope, or validity window is not trusted")
	}
	plan, err := storeref.Plan(storeref.ByID{ID: input.Body.WorkItemID, WorkAxis: apiWorkAxis{s}}, s.residencyTopology())
	if err != nil {
		return nil, apierr.ServiceUnavailable.Msg("authoritative work store resolution failed")
	}
	owner, err := storeref.ResolveOwnerRow(plan, input.Body.WorkItemID)
	if err != nil {
		if errors.Is(err, beads.ErrNotFound) {
			return nil, apierr.BeadNotFound.Msg("recovery work item not found")
		}
		return nil, apierr.ServiceUnavailable.Msg("authoritative work store read failed")
	}
	work := owner.Bead
	if !owner.Read {
		work, err = owner.Store.Get(input.Body.WorkItemID)
		if err != nil {
			if errors.Is(err, beads.ErrNotFound) {
				return nil, apierr.BeadNotFound.Msg("recovery work item not found")
			}
			return nil, apierr.ServiceUnavailable.Msg("authoritative work store read failed")
		}
	}
	scope, idPrefix, err := lifecycleRequestStoreIdentity(s.state.CityName(), cfg, owner.Ref)
	if err != nil || scope != input.Body.Scope {
		return nil, apierr.Forbidden.Msg("recovery request does not name the authoritative work store scope")
	}
	if err := worklifecycle.ValidateRecoveryWorkEvidence(work, cfg.Lifecycle, scope); err != nil {
		return nil, apierr.ConflictWrongState.Msg("work item has no current, attached lifecycle admission")
	}
	if work.Revision != input.Body.ExpectedRevision || work.Status != "in_progress" || work.Assignee != input.Body.Owner {
		return nil, apierr.ConflictWrongState.Msg("work revision or current owner changed")
	}
	if work.Metadata[beadmeta.ClaimGenerationMetadataKey] != input.Body.ClaimGeneration || work.Metadata[beadmeta.SessionIDMetadataKey] != input.Body.SessionID {
		return nil, apierr.ConflictWrongState.Msg("work claim generation or session changed")
	}
	sessions := s.state.SessionsBeadStore()
	if sessions.Store == nil {
		return nil, apierr.ServiceUnavailable.Msg("authoritative session store unavailable")
	}
	sessionStore := session.NewStore(sessions)
	info, err := sessionStore.Get(input.Body.SessionID)
	if err != nil {
		if errors.Is(err, beads.ErrNotFound) {
			return nil, apierr.ConflictWrongState.Msg("recovery session no longer exists")
		}
		return nil, apierr.ServiceUnavailable.Msg("authoritative session read failed")
	}
	claim, err := sessionStore.CurrentClaimBeadID(input.Body.SessionID)
	if err != nil {
		return nil, apierr.ServiceUnavailable.Msg("authoritative reciprocal claim read failed")
	}
	if info.Closed || info.Generation != input.Body.SessionGeneration || claim != work.ID {
		return nil, apierr.ConflictWrongState.Msg("session generation or reciprocal claim changed")
	}
	intent, _, err := worklifecycle.PersistRecoveryIntent(owner.Store, idPrefix, input.Body, digest)
	if err != nil {
		return nil, lifecycleRecoveryIntakeError(err)
	}
	out := &LifecycleRecoverySubmitOutput{}
	out.Body.RequestID = input.Body.RequestID
	out.Body.IntentID = intent.ID
	out.Body.Status = "accepted"
	return out, nil
}

func lifecycleRequestStoreIdentity(city string, cfg *config.City, ref storeref.StoreRef) (scope, prefix string, err error) {
	city = strings.TrimSpace(city)
	if city == "" || cfg == nil {
		return "", "", fmt.Errorf("city identity is unavailable")
	}
	switch {
	case ref == storeref.WorkRef:
		prefix = strings.TrimSpace(config.EffectiveHQPrefix(cfg))
		return worklifecycle.ScopeForStore(city, "city:"+city), prefix, nil
	case strings.HasPrefix(string(ref), "rig:"):
		rigName := strings.TrimPrefix(string(ref), "rig:")
		for _, rig := range cfg.Rigs {
			if rig.Name == rigName {
				return worklifecycle.ScopeForStore(city, string(ref)), strings.TrimSpace(rig.EffectivePrefix()), nil
			}
		}
	}
	return "", "", fmt.Errorf("unsupported lifecycle work store reference %q", ref)
}

func lifecycleRecoveryIntakeError(err error) error {
	switch {
	case errors.Is(err, worklifecycle.ErrRecoveryRequestConflict):
		return apierr.IdempotencyMismatch.Msg("recovery request ID was already used for different signed content")
	case errors.Is(err, worklifecycle.ErrRecoveryWorkStale):
		return apierr.ConflictWrongState.Msg("recovery target changed before durable request intake")
	case errors.Is(err, worklifecycle.ErrRecoveryIntentUnavailable), errors.Is(err, beads.ErrConditionalWriteUnsupported):
		return apierr.ServiceUnavailable.Msg("durable recovery intent storage is unavailable")
	default:
		return apierr.ServiceUnavailable.Msg("durable recovery request could not be verified")
	}
}
