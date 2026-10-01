package api

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/decisionfrontier"
	"github.com/gastownhall/gascity/internal/session"
)

type humanSourceSessionFixture struct {
	info     session.Info
	receipts map[string]session.RequestReceipt
	messages []string
	now      time.Time
}

func (f *humanSourceSessionFixture) ResolveID(target string) (string, error) {
	if target != f.info.ConfiguredNamedIdentity {
		return "", session.ErrSessionNotFound
	}
	return f.info.ID, nil
}

func (f *humanSourceSessionFixture) GetPersistedResponse(id string) (session.Info, session.PersistedResponse, error) {
	if id != f.info.ID {
		return session.Info{}, session.PersistedResponse{}, session.ErrSessionNotFound
	}
	return f.info, session.PersistedResponse{Status: "open"}, nil
}

func (f *humanSourceSessionFixture) GetRequest(id, request string) (session.RequestReceipt, error) {
	if id != f.info.ID {
		return session.RequestReceipt{}, session.ErrSessionNotFound
	}
	r, ok := f.receipts[request]
	if !ok {
		return r, session.ErrRequestNotFound
	}
	return r, nil
}

func (f *humanSourceSessionFixture) SubmitRequest(_ context.Context, id, request string, generation int, message string) (session.RequestReceipt, error) {
	if id != f.info.ID || strconv.Itoa(generation) != f.info.Generation {
		return session.RequestReceipt{}, session.ErrRequestConflict
	}
	sha := sha256.Sum256([]byte(message))
	if r, ok := f.receipts[request]; ok {
		if r.MessageDigest != hex.EncodeToString(sha[:]) {
			return session.RequestReceipt{}, session.ErrRequestConflict
		}
		return r, nil
	}
	r := session.RequestReceipt{SessionID: id, RequestID: request, Generation: generation, MessageDigest: hex.EncodeToString(sha[:]), AcceptedAt: f.now, ProviderResultAt: &f.now, Delivery: session.RequestDeliveryAccepted, Effect: "unverified"}
	f.receipts[request] = r
	f.messages = append(f.messages, message)
	return r, nil
}

type humanSourceHTTPFixture struct {
	state     *fakeState
	store     *beads.MemStore
	wrapped   *decisionFrontierState
	handler   http.Handler
	sessions  *humanSourceSessionFixture
	workerKey ed25519.PrivateKey
	humanKey  ed25519.PrivateKey
	now       time.Time
	sequence  int
}

func newHumanSourceHTTPFixture(t *testing.T) *humanSourceHTTPFixture {
	t.Helper()
	now := time.Date(2026, 9, 30, 10, 0, 0, 0, time.UTC)
	humanKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{71}, ed25519.SeedSize))
	workerKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{72}, ed25519.SeedSize))
	humanTrust, err := NewPRHumanGrantVerifier(PRHumanTrustConfig{
		Keys:        []PRHumanGrantKey{{KeyID: "answer-key", PublicKey: base64.StdEncoding.EncodeToString(humanKey.Public().(ed25519.PublicKey))}},
		Authorities: []PRHumanAuthority{{KeyID: "answer-key", Issuer: "city-governance", Subject: "reviewer@example.test", Scopes: []string{DecisionAnswerScope}}},
	}, map[string]ed25519.PublicKey{"k1": workerKey.Public().(ed25519.PublicKey)}, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	sessions := &humanSourceSessionFixture{
		info:     session.Info{ID: "session-human-source", State: session.StateActive, MetadataState: string(session.StateActive), Generation: "7", ConfiguredNamedSession: true, ConfiguredNamedIdentity: "review-coordinator"},
		receipts: make(map[string]session.RequestReceipt), now: now,
	}
	delivery, err := decisionfrontier.NewSessionPromptDelivery("review-coordinator", sessions, sessions)
	if err != nil {
		t.Fatal(err)
	}
	service := decisionfrontier.Service{Verifier: NewDecisionAnswerGrantVerifier(humanTrust), Delivery: delivery}
	state, store, _ := setupDecisionFrontierAPI(t, service)
	if _, err := store.Create(beads.Bead{ID: sessions.info.ID, Type: session.BeadType, Title: "Review coordinator session", Labels: []string{session.LabelSession}, Metadata: map[string]string{
		"state": string(session.StateActive), "generation": sessions.info.Generation,
		"alias":                    sessions.info.ConfiguredNamedIdentity,
		"configured_named_session": "true", "configured_named_identity": sessions.info.ConfiguredNamedIdentity,
	}}); err != nil {
		t.Fatal(err)
	}
	f := &humanSourceHTTPFixture{state: state, store: store, wrapped: &decisionFrontierState{State: state, service: service}, sessions: sessions, workerKey: workerKey, humanKey: humanKey, now: now}
	f.restart(t)
	return f
}

func (f *humanSourceHTTPFixture) restart(t *testing.T) {
	t.Helper()
	pub := f.workerKey.Public().(ed25519.PublicKey)
	f.handler = readAuthMiddleware(newTestReadVerifier(t, pub, f.now), writeAuthMiddleware(newTestWriteVerifier(t, pub, f.now), false, newTestCityHandler(t, f.wrapped)))
}

func (f *humanSourceHTTPFixture) request(t *testing.T, method, tail, key string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var encoded []byte
	var err error
	if body != nil {
		encoded, err = json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
	}
	path := cityURL(f.state, tail)
	req := httptest.NewRequest(method, path, bytes.NewReader(encoded))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(csrfHeaderName, "true")
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	f.sequence++
	jti := fmt.Sprintf("human-source-request-%d", f.sequence)
	if method == http.MethodGet {
		req.Header.Set(readAuthHeader, mintToken(t, f.workerKey, readGrant(f.now, f.state.CityName(), method, req.URL.Path, req.URL.RawQuery, jti)))
	} else {
		req.Header.Set(writeAuthHeader, mintToken(t, f.workerKey, grantFor(f.now, f.state.CityName(), method, req.URL.Path, encoded, jti)))
	}
	rec := httptest.NewRecorder()
	f.handler.ServeHTTP(rec, req)
	return rec
}

func decodeHumanSource[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("HTTP %d: %s", rec.Code, rec.Body.String())
	}
	var value T
	if err := json.Unmarshal(rec.Body.Bytes(), &value); err != nil {
		t.Fatal(err)
	}
	return value
}

func (f *humanSourceHTTPFixture) signedAnswer(t *testing.T, frontier decisionfrontier.Frontier, index int, resolution decisionfrontier.Resolution) decisionfrontier.AnswerSubmission {
	t.Helper()
	q := frontier.Questions[index]
	sub := decisionfrontier.AnswerSubmission{TicketID: q.TicketID, WorkRevision: frontier.WorkRevision, QuestionVersion: q.Version, Resolution: resolution, Text: "Explicit human decision."}
	challenge := decisionfrontier.AnswerChallenge{CityRef: frontier.CityRef, StoreRef: frontier.StoreRef, WorkID: frontier.WorkID, WorkRevision: frontier.WorkRevision, WorkDigest: frontier.WorkDigest, MapID: frontier.MapID, TicketID: q.TicketID, QuestionID: q.ID, QuestionVersion: q.Version, AnswerDigest: decisionfrontier.AnswerDigest(sub.Resolution, sub.Text), Resolution: sub.Resolution}
	sub.Proof = signDecisionAnswerClaims(t, f.humanKey, decisionAnswerTestClaims(challenge, f.now))
	return sub
}

func TestHumanSourceHTTPPrepareFencesGenerationBeforeCreation(t *testing.T) {
	f := newHumanSourceHTTPFixture(t)
	work, _ := f.store.Get("wrk-frontier-api")
	revision, _ := decisionfrontier.WorkRevision(work)
	base := "/bead/" + work.ID + "/decision-frontier"
	proposal := DecisionFrontierEnsureRequest{WorkRevision: revision, Proposal: decisionfrontier.Proposal{Questions: []decisionfrontier.Question{{ID: "first", Title: "First", Prompt: "First question"}}}}
	p := decodeHumanSource[HumanSourcePreparation](t, f.request(t, http.MethodPost, base+"/prepare", "", proposal))
	if p.Binding.ExecutionGeneration != 7 || p.Binding.SessionID != f.sessions.info.ID || p.TargetName != f.sessions.info.ConfiguredNamedIdentity {
		t.Fatalf("wrong preparation: %+v", p)
	}
	// The delivery reader remains stale. Only the backend's actual persisted
	// session generation changes, modeling an out-of-process session writer.
	if err := f.store.SetMetadata(f.sessions.info.ID, "generation", "8"); err != nil {
		t.Fatal(err)
	}
	rec := f.request(t, http.MethodPost, base+"/prepared", "ensure-1", HumanSourceEnsureRequest{PreparationToken: p.PreparationToken})
	if rec.Code != http.StatusConflict {
		t.Fatalf("stale target status=%d %s", rec.Code, rec.Body.String())
	}
	rows, _ := f.store.List(beads.ListQuery{Type: "gate", IncludeClosed: true, AllowScan: true})
	if len(rows) != 0 || len(f.sessions.messages) != 0 {
		t.Fatalf("stale preparation created records/messages: %d/%d", len(rows), len(f.sessions.messages))
	}
	f.restart(t)
	rec = f.request(t, http.MethodPost, base+"/prepared", "ensure-1", HumanSourceEnsureRequest{PreparationToken: p.PreparationToken})
	if rec.Code != http.StatusConflict {
		t.Fatalf("restart accepted lost preparation: %d", rec.Code)
	}
}

func TestHumanSourceHTTPIndependentRoundsSignedReadbackRestartAndResume(t *testing.T) {
	f := newHumanSourceHTTPFixture(t)
	work, _ := f.store.Get("wrk-frontier-api")
	revision, _ := decisionfrontier.WorkRevision(work)
	base := "/bead/" + work.ID + "/decision-frontier"
	proposal := DecisionFrontierEnsureRequest{WorkRevision: revision, Proposal: decisionfrontier.Proposal{Questions: []decisionfrontier.Question{
		{ID: "first", Title: "First", Prompt: "First question"}, {ID: "dependent", Title: "Next", Prompt: "Hidden until approved", DependsOn: []string{"first"}},
	}}}
	p := decodeHumanSource[HumanSourcePreparation](t, f.request(t, http.MethodPost, base+"/prepare", "", proposal))
	frontier := decodeHumanSource[decisionfrontier.Frontier](t, f.request(t, http.MethodPost, base+"/prepared", "ensure-1", HumanSourceEnsureRequest{PreparationToken: p.PreparationToken}))
	decodeHumanSource[decisionfrontier.Frontier](t, f.request(t, http.MethodPost, base+"/prepared", "ensure-1", HumanSourceEnsureRequest{PreparationToken: p.PreparationToken}))
	if len(f.sessions.messages) != 1 || strings.Contains(f.sessions.messages[0], "Hidden until approved") || !strings.Contains(f.sessions.messages[0], `"number":1`) {
		t.Fatalf("first round wrong: %+v", f.sessions.messages)
	}
	firstAnswer := f.signedAnswer(t, frontier, 0, decisionfrontier.ResolutionAnswered)
	if rec := f.request(t, http.MethodPost, base+"/authorized/answers", "held-answer", f.signedAnswer(t, frontier, 1, decisionfrontier.ResolutionAnswered)); rec.Code != http.StatusConflict {
		t.Fatalf("dependency-blocked answer accepted: %d %s", rec.Code, rec.Body.String())
	}
	challenge := decisionfrontier.AnswerChallenge{CityRef: "city:other", StoreRef: frontier.StoreRef, WorkID: frontier.WorkID, WorkRevision: revision, WorkDigest: frontier.WorkDigest, MapID: frontier.MapID, TicketID: firstAnswer.TicketID, QuestionID: "first", QuestionVersion: firstAnswer.QuestionVersion, AnswerDigest: decisionfrontier.AnswerDigest(firstAnswer.Resolution, firstAnswer.Text), Resolution: firstAnswer.Resolution}
	crossScope := firstAnswer
	crossScope.Proof = signDecisionAnswerClaims(t, f.humanKey, decisionAnswerTestClaims(challenge, f.now))
	if rec := f.request(t, http.MethodPost, base+"/authorized/answers", "cross-scope-answer", crossScope); rec.Code != http.StatusForbidden {
		t.Fatalf("cross-scope signature accepted: %d %s", rec.Code, rec.Body.String())
	}
	challenge.CityRef = frontier.CityRef
	workerAnswer := firstAnswer
	workerAnswer.Proof = signDecisionAnswerClaims(t, f.workerKey, decisionAnswerTestClaims(challenge, f.now))
	if rec := f.request(t, http.MethodPost, base+"/authorized/answers", "worker-answer", workerAnswer); rec.Code != http.StatusForbidden {
		t.Fatalf("worker key authorized a human answer: %d %s", rec.Code, rec.Body.String())
	}
	bad := firstAnswer
	bad.WorkRevision = "999"
	if rec := f.request(t, http.MethodPost, base+"/authorized/answers", "bad-answer", bad); rec.Code != http.StatusConflict && rec.Code != http.StatusNotFound {
		t.Fatalf("stale answer: %d %s", rec.Code, rec.Body.String())
	}
	unsigned := postDecisionFrontier(t, newTestCityHandler(t, f.wrapped), cityURL(f.state, base+"/answers"), "answer-unsigned-original", firstAnswer)
	if unsigned.Code != http.StatusForbidden {
		t.Fatalf("original answer route bypassed protected request authentication: %d %s", unsigned.Code, unsigned.Body.String())
	}
	decodeHumanSource[decisionfrontier.Frontier](t, f.request(t, http.MethodPost, base+"/answers", "answer-1", firstAnswer))
	readback := decodeHumanSource[decisionfrontier.AuthorizedFrontier](t, f.request(t, http.MethodGet, base+"/authorized?work_revision="+revision, "", nil))
	if readback.Frontier.Prompt.ID != readback.Session.Binding.RequestID || readback.Frontier.Prompt.ID == frontier.Prompt.ID {
		t.Fatalf("downstream readback reused the initial delivery identity: %+v", readback.Session)
	}
	answerID := readback.Frontier.Questions[0].Answer.ID
	for _, tail := range []string{"/bead/" + answerID, "/bead/" + answerID + "/deps", "/beads/graph/" + answerID} {
		rec := f.request(t, http.MethodGet, tail, "", nil)
		if rec.Code != http.StatusNotFound || strings.Contains(rec.Body.String(), firstAnswer.Proof) {
			t.Fatalf("private answer proof exposed at %s: %d %s", tail, rec.Code, rec.Body.String())
		}
	}
	for _, tail := range []string{"/beads?type=gate", "/bead/" + work.ID + "/deps", "/beads/graph/" + work.ID} {
		rec := f.request(t, http.MethodGet, tail, "", nil)
		if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), firstAnswer.Proof) {
			t.Fatalf("generic evidence projection at %s: %d %s", tail, rec.Code, rec.Body.String())
		}
	}
	decodeHumanSource[decisionfrontier.AuthorizedFrontier](t, f.request(t, http.MethodPost, base+"/authorized/answers", "answer-1", firstAnswer))
	changedAnswer := firstAnswer
	changedAnswer.Text = "Changed replay payload."
	if rec := f.request(t, http.MethodPost, base+"/authorized/answers", "answer-1", changedAnswer); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("changed answer replay accepted: %d %s", rec.Code, rec.Body.String())
	}
	if len(f.sessions.messages) != 2 || !strings.Contains(f.sessions.messages[1], "Hidden until approved") {
		t.Fatalf("downstream round missing: %+v", f.sessions.messages)
	}
	f.restart(t)
	decodeHumanSource[decisionfrontier.AuthorizedFrontier](t, f.request(t, http.MethodGet, base+"/authorized?work_revision="+revision, "", nil))
	work, _ = f.store.Get(work.ID)
	if work.Metadata[beadmeta.DecisionFrontierHoldMetadataKey] == "" {
		t.Fatal("dependent question lost durable hold")
	}
	decodeHumanSource[decisionfrontier.AuthorizedFrontier](t, f.request(t, http.MethodPost, base+"/authorized/answers", "answer-2", f.signedAnswer(t, frontier, 1, decisionfrontier.ResolutionAnswered)))
	work, _ = f.store.Get(work.ID)
	physical, _ := decisionfrontier.WorkRevision(work)
	resume := decodeHumanSource[decisionfrontier.ResumeEligibility](t, f.request(t, http.MethodPost, base+"/check-resume", "", HumanSourceResumeRequest{FrontierRevision: revision, PhysicalRevision: physical}))
	if !resume.Eligible || resume.MapID != frontier.MapID || resume.PhysicalRevision != physical {
		t.Fatalf("exact signed-answer resume evidence: %+v", resume)
	}
	if rec := f.request(t, http.MethodPost, base+"/check-resume", "", HumanSourceResumeRequest{FrontierRevision: revision, PhysicalRevision: "999"}); rec.Code != http.StatusConflict {
		t.Fatalf("stale resume: %d %s", rec.Code, rec.Body.String())
	}
	if work.Metadata[beadmeta.DecisionFrontierHoldMetadataKey] != "" {
		t.Fatal("authorized exact answers did not release frontier hold")
	}
	if err := f.store.SetMetadata(f.sessions.info.ID, "generation", "8"); err != nil {
		t.Fatal(err)
	}
	if rec := f.request(t, http.MethodPost, base+"/check-resume", "", HumanSourceResumeRequest{FrontierRevision: revision, PhysicalRevision: physical}); rec.Code != http.StatusConflict {
		t.Fatalf("stale session reader authorized resume after backend generation change: %d %s", rec.Code, rec.Body.String())
	}
	unchanged, _ := f.store.Get(work.ID)
	if unchanged.Revision != work.Revision {
		t.Fatal("resume eligibility check changed source revision")
	}
}

func TestHumanSourceHTTPAlwaysRequiresAuthenticatedPrincipal(t *testing.T) {
	state, _, handler := setupDecisionFrontierAPI(t, decisionfrontier.Service{})
	base := cityURL(state, "/bead/wrk-frontier-api/decision-frontier")
	for _, request := range []struct {
		tail string
		body any
	}{
		{"/prepare", DecisionFrontierEnsureRequest{WorkRevision: "1", Proposal: decisionfrontier.Proposal{Questions: []decisionfrontier.Question{{ID: "q", Title: "Q", Prompt: "Q"}}}}},
		{"/prepared", HumanSourceEnsureRequest{PreparationToken: "opaque"}},
		{"/authorized/answers", decisionfrontier.AnswerSubmission{TicketID: "ticket", WorkRevision: "1", QuestionVersion: "version", Resolution: decisionfrontier.ResolutionAnswered, Text: "Answer", Proof: "opaque"}},
		{"/check-resume", HumanSourceResumeRequest{FrontierRevision: "1", PhysicalRevision: "1"}},
	} {
		rec := postDecisionFrontier(t, handler, base+request.tail, "key", request.body)
		if rec.Code != http.StatusForbidden {
			t.Fatalf("unauthenticated route %s: %d %s", request.tail, rec.Code, rec.Body.String())
		}
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, base+"/authorized?work_revision=1", nil))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("unhardened listener accepted source readback: %d %s", rec.Code, rec.Body.String())
	}
}
