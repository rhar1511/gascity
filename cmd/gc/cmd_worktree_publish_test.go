package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/worklifecycle"
)

func worktreePublishTestRequest() api.LifecycleWorkspaceSubmitRequest {
	return api.LifecycleWorkspaceSubmitRequest{
		WorkID: "source-1", SourceStoreRef: "rig:compute", ExpectedRevision: -7, ExpectedTransitionHead: "attached-1",
		Workspace: worklifecycle.SourceWorkspace{SourceID: "source-1", StoreRef: "rig:compute", RepoDir: "/repos/compute", Root: "/worktrees", Path: "/worktrees/source-1", Branch: "work/source-1", BaseRef: "main", BaseSHA: strings.Repeat("1", 40), Creator: "gc-worktree", Owner: "compute/worker", Generation: "1", Lifecycle: "persistent", AttemptID: "attempt-1"},
	}
}

func worktreePublishTestArgs(t *testing.T) []string {
	t.Helper()
	data, err := json.Marshal(worktreePublishTestRequest())
	if err != nil {
		t.Fatal(err)
	}
	return worktreePublishTestFile(t, data)
}

func worktreePublishTestFile(t *testing.T, data []byte) []string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "request.json")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return []string{"--request-file=" + path, fmt.Sprintf("--request-sha256=%x", sha256.Sum256(data))}
}

func executeWorktreePublishTest(t *testing.T, args []string) (string, string, error) {
	t.Helper()
	var out, errOut bytes.Buffer
	command := newWorktreePublishCmd(&out, &errOut)
	command.SilenceUsage = true
	command.SilenceErrors = true
	command.SetArgs(args)
	err := command.Execute()
	return out.String(), errOut.String(), err
}

func TestWorktreePublishRequiresCompleteExplicitSnapshotBeforeTarget(t *testing.T) {
	old := worktreePublishTarget
	t.Cleanup(func() { worktreePublishTarget = old })
	calls := 0
	worktreePublishTarget = func() (*api.Client, error) { calls++; return nil, errors.New("must not resolve target") }
	for _, missing := range []string{"--request-file=", "--request-sha256="} {
		var args []string
		for _, a := range worktreePublishTestArgs(t) {
			if !strings.HasPrefix(a, missing) {
				args = append(args, a)
			}
		}
		if out, _, err := executeWorktreePublishTest(t, args); err == nil || out != "" || calls != 0 {
			t.Fatalf("missing %s accepted: err=%v output=%q calls=%d", missing, err, out, calls)
		}
	}
	for _, extra := range []string{"--request-sha256=invalid", "--expected-revision=5", "--attempt-id=new-attempt", "--set-metadata=gc.work_dir=/forged", "--force", "--dry-run"} {
		if out, _, err := executeWorktreePublishTest(t, append(worktreePublishTestArgs(t), extra)); err == nil || out != "" || calls != 0 {
			t.Fatalf("invalid %s accepted: err=%v output=%q calls=%d", extra, err, out, calls)
		}
	}
}

func TestWorktreePublishUsesExactSourceSnapshotAndTypedReceipt(t *testing.T) {
	var calls atomic.Int32
	transport := roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		w := httptest.NewRecorder()
		calls.Add(1)
		if r.Method != http.MethodPost || r.URL.Path != "/v0/city/pilot/lifecycle/workspaces" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL)
		}
		var request api.LifecycleWorkspaceSubmitRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		if request.WorkID != "source-1" || request.ExpectedRevision != -7 || request.ExpectedTransitionHead != "attached-1" || request.Workspace.AttemptID != "attempt-1" || request.Workspace.SourceID != "source-1" || request.Workspace.Owner != "compute/worker" {
			t.Errorf("wrong native publication %+v", request)
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(api.LifecycleWorkspaceResult{WorkID: "source-1", ReceiptID: "workspace-1", Revision: -9, Replayed: true}); err != nil {
			t.Error(err)
		}
		return w.Result(), nil
	})

	old := worktreePublishTarget
	t.Cleanup(func() { worktreePublishTarget = old })
	worktreePublishTarget = func() (*api.Client, error) {
		return api.NewCityScopedClient("http://controller.test", "pilot", api.WithHTTPTransport(transport)), nil
	}
	out, errOut, err := executeWorktreePublishTest(t, append(worktreePublishTestArgs(t), "--json"))
	if err != nil || errOut != "" || calls.Load() != 1 {
		t.Fatalf("error=%v stderr=%s requests=%d", err, errOut, calls.Load())
	}
	var result worktreePublishResult
	if err := json.Unmarshal([]byte(out), &result); err != nil || !result.OK || result.SchemaVersion != "1" || result.WorkID != "source-1" || result.SourceStoreRef != "rig:compute" || result.ReceiptID != "workspace-1" || result.Revision != -9 || !result.Replayed {
		t.Fatalf("receipt=%+v error=%v output=%s", result, err, out)
	}
}

func TestWorktreePublishControllerRefusalHasNoRetryOrRawFallback(t *testing.T) {
	var calls atomic.Int32
	transport := roundTripperFunc(func(_ *http.Request) (*http.Response, error) {
		w := httptest.NewRecorder()
		calls.Add(1)
		w.WriteHeader(http.StatusConflict)
		return w.Result(), nil
	})

	old := worktreePublishTarget
	t.Cleanup(func() { worktreePublishTarget = old })
	worktreePublishTarget = func() (*api.Client, error) {
		return api.NewCityScopedClient("http://controller.test", "pilot", api.WithHTTPTransport(transport)), nil
	}
	if out, _, err := executeWorktreePublishTest(t, worktreePublishTestArgs(t)); err == nil || out != "" || calls.Load() != 1 {
		t.Fatalf("error=%v output=%q requests=%d", err, out, calls.Load())
	}
	worktreePublishTarget = func() (*api.Client, error) { return nil, errors.New("controller unavailable") }
	if out, _, err := executeWorktreePublishTest(t, worktreePublishTestArgs(t)); err == nil || out != "" || calls.Load() != 1 {
		t.Fatalf("unavailable target fell back: err=%v output=%q requests=%d", err, out, calls.Load())
	}
}

func TestWorktreePublishFullCLIJSONContract(t *testing.T) {
	previousCity, previousRig := cityFlag, rigFlag
	t.Cleanup(func() { cityFlag, rigFlag = previousCity, previousRig })
	transport := roundTripperFunc(func(_ *http.Request) (*http.Response, error) {
		w := httptest.NewRecorder()
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(api.LifecycleWorkspaceResult{WorkID: "source-1", ReceiptID: "workspace-1", Revision: -9}); err != nil {
			t.Error(err)
		}
		return w.Result(), nil
	})

	old := worktreePublishTarget
	t.Cleanup(func() { worktreePublishTarget = old })
	worktreePublishTarget = func() (*api.Client, error) {
		return api.NewCityScopedClient("http://controller.test", "pilot", api.WithHTTPTransport(transport)), nil
	}
	args := append([]string{"--city", t.TempDir(), "worktree", "publish"}, worktreePublishTestArgs(t)...)
	args = append(args, "--json")
	var out, errOut bytes.Buffer
	if code := run(args, &out, &errOut); code != 0 {
		t.Fatalf("exit=%d stdout=%s stderr=%s", code, &out, &errOut)
	}
	var result worktreePublishResult
	if err := json.Unmarshal(out.Bytes(), &result); err != nil || !result.OK || result.Revision != -9 {
		t.Fatalf("receipt=%+v error=%v", result, err)
	}
}

func TestWorktreePublishChangedSavedBytesRefuseBeforeTarget(t *testing.T) {
	old := worktreePublishTarget
	t.Cleanup(func() { worktreePublishTarget = old })
	calls := 0
	worktreePublishTarget = func() (*api.Client, error) { calls++; return nil, errors.New("must not resolve target") }
	args := worktreePublishTestArgs(t)
	path := strings.TrimPrefix(args[0], "--request-file=")
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	changed := bytes.Replace(original, []byte("attempt-1"), []byte("attempt-2"), 1)
	if err := os.WriteFile(path, changed, 0o600); err != nil {
		t.Fatal(err)
	}
	if out, _, err := executeWorktreePublishTest(t, args); err == nil || out != "" || calls != 0 {
		t.Fatalf("changed snapshot accepted: error=%v output=%q calls=%d", err, out, calls)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(changed, after) {
		t.Fatal("publish rewrote saved request")
	}
}

func TestWorktreePublishMalformedSavedRequestsRefuseBeforeTarget(t *testing.T) {
	old := worktreePublishTarget
	t.Cleanup(func() { worktreePublishTarget = old })
	calls := 0
	worktreePublishTarget = func() (*api.Client, error) { calls++; return nil, errors.New("must not resolve target") }
	valid, err := json.Marshal(worktreePublishTestRequest())
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string][]byte{
		"unknown field":  append(append([]byte{}, valid[:len(valid)-1]...), []byte(`,"metadata":{"gc.work_dir":"/forged"}}`)...),
		"trailing value": append(append([]byte{}, valid...), []byte(` {}`)...),
		"null":           []byte(`null`),
		"zero revision":  bytes.Replace(valid, []byte(`"expected_revision":-7`), []byte(`"expected_revision":0`), 1),
		"foreign source": bytes.Replace(valid, []byte(`"source_id":"source-1"`), []byte(`"source_id":"other"`), 1),
		"empty attempt":  bytes.Replace(valid, []byte(`"attempt_id":"attempt-1"`), []byte(`"attempt_id":""`), 1),
	} {
		t.Run(name, func(t *testing.T) {
			if out, _, err := executeWorktreePublishTest(t, worktreePublishTestFile(t, data)); err == nil || out != "" || calls != 0 {
				t.Fatalf("invalid request accepted: error=%v output=%q calls=%d", err, out, calls)
			}
		})
	}
}
