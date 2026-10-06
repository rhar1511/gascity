package decisionfrontier_test

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	df "github.com/gastownhall/gascity/internal/decisionfrontier"
	"github.com/gastownhall/gascity/internal/session"
)

type humanSessions struct {
	generation   int
	receipts     map[string]session.RequestReceipt
	messages     []df.PromptRequest
	deliveryMode session.RequestDelivery
}

func (s *humanSessions) ResolveID(string) (string, error) { return "session-human", nil }
func (s *humanSessions) GetPersistedResponse(string) (session.Info, session.PersistedResponse, error) {
	return session.Info{
		ID: "session-human", State: session.StateActive, MetadataState: string(session.StateActive), Generation: strconv.Itoa(s.generation),
		ConfiguredNamedSession: true, ConfiguredNamedIdentity: "review-desk",
	}, session.PersistedResponse{Status: "open"}, nil
}

func (s *humanSessions) GetRequest(_, id string) (session.RequestReceipt, error) {
	r, ok := s.receipts[id]
	if !ok {
		return r, session.ErrRequestNotFound
	}
	return r, nil
}

func (s *humanSessions) SubmitRequest(_ context.Context, sid, id string, generation int, message string) (session.RequestReceipt, error) {
	if generation != s.generation {
		return session.RequestReceipt{}, session.ErrRequestConflict
	}
	if r, ok := s.receipts[id]; ok {
		return r, nil
	}
	var request df.PromptRequest
	if err := json.Unmarshal([]byte(message), &request); err != nil {
		return session.RequestReceipt{}, err
	}
	s.messages = append(s.messages, request)
	sum := sha256.Sum256([]byte(message))
	now := time.Unix(1800000000, 0)
	r := session.RequestReceipt{
		SessionID: sid, RequestID: id, Generation: generation, MessageDigest: hex.EncodeToString(sum[:]),
		AcceptedAt: now, ProviderResultAt: &now, Delivery: session.RequestDeliveryAccepted, Effect: "unverified",
	}
	if s.deliveryMode != "" {
		r.Delivery = s.deliveryMode
		if r.Delivery == session.RequestDeliveryPending {
			r.ProviderResultAt = nil
		}
		if r.Delivery == session.RequestDeliveryUnknown {
			r.DeliveryAttemptedAt = &now
		}
	}
	s.receipts[id] = r
	return r, nil
}

type humanFixture struct {
	store    *beads.MemStore
	sessions *humanSessions
	service  df.Service
	scope    df.Scope
	work     beads.Bead
	revision string
	private  ed25519.PrivateKey
	now      time.Time
}

func newHumanFixture(t *testing.T) *humanFixture {
	t.Helper()
	x := &humanFixture{
		store: beads.NewMemStore(), sessions: &humanSessions{generation: 7, receipts: map[string]session.RequestReceipt{}},
		scope: df.Scope{CityRef: "city:test", StoreRef: "city:test"}, now: time.Unix(1800000000, 0),
	}
	x.store.HonorExplicitIDs = true
	var err error
	_, err = x.store.Create(beads.Bead{
		ID: "session-human", Type: session.BeadType, Title: "Human review session", Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"session_name": "review-desk", "state": string(session.StateActive), "generation": "7",
			session.NamedSessionMetadataKey: "true", session.NamedSessionIdentityMetadata: "review-desk",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	x.work, err = x.store.Create(beads.Bead{ID: "work-human", Type: "task", Title: "Choose"})
	if err != nil {
		t.Fatal(err)
	}
	x.revision, err = df.WorkRevision(x.work)
	if err != nil {
		t.Fatal(err)
	}
	x.private = ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
	trust, err := api.NewPRHumanGrantVerifier(api.PRHumanTrustConfig{
		Keys:        []api.PRHumanGrantKey{{KeyID: "fixture", PublicKey: base64.StdEncoding.EncodeToString(x.private.Public().(ed25519.PublicKey))}},
		Authorities: []api.PRHumanAuthority{{KeyID: "fixture", Issuer: "fixture-issuer", Subject: "human@example.test", Scopes: []string{api.DecisionAnswerScope}}},
	}, nil, func() time.Time { return x.now })
	if err != nil {
		t.Fatal(err)
	}
	delivery, err := df.NewSessionPromptDelivery("review-desk", x.sessions, x.sessions)
	if err != nil {
		t.Fatal(err)
	}
	x.service = df.Service{Verifier: api.NewDecisionAnswerGrantVerifier(trust), Delivery: delivery}
	return x
}

func humanProposal() df.Proposal {
	return df.Proposal{Questions: []df.Question{
		{ID: "first", Title: "First", Prompt: "Choose first"},
		{ID: "later", Title: "Later", Prompt: "Choose later", DependsOn: []string{"first"}},
	}}
}

func restoreHumanBackend(t *testing.T, store *beads.MemStore, mutate func(*beads.Bead)) *beads.MemStore {
	t.Helper()
	rows, err := store.List(beads.ListQuery{AllowScan: true, IncludeClosed: true})
	if err != nil {
		t.Fatal(err)
	}
	var deps []beads.Dep
	for i := range rows {
		edges, err := store.DepList(rows[i].ID, "down")
		if err != nil {
			t.Fatal(err)
		}
		deps = append(deps, edges...)
		if mutate != nil {
			mutate(&rows[i])
		}
	}
	restored := beads.NewMemStoreFrom(100, rows, deps)
	restored.HonorExplicitIDs = true
	return restored
}

func (x *humanFixture) ensure(t *testing.T) df.Frontier {
	t.Helper()
	p, err := x.service.PrepareProposal(context.Background(), x.store, x.scope, x.work.ID, x.revision, humanProposal())
	if err != nil {
		t.Fatal(err)
	}
	f, err := x.service.EnsurePrepared(context.Background(), x.store, p)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func (x *humanFixture) signed(t *testing.T, f df.Frontier, index int, resolution df.Resolution, mutate func(*api.DecisionAnswerGrantClaims)) df.AnswerSubmission {
	t.Helper()
	q := f.Questions[index]
	sub := df.AnswerSubmission{TicketID: q.TicketID, WorkRevision: f.WorkRevision, QuestionVersion: q.Version, Resolution: resolution, Text: "Human choice"}
	claims := api.DecisionAnswerGrantClaims{
		Protocol: "gascity.decision-answer.v1", KeyID: "fixture", Issuer: "fixture-issuer", Subject: "human@example.test", Scope: api.DecisionAnswerScope,
		IssuedAt: x.now.Add(-time.Second).Unix(), ExpiresAt: x.now.Add(time.Minute).Unix(), TokenID: "fixture-" + q.ID,
		Challenge: df.AnswerChallenge{
			CityRef: f.CityRef, StoreRef: f.StoreRef, WorkID: f.WorkID, WorkRevision: f.WorkRevision, WorkDigest: f.WorkDigest,
			MapID: f.MapID, TicketID: q.TicketID, QuestionID: q.ID, QuestionVersion: q.Version, AnswerDigest: df.AnswerDigest(sub.Resolution, sub.Text), Resolution: sub.Resolution,
		},
	}
	if mutate != nil {
		mutate(&claims)
	}
	input, err := api.DecisionAnswerGrantSigningInput(claims)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	sub.Proof = base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(x.private, input))
	return sub
}

func TestHumanDomainSignedRoundsRestartReplayAndResume(t *testing.T) {
	x := newHumanFixture(t)
	f := x.ensure(t)
	if len(x.sessions.messages) != 1 || len(x.sessions.messages[0].Questions) != 1 || x.sessions.messages[0].Questions[0].ID != "first" || x.sessions.messages[0].Questions[0].Number != 1 {
		t.Fatalf("initial round: %+v", x.sessions.messages)
	}
	sub := x.signed(t, f, 0, df.ResolutionAnswered, nil)
	a, err := x.service.SubmitAnswer(context.Background(), x.store, x.scope, x.work.ID, sub)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Answers) != 1 || len(a.Frontier.OpenQuestions) != 1 || a.Frontier.OpenQuestions[0].ID != "later" {
		t.Fatalf("authorized frontier: %+v", a)
	}
	if len(x.sessions.messages) != 2 || x.sessions.messages[1].ID == x.sessions.messages[0].ID || x.sessions.messages[1].Questions[0].ID != "later" {
		t.Fatalf("downstream round: %+v", x.sessions.messages)
	}
	// Recompose the service over the persisted ledgers: no process-local cache
	// supplies answer authority, and exact replay cannot send another round.
	delivery, err := df.NewSessionPromptDelivery("review-desk", x.sessions, x.sessions)
	if err != nil {
		t.Fatal(err)
	}
	x.service = df.Service{Verifier: x.service.Verifier, Delivery: delivery}
	x.store = restoreHumanBackend(t, x.store, nil)
	x.ensure(t)
	if _, err := x.service.SubmitAnswer(context.Background(), x.store, x.scope, x.work.ID, sub); err != nil {
		t.Fatal(err)
	}
	if len(x.sessions.messages) != 2 {
		t.Fatal("duplicate replay delivered again")
	}
	sub2 := x.signed(t, a.Frontier, 1, df.ResolutionAnswered, nil)
	a, err = x.service.SubmitAnswer(context.Background(), x.store, x.scope, x.work.ID, sub2)
	if err != nil {
		t.Fatal(err)
	}
	r, err := x.service.CheckResume(context.Background(), x.store, x.scope, x.work.ID, x.revision, a.PhysicalRevision)
	if err != nil || !r.Eligible {
		t.Fatalf("resume: %+v, %v", r, err)
	}
	if _, err := x.service.CheckResume(context.Background(), x.store, x.scope, x.work.ID, x.revision, x.revision); !errors.Is(err, df.ErrStale) {
		t.Fatalf("stale resume: %v", err)
	}
	x.now = x.now.Add(10 * time.Minute)
	if _, err := x.service.ReadAuthorizedFrontier(context.Background(), x.store, x.scope, x.work.ID, x.revision); !errors.Is(err, df.ErrUnauthorized) {
		t.Fatalf("expired persisted proof: %v", err)
	}
}

type interruptedHumanStore struct {
	*beads.MemStore
	failCreate, failFinish bool
	resumeRace             bool
	resumeWorkID           string
}

func (s *interruptedHumanStore) StableCreateIDResolveTarget() beads.Store { return s.MemStore }
func (s *interruptedHumanStore) DecisionFrontierRecordWriterHandle() (beads.DecisionFrontierRecordWriter, bool) {
	return s, true
}

func (s *interruptedHumanStore) DecisionFrontierAtomicBackendHandle() (beads.DecisionFrontierAtomicBackend, bool) {
	return s, true
}

func (s *interruptedHumanStore) EnsureDecisionFrontierTargetBound(target beads.DecisionFrontierTargetBinding, operation func(beads.Store) error) error {
	if s.resumeRace {
		s.resumeRace = false
		title := "Concurrent backend edit"
		if err := s.Update(s.resumeWorkID, beads.UpdateOpts{Title: &title}); err != nil {
			return err
		}
	}
	return s.MemStore.EnsureDecisionFrontierTargetBound(target, operation)
}

func (s *interruptedHumanStore) ConditionalWriterHandle() (beads.ConditionalWriter, bool) {
	return s.MemStore, true
}

func (s *interruptedHumanStore) RevisionTransitionWriterHandle() (beads.RevisionTransitionWriter, bool) {
	return s.MemStore, true
}

func (s *interruptedHumanStore) DecisionFrontierSourceReaderHandle() (beads.DecisionFrontierSourceReader, bool) {
	return s.MemStore, true
}

func (s *interruptedHumanStore) RevisionTransitionReceiptReaderHandle() (beads.RevisionTransitionReceiptReader, bool) {
	return s.MemStore, true
}

func (s *interruptedHumanStore) CreateDecisionFrontierRecord(b beads.Bead) (beads.Bead, error) {
	if s.failCreate && b.Metadata[beadmeta.DecisionFrontierRecordMetadataKey] == "decision-frontier/answer/v1" {
		s.failCreate = false
		return beads.Bead{}, errors.New("injected answer creation failure")
	}
	return s.MemStore.CreateDecisionFrontierRecord(b)
}

func (s *interruptedHumanStore) CompareAndSetDecisionFrontierRecordMetadataKey(id, key, expected, next string) (bool, error) {
	if s.failFinish && strings.HasPrefix(expected, "answering:") && strings.HasPrefix(next, "answered:") {
		s.failFinish = false
		return false, errors.New("injected final CAS failure")
	}
	return s.MemStore.CompareAndSetDecisionFrontierRecordMetadataKey(id, key, expected, next)
}

func TestHumanDomainSignedAnswerRecoversInterruptedReservationAndCommit(t *testing.T) {
	for _, index := range []int{0, 1} {
		for _, finish := range []bool{false, true} {
			t.Run("question="+strconv.Itoa(index)+"/finish="+strconv.FormatBool(finish), func(t *testing.T) {
				x := newHumanFixture(t)
				f := x.ensure(t)
				if index == 1 {
					a, err := x.service.SubmitAnswer(context.Background(), x.store, x.scope, x.work.ID, x.signed(t, f, 0, df.ResolutionAnswered, nil))
					if err != nil {
						t.Fatal(err)
					}
					f = a.Frontier
				}
				store := &interruptedHumanStore{MemStore: x.store, failCreate: !finish, failFinish: finish}
				sub := x.signed(t, f, index, df.ResolutionAnswered, nil)
				if _, err := x.service.SubmitAnswer(context.Background(), store, x.scope, x.work.ID, sub); err == nil {
					t.Fatal("injected failure was ignored")
				}
				q, err := store.Get(sub.TicketID)
				if err != nil {
					t.Fatal(err)
				}
				if !strings.HasPrefix(q.Metadata[beadmeta.DecisionFrontierStateMetadataKey], "answering:") {
					t.Fatalf("reservation lost: %+v", q)
				}
				other := x.signed(t, f, index, df.ResolutionDeclined, nil)
				if _, err := x.service.SubmitAnswer(context.Background(), store, x.scope, x.work.ID, other); err == nil {
					t.Fatal("different signed answer replaced reservation")
				}
				a, err := x.service.SubmitAnswer(context.Background(), store, x.scope, x.work.ID, sub)
				if err != nil {
					t.Fatal(err)
				}
				if a.Frontier.Questions[index].Status != "answered" || len(x.sessions.messages) != 2 {
					t.Fatalf("exact recovery: %+v", a)
				}
			})
		}
	}
}

func TestHumanDomainLegacyAnswerCannotReleaseExpiredPrerequisite(t *testing.T) {
	x := newHumanFixture(t)
	f := x.ensure(t)
	a, err := x.service.SubmitAnswer(context.Background(), x.store, x.scope, x.work.ID, x.signed(t, f, 0, df.ResolutionAnswered, nil))
	if err != nil {
		t.Fatal(err)
	}
	x.now = x.now.Add(10 * time.Minute)
	// The new answer itself is fresh; only the persisted prerequisite expired.
	sub := x.signed(t, a.Frontier, 1, df.ResolutionAnswered, nil)
	if _, err := x.service.Answer(context.Background(), x.store, x.scope, x.work.ID, sub); !errors.Is(err, df.ErrUnauthorized) {
		t.Fatalf("legacy authority bypass: %v", err)
	}
	work, err := x.store.Get(x.work.ID)
	if err != nil || !beads.HasDecisionFrontierHold(work) {
		t.Fatalf("expired prerequisite released source: %+v, %v", work, err)
	}
}

func TestHumanDomainLegacyAnswerCannotReleaseRevokedPrerequisite(t *testing.T) {
	x := newHumanFixture(t)
	f := x.ensure(t)
	a, err := x.service.SubmitAnswer(context.Background(), x.store, x.scope, x.work.ID, x.signed(t, f, 0, df.ResolutionAnswered, nil))
	if err != nil {
		t.Fatal(err)
	}
	trust, err := api.NewPRHumanGrantVerifier(api.PRHumanTrustConfig{
		Keys:        []api.PRHumanGrantKey{{KeyID: "fixture", PublicKey: base64.StdEncoding.EncodeToString(x.private.Public().(ed25519.PublicKey))}},
		Authorities: []api.PRHumanAuthority{{KeyID: "fixture", Issuer: "fixture-issuer", Subject: "replacement@example.test", Scopes: []string{api.DecisionAnswerScope}}},
	}, nil, func() time.Time { return x.now })
	if err != nil {
		t.Fatal(err)
	}
	x.service.Verifier = api.NewDecisionAnswerGrantVerifier(trust)
	sub := x.signed(t, a.Frontier, 1, df.ResolutionAnswered, func(c *api.DecisionAnswerGrantClaims) { c.Subject = "replacement@example.test" })
	if _, err := x.service.Answer(context.Background(), x.store, x.scope, x.work.ID, sub); !errors.Is(err, df.ErrUnauthorized) {
		t.Fatalf("revoked prerequisite bypass: %v", err)
	}
	if _, err := x.service.ReadAuthorizedFrontier(context.Background(), x.store, x.scope, x.work.ID, x.revision); !errors.Is(err, df.ErrUnauthorized) {
		t.Fatalf("revoked readback authority: %v", err)
	}
	work, err := x.store.Get(x.work.ID)
	if err != nil || !beads.HasDecisionFrontierHold(work) {
		t.Fatal("revoked prerequisite released source")
	}
}

func TestHumanDomainBackendGenerationFenceDoesNotTrustSessionReader(t *testing.T) {
	x := newHumanFixture(t)
	p, err := x.service.PrepareProposal(context.Background(), x.store, x.scope, x.work.ID, x.revision, humanProposal())
	if err != nil {
		t.Fatal(err)
	}
	// Simulate another backend process. The injected Q52 reader deliberately
	// retains its stale generation and does not participate in session locks.
	if err := x.store.SetMetadata("session-human", "generation", "8"); err != nil {
		t.Fatal(err)
	}
	if _, err := x.service.EnsurePrepared(context.Background(), x.store, p); !errors.Is(err, df.ErrConflict) {
		t.Fatalf("backend generation fence: %v", err)
	}
	work, err := x.store.Get(x.work.ID)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := x.store.List(beads.ListQuery{Type: "gate", AllowScan: true})
	if err != nil {
		t.Fatal(err)
	}
	if beads.HasDecisionFrontierHold(work) || len(rows) != 0 || len(x.sessions.messages) != 0 {
		t.Fatal("stale backend target created/reserved/delivered")
	}
}

func TestHumanDomainReadbackRejectsBackendGenerationWithStaleQ52Reader(t *testing.T) {
	x := newHumanFixture(t)
	x.ensure(t)
	if err := x.store.SetMetadata("session-human", "generation", "8"); err != nil {
		t.Fatal(err)
	}
	if _, err := x.service.ReadAuthorizedFrontier(context.Background(), x.store, x.scope, x.work.ID, x.revision); !errors.Is(err, df.ErrConflict) {
		t.Fatalf("stale Q52 readback: %v", err)
	}
}

func TestHumanDomainAbsentOrUnknownRoundCannotCommitNewSignedAnswer(t *testing.T) {
	for _, mode := range []session.RequestDelivery{session.RequestDeliveryPending, session.RequestDeliveryUnknown} {
		for _, downstream := range []bool{false, true} {
			t.Run(string(mode)+"/downstream="+strconv.FormatBool(downstream), func(t *testing.T) {
				x := newHumanFixture(t)
				var f df.Frontier
				if downstream {
					f = x.ensure(t)
				}
				x.sessions.deliveryMode = mode
				if downstream {
					if _, err := x.service.SubmitAnswer(context.Background(), x.store, x.scope, x.work.ID, x.signed(t, f, 0, df.ResolutionAnswered, nil)); !errors.Is(err, df.ErrPromptDeliveryUnavailable) {
						t.Fatalf("downstream delivery outcome: %v", err)
					}
				} else {
					p, err := x.service.PrepareProposal(context.Background(), x.store, x.scope, x.work.ID, x.revision, humanProposal())
					if err != nil {
						t.Fatal(err)
					}
					if _, err := x.service.EnsurePrepared(context.Background(), x.store, p); !errors.Is(err, df.ErrPromptDeliveryUnavailable) {
						t.Fatalf("initial delivery outcome: %v", err)
					}
				}
				var err error
				f, err = x.service.Read(context.Background(), x.store, x.scope, x.work.ID, x.revision)
				if err != nil {
					t.Fatal(err)
				}
				index := 0
				if downstream {
					index = 1
				}
				sub := x.signed(t, f, index, df.ResolutionAnswered, nil)
				if _, err := x.service.Answer(context.Background(), x.store, x.scope, x.work.ID, sub); !errors.Is(err, df.ErrPromptDeliveryUnavailable) {
					t.Fatalf("uncovered signed answer: %v", err)
				}
				q, _ := x.store.Get(sub.TicketID)
				work, _ := x.store.Get(x.work.ID)
				if q.Metadata[beadmeta.DecisionFrontierStateMetadataKey] != "pending" || !beads.HasDecisionFrontierHold(work) {
					t.Fatal("undelivered answer was committed or source released")
				}
			})
		}
	}
}

func forceHumanSignedAnswer(t *testing.T, x *humanFixture, f df.Frontier, index int) {
	t.Helper()
	sub := x.signed(t, f, index, df.ResolutionAnswered, nil)
	answerDigest := df.AnswerDigest(sub.Resolution, sub.Text)
	sum := sha256.Sum256([]byte("answer\x00" + f.CityRef + "\x00" + f.StoreRef + "\x00" + sub.TicketID + "\x00" + answerDigest))
	id := "gcf-answer-" + hex.EncodeToString(sum[:16])
	proofSum := sha256.Sum256([]byte(sub.Proof))
	doc := struct {
		df.AnswerSubmission
		SchemaVersion int    `json:"schema_version"`
		CityRef       string `json:"city_ref"`
		StoreRef      string `json:"store_ref"`
		WorkID        string `json:"work_id"`
		WorkDigest    string `json:"work_digest"`
		MapID         string `json:"map_id"`
		QuestionID    string `json:"question_id"`
		Digest        string `json:"digest"`
		ProofDigest   string `json:"proof_digest"`
		Subject       string `json:"subject"`
		Issuer        string `json:"issuer"`
		KeyID         string `json:"key_id"`
	}{sub, 1, f.CityRef, f.StoreRef, f.WorkID, f.WorkDigest, f.MapID, f.Questions[index].ID, answerDigest, hex.EncodeToString(proofSum[:]), "human@example.test", "fixture-issuer", "fixture"}
	body, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := x.store.CreateDecisionFrontierRecord(beads.Bead{
		ID: id, Type: "gate", Title: "Verified answer for " + sub.TicketID, Description: string(body),
		Metadata: map[string]string{beadmeta.DecisionFrontierRecordMetadataKey: "decision-frontier/answer/v1", beadmeta.DecisionFrontierStateMetadataKey: "recorded"},
	}); err != nil {
		t.Fatal(err)
	}
	for _, step := range [][2]string{{"pending", "answering:" + id}, {"answering:" + id, "answered:" + id}} {
		if changed, err := x.store.CompareAndSetDecisionFrontierRecordMetadataKey(sub.TicketID, beadmeta.DecisionFrontierStateMetadataKey, step[0], step[1]); err != nil || !changed {
			t.Fatalf("fixture protected answer transition: %v", err)
		}
	}
}

func TestHumanDomainNativeReleaseRejectsMissingRoundAndChangedTarget(t *testing.T) {
	for _, failure := range []string{"missing", "unknown", "target", "version", "binding"} {
		t.Run(failure, func(t *testing.T) {
			x := newHumanFixture(t)
			f := x.ensure(t)
			missing := failure == "missing" || failure == "unknown"
			if missing {
				x.sessions.deliveryMode = session.RequestDeliveryPending
				if failure == "unknown" {
					x.sessions.deliveryMode = session.RequestDeliveryUnknown
				}
			}
			_, err := x.service.SubmitAnswer(context.Background(), x.store, x.scope, x.work.ID, x.signed(t, f, 0, df.ResolutionAnswered, nil))
			if missing && !errors.Is(err, df.ErrPromptDeliveryUnavailable) || !missing && err != nil {
				t.Fatalf("first answer/downstream round: %v", err)
			}
			f, err = x.service.Read(context.Background(), x.store, x.scope, x.work.ID, x.revision)
			if err != nil {
				t.Fatal(err)
			}
			// Exercise the protected backend directly with genuinely signed answer
			// records, bypassing the domain coverage check as a hostile controller
			// writer would. The joint-ledger release guard must still refuse.
			forceHumanSignedAnswer(t, x, f, 1)
			if changed, err := x.store.CompareAndSetDecisionFrontierRecordMetadataKey(f.MapID, beadmeta.DecisionFrontierStateMetadataKey, "pending", "resolved"); err != nil || !changed {
				t.Fatalf("fixture map finalize: %v", err)
			}
			if failure == "target" {
				if err := x.store.SetMetadata("session-human", "generation", "8"); err != nil {
					t.Fatal(err)
				}
			}
			if failure == "version" || failure == "binding" {
				x.store = restoreHumanBackend(t, x.store, func(row *beads.Bead) {
					if row.Metadata[beadmeta.DecisionFrontierRecordMetadataKey] != "decision-frontier/prompt/v1" || row.ID == f.Prompt.ID {
						return
					}
					if failure == "version" {
						row.Description = strings.Replace(row.Description, `"version":"`+f.Questions[1].Version+`"`, `"version":"wrong-version"`, 1)
					} else {
						row.Description = strings.Replace(row.Description, `"execution_generation":7`, `"execution_generation":8`, 1)
					}
				})
			}
			work, _ := x.store.Get(x.work.ID)
			mapBead, _ := x.store.Get(f.MapID)
			var doc struct {
				ReleaseID string `json:"release_id"`
			}
			if err := json.Unmarshal([]byte(mapBead.Description), &doc); err != nil {
				t.Fatal(err)
			}
			receipt := beads.RevisionTransitionReceipt{ID: doc.ReleaseID, CityRef: f.CityRef, StoreRef: f.StoreRef, WorkID: f.WorkID, MapID: f.MapID, Operation: "release", FromRevision: work.Revision}
			if _, changed, err := x.store.CompareAndSetMetadataKeyWithReceipt(work.ID, beadmeta.DecisionFrontierHoldMetadataKey, work.Metadata[beadmeta.DecisionFrontierHoldMetadataKey], "", work.Revision, receipt); err == nil || changed {
				t.Fatalf("direct native release bypass: changed=%v err=%v", changed, err)
			}
			current, _ := x.store.Get(work.ID)
			if current.Revision != work.Revision || !beads.HasDecisionFrontierHold(current) {
				t.Fatal("rejected release changed source")
			}
		})
	}
}

func TestHumanDomainConditionalResumeChecksBackendDependenciesWithoutMutation(t *testing.T) {
	x := newHumanFixture(t)
	dependency, err := x.store.Create(beads.Bead{ID: "dependency", Type: "task", Title: "Dependency"})
	if err != nil {
		t.Fatal(err)
	}
	if err := x.store.DepAdd(x.work.ID, dependency.ID, "blocks"); err != nil {
		t.Fatal(err)
	}
	x.work, _ = x.store.Get(x.work.ID)
	x.revision, _ = df.WorkRevision(x.work)
	f := x.ensure(t)
	a, err := x.service.SubmitAnswer(context.Background(), x.store, x.scope, x.work.ID, x.signed(t, f, 0, df.ResolutionAnswered, nil))
	if err != nil {
		t.Fatal(err)
	}
	a, err = x.service.SubmitAnswer(context.Background(), x.store, x.scope, x.work.ID, x.signed(t, a.Frontier, 1, df.ResolutionAnswered, nil))
	if err != nil {
		t.Fatal(err)
	}
	r, err := x.service.CheckResume(context.Background(), x.store, x.scope, x.work.ID, x.revision, a.PhysicalRevision)
	if err != nil || r.Eligible || r.Reason != "source work has blocking dependencies" {
		t.Fatalf("open backend dependency: %+v, %v", r, err)
	}
	if err := x.store.Close(dependency.ID); err != nil {
		t.Fatal(err)
	}
	r, err = x.service.CheckResume(context.Background(), x.store, x.scope, x.work.ID, x.revision, a.PhysicalRevision)
	if err != nil || !r.Eligible {
		t.Fatalf("closed backend dependency: %+v, %v", r, err)
	}
	work, err := x.store.Get(x.work.ID)
	if err != nil || strconv.FormatInt(work.Revision, 10) != a.PhysicalRevision {
		t.Fatal("eligibility observation mutated source")
	}
	racing := &interruptedHumanStore{MemStore: x.store, resumeRace: true, resumeWorkID: x.work.ID}
	if _, err := x.service.CheckResume(context.Background(), racing, x.scope, x.work.ID, x.revision, a.PhysicalRevision); !errors.Is(err, df.ErrStale) {
		t.Fatalf("backend race after authenticated readback: %v", err)
	}
}

func TestHumanDomainHeldAnsweredPrerequisiteBlocksDeliveryAndProtectedRelease(t *testing.T) {
	x := newHumanFixture(t)
	f := x.ensure(t)
	a, err := x.service.SubmitAnswer(context.Background(), x.store, x.scope, x.work.ID, x.signed(t, f, 0, df.ResolutionAnswered, nil))
	if err != nil {
		t.Fatal(err)
	}
	rows, err := x.store.List(beads.ListQuery{AllowScan: true, IncludeClosed: true})
	if err != nil {
		t.Fatal(err)
	}
	var deps []beads.Dep
	for i, row := range rows {
		edges, err := x.store.DepList(row.ID, "down")
		if err != nil {
			t.Fatal(err)
		}
		deps = append(deps, edges...)
		if row.ID == f.Questions[0].TicketID {
			rows[i].Labels = append(rows[i].Labels, "hold:external")
		}
	}
	// Restore a durable backend containing an explicit held answered ticket.
	x.store = beads.NewMemStoreFrom(100, rows, deps)
	x.store.HonorExplicitIDs = true
	held, err := x.service.ReadAuthorizedFrontier(context.Background(), x.store, x.scope, x.work.ID, x.revision)
	if err != nil {
		t.Fatal(err)
	}
	if held.Frontier.State != df.StatePending || held.Frontier.Questions[0].Status != "held" || len(held.Frontier.OpenQuestions) != 0 {
		t.Fatalf("held prerequisite readiness: %+v", held)
	}
	x.ensure(t)
	if len(x.sessions.messages) != 2 {
		t.Fatal("held answered prerequisite caused another downstream delivery")
	}
	if _, err := x.service.Answer(context.Background(), x.store, x.scope, x.work.ID, x.signed(t, a.Frontier, 1, df.ResolutionAnswered, nil)); !errors.Is(err, df.ErrStale) {
		t.Fatalf("held prerequisite answer: %v", err)
	}
	work, _ := x.store.Get(x.work.ID)
	mapBead, _ := x.store.Get(f.MapID)
	var doc struct {
		ReleaseID string `json:"release_id"`
	}
	if err := json.Unmarshal([]byte(mapBead.Description), &doc); err != nil {
		t.Fatal(err)
	}
	receipt := beads.RevisionTransitionReceipt{ID: doc.ReleaseID, CityRef: f.CityRef, StoreRef: f.StoreRef, WorkID: f.WorkID, MapID: f.MapID, Operation: "release", FromRevision: work.Revision}
	if _, changed, err := x.store.CompareAndSetMetadataKeyWithReceipt(work.ID, beadmeta.DecisionFrontierHoldMetadataKey, work.Metadata[beadmeta.DecisionFrontierHoldMetadataKey], "", work.Revision, receipt); err == nil || changed {
		t.Fatalf("backend released held prerequisite: changed=%v err=%v", changed, err)
	}
}

func TestHumanDomainPreparationFencesGenerationBeforeCreation(t *testing.T) {
	x := newHumanFixture(t)
	p, err := x.service.PrepareProposal(context.Background(), x.store, x.scope, x.work.ID, x.revision, humanProposal())
	if err != nil {
		t.Fatal(err)
	}
	x.sessions.generation++
	if _, err := x.service.EnsurePrepared(context.Background(), x.store, p); !errors.Is(err, df.ErrConflict) {
		t.Fatalf("generation fence: %v", err)
	}
	rows, err := x.store.List(beads.ListQuery{Type: "gate", AllowScan: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 || len(x.sessions.messages) != 0 {
		t.Fatal("failed preparation created or delivered records")
	}
}

func TestHumanDomainPreparationRejectsChangedCompositionAndMissingContracts(t *testing.T) {
	x := newHumanFixture(t)
	p, err := x.service.PrepareProposal(context.Background(), x.store, x.scope, x.work.ID, x.revision, humanProposal())
	if err != nil {
		t.Fatal(err)
	}
	otherDelivery, err := df.NewSessionPromptDelivery("other-desk", x.sessions, x.sessions)
	if err != nil {
		t.Fatal(err)
	}
	changed := df.Service{Verifier: x.service.Verifier, Delivery: otherDelivery}
	if _, err := changed.EnsurePrepared(context.Background(), x.store, p); !errors.Is(err, df.ErrUnavailable) {
		t.Fatalf("changed target: %v", err)
	}
	otherStore := beads.NewMemStore()
	otherStore.HonorExplicitIDs = true
	if _, err := x.service.EnsurePrepared(context.Background(), otherStore, p); !errors.Is(err, df.ErrUnavailable) {
		t.Fatalf("changed physical store: %v", err)
	}
	if _, err := (df.Service{}).PrepareProposal(context.Background(), x.store, x.scope, x.work.ID, x.revision, humanProposal()); !errors.Is(err, df.ErrUnavailable) {
		t.Fatalf("default off: %v", err)
	}
	if _, err := x.service.EnsurePrepared(context.Background(), x.store, df.PreparedProposal{}); !errors.Is(err, df.ErrUnavailable) {
		t.Fatalf("missing preparation: %v", err)
	}
	x.store.DisableConditionalWrites = true
	if _, err := x.service.PrepareProposal(context.Background(), x.store, x.scope, x.work.ID, x.revision, humanProposal()); !errors.Is(err, df.ErrUnavailable) {
		t.Fatalf("missing protected backend: %v", err)
	}
	if len(x.sessions.messages) != 0 {
		t.Fatal("unavailable contracts sent a prompt")
	}
}

func TestHumanDomainReadRejectsChangedSessionEvidenceAndSignedPayload(t *testing.T) {
	x := newHumanFixture(t)
	f := x.ensure(t)
	sub := x.signed(t, f, 0, df.ResolutionAnswered, nil)
	sub.Text = "Worker replacement"
	if _, err := x.service.SubmitAnswer(context.Background(), x.store, x.scope, x.work.ID, sub); !errors.Is(err, df.ErrUnauthorized) {
		t.Fatalf("changed signed payload: %v", err)
	}
	sub = x.signed(t, f, 0, df.ResolutionAnswered, nil)
	sub.QuestionVersion = "changed"
	if _, err := x.service.SubmitAnswer(context.Background(), x.store, x.scope, x.work.ID, sub); !errors.Is(err, df.ErrStale) {
		t.Fatalf("changed question version: %v", err)
	}
	x.sessions.generation++
	if _, err := x.service.ReadAuthorizedFrontier(context.Background(), x.store, x.scope, x.work.ID, x.revision); !errors.Is(err, df.ErrConflict) {
		t.Fatalf("wrong persisted generation: %v", err)
	}
	if len(x.sessions.messages) != 1 {
		t.Fatal("readback performed delivery")
	}
}

func TestHumanDomainHeldAnswerTransactionIsExcludedFromRound(t *testing.T) {
	x := newHumanFixture(t)
	f := x.ensure(t)
	changed, err := x.store.CompareAndSetDecisionFrontierRecordMetadataKey(f.Questions[0].TicketID, beadmeta.DecisionFrontierStateMetadataKey, "pending", "answering:reserved-answer")
	if err != nil || !changed {
		t.Fatalf("hold exact answer transaction: %v", err)
	}
	a, err := x.service.ReadAuthorizedFrontier(context.Background(), x.store, x.scope, x.work.ID, x.revision)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Frontier.OpenQuestions) != 0 || a.Frontier.Questions[0].Status != "held" {
		t.Fatalf("held question leaked into round: %+v", a.Frontier)
	}
	x.ensure(t)
	if len(x.sessions.messages) != 1 {
		t.Fatal("held question was redelivered")
	}
}

func TestHumanDomainResumePreservesIndependentSourceHold(t *testing.T) {
	x := newHumanFixture(t)
	// An existing source hold is separate from the protected decision hold.
	if err := x.store.Update(x.work.ID, beads.UpdateOpts{Labels: []string{"hold:external"}}); err != nil {
		t.Fatal(err)
	}
	x.work, _ = x.store.Get(x.work.ID)
	x.revision, _ = df.WorkRevision(x.work)
	f := x.ensure(t)
	a, err := x.service.SubmitAnswer(context.Background(), x.store, x.scope, x.work.ID, x.signed(t, f, 0, df.ResolutionAnswered, nil))
	if err != nil {
		t.Fatal(err)
	}
	a, err = x.service.SubmitAnswer(context.Background(), x.store, x.scope, x.work.ID, x.signed(t, a.Frontier, 1, df.ResolutionAnswered, nil))
	if err != nil {
		t.Fatal(err)
	}
	r, err := x.service.CheckResume(context.Background(), x.store, x.scope, x.work.ID, x.revision, a.PhysicalRevision)
	if err != nil || r.Eligible || r.Reason != "source work has an independent hold" {
		t.Fatalf("independent hold: %+v, %v", r, err)
	}
}

func TestHumanDomainSignedPolicyRejectsCrossScopeAndKeepsNonAnswersPending(t *testing.T) {
	for _, resolution := range []df.Resolution{df.ResolutionDeclined, df.ResolutionDeferred, df.ResolutionUnclear, df.ResolutionPartial} {
		t.Run(string(resolution), func(t *testing.T) {
			x := newHumanFixture(t)
			f := x.ensure(t)
			bad := x.signed(t, f, 0, df.ResolutionAnswered, func(c *api.DecisionAnswerGrantClaims) { c.Challenge.StoreRef = "rig:other" })
			if _, err := x.service.SubmitAnswer(context.Background(), x.store, x.scope, x.work.ID, bad); !errors.Is(err, df.ErrUnauthorized) {
				t.Fatalf("cross scope: %v", err)
			}
			sub := x.signed(t, f, 0, resolution, nil)
			a, err := x.service.SubmitAnswer(context.Background(), x.store, x.scope, x.work.ID, sub)
			if err != nil {
				t.Fatal(err)
			}
			if a.Frontier.State != df.StatePending || len(a.Frontier.OpenQuestions) != 1 || a.Frontier.OpenQuestions[0].ID != "first" || len(x.sessions.messages) != 1 {
				t.Fatalf("non-answer unlocked dependent: %+v", a)
			}
		})
	}
}
