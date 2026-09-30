package main

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/agentutil"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/formula"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/qualification"
	"github.com/gastownhall/gascity/internal/sling"
	"github.com/gastownhall/gascity/internal/storeref"
	"github.com/gastownhall/gascity/internal/suspensionstate"
	"github.com/gastownhall/gascity/internal/worklifecycle"
)

type lifecycleAdmissionPolicy struct {
	projection    worklifecycle.AdmissionPolicyProjectionV2
	target        config.Agent
	deps          sling.SlingDeps
	graphStoreRef string
}

// buildLifecycleAdmissionPolicy recomputes the Q54 policy from the exact
// signed route, current expanded config, current runtime suspension state,
// compiler provenance, and authoritative store placement. It does not mutate
// stores or load signing keys.
func buildLifecycleAdmissionPolicy(
	bead beads.Bead,
	receipt worklifecycle.AdmissionReceiptV2,
	scope, cityName, cityPath string,
	cfg *config.City,
	store beads.Store,
	rigStores map[string]beads.Store,
	leg classStoreCandidate,
	legs []classStoreCandidate,
	runner sling.SlingRunner,
	authority qualification.CompatibilityAuthority,
) (lifecycleAdmissionPolicy, error) {
	if cfg == nil {
		return lifecycleAdmissionPolicy{}, fmt.Errorf("authoritative city configuration is unavailable")
	}
	if formula.IsFormulaV2Enabled() != cfg.Daemon.FormulaV2Enabled() {
		return lifecycleAdmissionPolicy{}, fmt.Errorf("global FormulaV2 mode does not match the effective city configuration")
	}

	state, err := loadSuspensionState(fsys.OSFS{}, cityPath)
	if err != nil {
		return lifecycleAdmissionPolicy{}, fmt.Errorf("reading current runtime suspension evidence: %w", err)
	}
	strictSuspendedRigPaths := make(map[string]bool)
	for index := range cfg.Rigs {
		rig := &cfg.Rigs[index]
		if suspensionstate.EffectiveRigSuspended(state, rig.Name, rig.EffectiveSuspendedOnStart()) {
			strictSuspendedRigPaths[filepath.Clean(rig.Path)] = true
		}
	}
	rigName, _ := config.ParseQualifiedName(receipt.Route)
	var targetRig *config.Rig
	for index := range cfg.Rigs {
		if cfg.Rigs[index].Name != rigName {
			continue
		}
		if targetRig != nil {
			return lifecycleAdmissionPolicy{}, fmt.Errorf("target rig %q is ambiguous in current configuration", rigName)
		}
		targetRig = &cfg.Rigs[index]
	}
	if targetRig == nil {
		return lifecycleAdmissionPolicy{}, fmt.Errorf("target rig %q is unavailable in current configuration", rigName)
	}
	runtimeSuspended := suspensionstate.EffectiveRigSuspended(state, targetRig.Name, targetRig.EffectiveSuspendedOnStart())
	target, err := worklifecycle.ResolveCanonicalAdmissionPoolV2(receipt.Route, worklifecycle.AdmissionTargetResolutionContextV2{
		City: cfg, RuntimeRigSuspended: &runtimeSuspended,
	})
	if err != nil {
		return lifecycleAdmissionPolicy{}, fmt.Errorf("resolving exact signed route %q: %w", receipt.Route, err)
	}
	if target.DefaultSlingFormula != receipt.Workflow {
		return lifecycleAdmissionPolicy{}, fmt.Errorf("signed workflow %q does not match exact route %q configured workflow %q", receipt.Workflow, receipt.Route, target.DefaultSlingFormula)
	}
	var targetAgent *config.Agent
	for index := range cfg.Agents {
		if agentutil.RoutedToIdentity(&cfg.Agents[index]) != receipt.Route {
			continue
		}
		if targetAgent != nil {
			return lifecycleAdmissionPolicy{}, fmt.Errorf("exact signed route %q is ambiguous in current configuration", receipt.Route)
		}
		targetAgent = &cfg.Agents[index]
	}
	if targetAgent == nil {
		return lifecycleAdmissionPolicy{}, fmt.Errorf("exact signed route %q has no configured pool template", receipt.Route)
	}

	deps, graphStoreRef, err := lifecycleSlingDeps(cityName, cityPath, cfg, store, rigStores, strictSuspendedRigPaths, leg, legs, runner, authority)
	if err != nil {
		return lifecycleAdmissionPolicy{}, fmt.Errorf("resolving current graph/workflow stores: %w", err)
	}
	plan, err := resolveCityStoragePlan(cityPath, cfg)
	if err != nil {
		return lifecycleAdmissionPolicy{}, fmt.Errorf("resolving authoritative storage plan: %w", err)
	}
	planProof, err := worklifecycle.NewAdmissionResolvedStoragePlanV2(plan)
	if err != nil {
		return lifecycleAdmissionPolicy{}, fmt.Errorf("capturing authoritative storage plan: %w", err)
	}

	compileDeps := deps
	// Admission cannot use a live Git probe as a hidden formula input. Current
	// bead metadata and configured rig defaults remain reproducible inputs.
	compileDeps.Branches = nil
	vars := sling.BuildSlingFormulaVars(receipt.Workflow, bead.ID, nil, *targetAgent, compileDeps)
	if sling.SlingFormulaUsesBaseBranch(receipt.Workflow) && strings.TrimSpace(vars["base_branch"]) == "" {
		return lifecycleAdmissionPolicy{}, fmt.Errorf("workflow base_branch would require a live or generated invocation value")
	}
	if sling.SlingFormulaUsesTargetBranch(receipt.Workflow) && strings.TrimSpace(vars["target_branch"]) == "" {
		return lifecycleAdmissionPolicy{}, fmt.Errorf("workflow target_branch would require a live or generated invocation value")
	}
	recipe, provenance, err := formula.CompileWithoutRuntimeVarValidationWithProvenance(
		context.Background(), receipt.Workflow, sling.SlingFormulaSearchPaths(compileDeps, *targetAgent), vars,
	)
	if err != nil {
		return lifecycleAdmissionPolicy{}, fmt.Errorf("compiling current workflow with provenance: %w", err)
	}
	if provenance.Status != formula.CompileProvenanceAvailable {
		return lifecycleAdmissionPolicy{}, fmt.Errorf("workflow compiler provenance is incomplete: %v", provenance.UnavailableReasons)
	}
	if len(provenance.ExternalAssets) != 0 {
		return lifecycleAdmissionPolicy{}, fmt.Errorf("workflow description/external asset closure is not supported by admission evidence")
	}
	if len(provenance.CheckPaths) != 0 {
		return lifecycleAdmissionPolicy{}, fmt.Errorf("workflow check asset/dependency closure is not supported by admission evidence")
	}
	if recipe == nil || len(recipe.Steps) == 0 {
		return lifecycleAdmissionPolicy{}, fmt.Errorf("workflow compiler returned no root step")
	}
	if strings.EqualFold(strings.TrimSpace(recipe.Steps[0].Metadata[beadmeta.FormulaContractMetadataKey]), beadmeta.FormulaContractGraphV2) {
		return lifecycleAdmissionPolicy{}, fmt.Errorf("graph.v2 workflow invocation requires a generated input convoy and is not supported by current admission evidence")
	}
	if len(recipe.FormulaSources) == 0 || len(recipe.FormulaSources) != len(provenance.FormulaSources) {
		return lifecycleAdmissionPolicy{}, fmt.Errorf("workflow formula source closure is missing or inconsistent")
	}
	formulaSources, err := admissionFormulaSourceClosure(recipe.FormulaSources, provenance.FormulaSources)
	if err != nil {
		return lifecycleAdmissionPolicy{}, fmt.Errorf("workflow formula source identity/hash closure is incomplete: %w", err)
	}
	composedIDs := make([]string, 0)
	for _, entry := range provenance.Trace {
		switch entry.Kind {
		case formula.CompileTraceInheritance:
			// Inherited source bytes are already in FormulaSources.
		case formula.CompileTraceInlineExpansion, formula.CompileTraceComposeExpand,
			formula.CompileTraceComposeMap, formula.CompileTraceAspect, formula.CompileTraceStandaloneExpand:
			if strings.TrimSpace(entry.FormulaName) == "" {
				return lifecycleAdmissionPolicy{}, fmt.Errorf("workflow composition provenance has no logical formula identity")
			}
			composedIDs = append(composedIDs, entry.FormulaName)
		default:
			return lifecycleAdmissionPolicy{}, fmt.Errorf("workflow provenance contains an unsupported compiler trace kind %q", entry.Kind)
		}
	}
	formulaSchema := "formula.v1"
	storePlacement := worklifecycle.AdmissionStorePlacementV2{
		SourceStoreRef:        leg.ref,
		GraphPlacementMode:    worklifecycle.AdmissionGraphPlacementClass,
		ResolvedStoragePlan:   planProof,
		GraphStoreRef:         graphStoreRef,
		WorkflowPlacementMode: worklifecycle.AdmissionWorkflowPlacementSource,
		WorkflowStoreRef:      leg.ref,
	}
	if _, scoped := storeref.ScopeRigContext(leg.ref); scoped {
		storePlacement.GraphPlacementMode = worklifecycle.AdmissionGraphPlacementSourceRig
	}
	projection := worklifecycle.AdmissionPolicyProjectionV2{
		SourceScope:                          scope,
		RouteResolverVersion:                 worklifecycle.AdmissionRouteResolverV2Version,
		Target:                               target,
		Workflow:                             receipt.Workflow,
		FormulaSources:                       formulaSources,
		FormulaSourceCount:                   len(recipe.FormulaSources),
		ExternalAssets:                       []worklifecycle.AdmissionExternalAssetV2{},
		ExternalAssetCount:                   0,
		ExternalAssetClosureComplete:         true,
		CheckPathCount:                       0,
		CheckMappingsComplete:                true,
		CheckClosures:                        []worklifecycle.AdmissionCheckClosureV2{},
		FormulaCompilerCapability:            provenance.CompilerCapability,
		FormulaCompilerImplementationVersion: provenance.CompilerImplementationVersion,
		FormulaProvenanceSchemaVersion:       provenance.SchemaVersion,
		FormulaSchemaVersion:                 formulaSchema,
		FormulaV2Enabled:                     provenance.FormulaV2Enabled,
		EffectiveCompileVariables:            provenance.EffectiveCompileVariables,
		EffectiveComposedFormulaIDs:          composedIDs,
		MergeStrategy:                        receipt.MergeStrategy,
		StorePlacement:                       storePlacement,
	}
	if _, err := worklifecycle.DigestAdmissionPolicyV2(projection); err != nil {
		return lifecycleAdmissionPolicy{}, fmt.Errorf("current admission policy projection is incomplete: %w", err)
	}
	return lifecycleAdmissionPolicy{projection: projection, target: *targetAgent, deps: deps, graphStoreRef: graphStoreRef}, nil
}

func admissionFormulaSourceClosure(recipeSources, provenanceSources []formula.SourceIdentity) ([]worklifecycle.AdmissionFormulaSourceV2, error) {
	if len(recipeSources) == 0 || len(recipeSources) != len(provenanceSources) {
		return nil, fmt.Errorf("recipe/provenance formula source counts differ")
	}
	canonical := make(map[string]string, len(recipeSources))
	logical := make(map[string]string, len(recipeSources))
	for _, source := range recipeSources {
		if source.FormulaName == "" || source.ContentSHA256 == "" {
			return nil, fmt.Errorf("recipe source has no logical identity or content hash")
		}
		if prior, exists := logical[source.FormulaName]; exists {
			return nil, fmt.Errorf("recipe source logical identity %q is duplicated or ambiguous (%s, %s)", source.FormulaName, prior, source.ContentSHA256)
		}
		logical[source.FormulaName] = source.ContentSHA256
		canonical[source.FormulaName] = source.ContentSHA256
	}
	seenProvenance := make(map[string]string, len(provenanceSources))
	for _, source := range provenanceSources {
		if source.FormulaName == "" || source.ContentSHA256 == "" {
			return nil, fmt.Errorf("provenance source has no logical identity or content hash")
		}
		if _, exists := seenProvenance[source.FormulaName]; exists {
			return nil, fmt.Errorf("provenance source logical identity %q is duplicated or ambiguous", source.FormulaName)
		}
		seenProvenance[source.FormulaName] = source.ContentSHA256
	}
	if len(canonical) != len(seenProvenance) {
		return nil, fmt.Errorf("recipe/provenance source identities differ")
	}
	for name, hash := range canonical {
		if seenProvenance[name] != hash {
			return nil, fmt.Errorf("recipe/provenance source identity or hash differs for %q", name)
		}
	}
	result := make([]worklifecycle.AdmissionFormulaSourceV2, 0, len(recipeSources))
	for _, source := range recipeSources {
		result = append(result, worklifecycle.AdmissionFormulaSourceV2{LogicalID: source.FormulaName, SHA256: source.ContentSHA256})
	}
	return result, nil
}
