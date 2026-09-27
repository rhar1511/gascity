package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/agentutil"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	convoycore "github.com/gastownhall/gascity/internal/convoy"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/sling"
	"github.com/gastownhall/gascity/internal/sourceworkflow"
	"github.com/gastownhall/gascity/internal/storeref"
	"github.com/gastownhall/gascity/internal/worklifecycle"
)

// reconcileLifecycleAdmission discovers explicitly admitted, not-yet-routed
// work on the controller's authoritative routed-work legs. A signed receipt
// supplies the route and workflow contract. A unique conditional reservation
// grants exactly one controller pass permission to invoke the existing sling
// materializer; later passes hold an incomplete reservation for review.
func reconcileLifecycleAdmission(
	cityName string,
	cityPath string,
	cfg *config.City,
	store beads.Store,
	rigStores map[string]beads.Store,
	suspendedRigPaths map[string]bool,
	stderr io.Writer,
) {
	if cfg == nil || !cfg.Lifecycle.AdmissionEnabled {
		return
	}
	runner := sling.SlingRunner(shellSlingRunner)
	legs, err := routedWorkStoreCandidates(cityPath, cfg, store, rigStores, suspendedRigPaths)
	if err != nil {
		fmt.Fprintf(stderr, "lifecycle admission: resolving work stores: %v\n", err) //nolint:errcheck
		return
	}
	seen := make(map[storeScopedBeadKey]struct{}, 16)
	for _, leg := range legs {
		if leg.store == nil {
			continue
		}
		rows, err := leg.store.List(beads.ListQuery{
			Status: "open",
			Live:   true,
		})
		if err != nil {
			fmt.Fprintf(stderr, "lifecycle admission: listing %s intent: %v\n", leg.ref, err) //nolint:errcheck
			continue
		}
		readyRows, err := beads.HandlesFor(leg.store).Live.Ready(beads.ReadyQuery{TierMode: beads.FederatedReadTier})
		if err != nil {
			fmt.Fprintf(stderr, "lifecycle admission: reading ready work in %s: %v\n", leg.ref, err) //nolint:errcheck
			continue
		}
		ready := make(map[string]struct{}, len(readyRows))
		for _, row := range readyRows {
			ready[row.ID] = struct{}{}
		}
		for _, bead := range rows {
			key := storeScopedBeadKey{StoreRef: leg.ref, ID: bead.ID}
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			if !rootStoreRefMatchesCandidate(bead.Metadata[beadmeta.RootStoreRefMetadataKey], leg.ref) {
				continue
			}
			scope := lifecycleScopeForRef(cityName, cfg, leg.ref)
			admission := worklifecycle.EvaluateAdmission(bead, cfg.Lifecycle, scope)
			if !admission.Requested || !admission.Admitted {
				continue
			}
			if _, ok := ready[bead.ID]; !ok {
				continue
			}
			// The route write is not an eligibility override. Do not mutate held,
			// assigned, excluded, or deferred work; normal ready/dependency checks
			// still decide when an admitted route becomes demand.
			if !demandRowServable(bead) || beads.IsDeferred(bead, time.Now()) {
				continue
			}
			route := agentutil.NormalizePoolRouteTarget(cfg, admission.Receipt.Route)
			agentCfg := findAgentByTemplate(cfg, route)
			if agentCfg == nil || agentCfg.Suspended || !agentCfg.SupportsGenericEphemeralSessions() {
				fmt.Fprintf(stderr, "lifecycle admission: %s has an unusable configured route %q\n", bead.ID, route) //nolint:errcheck
				continue
			}
			if strings.TrimSpace(agentCfg.EffectiveDefaultSlingFormula()) != admission.Receipt.Workflow {
				fmt.Fprintf(stderr, "lifecycle admission: %s signed workflow %q does not match route %q configured workflow %q\n", bead.ID, admission.Receipt.Workflow, route, agentCfg.EffectiveDefaultSlingFormula()) //nolint:errcheck
				continue
			}
			if isCustomSlingQuery(*agentCfg) {
				fmt.Fprintf(stderr, "lifecycle admission: %s route %q uses a custom sling_query; controller materialization is held\n", bead.ID, route) //nolint:errcheck
				continue
			}
			routes := controllerDemandRouteCandidates(bead)
			if len(routes) != 0 {
				matches := false
				for _, current := range routes {
					if agentutil.NormalizePoolRouteTarget(cfg, current) == route {
						matches = true
						break
					}
				}
				if !matches {
					fmt.Fprintf(stderr, "lifecycle admission: %s route conflicts with its signed admission receipt; preserving current route\n", bead.ID) //nolint:errcheck
				} else if !lifecycleWorkflowAttached(bead) {
					fmt.Fprintf(stderr, "lifecycle admission: %s already has route metadata but no attached workflow evidence; preserving route and holding work\n", bead.ID) //nolint:errcheck
				}
				continue
			}
			if lifecycleWorkflowAttached(bead) {
				fmt.Fprintf(stderr, "lifecycle admission: %s has workflow evidence without a route; preserving it for review\n", bead.ID) //nolint:errcheck
				continue
			}
			if bead.Revision == 0 {
				fmt.Fprintf(stderr, "lifecycle admission: %s has no usable revision; workflow materialization held\n", bead.ID) //nolint:errcheck
				continue
			}
			writer, ok := beads.ConditionalWriterFor(leg.store)
			if !ok {
				fmt.Fprintf(stderr, "lifecycle admission: %s store does not support revision-conditional writes; workflow materialization held\n", bead.ID) //nolint:errcheck
				continue
			}
			digest, err := worklifecycle.AdmissionDigest(admission.Receipt)
			if err != nil {
				fmt.Fprintf(stderr, "lifecycle admission: hashing admission contract for %s: %v\n", bead.ID, err) //nolint:errcheck
				continue
			}
			previous := strings.TrimSpace(bead.Metadata[beadmeta.LifecycleMaterializationMetadataKey])
			if previous != "" {
				fmt.Fprintf(stderr, "lifecycle admission: %s has an incomplete prior materialization reservation; holding for review\n", bead.ID) //nolint:errcheck
				continue
			}
			deps, graphStoreRef, err := lifecycleSlingDeps(cityName, cityPath, cfg, store, rigStores, suspendedRigPaths, leg, legs, runner)
			if err != nil {
				fmt.Fprintf(stderr, "lifecycle admission: preparing workflow materialization for %s: %v\n", bead.ID, err) //nolint:errcheck
				continue
			}
			token, err := lifecycleReservationToken()
			if err != nil {
				fmt.Fprintf(stderr, "lifecycle admission: creating materialization reservation token for %s: %v\n", bead.ID, err) //nolint:errcheck
				continue
			}
			reservationValue, err := encodeLifecycleMaterialization(lifecycleMaterialization{
				Version:          1,
				State:            "reserved",
				Scope:            scope,
				Contract:         digest,
				Route:            route,
				Workflow:         admission.Receipt.Workflow,
				MergeStrategy:    admission.Receipt.MergeStrategy,
				Token:            token,
				SourceID:         bead.ID,
				SourceStoreRef:   leg.ref,
				WorkflowStoreRef: graphStoreRef,
				AdmissionReceipt: bead.Metadata[beadmeta.LifecycleAdmissionReceiptMetadataKey],
			})
			if err != nil {
				fmt.Fprintf(stderr, "lifecycle admission: encoding materialization reservation for %s: %v\n", bead.ID, err) //nolint:errcheck
				continue
			}
			if err := writer.UpdateIfMatch(bead.ID, bead.Revision, beads.UpdateOpts{
				Metadata: map[string]string{beadmeta.LifecycleMaterializationMetadataKey: reservationValue},
			}); err != nil {
				fmt.Fprintf(stderr, "lifecycle admission: reserving workflow materialization for %s: %v\n", bead.ID, err) //nolint:errcheck
				continue
			}
			lineageValue, err := encodeLifecycleMaterialization(lifecycleMaterialization{
				Version:          1,
				State:            "lineage_pending",
				Scope:            scope,
				Contract:         digest,
				Route:            route,
				Workflow:         admission.Receipt.Workflow,
				MergeStrategy:    admission.Receipt.MergeStrategy,
				Token:            token,
				SourceID:         bead.ID,
				SourceStoreRef:   leg.ref,
				WorkflowStoreRef: graphStoreRef,
				AdmissionReceipt: bead.Metadata[beadmeta.LifecycleAdmissionReceiptMetadataKey],
			})
			if err != nil {
				fmt.Fprintf(stderr, "lifecycle admission: encoding graph lineage for %s: %v\n", bead.ID, err) //nolint:errcheck
				continue
			}
			deps.LifecycleRecipeMetadata = map[string]string{
				beadmeta.LifecycleMaterializationMetadataKey: lineageValue,
				beadmeta.MergeStrategyMetadataKey:            admission.Receipt.MergeStrategy,
			}
			scopeKind, scopeRef := lifecycleSlingScope(leg.ref, cityName)
			result, err := sling.DoSling(sling.SlingOpts{
				Target:               *agentCfg,
				BeadOrFormula:        bead.ID,
				Merge:                admission.Receipt.MergeStrategy,
				RequireFormulaAttach: true,
				ScopeKind:            scopeKind,
				ScopeRef:             scopeRef,
				BeforeFormulaAttach: func() error {
					return recheckLifecycleMaterialization(leg.store, bead.ID, scope, reservationValue, digest, cfg.Lifecycle)
				},
			}, deps, leg.store)
			if err != nil {
				fmt.Fprintf(stderr, "lifecycle admission: materializing workflow %q for %s: %v; work remains held\n", admission.Receipt.Workflow, bead.ID, err) //nolint:errcheck
				continue
			}
			workflowID := strings.TrimSpace(result.WorkflowID)
			if workflowID == "" {
				workflowID = strings.TrimSpace(result.WispRootID)
			}
			if workflowID == "" {
				fmt.Fprintf(stderr, "lifecycle admission: sling did not report an attached workflow for %s; work remains held\n", bead.ID) //nolint:errcheck
				continue
			}
			current, err := leg.store.Get(bead.ID)
			if err != nil {
				fmt.Fprintf(stderr, "lifecycle admission: verifying attached workflow for %s: %v\n", bead.ID, err) //nolint:errcheck
				continue
			}
			graphWorkflow := strings.TrimSpace(result.WorkflowID) != ""
			workflowStoreRef := leg.ref
			if graphWorkflow {
				workflowStoreRef = graphStoreRef
			}
			if !lifecycleSlingResultMatches(current, leg.store, deps.GraphStore, cfg, scope, digest, route, admission.Receipt.Workflow, admission.Receipt.MergeStrategy, workflowID, graphWorkflow, reservationValue, graphStoreRef) {
				fmt.Fprintf(stderr, "lifecycle admission: sling result did not leave the signed workflow and route visible for %s; work remains held\n", bead.ID) //nolint:errcheck
				continue
			}
			if graphWorkflow {
				attachedLineage, err := encodeLifecycleMaterialization(lifecycleMaterialization{
					Version:          1,
					State:            "attached",
					Scope:            scope,
					Contract:         digest,
					Route:            route,
					Workflow:         admission.Receipt.Workflow,
					MergeStrategy:    admission.Receipt.MergeStrategy,
					Token:            token,
					WorkflowID:       workflowID,
					SourceID:         bead.ID,
					SourceStoreRef:   leg.ref,
					WorkflowStoreRef: graphStoreRef,
					AdmissionReceipt: bead.Metadata[beadmeta.LifecycleAdmissionReceiptMetadataKey],
				})
				if err != nil {
					fmt.Fprintf(stderr, "lifecycle admission: encoding attached graph lineage for %s: %v\n", bead.ID, err) //nolint:errcheck
					continue
				}
				if err := attachLifecycleGraphLineage(deps.GraphStore, workflowID, lineageValue, attachedLineage); err != nil {
					fmt.Fprintf(stderr, "lifecycle admission: persisting graph lineage for %s: %v; source remains held\n", bead.ID, err) //nolint:errcheck
					continue
				}
			}
			attachedValue, err := encodeLifecycleMaterialization(lifecycleMaterialization{
				Version:          1,
				State:            "attached",
				Scope:            scope,
				Contract:         digest,
				Route:            route,
				Workflow:         admission.Receipt.Workflow,
				MergeStrategy:    admission.Receipt.MergeStrategy,
				Token:            token,
				WorkflowID:       workflowID,
				SourceID:         bead.ID,
				SourceStoreRef:   leg.ref,
				WorkflowStoreRef: workflowStoreRef,
				AdmissionReceipt: bead.Metadata[beadmeta.LifecycleAdmissionReceiptMetadataKey],
			})
			if err != nil {
				fmt.Fprintf(stderr, "lifecycle admission: encoding attached workflow evidence for %s: %v\n", bead.ID, err) //nolint:errcheck
				continue
			}
			writer, ok = beads.ConditionalWriterFor(leg.store)
			if !ok || writer.UpdateIfMatch(current.ID, current.Revision, beads.UpdateOpts{Metadata: map[string]string{beadmeta.LifecycleMaterializationMetadataKey: attachedValue}}) != nil {
				fmt.Fprintf(stderr, "lifecycle admission: could not persist verified workflow evidence for %s; work remains held\n", bead.ID) //nolint:errcheck
			}
		}
	}
}

type lifecycleMaterialization struct {
	Version          int    `json:"version"`
	State            string `json:"state"`
	Scope            string `json:"scope"`
	Contract         string `json:"contract"`
	Route            string `json:"route"`
	Workflow         string `json:"workflow"`
	MergeStrategy    string `json:"merge_strategy"`
	Token            string `json:"token"`
	WorkflowID       string `json:"workflow_id,omitempty"`
	SourceID         string `json:"source_id,omitempty"`
	SourceStoreRef   string `json:"source_store_ref,omitempty"`
	WorkflowStoreRef string `json:"workflow_store_ref,omitempty"`
	AdmissionReceipt string `json:"admission_receipt,omitempty"`
}

func encodeLifecycleMaterialization(value lifecycleMaterialization) (string, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

func lifecycleReservationToken() (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}

func recheckLifecycleMaterialization(store beads.Store, beadID, scope, reservation, digest string, cfg config.LifecycleConfig) error {
	current, err := store.Get(beadID)
	if err != nil {
		return fmt.Errorf("reread source bead: %w", err)
	}
	decision := worklifecycle.EvaluateAdmission(current, cfg, scope)
	if !decision.Requested || !decision.Admitted {
		return fmt.Errorf("signed admission changed or became invalid: %s", decision.Reason)
	}
	currentDigest, err := worklifecycle.AdmissionDigest(decision.Receipt)
	if err != nil || currentDigest != digest {
		return fmt.Errorf("signed workflow contract changed before materialization")
	}
	if strings.TrimSpace(current.Metadata[beadmeta.LifecycleMaterializationMetadataKey]) != reservation {
		return fmt.Errorf("controller materialization reservation changed before effect")
	}
	if len(controllerDemandRouteCandidates(current)) != 0 || lifecycleWorkflowAttached(current) {
		return fmt.Errorf("work was routed or attached before materialization")
	}
	if !demandRowServable(current) || beads.IsDeferred(current, time.Now()) {
		return fmt.Errorf("work is held, assigned, or deferred")
	}
	ready, err := beads.HandlesFor(store).Live.Ready(beads.ReadyQuery{TierMode: beads.FederatedReadTier})
	if err != nil {
		return fmt.Errorf("reread ready work: %w", err)
	}
	for _, row := range ready {
		if row.ID == beadID {
			return nil
		}
	}
	return fmt.Errorf("work is no longer in the live ready set")
}

func lifecycleSlingResultMatches(bead beads.Bead, workStore, graphStore beads.Store, cfg *config.City, scope, digest, route, workflow, merge, workflowID string, graphWorkflow bool, reservation, plannedGraphStoreRef string) bool {
	decision := worklifecycle.EvaluateAdmission(bead, cfg.Lifecycle, scope)
	currentDigest, err := worklifecycle.AdmissionDigest(decision.Receipt)
	marker, markerOK := lifecycleMaterializationFor(bead)
	if err != nil || !decision.Admitted || currentDigest != digest ||
		!markerOK || marker.State != "reserved" || strings.TrimSpace(bead.Metadata[beadmeta.LifecycleMaterializationMetadataKey]) != reservation ||
		marker.Scope != scope || marker.Contract != digest || marker.Token == "" || marker.SourceID != bead.ID || marker.SourceStoreRef == "" ||
		marker.WorkflowStoreRef != plannedGraphStoreRef || bead.Status != "open" || !demandRowServable(bead) || beads.IsDeferred(bead, time.Now()) {
		return false
	}
	if !graphWorkflow {
		if !lifecycleRoutesMatch(cfg, bead, route) || strings.TrimSpace(bead.Metadata[beadmeta.MergeStrategyMetadataKey]) != merge {
			return false
		}
	}
	if strings.TrimSpace(workflowID) == "" {
		return false
	}
	workflowStore := workStore
	if graphWorkflow {
		workflowStore = graphStore
	}
	if workflowStore == nil {
		return false
	}
	root, err := workflowStore.Get(workflowID)
	if err != nil || strings.TrimSpace(root.Metadata[beadmeta.FormulaNameMetadataKey]) != workflow || strings.EqualFold(strings.TrimSpace(root.Status), "closed") {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(root.Metadata[beadmeta.FormulaContractMetadataKey]), beadmeta.FormulaContractGraphV2) {
		if !lifecycleRoutesMatch(cfg, root, route) || strings.TrimSpace(root.Metadata[beadmeta.MergeStrategyMetadataKey]) != merge {
			return false
		}
		rootMarker, rootMarkerOK := lifecycleMaterializationFor(root)
		if !rootMarkerOK || rootMarker.State != "lineage_pending" || rootMarker.SourceID != bead.ID ||
			rootMarker.SourceStoreRef != marker.SourceStoreRef || rootMarker.WorkflowStoreRef != plannedGraphStoreRef || rootMarker.Contract != digest {
			return false
		}
		if !sourceworkflow.IsWorkflowRoot(root) {
			return false
		}
		inputConvoy := strings.TrimSpace(root.Metadata[beadmeta.InputConvoyIDMetadataKey])
		if inputConvoy == "" {
			return false
		}
		members, err := convoycore.Members(workStore, inputConvoy, false)
		if err != nil || !slices.ContainsFunc(members, func(member beads.Bead) bool { return member.ID == bead.ID }) {
			return false
		}
	} else if strings.TrimSpace(bead.Metadata[beadmeta.MoleculeIDMetadataKey]) != workflowID && strings.TrimSpace(bead.Metadata[beadmeta.WorkflowIDMetadataKey]) != workflowID {
		return false
	}
	return true
}

func lifecycleScopeForRef(cityName string, cfg *config.City, ref string) string {
	if strings.TrimSpace(cityName) == "" {
		cityName = censusCityName(cfg)
	}
	return worklifecycle.ScopeForStore(cityName, ref)
}

func lifecycleScopeForDemand(cityName string, cfg *config.City, storeKey string) string {
	storeKey = strings.TrimSpace(storeKey)
	if storeKey == "city" {
		storeKey = "city:" + censusCityName(cfg)
	}
	return lifecycleScopeForRef(cityName, cfg, storeKey)
}

func lifecycleWorkflowAttached(bead beads.Bead) bool {
	return strings.TrimSpace(bead.Metadata["workflow_id"]) != "" || strings.TrimSpace(bead.Metadata["molecule_id"]) != "" || lifecycleMaterializationEvidence(bead)
}

func lifecycleMaterializationEvidence(bead beads.Bead) bool {
	marker, ok := lifecycleMaterializationFor(bead)
	if !ok {
		return false
	}
	return marker.Version == 1 && marker.State == "attached" && strings.TrimSpace(marker.Scope) != "" &&
		strings.TrimSpace(marker.Contract) != "" && strings.TrimSpace(marker.Route) != "" &&
		strings.TrimSpace(marker.Workflow) != "" && strings.TrimSpace(marker.MergeStrategy) != "" &&
		strings.TrimSpace(marker.Token) != "" && strings.TrimSpace(marker.WorkflowID) != "" &&
		strings.TrimSpace(marker.SourceID) != "" && strings.TrimSpace(marker.SourceStoreRef) != "" &&
		strings.TrimSpace(marker.WorkflowStoreRef) != "" && strings.TrimSpace(marker.AdmissionReceipt) != ""
}

func lifecycleMaterializationFor(bead beads.Bead) (lifecycleMaterialization, bool) {
	raw := strings.TrimSpace(bead.Metadata[beadmeta.LifecycleMaterializationMetadataKey])
	if raw == "" {
		return lifecycleMaterialization{}, false
	}
	var marker lifecycleMaterialization
	if err := json.Unmarshal([]byte(raw), &marker); err != nil {
		return lifecycleMaterialization{}, false
	}
	return marker, true
}

func lifecycleSlingDeps(
	cityName, cityPath string,
	cfg *config.City,
	store beads.Store,
	rigStores map[string]beads.Store,
	suspendedRigPaths map[string]bool,
	selected classStoreCandidate,
	sourceStores []classStoreCandidate,
	runner sling.SlingRunner,
) (sling.SlingDeps, string, error) {
	work := censusWorkLeg(cityPath, store)
	topology := residencyTopologyForCity(cityPath, cfg, work, servingRigStores(cfg, rigStores, suspendedRigPaths))
	var graphStore beads.Store
	graphStoreRef := ""
	if rig, scoped := storeref.ScopeRigContext(selected.ref); scoped && rig != "" {
		// Rig-scoped formulas materialize their graph in the rig scope. A city
		// graph-class binding is authoritative only for city-scoped formulas.
		graphStore = selected.store
		graphStoreRef = selected.ref
	} else {
		graphPlan, err := storeref.Plan(storeref.Class{C: coordclass.ClassGraph}, topology)
		if err != nil {
			return sling.SlingDeps{}, "", fmt.Errorf("resolving graph workflow store: %w", err)
		}
		graphLeg, err := storeref.ResolvePlacement(graphPlan)
		if err != nil {
			return sling.SlingDeps{}, "", fmt.Errorf("resolving graph workflow placement: %w", err)
		}
		graphStore = graphLeg.Store
		graphStoreRef = censusRef(cfg, graphLeg.Ref, censusRefScoped)
	}
	if graphStore == nil {
		return sling.SlingDeps{}, "", fmt.Errorf("graph workflow store is unavailable")
	}
	if strings.TrimSpace(cityName) == "" {
		cityName = censusCityName(cfg)
	}
	deps := sling.SlingDeps{
		CityName:           cityName,
		CityPath:           cityPath,
		Cfg:                cfg,
		Runner:             runner,
		Store:              selected.store,
		GraphStore:         graphStore,
		ExecutionWorkStore: selected.store,
		StoreRef:           selected.ref,
		SourceWorkflowStores: func() ([]sling.SourceWorkflowStore, error) {
			out := make([]sling.SourceWorkflowStore, 0, len(sourceStores))
			for _, source := range sourceStores {
				if source.store == nil {
					continue
				}
				out = append(out, sling.SourceWorkflowStore{Store: source.store, StoreRef: source.ref, Strict: true})
			}
			return out, nil
		},
	}
	populateSlingDepsCallbacks(&deps)
	return deps, graphStoreRef, nil
}

func lifecycleSlingScope(storeRef, cityName string) (kind, ref string) {
	if rig, scoped := storeref.ScopeRigContext(storeRef); scoped && rig != "" {
		return "rig", rig
	}
	cityName = strings.TrimSpace(cityName)
	if cityName == "" {
		cityName = "city"
	}
	return "city", cityName
}

// lifecycleStoreForRef resolves a persisted census/store ref to exactly the
// store that ref names. It is intentionally strict: a missing class binding or
// rig path is an unavailable authority, never a reason to try the city store.
func lifecycleStoreForRef(cityPath string, cfg *config.City, ref string) (beads.Store, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return nil, fmt.Errorf("empty lifecycle store ref")
	}
	if strings.HasPrefix(ref, "city:") {
		city := strings.TrimSpace(strings.TrimPrefix(ref, "city:"))
		if cfg == nil || city == "" || city != censusCityName(cfg) {
			return nil, fmt.Errorf("lifecycle store ref %q does not name this city", ref)
		}
		return openStoreAtForCity(cityPath, cityPath)
	}
	if strings.HasPrefix(ref, "rig:") {
		rigName := strings.TrimSpace(strings.TrimPrefix(ref, "rig:"))
		if cfg == nil || rigName == "" {
			return nil, fmt.Errorf("invalid lifecycle rig store ref %q", ref)
		}
		for _, rig := range cfg.Rigs {
			if strings.TrimSpace(rig.Name) == rigName {
				return openStoreAtForCity(rig.Path, cityPath)
			}
		}
		return nil, fmt.Errorf("lifecycle rig store %q is not configured", ref)
	}
	if !storeref.IsClassRef(ref) {
		return nil, fmt.Errorf("unsupported lifecycle store ref %q", ref)
	}
	if cfg == nil {
		return nil, fmt.Errorf("cannot resolve class store %q without city config", ref)
	}
	work, err := openStoreAtForCity(cityPath, cityPath)
	if err != nil {
		return nil, fmt.Errorf("open city work store for class resolution: %w", err)
	}
	rigs := make(map[string]beads.Store, len(cfg.Rigs))
	for _, rig := range cfg.Rigs {
		rigStore, err := openStoreAtForCity(rig.Path, cityPath)
		if err != nil {
			return nil, fmt.Errorf("open rig %q while resolving class store %s: %w", rig.Name, ref, err)
		}
		rigs[rig.Name] = rigStore
	}
	topology := residencyTopologyForCity(cityPath, cfg, work, servingRigStores(cfg, rigs, nil))
	for _, class := range coordclass.Classes() {
		plan, err := storeref.Plan(storeref.Class{C: class}, topology)
		if err != nil {
			return nil, fmt.Errorf("resolve class %s while looking for %s: %w", class, ref, err)
		}
		leg, err := storeref.ResolvePlacement(plan)
		if err != nil {
			return nil, fmt.Errorf("resolve class %s placement while looking for %s: %w", class, ref, err)
		}
		if censusRef(cfg, leg.Ref, censusRefScoped) == ref {
			return leg.Store, nil
		}
	}
	return nil, fmt.Errorf("configured topology has no store named %q", ref)
}

// attachLifecycleGraphLineage flips every node in one materialized graph from
// its pre-create hold to attached lineage. The source bead remains reserved
// until this entire exact-store pass and readback succeed, so a partial graph
// stamp cannot make any descendant executable.
func attachLifecycleGraphLineage(store beads.Store, workflowID, pendingValue, attachedValue string) error {
	if store == nil || strings.TrimSpace(workflowID) == "" {
		return fmt.Errorf("workflow store and ID are required")
	}
	root, err := store.Get(workflowID)
	if err != nil {
		return fmt.Errorf("read workflow root %s from its selected store: %w", workflowID, err)
	}
	rows, err := store.ListByMetadata(map[string]string{beadmeta.RootBeadIDMetadataKey: workflowID}, 0, beads.WithBothTiers)
	if err != nil {
		return fmt.Errorf("list workflow descendants %s from its selected store: %w", workflowID, err)
	}
	rows = append(rows, root)
	seen := make(map[string]struct{}, len(rows))
	writer, ok := beads.ConditionalWriterFor(store)
	if !ok || !beads.InspectConditionalWrites(store).Capable {
		return fmt.Errorf("workflow store does not support revision-conditional lineage writes")
	}
	for _, row := range rows {
		if row.ID == "" {
			return fmt.Errorf("workflow %s contains a row without an ID", workflowID)
		}
		if _, ok := seen[row.ID]; ok {
			continue
		}
		seen[row.ID] = struct{}{}
		if row.ID != workflowID && strings.TrimSpace(row.Metadata[beadmeta.RootBeadIDMetadataKey]) != workflowID {
			return fmt.Errorf("row %s does not belong to workflow %s", row.ID, workflowID)
		}
		if strings.TrimSpace(row.Metadata[beadmeta.LifecycleMaterializationMetadataKey]) != pendingValue {
			return fmt.Errorf("row %s did not retain the controller's pending lineage marker", row.ID)
		}
		if row.Revision == 0 {
			return fmt.Errorf("row %s has no usable revision for lineage attachment", row.ID)
		}
		if err := writer.UpdateIfMatch(row.ID, row.Revision, beads.UpdateOpts{
			Metadata: map[string]string{beadmeta.LifecycleMaterializationMetadataKey: attachedValue},
		}); err != nil {
			return fmt.Errorf("attach lineage to %s: %w", row.ID, err)
		}
		verified, err := store.Get(row.ID)
		if err != nil || strings.TrimSpace(verified.Metadata[beadmeta.LifecycleMaterializationMetadataKey]) != attachedValue {
			return fmt.Errorf("lineage readback for %s did not verify", row.ID)
		}
	}
	return nil
}

// lifecycleAdmissionRouteMatches reports whether a work item whose lifecycle
// enrollment has been verified still carries the route the trusted receipt
// authorized. It deliberately does not repair a conflicting pre-existing
// route; an operator must resolve that ambiguity.
func lifecycleAdmissionRouteMatches(cfg *config.City, bead beads.Bead, scope string) bool {
	if cfg == nil || !cfg.Lifecycle.AdmissionEnabled {
		return true
	}
	if marker, ok := lifecycleMaterializationFor(bead); ok && marker.SourceID != "" && marker.SourceID != bead.ID {
		return lifecycleLineageAdmissionMatches(cfg, bead, marker)
	}
	decision := worklifecycle.EvaluateAdmission(bead, cfg.Lifecycle, scope)
	if !decision.Requested {
		return !lifecycleEnrollmentEvidence(bead)
	}
	if !decision.Admitted || !lifecycleMaterializationEvidence(bead) {
		return false
	}
	marker, ok := lifecycleMaterializationFor(bead)
	if !ok {
		return false
	}
	digest, err := worklifecycle.AdmissionDigest(decision.Receipt)
	if err != nil || marker.Scope != scope || marker.Contract != digest || marker.Workflow != decision.Receipt.Workflow ||
		marker.MergeStrategy != decision.Receipt.MergeStrategy || marker.WorkflowID == "" ||
		agentutil.NormalizePoolRouteTarget(cfg, marker.Route) != agentutil.NormalizePoolRouteTarget(cfg, decision.Receipt.Route) {
		return false
	}
	// Formula-v1 attachments write one of these identifiers onto the source.
	// If present, it must agree with the root that the controller verified. A
	// graph.v2 attachment is convoy-first and can legitimately have neither.
	for _, sourceWorkflowID := range []string{bead.Metadata[beadmeta.MoleculeIDMetadataKey], bead.Metadata[beadmeta.WorkflowIDMetadataKey]} {
		if sourceWorkflowID = strings.TrimSpace(sourceWorkflowID); sourceWorkflowID != "" && sourceWorkflowID != marker.WorkflowID {
			return false
		}
	}
	permitted := agentutil.NormalizePoolRouteTarget(cfg, decision.Receipt.Route)
	agentCfg := findAgentByTemplate(cfg, permitted)
	if agentCfg == nil || agentCfg.Suspended || !agentCfg.SupportsGenericEphemeralSessions() ||
		isCustomSlingQuery(*agentCfg) || strings.TrimSpace(agentCfg.EffectiveDefaultSlingFormula()) != decision.Receipt.Workflow {
		return false
	}
	if strings.TrimSpace(bead.Metadata[beadmeta.MergeStrategyMetadataKey]) != decision.Receipt.MergeStrategy {
		return false
	}
	return lifecycleRoutesMatch(cfg, bead, permitted)
}

// lifecycleRouteCandidates includes the execution route attached to graph.v2
// recipe steps. Those steps are selected through the workflow's input convoy,
// so they intentionally do not carry gc.routed_to (the source work's direct
// demand route); their gc.execution_routed_to is covered by the controller's
// signed lineage metadata instead.
func lifecycleRouteCandidates(bead beads.Bead) []string {
	candidates := controllerDemandRouteCandidates(bead)
	if executionRoute := strings.TrimSpace(bead.Metadata[beadmeta.ExecutionRoutedToMetadataKey]); executionRoute != "" {
		candidates = append(candidates, executionRoute)
	}
	return candidates
}

func lifecycleRoutesMatch(cfg *config.City, bead beads.Bead, authorizedRoute string) bool {
	routes := lifecycleRouteCandidates(bead)
	if len(routes) == 0 {
		return false
	}
	authorizedRoute = agentutil.NormalizePoolRouteTarget(cfg, authorizedRoute)
	for _, route := range routes {
		if agentutil.NormalizePoolRouteTarget(cfg, route) != authorizedRoute {
			return false
		}
	}
	return true
}

// lifecycleLineageAdmissionMatches verifies the controller-carried signed
// contract on one graph.v2 node without trusting the node's own role or route
// strings. The hook adds live source/root reads before it can execute the node;
// demand uses this local signature check so an attached descendant remains
// visible to its configured worker.
func lifecycleLineageAdmissionMatches(cfg *config.City, bead beads.Bead, marker lifecycleMaterialization) bool {
	if cfg == nil || !cfg.Lifecycle.AdmissionEnabled || marker.Version != 1 || marker.State != "attached" ||
		strings.TrimSpace(marker.SourceID) == "" || strings.TrimSpace(marker.SourceStoreRef) == "" ||
		strings.TrimSpace(marker.WorkflowStoreRef) == "" || strings.TrimSpace(marker.WorkflowID) == "" ||
		strings.TrimSpace(marker.AdmissionReceipt) == "" ||
		strings.TrimSpace(bead.Metadata[beadmeta.RootStoreRefMetadataKey]) != marker.SourceStoreRef {
		return false
	}
	if bead.ID == marker.WorkflowID {
		if !sourceworkflow.IsWorkflowRoot(bead) || strings.TrimSpace(bead.Metadata[beadmeta.FormulaNameMetadataKey]) != marker.Workflow {
			return false
		}
		if !lifecycleRoutesMatch(cfg, bead, marker.Route) {
			return false
		}
	} else if rootID := strings.TrimSpace(bead.Metadata[beadmeta.RootBeadIDMetadataKey]); rootID != marker.WorkflowID {
		return false
	}
	if strings.TrimSpace(bead.Metadata[beadmeta.MergeStrategyMetadataKey]) != marker.MergeStrategy {
		return false
	}
	if !lifecycleRoutesMatch(cfg, bead, marker.Route) {
		return false
	}
	receiptBead := beads.Bead{
		ID:     marker.SourceID,
		Labels: []string{worklifecycle.AdmissionIntentLabel},
		Metadata: map[string]string{
			beadmeta.LifecycleAdmissionReceiptMetadataKey: marker.AdmissionReceipt,
		},
	}
	decision := worklifecycle.EvaluateAdmission(receiptBead, cfg.Lifecycle, marker.Scope)
	digest, err := worklifecycle.AdmissionDigest(decision.Receipt)
	return err == nil && decision.Admitted && digest == marker.Contract &&
		decision.Receipt.Route == marker.Route && decision.Receipt.Workflow == marker.Workflow &&
		decision.Receipt.MergeStrategy == marker.MergeStrategy
}

func lifecycleAdmissionRequested(bead beads.Bead, cfg *config.City) bool {
	if cfg == nil || !cfg.Lifecycle.AdmissionEnabled {
		return false
	}
	return worklifecycle.EvaluateAdmission(bead, cfg.Lifecycle, "").Requested || lifecycleEnrollmentEvidence(bead)
}

// lifecycleEnrollmentEvidence persists enforcement after the operator removes
// the original intent label. Missing or damaged signed admission evidence
// then becomes a hold instead of silently opting the item back out.
func lifecycleEnrollmentEvidence(bead beads.Bead) bool {
	return worklifecycle.HasDurableEnrollment(bead)
}

func lifecycleProtectedWork(bead beads.Bead, cfg *config.City) bool {
	// Enrollment is durable. Turning off admission stops new work from joining
	// this controller-owned lane; it must not return already enrolled work to
	// legacy release/restart writers.
	if lifecycleEnrollmentEvidence(bead) {
		return true
	}
	if cfg == nil || !cfg.Lifecycle.AdmissionEnabled {
		return false
	}
	for _, label := range bead.Labels {
		if strings.EqualFold(strings.TrimSpace(label), worklifecycle.AdmissionIntentLabel) {
			return true
		}
	}
	return false
}

func filterHookLifecycleCandidates(candidates []beads.Bead, opts hookClaimOptions, stderr io.Writer) []beads.Bead {
	filtered := make([]beads.Bead, 0, len(candidates))
	for _, bead := range candidates {
		if !opts.Lifecycle.AdmissionEnabled {
			if !lifecycleEnrollmentEvidence(bead) {
				filtered = append(filtered, bead)
				continue
			}
			fmt.Fprintf(stderr, "gc hook --claim: holding lifecycle item %s: lifecycle admission is disabled; existing enrollment remains protected\n", bead.ID) //nolint:errcheck
			continue
		}
		if marker, ok := lifecycleMaterializationFor(bead); ok && marker.SourceID != "" && marker.SourceID != bead.ID {
			reason := "graph lineage does not match a trusted admission contract"
			switch {
			case !opts.TrustedLifecycleScope:
				reason = "lifecycle scope came from a custom work query"
			case !lifecycleLineageAdmissionMatches(opts.LifecycleCity, bead, marker):
				// Keep the contract verifier's fail-closed result.
			case !lifecycleLineageCurrent(bead, marker, opts):
				reason = "graph lineage, source admission, or workflow store could not be verified"
			default:
				filtered = append(filtered, bead)
				continue
			}
			fmt.Fprintf(stderr, "gc hook --claim: holding lifecycle descendant %s: %s\n", bead.ID, reason) //nolint:errcheck
			continue
		}
		decision := worklifecycle.EvaluateAdmission(bead, opts.Lifecycle, bead.LifecycleScope)
		if !decision.Requested && !lifecycleEnrollmentEvidence(bead) {
			filtered = append(filtered, bead)
			continue
		}
		reason := decision.Reason
		switch {
		case !decision.Requested:
			reason = "durable lifecycle enrollment has no verifiable admission receipt"
		case !opts.TrustedLifecycleScope:
			reason = "lifecycle scope came from a custom work query"
		case strings.TrimSpace(bead.LifecycleScope) == "":
			reason = "trusted lifecycle scope is missing"
		case !decision.Admitted:
			// Keep the receipt verifier's item-specific explanation.
		case !lifecycleCurrentSource(bead, opts):
			reason = "canonical source admission or current eligibility could not be verified"
		case !lifecycleAdmissionRouteMatches(opts.LifecycleCity, bead, bead.LifecycleScope):
			reason = "attached workflow or route does not match the signed admission contract"
		case !hookLifecycleRouteMatches(opts.LifecycleCity, decision.Receipt.Route, opts.RouteTargets):
			reason = "candidate route does not match the signed admission contract"
		default:
			filtered = append(filtered, bead)
			continue
		}
		fmt.Fprintf(stderr, "gc hook --claim: holding lifecycle item %s: %s\n", bead.ID, reason) //nolint:errcheck
	}
	return filtered
}

func lifecycleCurrentSource(candidate beads.Bead, opts hookClaimOptions) bool {
	if opts.ResolveLifecycleStore == nil || strings.TrimSpace(candidate.SourceStoreRef) == "" || strings.TrimSpace(candidate.LifecycleScope) == "" {
		return false
	}
	store, err := opts.ResolveLifecycleStore(candidate.SourceStoreRef)
	if err != nil || store == nil {
		return false
	}
	current, err := store.Get(candidate.ID)
	if err != nil {
		return false
	}
	current.SourceStoreRef = candidate.SourceStoreRef
	current.LifecycleScope = candidate.LifecycleScope
	decision := worklifecycle.EvaluateAdmission(current, opts.Lifecycle, current.LifecycleScope)
	status := strings.ToLower(strings.TrimSpace(current.Status))
	owner := strings.TrimSpace(current.Assignee)
	return decision.Admitted && lifecycleAdmissionRouteMatches(opts.LifecycleCity, current, current.LifecycleScope) &&
		(status == "open" || status == "in_progress") && !beads.IsDeferred(current, time.Now()) && !lifecycleRowHeld(current) &&
		(owner == "" || hookClaimHasIdentity(owner, opts.IdentityCandidates))
}

func lifecycleLineageCurrent(candidate beads.Bead, lineage lifecycleMaterialization, opts hookClaimOptions) bool {
	if opts.ResolveLifecycleStore == nil || !opts.TrustedLifecycleScope ||
		strings.TrimSpace(candidate.SourceStoreRef) == "" {
		return false
	}
	workflowStore, err := opts.ResolveLifecycleStore(lineage.WorkflowStoreRef)
	if err != nil || workflowStore == nil {
		return false
	}
	root, err := workflowStore.Get(lineage.WorkflowID)
	if err != nil || root.ID != lineage.WorkflowID || !sourceworkflow.IsWorkflowRoot(root) ||
		strings.TrimSpace(root.Metadata[beadmeta.FormulaContractMetadataKey]) != beadmeta.FormulaContractGraphV2 ||
		strings.TrimSpace(root.Metadata[beadmeta.FormulaNameMetadataKey]) != lineage.Workflow ||
		strings.TrimSpace(root.Metadata[beadmeta.RootStoreRefMetadataKey]) != lineage.SourceStoreRef ||
		strings.EqualFold(strings.TrimSpace(root.Status), "closed") {
		return false
	}
	rootLineage, ok := lifecycleMaterializationFor(root)
	if !ok || !sameLifecycleLineage(rootLineage, lineage) || rootLineage.State != "attached" {
		return false
	}
	if candidate.ID != lineage.WorkflowID && strings.TrimSpace(candidate.Metadata[beadmeta.RootBeadIDMetadataKey]) != lineage.WorkflowID {
		return false
	}
	candidateStore, err := opts.ResolveLifecycleStore(candidate.SourceStoreRef)
	if err != nil || candidateStore == nil {
		return false
	}
	currentCandidate, err := candidateStore.Get(candidate.ID)
	if err != nil || currentCandidate.ID != candidate.ID {
		return false
	}
	currentLineage, ok := lifecycleMaterializationFor(currentCandidate)
	if !ok || currentLineage.State != "attached" || !sameLifecycleLineage(currentLineage, lineage) ||
		currentCandidate.Metadata[beadmeta.RootStoreRefMetadataKey] != lineage.SourceStoreRef ||
		lifecycleRowHeld(currentCandidate) || beads.IsDeferred(currentCandidate, time.Now()) {
		return false
	}
	sourceStore, err := opts.ResolveLifecycleStore(lineage.SourceStoreRef)
	if err != nil || sourceStore == nil {
		return false
	}
	source, err := sourceStore.Get(lineage.SourceID)
	if err != nil {
		return false
	}
	source.LifecycleScope = lineage.Scope
	source.SourceStoreRef = lineage.SourceStoreRef
	decision := worklifecycle.EvaluateAdmission(source, opts.Lifecycle, lineage.Scope)
	digest, err := worklifecycle.AdmissionDigest(decision.Receipt)
	sourceStatus := strings.ToLower(strings.TrimSpace(source.Status))
	sourceOwner := strings.TrimSpace(source.Assignee)
	if err != nil || !decision.Admitted || digest != lineage.Contract ||
		!lifecycleAdmissionRouteMatches(opts.LifecycleCity, source, lineage.Scope) ||
		(sourceStatus != "open" && sourceStatus != "in_progress") ||
		(sourceOwner != "" && !hookClaimHasIdentity(sourceOwner, opts.IdentityCandidates)) ||
		beads.IsDeferred(source, time.Now()) || lifecycleRowHeld(source) {
		return false
	}
	attached, ok := lifecycleMaterializationFor(source)
	if !ok || attached.State != "attached" || !sameLifecycleLineage(attached, lineage) || attached.WorkflowID != lineage.WorkflowID {
		return false
	}
	inputConvoyID := strings.TrimSpace(root.Metadata[beadmeta.InputConvoyIDMetadataKey])
	if inputConvoyID == "" {
		return false
	}
	members, err := convoycore.Members(sourceStore, inputConvoyID, false)
	if err != nil || !slices.ContainsFunc(members, func(member beads.Bead) bool { return member.ID == lineage.SourceID }) {
		return false
	}
	return true
}

func sameLifecycleLineage(a, b lifecycleMaterialization) bool {
	return a.Version == b.Version && a.Scope == b.Scope && a.Contract == b.Contract && a.Route == b.Route &&
		a.Workflow == b.Workflow && a.MergeStrategy == b.MergeStrategy && a.Token == b.Token &&
		a.SourceID == b.SourceID && a.SourceStoreRef == b.SourceStoreRef &&
		a.WorkflowStoreRef == b.WorkflowStoreRef && a.AdmissionReceipt == b.AdmissionReceipt
}

func lifecycleRowHeld(row beads.Bead) bool {
	for _, label := range row.Labels {
		for _, hold := range beadmeta.DispatchHoldLabels {
			if strings.EqualFold(strings.TrimSpace(label), hold) {
				return true
			}
		}
	}
	return false
}

// lifecycleAuthoritativeCandidate reloads an enrolled work row from the exact
// store provenance emitted by gc ready and revalidates its admission or graph
// lineage against current source/root rows. The work-query object is only a
// locator; it never authorizes a claim.
func lifecycleAuthoritativeCandidate(candidate beads.Bead, opts hookClaimOptions) (beads.Store, beads.Bead, bool) {
	if !lifecycleEnrollmentEvidence(candidate) || !opts.Lifecycle.AdmissionEnabled ||
		!opts.TrustedLifecycleScope || opts.ResolveLifecycleStore == nil ||
		strings.TrimSpace(candidate.SourceStoreRef) == "" {
		return nil, beads.Bead{}, false
	}
	store, err := opts.ResolveLifecycleStore(candidate.SourceStoreRef)
	if err != nil || store == nil {
		return nil, beads.Bead{}, false
	}
	current, err := store.Get(candidate.ID)
	if err != nil || current.ID != candidate.ID {
		return nil, beads.Bead{}, false
	}
	current.SourceStoreRef = candidate.SourceStoreRef
	current.LifecycleScope = candidate.LifecycleScope
	if strings.TrimSpace(current.LifecycleScope) == "" {
		return nil, beads.Bead{}, false
	}
	if filtered := filterHookLifecycleCandidates([]beads.Bead{current}, opts, io.Discard); len(filtered) != 1 {
		return nil, beads.Bead{}, false
	}
	if lifecycleRowHeld(current) || beads.IsDeferred(current, time.Now()) {
		return nil, beads.Bead{}, false
	}
	status := strings.ToLower(strings.TrimSpace(current.Status))
	if status != "open" && status != "in_progress" {
		return nil, beads.Bead{}, false
	}
	return store, current, true
}

// lifecycleConditionalClaim is the only claim writer for enrolled work. It
// fences the exact row revision and re-reads both the work row and its signed
// source/root lineage after the write. Stores without real revision CAS hold
// lifecycle work; the legacy `bd update --claim` path is never a fallback.
func lifecycleConditionalClaim(candidate beads.Bead, actor string, readyAssignment bool, opts hookClaimOptions) (beads.Bead, bool, error) {
	store, current, ok := lifecycleAuthoritativeCandidate(candidate, opts)
	if !ok {
		return beads.Bead{}, false, nil
	}
	actor = strings.TrimSpace(actor)
	if actor == "" || !hookClaimHasIdentity(actor, opts.IdentityCandidates) || current.Revision <= 0 {
		return beads.Bead{}, false, nil
	}
	status := strings.ToLower(strings.TrimSpace(current.Status))
	if status != "open" {
		return beads.Bead{}, false, nil
	}
	owner := strings.TrimSpace(current.Assignee)
	if readyAssignment {
		if owner == "" || !hookClaimHasIdentity(owner, opts.IdentityCandidates) || owner != actor {
			return beads.Bead{}, false, nil
		}
	} else if owner != "" {
		return beads.Bead{}, false, nil
	}
	writer, supported := beads.ConditionalWriterFor(store)
	if !supported || !beads.InspectConditionalWrites(store).Capable {
		return beads.Bead{}, false, beads.ErrConditionalWriteUnsupported
	}
	if err := writer.UpdateIfMatch(current.ID, current.Revision, beads.UpdateOpts{
		Status:   stringPtr("in_progress"),
		Assignee: stringPtr(actor),
	}); err != nil {
		return beads.Bead{}, false, err
	}
	claimed, err := store.Get(current.ID)
	if err != nil {
		return beads.Bead{}, true, fmt.Errorf("lifecycle claim %s committed but readback failed: %w", current.ID, err)
	}
	claimed.SourceStoreRef = candidate.SourceStoreRef
	claimed.LifecycleScope = candidate.LifecycleScope
	if strings.TrimSpace(claimed.Assignee) != actor || !strings.EqualFold(strings.TrimSpace(claimed.Status), "in_progress") {
		return claimed, true, fmt.Errorf("lifecycle claim %s readback does not show in_progress owned by %s", current.ID, actor)
	}
	if _, _, stillEligible := lifecycleAuthoritativeCandidate(claimed, opts); !stillEligible {
		return claimed, true, fmt.Errorf("lifecycle claim %s lost current admission or lineage after its fenced write", current.ID)
	}
	return claimed, true, nil
}

func hookLifecycleRouteMatches(cfg *config.City, route string, candidates []string) bool {
	if cfg == nil || strings.TrimSpace(route) == "" {
		return false
	}
	permitted := agentutil.NormalizePoolRouteTarget(cfg, route)
	for _, target := range candidates {
		if agentutil.NormalizePoolRouteTarget(cfg, target) == permitted {
			return true
		}
	}
	return false
}
