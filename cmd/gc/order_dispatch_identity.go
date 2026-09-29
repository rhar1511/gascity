package main

import (
	"crypto/rand"
	"encoding/hex"
	"sort"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/gastownhall/gascity/internal/qualification"
)

// OrderDispatchIdentity is the privacy-limited identity of one controller
// dispatch. RunID is the order tracking bead; WorkID is the instantiated
// formula root when the dispatch has one. This value never carries an order
// command, environment, or filesystem path.
type OrderDispatchIdentity struct {
	ScopedOrder              string                          `json:"scoped_order"`
	RunID                    string                          `json:"run_id"`
	WorkID                   string                          `json:"work_id,omitempty"`
	WorkKind                 string                          `json:"work_kind"`
	WorkIdentityAvailability SelectorObservationAvailability `json:"work_identity_availability"`
	ExecutionGeneration      string                          `json:"execution_generation"`
}

// OrderDispatchIdentitySnapshot is a fenced copy of the controller's exact
// in-flight dispatch identities. A consumer must ignore Identities unless
// Availability is available and both mutation fences match.
type OrderDispatchIdentitySnapshot struct {
	Availability        SelectorObservationAvailability `json:"availability"`
	ExecutionGeneration string                          `json:"execution_generation,omitempty"`
	StartFence          uint64                          `json:"start_fence"`
	EndFence            uint64                          `json:"end_fence"`
	Identities          []OrderDispatchIdentity         `json:"identities"`
}

type orderDispatchIdentityKey struct {
	scopedOrder string
	runID       string
}

type orderDispatchIdentityEntry struct {
	identity OrderDispatchIdentity
	token    uint64
}

// orderDispatchIdentityRegistry is shared by a CityRuntime's scheduled order
// dispatcher and its short-lived webhook dispatchers. Its generation is
// created once per controller execution and changes on process restart.
type orderDispatchIdentityRegistry struct {
	mu sync.RWMutex

	generation        string
	version           uint64
	nextToken         uint64
	entries           map[orderDispatchIdentityKey]orderDispatchIdentityEntry
	pendingStarts     map[uint64]string
	unavailableReason string
}

type orderDispatchIdentityLease struct {
	registry   *orderDispatchIdentityRegistry
	key        orderDispatchIdentityKey
	generation string
	token      uint64
	registered bool
}

type orderDispatchIdentityReservation struct {
	registry    *orderDispatchIdentityRegistry
	scopedOrder string
	generation  string
	token       uint64
	reserved    bool
}

func newOrderDispatchIdentityRegistry() *orderDispatchIdentityRegistry {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return newOrderDispatchIdentityRegistryWithGeneration("")
	}
	return newOrderDispatchIdentityRegistryWithGeneration(hex.EncodeToString(nonce[:]))
}

func newOrderDispatchIdentityRegistryWithGeneration(generation string) *orderDispatchIdentityRegistry {
	registry := &orderDispatchIdentityRegistry{
		generation:    generation,
		entries:       make(map[orderDispatchIdentityKey]orderDispatchIdentityEntry),
		pendingStarts: make(map[uint64]string),
	}
	if !safeDispatchIdentityValue(generation) {
		registry.generation = ""
		registry.unavailableReason = "controller_execution_generation_unavailable"
	}
	return registry
}

func safeDispatchIdentityValue(value string) bool {
	if value == "" || value != strings.TrimSpace(value) || len(value) > 256 || !utf8.ValidString(value) {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) || unicode.IsSpace(r) || r == '/' || r == '\\' {
			return false
		}
	}
	return true
}

func selectorDispatchIdentityIssue(identity OrderDispatchIdentity, expectedExecutionGeneration string) string {
	if !safeDispatchIdentityValue(identity.ScopedOrder) {
		return "canonical_order_identity_unavailable"
	}
	if !safeDispatchIdentityValue(identity.RunID) {
		return "canonical_run_identity_unavailable"
	}
	if !safeDispatchIdentityValue(expectedExecutionGeneration) || identity.ExecutionGeneration != expectedExecutionGeneration {
		return "controller_execution_generation_mismatch"
	}
	switch identity.WorkKind {
	case "formula_root":
		if !safeDispatchIdentityValue(identity.WorkID) ||
			identity.WorkIdentityAvailability.Status != qualification.StatusAvailable ||
			identity.WorkIdentityAvailability.Reason != "" {
			return "formula_root_identity_unavailable"
		}
	case "exec":
		if identity.WorkID != "" {
			return "exec_dispatch_has_unexpected_formula_root"
		}
		if identity.WorkIdentityAvailability.Status != qualification.StatusUnavailable ||
			identity.WorkIdentityAvailability.Reason != "exec_has_no_canonical_work_identity" {
			return "exec_dispatch_identity_unavailable"
		}
	default:
		return "dispatch_work_kind_unavailable"
	}
	return ""
}

func (registry *orderDispatchIdentityRegistry) begin(scopedOrder, runID, workKind string) orderDispatchIdentityLease {
	reservation := registry.reserve(scopedOrder)
	return reservation.promote(runID, workKind)
}

func (registry *orderDispatchIdentityRegistry) reserve(scopedOrder string) orderDispatchIdentityReservation {
	if registry == nil {
		return orderDispatchIdentityReservation{}
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if registry.generation == "" {
		registry.markUnavailableLocked("controller_execution_generation_unavailable")
		return orderDispatchIdentityReservation{}
	}
	if !safeDispatchIdentityValue(scopedOrder) {
		registry.markUnavailableLocked("canonical_order_identity_unavailable")
		return orderDispatchIdentityReservation{}
	}
	registry.nextToken++
	token := registry.nextToken
	registry.pendingStarts[token] = scopedOrder
	registry.version++
	return orderDispatchIdentityReservation{
		registry: registry, scopedOrder: scopedOrder,
		generation: registry.generation, token: token, reserved: true,
	}
}

func (registry *orderDispatchIdentityRegistry) promote(reservation orderDispatchIdentityReservation, runID, workKind string) orderDispatchIdentityLease {
	if registry == nil || !reservation.reserved {
		return orderDispatchIdentityLease{}
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if reservation.generation != registry.generation {
		registry.markUnavailableLocked("controller_execution_generation_mismatch")
		return orderDispatchIdentityLease{}
	}
	if scopedOrder, exists := registry.pendingStarts[reservation.token]; !exists || scopedOrder != reservation.scopedOrder {
		registry.markUnavailableLocked("dispatch_identity_reservation_mismatch")
		return orderDispatchIdentityLease{}
	}
	delete(registry.pendingStarts, reservation.token)
	if !safeDispatchIdentityValue(runID) {
		registry.markUnavailableLocked("canonical_run_identity_unavailable")
		registry.version++
		return orderDispatchIdentityLease{}
	}
	if workKind != "formula_root" && workKind != "exec" {
		registry.markUnavailableLocked("dispatch_work_kind_unavailable")
		registry.version++
		return orderDispatchIdentityLease{}
	}
	key := orderDispatchIdentityKey{scopedOrder: reservation.scopedOrder, runID: runID}
	if _, exists := registry.entries[key]; exists {
		registry.markUnavailableLocked("duplicate_dispatch_identity")
		registry.version++
		return orderDispatchIdentityLease{}
	}

	workAvailability := unavailableSelectorObservation("canonical_work_identity_pending")
	if workKind == "exec" {
		workAvailability = unavailableSelectorObservation("exec_has_no_canonical_work_identity")
	}
	identity := OrderDispatchIdentity{
		ScopedOrder:              reservation.scopedOrder,
		RunID:                    runID,
		WorkKind:                 workKind,
		WorkIdentityAvailability: workAvailability,
		ExecutionGeneration:      registry.generation,
	}
	registry.entries[key] = orderDispatchIdentityEntry{identity: identity, token: reservation.token}
	registry.version++
	return orderDispatchIdentityLease{
		registry: registry, key: key, generation: registry.generation,
		token: reservation.token, registered: true,
	}
}

func (registry *orderDispatchIdentityRegistry) cancelReservation(reservation orderDispatchIdentityReservation) {
	if registry == nil || !reservation.reserved {
		return
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if reservation.generation != registry.generation {
		registry.markUnavailableLocked("controller_execution_generation_mismatch")
		return
	}
	if scopedOrder, exists := registry.pendingStarts[reservation.token]; !exists || scopedOrder != reservation.scopedOrder {
		registry.markUnavailableLocked("dispatch_identity_reservation_mismatch")
		return
	}
	delete(registry.pendingStarts, reservation.token)
	registry.version++
}

func (registry *orderDispatchIdentityRegistry) bindWork(lease orderDispatchIdentityLease, workID string) {
	if registry == nil || !lease.registered {
		return
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if lease.generation != registry.generation {
		registry.markUnavailableLocked("controller_execution_generation_mismatch")
		return
	}
	entry, exists := registry.entries[lease.key]
	if !exists || entry.token != lease.token {
		registry.markUnavailableLocked("dispatch_identity_lease_mismatch")
		return
	}
	if entry.identity.WorkKind != "formula_root" || !safeDispatchIdentityValue(workID) {
		entry.identity.WorkIdentityAvailability = unavailableSelectorObservation("canonical_work_identity_unavailable")
		registry.entries[lease.key] = entry
		registry.version++
		return
	}
	for key, other := range registry.entries {
		if key != lease.key && other.identity.WorkID == workID {
			registry.markUnavailableLocked("duplicate_work_identity")
			entry.identity.WorkIdentityAvailability = unavailableSelectorObservation("duplicate_work_identity")
			registry.entries[lease.key] = entry
			registry.version++
			return
		}
	}
	if entry.identity.WorkID != "" && entry.identity.WorkID != workID {
		entry.identity.WorkIdentityAvailability = unavailableSelectorObservation("conflicting_work_identity")
		registry.entries[lease.key] = entry
		registry.markUnavailableLocked("conflicting_work_identity")
		registry.version++
		return
	}
	entry.identity.WorkID = workID
	entry.identity.WorkIdentityAvailability = availableSelectorObservation()
	registry.entries[lease.key] = entry
	registry.version++
}

func (registry *orderDispatchIdentityRegistry) complete(lease orderDispatchIdentityLease) {
	if registry == nil || !lease.registered {
		return
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if lease.generation != registry.generation {
		registry.markUnavailableLocked("controller_execution_generation_mismatch")
		return
	}
	entry, exists := registry.entries[lease.key]
	if !exists || entry.token != lease.token {
		registry.markUnavailableLocked("dispatch_identity_lease_mismatch")
		return
	}
	delete(registry.entries, lease.key)
	registry.version++
}

func (registry *orderDispatchIdentityRegistry) markUnavailableLocked(reason string) {
	if registry.unavailableReason == "" {
		registry.unavailableReason = reason
		registry.version++
	}
}

func (lease orderDispatchIdentityLease) bindWork(workID string) {
	if lease.registry != nil {
		lease.registry.bindWork(lease, workID)
	}
}

func (lease orderDispatchIdentityLease) complete() {
	if lease.registry != nil {
		lease.registry.complete(lease)
	}
}

func (reservation orderDispatchIdentityReservation) promote(runID, workKind string) orderDispatchIdentityLease {
	if reservation.registry == nil {
		return orderDispatchIdentityLease{}
	}
	return reservation.registry.promote(reservation, runID, workKind)
}

func (reservation orderDispatchIdentityReservation) cancel() {
	if reservation.registry != nil {
		reservation.registry.cancelReservation(reservation)
	}
}

// SnapshotForGeneration returns a deterministic, read-only identity snapshot.
// expectedGeneration may be empty when the caller has no independently pinned
// controller execution generation; a non-empty mismatch fails closed.
func (registry *orderDispatchIdentityRegistry) SnapshotForGeneration(expectedGeneration string) OrderDispatchIdentitySnapshot {
	return registry.snapshotForGeneration(expectedGeneration, nil)
}

func (registry *orderDispatchIdentityRegistry) snapshotForGeneration(expectedGeneration string, afterCopy func()) OrderDispatchIdentitySnapshot {
	snapshot := OrderDispatchIdentitySnapshot{
		Availability: unavailableSelectorObservation("controller_identity_registry_unavailable"),
		Identities:   []OrderDispatchIdentity{},
	}
	if registry == nil {
		return snapshot
	}

	registry.mu.RLock()
	start := registry.version
	generation := registry.generation
	reason := registry.unavailableReason
	pendingStarts := len(registry.pendingStarts)
	identities := make([]OrderDispatchIdentity, 0, len(registry.entries))
	for _, entry := range registry.entries {
		identities = append(identities, entry.identity)
	}
	registry.mu.RUnlock()
	snapshot.ExecutionGeneration = generation
	snapshot.StartFence = start
	snapshot.Identities = identities
	if afterCopy != nil {
		afterCopy()
	}

	registry.mu.RLock()
	end := registry.version
	currentGeneration := registry.generation
	if reason == "" {
		reason = registry.unavailableReason
	}
	registry.mu.RUnlock()
	snapshot.EndFence = end

	sort.Slice(snapshot.Identities, func(i, j int) bool {
		left, right := snapshot.Identities[i], snapshot.Identities[j]
		if left.ScopedOrder != right.ScopedOrder {
			return left.ScopedOrder < right.ScopedOrder
		}
		if left.RunID != right.RunID {
			return left.RunID < right.RunID
		}
		return left.WorkID < right.WorkID
	})

	switch {
	case generation == "" || currentGeneration == "":
		snapshot.Availability = unavailableSelectorObservation("controller_execution_generation_unavailable")
	case expectedGeneration != "" && expectedGeneration != generation:
		snapshot.Availability = unavailableSelectorObservation("controller_execution_generation_mismatch")
	case generation != currentGeneration:
		snapshot.Availability = unavailableSelectorObservation("controller_execution_generation_changed")
	case start != end:
		snapshot.Availability = unavailableSelectorObservation("in_flight_identity_snapshot_raced")
	case reason != "":
		snapshot.Availability = unavailableSelectorObservation(reason)
	case pendingStarts > 0:
		snapshot.Availability = unavailableSelectorObservation("dispatch_identity_start_pending")
	default:
		for _, identity := range snapshot.Identities {
			if issue := selectorDispatchIdentityIssue(identity, generation); issue != "" {
				if issue == "formula_root_identity_unavailable" {
					issue = "exact_work_identity_unavailable"
				}
				snapshot.Availability = unavailableSelectorObservation(issue)
				return snapshot
			}
		}
		snapshot.Availability = availableSelectorObservation()
	}
	return snapshot
}

// InFlightDispatchIdentitySnapshot returns the dispatcher registry's exact
// identities without starting work or changing dispatcher state.
func (m *memoryOrderDispatcher) InFlightDispatchIdentitySnapshot(expectedGeneration string) OrderDispatchIdentitySnapshot {
	if m == nil || m.inflightIdentityRegistry == nil {
		return OrderDispatchIdentitySnapshot{
			Availability: unavailableSelectorObservation("controller_identity_registry_unavailable"),
			Identities:   []OrderDispatchIdentity{},
		}
	}
	return m.inflightIdentityRegistry.SnapshotForGeneration(expectedGeneration)
}

// InFlightDispatchIdentitySnapshot returns the controller's shared in-flight
// dispatch identities for a future read-only selector collector.
func (cs *controllerState) InFlightDispatchIdentitySnapshot(expectedGeneration string) OrderDispatchIdentitySnapshot {
	if cs == nil {
		return OrderDispatchIdentitySnapshot{
			Availability: unavailableSelectorObservation("controller_state_unavailable"),
			Identities:   []OrderDispatchIdentity{},
		}
	}
	cs.mu.RLock()
	registry := cs.orderDispatchIdentityRegistry
	cs.mu.RUnlock()
	if registry == nil {
		return OrderDispatchIdentitySnapshot{
			Availability: unavailableSelectorObservation("controller_identity_registry_unavailable"),
			Identities:   []OrderDispatchIdentity{},
		}
	}
	return registry.SnapshotForGeneration(expectedGeneration)
}
