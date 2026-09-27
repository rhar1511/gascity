package dispatch

import (
	"context"
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/formula"
	"github.com/gastownhall/gascity/internal/molecule"
)

type dispatchActionGate struct {
	err   error
	calls int
}

func (g *dispatchActionGate) AuthorizeRecipe(context.Context, *formula.Recipe, beads.Store) (molecule.FormulaActionAuthorization, error) {
	return molecule.FormulaActionAuthorization{}, g.err
}

func (g *dispatchActionGate) AuthorizeFragment(context.Context, *formula.FragmentRecipe, beads.Store) (molecule.FormulaActionAuthorization, error) {
	return molecule.FormulaActionAuthorization{}, g.err
}

func (g *dispatchActionGate) RevalidateRecipe(context.Context, *formula.Recipe, beads.Store, molecule.FormulaActionAuthorization) error {
	return g.err
}

func (g *dispatchActionGate) RevalidateFragment(context.Context, *formula.FragmentRecipe, beads.Store, molecule.FormulaActionAuthorization) error {
	return g.err
}

func (g *dispatchActionGate) RevalidateBead(context.Context, beads.Bead, beads.Store) error {
	g.calls++
	return g.err
}

func TestProcessControlRevalidatesBeforeOrphanClose(t *testing.T) {
	store := beads.NewMemStore()
	control, err := store.Create(beads.Bead{
		Title: "retry control",
		Type:  "task",
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:         beadmeta.KindRetry,
			beadmeta.RootBeadIDMetadataKey:   "missing-root",
			beadmeta.RootStoreRefMetadataKey: "city:test",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	gate := &dispatchActionGate{err: errors.New("release authorization revoked")}
	_, err = ProcessControl(store, control, ProcessOptions{FormulaActionGate: gate})
	if err == nil {
		t.Fatal("ProcessControl succeeded after formula action authorization was revoked")
	}
	if gate.calls != 1 {
		t.Fatalf("RevalidateBead calls = %d, want 1", gate.calls)
	}
	current, err := store.Get(control.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Status != "open" {
		t.Fatalf("control status after gate refusal = %q, want open", current.Status)
	}
}

func TestProcessControlRequiresConfiguredGateWhenRequested(t *testing.T) {
	store := beads.NewMemStore()
	control, err := store.Create(beads.Bead{Title: "retry control", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = ProcessControl(store, control, ProcessOptions{RequireFormulaActionGate: true})
	if err == nil {
		t.Fatal("ProcessControl succeeded with a required gate missing")
	}
	current, err := store.Get(control.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Status != "open" {
		t.Fatalf("control status after missing gate = %q, want open", current.Status)
	}
}
