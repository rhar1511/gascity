package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gastownhall/gascity/internal/storeref"
	"github.com/gastownhall/gascity/internal/worklifecycle"
)

type lifecycleCompletionTestState struct {
	*fakeState
	complete func(context.Context, LifecycleCompletionTransitionRequest) (LifecycleCompletionResult, error)
}

func (s *lifecycleCompletionTestState) CompleteLifecycleWork(ctx context.Context, request LifecycleCompletionTransitionRequest) (LifecycleCompletionResult, error) {
	return s.complete(ctx, request)
}

func TestLifecycleCompletionSubmitUsesAuthoritativeControllerBinding(t *testing.T) {
	claimState, store, _, work := newLifecycleClaimTestStateWithRevision(t, -7)
	state := &lifecycleCompletionTestState{fakeState: claimState.fakeState}
	input := LifecycleCompletionSubmitRequest{WorkID: work.ID, SourceStoreRef: "city:test-city", ExpectedRevision: -7, ExpectedTransitionHead: "head-1", CompletionReceipt: `{"signed":"acceptance"}`}
	called := false
	state.complete = func(_ context.Context, request LifecycleCompletionTransitionRequest) (LifecycleCompletionResult, error) {
		called = true
		if request.Work.ID != work.ID || request.WorkStore != store || request.WorkStoreRef != storeref.WorkRef || request.Scope != "city:test-city/city:test-city" || request.Submission != input {
			t.Fatalf("completion did not retain exact controller binding: %+v", request)
		}
		return LifecycleCompletionResult{WorkID: work.ID, ReceiptID: "close-1", Revision: -9, Replayed: true}, nil
	}
	body, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	newTestCityHandler(t, state).ServeHTTP(recorder, newPostRequest(cityURL(state, "/lifecycle/completions"), bytes.NewReader(body)))
	if recorder.Code != http.StatusOK || !called {
		t.Fatalf("completion HTTP %d: %s", recorder.Code, recorder.Body.String())
	}
	var result LifecycleCompletionResult
	if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.WorkID != work.ID || result.ReceiptID != "close-1" || result.Revision != -9 || !result.Replayed {
		t.Fatalf("completion result: %+v", result)
	}
}

func TestLifecycleCompletionSubmitRefusesWrongBindingAndProviderFailure(t *testing.T) {
	for _, tc := range []struct {
		name          string
		change        func(*LifecycleCompletionSubmitRequest)
		providerError error
		status        int
		called        bool
	}{
		{name: "wrong store", change: func(r *LifecycleCompletionSubmitRequest) { r.SourceStoreRef = "rig:other" }, status: http.StatusConflict},
		{name: "zero revision", change: func(r *LifecycleCompletionSubmitRequest) { r.ExpectedRevision = 0 }, status: http.StatusBadRequest},
		{name: "stale proof", providerError: worklifecycle.ErrTransitionChainEvidence, status: http.StatusConflict, called: true},
		{name: "unavailable permit", providerError: worklifecycle.ErrTransitionChainPermit, status: http.StatusServiceUnavailable, called: true},
		{name: "unknown backend failure", providerError: errors.New("private backend detail"), status: http.StatusServiceUnavailable, called: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			original, _, _, work := newLifecycleClaimTestState(t)
			state := &lifecycleCompletionTestState{fakeState: original.fakeState}
			called := false
			state.complete = func(context.Context, LifecycleCompletionTransitionRequest) (LifecycleCompletionResult, error) {
				called = true
				return LifecycleCompletionResult{}, tc.providerError
			}
			input := LifecycleCompletionSubmitRequest{WorkID: work.ID, SourceStoreRef: "city:test-city", ExpectedRevision: work.Revision, ExpectedTransitionHead: "head-1", CompletionReceipt: "signed acceptance"}
			if tc.change != nil {
				tc.change(&input)
			}
			raw, err := json.Marshal(input)
			if err != nil {
				t.Fatal(err)
			}
			response := httptest.NewRecorder()
			newTestCityHandler(t, state).ServeHTTP(response, newPostRequest(cityURL(state, "/lifecycle/completions"), bytes.NewReader(raw)))
			if response.Code != tc.status || called != tc.called || bytes.Contains(response.Body.Bytes(), []byte("private backend detail")) {
				t.Fatalf("HTTP %d called=%t: %s", response.Code, called, response.Body.String())
			}
		})
	}
}

func TestLifecycleCompletionClientRoundTrip(t *testing.T) {
	original, _, _, work := newLifecycleClaimTestStateWithRevision(t, -7)
	state := &lifecycleCompletionTestState{fakeState: original.fakeState}
	input := LifecycleCompletionSubmitRequest{WorkID: work.ID, SourceStoreRef: "city:test-city", ExpectedRevision: -7, ExpectedTransitionHead: "head-1", CompletionReceipt: `{"signed":"acceptance"}`}
	state.complete = func(_ context.Context, request LifecycleCompletionTransitionRequest) (LifecycleCompletionResult, error) {
		if request.Submission != input {
			t.Fatalf("client changed completion request: %+v", request.Submission)
		}
		return LifecycleCompletionResult{WorkID: work.ID, ReceiptID: "close-receipt", Revision: -9223372036854775000, Replayed: true}, nil
	}
	client := NewCityScopedClient("http://127.0.0.1", state.CityName(), WithHTTPTransport(loopbackTransport{h: newTestCityHandler(t, state)}))
	submitter, ok := any(client).(interface {
		CompleteLifecycleWork(context.Context, LifecycleCompletionSubmitRequest) (LifecycleCompletionResult, error)
	})
	if !ok {
		t.Fatal("typed control-plane client has no protected completion operation")
	}
	result, err := submitter.CompleteLifecycleWork(context.Background(), input)
	if err != nil || result.WorkID != work.ID || result.ReceiptID != "close-receipt" || result.Revision != -9223372036854775000 || !result.Replayed {
		t.Fatalf("client completion result=%+v err=%v", result, err)
	}
}
