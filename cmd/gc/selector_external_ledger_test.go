package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/selectorinventory"
)

func TestControllerStateSelectorInFlightResolutionDigestUsesExactFormulaIdentity(t *testing.T) {
	state, registry := selectorExternalLedgerTestState("execution-generation-1", "formula_root", true)
	got := state.SelectorInFlightResolutionDigest("execution-generation-1", []byte("0123456789abcdef0123456789abcdef"))
	if got.Status != selectorinventory.StatusAvailable || len(got.SHA256) != 64 || got.IssueCode != "" {
		t.Fatalf("resolution digest = %#v, want available keyed digest", got)
	}

	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{"city.order", "run-1", "work-1", registry.generation} {
		if strings.Contains(string(encoded), raw) {
			t.Fatalf("resolution result disclosed transient identity %q: %s", raw, encoded)
		}
	}
}

func TestControllerStateSelectorInFlightResolutionDigestAcceptsExecIdentityWithoutFormulaRoot(t *testing.T) {
	state, registry := selectorExternalLedgerTestState("execution-generation-exec", "exec", false)
	got := state.SelectorInFlightResolutionDigest("execution-generation-exec", []byte("0123456789abcdef0123456789abcdef"))
	if got.Status != selectorinventory.StatusAvailable || len(got.SHA256) != 64 || got.IssueCode != "" {
		t.Fatalf("exec resolution digest = %#v, want available exact identity digest", got)
	}

	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{"city.order", "run-1", registry.generation, "work-1"} {
		if strings.Contains(string(encoded), raw) {
			t.Fatalf("exec resolution result disclosed transient identity %q: %s", raw, encoded)
		}
	}
}

func TestControllerStateSelectorInFlightResolutionDistinguishesMissingWorkKinds(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	tests := []struct {
		name     string
		workKind string
		bindWork bool
		wantCode string
	}{
		{name: "formula root is unresolved", workKind: "formula_root", wantCode: "formula_root_identity_unavailable"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			state, _ := selectorExternalLedgerTestState("execution-generation-1", test.workKind, test.bindWork)
			got := state.SelectorInFlightResolutionDigest("execution-generation-1", key)
			if got.Status != selectorinventory.StatusUnavailable || got.IssueCode != test.wantCode || got.SHA256 != "" {
				t.Fatalf("resolution digest = %#v, want unavailable issue %q", got, test.wantCode)
			}
		})
	}
}

func TestSelectorRegistrySnapshotInputFailsClosedOnPendingStartAndFenceChange(t *testing.T) {
	registry := newOrderDispatchIdentityRegistryWithGeneration("execution-generation-1")
	registry.reserve("city.order")
	pending := registry.SnapshotForGeneration("execution-generation-1")
	if _, availability := selectorRegistrySnapshotInput(pending, "execution-generation-1"); availability.Status != "unavailable" || availability.Reason != "dispatch_identity_start_pending" {
		t.Fatalf("pending start availability = %#v", availability)
	}

	snapshot := OrderDispatchIdentitySnapshot{
		Availability:        availableSelectorObservation(),
		ExecutionGeneration: "execution-generation-1",
		StartFence:          12,
		EndFence:            13,
		Identities:          []OrderDispatchIdentity{},
	}
	if _, availability := selectorRegistrySnapshotInput(snapshot, "execution-generation-1"); availability.Status != "unavailable" || availability.Reason != "in_flight_identity_snapshot_raced" {
		t.Fatalf("changed fence availability = %#v", availability)
	}
}

func TestControllerStateJoinSelectorExternalLedgerStopsAtUnresolvedIdentity(t *testing.T) {
	state, _ := selectorExternalLedgerTestState("execution-generation-1", "formula_root", false)
	got := state.JoinSelectorExternalLedger(context.Background(), nil, nil, selectorinventory.ExternalLedgerExpectation{
		ExecutionGeneration: "execution-generation-1",
	}, "host-id", "boot-id")
	if got.Status != selectorinventory.StatusUnavailable || got.IssueCode != "formula_root_identity_unavailable" || got.Evidence != nil {
		t.Fatalf("join result = %#v, want unresolved controller identity to fail before host verification", got)
	}
}

func TestControllerStateJoinSelectorExternalLedgerRequiresHostSignedEvidence(t *testing.T) {
	state, _ := selectorExternalLedgerTestState("execution-generation-1", "formula_root", true)
	got := state.JoinSelectorExternalLedger(context.Background(), nil, nil, selectorinventory.ExternalLedgerExpectation{
		ExecutionGeneration: "execution-generation-1",
	}, "host-id", "boot-id")
	if got.Status != selectorinventory.StatusUnavailable || got.IssueCode != "host_collector_record_unavailable" || got.Evidence != nil {
		t.Fatalf("join result = %#v, want host-signed collector evidence required", got)
	}
}

func TestControllerStateJoinSelectorExternalLedgerFencesRegistryChangeAfterVerification(t *testing.T) {
	state, registry := selectorExternalLedgerTestState("execution-generation-1", "formula_root", true)
	calls := 0
	verifier := selectorExternalLedgerVerifierFunc(func(_ context.Context, raw []byte, expected selectorinventory.ExternalLedgerExpectation, snapshot selectorinventory.RegistrySnapshotInput, hostID, bootID string) selectorinventory.ExternalLedgerJoinResult {
		calls++
		if string(raw) != "signed-host-record" {
			t.Errorf("verifier raw record = %q, want signed host record", raw)
		}
		if expected.ExecutionGeneration != "execution-generation-1" || !snapshot.Available || snapshot.ExecutionGeneration != expected.ExecutionGeneration {
			t.Errorf("verifier inputs = expected %#v snapshot %#v, want matching available generation", expected, snapshot)
		}
		if hostID != "host-id" || bootID != "boot-id" {
			t.Errorf("verifier host binding = %q/%q, want host-id/boot-id", hostID, bootID)
		}
		registry.begin("city.order.2", "run-2", "exec")
		return selectorinventory.ExternalLedgerJoinResult{Status: selectorinventory.StatusAvailable}
	})

	got := state.JoinSelectorExternalLedger(context.Background(), []byte("signed-host-record"), verifier, selectorinventory.ExternalLedgerExpectation{
		ExecutionGeneration: "execution-generation-1",
	}, "host-id", "boot-id")
	if calls != 1 {
		t.Fatalf("verifier calls = %d, want 1", calls)
	}
	if got.Status != selectorinventory.StatusUnavailable || got.IssueCode != "in_flight_identity_snapshot_raced" || got.Evidence != nil {
		t.Fatalf("join result = %#v, want registry change after verification to fail closed", got)
	}
}

type selectorExternalLedgerVerifierFunc func(context.Context, []byte, selectorinventory.ExternalLedgerExpectation, selectorinventory.RegistrySnapshotInput, string, string) selectorinventory.ExternalLedgerJoinResult

func (verify selectorExternalLedgerVerifierFunc) VerifyAndJoin(ctx context.Context, raw []byte, expected selectorinventory.ExternalLedgerExpectation, snapshot selectorinventory.RegistrySnapshotInput, hostID, bootID string) selectorinventory.ExternalLedgerJoinResult {
	return verify(ctx, raw, expected, snapshot, hostID, bootID)
}

func selectorExternalLedgerTestState(generation, workKind string, bindWork bool) (*controllerState, *orderDispatchIdentityRegistry) {
	registry := newOrderDispatchIdentityRegistryWithGeneration(generation)
	lease := registry.begin("city.order", "run-1", workKind)
	if bindWork {
		lease.bindWork("work-1")
	}
	return &controllerState{orderDispatchIdentityRegistry: registry}, registry
}
