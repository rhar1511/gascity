package api

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/api/apierr"
	"github.com/gastownhall/gascity/internal/attemptevidence"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/storeref"
)

// ErrAttemptEvidenceReadAuthorizationUnavailable indicates that no read
// authorizer is configured for attempt-evidence requests.
var ErrAttemptEvidenceReadAuthorizationUnavailable = errors.New("attempt evidence read authorization is unavailable")

// ErrAttemptEvidenceReadDenied indicates that the caller lacks permission to
// read the requested attempt evidence.
var ErrAttemptEvidenceReadDenied = errors.New("attempt evidence read is not authorized")

// AttemptEvidenceReadAuthorizer proves that the request identity may read one
// exact repository/work/attempt scope. A city-scoped route does not grant
// access by itself. The API refuses reads when no implementation is composed.
type AttemptEvidenceReadAuthorizer interface {
	AuthorizeAttemptEvidenceRead(ctx context.Context, request attemptevidence.ReadAuthorizationRequest) error
}

// AttemptEvidenceReadAuthorizerProvider lets controller State override the
// built-in signed-scope authorizer without expanding the general State
// interface. An explicit provider returning nil disables archive reads.
type AttemptEvidenceReadAuthorizerProvider interface {
	AttemptEvidenceReadAuthorizer() AttemptEvidenceReadAuthorizer
}

type attemptEvidenceStore struct {
	// ref is the required logical source-work reference for a city/rig leg.
	ref   string
	store beads.Store
}

// attemptEvidencePlan uses the canonical residency topology to search for
// retained archives after the owner bead is deleted. The owner id alone cannot
// choose a store once that row is gone, so the search uses the resolver's full
// plan and later reads only its city/rig work legs; class bindings are not valid
// physical homes for work-attempt evidence. A refused topology fails closed.
func (s *Server) attemptEvidencePlan() (storeref.ResolvedPlan, error) {
	return storeref.Plan(storeref.Census{}, s.residencyTopology())
}

func (s *Server) attemptEvidenceReader() attemptevidence.Reader {
	if s.attemptEvidenceReaderPort != nil {
		return s.attemptEvidenceReaderPort
	}
	return attemptevidence.StoreReader{}
}

func (s *Server) requireAttemptEvidenceAuthorizer(ctx context.Context) (AttemptEvidenceReadAuthorizer, error) {
	if s.attemptEvidenceReadAuthorizer != nil {
		return s.attemptEvidenceReadAuthorizer, nil
	}
	// An explicit provider returning nil deliberately leaves reads disabled.
	if _, configured := s.state.(AttemptEvidenceReadAuthorizerProvider); configured {
		return nil, apierr.ServiceUnavailable.Msg(ErrAttemptEvidenceReadAuthorizationUnavailable.Error())
	}
	principal, ok := verifiedCityReadPrincipal(ctx)
	if !ok {
		return nil, apierr.ServiceUnavailable.Msg(ErrAttemptEvidenceReadAuthorizationUnavailable.Error())
	}
	if principal.City != s.state.CityName() || strings.TrimSpace(principal.Subject) == "" || len(principal.ReadScopes) == 0 {
		return nil, apierr.Forbidden.Msg(ErrAttemptEvidenceReadDenied.Error())
	}
	scopes := make(map[string]struct{}, len(principal.ReadScopes))
	for _, scope := range principal.ReadScopes {
		scopes[scope] = struct{}{}
	}
	return signedAttemptEvidenceReadAuthorizer{scopes: scopes}, nil
}

// signedAttemptEvidenceReadAuthorizer is constructed only after the request's
// city-read signature, reader, request digest and freshness have been checked.
// Its scopes come from the existing permission authority, never from query
// parameters or the current location of a mutable owner bead.
type signedAttemptEvidenceReadAuthorizer struct {
	scopes map[string]struct{}
}

func (a signedAttemptEvidenceReadAuthorizer) AuthorizeAttemptEvidenceRead(_ context.Context, request attemptevidence.ReadAuthorizationRequest) error {
	scope, err := attemptevidence.ReadGrantScope(request)
	if err != nil {
		return err
	}
	if _, ok := a.scopes[scope]; !ok {
		return ErrAttemptEvidenceReadDenied
	}
	return nil
}

func attemptEvidenceRefForLeg(leg storeref.Leg, cityName string) (string, bool) {
	switch {
	case leg.Ref == storeref.WorkRef:
		if strings.TrimSpace(cityName) == "" {
			return "", false
		}
		return "city:" + strings.TrimSpace(cityName), true
	case strings.HasPrefix(string(leg.Ref), "rig:"):
		return string(leg.Ref), true
	default:
		// Attempts are captured beside their city/rig work owner. A class
		// binding is never a valid physical home for one of these records.
		return "", false
	}
}

func (s *Server) exactAttemptEvidence(_ context.Context, ownerID, attemptID string) (attemptevidence.Evidence, error) {
	plan, err := s.attemptEvidencePlan()
	if err != nil {
		return attemptevidence.Evidence{}, apierr.ServiceUnavailable.Msg("attempt evidence residency is unavailable: " + err.Error())
	}
	reader := s.attemptEvidenceReader()
	var found *attemptevidence.Evidence
	var scopeErr error
	walk, walkErr := storeref.Walk(plan, func(leg storeref.Leg) (bool, error) {
		ref, workLeg := attemptEvidenceRefForLeg(leg, s.state.CityName())
		if !workLeg {
			return false, nil
		}
		candidate := attemptEvidenceStore{ref: ref, store: leg.Store}
		evidence, readErr := reader.Read(candidate.store, ownerID, attemptID)
		if errors.Is(readErr, attemptevidence.ErrNotFound) {
			return false, nil
		}
		if readErr != nil {
			return false, fmt.Errorf("read attempt evidence in %q: %w", leg.Ref, readErr)
		}
		if candidate.ref != "" && evidence.StoreRef != candidate.ref && s.knownAttemptEvidenceScopeRef(evidence.StoreRef) {
			return false, nil
		}
		if !s.attemptEvidenceScopeMatches(evidence, candidate.ref, ownerID, attemptID) {
			scopeErr = errors.New("attempt evidence provenance does not match its authoritative store")
			return true, nil
		}
		if found != nil {
			scopeErr = errors.New("attempt evidence exists in multiple stores")
			return true, nil
		}
		evidenceCopy := evidence
		found = &evidenceCopy
		return false, nil
	})
	if walkErr != nil {
		return attemptevidence.Evidence{}, apierr.ServiceUnavailable.Msg("attempt evidence read failed: " + walkErr.Error())
	}
	if scopeErr != nil {
		return attemptevidence.Evidence{}, apierr.ServiceUnavailable.Msg(scopeErr.Error())
	}
	if walk.Partial {
		return attemptevidence.Evidence{}, apierr.ServiceUnavailable.Msg("attempt evidence read was incomplete; one or more configured stores could not be checked")
	}
	if found == nil {
		return attemptevidence.Evidence{}, apierr.BeadNotFound.Msg("attempt evidence " + attemptID + " not found for work " + ownerID)
	}
	return *found, nil
}

func (s *Server) listAttemptEvidence(_ context.Context, ownerID string) ([]attemptevidence.Evidence, error) {
	plan, err := s.attemptEvidencePlan()
	if err != nil {
		return nil, apierr.ServiceUnavailable.Msg("attempt evidence residency is unavailable: " + err.Error())
	}
	reader := s.attemptEvidenceReader()
	byAttempt := make(map[string]attemptevidence.Evidence)
	var scopeErr error
	walk, walkErr := storeref.Walk(plan, func(leg storeref.Leg) (bool, error) {
		ref, workLeg := attemptEvidenceRefForLeg(leg, s.state.CityName())
		if !workLeg {
			return false, nil
		}
		candidate := attemptEvidenceStore{ref: ref, store: leg.Store}
		rows, listErr := reader.List(candidate.store, ownerID)
		if listErr != nil {
			return false, fmt.Errorf("list attempt evidence in %q: %w", leg.Ref, listErr)
		}
		for _, evidence := range rows {
			if candidate.ref != "" && evidence.StoreRef != candidate.ref && s.knownAttemptEvidenceScopeRef(evidence.StoreRef) {
				continue
			}
			if !s.attemptEvidenceScopeMatches(evidence, candidate.ref, ownerID, evidence.AttemptID) {
				scopeErr = errors.New("attempt evidence provenance does not match its authoritative store")
				return true, nil
			}
			if _, exists := byAttempt[evidence.AttemptID]; exists {
				scopeErr = errors.New("attempt evidence exists in multiple stores")
				return true, nil
			}
			byAttempt[evidence.AttemptID] = evidence
		}
		return false, nil
	})
	if walkErr != nil {
		return nil, apierr.ServiceUnavailable.Msg("attempt evidence list failed: " + walkErr.Error())
	}
	if scopeErr != nil {
		return nil, apierr.ServiceUnavailable.Msg(scopeErr.Error())
	}
	if walk.Partial {
		return nil, apierr.ServiceUnavailable.Msg("attempt evidence list was incomplete; one or more configured stores could not be checked")
	}
	out := make([]attemptevidence.Evidence, 0, len(byAttempt))
	for _, evidence := range byAttempt {
		out = append(out, evidence)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CapturedAt.Equal(out[j].CapturedAt) {
			return out[i].CapturedAt.Before(out[j].CapturedAt)
		}
		return out[i].AttemptID < out[j].AttemptID
	})
	return out, nil
}

func (s *Server) attemptEvidenceScopeMatches(evidence attemptevidence.Evidence, storeRef, ownerID, attemptID string) bool {
	if storeRef == "" {
		storeRef = evidence.StoreRef
	}
	return evidence.AttemptID == attemptID &&
		evidence.Identity.OwnerBeadID == ownerID &&
		evidence.StoreRef == storeRef &&
		s.knownAttemptEvidenceScopeRef(storeRef) &&
		evidence.Permission.StoreRef == storeRef &&
		evidence.Permission.WorkID == ownerID &&
		strings.TrimSpace(evidence.Permission.RepositoryRoot) != "" &&
		strings.TrimSpace(evidence.Permission.WorkspaceRoot) != ""
}

func (s *Server) knownAttemptEvidenceScopeRef(storeRef string) bool {
	if storeRef == "city:"+strings.TrimSpace(s.state.CityName()) {
		return true
	}
	if !strings.HasPrefix(storeRef, "rig:") {
		return false
	}
	name := strings.TrimPrefix(storeRef, "rig:")
	if strings.TrimSpace(name) == "" {
		return false
	}
	for _, rig := range s.residencyTopology().Rigs {
		if string(rig.Ref) == storeRef {
			return true
		}
	}
	return false
}

func (s *Server) authorizeAttemptEvidence(ctx context.Context, authorizer AttemptEvidenceReadAuthorizer, evidence attemptevidence.Evidence) error {
	if err := authorizer.AuthorizeAttemptEvidenceRead(ctx, attemptevidence.ReadAuthorizationRequest{
		Scope: evidence.Permission, AttemptID: evidence.AttemptID,
	}); err != nil {
		if errors.Is(err, ErrAttemptEvidenceReadDenied) {
			return apierr.Forbidden.Msg(ErrAttemptEvidenceReadDenied.Error())
		}
		return apierr.ServiceUnavailable.Msg(fmt.Sprintf("attempt evidence authorization failed: %v", err))
	}
	return nil
}

func publicAttemptEvidenceBead(b beads.Bead) (beads.Bead, bool) {
	if _, privatePayload := b.Metadata[beadmeta.AttemptEvidenceArchivePayloadMetadataKey]; privatePayload {
		return beads.Bead{}, false
	}
	for key := range b.Metadata {
		if isAttemptEvidenceMetadataKey(key) {
			if b.Metadata == nil {
				break
			}
			b.Metadata = maps.Clone(b.Metadata)
			break
		}
	}
	for key := range b.Metadata {
		if isAttemptEvidenceMetadataKey(key) {
			delete(b.Metadata, key)
		}
	}
	return b, true
}

func isAttemptEvidenceMetadataKey(key string) bool {
	return key == beadmeta.AttemptEvidenceArchiveAttemptIDMetadataKey ||
		key == beadmeta.AttemptEvidenceArchiveOwnerIDMetadataKey ||
		key == beadmeta.AttemptEvidenceArchivePayloadMetadataKey ||
		key == beadmeta.AttemptEvidenceArchiveDigestMetadataKey ||
		strings.HasPrefix(key, beadmeta.AttemptEvidenceIndexPrefix)
}

func validateAttemptEvidenceMetadata(metadata map[string]string) error {
	for key := range metadata {
		if isAttemptEvidenceMetadataKey(key) {
			return apierr.InvalidRequest.Msg("attempt-evidence metadata is controller-managed")
		}
	}
	return nil
}

func rejectAttemptEvidenceArchive(b beads.Bead) error {
	if beads.IsAttemptEvidenceArchive(b) {
		return apierr.Forbidden.Msg("immutable attempt-evidence archive rows cannot be accessed through generic bead operations")
	}
	return nil
}

// humaHandleAttemptEvidenceList exposes immutable attempts only after every
// returned record passes the composed exact-scope authorizer.
func (s *Server) humaHandleAttemptEvidenceList(ctx context.Context, input *AttemptEvidenceListInput) (*IndexOutput[[]attemptevidence.Evidence], error) {
	authorizer, err := s.requireAttemptEvidenceAuthorizer(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := s.listAttemptEvidence(ctx, input.ID)
	if err != nil {
		return nil, err
	}
	for _, evidence := range rows {
		if err := s.authorizeAttemptEvidence(ctx, authorizer, evidence); err != nil {
			return nil, err
		}
	}
	return &IndexOutput[[]attemptevidence.Evidence]{Index: s.latestIndex(), Body: rows}, nil
}

// humaHandleAttemptEvidenceGet reads one immutable execution attempt by its
// exact ID. It never resolves a latest-attempt alias.
func (s *Server) humaHandleAttemptEvidenceGet(ctx context.Context, input *AttemptEvidenceGetInput) (*IndexOutput[AttemptEvidenceRead], error) {
	authorizer, err := s.requireAttemptEvidenceAuthorizer(ctx)
	if err != nil {
		return nil, err
	}
	evidence, err := s.exactAttemptEvidence(ctx, input.ID, input.AttemptID)
	if err != nil {
		return nil, err
	}
	if err := s.authorizeAttemptEvidence(ctx, authorizer, evidence); err != nil {
		return nil, err
	}
	return &IndexOutput[AttemptEvidenceRead]{Index: s.latestIndex(), Body: AttemptEvidenceRead{Evidence: evidence, RelatedRecords: s.attemptRelatedRecords(evidence)}}, nil
}
