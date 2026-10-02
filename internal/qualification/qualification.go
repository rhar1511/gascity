// Package qualification defines the versioned, non-secret identity reported
// by a controller and the fail-closed authorization seam used before a
// qualified pack may act.
package qualification

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// SchemaVersion identifies the qualification and release-request JSON contract.
const SchemaVersion = 1

// Status values describe whether an identity is complete, trusted, or usable.
const (
	StatusAvailable   = "available"
	StatusUnavailable = "unavailable"
	StatusAuthorized  = "authorized"
	StatusDenied      = "denied"
)

// InputRoot binds a loaded input closure to a stable root identity. Paths are
// represented only by a digest so health responses do not disclose host paths.
type InputRoot struct {
	ID                 string `json:"id"`
	Kind               string `json:"kind"`
	Pin                string `json:"pin,omitempty"`
	PinStatus          string `json:"pin_status"`
	ResolvedPathSHA256 string `json:"resolved_path_sha256"`
	InputsSHA256       string `json:"inputs_sha256"`
	InputCount         int    `json:"input_count"`
	UnavailableReason  string `json:"unavailable_reason,omitempty"`
}

// InputClosure is assembled by the config loader from bytes read during the
// load. A later filesystem scan is not evidence of what the loader consumed.
type InputClosure struct {
	SchemaVersion  int         `json:"schema_version"`
	Status         string      `json:"status"`
	Reason         string      `json:"reason,omitempty"`
	SHA256         string      `json:"sha256,omitempty"`
	EnvironmentSHA string      `json:"environment_sha256,omitempty"`
	Roots          []InputRoot `json:"roots"`
}

// Snapshot describes one loaded effective configuration. Its hashes are
// stable across map iteration and contain no raw configuration values.
type Snapshot struct {
	SchemaVersion                     int         `json:"schema_version"`
	Status                            string      `json:"status"`
	Reason                            string      `json:"reason,omitempty"`
	EffectiveConfigSHA256             string      `json:"effective_config_sha256,omitempty"`
	EffectiveConfigInputClosureSHA256 string      `json:"effective_config_input_closure_sha256,omitempty"`
	InputEnvironmentSHA256            string      `json:"input_environment_sha256,omitempty"`
	EffectiveConfigIdentitySHA256     string      `json:"effective_config_identity_sha256,omitempty"`
	InputRoots                        []InputRoot `json:"input_roots"`
}

// BuildIdentity reports the identity stamped into and measured from the
// running controller process.
type BuildIdentity struct {
	Status         string `json:"status"`
	Reason         string `json:"reason,omitempty"`
	SourceRevision string `json:"source_revision,omitempty"`
	BuildID        string `json:"build_id,omitempty"`
	Version        string `json:"version,omitempty"`
	SourceDirty    bool   `json:"source_dirty"`
	ArtifactStatus string `json:"artifact_status"`
	ArtifactSHA256 string `json:"artifact_sha256,omitempty"`
}

// Authorization reports a request to a trusted release authority. A
// successful authorization is bound to one exact qualification identity.
type Authorization struct {
	Status               string `json:"status"`
	Reason               string `json:"reason,omitempty"`
	IdentitySHA          string `json:"identity_sha256,omitempty"`
	ReleaseRequestSHA256 string `json:"release_request_sha256,omitempty"`
	RecordID             string `json:"record_id,omitempty"`
}

// ControllerReport keeps the loaded configuration, running build, and
// release decision in one health-response snapshot. A caller must not fetch
// those independently across a config reload and accidentally pair identities.
type ControllerReport struct {
	Qualification        Snapshot      `json:"qualification"`
	ControllerBuild      BuildIdentity `json:"controller_build"`
	ReleaseAuthorization Authorization `json:"release_authorization"`
}

// ReleaseRequest is the exact controller/configuration scope a trusted
// release authority must approve. It intentionally has no pack or role name.
type ReleaseRequest struct {
	Snapshot Snapshot
	Build    BuildIdentity
}

// ReleaseRequestIdentitySHA binds the loaded config/input identity to the
// exact source and executable artifact. An approval for a different binary
// cannot be replayed merely because that binary serves the same city config.
func ReleaseRequestIdentitySHA(request ReleaseRequest) (string, error) {
	return DigestJSON(map[string]any{
		"schema_version":                   SchemaVersion,
		"effective_config_identity_sha256": request.Snapshot.EffectiveConfigIdentitySHA256,
		"build": map[string]any{
			"status":          request.Build.Status,
			"source_revision": request.Build.SourceRevision,
			"build_id":        request.Build.BuildID,
			"version":         request.Build.Version,
			"source_dirty":    request.Build.SourceDirty,
			"artifact_status": request.Build.ArtifactStatus,
			"artifact_sha256": request.Build.ArtifactSHA256,
		},
	})
}

// ReleaseAuthorizer verifies a trusted, externally approved release record.
// The default controller composition leaves this interface nil, which is
// reported as unavailable rather than treated as approval.
type ReleaseAuthorizer interface {
	Authorize(context.Context, ReleaseRequest) (Authorization, error)
}

// ErrUnavailable means the controller lacks enough trusted identity data to
// make a qualification or authorization decision.
var ErrUnavailable = errors.New("qualification unavailable")

// NewSnapshot computes canonical digests for the effective config, its loaded
// input closure, and their joint identity. The caller must supply the loader's
// captured closure; this function never rereads files.
func NewSnapshot(effectiveConfig any, closure InputClosure) (Snapshot, error) {
	s := Snapshot{
		SchemaVersion: SchemaVersion,
		Status:        StatusUnavailable,
		InputRoots:    append([]InputRoot(nil), closure.Roots...),
	}
	if effectiveConfig == nil {
		s.Reason = "effective_config_missing"
		return s, nil
	}
	if closure.SchemaVersion != SchemaVersion {
		s.Reason = "input_closure_schema_unsupported"
		return s, nil
	}
	configDigest, err := DigestJSON(effectiveConfig)
	if err != nil {
		return s, fmt.Errorf("hash effective config: %w", err)
	}
	s.EffectiveConfigSHA256 = configDigest
	if isSHA256(closure.SHA256) {
		s.EffectiveConfigInputClosureSHA256 = closure.SHA256
	}
	if isSHA256(closure.EnvironmentSHA) {
		s.InputEnvironmentSHA256 = closure.EnvironmentSHA
	}
	if closure.Status != StatusAvailable || closure.Reason != "" || !isSHA256(closure.SHA256) || !isSHA256(closure.EnvironmentSHA) {
		s.Reason = closure.Reason
		if s.Reason == "" {
			s.Reason = "input_closure_unavailable"
		}
		return s, nil
	}
	recomputedClosure, err := InputClosureDigest(closure)
	if err != nil || recomputedClosure != closure.SHA256 {
		s.Reason = "input_closure_digest_mismatch"
		return s, nil
	}
	if reason := validateInputRoots(closure.Roots); reason != "" {
		s.Reason = reason
		return s, nil
	}
	identity, err := DigestJSON(map[string]string{
		"config_sha256":        configDigest,
		"input_closure_sha256": closure.SHA256,
	})
	if err != nil {
		return s, fmt.Errorf("hash qualification identity: %w", err)
	}
	s.EffectiveConfigIdentitySHA256 = identity
	s.Status = StatusAvailable
	return s, nil
}

// Authorize fails closed when the snapshot, build identity, or trusted
// authority is unavailable. It never interprets a local pack lock as trust.
func Authorize(ctx context.Context, authorizer ReleaseAuthorizer, snapshot Snapshot, build BuildIdentity) (Authorization, error) {
	if snapshot.SchemaVersion != SchemaVersion {
		return Authorization{Status: StatusUnavailable, Reason: "qualification_snapshot_schema_unsupported"}, ErrUnavailable
	}
	if snapshot.Status != StatusAvailable || !isSHA256(snapshot.EffectiveConfigIdentitySHA256) ||
		!isSHA256(snapshot.EffectiveConfigSHA256) || !isSHA256(snapshot.EffectiveConfigInputClosureSHA256) ||
		!isSHA256(snapshot.InputEnvironmentSHA256) || validateInputRoots(snapshot.InputRoots) != "" {
		return Authorization{Status: StatusUnavailable, Reason: reasonOr(snapshot.Reason, "qualification_snapshot_unavailable")}, ErrUnavailable
	}
	closureDigest, closureErr := InputClosureDigest(InputClosure{
		SchemaVersion:  snapshot.SchemaVersion,
		EnvironmentSHA: snapshot.InputEnvironmentSHA256,
		Roots:          snapshot.InputRoots,
	})
	if closureErr != nil || closureDigest != snapshot.EffectiveConfigInputClosureSHA256 {
		return Authorization{Status: StatusUnavailable, Reason: "input_closure_identity_mismatch"}, ErrUnavailable
	}
	expectedIdentity, identityErr := DigestJSON(map[string]string{
		"config_sha256":        snapshot.EffectiveConfigSHA256,
		"input_closure_sha256": snapshot.EffectiveConfigInputClosureSHA256,
	})
	if identityErr != nil || expectedIdentity != snapshot.EffectiveConfigIdentitySHA256 {
		return Authorization{Status: StatusUnavailable, Reason: "qualification_identity_mismatch"}, ErrUnavailable
	}
	if build.Status != StatusAvailable || build.ArtifactStatus != StatusAvailable || build.SourceDirty ||
		!isFullRevision(build.SourceRevision) || build.BuildID != build.SourceRevision ||
		!isSHA256(build.ArtifactSHA256) || strings.TrimSpace(build.Version) == "" {
		reason := reasonOr(build.Reason, "controller_build_unavailable")
		if build.SourceDirty {
			reason = "controller_build_dirty"
		}
		return Authorization{Status: StatusUnavailable, Reason: reason}, ErrUnavailable
	}
	request := ReleaseRequest{Snapshot: snapshot, Build: build}
	requestSHA, err := ReleaseRequestIdentitySHA(request)
	if err != nil {
		return Authorization{Status: StatusUnavailable, Reason: "release_request_identity_unavailable", IdentitySHA: snapshot.EffectiveConfigIdentitySHA256}, ErrUnavailable
	}
	if authorizer == nil {
		return Authorization{Status: StatusUnavailable, Reason: "release_authorizer_unconfigured", IdentitySHA: snapshot.EffectiveConfigIdentitySHA256, ReleaseRequestSHA256: requestSHA}, ErrUnavailable
	}
	decision, err := authorizer.Authorize(ctx, request)
	if err != nil {
		return Authorization{Status: StatusUnavailable, Reason: "release_authority_unavailable", IdentitySHA: snapshot.EffectiveConfigIdentitySHA256, ReleaseRequestSHA256: requestSHA}, err
	}
	if decision.IdentitySHA != snapshot.EffectiveConfigIdentitySHA256 {
		return Authorization{Status: StatusDenied, Reason: "release_scope_mismatch", IdentitySHA: snapshot.EffectiveConfigIdentitySHA256, ReleaseRequestSHA256: requestSHA}, nil
	}
	if decision.ReleaseRequestSHA256 != requestSHA {
		return Authorization{Status: StatusDenied, Reason: "release_request_scope_mismatch", IdentitySHA: snapshot.EffectiveConfigIdentitySHA256, ReleaseRequestSHA256: requestSHA}, nil
	}
	if decision.Status != StatusAuthorized {
		if decision.Status != StatusDenied && decision.Status != StatusUnavailable {
			decision.Status = StatusUnavailable
			decision.Reason = "release_authority_status_invalid"
		}
		decision.IdentitySHA = snapshot.EffectiveConfigIdentitySHA256
		decision.ReleaseRequestSHA256 = requestSHA
		return decision, nil
	}
	if strings.TrimSpace(decision.RecordID) == "" {
		return Authorization{Status: StatusUnavailable, Reason: "release_record_identity_missing", IdentitySHA: snapshot.EffectiveConfigIdentitySHA256, ReleaseRequestSHA256: requestSHA}, ErrUnavailable
	}
	decision.IdentitySHA = snapshot.EffectiveConfigIdentitySHA256
	decision.ReleaseRequestSHA256 = requestSHA
	return decision, nil
}

// DigestJSON hashes CanonicalJSON output. Cross-language release inputs use
// strings, booleans, integer counters, arrays, and objects; effective City
// configuration is hashed by this controller and exposed only as an opaque
// digest, so a checker does not need to recreate Go's config encoding.
func DigestJSON(value any) (string, error) {
	data, err := CanonicalJSON(value)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// InputClosureDigest hashes a versioned set of stable roots and the loader's
// environment identity. Root array order is canonicalized by stable ID.
func InputClosureDigest(closure InputClosure) (string, error) {
	roots := append([]InputRoot(nil), closure.Roots...)
	sort.Slice(roots, func(i, j int) bool { return roots[i].ID < roots[j].ID })
	return DigestJSON(struct {
		SchemaVersion  int         `json:"schema_version"`
		EnvironmentSHA string      `json:"environment_sha256"`
		Roots          []InputRoot `json:"roots"`
	}{closure.SchemaVersion, closure.EnvironmentSHA, roots})
}

func validateInputRoots(roots []InputRoot) string {
	if len(roots) == 0 {
		return "input_roots_missing"
	}
	seen := make(map[string]struct{}, len(roots))
	for _, root := range roots {
		if strings.TrimSpace(root.ID) == "" || strings.TrimSpace(root.Kind) == "" {
			return "input_root_identity_missing"
		}
		if _, exists := seen[root.ID]; exists {
			return "input_root_id_duplicate"
		}
		seen[root.ID] = struct{}{}
		if root.Kind == "external" || root.UnavailableReason != "" || root.PinStatus == "unbound" || root.PinStatus == "" {
			return reasonOr(root.UnavailableReason, "input_root_unbound")
		}
		if !isSHA256(root.ResolvedPathSHA256) || !isSHA256(root.InputsSHA256) {
			return "input_root_digest_unavailable"
		}
		if root.InputCount <= 0 {
			return "input_root_inputs_missing"
		}
		switch root.PinStatus {
		case "content":
			if root.Pin != "" {
				return "input_root_content_pin_invalid"
			}
		case "locked":
			if !isFullRevision(root.Pin) {
				return "input_root_locked_pin_invalid"
			}
		case "bundled":
			if strings.TrimSpace(root.Pin) == "" {
				return "input_root_bundled_pin_missing"
			}
		default:
			return "input_root_pin_status_unsupported"
		}
	}
	return ""
}

func isSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil && strings.ToLower(value) == value
}

func isFullRevision(value string) bool {
	if len(value) != 40 {
		return false
	}
	for _, r := range value {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

func reasonOr(reason, fallback string) string {
	if strings.TrimSpace(reason) != "" {
		return reason
	}
	return fallback
}
