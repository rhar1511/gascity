package orders

import (
	"errors"
	"path/filepath"
	"sort"

	"github.com/gastownhall/gascity/internal/fsys"
)

// ActivationDisposition records why a winning order definition is or is not
// in the scheduler's active set. It is an inventory fact, not an authorization
// decision.
type ActivationDisposition string

const (
	// ActivationEnabled means the winning definition is in the active set.
	ActivationEnabled ActivationDisposition = "enabled"
	// ActivationDisabledBySource means enabled=false excluded the definition.
	ActivationDisabledBySource ActivationDisposition = "disabled_by_source"
	// ActivationDisabledByOverride means a config override disabled the definition.
	ActivationDisabledByOverride ActivationDisposition = "disabled_by_override"
	// ActivationSkippedByName means [orders].skip matched the canonical name.
	ActivationSkippedByName ActivationDisposition = "skipped_by_name"
	// ActivationSkippedByAlias means [orders].skip matched a declared alias.
	ActivationSkippedByAlias ActivationDisposition = "skipped_by_alias"
)

// InventoryOrder retains one winning definition before active filtering.
// SkipMatches is canonical and nonempty only for a skipped disposition.
type InventoryOrder struct {
	Order       Order                 `json:"order"`
	Activation  ActivationDisposition `json:"activation"`
	SkipMatches []string              `json:"skip_matches"`
}

// orderDir is the subdirectory name within formula layers that contains orders.
const orderDir = "orders"

// orderFileName is the expected filename inside each order subdirectory.
const orderFileName = "order.toml"

// ScanRoot describes one order discovery root and, optionally, the
// formula layer it belongs to for PACK_DIR semantics.
type ScanRoot struct {
	Dir          string
	FormulaLayer string
}

// Scan discovers orders across formula layers. Wave 2 requires top-level flat
// order files; older PackV1 directory layouts now hard-error. Higher-priority
// layers (later in the slice) override lower ones by order name. Disabled
// orders and those in the skip list are excluded.
func Scan(fs fsys.FS, formulaLayers []string, skip []string) ([]Order, error) {
	roots := make([]ScanRoot, 0, len(formulaLayers))
	for _, layer := range formulaLayers {
		roots = append(roots, ScanRoot{
			Dir:          filepath.Join(filepath.Dir(layer), orderDir),
			FormulaLayer: layer,
		})
	}
	return ScanRoots(fs, roots, skip)
}

// ScanRoots discovers orders across explicit order roots. Higher-priority
// roots (later in the slice) override lower ones by order name.
func ScanRoots(fs fsys.FS, roots []ScanRoot, skip []string) ([]Order, error) {
	inventory, err := ScanRootsInventory(fs, roots, skip)
	if err != nil {
		return nil, err
	}
	var result []Order
	for _, entry := range inventory {
		if entry.Activation == ActivationEnabled {
			result = append(result, entry.Order)
		}
	}
	return result, nil
}

// ScanRootsInventory discovers the same winning definitions as ScanRoots but
// retains definitions excluded from the active set by source configuration or
// [orders].skip. Existing ScanRoots callers keep their filtered behavior.
func ScanRootsInventory(fs fsys.FS, roots []ScanRoot, skip []string) ([]InventoryOrder, error) {
	skipSet := make(map[string]bool, len(skip))
	for _, s := range skip {
		skipSet[s] = true
	}

	// Scan layers lowest → highest priority. Later entries override earlier ones.
	found := make(map[string]Order) // name → order
	var order []string              // preserve discovery order
	var legacyFindings []legacyOrderLayoutFinding

	for _, root := range roots {
		discovered, err := discoverRoot(fs, root)
		if err != nil {
			var legacyErr legacyOrderLayoutError
			if errors.As(err, &legacyErr) {
				legacyFindings = append(legacyFindings, legacyErr.findings...)
				continue
			}
			return nil, err
		}
		for _, a := range discovered {
			name := a.Name
			if _, exists := found[name]; !exists {
				order = append(order, name)
			}
			found[name] = a // higher-priority layer overwrites
		}
	}
	if len(legacyFindings) > 0 {
		return nil, legacyOrderLayoutError{findings: legacyFindings}
	}

	// Collect winning definitions and record the active-filter disposition.
	result := make([]InventoryOrder, 0, len(order))
	for _, name := range order {
		a := found[name]
		entry := InventoryOrder{Order: a, Activation: ActivationEnabled, SkipMatches: []string{}}
		if !a.IsEnabled() {
			entry.Activation = ActivationDisabledBySource
		} else if skipSet[name] {
			entry.Activation = ActivationSkippedByName
			entry.SkipMatches = []string{name}
		} else if matches := skippedAliases(a, skipSet); len(matches) > 0 {
			entry.Activation = ActivationSkippedByAlias
			entry.SkipMatches = matches
		}
		result = append(result, entry)
	}
	return result, nil
}

func skippedAliases(a Order, skipSet map[string]bool) []string {
	seen := make(map[string]struct{})
	var matches []string
	for _, alias := range a.skipAliases {
		if !skipSet[alias] {
			continue
		}
		if _, exists := seen[alias]; !exists {
			seen[alias] = struct{}{}
			matches = append(matches, alias)
		}
	}
	sort.Strings(matches)
	return matches
}
