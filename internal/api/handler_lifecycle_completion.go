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

// LifecycleCompletionSubmitRequest carries externally signed acceptance and an
// exact observed source snapshot. No signing key or mutation permit crosses the API.
type LifecycleCompletionSubmitRequest struct {
	WorkID                 string `json:"work_id" minLength:"1" maxLength:"200"`
	SourceStoreRef         string `json:"source_store_ref" minLength:"1" maxLength:"200"`
	ExpectedRevision       int64  `json:"expected_revision" doc:"Nonzero opaque signed source revision; compare only for equality."`
	ExpectedTransitionHead string `json:"expected_transition_head" minLength:"1" maxLength:"200"`
	CompletionReceipt      string `json:"completion_receipt" minLength:"1" maxLength:"65536" doc:"Canonical acceptance JSON signed by the authority named in this admission."`
}

// LifecycleCompletionSubmitInput binds reviewed acceptance to a city-scoped request.
type LifecycleCompletionSubmitInput struct {
	CityScope
	Body LifecycleCompletionSubmitRequest
}

// LifecycleCompletionResult identifies the verified terminal transition and revision.
type LifecycleCompletionResult struct {
	WorkID    string `json:"work_id"`
	ReceiptID string `json:"receipt_id"`
	Revision  int64  `json:"revision"`
	Replayed  bool   `json:"replayed"`
}

// LifecycleCompletionSubmitOutput is the typed completion response.
type LifecycleCompletionSubmitOutput struct{ Body LifecycleCompletionResult }

// LifecycleCompletionTransitionRequest carries the API's authoritative store
// binding. The controller reconstructs all admission and transition authority.
type LifecycleCompletionTransitionRequest struct {
	Work         beads.Bead
	WorkStore    beads.Store
	WorkStoreRef storeref.StoreRef
	Scope        string
	Submission   LifecycleCompletionSubmitRequest
}

// LifecycleCompletionTransitionProvider applies acceptance through controller-owned authority.
type LifecycleCompletionTransitionProvider interface {
	CompleteLifecycleWork(context.Context, LifecycleCompletionTransitionRequest) (LifecycleCompletionResult, error)
}

func registerLifecycleCompletionRoutes(sm *SupervisorMux) {
	cityRegister(sm, huma.Operation{
		OperationID: "complete-admitted-lifecycle-work", Method: http.MethodPost, Path: "/lifecycle/completions",
		Summary:       "Complete admitted work with separately signed acceptance",
		Description:   "Reconstructs current admission and transition authority, atomically attaches signed acceptance and its completion budget, then closes through a protected transition. Exact retries recover the same receipts.",
		DefaultStatus: http.StatusOK,
		Errors:        []int{http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusServiceUnavailable},
	}, (*Server).humaHandleLifecycleCompletionSubmit)
}

func (s *Server) humaHandleLifecycleCompletionSubmit(ctx context.Context, input *LifecycleCompletionSubmitInput) (*LifecycleCompletionSubmitOutput, error) {
	body := input.Body
	if strings.TrimSpace(body.WorkID) == "" || strings.TrimSpace(body.WorkID) != body.WorkID ||
		body.ExpectedRevision == 0 || strings.TrimSpace(body.ExpectedTransitionHead) == "" || strings.TrimSpace(body.ExpectedTransitionHead) != body.ExpectedTransitionHead ||
		body.CompletionReceipt == "" || strings.TrimSpace(body.SourceStoreRef) != body.SourceStoreRef {
		return nil, apierr.InvalidRequest.Msg("completion requires exact work/store revision and head plus signed acceptance")
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
			return nil, apierr.BeadNotFound.Msg("completion work item not found")
		}
		return nil, apierr.ServiceUnavailable.Msg("authoritative work store read failed")
	}
	work := owner.Bead
	if !owner.Read {
		work, err = owner.Store.Get(body.WorkID)
		if err != nil {
			if errors.Is(err, beads.ErrNotFound) {
				return nil, apierr.BeadNotFound.Msg("completion work item not found")
			}
			return nil, apierr.ServiceUnavailable.Msg("authoritative work store read failed")
		}
	}
	scope, _, err := lifecycleRequestStoreIdentity(s.state.CityName(), cfg, owner.Ref)
	sourceRef, refErr := lifecycleClaimRequestSourceStoreRef(s.state.CityName(), owner.Ref)
	if err != nil || refErr != nil {
		return nil, apierr.ServiceUnavailable.Msg("authoritative lifecycle scope is unavailable")
	}
	if owner.Store == nil || work.ID != body.WorkID || body.SourceStoreRef != sourceRef || work.Metadata[beadmeta.LifecycleAdmissionReceiptV2MetadataKey] == "" || work.Metadata[beadmeta.LifecycleAdmissionReceiptMetadataKey] != "" {
		return nil, apierr.ConflictWrongState.Msg("completion requires the exact store and supported source admission")
	}
	provider, ok := s.state.(LifecycleCompletionTransitionProvider)
	if !ok {
		return nil, apierr.ServiceUnavailable.Msg("controller lifecycle completion provider is unavailable")
	}
	result, err := provider.CompleteLifecycleWork(ctx, LifecycleCompletionTransitionRequest{Work: work, WorkStore: owner.Store, WorkStoreRef: owner.Ref, Scope: scope, Submission: body})
	if err != nil {
		if errors.Is(err, worklifecycle.ErrTransitionChainStale) || errors.Is(err, worklifecycle.ErrTransitionChainEvidence) || errors.Is(err, worklifecycle.ErrTransitionChainReceipt) || errors.Is(err, worklifecycle.ErrTransitionChainInvalid) {
			return nil, apierr.ConflictWrongState.Msg("completion acceptance, source snapshot, or lifecycle proof is invalid or changed")
		}
		return nil, apierr.ServiceUnavailable.Msg("durable lifecycle completion is unavailable")
	}
	if result.WorkID != work.ID || strings.TrimSpace(result.ReceiptID) == "" || result.Revision == 0 {
		return nil, apierr.ServiceUnavailable.Msg("controller returned an invalid completion receipt")
	}
	return &LifecycleCompletionSubmitOutput{Body: result}, nil
}
