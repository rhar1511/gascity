package config

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Masterminds/semver/v3"
)

// PackCompatibilityBinding is loader-captured pack metadata needed to resolve
// a required formula to its exact pack root. Source paths remain process-local
// and are omitted from any wire representation.
type PackCompatibilityBinding struct {
	Name                string
	RequiresGC          string
	RootID              string
	Pin                 string
	PinStatus           string
	ManifestSHA256      string
	SourceSubpathSHA256 string
	sourceDir           string
}

// ErrCompatibilityProvenanceUnavailable means a formula belongs to a pack
// requiring controller compatibility, but the loader cannot bind it to an
// exact, captured root and manifest.
var ErrCompatibilityProvenanceUnavailable = errors.New("required pack compatibility provenance unavailable")

type packCompatibilityCapture struct {
	bindings []PackCompatibilityBinding
	seen     map[string]PackCompatibilityBinding
}

func newPackCompatibilityCapture() *packCompatibilityCapture {
	return &packCompatibilityCapture{seen: make(map[string]PackCompatibilityBinding)}
}

func (c *packCompatibilityCapture) record(topoDir string, meta PackMeta, data []byte, roots *qualificationCapture) {
	if c == nil || strings.TrimSpace(meta.RequiresGC) == "" {
		return
	}
	canonicalDir := canonicalQualificationPath(topoDir)
	if canonicalDir == "" {
		return
	}
	binding := PackCompatibilityBinding{
		Name:           meta.Name,
		RequiresGC:     strings.TrimSpace(meta.RequiresGC),
		ManifestSHA256: digestBytes(data),
		sourceDir:      canonicalDir,
		PinStatus:      "unbound",
	}
	if roots != nil {
		if root, ok := roots.compatibilityRoot(canonicalDir); ok {
			rel, err := filepath.Rel(root.path, canonicalDir)
			if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				binding.RootID = root.id
				binding.Pin = root.pin
				binding.PinStatus = root.pinStatus
				binding.SourceSubpathSHA256 = sha256Hex([]byte(filepath.ToSlash(rel)))
			}
		}
	}
	key := binding.RootID + "\x00" + binding.SourceSubpathSHA256
	if prior, exists := c.seen[key]; exists {
		if prior != binding {
			prior.RootID = ""
			prior.PinStatus = "unbound"
			prior.ManifestSHA256 = ""
			c.seen[key] = prior
			for i := range c.bindings {
				if c.bindings[i].RootID == binding.RootID && c.bindings[i].SourceSubpathSHA256 == binding.SourceSubpathSHA256 {
					c.bindings[i] = prior
				}
			}
		}
		return
	}
	c.seen[key] = binding
	c.bindings = append(c.bindings, binding)
}

// RequiredCompatibilityPacks returns the captured pack bindings that contain
// the exact formula source paths, along with whether any source requires
// compatibility authorization.
func (c *City) RequiredCompatibilityPacks(formulaSources []string) ([]PackCompatibilityBinding, bool, error) {
	if c == nil || len(formulaSources) == 0 {
		return nil, false, nil
	}
	selected := make(map[string]PackCompatibilityBinding)
	required := false
	for _, rawSource := range formulaSources {
		source := canonicalQualificationPath(rawSource)
		if source == "" {
			return nil, required, fmt.Errorf("formula source path is unavailable: %w", ErrCompatibilityProvenanceUnavailable)
		}
		for _, binding := range c.packCompatibilityBindings {
			if !pathIsWithin(source, binding.sourceDir) {
				continue
			}
			required = true
			if binding.RootID == "" || binding.PinStatus == "unbound" || binding.ManifestSHA256 == "" || binding.SourceSubpathSHA256 == "" {
				return nil, true, fmt.Errorf("required pack %q has no captured root identity: %w", binding.Name, ErrCompatibilityProvenanceUnavailable)
			}
			key := binding.RootID + "\x00" + binding.SourceSubpathSHA256
			if prior, exists := selected[key]; exists && prior != binding {
				return nil, true, fmt.Errorf("required pack root alias %q is ambiguous: %w", binding.Name, ErrCompatibilityProvenanceUnavailable)
			}
			selected[key] = binding
		}
	}
	if !required {
		return nil, false, nil
	}
	packs := make([]PackCompatibilityBinding, 0, len(selected))
	for _, binding := range selected {
		packs = append(packs, binding)
	}
	sort.Slice(packs, func(i, j int) bool {
		if packs[i].RootID == packs[j].RootID {
			return packs[i].SourceSubpathSHA256 < packs[j].SourceSubpathSHA256
		}
		return packs[i].RootID < packs[j].RootID
	})
	return packs, true, nil
}

// HasRequiredCompatibilityPacks reports whether any loader-captured pack
// declares requires_gc. It is used to require controller-owned, structured
// formula provenance from custom worker queries in a city where missing
// candidate metadata could otherwise bypass the action gate.
func (c *City) HasRequiredCompatibilityPacks() bool {
	if c == nil {
		return false
	}
	for _, binding := range c.packCompatibilityBindings {
		if strings.TrimSpace(binding.RequiresGC) != "" {
			return true
		}
	}
	return false
}

func validateRequiresGC(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	if _, err := semver.NewConstraint(value); err != nil {
		return fmt.Errorf("requires_gc %q is not a valid minimum-version constraint: %w", value, err)
	}
	return nil
}

// CheckControllerVersion applies a pack's declared minimum-controller
// constraint. This is a compatibility trigger/version check only; the trusted
// release authority supplies the required capability set separately.
func (p PackCompatibilityBinding) CheckControllerVersion(version string) error {
	constraint, err := semver.NewConstraint(strings.TrimSpace(p.RequiresGC))
	if err != nil {
		return fmt.Errorf("required pack %q has invalid requires_gc: %w", p.Name, ErrCompatibilityProvenanceUnavailable)
	}
	parsed, err := semver.NewVersion(strings.TrimSpace(version))
	if err != nil {
		return fmt.Errorf("controller version is unavailable for required pack %q: %w", p.Name, ErrCompatibilityProvenanceUnavailable)
	}
	if !constraint.Check(parsed) {
		return fmt.Errorf("controller version %s does not satisfy required pack %q constraint %s: %w", parsed, p.Name, p.RequiresGC, ErrCompatibilityProvenanceUnavailable)
	}
	return nil
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
