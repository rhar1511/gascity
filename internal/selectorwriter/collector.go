// Package selectorwriter composes a bounded host-signed envelope around the
// canonical external-writer ledger used by selectorinventory. It owns no host
// I/O: production order, Linux-scope, identity, key, and retention adapters
// are injected by trusted host startup code.
package selectorwriter

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gastownhall/gascity/internal/selectorinventory"
)

const (
	SchemaVersion     = "gc.external-writer-host-record.v1"
	SigningDomain     = "gascity.external-writer-host-record.v1\n"
	KeyPurpose        = "external_writer_host_collector"
	MaxObservationAge = 5 * time.Minute
	RetentionPeriod   = 30 * 24 * time.Hour
	maxRecordBytes    = 6 << 20
	maxLedgerBytes    = 4 << 20
	maxLedgerEntries  = 100000
	maxScopeEntries   = 4096
	maxScopeCommands  = 8192
)

const (
	// ScopeCron covers system/user cron and anacron configuration.
	ScopeCron = "cron"
	// ScopeManual covers direct process/manual launch evidence.
	ScopeManual = "manual"
	// ScopeOrders covers the Gas City order execution ledger.
	ScopeOrders = "orders"
	// ScopeServices covers system and user systemd units and timers.
	ScopeServices = "services"
	// ScopeStartup covers startup scripts and boot/login launch wiring.
	ScopeStartup = "startup"
	completeAtom = "external-writer-mandatory-scopes.v1"
)

var mandatoryScopes = []string{ScopeCron, ScopeManual, ScopeOrders, ScopeServices, ScopeStartup}

var (
	ErrUnavailable = errors.New("external writer evidence unavailable")
	ErrMalformed   = errors.New("external writer evidence malformed")
	ErrBinding     = errors.New("external writer evidence binding mismatch")
	ErrSignature   = errors.New("external writer record signature invalid")
	ErrStale       = errors.New("external writer record stale or outside retention")
)

// MandatoryScopes returns the fixed scope contract in canonical sort order.
func MandatoryScopes() []string { return append([]string(nil), mandatoryScopes...) }

// HostIdentity is transient input. The raw host and boot identifiers are
// never serialized; the signed record carries domain-separated SHA-256
// fingerprints so verification can bind an expected exact machine and boot.
type HostIdentity struct {
	HostID        string    `json:"-"`
	BootID        string    `json:"-"`
	BootStartedAt time.Time `json:"-"`
}

// IdentityReader reads the same fixed host identity and current boot start
// before and after the source snapshots. A changed boot ID, a window starting
// before this boot, or changed boot start makes the observation unavailable.
type IdentityReader interface {
	ReadIdentity(context.Context) (HostIdentity, error)
}

// OrderLedgerSnapshot is a bounded, read-only view of the canonical Gas City
// order sequence and its retention watermark for one requested window.
type OrderLedgerSnapshot struct {
	Coverage       selectorinventory.ExternalSourceCoverage
	EvidenceSHA256 string
	Retention      selectorinventory.CaptureWindow
	SequenceStart  uint64
	SequenceEnd    uint64
	Sequences      []selectorinventory.LedgerSequence
}

// LinuxScopeSnapshot pairs canonical coverage with the digest of the
// bounded transient source evidence for that exact mandatory scope.
type LinuxScopeSnapshot struct {
	Coverage       selectorinventory.ExternalSourceCoverage
	EvidenceSHA256 string
}

// OrderLedgerReader reads the canonical order ledger without changing it.
// It must return every retained sequence in the requested fixed window.
type OrderLedgerReader interface {
	ReadOrderLedger(context.Context, selectorinventory.CaptureWindow, selectorinventory.Limits) (OrderLedgerSnapshot, error)
}

// LinuxScopeReader reads one of ScopeStartup, ScopeServices, ScopeCron, or
// ScopeManual without running discovered commands. It must honor the supplied
// byte, entry, and command limits. The returned digest identifies the bounded
// transient source snapshot; raw content never enters a signed record.
type LinuxScopeReader interface {
	ReadLinuxScope(context.Context, string, selectorinventory.CaptureWindow, selectorinventory.Limits) (LinuxScopeSnapshot, error)
}

// SigningKeyProvider supplies the dedicated root-owned host-collector key.
// Key provisioning and access control belong to trusted host startup code;
// this collector never reads a key path, generates a key, or reuses the
// selector-attestation or mutation-signing keys.
type SigningKeyProvider interface {
	PrivateKey(context.Context, string) (ed25519.PrivateKey, error)
}

// RetentionSink durably and immutably retains the exact signed record through retainUntil.
// Implementations must not return success unless they can honor the full
// retention period, and must reject replacement of an existing observation
// ID. The collector does not choose or mutate a live path.
type RetentionSink interface {
	Retain(context.Context, string, []byte, time.Time) error
}

// Clock supplies trusted collector timestamps.
type Clock interface {
	Now() time.Time
}

// Request carries controller facts from the exact controller generation
// being observed. Capture is fixed by the caller before any source read.
type Request struct {
	ObservationID            string
	Controller               selectorinventory.ControllerBinding
	ExecutionGeneration      string
	Capture                  selectorinventory.CaptureWindow
	InFlightResolutionSHA256 string
}

// Collector combines order-ledger and Linux source evidence, then signs the
// existing canonical ExternalLedgerEvidence bytes. It has no default host
// readers or key source.
type Collector struct {
	KeyID     string
	Keys      SigningKeyProvider
	Identity  IdentityReader
	Orders    OrderLedgerReader
	Linux     LinuxScopeReader
	Clock     Clock
	Retention RetentionSink
}

// HostRecordClaims binds the exact canonical ledger digest to its host, boot,
// controller generation/build, execution generation, and fixed capture window.
type HostRecordClaims struct {
	SchemaVersion            string                          `json:"schema_version"`
	KeyPurpose               string                          `json:"key_purpose"`
	KeyID                    string                          `json:"key_id"`
	ObservationID            string                          `json:"observation_id"`
	HostFingerprintSHA256    string                          `json:"host_fingerprint_sha256"`
	BootFingerprintSHA256    string                          `json:"boot_fingerprint_sha256"`
	BootStartedAt            time.Time                       `json:"boot_started_at"`
	ControllerSnapshotSHA256 string                          `json:"controller_snapshot_sha256"`
	ControllerGeneration     uint64                          `json:"controller_generation"`
	ControllerBuild          string                          `json:"controller_build"`
	ExecutionGeneration      string                          `json:"execution_generation"`
	Capture                  selectorinventory.CaptureWindow `json:"capture"`
	LedgerSHA256             string                          `json:"ledger_sha256"`
	ScopeEvidence            []ScopeEvidenceDigest           `json:"scope_evidence"`
	IssuedAt                 time.Time                       `json:"issued_at"`
	RetainUntil              time.Time                       `json:"retain_until"`
}

// ScopeEvidenceDigest is a privacy-limited digest of one mandatory source
// snapshot. It is carried in the signed host envelope and paired one-to-one
// with the corresponding schema-v1 ledger coverage entry.
type ScopeEvidenceDigest struct {
	ScopeID        string `json:"scope_id"`
	EvidenceSHA256 string `json:"evidence_sha256"`
}

// SignedRecord wraps the canonical selectorinventory ledger bytes. The
// ledger schema remains the source of truth; this envelope adds host key
// authentication and the binding required by ADR 0025.
type SignedRecord struct {
	Claims    HostRecordClaims `json:"claims"`
	Ledger    string           `json:"ledger_base64"`
	Signature string           `json:"signature"`
}

// Collect reads all mandatory scopes over one exact window and returns the
// signed canonical external ledger. Any incomplete scope, stale window,
// source error, reboot, or failed retention write returns unavailable. Each
// source window is capped at five minutes as a collection bound as well as
// enforcing a five-minute freshness limit.
func (c Collector) Collect(ctx context.Context, request Request) ([]byte, error) {
	if ctx == nil || c.Keys == nil || c.Identity == nil || c.Orders == nil || c.Linux == nil || c.Clock == nil || c.Retention == nil ||
		!validKeyID(c.KeyID) || !validRequest(request) {
		return nil, ErrUnavailable
	}
	started := c.Clock.Now().UTC()
	if !validFreshWindow(request.Capture, started) {
		return nil, ErrStale
	}
	identityBefore, err := c.Identity.ReadIdentity(ctx)
	if err != nil || !validIdentity(identityBefore) || request.Capture.Start.Before(identityBefore.BootStartedAt) {
		return nil, ErrUnavailable
	}
	orders, err := c.Orders.ReadOrderLedger(ctx, request.Capture, orderReadLimits())
	if err != nil || !validOrderSnapshot(orders, request.Capture, started) {
		return nil, ErrUnavailable
	}
	coverageByID := make(map[string]selectorinventory.ExternalSourceCoverage, len(mandatoryScopes))
	scopeEvidenceByID := make(map[string]string, len(mandatoryScopes))
	coverageByID[ScopeOrders] = orders.Coverage
	scopeEvidenceByID[ScopeOrders] = orders.EvidenceSHA256
	for _, scope := range mandatoryScopes {
		if scope == ScopeOrders {
			continue
		}
		result, readErr := c.Linux.ReadLinuxScope(ctx, scope, request.Capture, linuxReadLimits())
		if readErr != nil || !validScopeCoverage(result.Coverage, scope, request.Capture) || !validDigest(result.EvidenceSHA256) {
			return nil, ErrUnavailable
		}
		coverageByID[scope] = result.Coverage
		scopeEvidenceByID[scope] = result.EvidenceSHA256
	}
	identityAfter, err := c.Identity.ReadIdentity(ctx)
	if err != nil || !validIdentity(identityAfter) || identityAfter != identityBefore {
		return nil, ErrUnavailable
	}
	if err := ctx.Err(); err != nil {
		return nil, ErrUnavailable
	}
	issuedAt := c.Clock.Now().UTC()
	if !validFreshWindow(request.Capture, issuedAt) || issuedAt.Before(started) {
		return nil, ErrStale
	}
	loadedKey, err := c.Keys.PrivateKey(ctx, c.KeyID)
	if err != nil || len(loadedKey) != ed25519.PrivateKeySize {
		return nil, ErrUnavailable
	}
	privateKey := append(ed25519.PrivateKey(nil), loadedKey...)
	defer clear(privateKey)
	scopeEvidence := make([]ScopeEvidenceDigest, 0, len(mandatoryScopes))
	coverage := make([]selectorinventory.ExternalSourceCoverage, 0, len(mandatoryScopes))
	for _, scope := range mandatoryScopes {
		coverage = append(coverage, coverageByID[scope])
		scopeEvidence = append(scopeEvidence, ScopeEvidenceDigest{ScopeID: scope, EvidenceSHA256: scopeEvidenceByID[scope]})
	}

	ledger := selectorinventory.ExternalLedgerEvidence{
		SchemaVersion:            selectorinventory.ExternalLedgerSchemaVersion,
		ObservationID:            request.ObservationID,
		ControllerSnapshotSHA256: request.Controller.SnapshotSHA256,
		GraphConfigGeneration:    request.Controller.Generation,
		ControllerBuild:          request.Controller.Build,
		ExecutionGeneration:      request.ExecutionGeneration,
		Capture:                  request.Capture,
		Retention:                orders.Retention,
		SequenceStart:            orders.SequenceStart,
		SequenceEnd:              orders.SequenceEnd,
		Sequences:                append([]selectorinventory.LedgerSequence(nil), orders.Sequences...),
		ExternalScope:            MandatoryScopes(),
		SourceCoverage:           coverage,
		CompleteResultAtoms:      []selectorinventory.CompleteResultAtom{{Atom: completeAtom, Complete: true}},
		InFlightResolutionSHA256: request.InFlightResolutionSHA256,
	}
	canonicalLedger, ledgerJSON, err := selectorinventory.CanonicalizeExternalLedger(ledger)
	if err != nil || !validCanonicalLedger(canonicalLedger, ledgerJSON, request) {
		return nil, ErrUnavailable
	}
	claims := HostRecordClaims{
		SchemaVersion: SchemaVersion, KeyPurpose: KeyPurpose, KeyID: c.KeyID,
		ObservationID:            request.ObservationID,
		HostFingerprintSHA256:    hostFingerprint(identityBefore.HostID),
		BootFingerprintSHA256:    bootFingerprint(identityBefore.BootID),
		BootStartedAt:            identityBefore.BootStartedAt,
		ControllerSnapshotSHA256: request.Controller.SnapshotSHA256,
		ControllerGeneration:     request.Controller.Generation, ControllerBuild: request.Controller.Build,
		ExecutionGeneration: request.ExecutionGeneration, Capture: request.Capture,
		LedgerSHA256: canonicalLedger.DigestSHA256, ScopeEvidence: scopeEvidence, IssuedAt: issuedAt,
		RetainUntil: issuedAt.Add(RetentionPeriod),
	}
	claimsJSON, err := json.Marshal(claims)
	if err != nil {
		return nil, ErrUnavailable
	}
	signature := ed25519.Sign(privateKey, append([]byte(SigningDomain), claimsJSON...))
	signed := SignedRecord{
		Claims: claims, Ledger: base64.StdEncoding.EncodeToString(ledgerJSON),
		Signature: base64.RawURLEncoding.EncodeToString(signature),
	}
	raw, err := json.Marshal(signed)
	if err != nil || len(raw) > maxRecordBytes {
		return nil, ErrUnavailable
	}
	if err := c.Retention.Retain(ctx, request.ObservationID, raw, claims.RetainUntil); err != nil {
		return nil, ErrUnavailable
	}
	return raw, nil
}

// VerifiedRecord is the sealed output of signature, host, boot, freshness,
// retention, canonical-ledger, and mandatory-scope verification.
type VerifiedRecord struct {
	Claims HostRecordClaims
	ledger []byte
	seal   [sha256.Size]byte
}

// Valid reports whether the verified record still matches verifier output.
func (r VerifiedRecord) Valid() bool {
	if r.seal == ([sha256.Size]byte{}) {
		return false
	}
	expected := verifiedSeal(r.Claims, r.ledger)
	return bytes.Equal(r.seal[:], expected[:])
}

// ExternalLedgerJSON returns a copy of the canonical existing ledger bytes.
func (r VerifiedRecord) ExternalLedgerJSON() []byte { return append([]byte(nil), r.ledger...) }

// Verifier trusts one dedicated public key. Public-key configuration is
// supplied by controller startup code and kept separate from selector
// attestation, review, and protected-mutation authorities.
type Verifier struct {
	keyID string
	key   ed25519.PublicKey
}

// NewVerifier pins one external-writer collector key.
func NewVerifier(keyID string, publicKey ed25519.PublicKey) (*Verifier, error) {
	if !validKeyID(keyID) || len(publicKey) != ed25519.PublicKeySize {
		return nil, ErrMalformed
	}
	return &Verifier{keyID: keyID, key: append(ed25519.PublicKey(nil), publicKey...)}, nil
}

// Verify authenticates a retained record for the exact expected host and boot.
func (v *Verifier) Verify(ctx context.Context, raw []byte, now time.Time, hostID, bootID string) (VerifiedRecord, error) {
	if ctx == nil || v == nil || len(v.key) != ed25519.PublicKeySize || len(raw) == 0 || len(raw) > maxRecordBytes ||
		!validAtom(hostID, 512) || !validAtom(bootID, 512) {
		return VerifiedRecord{}, ErrUnavailable
	}
	if err := ctx.Err(); err != nil {
		return VerifiedRecord{}, ErrUnavailable
	}
	var signed SignedRecord
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&signed); err != nil {
		return VerifiedRecord{}, ErrMalformed
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return VerifiedRecord{}, ErrMalformed
	}
	canonical, err := json.Marshal(signed)
	if err != nil || !bytes.Equal(raw, canonical) {
		return VerifiedRecord{}, ErrMalformed
	}
	claimsJSON, err := json.Marshal(signed.Claims)
	if err != nil || !validClaims(signed.Claims) || signed.Claims.KeyID != v.keyID {
		return VerifiedRecord{}, ErrMalformed
	}
	signature, err := base64.RawURLEncoding.DecodeString(signed.Signature)
	if err != nil || len(signature) != ed25519.SignatureSize || base64.RawURLEncoding.EncodeToString(signature) != signed.Signature {
		return VerifiedRecord{}, ErrMalformed
	}
	if !ed25519.Verify(v.key, append([]byte(SigningDomain), claimsJSON...), signature) {
		return VerifiedRecord{}, ErrSignature
	}
	ledgerJSON, err := base64.StdEncoding.DecodeString(signed.Ledger)
	if err != nil || len(ledgerJSON) == 0 || len(ledgerJSON) > maxLedgerBytes || base64.StdEncoding.EncodeToString(ledgerJSON) != signed.Ledger {
		return VerifiedRecord{}, ErrMalformed
	}
	if err := validateSignedLedger(signed.Claims, ledgerJSON); err != nil {
		return VerifiedRecord{}, err
	}
	now = now.UTC()
	if now.IsZero() || signed.Claims.IssuedAt.After(now) || now.Sub(signed.Claims.IssuedAt) > MaxObservationAge ||
		!validFreshWindow(signed.Claims.Capture, now) || signed.Claims.RetainUntil.Before(signed.Claims.IssuedAt.Add(RetentionPeriod)) || !now.Before(signed.Claims.RetainUntil) {
		return VerifiedRecord{}, ErrStale
	}
	if signed.Claims.HostFingerprintSHA256 != hostFingerprint(hostID) || signed.Claims.BootFingerprintSHA256 != bootFingerprint(bootID) {
		return VerifiedRecord{}, ErrBinding
	}
	if signed.Claims.Capture.Start.Before(signed.Claims.BootStartedAt) {
		return VerifiedRecord{}, ErrBinding
	}
	record := VerifiedRecord{Claims: signed.Claims, ledger: append([]byte(nil), ledgerJSON...)}
	record.seal = verifiedSeal(record.Claims, record.ledger)
	return record, nil
}

// VerifyAndJoin validates the host record first, then delegates to the
// existing canonical external-ledger/controller join. This is the adapter
// that keeps host signing attached to the established ledger contract.
func (v *Verifier) VerifyAndJoin(ctx context.Context, raw []byte, expected selectorinventory.ExternalLedgerExpectation, registry selectorinventory.RegistrySnapshotInput, hostID, bootID string) selectorinventory.ExternalLedgerJoinResult {
	verified, err := v.Verify(ctx, raw, expected.TrustedNow, hostID, bootID)
	if err != nil || !verified.Valid() {
		return selectorinventory.ExternalLedgerJoinResult{Status: selectorinventory.StatusUnavailable, IssueCode: "host_collector_record_unavailable"}
	}
	if expected.MaxObservationAge <= 0 || !equalStringList(expected.ExternalScope, mandatoryScopes) ||
		!containsString(expected.RequiredResultAtoms, completeAtom) ||
		expected.Controller.SnapshotSHA256 != verified.Claims.ControllerSnapshotSHA256 ||
		expected.Controller.Generation != verified.Claims.ControllerGeneration || expected.Controller.Build != verified.Claims.ControllerBuild ||
		expected.ExecutionGeneration != verified.Claims.ExecutionGeneration || expected.ObservationID != verified.Claims.ObservationID ||
		!expected.Attestation.Valid() || expected.Attestation.ExternalWriterInventorySHA256 == "" {
		return selectorinventory.ExternalLedgerJoinResult{Status: selectorinventory.StatusUnavailable, IssueCode: "host_collector_binding_mismatch"}
	}
	if expected.MaxObservationAge > MaxObservationAge {
		expected.MaxObservationAge = MaxObservationAge
	}
	return selectorinventory.JoinExternalLedger(verified.ledger, expected, registry)
}

func validRequest(request Request) bool {
	return validAtom(request.ObservationID, 200) && validControllerBinding(request.Controller) &&
		validAtom(request.ExecutionGeneration, 256) && validCapture(request.Capture) &&
		validDigest(request.InFlightResolutionSHA256)
}

func orderReadLimits() selectorinventory.Limits {
	return selectorinventory.Limits{MaxEntries: maxLedgerEntries, MaxBytes: maxLedgerBytes}
}

func linuxReadLimits() selectorinventory.Limits {
	return selectorinventory.Limits{MaxEntries: maxScopeEntries, MaxBytes: maxLedgerBytes, MaxCommands: maxScopeCommands}
}

func validOrderSnapshot(snapshot OrderLedgerSnapshot, window selectorinventory.CaptureWindow, now time.Time) bool {
	if !validScopeCoverage(snapshot.Coverage, ScopeOrders, window) || !validCapture(snapshot.Retention) ||
		snapshot.Retention.Start.Location() != time.UTC || snapshot.Retention.End.Location() != time.UTC ||
		snapshot.Retention.Start.After(window.Start) || snapshot.Retention.End.Before(window.End) || snapshot.Retention.End.After(now) ||
		!validDigest(snapshot.EvidenceSHA256) ||
		snapshot.Sequences == nil || len(snapshot.Sequences) == 0 || len(snapshot.Sequences) > maxLedgerEntries ||
		snapshot.SequenceStart == 0 || snapshot.SequenceEnd < snapshot.SequenceStart ||
		uint64(len(snapshot.Sequences)) != snapshot.SequenceEnd-snapshot.SequenceStart+1 {
		return false
	}
	for index, sequence := range snapshot.Sequences {
		if sequence.Sequence != snapshot.SequenceStart+uint64(index) || sequence.SourceScope != ScopeOrders || !validDigest(sequence.EntrySHA256) {
			return false
		}
	}
	return true
}

func validScopeCoverage(coverage selectorinventory.ExternalSourceCoverage, scope string, window selectorinventory.CaptureWindow) bool {
	return coverage.ScopeID == scope && coverage.Status == selectorinventory.StatusAvailable && coverage.Complete &&
		validCapture(coverage.Capture) && coverage.Capture.Start.Location() == time.UTC && coverage.Capture.End.Location() == time.UTC &&
		coverage.Capture.Start.Equal(window.Start) && coverage.Capture.End.Equal(window.End)
}

func validCanonicalLedger(ledger selectorinventory.ExternalLedgerEvidence, raw []byte, request Request) bool {
	if len(raw) == 0 || len(raw) > maxLedgerBytes || ledger.ObservationID != request.ObservationID ||
		ledger.ControllerSnapshotSHA256 != request.Controller.SnapshotSHA256 || ledger.GraphConfigGeneration != request.Controller.Generation ||
		ledger.ControllerBuild != request.Controller.Build || ledger.ExecutionGeneration != request.ExecutionGeneration ||
		!ledger.Capture.Start.Equal(request.Capture.Start) || !ledger.Capture.End.Equal(request.Capture.End) ||
		ledger.InFlightResolutionSHA256 != request.InFlightResolutionSHA256 || !equalStringList(ledger.ExternalScope, mandatoryScopes) ||
		len(ledger.SourceCoverage) != len(mandatoryScopes) || len(ledger.CompleteResultAtoms) != 1 ||
		ledger.CompleteResultAtoms[0] != (selectorinventory.CompleteResultAtom{Atom: completeAtom, Complete: true}) {
		return false
	}
	for index, scope := range mandatoryScopes {
		if ledger.SourceCoverage[index].ScopeID != scope || !validScopeCoverage(ledger.SourceCoverage[index], scope, request.Capture) {
			return false
		}
	}
	return true
}

func validateSignedLedger(claims HostRecordClaims, raw []byte) error {
	var ledger selectorinventory.ExternalLedgerEvidence
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&ledger); err != nil {
		return ErrMalformed
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return ErrMalformed
	}
	canonicalLedger, canonical, err := selectorinventory.CanonicalizeExternalLedger(ledger)
	if err != nil || !bytes.Equal(raw, canonical) {
		return ErrMalformed
	}
	request := Request{
		ObservationID: claims.ObservationID,
		Controller: selectorinventory.ControllerBinding{
			SnapshotSHA256: claims.ControllerSnapshotSHA256, Generation: claims.ControllerGeneration, Build: claims.ControllerBuild,
		},
		ExecutionGeneration: claims.ExecutionGeneration, Capture: claims.Capture,
		InFlightResolutionSHA256: ledger.InFlightResolutionSHA256,
	}
	if !validCanonicalLedger(canonicalLedger, canonical, request) || canonicalLedger.DigestSHA256 != claims.LedgerSHA256 {
		return ErrBinding
	}
	if !validScopeEvidence(claims.ScopeEvidence) {
		return ErrBinding
	}
	return nil
}

func validClaims(claims HostRecordClaims) bool {
	return claims.SchemaVersion == SchemaVersion && claims.KeyPurpose == KeyPurpose && validKeyID(claims.KeyID) &&
		validAtom(claims.ObservationID, 200) && validDigest(claims.HostFingerprintSHA256) && validDigest(claims.BootFingerprintSHA256) &&
		validControllerBinding(selectorinventory.ControllerBinding{
			SnapshotSHA256: claims.ControllerSnapshotSHA256, Generation: claims.ControllerGeneration, Build: claims.ControllerBuild,
		}) && validAtom(claims.ExecutionGeneration, 256) && validCapture(claims.Capture) && validDigest(claims.LedgerSHA256) &&
		validScopeEvidence(claims.ScopeEvidence) && !claims.BootStartedAt.IsZero() && claims.BootStartedAt.Location() == time.UTC &&
		!claims.Capture.Start.Before(claims.BootStartedAt) &&
		!claims.IssuedAt.IsZero() && claims.IssuedAt.Location() == time.UTC && !claims.RetainUntil.IsZero() && claims.RetainUntil.Location() == time.UTC
}

func validScopeEvidence(evidence []ScopeEvidenceDigest) bool {
	if len(evidence) != len(mandatoryScopes) {
		return false
	}
	for index, scope := range mandatoryScopes {
		if evidence[index].ScopeID != scope || !validDigest(evidence[index].EvidenceSHA256) {
			return false
		}
	}
	return true
}

func validFreshWindow(window selectorinventory.CaptureWindow, now time.Time) bool {
	return validCapture(window) && window.Start.Location() == time.UTC && window.End.Location() == time.UTC &&
		!now.IsZero() && now.Location() == time.UTC && window.End.Sub(window.Start) <= MaxObservationAge &&
		!window.End.After(now) && now.Sub(window.End) <= MaxObservationAge
}

func validCapture(window selectorinventory.CaptureWindow) bool {
	return !window.Start.IsZero() && !window.End.IsZero() && window.End.After(window.Start)
}

func validIdentity(identity HostIdentity) bool {
	return validAtom(identity.HostID, 512) && validAtom(identity.BootID, 512) &&
		!identity.BootStartedAt.IsZero() && identity.BootStartedAt.Location() == time.UTC
}

func validControllerBinding(binding selectorinventory.ControllerBinding) bool {
	return validDigest(binding.SnapshotSHA256) && binding.Generation > 0 && validBuild(binding.Build)
}

func validBuild(build string) bool {
	if build == "" || len(build) > 128 {
		return false
	}
	for _, r := range build {
		if (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || strings.ContainsRune("._+-", r) {
			continue
		}
		return false
	}
	return true
}

func validDigest(value string) bool {
	if len(value) != sha256.Size*2 || strings.ToLower(value) != value {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size
}

func validKeyID(value string) bool { return validAtom(value, 200) }

func validAtom(value string, maximum int) bool {
	return value != "" && len(value) <= maximum && utf8.ValidString(value) && strings.TrimSpace(value) == value &&
		!strings.ContainsAny(value, "\x00\r\n")
}

func hostFingerprint(value string) string { return fingerprint("host", value) }
func bootFingerprint(value string) string { return fingerprint("boot", value) }

func fingerprint(kind, value string) string {
	sum := sha256.Sum256([]byte("gascity.selectorwriter." + kind + ".v1\x00" + value))
	return hex.EncodeToString(sum[:])
}

func verifiedSeal(claims HostRecordClaims, ledger []byte) [sha256.Size]byte {
	encoded, _ := json.Marshal(claims)
	hash := sha256.New()
	_, _ = hash.Write([]byte("gascity.verified-selector-writer-record.v1\n"))
	_, _ = hash.Write(encoded)
	_, _ = hash.Write([]byte{0})
	_, _ = hash.Write(ledger)
	var seal [sha256.Size]byte
	copy(seal[:], hash.Sum(nil))
	return seal
}

func equalStringList(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
