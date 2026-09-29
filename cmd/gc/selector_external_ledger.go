package main

import (
	"context"

	"github.com/gastownhall/gascity/internal/qualification"
	"github.com/gastownhall/gascity/internal/selectorinventory"
)

type selectorExternalLedgerVerifier interface {
	VerifyAndJoin(context.Context, []byte, selectorinventory.ExternalLedgerExpectation, selectorinventory.RegistrySnapshotInput, string, string) selectorinventory.ExternalLedgerJoinResult
}

// SelectorInFlightResolutionDigest computes the selector inventory's keyed
// in-flight identity digest from the controller's current dispatch registry.
// The returned value contains no raw order, run, or work identities.
func (cs *controllerState) SelectorInFlightResolutionDigest(expectedExecutionGeneration string, key []byte) selectorinventory.ResolutionDigestResult {
	snapshot := cs.InFlightDispatchIdentitySnapshot(expectedExecutionGeneration)
	registry, availability := selectorRegistrySnapshotInput(snapshot, expectedExecutionGeneration)
	if availability.Status != qualification.StatusAvailable {
		return unavailableSelectorResolution(availability.Reason)
	}
	result := selectorinventory.DigestInFlightResolution(registry, expectedExecutionGeneration, key)
	if result.Status != selectorinventory.StatusAvailable {
		return result
	}
	if issue := selectorDispatchSnapshotFenceChange(snapshot, cs.InFlightDispatchIdentitySnapshot(expectedExecutionGeneration), expectedExecutionGeneration); issue != "" {
		return unavailableSelectorResolution(issue)
	}
	return result
}

// JoinSelectorExternalLedger validates a retained external ledger against a
// fresh controller registry snapshot. The registry is sampled again after the
// join so a change during ledger validation cannot leave usable evidence.
func (cs *controllerState) JoinSelectorExternalLedger(ctx context.Context, raw []byte, verifier selectorExternalLedgerVerifier, expected selectorinventory.ExternalLedgerExpectation, hostID, bootID string) selectorinventory.ExternalLedgerJoinResult {
	if ctx == nil {
		return unavailableSelectorExternalJoin("host_collector_record_unavailable")
	}
	snapshot := cs.InFlightDispatchIdentitySnapshot(expected.ExecutionGeneration)
	registry, availability := selectorRegistrySnapshotInput(snapshot, expected.ExecutionGeneration)
	if availability.Status != qualification.StatusAvailable {
		return unavailableSelectorExternalJoin(availability.Reason)
	}
	if verifier == nil {
		return unavailableSelectorExternalJoin("host_collector_record_unavailable")
	}
	result := verifier.VerifyAndJoin(ctx, raw, expected, registry, hostID, bootID)
	if issue := selectorDispatchSnapshotFenceChange(snapshot, cs.InFlightDispatchIdentitySnapshot(expected.ExecutionGeneration), expected.ExecutionGeneration); issue != "" {
		return unavailableSelectorExternalJoin(issue)
	}
	return result
}

func selectorRegistrySnapshotInput(snapshot OrderDispatchIdentitySnapshot, expectedExecutionGeneration string) (selectorinventory.RegistrySnapshotInput, SelectorObservationAvailability) {
	unavailable := func(reason string) (selectorinventory.RegistrySnapshotInput, SelectorObservationAvailability) {
		if reason == "" {
			reason = "in_flight_registry_unavailable"
		}
		return selectorinventory.RegistrySnapshotInput{}, unavailableSelectorObservation(reason)
	}
	if expectedExecutionGeneration == "" || snapshot.ExecutionGeneration == "" {
		return unavailable("controller_execution_generation_unavailable")
	}
	if snapshot.ExecutionGeneration != expectedExecutionGeneration {
		return unavailable("controller_execution_generation_mismatch")
	}
	if snapshot.StartFence != snapshot.EndFence {
		return unavailable("in_flight_identity_snapshot_raced")
	}
	if snapshot.Availability.Status != qualification.StatusAvailable {
		reason := snapshot.Availability.Reason
		if reason == "exact_work_identity_unavailable" || reason == "" {
			if identityIssue := selectorDispatchWorkIdentityIssue(snapshot.Identities); identityIssue != "" {
				reason = identityIssue
			}
		}
		return unavailable(reason)
	}

	identities := make([]selectorinventory.RegistryDispatchIdentity, 0, len(snapshot.Identities))
	seen := make(map[orderDispatchIdentityKey]struct{}, len(snapshot.Identities))
	for _, identity := range snapshot.Identities {
		if issue := selectorDispatchIdentityIssue(identity, expectedExecutionGeneration); issue != "" {
			return unavailable(issue)
		}
		key := orderDispatchIdentityKey{scopedOrder: identity.ScopedOrder, runID: identity.RunID}
		if _, exists := seen[key]; exists {
			return unavailable("in_flight_identity_ambiguous")
		}
		seen[key] = struct{}{}
		identities = append(identities, selectorinventory.RegistryDispatchIdentity{
			ScopedOrderID:       identity.ScopedOrder,
			RunID:               identity.RunID,
			WorkID:              identity.WorkID,
			WorkKind:            identity.WorkKind,
			ExecutionGeneration: identity.ExecutionGeneration,
		})
	}
	return selectorinventory.RegistrySnapshotInput{
		Available:           true,
		ExecutionGeneration: snapshot.ExecutionGeneration,
		StartFence:          snapshot.StartFence,
		EndFence:            snapshot.EndFence,
		Identities:          identities,
	}, availableSelectorObservation()
}

func selectorDispatchWorkIdentityIssue(identities []OrderDispatchIdentity) string {
	for _, identity := range identities {
		if issue := selectorDispatchIdentityIssue(identity, identity.ExecutionGeneration); issue != "" {
			return issue
		}
	}
	return ""
}

func selectorDispatchSnapshotFenceChange(before, after OrderDispatchIdentitySnapshot, expectedExecutionGeneration string) string {
	if before.ExecutionGeneration != expectedExecutionGeneration || after.ExecutionGeneration != expectedExecutionGeneration {
		return "controller_execution_generation_mismatch"
	}
	if before.StartFence != before.EndFence || after.StartFence != after.EndFence ||
		before.StartFence != after.StartFence || before.EndFence != after.EndFence {
		return "in_flight_identity_snapshot_raced"
	}
	if after.Availability.Status != qualification.StatusAvailable {
		reason := after.Availability.Reason
		if reason == "exact_work_identity_unavailable" || reason == "" {
			return selectorDispatchWorkIdentityIssue(after.Identities)
		}
		return reason
	}
	if _, availability := selectorRegistrySnapshotInput(after, expectedExecutionGeneration); availability.Status != qualification.StatusAvailable {
		return availability.Reason
	}
	if !sameSelectorDispatchIdentities(before.Identities, after.Identities) {
		return "in_flight_identity_snapshot_conflict"
	}
	return ""
}

func sameSelectorDispatchIdentities(left, right []OrderDispatchIdentity) bool {
	if len(left) != len(right) {
		return false
	}
	byKey := make(map[orderDispatchIdentityKey]OrderDispatchIdentity, len(left))
	for _, identity := range left {
		key := orderDispatchIdentityKey{scopedOrder: identity.ScopedOrder, runID: identity.RunID}
		if _, exists := byKey[key]; exists {
			return false
		}
		byKey[key] = identity
	}
	for _, identity := range right {
		key := orderDispatchIdentityKey{scopedOrder: identity.ScopedOrder, runID: identity.RunID}
		previous, exists := byKey[key]
		if !exists || previous.WorkID != identity.WorkID || previous.WorkKind != identity.WorkKind ||
			previous.ExecutionGeneration != identity.ExecutionGeneration ||
			previous.WorkIdentityAvailability != identity.WorkIdentityAvailability {
			return false
		}
		delete(byKey, key)
	}
	return len(byKey) == 0
}

func unavailableSelectorResolution(issue string) selectorinventory.ResolutionDigestResult {
	if issue == "" {
		issue = "in_flight_registry_unavailable"
	}
	return selectorinventory.ResolutionDigestResult{
		Status: selectorinventory.StatusUnavailable, IssueCode: issue,
	}
}

func unavailableSelectorExternalJoin(issue string) selectorinventory.ExternalLedgerJoinResult {
	if issue == "" {
		issue = "in_flight_registry_unavailable"
	}
	return selectorinventory.ExternalLedgerJoinResult{
		Status: selectorinventory.StatusUnavailable, IssueCode: issue,
	}
}
