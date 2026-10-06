package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/orders"
	"github.com/gastownhall/gascity/internal/qualification"
)

func TestSelectorObservationSnapshotBindsGenerationIdentityAndExactOrderBytes(t *testing.T) {
	cityPath := t.TempDir()
	t.Setenv("GC_HOME", filepath.Join(cityPath, "isolated-gc-home"))
	writeSelectorObservationFile(t, filepath.Join(cityPath, "city.toml"), "[workspace]\nname = \"selector-observation\"\n")
	orderSource := filepath.Join(cityPath, "orders", "candidate.toml")
	orderBytes := "[order]\nexec = \"true\"\nscope = \"city\"\ntrigger = \"cooldown\"\ninterval = \"5m\"\n"
	writeSelectorObservationFile(t, orderSource, orderBytes)

	cfg, provenance, err := config.LoadWithIncludesOptions(
		fsys.OSFS{}, filepath.Join(cityPath, "city.toml"),
		config.LoadOptions{CaptureQualificationInputs: true},
	)
	if err != nil {
		t.Fatalf("LoadWithIncludesOptions: %v", err)
	}
	qualificationSnapshot := config.RefreshQualificationSnapshot(cfg, provenance)
	if qualificationSnapshot.Status != qualification.StatusAvailable {
		t.Fatalf("qualification snapshot = %#v, want available", qualificationSnapshot)
	}

	build := selectorObservationTestBuildIdentity()
	eventProvider := events.NewFake()
	eventProvider.Record(events.Event{Type: events.BeadCreated, Subject: "work-1"})
	state := &controllerState{
		cfg: cfg, cityPath: cityPath, graphStoreGeneration: 7,
		qualificationBuild: build, releaseAuthorizer: selectorObservationTestAuthorizer{},
		eventProv: eventProvider,
	}

	got := state.SelectorObservationSnapshot([]string{"missing", "candidate", "candidate"})
	if got.SchemaVersion != SelectorObservationSchemaVersion || got.ControllerGeneration != 7 {
		t.Fatalf("snapshot identity = schema %d generation %d", got.SchemaVersion, got.ControllerGeneration)
	}
	if got.GenerationFence.Status != qualification.StatusAvailable {
		t.Fatalf("generation fence = %#v, want available", got.GenerationFence)
	}
	if got.Qualification.EffectiveConfigIdentitySHA256 != qualificationSnapshot.EffectiveConfigIdentitySHA256 {
		t.Fatalf("qualification identity = %q, want %q", got.Qualification.EffectiveConfigIdentitySHA256, qualificationSnapshot.EffectiveConfigIdentitySHA256)
	}
	if got.ControllerBuild.ArtifactSHA256 != build.ArtifactSHA256 || got.ReleaseAuthorization.Status != qualification.StatusAuthorized {
		t.Fatalf("build/release binding = %#v / %#v", got.ControllerBuild, got.ReleaseAuthorization)
	}
	if got.EventWatermarks.Start.Availability.Status != qualification.StatusAvailable || got.EventWatermarks.Start.Sequence != 1 || got.EventWatermarks.End.Sequence != 1 {
		t.Fatalf("event watermarks = %#v, want available 1..1", got.EventWatermarks)
	}
	if got.Orders.Availability.Status != qualification.StatusAvailable || got.Orders.DiscoveredOrderCount != 1 || got.Orders.EffectiveOrderCount != 1 || len(got.Orders.InventorySHA256) != 64 || len(got.Orders.EffectiveOrderSetSHA256) != 64 {
		t.Fatalf("order inventory = %#v, want one available effective order", got.Orders)
	}
	if strings.Join(got.Orders.RequestedScopedOrders, ",") != "candidate,missing" || len(got.Orders.Orders) != 2 {
		t.Fatalf("canonical requested scope = %#v / %#v", got.Orders.RequestedScopedOrders, got.Orders.Orders)
	}
	found := got.Orders.Orders[0]
	if found.ScopedName != "candidate" || found.Resolution != selectorOrderFound || found.Enabled == nil || !*found.Enabled || found.Activation != orders.ActivationEnabled || found.WinningSourcePathSHA256 != selectorObservationPathDigest(orderSource) || found.FormulaLayerPathSHA256 != selectorObservationPathDigest(filepath.Join(cityPath, "formulas")) || found.DispatchKind != "exec" {
		t.Fatalf("candidate observation = %#v", found)
	}
	rawSum := sha256.Sum256([]byte(orderBytes))
	wantRawDigest := hex.EncodeToString(rawSum[:])
	if found.RawSourceSHA256 != wantRawDigest {
		t.Fatalf("candidate raw source digest = %q, want exact byte digest %q", found.RawSourceSHA256, wantRawDigest)
	}
	missing := got.Orders.Orders[1]
	if missing.ScopedName != "missing" || missing.Resolution != selectorOrderMissing || missing.Availability.Status != qualification.StatusAvailable {
		t.Fatalf("missing observation = %#v", missing)
	}
	if got.InternalInFlight.Availability.Status != qualification.StatusUnavailable || got.ExternalWriters.Availability.Status != qualification.StatusUnavailable || got.SequenceCompleteness.Status != qualification.StatusUnavailable {
		t.Fatalf("unsupported collectors were not explicit: in-flight=%#v external=%#v sequence=%#v", got.InternalInFlight, got.ExternalWriters, got.SequenceCompleteness)
	}
	if got.Status != qualification.StatusUnavailable || len(got.CanonicalSHA256) != 64 {
		t.Fatalf("snapshot status/digest = %q/%q", got.Status, got.CanonicalSHA256)
	}
	wantSnapshotDigest, err := selectorObservationSnapshotDigest(got)
	if err != nil {
		t.Fatal(err)
	}
	if got.CanonicalSHA256 != wantSnapshotDigest {
		t.Fatalf("canonical digest = %q, want %q", got.CanonicalSHA256, wantSnapshotDigest)
	}
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), cityPath) || strings.Contains(string(encoded), orderSource) {
		t.Fatalf("serialized observation disclosed a host path: %s", encoded)
	}
}

func TestSelectorObservationSnapshotJoinsInFlightExecIdentity(t *testing.T) {
	cityPath := t.TempDir()
	writeSelectorObservationFile(t, filepath.Join(cityPath, "orders", "candidate.toml"), `[order]
exec = "true"
scope = "city"
trigger = "manual"
`)
	registry := newOrderDispatchIdentityRegistryWithGeneration("execution-generation-exec")
	lease := registry.begin("candidate", "run-exec-1", "exec")
	if !lease.registered {
		t.Fatal("exec dispatch identity did not register")
	}
	defer lease.complete()
	state := &controllerState{
		cfg: &config.City{}, cityPath: cityPath, graphStoreGeneration: 13,
		qualificationBuild:            selectorObservationTestBuildIdentity(),
		releaseAuthorizer:             selectorObservationTestAuthorizer{},
		orderDispatchIdentityRegistry: registry,
	}

	got := state.SelectorObservationSnapshot([]string{"candidate"})
	if got.InternalInFlight.Availability.Status != qualification.StatusAvailable {
		t.Fatalf("in-flight dispatch inventory = %#v, want available exact exec identity", got.InternalInFlight)
	}
	if got.InternalInFlight.ExecutionGeneration != "execution-generation-exec" ||
		got.InternalInFlight.StartFence != got.InternalInFlight.EndFence || len(got.InternalInFlight.Dispatches) != 1 {
		t.Fatalf("in-flight dispatch fence/identities = %#v, want one stable exec identity", got.InternalInFlight)
	}
	dispatch := got.InternalInFlight.Dispatches[0]
	if dispatch.ScopedOrder != "candidate" || dispatch.RunID != "run-exec-1" ||
		dispatch.WorkKind != "exec" || dispatch.WorkID != "" ||
		dispatch.ExecutionGeneration != "execution-generation-exec" {
		t.Fatalf("in-flight exec dispatch = %#v, want exact scoped-order/run/generation without a formula WorkID", dispatch)
	}
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), `"work_id"`) {
		t.Fatalf("exec dispatch serialized a formula WorkID field: %s", encoded)
	}
}

func TestSelectorObservationSnapshotPreservesInFlightFormulaIdentity(t *testing.T) {
	cityPath := t.TempDir()
	writeSelectorObservationFile(t, filepath.Join(cityPath, "orders", "candidate.toml"), `[order]
formula = "candidate-formula"
scope = "city"
trigger = "manual"
`)
	registry := newOrderDispatchIdentityRegistryWithGeneration("execution-generation-formula")
	lease := registry.begin("candidate", "run-formula-1", "formula_root")
	if !lease.registered {
		t.Fatal("formula dispatch identity did not register")
	}
	lease.bindWork("formula-root-1")
	defer lease.complete()
	state := &controllerState{
		cfg: &config.City{}, cityPath: cityPath, graphStoreGeneration: 14,
		qualificationBuild:            selectorObservationTestBuildIdentity(),
		releaseAuthorizer:             selectorObservationTestAuthorizer{},
		orderDispatchIdentityRegistry: registry,
	}

	got := state.SelectorObservationSnapshot([]string{"candidate"})
	if got.InternalInFlight.Availability.Status != qualification.StatusAvailable || len(got.InternalInFlight.Dispatches) != 1 {
		t.Fatalf("in-flight formula inventory = %#v, want one available formula identity", got.InternalInFlight)
	}
	dispatch := got.InternalInFlight.Dispatches[0]
	if dispatch.WorkKind != "formula_root" || dispatch.WorkID != "formula-root-1" || dispatch.RunID != "run-formula-1" {
		t.Fatalf("in-flight formula dispatch = %#v, want exact formula-root identity", dispatch)
	}
}

func TestJoinSelectorInFlightDispatchesFailsClosedOnBadEvidence(t *testing.T) {
	base := OrderDispatchIdentitySnapshot{
		Availability: availableSelectorObservation(), ExecutionGeneration: "execution-generation-join",
		StartFence: 7, EndFence: 7,
		Identities: []OrderDispatchIdentity{{
			ScopedOrder: "candidate", RunID: "run-1", WorkKind: "exec",
			WorkIdentityAvailability: unavailableSelectorObservation("exec_has_no_canonical_work_identity"),
			ExecutionGeneration:      "execution-generation-join",
		}},
	}
	orders := SelectorOrderInventory{
		Availability: availableSelectorObservation(),
		Orders: []SelectorObservedOrder{{
			ScopedName: "candidate", Resolution: selectorOrderFound, Availability: availableSelectorObservation(),
			Enabled: selectorObservationBool(true), DispatchKind: "exec",
		}},
	}
	malformed := selectorDispatchSnapshotWithIdentity(base, func(identity *OrderDispatchIdentity) {
		identity.WorkKind = "unknown"
	})
	stale := selectorDispatchSnapshotWithIdentity(base, func(identity *OrderDispatchIdentity) {
		identity.ExecutionGeneration = "execution-generation-old"
	})
	ambiguous := base
	ambiguous.Identities = append(append([]OrderDispatchIdentity(nil), base.Identities...), OrderDispatchIdentity{
		ScopedOrder: "candidate", RunID: "run-2", WorkKind: "exec",
		WorkIdentityAvailability: unavailableSelectorObservation("exec_has_no_canonical_work_identity"),
		ExecutionGeneration:      base.ExecutionGeneration,
	})
	conflictingOrders := SelectorOrderInventory{
		Availability: availableSelectorObservation(),
		Orders: []SelectorObservedOrder{{
			ScopedName: "candidate", Resolution: selectorOrderFound, Availability: availableSelectorObservation(),
			Enabled: selectorObservationBool(true), DispatchKind: "formula",
		}},
	}
	tests := []struct {
		name   string
		before OrderDispatchIdentitySnapshot
		after  OrderDispatchIdentitySnapshot
		orders SelectorOrderInventory
	}{
		{name: "incomplete", before: unavailableSelectorDispatchTestSnapshot("controller_identity_registry_unavailable"), after: unavailableSelectorDispatchTestSnapshot("controller_identity_registry_unavailable"), orders: orders},
		{name: "malformed kind", before: malformed, after: malformed, orders: orders},
		{name: "stale generation", before: stale, after: stale, orders: orders},
		{name: "ambiguous runs", before: ambiguous, after: ambiguous, orders: orders},
		{name: "dispatch conflict", before: base, after: base, orders: conflictingOrders},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := joinSelectorInFlightDispatches(test.before, test.after, test.orders)
			if got.Availability.Status != qualification.StatusUnavailable || len(got.Dispatches) != 0 {
				t.Fatalf("joined in-flight inventory = %#v, want unavailable with no identities", got)
			}
		})
	}
}

func selectorObservationBool(value bool) *bool {
	return &value
}

func selectorDispatchSnapshotWithIdentity(snapshot OrderDispatchIdentitySnapshot, mutate func(*OrderDispatchIdentity)) OrderDispatchIdentitySnapshot {
	snapshot.Identities = append([]OrderDispatchIdentity(nil), snapshot.Identities...)
	mutate(&snapshot.Identities[0])
	return snapshot
}

func unavailableSelectorDispatchTestSnapshot(reason string) OrderDispatchIdentitySnapshot {
	return OrderDispatchIdentitySnapshot{
		Availability: unavailableSelectorObservation(reason), Identities: []OrderDispatchIdentity{},
	}
}

func TestSelectorObservationInventoryPreservesEveryValidationError(t *testing.T) {
	cityPath := t.TempDir()
	writeSelectorObservationFile(t, filepath.Join(cityPath, "orders", "missing-action.toml"), "[order]\ntrigger = \"manual\"\n")
	writeSelectorObservationFile(t, filepath.Join(cityPath, "orders", "double-action.toml"), "[order]\nformula = \"one\"\nexec = \"true\"\ntrigger = \"manual\"\n")

	got := scanSelectorOrderInventoryFS(
		fsys.OSFS{}, cityPath, &config.City{},
		[]string{"missing-action", "double-action"},
	)
	if got.Availability.Status != qualification.StatusUnavailable || got.Availability.Reason != "effective_order_inventory_unavailable" {
		t.Fatalf("inventory availability = %#v", got.Availability)
	}
	var validationIssues []SelectorObservationIssue
	for _, issue := range got.Issues {
		if issue.Stage == "order_validation" {
			validationIssues = append(validationIssues, issue)
		}
	}
	if len(validationIssues) != 2 {
		t.Fatalf("validation issues = %#v, want both invalid orders", got.Issues)
	}
	if validationIssues[0].ScopedOrder != "double-action" || validationIssues[1].ScopedOrder != "missing-action" {
		t.Fatalf("validation issues not canonical or complete: %#v", validationIssues)
	}
	if got.InventorySHA256 != "" || got.EffectiveOrderSetSHA256 != "" {
		t.Fatalf("unavailable inventory published digests %q / %q", got.InventorySHA256, got.EffectiveOrderSetSHA256)
	}
	for _, order := range got.Orders {
		if order.Availability.Status != qualification.StatusUnavailable {
			t.Fatalf("requested order retained available status after validation failure: %#v", order)
		}
	}
}

func TestSelectorObservationInventoryDistinguishesEveryActivationDisposition(t *testing.T) {
	cityPath := t.TempDir()
	writeSelectorObservationFile(t, filepath.Join(cityPath, "orders", "source-disabled.toml"), `[order]
formula = "mol-source-disabled"
trigger = "manual"
enabled = false
`)
	writeSelectorObservationFile(t, filepath.Join(cityPath, "orders", "override-disabled.toml"), `[order]
formula = "mol-override-disabled"
trigger = "manual"
`)
	writeSelectorObservationFile(t, filepath.Join(cityPath, "orders", "skipped-name.toml"), `[order]
formula = "mol-skipped-name"
trigger = "manual"
`)
	writeSelectorObservationFile(t, filepath.Join(cityPath, "orders", "skipped-alias.toml"), `[order]
formula = "mol-skipped-alias"
trigger = "manual"
skip_aliases = ["legacy-selector"]
`)
	lowerPack := filepath.Join(cityPath, "packs", "lower")
	higherPack := filepath.Join(cityPath, "packs", "higher")
	writeSelectorObservationFile(t, filepath.Join(lowerPack, "orders", "winner.toml"), `[order]
formula = "mol-lower"
trigger = "manual"
`)
	higherSource := filepath.Join(higherPack, "orders", "winner.toml")
	writeSelectorObservationFile(t, higherSource, `[order]
formula = "mol-higher"
trigger = "manual"
`)
	disabled := false
	cfg := &config.City{
		PackDirs: []string{lowerPack, higherPack},
		Orders: config.OrdersConfig{
			Skip:      []string{"skipped-name", "legacy-selector"},
			Overrides: []config.OrderOverride{{Name: "override-disabled", Enabled: &disabled}},
		},
	}
	requested := []string{"winner", "source-disabled", "override-disabled", "skipped-name", "skipped-alias", "absent"}

	got := scanSelectorOrderInventoryFS(fsys.OSFS{}, cityPath, cfg, requested)
	if got.Availability.Status != qualification.StatusAvailable || got.DiscoveredOrderCount != 5 || got.EffectiveOrderCount != 1 {
		t.Fatalf("inventory = %#v, want five discovered definitions and one active winner", got)
	}
	if len(got.InventorySHA256) != 64 || len(got.EffectiveOrderSetSHA256) != 64 {
		t.Fatalf("inventory digests = %q / %q", got.InventorySHA256, got.EffectiveOrderSetSHA256)
	}
	byName := make(map[string]SelectorObservedOrder, len(got.Orders))
	for _, order := range got.Orders {
		byName[order.ScopedName] = order
	}
	assertSelectorActivation(t, byName["source-disabled"], orders.ActivationDisabledBySource, false, false)
	assertSelectorActivation(t, byName["override-disabled"], orders.ActivationDisabledByOverride, false, false)
	assertSelectorActivation(t, byName["skipped-name"], orders.ActivationSkippedByName, false, true)
	assertSelectorActivation(t, byName["skipped-alias"], orders.ActivationSkippedByAlias, false, true)
	assertSelectorActivation(t, byName["winner"], orders.ActivationEnabled, true, true)
	if byName["winner"].Formula != "mol-higher" || byName["winner"].WinningSourcePathSHA256 != selectorObservationPathDigest(higherSource) {
		t.Fatalf("higher-layer winner = %#v", byName["winner"])
	}
	if byName["absent"].Resolution != selectorOrderMissing || byName["absent"].Enabled != nil || byName["absent"].ConfiguredEnabled != nil {
		t.Fatalf("absent order = %#v, want conclusive absence distinct from inactive", byName["absent"])
	}
}

func TestSelectorObservationInventoryRawDigestChangesWithUnparsedBytes(t *testing.T) {
	cityPath := t.TempDir()
	orderSource := filepath.Join(cityPath, "orders", "candidate.toml")
	base := "[order]\nformula = \"mol-candidate\"\ntrigger = \"manual\"\n"
	writeSelectorObservationFile(t, orderSource, base)
	first := scanSelectorOrderInventoryFS(fsys.OSFS{}, cityPath, &config.City{}, []string{"candidate"})
	writeSelectorObservationFile(t, orderSource, "# retained source revision two\n"+base)
	second := scanSelectorOrderInventoryFS(fsys.OSFS{}, cityPath, &config.City{}, []string{"candidate"})

	if first.Availability.Status != qualification.StatusAvailable || second.Availability.Status != qualification.StatusAvailable {
		t.Fatalf("inventories unavailable: first=%#v second=%#v", first, second)
	}
	if first.Orders[0].Formula != second.Orders[0].Formula || first.Orders[0].RawSourceSHA256 == second.Orders[0].RawSourceSHA256 {
		t.Fatalf("raw byte binding failed: first=%#v second=%#v", first.Orders[0], second.Orders[0])
	}
	if first.EffectiveOrderSetSHA256 == second.EffectiveOrderSetSHA256 {
		t.Fatal("effective order-set digest ignored a raw-source-only change")
	}
}

func TestSelectorObservationInventoryFailsClosedWhenSelectedSourceChangesDuringScan(t *testing.T) {
	cityPath := t.TempDir()
	orderSource := filepath.Join(cityPath, "orders", "candidate.toml")
	writeSelectorObservationFile(t, orderSource, "[order]\nformula = \"mol-original\"\ntrigger = \"manual\"\n")
	changingFS := &selectorObservationChangingSourceFS{
		FS: fsys.OSFS{}, path: orderSource,
		replacement: []byte("[order]\nformula = \"mol-replacement\"\ntrigger = \"manual\"\n"),
	}

	got := scanSelectorOrderInventoryFS(changingFS, cityPath, &config.City{}, []string{"candidate"})
	if got.Availability.Status != qualification.StatusUnavailable || got.Orders[0].Availability.Status != qualification.StatusUnavailable {
		t.Fatalf("changing source remained conclusive: %#v", got)
	}
	foundFence := false
	for _, issue := range got.Issues {
		if issue.Stage == "source_fence" && issue.SourcePathSHA256 == selectorObservationPathDigest(orderSource) {
			foundFence = true
		}
	}
	if !foundFence {
		t.Fatalf("changing source did not produce a source fence issue: %#v", got.Issues)
	}
}

func TestSelectorObservationInventoryFailsClosedWhenSelectedSourceDisappears(t *testing.T) {
	cityPath := t.TempDir()
	orderSource := filepath.Join(cityPath, "orders", "candidate.toml")
	writeSelectorObservationFile(t, orderSource, "[order]\nformula = \"mol-candidate\"\ntrigger = \"manual\"\n")

	got := scanSelectorOrderInventoryFS(
		selectorObservationDisappearingFS{FS: fsys.OSFS{}, path: orderSource},
		cityPath, &config.City{}, []string{"candidate"},
	)
	if got.Availability.Status != qualification.StatusUnavailable || len(got.Issues) != 1 {
		t.Fatalf("inventory = %#v, want one explicit disappearing-source issue", got)
	}
	issue := got.Issues[0]
	if issue.Stage != "source_read" || issue.SourcePathSHA256 != selectorObservationPathDigest(orderSource) {
		t.Fatalf("source issue = %#v", issue)
	}
	if got.Orders[0].Availability.Status != qualification.StatusUnavailable {
		t.Fatalf("missing order was treated as conclusive after source race: %#v", got.Orders[0])
	}
}

func TestSelectorObservationInventoryIgnoresMissingLegacyProbeInUnrelatedDirectory(t *testing.T) {
	cityPath := t.TempDir()
	writeSelectorObservationFile(t, filepath.Join(cityPath, "orders", "candidate.toml"), "[order]\nformula = \"mol-candidate\"\ntrigger = \"manual\"\n")
	if err := os.MkdirAll(filepath.Join(cityPath, "orders", "unrelated"), 0o755); err != nil {
		t.Fatal(err)
	}

	got := scanSelectorOrderInventoryFS(fsys.OSFS{}, cityPath, &config.City{}, []string{"candidate"})
	if got.Availability.Status != qualification.StatusAvailable || len(got.Issues) != 0 {
		t.Fatalf("normal missing legacy probe made inventory unavailable: %#v", got)
	}
}

func TestSelectorObservationInventoryFailsClosedWhenOrderAppearsDuringScan(t *testing.T) {
	cityPath := t.TempDir()
	ordersPath := filepath.Join(cityPath, "orders")
	writeSelectorObservationFile(t, filepath.Join(ordersPath, "candidate.toml"), "[order]\nformula = \"mol-candidate\"\ntrigger = \"manual\"\n")
	appearingPath := filepath.Join(ordersPath, "appearing.toml")
	changingFS := &selectorObservationAppearingFS{
		FS: fsys.OSFS{}, dir: ordersPath, path: appearingPath,
		contents: []byte("[order]\nformula = \"mol-appearing\"\ntrigger = \"manual\"\n"),
	}

	got := scanSelectorOrderInventoryFS(changingFS, cityPath, &config.City{}, []string{"appearing", "candidate"})
	if got.Availability.Status != qualification.StatusUnavailable {
		t.Fatalf("changing directory inventory = %#v, want unavailable", got)
	}
	foundFence := false
	for _, issue := range got.Issues {
		if issue.Stage == "directory_fence" && issue.SourcePathSHA256 == selectorObservationPathDigest(ordersPath) {
			foundFence = true
		}
	}
	if !foundFence {
		t.Fatalf("changing directory did not produce a directory fence issue: %#v", got.Issues)
	}
	for _, order := range got.Orders {
		if order.Availability.Status != qualification.StatusUnavailable {
			t.Fatalf("requested identity remained conclusive across directory change: %#v", order)
		}
	}
}

func TestSelectorObservationInventoryFailsClosedWhenOrderRootAppearsDuringScan(t *testing.T) {
	cityPath := t.TempDir()
	ordersPath := filepath.Join(cityPath, "orders")
	appearingPath := filepath.Join(ordersPath, "appearing.toml")
	changingFS := &selectorObservationAppearingFS{
		FS: fsys.OSFS{}, dir: ordersPath, path: appearingPath,
		contents: []byte("[order]\nformula = \"mol-appearing\"\ntrigger = \"manual\"\n"),
	}

	got := scanSelectorOrderInventoryFS(changingFS, cityPath, &config.City{}, []string{"appearing"})
	if got.Availability.Status != qualification.StatusUnavailable || got.Orders[0].Availability.Status != qualification.StatusUnavailable {
		t.Fatalf("appearing order root remained conclusive: %#v", got)
	}
	foundFence := false
	for _, issue := range got.Issues {
		if issue.Stage == "directory_fence" && issue.SourcePathSHA256 == selectorObservationPathDigest(ordersPath) {
			foundFence = true
		}
	}
	if !foundFence {
		t.Fatalf("appearing order root did not produce a directory fence issue: %#v", got.Issues)
	}
}

func TestSelectorObservationSnapshotRejectsGenerationChangeDuringScan(t *testing.T) {
	cityPath := t.TempDir()
	writeSelectorObservationFile(t, filepath.Join(cityPath, "orders", "candidate.toml"), "[order]\nformula = \"mol-candidate\"\ntrigger = \"manual\"\n")
	baseFS := fsys.OSFS{}
	blockingFS := &selectorObservationBlockingFS{
		FS: baseFS, entered: make(chan struct{}), release: make(chan struct{}),
	}
	state := &controllerState{
		cfg: &config.City{}, cityPath: cityPath, graphStoreGeneration: 11,
		qualificationBuild: selectorObservationTestBuildIdentity(), eventProv: events.NewFake(),
	}

	result := make(chan SelectorObservationSnapshot, 1)
	go func() {
		result <- state.selectorObservationSnapshotFS(blockingFS, []string{"candidate"})
	}()
	<-blockingFS.entered
	state.mu.Lock()
	state.graphStoreGeneration++
	state.mu.Unlock()
	close(blockingFS.release)
	got := <-result
	if got.GenerationFence.Status != qualification.StatusUnavailable || got.GenerationFence.Reason != "controller_generation_changed" {
		t.Fatalf("generation fence = %#v, want changed", got.GenerationFence)
	}
	if got.ControllerGeneration != 11 {
		t.Fatalf("captured generation = %d, want starting generation 11", got.ControllerGeneration)
	}
}

func TestSelectorObservationScopeValidationDeduplicatesAndFailsClosed(t *testing.T) {
	got := scanSelectorOrderInventoryFS(
		fsys.OSFS{}, t.TempDir(), &config.City{},
		[]string{"zeta", "alpha", "zeta", " invalid "},
	)
	if strings.Join(got.RequestedScopedOrders, ",") != "alpha,zeta" {
		t.Fatalf("requested scope = %#v, want sorted deduplicated valid identities", got.RequestedScopedOrders)
	}
	if got.Availability.Status != qualification.StatusUnavailable || len(got.Issues) != 1 || got.Issues[0].Stage != "scope_validation" {
		t.Fatalf("scope validation result = %#v", got)
	}
}

type selectorObservationTestAuthorizer struct{}

func (selectorObservationTestAuthorizer) Authorize(_ context.Context, request qualification.ReleaseRequest) (qualification.Authorization, error) {
	requestSHA, err := qualification.ReleaseRequestIdentitySHA(request)
	if err != nil {
		return qualification.Authorization{}, err
	}
	return qualification.Authorization{
		Status:               qualification.StatusAuthorized,
		IdentitySHA:          request.Snapshot.EffectiveConfigIdentitySHA256,
		ReleaseRequestSHA256: requestSHA,
		RecordID:             "selector-observation-release",
	}, nil
}

func selectorObservationTestBuildIdentity() qualification.BuildIdentity {
	return qualification.BuildIdentity{
		Status:         qualification.StatusAvailable,
		SourceRevision: strings.Repeat("1", 40),
		BuildID:        strings.Repeat("1", 40),
		Version:        "test",
		ArtifactStatus: qualification.StatusAvailable,
		ArtifactSHA256: strings.Repeat("2", 64),
	}
}

func assertSelectorActivation(t *testing.T, got SelectorObservedOrder, want orders.ActivationDisposition, wantEnabled, wantConfigured bool) {
	t.Helper()
	if got.Resolution != selectorOrderFound || got.Activation != want || got.Enabled == nil || *got.Enabled != wantEnabled || got.ConfiguredEnabled == nil || *got.ConfiguredEnabled != wantConfigured {
		t.Fatalf("selector activation = %#v, want activation=%q enabled=%t configured=%t", got, want, wantEnabled, wantConfigured)
	}
}

type selectorObservationBlockingFS struct {
	fsys.FS
	once    sync.Once
	entered chan struct{}
	release chan struct{}
}

type selectorObservationDisappearingFS struct {
	fsys.FS
	path string
}

type selectorObservationChangingSourceFS struct {
	fsys.FS
	path        string
	replacement []byte
	once        sync.Once
	writeErr    error
}

type selectorObservationAppearingFS struct {
	fsys.FS
	dir      string
	path     string
	contents []byte
	once     sync.Once
}

func (fs *selectorObservationAppearingFS) ReadDir(path string) ([]os.DirEntry, error) {
	entries, err := fs.FS.ReadDir(path)
	if filepath.Clean(path) == filepath.Clean(fs.dir) {
		var writeErr error
		fs.once.Do(func() {
			if writeErr = os.MkdirAll(filepath.Dir(fs.path), 0o755); writeErr == nil {
				writeErr = os.WriteFile(fs.path, fs.contents, 0o644)
			}
		})
		if writeErr != nil && err == nil {
			return entries, writeErr
		}
	}
	return entries, err
}

func (fs selectorObservationDisappearingFS) ReadFile(path string) ([]byte, error) {
	if path == fs.path {
		return nil, os.ErrNotExist
	}
	return fs.FS.ReadFile(path)
}

func (fs *selectorObservationChangingSourceFS) ReadFile(path string) ([]byte, error) {
	data, err := fs.FS.ReadFile(path)
	if err == nil && filepath.Clean(path) == filepath.Clean(fs.path) {
		fs.once.Do(func() {
			fs.writeErr = os.WriteFile(fs.path, fs.replacement, 0o644)
		})
		if fs.writeErr != nil {
			return data, fs.writeErr
		}
	}
	return data, err
}

func (fs *selectorObservationBlockingFS) ReadDir(path string) ([]os.DirEntry, error) {
	fs.once.Do(func() {
		close(fs.entered)
		<-fs.release
	})
	return fs.FS.ReadDir(path)
}

func writeSelectorObservationFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}
