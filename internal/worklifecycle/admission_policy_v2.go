package worklifecycle

import (
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/agentutil"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/qualification"
)

const (
	// AdmissionPolicyProjectionV2Version versions the canonical input schema
	// hashed by DigestAdmissionPolicyV2. Change it when the projected fields or
	// their meaning changes.
	AdmissionPolicyProjectionV2Version = 1

	admissionPolicyProjectionV2Domain = "gascity.lifecycle.routing-formula-policy.v2\n"
	// AdmissionRouteResolverV2Version identifies the exact canonical-target
	// matching and legacy-alias rejection rules projected into Q54 policy.
	AdmissionRouteResolverV2Version = "exact-rig-pool-v1"

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

// CanonicalAdmissionPoolV2 is the resolved, rig-qualified pool identity and
// the facts that allow it to receive generic admitted work. It contains no
// filesystem paths or live runtime state.
type CanonicalAdmissionPoolV2 struct {
	Identity                 string `json:"identity"`
	PoolTemplate             bool   `json:"pool_template"`
	Suspended                bool   `json:"suspended"`
	SupportsGenericEphemeral bool   `json:"supports_generic_ephemeral"`
	CustomSlingQueryAbsent   bool   `json:"custom_sling_query_absent"`
	MaxActiveSessions        *int   `json:"max_active_sessions,omitempty"`
	MinActiveSessions        int    `json:"min_active_sessions"`
}

// ResolveCanonicalAdmissionPoolV2 accepts only an exact canonical identity
// for one configured rig-scoped pool template. It does not normalize
// pool slots or resolve migration-era bound/unbound aliases.
func ResolveCanonicalAdmissionPoolV2(identity string, agents []config.Agent) (CanonicalAdmissionPoolV2, error) {
	if identity == "" || strings.TrimSpace(identity) != identity || !isRigQualifiedAdmissionIdentity(identity) {
		return CanonicalAdmissionPoolV2{}, admissionPolicyV2Error("target identity is not a canonical rig-qualified identity")
	}
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
	if !agent.SupportsGenericEphemeralSessions() {
		return CanonicalAdmissionPoolV2{}, admissionPolicyV2Error("target pool does not support generic ephemeral sessions")
	}
	if agent.MinActiveSessions != nil && *agent.MinActiveSessions < 0 {
		return CanonicalAdmissionPoolV2{}, admissionPolicyV2Error("target pool has an invalid minimum session capacity")
	}
	if max := agent.EffectiveMaxActiveSessions(); max != nil && (*max < -1 || (*max >= 0 && agent.EffectiveMinActiveSessions() > *max)) {
		return CanonicalAdmissionPoolV2{}, admissionPolicyV2Error("target pool has invalid session capacity bounds")
	}
	customSlingQueryAbsent := strings.TrimSpace(agent.SlingQuery) == ""
	if !customSlingQueryAbsent {
		return CanonicalAdmissionPoolV2{}, admissionPolicyV2Error("target pool has a custom sling query")
	}
	var maxActiveSessions *int
	if max := agent.EffectiveMaxActiveSessions(); max != nil {
		maxValue := *max
		maxActiveSessions = &maxValue
	}
	return CanonicalAdmissionPoolV2{
		Identity:                 canonical,
		PoolTemplate:             true,
		Suspended:                false,
		SupportsGenericEphemeral: true,
		CustomSlingQueryAbsent:   true,
		MaxActiveSessions:        maxActiveSessions,
		MinActiveSessions:        agent.EffectiveMinActiveSessions(),
	}, nil
}

// AdmissionFormulaSourceV2 identifies one formula source by its stable
// logical formula name and SHA-256 of its loaded bytes. LogicalID must not be a
// process-local source path.
type AdmissionFormulaSourceV2 struct {
	LogicalID string `json:"logical_id"`
	SHA256    string `json:"sha256"`
}

// AdmissionStorePlacementV2 captures the stable store references and
// selection facts used when a workflow is materialized.
type AdmissionStorePlacementV2 struct {
	SourceStoreRef        string `json:"source_store_ref"`
	GraphPlacementMode    string `json:"graph_placement_mode"`
	GraphClassBinding     string `json:"graph_class_binding"`
	GraphStoreRef         string `json:"graph_store_ref"`
	WorkflowPlacementMode string `json:"workflow_placement_mode"`
	WorkflowStoreRef      string `json:"workflow_store_ref"`
}

// AdmissionPolicyProjectionV2 is the complete, caller-resolved input to the
// versioned route/formula policy digest. FormulaSources must include the
// effective workflow and every inherited or composed formula source. Their
// LogicalID values come from stable formula identities, never source paths;
// FormulaSourceCount must equal the source count from the compiled recipe.
type AdmissionPolicyProjectionV2 struct {
	SourceScope                 string                     `json:"source_scope"`
	RouteResolverVersion        string                     `json:"route_resolver_version"`
	Target                      CanonicalAdmissionPoolV2   `json:"target"`
	Workflow                    string                     `json:"workflow"`
	FormulaSources              []AdmissionFormulaSourceV2 `json:"formula_sources"`
	FormulaSourceCount          int                        `json:"formula_source_count"`
	FormulaCompilerVersion      string                     `json:"formula_compiler_version"`
	FormulaSchemaVersion        string                     `json:"formula_schema_version"`
	FormulaV2Enabled            bool                       `json:"formula_v2_enabled"`
	EffectiveCompileVariables   map[string]string          `json:"effective_compile_variables"`
	EffectiveComposedFormulaIDs []string                   `json:"effective_composed_formula_ids"`
	MergeStrategy               string                     `json:"merge_strategy"`
	StorePlacement              AdmissionStorePlacementV2  `json:"store_placement"`
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
	if !isRigQualifiedAdmissionIdentity(input.Target.Identity) || !input.Target.PoolTemplate ||
		input.Target.Suspended || !input.Target.SupportsGenericEphemeral || !input.Target.CustomSlingQueryAbsent {
		return AdmissionPolicyProjectionV2{}, admissionPolicyV2Error("target is missing or not eligible for generic admitted work")
	}
	if input.Target.MinActiveSessions < 0 || (input.Target.MaxActiveSessions != nil && *input.Target.MaxActiveSessions < -1) ||
		(input.Target.MaxActiveSessions != nil && *input.Target.MaxActiveSessions == 0) ||
		(input.Target.MaxActiveSessions != nil && *input.Target.MaxActiveSessions >= 0 && input.Target.MinActiveSessions > *input.Target.MaxActiveSessions) {
		return AdmissionPolicyProjectionV2{}, admissionPolicyV2Error("target session capacity facts are invalid")
	}
	if !validLogicalFormulaID(input.Workflow) {
		return AdmissionPolicyProjectionV2{}, admissionPolicyV2Error("effective workflow identity is missing or non-canonical")
	}
	if !validCanonicalText(input.FormulaCompilerVersion) {
		return AdmissionPolicyProjectionV2{}, admissionPolicyV2Error("formula compiler version is missing or non-canonical")
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
	input.EffectiveComposedFormulaIDs = composed
	input.EffectiveCompileVariables = variables
	return input, nil
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
		if placement.GraphClassBinding != "" || placement.GraphStoreRef != placement.SourceStoreRef {
			return admissionPolicyV2Error("source-rig graph placement does not match its source store")
		}
	case AdmissionGraphPlacementClass:
		if !validCanonicalText(placement.GraphClassBinding) {
			return admissionPolicyV2Error("graph-class placement is missing its selected binding identity")
		}
	default:
		return admissionPolicyV2Error("graph placement mode is unknown")
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
	return hasSeparator && validCanonicalText(kind) && validCanonicalText(name) && !strings.Contains(name, ":")
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
