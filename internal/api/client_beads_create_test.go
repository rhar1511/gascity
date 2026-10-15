package api

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/gastownhall/gascity/internal/api/genclient"
	"github.com/gastownhall/gascity/internal/beads"
)

func TestClientCreateUnassignedTaskUsesExactScopeAndIdempotency(t *testing.T) {
	request := TaskCreateRequest{Rig: "compute", Title: "Inspect isolated workspace", Description: "Read only.\nPreserve owners.", IdempotencyKey: "qualification-1"}
	var calls atomic.Int32
	transport := rtFunc(func(r *http.Request) (*http.Response, error) {
		w := httptest.NewRecorder()
		calls.Add(1)
		if r.Method != http.MethodPost || r.URL.Path != "/v0/city/pilot/beads" || r.URL.RawQuery != "" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL)
		}
		if r.Header.Get("X-GC-Request") == "" || r.Header.Get("Idempotency-Key") != request.IdempotencyKey {
			t.Errorf("missing mutation or exact idempotency header")
		}
		var body genclient.BeadCreateInputBody
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body.Rig == nil || *body.Rig != request.Rig || body.Title != request.Title || body.Description == nil || *body.Description != request.Description || body.Type == nil || *body.Type != "task" {
			t.Errorf("wrong task body: %+v", body)
		}
		if body.Assignee != nil || body.Metadata != nil || body.Parent != nil || body.Priority != nil || body.Labels != nil || body.DeferUntil != nil {
			t.Errorf("unexpected optional authority/routing fields: %+v", body)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		if err := json.NewEncoder(w).Encode(beads.Bead{ID: "co-1", Title: request.Title, Type: "task", Status: "open"}); err != nil {
			t.Error(err)
		}
		return w.Result(), nil
	})

	row, err := NewCityScopedClient("http://controller.test", "pilot", WithHTTPTransport(transport)).CreateUnassignedTask(context.Background(), request)
	if err != nil || row.ID != "co-1" || calls.Load() != 1 {
		t.Fatalf("create = %+v, %v; calls=%d", row, err, calls.Load())
	}
}

func TestClientCreateUnassignedTaskRejectsIncompleteInputBeforeRequest(t *testing.T) {
	base := TaskCreateRequest{Rig: "compute", Title: "Inspect workspace", IdempotencyKey: "qualification-1"}
	for _, tc := range []struct {
		name string
		edit func(*TaskCreateRequest)
	}{
		{"missing rig", func(r *TaskCreateRequest) { r.Rig = "" }},
		{"rig path", func(r *TaskCreateRequest) { r.Rig = "../compute" }},
		{"rig whitespace", func(r *TaskCreateRequest) { r.Rig = " compute" }},
		{"missing idempotency", func(r *TaskCreateRequest) { r.IdempotencyKey = "" }},
		{"header newline", func(r *TaskCreateRequest) { r.IdempotencyKey = "key\r\nother" }},
		{"blank title", func(r *TaskCreateRequest) { r.Title = "  " }},
		{"multiline title", func(r *TaskCreateRequest) { r.Title = "one\ntwo" }},
		{"invalid title UTF8", func(r *TaskCreateRequest) { r.Title = string([]byte{0xff}) }},
		{"description NUL", func(r *TaskCreateRequest) { r.Description = "text\x00" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := base
			tc.edit(&request)
			// No client transport exists: validation must reject first.
			if _, err := (&Client{}).CreateUnassignedTask(context.Background(), request); err == nil {
				t.Fatal("incomplete create accepted")
			}
			if err := request.Validate(); err == nil {
				t.Fatal("invalid input reached transport validation")
			}
		})
	}
}

func TestClientCreateUnassignedTaskPreservesRemoteWriteGrant(t *testing.T) {
	var mu sync.Mutex
	var binding GrantBinding
	var bodyHash string
	grants := 0
	transport := rtFunc(func(r *http.Request) (*http.Response, error) {
		w := httptest.NewRecorder()
		if r.Header.Get("Authorization") != "Bearer fixture-token" || r.Header.Get("X-GC-City-Write") != "fixture-grant" {
			t.Error("existing authenticated write transport was not used")
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		mu.Lock()
		bodyHash = fmt.Sprintf("%x", sha256.Sum256(body))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		if err := json.NewEncoder(w).Encode(beads.Bead{ID: "co-1", Title: "Inspect workspace", Type: "task", Status: "open"}); err != nil {
			t.Error(err)
		}
		return w.Result(), nil
	})

	client, err := NewRemoteCityScopedClient("https://controller.test", "pilot", RemoteOptions{
		Token: func() (string, error) { return "fixture-token", nil },
		Grant: func(b GrantBinding) (string, error) {
			mu.Lock()
			defer mu.Unlock()
			grants++
			binding = b
			return "fixture-grant", nil
		},
	}, WithHTTPTransport(transport))
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.CreateUnassignedTask(context.Background(), TaskCreateRequest{Rig: "compute", Title: "Inspect workspace", IdempotencyKey: "qualification-1"})
	mu.Lock()
	defer mu.Unlock()
	if err != nil || grants != 1 || binding.Method != http.MethodPost || binding.Path != "/v0/city/pilot/beads" || binding.BodySHA256 != bodyHash || binding.CanonicalQuery != "" || binding.ReqDigest == "" {
		t.Fatalf("err=%v grants=%d binding=%+v observed body hash=%s", err, grants, binding, bodyHash)
	}
}

func TestClientCreateUnassignedTaskHonorsCancellation(t *testing.T) {
	var calls atomic.Int32
	transport := rtFunc(func(r *http.Request) (*http.Response, error) {
		if err := r.Context().Err(); err != nil {
			return nil, err
		}
		calls.Add(1)
		t.Error("canceled request context was not propagated to transport")
		return nil, errors.New("unexpected uncanceled transport call")
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := NewCityScopedClient("http://controller.test", "pilot", WithHTTPTransport(transport)).CreateUnassignedTask(ctx, TaskCreateRequest{Rig: "compute", Title: "Inspect workspace", IdempotencyKey: "qualification-1"})
	if !errors.Is(err, context.Canceled) || calls.Load() != 0 {
		t.Fatalf("canceled create: error=%v requests=%d", err, calls.Load())
	}
}

func TestClientCreateUnassignedTaskRetainsErrorsWithoutRetry(t *testing.T) {
	for _, status := range []int{http.StatusForbidden, http.StatusNotFound, http.StatusConflict, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var calls atomic.Int32
			transport := rtFunc(func(_ *http.Request) (*http.Response, error) {
				w := httptest.NewRecorder()
				calls.Add(1)
				w.WriteHeader(status)
				return w.Result(), nil
			})

			_, err := NewCityScopedClient("http://controller.test", "pilot", WithHTTPTransport(transport)).CreateUnassignedTask(context.Background(), TaskCreateRequest{Rig: "compute", Title: "Inspect workspace", IdempotencyKey: "qualification-1"})
			if err == nil || calls.Load() != 1 {
				t.Fatalf("error=%v, calls=%d; want error and exactly one request", err, calls.Load())
			}
		})
	}
}

func TestClientCreateUnassignedTaskRejectsUnprovenCreationResponse(t *testing.T) {
	for _, row := range []beads.Bead{
		{Title: "Inspect workspace", Type: "task", Status: "open"},
		{ID: "co-1", Title: "Different", Type: "task", Status: "open"},
		{ID: "co-1", Title: "Inspect workspace", Type: "session", Status: "open"},
		{ID: "co-1", Title: "Inspect workspace", Type: "task", Status: "in_progress"},
		{ID: "co-1", Title: "Inspect workspace", Type: "task", Status: "open", Assignee: "existing-owner"},
	} {
		t.Run(row.ID+row.Type+row.Status+row.Assignee+row.Title, func(t *testing.T) {
			transport := rtFunc(func(_ *http.Request) (*http.Response, error) {
				w := httptest.NewRecorder()
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusCreated)
				if err := json.NewEncoder(w).Encode(row); err != nil {
					t.Error(err)
				}
				return w.Result(), nil
			})

			_, err := NewCityScopedClient("http://controller.test", "pilot", WithHTTPTransport(transport)).CreateUnassignedTask(context.Background(), TaskCreateRequest{Rig: "compute", Title: "Inspect workspace", IdempotencyKey: "qualification-1"})
			if err == nil {
				t.Fatal("unexpected creation response accepted")
			}
		})
	}
}
