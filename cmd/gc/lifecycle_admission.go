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
	legs, err := routedWorkStoreCandidates(cityPath, cfg, store, rigStores, suspendedRigPaths, censusRefScoped)
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
			token, err := lifecycleReservationToken()
			if err != nil {
				fmt.Fprintf(stderr, "lifecycle admission: creating materialization reservation token for %s: %v\n", bead.ID, err) //nolint:errcheck
				continue
			}
			reservationValue, err := encodeLifecycleMaterialization(lifecycleMaterialization{
				Version:       1,
				State:         "reserved",
				Scope:         scope,
				Contract:      digest,
				Route:         route,
				Workflow:      admission.Receipt.Workflow,
				MergeStrategy: admission.Receipt.MergeStrategy,
				Token:         token,
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
			deps, err := lifecycleSlingDeps(cityName, cityPath, cfg, store, rigStores, suspendedRigPaths, leg, legs, runner)
			if err != nil {
				fmt.Fprintf(stderr, "lifecycle admission: preparing workflow materialization for %s: %v\n", bead.ID, err) //nolint:errcheck
				continue
			}
			result, err := sling.DoSling(sling.SlingOpts{
				Target:               *agentCfg,
				BeadOrFormula:        bead.ID,
				Merge:                admission.Receipt.MergeStrategy,
				RequireFormulaAttach: true,
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
			if !lifecycleSlingResultMatches(current, leg.store, deps.GraphStore, cfg, scope, digest, route, admission.Receipt.Workflow, admission.Receipt.MergeStrategy, workflowID) {
				fmt.Fprintf(stderr, "lifecycle admission: sling result did not leave the signed workflow and route visible for %s; work remains held\n", bead.ID) //nolint:errcheck
				continue
			}
			attachedValue, err := encodeLifecycleMaterialization(lifecycleMaterialization{
				Version:       1,
				State:         "attached",
				Scope:         scope,
				Contract:      digest,
				Route:         route,
				Workflow:      admission.Receipt.Workflow,
				MergeStrategy: admission.Receipt.MergeStrategy,
				Token:         token,
				WorkflowID:    workflowID,
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
	Version       int    `json:"version"`
	State         string `json:"state"`
	Scope         string `json:"scope"`
	Contract      string `json:"contract"`
	Route         string `json:"route"`
	Workflow      string `json:"workflow"`
	MergeStrategy string `json:"merge_strategy"`
	Token         string `json:"token"`
	WorkflowID    string `json:"workflow_id,omitempty"`
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

func lifecycleSlingResultMatches(bead beads.Bead, workStore, graphStore beads.Store, cfg *config.City, scope, digest, route, workflow, merge, workflowID string) bool {
	decision := worklifecycle.EvaluateAdmission(bead, cfg.Lifecycle, scope)
	currentDigest, err := worklifecycle.AdmissionDigest(decision.Receipt)
	if err != nil || !decision.Admitted || currentDigest != digest || strings.TrimSpace(bead.Metadata[beadmeta.MergeStrategyMetadataKey]) != merge {
		return false
	}
	matchedRoute := false
	for _, candidate := range controllerDemandRouteCandidates(bead) {
		if agentutil.NormalizePoolRouteTarget(cfg, candidate) == route {
			matchedRoute = true
			break
		}
	}
	if !matchedRoute || strings.TrimSpace(workflowID) == "" {
		return false
	}
	root, err := graphStore.Get(workflowID)
	if err != nil {
		root, err = workStore.Get(workflowID)
	}
	if err != nil || strings.TrimSpace(root.Metadata[beadmeta.FormulaNameMetadataKey]) != workflow || strings.EqualFold(strings.TrimSpace(root.Status), "closed") {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(root.Metadata[beadmeta.FormulaContractMetadataKey]), beadmeta.FormulaContractGraphV2) {
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
		strings.TrimSpace(marker.Token) != "" && strings.TrimSpace(marker.WorkflowID) != ""
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
) (sling.SlingDeps, error) {
	work := censusWorkLeg(cityPath, store)
	topology := residencyTopologyForCity(cityPath, cfg, work, servingRigStores(cfg, rigStores, suspendedRigPaths))
	graphPlan, err := storeref.Plan(storeref.Class{C: coordclass.ClassGraph}, topology)
	if err != nil {
		return sling.SlingDeps{}, fmt.Errorf("resolving graph workflow store: %w", err)
	}
	var graphStore beads.Store
	storeref.EachLeg(graphPlan, func(leg storeref.Leg, _ storeref.Role, _ storeref.ErrPolicy) {
		if graphStore == nil {
			graphStore = leg.Store
		}
	})
	if graphStore == nil {
		return sling.SlingDeps{}, fmt.Errorf("graph workflow store is unavailable")
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
	return deps, nil
}

// lifecycleAdmissionRouteMatches reports whether a work item whose lifecycle
// enrollment has been verified still carries the route the trusted receipt
// authorized. It deliberately does not repair a conflicting pre-existing
// route; an operator must resolve that ambiguity.
func lifecycleAdmissionRouteMatches(cfg *config.City, bead beads.Bead, scope string) bool {
	if cfg == nil || !cfg.Lifecycle.AdmissionEnabled {
		return true
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
	for _, current := range controllerDemandRouteCandidates(bead) {
		if agentutil.NormalizePoolRouteTarget(cfg, current) == permitted {
			return true
		}
	}
	return false
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
	for _, key := range []string{
		beadmeta.LifecycleAdmissionReceiptMetadataKey,
		beadmeta.LifecycleMaterializationMetadataKey,
		beadmeta.LifecycleCompletionReceiptMetadataKey,
		beadmeta.LifecycleRecoveryStateMetadataKey,
	} {
		if strings.TrimSpace(bead.Metadata[key]) != "" {
			return true
		}
	}
	return false
}

func lifecycleProtectedWork(bead beads.Bead, cfg *config.City) bool {
	if cfg == nil || !cfg.Lifecycle.AdmissionEnabled {
		return false
	}
	if lifecycleEnrollmentEvidence(bead) {
		return true
	}
	for _, label := range bead.Labels {
		if strings.EqualFold(strings.TrimSpace(label), worklifecycle.AdmissionIntentLabel) {
			return true
		}
	}
	return false
}

func filterHookLifecycleCandidates(candidates []beads.Bead, opts hookClaimOptions, stderr io.Writer) []beads.Bead {
	if !opts.Lifecycle.AdmissionEnabled {
		return candidates
	}
	filtered := make([]beads.Bead, 0, len(candidates))
	for _, bead := range candidates {
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
