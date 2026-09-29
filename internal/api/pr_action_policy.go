package api

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
)

// PRActionPolicyEnv names the controller-only signed PR policy input.
const PRActionPolicyEnv = "GC_PR_ACTION_POLICY"

// ErrPRActionPolicyUnavailable and the related errors describe trusted policy
// loading and validation failures.
var (
	ErrPRActionPolicyUnavailable = errors.New("trusted PR action policy is unavailable")
	ErrPRActionPolicyInvalid     = errors.New("trusted PR action policy is invalid")
)

// PRActionPolicyDocument is the root-owned policy scope. It is signed by a
// principal with pr.policy.write authority and never read from city config.
// The fixed code rules still require exact attempt evidence and human approval
// for merges; this document selects which repositories, branches, and checks
// those rules apply to.
type PRActionPolicyDocument struct {
	Monitors []PRActionPolicyMonitor `json:"monitors"`
}

// PRActionPolicyMonitor selects one repository, its allowed base branches and
// required checks, and the held repair route.
type PRActionPolicyMonitor struct {
	Name           string   `json:"name"`
	Owner          string   `json:"owner"`
	Repo           string   `json:"repo"`
	Rig            string   `json:"rig"`
	BaseBranches   []string `json:"base_branches"`
	RepairRoute    string   `json:"repair_route"`
	RequiredChecks []string `json:"required_checks"`
}

type prActionPolicyEnvelope struct {
	City      string                 `json:"city"`
	KeyID     string                 `json:"key_id"`
	Issuer    string                 `json:"issuer"`
	Subject   string                 `json:"subject"`
	Policy    PRActionPolicyDocument `json:"policy"`
	Signature string                 `json:"signature"`
}

type prActionPolicySet struct {
	Policies map[string]prActionPolicyEnvelope `json:"policies"`
}

type signedPRActionPolicyPayload struct {
	Domain string                 `json:"domain"`
	City   string                 `json:"city"`
	Policy PRActionPolicyDocument `json:"policy"`
}

// TrustedPRActionPolicy is constructed only by ResolvePRActionPolicy after
// validating the root-supplied signature and human policy authority.
type TrustedPRActionPolicy struct {
	City     string
	Version  string
	Monitors []PRActionPolicyMonitor
}

func (monitor PRActionPolicyMonitor) githubMonitor() config.GitHubPRMonitor {
	return config.GitHubPRMonitor{
		Name: monitor.Name, Owner: monitor.Owner, Repo: monitor.Repo, Rig: monitor.Rig,
		BaseBranches: append([]string(nil), monitor.BaseBranches...), RepairRoute: monitor.RepairRoute,
	}
}

// ResolvePRActionPolicy parses the host-provided policy document. An absent
// document disables PR actions. Configured content must be signed by a key,
// issuer, and subject with the separate pr.policy.write authority.
func ResolvePRActionPolicy(raw, city string, verifier *PRHumanGrantVerifier) (*TrustedPRActionPolicy, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	if verifier == nil {
		return nil, fmt.Errorf("%w: human policy authority is unavailable", ErrPRActionPolicyInvalid)
	}
	var configured prActionPolicySet
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&configured); err != nil {
		return nil, fmt.Errorf("%w: decode %s: %w", ErrPRActionPolicyInvalid, PRActionPolicyEnv, err)
	}
	if err := ensureJSONEOF(dec); err != nil {
		return nil, fmt.Errorf("%w: decode %s: %w", ErrPRActionPolicyInvalid, PRActionPolicyEnv, err)
	}
	envelope, found := configured.Policies[city]
	if !found {
		return nil, nil
	}
	envelope.City = strings.TrimSpace(envelope.City)
	envelope.KeyID = strings.TrimSpace(envelope.KeyID)
	envelope.Issuer = strings.TrimSpace(envelope.Issuer)
	envelope.Subject = strings.TrimSpace(envelope.Subject)
	if envelope.City == "" || envelope.City != city || envelope.KeyID == "" || envelope.Issuer == "" || envelope.Subject == "" {
		return nil, fmt.Errorf("%w: city and signer identity must be complete and exact", ErrPRActionPolicyInvalid)
	}
	if !verifier.authorizesIdentity(envelope.KeyID, envelope.Issuer, envelope.Subject, PRActionScopePolicyWrite) {
		return nil, fmt.Errorf("%w: signer lacks pr.policy.write authority", ErrPRActionPolicyInvalid)
	}
	policy, err := normalizePRActionPolicy(envelope.Policy)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrPRActionPolicyInvalid, err)
	}
	signingBytes, err := PRActionPolicySigningBytes(envelope.City, policy)
	if err != nil {
		return nil, fmt.Errorf("%w: canonicalize policy: %w", ErrPRActionPolicyInvalid, err)
	}
	signature, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(envelope.Signature))
	if err != nil || len(signature) != ed25519.SignatureSize {
		return nil, fmt.Errorf("%w: signature is malformed", ErrPRActionPolicyInvalid)
	}
	publicKey := verifier.keys[envelope.KeyID]
	if !ed25519.Verify(publicKey, signingBytes, signature) {
		return nil, fmt.Errorf("%w: signature does not bind the normalized policy", ErrPRActionPolicyInvalid)
	}
	digest := sha256.Sum256(signingBytes)
	return &TrustedPRActionPolicy{
		City: envelope.City, Version: "pr-actions-" + hex.EncodeToString(digest[:]),
		Monitors: policy.Monitors,
	}, nil
}

// PRActionPolicySigningBytes returns the domain-separated canonical bytes a
// human policy authority signs. The policy is normalized and sorted first.
func PRActionPolicySigningBytes(city string, policy PRActionPolicyDocument) ([]byte, error) {
	city = strings.TrimSpace(city)
	if city == "" {
		return nil, fmt.Errorf("city is required")
	}
	normalized, err := normalizePRActionPolicy(policy)
	if err != nil {
		return nil, err
	}
	return json.Marshal(signedPRActionPolicyPayload{Domain: "gascity.pr-action-policy.v1", City: city, Policy: normalized})
}

func normalizePRActionPolicy(policy PRActionPolicyDocument) (PRActionPolicyDocument, error) {
	if len(policy.Monitors) == 0 {
		return PRActionPolicyDocument{}, fmt.Errorf("at least one monitor is required")
	}
	monitors := make([]PRActionPolicyMonitor, 0, len(policy.Monitors))
	names := make(map[string]struct{}, len(policy.Monitors))
	for _, monitor := range policy.Monitors {
		monitor.Name = strings.TrimSpace(monitor.Name)
		monitor.Owner = strings.ToLower(strings.TrimSpace(monitor.Owner))
		monitor.Repo = strings.ToLower(strings.TrimSpace(monitor.Repo))
		monitor.Rig = strings.TrimSpace(monitor.Rig)
		monitor.RepairRoute = strings.TrimSpace(monitor.RepairRoute)
		if monitor.Name == "" || !validGitHubPathSegment(monitor.Owner) || !validGitHubPathSegment(monitor.Repo) || monitor.Rig == "" || monitor.RepairRoute == "" {
			return PRActionPolicyDocument{}, fmt.Errorf("each monitor needs a name, valid repository, rig, and repair route")
		}
		if _, duplicate := names[monitor.Name]; duplicate {
			return PRActionPolicyDocument{}, fmt.Errorf("duplicate monitor name %q", monitor.Name)
		}
		names[monitor.Name] = struct{}{}
		branches, err := normalizeUniqueStrings(monitor.BaseBranches, false)
		if err != nil || len(branches) == 0 {
			return PRActionPolicyDocument{}, fmt.Errorf("monitor %q needs unique allowed base branches", monitor.Name)
		}
		checks, err := normalizeUniqueStrings(monitor.RequiredChecks, false)
		if err != nil || len(checks) == 0 {
			return PRActionPolicyDocument{}, fmt.Errorf("monitor %q needs unique required check names", monitor.Name)
		}
		monitor.BaseBranches = branches
		monitor.RequiredChecks = checks
		monitors = append(monitors, monitor)
	}
	sort.Slice(monitors, func(i, j int) bool { return monitors[i].Name < monitors[j].Name })
	return PRActionPolicyDocument{Monitors: monitors}, nil
}

func normalizeUniqueStrings(values []string, lower bool) ([]string, error) {
	out := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, raw := range values {
		value := strings.TrimSpace(raw)
		if lower {
			value = strings.ToLower(value)
		}
		if value == "" {
			return nil, fmt.Errorf("empty value")
		}
		if _, duplicate := seen[value]; duplicate {
			return nil, fmt.Errorf("duplicate value %q", value)
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	sort.Strings(out)
	return out, nil
}

func (v *PRHumanGrantVerifier) authorizesIdentity(keyID, issuer, subject, scope string) bool {
	if v == nil {
		return false
	}
	scopes, ok := v.authorities[prHumanAuthorityKey{
		keyID: strings.TrimSpace(keyID), issuer: strings.TrimSpace(issuer), subject: strings.TrimSpace(subject),
	}]
	if !ok {
		return false
	}
	_, ok = scopes[scope]
	return ok
}
