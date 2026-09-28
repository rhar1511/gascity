package main

import (
	"github.com/gastownhall/gascity/internal/qualification"
	"github.com/gastownhall/gascity/internal/selectorinventory"
)

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
func (cs *controllerState) JoinSelectorExternalLedger(raw []byte, expected selectorinventory.ExternalLedgerExpectation) selectorinventory.ExternalLedgerJoinResult {
	snapshot := cs.InFlightDispatchIdentitySnapshot(expected.ExecutionGeneration)
	registry, availability := selectorRegistrySnapshotInput(snapshot, expected.ExecutionGeneration)
	if availability.Status != qualification.StatusAvailable {
		return unavailableSelectorExternalJoin(availability.Reason)
	}
	result := selectorinventory.JoinExternalLedger(raw, expected, registry)
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
	for _, identity := range snapshot.Identities {
		if identity.ExecutionGeneration != expectedExecutionGeneration {
			return unavailable("controller_execution_generation_mismatch")
		}
		switch identity.WorkKind {
		case "exec":
			if identity.WorkID != "" {
				return unavailable("exec_dispatch_has_unexpected_formula_root")
			}
			return unavailable("exec_dispatch_has_no_formula_root")
		case "formula_root":
			if identity.WorkIdentityAvailability.Status != qualification.StatusAvailable || identity.WorkID == "" {
				return unavailable("formula_root_identity_unavailable")
			}
		default:
			return unavailable("dispatch_work_kind_unavailable")
		}
		identities = append(identities, selectorinventory.RegistryDispatchIdentity{
			ScopedOrderID:       identity.ScopedOrder,
			RunID:               identity.RunID,
			WorkID:              identity.WorkID,
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
		switch identity.WorkKind {
		case "exec":
			if identity.WorkID != "" {
				return "exec_dispatch_has_unexpected_formula_root"
			}
			return "exec_dispatch_has_no_formula_root"
		case "formula_root":
			if identity.WorkIdentityAvailability.Status != qualification.StatusAvailable || identity.WorkID == "" {
				return "formula_root_identity_unavailable"
			}
		default:
			return "dispatch_work_kind_unavailable"
		}
	}
	return "exact_work_identity_unavailable"
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
	return ""
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
