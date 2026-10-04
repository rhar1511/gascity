package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

type failNextReceiptResolutionStore struct {
	*beads.MemStore
	injectAfterReceiptWrite atomic.Bool
	failNextGet             atomic.Bool
}

func (s *failNextReceiptResolutionStore) Get(id string) (beads.Bead, error) {
	if s.failNextGet.CompareAndSwap(true, false) {
		return beads.Bead{}, errors.New("injected post-acceptance resolution failure")
	}
	return s.MemStore.Get(id)
}

func (s *failNextReceiptResolutionStore) UpdateIfMatch(id string, revision int64, opts beads.UpdateOpts) error {
	err := s.MemStore.UpdateIfMatch(id, revision, opts)
	if err == nil && s.injectAfterReceiptWrite.CompareAndSwap(true, false) {
		for key := range opts.Metadata {
			if strings.HasPrefix(key, beadmeta.SessionRequestReceiptPrefix) {
				s.failNextGet.Store(true)
				break
			}
		}
	}
	return err
}

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
	h := newTestCityHandler(t, fs)
	server := httptest.NewServer(h)
	defer server.Close()
	client := NewCityScopedClient(server.URL, fs.CityName())
	if _, err := client.SubmitSessionRequest(info.ID, "request-http-1", generation, "report progress"); err != nil {
		t.Fatal(err)
	}
	if success, failure := waitForSessionSubmitResult(t, fs.eventProv, "request-http-1"); failure != nil || success == nil {
		t.Fatalf("request delivery result: success=%+v failure=%+v", success, failure)
	}
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
		{token: "", want: http.StatusUnprocessableEntity},
		{token: "another-execution", want: http.StatusForbidden},
		{token: persisted.InstanceToken, want: http.StatusOK},
	} {
		req := newPostRequest(url+"/ack", strings.NewReader(fmt.Sprintf(`{"generation":%d}`, generation)))
		if tc.token != "" {
			req.Header.Set("X-GC-Session-Token", tc.token)
		}
		response := httptest.NewRecorder()
		h.ServeHTTP(response, req)
		if response.Code != tc.want {
			t.Fatalf("acknowledgement = %d, want %d: %s", response.Code, tc.want, response.Body.String())
		}
		if tc.token != "" && strings.Contains(response.Body.String(), tc.token) {
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

func TestGenericIdentityChangesCannotHideHistoricalRequestReceipts(t *testing.T) {
	state := newSessionFakeState(t)
	info := createTestSession(t, state.cityBeadStore, state.sp, "Historical receipt")
	front := session.NewStore(state.SessionsBeadStore())
	persisted, err := front.Get(info.ID)
	if err != nil {
		t.Fatal(err)
	}
	generation, err := strconv.Atoi(persisted.Generation)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := front.AcceptRequest(info.ID, "historical-request", generation, "remember this", time.Now()); err != nil {
		t.Fatal(err)
	}

	h := newTestCityHandler(t, state)
	for name, body := range map[string]string{
		"type":          `{"type":"task"}`,
		"session label": `{"remove_labels":["gc:session"]}`,
	} {
		recorder := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPatch, cityURL(state, "/bead/"+info.ID), strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-GC-Request", "true")
		h.ServeHTTP(recorder, req)
		if recorder.Code != http.StatusConflict {
			t.Fatalf("generic %s change = %d, want 409: %s", name, recorder.Code, recorder.Body.String())
		}
	}
	read := httptest.NewRecorder()
	h.ServeHTTP(read, httptest.NewRequest(http.MethodGet, cityURL(state, "/session/"+info.ID+"/requests/historical-request"), nil))
	var httpReceipt session.RequestReceipt
	if err := json.Unmarshal(read.Body.Bytes(), &httpReceipt); read.Code != http.StatusOK || err != nil || httpReceipt.RequestID != "historical-request" {
		t.Fatalf("historical HTTP GET = %d %+v, %v", read.Code, httpReceipt, err)
	}
	if receipt, err := front.GetRequest(info.ID, "historical-request"); err != nil || receipt.RequestID != "historical-request" {
		t.Fatalf("historical GetRequest = %+v, %v", receipt, err)
	}
	receipts, err := front.ListRequests(info.ID, generation)
	if err != nil || len(receipts) != 1 || receipts[0].RequestID != "historical-request" {
		t.Fatalf("historical ListRequests = %+v, %v", receipts, err)
	}
	bead, err := state.cityBeadStore.Get(info.ID)
	if err != nil || !slices.Contains(bead.Labels, session.LabelSession) {
		t.Fatalf("historical session identity was removed: %+v, %v", bead, err)
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
	body := fmt.Sprintf(`{"request_id":"submit-http-1","generation":%d,"message":"report progress"}`, generation)
	var firstResponse string
	for i := range 2 {
		response := httptest.NewRecorder()
		req := newPostRequest(url, strings.NewReader(body))
		req.Header.Set("Idempotency-Key", "submit-http-retry-1")
		h.ServeHTTP(response, req)
		if response.Code != http.StatusAccepted {
			t.Fatalf("submit = %d: %s", response.Code, response.Body.String())
		}
		if i == 0 {
			firstResponse = response.Body.String()
		} else if response.Body.String() != firstResponse {
			t.Fatalf("idempotent replay = %s, want cached response %s", response.Body.String(), firstResponse)
		}
	}
	success, failure := waitForSessionSubmitResult(t, state.eventProv, "submit-http-1")
	if failure != nil {
		t.Fatalf("request delivery failed: %+v", failure)
	}
	if success == nil {
		t.Fatal("request delivery did not complete")
	}
	receipt, err := front.GetRequest(info.ID, "submit-http-1")
	if err != nil || receipt.Delivery != session.RequestDeliveryAccepted {
		t.Fatalf("delivery receipt = %+v, %v", receipt, err)
	}
	if receipt.AcknowledgedAt != nil || receipt.Effect != "unverified" {
		t.Fatal("provider send invented acknowledgement/effect")
	}
	if got := state.sp.CountCalls("Nudge", info.SessionName); got != 1 {
		t.Fatalf("provider sends = %d, want 1", got)
	}
}

func TestSessionRequestSubmitHTTPResumesAcceptedPendingDelivery(t *testing.T) {
	state := newSessionFakeState(t)
	info := createTestSession(t, state.cityBeadStore, state.sp, "Pending target")
	front := session.NewStore(state.SessionsBeadStore())
	persisted, err := front.Get(info.ID)
	if err != nil {
		t.Fatal(err)
	}
	generation, _ := strconv.Atoi(persisted.Generation)
	if _, err := front.AcceptRequest(info.ID, "pending-http-1", generation, "report progress", time.Now()); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{"request_id":"pending-http-1","generation":%d,"message":"report progress"}`, generation)
	response := httptest.NewRecorder()
	newTestCityHandler(t, state).ServeHTTP(response, newPostRequest(cityURL(state, "/session/"+info.ID+"/requests"), strings.NewReader(body)))
	if response.Code != http.StatusAccepted {
		t.Fatalf("submit = %d: %s", response.Code, response.Body.String())
	}
	success, failure := waitForSessionSubmitResult(t, state.eventProv, "pending-http-1")
	if failure != nil || success == nil {
		t.Fatalf("pending delivery was not resumed: success=%+v failure=%+v", success, failure)
	}
	receipt, err := front.GetRequest(info.ID, "pending-http-1")
	if err != nil || receipt.Delivery != session.RequestDeliveryAccepted || state.sp.CountCalls("Nudge", info.SessionName) != 1 {
		t.Fatalf("resumed receipt=%+v error=%v", receipt, err)
	}
}

func TestSessionRequestSubmitHTTPReturnsAcceptanceBeforeResolutionAndReplayRecovers(t *testing.T) {
	state := newSessionFakeState(t)
	store := &failNextReceiptResolutionStore{MemStore: beads.NewMemStore()}
	state.cityBeadStore = store
	info := createTestSession(t, store, state.sp, "Resolution retry target")
	persisted, err := session.NewStore(state.SessionsBeadStore()).Get(info.ID)
	if err != nil {
		t.Fatal(err)
	}
	generation, _ := strconv.Atoi(persisted.Generation)
	store.injectAfterReceiptWrite.Store(true)
	h := newTestCityHandler(t, state)
	url := cityURL(state, "/session/"+info.ID+"/requests")
	body := fmt.Sprintf(`{"request_id":"resolution-http-1","generation":%d,"message":"report progress"}`, generation)
	post := func() *httptest.ResponseRecorder {
		response := httptest.NewRecorder()
		req := newPostRequest(url, strings.NewReader(body))
		req.Header.Set("Idempotency-Key", "resolution-key-1")
		h.ServeHTTP(response, req)
		return response
	}

	first := post()
	if first.Code != http.StatusAccepted {
		t.Fatalf("post-acceptance resolution failure replaced 202: %d %s", first.Code, first.Body.String())
	}
	if success, failure := waitForSessionSubmitResult(t, state.eventProv, "resolution-http-1"); success != nil || failure == nil || failure.ErrorCode != "tracked_submit_resolution_failed" {
		t.Fatalf("resolution result: success=%+v failure=%+v", success, failure)
	}
	receipt, err := session.NewStore(state.SessionsBeadStore()).GetRequest(info.ID, "resolution-http-1")
	if err != nil || receipt.Delivery != session.RequestDeliveryPending || receipt.DeliveryAttemptedAt != nil {
		t.Fatalf("resolution failure receipt=%+v error=%v", receipt, err)
	}
	if got := state.sp.CountCalls("Nudge", info.SessionName); got != 0 {
		t.Fatalf("provider calls after resolution failure = %d", got)
	}

	second := post()
	if second.Code != http.StatusAccepted || second.Body.String() != first.Body.String() {
		t.Fatalf("same-key replay = %d %s, want exact %s", second.Code, second.Body.String(), first.Body.String())
	}
	waitForSessionRequestEventCount(t, state.eventProv, events.RequestResultSessionSubmit, "resolution-http-1", 1)
	receipt, err = session.NewStore(state.SessionsBeadStore()).GetRequest(info.ID, "resolution-http-1")
	if err != nil || receipt.Delivery != session.RequestDeliveryAccepted || state.sp.CountCalls("Nudge", info.SessionName) != 1 {
		t.Fatalf("recovered receipt=%+v error=%v", receipt, err)
	}
}

func TestSessionRequestSameKeyReplayResumesAfterPreReservationFailure(t *testing.T) {
	state := newSessionFakeState(t)
	info := createTestSession(t, state.cityBeadStore, state.sp, "Retry target")
	persisted, err := session.NewStore(state.SessionsBeadStore()).Get(info.ID)
	if err != nil {
		t.Fatal(err)
	}
	generation, _ := strconv.Atoi(persisted.Generation)
	state.sp.SetPendingInteraction(info.SessionName, &runtime.PendingInteraction{})
	h := newTestCityHandler(t, state)
	url := cityURL(state, "/session/"+info.ID+"/requests")
	body := fmt.Sprintf(`{"request_id":"retry-http-1","generation":%d,"message":"report progress"}`, generation)
	post := func() *httptest.ResponseRecorder {
		response := httptest.NewRecorder()
		req := newPostRequest(url, strings.NewReader(body))
		req.Header.Set("Idempotency-Key", "retry-key-1")
		h.ServeHTTP(response, req)
		return response
	}
	first := post()
	if first.Code != http.StatusAccepted {
		t.Fatalf("first submit = %d: %s", first.Code, first.Body.String())
	}
	if success, failure := waitForSessionSubmitResult(t, state.eventProv, "retry-http-1"); success != nil || failure == nil {
		t.Fatalf("first delivery should fail before reservation: success=%+v failure=%+v", success, failure)
	}
	receipt, err := session.NewStore(state.SessionsBeadStore()).GetRequest(info.ID, "retry-http-1")
	if err != nil || receipt.Delivery != session.RequestDeliveryPending || state.sp.CountCalls("Nudge", info.SessionName) != 0 {
		t.Fatalf("pre-reservation failure receipt=%+v error=%v", receipt, err)
	}
	state.sp.SetPendingInteraction(info.SessionName, nil)
	second := post()
	if second.Code != http.StatusAccepted || second.Body.String() != first.Body.String() {
		t.Fatalf("same-key replay = %d %s, want exact %s", second.Code, second.Body.String(), first.Body.String())
	}
	waitForSessionRequestEventCount(t, state.eventProv, events.RequestResultSessionSubmit, "retry-http-1", 1)
	receipt, err = session.NewStore(state.SessionsBeadStore()).GetRequest(info.ID, "retry-http-1")
	if err != nil || receipt.Delivery != session.RequestDeliveryAccepted || state.sp.CountCalls("Nudge", info.SessionName) != 1 {
		t.Fatalf("resumed receipt=%+v error=%v", receipt, err)
	}
}

func TestSessionRequestSameKeyReplayDoesNotResendUnknownDelivery(t *testing.T) {
	state := newSessionFakeState(t)
	info := createTestSession(t, state.cityBeadStore, state.sp, "Uncertain target")
	persisted, err := session.NewStore(state.SessionsBeadStore()).Get(info.ID)
	if err != nil {
		t.Fatal(err)
	}
	generation, _ := strconv.Atoi(persisted.Generation)
	state.sp.NudgeErrors = map[string]error{info.SessionName: errors.New("connection lost after send")}
	h := newTestCityHandler(t, state)
	url := cityURL(state, "/session/"+info.ID+"/requests")
	body := fmt.Sprintf(`{"request_id":"unknown-http-1","generation":%d,"message":"report progress"}`, generation)
	post := func() *httptest.ResponseRecorder {
		response := httptest.NewRecorder()
		req := newPostRequest(url, strings.NewReader(body))
		req.Header.Set("Idempotency-Key", "unknown-key-1")
		h.ServeHTTP(response, req)
		return response
	}
	first := post()
	if first.Code != http.StatusAccepted {
		t.Fatalf("first submit = %d: %s", first.Code, first.Body.String())
	}
	if success, failure := waitForSessionSubmitResult(t, state.eventProv, "unknown-http-1"); success != nil || failure == nil {
		t.Fatalf("uncertain send result: success=%+v failure=%+v", success, failure)
	}
	delete(state.sp.NudgeErrors, info.SessionName)
	second := post()
	if second.Code != http.StatusAccepted || second.Body.String() != first.Body.String() {
		t.Fatalf("same-key replay = %d %s, want exact %s", second.Code, second.Body.String(), first.Body.String())
	}
	waitForSessionRequestFailures(t, state, "unknown-http-1", 2)
	receipt, err := session.NewStore(state.SessionsBeadStore()).GetRequest(info.ID, "unknown-http-1")
	if err != nil || receipt.Delivery != session.RequestDeliveryUnknown || state.sp.CountCalls("Nudge", info.SessionName) != 1 {
		t.Fatalf("uncertain replay receipt=%+v error=%v", receipt, err)
	}
}

func waitForSessionRequestFailures(t *testing.T, state *fakeState, requestID string, want int) {
	t.Helper()
	waitForSessionRequestEventCount(t, state.eventProv, events.RequestFailed, requestID, want)
}

// waitForSessionRequestEventCount observes durable test events instead of
// sampling elapsed time. Watch(0) includes results emitted before subscription.
func waitForSessionRequestEventCount(t *testing.T, prov events.Provider, eventType, requestID string, want int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), testEventTimeout)
	defer cancel()
	watcher, err := prov.Watch(ctx, 0)
	if err != nil {
		t.Fatalf("watch session request events: %v", err)
	}
	defer func() {
		if err := watcher.Close(); err != nil {
			t.Errorf("close session request watcher: %v", err)
		}
	}()
	for count := 0; count < want; {
		row, err := watcher.Next()
		if err != nil {
			t.Fatalf("waiting for %d %s events for %s: got %d: %v", want, eventType, requestID, count, err)
		}
		if row.Type != eventType {
			continue
		}
		switch eventType {
		case events.RequestResultSessionSubmit:
			var payload SessionSubmitSucceededPayload
			if json.Unmarshal(row.Payload, &payload) == nil && requestIDMatches(payload.RequestID, requestID) {
				count++
			}
		case events.RequestFailed:
			var payload RequestFailedPayload
			if json.Unmarshal(row.Payload, &payload) == nil && payload.Operation == RequestOperationSessionSubmit && requestIDMatches(payload.RequestID, requestID) {
				count++
			}
		default:
			t.Fatalf("unsupported session request event type %q", eventType)
		}
	}
}

func TestSessionRequestReceiptMetadataRejectsGenericMutation(t *testing.T) {
	state := newSessionFakeState(t)
	info := createTestSession(t, state.cityBeadStore, state.sp, "Protected target")
	front := session.NewStore(state.SessionsBeadStore())
	persisted, err := front.Get(info.ID)
	if err != nil {
		t.Fatal(err)
	}
	generation, _ := strconv.Atoi(persisted.Generation)
	if _, err := front.AcceptRequest(info.ID, "protected-request", generation, "report progress", time.Now()); err != nil {
		t.Fatal(err)
	}
	row, err := state.cityBeadStore.Get(info.ID)
	if err != nil {
		t.Fatal(err)
	}
	var receiptKey, raw string
	for key, value := range row.Metadata {
		if strings.HasPrefix(key, beadmeta.SessionRequestReceiptPrefix) {
			receiptKey, raw = key, value
		}
	}
	if receiptKey == "" {
		t.Fatal("receipt metadata key not persisted")
	}
	var forged map[string]any
	if err := json.Unmarshal([]byte(raw), &forged); err != nil {
		t.Fatal(err)
	}
	forged["acknowledged_at"] = time.Now().UTC().Format(time.RFC3339Nano)
	forgedRaw, err := json.Marshal(forged)
	if err != nil {
		t.Fatal(err)
	}
	metadata := map[string]string{receiptKey: string(forgedRaw)}
	updateBody, _ := json.Marshal(map[string]any{"metadata": metadata})
	h := newTestCityHandler(t, state)
	for _, method := range []string{http.MethodPost, http.MethodPatch} {
		response := httptest.NewRecorder()
		req := newPostRequest(cityURL(state, "/bead/"+info.ID+map[bool]string{true: "/update"}[method == http.MethodPost]), strings.NewReader(string(updateBody)))
		req.Method = method
		h.ServeHTTP(response, req)
		if response.Code != http.StatusForbidden {
			t.Fatalf("generic %s overwrite = %d: %s", method, response.Code, response.Body.String())
		}
	}
	createBody, _ := json.Marshal(map[string]any{"title": "forged receipt", "type": "task", "metadata": metadata})
	response := httptest.NewRecorder()
	h.ServeHTTP(response, newPostRequest(cityURL(state, "/beads"), strings.NewReader(string(createBody))))
	if response.Code != http.StatusForbidden {
		t.Fatalf("generic create = %d: %s", response.Code, response.Body.String())
	}
	for _, fenceValue := range []string{"forged-owner", ""} {
		fenceMetadata := map[string]string{beadmeta.SessionRequestPurgeFenceMetadataKey: fenceValue}
		fenceUpdateBody, _ := json.Marshal(map[string]any{"metadata": fenceMetadata})
		for _, method := range []string{http.MethodPost, http.MethodPatch} {
			response := httptest.NewRecorder()
			req := newPostRequest(cityURL(state, "/bead/"+info.ID+map[bool]string{true: "/update"}[method == http.MethodPost]), strings.NewReader(string(fenceUpdateBody)))
			req.Method = method
			h.ServeHTTP(response, req)
			if response.Code != http.StatusForbidden {
				t.Fatalf("generic %s purge-fence mutation %q = %d: %s", method, fenceValue, response.Code, response.Body.String())
			}
		}
	}
	fenceMetadata := map[string]string{beadmeta.SessionRequestPurgeFenceMetadataKey: "forged-owner"}
	fenceCreateBody, _ := json.Marshal(map[string]any{"title": "forged fence", "type": "task", "metadata": fenceMetadata})
	response = httptest.NewRecorder()
	h.ServeHTTP(response, newPostRequest(cityURL(state, "/beads"), strings.NewReader(string(fenceCreateBody))))
	if response.Code != http.StatusForbidden {
		t.Fatalf("generic purge-fence create = %d: %s", response.Code, response.Body.String())
	}
	receipt, err := front.GetRequest(info.ID, "protected-request")
	if err != nil || receipt.AcknowledgedAt != nil || receipt.Effect != "unverified" {
		t.Fatalf("generic mutation changed receipt: %+v, %v", receipt, err)
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
	server := httptest.NewServer(newTestCityHandler(t, state))
	defer server.Close()
	client := NewCityScopedClient(server.URL, state.CityName())
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
