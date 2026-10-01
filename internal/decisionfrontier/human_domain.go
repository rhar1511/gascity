package decisionfrontier

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"sort"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	sessiondomain "github.com/gastownhall/gascity/internal/session"
)

// IndependentQuestionsContract identifies the numbered, independent-round port.
const IndependentQuestionsContract = beads.DecisionFrontierIndependentQuestionsContract

func requireIndependentRoundContract(store beads.Store) error {
	if _, ok := beads.DecisionFrontierAtomicBackendFor(store); !ok {
		return fmt.Errorf("%w: atomic target creation and conditional resume backend are required", ErrUnavailable)
	}
	writer, ok := beads.DecisionFrontierRecordWriterFor(store)
	if !ok || beads.DecisionFrontierDeliveryContractFor(writer) != IndependentQuestionsContract {
		return fmt.Errorf("%w: protected backend does not advertise %s", ErrUnavailable, IndependentQuestionsContract)
	}
	return nil
}

// PreparedProposal is an opaque, server-held capability. It cannot be restored
// from caller JSON; restart requires preparation against persisted evidence.
type PreparedProposal struct {
	store                          beads.Store
	scope                          Scope
	workID, revision, hash, target string
	proposal                       Proposal
	binding                        PromptBinding
	request                        PromptRequest
}

func backendPromptTarget(store beads.Store, target string, binding PromptBinding) error {
	reader := sessiondomain.NewStore(beads.SessionStore{Store: store})
	id, err := reader.ResolveID(target)
	if err != nil || id != binding.SessionID {
		return errors.Join(ErrConflict, err)
	}
	info, response, err := reader.GetPersistedResponse(id)
	if err != nil {
		return errors.Join(ErrUnavailable, err)
	}
	if err := configuredPromptTarget(info, target, id); err != nil {
		return err
	}
	generation, err := persistedSessionGeneration(info, response, id, true)
	if err != nil {
		return err
	}
	if int64(generation) != binding.ExecutionGeneration {
		return ErrConflict
	}
	_, err = store.Get(id)
	return err
}

// TargetName returns the configured named identity captured by preparation.
func (p PreparedProposal) TargetName() string { return p.target }

// TargetBinding returns a copy of the exact prepared session execution binding.
func (p PreparedProposal) TargetBinding() PromptBinding { return p.binding }

func (p PreparedProposal) matchesMap(doc mapRecord) bool {
	return doc.DeliveryContract == IndependentQuestionsContract && doc.PromptTarget == p.target &&
		doc.ProposalHash == p.hash && doc.PromptBinding != nil && *doc.PromptBinding == p.binding
}

// PrepareProposal resolves one configured named session execution without
// writing records or contacting a provider. Only the concrete Q52 adapter can
// supply this contract; an ordinary delivery port is insufficient.
func (s Service) PrepareProposal(ctx context.Context, store beads.Store, scope Scope, workID, revision string, proposal Proposal) (PreparedProposal, error) {
	if err := requireIndependentRoundContract(store); err != nil {
		return PreparedProposal{}, err
	}
	d, ok := s.Delivery.(*SessionPromptDelivery)
	if !ok || d == nil || s.Verifier == nil || store == nil || !reflect.TypeOf(store).Comparable() {
		return PreparedProposal{}, ErrUnavailable
	}
	if err := ctx.Err(); err != nil {
		return PreparedProposal{}, err
	}
	if err := validateScope(scope); err != nil {
		return PreparedProposal{}, err
	}
	if _, err := canonicalWorkRevision(revision); err != nil {
		return PreparedProposal{}, err
	}
	_, _, reader, _, err := requireStoreCapabilities(store)
	if err != nil {
		return PreparedProposal{}, err
	}
	questions, hash, err := normalizeProposal(proposal)
	if err != nil {
		return PreparedProposal{}, err
	}
	work, err := reader.DecisionFrontierSourceSnapshot(workID)
	if err != nil {
		return PreparedProposal{}, err
	}
	workDigest, err := WorkDigest(work)
	if err != nil {
		return PreparedProposal{}, err
	}
	doc := mapRecord{
		CityRef: scope.CityRef, StoreRef: scope.StoreRef, WorkID: workID, WorkRevision: revision,
		WorkDigest: workDigest, MapID: frontierMapID(scope, workID, revision), Questions: questions,
		DeliveryContract: IndependentQuestionsContract, SourceLinks: cloneStringMap(proposal.SourceLinks), SourceIssue: cloneSourceIssueRef(proposal.SourceIssue),
	}
	doc.PromptID = frontierPromptID(scope, doc.MapID)
	request, err := promptRequestForMap(doc)
	if err != nil {
		return PreparedProposal{}, err
	}
	binding, err := d.ResolveDecisionPrompt(ctx, request)
	if err != nil {
		return PreparedProposal{}, err
	}
	if err := backendPromptTarget(store, d.target, binding); err != nil {
		return PreparedProposal{}, err
	}
	p := PreparedProposal{
		store: store, scope: scope, workID: workID, revision: revision, hash: hash, target: d.target, binding: binding, request: clonePromptRequest(request),
		proposal: Proposal{Questions: questions, SourceLinks: cloneStringMap(proposal.SourceLinks), SourceIssue: cloneSourceIssueRef(proposal.SourceIssue)},
	}
	if b, err := store.Get(doc.MapID); err == nil {
		var persisted mapRecord
		if json.Unmarshal([]byte(b.Description), &persisted) != nil || !p.matchesMap(persisted) {
			return PreparedProposal{}, ErrConflict
		}
		if _, err := s.ReadAuthorizedFrontier(ctx, store, scope, workID, revision); err != nil {
			return PreparedProposal{}, err
		}
	} else if !errors.Is(err, beads.ErrNotFound) {
		return PreparedProposal{}, err
	} else if actual, err := WorkRevision(work); err != nil || actual != revision {
		return PreparedProposal{}, ErrStale
	}
	return p, nil
}

// EnsurePrepared enforces the opaque preparation inside record creation and
// replay. A competing immutable map with a different binding is never adopted.
func (s Service) EnsurePrepared(ctx context.Context, store beads.Store, p PreparedProposal) (Frontier, error) {
	if err := requireIndependentRoundContract(store); err != nil {
		return Frontier{}, err
	}
	d, ok := s.Delivery.(*SessionPromptDelivery)
	if !ok || d == nil || s.Verifier == nil || p.store == nil || store == nil || !reflect.TypeOf(store).Comparable() || p.store != store || d.target != p.target {
		return Frontier{}, ErrUnavailable
	}
	fenced := s
	fenced.prepared = &p
	var f Frontier
	current, err := d.ResolveDecisionPrompt(ctx, p.request)
	if err != nil {
		return Frontier{}, err
	}
	if current != p.binding {
		return Frontier{}, ErrConflict
	}
	if _, err := store.Get(frontierMapID(p.scope, p.workID, p.revision)); err == nil {
		if _, err := s.ReadAuthorizedFrontier(ctx, store, p.scope, p.workID, p.revision); err != nil {
			return Frontier{}, err
		}
	} else if !errors.Is(err, beads.ErrNotFound) {
		return Frontier{}, err
	}
	backend, _ := beads.DecisionFrontierAtomicBackendFor(store)
	// The physical transaction, not a process-local session lock, owns target
	// revision comparison, source reservation and immutable record creation.
	err = backend.EnsureDecisionFrontierTargetBound(beads.DecisionFrontierTargetBinding{SessionID: p.binding.SessionID, Generation: p.binding.ExecutionGeneration, TargetName: p.target}, func(view beads.Store) error {
		if err := backendPromptTarget(view, p.target, p.binding); err != nil {
			return err
		}
		fenced.Delivery = preparedPromptDelivery{SessionPromptDelivery: d, binding: p.binding, store: view, target: p.target}
		var err error
		f, err = fenced.Ensure(ctx, view, p.scope, p.workID, p.revision, p.proposal)
		return err
	})
	if beads.IsPreconditionFailed(err) || errors.Is(err, beads.ErrDecisionFrontierLinkConflict) {
		return Frontier{}, errors.Join(ErrConflict, err)
	}
	if err != nil {
		return f, err
	}
	if err := s.deliverIndependentRound(ctx, store, p.scope, p.workID, p.revision); err != nil {
		return f, err
	}
	a, err := s.ReadAuthorizedFrontier(ctx, store, p.scope, p.workID, p.revision)
	return a.Frontier, err
}

type preparedPromptDelivery struct {
	*SessionPromptDelivery
	binding PromptBinding
	store   beads.Store
	target  string
}

func (d preparedPromptDelivery) ResolveDecisionPrompt(ctx context.Context, request PromptRequest) (PromptBinding, error) {
	if err := ctx.Err(); err != nil {
		return PromptBinding{}, err
	}
	if !validPromptBinding(d.binding, request.ID) {
		return PromptBinding{}, ErrConflict
	}
	if err := backendPromptTarget(d.store, d.target, d.binding); err != nil {
		return PromptBinding{}, err
	}
	return d.binding, nil
}

// AuthorizedAnswer is derived by re-verifying a persisted signed proof.
type AuthorizedAnswer struct {
	TicketID   string     `json:"ticket_id"`
	AnswerID   string     `json:"answer_id"`
	Subject    string     `json:"subject"`
	Issuer     string     `json:"issuer"`
	KeyID      string     `json:"key_id"`
	Resolution Resolution `json:"resolution"`
}

// SessionEvidence is read from the exact persisted Q52 execution and receipt.
// Effect remains unverified; delivery evidence never authorizes an answer.
type SessionEvidence struct {
	Binding PromptBinding                 `json:"binding"`
	Status  string                        `json:"status"`
	Effect  string                        `json:"effect"`
	Receipt *sessiondomain.RequestReceipt `json:"receipt,omitempty"`
}

// AuthorizedFrontier carries independently verified authority and source evidence.
type AuthorizedFrontier struct {
	Frontier         Frontier                         `json:"frontier"`
	Answers          []AuthorizedAnswer               `json:"answers"`
	Session          SessionEvidence                  `json:"session"`
	Rounds           []SessionEvidence                `json:"rounds"`
	PhysicalRevision string                           `json:"physical_revision"`
	Reservation      beads.RevisionTransitionReceipt  `json:"reservation"`
	Release          *beads.RevisionTransitionReceipt `json:"release,omitempty"`
}

// ReadAuthorizedFrontier re-verifies proofs with current trust, and reconciles
// the exact bound session receipt without sending. Legacy hash-only records are
// not authority. Request authentication is the responsibility of the caller.
func (s Service) ReadAuthorizedFrontier(ctx context.Context, store beads.Store, scope Scope, workID, revision string) (AuthorizedFrontier, error) {
	if err := requireIndependentRoundContract(store); err != nil {
		return AuthorizedFrontier{}, err
	}
	if s.Verifier == nil {
		return AuthorizedFrontier{}, ErrAnswerVerifierUnavailable
	}
	d, ok := s.Delivery.(*SessionPromptDelivery)
	if !ok || d == nil {
		return AuthorizedFrontier{}, ErrPromptDeliveryUnavailable
	}
	f, err := s.Read(ctx, store, scope, workID, revision)
	if err != nil {
		return AuthorizedFrontier{}, err
	}
	b, err := store.Get(f.MapID)
	if err != nil {
		return AuthorizedFrontier{}, err
	}
	var doc mapRecord
	if json.Unmarshal([]byte(b.Description), &doc) != nil || doc.DeliveryContract != IndependentQuestionsContract || doc.PromptTarget != d.target || doc.PromptBinding == nil {
		return AuthorizedFrontier{}, ErrConflict
	}
	if err := backendPromptTarget(store, doc.PromptTarget, *doc.PromptBinding); err != nil {
		return AuthorizedFrontier{}, err
	}
	a := AuthorizedFrontier{Frontier: f, Answers: []AuthorizedAnswer{}}
	for _, q := range f.Questions {
		if q.Answer == nil {
			continue
		}
		b, err := store.Get(q.Answer.ID)
		if err != nil {
			return AuthorizedFrontier{}, err
		}
		var record answerRecord
		if json.Unmarshal([]byte(b.Description), &record) != nil || record.Proof == "" || digest([]byte(record.Proof)) != record.ProofDigest {
			return AuthorizedFrontier{}, ErrUnauthorized
		}
		challenge := AnswerChallenge{
			CityRef: scope.CityRef, StoreRef: scope.StoreRef, WorkID: workID, WorkRevision: revision, WorkDigest: f.WorkDigest,
			MapID: f.MapID, TicketID: q.TicketID, QuestionID: q.ID, QuestionVersion: q.Version, AnswerDigest: q.Answer.Digest, Resolution: q.Answer.Resolution,
		}
		sub := AnswerSubmission{TicketID: q.TicketID, WorkRevision: revision, QuestionVersion: q.Version, Resolution: record.Resolution, Text: record.Text, Proof: record.Proof}
		verified, err := s.Verifier.VerifyDecisionAnswer(ctx, challenge, sub)
		if err != nil {
			return AuthorizedFrontier{}, errors.Join(ErrUnauthorized, err)
		}
		if !verifiedAnswerMatches(verified, challenge) || verified.Subject != record.Subject || verified.Issuer != record.Issuer || verified.KeyID != record.KeyID {
			return AuthorizedFrontier{}, ErrUnauthorized
		}
		a.Answers = append(a.Answers, AuthorizedAnswer{TicketID: q.TicketID, AnswerID: q.Answer.ID, Subject: verified.Subject, Issuer: verified.Issuer, KeyID: verified.KeyID, Resolution: verified.Resolution})
	}
	request, err := independentRoundRequest(f, doc)
	if err != nil {
		return AuthorizedFrontier{}, err
	}
	// A resolved map has no current open round; retain the original execution
	// receipt as session evidence rather than inventing an empty delivery.
	if len(request.Questions) == 0 {
		request, err = promptRequestForMap(doc)
	}
	if err != nil {
		return AuthorizedFrontier{}, err
	}
	binding := *doc.PromptBinding
	binding.RequestID = request.ID
	result, err := d.ReconcileDecisionPrompt(ctx, request, binding)
	if err != nil {
		return AuthorizedFrontier{}, err
	}
	a.Session = SessionEvidence{Binding: binding, Status: result.Status, Effect: "unverified", Receipt: result.Receipt}
	// The protected source-flow view names the exact current round, not the
	// map's original intent when a downstream question set has become visible.
	a.Frontier.Prompt = PromptView{ID: request.ID, Status: result.Status, Reason: result.Reason}
	// Authenticate every persisted round by its immutable map presentation and
	// exact session receipt, including downstream rounds after full resolution.
	edges, err := store.DepList(f.MapID, "up")
	if err != nil {
		return AuthorizedFrontier{}, err
	}
	sort.Slice(edges, func(i, j int) bool { return edges[i].IssueID < edges[j].IssueID })
	a.Rounds = []SessionEvidence{}
	currentFound := false
	for _, edge := range edges {
		if edge.Type != "relates-to" {
			continue
		}
		b, err := store.Get(edge.IssueID)
		if err != nil {
			return AuthorizedFrontier{}, err
		}
		if b.Metadata[beadmeta.DecisionFrontierRecordMetadataKey] != promptRecordKind {
			continue
		}
		var round promptRecord
		if json.Unmarshal([]byte(b.Description), &round) != nil || !validRecordMetadata(b, promptRecordKind, "unconfigured", true) || b.Title != "Decision prompt intent for "+workID {
			return AuthorizedFrontier{}, ErrConflict
		}
		r, err := verifiedRoundRequest(round, doc)
		if err != nil {
			return AuthorizedFrontier{}, err
		}
		observed, err := d.ReconcileDecisionPrompt(ctx, r, *round.PromptBinding)
		if err != nil {
			return AuthorizedFrontier{}, err
		}
		a.Rounds = append(a.Rounds, SessionEvidence{Binding: *round.PromptBinding, Status: observed.Status, Effect: "unverified", Receipt: observed.Receipt})
		if r.ID == request.ID {
			if r.MessageDigest != request.MessageDigest {
				return AuthorizedFrontier{}, ErrConflict
			}
			currentFound = true
		}
	}
	if !currentFound && result.Status != "absent" {
		return AuthorizedFrontier{}, ErrConflict
	}
	for _, q := range f.Questions {
		if q.Answer == nil {
			continue
		}
		if _, err := deliveredRoundForQuestion(store, a, q); err != nil {
			return AuthorizedFrontier{}, err
		}
	}
	_, _, sourceReader, receipts, err := requireStoreCapabilities(store)
	if err != nil {
		return AuthorizedFrontier{}, err
	}
	work, err := sourceReader.DecisionFrontierSourceSnapshot(workID)
	if err != nil {
		return AuthorizedFrontier{}, err
	}
	marker := canonicalHoldValue(scope, workID, revision, f.WorkDigest, f.MapID, f.proposalHash)
	if err := validateFrontierSource(work, f, marker, receipts); err != nil {
		return AuthorizedFrontier{}, err
	}
	a.PhysicalRevision, err = WorkRevision(work)
	if err != nil {
		return AuthorizedFrontier{}, err
	}
	reserved, found, err := transitionReceipt(receipts, workID, doc.ReservationID)
	if err != nil || !found {
		return AuthorizedFrontier{}, errors.Join(ErrConflict, err)
	}
	a.Reservation = reserved
	if released, found, err := transitionReceipt(receipts, workID, doc.ReleaseID); err != nil {
		return AuthorizedFrontier{}, err
	} else if found {
		a.Release = &released
	}
	latest, err := loadFrontier(store, scope, f.MapID)
	if err != nil {
		return AuthorizedFrontier{}, err
	}
	before, _ := json.Marshal(f.Questions)
	after, _ := json.Marshal(latest.Questions)
	if !slices.Equal(before, after) {
		return AuthorizedFrontier{}, ErrStale
	}
	return a, nil
}

func deliveredRoundForQuestion(store beads.Store, a AuthorizedFrontier, q QuestionView) (promptRecord, error) {
	for _, evidence := range a.Rounds {
		if (evidence.Status != "accepted" && evidence.Status != "acknowledged") || evidence.Receipt == nil {
			continue
		}
		b, err := store.Get(evidence.Binding.RequestID)
		if err != nil {
			return promptRecord{}, err
		}
		var round promptRecord
		if json.Unmarshal([]byte(b.Description), &round) != nil || round.PromptBinding == nil || *round.PromptBinding != evidence.Binding ||
			round.MessageDigest != evidence.Receipt.MessageDigest {
			return promptRecord{}, ErrConflict
		}
		for _, slot := range round.Questions {
			if slot.TicketID == q.TicketID && slot.Version == q.Version {
				return round, nil
			}
		}
	}
	return promptRecord{}, fmt.Errorf("%w: question %s version %s lacks an accepted exact delivery round", ErrPromptDeliveryUnavailable, q.ID, q.Version)
}

func (s Service) requireDeliveredQuestion(ctx context.Context, store beads.Store, a AuthorizedFrontier, q QuestionView) error {
	round, err := deliveredRoundForQuestion(store, a, q)
	if err != nil {
		return err
	}
	b, err := store.Get(round.ID)
	if err != nil {
		return err
	}
	state := b.Metadata[beadmeta.DecisionFrontierStateMetadataKey]
	// Converge a lost protected-state write using read-only Q52 reconciliation.
	// The matching session receipt already proves acceptance; this path sends
	// nothing and cannot invent evidence for an absent or unknown round.
	if state == "unknown" || state == "submitting" {
		if err := s.advancePromptDelivery(ctx, store, round); err != nil {
			return err
		}
		b, err = store.Get(round.ID)
		if err != nil {
			return err
		}
		state = b.Metadata[beadmeta.DecisionFrontierStateMetadataKey]
	}
	if state != "accepted" && state != "acknowledged" {
		return ErrPromptDeliveryUnavailable
	}
	return nil
}

func verifiedRoundRequest(round promptRecord, doc mapRecord) (PromptRequest, error) {
	r, err := promptRequestFromRecord(round)
	if err != nil {
		return PromptRequest{}, err
	}
	if doc.PromptBinding == nil || round.PromptBinding == nil || round.PromptBinding.SessionID != doc.PromptBinding.SessionID ||
		round.PromptBinding.ExecutionGeneration != doc.PromptBinding.ExecutionGeneration || !validPromptBinding(*round.PromptBinding, r.ID) {
		return PromptRequest{}, ErrConflict
	}
	// Reconstruct immutable presentation in map order, without trusting text,
	// provenance, numbering, versions or scope copied into a round record.
	f := Frontier{}
	index := 0
	for _, q := range doc.Questions {
		if index >= len(r.Questions) || q.ID != r.Questions[index].ID {
			continue
		}
		version, err := questionVersion(q)
		if err != nil {
			return PromptRequest{}, err
		}
		f.OpenQuestions = append(f.OpenQuestions, QuestionView{
			ID: q.ID, TicketID: frontierQuestionID(Scope{CityRef: doc.CityRef, StoreRef: doc.StoreRef}, doc.MapID, q.ID),
			Version: version, Title: q.Title, Prompt: q.Prompt, Recommendations: q.Recommendations, DependsOn: q.DependsOn, SourceLinks: q.SourceLinks, SourceIssue: doc.SourceIssue,
		})
		index++
	}
	if index != len(r.Questions) || index == 0 {
		return PromptRequest{}, ErrConflict
	}
	expected, err := independentRoundRequest(f, doc)
	if err != nil {
		return PromptRequest{}, err
	}
	if expected.ID != r.ID || expected.MessageDigest != r.MessageDigest {
		return PromptRequest{}, ErrConflict
	}
	return r, nil
}

func independentRoundRequest(f Frontier, doc mapRecord) (PromptRequest, error) {
	initial, err := promptRequestForMap(doc)
	if err != nil {
		return PromptRequest{}, err
	}
	r := initial
	r.Questions = nil
	r.TicketIDs = nil
	for _, q := range f.OpenQuestions {
		r.TicketIDs = append(r.TicketIDs, q.TicketID)
		r.Questions = append(r.Questions, PromptQuestionPresentation{
			Number: len(r.Questions) + 1, ID: q.ID, TicketID: q.TicketID, Version: q.Version,
			Title: q.Title, Prompt: q.Prompt, Recommendations: q.Recommendations, DependsOn: q.DependsOn, SourceLinks: q.SourceLinks, SourceIssue: q.SourceIssue,
		})
	}
	if !slices.Equal(r.TicketIDs, initial.TicketIDs) {
		versions := make([]string, 0, len(r.Questions))
		for _, q := range r.Questions {
			versions = append(versions, q.TicketID+":"+q.Version)
		}
		r.ID = beads.DecisionFrontierRoundRecordID(r.CityRef, r.StoreRef, r.MapID, versions)
	}
	r.MessageDigest = promptMessageDigest(r)
	return r, nil
}

func (s Service) deliverIndependentRound(ctx context.Context, store beads.Store, scope Scope, workID, revision string) error {
	a, err := s.ReadAuthorizedFrontier(ctx, store, scope, workID, revision)
	if err != nil {
		return err
	}
	if len(a.Frontier.OpenQuestions) == 0 {
		return nil
	}
	b, err := store.Get(a.Frontier.MapID)
	if err != nil {
		return err
	}
	var doc mapRecord
	if json.Unmarshal([]byte(b.Description), &doc) != nil {
		return ErrConflict
	}
	r, err := independentRoundRequest(a.Frontier, doc)
	if err != nil {
		return err
	}
	binding := *doc.PromptBinding
	binding.RequestID = r.ID
	intent := promptRecord{
		SchemaVersion: 1, CityRef: r.CityRef, StoreRef: r.StoreRef, ID: r.ID, WorkID: r.WorkID, WorkRevision: r.WorkRevision,
		WorkDigest: r.WorkDigest, MapID: r.MapID, PresentationVersion: r.PresentationVersion, MessageDigest: r.MessageDigest,
		TicketIDs: r.TicketIDs, Questions: r.Questions, SourceLinks: r.SourceLinks, SourceIssue: r.SourceIssue, PromptBinding: &binding,
	}
	writer, _, _, _, err := requireStoreCapabilities(store)
	if err != nil {
		return err
	}
	if err := ensureImmutableRecord(writer, store, r.ID, "Decision prompt intent for "+workID, promptRecordKind, "unconfigured", intent); err != nil {
		return err
	}
	if err := ensureDecisionDependency(writer, r.ID, r.MapID, "relates-to"); err != nil {
		return err
	}
	return s.advancePromptDelivery(ctx, store, intent)
}

// SubmitAnswer verifies current durable authority before using the existing Q43
// signed-answer transition, then delivers any newly independent Q52 round.
func (s Service) SubmitAnswer(ctx context.Context, store beads.Store, scope Scope, workID string, sub AnswerSubmission) (AuthorizedFrontier, error) {
	if _, err := s.ReadAuthorizedFrontier(ctx, store, scope, workID, sub.WorkRevision); err != nil {
		return AuthorizedFrontier{}, err
	}
	if _, err := s.Answer(ctx, store, scope, workID, sub); err != nil {
		return AuthorizedFrontier{}, err
	}
	if err := s.deliverIndependentRound(ctx, store, scope, workID, sub.WorkRevision); err != nil {
		return AuthorizedFrontier{}, err
	}
	return s.ReadAuthorizedFrontier(ctx, store, scope, workID, sub.WorkRevision)
}

// ResumeEligibility is a conditional observation, never an admission receipt.
type ResumeEligibility struct {
	Eligible         bool   `json:"eligible"`
	Reason           string `json:"reason,omitempty"`
	MapID            string `json:"map_id"`
	FrontierRevision string `json:"frontier_revision"`
	PhysicalRevision string `json:"physical_revision"`
}

// CheckResume binds the original frontier and current physical row revisions to
// protected hold/release evidence. Later actions must independently CAS this
// physical revision; there is no multi-row admission transaction in Q43.
func (s Service) CheckResume(ctx context.Context, store beads.Store, scope Scope, workID, frontierRevision, physicalRevision string) (ResumeEligibility, error) {
	physical, err := canonicalWorkRevision(physicalRevision)
	if err != nil {
		return ResumeEligibility{}, err
	}
	a, err := s.ReadAuthorizedFrontier(ctx, store, scope, workID, frontierRevision)
	if err != nil {
		return ResumeEligibility{}, err
	}
	if physicalRevision != a.PhysicalRevision {
		return ResumeEligibility{}, ErrStale
	}
	f := a.Frontier
	backend, ok := beads.DecisionFrontierAtomicBackendFor(store)
	if !ok {
		return ResumeEligibility{}, ErrUnavailable
	}
	check := beads.DecisionFrontierResumeCheck{
		CityRef: scope.CityRef, StoreRef: scope.StoreRef, WorkID: workID, MapID: f.MapID,
		FrontierRevision: frontierRevision, PhysicalRevision: physical,
	}
	for _, q := range f.Questions {
		if q.Status == "answered" && q.Answer != nil {
			check.Answers = append(check.Answers, beads.DecisionFrontierAnswerSlot{TicketID: q.TicketID, AnswerID: q.Answer.ID})
		}
	}
	var observed beads.DecisionFrontierResumeResult
	delivery := s.Delivery.(*SessionPromptDelivery) // ReadAuthorizedFrontier checked composition.
	err = backend.EnsureDecisionFrontierTargetBound(beads.DecisionFrontierTargetBinding{
		SessionID: a.Session.Binding.SessionID, Generation: a.Session.Binding.ExecutionGeneration, TargetName: delivery.target,
	}, func(view beads.Store) error {
		// Re-resolve the named target from this same physical transaction image.
		// A stale Q52 reader cannot authorize resume after an external session
		// generation, name, state or alias change.
		if err := backendPromptTarget(view, delivery.target, a.Session.Binding); err != nil {
			return err
		}
		guard, ok := beads.DecisionFrontierAtomicBackendFor(view)
		if !ok {
			return ErrUnavailable
		}
		var err error
		observed, err = guard.CheckDecisionFrontierResume(check)
		return err
	})
	if beads.IsPreconditionFailed(err) {
		return ResumeEligibility{}, errors.Join(ErrStale, err)
	}
	if errors.Is(err, beads.ErrDecisionFrontierLinkConflict) {
		return ResumeEligibility{}, errors.Join(ErrConflict, err)
	}
	if err != nil {
		return ResumeEligibility{}, err
	}
	return ResumeEligibility{MapID: f.MapID, FrontierRevision: frontierRevision, PhysicalRevision: physicalRevision, Eligible: observed.Eligible, Reason: observed.Reason}, nil
}
