package selectorinventory

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/selectorattestation"
)

type externalLedgerTestAuthority struct {
	bundle selectorattestation.Bundle
}

func (s externalLedgerTestAuthority) Load(context.Context) (selectorattestation.Bundle, error) {
	return s.bundle, nil
}

type externalLedgerTestFixture struct {
	now              time.Time
	ledger           ExternalLedgerEvidence
	raw              []byte
	expected         ExternalLedgerExpectation
	registry         RegistrySnapshotInput
	verifier         *selectorattestation.Verifier
	privateKey       ed25519.PrivateKey
	observation      selectorattestation.ObservationClaims
	observationToken string
}

func newExternalLedgerTestFixture(t *testing.T) *externalLedgerTestFixture {
	t.Helper()
	now := time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)
	observationPublic, observationPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	revocationPublic, revocationPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	audience := "fixture-audience"
	workspace := "fixture-workspace"
	revocations := selectorattestation.Revocations{
		SchemaVersion:    selectorattestation.RevocationsSchemaVersionV1,
		Purpose:          selectorattestation.RevocationKeyPurpose,
		KeyID:            "fixture-revocation-key",
		Issuer:           "fixture-revocation-issuer",
		Audience:         audience,
		Workspace:        workspace,
		IssuedAt:         now.Add(-time.Hour).Format(time.RFC3339Nano),
		ExpiresAt:        now.Add(time.Hour).Format(time.RFC3339Nano),
		RevokedRecordIDs: []string{},
	}
	revocationBytes, err := selectorattestation.RevocationSigningBytes(revocations)
	if err != nil {
		t.Fatal(err)
	}
	revocationPayload := append([]byte(nil), revocationBytes[len(selectorattestation.RevocationSigningDomain):]...)
	revocationSignature := ed25519.Sign(revocationPrivate, revocationBytes)
	signedRevocations := selectorattestation.SignedRevocations{
		Payload:   append(json.RawMessage(nil), revocationPayload...),
		Signature: base64.RawURLEncoding.EncodeToString(revocationSignature),
	}
	revocationDigest := sha256.Sum256(revocationPayload)
	verifier, err := selectorattestation.NewVerifier(externalLedgerTestAuthority{bundle: selectorattestation.Bundle{
		Keys: []selectorattestation.TrustedKey{
			{
				KeyID: "fixture-observation-key", Issuer: "fixture-observation-issuer",
				Subject: "fixture-collector", Purpose: selectorattestation.ObservationKeyPurpose,
				PublicKey: base64.StdEncoding.EncodeToString(observationPublic),
			},
			{
				KeyID: "fixture-revocation-key", Issuer: "fixture-revocation-issuer",
				Purpose:   selectorattestation.RevocationKeyPurpose,
				PublicKey: base64.StdEncoding.EncodeToString(revocationPublic),
			},
		},
		Revocations: signedRevocations,
	}}, selectorattestation.Options{
		Audience: audience, RevocationPayloadSHA256: hex.EncodeToString(revocationDigest[:]),
		MaxRecordAge: 2 * time.Hour, MaxRevocationAge: 3 * time.Hour,
		Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}

	controller := ControllerBinding{
		SnapshotSHA256: strings.Repeat("a", 64), Generation: 19, Build: "build-fixture-1",
	}
	capture := CaptureWindow{Start: now.Add(-2 * time.Hour), End: now.Add(-time.Hour)}
	scope := []string{"audit.primary", "audit.secondary"}
	resolutionKey := []byte("fixture-key-with-at-least-thirty-two-bytes")
	registry := RegistrySnapshotInput{
		Available: true, ExecutionGeneration: "execution-generation-canary", StartFence: 41, EndFence: 41,
		Identities: []RegistryDispatchIdentity{
			{
				ScopedOrderID: "scoped-order-canary", RunID: "run-id-canary", WorkID: "work-id-canary",
				WorkKind: "formula_root", ExecutionGeneration: "execution-generation-canary",
			},
		},
	}
	resolution := DigestInFlightResolution(registry, registry.ExecutionGeneration, resolutionKey)
	if resolution.Status != StatusAvailable {
		t.Fatalf("fixture in-flight digest unavailable: %#v", resolution)
	}
	buildIdentity := externalLedgerTestDigest("build-identity")
	configIdentity := externalLedgerTestDigest("config-identity")
	ledger := ExternalLedgerEvidence{
		SchemaVersion:            ExternalLedgerSchemaVersion,
		ObservationID:            "observation-fixture-1",
		ControllerSnapshotSHA256: controller.SnapshotSHA256,
		GraphConfigGeneration:    controller.Generation,
		ControllerBuild:          controller.Build,
		ExecutionGeneration:      registry.ExecutionGeneration,
		Capture:                  capture,
		Retention:                CaptureWindow{Start: now.Add(-7 * 24 * time.Hour), End: capture.End},
		SequenceStart:            101,
		SequenceEnd:              102,
		Sequences: []LedgerSequence{
			{Sequence: 101, SourceScope: scope[0], EntrySHA256: externalLedgerTestDigest("entry-101")},
			{Sequence: 102, SourceScope: scope[1], EntrySHA256: externalLedgerTestDigest("entry-102")},
		},
		ExternalScope: scope,
		SourceCoverage: []ExternalSourceCoverage{
			{ScopeID: scope[0], Status: StatusAvailable, Complete: true, Capture: capture},
			{ScopeID: scope[1], Status: StatusAvailable, Complete: true, Capture: capture},
		},
		CompleteResultAtoms: []CompleteResultAtom{
			{Atom: "coverage-complete", Complete: true},
			{Atom: "sequence-complete", Complete: true},
		},
		InFlightResolutionSHA256: resolution.SHA256,
	}
	fixture := &externalLedgerTestFixture{
		now: now, ledger: ledger, registry: registry, verifier: verifier,
		privateKey: observationPrivate,
		expected: ExternalLedgerExpectation{
			Controller: controller, ObservationID: ledger.ObservationID,
			Audience: audience, Workspace: workspace,
			CandidateManifestSHA256: externalLedgerTestDigest("candidate"),
			ExecutionGeneration:     registry.ExecutionGeneration,
			BuildIdentitySHA256:     buildIdentity, ConfigIdentitySHA256: configIdentity,
			TrustedNow: now, MaxObservationAge: 3 * time.Hour,
			ExternalScope:       append([]string(nil), scope...),
			RequiredResultAtoms: []string{"coverage-complete", "sequence-complete"},
			ResolutionKey:       resolutionKey,
		},
		observation: selectorattestation.ObservationClaims{
			SchemaVersion:     selectorattestation.ObservationSchemaVersionV1,
			Purpose:           selectorattestation.ObservationKeyPurpose,
			RecordID:          ledger.ObservationID,
			KeyID:             "fixture-observation-key",
			Issuer:            "fixture-observation-issuer",
			CollectorIdentity: "fixture-collector",
			Audience:          audience, Workspace: workspace,
			CandidateManifestSHA256:    externalLedgerTestDigest("candidate"),
			SelectorSnapshotSHA256:     controller.SnapshotSHA256,
			SequenceCompletenessResult: "complete", CoverageResult: "complete",
			RuntimeIdentitySHA256: externalLedgerTestDigest("runtime"),
			BuildIdentitySHA256:   buildIdentity, ConfigIdentitySHA256: configIdentity,
			OrderInventorySHA256: externalLedgerTestDigest("order-inventory"),
		},
	}
	fixture.observation.ObservationStartedAt = capture.Start.Format(time.RFC3339Nano)
	fixture.observation.ObservationEndedAt = capture.End.Format(time.RFC3339Nano)
	fixture.observation.IssuedAt = now.Add(-30 * time.Minute).Format(time.RFC3339Nano)
	fixture.observation.ExpiresAt = now.Add(20 * time.Minute).Format(time.RFC3339Nano)
	fixture.refresh(t)
	return fixture
}

func (f *externalLedgerTestFixture) refresh(t *testing.T) {
	t.Helper()
	var err error
	f.ledger, f.raw, err = CanonicalizeExternalLedger(f.ledger)
	if err != nil {
		t.Fatalf("canonicalize fixture ledger: %v", err)
	}
	f.observation.ObservationLedgerSHA256 = f.ledger.DigestSHA256
	f.observation.SequenceStart = f.ledger.SequenceStart
	f.observation.SequenceEnd = f.ledger.SequenceEnd
	f.observation.ExternalWriterInventorySHA256 = f.ledger.ExternalWriterInventorySHA256
	f.observation.InFlightResolutionSHA256 = f.ledger.InFlightResolutionSHA256
	f.observationToken = externalLedgerTestObservationToken(t, f.observation, f.privateKey)
	want := selectorattestation.ObservationExpectation{
		RecordID: f.observation.RecordID, CollectorIdentity: f.observation.CollectorIdentity,
		Audience: f.observation.Audience, Workspace: f.observation.Workspace,
		CandidateManifestSHA256: f.observation.CandidateManifestSHA256,
		SelectorSnapshotSHA256:  f.observation.SelectorSnapshotSHA256,
		ObservationLedgerSHA256: f.observation.ObservationLedgerSHA256,
		SequenceStart:           f.observation.SequenceStart, SequenceEnd: f.observation.SequenceEnd,
		SequenceCompletenessResult:    f.observation.SequenceCompletenessResult,
		CoverageResult:                f.observation.CoverageResult,
		RuntimeIdentitySHA256:         f.observation.RuntimeIdentitySHA256,
		BuildIdentitySHA256:           f.observation.BuildIdentitySHA256,
		ConfigIdentitySHA256:          f.observation.ConfigIdentitySHA256,
		OrderInventorySHA256:          f.observation.OrderInventorySHA256,
		ExternalWriterInventorySHA256: f.observation.ExternalWriterInventorySHA256,
		InFlightResolutionSHA256:      f.observation.InFlightResolutionSHA256,
		ObservationStartedAt:          f.ledger.Capture.Start,
		ObservationEndedAt:            f.ledger.Capture.End,
	}
	verified, err := f.verifier.VerifyObservation(context.Background(), f.observationToken, want)
	if err != nil {
		t.Fatalf("verify fixture observation: %v", err)
	}
	f.expected.Attestation = verified
}

func TestJoinExternalLedgerAcceptsExactCanonicalEvidence(t *testing.T) {
	fixture := newExternalLedgerTestFixture(t)
	result := JoinExternalLedger(fixture.raw, fixture.expected, fixture.registry)
	if result.Status != StatusAvailable || result.IssueCode != "" || result.Evidence == nil {
		t.Fatalf("join result = %#v, want available evidence", result)
	}
	evidence := *result.Evidence
	if !evidence.Valid() || evidence.ControllerBinding() != fixture.expected.Controller ||
		!evidence.CaptureWindow().Start.Equal(fixture.ledger.Capture.Start) ||
		evidence.LedgerDigestSHA256() != fixture.ledger.DigestSHA256 ||
		evidence.InFlightResolutionDigestSHA256() != fixture.ledger.InFlightResolutionSHA256 {
		t.Fatalf("joined evidence lost validated bindings: %#v", evidence)
	}
	if evidence.ExecutionGenerationDigestSHA256() == fixture.expected.ExecutionGeneration {
		t.Fatal("joined evidence exposed the raw execution generation")
	}
	tampered := evidence
	tampered.LedgerSHA256 = externalLedgerTestDigest("tampered-ledger")
	if tampered.Valid() {
		t.Fatal("mutated joined evidence remained valid")
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"scoped-order-canary", "run-id-canary", "work-id-canary", "execution-generation-canary"} {
		if bytes.Contains(encoded, []byte(secret)) {
			t.Fatalf("joined result exposed %q: %s", secret, encoded)
		}
	}
}

func TestExternalLedgerV1EventOnlyRowsPreserveLegacyBytes(t *testing.T) {
	fixture := newExternalLedgerTestFixture(t)
	if fixture.ledger.SchemaVersion != ExternalLedgerSchemaVersionV1 {
		t.Fatalf("event-only fixture schema = %d, want v1", fixture.ledger.SchemaVersion)
	}
	type legacyLedgerSequence struct {
		Sequence    uint64 `json:"sequence"`
		SourceScope string `json:"source_scope"`
		EntrySHA256 string `json:"entry_sha256"`
	}
	legacy := make([]legacyLedgerSequence, len(fixture.ledger.Sequences))
	for index, sequence := range fixture.ledger.Sequences {
		legacy[index] = legacyLedgerSequence{Sequence: sequence.Sequence, SourceScope: sequence.SourceScope, EntrySHA256: sequence.EntrySHA256}
	}
	legacyBytes, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	currentBytes, err := json.Marshal(fixture.ledger.Sequences)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(currentBytes, legacyBytes) {
		t.Fatalf("v1 event-only sequence bytes changed: got %s, want legacy %s", currentBytes, legacyBytes)
	}
	if bytes.Contains(fixture.raw, []byte(`"entry_kind"`)) || bytes.Contains(fixture.raw, []byte(`"coverage_window"`)) {
		t.Fatalf("v1 event-only ledger serialized v2 fields: %s", fixture.raw)
	}
}

func TestJoinExternalLedgerAcceptsSequencedEmptyWindowCheckpoints(t *testing.T) {
	fixture := newQuietExternalLedgerTestFixture(t)
	result := JoinExternalLedger(fixture.raw, fixture.expected, fixture.registry)
	if result.Status != StatusAvailable || result.IssueCode != "" || result.Evidence == nil || !result.Evidence.Valid() {
		t.Fatalf("quiet-window join = %#v, want available evidence", result)
	}
	if fixture.ledger.SequenceStart != 103 || fixture.ledger.SequenceEnd != 104 || len(fixture.ledger.Sequences) != 2 {
		t.Fatalf("quiet-window global sequence = %d..%d with %d checkpoints, want 103..104", fixture.ledger.SequenceStart, fixture.ledger.SequenceEnd, len(fixture.ledger.Sequences))
	}
}

func TestJoinExternalLedgerRejectsMalformedEmptyWindowCheckpoints(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*externalLedgerTestFixture)
	}{
		{name: "coverage gap", mutate: func(f *externalLedgerTestFixture) {
			coverage := *f.ledger.Sequences[1].CoverageWindow
			coverage.Start = coverage.Start.Add(time.Second)
			f.ledger.Sequences[1].CoverageWindow = &coverage
		}},
		{name: "checkpoint digest does not bind interval", mutate: func(f *externalLedgerTestFixture) {
			first := *f.ledger.Sequences[0].CoverageWindow
			second := *f.ledger.Sequences[1].CoverageWindow
			boundary := first.End.Add(time.Second)
			first.End = boundary
			second.Start = boundary
			f.ledger.Sequences[0].CoverageWindow = &first
			f.ledger.Sequences[1].CoverageWindow = &second
		}},
		{name: "mixed order row", mutate: func(f *externalLedgerTestFixture) {
			f.ledger.Sequences[1].EntryKind = ""
			f.ledger.Sequences[1].CoverageWindow = nil
		}},
		{name: "wrong scope", mutate: func(f *externalLedgerTestFixture) {
			f.ledger.Sequences[0].SourceScope = "audit.secondary"
		}},
		{name: "missing checkpoint window", mutate: func(f *externalLedgerTestFixture) {
			f.ledger.Sequences[0].CoverageWindow = nil
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newQuietExternalLedgerTestFixture(t)
			test.mutate(fixture)
			fixture.refresh(t)
			result := JoinExternalLedger(fixture.raw, fixture.expected, fixture.registry)
			assertExternalJoinUnavailable(t, result, "ledger_sequence_gap_or_ambiguity")
		})
	}
}

func TestJoinExternalLedgerEnforcesCheckpointSchemaVersion(t *testing.T) {
	t.Run("v1 rejects checkpoint fields", func(t *testing.T) {
		fixture := newQuietExternalLedgerTestFixture(t)
		fixture.ledger.SchemaVersion = ExternalLedgerSchemaVersionV1
		fixture.refresh(t)
		result := JoinExternalLedger(fixture.raw, fixture.expected, fixture.registry)
		assertExternalJoinUnavailable(t, result, "ledger_sequence_gap_or_ambiguity")
	})
	t.Run("v2 requires checkpoint rows", func(t *testing.T) {
		fixture := newExternalLedgerTestFixture(t)
		fixture.ledger.SchemaVersion = ExternalLedgerSchemaVersionV2
		fixture.refresh(t)
		result := JoinExternalLedger(fixture.raw, fixture.expected, fixture.registry)
		assertExternalJoinUnavailable(t, result, "ledger_sequence_gap_or_ambiguity")
	})
}

func newQuietExternalLedgerTestFixture(t *testing.T) *externalLedgerTestFixture {
	t.Helper()
	fixture := newExternalLedgerTestFixture(t)
	fixture.ledger.SchemaVersion = ExternalLedgerSchemaVersionV2
	fixture.ledger.ExternalScope = []string{"audit.secondary", "orders"}
	fixture.expected.ExternalScope = append([]string(nil), fixture.ledger.ExternalScope...)
	fixture.ledger.SourceCoverage = []ExternalSourceCoverage{
		{ScopeID: "audit.secondary", Status: StatusAvailable, Complete: true, Capture: fixture.ledger.Capture},
		{ScopeID: "orders", Status: StatusAvailable, Complete: true, Capture: fixture.ledger.Capture},
	}
	capture := fixture.ledger.Capture
	midpoint := capture.Start.Add(capture.End.Sub(capture.Start) / 2)
	firstCoverage := CaptureWindow{Start: capture.Start, End: midpoint}
	secondCoverage := CaptureWindow{Start: midpoint, End: capture.End}
	fixture.ledger.SequenceStart = 103
	fixture.ledger.SequenceEnd = 104
	fixture.ledger.Sequences = []LedgerSequence{
		{Sequence: 103, SourceScope: "orders", EntrySHA256: LedgerSequenceCoverageCheckpointDigest(103, firstCoverage),
			EntryKind: LedgerSequenceKindCoverageCheckpoint, CoverageWindow: &firstCoverage},
		{Sequence: 104, SourceScope: "orders", EntrySHA256: LedgerSequenceCoverageCheckpointDigest(104, secondCoverage),
			EntryKind: LedgerSequenceKindCoverageCheckpoint, CoverageWindow: &secondCoverage},
	}
	fixture.refresh(t)
	return fixture
}

func TestJoinExternalLedgerRejectsSequenceGapsAndDuplicates(t *testing.T) {
	for _, test := range []struct {
		name   string
		second uint64
	}{
		{name: "gap", second: 103},
		{name: "duplicate", second: 101},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newExternalLedgerTestFixture(t)
			fixture.ledger.Sequences[1].Sequence = test.second
			fixture.refresh(t)
			result := JoinExternalLedger(fixture.raw, fixture.expected, fixture.registry)
			assertExternalJoinUnavailable(t, result, "ledger_sequence_gap_or_ambiguity")
		})
	}
}

func TestJoinExternalLedgerRejectsStaleObservationAndReplay(t *testing.T) {
	t.Run("stale observation", func(t *testing.T) {
		fixture := newExternalLedgerTestFixture(t)
		fixture.expected.TrustedNow = fixture.now.Add(5 * time.Hour)
		result := JoinExternalLedger(fixture.raw, fixture.expected, fixture.registry)
		assertExternalJoinUnavailable(t, result, "observation_stale_or_future")
	})
	t.Run("replayed observation identity", func(t *testing.T) {
		fixture := newExternalLedgerTestFixture(t)
		fixture.expected.ObservationID = "new-observation-expectation"
		result := JoinExternalLedger(fixture.raw, fixture.expected, fixture.registry)
		assertExternalJoinUnavailable(t, result, "observation_replay_or_identity_mismatch")
	})
}

func TestJoinExternalLedgerRejectsScopeAndGenerationMismatches(t *testing.T) {
	t.Run("scope", func(t *testing.T) {
		fixture := newExternalLedgerTestFixture(t)
		fixture.expected.ExternalScope = []string{"audit.primary"}
		result := JoinExternalLedger(fixture.raw, fixture.expected, fixture.registry)
		assertExternalJoinUnavailable(t, result, "external_scope_mismatch")
	})
	t.Run("controller snapshot digest", func(t *testing.T) {
		fixture := newExternalLedgerTestFixture(t)
		fixture.expected.Controller.SnapshotSHA256 = strings.Repeat("b", 64)
		result := JoinExternalLedger(fixture.raw, fixture.expected, fixture.registry)
		assertExternalJoinUnavailable(t, result, "controller_snapshot_join_mismatch")
	})
	t.Run("graph config generation", func(t *testing.T) {
		fixture := newExternalLedgerTestFixture(t)
		fixture.expected.Controller.Generation++
		result := JoinExternalLedger(fixture.raw, fixture.expected, fixture.registry)
		assertExternalJoinUnavailable(t, result, "controller_snapshot_join_mismatch")
	})
	t.Run("controller build", func(t *testing.T) {
		fixture := newExternalLedgerTestFixture(t)
		fixture.expected.Controller.Build = "build-fixture-2"
		result := JoinExternalLedger(fixture.raw, fixture.expected, fixture.registry)
		assertExternalJoinUnavailable(t, result, "controller_snapshot_join_mismatch")
	})
	t.Run("execution generation", func(t *testing.T) {
		fixture := newExternalLedgerTestFixture(t)
		fixture.expected.ExecutionGeneration = "another-execution-generation"
		result := JoinExternalLedger(fixture.raw, fixture.expected, fixture.registry)
		assertExternalJoinUnavailable(t, result, "execution_generation_mismatch")
	})
}

func TestJoinExternalLedgerRejectsFenceAndDigestMismatches(t *testing.T) {
	t.Run("registry mutation fence", func(t *testing.T) {
		fixture := newExternalLedgerTestFixture(t)
		fixture.registry.EndFence++
		result := JoinExternalLedger(fixture.raw, fixture.expected, fixture.registry)
		assertExternalJoinUnavailable(t, result, "in_flight_mutation_fence_mismatch")
	})
	t.Run("ledger digest", func(t *testing.T) {
		fixture := newExternalLedgerTestFixture(t)
		fixture.ledger.DigestSHA256 = strings.Repeat("0", 64)
		fixture.raw, _ = json.Marshal(fixture.ledger)
		result := JoinExternalLedger(fixture.raw, fixture.expected, fixture.registry)
		assertExternalJoinUnavailable(t, result, "ledger_digest_mismatch")
	})
	t.Run("collector resolution digest", func(t *testing.T) {
		fixture := newExternalLedgerTestFixture(t)
		fixture.ledger.InFlightResolutionSHA256 = externalLedgerTestDigest("wrong-resolution")
		fixture.refresh(t)
		result := JoinExternalLedger(fixture.raw, fixture.expected, fixture.registry)
		assertExternalJoinUnavailable(t, result, "in_flight_resolution_digest_mismatch")
	})
}

func TestJoinExternalLedgerRejectsIncompleteCoverageResultsAndNoncanonicalBytes(t *testing.T) {
	t.Run("source coverage may bracket capture through trusted present", func(t *testing.T) {
		fixture := newExternalLedgerTestFixture(t)
		fixture.ledger.SourceCoverage[0].Capture.Start = fixture.ledger.Capture.Start.Add(-time.Minute)
		fixture.ledger.SourceCoverage[0].Capture.End = fixture.expected.TrustedNow
		fixture.refresh(t)
		result := JoinExternalLedger(fixture.raw, fixture.expected, fixture.registry)
		if result.Status != StatusAvailable || result.Evidence == nil {
			t.Fatalf("join result = %#v, want available evidence", result)
		}
	})
	t.Run("source coverage cannot end after trusted present", func(t *testing.T) {
		fixture := newExternalLedgerTestFixture(t)
		fixture.ledger.SourceCoverage[1].Capture.End = fixture.expected.TrustedNow.Add(time.Second)
		fixture.refresh(t)
		result := JoinExternalLedger(fixture.raw, fixture.expected, fixture.registry)
		assertExternalJoinUnavailable(t, result, "external_scope_coverage_incomplete")
	})
	t.Run("retention does not cover observation", func(t *testing.T) {
		fixture := newExternalLedgerTestFixture(t)
		fixture.ledger.Retention.Start = fixture.ledger.Capture.Start.Add(time.Second)
		fixture.refresh(t)
		result := JoinExternalLedger(fixture.raw, fixture.expected, fixture.registry)
		assertExternalJoinUnavailable(t, result, "retention_coverage_incomplete")
	})
	t.Run("source coverage is incomplete", func(t *testing.T) {
		fixture := newExternalLedgerTestFixture(t)
		fixture.ledger.SourceCoverage[1].Complete = false
		fixture.refresh(t)
		result := JoinExternalLedger(fixture.raw, fixture.expected, fixture.registry)
		assertExternalJoinUnavailable(t, result, "external_scope_coverage_incomplete")
	})
	t.Run("required result atom", func(t *testing.T) {
		fixture := newExternalLedgerTestFixture(t)
		fixture.ledger.CompleteResultAtoms[0].Complete = false
		fixture.refresh(t)
		result := JoinExternalLedger(fixture.raw, fixture.expected, fixture.registry)
		assertExternalJoinUnavailable(t, result, "required_result_atoms_incomplete")
	})
	t.Run("noncanonical bytes", func(t *testing.T) {
		fixture := newExternalLedgerTestFixture(t)
		fixture.raw = append([]byte(" "), fixture.raw...)
		result := JoinExternalLedger(fixture.raw, fixture.expected, fixture.registry)
		assertExternalJoinUnavailable(t, result, "ledger_not_canonical")
	})
}

func TestInFlightResolutionDigestIsKeyedAndRejectsDuplicateIDs(t *testing.T) {
	fixture := newExternalLedgerTestFixture(t)
	first := DigestInFlightResolution(fixture.registry, fixture.registry.ExecutionGeneration, fixture.expected.ResolutionKey)
	otherKey := DigestInFlightResolution(fixture.registry, fixture.registry.ExecutionGeneration, bytes.Repeat([]byte{'x'}, 32))
	if first.Status != StatusAvailable || otherKey.Status != StatusAvailable || first.SHA256 == otherKey.SHA256 {
		t.Fatal("in-flight resolution digest was not keyed")
	}
	duplicate := fixture.registry
	duplicate.Identities = append(append([]RegistryDispatchIdentity(nil), fixture.registry.Identities...), fixture.registry.Identities[0])
	result := DigestInFlightResolution(duplicate, duplicate.ExecutionGeneration, fixture.expected.ResolutionKey)
	if result.Status != StatusUnavailable || result.IssueCode != "in_flight_identity_ambiguous" || result.SHA256 != "" {
		t.Fatalf("duplicate identity digest = %#v, want unavailable ambiguity", result)
	}
	conflicting := fixture.registry
	conflicting.Identities = append(append([]RegistryDispatchIdentity(nil), fixture.registry.Identities...), fixture.registry.Identities[0])
	conflicting.Identities[1].WorkID = "another-work-id"
	result = DigestInFlightResolution(conflicting, conflicting.ExecutionGeneration, fixture.expected.ResolutionKey)
	if result.Status != StatusUnavailable || result.IssueCode != "in_flight_identity_ambiguous" || result.SHA256 != "" {
		t.Fatalf("conflicting run identity digest = %#v, want unavailable ambiguity", result)
	}
}

func TestInFlightResolutionDigestAcceptsExecWithoutFormulaWorkID(t *testing.T) {
	snapshot := RegistrySnapshotInput{
		Available: true, ExecutionGeneration: "execution-generation-exec", StartFence: 9, EndFence: 9,
		Identities: []RegistryDispatchIdentity{{
			ScopedOrderID: "scoped-order-exec", RunID: "run-exec", WorkKind: "exec",
			ExecutionGeneration: "execution-generation-exec",
		}},
	}
	key := bytes.Repeat([]byte{'e'}, 32)
	got := DigestInFlightResolution(snapshot, snapshot.ExecutionGeneration, key)
	if got.Status != StatusAvailable || len(got.SHA256) != 64 || got.IssueCode != "" {
		t.Fatalf("exec resolution digest = %#v, want available exact scoped-order/run/generation identity", got)
	}

	for name, mutate := range map[string]func(*RegistryDispatchIdentity){
		"invented formula work id": func(identity *RegistryDispatchIdentity) { identity.WorkID = "formula-root" },
		"missing kind":             func(identity *RegistryDispatchIdentity) { identity.WorkKind = "" },
		"wrong kind":               func(identity *RegistryDispatchIdentity) { identity.WorkKind = "formula_root" },
		"stale generation":         func(identity *RegistryDispatchIdentity) { identity.ExecutionGeneration = "execution-generation-old" },
	} {
		t.Run(name, func(t *testing.T) {
			malformed := snapshot
			malformed.Identities = append([]RegistryDispatchIdentity(nil), snapshot.Identities...)
			mutate(&malformed.Identities[0])
			result := DigestInFlightResolution(malformed, snapshot.ExecutionGeneration, key)
			if result.Status != StatusUnavailable || result.SHA256 != "" {
				t.Fatalf("malformed exec identity digest = %#v, want unavailable", result)
			}
		})
	}
}

func externalLedgerTestObservationToken(t *testing.T, claims selectorattestation.ObservationClaims, privateKey ed25519.PrivateKey) string {
	t.Helper()
	signingBytes, err := selectorattestation.ObservationSigningBytes(claims)
	if err != nil {
		t.Fatal(err)
	}
	payload := signingBytes[len(selectorattestation.ObservationSigningDomain):]
	signature := ed25519.Sign(privateKey, signingBytes)
	return base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(signature)
}

func externalLedgerTestDigest(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

func assertExternalJoinUnavailable(t *testing.T, result ExternalLedgerJoinResult, issue string) {
	t.Helper()
	if result.Status != StatusUnavailable || result.IssueCode != issue || result.Evidence != nil {
		t.Fatalf("join result = %#v, want unavailable %q with no evidence", result, issue)
	}
}
