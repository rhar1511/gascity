package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/session"
)

func TestSessionRequestReadAndAcknowledgementHTTP(t *testing.T) {
	fs := newSessionFakeState(t)
	info := createTestSession(t, fs.cityBeadStore, fs.sp, "Receipt target")
	store := session.NewStore(beads.SessionStore{Store: fs.cityBeadStore})
	persisted, err := store.Get(info.ID)
	if err != nil {
		t.Fatal(err)
	}
	generation, err := strconv.Atoi(persisted.Generation)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.AcceptRequest(info.ID, "request-http-1", generation, "report progress", time.Now()); err != nil {
		t.Fatal(err)
	}
	h := newTestCityHandler(t, fs)
	url := cityURL(fs, "/session/"+info.ID+"/requests/request-http-1")
	read := httptest.NewRecorder()
	h.ServeHTTP(read, httptest.NewRequest(http.MethodGet, url, nil))
	if read.Code != http.StatusOK {
		t.Fatalf("request read = %d: %s", read.Code, read.Body.String())
	}
	var receipt session.RequestReceipt
	if err := json.Unmarshal(read.Body.Bytes(), &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.RequestID != "request-http-1" || receipt.AcknowledgedAt != nil || receipt.Effect != "unverified" {
		t.Fatalf("incorrect initial receipt: %+v", receipt)
	}
	for _, tc := range []struct {
		token string
		want  int
	}{
		{token: "another-execution", want: http.StatusForbidden},
		{token: persisted.InstanceToken, want: http.StatusOK},
	} {
		req := newPostRequest(url+"/ack", strings.NewReader(fmt.Sprintf(`{"generation":%d}`, generation)))
		req.Header.Set("X-GC-Session-Token", tc.token)
		response := httptest.NewRecorder()
		h.ServeHTTP(response, req)
		if response.Code != tc.want {
			t.Fatalf("acknowledgement = %d, want %d: %s", response.Code, tc.want, response.Body.String())
		}
		if strings.Contains(response.Body.String(), tc.token) {
			t.Fatal("response exposed execution credential")
		}
	}
	read = httptest.NewRecorder()
	h.ServeHTTP(read, httptest.NewRequest(http.MethodGet, url, nil))
	if err := json.Unmarshal(read.Body.Bytes(), &receipt); err != nil {
		t.Fatal(err)
	}
	if read.Code != http.StatusOK || receipt.AcknowledgedAt == nil || receipt.Effect != "unverified" {
		t.Fatalf("acknowledgement read = %d: %+v", read.Code, receipt)
	}
}

func TestSessionRequestSubmitHTTPPreservesAcceptanceAndSendsOnce(t *testing.T) {
	state := newSessionFakeState(t)
	info := createTestSession(t, state.cityBeadStore, state.sp, "Tracked target")
	front := session.NewStore(state.SessionsBeadStore())
	persisted, err := front.Get(info.ID)
	if err != nil {
		t.Fatal(err)
	}
	generation, _ := strconv.Atoi(persisted.Generation)
	h := newTestCityHandler(t, state)
	url := cityURL(state, "/session/"+info.ID+"/requests")
	cursor, err := state.eventProv.LatestSeq()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), testEventTimeout)
	defer cancel()
	watcher, err := state.eventProv.Watch(ctx, cursor)
	if err != nil {
		t.Fatal(err)
	}
	defer watcher.Close() //nolint:errcheck // best-effort test cleanup
	body := fmt.Sprintf(`{"request_id":"submit-http-1","generation":%d,"message":"report progress"}`, generation)
	for range 2 {
		response := httptest.NewRecorder()
		h.ServeHTTP(response, newPostRequest(url, strings.NewReader(body)))
		if response.Code != http.StatusAccepted {
			t.Fatalf("submit = %d: %s", response.Code, response.Body.String())
		}
	}
	for {
		event, err := watcher.Next()
		if err != nil {
			t.Fatalf("waiting for session submit completion: %v", err)
		}
		if event.Type == events.RequestResultSessionSubmit {
			var result SessionSubmitSucceededPayload
			if json.Unmarshal(event.Payload, &result) == nil && result.RequestID == "submit-http-1" {
				break
			}
		}
		if event.Type == events.RequestFailed {
			var failure RequestFailedPayload
			if json.Unmarshal(event.Payload, &failure) == nil && failure.Operation == RequestOperationSessionSubmit && failure.RequestID == "submit-http-1" {
				t.Fatalf("session submit failed: %s: %s", failure.ErrorCode, failure.ErrorMessage)
			}
		}
	}
	receipt, err := front.GetRequest(info.ID, "submit-http-1")
	if err != nil || receipt.Delivery != session.RequestDeliveryAccepted {
		t.Fatalf("delivery after completion event = %+v, %v", receipt, err)
	}
	if receipt.AcknowledgedAt != nil || receipt.Effect != "unverified" {
		t.Fatal("provider send invented acknowledgement/effect")
	}
}

func TestSessionRequestClientRoundTrip(t *testing.T) {
	state := newSessionFakeState(t)
	info := createTestSession(t, state.cityBeadStore, state.sp, "Client target")
	persisted, err := session.NewStore(state.SessionsBeadStore()).Get(info.ID)
	if err != nil {
		t.Fatal(err)
	}
	generation, _ := strconv.Atoi(persisted.Generation)
	client := newInProcessCityClient(t, state.CityName(), newTestCityHandler(t, state))
	receipt, err := client.SubmitSessionRequest(info.ID, "client-request", generation, "report progress")
	if err != nil || receipt.RequestId != "client-request" || receipt.AcknowledgedAt != nil {
		t.Fatalf("submit %+v,%v", receipt, err)
	}
	receipt, err = client.AcknowledgeSessionRequest(info.ID, "client-request", generation, persisted.InstanceToken)
	if err != nil || receipt.AcknowledgedAt == nil {
		t.Fatalf("ack %+v,%v", receipt, err)
	}
	receipt, err = client.GetSessionRequest(info.ID, "client-request")
	if err != nil || receipt.AcknowledgedAt == nil || receipt.Effect != "unverified" {
		t.Fatalf("read %+v,%v", receipt, err)
	}
}

func TestSessionRequestHistorySurvivesCloseDeleteAPI(t *testing.T) {
	state := newSessionFakeState(t)
	info := createTestSession(t, state.cityBeadStore, state.sp, "Retained target")
	front := session.NewStore(state.SessionsBeadStore())
	persisted, err := front.Get(info.ID)
	if err != nil {
		t.Fatal(err)
	}
	generation, _ := strconv.Atoi(persisted.Generation)
	if _, err := front.AcceptRequest(info.ID, "retained-http", generation, "report progress", time.Now()); err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	newTestCityHandler(t, state).ServeHTTP(response, newPostRequest(cityURL(state, "/session/"+info.ID+"/close?delete=true"), nil))
	if response.Code != http.StatusConflict {
		t.Fatalf("close/delete = %d: %s", response.Code, response.Body.String())
	}
	if _, err := front.GetRequest(info.ID, "retained-http"); err != nil {
		t.Fatalf("receipt lost: %v", err)
	}
}
