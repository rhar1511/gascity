package main

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

type observedRuntimeProvider struct {
	runtime.Provider
	meta map[string]string
}

func (p observedRuntimeProvider) GetMeta(_, key string) (string, error) {
	return p.meta[key], nil
}

func observedRuntimeInfo() sessionpkg.Info {
	return sessionpkg.Info{
		ID:                       "session-1",
		ConfiguredNamedSession:   true,
		ConfiguredNamedIdentity:  "arbitrary-agent",
		SessionNameMetadata:      "city--arbitrary-agent",
		MetadataState:            string(sessionpkg.StateAwake),
		StateReason:              "reset-pending",
		ContinuationResetPending: "true",
		ResetCommittedAt:         "2026-09-22T10:00:00Z",
		PendingCreateStartedAt:   "2026-09-22T10:00:00Z",
		InstanceToken:            "token-1",
		Generation:               "79",
		SessionCircuitState:      sessionpkg.SessionCircuitStateOpen,
	}
}

func TestAdoptObservedRuntimeClearsGenericResidueAndBreaker(t *testing.T) {
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	identity := "arbitrary-agent"
	cb := newSessionCircuitBreaker(sessionCircuitBreakerConfig{MaxRestarts: 1, Window: time.Hour, ResetAfter: time.Hour})
	cb.RecordRestart(identity, now.Add(-2*time.Minute))
	if got := cb.RecordRestart(identity, now.Add(-time.Minute)); got != circuitOpen {
		t.Fatalf("RecordRestart state = %v, want open", got)
	}

	patch, gotIdentity, _, breakerChanged, adopted := adoptObservedRuntimeIfNeeded(
		observedRuntimeInfo(),
		"city--arbitrary-agent",
		true,
		true,
		observedRuntimeProvider{meta: map[string]string{
			"GC_SESSION_ID":     "session-1",
			"GC_INSTANCE_TOKEN": "token-1",
			"GC_RUNTIME_EPOCH":  "79",
		}},
		cb,
		now,
	)
	if !adopted {
		t.Fatal("matching live runtime was not adopted")
	}
	if gotIdentity != identity {
		t.Fatalf("identity = %q, want %q", gotIdentity, identity)
	}
	if !breakerChanged {
		t.Fatal("breakerChanged = false, want true")
	}
	if patch["state"] != string(sessionpkg.StateActive) || patch["state_reason"] != "creation_complete" {
		t.Fatalf("lifecycle patch = %#v, want active/creation_complete", patch)
	}
	for _, key := range []string{"continuation_reset_pending", "reset_committed_at", "pending_create_started_at", "pending_create_claim", "sleep_reason", "session_circuit_state"} {
		if patch[key] != "" {
			t.Errorf("patch[%q] = %q, want cleared", key, patch[key])
		}
	}
	if cb.IsOpen(identity, now) {
		t.Fatal("breaker remains open after adoption")
	}
}

func TestAdoptObservedRuntimeRejectsMismatchedIdentity(t *testing.T) {
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	identity := "arbitrary-agent"
	cb := newSessionCircuitBreaker(sessionCircuitBreakerConfig{MaxRestarts: 1, Window: time.Hour, ResetAfter: time.Hour})
	cb.RecordRestart(identity, now.Add(-2*time.Minute))
	if got := cb.RecordRestart(identity, now.Add(-time.Minute)); got != circuitOpen {
		t.Fatalf("RecordRestart state = %v, want open", got)
	}

	_, _, _, _, adopted := adoptObservedRuntimeIfNeeded(
		observedRuntimeInfo(),
		"city--arbitrary-agent",
		true,
		true,
		observedRuntimeProvider{meta: map[string]string{
			"GC_SESSION_ID":     "other-session",
			"GC_INSTANCE_TOKEN": "token-1",
			"GC_RUNTIME_EPOCH":  "79",
		}},
		cb,
		now,
	)
	if adopted {
		t.Fatal("mismatched runtime identity was adopted")
	}
	if !cb.IsOpen(identity, now) {
		t.Fatal("breaker was cleared for mismatched runtime")
	}
}

func TestAdoptObservedRuntimeIsProviderAndAgentAgnostic(t *testing.T) {
	now := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	info := observedRuntimeInfo()
	info.ConfiguredNamedIdentity = "provider-with-spaces/agent.v2"
	info.SessionNameMetadata = "city--opaque"
	info.InstanceToken = "opaque-token"
	info.Generation = "opaque-epoch"
	patch, _, _, _, adopted := adoptObservedRuntimeIfNeeded(
		info,
		"city--opaque",
		true,
		true,
		observedRuntimeProvider{meta: map[string]string{
			"GC_SESSION_ID":     "session-1",
			"GC_INSTANCE_TOKEN": "opaque-token",
			"GC_RUNTIME_EPOCH":  "opaque-epoch",
		}},
		nil,
		now,
	)
	if !adopted {
		t.Fatal("opaque provider/agent identity was not adopted")
	}
	if patch["state_reason"] != "creation_complete" {
		t.Fatalf("state_reason = %q, want creation_complete", patch["state_reason"])
	}
}
