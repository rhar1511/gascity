package api

import (
	"context"
	"errors"
	"log"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/gastownhall/gascity/internal/api/apierr"
	"github.com/gastownhall/gascity/internal/qualification"
	"github.com/gastownhall/gascity/internal/retirementrelease"
)

// RetirementSourceProvider supplies trusted read-only verifier composition.
// Requests cannot install an authority, context loader, or verifier.
type RetirementSourceProvider interface {
	RetirementSourceAdapter() *retirementrelease.Adapter
}

// RetirementSourceInput carries retained evidence to the canonical verifiers.
type RetirementSourceInput struct {
	CityScope
	Body retirementrelease.Request
}

func (sm *SupervisorMux) registerCitySourceCompositionRoutes() {
	sm.registerCityHumanSourceRoutes()
	cityPost(sm, "/retirement-release/verify", (*Server).humaHandleRetirementSource,
		func(op *huma.Operation) {
			humanSourceOperation("verify-retirement-release-source", "Verify retained retirement source evidence", writeAuthHeader)(op)
			op.Description = "Read-only trusted composition of canonical authority APIs. Requires request-bound city-write authentication plus independently scoped permission and current context. Missing evidence contracts stay unavailable; this endpoint grants no execution or activation permit."
		}, errorStatuses(http.StatusBadRequest, http.StatusUnauthorized, http.StatusForbidden, http.StatusConflict, http.StatusRequestEntityTooLarge, http.StatusUnprocessableEntity, http.StatusServiceUnavailable))
}

func (s *Server) humaHandleRetirementSource(ctx context.Context, input *RetirementSourceInput) (*IndexOutput[retirementrelease.Verdict], error) {
	if err := s.requireHumanSourceWrite(ctx); err != nil {
		return nil, err
	}
	provider, ok := s.state.(RetirementSourceProvider)
	if !ok || provider.RetirementSourceAdapter() == nil {
		return nil, apierr.ServiceUnavailable.Msg("retirement source authority composition is disabled")
	}
	verdict, err := provider.RetirementSourceAdapter().Verify(ctx, input.Body)
	if err != nil {
		// Canonical verifier errors and composition callbacks receive bindings,
		// not a caller-selected verifier. Never log the request or retained bytes.
		log.Printf("retirement source verification failed: reason=%q: %v", verdict.Reason, err)
		return nil, retirementSourceAPIError(verdict, err)
	}
	return &IndexOutput[retirementrelease.Verdict]{Index: s.latestIndex(), Body: verdict}, nil
}

func retirementSourceAPIError(verdict retirementrelease.Verdict, err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return apierr.ServiceUnavailable.Msg("retirement source verification was interrupted")
	}
	if verdict.Status == "rejected" {
		return apierr.InvalidRequest.Msg("retirement source request rejected: " + verdict.Reason)
	}
	if errors.Is(err, qualification.ErrCompatibilityDenied) || verdict.Reason == "scoped_permission_denied" {
		return apierr.Forbidden.Msg("retirement source permission denied: " + verdict.Reason)
	}
	if verdict.Reason == "context_changed" || verdict.Reason == "trusted_context_mismatch" {
		return apierr.ConflictWrongState.Msg("retirement source context changed or does not match: " + verdict.Reason)
	}
	return apierr.ServiceUnavailable.Msg("retirement source verification unavailable: " + verdict.Reason)
}
