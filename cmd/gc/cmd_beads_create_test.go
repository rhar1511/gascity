package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/spf13/cobra"
)

func executeBeadsCreateTest(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	var rig string
	root := &cobra.Command{Use: "gc", SilenceUsage: true, SilenceErrors: true}
	root.PersistentFlags().StringVar(&rig, "rig", "", "exact rig")
	root.AddCommand(newBeadsCmd(&stdout, &stderr))
	root.SetArgs(args)
	err := root.Execute()
	return stdout.String(), stderr.String(), err
}

func TestBeadsCreateRequiresExactExplicitInputBeforeResolvingTarget(t *testing.T) {
	old := beadsCreateTarget
	t.Cleanup(func() { beadsCreateTarget = old })
	calls := 0
	beadsCreateTarget = func() (*api.Client, error) { calls++; return nil, errors.New("target must not be resolved") }
	for _, args := range [][]string{
		{"beads", "create", "Inspect workspace", "--idempotency-key=qualification-1"},
		{"--rig=../compute", "beads", "create", "Inspect workspace", "--idempotency-key=qualification-1"},
		{"--rig=compute", "beads", "create", "Inspect workspace"},
		{"--rig=compute", "beads", "create", "  ", "--idempotency-key=qualification-1"},
		{"--rig=compute", "beads", "create", "Inspect workspace", "--idempotency-key=qualification-1", "--assignee=other"},
		{"--rig=compute", "beads", "create", "Inspect workspace", "--idempotency-key=qualification-1", "--set-metadata=gc.work_dir=/tmp"},
		{"--rig=compute", "beads", "create", "Inspect workspace", "--idempotency-key=qualification-1", "--status=in_progress"},
	} {
		stdout, _, err := executeBeadsCreateTest(t, args...)
		if err == nil || stdout != "" || calls != 0 {
			t.Fatalf("args=%q: err=%v stdout=%q target calls=%d", args, err, stdout, calls)
		}
	}
}

func TestBeadsCreateUsesTypedAPIAndReportsStableIdentity(t *testing.T) {
	var calls atomic.Int32
	transport := roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		w := httptest.NewRecorder()
		calls.Add(1)
		if r.Method != http.MethodPost || r.URL.Path != "/v0/city/pilot/beads" || r.Header.Get("Idempotency-Key") != "qualification-1" {
			t.Errorf("unexpected create request %s %s", r.Method, r.URL)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		if err := json.NewEncoder(w).Encode(beads.Bead{ID: "co-1", Title: "Inspect workspace", Type: "task", Status: "open"}); err != nil {
			t.Error(err)
		}
		return w.Result(), nil
	})

	old := beadsCreateTarget
	t.Cleanup(func() { beadsCreateTarget = old })
	beadsCreateTarget = func() (*api.Client, error) {
		return api.NewCityScopedClient("http://controller.test", "pilot", api.WithHTTPTransport(transport)), nil
	}
	stdout, stderr, err := executeBeadsCreateTest(t, "--rig=compute", "beads", "create", "Inspect workspace", "--description=Read only.", "--idempotency-key=qualification-1", "--json")
	if err != nil || stderr != "" || calls.Load() != 1 {
		t.Fatalf("err=%v stdout=%q stderr=%q requests=%d", err, stdout, stderr, calls.Load())
	}
	var result beadsCreateResult
	if err := json.Unmarshal([]byte(stdout), &result); err != nil {
		t.Fatal(err)
	}
	if !result.OK || result.SchemaVersion != "1" || result.BeadID != "co-1" || result.StoreRef != "rig:compute" || result.IdempotencyKey != "qualification-1" {
		t.Fatalf("unexpected receipt: %+v", result)
	}
}

func TestBeadsCreateUnavailableTargetNeverFallsBack(t *testing.T) {
	old := beadsCreateTarget
	t.Cleanup(func() { beadsCreateTarget = old })
	beadsCreateTarget = func() (*api.Client, error) { return nil, errors.New("controller unavailable") }
	stdout, stderr, err := executeBeadsCreateTest(t, "--rig=compute", "beads", "create", "Inspect workspace", "--idempotency-key=qualification-1")
	if err == nil || stdout != "" || !strings.Contains(stderr, "controller unavailable") {
		t.Fatalf("err=%v stdout=%q stderr=%q", err, stdout, stderr)
	}
}

func TestBeadsCreateTransportFailureNeverRetriesOrFallsBack(t *testing.T) {
	var calls atomic.Int32
	transport := roundTripperFunc(func(_ *http.Request) (*http.Response, error) {
		w := httptest.NewRecorder()
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
		return w.Result(), nil
	})

	old := beadsCreateTarget
	t.Cleanup(func() { beadsCreateTarget = old })
	beadsCreateTarget = func() (*api.Client, error) {
		return api.NewCityScopedClient("http://controller.test", "pilot", api.WithHTTPTransport(transport)), nil
	}
	stdout, stderr, err := executeBeadsCreateTest(t, "--rig=compute", "beads", "create", "Inspect workspace", "--idempotency-key=qualification-1")
	if err == nil || stdout != "" || stderr == "" || calls.Load() != 1 {
		t.Fatalf("err=%v stdout=%q stderr=%q requests=%d", err, stdout, stderr, calls.Load())
	}
}

func TestBeadsCreateFullCLIJSONContract(t *testing.T) {
	previousCity, previousRig := cityFlag, rigFlag
	t.Cleanup(func() { cityFlag, rigFlag = previousCity, previousRig })
	transport := roundTripperFunc(func(_ *http.Request) (*http.Response, error) {
		w := httptest.NewRecorder()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		if err := json.NewEncoder(w).Encode(beads.Bead{ID: "co-1", Title: "Inspect workspace", Type: "task", Status: "open"}); err != nil {
			t.Error(err)
		}
		return w.Result(), nil
	})

	old := beadsCreateTarget
	t.Cleanup(func() { beadsCreateTarget = old })
	beadsCreateTarget = func() (*api.Client, error) {
		return api.NewCityScopedClient("http://controller.test", "pilot", api.WithHTTPTransport(transport)), nil
	}
	var stdout, stderr bytes.Buffer
	code := run([]string{"--city", t.TempDir(), "--rig=compute", "beads", "create", "Inspect workspace", "--idempotency-key=qualification-1", "--json"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("full CLI exit=%d stderr=%s stdout=%s", code, &stderr, &stdout)
	}
	var receipt beadsCreateResult
	if err := json.Unmarshal(stdout.Bytes(), &receipt); err != nil || !receipt.OK || receipt.BeadID != "co-1" {
		t.Fatalf("receipt=%+v error=%v stdout=%s", receipt, err, &stdout)
	}
}
