package worklifecycle

import (
	"context"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/formula"
	"github.com/gastownhall/gascity/internal/storebinding"
)

func TestResolveCanonicalAdmissionPoolV2(t *testing.T) {
	maxOne := 1
	defaultFormula := "mol-work"
	pool := config.Agent{Name: "worker", Dir: "rig-a", BindingName: "pack", MinActiveSessions: &maxOne, DefaultSlingFormula: &defaultFormula}
	wantIdentity := "rig-a/pack.worker"

	target, err := ResolveCanonicalAdmissionPoolV2(wantIdentity, admissionTargetContext([]config.Agent{pool}))
	if err != nil {
		t.Fatalf("ResolveCanonicalAdmissionPoolV2() error = %v", err)
	}
	if target.Identity != wantIdentity || target.DefaultSlingFormula != defaultFormula || !target.PoolTemplate || target.Suspended ||
		!target.SupportsGenericEphemeral || !target.CustomSlingQueryAbsent {
		t.Fatalf("resolved target = %+v, want eligible canonical pool %q", target, wantIdentity)
	}
	if target.InheritedMaxActiveSessions != -1 || target.InheritedMaxSource != "unlimited" || target.MinActiveSessions != 1 {
		t.Fatalf("resolved capacity = max %d (%s), min %d, want unlimited max and min 1", target.InheritedMaxActiveSessions, target.InheritedMaxSource, target.MinActiveSessions)
	}
}

func TestResolveCanonicalAdmissionPoolV2RequiresEffectiveRigSuspensionAndBindsConfig(t *testing.T) {
	maximum := 3
	defaultFormula := "mol-work"
	agent := config.Agent{Name: "worker", Dir: "rig-a", MinActiveSessions: intPtr(1), DefaultSlingFormula: &defaultFormula}
	baseCity := &config.City{
		Agents:    []config.Agent{agent},
		Workspace: config.Workspace{MaxActiveSessions: intPtr(12)},
		Rigs:      []config.Rig{{Name: "rig-a", MaxActiveSessions: intPtr(maximum)}},
	}
	runtimeSuspended := false
	target, err := ResolveCanonicalAdmissionPoolV2("rig-a/worker", AdmissionTargetResolutionContextV2{City: baseCity, RuntimeRigSuspended: &runtimeSuspended})
	if err != nil {
		t.Fatalf("ResolveCanonicalAdmissionPoolV2(base): %v", err)
	}
	if target.AgentMaxActiveSessions != nil || target.RigMaxActiveSessions == nil || *target.RigMaxActiveSessions != 3 ||
		target.WorkspaceMaxActiveSessions == nil || *target.WorkspaceMaxActiveSessions != 12 ||
		target.InheritedMaxActiveSessions != 3 || target.InheritedMaxSource != "rig" {
		t.Fatalf("resolved capacity facts = %+v, want agent<-rig cap 3 and workspace cap 12", target)
	}

	agentLimit := 2
	agentLimitedCity := *baseCity
	agentLimitedCity.Agents = []config.Agent{{Name: "worker", Dir: "rig-a", MinActiveSessions: intPtr(1), MaxActiveSessions: &agentLimit, DefaultSlingFormula: &defaultFormula}}
	agentLimitedTarget, err := ResolveCanonicalAdmissionPoolV2("rig-a/worker", AdmissionTargetResolutionContextV2{City: &agentLimitedCity, RuntimeRigSuspended: &runtimeSuspended})
	if err != nil {
		t.Fatalf("ResolveCanonicalAdmissionPoolV2(agent override): %v", err)
	}
	if agentLimitedTarget.InheritedMaxActiveSessions != 2 || agentLimitedTarget.InheritedMaxSource != "agent" {
		t.Fatalf("agent override capacity = %+v, want agent cap 2 and rig/workspace caps retained", agentLimitedTarget)
	}

	base := validAdmissionPolicyProjectionV2(t)
	base.Target = target
	baseDigest, err := DigestAdmissionPolicyV2(base)
	if err != nil {
		t.Fatalf("DigestAdmissionPolicyV2(base): %v", err)
	}

	changedRig := *baseCity
	changedRig.Rigs = append([]config.Rig(nil), baseCity.Rigs...)
	changedRig.Rigs[0].MaxActiveSessions = intPtr(4)
	changedTarget, err := ResolveCanonicalAdmissionPoolV2("rig-a/worker", AdmissionTargetResolutionContextV2{City: &changedRig, RuntimeRigSuspended: &runtimeSuspended})
	if err != nil {
		t.Fatalf("ResolveCanonicalAdmissionPoolV2(changed rig cap): %v", err)
	}
	changed := base
	changed.Target = changedTarget
	changedDigest, err := DigestAdmissionPolicyV2(changed)
	if err != nil {
		t.Fatalf("DigestAdmissionPolicyV2(changed rig cap): %v", err)
	}
	if changedDigest == baseDigest {
		t.Fatal("digest did not change when inherited rig capacity changed")
	}

	workspaceInherited := *baseCity
	workspaceInherited.Rigs = []config.Rig{{Name: "rig-a"}}
	workspaceTarget, err := ResolveCanonicalAdmissionPoolV2("rig-a/worker", AdmissionTargetResolutionContextV2{City: &workspaceInherited, RuntimeRigSuspended: &runtimeSuspended})
	if err != nil {
		t.Fatalf("ResolveCanonicalAdmissionPoolV2(workspace inherited cap): %v", err)
	}
	workspaceBase := base
	workspaceBase.Target = workspaceTarget
	workspaceBaseDigest, err := DigestAdmissionPolicyV2(workspaceBase)
	if err != nil {
		t.Fatalf("DigestAdmissionPolicyV2(workspace inherited cap): %v", err)
	}
	workspaceChanged := workspaceInherited
	workspaceChanged.Workspace.MaxActiveSessions = intPtr(11)
	workspaceChangedTarget, err := ResolveCanonicalAdmissionPoolV2("rig-a/worker", AdmissionTargetResolutionContextV2{City: &workspaceChanged, RuntimeRigSuspended: &runtimeSuspended})
	if err != nil {
		t.Fatalf("ResolveCanonicalAdmissionPoolV2(changed workspace cap): %v", err)
	}
	workspaceBase.Target = workspaceChangedTarget
	workspaceChangedDigest, err := DigestAdmissionPolicyV2(workspaceBase)
	if err != nil {
		t.Fatalf("DigestAdmissionPolicyV2(changed workspace cap): %v", err)
	}
	if workspaceChangedDigest == workspaceBaseDigest {
		t.Fatal("digest did not change when inherited workspace capacity changed")
	}

	changedConfigSuspension := *baseCity
	changedConfigSuspension.Rigs = append([]config.Rig(nil), baseCity.Rigs...)
	changedConfigSuspension.Rigs[0].SuspendedOnStart = true
	configSuspendedTarget, err := ResolveCanonicalAdmissionPoolV2("rig-a/worker", AdmissionTargetResolutionContextV2{City: &changedConfigSuspension, RuntimeRigSuspended: &runtimeSuspended})
	if err != nil {
		t.Fatalf("ResolveCanonicalAdmissionPoolV2(config suspension): %v", err)
	}
	if !configSuspendedTarget.ConfigRigSuspendedOnStart || configSuspendedTarget.RuntimeRigSuspended {
		t.Fatalf("suspension facts = %+v, want config start-suspended and runtime resumed", configSuspendedTarget)
	}
	changed = base
	changed.Target = configSuspendedTarget
	configSuspendedDigest, err := DigestAdmissionPolicyV2(changed)
	if err != nil {
		t.Fatalf("DigestAdmissionPolicyV2(config suspension): %v", err)
	}
	if configSuspendedDigest == baseDigest {
		t.Fatal("digest did not change when rig config suspension changed")
	}

	if _, err := ResolveCanonicalAdmissionPoolV2("rig-a/worker", AdmissionTargetResolutionContextV2{City: baseCity}); err == nil {
		t.Fatal("resolver accepted unknown runtime rig suspension")
	}
	zero := 0
	for _, zeroRig := range []bool{true, false} {
		zeroCapacityCity := &config.City{
			Agents:    []config.Agent{{Name: "worker", Dir: "rig-a", MinActiveSessions: intPtr(1), MaxActiveSessions: &agentLimit}},
			Workspace: config.Workspace{},
			Rigs:      []config.Rig{{Name: "rig-a"}},
		}
		if zeroRig {
			zeroCapacityCity.Rigs[0].MaxActiveSessions = &zero
		} else {
			zeroCapacityCity.Workspace.MaxActiveSessions = &zero
		}
		if _, err := ResolveCanonicalAdmissionPoolV2("rig-a/worker", AdmissionTargetResolutionContextV2{City: zeroCapacityCity, RuntimeRigSuspended: &runtimeSuspended}); err == nil {
			t.Fatalf("resolver accepted a zero %s capacity despite agent override", map[bool]string{true: "rig", false: "workspace"}[zeroRig])
		}
	}
	runtimeSuspended = true
	if _, err := ResolveCanonicalAdmissionPoolV2("rig-a/worker", AdmissionTargetResolutionContextV2{City: baseCity, RuntimeRigSuspended: &runtimeSuspended}); err == nil {
		t.Fatal("resolver accepted a runtime-suspended rig")
	}
}

func TestResolveCanonicalAdmissionPoolV2AcceptsPinnedBuiltInSlingQuery(t *testing.T) {
	maxOne := 1
	defaultFormula := "mol-work"
	pool := config.Agent{Name: "worker", Dir: "rig-a", MinActiveSessions: &maxOne, DefaultSlingFormula: &defaultFormula}
	pool.SlingQuery = "  " + strings.Join(strings.Fields(pool.DefaultSlingQuery()), "   ") + "  "
	city := &config.City{Agents: []config.Agent{pool}, Rigs: []config.Rig{{Name: "rig-a"}}}
	runtimeSuspended := false
	target, err := ResolveCanonicalAdmissionPoolV2("rig-a/worker", AdmissionTargetResolutionContextV2{City: city, RuntimeRigSuspended: &runtimeSuspended})
	if err != nil {
		t.Fatalf("resolver rejected normalized built-in sling query: %v", err)
	}
	if !target.CustomSlingQueryAbsent {
		t.Fatalf("target facts = %+v, want built-in query to be treated as no custom query", target)
	}

	pool.SlingQuery += " --extra"
	city.Agents = []config.Agent{pool}
	if _, err := ResolveCanonicalAdmissionPoolV2("rig-a/worker", AdmissionTargetResolutionContextV2{City: city, RuntimeRigSuspended: &runtimeSuspended}); err == nil {
		t.Fatal("resolver accepted a custom sling query")
	}
}

func TestResolveCanonicalAdmissionPoolV2RequiresCanonicalDefaultFormula(t *testing.T) {
	empty := ""
	spaced := " mol-work"
	unsafe := "../mol-work"
	tests := []struct {
		name    string
		formula *string
	}{
		{name: "unset"},
		{name: "explicitly empty", formula: &empty},
		{name: "noncanonical whitespace", formula: &spaced},
		{name: "unsafe path", formula: &unsafe},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			runtimeSuspended := false
			city := &config.City{
				Agents: []config.Agent{{Name: "worker", Dir: "rig-a", MinActiveSessions: intPtr(1), DefaultSlingFormula: test.formula}},
				Rigs:   []config.Rig{{Name: "rig-a"}},
			}
			context := AdmissionTargetResolutionContextV2{City: city, RuntimeRigSuspended: &runtimeSuspended}
			if _, err := ResolveCanonicalAdmissionPoolV2("rig-a/worker", context); err == nil {
				t.Fatal("resolver accepted a missing or noncanonical effective default formula")
			}
		})
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
	suspended := base
	suspended.Suspended = true
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
		{name: "agent suspended", identity: "rig-a/worker", agents: []config.Agent{suspended}},
		{name: "legacy bound alias", identity: "rig-a/pack.worker", agents: []config.Agent{bound}},
		{name: "custom sling query", identity: "rig-a/worker", agents: []config.Agent{customQuery}},
		{name: "generic ephemeral disabled", identity: "rig-a/worker", agents: []config.Agent{noGeneric}},
		{name: "invalid capacity bounds", identity: "rig-a/worker", agents: []config.Agent{{Name: "worker", Dir: "rig-a", MinActiveSessions: intPtr(2), MaxActiveSessions: intPtr(1)}}},
		{name: "not a pool template", identity: "rig-a/worker", agents: []config.Agent{singleton}},
		{name: "city scoped", identity: "worker", agents: []config.Agent{{Name: "worker"}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := ResolveCanonicalAdmissionPoolV2(test.identity, admissionTargetContext(test.agents)); err == nil {
				t.Fatalf("ResolveCanonicalAdmissionPoolV2(%q) unexpectedly succeeded", test.identity)
			}
		})
	}
}

func TestResolveCanonicalAdmissionPoolV2RequiresUniqueConfiguredRig(t *testing.T) {
	maxOne := 1
	agent := config.Agent{Name: "worker", Dir: "rig-a", MinActiveSessions: &maxOne}
	runtimeSuspended := false
	city := &config.City{Agents: []config.Agent{agent}}
	if _, err := ResolveCanonicalAdmissionPoolV2("rig-a/worker", AdmissionTargetResolutionContextV2{City: city, RuntimeRigSuspended: &runtimeSuspended}); err == nil {
		t.Fatal("resolver accepted a target whose rig is absent from the city")
	}
	city.Rigs = []config.Rig{{Name: "rig-a"}, {Name: "rig-a"}}
	if _, err := ResolveCanonicalAdmissionPoolV2("rig-a/worker", AdmissionTargetResolutionContextV2{City: city, RuntimeRigSuspended: &runtimeSuspended}); err == nil {
		t.Fatal("resolver accepted an ambiguous duplicate rig configuration")
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
			if _, err := ResolveCanonicalAdmissionPoolV2(test.identity, admissionTargetContext(test.agents)); err == nil {
				t.Fatalf("ResolveCanonicalAdmissionPoolV2(%q) unexpectedly succeeded", test.identity)
			}
		})
	}
}

func TestDigestAdmissionPolicyV2IsStableForMapSourceOrderAndFilesystemPath(t *testing.T) {
	first := validAdmissionPolicyProjectionV2(t)
	first.FormulaSources = []AdmissionFormulaSourceV2{
		{LogicalID: "mol-parent", SHA256: strings.Repeat("b", 64)},
		{LogicalID: "mol-work", SHA256: strings.Repeat("a", 64)},
	}
	first.EffectiveCompileVariables = map[string]string{"component": "api", "mode": "safe"}

	second := validAdmissionPolicyProjectionV2(t)
	second.FormulaSources = []AdmissionFormulaSourceV2{
		{LogicalID: "mol-work", SHA256: strings.Repeat("a", 64)},
		{LogicalID: "mol-parent", SHA256: strings.Repeat("b", 64)},
	}
	second.EffectiveCompileVariables = map[string]string{"mode": "safe", "component": "api"}
	first.ExternalAssets[0], first.ExternalAssets[3] = first.ExternalAssets[3], first.ExternalAssets[0]
	first.CheckClosures[0].DependencyLogicalIDs[0], first.CheckClosures[0].DependencyLogicalIDs[1] = first.CheckClosures[0].DependencyLogicalIDs[1], first.CheckClosures[0].DependencyLogicalIDs[0]
	first.CheckClosures[0], first.CheckClosures[1] = first.CheckClosures[1], first.CheckClosures[0]

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
	base := validAdmissionPolicyProjectionV2(t)
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
			p.StorePlacement.GraphStoreRef = "city:other-city"
			p.StorePlacement.WorkflowStoreRef = "city:other-city"
		}},
		{name: "target identity", mutate: func(p *AdmissionPolicyProjectionV2) { p.Target.Identity = "rig-b/worker" }},
		{name: "effective workflow", mutate: func(p *AdmissionPolicyProjectionV2) {
			p.Workflow = "mol-other"
			p.Target.DefaultSlingFormula = "mol-other"
			p.FormulaSources = append(p.FormulaSources, AdmissionFormulaSourceV2{LogicalID: "mol-other", SHA256: strings.Repeat("d", 64)})
			p.FormulaSourceCount++
		}},
		{name: "compiler capability", mutate: func(p *AdmissionPolicyProjectionV2) { p.FormulaCompilerCapability = "2.1.0" }},
		{name: "formula schema", mutate: func(p *AdmissionPolicyProjectionV2) { p.FormulaSchemaVersion = "formula.v2" }},
		{name: "formula v2 mode", mutate: func(p *AdmissionPolicyProjectionV2) { p.FormulaV2Enabled = false }},
		{name: "compile variable", mutate: func(p *AdmissionPolicyProjectionV2) { p.EffectiveCompileVariables["component"] = "worker" }},
		{name: "composition input", mutate: func(p *AdmissionPolicyProjectionV2) {
			p.EffectiveComposedFormulaIDs = append(p.EffectiveComposedFormulaIDs, "mol-aspect")
			p.FormulaSources = append(p.FormulaSources, AdmissionFormulaSourceV2{LogicalID: "mol-aspect", SHA256: strings.Repeat("e", 64)})
			p.FormulaSourceCount++
		}},
		{name: "merge behavior", mutate: func(p *AdmissionPolicyProjectionV2) { p.MergeStrategy = "direct" }},
		{name: "pool capacity", mutate: func(p *AdmissionPolicyProjectionV2) {
			p.Target.MaxActiveSessions = intPtr(3)
			p.Target.AgentMaxActiveSessions = intPtr(3)
			p.Target.InheritedMaxActiveSessions = 3
			p.Target.InheritedMaxSource = "agent"
		}},
		{name: "minimum pool capacity", mutate: func(p *AdmissionPolicyProjectionV2) { p.Target.MinActiveSessions = 2 }},
		{name: "source store placement", mutate: func(p *AdmissionPolicyProjectionV2) {
			p.SourceScope = ScopeForStore("city-a", "rig:other")
			p.StorePlacement.SourceStoreRef = "rig:other"
		}},
		{name: "resolved storage plan", mutate: func(p *AdmissionPolicyProjectionV2) {
			p.StorePlacement.ResolvedStoragePlan = admissionStoragePlanBindingForTest(t, "another-storage-plan")
		}},
		{name: "source-rig graph placement", mutate: func(p *AdmissionPolicyProjectionV2) {
			p.StorePlacement.GraphPlacementMode = AdmissionGraphPlacementSourceRig
			p.StorePlacement.GraphStoreRef = p.StorePlacement.SourceStoreRef
			p.StorePlacement.WorkflowStoreRef = p.StorePlacement.SourceStoreRef
		}},
		{name: "workflow placement mode", mutate: func(p *AdmissionPolicyProjectionV2) {
			p.StorePlacement.WorkflowPlacementMode = "source"
			p.StorePlacement.WorkflowStoreRef = p.StorePlacement.SourceStoreRef
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			changed := validAdmissionPolicyProjectionV2(t)
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

func TestDigestAdmissionPolicyV2BindsTargetDefaultFormula(t *testing.T) {
	base := validAdmissionPolicyProjectionV2(t)
	runtimeSuspended := false
	defaultFormula := "mol-work"
	city := &config.City{
		Agents: []config.Agent{{Name: "worker", Dir: "rig-a", MinActiveSessions: intPtr(1), DefaultSlingFormula: &defaultFormula}},
		Rigs:   []config.Rig{{Name: "rig-a"}},
	}
	target, err := ResolveCanonicalAdmissionPoolV2("rig-a/worker", AdmissionTargetResolutionContextV2{
		City: city, RuntimeRigSuspended: &runtimeSuspended,
	})
	if err != nil {
		t.Fatalf("ResolveCanonicalAdmissionPoolV2(base): %v", err)
	}
	base.Target = target
	baseDigest, err := DigestAdmissionPolicyV2(base)
	if err != nil {
		t.Fatalf("DigestAdmissionPolicyV2(base): %v", err)
	}

	changedFormula := "mol-other"
	changedCity := *city
	changedCity.Agents = append([]config.Agent(nil), city.Agents...)
	changedCity.Agents[0].DefaultSlingFormula = &changedFormula
	changedTarget, err := ResolveCanonicalAdmissionPoolV2("rig-a/worker", AdmissionTargetResolutionContextV2{
		City: &changedCity, RuntimeRigSuspended: &runtimeSuspended,
	})
	if err != nil {
		t.Fatalf("ResolveCanonicalAdmissionPoolV2(changed default): %v", err)
	}
	changed := validAdmissionPolicyProjectionV2(t)
	changed.Target = changedTarget
	changed.Workflow = "mol-other"
	changed.FormulaSources[0].LogicalID = "mol-other"
	changedDigest, err := DigestAdmissionPolicyV2(changed)
	if err != nil {
		t.Fatalf("DigestAdmissionPolicyV2(changed target default): %v", err)
	}
	if changedDigest == baseDigest {
		t.Fatal("digest did not change when the target's effective default formula changed")
	}

	mismatched := validAdmissionPolicyProjectionV2(t)
	mismatched.Target = changedTarget
	if _, err := DigestAdmissionPolicyV2(mismatched); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("DigestAdmissionPolicyV2(mismatched workflow) error = %v, want target default mismatch", err)
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
		{name: "missing compiler capability", mutate: func(p *AdmissionPolicyProjectionV2) { p.FormulaCompilerCapability = "" }},
		{name: "missing compiler implementation version", mutate: func(p *AdmissionPolicyProjectionV2) { p.FormulaCompilerImplementationVersion = "" }},
		{name: "unsupported compiler implementation version", mutate: func(p *AdmissionPolicyProjectionV2) {
			p.FormulaCompilerImplementationVersion = "gascity-formula-compiler-v2"
		}},
		{name: "missing provenance schema version", mutate: func(p *AdmissionPolicyProjectionV2) { p.FormulaProvenanceSchemaVersion = 0 }},
		{name: "unsupported provenance schema version", mutate: func(p *AdmissionPolicyProjectionV2) { p.FormulaProvenanceSchemaVersion++ }},
		{name: "missing schema version", mutate: func(p *AdmissionPolicyProjectionV2) { p.FormulaSchemaVersion = "" }},
		{name: "unsupported merge behavior", mutate: func(p *AdmissionPolicyProjectionV2) { p.MergeStrategy = "unknown" }},
		{name: "missing source store", mutate: func(p *AdmissionPolicyProjectionV2) { p.StorePlacement.SourceStoreRef = "" }},
		{name: "unsupported store ref kind", mutate: func(p *AdmissionPolicyProjectionV2) { p.StorePlacement.GraphStoreRef = "graph:arbitrary-binding" }},
		{name: "graph store ref path", mutate: func(p *AdmissionPolicyProjectionV2) { p.StorePlacement.GraphStoreRef = "city:other/path" }},
		{name: "unknown graph placement", mutate: func(p *AdmissionPolicyProjectionV2) { p.StorePlacement.GraphPlacementMode = "unknown" }},
		{name: "missing resolved plan proof", mutate: func(p *AdmissionPolicyProjectionV2) {
			p.StorePlacement.ResolvedStoragePlan = AdmissionResolvedStoragePlanV2{}
		}},
		{name: "scope store mismatch", mutate: func(p *AdmissionPolicyProjectionV2) { p.SourceScope = ScopeForStore("city-a", "rig:other") }},
		{name: "scope with extra store path", mutate: func(p *AdmissionPolicyProjectionV2) { p.SourceScope = "city:city-a/rig:source/child" }},
		{name: "scope with empty city", mutate: func(p *AdmissionPolicyProjectionV2) { p.SourceScope = "city:/rig:source" }},
		{name: "scope with noncanonical whitespace", mutate: func(p *AdmissionPolicyProjectionV2) { p.SourceScope = "city:city-a /rig:source" }},
		{name: "workflow store mismatch", mutate: func(p *AdmissionPolicyProjectionV2) { p.StorePlacement.WorkflowStoreRef = "rig:other" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := validAdmissionPolicyProjectionV2(t)
			test.mutate(&input)
			if _, err := DigestAdmissionPolicyV2(input); err == nil {
				t.Fatalf("DigestAdmissionPolicyV2() unexpectedly accepted %s", test.name)
			}
		})
	}
}

func TestDigestAdmissionPolicyV2BindsExternalCheckAssetClosure(t *testing.T) {
	base := validAdmissionPolicyProjectionV2(t)
	baseDigest, err := DigestAdmissionPolicyV2(base)
	if err != nil {
		t.Fatalf("DigestAdmissionPolicyV2(base): %v", err)
	}
	changed := validAdmissionPolicyProjectionV2(t)
	changed.ExternalAssets[0].SHA256 = strings.Repeat("c", 64)
	changedDigest, err := DigestAdmissionPolicyV2(changed)
	if err != nil {
		t.Fatalf("DigestAdmissionPolicyV2(changed asset bytes): %v", err)
	}
	if baseDigest == changedDigest {
		t.Fatal("digest did not change when check asset bytes changed")
	}
}

func TestDigestAdmissionPolicyV2RejectsIncompleteExternalAssetClosure(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*AdmissionPolicyProjectionV2)
	}{
		{name: "asset count mismatch", mutate: func(p *AdmissionPolicyProjectionV2) { p.ExternalAssetCount++ }},
		{name: "absolute external asset path as logical ID", mutate: func(p *AdmissionPolicyProjectionV2) { p.ExternalAssets[0].LogicalID = "/tmp/check.sh" }},
		{name: "asset closure not complete", mutate: func(p *AdmissionPolicyProjectionV2) { p.ExternalAssetClosureComplete = false }},
		{name: "check mapping unavailable", mutate: func(p *AdmissionPolicyProjectionV2) { p.CheckMappingsComplete = false }},
		{name: "check count mismatch", mutate: func(p *AdmissionPolicyProjectionV2) { p.CheckPathCount++ }},
		{name: "dependency closure not complete", mutate: func(p *AdmissionPolicyProjectionV2) { p.CheckClosures[0].DependenciesComplete = false }},
		{name: "missing dependency asset mapping", mutate: func(p *AdmissionPolicyProjectionV2) {
			p.CheckClosures[0].DependencyLogicalIDs = []string{"check-missing-dependency"}
		}},
		{name: "dependency count mismatch", mutate: func(p *AdmissionPolicyProjectionV2) { p.CheckClosures[0].DependencyCount++ }},
		{name: "duplicate check step mapping", mutate: func(p *AdmissionPolicyProjectionV2) {
			p.CheckClosures = append(p.CheckClosures, p.CheckClosures[0])
			p.CheckPathCount++
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := validAdmissionPolicyProjectionV2(t)
			test.mutate(&input)
			if _, err := DigestAdmissionPolicyV2(input); err == nil {
				t.Fatalf("DigestAdmissionPolicyV2() unexpectedly accepted %s", test.name)
			}
		})
	}
}

func TestAdmissionResolvedStoragePlanV2BindsGraphClassStoreRef(t *testing.T) {
	if _, err := NewAdmissionResolvedStoragePlanV2(nil); err == nil {
		t.Fatal("NewAdmissionResolvedStoragePlanV2 accepted a nil plan")
	}
	input := validAdmissionPolicyProjectionV2(t)
	input.StorePlacement.ResolvedStoragePlan = admissionGraphClassStoragePlanForTest(t)
	input.StorePlacement.GraphStoreRef = "class:g"
	input.StorePlacement.WorkflowStoreRef = "class:g"
	if _, err := DigestAdmissionPolicyV2(input); err != nil {
		t.Fatalf("DigestAdmissionPolicyV2(resolved graph class plan): %v", err)
	}
	input.StorePlacement.GraphStoreRef = "class:fake"
	input.StorePlacement.WorkflowStoreRef = "class:fake"
	if _, err := DigestAdmissionPolicyV2(input); err == nil {
		t.Fatal("DigestAdmissionPolicyV2 accepted a graph store ref inconsistent with its resolved plan")
	}
}

func validAdmissionPolicyProjectionV2(t *testing.T) AdmissionPolicyProjectionV2 {
	t.Helper()
	return AdmissionPolicyProjectionV2{
		SourceScope:          ScopeForStore("city-a", "rig:source"),
		RouteResolverVersion: AdmissionRouteResolverV2Version,
		Target: CanonicalAdmissionPoolV2{
			Identity:                   "rig-a/worker",
			DefaultSlingFormula:        "mol-work",
			PoolTemplate:               true,
			SupportsGenericEphemeral:   true,
			CustomSlingQueryAbsent:     true,
			InheritedMaxActiveSessions: -1,
			InheritedMaxSource:         "unlimited",
			RuntimeRigSuspensionKnown:  true,
		},
		Workflow:                             "mol-work",
		FormulaCompilerCapability:            "2.0.0",
		FormulaCompilerImplementationVersion: formula.FormulaCompilerImplementationVersion,
		FormulaProvenanceSchemaVersion:       formula.CompileProvenanceSchemaVersion,
		FormulaSchemaVersion:                 "graph.v2",
		FormulaV2Enabled:                     true,
		FormulaSources: []AdmissionFormulaSourceV2{
			{LogicalID: "mol-work", SHA256: strings.Repeat("a", 64)},
			{LogicalID: "mol-parent", SHA256: strings.Repeat("b", 64)},
		},
		FormulaSourceCount: 2,
		ExternalAssets: []AdmissionExternalAssetV2{
			{LogicalID: "check-implement", SHA256: strings.Repeat("d", 64)},
			{LogicalID: "check-verify", SHA256: strings.Repeat("f", 64)},
			{LogicalID: "script-lib/common", SHA256: strings.Repeat("e", 64)},
			{LogicalID: "script-lib/extra", SHA256: strings.Repeat("c", 64)},
		},
		ExternalAssetCount:           4,
		ExternalAssetClosureComplete: true,
		CheckPathCount:               2,
		CheckMappingsComplete:        true,
		CheckClosures: []AdmissionCheckClosureV2{
			{
				StepID:               "mol-work/implement",
				CheckAssetLogicalID:  "check-implement",
				DependencyLogicalIDs: []string{"script-lib/common", "script-lib/extra"},
				DependencyCount:      2,
				DependenciesComplete: true,
			},
			{
				StepID:               "mol-work/verify",
				CheckAssetLogicalID:  "check-verify",
				DependencyCount:      0,
				DependenciesComplete: true,
			},
		},
		EffectiveCompileVariables:   map[string]string{"component": "api"},
		EffectiveComposedFormulaIDs: []string{"mol-parent"},
		MergeStrategy:               "mr",
		StorePlacement: AdmissionStorePlacementV2{
			SourceStoreRef:        "rig:source",
			GraphPlacementMode:    "graph-class",
			ResolvedStoragePlan:   admissionStoragePlanBindingForTest(t, "default-storage-plan"),
			GraphStoreRef:         "city:city-a",
			WorkflowPlacementMode: "graph",
			WorkflowStoreRef:      "city:city-a",
		},
	}
}

func admissionStoragePlanBindingForTest(t *testing.T, context string) AdmissionResolvedStoragePlanV2 {
	t.Helper()
	registry := storebinding.NewProviderRegistry()
	if err := registry.Freeze(); err != nil {
		t.Fatalf("freeze storage provider registry: %v", err)
	}
	city := &config.City{}
	seed := byte('a')
	if context != "default-storage-plan" {
		seed = 'b'
	}
	pins := storebinding.WorkPinInputs{
		Recorded:      true,
		ConfigContext: storebinding.ConfigRefDigest("sha256:" + strings.Repeat(string(seed), 64)),
		HQ: storebinding.WorkScopePin{
			Scope:       storebinding.HQScope(),
			Prefix:      "hq",
			OpenerID:    "beads",
			ComponentID: "work",
			PhysicalID:  "hq-physical-" + context,
		},
	}
	plan, err := storebinding.ResolveStoragePlan(registry, city.EffectiveStorage(), pins, "")
	if err != nil {
		t.Fatalf("resolve storage plan: %v", err)
	}
	proof, err := NewAdmissionResolvedStoragePlanV2(plan)
	if err != nil {
		t.Fatalf("construct storage plan proof: %v", err)
	}
	return proof
}

func admissionGraphClassStoragePlanForTest(t *testing.T) AdmissionResolvedStoragePlanV2 {
	t.Helper()
	registry := storebinding.NewProviderRegistry()
	if err := registry.Register(admissionTestProviderFactory{}); err != nil {
		t.Fatalf("register graph provider: %v", err)
	}
	if err := registry.Freeze(); err != nil {
		t.Fatalf("freeze graph provider registry: %v", err)
	}
	city := &config.City{Storage: &config.StorageConfig{
		Classes: config.StorageClasses{
			Work:      config.StorageWorkBinding,
			Graph:     "infra",
			Sessions:  config.StorageWorkBinding,
			Messaging: config.StorageWorkBinding,
			Orders:    config.StorageWorkBinding,
			Nudges:    config.StorageWorkBinding,
		},
		Bindings: map[string]config.StorageBindingConfig{
			"infra": {Provider: "admission-test-provider", ConfigRef: "graph-store"},
		},
	}}
	pins := storebinding.WorkPinInputs{
		Recorded:      true,
		ConfigContext: storebinding.ConfigRefDigest("sha256:" + strings.Repeat("a", 64)),
		HQ: storebinding.WorkScopePin{
			Scope:       storebinding.HQScope(),
			Prefix:      "hq",
			OpenerID:    "beads",
			ComponentID: "work",
			PhysicalID:  "hq-physical",
		},
	}
	plan, err := storebinding.ResolveStoragePlan(registry, city.EffectiveStorage(), pins, "")
	if err != nil {
		t.Fatalf("resolve graph-class storage plan: %v", err)
	}
	proof, err := NewAdmissionResolvedStoragePlanV2(plan)
	if err != nil {
		t.Fatalf("construct graph-class storage plan proof: %v", err)
	}
	return proof
}

type admissionTestProviderFactory struct{}

func (admissionTestProviderFactory) ID() storebinding.ProviderID { return "admission-test-provider" }

func (admissionTestProviderFactory) New(storebinding.BindingSpec) (storebinding.Provider, error) {
	return admissionTestProvider{}, nil
}

type admissionTestProvider struct{}

func (admissionTestProvider) Inspect(context.Context, storebinding.BindingSpec) (storebinding.Inspection, error) {
	return storebinding.Inspection{}, storebinding.ErrProviderUnavailable
}

func (admissionTestProvider) InspectFenced(context.Context, storebinding.FencedInspectionRequest) (storebinding.Descriptor, error) {
	return storebinding.Descriptor{}, storebinding.ErrProviderUnavailable
}

func (admissionTestProvider) AcquireFence(context.Context, storebinding.MigrationGuardClaim, storebinding.FenceRequest) (storebinding.WriterFence, error) {
	return nil, storebinding.ErrProviderUnavailable
}

func (admissionTestProvider) RetainedGuards() (storebinding.RetainedGuardLifecycle, bool) {
	return nil, false
}

func (admissionTestProvider) BindingMigration() (storebinding.BindingMigrationLifecycle, bool) {
	return nil, false
}

func (admissionTestProvider) WorkMigration() (storebinding.WorkMigrationLifecycle, bool) {
	return nil, false
}

func (admissionTestProvider) Open(context.Context, storebinding.OpenRequest) (storebinding.OpenedBinding, error) {
	return nil, storebinding.ErrProviderUnavailable
}

func admissionTargetContext(agents []config.Agent) AdmissionTargetResolutionContextV2 {
	runtimeSuspended := false
	for index := range agents {
		if agents[index].DefaultSlingFormula == nil {
			defaultFormula := "mol-work"
			agents[index].DefaultSlingFormula = &defaultFormula
		}
	}
	return AdmissionTargetResolutionContextV2{
		City:                &config.City{Agents: agents, Rigs: []config.Rig{{Name: "rig-a"}}},
		RuntimeRigSuspended: &runtimeSuspended,
	}
}

func intPtr(value int) *int {
	return &value
}
