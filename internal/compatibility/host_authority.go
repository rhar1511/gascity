package compatibility

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/qualification"
)

// HostCompatibilityAuthorityDirectoryEnv names the host-only environment
// variable read by trusted supervisor startup for the signed compatibility
// authority directory. City, pack, provider, and worker configuration must not
// override the captured authority.
const (
	HostCompatibilityAuthorityDirectoryEnv        = "GC_COMPATIBILITY_AUTHORITY_DIR"
	HostCompatibilityAuthorityMaxRevocationAgeEnv = "GC_COMPATIBILITY_AUTHORITY_MAX_REVOCATION_AGE"
	HostCompatibilityAuthoritySchemaV1            = 1
	HostCompatibilityRecordRole                   = "compatibility_release"
	HostCompatibilityRevocationRole               = "compatibility_revocation"
	HostCompatibilityRecordSchemaV1               = "gc.compatibility-release.v1"
	HostCompatibilityRevocationsSchemaV1          = "gc.compatibility-revocations.v1"
	HostCompatibilityKeyringFile                  = "keyring.json"
	HostCompatibilityRecordsFile                  = "records.json"
	HostCompatibilityRevocationsFile              = "revocations.json"
	maxHostCompatibilityFileBytes                 = 4 << 20
)

const (
	compatibilityRecordSigningDomain     = "gascity.compatibility-release.v1"
	compatibilityRevocationSigningDomain = "gascity.compatibility-revocations.v1"
	compatibilityAuthorityClockSkew      = 30 * time.Second
)

// HostCompatibilityPublicKey is a dedicated host trust key. A key has one
// signing purpose; release approval keys cannot also revoke approvals.
type HostCompatibilityPublicKey struct {
	KeyID     string `json:"key_id"`
	Role      string `json:"role"`
	PublicKey string `json:"public_key"`
}

// SignedHostCompatibilityRecord carries an exact release/config-scope policy.
// Payload contains canonical JSON bytes signed with the release-record domain.
type SignedHostCompatibilityRecord struct {
	Payload   json.RawMessage `json:"payload"`
	Signature string          `json:"signature"`
}

// HostCompatibilityRecordPayload is an operator-approved record for one exact
// controller, city, store, formula, and pack scope. Its capability list is
// supplied by the trusted record; the public controller does not hard-code a
// private pack's capability names.
type HostCompatibilityRecordPayload struct {
	SchemaVersion        string   `json:"schema_version"`
	RecordID             string   `json:"record_id"`
	KeyID                string   `json:"key_id"`
	Status               string   `json:"status"`
	IssuedAt             string   `json:"issued_at"`
	ExpiresAt            string   `json:"expires_at"`
	ScopeSHA256          string   `json:"scope_sha256"`
	ReleaseRequestSHA256 string   `json:"release_request_sha256"`
	PolicyReference      string   `json:"policy_reference"`
	PolicyVersion        string   `json:"policy_version"`
	RequiredCapabilities []string `json:"required_capabilities"`
}

// SignedHostCompatibilityRevocations is the currently trusted revocation
// snapshot. It is signed for a separate purpose from release approvals.
type SignedHostCompatibilityRevocations struct {
	Payload   json.RawMessage `json:"payload"`
	Signature string          `json:"signature"`
}

// HostCompatibilityRevocationsPayload is a signed snapshot, not a caller
// supplied deny list. A missing, stale, or invalid snapshot makes authority
// unavailable.
type HostCompatibilityRevocationsPayload struct {
	SchemaVersion  string   `json:"schema_version"`
	KeyID          string   `json:"key_id"`
	IssuedAt       string   `json:"issued_at"`
	ExpiresAt      string   `json:"expires_at"`
	RevokedRecords []string `json:"revoked_record_ids"`
}

// HostCompatibilityAuthorityBundle is loaded from a host-owned trust source.
// It must never be populated from city, pack, provider, or worker environment
// data. The controller reloads this bundle for each authority operation so an
// expiry or signed revocation takes effect before later work.
type HostCompatibilityAuthorityBundle struct {
	Keys       []HostCompatibilityPublicKey
	Records    []SignedHostCompatibilityRecord
	Revocation SignedHostCompatibilityRevocations
}

// HostCompatibilityAuthoritySource reads the current host trust material.
// Implementations must be supplied by a trusted controller startup path.
type HostCompatibilityAuthoritySource interface {
	Load(context.Context) (HostCompatibilityAuthorityBundle, error)
}

// HostCompatibilityAuthority verifies host-signed release records and
// revocations before making the generic qualification authority decision.
type HostCompatibilityAuthority struct {
	source           HostCompatibilityAuthoritySource
	maxRevocationAge time.Duration
	now              func() time.Time
}

var _ qualification.CompatibilityAuthority = (*HostCompatibilityAuthority)(nil)

// NewHostCompatibilityAuthority creates a fail-closed verifier over an
// injected host-owned source. maxRevocationAge must be a positive value from
// trusted server startup policy; there is no built-in freshness default.
func NewHostCompatibilityAuthority(source HostCompatibilityAuthoritySource, maxRevocationAge time.Duration, now func() time.Time) *HostCompatibilityAuthority {
	if now == nil {
		now = time.Now
	}
	return &HostCompatibilityAuthority{source: source, maxRevocationAge: maxRevocationAge, now: now}
}

// HostCompatibilityRecordSigningBytes returns the domain-separated canonical
// payload bytes a trusted release-record signer must sign.
func HostCompatibilityRecordSigningBytes(record HostCompatibilityRecordPayload) ([]byte, error) {
	if err := normalizeHostCompatibilityRecord(&record); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		return nil, err
	}
	return append([]byte(compatibilityRecordSigningDomain+"\n"), encoded...), nil
}

// HostCompatibilityRevocationsSigningBytes returns the domain-separated
// canonical payload bytes a revocation-list signer must sign.
func HostCompatibilityRevocationsSigningBytes(list HostCompatibilityRevocationsPayload) ([]byte, error) {
	if err := normalizeHostCompatibilityRevocations(&list); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(list)
	if err != nil {
		return nil, err
	}
	return append([]byte(compatibilityRevocationSigningDomain+"\n"), encoded...), nil
}

// Resolve returns policy requirements from exactly one signed record whose
// scope and controller release identity match. Duplicate records, a missing
// current revocation snapshot, expiration, and revocation all fail closed.
func (a *HostCompatibilityAuthority) Resolve(ctx context.Context, scope qualification.CompatibilityScope) (qualification.CompatibilityPolicy, error) {
	if a == nil || a.source == nil {
		return qualification.CompatibilityPolicy{}, fmt.Errorf("host compatibility authority is not configured: %w", qualification.ErrUnavailable)
	}
	scopeSHA, err := qualification.CompatibilityScopeIdentitySHA(scope)
	if err != nil {
		return qualification.CompatibilityPolicy{}, err
	}
	verified, err := a.loadVerifiedBundle(ctx)
	if err != nil {
		return qualification.CompatibilityPolicy{}, err
	}
	policy, _, err := resolveVerifiedHostCompatibilityRecord(scope, scopeSHA, verified, a.now())
	if err != nil {
		return qualification.CompatibilityPolicy{}, err
	}
	return policy, nil
}

// Authorize creates an opaque reference bound to the full proof-bearing
// request and the host-signed release record. It does not sign locally; the
// external record is the trust root and the computed reference is revalidated
// against that record and fresh capability proofs before every later action.
func (a *HostCompatibilityAuthority) Authorize(ctx context.Context, request qualification.CompatibilityRequest) (qualification.ActionAuthorization, error) {
	requestSHA, err := qualification.CompatibilityRequestIdentitySHA(request)
	if err != nil || requestSHA != request.RequestSHA256 {
		return qualification.ActionAuthorization{Status: qualification.StatusUnavailable, Reason: "compatibility_request_invalid"}, qualification.ErrUnavailable
	}
	scopeSHA, err := qualification.CompatibilityScopeIdentitySHA(request.Scope)
	if err != nil {
		return qualification.ActionAuthorization{Status: qualification.StatusUnavailable, Reason: "host_compatibility_policy_unavailable"}, qualification.ErrUnavailable
	}
	verified, err := a.loadVerifiedBundle(ctx)
	if err != nil {
		return qualification.ActionAuthorization{Status: qualification.StatusUnavailable, Reason: "host_compatibility_record_unavailable"}, err
	}
	policy, record, err := resolveVerifiedHostCompatibilityRecord(request.Scope, scopeSHA, verified, a.now())
	if err != nil || !samePolicy(policy, request.Policy) {
		return qualification.ActionAuthorization{Status: qualification.StatusUnavailable, Reason: "host_compatibility_policy_unavailable"}, qualification.ErrUnavailable
	}
	reference, err := hostCompatibilityActionReference(record.RecordID, request.ScopeSHA256, requestSHA, policy.PolicyVersion)
	if err != nil {
		return qualification.ActionAuthorization{Status: qualification.StatusUnavailable, Reason: "compatibility_reference_unavailable"}, err
	}
	return qualification.ActionAuthorization{
		Status:          qualification.StatusAuthorized,
		PolicyReference: policy.PolicyReference,
		PolicyVersion:   policy.PolicyVersion,
		RequestSHA256:   requestSHA,
		RecordID:        record.RecordID,
		Reference:       reference,
	}, nil
}

// Verify reloads the host record and signed revocation snapshot, then checks
// the exact request and reference. Removing/expiring/revoking the record makes
// already stamped action metadata unavailable.
func (a *HostCompatibilityAuthority) Verify(ctx context.Context, request qualification.CompatibilityRequest, authorization qualification.ActionAuthorization) error {
	requestSHA, err := qualification.CompatibilityRequestIdentitySHA(request)
	if err != nil || requestSHA != request.RequestSHA256 || authorization.Status != qualification.StatusAuthorized ||
		authorization.RequestSHA256 != requestSHA || authorization.PolicyReference != request.Policy.PolicyReference ||
		authorization.PolicyVersion != request.Policy.PolicyVersion || strings.TrimSpace(authorization.RecordID) == "" {
		return qualification.ErrUnavailable
	}
	scopeSHA, err := qualification.CompatibilityScopeIdentitySHA(request.Scope)
	if err != nil {
		return qualification.ErrUnavailable
	}
	verified, err := a.loadVerifiedBundle(ctx)
	if err != nil {
		return qualification.ErrUnavailable
	}
	policy, record, err := resolveVerifiedHostCompatibilityRecord(request.Scope, scopeSHA, verified, a.now())
	if err != nil || !samePolicy(policy, request.Policy) || authorization.RecordID != record.RecordID {
		return qualification.ErrUnavailable
	}
	wantReference, err := hostCompatibilityActionReference(record.RecordID, request.ScopeSHA256, requestSHA, policy.PolicyVersion)
	if err != nil || authorization.Reference != wantReference {
		return qualification.ErrUnavailable
	}
	return nil
}

type verifiedHostCompatibilityBundle struct {
	records []HostCompatibilityRecordPayload
	revoked map[string]struct{}
}

func (a *HostCompatibilityAuthority) loadVerifiedBundle(ctx context.Context) (verifiedHostCompatibilityBundle, error) {
	if a == nil || a.source == nil || a.maxRevocationAge <= 0 {
		return verifiedHostCompatibilityBundle{}, fmt.Errorf("host compatibility authority policy is unavailable: %w", qualification.ErrUnavailable)
	}
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return verifiedHostCompatibilityBundle{}, err
		}
	}
	bundle, err := a.source.Load(ctx)
	if err != nil {
		return verifiedHostCompatibilityBundle{}, fmt.Errorf("loading host compatibility trust: %w", qualification.ErrUnavailable)
	}
	keys, err := validateHostCompatibilityKeys(bundle.Keys)
	if err != nil {
		return verifiedHostCompatibilityBundle{}, err
	}
	revoked, err := verifyHostCompatibilityRevocations(bundle.Revocation, keys, a.now(), a.maxRevocationAge)
	if err != nil {
		return verifiedHostCompatibilityBundle{}, err
	}
	records := make([]HostCompatibilityRecordPayload, 0, len(bundle.Records))
	seenRecordIDs := make(map[string]struct{}, len(bundle.Records))
	for _, envelope := range bundle.Records {
		record, err := verifyHostCompatibilityRecord(envelope, keys)
		if err != nil {
			return verifiedHostCompatibilityBundle{}, err
		}
		if _, duplicate := seenRecordIDs[record.RecordID]; duplicate {
			return verifiedHostCompatibilityBundle{}, fmt.Errorf("duplicate host compatibility record id %q: %w", record.RecordID, qualification.ErrUnavailable)
		}
		seenRecordIDs[record.RecordID] = struct{}{}
		records = append(records, record)
	}
	return verifiedHostCompatibilityBundle{records: records, revoked: revoked}, nil
}

func resolveVerifiedHostCompatibilityRecord(scope qualification.CompatibilityScope, scopeSHA string, bundle verifiedHostCompatibilityBundle, now time.Time) (qualification.CompatibilityPolicy, HostCompatibilityRecordPayload, error) {
	var match *HostCompatibilityRecordPayload
	for i := range bundle.records {
		record := bundle.records[i]
		if record.ScopeSHA256 != scopeSHA || record.ReleaseRequestSHA256 != scope.ReleaseRequestSHA256 {
			continue
		}
		if err := validateHostCompatibilityFreshness(record.IssuedAt, record.ExpiresAt, now); err != nil {
			return qualification.CompatibilityPolicy{}, HostCompatibilityRecordPayload{}, fmt.Errorf("compatibility release record %q is stale: %w", record.RecordID, qualification.ErrUnavailable)
		}
		if _, isRevoked := bundle.revoked[record.RecordID]; isRevoked {
			return qualification.CompatibilityPolicy{}, HostCompatibilityRecordPayload{}, fmt.Errorf("compatibility release record %q is revoked: %w", record.RecordID, qualification.ErrUnavailable)
		}
		if match != nil {
			return qualification.CompatibilityPolicy{}, HostCompatibilityRecordPayload{}, fmt.Errorf("multiple compatibility release records match this scope: %w", qualification.ErrUnavailable)
		}
		copyRecord := record
		match = &copyRecord
	}
	if match == nil {
		return qualification.CompatibilityPolicy{}, HostCompatibilityRecordPayload{}, fmt.Errorf("no current host compatibility record matches the exact scope: %w", qualification.ErrUnavailable)
	}
	policy := qualification.CompatibilityPolicy{
		Status: qualification.StatusAvailable, ScopeSHA256: scopeSHA,
		PolicyReference: match.PolicyReference, PolicyVersion: match.PolicyVersion,
		RequiredCapabilities: append([]string(nil), match.RequiredCapabilities...),
	}
	return policy, *match, nil
}

type verifiedHostKey struct {
	role string
	key  ed25519.PublicKey
}

func validateHostCompatibilityKeys(entries []HostCompatibilityPublicKey) (map[string]verifiedHostKey, error) {
	if len(entries) == 0 {
		return nil, fmt.Errorf("host compatibility keyring is empty: %w", qualification.ErrUnavailable)
	}
	keys := make(map[string]verifiedHostKey, len(entries))
	material := make(map[string]string, len(entries))
	for _, entry := range entries {
		id := strings.TrimSpace(entry.KeyID)
		role := strings.TrimSpace(entry.Role)
		encoded := strings.TrimSpace(entry.PublicKey)
		if id == "" || id != entry.KeyID || encoded == "" || encoded != entry.PublicKey {
			return nil, fmt.Errorf("host compatibility key identity is invalid: %w", qualification.ErrUnavailable)
		}
		if role != HostCompatibilityRecordRole && role != HostCompatibilityRevocationRole {
			return nil, fmt.Errorf("host compatibility key role %q is unsupported: %w", role, qualification.ErrUnavailable)
		}
		if _, duplicate := keys[id]; duplicate {
			return nil, fmt.Errorf("duplicate host compatibility key %q: %w", id, qualification.ErrUnavailable)
		}
		raw, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil || len(raw) != ed25519.PublicKeySize || base64.StdEncoding.EncodeToString(raw) != encoded {
			return nil, fmt.Errorf("host compatibility key %q is malformed: %w", id, qualification.ErrUnavailable)
		}
		keyDigest := sha256.Sum256(raw)
		fingerprint := hex.EncodeToString(keyDigest[:])
		if prior, duplicate := material[fingerprint]; duplicate {
			return nil, fmt.Errorf("host compatibility key %q reuses key material from %q: %w", id, prior, qualification.ErrUnavailable)
		}
		material[fingerprint] = id
		keys[id] = verifiedHostKey{role: role, key: append(ed25519.PublicKey(nil), raw...)}
	}
	return keys, nil
}

func verifyHostCompatibilityRecord(envelope SignedHostCompatibilityRecord, keys map[string]verifiedHostKey) (HostCompatibilityRecordPayload, error) {
	var record HostCompatibilityRecordPayload
	if len(envelope.Payload) == 0 || len(envelope.Payload) > maxHostCompatibilityFileBytes {
		return record, fmt.Errorf("host compatibility release record payload is unavailable: %w", qualification.ErrUnavailable)
	}
	if err := decodeHostCompatibilityJSON(envelope.Payload, &record); err != nil {
		return record, fmt.Errorf("decode host compatibility release record: %w", qualification.ErrUnavailable)
	}
	canonical, err := HostCompatibilityRecordSigningBytes(record)
	if err != nil || !bytes.Equal(canonical[len(compatibilityRecordSigningDomain)+1:], envelope.Payload) {
		return record, fmt.Errorf("host compatibility release record is not canonical: %w", qualification.ErrUnavailable)
	}
	key, ok := keys[record.KeyID]
	if !ok || key.role != HostCompatibilityRecordRole {
		return record, fmt.Errorf("host compatibility release key is unavailable: %w", qualification.ErrUnavailable)
	}
	signature, err := base64.RawURLEncoding.DecodeString(envelope.Signature)
	if err != nil || len(signature) != ed25519.SignatureSize || base64.RawURLEncoding.EncodeToString(signature) != envelope.Signature || !ed25519.Verify(key.key, canonical, signature) {
		return record, fmt.Errorf("host compatibility release signature is invalid: %w", qualification.ErrUnavailable)
	}
	return record, nil
}

func verifyHostCompatibilityRevocations(envelope SignedHostCompatibilityRevocations, keys map[string]verifiedHostKey, now time.Time, maxAge time.Duration) (map[string]struct{}, error) {
	if maxAge <= 0 {
		return nil, fmt.Errorf("host compatibility maximum revocation age is unavailable: %w", qualification.ErrUnavailable)
	}
	var list HostCompatibilityRevocationsPayload
	if len(envelope.Payload) == 0 || len(envelope.Payload) > maxHostCompatibilityFileBytes {
		return nil, fmt.Errorf("host compatibility revocation list is unavailable: %w", qualification.ErrUnavailable)
	}
	if err := decodeHostCompatibilityJSON(envelope.Payload, &list); err != nil {
		return nil, fmt.Errorf("decode host compatibility revocation list: %w", qualification.ErrUnavailable)
	}
	canonical, err := HostCompatibilityRevocationsSigningBytes(list)
	if err != nil || !bytes.Equal(canonical[len(compatibilityRevocationSigningDomain)+1:], envelope.Payload) {
		return nil, fmt.Errorf("host compatibility revocation list is not canonical: %w", qualification.ErrUnavailable)
	}
	key, ok := keys[list.KeyID]
	if !ok || key.role != HostCompatibilityRevocationRole {
		return nil, fmt.Errorf("host compatibility revocation key is unavailable: %w", qualification.ErrUnavailable)
	}
	signature, err := base64.RawURLEncoding.DecodeString(envelope.Signature)
	if err != nil || len(signature) != ed25519.SignatureSize || base64.RawURLEncoding.EncodeToString(signature) != envelope.Signature || !ed25519.Verify(key.key, canonical, signature) {
		return nil, fmt.Errorf("host compatibility revocation signature is invalid: %w", qualification.ErrUnavailable)
	}
	if err := validateHostCompatibilityFreshness(list.IssuedAt, list.ExpiresAt, now); err != nil {
		return nil, fmt.Errorf("host compatibility revocation list is stale: %w", qualification.ErrUnavailable)
	}
	issued, _ := time.Parse(time.RFC3339Nano, list.IssuedAt)
	expires, _ := time.Parse(time.RFC3339Nano, list.ExpiresAt)
	if now.Sub(issued) > maxAge || expires.Sub(issued) > maxAge {
		return nil, fmt.Errorf("host compatibility revocation list exceeds its trusted maximum age: %w", qualification.ErrUnavailable)
	}
	revoked := make(map[string]struct{}, len(list.RevokedRecords))
	for _, recordID := range list.RevokedRecords {
		revoked[recordID] = struct{}{}
	}
	return revoked, nil
}

func normalizeHostCompatibilityRecord(record *HostCompatibilityRecordPayload) error {
	if record == nil || record.SchemaVersion != HostCompatibilityRecordSchemaV1 || strings.TrimSpace(record.RecordID) == "" || strings.TrimSpace(record.RecordID) != record.RecordID ||
		strings.TrimSpace(record.KeyID) == "" || strings.TrimSpace(record.KeyID) != record.KeyID || record.Status != "approved" ||
		!isSHA256(record.ScopeSHA256) || !isSHA256(record.ReleaseRequestSHA256) ||
		strings.TrimSpace(record.PolicyReference) == "" || strings.TrimSpace(record.PolicyReference) != record.PolicyReference ||
		strings.TrimSpace(record.PolicyVersion) == "" || strings.TrimSpace(record.PolicyVersion) != record.PolicyVersion {
		return fmt.Errorf("host compatibility release record identity is incomplete: %w", qualification.ErrUnavailable)
	}
	capabilities, err := canonicalHostCapabilities(record.RequiredCapabilities)
	if err != nil {
		return err
	}
	record.RequiredCapabilities = capabilities
	if _, err := time.Parse(time.RFC3339Nano, record.IssuedAt); err != nil {
		return fmt.Errorf("host compatibility issued_at is invalid: %w", qualification.ErrUnavailable)
	}
	if _, err := time.Parse(time.RFC3339Nano, record.ExpiresAt); err != nil {
		return fmt.Errorf("host compatibility expires_at is invalid: %w", qualification.ErrUnavailable)
	}
	return nil
}

func normalizeHostCompatibilityRevocations(list *HostCompatibilityRevocationsPayload) error {
	if list == nil || list.SchemaVersion != HostCompatibilityRevocationsSchemaV1 || strings.TrimSpace(list.KeyID) == "" || strings.TrimSpace(list.KeyID) != list.KeyID {
		return fmt.Errorf("host compatibility revocation identity is incomplete: %w", qualification.ErrUnavailable)
	}
	if _, err := time.Parse(time.RFC3339Nano, list.IssuedAt); err != nil {
		return fmt.Errorf("host compatibility revocation issued_at is invalid: %w", qualification.ErrUnavailable)
	}
	if _, err := time.Parse(time.RFC3339Nano, list.ExpiresAt); err != nil {
		return fmt.Errorf("host compatibility revocation expires_at is invalid: %w", qualification.ErrUnavailable)
	}
	ids := append([]string(nil), list.RevokedRecords...)
	for i := range ids {
		ids[i] = strings.TrimSpace(ids[i])
		if ids[i] == "" {
			return fmt.Errorf("host compatibility revocation id is empty: %w", qualification.ErrUnavailable)
		}
	}
	sort.Strings(ids)
	for i := 1; i < len(ids); i++ {
		if ids[i] == ids[i-1] {
			return fmt.Errorf("duplicate host compatibility revocation id %q: %w", ids[i], qualification.ErrUnavailable)
		}
	}
	list.RevokedRecords = ids
	return nil
}

func canonicalHostCapabilities(values []string) ([]string, error) {
	if len(values) == 0 {
		return nil, fmt.Errorf("host compatibility capability set is empty: %w", qualification.ErrUnavailable)
	}
	result := append([]string(nil), values...)
	for i := range result {
		result[i] = strings.TrimSpace(result[i])
		if result[i] == "" {
			return nil, fmt.Errorf("host compatibility capability is empty: %w", qualification.ErrUnavailable)
		}
	}
	sort.Strings(result)
	for i := 1; i < len(result); i++ {
		if result[i] == result[i-1] {
			return nil, fmt.Errorf("duplicate host compatibility capability %q: %w", result[i], qualification.ErrUnavailable)
		}
	}
	return result, nil
}

func validateHostCompatibilityFreshness(issuedAt, expiresAt string, now time.Time) error {
	issued, err := time.Parse(time.RFC3339Nano, issuedAt)
	if err != nil {
		return err
	}
	expires, err := time.Parse(time.RFC3339Nano, expiresAt)
	if err != nil || !expires.After(issued) || issued.After(now.Add(compatibilityAuthorityClockSkew)) || !expires.After(now) {
		return errors.New("host compatibility authority is expired or not yet valid")
	}
	return nil
}

func decodeHostCompatibilityJSON(raw []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("trailing host compatibility JSON")
		}
		return err
	}
	return nil
}

func hostCompatibilityActionReference(recordID, scopeSHA, requestSHA, policyVersion string) (string, error) {
	digest, err := qualification.DigestJSON(struct {
		Domain        string `json:"domain"`
		RecordID      string `json:"record_id"`
		ScopeSHA256   string `json:"scope_sha256"`
		RequestSHA256 string `json:"request_sha256"`
		PolicyVersion string `json:"policy_version"`
	}{
		Domain: "gascity.compatibility-action-reference.v1", RecordID: recordID,
		ScopeSHA256: scopeSHA, RequestSHA256: requestSHA, PolicyVersion: policyVersion,
	})
	if err != nil {
		return "", err
	}
	return "compatibility:" + digest, nil
}
