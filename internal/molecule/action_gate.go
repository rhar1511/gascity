package molecule

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/formula"
)

// FormulaActionAuthorization is the controller's immutable approval record
// for one loaded formula. The JSON values are opaque to molecule; the
// controller authority validates them again before execution.
type FormulaActionAuthorization struct {
	Required          bool
	RequestJSON       string
	AuthorizationJSON string
}

// FormulaActionError reports a refused authorization or revalidation. Callers
// must return it without recording retry metadata or quarantining work: those
// mutations require the authorization that this error says is unavailable.
type FormulaActionError struct {
	Err error
}

func (e *FormulaActionError) Error() string {
	return fmt.Sprintf("formula action refused: %v", e.Err)
}

// Unwrap preserves the authority or store error for normal error inspection.
func (e *FormulaActionError) Unwrap() error { return e.Err }

// FormulaActionGate authorizes a formula before molecule writes any beads.
// The fragment method covers dynamically composed expansion descendants.
type FormulaActionGate interface {
	AuthorizeRecipe(context.Context, *formula.Recipe, beads.Store) (FormulaActionAuthorization, error)
	AuthorizeFragment(context.Context, *formula.FragmentRecipe, beads.Store) (FormulaActionAuthorization, error)
	RevalidateRecipe(context.Context, *formula.Recipe, beads.Store, FormulaActionAuthorization) error
	RevalidateFragment(context.Context, *formula.FragmentRecipe, beads.Store, FormulaActionAuthorization) error
	RevalidateBead(context.Context, beads.Bead, beads.Store) error
}

// ValidateExistingFragmentAction authorizes and revalidates a fragment that
// the controller has matched to an existing instance. It performs no bead
// writes, so callers can refuse before wiring that instance into new control
// state. It uses the same normalized fragment as InstantiateFragment.
func ValidateExistingFragmentAction(ctx context.Context, store beads.Store, recipe *formula.FragmentRecipe, opts FragmentOptions) error {
	if recipe == nil {
		return fmt.Errorf("recipe is nil")
	}
	if opts.RootID == "" {
		return fmt.Errorf("fragment validation requires RootID")
	}
	if len(recipe.Steps) == 0 {
		return nil
	}
	prepared, err := prepareFragmentForStore(store, recipe, opts.RootID, opts.ExternalDeps)
	if err != nil {
		return err
	}
	gate := opts.ActionGate
	if opts.ActionGateForStore != nil {
		gate = opts.ActionGateForStore(store)
	}
	authorization, err := authorizeFragmentAction(ctx, gate, prepared, store, opts.RequireActionGate)
	if err != nil {
		return err
	}
	return revalidateFragmentAction(ctx, gate, prepared, store, authorization)
}

func revalidateRecipeAction(ctx context.Context, gate FormulaActionGate, recipe *formula.Recipe, store beads.Store, authorization *FormulaActionAuthorization) error {
	if authorization == nil || !authorization.Required {
		return nil
	}
	if gate == nil {
		return &FormulaActionError{Err: fmt.Errorf("formula compatibility gate is unavailable")}
	}
	if err := gate.RevalidateRecipe(ctx, recipe, store, *authorization); err != nil {
		return &FormulaActionError{Err: fmt.Errorf("revalidating formula compatibility: %w", err)}
	}
	return nil
}

func revalidateFragmentAction(ctx context.Context, gate FormulaActionGate, recipe *formula.FragmentRecipe, store beads.Store, authorization *FormulaActionAuthorization) error {
	if authorization == nil || !authorization.Required {
		return nil
	}
	if gate == nil {
		return &FormulaActionError{Err: fmt.Errorf("fragment compatibility gate is unavailable")}
	}
	if err := gate.RevalidateFragment(ctx, recipe, store, *authorization); err != nil {
		return &FormulaActionError{Err: fmt.Errorf("revalidating fragment compatibility: %w", err)}
	}
	return nil
}

func authorizeRecipeAction(ctx context.Context, gate FormulaActionGate, recipe *formula.Recipe, store beads.Store, required bool) (*FormulaActionAuthorization, error) {
	if gate == nil {
		if required {
			return nil, &FormulaActionError{Err: fmt.Errorf("formula compatibility gate is unavailable")}
		}
		return nil, nil
	}
	authorization, err := gate.AuthorizeRecipe(ctx, recipe, store)
	if err != nil {
		return nil, &FormulaActionError{Err: err}
	}
	if err := validateFormulaActionAuthorization(authorization); err != nil {
		return nil, &FormulaActionError{Err: err}
	}
	if !authorization.Required {
		return nil, nil
	}
	return &authorization, nil
}

func authorizeFragmentAction(ctx context.Context, gate FormulaActionGate, recipe *formula.FragmentRecipe, store beads.Store, required bool) (*FormulaActionAuthorization, error) {
	if gate == nil {
		if required {
			return nil, &FormulaActionError{Err: fmt.Errorf("fragment compatibility gate is unavailable")}
		}
		return nil, nil
	}
	authorization, err := gate.AuthorizeFragment(ctx, recipe, store)
	if err != nil {
		return nil, &FormulaActionError{Err: err}
	}
	if err := validateFormulaActionAuthorization(authorization); err != nil {
		return nil, &FormulaActionError{Err: err}
	}
	if !authorization.Required {
		return nil, nil
	}
	return &authorization, nil
}

func validateFormulaActionAuthorization(authorization FormulaActionAuthorization) error {
	if !authorization.Required {
		if authorization.RequestJSON != "" || authorization.AuthorizationJSON != "" {
			return fmt.Errorf("ordinary formula has compatibility approval metadata")
		}
		return nil
	}
	for label, raw := range map[string]string{
		"request":       authorization.RequestJSON,
		"authorization": authorization.AuthorizationJSON,
	} {
		var object map[string]json.RawMessage
		if err := json.Unmarshal([]byte(raw), &object); err != nil || len(object) == 0 {
			return fmt.Errorf("required formula compatibility %s is malformed", label)
		}
	}
	return nil
}

func stampFormulaActionAuthorization(metadata map[string]string, authorization *FormulaActionAuthorization) {
	if authorization == nil || !authorization.Required {
		return
	}
	metadata[beadmeta.CompatibilityRequestMetadataKey] = authorization.RequestJSON
	metadata[beadmeta.CompatibilityAuthorizationMetadataKey] = authorization.AuthorizationJSON
}
