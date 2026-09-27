// Package decisionfrontier stores version-bound human decisions alongside
// their source work. It is deliberately inert without separately composed
// verifier and delivery ports.
package decisionfrontier

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

var (
	// ErrUnavailable reports that a required authority or store capability is missing.
	ErrUnavailable = errors.New("decision frontier is unavailable")
	// ErrAnswerVerifierUnavailable reports that no trusted answer verifier is configured.
	ErrAnswerVerifierUnavailable = errors.New("trusted human-answer verifier is unavailable")
	// ErrPromptDeliveryUnavailable reports that trusted prompt delivery is unavailable or unconfirmed.
	ErrPromptDeliveryUnavailable = errors.New("decision prompt delivery is unavailable")
	// ErrConflict reports that persisted frontier records conflict with the requested state.
	ErrConflict = errors.New("decision frontier conflicts with persisted state")
	// ErrStale reports that work or question state changed since the request was formed.
	ErrStale = errors.New("decision frontier references stale work or question state")
	// ErrInvalid reports that the proposed question map or answer request is invalid.
	ErrInvalid = errors.New("invalid decision frontier request")
	// ErrUnauthorized reports that the trusted verifier rejected the submitted answer.
	ErrUnauthorized = errors.New("decision answer is not authorized")
	// ErrAnswerInProgress reports that another answer transaction owns the ticket.
	ErrAnswerInProgress = errors.New("another answer is being recorded for this question")
)

const (
	frontierSchemaVersion = 1
	mapRecordKind         = "decision-frontier/map/v1"
	ticketRecordKind      = "decision-frontier/question/v1"
	answerRecordKind      = "decision-frontier/answer/v1"
	promptRecordKind      = "decision-frontier/prompt/v1"
	statePending          = "pending"
	stateResolved         = "resolved"
)

// Question is one exact human decision. Recommendations are advisory text;
// they are never copied into a resolution without a verified answer.
type Question struct {
	ID              string   `json:"id"`
	Title           string   `json:"title"`
	Prompt          string   `json:"prompt"`
	Recommendations []string `json:"recommendations,omitempty"`
	DependsOn       []string `json:"depends_on,omitempty"`
	SourceLinks     []string `json:"source_links,omitempty"`
}

// Proposal is a proposed independent question map for one exact source-work
// revision. SourceLinks preserves tracker/repository/issue references supplied
// by the authoritative intake path.
type Proposal struct {
	Questions   []Question        `json:"questions"`
	SourceLinks map[string]string `json:"source_links,omitempty"`
}

// Scope is resolved by the trusted controller from the owning city and
// physical store. A browser request cannot choose either authority boundary.
type Scope struct {
	CityRef  string `json:"city_ref"`
	StoreRef string `json:"store_ref"`
}

// State describes the persisted frontier state.
type State string

const (
	// StatePending means the frontier has one or more unresolved questions.
	StatePending State = "pending"
	// StateResolved means every required question has an accepted answer.
	StateResolved State = "resolved"
)

// Frontier is the current exact map and its question status for one work
// revision.
type Frontier struct {
	CityRef       string            `json:"city_ref"`
	StoreRef      string            `json:"store_ref"`
	MapID         string            `json:"map_id"`
	WorkID        string            `json:"work_id"`
	WorkRevision  string            `json:"work_revision"`
	WorkDigest    string            `json:"work_digest"`
	State         State             `json:"state"`
	Questions     []QuestionView    `json:"questions"`
	OpenQuestions []QuestionView    `json:"open_questions"`
	Prompt        PromptView        `json:"prompt"`
	SourceLinks   map[string]string `json:"source_links,omitempty"`
	proposalHash  string
}

// QuestionView joins immutable question text to its durable ticket state.
type QuestionView struct {
	ID              string      `json:"id"`
	TicketID        string      `json:"ticket_id"`
	Version         string      `json:"version"`
	Title           string      `json:"title"`
	Prompt          string      `json:"prompt"`
	Recommendations []string    `json:"recommendations,omitempty"`
	DependsOn       []string    `json:"depends_on,omitempty"`
	SourceLinks     []string    `json:"source_links,omitempty"`
	Status          string      `json:"status"`
	Answer          *AnswerView `json:"answer,omitempty"`
}

// AnswerView contains the exact signed human answer record. It contains no
// signing proof or bearer credential.
type AnswerView struct {
	ID         string     `json:"id"`
	Resolution Resolution `json:"resolution"`
	Text       string     `json:"text"`
	Subject    string     `json:"subject"`
	Issuer     string     `json:"issuer"`
	KeyID      string     `json:"key_id"`
	Digest     string     `json:"digest"`
}

// PromptView reports the durable prompt intent independently from answer
// status. The default service creates an intent but never sends it.
type PromptView struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
}

// Resolution is the human's explicit answer state. Only "answered" resolves a
// dependency; every other value remains open and keeps source work held.
type Resolution string

const (
	// ResolutionAnswered accepts the question and satisfies its dependencies.
	ResolutionAnswered Resolution = "answered"
	// ResolutionDeclined records an explicit refusal and keeps the question unresolved.
	ResolutionDeclined Resolution = "declined"
	// ResolutionDeferred records that the human postponed a decision.
	ResolutionDeferred Resolution = "deferred"
	// ResolutionUnclear records that the submitted answer did not resolve the question.
	ResolutionUnclear Resolution = "unclear"
	// ResolutionPartial records an incomplete answer that does not resolve the question.
	ResolutionPartial Resolution = "partial"
)

// AnswerSubmission carries the exact ticket/question revision and opaque
// signature envelope to a separately trusted verifier. Subject and authority
// are intentionally absent from caller-controlled fields.
type AnswerSubmission struct {
	TicketID        string     `json:"ticket_id"`
	WorkRevision    string     `json:"work_revision"`
	QuestionVersion string     `json:"question_version"`
	Resolution      Resolution `json:"resolution"`
	Text            string     `json:"text"`
	Proof           string     `json:"proof"`
}

// AnswerChallenge is the complete verifier scope. A verifier must authenticate
// a human principal and bind every field, including AnswerDigest.
type AnswerChallenge struct {
	CityRef         string     `json:"city_ref"`
	StoreRef        string     `json:"store_ref"`
	WorkID          string     `json:"work_id"`
	WorkRevision    string     `json:"work_revision"`
	WorkDigest      string     `json:"work_digest"`
	MapID           string     `json:"map_id"`
	TicketID        string     `json:"ticket_id"`
	QuestionID      string     `json:"question_id"`
	QuestionVersion string     `json:"question_version"`
	AnswerDigest    string     `json:"answer_digest"`
	Resolution      Resolution `json:"resolution"`
}

// VerifiedAnswer is returned only by an explicitly composed trust provider.
// The service independently compares its exact scope to the challenge.
type VerifiedAnswer struct {
	CityRef         string
	StoreRef        string
	KeyID           string
	Issuer          string
	Subject         string
	WorkID          string
	WorkRevision    string
	WorkDigest      string
	MapID           string
	TicketID        string
	QuestionVersion string
	AnswerDigest    string
	Resolution      Resolution
}

// AnswerVerifier verifies an authenticated human signature and authorization
// for one exact answer challenge. Implementations must reject worker identities
// and bind all challenge fields; a city-write identity is not sufficient.
type AnswerVerifier interface {
	VerifyDecisionAnswer(context.Context, AnswerChallenge, AnswerSubmission) (VerifiedAnswer, error)
}

// PromptRequest is the durable-intent identity passed to an optional delivery
// provider. It contains no guessed session ID.
type PromptRequest struct {
	CityRef      string
	StoreRef     string
	ID           string
	WorkID       string
	WorkRevision string
	MapID        string
	TicketIDs    []string
	SourceLinks  map[string]string
}

// PromptResult reports an observed delivery stage. DefinitivelyAbsent must be
// true only when the provider can prove that no external effect occurred.
type PromptResult struct {
	Status             string
	Reason             string
	DefinitivelyAbsent bool
}

// PromptDelivery must reconcile an uncertain request ID before retrying its
// external effect. Implementations must make request IDs idempotent where the
// delivery system supports it.
type PromptDelivery interface {
	DeliverDecisionPrompt(context.Context, PromptRequest) (PromptResult, error)
	ReconcileDecisionPrompt(context.Context, string) (PromptResult, error)
}

// Service owns domain transitions. Nil verifier and delivery ports are the
// default and fail closed: no answer can release work and no prompt is sent.
type Service struct {
	Verifier AnswerVerifier
	Delivery PromptDelivery
}

type mapRecord struct {
	SchemaVersion int               `json:"schema_version"`
	CityRef       string            `json:"city_ref"`
	StoreRef      string            `json:"store_ref"`
	WorkID        string            `json:"work_id"`
	WorkRevision  string            `json:"work_revision"`
	WorkDigest    string            `json:"work_digest"`
	MapID         string            `json:"map_id"`
	ProposalHash  string            `json:"proposal_hash"`
	Questions     []Question        `json:"questions"`
	SourceLinks   map[string]string `json:"source_links,omitempty"`
	PromptID      string            `json:"prompt_id"`
	ReservationID string            `json:"reservation_id"`
	ReleaseID     string            `json:"release_id"`
}

type ticketRecord struct {
	SchemaVersion int      `json:"schema_version"`
	CityRef       string   `json:"city_ref"`
	StoreRef      string   `json:"store_ref"`
	WorkID        string   `json:"work_id"`
	WorkRevision  string   `json:"work_revision"`
	WorkDigest    string   `json:"work_digest"`
	MapID         string   `json:"map_id"`
	Question      Question `json:"question"`
	Version       string   `json:"version"`
}

type answerRecord struct {
	SchemaVersion   int        `json:"schema_version"`
	CityRef         string     `json:"city_ref"`
	StoreRef        string     `json:"store_ref"`
	WorkID          string     `json:"work_id"`
	WorkRevision    string     `json:"work_revision"`
	WorkDigest      string     `json:"work_digest"`
	MapID           string     `json:"map_id"`
	TicketID        string     `json:"ticket_id"`
	QuestionID      string     `json:"question_id"`
	QuestionVersion string     `json:"question_version"`
	Resolution      Resolution `json:"resolution"`
	Text            string     `json:"text"`
	Digest          string     `json:"digest"`
	ProofDigest     string     `json:"proof_digest"`
	Subject         string     `json:"subject"`
	Issuer          string     `json:"issuer"`
	KeyID           string     `json:"key_id"`
}

type promptRecord struct {
	SchemaVersion int               `json:"schema_version"`
	CityRef       string            `json:"city_ref"`
	StoreRef      string            `json:"store_ref"`
	ID            string            `json:"id"`
	WorkID        string            `json:"work_id"`
	WorkRevision  string            `json:"work_revision"`
	WorkDigest    string            `json:"work_digest"`
	MapID         string            `json:"map_id"`
	TicketIDs     []string          `json:"ticket_ids"`
	SourceLinks   map[string]string `json:"source_links,omitempty"`
}

type sourceHold struct {
	SchemaVersion int    `json:"schema_version"`
	CityRef       string `json:"city_ref"`
	StoreRef      string `json:"store_ref"`
	WorkID        string `json:"work_id"`
	MapID         string `json:"map_id"`
	WorkRevision  string `json:"work_revision"`
	WorkDigest    string `json:"work_digest"`
	ProposalHash  string `json:"proposal_hash"`
	ReservationID string `json:"reservation_id"`
}

func validateScope(scope Scope) error {
	if err := validateCityRef(scope.CityRef); err != nil {
		return err
	}
	if err := validateStoreRef(scope.StoreRef); err != nil {
		return err
	}
	if strings.HasPrefix(scope.StoreRef, "city:") && scope.StoreRef != scope.CityRef {
		return fmt.Errorf("%w: city store reference must match its owning city", ErrInvalid)
	}
	return nil
}

func validateCityRef(ref string) error {
	if !strings.HasPrefix(ref, "city:") || strings.TrimSpace(ref) != ref ||
		strings.TrimSpace(strings.TrimPrefix(ref, "city:")) == "" ||
		strings.ContainsAny(strings.TrimPrefix(ref, "city:"), " \t\r\n") {
		return fmt.Errorf("%w: canonical owning-city reference is required", ErrInvalid)
	}
	return nil
}

func frontierMapID(scope Scope, workID, revision string) string {
	return beads.DecisionFrontierMapRecordID(scope.CityRef, scope.StoreRef, workID, revision)
}

func frontierQuestionID(scope Scope, mapID, questionID string) string {
	return beads.DecisionFrontierQuestionRecordID(scope.CityRef, scope.StoreRef, mapID, questionID)
}

func frontierPromptID(scope Scope, mapID string) string {
	return beads.DecisionFrontierPromptRecordID(scope.CityRef, scope.StoreRef, mapID)
}

func reservationReceiptID(scope Scope, mapID string) string {
	return stableID("frontier-transition", scope.CityRef, scope.StoreRef, mapID, "reserve")
}

func releaseReceiptID(scope Scope, mapID string) string {
	return stableID("frontier-transition", scope.CityRef, scope.StoreRef, mapID, "release")
}

func answerRecordID(scope Scope, ticketID, answerDigest string) string {
	return stableID("answer", scope.CityRef, scope.StoreRef, ticketID, answerDigest)
}

func canonicalHoldValue(scope Scope, workID, revision, workDigest, mapID, proposalHash string) string {
	data, _ := json.Marshal(sourceHold{
		SchemaVersion: frontierSchemaVersion, CityRef: scope.CityRef, StoreRef: scope.StoreRef,
		WorkID: workID, MapID: mapID, WorkRevision: revision, WorkDigest: workDigest,
		ProposalHash: proposalHash, ReservationID: reservationReceiptID(scope, mapID),
	})
	return string(data)
}

func proposalHashFromFrontier(frontier Frontier) string { return frontier.proposalHash }

func transitionReceipt(bead beads.Bead, id string) (beads.RevisionTransitionReceipt, bool, error) {
	raw := bead.Metadata[beadmeta.DecisionFrontierRevisionReceiptsMetadataKey]
	if strings.TrimSpace(raw) == "" {
		return beads.RevisionTransitionReceipt{}, false, nil
	}
	var receipts []beads.RevisionTransitionReceipt
	if err := json.Unmarshal([]byte(raw), &receipts); err != nil {
		return beads.RevisionTransitionReceipt{}, false, fmt.Errorf("decode decision-frontier source receipts: %w", ErrConflict)
	}
	for _, receipt := range receipts {
		if receipt.ID == id {
			return receipt, true, nil
		}
	}
	return beads.RevisionTransitionReceipt{}, false, nil
}

func receiptMatches(receipt beads.RevisionTransitionReceipt, scope Scope, workID, mapID, operation string, fromRevision int64) bool {
	return receipt.ID == map[bool]string{true: reservationReceiptID(scope, mapID), false: releaseReceiptID(scope, mapID)}[operation == "reserve"] &&
		receipt.CityRef == scope.CityRef && receipt.StoreRef == scope.StoreRef && receipt.WorkID == workID &&
		receipt.MapID == mapID && receipt.Operation == operation && receipt.FromRevision == fromRevision &&
		fromRevision != 0 && receipt.FromRevision != 0 && receipt.ToRevision != 0 && receipt.ToRevision != fromRevision
}

func validateFrontierSource(work beads.Bead, frontier Frontier, expectedMarker string) error {
	scope := Scope{CityRef: frontier.CityRef, StoreRef: frontier.StoreRef}
	if validateScope(scope) != nil || work.ID != frontier.WorkID || expectedMarker == "" {
		return ErrConflict
	}
	workDigest, err := WorkDigest(work)
	if err != nil || workDigest != frontier.WorkDigest {
		return ErrStale
	}
	baseRevision, err := canonicalWorkRevision(frontier.WorkRevision)
	if err != nil {
		return ErrConflict
	}
	currentRevision, err := WorkRevision(work)
	if err != nil {
		return ErrStale
	}
	marker := work.Metadata[beadmeta.DecisionFrontierHoldMetadataKey]
	reservationID := reservationReceiptID(scope, frontier.MapID)
	reserved, found, err := transitionReceipt(work, reservationID)
	if err != nil || !found || !receiptMatches(reserved, scope, frontier.WorkID, frontier.MapID, "reserve", baseRevision) {
		return ErrConflict
	}
	if marker == expectedMarker {
		var hold sourceHold
		if err := json.Unmarshal([]byte(marker), &hold); err != nil || hold.SchemaVersion != frontierSchemaVersion ||
			hold.CityRef != scope.CityRef || hold.StoreRef != scope.StoreRef || hold.WorkID != frontier.WorkID ||
			hold.MapID != frontier.MapID || hold.WorkRevision != frontier.WorkRevision || hold.WorkDigest != frontier.WorkDigest ||
			hold.ProposalHash != frontier.proposalHash || hold.ReservationID != reservationID ||
			currentRevision != strconv.FormatInt(reserved.ToRevision, 10) {
			return ErrStale
		}
		if _, found, err := transitionReceipt(work, releaseReceiptID(scope, frontier.MapID)); err != nil || found {
			return ErrConflict
		}
		return nil
	}
	if marker == "" && frontier.State == StateResolved {
		released, found, err := transitionReceipt(work, releaseReceiptID(scope, frontier.MapID))
		if err != nil || !found || !receiptMatches(released, scope, frontier.WorkID, frontier.MapID, "release", reserved.ToRevision) ||
			currentRevision != strconv.FormatInt(released.ToRevision, 10) {
			return ErrStale
		}
		return nil
	}
	return ErrStale
}

// WorkRevision returns the canonical signed decimal Beads revision. A zero or
// missing revision cannot support a durable decision frontier and fails closed.
func WorkRevision(work beads.Bead) (string, error) {
	if work.Revision == 0 {
		return "", fmt.Errorf("%w: source bead has no durable revision", ErrUnavailable)
	}
	return strconv.FormatInt(work.Revision, 10), nil
}

func canonicalWorkRevision(value string) (int64, error) {
	revision, err := strconv.ParseInt(value, 10, 64)
	if err != nil || revision == 0 || strconv.FormatInt(revision, 10) != value {
		return 0, fmt.Errorf("%w: work revision must be a canonical nonzero signed decimal", ErrInvalid)
	}
	return revision, nil
}

func validateStoreRef(storeRef string) error {
	kind, name, found := strings.Cut(storeRef, ":")
	if !found || (kind != "city" && kind != "rig") || name == "" ||
		strings.TrimSpace(storeRef) != storeRef || strings.TrimSpace(name) != name ||
		strings.ContainsAny(name, " \t\r\n") {
		return fmt.Errorf("%w: canonical server store reference is required", ErrInvalid)
	}
	return nil
}

// WorkDigest hashes source content separately from its store revision. The
// digest catches changes in status, ownership, holds and content even if a
// backend's revision projection is incomplete.
func WorkDigest(work beads.Bead) (string, error) {
	type revisionInput struct {
		ID                   string            `json:"id"`
		Type                 string            `json:"type"`
		Status               string            `json:"status"`
		Title                string            `json:"title"`
		Description          string            `json:"description"`
		ParentID             string            `json:"parent_id"`
		Ref                  string            `json:"ref"`
		From                 string            `json:"from"`
		Assignee             string            `json:"assignee"`
		ClaimFence           int64             `json:"claim_fence"`
		Priority             *int              `json:"priority,omitempty"`
		Labels               []string          `json:"labels"`
		Metadata             map[string]string `json:"metadata"`
		Needs                []string          `json:"needs"`
		Dependencies         []beads.Dep       `json:"dependencies"`
		DeferredUntil        *time.Time        `json:"defer_until,omitempty"`
		IsBlocked            *bool             `json:"is_blocked,omitempty"`
		IndefinitelyDeferred bool              `json:"indefinitely_deferred"`
		Ephemeral            bool              `json:"ephemeral"`
		NoHistory            bool              `json:"no_history"`
	}
	labels := make([]string, 0, len(work.Labels))
	labels = append(labels, work.Labels...)
	sort.Strings(labels)
	needs := append([]string(nil), work.Needs...)
	sort.Strings(needs)
	dependencies := append([]beads.Dep(nil), work.Dependencies...)
	sort.Slice(dependencies, func(i, j int) bool {
		if dependencies[i].Type != dependencies[j].Type {
			return dependencies[i].Type < dependencies[j].Type
		}
		if dependencies[i].DependsOnID != dependencies[j].DependsOnID {
			return dependencies[i].DependsOnID < dependencies[j].DependsOnID
		}
		return dependencies[i].IssueID < dependencies[j].IssueID
	})
	metadata := make(map[string]string, len(work.Metadata))
	for key, value := range work.Metadata {
		if key == beadmeta.DecisionFrontierHoldMetadataKey || key == beadmeta.DecisionFrontierRevisionReceiptsMetadataKey {
			continue
		}
		metadata[key] = value
	}
	data, err := json.Marshal(revisionInput{
		ID: work.ID, Type: work.Type, Status: work.Status, Title: work.Title, Description: work.Description,
		ParentID: work.ParentID, Ref: work.Ref, From: work.From, Assignee: work.Assignee, ClaimFence: work.ClaimFence,
		Priority: work.Priority, Labels: labels, Metadata: metadata, Needs: needs, Dependencies: dependencies,
		DeferredUntil: work.DeferUntil, IsBlocked: work.IsBlocked, IndefinitelyDeferred: work.IndefinitelyDeferred,
		Ephemeral: work.Ephemeral, NoHistory: work.NoHistory,
	})
	if err != nil {
		return "", fmt.Errorf("encode work digest: %w", err)
	}
	return digest(data), nil
}

// AnswerDigest identifies the exact resolution and human text. The proof is
// excluded because it authenticates this value rather than defining it.
func AnswerDigest(resolution Resolution, text string) string {
	data, _ := json.Marshal(struct {
		SchemaVersion int        `json:"schema_version"`
		Resolution    Resolution `json:"resolution"`
		Text          string     `json:"text"`
	}{frontierSchemaVersion, resolution, strings.TrimSpace(text)})
	return digest(data)
}

// Ensure idempotently creates the map, tickets, dependencies, and one durable
// prompt intent after it first reserves the source work with a CAS marker.
// storeRef must be resolved by the trusted controller, not copied from a
// request body.
func (s Service) Ensure(ctx context.Context, store beads.Store, scope Scope, workID, expectedRevision string, proposal Proposal) (Frontier, error) {
	if err := ctx.Err(); err != nil {
		return Frontier{}, err
	}
	if err := validateScope(scope); err != nil {
		return Frontier{}, err
	}
	if _, err := canonicalWorkRevision(expectedRevision); err != nil {
		return Frontier{}, err
	}
	recordWriter, transitionWriter, sourceReader, err := requireStoreCapabilities(store)
	if err != nil {
		return Frontier{}, err
	}
	work, err := sourceReader.DecisionFrontierSourceSnapshot(workID)
	if err != nil {
		return Frontier{}, fmt.Errorf("read decision source %q: %w", workID, err)
	}
	actualRevision, err := WorkRevision(work)
	if err != nil {
		return Frontier{}, err
	}
	questions, proposalHash, err := normalizeProposal(proposal)
	if err != nil {
		return Frontier{}, err
	}
	mapID := frontierMapID(scope, workID, expectedRevision)
	workDigest, err := WorkDigest(work)
	if err != nil {
		return Frontier{}, err
	}
	markerValue := canonicalHoldValue(scope, workID, expectedRevision, workDigest, mapID, proposalHash)
	markerPresent := work.Metadata[beadmeta.DecisionFrontierHoldMetadataKey]
	if existing, getErr := store.Get(mapID); getErr == nil {
		var existingDoc mapRecord
		if existing.Metadata[beadmeta.DecisionFrontierRecordMetadataKey] != mapRecordKind ||
			json.Unmarshal([]byte(existing.Description), &existingDoc) != nil ||
			existingDoc.SchemaVersion != frontierSchemaVersion || existingDoc.CityRef != scope.CityRef || existingDoc.StoreRef != scope.StoreRef ||
			existingDoc.WorkID != workID || existingDoc.WorkRevision != expectedRevision ||
			existingDoc.MapID != mapID || existingDoc.WorkDigest == "" || existingDoc.ProposalHash != proposalHash ||
			existingDoc.ReservationID != reservationReceiptID(scope, mapID) || existingDoc.ReleaseID != releaseReceiptID(scope, mapID) ||
			existingDoc.PromptID != frontierPromptID(scope, mapID) {
			return Frontier{}, ErrConflict
		}
		_, persistedHash, hashErr := normalizeProposal(Proposal{Questions: existingDoc.Questions, SourceLinks: existingDoc.SourceLinks})
		if hashErr != nil || persistedHash != proposalHash {
			return Frontier{}, ErrConflict
		}
		if workDigest != existingDoc.WorkDigest {
			return Frontier{}, ErrStale
		}
		markerValue = canonicalHoldValue(scope, workID, expectedRevision, existingDoc.WorkDigest, mapID, existingDoc.ProposalHash)
		markerPresent = work.Metadata[beadmeta.DecisionFrontierHoldMetadataKey]
		if markerPresent != "" {
			if markerPresent != markerValue {
				return Frontier{}, ErrStale
			}
			if err := validatePendingReservation(work, scope, workID, expectedRevision, existingDoc.WorkDigest, mapID, existingDoc.ProposalHash, markerValue); err != nil {
				return Frontier{}, err
			}
			if err := ensureFrontierRecords(recordWriter, store, existingDoc); err != nil {
				return Frontier{}, err
			}
		}
		frontier, loadErr := loadFrontier(store, scope, mapID)
		if loadErr != nil {
			return Frontier{}, loadErr
		}
		work, err = sourceReader.DecisionFrontierSourceSnapshot(workID)
		if err != nil {
			return Frontier{}, fmt.Errorf("re-read decision source %q: %w", workID, err)
		}
		workDigest, err = WorkDigest(work)
		if err != nil || workDigest != existingDoc.WorkDigest {
			return Frontier{}, ErrStale
		}
		if err := validateFrontierSource(work, frontier, markerValue); err != nil {
			return Frontier{}, err
		}
		if frontier.State == StateResolved && markerPresent != "" {
			if err := releaseSource(transitionWriter, sourceReader, scope, workID, expectedRevision, workDigest, mapID, markerValue); err != nil {
				return Frontier{}, err
			}
		}
		if s.Delivery != nil {
			promptDoc := promptRecord{
				SchemaVersion: frontierSchemaVersion, CityRef: scope.CityRef, StoreRef: scope.StoreRef, ID: existingDoc.PromptID,
				WorkID: workID, WorkRevision: expectedRevision, WorkDigest: existingDoc.WorkDigest,
				MapID: mapID, TicketIDs: ticketIDsFor(scope, mapID, existingDoc.Questions),
				SourceLinks: cloneStringMap(existingDoc.SourceLinks),
			}
			if err := s.advancePromptDelivery(ctx, store, promptDoc); err != nil {
				return frontier, err
			}
		}
		frontier, loadErr = loadFrontier(store, scope, mapID)
		if loadErr != nil {
			return Frontier{}, loadErr
		}
		return frontier, nil
	} else if !errors.Is(getErr, beads.ErrNotFound) {
		return Frontier{}, fmt.Errorf("check existing decision map %s: %w", mapID, getErr)
	}
	if markerPresent == "" {
		if actualRevision != expectedRevision {
			return Frontier{}, ErrStale
		}
	} else {
		if markerPresent != markerValue {
			return Frontier{}, ErrStale
		}
		if err := validatePendingReservation(work, scope, workID, expectedRevision, workDigest, mapID, proposalHash, markerValue); err != nil {
			return Frontier{}, err
		}
	}
	promptID := frontierPromptID(scope, mapID)
	if markerPresent == "" {
		if _, err := reserveSource(transitionWriter, sourceReader, scope, work, expectedRevision, workDigest, mapID, markerValue); err != nil {
			return Frontier{}, err
		}
	}
	work, err = sourceReader.DecisionFrontierSourceSnapshot(workID)
	if err != nil {
		return Frontier{}, fmt.Errorf("re-read reserved decision source %q: %w", workID, err)
	}
	if err := validatePendingReservation(work, scope, workID, expectedRevision, workDigest, mapID, proposalHash, markerValue); err != nil {
		return Frontier{}, err
	}
	reservationID := reservationReceiptID(scope, mapID)
	releaseID := releaseReceiptID(scope, mapID)
	mapDoc := mapRecord{
		SchemaVersion: frontierSchemaVersion, CityRef: scope.CityRef, StoreRef: scope.StoreRef, WorkID: workID,
		WorkRevision: expectedRevision, WorkDigest: workDigest, MapID: mapID,
		ProposalHash: proposalHash, Questions: questions, SourceLinks: cloneStringMap(proposal.SourceLinks), PromptID: promptID,
		ReservationID: reservationID, ReleaseID: releaseID,
	}
	promptDoc := promptRecord{
		SchemaVersion: frontierSchemaVersion, CityRef: scope.CityRef, StoreRef: scope.StoreRef, ID: promptID, WorkID: workID,
		WorkRevision: expectedRevision, WorkDigest: workDigest, MapID: mapID, TicketIDs: ticketIDsFor(scope, mapID, questions),
		SourceLinks: cloneStringMap(proposal.SourceLinks),
	}
	if err := ensureFrontierRecords(recordWriter, store, mapDoc); err != nil {
		return Frontier{}, err
	}
	frontier, err := loadFrontier(store, scope, mapID)
	if err != nil {
		return Frontier{}, err
	}
	if frontier.State == StateResolved {
		if err := finalizeFrontier(recordWriter, transitionWriter, sourceReader, store, scope, workID, expectedRevision, workDigest, mapID, markerValue); err != nil {
			return Frontier{}, err
		}
	}
	if s.Delivery != nil {
		if err := s.advancePromptDelivery(ctx, store, promptDoc); err != nil {
			return frontier, err
		}
		frontier, err = loadFrontier(store, scope, mapID)
		if err != nil {
			return Frontier{}, err
		}
	}
	return frontier, nil
}

// Read returns the exact current map for the source work revision without
// creating work, changing state, or invoking a delivery adapter.
func (Service) Read(ctx context.Context, store beads.Store, scope Scope, workID, expectedRevision string) (Frontier, error) {
	if err := ctx.Err(); err != nil {
		return Frontier{}, err
	}
	if err := validateScope(scope); err != nil {
		return Frontier{}, err
	}
	if _, err := canonicalWorkRevision(expectedRevision); err != nil {
		return Frontier{}, err
	}
	sourceReader, ok := beads.DecisionFrontierSourceReaderFor(store)
	if !ok || sourceReader == nil {
		return Frontier{}, fmt.Errorf("%w: authoritative decision-source snapshots are required", ErrUnavailable)
	}
	work, err := sourceReader.DecisionFrontierSourceSnapshot(workID)
	if err != nil {
		return Frontier{}, err
	}
	mapID := frontierMapID(scope, workID, expectedRevision)
	frontier, err := loadFrontier(store, scope, mapID)
	if err != nil {
		return Frontier{}, err
	}
	if frontier.CityRef != scope.CityRef || frontier.StoreRef != scope.StoreRef || frontier.WorkID != workID || frontier.WorkRevision != expectedRevision {
		return Frontier{}, ErrConflict
	}
	marker := canonicalHoldValue(scope, workID, expectedRevision, frontier.WorkDigest, mapID, proposalHashFromFrontier(frontier))
	if err := validateFrontierSource(work, frontier, marker); err != nil {
		return Frontier{}, err
	}
	return frontier, nil
}

// Answer accepts one signed human answer for the current exact question. An
// absent verifier refuses before writing any answer or changing ticket state.
func (s Service) Answer(ctx context.Context, store beads.Store, scope Scope, workID string, submission AnswerSubmission) (Frontier, error) {
	if s.Verifier == nil {
		return Frontier{}, ErrAnswerVerifierUnavailable
	}
	if err := ctx.Err(); err != nil {
		return Frontier{}, err
	}
	if err := validateScope(scope); err != nil {
		return Frontier{}, err
	}
	if _, err := canonicalWorkRevision(submission.WorkRevision); err != nil {
		return Frontier{}, err
	}
	recordWriter, transitionWriter, sourceReader, err := requireStoreCapabilities(store)
	if err != nil {
		return Frontier{}, err
	}
	work, err := sourceReader.DecisionFrontierSourceSnapshot(workID)
	if err != nil {
		return Frontier{}, err
	}
	mapID := frontierMapID(scope, workID, submission.WorkRevision)
	frontier, err := loadFrontier(store, scope, mapID)
	if err != nil {
		return Frontier{}, err
	}
	markerValue := canonicalHoldValue(scope, workID, submission.WorkRevision, frontier.WorkDigest, mapID, proposalHashFromFrontier(frontier))
	if err := validateFrontierSource(work, frontier, markerValue); err != nil {
		return Frontier{}, err
	}
	if frontier.WorkID != workID || frontier.WorkRevision != submission.WorkRevision ||
		frontier.CityRef != scope.CityRef || frontier.StoreRef != scope.StoreRef {
		return Frontier{}, ErrStale
	}
	view, ok := questionTicket(frontier, submission.TicketID)
	if !ok {
		return Frontier{}, ErrInvalid
	}
	if view.Version != submission.QuestionVersion {
		return Frontier{}, ErrStale
	}
	if !validResolution(submission.Resolution) || strings.TrimSpace(submission.Text) == "" || strings.TrimSpace(submission.Proof) == "" {
		return Frontier{}, ErrInvalid
	}
	digestValue := AnswerDigest(submission.Resolution, submission.Text)
	challenge := AnswerChallenge{
		CityRef: scope.CityRef, StoreRef: scope.StoreRef, WorkID: workID, WorkRevision: submission.WorkRevision,
		WorkDigest: frontier.WorkDigest, MapID: mapID,
		TicketID: submission.TicketID, QuestionID: view.ID, QuestionVersion: view.Version,
		AnswerDigest: digestValue, Resolution: submission.Resolution,
	}
	verified, err := s.Verifier.VerifyDecisionAnswer(ctx, challenge, submission)
	if err != nil {
		return Frontier{}, fmt.Errorf("verify signed human answer: %w", errors.Join(ErrUnauthorized, err))
	}
	if !verifiedAnswerMatches(verified, challenge) {
		return Frontier{}, ErrUnauthorized
	}
	answerID := answerRecordID(scope, submission.TicketID, digestValue)
	answerDoc := answerRecord{
		SchemaVersion: frontierSchemaVersion, CityRef: scope.CityRef, StoreRef: scope.StoreRef, WorkID: workID,
		WorkRevision: submission.WorkRevision, WorkDigest: frontier.WorkDigest,
		MapID: mapID, TicketID: submission.TicketID, QuestionID: view.ID, QuestionVersion: view.Version,
		Resolution: submission.Resolution, Text: strings.TrimSpace(submission.Text), Digest: digestValue,
		ProofDigest: digest([]byte(submission.Proof)), Subject: verified.Subject, Issuer: verified.Issuer, KeyID: verified.KeyID,
	}
	if !questionIsOpen(frontier, view.ID) {
		if view.Status == "answered" && view.Answer != nil && view.Answer.ID == answerID && view.Answer.Digest == digestValue {
			if err := verifyAnswerRecord(store, answerID, answerDoc); err != nil {
				return Frontier{}, err
			}
			if frontier.State == StateResolved {
				if work.Metadata[beadmeta.DecisionFrontierHoldMetadataKey] != "" {
					if err := finalizeFrontier(recordWriter, transitionWriter, sourceReader, store, scope, workID, submission.WorkRevision, frontier.WorkDigest, mapID, markerValue); err != nil {
						return Frontier{}, err
					}
				}
			}
			return frontier, nil
		}
		return Frontier{}, ErrStale
	}
	if work.Metadata[beadmeta.DecisionFrontierHoldMetadataKey] == "" {
		return Frontier{}, ErrStale
	}
	if err := persistAnswerOnce(ctx, recordWriter, store, submission.TicketID, answerID, answerDoc, submission.Resolution); err != nil {
		return Frontier{}, err
	}
	frontier, err = loadFrontier(store, scope, mapID)
	if err != nil {
		return Frontier{}, err
	}
	if frontier.State == StateResolved {
		if err := finalizeFrontier(recordWriter, transitionWriter, sourceReader, store, scope, workID, submission.WorkRevision, frontier.WorkDigest, mapID, markerValue); err != nil {
			return Frontier{}, err
		}
	}
	return frontier, nil
}

func requireStoreCapabilities(store beads.Store) (beads.DecisionFrontierRecordWriter, beads.RevisionTransitionWriter, beads.DecisionFrontierSourceReader, error) {
	if store == nil || !beads.StableCreateIDFor(store) {
		return nil, nil, nil, fmt.Errorf("%w: durable stable-ID creation is required", ErrUnavailable)
	}
	writer, ok := beads.DecisionFrontierRecordWriterFor(store)
	if !ok || writer == nil {
		return nil, nil, nil, fmt.Errorf("%w: trusted decision-record writes are required", ErrUnavailable)
	}
	transitionWriter, ok := beads.RevisionTransitionWriterFor(store)
	if !ok || transitionWriter == nil {
		return nil, nil, nil, fmt.Errorf("%w: atomic revision-bound transition receipts are required", ErrUnavailable)
	}
	sourceReader, ok := beads.DecisionFrontierSourceReaderFor(store)
	if !ok || sourceReader == nil {
		return nil, nil, nil, fmt.Errorf("%w: authoritative decision-source snapshots are required", ErrUnavailable)
	}
	return writer, transitionWriter, sourceReader, nil
}

func validatePendingReservation(work beads.Bead, scope Scope, workID, revision, workDigest, mapID, proposalHash, marker string) error {
	currentRevision, err := WorkRevision(work)
	if err != nil || work.Metadata[beadmeta.DecisionFrontierHoldMetadataKey] != marker {
		return ErrStale
	}
	currentDigest, err := WorkDigest(work)
	if err != nil || currentDigest != workDigest {
		return ErrStale
	}
	baseRevision, err := canonicalWorkRevision(revision)
	if err != nil {
		return ErrConflict
	}
	receipt, found, err := transitionReceipt(work, reservationReceiptID(scope, mapID))
	if err != nil || !found || !receiptMatches(receipt, scope, workID, mapID, "reserve", baseRevision) ||
		currentRevision != strconv.FormatInt(receipt.ToRevision, 10) {
		return ErrStale
	}
	var hold sourceHold
	if err := json.Unmarshal([]byte(marker), &hold); err != nil || hold.SchemaVersion != frontierSchemaVersion ||
		hold.CityRef != scope.CityRef || hold.StoreRef != scope.StoreRef || hold.WorkID != workID || hold.MapID != mapID ||
		hold.WorkRevision != revision || hold.WorkDigest != workDigest || hold.ProposalHash != proposalHash ||
		hold.ReservationID != receipt.ID {
		return ErrConflict
	}
	if _, found, err := transitionReceipt(work, releaseReceiptID(scope, mapID)); err != nil || found {
		return ErrConflict
	}
	return nil
}

func reserveSource(writer beads.RevisionTransitionWriter, sourceReader beads.DecisionFrontierSourceReader, scope Scope, work beads.Bead, revision, workDigest, mapID, marker string) (beads.RevisionTransitionReceipt, error) {
	baseRevision, err := canonicalWorkRevision(revision)
	if err != nil {
		return beads.RevisionTransitionReceipt{}, err
	}
	receipt := beads.RevisionTransitionReceipt{
		ID: reservationReceiptID(scope, mapID), CityRef: scope.CityRef,
		StoreRef: scope.StoreRef, WorkID: work.ID, MapID: mapID, Operation: "reserve", FromRevision: baseRevision,
	}
	if work.Metadata[beadmeta.DecisionFrontierHoldMetadataKey] == "" {
		_, won, transitionErr := writer.CompareAndSetMetadataKeyWithReceipt(work.ID,
			beadmeta.DecisionFrontierHoldMetadataKey, "", marker, baseRevision, receipt)
		if transitionErr != nil && !won {
			latest, readErr := sourceReader.DecisionFrontierSourceSnapshot(work.ID)
			if readErr != nil {
				return beads.RevisionTransitionReceipt{}, errors.Join(fmt.Errorf("reserve decision source: %w", transitionErr), readErr)
			}
			if err := validatePendingReservation(latest, scope, work.ID, revision, workDigest, mapID, holdProposalHash(marker), marker); err != nil {
				return beads.RevisionTransitionReceipt{}, errors.Join(fmt.Errorf("reserve decision source: %w", transitionErr), err)
			}
			actual, found, readErr := transitionReceipt(latest, receipt.ID)
			if readErr != nil || !found {
				return beads.RevisionTransitionReceipt{}, errors.Join(ErrConflict, readErr)
			}
			return actual, nil
		}
		if !won {
			latest, readErr := sourceReader.DecisionFrontierSourceSnapshot(work.ID)
			if readErr != nil {
				return beads.RevisionTransitionReceipt{}, readErr
			}
			if err := validatePendingReservation(latest, scope, work.ID, revision, workDigest, mapID, holdProposalHash(marker), marker); err != nil {
				return beads.RevisionTransitionReceipt{}, ErrConflict
			}
		}
	}
	latest, err := sourceReader.DecisionFrontierSourceSnapshot(work.ID)
	if err != nil {
		return beads.RevisionTransitionReceipt{}, fmt.Errorf("read reserved decision source: %w", err)
	}
	if err := validatePendingReservation(latest, scope, work.ID, revision, workDigest, mapID, holdProposalHash(marker), marker); err != nil {
		return beads.RevisionTransitionReceipt{}, err
	}
	actual, found, err := transitionReceipt(latest, receipt.ID)
	if err != nil || !found {
		return beads.RevisionTransitionReceipt{}, ErrConflict
	}
	return actual, nil
}

func holdProposalHash(marker string) string {
	var hold sourceHold
	if json.Unmarshal([]byte(marker), &hold) != nil {
		return ""
	}
	return hold.ProposalHash
}

func releaseSource(writer beads.RevisionTransitionWriter, sourceReader beads.DecisionFrontierSourceReader, scope Scope, workID, revision, workDigest, mapID, marker string) error {
	baseRevision, err := canonicalWorkRevision(revision)
	if err != nil {
		return err
	}
	work, err := sourceReader.DecisionFrontierSourceSnapshot(workID)
	if err != nil {
		return fmt.Errorf("re-read source before decision release: %w", err)
	}
	currentDigest, err := WorkDigest(work)
	if err != nil || currentDigest != workDigest {
		return ErrStale
	}
	reserved, found, err := transitionReceipt(work, reservationReceiptID(scope, mapID))
	if err != nil || !found || !receiptMatches(reserved, scope, workID, mapID, "reserve", baseRevision) {
		return ErrConflict
	}
	currentRevision, err := WorkRevision(work)
	if err != nil {
		return ErrStale
	}
	if work.Metadata[beadmeta.DecisionFrontierHoldMetadataKey] == "" {
		released, found, readErr := transitionReceipt(work, releaseReceiptID(scope, mapID))
		if readErr == nil && found && receiptMatches(released, scope, workID, mapID, "release", reserved.ToRevision) &&
			currentRevision == strconv.FormatInt(released.ToRevision, 10) {
			return nil
		}
		return ErrStale
	}
	if err := validatePendingReservation(work, scope, workID, revision, workDigest, mapID, holdProposalHash(marker), marker); err != nil {
		return err
	}
	receipt := beads.RevisionTransitionReceipt{
		ID: releaseReceiptID(scope, mapID), CityRef: scope.CityRef,
		StoreRef: scope.StoreRef, WorkID: workID, MapID: mapID, Operation: "release", FromRevision: reserved.ToRevision,
	}
	_, won, transitionErr := writer.CompareAndSetMetadataKeyWithReceipt(workID,
		beadmeta.DecisionFrontierHoldMetadataKey, marker, "", reserved.ToRevision, receipt)
	if transitionErr != nil && !won {
		latest, readErr := sourceReader.DecisionFrontierSourceSnapshot(workID)
		if readErr != nil {
			return errors.Join(fmt.Errorf("release decision source: %w", transitionErr), readErr)
		}
		if err := verifyReleasedSource(latest, scope, workID, workDigest, mapID, reserved); err != nil {
			return errors.Join(fmt.Errorf("release decision source: %w", transitionErr), err)
		}
		return nil
	}
	if !won {
		return ErrStale
	}
	latest, err := sourceReader.DecisionFrontierSourceSnapshot(workID)
	if err != nil {
		return err
	}
	return verifyReleasedSource(latest, scope, workID, workDigest, mapID, reserved)
}

func verifyReleasedSource(work beads.Bead, scope Scope, workID, workDigest, mapID string, reserved beads.RevisionTransitionReceipt) error {
	currentRevision, err := WorkRevision(work)
	if err != nil {
		return ErrStale
	}
	currentDigest, err := WorkDigest(work)
	if err != nil || currentDigest != workDigest || work.Metadata[beadmeta.DecisionFrontierHoldMetadataKey] != "" {
		return ErrStale
	}
	released, found, err := transitionReceipt(work, releaseReceiptID(scope, mapID))
	if err != nil || !found || !receiptMatches(released, scope, workID, mapID, "release", reserved.ToRevision) ||
		currentRevision != strconv.FormatInt(released.ToRevision, 10) {
		return ErrStale
	}
	return nil
}

func finalizeFrontier(writer beads.DecisionFrontierRecordWriter, transitionWriter beads.RevisionTransitionWriter, sourceReader beads.DecisionFrontierSourceReader, store beads.Store, scope Scope, workID, revision, workDigest, mapID, marker string) error {
	if err := markMapResolved(writer, store, mapID); err != nil {
		return err
	}
	return releaseSource(transitionWriter, sourceReader, scope, workID, revision, workDigest, mapID, marker)
}

func ensureImmutableRecord(writer beads.DecisionFrontierRecordWriter, store beads.Store, id, title, kind, initialState string, doc any) error {
	body, err := json.Marshal(doc)
	if err != nil {
		return fmt.Errorf("encode decision record %s: %w", id, err)
	}
	_, err = writer.CreateDecisionFrontierRecord(beads.Bead{
		ID: id, Type: "gate", Title: title, Description: string(body),
		Metadata: beads.StringMap{
			beadmeta.DecisionFrontierRecordMetadataKey: kind,
			beadmeta.DecisionFrontierStateMetadataKey:  initialState,
		},
	})
	if err == nil {
		return nil
	}
	existing, getErr := store.Get(id)
	if getErr != nil {
		return fmt.Errorf("create decision record %s: %w (read after create failure: %w)", id, err, getErr)
	}
	if existing.ID != id || existing.Type != "gate" || existing.Title != title || existing.Description != string(body) ||
		existing.Metadata[beadmeta.DecisionFrontierRecordMetadataKey] != kind ||
		!validStoredRecordState(kind, initialState, existing.Metadata[beadmeta.DecisionFrontierStateMetadataKey]) {
		return fmt.Errorf("%w: persisted record %s does not match requested %s", ErrConflict, id, kind)
	}
	for key := range existing.Metadata {
		if key == beadmeta.DecisionFrontierRecordMetadataKey || key == beadmeta.DecisionFrontierStateMetadataKey {
			continue
		}
		if key == beadmeta.DecisionFrontierReasonMetadataKey && kind == promptRecordKind {
			continue
		}
		return fmt.Errorf("%w: persisted record %s has unexpected metadata", ErrConflict, id)
	}
	return nil
}

// ensureFrontierRecords converges every immutable row and dependency from the
// exact persisted map document. It only creates absent rows; a conflicting
// existing row or dependency aborts without being overwritten.
func ensureFrontierRecords(writer beads.DecisionFrontierRecordWriter, store beads.Store, mapDoc mapRecord) error {
	scope := Scope{CityRef: mapDoc.CityRef, StoreRef: mapDoc.StoreRef}
	_, proposalHash, err := normalizeProposal(Proposal{Questions: mapDoc.Questions, SourceLinks: mapDoc.SourceLinks})
	if err != nil || proposalHash != mapDoc.ProposalHash || mapDoc.SchemaVersion != frontierSchemaVersion ||
		mapDoc.MapID != frontierMapID(scope, mapDoc.WorkID, mapDoc.WorkRevision) ||
		mapDoc.PromptID != frontierPromptID(scope, mapDoc.MapID) || mapDoc.ReservationID != reservationReceiptID(scope, mapDoc.MapID) ||
		mapDoc.ReleaseID != releaseReceiptID(scope, mapDoc.MapID) {
		return ErrConflict
	}
	if err := ensureImmutableRecord(writer, store, mapDoc.MapID, "Decision map for "+mapDoc.WorkID,
		mapRecordKind, statePending, mapDoc); err != nil {
		return err
	}
	ticketIDs := ticketIDsFor(scope, mapDoc.MapID, mapDoc.Questions)
	for i, question := range mapDoc.Questions {
		version, err := questionVersion(question)
		if err != nil {
			return err
		}
		doc := ticketRecord{
			SchemaVersion: frontierSchemaVersion, CityRef: mapDoc.CityRef, StoreRef: mapDoc.StoreRef,
			WorkID: mapDoc.WorkID, WorkRevision: mapDoc.WorkRevision, WorkDigest: mapDoc.WorkDigest,
			MapID: mapDoc.MapID, Question: question, Version: version,
		}
		if err := ensureImmutableRecord(writer, store, ticketIDs[i], question.Title, ticketRecordKind, statePending, doc); err != nil {
			return err
		}
	}
	prompt := promptRecord{
		SchemaVersion: frontierSchemaVersion, CityRef: mapDoc.CityRef, StoreRef: mapDoc.StoreRef,
		ID: mapDoc.PromptID, WorkID: mapDoc.WorkID, WorkRevision: mapDoc.WorkRevision, WorkDigest: mapDoc.WorkDigest,
		MapID: mapDoc.MapID, TicketIDs: append([]string(nil), ticketIDs...), SourceLinks: cloneStringMap(mapDoc.SourceLinks),
	}
	if err := ensureImmutableRecord(writer, store, mapDoc.PromptID, "Decision prompt intent for "+mapDoc.WorkID,
		promptRecordKind, "unconfigured", prompt); err != nil {
		return err
	}
	// All immutable endpoint records must exist before any graph link is
	// written. A retry can then safely converge either a partial create or an
	// edge that committed before its response was lost.
	for i, question := range mapDoc.Questions {
		for _, prerequisite := range question.DependsOn {
			prerequisiteIndex := indexQuestion(mapDoc.Questions, prerequisite)
			if prerequisiteIndex < 0 {
				return ErrConflict
			}
			prerequisiteID := ticketIDs[prerequisiteIndex]
			if err := ensureDecisionDependency(writer, ticketIDs[i], prerequisiteID, "blocks"); err != nil {
				return fmt.Errorf("link question %q to prerequisite %q: %w", question.ID, prerequisite, err)
			}
		}
		if err := ensureDecisionDependency(writer, ticketIDs[i], mapDoc.MapID, "relates-to"); err != nil {
			return fmt.Errorf("link question %q to map: %w", question.ID, err)
		}
	}
	return ensureDecisionDependency(writer, mapDoc.PromptID, mapDoc.MapID, "relates-to")
}

func ensureDecisionDependency(writer beads.DecisionFrontierRecordWriter, issueID, targetID, depType string) error {
	if err := writer.EnsureDecisionFrontierLink(issueID, targetID, depType); err != nil {
		if errors.Is(err, beads.ErrDecisionFrontierLinkConflict) {
			return ErrConflict
		}
		return err
	}
	return nil
}

func validStoredRecordState(kind, initial, state string) bool {
	switch kind {
	case mapRecordKind:
		return state == initial || state == stateResolved
	case ticketRecordKind:
		if state == initial {
			return true
		}
		for _, prefix := range []string{"answering:", "answered:", "unresolved:"} {
			if strings.HasPrefix(state, prefix) && strings.TrimPrefix(state, prefix) != "" {
				return true
			}
		}
		return false
	case answerRecordKind:
		return state == initial
	case promptRecordKind:
		switch state {
		case initial, "pending", "submitting", "accepted", "delivered", "acknowledged", "unknown", "failed":
			return true
		default:
			return false
		}
	default:
		return false
	}
}

func validRecordMetadata(bead beads.Bead, kind, initialState string, allowReason bool) bool {
	if bead.ID == "" || bead.Type != "gate" || bead.Metadata[beadmeta.DecisionFrontierRecordMetadataKey] != kind ||
		!validStoredRecordState(kind, initialState, bead.Metadata[beadmeta.DecisionFrontierStateMetadataKey]) {
		return false
	}
	for key := range bead.Metadata {
		if key == beadmeta.DecisionFrontierRecordMetadataKey || key == beadmeta.DecisionFrontierStateMetadataKey {
			continue
		}
		if allowReason && key == beadmeta.DecisionFrontierReasonMetadataKey {
			continue
		}
		return false
	}
	return true
}

func persistAnswerOnce(ctx context.Context, writer beads.DecisionFrontierRecordWriter, store beads.Store, ticketID, answerID string, doc answerRecord, resolution Resolution) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	answerState := "answered:" + answerID
	if resolution != ResolutionAnswered {
		answerState = "unresolved:" + answerID
	}
	for attempts := 0; attempts < 3; attempts++ {
		ticket, err := store.Get(ticketID)
		if err != nil {
			return fmt.Errorf("read decision question ticket: %w", err)
		}
		state := ticket.Metadata[beadmeta.DecisionFrontierStateMetadataKey]
		if state == answerState {
			return verifyAnswerRecord(store, answerID, doc)
		}
		if strings.HasPrefix(state, "answering:") {
			if state != "answering:"+answerID {
				return ErrAnswerInProgress
			}
			if err := ensureImmutableRecord(writer, store, answerID, "Verified answer for "+ticketID, answerRecordKind, "recorded", doc); err != nil {
				return err
			}
			won, err := writer.CompareAndSetDecisionFrontierRecordMetadataKey(ticketID, beadmeta.DecisionFrontierStateMetadataKey, state, answerState)
			if err != nil {
				return fmt.Errorf("finish answer ticket transition: %w", err)
			}
			if won {
				return nil
			}
			continue
		}
		if state != statePending && !strings.HasPrefix(state, "unresolved:") {
			return ErrConflict
		}
		won, err := writer.CompareAndSetDecisionFrontierRecordMetadataKey(ticketID, beadmeta.DecisionFrontierStateMetadataKey, state, "answering:"+answerID)
		if err != nil {
			return fmt.Errorf("reserve exact answer for ticket: %w", err)
		}
		if !won {
			continue
		}
		if err := ensureImmutableRecord(writer, store, answerID, "Verified answer for "+ticketID, answerRecordKind, "recorded", doc); err != nil {
			// Leave the ticket in answering state. A retry of this same exact
			// answer can complete it; a different answer cannot replace it.
			return err
		}
		won, err = writer.CompareAndSetDecisionFrontierRecordMetadataKey(ticketID, beadmeta.DecisionFrontierStateMetadataKey, "answering:"+answerID, answerState)
		if err != nil {
			return fmt.Errorf("commit exact answer for ticket: %w", err)
		}
		if !won {
			return ErrConflict
		}
		return nil
	}
	return ErrAnswerInProgress
}

func verifyAnswerRecord(store beads.Store, answerID string, want answerRecord) error {
	bead, err := store.Get(answerID)
	if err != nil {
		return fmt.Errorf("read persisted answer %s: %w", answerID, err)
	}
	var got answerRecord
	if err := json.Unmarshal([]byte(bead.Description), &got); err != nil || got != want ||
		!validRecordMetadata(bead, answerRecordKind, "recorded", false) || bead.Title != "Verified answer for "+want.TicketID {
		return fmt.Errorf("%w: answer %s does not match its verified payload", ErrConflict, answerID)
	}
	return nil
}

func loadFrontier(store beads.Store, scope Scope, mapID string) (Frontier, error) {
	mapBead, err := store.Get(mapID)
	if err != nil {
		return Frontier{}, fmt.Errorf("read decision map %s: %w", mapID, err)
	}
	if !validRecordMetadata(mapBead, mapRecordKind, statePending, false) {
		return Frontier{}, ErrConflict
	}
	var doc mapRecord
	if err := json.Unmarshal([]byte(mapBead.Description), &doc); err != nil || doc.SchemaVersion != frontierSchemaVersion ||
		doc.CityRef != scope.CityRef || doc.StoreRef != scope.StoreRef || validateScope(Scope{CityRef: doc.CityRef, StoreRef: doc.StoreRef}) != nil || doc.MapID != mapID ||
		doc.MapID != frontierMapID(scope, doc.WorkID, doc.WorkRevision) {
		return Frontier{}, ErrConflict
	}
	if mapBead.Title != "Decision map for "+doc.WorkID {
		return Frontier{}, ErrConflict
	}
	if _, err := canonicalWorkRevision(doc.WorkRevision); err != nil || doc.WorkDigest == "" {
		return Frontier{}, ErrConflict
	}
	_, proposalHash, err := normalizeProposal(Proposal{Questions: doc.Questions, SourceLinks: doc.SourceLinks})
	if err != nil || proposalHash != doc.ProposalHash || doc.WorkID == "" || doc.PromptID != frontierPromptID(scope, mapID) {
		return Frontier{}, ErrConflict
	}
	frontier := Frontier{
		CityRef: doc.CityRef, StoreRef: doc.StoreRef, MapID: mapID, WorkID: doc.WorkID, WorkRevision: doc.WorkRevision,
		WorkDigest: doc.WorkDigest, proposalHash: doc.ProposalHash,
		State: StatePending, SourceLinks: cloneStringMap(doc.SourceLinks),
		Questions: make([]QuestionView, 0, len(doc.Questions)), OpenQuestions: make([]QuestionView, 0, len(doc.Questions)),
		Prompt: PromptView{ID: doc.PromptID, Status: "unavailable", Reason: "prompt delivery is not configured"},
	}
	allResolved := true
	for _, question := range doc.Questions {
		version, err := questionVersion(question)
		if err != nil {
			return Frontier{}, err
		}
		ticketID := frontierQuestionID(scope, mapID, question.ID)
		ticket, err := store.Get(ticketID)
		if err != nil {
			return Frontier{}, fmt.Errorf("read decision ticket %s: %w", ticketID, err)
		}
		if !validRecordMetadata(ticket, ticketRecordKind, statePending, false) {
			return Frontier{}, ErrConflict
		}
		var ticketDoc ticketRecord
		expectedQuestion, _ := json.Marshal(question)
		var storedQuestion []byte
		ticketErr := json.Unmarshal([]byte(ticket.Description), &ticketDoc)
		if ticketErr == nil {
			storedQuestion, _ = json.Marshal(ticketDoc.Question)
		}
		if ticketErr != nil || !slices.Equal(expectedQuestion, storedQuestion) || ticketDoc.SchemaVersion != frontierSchemaVersion ||
			ticketDoc.CityRef != doc.CityRef || ticketDoc.StoreRef != doc.StoreRef ||
			ticketDoc.WorkID != doc.WorkID || ticketDoc.WorkRevision != doc.WorkRevision || ticketDoc.WorkDigest != doc.WorkDigest ||
			ticketDoc.MapID != mapID || ticketDoc.Version != version || ticket.Title != question.Title {
			return Frontier{}, ErrConflict
		}
		view := QuestionView{
			ID: question.ID, TicketID: ticketID, Version: version, Title: question.Title, Prompt: question.Prompt,
			Recommendations: append([]string(nil), question.Recommendations...), DependsOn: append([]string(nil), question.DependsOn...),
			SourceLinks: append([]string(nil), question.SourceLinks...), Status: "open",
		}
		state := ticket.Metadata[beadmeta.DecisionFrontierStateMetadataKey]
		if strings.HasPrefix(state, "answered:") {
			answerID := strings.TrimPrefix(state, "answered:")
			view.Status = "answered"
			answer, err := readAnswerView(store, answerID, doc, ticketDoc)
			if err != nil {
				return Frontier{}, err
			}
			if answer.Resolution != ResolutionAnswered {
				return Frontier{}, ErrConflict
			}
			view.Answer = &answer
		} else {
			if state != statePending && !strings.HasPrefix(state, "answering:") && !strings.HasPrefix(state, "unresolved:") {
				return Frontier{}, ErrConflict
			}
			allResolved = false
			if strings.HasPrefix(state, "unresolved:") {
				answerID := strings.TrimPrefix(state, "unresolved:")
				answer, err := readAnswerView(store, answerID, doc, ticketDoc)
				if err != nil {
					return Frontier{}, err
				}
				view.Answer = &answer
			}
		}
		frontier.Questions = append(frontier.Questions, view)
	}
	byID := make(map[string]QuestionView, len(frontier.Questions))
	for _, question := range frontier.Questions {
		byID[question.ID] = question
	}
	for _, question := range frontier.Questions {
		if question.Status == "open" && dependenciesResolvedByID(byID, question.DependsOn) {
			frontier.OpenQuestions = append(frontier.OpenQuestions, question)
		}
	}
	if allResolved {
		frontier.State = StateResolved
	}
	mapState := mapBead.Metadata[beadmeta.DecisionFrontierStateMetadataKey]
	if mapState != statePending && mapState != stateResolved || mapState == stateResolved && !allResolved {
		return Frontier{}, ErrConflict
	}
	prompt, err := store.Get(doc.PromptID)
	if err == nil {
		if !validRecordMetadata(prompt, promptRecordKind, "unconfigured", true) || prompt.Title != "Decision prompt intent for "+doc.WorkID {
			return Frontier{}, ErrConflict
		}
		var promptDoc promptRecord
		if err := json.Unmarshal([]byte(prompt.Description), &promptDoc); err != nil || promptDoc.SchemaVersion != frontierSchemaVersion ||
			promptDoc.CityRef != doc.CityRef || promptDoc.StoreRef != doc.StoreRef || promptDoc.ID != doc.PromptID || promptDoc.WorkID != doc.WorkID ||
			promptDoc.WorkRevision != doc.WorkRevision || promptDoc.WorkDigest != doc.WorkDigest || promptDoc.MapID != mapID ||
			!slices.Equal(promptDoc.TicketIDs, ticketIDsFor(scope, mapID, doc.Questions)) ||
			!maps.Equal(promptDoc.SourceLinks, doc.SourceLinks) {
			return Frontier{}, ErrConflict
		}
		frontier.Prompt.Status = prompt.Metadata[beadmeta.DecisionFrontierStateMetadataKey]
		frontier.Prompt.Reason = prompt.Metadata[beadmeta.DecisionFrontierReasonMetadataKey]
		if frontier.Prompt.Status == "unconfigured" {
			if frontier.Prompt.Reason == "" {
				frontier.Prompt.Reason = "prompt delivery is not configured"
			}
		}
	} else {
		return Frontier{}, fmt.Errorf("read decision prompt intent: %w", err)
	}
	return frontier, nil
}

func readAnswerView(store beads.Store, answerID string, mapDoc mapRecord, ticket ticketRecord) (AnswerView, error) {
	bead, err := store.Get(answerID)
	if err != nil {
		return AnswerView{}, fmt.Errorf("read exact decision answer %s: %w", answerID, err)
	}
	if !validRecordMetadata(bead, answerRecordKind, "recorded", false) || bead.Title != "Verified answer for "+ticketIDFor(ticket) {
		return AnswerView{}, ErrConflict
	}
	var answer answerRecord
	if err := json.Unmarshal([]byte(bead.Description), &answer); err != nil || answer.SchemaVersion != frontierSchemaVersion ||
		answer.CityRef != mapDoc.CityRef || answer.StoreRef != mapDoc.StoreRef || answer.WorkID != mapDoc.WorkID || answer.WorkRevision != mapDoc.WorkRevision ||
		answer.WorkDigest != mapDoc.WorkDigest || answer.MapID != mapDoc.MapID ||
		answer.TicketID != ticketIDFor(ticket) || answer.QuestionID != ticket.Question.ID || answer.QuestionVersion != ticket.Version ||
		answer.Digest != AnswerDigest(answer.Resolution, answer.Text) ||
		answerID != answerRecordID(Scope{CityRef: mapDoc.CityRef, StoreRef: mapDoc.StoreRef}, answer.TicketID, answer.Digest) ||
		answer.Subject == "" || answer.Issuer == "" || answer.KeyID == "" {
		return AnswerView{}, ErrConflict
	}
	return AnswerView{
		ID: answerID, Resolution: answer.Resolution, Text: answer.Text, Subject: answer.Subject,
		Issuer: answer.Issuer, KeyID: answer.KeyID, Digest: answer.Digest,
	}, nil
}

func (s Service) advancePromptDelivery(ctx context.Context, store beads.Store, doc promptRecord) error {
	if s.Delivery == nil {
		return nil
	}
	bead, err := store.Get(doc.ID)
	if err != nil {
		return err
	}
	state := bead.Metadata[beadmeta.DecisionFrontierStateMetadataKey]
	if state == "unconfigured" {
		won, err := conditionalCAS(store, doc.ID, "unconfigured", "pending")
		if err != nil {
			return err
		}
		if !won {
			return nil
		}
		state = "pending"
	}
	request := PromptRequest{
		CityRef: doc.CityRef, StoreRef: doc.StoreRef, ID: doc.ID, WorkID: doc.WorkID, WorkRevision: doc.WorkRevision, MapID: doc.MapID,
		TicketIDs: append([]string(nil), doc.TicketIDs...), SourceLinks: cloneStringMap(doc.SourceLinks),
	}
	if state == "submitting" || state == "unknown" {
		result, reconcileErr := s.Delivery.ReconcileDecisionPrompt(ctx, doc.ID)
		if reconcileErr != nil {
			if err := setPromptState(store, doc.ID, state, "unknown", reconcileErr.Error()); err != nil {
				return err
			}
			return errors.Join(ErrPromptDeliveryUnavailable, reconcileErr)
		}
		switch {
		case result.DefinitivelyAbsent:
			won, err := conditionalCAS(store, doc.ID, state, "pending")
			if err != nil || !won {
				return err
			}
			state = "pending"
		case result.Status != "" && result.Status != "absent" && result.Status != "unknown":
			return setPromptState(store, doc.ID, state, normalizeDeliveryStatus(result.Status), result.Reason)
		default:
			reason := firstNonEmpty(result.Reason, "provider cannot prove that no delivery effect occurred")
			if err := setPromptState(store, doc.ID, state, "unknown", reason); err != nil {
				return err
			}
			return ErrPromptDeliveryUnavailable
		}
	}
	if state == "pending" {
		won, err := conditionalCAS(store, doc.ID, "pending", "submitting")
		if err != nil || !won {
			return err
		}
		result, deliverErr := s.Delivery.DeliverDecisionPrompt(ctx, request)
		if deliverErr != nil {
			if err := setPromptState(store, doc.ID, "submitting", "unknown", deliverErr.Error()); err != nil {
				return err
			}
			return errors.Join(ErrPromptDeliveryUnavailable, deliverErr)
		}
		return setPromptState(store, doc.ID, "submitting", normalizeDeliveryStatus(result.Status), result.Reason)
	}
	return nil
}

func conditionalCAS(store beads.Store, id, expected, next string) (bool, error) {
	writer, _, _, err := requireStoreCapabilities(store)
	if err != nil {
		return false, err
	}
	return writer.CompareAndSetDecisionFrontierRecordMetadataKey(id, beadmeta.DecisionFrontierStateMetadataKey, expected, next)
}

func setPromptState(store beads.Store, id, expected, next, reason string) error {
	won, err := conditionalCAS(store, id, expected, next)
	if err != nil {
		return err
	}
	if !won {
		return ErrConflict
	}
	bead, err := store.Get(id)
	if err != nil {
		return err
	}
	writer, _, _, err := requireStoreCapabilities(store)
	if err != nil {
		return err
	}
	previous := bead.Metadata[beadmeta.DecisionFrontierReasonMetadataKey]
	if _, err := writer.CompareAndSetDecisionFrontierRecordMetadataKey(id, beadmeta.DecisionFrontierReasonMetadataKey, previous, reason); err != nil {
		return err
	}
	return nil
}

func normalizeDeliveryStatus(status string) string {
	switch status {
	case "accepted", "delivered", "acknowledged", "unknown", "failed":
		return status
	default:
		return "unknown"
	}
}

func normalizeProposal(proposal Proposal) ([]Question, string, error) {
	if len(proposal.Questions) == 0 || len(proposal.Questions) > 64 {
		return nil, "", fmt.Errorf("%w: question count must be between 1 and 64", ErrInvalid)
	}
	questions := make([]Question, len(proposal.Questions))
	seen := make(map[string]struct{}, len(proposal.Questions))
	for i, question := range proposal.Questions {
		question.ID = strings.TrimSpace(question.ID)
		question.Title = strings.TrimSpace(question.Title)
		question.Prompt = strings.TrimSpace(question.Prompt)
		if !validQuestionID(question.ID) || question.Title == "" || question.Prompt == "" {
			return nil, "", fmt.Errorf("%w: question %d requires a stable ID, title and prompt", ErrInvalid, i)
		}
		if _, exists := seen[question.ID]; exists {
			return nil, "", fmt.Errorf("%w: duplicate question ID %q", ErrInvalid, question.ID)
		}
		seen[question.ID] = struct{}{}
		question.DependsOn = normalizedUniqueStrings(question.DependsOn)
		question.Recommendations = trimStrings(question.Recommendations)
		question.SourceLinks = trimStrings(question.SourceLinks)
		questions[i] = question
	}
	for _, question := range questions {
		for _, prerequisite := range question.DependsOn {
			if prerequisite == question.ID {
				return nil, "", fmt.Errorf("%w: question %q depends on itself", ErrInvalid, question.ID)
			}
			if _, exists := seen[prerequisite]; !exists {
				return nil, "", fmt.Errorf("%w: question %q depends on unknown question %q", ErrInvalid, question.ID, prerequisite)
			}
		}
	}
	if err := validateAcyclic(questions); err != nil {
		return nil, "", err
	}
	links := cloneStringMap(proposal.SourceLinks)
	for key, value := range links {
		if strings.TrimSpace(key) == "" || strings.TrimSpace(value) == "" {
			return nil, "", fmt.Errorf("%w: source links require nonempty names and values", ErrInvalid)
		}
	}
	body, err := json.Marshal(struct {
		Questions []Question        `json:"questions"`
		Links     map[string]string `json:"source_links,omitempty"`
	}{questions, links})
	if err != nil {
		return nil, "", err
	}
	return questions, digest(body), nil
}

func questionVersion(question Question) (string, error) {
	body, err := json.Marshal(question)
	if err != nil {
		return "", err
	}
	return digest(body), nil
}

func validateAcyclic(questions []Question) error {
	byID := make(map[string]Question, len(questions))
	for _, q := range questions {
		byID[q.ID] = q
	}
	state := make(map[string]uint8, len(questions))
	var visit func(string) bool
	visit = func(id string) bool {
		if state[id] == 1 {
			return false
		}
		if state[id] == 2 {
			return true
		}
		state[id] = 1
		for _, dep := range byID[id].DependsOn {
			if !visit(dep) {
				return false
			}
		}
		state[id] = 2
		return true
	}
	for _, q := range questions {
		if !visit(q.ID) {
			return fmt.Errorf("%w: question dependencies contain a cycle", ErrInvalid)
		}
	}
	return nil
}

func validQuestionID(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for i, r := range id {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || (i > 0 && (r == '.' || r == '_' || r == '-')) {
			continue
		}
		return false
	}
	return true
}

func normalizedUniqueStrings(values []string) []string {
	seen := map[string]struct{}{}
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func trimStrings(values []string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			result = append(result, value)
		}
	}
	return result
}

func validResolution(r Resolution) bool {
	switch r {
	case ResolutionAnswered, ResolutionDeclined, ResolutionDeferred, ResolutionUnclear, ResolutionPartial:
		return true
	default:
		return false
	}
}

func verifiedAnswerMatches(answer VerifiedAnswer, challenge AnswerChallenge) bool {
	return strings.TrimSpace(answer.KeyID) != "" && strings.TrimSpace(answer.Issuer) != "" && strings.TrimSpace(answer.Subject) != "" &&
		answer.CityRef == challenge.CityRef && answer.StoreRef == challenge.StoreRef && answer.WorkID == challenge.WorkID && answer.WorkRevision == challenge.WorkRevision &&
		answer.WorkDigest == challenge.WorkDigest && answer.MapID == challenge.MapID &&
		answer.TicketID == challenge.TicketID && answer.QuestionVersion == challenge.QuestionVersion &&
		answer.AnswerDigest == challenge.AnswerDigest && answer.Resolution == challenge.Resolution
}

func questionTicket(frontier Frontier, ticketID string) (QuestionView, bool) {
	for _, question := range frontier.Questions {
		if question.TicketID == ticketID {
			return question, true
		}
	}
	return QuestionView{}, false
}

func questionIsOpen(frontier Frontier, questionID string) bool {
	for _, question := range frontier.OpenQuestions {
		if question.ID == questionID {
			return true
		}
	}
	return false
}

func dependenciesResolvedByID(questions map[string]QuestionView, dependencies []string) bool {
	for _, dependency := range dependencies {
		question, ok := questions[dependency]
		if !ok || question.Status != "answered" {
			return false
		}
	}
	return true
}

func ticketIDsFor(scope Scope, mapID string, questions []Question) []string {
	ids := make([]string, len(questions))
	for i, question := range questions {
		ids[i] = frontierQuestionID(scope, mapID, question.ID)
	}
	return ids
}

func markMapResolved(writer beads.DecisionFrontierRecordWriter, store beads.Store, mapID string) error {
	mapBead, err := store.Get(mapID)
	if err != nil {
		return err
	}
	state := mapBead.Metadata[beadmeta.DecisionFrontierStateMetadataKey]
	if state == stateResolved {
		return nil
	}
	if state != statePending {
		return ErrConflict
	}
	won, err := writer.CompareAndSetDecisionFrontierRecordMetadataKey(mapID, beadmeta.DecisionFrontierStateMetadataKey, statePending, stateResolved)
	if err != nil {
		return err
	}
	if !won {
		return ErrConflict
	}
	return nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func indexQuestion(questions []Question, id string) int {
	for index, question := range questions {
		if question.ID == id {
			return index
		}
	}
	return -1
}

func ticketIDFor(ticket ticketRecord) string {
	return frontierQuestionID(Scope{CityRef: ticket.CityRef, StoreRef: ticket.StoreRef}, ticket.MapID, ticket.Question.ID)
}

func stableID(kind string, parts ...string) string {
	joined := kind
	for _, part := range parts {
		joined += "\x00" + part
	}
	sum := sha256.Sum256([]byte(joined))
	return "gcf-" + kind + "-" + hex.EncodeToString(sum[:16])
}

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func cloneStringMap(input map[string]string) map[string]string {
	if len(input) == 0 {
		return nil
	}
	return maps.Clone(input)
}
