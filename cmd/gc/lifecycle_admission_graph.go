package main

import (
	"context"
	"fmt"
	"maps"
	"reflect"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	convoycore "github.com/gastownhall/gascity/internal/convoy"
	"github.com/gastownhall/gascity/internal/formula"
	"github.com/gastownhall/gascity/internal/graphroute"
	"github.com/gastownhall/gascity/internal/graphv2"
	"github.com/gastownhall/gascity/internal/molecule"
	"github.com/gastownhall/gascity/internal/sling"
	"github.com/gastownhall/gascity/internal/worklifecycle"
)

// verifyLifecycleGraphInvocation checks the generated convoy and both persisted
// variable views against the signed source contract. A convoy id is not trusted
// merely because it appears on a workflow carrying lifecycle lineage.
func verifyLifecycleGraphInvocation(store beads.Store, root beads.Bead, sourceID string, policy worklifecycle.AdmissionPolicyProjectionV2) error {
	if _, err := worklifecycle.DigestAdmissionPolicyV2(policy); err != nil {
		return fmt.Errorf("graph invocation policy: %w", err)
	}
	inv := policy.GraphInvocation
	if store == nil || inv == nil || inv.SourceID != sourceID || policy.FormulaSchemaVersion != beadmeta.FormulaContractGraphV2 {
		return fmt.Errorf("graph invocation has no signed exact source binding")
	}
	convoyID := root.Metadata[beadmeta.InputConvoyIDMetadataKey]
	if convoyID == "" || strings.TrimSpace(convoyID) != convoyID || convoyID == sourceID || convoyID == inv.PreviewConvoyID {
		return fmt.Errorf("graph invocation has no materialized input convoy")
	}
	source, err := store.Get(sourceID)
	if err != nil || source.ID != sourceID || source.Type == "convoy" {
		return fmt.Errorf("graph invocation source is unavailable or is itself a convoy")
	}
	convoy, err := store.Get(convoyID)
	if err != nil || convoy.ID != convoyID || convoy.Type != "convoy" || convoy.ParentID != "" || convoy.Metadata[beadmeta.SyntheticMetadataKey] != "true" {
		return fmt.Errorf("graph invocation input is not its generated synthetic convoy")
	}
	edges, err := store.DepList(convoyID, "down")
	if err != nil || len(edges) != 1 || edges[0].IssueID != convoyID || edges[0].DependsOnID != sourceID || edges[0].Type != convoycore.TrackingDepType {
		return fmt.Errorf("graph invocation input does not track exactly its signed source")
	}
	// Include terminal members so an extra closed or legacy child cannot hide
	// behind the normal ready-work projection.
	members, err := convoycore.Members(store, convoyID, true)
	if err != nil || len(members) != 1 || members[0].ID != sourceID {
		return fmt.Errorf("graph invocation input contains missing or additional members")
	}
	vars := maps.Clone(policy.EffectiveCompileVariables)
	vars[graphv2.ConvoyIDVar] = convoyID
	for key, value := range vars {
		if root.Metadata[beadmeta.FormulaVarPrefix+key] != value {
			return fmt.Errorf("graph invocation variable %q differs from the signed compiler inputs", key)
		}
	}
	for key, value := range root.Metadata {
		if name, ok := strings.CutPrefix(key, beadmeta.FormulaVarPrefix); ok {
			expected, present := vars[name]
			if !present || value != expected {
				return fmt.Errorf("graph invocation contains an unsigned compiler variable %q", name)
			}
		}
	}
	if root.Metadata[graphv2.RuntimeVarsMetadataKey] != graphv2.RuntimeVarsMetadata(vars) {
		return fmt.Errorf("graph invocation runtime inputs differ from the signed compiler inputs")
	}
	scopeKind, scopeRef := lifecycleSlingScope(inv.SourceStoreRef, "")
	if root.Metadata[beadmeta.Graphv2RootKeyMetadataKey] != graphv2.RootKey(convoyID, policy.Workflow, vars, scopeKind, scopeRef) {
		return fmt.Errorf("graph invocation root key differs from its signed scope and inputs")
	}
	return nil
}

// Recompile with the real convoy, after the signed preview invocation has been
// verified. Formula source identities must still match the signed closure.
func verifyLifecycleGraphWorkerStep(ctx context.Context, store beads.Store, source, candidate beads.Bead, policy lifecycleAdmissionPolicy, cfg *config.City) error {
	marker, ok := lifecycleMaterializationFor(source)
	if !ok || marker.WorkflowID == "" {
		return fmt.Errorf("source has no attached workflow")
	}
	root, err := store.Get(marker.WorkflowID)
	if err != nil {
		return err
	}
	if err := verifyLifecycleGraphInvocation(store, root, source.ID, policy.projection); err != nil {
		return err
	}
	vars := maps.Clone(policy.projection.EffectiveCompileVariables)
	vars[graphv2.ConvoyIDVar] = root.Metadata[beadmeta.InputConvoyIDMetadataKey]
	recipe, provenance, err := formula.CompileWithoutRuntimeVarValidationWithProvenance(ctx, policy.projection.Workflow, sling.SlingFormulaSearchPaths(policy.deps, policy.target), vars)
	if err != nil {
		return err
	}
	if recipe == nil || provenance.Status != formula.CompileProvenanceAvailable || provenance.FormulaV2Enabled != policy.projection.FormulaV2Enabled ||
		provenance.CompilerCapability != policy.projection.FormulaCompilerCapability || provenance.CompilerImplementationVersion != policy.projection.FormulaCompilerImplementationVersion ||
		len(provenance.ExternalAssets) != 0 || len(provenance.CheckPaths) != 0 {
		return fmt.Errorf("graph compiler provenance changed")
	}
	sources, err := admissionFormulaSourceClosure(recipe.FormulaSources, provenance.FormulaSources)
	if err != nil || !reflect.DeepEqual(sources, policy.projection.FormulaSources) {
		return fmt.Errorf("graph formula source closure changed")
	}
	scopeKind, scopeRef := lifecycleSlingScope(marker.SourceStoreRef, "")
	if err := graphroute.ApplyGraphRouting(recipe, &policy.target, marker.Route, vars, "", scopeKind, scopeRef, marker.SourceStoreRef, store, censusCityName(cfg), cfg, graphroute.Deps{
		CityPath: policy.deps.CityPath, Resolver: policy.deps.Resolver, DirectSessionResolver: policy.deps.DirectSessionResolver,
	}); err != nil {
		return err
	}
	return molecule.VerifyMaterializedStep(store, root, candidate, recipe, molecule.Options{Vars: vars})
}
