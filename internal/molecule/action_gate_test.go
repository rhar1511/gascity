package molecule

import (
	"context"
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/formula"
)

func TestInstantiateRequiresGateBeforeFirstStoreWrite(t *testing.T) {
	store := beads.NewMemStore()
	recipe := actionGateTestRecipe()
	_, err := Instantiate(context.Background(), store, recipe, Options{RequireActionGate: true})
	if err == nil {
		t.Fatal("Instantiate succeeded without required action gate")
	}
	if got, err := store.ListOpen(); err != nil || len(got) != 0 {
		t.Fatalf("store contains beads after gate refusal: %#v, %v", got, err)
	}
}

func TestInstantiateStampsExactAuthorizationOnEveryCreatedMember(t *testing.T) {
	store := beads.NewMemStore()
	gate := &actionGateTestGate{authorization: FormulaActionAuthorization{
		Required: true, RequestJSON: `{"request_sha256":"request-1"}`, AuthorizationJSON: `{"record_id":"approval-1"}`,
	}}
	result, err := Instantiate(context.Background(), store, actionGateTestRecipe(), Options{
		ActionGate: gate, RequireActionGate: true,
	})
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}
	if result.Created != 2 || gate.recipeCalls != 1 || gate.recipeRevalidateCalls != 1 {
		t.Fatalf("result=%#v recipe gate calls=%d revalidate calls=%d; want two beads and one authorization/revalidation", result, gate.recipeCalls, gate.recipeRevalidateCalls)
	}
	created, err := store.ListOpen()
	if err != nil {
		t.Fatal(err)
	}
	if len(created) != result.Created {
		t.Fatalf("open beads=%d, want %d", len(created), result.Created)
	}
	for _, bead := range created {
		if bead.Metadata[beadmeta.CompatibilityRequestMetadataKey] != gate.authorization.RequestJSON ||
			bead.Metadata[beadmeta.CompatibilityAuthorizationMetadataKey] != gate.authorization.AuthorizationJSON {
			t.Errorf("bead %s lacks exact compatibility authorization: %#v", bead.ID, bead.Metadata)
		}
	}
}

func TestInstantiateRejectsMalformedGateResultBeforeStoreWrite(t *testing.T) {
	store := beads.NewMemStore()
	gate := &actionGateTestGate{authorization: FormulaActionAuthorization{
		Required: true, RequestJSON: `not-json`, AuthorizationJSON: `{"record_id":"approval-1"}`,
	}}
	if _, err := Instantiate(context.Background(), store, actionGateTestRecipe(), Options{ActionGate: gate}); err == nil {
		t.Fatal("Instantiate succeeded with malformed authorization")
	}
	if got, err := store.ListOpen(); err != nil || len(got) != 0 {
		t.Fatalf("store contains beads after malformed authorization: %#v, %v", got, err)
	}
}

func TestInstantiateFragmentRequiresGateBeforeFirstStoreWrite(t *testing.T) {
	store := beads.NewMemStore()
	root, err := store.Create(beads.Bead{Title: "workflow", Type: "workflow"})
	if err != nil {
		t.Fatal(err)
	}
	before, err := store.ListOpen()
	if err != nil {
		t.Fatal(err)
	}
	fragment := &formula.FragmentRecipe{
		Name:  "required-expansion",
		Steps: []formula.RecipeStep{{ID: "required-expansion.item", Title: "item", Type: "task"}},
	}
	_, err = InstantiateFragment(context.Background(), store, fragment, FragmentOptions{RootID: root.ID, RequireActionGate: true})
	if err == nil {
		t.Fatal("InstantiateFragment succeeded without required action gate")
	}
	after, err := store.ListOpen()
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("fragment wrote beads before gate refusal: before=%d after=%d", len(before), len(after))
	}
}

func TestInstantiateRejectsGateErrorsBeforeStoreWrite(t *testing.T) {
	store := beads.NewMemStore()
	gate := &actionGateTestGate{err: errors.New("release record unavailable")}
	if _, err := Instantiate(context.Background(), store, actionGateTestRecipe(), Options{ActionGate: gate, RequireActionGate: true}); err == nil {
		t.Fatal("Instantiate succeeded after gate error")
	}
	if got, err := store.ListOpen(); err != nil || len(got) != 0 {
		t.Fatalf("store contains beads after gate error: %#v, %v", got, err)
	}
}

type actionGateTestGate struct {
	authorization           FormulaActionAuthorization
	err                     error
	recipeCalls             int
	fragmentCalls           int
	recipeRevalidateCalls   int
	fragmentRevalidateCalls int
}

func (g *actionGateTestGate) AuthorizeRecipe(context.Context, *formula.Recipe, beads.Store) (FormulaActionAuthorization, error) {
	g.recipeCalls++
	return g.authorization, g.err
}

func (g *actionGateTestGate) AuthorizeFragment(context.Context, *formula.FragmentRecipe, beads.Store) (FormulaActionAuthorization, error) {
	g.fragmentCalls++
	return g.authorization, g.err
}

func (g *actionGateTestGate) RevalidateRecipe(context.Context, *formula.Recipe, beads.Store, FormulaActionAuthorization) error {
	g.recipeRevalidateCalls++
	return g.err
}

func (g *actionGateTestGate) RevalidateFragment(context.Context, *formula.FragmentRecipe, beads.Store, FormulaActionAuthorization) error {
	g.fragmentRevalidateCalls++
	return g.err
}

func (g *actionGateTestGate) RevalidateBead(context.Context, beads.Bead, beads.Store) error {
	return g.err
}

func actionGateTestRecipe() *formula.Recipe {
	return &formula.Recipe{
		Name: "required-formula",
		Steps: []formula.RecipeStep{
			{ID: "required-formula", Title: "workflow", Type: "molecule", IsRoot: true},
			{ID: "required-formula.work", Title: "work", Type: "task"},
		},
		Deps: []formula.RecipeDep{{StepID: "required-formula.work", DependsOnID: "required-formula", Type: "parent-child"}},
	}
}
