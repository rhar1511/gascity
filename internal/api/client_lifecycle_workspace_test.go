package api

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/gastownhall/gascity/internal/worklifecycle"
)

func clientWorkspaceRequest() LifecycleWorkspaceSubmitRequest {
	return LifecycleWorkspaceSubmitRequest{
		WorkID: "source-1", SourceStoreRef: "rig:compute", ExpectedRevision: -7, ExpectedTransitionHead: "attached-1",
		Workspace: worklifecycle.SourceWorkspace{SourceID: "source-1", StoreRef: "rig:compute", Path: "/worktrees/source-1", RepoDir: "/repos/compute", Root: "/worktrees", Branch: "work/source-1", BaseRef: "main", BaseSHA: "1111111111111111111111111111111111111111", Creator: "gc-worktree", Owner: "compute/worker", Generation: "1", Lifecycle: "active", AttemptID: "attempt-1"},
	}
}

func TestClientPublishLifecycleWorkspacePreservesExactSnapshotAndAttempt(t *testing.T) {
	request := clientWorkspaceRequest()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodPost || r.URL.Path != "/v0/city/pilot/lifecycle/workspaces" || r.URL.RawQuery != "" || r.Header.Get("X-GC-Request") == "" {
			t.Errorf("unexpected publication request: %s %s", r.Method, r.URL)
		}
		var got LifecycleWorkspaceSubmitRequest
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&got); err != nil {
			t.Error(err)
		}
		if !reflect.DeepEqual(got, request) {
			t.Errorf("snapshot or native tuple changed: %+v", got)
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(LifecycleWorkspaceResult{WorkID: request.WorkID, ReceiptID: "workspace-1", Revision: -9}); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	got, err := NewCityScopedClient(server.URL, "pilot").PublishLifecycleWorkspace(context.Background(), request)
	if err != nil || calls.Load() != 1 || got.WorkID != request.WorkID || got.Revision != -9 || got.ReceiptID != "workspace-1" {
		t.Fatalf("result=%+v error=%v calls=%d", got, err, calls.Load())
	}
}

func TestClientPublishLifecycleWorkspaceRejectsIncompleteInputBeforeTransport(t *testing.T) {
	for _, alter := range []func(*LifecycleWorkspaceSubmitRequest){
		func(r *LifecycleWorkspaceSubmitRequest) { r.ExpectedRevision = 0 },
		func(r *LifecycleWorkspaceSubmitRequest) { r.ExpectedTransitionHead = "" },
		func(r *LifecycleWorkspaceSubmitRequest) { r.Workspace.AttemptID = "" },
		func(r *LifecycleWorkspaceSubmitRequest) { r.Workspace.SourceID = "step-1" },
		func(r *LifecycleWorkspaceSubmitRequest) { r.Workspace.StoreRef = "rig:other" },
		func(r *LifecycleWorkspaceSubmitRequest) { r.Workspace.Path = "relative/path" },
	} {
		request := clientWorkspaceRequest()
		alter(&request)
		if err := request.Validate(); err == nil {
			t.Fatal("invalid publication input passed validation")
		}
		if _, err := (&Client{}).PublishLifecycleWorkspace(context.Background(), request); err == nil {
			t.Fatal("invalid publication reached transport")
		}
	}
	var absent *Client
	if _, err := absent.PublishLifecycleWorkspace(context.Background(), clientWorkspaceRequest()); err == nil {
		t.Fatal("nil client accepted publication")
	}
}

func TestClientPublishLifecycleWorkspaceDoesNotRetryRefusal(t *testing.T) {
	for _, status := range []int{http.StatusForbidden, http.StatusConflict, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { calls.Add(1); w.WriteHeader(status) }))
			defer server.Close()
			if _, err := NewCityScopedClient(server.URL, "pilot").PublishLifecycleWorkspace(context.Background(), clientWorkspaceRequest()); err == nil || calls.Load() != 1 {
				t.Fatalf("error=%v calls=%d", err, calls.Load())
			}
		})
	}
}

func TestClientPublishLifecycleWorkspaceRejectsUnboundReceipt(t *testing.T) {
	for _, result := range []LifecycleWorkspaceResult{
		{WorkID: "other", ReceiptID: "workspace-1", Revision: 9},
		{WorkID: "source-1", Revision: 9},
		{WorkID: "source-1", ReceiptID: "workspace-1"},
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(result); err != nil {
				t.Error(err)
			}
		}))
		_, err := NewCityScopedClient(server.URL, "pilot").PublishLifecycleWorkspace(context.Background(), clientWorkspaceRequest())
		server.Close()
		if err == nil {
			t.Fatalf("accepted unbound receipt %+v", result)
		}
	}
}

func TestClientPublishLifecycleWorkspacePreservesAuthenticatedWriteGrant(t *testing.T) {
	var mu sync.Mutex
	var bound GrantBinding
	var actualHash string
	var calls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer fixture-token" || r.Header.Get("X-GC-City-Write") != "fixture-grant" {
			t.Error("existing authenticated transport lost")
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		mu.Lock()
		actualHash = fmt.Sprintf("%x", sha256.Sum256(body))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(LifecycleWorkspaceResult{WorkID: "source-1", ReceiptID: "workspace-1", Revision: -9, Replayed: true}); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	client, err := NewRemoteCityScopedClient(server.URL, "pilot", RemoteOptions{
		InsecureSkipVerify: true,
		Token:              func() (string, error) { return "fixture-token", nil }, Grant: func(b GrantBinding) (string, error) {
			mu.Lock()
			defer mu.Unlock()
			bound = b
			return "fixture-grant", nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.PublishLifecycleWorkspace(context.Background(), clientWorkspaceRequest())
	mu.Lock()
	defer mu.Unlock()
	if err != nil || !result.Replayed || calls.Load() != 1 || bound.Method != http.MethodPost || bound.Path != "/v0/city/pilot/lifecycle/workspaces" || bound.BodySHA256 != actualHash || bound.ReqDigest == "" {
		t.Fatalf("result=%+v error=%v grant=%+v", result, err, bound)
	}
}

func TestClientPublishLifecycleWorkspaceHonorsCancellation(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := NewCityScopedClient(server.URL, "pilot").PublishLifecycleWorkspace(ctx, clientWorkspaceRequest()); err == nil || calls.Load() != 0 {
		t.Fatalf("error=%v requests=%d", err, calls.Load())
	}
}
