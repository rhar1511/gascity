package worklifecycle

import (
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/agentutil"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/formula"
	"github.com/gastownhall/gascity/internal/qualification"
	"github.com/gastownhall/gascity/internal/storebinding"
	"github.com/gastownhall/gascity/internal/storeref"
)

const (
	// AdmissionPolicyProjectionV2Version versions the canonical input schema
	// hashed by DigestAdmissionPolicyV2. Change it when the projected fields or
	// their meaning changes.
	AdmissionPolicyProjectionV2Version = 3

	admissionPolicyProjectionV2Domain = "gascity.lifecycle.routing-formula-policy.v2\n"
	// AdmissionRouteResolverV2Version identifies the exact canonical-target
	// matching and legacy-alias rejection rules projected into Q54 policy.
	AdmissionRouteResolverV2Version = "exact-rig-pool-v2"

	// AdmissionGraphPlacementSourceRig records that graph workflow placement
	// follows the source rig store.
	AdmissionGraphPlacementSourceRig = "source-rig"
	// AdmissionGraphPlacementClass records that graph workflow placement is
	// selected by a named graph class binding.
	AdmissionGraphPlacementClass = "graph-class"
	// AdmissionWorkflowPlacementSource records that the workflow is stored
	// beside its source work item.
	AdmissionWorkflowPlacementSource = "source"
	// AdmissionWorkflowPlacementGraph records that the workflow is stored in
	// the selected graph store.
	AdmissionWorkflowPlacementGraph = "graph"
)

var errAdmissionPolicyV2Invalid = errors.New("admission policy v2 input is invalid")

// AdmissionTargetResolutionContextV2 contains the authoritative expanded city
// configuration and the caller's effective runtime suspension fact. A nil
// RuntimeRigSuspended means that runtime state is unknown and fails closed.
type AdmissionTargetResolutionContextV2 struct {
	City                *config.City
	RuntimeRigSuspended *bool
}

// CanonicalAdmissionPoolV2 is the resolved, rig-qualified pool identity and
// the configuration and runtime facts that allow it to receive generic
// admitted work, including its exact effective default sling formula. It
// contains no filesystem paths.
type CanonicalAdmissionPoolV2 struct {
	Identity                   string `json:"identity"`
	DefaultSlingFormula        string `json:"default_sling_formula"`
	PoolTemplate               bool   `json:"pool_template"`
	Suspended                  bool   `json:"suspended"`
	SupportsGenericEphemeral   bool   `json:"supports_generic_ephemeral"`
	CustomSlingQueryAbsent     bool   `json:"custom_sling_query_absent"`
	AgentMaxActiveSessions     *int   `json:"agent_max_active_sessions,omitempty"`
	RigMaxActiveSessions       *int   `json:"rig_max_active_sessions,omitempty"`
	WorkspaceMaxActiveSessions *int   `json:"workspace_max_active_sessions,omitempty"`
	MaxActiveSessions          *int   `json:"max_active_sessions,omitempty"`
	InheritedMaxActiveSessions int    `json:"inherited_max_active_sessions"`
	InheritedMaxSource         string `json:"inherited_max_source"`
	MinActiveSessions          int    `json:"min_active_sessions"`
	ConfigRigSuspendedOnStart  bool   `json:"config_rig_suspended_on_start"`
	RuntimeRigSuspensionKnown  bool   `json:"runtime_rig_suspension_known"`
	RuntimeRigSuspended        bool   `json:"runtime_rig_suspended"`
}

// ResolveCanonicalAdmissionPoolV2 accepts only an exact canonical identity
// for one configured rig-scoped pool template. It does not normalize
// pool slots or resolve migration-era bound/unbound aliases.
func ResolveCanonicalAdmissionPoolV2(identity string, context AdmissionTargetResolutionContextV2) (CanonicalAdmissionPoolV2, error) {
	if identity == "" || strings.TrimSpace(identity) != identity || !isRigQualifiedAdmissionIdentity(identity) {
		return CanonicalAdmissionPoolV2{}, admissionPolicyV2Error("target identity is not a canonical rig-qualified identity")
	}
	if context.City == nil {
		return CanonicalAdmissionPoolV2{}, admissionPolicyV2Error("authoritative city configuration is missing")
	}
	if context.RuntimeRigSuspended == nil {
		return CanonicalAdmissionPoolV2{}, admissionPolicyV2Error("effective runtime rig suspension is unknown")
	}
	if *context.RuntimeRigSuspended {
		return CanonicalAdmissionPoolV2{}, admissionPolicyV2Error("target rig is runtime-suspended")
	}
	identityRig, _ := config.ParseQualifiedName(identity)
	var rig *config.Rig
	for index := range context.City.Rigs {
		candidate := &context.City.Rigs[index]
		if candidate.Name != identityRig {
			continue
		}
		if rig != nil {
			return CanonicalAdmissionPoolV2{}, admissionPolicyV2Error("target rig configuration is ambiguous")
		}
		rig = candidate
	}
	if rig == nil {
		return CanonicalAdmissionPoolV2{}, admissionPolicyV2Error("target rig is missing from authoritative city configuration")
	}
	agents := context.City.Agents
	if isAdmissionPoolSlotSuffix(identity, agents) {
		return CanonicalAdmissionPoolV2{}, admissionPolicyV2Error("target identity names a pool slot, not its base template")
	}

	var matches []config.Agent
	for i := range agents {
		agent := agents[i]
		canonical := agentutil.RoutedToIdentity(&agent)
		if canonical == "" {
			continue
		}
		if admissionLegacyAliasMatches(&agent, identity) {
			return CanonicalAdmissionPoolV2{}, admissionPolicyV2Error("target identity is a legacy bound/unbound alias")
		}
		if canonical == identity {
			matches = append(matches, agent)
		}
	}
	if len(matches) != 1 {
		return CanonicalAdmissionPoolV2{}, admissionPolicyV2Error("target identity is missing or ambiguous")
	}

	agent := matches[0]
	canonical := agentutil.RoutedToIdentity(&agent)
	if agent.PoolName != "" || canonical != agent.QualifiedName() || agent.Dir == "" || !isAdmissionPoolTemplate(&agent) {
		return CanonicalAdmissionPoolV2{}, admissionPolicyV2Error("target is not a canonical rig-scoped pool template")
	}
	if agent.Suspended {
		return CanonicalAdmissionPoolV2{}, admissionPolicyV2Error("target pool is suspended")
	}
	if agent.MinActiveSessions != nil && *agent.MinActiveSessions < 0 {
		return CanonicalAdmissionPoolV2{}, admissionPolicyV2Error("target pool has an invalid minimum session capacity")
	}
	if !validAdmissionCapacity(agent.MaxActiveSessions) || !validAdmissionCapacity(rig.MaxActiveSessions) ||
		!validAdmissionCapacity(context.City.Workspace.MaxActiveSessions) {
		return CanonicalAdmissionPoolV2{}, admissionPolicyV2Error("target pool has invalid session capacity bounds")
	}
	max, maxSource := inheritedAdmissionCapacity(agent.MaxActiveSessions, rig.MaxActiveSessions, context.City.Workspace.MaxActiveSessions)
	min := agent.EffectiveMinActiveSessions()
	supportsGenericEphemeral := agent.SupportsGenericEphemeralSessions() && max != 0 &&
		!admissionAncestorCapacityIsZero(rig.MaxActiveSessions, context.City.Workspace.MaxActiveSessions)
	if !supportsGenericEphemeral ||
		(max >= 0 && min > max) {
		return CanonicalAdmissionPoolV2{}, admissionPolicyV2Error("target pool does not support generic ephemeral sessions within its inherited capacity")
	}
	customSlingQueryAbsent := normalizeAdmissionSlingQuery(agent.SlingQuery) == "" ||
		normalizeAdmissionSlingQuery(agent.SlingQuery) == normalizeAdmissionSlingQuery(agent.DefaultSlingQuery())
	if !customSlingQueryAbsent {
		return CanonicalAdmissionPoolV2{}, admissionPolicyV2Error("target pool has a custom sling query")
	}
	defaultSlingFormula := agent.EffectiveDefaultSlingFormula()
	if !validLogicalFormulaID(defaultSlingFormula) {
		return CanonicalAdmissionPoolV2{}, admissionPolicyV2Error("target pool has no canonical effective default sling formula")
	}
	var maxActiveSessions *int
	if max != -1 {
		maxValue := max
		maxActiveSessions = &maxValue
	}
	agentMax := cloneAdmissionInt(agent.MaxActiveSessions)
	rigMax := cloneAdmissionInt(rig.MaxActiveSessions)
	workspaceMax := cloneAdmissionInt(context.City.Workspace.MaxActiveSessions)
	return CanonicalAdmissionPoolV2{
		Identity:                   canonical,
		DefaultSlingFormula:        defaultSlingFormula,
		PoolTemplate:               true,
		Suspended:                  agent.Suspended,
		SupportsGenericEphemeral:   supportsGenericEphemeral,
		CustomSlingQueryAbsent:     true,
		AgentMaxActiveSessions:     agentMax,
		RigMaxActiveSessions:       rigMax,
		WorkspaceMaxActiveSessions: workspaceMax,
		MaxActiveSessions:          maxActiveSessions,
		InheritedMaxActiveSessions: max,
		InheritedMaxSource:         maxSource,
		MinActiveSessions:          min,
		ConfigRigSuspendedOnStart:  rig.EffectiveSuspendedOnStart(),
		RuntimeRigSuspensionKnown:  context.RuntimeRigSuspended != nil,
		RuntimeRigSuspended:        *context.RuntimeRigSuspended,
	}, nil
}

func inheritedAdmissionCapacity(agent, rig, workspace *int) (int, string) {
	for _, candidate := range []struct {
		name  string
		value *int
	}{{"agent", agent}, {"rig", rig}, {"workspace", workspace}} {
		if candidate.value != nil {
			return *candidate.value, candidate.name
		}
	}
	return -1, "unlimited"
}

func admissionAncestorCapacityIsZero(rig, workspace *int) bool {
	return (rig != nil && *rig == 0) || (workspace != nil && *workspace == 0)
}

func validAdmissionCapacity(value *int) bool {
	return value == nil || *value >= -1
}

func cloneAdmissionInt(value *int) *int {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func normalizeAdmissionSlingQuery(value string) string {
	return strings.Join(strings.Fields(value), " ")
}

// AdmissionFormulaSourceV2 identifies one formula source by its stable
// logical formula name and SHA-256 of its loaded bytes. LogicalID must not be a
// process-local source path.
type AdmissionFormulaSourceV2 struct {
	LogicalID string `json:"logical_id"`
	SHA256    string `json:"sha256"`
}

// AdmissionExternalAssetV2 identifies one loaded check script or external
// dependency by a stable logical name and the hash of its bytes. It never
// carries a local check_path.
type AdmissionExternalAssetV2 struct {
	LogicalID string `json:"logical_id"`
	SHA256    string `json:"sha256"`
}

// AdmissionCheckClosureV2 maps one compiled step's check script to the full
// logical dependency closure the caller resolved. Completeness and count
// fields make unavailable check_path or dependency mappings fail closed.
type AdmissionCheckClosureV2 struct {
	StepID               string   `json:"step_id"`
	CheckAssetLogicalID  string   `json:"check_asset_logical_id"`
	DependencyLogicalIDs []string `json:"dependency_logical_ids"`
	DependencyCount      int      `json:"dependency_count"`
	DependenciesComplete bool     `json:"dependencies_complete"`
}

// AdmissionResolvedStoragePlanV2 is created only from a resolved
// storebinding.StoragePlan. Its private fields prevent callers from asserting
// an arbitrary graph binding name or digest in a policy projection.
type AdmissionResolvedStoragePlanV2 struct {
	planSHA256         string
	graphBinding       string
	graphBindingSHA256 string
	graphClassStoreRef string
}

// NewAdmissionResolvedStoragePlanV2 captures the frozen plan digest, its
// ClassGraph binding, and that binding's configuration digest.
func NewAdmissionResolvedStoragePlanV2(plan *storebinding.StoragePlan) (AdmissionResolvedStoragePlanV2, error) {
	if plan == nil {
		return AdmissionResolvedStoragePlanV2{}, admissionPolicyV2Error("resolved storage plan is missing")
	}
	binding, assigned := plan.BindingFor(coordclass.ClassGraph)
	if !assigned || !validCanonicalText(string(binding)) {
		return AdmissionResolvedStoragePlanV2{}, admissionPolicyV2Error("resolved storage plan has no ClassGraph binding")
	}
	bindingDigest, exists := plan.BindingConfigDigests()[binding]
	var graphClassStoreRef string
	if binding != storebinding.ReservedWorkBinding {
		var classes []coordclass.Class
		for class, assigned := range plan.Assignments() {
			if assigned == binding {
				classes = append(classes, class)
			}
		}
		graphClassStoreRef = string(storeref.ClassRef(classes))
	}
	proof := AdmissionResolvedStoragePlanV2{
		planSHA256:         strings.TrimPrefix(string(plan.ConfigDigest()), "sha256:"),
		graphBinding:       string(binding),
		graphBindingSHA256: strings.TrimPrefix(string(bindingDigest), "sha256:"),
		graphClassStoreRef: graphClassStoreRef,
	}
	if !exists || !validSHA256Hex(proof.planSHA256) || !validSHA256Hex(proof.graphBindingSHA256) {
		return AdmissionResolvedStoragePlanV2{}, admissionPolicyV2Error("resolved storage plan binding or digest is incomplete")
	}
	return proof, nil
}

// MarshalJSON retains the opaque caller proof in the canonical projection.
func (proof AdmissionResolvedStoragePlanV2) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		PlanSHA256         string `json:"plan_sha256"`
		GraphBinding       string `json:"graph_binding"`
		GraphBindingSHA256 string `json:"graph_binding_sha256"`
		GraphClassStoreRef string `json:"graph_class_store_ref"`
	}{proof.planSHA256, proof.graphBinding, proof.graphBindingSHA256, proof.graphClassStoreRef})
}

// AdmissionStorePlacementV2 captures the stable store references and
// selection facts used when a workflow is materialized.
type AdmissionStorePlacementV2 struct {
	SourceStoreRef        string                         `json:"source_store_ref"`
	GraphPlacementMode    string                         `json:"graph_placement_mode"`
	ResolvedStoragePlan   AdmissionResolvedStoragePlanV2 `json:"resolved_storage_plan"`
	GraphStoreRef         string                         `json:"graph_store_ref"`
	WorkflowPlacementMode string                         `json:"workflow_placement_mode"`
	WorkflowStoreRef      string                         `json:"workflow_store_ref"`
}

// AdmissionPolicyProjectionV2 is the complete, caller-resolved input to the
// versioned route/formula policy digest. FormulaSources must include the
// effective workflow and every inherited or composed formula source. Their
// LogicalID values come from stable formula identities, never source paths;
// FormulaSourceCount must equal the source count from the compiled recipe.
// ExternalAssets must include every check script and external asset whose
// bytes affect the compiled recipe. CheckPathCount must equal the compiled
// recipe's check_path count, and CheckClosures must map each one to its
// complete external dependency set. A caller unable to resolve a check_path
// or dependency must set the matching completeness flag false; the digest
// rejects it.
type AdmissionPolicyProjectionV2 struct {
	SourceScope                          string                     `json:"source_scope"`
	RouteResolverVersion                 string                     `json:"route_resolver_version"`
	Target                               CanonicalAdmissionPoolV2   `json:"target"`
	Workflow                             string                     `json:"workflow"`
	FormulaSources                       []AdmissionFormulaSourceV2 `json:"formula_sources"`
	FormulaSourceCount                   int                        `json:"formula_source_count"`
	ExternalAssets                       []AdmissionExternalAssetV2 `json:"external_assets"`
	ExternalAssetCount                   int                        `json:"external_asset_count"`
	ExternalAssetClosureComplete         bool                       `json:"external_asset_closure_complete"`
	CheckPathCount                       int                        `json:"check_path_count"`
	CheckMappingsComplete                bool                       `json:"check_mappings_complete"`
	CheckClosures                        []AdmissionCheckClosureV2  `json:"check_closures"`
	FormulaCompilerCapability            string                     `json:"formula_compiler_capability"`
	FormulaCompilerImplementationVersion string                     `json:"formula_compiler_implementation_version"`
	FormulaProvenanceSchemaVersion       int                        `json:"formula_provenance_schema_version"`
	FormulaSchemaVersion                 string                     `json:"formula_schema_version"`
	FormulaV2Enabled                     bool                       `json:"formula_v2_enabled"`
	EffectiveCompileVariables            map[string]string          `json:"effective_compile_variables"`
	EffectiveComposedFormulaIDs          []string                   `json:"effective_composed_formula_ids"`
	MergeStrategy                        string                     `json:"merge_strategy"`
	StorePlacement                       AdmissionStorePlacementV2  `json:"store_placement"`
}

// DigestAdmissionPolicyV2 validates and hashes a canonical projection of the
// reviewed source scope, exact target, formula closure, compilation inputs,
// merge behavior, and resolved store placement. Source closure order and map
// insertion order do not affect the digest; composition order is retained
// because it can change the compiled recipe.
func DigestAdmissionPolicyV2(input AdmissionPolicyProjectionV2) (string, error) {
	canonical, err := canonicalAdmissionPolicyProjectionV2(input)
	if err != nil {
		return "", err
	}
	return qualification.DigestJSON(struct {
		Domain            string                      `json:"domain"`
		ProjectionVersion int                         `json:"projection_version"`
		Projection        AdmissionPolicyProjectionV2 `json:"projection"`
	}{
		Domain:            admissionPolicyProjectionV2Domain,
		ProjectionVersion: AdmissionPolicyProjectionV2Version,
		Projection:        canonical,
	})
}

func canonicalAdmissionPolicyProjectionV2(input AdmissionPolicyProjectionV2) (AdmissionPolicyProjectionV2, error) {
	if !validCanonicalText(input.SourceScope) {
		return AdmissionPolicyProjectionV2{}, admissionPolicyV2Error("source scope is missing or non-canonical")
	}
	if input.RouteResolverVersion != AdmissionRouteResolverV2Version {
		return AdmissionPolicyProjectionV2{}, admissionPolicyV2Error("route resolver version is missing or unsupported")
	}
	if !isRigQualifiedAdmissionIdentity(input.Target.Identity) || !validLogicalFormulaID(input.Target.DefaultSlingFormula) || !input.Target.PoolTemplate ||
		input.Target.Suspended || !input.Target.SupportsGenericEphemeral || !input.Target.CustomSlingQueryAbsent ||
		!input.Target.RuntimeRigSuspensionKnown || input.Target.RuntimeRigSuspended {
		return AdmissionPolicyProjectionV2{}, admissionPolicyV2Error("target is missing or not eligible for generic admitted work")
	}
	if !validAdmissionCapacity(input.Target.AgentMaxActiveSessions) || !validAdmissionCapacity(input.Target.RigMaxActiveSessions) ||
		!validAdmissionCapacity(input.Target.WorkspaceMaxActiveSessions) || input.Target.MinActiveSessions < 0 {
		return AdmissionPolicyProjectionV2{}, admissionPolicyV2Error("target session capacity facts are invalid")
	}
	inheritedMax, inheritedSource := inheritedAdmissionCapacity(
		input.Target.AgentMaxActiveSessions,
		input.Target.RigMaxActiveSessions,
		input.Target.WorkspaceMaxActiveSessions,
	)
	if inheritedMax != input.Target.InheritedMaxActiveSessions || inheritedSource != input.Target.InheritedMaxSource ||
		!equalAdmissionCapacityPointer(input.Target.MaxActiveSessions, inheritedMax) || inheritedMax == 0 ||
		admissionAncestorCapacityIsZero(input.Target.RigMaxActiveSessions, input.Target.WorkspaceMaxActiveSessions) ||
		(inheritedMax >= 0 && input.Target.MinActiveSessions > inheritedMax) {
		return AdmissionPolicyProjectionV2{}, admissionPolicyV2Error("target inherited capacity facts are inconsistent or ineligible")
	}
	if !validLogicalFormulaID(input.Workflow) {
		return AdmissionPolicyProjectionV2{}, admissionPolicyV2Error("effective workflow identity is missing or non-canonical")
	}
	if input.Workflow != input.Target.DefaultSlingFormula {
		return AdmissionPolicyProjectionV2{}, admissionPolicyV2Error("effective workflow does not match the target pool's configured default sling formula")
	}
	if !validCanonicalText(input.FormulaCompilerCapability) {
		return AdmissionPolicyProjectionV2{}, admissionPolicyV2Error("formula compiler capability is missing or non-canonical")
	}
	if input.FormulaCompilerImplementationVersion != formula.FormulaCompilerImplementationVersion {
		return AdmissionPolicyProjectionV2{}, admissionPolicyV2Error("formula compiler implementation version is missing or unsupported")
	}
	if input.FormulaProvenanceSchemaVersion != formula.CompileProvenanceSchemaVersion {
		return AdmissionPolicyProjectionV2{}, admissionPolicyV2Error("formula provenance schema version is missing or unsupported")
	}
	if !validCanonicalText(input.FormulaSchemaVersion) {
		return AdmissionPolicyProjectionV2{}, admissionPolicyV2Error("formula schema version is missing or non-canonical")
	}
	switch input.MergeStrategy {
	case "direct", "mr", "local":
	default:
		return AdmissionPolicyProjectionV2{}, admissionPolicyV2Error("merge strategy is missing or unsupported")
	}
	if err := validateAdmissionStorePlacementV2(input.SourceScope, input.StorePlacement); err != nil {
		return AdmissionPolicyProjectionV2{}, err
	}

	sources := append([]AdmissionFormulaSourceV2(nil), input.FormulaSources...)
	if input.FormulaSourceCount < 1 || len(sources) != input.FormulaSourceCount {
		return AdmissionPolicyProjectionV2{}, admissionPolicyV2Error("formula source closure is missing or incomplete")
	}
	for index := range sources {
		if !validLogicalFormulaID(sources[index].LogicalID) || !validSHA256Hex(sources[index].SHA256) {
			return AdmissionPolicyProjectionV2{}, admissionPolicyV2Error("formula source closure contains a missing or invalid identity/hash")
		}
	}
	sortAdmissionFormulaSources(sources)
	sourceIDs := make(map[string]struct{}, len(sources))
	for index, source := range sources {
		if index > 0 && sources[index-1].LogicalID == source.LogicalID {
			return AdmissionPolicyProjectionV2{}, admissionPolicyV2Error("formula source closure contains an ambiguous duplicate identity")
		}
		sourceIDs[source.LogicalID] = struct{}{}
	}
	if _, ok := sourceIDs[input.Workflow]; !ok {
		return AdmissionPolicyProjectionV2{}, admissionPolicyV2Error("formula source closure does not include the effective workflow")
	}

	assets, assetIDs, err := canonicalAdmissionExternalAssetsV2(input)
	if err != nil {
		return AdmissionPolicyProjectionV2{}, err
	}
	checkClosures, err := canonicalAdmissionCheckClosuresV2(input, assetIDs)
	if err != nil {
		return AdmissionPolicyProjectionV2{}, err
	}

	composed := append([]string{}, input.EffectiveComposedFormulaIDs...)
	seenComposed := make(map[string]struct{}, len(composed))
	for _, id := range composed {
		if !validLogicalFormulaID(id) {
			return AdmissionPolicyProjectionV2{}, admissionPolicyV2Error("composed formula identity is missing or non-canonical")
		}
		if _, duplicate := seenComposed[id]; duplicate {
			return AdmissionPolicyProjectionV2{}, admissionPolicyV2Error("composition inputs contain a duplicate formula identity")
		}
		if _, present := sourceIDs[id]; !present {
			return AdmissionPolicyProjectionV2{}, admissionPolicyV2Error("composition input is missing from the formula source closure")
		}
		seenComposed[id] = struct{}{}
	}
	variables := make(map[string]string, len(input.EffectiveCompileVariables))
	for key, value := range input.EffectiveCompileVariables {
		if !validCanonicalText(key) {
			return AdmissionPolicyProjectionV2{}, admissionPolicyV2Error("effective compile variable name is missing or non-canonical")
		}
		variables[key] = value
	}
	input.FormulaSources = sources
	input.ExternalAssets = assets
	input.CheckClosures = checkClosures
	input.EffectiveComposedFormulaIDs = composed
	input.EffectiveCompileVariables = variables
	input.Target.AgentMaxActiveSessions = cloneAdmissionInt(input.Target.AgentMaxActiveSessions)
	input.Target.RigMaxActiveSessions = cloneAdmissionInt(input.Target.RigMaxActiveSessions)
	input.Target.WorkspaceMaxActiveSessions = cloneAdmissionInt(input.Target.WorkspaceMaxActiveSessions)
	input.Target.MaxActiveSessions = cloneAdmissionInt(input.Target.MaxActiveSessions)
	return input, nil
}

func equalAdmissionCapacityPointer(value *int, inherited int) bool {
	if inherited == -1 {
		return value == nil
	}
	return value != nil && *value == inherited
}

func canonicalAdmissionExternalAssetsV2(input AdmissionPolicyProjectionV2) ([]AdmissionExternalAssetV2, map[string]struct{}, error) {
	assets := append([]AdmissionExternalAssetV2(nil), input.ExternalAssets...)
	if !input.ExternalAssetClosureComplete || input.ExternalAssetCount < 0 || len(assets) != input.ExternalAssetCount {
		return nil, nil, admissionPolicyV2Error("external formula/check asset closure is missing or incomplete")
	}
	for index, asset := range assets {
		if !validLogicalFormulaID(asset.LogicalID) || !validSHA256Hex(asset.SHA256) {
			return nil, nil, admissionPolicyV2Error("external asset closure contains a missing or invalid identity/hash")
		}
		assets[index] = asset
	}
	sort.Slice(assets, func(i, j int) bool {
		if assets[i].LogicalID == assets[j].LogicalID {
			return assets[i].SHA256 < assets[j].SHA256
		}
		return assets[i].LogicalID < assets[j].LogicalID
	})
	assetIDs := make(map[string]struct{}, len(assets))
	for index, asset := range assets {
		if index > 0 && assets[index-1].LogicalID == asset.LogicalID {
			return nil, nil, admissionPolicyV2Error("external asset closure contains an ambiguous duplicate identity")
		}
		assetIDs[asset.LogicalID] = struct{}{}
	}
	return assets, assetIDs, nil
}

func canonicalAdmissionCheckClosuresV2(input AdmissionPolicyProjectionV2, assetIDs map[string]struct{}) ([]AdmissionCheckClosureV2, error) {
	closures := append([]AdmissionCheckClosureV2(nil), input.CheckClosures...)
	if !input.CheckMappingsComplete || input.CheckPathCount < 0 || len(closures) != input.CheckPathCount {
		return nil, admissionPolicyV2Error("check_path-to-asset mapping is missing or incomplete")
	}
	for index := range closures {
		closure := &closures[index]
		if !validLogicalFormulaID(closure.StepID) || !validLogicalFormulaID(closure.CheckAssetLogicalID) ||
			!closure.DependenciesComplete || closure.DependencyCount < 0 || len(closure.DependencyLogicalIDs) != closure.DependencyCount {
			return nil, admissionPolicyV2Error("check dependency mapping is missing or incomplete")
		}
		if _, exists := assetIDs[closure.CheckAssetLogicalID]; !exists {
			return nil, admissionPolicyV2Error("check script is missing from the external asset closure")
		}
		dependencies := append([]string(nil), closure.DependencyLogicalIDs...)
		sort.Strings(dependencies)
		for index, dependency := range dependencies {
			if !validLogicalFormulaID(dependency) {
				return nil, admissionPolicyV2Error("check dependency identity is missing or non-canonical")
			}
			if _, exists := assetIDs[dependency]; !exists {
				return nil, admissionPolicyV2Error("check dependency is missing from the external asset closure")
			}
			if index > 0 && dependencies[index-1] == dependency {
				return nil, admissionPolicyV2Error("check dependency closure contains a duplicate identity")
			}
		}
		closure.DependencyLogicalIDs = dependencies
	}
	sort.Slice(closures, func(i, j int) bool { return closures[i].StepID < closures[j].StepID })
	for index := 1; index < len(closures); index++ {
		if closures[index-1].StepID == closures[index].StepID {
			return nil, admissionPolicyV2Error("check mapping contains a duplicate step identity")
		}
	}
	return closures, nil
}

func validateAdmissionStorePlacementV2(sourceScope string, placement AdmissionStorePlacementV2) error {
	if !validCanonicalText(placement.SourceStoreRef) || !validCanonicalStoreRef(placement.SourceStoreRef) ||
		!validCanonicalStoreRef(placement.GraphStoreRef) || !validCanonicalStoreRef(placement.WorkflowStoreRef) {
		return admissionPolicyV2Error("source, graph, or workflow store reference is missing or non-canonical")
	}
	if !strings.HasPrefix(sourceScope, "city:") {
		return admissionPolicyV2Error("source scope does not identify the selected source store")
	}
	city, scopedStore, hasStore := strings.Cut(strings.TrimPrefix(sourceScope, "city:"), "/")
	if !hasStore || !validCanonicalText(city) || strings.ContainsAny(city, ":/\\") ||
		!validCanonicalStoreRef(scopedStore) || scopedStore != placement.SourceStoreRef {
		return admissionPolicyV2Error("source scope does not identify the selected source store")
	}
	switch placement.GraphPlacementMode {
	case AdmissionGraphPlacementSourceRig:
		if placement.GraphStoreRef != placement.SourceStoreRef {
			return admissionPolicyV2Error("source-rig graph placement does not match its source store")
		}
	case AdmissionGraphPlacementClass:
		if placement.ResolvedStoragePlan.graphBinding == string(storebinding.ReservedWorkBinding) {
			if placement.GraphStoreRef != "city:"+city {
				return admissionPolicyV2Error("graph placement does not match the resolved reserved-work binding")
			}
		} else if placement.ResolvedStoragePlan.graphClassStoreRef == "" ||
			placement.GraphStoreRef != placement.ResolvedStoragePlan.graphClassStoreRef {
			return admissionPolicyV2Error("graph placement does not match the resolved class binding")
		}
	default:
		return admissionPolicyV2Error("graph placement mode is unknown")
	}
	if !validSHA256Hex(placement.ResolvedStoragePlan.planSHA256) ||
		!validCanonicalText(placement.ResolvedStoragePlan.graphBinding) ||
		!validSHA256Hex(placement.ResolvedStoragePlan.graphBindingSHA256) {
		return admissionPolicyV2Error("graph placement has no resolved storage-plan binding proof")
	}
	switch placement.WorkflowPlacementMode {
	case AdmissionWorkflowPlacementSource:
		if placement.WorkflowStoreRef != placement.SourceStoreRef {
			return admissionPolicyV2Error("source workflow placement does not match its source store")
		}
	case AdmissionWorkflowPlacementGraph:
		if placement.WorkflowStoreRef != placement.GraphStoreRef {
			return admissionPolicyV2Error("graph workflow placement does not match its graph store")
		}
	default:
		return admissionPolicyV2Error("workflow placement mode is unknown")
	}
	return nil
}

func admissionLegacyAliasMatches(agent *config.Agent, identity string) bool {
	if agent == nil {
		return false
	}
	dir, local := config.ParseQualifiedName(identity)
	if dir != agent.Dir {
		return false
	}
	if agent.BindingName != "" {
		return local == agent.Name
	}
	_, unboundName, hasBinding := strings.Cut(local, ".")
	return hasBinding && unboundName == agent.Name
}

func isAdmissionPoolSlotSuffix(identity string, agents []config.Agent) bool {
	for index := range agents {
		agent := agents[index]
		if !isAdmissionPoolTemplate(&agent) {
			continue
		}
		base := agentutil.RoutedToIdentity(&agent)
		for _, name := range agent.NamepoolNames {
			member := name
			if agent.Dir != "" {
				member = agent.Dir + "/" + name
			}
			if identity == member {
				return true
			}
		}
		if !agent.SupportsInstanceExpansion() || agent.UsesCanonicalSingletonPoolIdentity() {
			continue
		}
		prefix := base + "-"
		if !strings.HasPrefix(identity, prefix) {
			continue
		}
		suffix := identity[len(prefix):]
		if suffix != "" && strings.Trim(suffix, "0123456789") == "" && strings.TrimLeft(suffix, "0") != "" {
			return true
		}
	}
	return false
}

func isAdmissionPoolTemplate(agent *config.Agent) bool {
	if agent == nil {
		return false
	}
	return agent.SupportsMultipleSessions() || agent.MinActiveSessions != nil || strings.TrimSpace(agent.ScaleCheck) != ""
}

func isRigQualifiedAdmissionIdentity(identity string) bool {
	if identity == "" || strings.TrimSpace(identity) != identity || strings.Count(identity, "/") != 1 ||
		strings.ContainsAny(identity, "\\\x00\r\n\t") {
		return false
	}
	rig, local := config.ParseQualifiedName(identity)
	return rig != "" && local != "" && strings.TrimSpace(rig) == rig && strings.TrimSpace(local) == local &&
		rig != "." && rig != ".." && local != "." && local != ".."
}

func validLogicalFormulaID(identity string) bool {
	if !validCanonicalText(identity) || strings.Contains(identity, "\\") || path.IsAbs(identity) ||
		isWindowsAbsolutePath(identity) || path.Clean(identity) != identity {
		return false
	}
	for _, segment := range strings.Split(identity, "/") {
		if segment == ".." || segment == "." || segment == "" {
			return false
		}
	}
	return true
}

func isWindowsAbsolutePath(value string) bool {
	return len(value) >= 3 && ((value[0] >= 'A' && value[0] <= 'Z') || (value[0] >= 'a' && value[0] <= 'z')) &&
		value[1] == ':' && (value[2] == '/' || value[2] == '\\')
}

func validCanonicalStoreRef(value string) bool {
	if !validCanonicalText(value) || strings.ContainsAny(value, "/\\") {
		return false
	}
	kind, name, hasSeparator := strings.Cut(value, ":")
	if !hasSeparator || !validCanonicalText(name) || strings.Contains(name, ":") || name == "." || name == ".." {
		return false
	}
	switch kind {
	case "city", "rig":
		return validCanonicalText(name)
	case "class":
		for _, char := range name {
			if char < 'a' || char > 'z' {
				return false
			}
		}
		return name != ""
	default:
		return false
	}
}

func validCanonicalText(value string) bool {
	return value != "" && strings.TrimSpace(value) == value && !strings.ContainsAny(value, "\x00\r\n\t")
}

func validSHA256Hex(value string) bool {
	if len(value) != 64 || strings.ToLower(value) != value {
		return false
	}
	for _, char := range value {
		if !(char >= '0' && char <= '9') && !(char >= 'a' && char <= 'f') {
			return false
		}
	}
	return true
}

func sortAdmissionFormulaSources(sources []AdmissionFormulaSourceV2) {
	sort.Slice(sources, func(i, j int) bool {
		if sources[i].LogicalID == sources[j].LogicalID {
			return sources[i].SHA256 < sources[j].SHA256
		}
		return sources[i].LogicalID < sources[j].LogicalID
	})
}

func admissionPolicyV2Error(reason string) error {
	return fmt.Errorf("%w: %s", errAdmissionPolicyV2Invalid, reason)
}
