package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/citywriteauth"
)

func TestClientPRActionBindsGrantToExactWireRequest(t *testing.T) {
	var binding GrantBinding
	var wire []byte
	var headerKey, grant, path string
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wire, _ = io.ReadAll(r.Body)
		headerKey, grant, path = r.Header.Get("Idempotency-Key"), r.Header.Get("X-GC-City-Write"), r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(PRActionResult{ID: "receipt-1", Status: PRActionStatusVerified})
	})
	client, err := NewRemoteCityScopedClient("http://pr-actions.test", "test-city", RemoteOptions{Grant: func(b GrantBinding) (string, error) {
		binding = b
		return "private-test-grant", nil
	}}, WithHTTPTransport(loopbackTransport{h: handler}))
	if err != nil {
		t.Fatal(err)
	}
	request := PRActionRequest{Monitor: "main", Owner: "example", Repo: "project", PullRequest: 7, Action: PRActionQueueReview, WorkID: "work-7", AttemptID: "attempt-2", HeadSHA: strings.Repeat("a", 40), BaseSHA: strings.Repeat("b", 40), PolicyVersion: "policy-1", IdempotencyKey: "action-key-7"}
	if _, err := client.ExecutePRAction(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if path != "/v0/city/test-city/pr-actions" || headerKey != request.IdempotencyKey || grant != "private-test-grant" {
		t.Fatal("action transport lost its city, idempotency key or authority grant")
	}
	if binding.ReqDigest != citywriteauth.ReqDigest(http.MethodPost, path, "", wire) {
		t.Fatal("authority grant is not bound to the actual action request")
	}
	if strings.Contains(string(wire), "idempotency_key") || strings.Contains(string(wire), "private-test-grant") {
		t.Fatal("request headers leaked into action JSON")
	}
}

func TestClientHTTPTransportPreservesRemoteReauthentication(t *testing.T) {
	var headers []string
	refreshes := 0
	transport := rtFunc(func(request *http.Request) (*http.Response, error) {
		headers = append(headers, request.Header.Get("Authorization"))
		if request.URL.String() != "https://pr-actions.test/v0/city/test-city/pr-actions/queue" {
			t.Fatalf("unexpected target %s", request.URL)
		}
		status := http.StatusUnauthorized
		if len(headers) == 2 {
			status = http.StatusOK
		}
		return &http.Response{
			StatusCode: status,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"availability":"ready"}`)),
			Request:    request,
		}, nil
	})
	client, err := NewRemoteCityScopedClient("https://pr-actions.test", "test-city", RemoteOptions{
		Token: func() (string, error) { return "original", nil },
		RefreshToken: func(context.Context) (string, error) {
			refreshes++
			return "refreshed", nil
		},
	}, WithHTTPTransport(transport))
	if err != nil {
		t.Fatal(err)
	}
	queue, err := client.GetPRActionQueue(context.Background())
	if err != nil || queue.Availability != PRActionAvailabilityReady {
		t.Fatalf("queue=%+v error=%v", queue, err)
	}
	if refreshes != 1 || len(headers) != 2 || headers[0] != "Bearer original" || headers[1] != "Bearer refreshed" {
		t.Fatalf("refreshes=%d headers=%v", refreshes, headers)
	}
}
