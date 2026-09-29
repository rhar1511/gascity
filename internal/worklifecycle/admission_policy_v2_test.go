package worklifecycle

import (
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/formula"
)

func TestResolveCanonicalAdmissionPoolV2(t *testing.T) {
	maxOne := 1
	pool := config.Agent{Name: "worker", Dir: "rig-a", BindingName: "pack", MinActiveSessions: &maxOne}
	wantIdentity := "rig-a/pack.worker"

	target, err := ResolveCanonicalAdmissionPoolV2(wantIdentity, []config.Agent{pool})
	if err != nil {
		t.Fatalf("ResolveCanonicalAdmissionPoolV2() error = %v", err)
	}
	if target.Identity != wantIdentity || !target.PoolTemplate || target.Suspended ||
		!target.SupportsGenericEphemeral || !target.CustomSlingQueryAbsent {
		t.Fatalf("resolved target = %+v, want eligible canonical pool %q", target, wantIdentity)
	}
	if target.MaxActiveSessions != nil || target.MinActiveSessions != 1 {
		t.Fatalf("resolved capacity = max %v, min %d, want unlimited max and min 1", target.MaxActiveSessions, target.MinActiveSessions)
	}
}

func TestResolveCanonicalAdmissionPoolV2RejectsUnsafeIdentityAndTarget(t *testing.T) {
	maxOne := 1
	base := config.Agent{Name: "worker", Dir: "rig-a", MinActiveSessions: &maxOne}
	bound := base
	bound.BindingName = "pack"
	bound.Suspended = true
	customQuery := base
	customQuery.SlingQuery = "bd update {} --set-metadata custom=yes"
	noGeneric := base
	zero := 0
	noGeneric.MaxActiveSessions = &zero
	singleton := base
	singleton.MinActiveSessions = nil
	singleton.MaxActiveSessions = &maxOne

	tests := []struct {
		name     string
		identity string
		agents   []config.Agent
	}{
		{name: "empty", agents: []config.Agent{base}},
		{name: "unqualified target", identity: "worker", agents: []config.Agent{base}},
		{name: "noncanonical whitespace", identity: " rig-a/worker", agents: []config.Agent{base}},
		{name: "whitespace in rig component", identity: "rig-a /worker", agents: []config.Agent{base}},
		{name: "whitespace in target component", identity: "rig-a/worker ", agents: []config.Agent{base}},
		{name: "missing target", identity: "rig-a/other", agents: []config.Agent{base}},
		{name: "suspended", identity: "rig-a/pack.worker", agents: []config.Agent{bound}},
		{name: "custom sling query", identity: "rig-a/worker", agents: []config.Agent{customQuery}},
		{name: "generic ephemeral disabled", identity: "rig-a/worker", agents: []config.Agent{noGeneric}},
		{name: "invalid capacity bounds", identity: "rig-a/worker", agents: []config.Agent{{Name: "worker", Dir: "rig-a", MinActiveSessions: intPtr(2), MaxActiveSessions: intPtr(1)}}},
		{name: "not a pool template", identity: "rig-a/worker", agents: []config.Agent{singleton}},
		{name: "city scoped", identity: "worker", agents: []config.Agent{{Name: "worker"}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := ResolveCanonicalAdmissionPoolV2(test.identity, test.agents); err == nil {
				t.Fatalf("ResolveCanonicalAdmissionPoolV2(%q) unexpectedly succeeded", test.identity)
			}
		})
	}
}

func TestResolveCanonicalAdmissionPoolV2RejectsSlotsAliasesAndDuplicates(t *testing.T) {
	maxOne := 1
	pool := config.Agent{Name: "worker", Dir: "rig-a", MinActiveSessions: &maxOne}
	bound := config.Agent{Name: "worker", Dir: "rig-a", BindingName: "pack", MinActiveSessions: &maxOne}
	unbound := config.Agent{Name: "worker", Dir: "rig-a", MinActiveSessions: &maxOne}

	tests := []struct {
		name     string
		identity string
		agents   []config.Agent
	}{
		{
			name:     "slot suffix despite literal collision",
			identity: "rig-a/worker-1",
			agents: []config.Agent{
				pool,
				{Name: "worker-1", Dir: "rig-a", MinActiveSessions: &maxOne},
			},
		},
		{name: "legacy unbound alias", identity: "rig-a/worker", agents: []config.Agent{bound}},
		{name: "legacy bound alias", identity: "rig-a/pack.worker", agents: []config.Agent{unbound}},
		{name: "duplicate canonical identity", identity: "rig-a/worker", agents: []config.Agent{pool, pool}},
		{
			name:     "canonical target collides with another templates legacy alias",
			identity: "rig-a/worker",
			agents: []config.Agent{
				unbound,
				bound,
			},
		},
		{
			name:     "namepool member identity",
			identity: "rig-a/Ada",
			agents:   []config.Agent{{Name: "worker", Dir: "rig-a", MinActiveSessions: &maxOne, NamepoolNames: []string{"Ada"}}},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := ResolveCanonicalAdmissionPoolV2(test.identity, test.agents); err == nil {
				t.Fatalf("ResolveCanonicalAdmissionPoolV2(%q) unexpectedly succeeded", test.identity)
			}
		})
	}
}

func TestDigestAdmissionPolicyV2IsStableForMapSourceOrderAndFilesystemPath(t *testing.T) {
	first := validAdmissionPolicyProjectionV2()
	first.FormulaSources = []AdmissionFormulaSourceV2{
		{LogicalID: "mol-parent", SHA256: strings.Repeat("b", 64)},
		{LogicalID: "mol-work", SHA256: strings.Repeat("a", 64)},
	}
	first.EffectiveCompileVariables = map[string]string{"component": "api", "mode": "safe"}

	second := validAdmissionPolicyProjectionV2()
	second.FormulaSources = []AdmissionFormulaSourceV2{
		{LogicalID: "mol-work", SHA256: strings.Repeat("a", 64)},
		{LogicalID: "mol-parent", SHA256: strings.Repeat("b", 64)},
	}
	second.EffectiveCompileVariables = map[string]string{"mode": "safe", "component": "api"}

	// Formula SourceIdentity.Path is process-local. Only its stable logical ID
	// and content hash enter the projection.
	fromDifferentPaths := func(path string) AdmissionFormulaSourceV2 {
		source := formula.SourceIdentity{Path: path, ContentSHA256: strings.Repeat("a", 64)}
		return AdmissionFormulaSourceV2{LogicalID: "mol-work", SHA256: source.ContentSHA256}
	}
	second.FormulaSources[0] = fromDifferentPaths("/tmp/another-checkout/formulas/work.formula")
	first.FormulaSources[1] = fromDifferentPaths("/srv/city/formulas/work.formula")

	firstDigest, err := DigestAdmissionPolicyV2(first)
	if err != nil {
		t.Fatalf("DigestAdmissionPolicyV2(first): %v", err)
	}
	secondDigest, err := DigestAdmissionPolicyV2(second)
	if err != nil {
		t.Fatalf("DigestAdmissionPolicyV2(second): %v", err)
	}
	if firstDigest != secondDigest {
		t.Fatalf("digest differs across map, source order, or absolute path: %s != %s", firstDigest, secondDigest)
	}
	if !validSHA256Hex(firstDigest) {
		t.Fatalf("digest = %q, want lowercase 64-character SHA-256 hex", firstDigest)
	}
}

func TestDigestAdmissionPolicyV2ChangesForReviewedPolicyInputs(t *testing.T) {
	base := validAdmissionPolicyProjectionV2()
	baseDigest, err := DigestAdmissionPolicyV2(base)
	if err != nil {
		t.Fatalf("DigestAdmissionPolicyV2(base): %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*AdmissionPolicyProjectionV2)
	}{
		{name: "same-name formula source bytes", mutate: func(p *AdmissionPolicyProjectionV2) { p.FormulaSources[0].SHA256 = strings.Repeat("c", 64) }},
		{name: "inherited source identity", mutate: func(p *AdmissionPolicyProjectionV2) {
			p.FormulaSources[1].LogicalID = "mol-other-parent"
			p.EffectiveComposedFormulaIDs[0] = "mol-other-parent"
		}},
		{name: "source scope", mutate: func(p *AdmissionPolicyProjectionV2) {
			p.SourceScope = ScopeForStore("other-city", p.StorePlacement.SourceStoreRef)
		}},
		{name: "target identity", mutate: func(p *AdmissionPolicyProjectionV2) { p.Target.Identity = "rig-b/worker" }},
		{name: "effective workflow", mutate: func(p *AdmissionPolicyProjectionV2) {
			p.Workflow = "mol-other"
			p.FormulaSources = append(p.FormulaSources, AdmissionFormulaSourceV2{LogicalID: "mol-other", SHA256: strings.Repeat("d", 64)})
			p.FormulaSourceCount++
		}},
		{name: "compiler version", mutate: func(p *AdmissionPolicyProjectionV2) { p.FormulaCompilerVersion = "2.1.0" }},
		{name: "formula schema", mutate: func(p *AdmissionPolicyProjectionV2) { p.FormulaSchemaVersion = "formula.v2" }},
		{name: "formula v2 mode", mutate: func(p *AdmissionPolicyProjectionV2) { p.FormulaV2Enabled = false }},
		{name: "compile variable", mutate: func(p *AdmissionPolicyProjectionV2) { p.EffectiveCompileVariables["component"] = "worker" }},
		{name: "composition input", mutate: func(p *AdmissionPolicyProjectionV2) {
			p.EffectiveComposedFormulaIDs = append(p.EffectiveComposedFormulaIDs, "mol-aspect")
			p.FormulaSources = append(p.FormulaSources, AdmissionFormulaSourceV2{LogicalID: "mol-aspect", SHA256: strings.Repeat("e", 64)})
			p.FormulaSourceCount++
		}},
		{name: "merge behavior", mutate: func(p *AdmissionPolicyProjectionV2) { p.MergeStrategy = "direct" }},
		{name: "pool capacity", mutate: func(p *AdmissionPolicyProjectionV2) { p.Target.MaxActiveSessions = intPtr(3) }},
		{name: "minimum pool capacity", mutate: func(p *AdmissionPolicyProjectionV2) { p.Target.MinActiveSessions = 2 }},
		{name: "source store placement", mutate: func(p *AdmissionPolicyProjectionV2) {
			p.SourceScope = ScopeForStore("city-a", "rig:other")
			p.StorePlacement.SourceStoreRef = "rig:other"
		}},
		{name: "graph store binding", mutate: func(p *AdmissionPolicyProjectionV2) { p.StorePlacement.GraphClassBinding = "other-graph" }},
		{name: "graph store placement", mutate: func(p *AdmissionPolicyProjectionV2) {
			p.StorePlacement.GraphStoreRef = "city:other"
			p.StorePlacement.WorkflowStoreRef = "city:other"
		}},
		{name: "workflow placement mode", mutate: func(p *AdmissionPolicyProjectionV2) {
			p.StorePlacement.WorkflowPlacementMode = "source"
			p.StorePlacement.WorkflowStoreRef = p.StorePlacement.SourceStoreRef
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			changed := validAdmissionPolicyProjectionV2()
			test.mutate(&changed)
			got, err := DigestAdmissionPolicyV2(changed)
			if err != nil {
				t.Fatalf("DigestAdmissionPolicyV2(changed): %v", err)
			}
			if got == baseDigest {
				t.Fatalf("digest did not change for %s", test.name)
			}
		})
	}
}

func TestDigestAdmissionPolicyV2RejectsIncompleteOrAmbiguousInputs(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*AdmissionPolicyProjectionV2)
	}{
		{name: "missing scope", mutate: func(p *AdmissionPolicyProjectionV2) { p.SourceScope = "" }},
		{name: "missing target", mutate: func(p *AdmissionPolicyProjectionV2) { p.Target.Identity = "" }},
		{name: "unsupported route resolver", mutate: func(p *AdmissionPolicyProjectionV2) { p.RouteResolverVersion = "legacy-alias-v1" }},
		{name: "suspended target", mutate: func(p *AdmissionPolicyProjectionV2) { p.Target.Suspended = true }},
		{name: "not generic ephemeral", mutate: func(p *AdmissionPolicyProjectionV2) { p.Target.SupportsGenericEphemeral = false }},
		{name: "invalid pool capacity", mutate: func(p *AdmissionPolicyProjectionV2) { p.Target.MinActiveSessions = -1 }},
		{name: "generic capability conflicts with zero capacity", mutate: func(p *AdmissionPolicyProjectionV2) { p.Target.MaxActiveSessions = intPtr(0) }},
		{name: "custom sling query", mutate: func(p *AdmissionPolicyProjectionV2) { p.Target.CustomSlingQueryAbsent = false }},
		{name: "missing workflow", mutate: func(p *AdmissionPolicyProjectionV2) { p.Workflow = "" }},
		{name: "missing formula closure", mutate: func(p *AdmissionPolicyProjectionV2) { p.FormulaSources = nil }},
		{name: "incomplete formula source count", mutate: func(p *AdmissionPolicyProjectionV2) { p.FormulaSourceCount-- }},
		{name: "missing root formula source", mutate: func(p *AdmissionPolicyProjectionV2) { p.FormulaSources = p.FormulaSources[1:] }},
		{name: "absolute source path as logical ID", mutate: func(p *AdmissionPolicyProjectionV2) { p.FormulaSources[0].LogicalID = "/tmp/work.formula" }},
		{name: "Windows source path as logical ID", mutate: func(p *AdmissionPolicyProjectionV2) { p.FormulaSources[0].LogicalID = "C:/checkout/work.formula" }},
		{name: "invalid source digest", mutate: func(p *AdmissionPolicyProjectionV2) { p.FormulaSources[0].SHA256 = "not-a-sha256" }},
		{name: "duplicate source identity", mutate: func(p *AdmissionPolicyProjectionV2) { p.FormulaSources = append(p.FormulaSources, p.FormulaSources[0]) }},
		{name: "composition outside closure", mutate: func(p *AdmissionPolicyProjectionV2) { p.EffectiveComposedFormulaIDs = []string{"mol-missing"} }},
		{name: "duplicate composition", mutate: func(p *AdmissionPolicyProjectionV2) {
			p.EffectiveComposedFormulaIDs = []string{"mol-parent", "mol-parent"}
		}},
		{name: "missing compiler version", mutate: func(p *AdmissionPolicyProjectionV2) { p.FormulaCompilerVersion = "" }},
		{name: "missing schema version", mutate: func(p *AdmissionPolicyProjectionV2) { p.FormulaSchemaVersion = "" }},
		{name: "unsupported merge behavior", mutate: func(p *AdmissionPolicyProjectionV2) { p.MergeStrategy = "unknown" }},
		{name: "missing source store", mutate: func(p *AdmissionPolicyProjectionV2) { p.StorePlacement.SourceStoreRef = "" }},
		{name: "graph store ref path", mutate: func(p *AdmissionPolicyProjectionV2) { p.StorePlacement.GraphStoreRef = "city:other/path" }},
		{name: "unknown graph placement", mutate: func(p *AdmissionPolicyProjectionV2) { p.StorePlacement.GraphPlacementMode = "unknown" }},
		{name: "missing graph binding", mutate: func(p *AdmissionPolicyProjectionV2) { p.StorePlacement.GraphClassBinding = "" }},
		{name: "scope store mismatch", mutate: func(p *AdmissionPolicyProjectionV2) { p.SourceScope = ScopeForStore("city-a", "rig:other") }},
		{name: "scope with extra store path", mutate: func(p *AdmissionPolicyProjectionV2) { p.SourceScope = "city:city-a/rig:source/child" }},
		{name: "scope with empty city", mutate: func(p *AdmissionPolicyProjectionV2) { p.SourceScope = "city:/rig:source" }},
		{name: "scope with noncanonical whitespace", mutate: func(p *AdmissionPolicyProjectionV2) { p.SourceScope = "city:city-a /rig:source" }},
		{name: "workflow store mismatch", mutate: func(p *AdmissionPolicyProjectionV2) { p.StorePlacement.WorkflowStoreRef = "rig:other" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := validAdmissionPolicyProjectionV2()
			test.mutate(&input)
			if _, err := DigestAdmissionPolicyV2(input); err == nil {
				t.Fatalf("DigestAdmissionPolicyV2() unexpectedly accepted %s", test.name)
			}
		})
	}
}

func validAdmissionPolicyProjectionV2() AdmissionPolicyProjectionV2 {
	return AdmissionPolicyProjectionV2{
		SourceScope:          ScopeForStore("city-a", "rig:source"),
		RouteResolverVersion: AdmissionRouteResolverV2Version,
		Target: CanonicalAdmissionPoolV2{
			Identity:                 "rig-a/worker",
			PoolTemplate:             true,
			SupportsGenericEphemeral: true,
			CustomSlingQueryAbsent:   true,
		},
		Workflow:               "mol-work",
		FormulaCompilerVersion: "2.0.0",
		FormulaSchemaVersion:   "graph.v2",
		FormulaV2Enabled:       true,
		FormulaSources: []AdmissionFormulaSourceV2{
			{LogicalID: "mol-work", SHA256: strings.Repeat("a", 64)},
			{LogicalID: "mol-parent", SHA256: strings.Repeat("b", 64)},
		},
		FormulaSourceCount:          2,
		EffectiveCompileVariables:   map[string]string{"component": "api"},
		EffectiveComposedFormulaIDs: []string{"mol-parent"},
		MergeStrategy:               "mr",
		StorePlacement: AdmissionStorePlacementV2{
			SourceStoreRef:        "rig:source",
			GraphPlacementMode:    "graph-class",
			GraphClassBinding:     "graph-primary",
			GraphStoreRef:         "city:city-a",
			WorkflowPlacementMode: "graph",
			WorkflowStoreRef:      "city:city-a",
		},
	}
}

func intPtr(value int) *int {
	return &value
}
