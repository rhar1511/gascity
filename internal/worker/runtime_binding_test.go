package worker

import (
	"context"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/routing"
	"github.com/gastownhall/gascity/internal/session"
)

func TestWorkerCreateRetainsRuntimeBackendIdentity(t *testing.T) {
	for _, mode := range []CreateMode{CreateModeStarted, CreateModeDeferred} {
		t.Run(string(mode), func(t *testing.T) {
			local, remote := runtime.NewFake(), runtime.NewFake()
			fingerprint := strings.Repeat("a", 64)
			sp, err := routing.New("local", map[string]runtime.Provider{"local": local, "remote@" + fingerprint: remote})
			if err != nil {
				t.Fatal(err)
			}
			manager := session.NewManagerWithOptions(beads.NewMemStore(), sp)
			handle, err := NewSessionHandle(SessionHandleConfig{Manager: manager, Session: SessionSpec{Template: "helper", Command: "coding-provider", Provider: "coding-provider", WorkDir: t.TempDir(), RuntimeBackend: "remote", RuntimeFingerprint: fingerprint}})
			if err != nil {
				t.Fatal(err)
			}
			created, err := handle.Create(context.Background(), mode)
			if err != nil {
				t.Fatal(err)
			}
			stored, err := manager.PersistedStore().Get(created.ID)
			if err != nil {
				t.Fatal(err)
			}
			if stored.RuntimeBackend != "remote" || stored.RuntimeFingerprint != fingerprint {
				t.Fatalf("worker runtime identity lost: %#v", stored)
			}
			wantStarts := 0
			if mode == CreateModeStarted {
				wantStarts = 1
			}
			if local.CountCalls("Start", stored.SessionName) != 0 || remote.CountCalls("Start", stored.SessionName) != wantStarts {
				t.Fatal("worker creation selected another backend")
			}
		})
	}
}
