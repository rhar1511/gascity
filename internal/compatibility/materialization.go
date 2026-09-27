package compatibility

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/formula"
	"github.com/gastownhall/gascity/internal/molecule"
	"github.com/gastownhall/gascity/internal/qualification"
)

// MaterializationGate binds one loaded city configuration to the generic
// formula gate used by molecule. It deliberately defaults to an unavailable
// authority; a local pack lock is never an authorizer.
type MaterializationGate struct {
	Gate   Gate
	Config *config.City
	// Current returns one published config, qualification snapshot, build, and
	// trusted-authority tuple for runtime callers. Static CLI callers may leave
	// it nil and retain the constructor's authority (normally nil).
	Current func() (*config.City, qualification.Snapshot, qualification.BuildIdentity, qualification.CompatibilityAuthority, error)
}

// AuthorizeRecipe applies the active controller compatibility policy to one
// compiled formula and its exact destination store.
func (g MaterializationGate) AuthorizeRecipe(ctx context.Context, recipe *formula.Recipe, store beads.Store) (molecule.FormulaActionAuthorization, error) {
	gate, cfg, err := g.activeGate()
	if err != nil {
		return molecule.FormulaActionAuthorization{}, err
	}
	result, err := gate.AuthorizeFormula(ctx, cfg, recipe, store)
	if err != nil {
		return molecule.FormulaActionAuthorization{}, err
	}
	return moleculeAuthorizationMetadata(result)
}

// RevalidateRecipe checks a previously authorized formula against the active
// controller identity, current policy, and destination-store evidence.
func (g MaterializationGate) RevalidateRecipe(ctx context.Context, recipe *formula.Recipe, store beads.Store, authorization molecule.FormulaActionAuthorization) error {
	return g.revalidateRecipe(ctx, recipe, store, authorization)
}

// AuthorizeFragment applies the active controller compatibility policy to one
// dynamically compiled fragment and its exact destination store.
func (g MaterializationGate) AuthorizeFragment(ctx context.Context, fragment *formula.FragmentRecipe, store beads.Store) (molecule.FormulaActionAuthorization, error) {
	if fragment == nil {
		return molecule.FormulaActionAuthorization{Required: true}, qualification.ErrUnavailable
	}
	recipe := recipeForFragment(fragment)
	gate, cfg, err := g.activeGate()
	if err != nil {
		return molecule.FormulaActionAuthorization{}, err
	}
	result, err := gate.AuthorizeFormula(ctx, cfg, recipe, store)
	if err != nil {
		return molecule.FormulaActionAuthorization{}, err
	}
	return moleculeAuthorizationMetadata(result)
}

// RevalidateFragment checks a previously authorized fragment against the
// current formula source, controller identity, and destination-store evidence.
func (g MaterializationGate) RevalidateFragment(ctx context.Context, fragment *formula.FragmentRecipe, store beads.Store, authorization molecule.FormulaActionAuthorization) error {
	if fragment == nil {
		return qualification.ErrUnavailable
	}
	return g.revalidateRecipe(ctx, recipeForFragment(fragment), store, authorization)
}

// RevalidateBead checks persisted approval metadata or verifies that a bead's
// source does not now require approval before a later action.
func (g MaterializationGate) RevalidateBead(ctx context.Context, bead beads.Bead, store beads.Store) error {
	requestJSON := bead.Metadata[beadmeta.CompatibilityRequestMetadataKey]
	authorizationJSON := bead.Metadata[beadmeta.CompatibilityAuthorizationMetadataKey]
	if requestJSON != "" || authorizationJSON != "" {
		if requestJSON == "" || authorizationJSON == "" {
			return fmt.Errorf("compatibility approval metadata is incomplete: %w", qualification.ErrUnavailable)
		}
		return g.RevalidateActionMetadata(ctx, requestJSON, authorizationJSON, store)
	}

	source := strings.TrimSpace(bead.Metadata[beadmeta.FormulaSourceMetadataKey])
	if source == "" {
		rootID := strings.TrimSpace(bead.Metadata[beadmeta.RootBeadIDMetadataKey])
		if rootID == "" {
			return nil
		}
		root, err := store.Get(rootID)
		if err != nil {
			return fmt.Errorf("checking formula compatibility on workflow root %s: %w", rootID, qualification.ErrUnavailable)
		}
		source = strings.TrimSpace(root.Metadata[beadmeta.FormulaSourceMetadataKey])
	}
	if source == "" {
		return nil
	}
	_, cfg, err := g.activeGate()
	if err != nil || cfg == nil {
		return fmt.Errorf("loaded city config is unavailable for formula source: %w", qualification.ErrUnavailable)
	}
	_, required, err := cfg.RequiredCompatibilityPacks([]string{source})
	if err != nil {
		return fmt.Errorf("required formula source binding is unavailable: %w", qualification.ErrUnavailable)
	}
	if required {
		return fmt.Errorf("required formula bead has no compatibility approval: %w", qualification.ErrUnavailable)
	}
	return nil
}

func (g MaterializationGate) revalidateRecipe(ctx context.Context, recipe *formula.Recipe, store beads.Store, authorization molecule.FormulaActionAuthorization) error {
	if !authorization.Required {
		return qualification.ErrUnavailable
	}
	gate, cfg, err := g.activeGate()
	if err != nil {
		return err
	}
	var request qualification.CompatibilityRequest
	if err := decodeOneJSON(authorization.RequestJSON, &request); err != nil {
		return fmt.Errorf("compatibility request metadata is malformed: %w", qualification.ErrUnavailable)
	}
	scope, _, required, err := gate.scopeForFormula(ctx, cfg, recipe)
	if err != nil || !required {
		return fmt.Errorf("loaded formula compatibility scope is unavailable: %w", qualification.ErrUnavailable)
	}
	scopeSHA, err := qualification.CompatibilityScopeIdentitySHA(scope)
	if err != nil || scopeSHA != request.ScopeSHA256 {
		return fmt.Errorf("loaded formula differs from approved compatibility scope: %w", qualification.ErrUnavailable)
	}
	return g.RevalidateActionMetadata(ctx, authorization.RequestJSON, authorization.AuthorizationJSON, store)
}

func recipeForFragment(fragment *formula.FragmentRecipe) *formula.Recipe {
	return &formula.Recipe{
		Name:           fragment.Name,
		Steps:          fragment.Steps,
		Deps:           fragment.Deps,
		Vars:           fragment.Vars,
		ContentHash:    fragment.ContentHash,
		FormulaSource:  fragment.FormulaSource,
		FormulaSources: append([]formula.SourceIdentity(nil), fragment.FormulaSources...),
	}
}

func moleculeAuthorizationMetadata(result Result) (molecule.FormulaActionAuthorization, error) {
	if !result.Required {
		return molecule.FormulaActionAuthorization{}, nil
	}
	request, err := json.Marshal(result.Request)
	if err != nil {
		return molecule.FormulaActionAuthorization{}, fmt.Errorf("encoding compatibility request: %w", err)
	}
	authorization, err := json.Marshal(result.Authorization)
	if err != nil {
		return molecule.FormulaActionAuthorization{}, fmt.Errorf("encoding compatibility authorization: %w", err)
	}
	return molecule.FormulaActionAuthorization{
		Required:          true,
		RequestJSON:       string(request),
		AuthorizationJSON: string(authorization),
	}, nil
}

// RevalidateActionMetadata verifies a persisted authorization against the
// currently loaded config, exact build/store, freshly resolved trusted policy,
// and current backend capability evidence. Call it immediately before a
// worker or controller executes a bead stamped by a materializer.
func (g MaterializationGate) RevalidateActionMetadata(ctx context.Context, requestJSON, authorizationJSON string, store beads.Store) error {
	var request qualification.CompatibilityRequest
	if err := decodeOneJSON(requestJSON, &request); err != nil {
		return fmt.Errorf("compatibility request metadata is malformed: %w", qualification.ErrUnavailable)
	}
	var authorization qualification.ActionAuthorization
	if err := decodeOneJSON(authorizationJSON, &authorization); err != nil {
		return fmt.Errorf("compatibility authorization metadata is malformed: %w", qualification.ErrUnavailable)
	}
	gate, _, err := g.activeGate()
	if err != nil {
		return err
	}
	current, err := qualification.Authorize(ctx, nil, gate.Snapshot, gate.Build)
	if err == nil || current.Reason != "release_authorizer_unconfigured" || current.ReleaseRequestSHA256 == "" {
		return fmt.Errorf("active controller identity is unavailable: %w", qualification.ErrUnavailable)
	}
	scope := request.Scope
	if scope.CityID != gate.CityID || scope.ServerID != gate.ServerID || scope.StoreRef != gate.StoreRef ||
		scope.EffectiveConfigSHA256 != gate.Snapshot.EffectiveConfigIdentitySHA256 || scope.ReleaseRequestSHA256 != current.ReleaseRequestSHA256 {
		return fmt.Errorf("compatibility action belongs to a different active controller scope: %w", qualification.ErrUnavailable)
	}
	for _, pack := range scope.Packs {
		if err := (config.PackCompatibilityBinding{Name: pack.Name, RequiresGC: pack.RequiresGC}).CheckControllerVersion(gate.Build.Version); err != nil {
			return err
		}
	}
	if gate.Authority == nil {
		return fmt.Errorf("compatibility authority is unconfigured: %w", qualification.ErrUnavailable)
	}
	policy, err := gate.Authority.Resolve(ctx, scope)
	if err != nil || !samePolicy(policy, request.Policy) {
		return fmt.Errorf("compatibility policy is unavailable or changed: %w", qualification.ErrUnavailable)
	}
	prover := gate.ProverForStore
	var capabilityProver qualification.CapabilityProver
	if prover != nil {
		capabilityProver = prover(store, gate.StoreRef)
	} else {
		capabilityProver = StoreCapabilityProver{Store: store, StoreRef: gate.StoreRef}
	}
	if capabilityProver == nil {
		return fmt.Errorf("compatibility capability prover is unavailable: %w", qualification.ErrUnavailable)
	}
	for _, expected := range request.Proofs {
		currentProof, err := capabilityProver.Prove(ctx, scope, policy, expected.Capability)
		if err != nil || currentProof != expected {
			return fmt.Errorf("compatibility capability proof changed: %w", qualification.ErrUnavailable)
		}
	}
	if err := qualification.RevalidateCompatibility(ctx, gate.Authority, request, authorization); err != nil {
		return err
	}
	return nil
}

func (g MaterializationGate) activeGate() (Gate, *config.City, error) {
	gate := g.Gate
	cfg := g.Config
	var snapshot qualification.Snapshot
	build := gate.Build
	if g.Current != nil {
		var err error
		var authority qualification.CompatibilityAuthority
		cfg, snapshot, build, authority, err = g.Current()
		if err != nil {
			return Gate{}, nil, fmt.Errorf("reading current compatibility identity: %w", qualification.ErrUnavailable)
		}
		gate.Authority = authority
	} else if cfg != nil {
		snapshot = cfg.QualificationSnapshot()
	}
	if cfg == nil {
		return Gate{}, nil, fmt.Errorf("loaded city config is unavailable: %w", qualification.ErrUnavailable)
	}
	gate.Snapshot = snapshot
	gate.Build = build
	return gate, cfg, nil
}

func decodeOneJSON(raw string, target any) error {
	decoder := json.NewDecoder(bytes.NewBufferString(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err == nil {
		return fmt.Errorf("trailing JSON value")
	} else if !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

func samePolicy(left, right qualification.CompatibilityPolicy) bool {
	leftCapabilities := append([]string(nil), left.RequiredCapabilities...)
	rightCapabilities := append([]string(nil), right.RequiredCapabilities...)
	sort.Strings(leftCapabilities)
	sort.Strings(rightCapabilities)
	left.RequiredCapabilities = leftCapabilities
	right.RequiredCapabilities = rightCapabilities
	return reflect.DeepEqual(left, right)
}
