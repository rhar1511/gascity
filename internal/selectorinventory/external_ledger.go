package selectorinventory

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/gastownhall/gascity/internal/selectorattestation"
)

const (
	// ExternalLedgerSchemaVersion identifies canonical external ledger records.
	ExternalLedgerSchemaVersion = 1

	maxExternalLedgerBytes   = 4 << 20
	maxExternalLedgerEntries = 100000
	maxExternalLedgerScopes  = 1024
	maxExternalResultAtoms   = 1024
	maxExternalIDBytes       = 1024
	maxInFlightIdentities    = 8192

	externalLedgerDigestDomain = "gascity.selectorinventory.external-ledger.v1\n"
	externalInventoryDomain    = "gascity.selectorinventory.external-writers.v1\n"
	inflightResolutionDomain   = "gascity.selectorinventory.inflight-resolution.v1\n"
	joinedEvidenceDomain       = "gascity.selectorinventory.joined-evidence.v1\n"
)

// LedgerSequence is one retained sequence position. EntrySHA256 identifies
// the retained canonical entry without copying its payload into this package.
type LedgerSequence struct {
	Sequence    uint64 `json:"sequence"`
	SourceScope string `json:"source_scope"`
	EntrySHA256 string `json:"entry_sha256"`
}

// ExternalSourceCoverage records complete coverage for one configured source
// scope during an observation window.
type ExternalSourceCoverage struct {
	ScopeID  string        `json:"scope_id"`
	Status   Status        `json:"status"`
	Complete bool          `json:"complete"`
	Capture  CaptureWindow `json:"capture"`
}

// CompleteResultAtom is one bounded result claim from the external collector.
// The caller supplies the required atom names as trusted policy.
type CompleteResultAtom struct {
	Atom     string `json:"atom"`
	Complete bool   `json:"complete"`
}

// ExternalLedgerEvidence is a privacy-limited canonical view of an externally
// retained writer ledger. It carries no order, run, or work identity.
type ExternalLedgerEvidence struct {
	SchemaVersion                 int                      `json:"schema_version"`
	ObservationID                 string                   `json:"observation_id"`
	ControllerSnapshotSHA256      string                   `json:"controller_snapshot_sha256"`
	GraphConfigGeneration         uint64                   `json:"graph_config_generation"`
	ControllerBuild               string                   `json:"controller_build"`
	ExecutionGeneration           string                   `json:"execution_generation"`
	Capture                       CaptureWindow            `json:"capture"`
	Retention                     CaptureWindow            `json:"retention"`
	SequenceStart                 uint64                   `json:"sequence_start"`
	SequenceEnd                   uint64                   `json:"sequence_end"`
	Sequences                     []LedgerSequence         `json:"sequences"`
	ExternalScope                 []string                 `json:"external_scope"`
	SourceCoverage                []ExternalSourceCoverage `json:"source_coverage"`
	CompleteResultAtoms           []CompleteResultAtom     `json:"complete_result_atoms"`
	ExternalWriterInventorySHA256 string                   `json:"external_writer_inventory_sha256"`
	InFlightResolutionSHA256      string                   `json:"in_flight_resolution_sha256"`
	DigestSHA256                  string                   `json:"digest_sha256"`
}

// RegistryDispatchIdentity contains transient identifiers from one exact
// controller execution. The identifiers are used only as keyed digest input.
type RegistryDispatchIdentity struct {
	ScopedOrderID       string `json:"-"`
	RunID               string `json:"-"`
	WorkID              string `json:"-"`
	ExecutionGeneration string `json:"-"`
}

// RegistrySnapshotInput is the fixture-facing shape of a fenced registry
// snapshot. It is not a persisted or serialized evidence record.
type RegistrySnapshotInput struct {
	Available           bool                       `json:"-"`
	ExecutionGeneration string                     `json:"-"`
	StartFence          uint64                     `json:"-"`
	EndFence            uint64                     `json:"-"`
	Identities          []RegistryDispatchIdentity `json:"-"`
}

// ResolutionDigestResult exposes only the status, fixed issue code, and keyed
// digest. Raw registry identities and the execution generation stay private.
type ResolutionDigestResult struct {
	Status    Status `json:"status"`
	IssueCode string `json:"issue_code,omitempty"`
	SHA256    string `json:"sha256,omitempty"`
}

// DigestInFlightResolution validates exact registry-generation and mutation
// fences, then returns a keyed digest over sorted scoped identities.
func DigestInFlightResolution(snapshot RegistrySnapshotInput, expectedExecutionGeneration string, key []byte) ResolutionDigestResult {
	digest, issue := digestInFlightResolution(snapshot, expectedExecutionGeneration, key)
	if issue != "" {
		return ResolutionDigestResult{Status: StatusUnavailable, IssueCode: issue}
	}
	return ResolutionDigestResult{Status: StatusAvailable, SHA256: digest}
}

// CanonicalizeExternalLedger fills the canonical external-inventory and ledger
// digests, then returns the exact JSON bytes accepted by JoinExternalLedger.
// It does not authenticate or validate the claims in the supplied evidence.
func CanonicalizeExternalLedger(evidence ExternalLedgerEvidence) (ExternalLedgerEvidence, []byte, error) {
	evidence.ExternalWriterInventorySHA256 = externalWriterInventoryDigest(evidence)
	evidence.DigestSHA256 = externalLedgerDigest(evidence)
	encoded, err := json.Marshal(evidence)
	if err != nil {
		return ExternalLedgerEvidence{}, nil, err
	}
	if len(encoded) > maxExternalLedgerBytes {
		return ExternalLedgerEvidence{}, nil, errors.New("external ledger exceeds the size limit")
	}
	return evidence, encoded, nil
}

// ExternalLedgerExpectation contains trusted controller facts, required
// external source/result coverage, a trusted time sample, and a previously
// verified collector attestation. It must not be populated from ledger data.
type ExternalLedgerExpectation struct {
	Controller              ControllerBinding                       `json:"-"`
	ObservationID           string                                  `json:"-"`
	Audience                string                                  `json:"-"`
	Workspace               string                                  `json:"-"`
	CandidateManifestSHA256 string                                  `json:"-"`
	ExecutionGeneration     string                                  `json:"-"`
	BuildIdentitySHA256     string                                  `json:"-"`
	ConfigIdentitySHA256    string                                  `json:"-"`
	TrustedNow              time.Time                               `json:"-"`
	MaxObservationAge       time.Duration                           `json:"-"`
	ExternalScope           []string                                `json:"-"`
	RequiredResultAtoms     []string                                `json:"-"`
	ResolutionKey           []byte                                  `json:"-"`
	Attestation             selectorattestation.VerifiedObservation `json:"-"`
}

// JoinedExternalEvidence is an immutable usable join result. Its exported
// methods expose only digests and controller metadata, never scoped identities.
type JoinedExternalEvidence struct {
	Controller                    ControllerBinding `json:"controller"`
	Capture                       CaptureWindow     `json:"capture"`
	LedgerSHA256                  string            `json:"ledger_sha256"`
	ExternalWriterInventorySHA256 string            `json:"external_writer_inventory_sha256"`
	InFlightResolutionSHA256      string            `json:"in_flight_resolution_sha256"`
	ExecutionGenerationSHA256     string            `json:"execution_generation_sha256"`
	seal                          [sha256.Size]byte `json:"-"`
}

// Valid checks that the joined value still matches the validator's sealed
// output.
func (e JoinedExternalEvidence) Valid() bool {
	if e.seal == ([sha256.Size]byte{}) {
		return false
	}
	expected := sealJoinedExternalEvidence(e)
	return hmac.Equal(e.seal[:], expected[:])
}

// ControllerBinding returns the exact graph/config snapshot binding.
func (e JoinedExternalEvidence) ControllerBinding() ControllerBinding { return e.Controller }

// CaptureWindow returns the signed observation window.
func (e JoinedExternalEvidence) CaptureWindow() CaptureWindow { return e.Capture }

// LedgerDigestSHA256 returns the canonical ledger digest.
func (e JoinedExternalEvidence) LedgerDigestSHA256() string { return e.LedgerSHA256 }

// ExternalWriterInventoryDigestSHA256 returns the digest bound by the
// collector attestation.
func (e JoinedExternalEvidence) ExternalWriterInventoryDigestSHA256() string {
	return e.ExternalWriterInventorySHA256
}

// InFlightResolutionDigestSHA256 returns the keyed registry-resolution
// digest. It cannot be used to recover scoped order, run, or work IDs.
func (e JoinedExternalEvidence) InFlightResolutionDigestSHA256() string {
	return e.InFlightResolutionSHA256
}

// ExecutionGenerationDigestSHA256 returns the keyed controller-execution
// generation binding, kept distinct from the graph/config generation.
func (e JoinedExternalEvidence) ExecutionGenerationDigestSHA256() string {
	return e.ExecutionGenerationSHA256
}

// ExternalLedgerJoinResult reports validation status. Evidence is non-nil only
// after every canonical, coverage, freshness, fence, digest, and attestation
// check succeeds.
type ExternalLedgerJoinResult struct {
	Status    Status                  `json:"status"`
	IssueCode string                  `json:"issue_code,omitempty"`
	Evidence  *JoinedExternalEvidence `json:"evidence,omitempty"`
}

// JoinExternalLedger validates canonical retained ledger evidence against the
// exact controller snapshot, trusted policy, signed observation, and fenced
// in-flight registry snapshot. Failure always returns unavailable with no
// usable evidence.
func JoinExternalLedger(raw []byte, expected ExternalLedgerExpectation, registry RegistrySnapshotInput) ExternalLedgerJoinResult {
	if issue := validateExternalLedgerExpectation(expected); issue != "" {
		return unavailableExternalJoin(issue)
	}
	evidence, issue := decodeExternalLedger(raw)
	if issue != "" {
		return unavailableExternalJoin(issue)
	}
	if issue = validateExternalLedgerShape(evidence); issue != "" {
		return unavailableExternalJoin(issue)
	}
	if issue = validateExternalLedgerDigest(evidence); issue != "" {
		return unavailableExternalJoin(issue)
	}
	if issue = validateExternalControllerBinding(evidence, expected); issue != "" {
		return unavailableExternalJoin(issue)
	}
	if issue = validateExternalFreshness(evidence, expected); issue != "" {
		return unavailableExternalJoin(issue)
	}
	if issue = validateExternalScopeAndCoverage(evidence, expected); issue != "" {
		return unavailableExternalJoin(issue)
	}
	if issue = validateExternalSequence(evidence); issue != "" {
		return unavailableExternalJoin(issue)
	}
	if issue = validateCompleteResultAtoms(evidence, expected); issue != "" {
		return unavailableExternalJoin(issue)
	}
	if digest := externalWriterInventoryDigest(evidence); digest == "" || digest != evidence.ExternalWriterInventorySHA256 {
		return unavailableExternalJoin("external_inventory_digest_mismatch")
	}
	resolutionDigest, issue := digestInFlightResolution(registry, expected.ExecutionGeneration, expected.ResolutionKey)
	if issue != "" {
		return unavailableExternalJoin(issue)
	}
	if resolutionDigest != evidence.InFlightResolutionSHA256 {
		return unavailableExternalJoin("in_flight_resolution_digest_mismatch")
	}
	if issue = validateExternalAttestation(evidence, expected, resolutionDigest); issue != "" {
		return unavailableExternalJoin(issue)
	}

	joined := JoinedExternalEvidence{
		Controller:                    expected.Controller,
		Capture:                       evidence.Capture,
		LedgerSHA256:                  evidence.DigestSHA256,
		ExternalWriterInventorySHA256: evidence.ExternalWriterInventorySHA256,
		InFlightResolutionSHA256:      resolutionDigest,
		ExecutionGenerationSHA256:     keyedID(expected.ResolutionKey, "execution-generation", expected.ExecutionGeneration),
	}
	joined.seal = sealJoinedExternalEvidence(joined)
	return ExternalLedgerJoinResult{Status: StatusAvailable, Evidence: &joined}
}

func decodeExternalLedger(raw []byte) (ExternalLedgerEvidence, string) {
	if len(raw) == 0 || len(raw) > maxExternalLedgerBytes {
		return ExternalLedgerEvidence{}, "ledger_size_invalid"
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var evidence ExternalLedgerEvidence
	if err := decoder.Decode(&evidence); err != nil {
		return ExternalLedgerEvidence{}, "ledger_json_invalid"
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return ExternalLedgerEvidence{}, "ledger_json_invalid"
	}
	canonical, err := json.Marshal(evidence)
	if err != nil || !bytes.Equal(canonical, raw) {
		return ExternalLedgerEvidence{}, "ledger_not_canonical"
	}
	return evidence, ""
}

func validateExternalLedgerExpectation(expected ExternalLedgerExpectation) string {
	if !validBinding(expected.Controller) || !validOpaqueAtom(expected.ObservationID) ||
		!validOpaqueAtom(expected.Audience) || !validOpaqueAtom(expected.Workspace) ||
		!validSHA256Digest(expected.CandidateManifestSHA256) ||
		!validOpaqueAtom(expected.ExecutionGeneration) || !validSHA256Digest(expected.BuildIdentitySHA256) ||
		!validSHA256Digest(expected.ConfigIdentitySHA256) || expected.TrustedNow.IsZero() ||
		expected.MaxObservationAge <= 0 || len(expected.ResolutionKey) < 32 || len(expected.ResolutionKey) > maxHMACKeyBytes ||
		!expected.Attestation.Valid() {
		return "validation_policy_unavailable"
	}
	if _, ok := sortedUniqueAtoms(expected.ExternalScope, maxExternalLedgerScopes); !ok || len(expected.ExternalScope) == 0 {
		return "validation_scope_unavailable"
	}
	if _, ok := sortedUniqueAtoms(expected.RequiredResultAtoms, maxExternalResultAtoms); !ok || len(expected.RequiredResultAtoms) == 0 {
		return "validation_result_policy_unavailable"
	}
	return ""
}

func validateExternalLedgerShape(evidence ExternalLedgerEvidence) string {
	if evidence.SchemaVersion != ExternalLedgerSchemaVersion || evidence.ObservationID == "" ||
		evidence.ControllerSnapshotSHA256 == "" || evidence.ControllerBuild == "" || evidence.ExecutionGeneration == "" ||
		evidence.GraphConfigGeneration == 0 || evidence.SequenceStart == 0 || evidence.SequenceEnd < evidence.SequenceStart ||
		len(evidence.Sequences) == 0 || len(evidence.Sequences) > maxExternalLedgerEntries ||
		len(evidence.ExternalScope) == 0 || len(evidence.ExternalScope) > maxExternalLedgerScopes ||
		len(evidence.SourceCoverage) == 0 || len(evidence.SourceCoverage) > maxExternalLedgerScopes ||
		len(evidence.CompleteResultAtoms) > maxExternalResultAtoms || !validSHA256Digest(evidence.ControllerSnapshotSHA256) ||
		!validBuildID(evidence.ControllerBuild) || !validOpaqueAtom(evidence.ObservationID) ||
		!validOpaqueAtom(evidence.ExecutionGeneration) || !validSHA256Digest(evidence.ExternalWriterInventorySHA256) ||
		!validSHA256Digest(evidence.InFlightResolutionSHA256) || !validSHA256Digest(evidence.DigestSHA256) {
		return "ledger_shape_invalid"
	}
	if !validCaptureWindow(evidence.Capture) || !validCaptureWindow(evidence.Retention) {
		return "capture_window_invalid"
	}
	if evidence.Sequences == nil || evidence.ExternalScope == nil || evidence.SourceCoverage == nil || evidence.CompleteResultAtoms == nil {
		return "ledger_shape_invalid"
	}
	return ""
}

func validateExternalLedgerDigest(evidence ExternalLedgerEvidence) string {
	want := externalLedgerDigest(evidence)
	if want == "" || want != evidence.DigestSHA256 {
		return "ledger_digest_mismatch"
	}
	return ""
}

func validateExternalControllerBinding(evidence ExternalLedgerEvidence, expected ExternalLedgerExpectation) string {
	if evidence.ControllerSnapshotSHA256 != expected.Controller.SnapshotSHA256 ||
		evidence.GraphConfigGeneration != expected.Controller.Generation || evidence.ControllerBuild != expected.Controller.Build {
		return "controller_snapshot_join_mismatch"
	}
	if evidence.ExecutionGeneration != expected.ExecutionGeneration {
		return "execution_generation_mismatch"
	}
	if evidence.ObservationID != expected.ObservationID {
		return "observation_replay_or_identity_mismatch"
	}
	return ""
}

func validateExternalFreshness(evidence ExternalLedgerEvidence, expected ExternalLedgerExpectation) string {
	now := expected.TrustedNow.UTC()
	if !canonicalUTC(evidence.Capture.Start) || !canonicalUTC(evidence.Capture.End) ||
		!canonicalUTC(evidence.Retention.Start) || !canonicalUTC(evidence.Retention.End) {
		return "capture_time_not_canonical"
	}
	if evidence.Retention.Start.After(evidence.Capture.Start) || evidence.Retention.End.Before(evidence.Capture.End) ||
		evidence.Retention.End.After(now) {
		return "retention_coverage_incomplete"
	}
	if evidence.Capture.End.After(now) || now.Sub(evidence.Capture.End) > expected.MaxObservationAge {
		return "observation_stale_or_future"
	}
	return ""
}

func validateExternalScopeAndCoverage(evidence ExternalLedgerEvidence, expected ExternalLedgerExpectation) string {
	wantScope, ok := sortedUniqueAtoms(expected.ExternalScope, maxExternalLedgerScopes)
	if !ok || !sameStringSlice(evidence.ExternalScope, wantScope) ||
		!isSortedUniqueAtoms(evidence.ExternalScope, maxExternalLedgerScopes) {
		return "external_scope_mismatch"
	}
	if len(evidence.SourceCoverage) != len(evidence.ExternalScope) {
		return "external_scope_coverage_incomplete"
	}
	for index, coverage := range evidence.SourceCoverage {
		if coverage.ScopeID != evidence.ExternalScope[index] || coverage.Status != StatusAvailable || !coverage.Complete ||
			!validCaptureWindow(coverage.Capture) || !canonicalUTC(coverage.Capture.Start) || !canonicalUTC(coverage.Capture.End) ||
			coverage.Capture.Start.After(evidence.Capture.Start) ||
			coverage.Capture.End.Before(evidence.Capture.End) ||
			coverage.Capture.Start.After(expected.TrustedNow.UTC()) || coverage.Capture.End.After(expected.TrustedNow.UTC()) {
			return "external_scope_coverage_incomplete"
		}
	}
	return ""
}

func validateExternalSequence(evidence ExternalLedgerEvidence) string {
	if evidence.SequenceEnd-evidence.SequenceStart >= maxExternalLedgerEntries ||
		uint64(len(evidence.Sequences)) != evidence.SequenceEnd-evidence.SequenceStart+1 {
		return "ledger_sequence_gap_or_ambiguity"
	}
	allowed := make(map[string]struct{}, len(evidence.ExternalScope))
	for _, scope := range evidence.ExternalScope {
		allowed[scope] = struct{}{}
	}
	for index, entry := range evidence.Sequences {
		if entry.Sequence != evidence.SequenceStart+uint64(index) || !validSHA256Digest(entry.EntrySHA256) {
			return "ledger_sequence_gap_or_ambiguity"
		}
		if _, exists := allowed[entry.SourceScope]; !exists {
			return "external_scope_mismatch"
		}
	}
	return ""
}

func validateCompleteResultAtoms(evidence ExternalLedgerEvidence, expected ExternalLedgerExpectation) string {
	if !isSortedUniqueResultAtoms(evidence.CompleteResultAtoms) {
		return "required_result_atoms_incomplete"
	}
	complete := make(map[string]bool, len(evidence.CompleteResultAtoms))
	for _, result := range evidence.CompleteResultAtoms {
		if !validOpaqueAtom(result.Atom) {
			return "required_result_atoms_incomplete"
		}
		complete[result.Atom] = result.Complete
	}
	for _, atom := range expected.RequiredResultAtoms {
		if !complete[atom] {
			return "required_result_atoms_incomplete"
		}
	}
	return ""
}

func validateExternalAttestation(evidence ExternalLedgerEvidence, expected ExternalLedgerExpectation, resolutionDigest string) string {
	attestation := expected.Attestation
	if !attestation.Valid() || attestation.RecordID != expected.ObservationID ||
		attestation.Audience != expected.Audience || attestation.Workspace != expected.Workspace ||
		attestation.CandidateManifestSHA256 != expected.CandidateManifestSHA256 ||
		attestation.SelectorSnapshotSHA256 != expected.Controller.SnapshotSHA256 ||
		attestation.ObservationLedgerSHA256 != evidence.DigestSHA256 ||
		attestation.SequenceStart != evidence.SequenceStart || attestation.SequenceEnd != evidence.SequenceEnd ||
		attestation.SequenceCompletenessResult != "complete" || attestation.CoverageResult != "complete" ||
		attestation.ExternalWriterInventorySHA256 != evidence.ExternalWriterInventorySHA256 ||
		attestation.InFlightResolutionSHA256 != resolutionDigest ||
		attestation.BuildIdentitySHA256 != expected.BuildIdentitySHA256 ||
		attestation.ConfigIdentitySHA256 != expected.ConfigIdentitySHA256 ||
		!attestation.ObservationStartedAt.Equal(evidence.Capture.Start) ||
		!attestation.ObservationEndedAt.Equal(evidence.Capture.End) {
		return "collector_attestation_binding_mismatch"
	}
	now := expected.TrustedNow.UTC()
	if attestation.IssuedAt.After(now) || !attestation.ExpiresAt.After(now) ||
		now.Sub(attestation.IssuedAt) > expected.MaxObservationAge {
		return "collector_attestation_stale_or_future"
	}
	return ""
}

func digestInFlightResolution(snapshot RegistrySnapshotInput, expectedGeneration string, key []byte) (string, string) {
	if !snapshot.Available {
		return "", "in_flight_registry_unavailable"
	}
	if !validOpaqueAtom(expectedGeneration) || snapshot.ExecutionGeneration != expectedGeneration {
		return "", "execution_generation_mismatch"
	}
	if snapshot.StartFence != snapshot.EndFence {
		return "", "in_flight_mutation_fence_mismatch"
	}
	if len(key) < 32 || len(key) > maxHMACKeyBytes || len(snapshot.Identities) > maxInFlightIdentities {
		return "", "in_flight_resolution_unavailable"
	}
	identities := append([]RegistryDispatchIdentity(nil), snapshot.Identities...)
	for _, identity := range identities {
		if !validOpaqueAtom(identity.ScopedOrderID) || !validOpaqueAtom(identity.RunID) ||
			!validOpaqueAtom(identity.WorkID) || identity.ExecutionGeneration != expectedGeneration {
			return "", "in_flight_identity_unavailable"
		}
	}
	sort.Slice(identities, func(i, j int) bool {
		left, right := identities[i], identities[j]
		if left.ScopedOrderID != right.ScopedOrderID {
			return left.ScopedOrderID < right.ScopedOrderID
		}
		if left.RunID != right.RunID {
			return left.RunID < right.RunID
		}
		return left.WorkID < right.WorkID
	})
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(inflightResolutionDomain))
	writeDigestField(mac, expectedGeneration)
	writeDigestUint64(mac, snapshot.StartFence)
	writeDigestUint64(mac, snapshot.EndFence)
	writeDigestUint64(mac, uint64(len(identities)))
	for index, identity := range identities {
		if index > 0 && sameRegistryIdentity(identities[index-1], identity) {
			return "", "in_flight_identity_ambiguous"
		}
		writeDigestField(mac, identity.ScopedOrderID)
		writeDigestField(mac, identity.RunID)
		writeDigestField(mac, identity.WorkID)
		writeDigestField(mac, identity.ExecutionGeneration)
	}
	return hex.EncodeToString(mac.Sum(nil)), ""
}

func externalLedgerDigest(evidence ExternalLedgerEvidence) string {
	evidence.DigestSHA256 = ""
	encoded, err := json.Marshal(evidence)
	if err != nil {
		return ""
	}
	hash := sha256.New()
	_, _ = hash.Write([]byte(externalLedgerDigestDomain))
	_, _ = hash.Write(encoded)
	return hex.EncodeToString(hash.Sum(nil))
}

func externalWriterInventoryDigest(evidence ExternalLedgerEvidence) string {
	payload := struct {
		Capture             CaptureWindow            `json:"capture"`
		SequenceStart       uint64                   `json:"sequence_start"`
		SequenceEnd         uint64                   `json:"sequence_end"`
		ExternalScope       []string                 `json:"external_scope"`
		SourceCoverage      []ExternalSourceCoverage `json:"source_coverage"`
		CompleteResultAtoms []CompleteResultAtom     `json:"complete_result_atoms"`
	}{
		Capture: evidence.Capture, SequenceStart: evidence.SequenceStart, SequenceEnd: evidence.SequenceEnd,
		ExternalScope: evidence.ExternalScope, SourceCoverage: evidence.SourceCoverage,
		CompleteResultAtoms: evidence.CompleteResultAtoms,
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return ""
	}
	hash := sha256.New()
	_, _ = hash.Write([]byte(externalInventoryDomain))
	_, _ = hash.Write(encoded)
	return hex.EncodeToString(hash.Sum(nil))
}

func sealJoinedExternalEvidence(evidence JoinedExternalEvidence) [sha256.Size]byte {
	payload := struct {
		Controller                    ControllerBinding `json:"controller"`
		Capture                       CaptureWindow     `json:"capture"`
		LedgerSHA256                  string            `json:"ledger_sha256"`
		ExternalWriterInventorySHA256 string            `json:"external_writer_inventory_sha256"`
		InFlightResolutionSHA256      string            `json:"in_flight_resolution_sha256"`
		ExecutionGenerationSHA256     string            `json:"execution_generation_sha256"`
	}{
		Controller: evidence.Controller, Capture: evidence.Capture,
		LedgerSHA256: evidence.LedgerSHA256, ExternalWriterInventorySHA256: evidence.ExternalWriterInventorySHA256,
		InFlightResolutionSHA256:  evidence.InFlightResolutionSHA256,
		ExecutionGenerationSHA256: evidence.ExecutionGenerationSHA256,
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return [sha256.Size]byte{}
	}
	hash := sha256.New()
	_, _ = hash.Write([]byte(joinedEvidenceDomain))
	_, _ = hash.Write(encoded)
	var seal [sha256.Size]byte
	copy(seal[:], hash.Sum(nil))
	return seal
}

func unavailableExternalJoin(issue string) ExternalLedgerJoinResult {
	return ExternalLedgerJoinResult{Status: StatusUnavailable, IssueCode: issue}
}

func validCaptureWindow(window CaptureWindow) bool {
	return !window.Start.IsZero() && window.End.After(window.Start)
}

func canonicalUTC(value time.Time) bool {
	return !value.IsZero() && value.Location() == time.UTC
}

func validBuildID(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, char := range value {
		if (char >= 'A' && char <= 'Z') || (char >= 'a' && char <= 'z') ||
			(char >= '0' && char <= '9') || strings.ContainsRune("._+-", char) {
			continue
		}
		return false
	}
	return true
}

func validSHA256Digest(value string) bool {
	if len(value) != sha256.Size*2 || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func validOpaqueAtom(value string) bool {
	if value == "" || len(value) > maxExternalIDBytes || strings.TrimSpace(value) != value || !utf8.ValidString(value) {
		return false
	}
	for _, char := range value {
		if unicode.IsControl(char) {
			return false
		}
	}
	return true
}

func sortedUniqueAtoms(values []string, maximum int) ([]string, bool) {
	if values == nil || len(values) > maximum {
		return nil, false
	}
	result := append([]string(nil), values...)
	for _, value := range result {
		if !validOpaqueAtom(value) {
			return nil, false
		}
	}
	sort.Strings(result)
	for index := 1; index < len(result); index++ {
		if result[index] == result[index-1] {
			return nil, false
		}
	}
	return result, true
}

func isSortedUniqueAtoms(values []string, maximum int) bool {
	if _, ok := sortedUniqueAtoms(values, maximum); !ok {
		return false
	}
	return sort.StringsAreSorted(values) && noAdjacentDuplicates(values)
}

func isSortedUniqueResultAtoms(values []CompleteResultAtom) bool {
	if values == nil || len(values) > maxExternalResultAtoms {
		return false
	}
	for index, value := range values {
		if !validOpaqueAtom(value.Atom) || (index > 0 && values[index-1].Atom >= value.Atom) {
			return false
		}
	}
	return true
}

func noAdjacentDuplicates(values []string) bool {
	for index := 1; index < len(values); index++ {
		if values[index] == values[index-1] {
			return false
		}
	}
	return true
}

func sameStringSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for index := range a {
		if a[index] != b[index] {
			return false
		}
	}
	return true
}

func sameRegistryIdentity(a, b RegistryDispatchIdentity) bool {
	return a.ScopedOrderID == b.ScopedOrderID && a.RunID == b.RunID && a.WorkID == b.WorkID
}

type digestWriter interface {
	Write([]byte) (int, error)
}

func writeDigestField(writer digestWriter, value string) {
	writeDigestUint64(writer, uint64(len(value)))
	_, _ = writer.Write([]byte(value))
}

func writeDigestUint64(writer digestWriter, value uint64) {
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], value)
	_, _ = writer.Write(encoded[:])
}
