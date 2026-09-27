package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/citywriteauth"
)

func TestClientPRActionBindsGrantToExactWireRequest(t *testing.T) {
	var binding GrantBinding
	var wire []byte
	var headerKey, grant, path string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wire, _ = io.ReadAll(r.Body)
		headerKey, grant, path = r.Header.Get("Idempotency-Key"), r.Header.Get("X-GC-City-Write"), r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(PRActionResult{ID: "receipt-1", Status: PRActionStatusVerified})
	}))
	defer server.Close()
	client, err := NewRemoteCityScopedClient(server.URL, "test-city", RemoteOptions{Grant: func(b GrantBinding) (string, error) {
		binding = b
		return "private-test-grant", nil
	}})
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
