package api

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/gastownhall/gascity/internal/api/apierr"
	"github.com/gastownhall/gascity/internal/citywriteauth"
	"github.com/gastownhall/gascity/internal/decisionfrontier"
)

const (
	humanPreparationTTL   = 5 * time.Minute
	humanPreparationLimit = 256
)

type humanPreparation struct {
	prepared decisionfrontier.PreparedProposal
	request  DecisionFrontierEnsureRequest
	workID   string
	storeRef string
	keyID    string
	expires  time.Time
}

// HumanSourcePrepareInput prepares an exact proposal without creating a frontier.
type HumanSourcePrepareInput struct {
	CityScope
	ID   string `path:"id"`
	Body DecisionFrontierEnsureRequest
}

// HumanSourcePreparation identifies an ephemeral server-owned prepared client.
// It is not an answer, delivery receipt or reusable operational capability.
type HumanSourcePreparation struct {
	PreparationToken string                         `json:"preparation_token"`
	ExpiresAt        time.Time                      `json:"expires_at"`
	CityRef          string                         `json:"city_ref"`
	StoreRef         string                         `json:"store_ref"`
	WorkID           string                         `json:"work_id"`
	WorkRevision     string                         `json:"work_revision"`
	TargetName       string                         `json:"target_name"`
	Binding          decisionfrontier.PromptBinding `json:"binding"`
	DeliveryContract string                         `json:"delivery_contract"`
}

// HumanSourceEnsureInput uses only a server-owned preparation and fresh auth.
type HumanSourceEnsureInput struct {
	CityScope
	ID             string `path:"id"`
	IdempotencyKey string `header:"Idempotency-Key" required:"true"`
	Body           HumanSourceEnsureRequest
}

// HumanSourceEnsureRequest references a proposal prepared on this controller.
type HumanSourceEnsureRequest struct {
	PreparationToken string `json:"preparation_token" minLength:"1"`
}

// HumanSourceResumeInput binds original frontier and current physical revisions.
type HumanSourceResumeInput struct {
	CityScope
	ID   string `path:"id"`
	Body HumanSourceResumeRequest
}

// HumanSourceResumeRequest carries exact revisions, never a readiness claim.
type HumanSourceResumeRequest struct {
	FrontierRevision string `json:"frontier_revision" minLength:"1"`
	PhysicalRevision string `json:"physical_revision" minLength:"1"`
}

func (sm *SupervisorMux) registerCityHumanSourceRoutes() {
	errors := errorStatuses(http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusRequestEntityTooLarge, http.StatusUnprocessableEntity, http.StatusServiceUnavailable)
	cityPost(sm, "/bead/{id}/decision-frontier/prepare", (*Server).humaHandleHumanSourcePrepare,
		humanSourceOperation("prepare-human-source-proposal", "Prepare an exact target-bound human proposal", writeAuthHeader), errors)
	cityPost(sm, "/bead/{id}/decision-frontier/prepared", (*Server).humaHandleHumanSourceEnsure,
		humanSourceOperation("ensure-prepared-human-source-proposal", "Ensure or replay a fenced human proposal", writeAuthHeader), errors)
	cityGet(sm, "/bead/{id}/decision-frontier/authorized", (*Server).humaHandleHumanSourceRead,
		humanSourceOperation("read-authorized-human-source-frontier", "Read authenticated durable human authority evidence", readAuthHeader), errors)
	cityPost(sm, "/bead/{id}/decision-frontier/authorized/answers", (*Server).humaHandleHumanSourceAnswer,
		humanSourceOperation("submit-human-source-answer", "Submit a signed answer and revalidate durable authority", writeAuthHeader), errors)
	cityPost(sm, "/bead/{id}/decision-frontier/check-resume", (*Server).humaHandleHumanSourceResume,
		humanSourceOperation("check-human-source-resume", "Check conditional exact-revision resume eligibility", writeAuthHeader), errors)
}

// The middleware verifies these request-bound grants; endpoint policy requires
// the verified context even when the listener's global auth gate is optional.
func humanSourceOperation(id, summary, header string) func(*huma.Operation) {
	return func(op *huma.Operation) {
		op.OperationID, op.Summary = id, summary
		op.MaxBodyBytes = citywriteauth.MaxHTTPBodyBytes
		op.Parameters = append(op.Parameters, &huma.Param{
			Name: header, In: "header", Required: true,
			Description: "Fresh request-bound grant verified by the configured city authority. Network position and caller success fields are insufficient.",
			Schema:      &huma.Schema{Type: "string"},
		})
	}
}

func (s *Server) requireHumanSourceWrite(ctx context.Context) error {
	principal, ok := verifiedCityWritePrincipal(ctx)
	if !ok || principal.City != s.state.CityName() {
		return apierr.Forbidden.Msg("human source adapters require verified request-bound city-write authentication")
	}
	return nil
}

func (s *Server) humaHandleHumanSourcePrepare(ctx context.Context, input *HumanSourcePrepareInput) (*IndexOutput[HumanSourcePreparation], error) {
	if err := s.requireHumanSourceWrite(ctx); err != nil {
		return nil, err
	}
	store, scope, err := s.decisionFrontierSource(input.ID)
	if err != nil {
		return nil, err
	}
	prepared, err := s.decisionFrontierService().PrepareProposal(ctx, store, scope, input.ID, input.Body.WorkRevision, input.Body.Proposal)
	if err != nil {
		return nil, decisionFrontierAPIError(err)
	}
	var nonce [32]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, apierr.Internal.Msg("cannot allocate human proposal preparation")
	}
	token := base64.RawURLEncoding.EncodeToString(nonce[:])
	now := time.Now()
	principal, _ := verifiedCityWritePrincipal(ctx)
	entry := humanPreparation{prepared: prepared, request: input.Body, workID: input.ID, storeRef: scope.StoreRef, keyID: principal.KeyID, expires: now.Add(humanPreparationTTL)}
	s.humanPreparationsMu.Lock()
	defer s.humanPreparationsMu.Unlock()
	if s.humanPreparations == nil {
		s.humanPreparations = make(map[string]humanPreparation)
	}
	for key, value := range s.humanPreparations {
		if !now.Before(value.expires) {
			delete(s.humanPreparations, key)
		}
	}
	if len(s.humanPreparations) >= humanPreparationLimit {
		return nil, apierr.ServiceUnavailable.Msg("human proposal preparation capacity exhausted")
	}
	s.humanPreparations[token] = entry
	return &IndexOutput[HumanSourcePreparation]{Index: s.latestIndex(), Body: HumanSourcePreparation{
		PreparationToken: token, ExpiresAt: entry.expires, CityRef: scope.CityRef, StoreRef: scope.StoreRef, WorkID: input.ID, WorkRevision: input.Body.WorkRevision,
		TargetName: prepared.TargetName(), Binding: prepared.TargetBinding(), DeliveryContract: "gascity.decision-frontier.independent-questions.v1",
	}}, nil
}

func (s *Server) humaHandleHumanSourceEnsure(ctx context.Context, input *HumanSourceEnsureInput) (*IndexOutput[decisionfrontier.Frontier], error) {
	if err := s.requireHumanSourceWrite(ctx); err != nil {
		return nil, err
	}
	if strings.TrimSpace(input.IdempotencyKey) == "" {
		return nil, apierr.InvalidRequest.Msg("Idempotency-Key header is required")
	}
	store, scope, err := s.decisionFrontierSource(input.ID)
	if err != nil {
		return nil, err
	}
	principal, _ := verifiedCityWritePrincipal(ctx)
	s.humanPreparationsMu.Lock()
	entry, exists := s.humanPreparations[input.Body.PreparationToken]
	s.humanPreparationsMu.Unlock()
	if !exists || !time.Now().Before(entry.expires) || entry.workID != input.ID || entry.storeRef != scope.StoreRef || entry.keyID != principal.KeyID {
		return nil, apierr.ConflictWrongState.Msg("human proposal preparation is missing, expired or belongs to another request scope; prepare again")
	}
	// Cache binds the key to the exact intent, never shortcuts the target fence.
	_, err = withIdempotency(s.idem, "/bead/"+input.ID+"/decision-frontier/prepared", input.IdempotencyKey, entry.request, func() (bool, error) { return true, nil })
	if err != nil {
		return nil, err
	}
	frontier, err := s.decisionFrontierService().EnsurePrepared(ctx, store, entry.prepared)
	if err != nil {
		return nil, decisionFrontierAPIError(err)
	}
	return &IndexOutput[decisionfrontier.Frontier]{Index: s.latestIndex(), Body: frontier}, nil
}

func (s *Server) humaHandleHumanSourceRead(ctx context.Context, input *DecisionFrontierReadInput) (*IndexOutput[decisionfrontier.AuthorizedFrontier], error) {
	principal, ok := verifiedCityReadPrincipal(ctx)
	if !ok || principal.City != s.state.CityName() {
		return nil, apierr.Forbidden.Msg("human source readback requires verified request-bound city-read authentication")
	}
	store, scope, err := s.decisionFrontierSource(input.ID)
	if err != nil {
		return nil, err
	}
	frontier, err := s.decisionFrontierService().ReadAuthorizedFrontier(ctx, store, scope, input.ID, input.WorkRevision)
	if err != nil {
		return nil, decisionFrontierAPIError(err)
	}
	return &IndexOutput[decisionfrontier.AuthorizedFrontier]{Index: s.latestIndex(), Body: frontier}, nil
}

func (s *Server) humaHandleHumanSourceAnswer(ctx context.Context, input *DecisionFrontierAnswerInput) (*IndexOutput[decisionfrontier.AuthorizedFrontier], error) {
	if err := s.requireHumanSourceWrite(ctx); err != nil {
		return nil, err
	}
	if strings.TrimSpace(input.IdempotencyKey) == "" {
		return nil, apierr.InvalidRequest.Msg("Idempotency-Key header is required")
	}
	store, scope, err := s.decisionFrontierSource(input.ID)
	if err != nil {
		return nil, err
	}
	_, err = withIdempotency(s.idem, "/bead/"+input.ID+"/decision-frontier/authorized/answers", input.IdempotencyKey, input.Body,
		func() (bool, error) {
			_, err := s.decisionFrontierService().SubmitAnswer(ctx, store, scope, input.ID, input.Body)
			return err == nil, decisionFrontierAPIError(err)
		})
	if err != nil {
		return nil, err
	}
	frontier, err := s.decisionFrontierService().ReadAuthorizedFrontier(ctx, store, scope, input.ID, input.Body.WorkRevision)
	if err != nil {
		return nil, decisionFrontierAPIError(err)
	}
	return &IndexOutput[decisionfrontier.AuthorizedFrontier]{Index: s.latestIndex(), Body: frontier}, nil
}

func (s *Server) humaHandleHumanSourceResume(ctx context.Context, input *HumanSourceResumeInput) (*IndexOutput[decisionfrontier.ResumeEligibility], error) {
	if err := s.requireHumanSourceWrite(ctx); err != nil {
		return nil, err
	}
	store, scope, err := s.decisionFrontierSource(input.ID)
	if err != nil {
		return nil, err
	}
	result, err := s.decisionFrontierService().CheckResume(ctx, store, scope, input.ID, input.Body.FrontierRevision, input.Body.PhysicalRevision)
	if err != nil {
		return nil, decisionFrontierAPIError(err)
	}
	return &IndexOutput[decisionfrontier.ResumeEligibility]{Index: s.latestIndex(), Body: result}, nil
}
