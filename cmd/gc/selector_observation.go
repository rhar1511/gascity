package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/orderdiscovery"
	"github.com/gastownhall/gascity/internal/orders"
	"github.com/gastownhall/gascity/internal/qualification"
)

// SelectorObservationSchemaVersion identifies the read-only selector
// observation contract. This snapshot is evidence input only: it neither
// authorizes nor starts a trial, changes an order, or retires a selector.
const SelectorObservationSchemaVersion = 2

const (
	selectorOrderFound     = "found"
	selectorOrderMissing   = "missing"
	selectorOrderAmbiguous = "ambiguous"
)

// SelectorObservationAvailability makes every unsupported or incomplete
// observation explicit. Consumers must never infer availability from a zero
// value elsewhere in the snapshot.
type SelectorObservationAvailability struct {
	Status string `json:"status"`
	Reason string `json:"reason,omitempty"`
}

// SelectorObservationIssue preserves a discovery or validation failure in a
// stable, structured form. Multiple recoverable failures are retained rather
// than collapsed into the first error.
type SelectorObservationIssue struct {
	Stage            string `json:"stage"`
	ScopedOrder      string `json:"scoped_order,omitempty"`
	Rig              string `json:"rig,omitempty"`
	SourcePathSHA256 string `json:"source_path_sha256,omitempty"`
	ErrorSHA256      string `json:"error_sha256"`
	Message          string `json:"-"`
}

// SelectorObservedOrder is the effective winning definition for one exact
// caller-requested scoped order identity. Missing is an available result only
// when the complete scan succeeded. Ambiguity fails closed.
type SelectorObservedOrder struct {
	ScopedName              string                          `json:"scoped_name"`
	Resolution              string                          `json:"resolution"`
	Availability            SelectorObservationAvailability `json:"availability"`
	Enabled                 *bool                           `json:"enabled"`
	ConfiguredEnabled       *bool                           `json:"configured_enabled"`
	Activation              orders.ActivationDisposition    `json:"activation,omitempty"`
	SkipMatches             []string                        `json:"skip_matches"`
	WinningSourcePathSHA256 string                          `json:"winning_source_path_sha256,omitempty"`
	RawSourceSHA256         string                          `json:"raw_source_sha256,omitempty"`
	Formula                 string                          `json:"formula,omitempty"`
	FormulaLayerPathSHA256  string                          `json:"formula_layer_path_sha256,omitempty"`
	Rig                     string                          `json:"rig,omitempty"`
	ConfiguredScope         string                          `json:"configured_scope,omitempty"`
	Trigger                 string                          `json:"trigger,omitempty"`
	DispatchKind            string                          `json:"dispatch_kind,omitempty"`
}

// SelectorOrderInventory binds the requested scope to one discovery pass.
// InventorySHA256 covers every winning definition and its activation
// disposition. EffectiveOrderSetSHA256 covers only definitions in the active
// set. Both bind parsed behavior, source provenance digests, and exact source
// byte digests.
type SelectorOrderInventory struct {
	Availability            SelectorObservationAvailability `json:"availability"`
	RequestedScopedOrders   []string                        `json:"requested_scoped_orders"`
	DiscoveredOrderCount    int                             `json:"discovered_order_count"`
	EffectiveOrderCount     int                             `json:"effective_order_count"`
	InventorySHA256         string                          `json:"inventory_sha256,omitempty"`
	EffectiveOrderSetSHA256 string                          `json:"effective_order_set_sha256,omitempty"`
	Orders                  []SelectorObservedOrder         `json:"orders"`
	Issues                  []SelectorObservationIssue      `json:"issues"`
}

// SelectorEventWatermark is one read from the controller's existing event
// provider. A watermark bounds this observation; it does not establish that
// the retained event stream is complete.
type SelectorEventWatermark struct {
	Availability SelectorObservationAvailability `json:"availability"`
	Sequence     uint64                          `json:"sequence"`
}

// SelectorEventWindow records watermarks immediately before and after order
// discovery.
type SelectorEventWindow struct {
	Start SelectorEventWatermark `json:"start"`
	End   SelectorEventWatermark `json:"end"`
}

// SelectorReferenceInventory holds references supplied by an authoritative
// external collector.
type SelectorReferenceInventory struct {
	Availability SelectorObservationAvailability `json:"availability"`
	References   []string                        `json:"references"`
}

// SelectorObservedDispatch is one exact in-flight order identity joined to
// the effective selector inventory. Exec dispatches intentionally have no
// WorkID because they do not create formula roots.
type SelectorObservedDispatch struct {
	ScopedOrder         string `json:"scoped_order"`
	RunID               string `json:"run_id"`
	WorkKind            string `json:"work_kind"`
	WorkID              string `json:"work_id,omitempty"`
	ExecutionGeneration string `json:"execution_generation"`
}

// SelectorInFlightDispatchInventory records the fenced controller dispatch
// identities joined to the requested effective selector orders.
type SelectorInFlightDispatchInventory struct {
	Availability        SelectorObservationAvailability `json:"availability"`
	ExecutionGeneration string                          `json:"execution_generation,omitempty"`
	StartFence          uint64                          `json:"start_fence"`
	EndFence            uint64                          `json:"end_fence"`
	Dispatches          []SelectorObservedDispatch      `json:"dispatches"`
}

// SelectorObservationSnapshot is one generation-fenced, read-only input to a
// later selector-retirement evidence bundle. Status remains unavailable while
// sequence completeness or required writer/reference inventories are absent.
type SelectorObservationSnapshot struct {
	SchemaVersion        int                               `json:"schema_version"`
	Status               string                            `json:"status"`
	Reasons              []string                          `json:"reasons"`
	CanonicalSHA256      string                            `json:"canonical_sha256,omitempty"`
	Qualification        qualification.Snapshot            `json:"qualification"`
	ControllerBuild      qualification.BuildIdentity       `json:"controller_build"`
	ReleaseAuthorization qualification.Authorization       `json:"release_authorization"`
	ControllerGeneration uint64                            `json:"controller_generation"`
	GenerationFence      SelectorObservationAvailability   `json:"generation_fence"`
	Orders               SelectorOrderInventory            `json:"orders"`
	EventWatermarks      SelectorEventWindow               `json:"event_watermarks"`
	InternalInFlight     SelectorInFlightDispatchInventory `json:"internal_in_flight"`
	ExternalWriters      SelectorReferenceInventory        `json:"external_writers"`
	SequenceCompleteness SelectorObservationAvailability   `json:"sequence_completeness"`
}

// SelectorObservationSnapshot reads an evidence snapshot without publishing a
// route or changing controller state. requestedScopedOrders is supplied by the
// caller so generic controller code never embeds pack-specific order names.
func (cs *controllerState) SelectorObservationSnapshot(requestedScopedOrders []string) SelectorObservationSnapshot {
	return cs.selectorObservationSnapshotFS(fsys.OSFS{}, requestedScopedOrders)
}

func (cs *controllerState) selectorObservationSnapshotFS(fs fsys.FS, requestedScopedOrders []string) SelectorObservationSnapshot {
	if fs == nil {
		fs = fsys.OSFS{}
	}
	snapshot := SelectorObservationSnapshot{
		SchemaVersion: SelectorObservationSchemaVersion,
		Status:        qualification.StatusUnavailable,
		InternalInFlight: SelectorInFlightDispatchInventory{
			Availability: unavailableSelectorObservation("controller_identity_registry_unavailable"),
			Dispatches:   []SelectorObservedDispatch{},
		},
		ExternalWriters: SelectorReferenceInventory{
			Availability: unavailableSelectorObservation("external_writer_collector_unavailable"),
			References:   []string{},
		},
		SequenceCompleteness: unavailableSelectorObservation("sequence_completeness_collector_unavailable"),
	}

	if cs == nil {
		snapshot.GenerationFence = unavailableSelectorObservation("controller_state_unavailable")
		snapshot.Orders = unavailableSelectorOrderInventory(requestedScopedOrders, "controller_state_unavailable")
		snapshot.EventWatermarks = unavailableSelectorEventWindow("event_provider_unavailable")
		snapshot.Reasons = selectorObservationReasons(snapshot)
		sealSelectorObservationSnapshot(&snapshot)
		return snapshot
	}

	cs.mu.RLock()
	cfg := cs.cfg
	build := cs.qualificationBuild
	authorizer := cs.releaseAuthorizer
	generation := cs.graphStoreGeneration
	identityRegistry := cs.orderDispatchIdentityRegistry
	eventProvider := cs.eventProv
	cityPath := cs.cityPath
	cs.mu.RUnlock()

	snapshot.ControllerGeneration = generation
	snapshot.Qualification = cfg.QualificationSnapshot()
	snapshot.ControllerBuild = build
	snapshot.ReleaseAuthorization, _ = qualification.Authorize(context.Background(), authorizer, snapshot.Qualification, build)
	identityStart := selectorDispatchIdentitySnapshot(identityRegistry, "")
	snapshot.EventWatermarks.Start = selectorEventWatermark(eventProvider)
	snapshot.Orders = scanSelectorOrderInventoryFS(fs, cityPath, cfg, requestedScopedOrders)
	snapshot.EventWatermarks.End = selectorEventWatermark(eventProvider)
	identityEnd := selectorDispatchIdentitySnapshot(identityRegistry, identityStart.ExecutionGeneration)
	snapshot.InternalInFlight = joinSelectorInFlightDispatches(identityStart, identityEnd, snapshot.Orders)

	cs.mu.RLock()
	sameGeneration := generation != 0 && cs.graphStoreGeneration == generation && cs.cfg == cfg
	cs.mu.RUnlock()
	switch {
	case sameGeneration:
		snapshot.GenerationFence = availableSelectorObservation()
	case generation == 0:
		snapshot.GenerationFence = unavailableSelectorObservation("controller_generation_unavailable")
	default:
		snapshot.GenerationFence = unavailableSelectorObservation("controller_generation_changed")
	}

	snapshot.Reasons = selectorObservationReasons(snapshot)
	sealSelectorObservationSnapshot(&snapshot)
	return snapshot
}

func selectorDispatchIdentitySnapshot(registry *orderDispatchIdentityRegistry, expectedGeneration string) OrderDispatchIdentitySnapshot {
	if registry == nil {
		return OrderDispatchIdentitySnapshot{
			Availability: unavailableSelectorObservation("controller_identity_registry_unavailable"),
			Identities:   []OrderDispatchIdentity{},
		}
	}
	return registry.SnapshotForGeneration(expectedGeneration)
}

func joinSelectorInFlightDispatches(before, after OrderDispatchIdentitySnapshot, orders SelectorOrderInventory) SelectorInFlightDispatchInventory {
	unavailable := func(reason string) SelectorInFlightDispatchInventory {
		if reason == "" {
			reason = "in_flight_dispatch_inventory_unavailable"
		}
		return SelectorInFlightDispatchInventory{
			Availability: unavailableSelectorObservation(reason),
			Dispatches:   []SelectorObservedDispatch{},
		}
	}
	if before.ExecutionGeneration == "" {
		return unavailable("controller_execution_generation_unavailable")
	}
	_, availability := selectorRegistrySnapshotInput(before, before.ExecutionGeneration)
	if availability.Status != qualification.StatusAvailable {
		return unavailable(availability.Reason)
	}
	if issue := selectorDispatchSnapshotFenceChange(before, after, before.ExecutionGeneration); issue != "" {
		return unavailable(issue)
	}
	if orders.Availability.Status != qualification.StatusAvailable {
		return unavailable(reasonOrSelector(orders.Availability.Reason, "effective_order_inventory_unavailable"))
	}

	ordersByName := make(map[string]SelectorObservedOrder, len(orders.Orders))
	for _, order := range orders.Orders {
		if _, exists := ordersByName[order.ScopedName]; exists {
			return unavailable("effective_order_ambiguous")
		}
		ordersByName[order.ScopedName] = order
	}
	joined := make([]SelectorObservedDispatch, 0, len(before.Identities))
	joinedByOrder := make(map[string]struct{}, len(before.Identities))
	for _, identity := range before.Identities {
		order, requested := ordersByName[identity.ScopedOrder]
		if !requested {
			continue
		}
		if _, exists := joinedByOrder[identity.ScopedOrder]; exists {
			return unavailable("in_flight_selector_dispatch_ambiguous")
		}
		if order.Resolution != selectorOrderFound || order.Availability.Status != qualification.StatusAvailable ||
			order.Enabled == nil || !*order.Enabled {
			return unavailable("in_flight_selector_order_unresolved")
		}
		wantDispatchKind := "formula"
		if identity.WorkKind == "exec" {
			wantDispatchKind = "exec"
		}
		if order.DispatchKind != wantDispatchKind {
			return unavailable("in_flight_selector_dispatch_conflict")
		}
		joinedByOrder[identity.ScopedOrder] = struct{}{}
		joined = append(joined, SelectorObservedDispatch{
			ScopedOrder: identity.ScopedOrder, RunID: identity.RunID,
			WorkKind: identity.WorkKind, WorkID: identity.WorkID,
			ExecutionGeneration: identity.ExecutionGeneration,
		})
	}
	sort.Slice(joined, func(i, j int) bool {
		if joined[i].ScopedOrder != joined[j].ScopedOrder {
			return joined[i].ScopedOrder < joined[j].ScopedOrder
		}
		return joined[i].RunID < joined[j].RunID
	})
	return SelectorInFlightDispatchInventory{
		Availability: availableSelectorObservation(), ExecutionGeneration: before.ExecutionGeneration,
		StartFence: before.StartFence, EndFence: before.EndFence, Dispatches: joined,
	}
}

func selectorEventWatermark(provider interface{ LatestSeq() (uint64, error) }) SelectorEventWatermark {
	if provider == nil {
		return SelectorEventWatermark{Availability: unavailableSelectorObservation("event_provider_unavailable")}
	}
	seq, err := provider.LatestSeq()
	if err != nil {
		return SelectorEventWatermark{Availability: unavailableSelectorObservation("event_watermark_read_failed")}
	}
	return SelectorEventWatermark{Availability: availableSelectorObservation(), Sequence: seq}
}

func unavailableSelectorEventWindow(reason string) SelectorEventWindow {
	return SelectorEventWindow{
		Start: SelectorEventWatermark{Availability: unavailableSelectorObservation(reason)},
		End:   SelectorEventWatermark{Availability: unavailableSelectorObservation(reason)},
	}
}

func scanSelectorOrderInventoryFS(fs fsys.FS, cityPath string, cfg *config.City, requestedScopedOrders []string) SelectorOrderInventory {
	if fs == nil {
		fs = fsys.OSFS{}
	}
	requested, scopeIssues := canonicalSelectorObservationScope(requestedScopedOrders)
	inventory := SelectorOrderInventory{
		Availability:          availableSelectorObservation(),
		RequestedScopedOrders: requested,
		Orders:                make([]SelectorObservedOrder, 0, len(requested)),
		Issues:                append([]SelectorObservationIssue(nil), scopeIssues...),
	}
	if cfg == nil {
		cfg = &config.City{}
	}
	recordingFS := newSelectorObservationFS(fs)
	allOrders, err := orderdiscovery.ScanAllInventory(cityPath, cfg, orderdiscovery.ScanOptions{
		FS: recordingFS,
		OnRigScanError: func(rigName string, err error) error {
			inventory.Issues = append(inventory.Issues, SelectorObservationIssue{
				Stage: "rig_scan", Rig: rigName, Message: err.Error(),
			})
			return nil
		},
		OnOverrideError: func(err error) error {
			inventory.Issues = append(inventory.Issues, SelectorObservationIssue{
				Stage: "override_validation", Message: err.Error(),
			})
			return nil
		},
		OnValidateError: func(orderName string, err error) error {
			inventory.Issues = append(inventory.Issues, SelectorObservationIssue{
				Stage: "order_validation", ScopedOrder: orderName, Message: err.Error(),
			})
			return nil
		},
		ValidateOrder: validateOrderExecEnvOverrides,
	})
	if err != nil {
		inventory.Issues = append(inventory.Issues, SelectorObservationIssue{
			Stage: "order_scan", Message: err.Error(),
		})
	}
	for path, message := range recordingFS.readFailures {
		inventory.Issues = append(inventory.Issues, SelectorObservationIssue{
			Stage: "source_read", SourcePathSHA256: selectorObservationPathDigest(path), Message: message,
		})
	}
	for _, failure := range recordingFS.verify() {
		inventory.Issues = append(inventory.Issues, SelectorObservationIssue{
			Stage: failure.stage, SourcePathSHA256: selectorObservationPathDigest(failure.path), Message: failure.message,
		})
	}

	normalized := make([]selectorEffectiveOrder, 0, len(allOrders))
	byScopedName := make(map[string][]selectorEffectiveOrder, len(allOrders))
	for _, entry := range allOrders {
		order := entry.Order
		rawDigest, ok, conflicted := recordingFS.digest(order.Source)
		if !ok || conflicted {
			reason := "winning source bytes were not captured"
			if conflicted {
				reason = "winning source changed during discovery"
			}
			inventory.Issues = append(inventory.Issues, SelectorObservationIssue{
				Stage: "source_digest", ScopedOrder: order.ScopedName(),
				SourcePathSHA256: selectorObservationPathDigest(order.Source), Message: reason,
			})
		}
		effective := newSelectorEffectiveOrder(entry, rawDigest)
		normalized = append(normalized, effective)
		byScopedName[effective.ScopedName] = append(byScopedName[effective.ScopedName], effective)
	}
	sort.Slice(normalized, func(i, j int) bool {
		if normalized[i].ScopedName != normalized[j].ScopedName {
			return normalized[i].ScopedName < normalized[j].ScopedName
		}
		return normalized[i].SourcePathSHA256 < normalized[j].SourcePathSHA256
	})
	inventory.DiscoveredOrderCount = len(normalized)
	active := make([]selectorEffectiveOrder, 0, len(normalized))
	for _, order := range normalized {
		if order.Activation == orders.ActivationEnabled {
			active = append(active, order)
		}
	}
	inventory.EffectiveOrderCount = len(active)

	if len(inventory.Issues) == 0 {
		digest, digestErr := qualification.DigestJSON(normalized)
		if digestErr != nil {
			inventory.Issues = append(inventory.Issues, SelectorObservationIssue{
				Stage: "inventory_digest", Message: digestErr.Error(),
			})
		} else {
			inventory.InventorySHA256 = digest
		}
		activeDigest, activeDigestErr := qualification.DigestJSON(active)
		if activeDigestErr != nil {
			inventory.Issues = append(inventory.Issues, SelectorObservationIssue{
				Stage: "active_inventory_digest", Message: activeDigestErr.Error(),
			})
		} else {
			inventory.EffectiveOrderSetSHA256 = activeDigest
		}
	}

	for _, scopedName := range requested {
		matches := byScopedName[scopedName]
		observed := SelectorObservedOrder{ScopedName: scopedName}
		switch len(matches) {
		case 0:
			observed.Resolution = selectorOrderMissing
			observed.Availability = availableSelectorObservation()
		case 1:
			observed = matches[0].observation()
		case 2:
			observed.Resolution = selectorOrderAmbiguous
			observed.Availability = unavailableSelectorObservation("effective_order_ambiguous")
			inventory.Issues = append(inventory.Issues, SelectorObservationIssue{
				Stage: "scope_resolution", ScopedOrder: scopedName,
				Message: fmt.Sprintf("%d effective orders share the requested scoped identity", len(matches)),
			})
		default:
			observed.Resolution = selectorOrderAmbiguous
			observed.Availability = unavailableSelectorObservation("effective_order_ambiguous")
			inventory.Issues = append(inventory.Issues, SelectorObservationIssue{
				Stage: "scope_resolution", ScopedOrder: scopedName,
				Message: fmt.Sprintf("%d effective orders share the requested scoped identity", len(matches)),
			})
		}
		inventory.Orders = append(inventory.Orders, observed)
	}

	if len(inventory.Issues) > 0 {
		inventory.Availability = unavailableSelectorObservation("effective_order_inventory_unavailable")
		inventory.InventorySHA256 = ""
		inventory.EffectiveOrderSetSHA256 = ""
		for i := range inventory.Orders {
			if inventory.Orders[i].Availability.Status == qualification.StatusAvailable {
				inventory.Orders[i].Availability = unavailableSelectorObservation("effective_order_inventory_unavailable")
			}
		}
	}
	sortSelectorObservationIssues(inventory.Issues)
	return inventory
}

func unavailableSelectorOrderInventory(requested []string, reason string) SelectorOrderInventory {
	canonical, issues := canonicalSelectorObservationScope(requested)
	inventory := SelectorOrderInventory{
		Availability:          unavailableSelectorObservation(reason),
		RequestedScopedOrders: canonical,
		Orders:                make([]SelectorObservedOrder, 0, len(canonical)),
		Issues:                issues,
	}
	for _, scopedName := range canonical {
		inventory.Orders = append(inventory.Orders, SelectorObservedOrder{
			ScopedName:   scopedName,
			Availability: unavailableSelectorObservation(reason),
		})
	}
	return inventory
}

func canonicalSelectorObservationScope(requested []string) ([]string, []SelectorObservationIssue) {
	seen := make(map[string]struct{}, len(requested))
	canonical := make([]string, 0, len(requested))
	var issues []SelectorObservationIssue
	if len(requested) == 0 {
		issues = append(issues, SelectorObservationIssue{
			Stage: "scope_validation", Message: "at least one scoped order identity is required",
		})
	}
	for _, scopedName := range requested {
		if _, exists := seen[scopedName]; exists {
			continue
		}
		seen[scopedName] = struct{}{}
		if reason := validateSelectorScopedName(scopedName); reason != "" {
			issues = append(issues, SelectorObservationIssue{
				Stage: "scope_validation", ScopedOrder: scopedName, Message: reason,
			})
			continue
		}
		canonical = append(canonical, scopedName)
	}
	sort.Strings(canonical)
	sortSelectorObservationIssues(issues)
	return canonical, issues
}

func validateSelectorScopedName(scopedName string) string {
	if strings.TrimSpace(scopedName) == "" {
		return "scoped order identity is required"
	}
	if scopedName != strings.TrimSpace(scopedName) {
		return "scoped order identity has leading or trailing whitespace"
	}
	if !utf8.ValidString(scopedName) {
		return "scoped order identity is not valid UTF-8"
	}
	for _, r := range scopedName {
		if unicode.IsControl(r) {
			return "scoped order identity contains a control character"
		}
	}
	return ""
}

type selectorEffectiveOrder struct {
	Name                   string                       `json:"name"`
	ScopedName             string                       `json:"scoped_name"`
	Activation             orders.ActivationDisposition `json:"activation"`
	SkipMatches            []string                     `json:"skip_matches"`
	Description            string                       `json:"description,omitempty"`
	Formula                string                       `json:"formula,omitempty"`
	Exec                   string                       `json:"exec,omitempty"`
	Scope                  string                       `json:"scope,omitempty"`
	Trigger                string                       `json:"trigger"`
	Interval               string                       `json:"interval,omitempty"`
	Schedule               string                       `json:"schedule,omitempty"`
	Timezone               string                       `json:"timezone,omitempty"`
	Check                  string                       `json:"check,omitempty"`
	On                     string                       `json:"on,omitempty"`
	Pool                   string                       `json:"pool,omitempty"`
	Timeout                string                       `json:"timeout,omitempty"`
	CheckTimeout           string                       `json:"check_timeout,omitempty"`
	ConfiguredEnabled      bool                         `json:"configured_enabled"`
	EffectiveEnabled       bool                         `json:"enabled"`
	Idempotent             bool                         `json:"idempotent"`
	NoWorkGate             bool                         `json:"no_work_gate"`
	ReservedDispatch       bool                         `json:"reserved_dispatch"`
	Environment            map[string]string            `json:"environment,omitempty"`
	Params                 map[string]orders.OrderParam `json:"params,omitempty"`
	SourcePathSHA256       string                       `json:"source_path_sha256"`
	RawSourceSHA256        string                       `json:"raw_source_sha256"`
	FormulaLayerPathSHA256 string                       `json:"formula_layer_path_sha256"`
	Rig                    string                       `json:"rig,omitempty"`
}

func newSelectorEffectiveOrder(entry orders.InventoryOrder, rawDigest string) selectorEffectiveOrder {
	order := entry.Order
	return selectorEffectiveOrder{
		Name: order.Name, ScopedName: order.ScopedName(), Activation: entry.Activation,
		SkipMatches: append([]string(nil), entry.SkipMatches...), Description: order.Description,
		Formula: order.Formula, Exec: order.Exec, Scope: order.Scope, Trigger: order.Trigger,
		Interval: order.Interval, Schedule: order.Schedule, Timezone: order.TZ,
		Check: order.Check, On: order.On, Pool: order.Pool, Timeout: order.Timeout,
		CheckTimeout: order.CheckTimeout, ConfiguredEnabled: order.IsEnabled(),
		EffectiveEnabled: entry.Activation == orders.ActivationEnabled, Idempotent: order.Idempotent,
		NoWorkGate: order.NoWorkGate, ReservedDispatch: order.ReservedDispatch,
		Environment: order.Env, Params: order.Params,
		SourcePathSHA256: selectorObservationPathDigest(order.Source), RawSourceSHA256: rawDigest,
		FormulaLayerPathSHA256: selectorObservationPathDigest(order.FormulaLayer), Rig: order.Rig,
	}
}

func (order selectorEffectiveOrder) observation() SelectorObservedOrder {
	dispatchKind := "formula"
	if order.Exec != "" {
		dispatchKind = "exec"
	}
	enabled := order.EffectiveEnabled
	configuredEnabled := order.ConfiguredEnabled
	return SelectorObservedOrder{
		ScopedName: order.ScopedName, Resolution: selectorOrderFound,
		Availability: availableSelectorObservation(), Enabled: &enabled,
		ConfiguredEnabled: &configuredEnabled, Activation: order.Activation,
		SkipMatches:             append([]string(nil), order.SkipMatches...),
		WinningSourcePathSHA256: order.SourcePathSHA256, RawSourceSHA256: order.RawSourceSHA256,
		Formula: order.Formula, FormulaLayerPathSHA256: order.FormulaLayerPathSHA256, Rig: order.Rig,
		ConfiguredScope: order.Scope, Trigger: order.Trigger, DispatchKind: dispatchKind,
	}
}

type selectorObservationFS struct {
	fsys.FS
	digests             map[string]string
	conflicts           map[string]bool
	readFailures        map[string]string
	discoveredOrders    map[string]struct{}
	missingProbes       map[string]struct{}
	missingDirectories  map[string]struct{}
	directoryDigests    map[string]string
	directoryConflicts  map[string]bool
	directoryHashErrors map[string]string
}

func newSelectorObservationFS(fs fsys.FS) *selectorObservationFS {
	return &selectorObservationFS{
		FS: fs, digests: make(map[string]string), conflicts: make(map[string]bool),
		readFailures: make(map[string]string), discoveredOrders: make(map[string]struct{}),
		missingProbes: make(map[string]struct{}), missingDirectories: make(map[string]struct{}),
		directoryDigests:   make(map[string]string),
		directoryConflicts: make(map[string]bool), directoryHashErrors: make(map[string]string),
	}
}

func (fs *selectorObservationFS) ReadDir(path string) ([]os.DirEntry, error) {
	entries, err := fs.FS.ReadDir(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			fs.missingDirectories[filepath.Clean(path)] = struct{}{}
		}
		return entries, err
	}
	for _, entry := range entries {
		if !entry.IsDir() && orders.IsFlatOrderFilename(entry.Name()) {
			fs.discoveredOrders[filepath.Clean(filepath.Join(path, entry.Name()))] = struct{}{}
		}
	}
	cleanPath := filepath.Clean(path)
	digest, digestErr := selectorObservationDirectoryDigest(entries)
	if digestErr != nil {
		fs.directoryHashErrors[cleanPath] = digestErr.Error()
		return entries, nil
	}
	if previous, exists := fs.directoryDigests[cleanPath]; exists && previous != digest {
		fs.directoryConflicts[cleanPath] = true
	}
	fs.directoryDigests[cleanPath] = digest
	return entries, nil
}

func (fs *selectorObservationFS) ReadFile(path string) ([]byte, error) {
	data, err := fs.FS.ReadFile(path)
	if err != nil {
		cleanPath := filepath.Clean(path)
		_, discovered := fs.discoveredOrders[cleanPath]
		if orders.IsFlatOrderFilename(filepath.Base(path)) {
			if !errors.Is(err, os.ErrNotExist) || discovered {
				fs.readFailures[cleanPath] = err.Error()
			} else {
				fs.missingProbes[cleanPath] = struct{}{}
			}
		}
		return data, err
	}
	sum := sha256.Sum256(data)
	digest := hex.EncodeToString(sum[:])
	if previous, exists := fs.digests[path]; exists && previous != digest {
		fs.conflicts[path] = true
	}
	fs.digests[path] = digest
	return data, nil
}

type selectorObservationVerificationFailure struct {
	stage   string
	path    string
	message string
}

type selectorObservationDirectoryEntry struct {
	Name  string `json:"name"`
	IsDir bool   `json:"is_dir"`
	Type  string `json:"type"`
}

func selectorObservationDirectoryDigest(entries []os.DirEntry) (string, error) {
	relevant := make([]selectorObservationDirectoryEntry, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() && !orders.IsFlatOrderFilename(entry.Name()) {
			continue
		}
		relevant = append(relevant, selectorObservationDirectoryEntry{
			Name: entry.Name(), IsDir: entry.IsDir(), Type: entry.Type().String(),
		})
	}
	sort.Slice(relevant, func(i, j int) bool {
		if relevant[i].Name != relevant[j].Name {
			return relevant[i].Name < relevant[j].Name
		}
		if relevant[i].IsDir != relevant[j].IsDir {
			return !relevant[i].IsDir
		}
		return relevant[i].Type < relevant[j].Type
	})
	return qualification.DigestJSON(relevant)
}

func (fs *selectorObservationFS) verify() []selectorObservationVerificationFailure {
	var failures []selectorObservationVerificationFailure
	for path, message := range fs.directoryHashErrors {
		failures = append(failures, selectorObservationVerificationFailure{
			stage: "directory_fence", path: path, message: message,
		})
	}
	for path, expected := range fs.directoryDigests {
		entries, err := fs.FS.ReadDir(path)
		if err != nil {
			failures = append(failures, selectorObservationVerificationFailure{
				stage: "directory_fence", path: path, message: err.Error(),
			})
			continue
		}
		actual, digestErr := selectorObservationDirectoryDigest(entries)
		if digestErr != nil {
			failures = append(failures, selectorObservationVerificationFailure{
				stage: "directory_fence", path: path, message: digestErr.Error(),
			})
		} else if fs.directoryConflicts[path] || actual != expected {
			failures = append(failures, selectorObservationVerificationFailure{
				stage: "directory_fence", path: path, message: "order directory changed during discovery",
			})
		}
	}
	for path := range fs.missingDirectories {
		if _, err := fs.FS.ReadDir(path); err == nil {
			failures = append(failures, selectorObservationVerificationFailure{
				stage: "directory_fence", path: path, message: "previously absent order directory appeared during discovery",
			})
		} else if !errors.Is(err, os.ErrNotExist) {
			failures = append(failures, selectorObservationVerificationFailure{
				stage: "directory_fence", path: path, message: err.Error(),
			})
		}
	}
	for path, expected := range fs.digests {
		data, err := fs.FS.ReadFile(path)
		if err != nil {
			failures = append(failures, selectorObservationVerificationFailure{
				stage: "source_fence", path: path, message: err.Error(),
			})
			continue
		}
		sum := sha256.Sum256(data)
		if fs.conflicts[path] || hex.EncodeToString(sum[:]) != expected {
			failures = append(failures, selectorObservationVerificationFailure{
				stage: "source_fence", path: path, message: "order source changed during discovery",
			})
		}
	}
	for path := range fs.missingProbes {
		if _, err := fs.FS.ReadFile(path); err == nil {
			failures = append(failures, selectorObservationVerificationFailure{
				stage: "source_fence", path: path, message: "previously absent order source appeared during discovery",
			})
		} else if !errors.Is(err, os.ErrNotExist) {
			failures = append(failures, selectorObservationVerificationFailure{
				stage: "source_fence", path: path, message: err.Error(),
			})
		}
	}
	sort.Slice(failures, func(i, j int) bool {
		if failures[i].stage != failures[j].stage {
			return failures[i].stage < failures[j].stage
		}
		return failures[i].path < failures[j].path
	})
	return failures
}

func (fs *selectorObservationFS) digest(path string) (string, bool, bool) {
	digest, ok := fs.digests[path]
	return digest, ok, fs.conflicts[path]
}

func selectorObservationPathDigest(path string) string {
	if strings.TrimSpace(path) == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(filepath.Clean(path)))
	return hex.EncodeToString(sum[:])
}

func availableSelectorObservation() SelectorObservationAvailability {
	return SelectorObservationAvailability{Status: qualification.StatusAvailable}
}

func unavailableSelectorObservation(reason string) SelectorObservationAvailability {
	return SelectorObservationAvailability{Status: qualification.StatusUnavailable, Reason: reason}
}

func sortSelectorObservationIssues(issues []SelectorObservationIssue) {
	for i := range issues {
		if issues[i].ErrorSHA256 == "" {
			sum := sha256.Sum256([]byte(issues[i].Message))
			issues[i].ErrorSHA256 = hex.EncodeToString(sum[:])
		}
	}
	sort.Slice(issues, func(i, j int) bool {
		left := issues[i].Stage + "\x00" + issues[i].ScopedOrder + "\x00" + issues[i].Rig + "\x00" + issues[i].SourcePathSHA256 + "\x00" + issues[i].ErrorSHA256
		right := issues[j].Stage + "\x00" + issues[j].ScopedOrder + "\x00" + issues[j].Rig + "\x00" + issues[j].SourcePathSHA256 + "\x00" + issues[j].ErrorSHA256
		return left < right
	})
}

func selectorObservationReasons(snapshot SelectorObservationSnapshot) []string {
	set := make(map[string]struct{})
	add := func(availability SelectorObservationAvailability) {
		if availability.Status != qualification.StatusAvailable {
			reason := availability.Reason
			if reason == "" {
				reason = "availability_unspecified"
			}
			set[reason] = struct{}{}
		}
	}
	add(snapshot.GenerationFence)
	add(snapshot.Orders.Availability)
	add(snapshot.EventWatermarks.Start.Availability)
	add(snapshot.EventWatermarks.End.Availability)
	add(snapshot.InternalInFlight.Availability)
	add(snapshot.ExternalWriters.Availability)
	add(snapshot.SequenceCompleteness)
	if snapshot.Qualification.Status != qualification.StatusAvailable {
		set[reasonOrSelector(snapshot.Qualification.Reason, "qualification_snapshot_unavailable")] = struct{}{}
	}
	if snapshot.ControllerBuild.Status != qualification.StatusAvailable || snapshot.ControllerBuild.ArtifactStatus != qualification.StatusAvailable {
		set[reasonOrSelector(snapshot.ControllerBuild.Reason, "controller_build_unavailable")] = struct{}{}
	}
	if snapshot.ReleaseAuthorization.Status != qualification.StatusAuthorized {
		set[reasonOrSelector(snapshot.ReleaseAuthorization.Reason, "release_authorization_unavailable")] = struct{}{}
	}
	reasons := make([]string, 0, len(set))
	for reason := range set {
		reasons = append(reasons, reason)
	}
	sort.Strings(reasons)
	return reasons
}

func reasonOrSelector(reason, fallback string) string {
	if strings.TrimSpace(reason) != "" {
		return reason
	}
	return fallback
}

func sealSelectorObservationSnapshot(snapshot *SelectorObservationSnapshot) {
	if snapshot == nil {
		return
	}
	snapshot.CanonicalSHA256 = ""
	digest, err := qualification.DigestJSON(*snapshot)
	if err != nil {
		snapshot.Status = qualification.StatusUnavailable
		snapshot.Reasons = append(snapshot.Reasons, "canonical_digest_unavailable")
		sort.Strings(snapshot.Reasons)
		return
	}
	snapshot.CanonicalSHA256 = digest
}

func selectorObservationSnapshotDigest(snapshot SelectorObservationSnapshot) (string, error) {
	snapshot.CanonicalSHA256 = ""
	return qualification.DigestJSON(snapshot)
}
