package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/api/genclient"
	"github.com/gastownhall/gascity/internal/citywriteauth"
)

type inProcessHandlerRoundTripper struct {
	handler http.Handler
}

func (rt inProcessHandlerRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	recorder := httptest.NewRecorder()
	rt.handler.ServeHTTP(recorder, request)
	return recorder.Result(), nil
}

func inProcessHandlerClient(handler http.Handler) *http.Client {
	return &http.Client{Transport: inProcessHandlerRoundTripper{handler: handler}}
}

func newInProcessCityClient(t *testing.T, cityName string, handler http.Handler) *Client {
	t.Helper()
	baseURL := "http://127.0.0.1"
	cw, err := genclient.NewClientWithResponses(
		baseURL,
		genclient.WithHTTPClient(inProcessHandlerClient(handler)),
		genclient.WithRequestEditorFn(func(_ context.Context, request *http.Request) error {
			request.Header.Set("X-GC-Request", "true")
			return nil
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	return &Client{cw: cw, baseURL: baseURL, cityName: cityName}
}

func TestClientPRActionBindsGrantToExactWireRequest(t *testing.T) {
	var binding GrantBinding
	var wire []byte
	var authorization, headerKey, grant, path string
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wire, _ = io.ReadAll(r.Body)
		authorization = r.Header.Get("Authorization")
		headerKey, grant, path = r.Header.Get("Idempotency-Key"), r.Header.Get("X-GC-City-Write"), r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(PRActionResult{ID: "receipt-1", Status: PRActionStatusVerified})
	})
	client := &Client{
		baseURL: "https://city.test", cityName: "test-city", isRemote: true,
		tokenSource: func() (string, error) { return "private-test-bearer", nil },
		grantSource: func(b GrantBinding) (string, error) {
			binding = b
			return "private-test-grant", nil
		},
	}
	cw, err := genclient.NewClientWithResponses(
		client.baseURL,
		genclient.WithHTTPClient(inProcessHandlerClient(handler)),
		genclient.WithRequestEditorFn(func(_ context.Context, request *http.Request) error {
			request.Header.Set("X-GC-Request", "true")
			return nil
		}),
		genclient.WithRequestEditorFn(remoteAuthEditor(client)),
		genclient.WithRequestEditorFn(remoteGrantEditor(client)),
	)
	if err != nil {
		t.Fatal(err)
	}
	client.cw = cw
	request := PRActionRequest{Monitor: "main", Owner: "example", Repo: "project", PullRequest: 7, Action: PRActionQueueReview, WorkID: "work-7", AttemptID: "attempt-2", HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), PolicyVersion: "policy-1", IdempotencyKey: "action-key-7"}
	if _, err := client.ExecutePRAction(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if path != "/v0/city/test-city/pr-actions" || headerKey != request.IdempotencyKey || grant != "private-test-grant" || authorization != "Bearer private-test-bearer" {
		t.Fatal("action transport lost its city, idempotency key or authority grant")
	}
	if binding.ReqDigest != citywriteauth.ReqDigest(http.MethodPost, path, "", wire) {
		t.Fatal("authority grant is not bound to the actual action request")
	}
	if strings.Contains(string(wire), "idempotency_key") || strings.Contains(string(wire), "private-test-grant") {
		t.Fatal("request headers leaked into action JSON")
	}
}

func TestClientPRActionQueuePreservesServerVerdict(t *testing.T) {
	want := PRActionQueue{
		Availability:  PRActionAvailabilityReady,
		PolicyState:   PRActionSourceReady,
		PolicyVersion: "signed-policy",
		Sources:       []PRActionSource{{Monitor: "central", Owner: "example", Repo: "project", State: PRActionSourceReady}},
		Items: []PRActionQueueItem{{
			Monitor: "central", Owner: "example", Repo: "project", PullRequest: 7,
			PolicyVersion: "signed-policy",
			Actions:       []PRActionOption{{Action: PRActionPrepare, Available: true, Reason: "server verdict"}},
		}},
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodGet || request.URL.Path != "/v0/city/test-city/pr-actions/queue" {
			t.Errorf("request = %s %s", request.Method, request.URL.Path)
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(want); err != nil {
			t.Errorf("encode queue: %v", err)
		}
	})
	client := newInProcessCityClient(t, "test-city", handler)
	got, err := client.GetPRActionQueue(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.PolicyVersion != want.PolicyVersion || got.Items[0].Actions[0].Reason != "server verdict" {
		t.Fatalf("queue = %+v; want unchanged server verdict", got)
	}
}
