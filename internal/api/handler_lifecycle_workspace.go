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
	"github.com/gastownhall/gascity/internal/storeref"
	"github.com/gastownhall/gascity/internal/worklifecycle"
)

// LifecycleWorkspaceSubmitRequest publishes native creation evidence against
// one observed admitted source head. It supplies no permit or metadata patch.
type LifecycleWorkspaceSubmitRequest struct {
	WorkID                 string                        `json:"work_id" minLength:"1" maxLength:"200"`
	SourceStoreRef         string                        `json:"source_store_ref" minLength:"1" maxLength:"200"`
	ExpectedRevision       int64                         `json:"expected_revision" doc:"Nonzero opaque signed source revision; equality only."`
	ExpectedTransitionHead string                        `json:"expected_transition_head" minLength:"1" maxLength:"200"`
	Workspace              worklifecycle.SourceWorkspace `json:"workspace" doc:"Exact native Ensure evidence, independently verified by the controller before publication."`
}

// Validate checks exact identity and complete workspace input before resolution.
func (r LifecycleWorkspaceSubmitRequest) Validate() error {
	if r.WorkID == "" || strings.TrimSpace(r.WorkID) != r.WorkID || r.ExpectedRevision == 0 ||
		r.ExpectedTransitionHead == "" || strings.TrimSpace(r.ExpectedTransitionHead) != r.ExpectedTransitionHead ||
		r.WorkID != r.Workspace.SourceID || r.SourceStoreRef != r.Workspace.StoreRef {
		return worklifecycle.ErrTransitionChainInvalid
	}
	return r.Workspace.Validate()
}

// LifecycleWorkspaceSubmitInput binds publication to the selected city.
type LifecycleWorkspaceSubmitInput struct {
	CityScope
	Body LifecycleWorkspaceSubmitRequest
}

// LifecycleWorkspaceResult identifies the immutable ownership publication.
type LifecycleWorkspaceResult struct {
	WorkID    string `json:"work_id"`
	ReceiptID string `json:"receipt_id"`
	Revision  int64  `json:"revision"`
	Replayed  bool   `json:"replayed"`
}

// LifecycleWorkspaceSubmitOutput is the typed workspace publication response.
type LifecycleWorkspaceSubmitOutput struct{ Body LifecycleWorkspaceResult }

// LifecycleWorkspaceTransitionRequest carries the API's resolved owner store.
type LifecycleWorkspaceTransitionRequest struct {
	Work         beads.Bead
	WorkStore    beads.Store
	WorkStoreRef storeref.StoreRef
	Scope        string
	Submission   LifecycleWorkspaceSubmitRequest
}

// LifecycleWorkspaceTransitionProvider retains native and mutation authority
// inside the controller. Generic writers cannot publish these custody fields.
type LifecycleWorkspaceTransitionProvider interface {
	PublishLifecycleWorkspace(context.Context, LifecycleWorkspaceTransitionRequest) (LifecycleWorkspaceResult, error)
}

func registerLifecycleWorkspaceRoutes(sm *SupervisorMux) {
	cityRegister(sm, huma.Operation{
		OperationID: "publish-admitted-lifecycle-workspace", Method: http.MethodPost, Path: "/lifecycle/workspaces",
		Summary:       "Publish verified native ownership for an admitted source",
		Description:   "Verifies exact native creation against the registered rig and signed source admission, then publishes complete ownership through a protected revision-fenced transition. Does not create a workspace, claim work, or launch a provider.",
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusServiceUnavailable},
	}, (*Server).humaHandleLifecycleWorkspaceSubmit)
}

func (s *Server) humaHandleLifecycleWorkspaceSubmit(ctx context.Context, input *LifecycleWorkspaceSubmitInput) (*LifecycleWorkspaceSubmitOutput, error) {
	body := input.Body
	if err := body.Validate(); err != nil {
		return nil, apierr.InvalidRequest.Msg("workspace publication requires an exact source snapshot and complete native ownership")
	}
	cfg := s.state.Config()
	if cfg == nil || !cfg.Lifecycle.AdmissionEnabled {
		return nil, apierr.ServiceUnavailable.Msg("lifecycle admission configuration is unavailable")
	}
	plan, err := storeref.Plan(storeref.ByID{ID: body.WorkID, WorkAxis: apiWorkAxis{s}}, s.residencyTopology())
	if err != nil {
		return nil, apierr.ServiceUnavailable.Msg("authoritative source store resolution failed")
	}
	owner, err := storeref.ResolveOwnerRow(plan, body.WorkID)
	if err != nil {
		if errors.Is(err, beads.ErrNotFound) {
			return nil, apierr.BeadNotFound.Msg("workspace source not found")
		}
		return nil, apierr.ServiceUnavailable.Msg("authoritative source read failed")
	}
	if owner.Store == nil {
		return nil, apierr.ServiceUnavailable.Msg("authoritative source store unavailable")
	}
	work, err := beads.HandlesFor(owner.Store).Live.Get(body.WorkID)
	if err != nil {
		return nil, apierr.ServiceUnavailable.Msg("authoritative source read failed")
	}
	scope, _, err := lifecycleRequestStoreIdentity(s.state.CityName(), cfg, owner.Ref)
	sourceRef, refErr := lifecycleClaimRequestSourceStoreRef(s.state.CityName(), owner.Ref)
	if err != nil || refErr != nil {
		return nil, apierr.ServiceUnavailable.Msg("authoritative lifecycle scope unavailable")
	}
	if work.ID != body.WorkID || body.SourceStoreRef != sourceRef || work.Metadata[beadmeta.LifecycleAdmissionReceiptV2MetadataKey] == "" || work.Metadata[beadmeta.LifecycleAdmissionReceiptMetadataKey] != "" {
		return nil, apierr.ConflictWrongState.Msg("workspace requires exact source store and supported admission")
	}
	provider, ok := s.state.(LifecycleWorkspaceTransitionProvider)
	if !ok {
		return nil, apierr.ServiceUnavailable.Msg("controller workspace publication unavailable")
	}
	result, err := provider.PublishLifecycleWorkspace(ctx, LifecycleWorkspaceTransitionRequest{Work: work, WorkStore: owner.Store, WorkStoreRef: owner.Ref, Scope: scope, Submission: body})
	if err != nil {
		if errors.Is(err, worklifecycle.ErrTransitionChainStale) || errors.Is(err, worklifecycle.ErrTransitionChainEvidence) || errors.Is(err, worklifecycle.ErrTransitionChainInvalid) || errors.Is(err, worklifecycle.ErrTransitionChainReceipt) {
			return nil, apierr.ConflictWrongState.Msg("workspace ownership, source snapshot or lifecycle proof changed")
		}
		return nil, apierr.ServiceUnavailable.Msg("protected workspace publication unavailable")
	}
	if result.WorkID != work.ID || result.ReceiptID == "" || result.Revision == 0 {
		return nil, apierr.ServiceUnavailable.Msg("controller returned invalid workspace receipt")
	}
	return &LifecycleWorkspaceSubmitOutput{Body: result}, nil
}
