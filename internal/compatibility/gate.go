// Package compatibility joins loader-owned pack/formula provenance to the
// qualification authority contract used before executable work is created.
package compatibility

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/formula"
	"github.com/gastownhall/gascity/internal/qualification"
)

// Gate contains the exact active controller scope and unconfigured-by-default
// trusted authority. A pack's non-empty requires_gc marker triggers this gate;
// the authority, never the pack, chooses the required capabilities.
type Gate struct {
	Authority      qualification.CompatibilityAuthority
	ProverForStore func(beads.Store, string) qualification.CapabilityProver
	CityID         string
	ServerID       string
	StoreRef       string
	Snapshot       qualification.Snapshot
	Build          qualification.BuildIdentity
}

// Result distinguishes ordinary formulas from formulas that require a
// trusted compatibility decision. Required remains true on every error so
// callers cannot treat unavailable required work as an ordinary formula.
type Result struct {
	Required           bool
	Authorization      qualification.ActionAuthorization
	Request            qualification.CompatibilityRequest
	CompatibilityPacks []config.PackCompatibilityBinding
}

// AuthorizeFormula checks the exact parsed source set before a required
// formula may be cooked, routed, dispatched, or expanded.
func (g Gate) AuthorizeFormula(ctx context.Context, cfg *config.City, recipe *formula.Recipe, store beads.Store) (Result, error) {
	scope, packs, required, err := g.scopeForFormula(ctx, cfg, recipe)
	if err != nil {
		return Result{Required: required, CompatibilityPacks: packs}, err
	}
	if !required {
		return Result{}, nil
	}
	result := Result{Required: true, CompatibilityPacks: packs}
	var prover qualification.CapabilityProver
	if g.ProverForStore != nil {
		prover = g.ProverForStore(store, g.StoreRef)
	} else {
		prover = StoreCapabilityProver{Store: store, StoreRef: g.StoreRef}
	}
	decision, request, err := qualification.AuthorizeCompatibility(ctx, g.Authority, scope, prover)
	result.Authorization = decision
	result.Request = request
	if err != nil {
		return result, err
	}
	if decision.Status != qualification.StatusAuthorized {
		return result, fmt.Errorf("required formula compatibility is %s: %s", decision.Status, decision.Reason)
	}
	return result, nil
}

// RevalidateFormula checks that a previously authorized formula still has the
// same loaded config, pack, controller, and exact-store capability evidence
// immediately before a later side effect. It never creates a new authorization.
func (g Gate) RevalidateFormula(ctx context.Context, cfg *config.City, recipe *formula.Recipe, store beads.Store, result Result) error {
	if !result.Required {
		sources := recipeFormulaSources(recipe)
		if recipe == nil || len(sources) == 0 {
			return fmt.Errorf("formula source provenance unavailable: %w", qualification.ErrUnavailable)
		}
		_, required, err := cfg.RequiredCompatibilityPacks(sources)
		if err != nil || required {
			return fmt.Errorf("formula compatibility requirement changed: %w", qualification.ErrUnavailable)
		}
		return nil
	}
	scope, _, required, err := g.scopeForFormula(ctx, cfg, recipe)
	if err != nil || !required {
		return fmt.Errorf("formula compatibility scope changed: %w", qualification.ErrUnavailable)
	}
	scopeSHA, err := qualification.CompatibilityScopeIdentitySHA(scope)
	if err != nil || scopeSHA != result.Request.ScopeSHA256 {
		return fmt.Errorf("formula compatibility scope changed: %w", qualification.ErrUnavailable)
	}
	var prover qualification.CapabilityProver
	if g.ProverForStore != nil {
		prover = g.ProverForStore(store, g.StoreRef)
	} else {
		prover = StoreCapabilityProver{Store: store, StoreRef: g.StoreRef}
	}
	if prover == nil && len(result.Request.Policy.RequiredCapabilities) > 0 {
		return fmt.Errorf("formula compatibility prover is unavailable: %w", qualification.ErrUnavailable)
	}
	for _, expected := range result.Request.Proofs {
		current, err := prover.Prove(ctx, scope, result.Request.Policy, expected.Capability)
		if err != nil || current != expected {
			return fmt.Errorf("formula compatibility capability changed: %w", qualification.ErrUnavailable)
		}
	}
	return qualification.RevalidateCompatibility(ctx, g.Authority, result.Request, result.Authorization)
}

func (g Gate) scopeForFormula(ctx context.Context, cfg *config.City, recipe *formula.Recipe) (qualification.CompatibilityScope, []config.PackCompatibilityBinding, bool, error) {
	sources := recipeFormulaSources(recipe)
	if recipe == nil || len(sources) == 0 {
		return qualification.CompatibilityScope{}, nil, true, fmt.Errorf("formula source provenance unavailable: %w", qualification.ErrUnavailable)
	}
	packs, required, err := cfg.RequiredCompatibilityPacks(sources)
	if err != nil {
		return qualification.CompatibilityScope{}, packs, required, err
	}
	if !required {
		return qualification.CompatibilityScope{}, nil, false, nil
	}
	if strings.TrimSpace(recipe.Name) == "" {
		return qualification.CompatibilityScope{}, packs, true, fmt.Errorf("required formula identity unavailable: %w", qualification.ErrUnavailable)
	}
	for _, pack := range packs {
		if err := pack.CheckControllerVersion(g.Build.Version); err != nil {
			return qualification.CompatibilityScope{}, packs, true, err
		}
	}
	formulaScope, err := formulaIdentity(recipe)
	if err != nil {
		return qualification.CompatibilityScope{}, packs, true, err
	}
	validatedRelease, err := qualification.Authorize(ctx, nil, g.Snapshot, g.Build)
	if err == nil || validatedRelease.Reason != "release_authorizer_unconfigured" || validatedRelease.ReleaseRequestSHA256 == "" {
		return qualification.CompatibilityScope{}, packs, true, fmt.Errorf("controller release identity unavailable: %w", qualification.ErrUnavailable)
	}
	scope := qualification.CompatibilityScope{
		SchemaVersion:         qualification.SchemaVersion,
		CityID:                g.CityID,
		ServerID:              g.ServerID,
		StoreRef:              g.StoreRef,
		EffectiveConfigSHA256: g.Snapshot.EffectiveConfigIdentitySHA256,
		ReleaseRequestSHA256:  validatedRelease.ReleaseRequestSHA256,
		Formula:               formulaScope,
		Packs:                 make([]qualification.CompatibilityPack, 0, len(packs)),
	}
	for _, pack := range packs {
		scope.Packs = append(scope.Packs, qualification.CompatibilityPack{
			Name:                pack.Name,
			RootID:              pack.RootID,
			Pin:                 pack.Pin,
			PinStatus:           pack.PinStatus,
			ManifestSHA256:      pack.ManifestSHA256,
			SourceSubpathSHA256: pack.SourceSubpathSHA256,
			RequiresGC:          pack.RequiresGC,
		})
	}
	return scope, packs, true, nil
}

func recipeFormulaSources(recipe *formula.Recipe) []string {
	if recipe == nil {
		return nil
	}
	paths := make([]string, 0, len(recipe.FormulaSources)+1)
	for _, source := range recipe.FormulaSources {
		if strings.TrimSpace(source.Path) != "" {
			paths = append(paths, source.Path)
		}
	}
	if len(paths) == 0 && strings.TrimSpace(recipe.FormulaSource) != "" {
		paths = append(paths, recipe.FormulaSource)
	}
	return paths
}

func formulaIdentity(recipe *formula.Recipe) (qualification.CompatibilityFormula, error) {
	if recipe == nil || strings.TrimSpace(recipe.Name) == "" {
		return qualification.CompatibilityFormula{}, qualification.ErrUnavailable
	}
	type sourceIdentity struct {
		PathSHA256    string `json:"path_sha256"`
		ContentSHA256 string `json:"content_sha256"`
	}
	sources := append([]formula.SourceIdentity(nil), recipe.FormulaSources...)
	if len(sources) == 0 && recipe.FormulaSource != "" && recipe.ContentHash != "" {
		sources = append(sources, formula.SourceIdentity{Path: recipe.FormulaSource, ContentSHA256: recipe.ContentHash})
	}
	if len(sources) == 0 {
		return qualification.CompatibilityFormula{}, fmt.Errorf("formula source identity unavailable: %w", qualification.ErrUnavailable)
	}
	rows := make([]sourceIdentity, 0, len(sources))
	seen := make(map[string]string, len(sources))
	for _, source := range sources {
		path := filepath.Clean(strings.TrimSpace(source.Path))
		if path == "." || !isSHA256(source.ContentSHA256) {
			return qualification.CompatibilityFormula{}, fmt.Errorf("formula source digest unavailable: %w", qualification.ErrUnavailable)
		}
		pathDigest := sha256String(path)
		if prior, exists := seen[pathDigest]; exists {
			if prior != source.ContentSHA256 {
				return qualification.CompatibilityFormula{}, fmt.Errorf("formula source changed during composition: %w", qualification.ErrUnavailable)
			}
			continue
		}
		seen[pathDigest] = source.ContentSHA256
		rows = append(rows, sourceIdentity{PathSHA256: pathDigest, ContentSHA256: source.ContentSHA256})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].PathSHA256 < rows[j].PathSHA256 })
	contentSHA, err := qualification.DigestJSON(rows)
	if err != nil {
		return qualification.CompatibilityFormula{}, err
	}
	pathRows := make([]string, len(rows))
	for i := range rows {
		pathRows[i] = rows[i].PathSHA256
	}
	sourceSHA, err := qualification.DigestJSON(pathRows)
	if err != nil {
		return qualification.CompatibilityFormula{}, err
	}
	compiledSHA, err := qualification.DigestJSON(recipe)
	if err != nil {
		return qualification.CompatibilityFormula{}, fmt.Errorf("compiled formula identity unavailable: %w", qualification.ErrUnavailable)
	}
	return qualification.CompatibilityFormula{
		Name: recipe.Name, ContentSHA256: contentSHA, SourceSHA256: sourceSHA, CompiledSHA256: compiledSHA,
	}, nil
}

func isSHA256(value string) bool {
	if len(value) != 64 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func sha256String(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
