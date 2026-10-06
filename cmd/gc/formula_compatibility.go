package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/compatibility"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/molecule"
	"github.com/gastownhall/gascity/internal/qualification"
	"github.com/gastownhall/gascity/internal/storeref"
)

type formulaActionCandidate struct {
	Store    beads.Store
	Bead     beads.Bead
	Required bool
}

type formulaActionCandidateCheck func(context.Context, beads.Bead) (formulaActionCandidate, error)

func controllerFormulaActionCandidateCheck(cityPath string, cfg *config.City, customWorkQuery bool) formulaActionCandidateCheck {
	return func(ctx context.Context, candidate beads.Bead) (formulaActionCandidate, error) {
		return checkControllerFormulaActionWithCanonicalLookup(ctx, cityPath, cfg, candidate, customWorkQuery)
	}
}

// controllerFormulaActionGate supplies the source-aware gate to local CLI
// callers. They have no authority handle, so required formulas remain
// unavailable until a central verification path is available.
func controllerFormulaActionGate(cityPath string, cfg *config.City, storeRef string) molecule.FormulaActionGate {
	return controllerFormulaActionGateWithCurrent(cityPath, cfg, storeRef, nil, nil)
}

func controllerFormulaActionGateWithCurrent(
	cityPath string,
	cfg *config.City,
	storeRef string,
	authority qualification.CompatibilityAuthority,
	current func() (*config.City, qualification.Snapshot, qualification.BuildIdentity, qualification.CompatibilityAuthority, error),
) molecule.FormulaActionGate {
	cityID := loadedCityName(cfg, cityPath)
	serverID, _ := compatibility.ControllerScopeID(cityPath)
	gate := compatibility.NewMaterializationGate(cfg, cityID, serverID, storeRef, currentControllerBuildIdentity(), authority)
	gate.Current = current
	return gate
}

// formulaMaterializationStoreRef names the exact store that will receive a
// compiled formula. The caller supplies the same scope and graph stores used by
// moleculeClassStore; if opened topology cannot prove the class binding, the
// empty result intentionally leaves required compatibility unavailable.
func formulaMaterializationStoreRef(cityPath string, cfg *config.City, scopePath string, scopeStore, graphStore, targetStore beads.Store) string {
	if targetStore == nil {
		return ""
	}
	if targetStore == scopeStore {
		return workflowStoreRefForDir(scopePath, cityPath, loadedCityName(cfg, cityPath), cfg)
	}
	if targetStore != graphStore || cfg == nil {
		return ""
	}
	topology := residencyTopologyForCity(cityPath, cfg, scopeStore, nil)
	plan, err := storeref.Plan(storeref.Class{C: coordclass.ClassGraph}, topology)
	if err != nil {
		return ""
	}
	leg, err := storeref.ResolvePlacement(plan)
	if err != nil || leg.Store != targetStore {
		return ""
	}
	return censusRef(cfg, leg.Ref, censusRefScoped)
}

func controllerFormulaActionGateForStore(cityPath string, cfg *config.City, scopePath string, scopeStore, graphStore beads.Store) func(beads.Store) molecule.FormulaActionGate {
	return controllerFormulaActionGateForStoreWithCurrent(cityPath, cfg, scopePath, scopeStore, graphStore, nil, nil)
}

func controllerFormulaActionGateForStoreWithCurrent(
	cityPath string,
	cfg *config.City,
	scopePath string,
	scopeStore, graphStore beads.Store,
	authority qualification.CompatibilityAuthority,
	current func() (*config.City, qualification.Snapshot, qualification.BuildIdentity, qualification.CompatibilityAuthority, error),
) func(beads.Store) molecule.FormulaActionGate {
	return func(targetStore beads.Store) molecule.FormulaActionGate {
		storeRef := formulaMaterializationStoreRef(cityPath, cfg, scopePath, scopeStore, graphStore, targetStore)
		return controllerFormulaActionGateWithCurrent(cityPath, cfg, storeRef, authority, current)
	}
}

// graphMaterializationStoreRef resolves the graph-class destination used by a
// controller dispatcher. It follows the same opened topology as other class
// routes instead of deriving a store identity from its directory name.
func graphMaterializationStoreRef(cityPath, scopePath string, cfg *config.City, scopeStore, graphStore beads.Store) string {
	return formulaMaterializationStoreRef(cityPath, cfg, scopePath, scopeStore, graphStore, graphStore)
}

// checkControllerFormulaAction resolves a candidate through the exact store
// provenance emitted by the controller's work query, then validates any
// required formula authorization against the canonical row. Work-query JSON is
// only a locator; it never authorizes a worker to see or claim controlled work.
func checkControllerFormulaAction(ctx context.Context, cityPath string, cfg *config.City, candidate beads.Bead) (formulaActionCandidate, error) {
	return checkControllerFormulaActionWithCanonicalLookup(ctx, cityPath, cfg, candidate, false)
}

// checkControllerFormulaActionWithCanonicalLookup treats a custom work-query
// result as a locator even when its projection omits formula metadata. The
// exact source store and bead ID are enough to read canonical provenance; query
// metadata must not be required as a second, untrusted authority source.
func checkControllerFormulaActionWithCanonicalLookup(ctx context.Context, cityPath string, cfg *config.City, candidate beads.Bead, forceCanonicalLookup bool) (formulaActionCandidate, error) {
	state := formulaActionCandidate{Bead: candidate}
	storeRef := strings.TrimSpace(candidate.SourceStoreRef)
	queryHasCompatibilityMetadata := strings.TrimSpace(candidate.Metadata[beadmeta.CompatibilityRequestMetadataKey]) != "" ||
		strings.TrimSpace(candidate.Metadata[beadmeta.CompatibilityAuthorizationMetadataKey]) != "" ||
		strings.TrimSpace(candidate.Metadata[beadmeta.FormulaSourceMetadataKey]) != "" ||
		strings.TrimSpace(candidate.Metadata[beadmeta.RootBeadIDMetadataKey]) != ""
	if !forceCanonicalLookup && !queryHasCompatibilityMetadata && (cfg == nil || !cfg.HasRequiredCompatibilityPacks()) {
		return state, nil
	}
	if cfg == nil {
		return state, fmt.Errorf("loaded city config is unavailable for formula action %s: %w", candidate.ID, qualification.ErrUnavailable)
	}
	if storeRef == "" {
		return state, fmt.Errorf("formula action %s has no exact store reference: %w", candidate.ID, qualification.ErrUnavailable)
	}
	store, err := lifecycleStoreForRef(cityPath, cfg, storeRef)
	if err != nil || store == nil {
		return state, fmt.Errorf("resolving formula action store %q: %w", storeRef, qualification.ErrUnavailable)
	}
	canonical, err := store.Get(candidate.ID)
	if err != nil || canonical.ID != candidate.ID {
		return state, fmt.Errorf("reading canonical formula action %s: %w", candidate.ID, qualification.ErrUnavailable)
	}
	if candidate.Revision != 0 && canonical.Revision != candidate.Revision {
		return state, fmt.Errorf("formula action %s changed after discovery: %w", candidate.ID, qualification.ErrUnavailable)
	}
	canonical.SourceStoreRef = storeRef
	state.Bead = canonical

	// Work-query rows are locators, not provenance. In a city with a declared
	// compatibility pack every candidate is resolved above, so omitted metadata
	// cannot turn a guarded formula into an ordinary claim. Read source and
	// authorization only from the canonical row.
	request := strings.TrimSpace(canonical.Metadata[beadmeta.CompatibilityRequestMetadataKey])
	authorization := strings.TrimSpace(canonical.Metadata[beadmeta.CompatibilityAuthorizationMetadataKey])
	source := strings.TrimSpace(canonical.Metadata[beadmeta.FormulaSourceMetadataKey])
	rootID := strings.TrimSpace(canonical.Metadata[beadmeta.RootBeadIDMetadataKey])
	if source == "" && rootID != "" &&
		(cfg.HasRequiredCompatibilityPacks() || request != "" || authorization != "") {
		// RootStoreRef names the store that owns the workflow root. A graph can
		// place its children in a different class store, so it must not be
		// compared to the candidate's source store or used as its fallback.
		rootStoreRef := strings.TrimSpace(canonical.Metadata[beadmeta.RootStoreRefMetadataKey])
		if rootStoreRef == "" {
			rootStoreRef = storeRef
		}
		rootStore, err := lifecycleStoreForRef(cityPath, cfg, rootStoreRef)
		if err != nil || rootStore == nil {
			return state, fmt.Errorf("resolving formula action root store %q: %w", rootStoreRef, qualification.ErrUnavailable)
		}
		root, err := rootStore.Get(rootID)
		if err != nil || root.ID != rootID {
			return state, fmt.Errorf("reading formula action root %s: %w", rootID, qualification.ErrUnavailable)
		}
		source = strings.TrimSpace(root.Metadata[beadmeta.FormulaSourceMetadataKey])
	}

	required := request != "" || authorization != ""
	if source != "" {
		_, sourceRequired, err := cfg.RequiredCompatibilityPacks([]string{source})
		if err != nil {
			return state, fmt.Errorf("resolving required formula source %q: %w", source, qualification.ErrUnavailable)
		}
		required = required || sourceRequired
	}
	if !required {
		return state, nil
	}
	if request == "" || authorization == "" {
		return state, fmt.Errorf("formula action %s has incomplete compatibility approval metadata: %w", candidate.ID, qualification.ErrUnavailable)
	}
	gate := controllerFormulaActionGate(cityPath, cfg, storeRef)
	validator, ok := gate.(interface {
		RevalidateBead(context.Context, beads.Bead, beads.Store) error
	})
	if !ok {
		return state, fmt.Errorf("formula compatibility validator is unavailable: %w", qualification.ErrUnavailable)
	}
	if err := validator.RevalidateBead(ctx, canonical, store); err != nil {
		return state, fmt.Errorf("validating formula action %s: %w", candidate.ID, err)
	}
	return formulaActionCandidate{Store: store, Bead: canonical, Required: true}, nil
}
