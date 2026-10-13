package molecule

import (
	"fmt"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/formula"
)

// VerifyMaterializedStep checks an existing graph worker against the same
// recipe plan used to create it. The caller supplies an independently verified
// recipe and invocation; persisted step references only locate candidate rows.
// Dynamic expansions that are absent from that recipe are not authorized here.
func VerifyMaterializedStep(store beads.Store, root, candidate beads.Bead, recipe *formula.Recipe, opts Options) error {
	if store == nil || root.ID == "" || candidate.ID == "" || candidate.ID == root.ID {
		return fmt.Errorf("exact workflow and distinct worker step are required")
	}
	plan, graph, rootKey, err := buildRecipeApplyPlan(recipe, opts)
	if err != nil {
		return err
	}
	if !graph {
		return fmt.Errorf("materialized step verification requires a graph recipe")
	}
	rows, err := store.ListByMetadata(map[string]string{beadmeta.RootBeadIDMetadataKey: root.ID}, 0, beads.WithBothTiers)
	if err != nil {
		return err
	}
	ids := map[string]string{rootKey: root.ID}
	for _, row := range rows {
		if row.ID == root.ID {
			continue
		}
		ref := row.Metadata[beadmeta.StepRefMetadataKey]
		if ref == "" {
			return fmt.Errorf("workflow member has no compiled step reference")
		}
		if previous := ids[ref]; previous != "" && previous != row.ID {
			return fmt.Errorf("compiled step reference %q has multiple physical members", ref)
		}
		ids[ref] = row.ID
	}
	ref := candidate.Metadata[beadmeta.StepRefMetadataKey]
	if ids[ref] != candidate.ID || candidate.Metadata[beadmeta.RootBeadIDMetadataKey] != root.ID {
		return fmt.Errorf("candidate is not the unique physical member for its compiled step")
	}
	var expected *beads.GraphApplyNode
	for i := range plan.Nodes {
		if plan.Nodes[i].Key == ref {
			expected = &plan.Nodes[i]
			break
		}
	}
	if expected == nil || expected.Key == rootKey {
		return fmt.Errorf("candidate is absent from the verified recipe")
	}
	if candidate.Type != expected.Type || candidate.Title != expected.Title || candidate.Description != expected.Description {
		return fmt.Errorf("worker step content differs from the verified recipe")
	}
	for key, value := range expected.Metadata {
		if candidate.Metadata[key] != value {
			return fmt.Errorf("worker step metadata %q differs from the verified recipe", key)
		}
	}
	for key, target := range expected.MetadataRefs {
		if ids[target] == "" || candidate.Metadata[key] != ids[target] {
			return fmt.Errorf("worker step reference %q differs from its physical recipe member", key)
		}
	}
	parentID := expected.ParentID
	if expected.ParentKey != "" {
		parentID = ids[expected.ParentKey]
		if parentID == "" {
			return fmt.Errorf("worker step parent is absent from the physical recipe")
		}
	}
	if candidate.ParentID != parentID {
		return fmt.Errorf("worker step parent differs from the verified recipe")
	}
	deps, err := store.DepList(candidate.ID, "down")
	if err != nil {
		return err
	}
	for _, edge := range plan.Edges {
		if edge.FromKey != ref {
			continue
		}
		target := edge.ToID
		if edge.ToKey != "" {
			target = ids[edge.ToKey]
		}
		if target == "" {
			return fmt.Errorf("worker step prerequisite is absent from the physical recipe")
		}
		found := false
		for _, dep := range deps {
			if dep.IssueID == candidate.ID && dep.DependsOnID == target && dep.Type == edge.Type {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("worker step lacks a dependency from the verified recipe")
		}
	}
	return nil
}
