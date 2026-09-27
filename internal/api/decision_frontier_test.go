package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

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
				MapID: challenge.MapID, TicketID: challenge.TicketID, QuestionVersion: challenge.QuestionVersion,
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
