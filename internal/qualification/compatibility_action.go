package qualification

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// CompatibilityFormula identifies the exact selected formula without exposing
// its host path. SourceSHA256 is over the loader-resolved source identity;
// ContentSHA256 is over the loaded formula bytes/compiled recipe identity.
type CompatibilityFormula struct {
	Name           string `json:"name"`
	ContentSHA256  string `json:"content_sha256"`
	SourceSHA256   string `json:"source_sha256"`
	CompiledSHA256 string `json:"compiled_sha256"`
}

// CompatibilityPack binds one required pack to the exact loader-captured root
// and manifest. SourceSubpathSHA256 distinguishes packs nested in a pinned
// repository without disclosing the resolved absolute path.
type CompatibilityPack struct {
	Name                string `json:"name"`
	RootID              string `json:"root_id"`
	Pin                 string `json:"pin"`
	PinStatus           string `json:"pin_status"`
	ManifestSHA256      string `json:"manifest_sha256"`
	SourceSubpathSHA256 string `json:"source_subpath_sha256"`
	RequiresGC          string `json:"requires_gc"`
}

// CompatibilityScope is the exact city, controller, formula, and pack scope
// for which a trusted release record may resolve capabilities. Formula and
// pack names are descriptive; the hashes and pins carry the identity.
type CompatibilityScope struct {
	SchemaVersion         int                  `json:"schema_version"`
	CityID                string               `json:"city_id"`
	ServerID              string               `json:"server_id"`
	StoreRef              string               `json:"store_ref"`
	EffectiveConfigSHA256 string               `json:"effective_config_identity_sha256"`
	ReleaseRequestSHA256  string               `json:"release_request_sha256"`
	Formula               CompatibilityFormula `json:"formula"`
	Packs                 []CompatibilityPack  `json:"packs"`
}

// CompatibilityPolicy is resolved from the trusted release-authority record.
// A pack declaration can trigger this lookup, but cannot choose or reduce its
// required capability set.
type CompatibilityPolicy struct {
	Status               string   `json:"status"`
	Reason               string   `json:"reason,omitempty"`
	ScopeSHA256          string   `json:"scope_sha256"`
	PolicyReference      string   `json:"policy_reference"`
	PolicyVersion        string   `json:"policy_version"`
	RequiredCapabilities []string `json:"required_capabilities"`
}

// CapabilityProof is a controller-produced statement about one required
// capability in the exact scope and policy that were resolved. EvidenceSHA256
// binds backend/config-specific evidence without putting secrets in the record.
type CapabilityProof struct {
	Status          string `json:"status"`
	Capability      string `json:"capability"`
	ScopeSHA256     string `json:"scope_sha256"`
	PolicyReference string `json:"policy_reference"`
	PolicyVersion   string `json:"policy_version"`
	EvidenceSHA256  string `json:"evidence_sha256"`
}

// CompatibilityRequest is the immutable authority input after the controller
// has resolved the trusted requirement set and proved every capability.
type CompatibilityRequest struct {
	SchemaVersion int                 `json:"schema_version"`
	Scope         CompatibilityScope  `json:"scope"`
	ScopeSHA256   string              `json:"scope_sha256"`
	Policy        CompatibilityPolicy `json:"policy"`
	Proofs        []CapabilityProof   `json:"proofs"`
	RequestSHA256 string              `json:"request_sha256"`
}

// ActionAuthorization is an opaque, controller-issued reference bound to one
// exact compatibility request and immutable policy version.
type ActionAuthorization struct {
	Status          string `json:"status"`
	Reason          string `json:"reason,omitempty"`
	PolicyReference string `json:"policy_reference,omitempty"`
	PolicyVersion   string `json:"policy_version,omitempty"`
	RequestSHA256   string `json:"request_sha256,omitempty"`
	RecordID        string `json:"record_id,omitempty"`
	Reference       string `json:"reference,omitempty"`
}

// CompatibilityAuthority resolves trusted required capabilities, authorizes
// the complete request, and revalidates the same reference before later work.
// Production composition intentionally leaves this unconfigured by default.
type CompatibilityAuthority interface {
	Resolve(context.Context, CompatibilityScope) (CompatibilityPolicy, error)
	Authorize(context.Context, CompatibilityRequest) (ActionAuthorization, error)
	Verify(context.Context, CompatibilityRequest, ActionAuthorization) error
}

// CapabilityProver evaluates a named requirement against the selected
// runtime/backend. Unknown requirements must return ErrUnavailable.
type CapabilityProver interface {
	Prove(context.Context, CompatibilityScope, CompatibilityPolicy, string) (CapabilityProof, error)
}

// ErrCompatibilityDenied means a trusted authority returned a decision that
// is bound to a different scope, policy, or request.
var ErrCompatibilityDenied = errors.New("compatibility authorization denied")

// CompatibilityScopeIdentitySHA validates and hashes the complete action
// scope. It sorts pack roots so loader traversal order cannot change identity,
// but rejects duplicate root/subpath bindings instead of collapsing aliases.
func CompatibilityScopeIdentitySHA(scope CompatibilityScope) (string, error) {
	if scope.SchemaVersion != SchemaVersion || strings.TrimSpace(scope.CityID) == "" || strings.TrimSpace(scope.ServerID) == "" || strings.TrimSpace(scope.StoreRef) == "" {
		return "", fmt.Errorf("compatibility scope identity is incomplete: %w", ErrUnavailable)
	}
	if !isSHA256(scope.EffectiveConfigSHA256) || !isSHA256(scope.ReleaseRequestSHA256) {
		return "", fmt.Errorf("compatibility config or release identity is unavailable: %w", ErrUnavailable)
	}
	if strings.TrimSpace(scope.Formula.Name) == "" || !isSHA256(scope.Formula.ContentSHA256) || !isSHA256(scope.Formula.SourceSHA256) || !isSHA256(scope.Formula.CompiledSHA256) {
		return "", fmt.Errorf("compatibility formula identity is unavailable: %w", ErrUnavailable)
	}
	if len(scope.Packs) == 0 {
		return "", fmt.Errorf("compatibility required pack scope is empty: %w", ErrUnavailable)
	}
	packs := append([]CompatibilityPack(nil), scope.Packs...)
	for i := range packs {
		p := &packs[i]
		if strings.TrimSpace(p.Name) == "" || strings.TrimSpace(p.RootID) == "" || strings.TrimSpace(p.RequiresGC) == "" ||
			!isSHA256(p.ManifestSHA256) || !isSHA256(p.SourceSubpathSHA256) {
			return "", fmt.Errorf("compatibility pack identity is incomplete: %w", ErrUnavailable)
		}
		switch p.PinStatus {
		case "locked":
			if !isFullRevision(p.Pin) {
				return "", fmt.Errorf("compatibility pack pin is unavailable: %w", ErrUnavailable)
			}
		case "bundled":
			if strings.TrimSpace(p.Pin) == "" {
				return "", fmt.Errorf("compatibility pack pin is unavailable: %w", ErrUnavailable)
			}
		case "content":
			// Local content roots have no VCS pin; the manifest, subpath, full
			// loaded-configuration identity, and controller source identity bind
			// their content. An empty pin is therefore expected here.
		default:
			return "", fmt.Errorf("compatibility pack pin status %q is unsupported: %w", p.PinStatus, ErrUnavailable)
		}
	}
	sort.Slice(packs, func(i, j int) bool {
		if packs[i].RootID == packs[j].RootID {
			return packs[i].SourceSubpathSHA256 < packs[j].SourceSubpathSHA256
		}
		return packs[i].RootID < packs[j].RootID
	})
	for i := 1; i < len(packs); i++ {
		if packs[i-1].RootID == packs[i].RootID && packs[i-1].SourceSubpathSHA256 == packs[i].SourceSubpathSHA256 {
			return "", fmt.Errorf("compatibility pack root alias is ambiguous: %w", ErrUnavailable)
		}
	}
	scope.Packs = packs
	return DigestJSON(scope)
}

// AuthorizeCompatibility resolves the trusted policy, proves each exact
// requirement against the selected runtime, and asks the same authority to
// bind the resulting request. It returns unavailable before authorization if
// any identity, policy, or capability proof is missing.
func AuthorizeCompatibility(ctx context.Context, authority CompatibilityAuthority, scope CompatibilityScope, prover CapabilityProver) (ActionAuthorization, CompatibilityRequest, error) {
	if authority == nil {
		return ActionAuthorization{Status: StatusUnavailable, Reason: "compatibility_authority_unconfigured"}, CompatibilityRequest{}, ErrUnavailable
	}
	scopeSHA, err := CompatibilityScopeIdentitySHA(scope)
	if err != nil {
		return ActionAuthorization{Status: StatusUnavailable, Reason: "compatibility_scope_unavailable"}, CompatibilityRequest{}, err
	}
	policy, err := authority.Resolve(ctx, scope)
	if err != nil {
		return ActionAuthorization{Status: StatusUnavailable, Reason: "compatibility_policy_unavailable"}, CompatibilityRequest{}, err
	}
	if policy.Status != StatusAvailable || policy.ScopeSHA256 != scopeSHA || strings.TrimSpace(policy.PolicyReference) == "" || strings.TrimSpace(policy.PolicyVersion) == "" || len(policy.RequiredCapabilities) == 0 {
		return ActionAuthorization{Status: StatusUnavailable, Reason: reasonOr(policy.Reason, "compatibility_policy_scope_unavailable")}, CompatibilityRequest{}, ErrUnavailable
	}
	requirements, err := canonicalCapabilityNames(policy.RequiredCapabilities)
	if err != nil {
		return ActionAuthorization{Status: StatusUnavailable, Reason: "compatibility_policy_requirements_invalid"}, CompatibilityRequest{}, err
	}
	policy.RequiredCapabilities = requirements
	request := CompatibilityRequest{SchemaVersion: SchemaVersion, Scope: scope, ScopeSHA256: scopeSHA, Policy: policy}
	if prover == nil {
		return ActionAuthorization{Status: StatusUnavailable, Reason: "compatibility_capability_prover_unconfigured"}, request, ErrUnavailable
	}
	request.Proofs = make([]CapabilityProof, 0, len(requirements))
	for _, capability := range requirements {
		proof, err := prover.Prove(ctx, scope, policy, capability)
		if err != nil {
			return ActionAuthorization{Status: StatusUnavailable, Reason: "compatibility_capability_unavailable"}, request, err
		}
		if proof.Status != StatusAvailable || proof.Capability != capability || proof.ScopeSHA256 != scopeSHA ||
			proof.PolicyReference != policy.PolicyReference || proof.PolicyVersion != policy.PolicyVersion || !isSHA256(proof.EvidenceSHA256) {
			return ActionAuthorization{Status: StatusUnavailable, Reason: "compatibility_capability_proof_mismatch"}, request, ErrUnavailable
		}
		request.Proofs = append(request.Proofs, proof)
	}
	request.RequestSHA256, err = CompatibilityRequestIdentitySHA(request)
	if err != nil {
		return ActionAuthorization{Status: StatusUnavailable, Reason: "compatibility_request_identity_unavailable"}, request, err
	}
	decision, err := authority.Authorize(ctx, request)
	if err != nil {
		return ActionAuthorization{Status: StatusUnavailable, Reason: "compatibility_authority_unavailable"}, request, err
	}
	if decision.Status != StatusAuthorized {
		if decision.Status != StatusDenied && decision.Status != StatusUnavailable {
			decision.Status = StatusUnavailable
			decision.Reason = "compatibility_authority_status_invalid"
		}
		return decision, request, nil
	}
	if decision.PolicyReference != policy.PolicyReference || decision.PolicyVersion != policy.PolicyVersion || decision.RequestSHA256 != request.RequestSHA256 ||
		strings.TrimSpace(decision.RecordID) == "" || strings.TrimSpace(decision.Reference) == "" {
		return ActionAuthorization{Status: StatusDenied, Reason: "compatibility_authorization_scope_mismatch"}, request, nil
	}
	return decision, request, nil
}

// CompatibilityRequestIdentitySHA binds the selected scope, authority-owned
// requirements, immutable policy identity, and all runtime proofs.
func CompatibilityRequestIdentitySHA(request CompatibilityRequest) (string, error) {
	if request.SchemaVersion != SchemaVersion {
		return "", fmt.Errorf("compatibility request schema unsupported: %w", ErrUnavailable)
	}
	scopeSHA, err := CompatibilityScopeIdentitySHA(request.Scope)
	if err != nil || scopeSHA != request.ScopeSHA256 {
		return "", fmt.Errorf("compatibility request scope mismatch: %w", ErrUnavailable)
	}
	if request.Policy.Status != StatusAvailable || request.Policy.ScopeSHA256 != scopeSHA || strings.TrimSpace(request.Policy.PolicyReference) == "" || strings.TrimSpace(request.Policy.PolicyVersion) == "" {
		return "", fmt.Errorf("compatibility policy identity unavailable: %w", ErrUnavailable)
	}
	requirements, err := canonicalCapabilityNames(request.Policy.RequiredCapabilities)
	if err != nil || len(requirements) == 0 || len(request.Proofs) != len(requirements) {
		return "", fmt.Errorf("compatibility proof set is incomplete: %w", ErrUnavailable)
	}
	proofs := append([]CapabilityProof(nil), request.Proofs...)
	sort.Slice(proofs, func(i, j int) bool { return proofs[i].Capability < proofs[j].Capability })
	for i, proof := range proofs {
		if proof.Capability != requirements[i] || proof.Status != StatusAvailable || proof.ScopeSHA256 != scopeSHA ||
			proof.PolicyReference != request.Policy.PolicyReference || proof.PolicyVersion != request.Policy.PolicyVersion || !isSHA256(proof.EvidenceSHA256) {
			return "", fmt.Errorf("compatibility proof is mismatched: %w", ErrUnavailable)
		}
	}
	request.RequestSHA256 = ""
	request.Proofs = proofs
	return DigestJSON(request)
}

// RevalidateCompatibility verifies the controller-issued reference for the
// exact same request immediately before a later action or descendant write.
func RevalidateCompatibility(ctx context.Context, authority CompatibilityAuthority, request CompatibilityRequest, authorization ActionAuthorization) error {
	if authority == nil || authorization.Status != StatusAuthorized || strings.TrimSpace(authorization.Reference) == "" ||
		authorization.RecordID == "" || authorization.PolicyReference != request.Policy.PolicyReference ||
		authorization.PolicyVersion != request.Policy.PolicyVersion {
		return ErrUnavailable
	}
	expected, err := CompatibilityRequestIdentitySHA(request)
	if err != nil || expected != request.RequestSHA256 || authorization.RequestSHA256 != expected {
		return ErrUnavailable
	}
	if err := authority.Verify(ctx, request, authorization); err != nil {
		return fmt.Errorf("verify compatibility authorization: %w", ErrUnavailable)
	}
	return nil
}

func canonicalCapabilityNames(names []string) ([]string, error) {
	canonical := append([]string(nil), names...)
	for i := range canonical {
		canonical[i] = strings.TrimSpace(canonical[i])
		if canonical[i] == "" {
			return nil, fmt.Errorf("empty compatibility capability: %w", ErrUnavailable)
		}
	}
	sort.Strings(canonical)
	for i := 1; i < len(canonical); i++ {
		if canonical[i] == canonical[i-1] {
			return nil, fmt.Errorf("duplicate compatibility capability %q: %w", canonical[i], ErrUnavailable)
		}
	}
	return canonical, nil
}
