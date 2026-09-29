package api

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/decisionfrontier"
)

type decisionFrontierState struct {
	State
	service decisionfrontier.Service
}

func (s *decisionFrontierState) DecisionFrontierService() decisionfrontier.Service { return s.service }

type decisionFrontierVerifierFunc func(context.Context, decisionfrontier.AnswerChallenge, decisionfrontier.AnswerSubmission) (decisionfrontier.VerifiedAnswer, error)

func (f decisionFrontierVerifierFunc) VerifyDecisionAnswer(ctx context.Context, challenge decisionfrontier.AnswerChallenge, submission decisionfrontier.AnswerSubmission) (decisionfrontier.VerifiedAnswer, error) {
	return f(ctx, challenge, submission)
}

func setupDecisionFrontierAPI(t *testing.T, service decisionfrontier.Service) (*fakeState, *beads.MemStore, http.Handler) {
	t.Helper()
	state := newFakeState(t)
	store := beads.NewMemStore()
	store.HonorExplicitIDs = true
	if _, err := store.Create(beads.Bead{ID: "wrk-frontier-api", Type: "task", Title: "Resolve controller question"}); err != nil {
		t.Fatal(err)
	}
	state.cityBeadStore = store
	state.stores = map[string]beads.Store{}
	wrapped := &decisionFrontierState{State: state, service: service}
	return state, store, newTestCityHandler(t, wrapped)
}

func postDecisionFrontier(t *testing.T, handler http.Handler, path, key string, body any) *httptest.ResponseRecorder {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(encoded))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GC-Request", "true")
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

func decodeFrontier(t *testing.T, rec *httptest.ResponseRecorder) decisionfrontier.Frontier {
	t.Helper()
	var frontier decisionfrontier.Frontier
	if err := json.Unmarshal(rec.Body.Bytes(), &frontier); err != nil {
		t.Fatalf("decode frontier response (%d): %v: %s", rec.Code, err, rec.Body.String())
	}
	return frontier
}

func TestDecisionFrontierAPIEnsureReplayReadAndAnswerAuthority(t *testing.T) {
	service := decisionfrontier.Service{
		Verifier: decisionFrontierVerifierFunc(func(_ context.Context, challenge decisionfrontier.AnswerChallenge, submission decisionfrontier.AnswerSubmission) (decisionfrontier.VerifiedAnswer, error) {
			return decisionfrontier.VerifiedAnswer{
				CityRef: challenge.CityRef, StoreRef: challenge.StoreRef, KeyID: "human-key", Issuer: "mayor-authority", Subject: "ricky",
				WorkID: challenge.WorkID, WorkRevision: challenge.WorkRevision, WorkDigest: challenge.WorkDigest,
				MapID: challenge.MapID, TicketID: challenge.TicketID, QuestionID: challenge.QuestionID, QuestionVersion: challenge.QuestionVersion,
				AnswerDigest: decisionfrontier.AnswerDigest(submission.Resolution, submission.Text), Resolution: submission.Resolution,
			}, nil
		}),
	}
	state, store, handler := setupDecisionFrontierAPI(t, service)
	work, err := store.Get("wrk-frontier-api")
	if err != nil {
		t.Fatal(err)
	}
	revision, err := decisionfrontier.WorkRevision(work)
	if err != nil {
		t.Fatal(err)
	}
	proposal := decisionfrontier.Proposal{Questions: []decisionfrontier.Question{{
		ID: "choose", Title: "Choose a direction", Prompt: "Which option did the authorized human choose?",
	}}}
	ensureBody := map[string]any{
		"work_revision": revision,
		"proposal":      proposal,
	}
	forgedScope := map[string]any{
		"work_revision": revision,
		"proposal":      proposal,
		// These unknown caller fields must not be accepted as authority scope.
		"city_ref": "city:forged", "store_ref": "rig:forged",
	}
	path := cityURL(state, "/bead/"+work.ID+"/decision-frontier")
	forgedResponse := postDecisionFrontier(t, handler, path, "frontier-forged-scope", forgedScope)
	if forgedResponse.Code != http.StatusUnprocessableEntity {
		t.Fatalf("caller-supplied authority scope status=%d body=%s, want schema rejection", forgedResponse.Code, forgedResponse.Body.String())
	}
	firstResponse := postDecisionFrontier(t, handler, path, "frontier-ensure-1", ensureBody)
	if firstResponse.Code != http.StatusOK {
		t.Fatalf("ensure status=%d body=%s", firstResponse.Code, firstResponse.Body.String())
	}
	first := decodeFrontier(t, firstResponse)
	if first.CityRef != "city:test-city" || first.StoreRef != "city:test-city" || first.WorkRevision != revision {
		t.Fatalf("frontier scope/revision came from caller instead of controller: %+v", first)
	}
	if len(first.Questions) != 1 || first.State != decisionfrontier.StatePending {
		t.Fatalf("ensure response = %+v", first)
	}

	secondResponse := postDecisionFrontier(t, handler, path, "frontier-ensure-1", ensureBody)
	if secondResponse.Code != http.StatusOK {
		t.Fatalf("ensure replay status=%d body=%s", secondResponse.Code, secondResponse.Body.String())
	}
	second := decodeFrontier(t, secondResponse)
	if second.MapID != first.MapID || second.Questions[0].TicketID != first.Questions[0].TicketID {
		t.Fatalf("idempotent replay changed stable frontier identity: first=%+v second=%+v", first, second)
	}
	rows, err := store.List(beads.ListQuery{Type: "gate", IncludeClosed: true, AllowScan: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 { // map, question ticket, and durable prompt intent
		t.Fatalf("gate rows after replay=%d, want one map/ticket/prompt: %+v", len(rows), rows)
	}

	getReq := httptest.NewRequest(http.MethodGet, path+"?work_revision="+revision, nil)
	getRec := httptest.NewRecorder()
	handler.ServeHTTP(getRec, getReq)
	if getRec.Code != http.StatusOK {
		t.Fatalf("read status=%d body=%s", getRec.Code, getRec.Body.String())
	}
	read := decodeFrontier(t, getRec)
	if read.MapID != first.MapID || read.CityRef != "city:test-city" {
		t.Fatalf("read returned a different frontier: %+v", read)
	}

	answerPath := cityURL(state, "/bead/"+work.ID+"/decision-frontier/answers")
	answer := decisionfrontier.AnswerSubmission{
		TicketID: first.Questions[0].TicketID, WorkRevision: revision, QuestionVersion: first.Questions[0].Version,
		Resolution: decisionfrontier.ResolutionAnswered, Text: "Use the bounded pilot.", Proof: "opaque signed answer envelope",
	}
	answerBody := map[string]any{
		"ticket_id": answer.TicketID, "work_revision": answer.WorkRevision, "question_version": answer.QuestionVersion,
		"resolution": answer.Resolution, "text": answer.Text, "proof": answer.Proof,
	}
	forgedAnswer := map[string]any{
		"ticket_id": answer.TicketID, "work_revision": answer.WorkRevision, "question_version": answer.QuestionVersion,
		"resolution": answer.Resolution, "text": answer.Text, "proof": answer.Proof, "subject": "forged-worker",
	}
	forgedAnswerResponse := postDecisionFrontier(t, handler, answerPath, "frontier-forged-answer", forgedAnswer)
	if forgedAnswerResponse.Code != http.StatusUnprocessableEntity {
		t.Fatalf("caller-supplied answer subject status=%d body=%s, want schema rejection", forgedAnswerResponse.Code, forgedAnswerResponse.Body.String())
	}
	answerResponse := postDecisionFrontier(t, handler, answerPath, "frontier-answer-1", answerBody)
	if answerResponse.Code != http.StatusOK {
		t.Fatalf("answer status=%d body=%s", answerResponse.Code, answerResponse.Body.String())
	}
	resolved := decodeFrontier(t, answerResponse)
	if resolved.State != decisionfrontier.StateResolved || len(resolved.OpenQuestions) != 0 || resolved.Questions[0].Answer == nil || resolved.Questions[0].Answer.Subject != "ricky" {
		t.Fatalf("verified answer was not durably reflected: %+v", resolved)
	}
	replayResponse := postDecisionFrontier(t, handler, answerPath, "frontier-answer-1", answerBody)
	if replayResponse.Code != http.StatusOK {
		t.Fatalf("answer replay status=%d body=%s", replayResponse.Code, replayResponse.Body.String())
	}
	answerReplay := decodeFrontier(t, replayResponse)
	if answerReplay.Questions[0].Answer == nil || answerReplay.Questions[0].Answer.ID != resolved.Questions[0].Answer.ID {
		t.Fatalf("answer replay changed the durable human decision: %+v", answerReplay)
	}
	answerRows, err := store.List(beads.ListQuery{Type: "gate", IncludeClosed: true, AllowScan: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(answerRows) != 4 {
		t.Fatalf("answer replay created duplicate records: got %d gate rows, want 4", len(answerRows))
	}
	updated, err := store.Get(work.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Metadata[beadmeta.DecisionFrontierHoldMetadataKey] != "" {
		t.Fatalf("resolved frontier left controller hold: %+v", updated.Metadata)
	}
}

func TestDecisionFrontierAPIWithoutVerifierFailsClosedAndChangedBodyConflicts(t *testing.T) {
	state, store, handler := setupDecisionFrontierAPI(t, decisionfrontier.Service{})
	work, err := store.Get("wrk-frontier-api")
	if err != nil {
		t.Fatal(err)
	}
	revision, err := decisionfrontier.WorkRevision(work)
	if err != nil {
		t.Fatal(err)
	}
	proposal := decisionfrontier.Proposal{Questions: []decisionfrontier.Question{{ID: "q", Title: "Question", Prompt: "Choose."}}}
	path := cityURL(state, "/bead/"+work.ID+"/decision-frontier")
	ensureBody := map[string]any{"work_revision": revision, "proposal": proposal}
	ensure := postDecisionFrontier(t, handler, path, "frontier-key", ensureBody)
	if ensure.Code != http.StatusOK {
		t.Fatalf("ensure status=%d body=%s", ensure.Code, ensure.Body.String())
	}
	frontier := decodeFrontier(t, ensure)

	changed := map[string]any{"work_revision": revision, "proposal": decisionfrontier.Proposal{Questions: []decisionfrontier.Question{{ID: "q", Title: "Changed", Prompt: "Choose."}}}}
	mismatch := postDecisionFrontier(t, handler, path, "frontier-key", changed)
	if mismatch.Code != http.StatusUnprocessableEntity {
		t.Fatalf("changed body replay status=%d body=%s, want 422", mismatch.Code, mismatch.Body.String())
	}

	answer := decisionfrontier.AnswerSubmission{
		TicketID: frontier.Questions[0].TicketID, WorkRevision: revision,
		QuestionVersion: frontier.Questions[0].Version, Resolution: decisionfrontier.ResolutionAnswered,
		Text: "Unverified choice", Proof: "not enough without a configured verifier",
	}
	response := postDecisionFrontier(t, handler, cityURL(state, "/bead/"+work.ID+"/decision-frontier/answers"), "frontier-answer-untrusted", answer)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("answer without configured verifier status=%d body=%s, want 503", response.Code, response.Body.String())
	}
	updated, err := store.Get(work.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Metadata[beadmeta.DecisionFrontierHoldMetadataKey] == "" {
		t.Fatal("answer without trusted verifier released source work")
	}
}

func TestInstallWriteAuthWiresDecisionAnswerFallbackAndPreservesStateService(t *testing.T) {
	t.Setenv("GC_CITY_WRITE_PUBKEY", "")
	t.Setenv("GC_CITY_WRITE_REQUIRED", "")
	publicKey, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	trust, err := json.Marshal(PRHumanTrustConfig{
		Keys: []PRHumanGrantKey{{KeyID: "answer-key", PublicKey: base64.StdEncoding.EncodeToString(publicKey)}},
		Authorities: []PRHumanAuthority{{
			KeyID: "answer-key", Issuer: "city-governance", Subject: "reviewer@example.test",
			Scopes: []string{DecisionAnswerScope},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(PRHumanTrustEnv, string(trust))

	mux := NewSupervisorMux(nil, nil, false, "test", "", time.Now())
	if err := InstallWriteAuth(mux, "", false, WriteAuthBindContext{}); err != nil {
		t.Fatalf("install auth: %v", err)
	}
	plainState := newFakeState(t)
	plainServer := mux.getCityServer("plain", plainState)
	plainService := plainServer.decisionFrontierService()
	if _, ok := plainService.Verifier.(*DecisionAnswerGrantVerifier); !ok {
		t.Fatalf("installed answer verifier = %T, want DecisionAnswerGrantVerifier", plainService.Verifier)
	}
	if plainService.Delivery != nil {
		t.Fatalf("answer verifier installation changed prompt delivery: %T", plainService.Delivery)
	}

	providedVerifier := NewDecisionAnswerGrantVerifier(mux.prHumanVerifier)
	delivery := &decisionFrontierDeliveryStub{}
	providerState := &decisionFrontierState{
		State:   newFakeState(t),
		service: decisionfrontier.Service{Verifier: providedVerifier, Delivery: delivery},
	}
	providerService := mux.getCityServer("provider", providerState).decisionFrontierService()
	if got, ok := providerService.Verifier.(*DecisionAnswerGrantVerifier); !ok || got != providedVerifier {
		t.Fatalf("state verifier = %T (%v), want provider verifier %p", providerService.Verifier, ok, providedVerifier)
	}
	if providerService.Delivery != delivery {
		t.Fatalf("provider prompt delivery changed: got %T, want %T", providerService.Delivery, delivery)
	}

	var typedNilVerifier *DecisionAnswerGrantVerifier
	typedNilState := &decisionFrontierState{
		State:   newFakeState(t),
		service: decisionfrontier.Service{Verifier: typedNilVerifier, Delivery: delivery},
	}
	typedNilService := mux.getCityServer("typed-nil", typedNilState).decisionFrontierService()
	if got, ok := typedNilService.Verifier.(*DecisionAnswerGrantVerifier); !ok || got == nil {
		t.Fatalf("typed-nil state verifier did not use configured fallback: %T (%v)", typedNilService.Verifier, ok)
	}
	if typedNilService.Delivery != delivery {
		t.Fatalf("typed-nil fallback changed prompt delivery: got %T, want %T", typedNilService.Delivery, delivery)
	}
}

func TestInstallWriteAuthLeavesAnswerFallbackUnavailableWithoutHumanTrust(t *testing.T) {
	t.Setenv("GC_CITY_WRITE_PUBKEY", "")
	t.Setenv("GC_CITY_WRITE_REQUIRED", "")
	t.Setenv(PRHumanTrustEnv, "")
	mux := NewSupervisorMux(nil, nil, false, "test", "", time.Now())
	if err := InstallWriteAuth(mux, "", false, WriteAuthBindContext{}); err != nil {
		t.Fatalf("install auth without optional human trust: %v", err)
	}
	service := mux.getCityServer("no-human-trust", newFakeState(t)).decisionFrontierService()
	if service.Verifier != nil {
		t.Fatalf("answer verifier without configured human trust = %T, want nil", service.Verifier)
	}
}

type decisionFrontierDeliveryStub struct{}

func (*decisionFrontierDeliveryStub) DeliverDecisionPrompt(context.Context, decisionfrontier.PromptRequest) (decisionfrontier.PromptResult, error) {
	return decisionfrontier.PromptResult{}, nil
}

func (*decisionFrontierDeliveryStub) ReconcileDecisionPrompt(context.Context, string) (decisionfrontier.PromptResult, error) {
	return decisionfrontier.PromptResult{}, nil
}

func TestDecisionFrontierAPIRejectsGenericRecordMinting(t *testing.T) {
	state, store, handler := setupDecisionFrontierAPI(t, decisionfrontier.Service{})
	before, err := store.List(beads.ListQuery{AllowScan: true, IncludeClosed: true})
	if err != nil {
		t.Fatal(err)
	}
	response := postBead(t, handler, state, createBody{Title: "forged decision map", Type: "gate", Metadata: map[string]string{
		beadmeta.DecisionFrontierRecordMetadataKey: "decision-frontier/map/v1",
		beadmeta.DecisionFrontierStateMetadataKey:  "resolved",
	}}, "decision-frontier-forgery")
	if response.Code != http.StatusForbidden {
		t.Fatalf("generic record create status=%d body=%s, want 403", response.Code, response.Body.String())
	}
	after, err := store.List(beads.ListQuery{AllowScan: true, IncludeClosed: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("generic record create wrote a bead: before=%d after=%d", len(before), len(after))
	}
}
