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
				ExecutionGeneration: "execution-generation-canary",
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
