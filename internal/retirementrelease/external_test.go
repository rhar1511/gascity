package retirementrelease

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/qualification"
	"github.com/gastownhall/gascity/internal/selectorattestation"
	"github.com/gastownhall/gascity/internal/selectorinventory"
	"github.com/gastownhall/gascity/internal/selectorwriter"
)

func retarget(t *testing.T, f *signedFixture, gate string, raw []byte) {
	t.Helper()
	var m manifest
	if err := json.Unmarshal([]byte(f.request.ManifestJSON), &m); err != nil {
		t.Fatal(err)
	}
	m.Gate = gate
	if f.inputs.HostID != "" {
		m.Context.HostSHA256 = digestBytes([]byte("gascity.selectorwriter.host.v1\x00" + f.inputs.HostID))
		m.Context.BootSHA256 = digestBytes([]byte("gascity.selectorwriter.boot.v1\x00" + f.inputs.BootID))
	}
	f.request.Gate = gate
	f.request.ManifestJSON = canonical(t, m)
	f.request.RequestSHA256 = jsonDigest(t, m)
	f.request.RetainedBase64 = base64.StdEncoding.EncodeToString(raw)
	f.inputs.ExpectedManifestJSON = f.request.ManifestJSON
	f.inputs.RetainedSHA256 = digestBytes(raw)
	f.inputs.PermissionScope.Formula.Name = "retirement-source-read/" + gate
	f.inputs.PermissionScope.Formula.ContentSHA256 = f.request.RequestSHA256
}

func TestSignedExternalWriterCompositionChecksFencesAndHost(t *testing.T) {
	f := newSignedFixture(t)
	sha := strings.Repeat("a", 64)
	f.inputs.HostID = "host-fixture"
	f.inputs.BootID = "boot-fixture"
	window := selectorinventory.CaptureWindow{Start: f.now.Add(-2 * time.Minute), End: f.now.Add(-time.Minute)}
	controller := f.inputs.CurrentController
	registry := selectorinventory.RegistrySnapshotInput{Available: true, ExecutionGeneration: "execution-1", StartFence: 1, EndFence: 1, Identities: []selectorinventory.RegistryDispatchIdentity{}}
	resolutionKey := []byte("fixture-only-resolution-key-32-bytes")
	resolution := selectorinventory.DigestInFlightResolution(registry, "execution-1", resolutionKey)
	if resolution.Status != selectorinventory.StatusAvailable {
		t.Fatal(resolution)
	}
	coverage := []selectorinventory.ExternalSourceCoverage{}
	scopeEvidence := []selectorwriter.ScopeEvidenceDigest{}
	for _, scope := range selectorwriter.MandatoryScopes() {
		coverage = append(coverage, selectorinventory.ExternalSourceCoverage{ScopeID: scope, Status: selectorinventory.StatusAvailable, Complete: true, Capture: window})
		scopeEvidence = append(scopeEvidence, selectorwriter.ScopeEvidenceDigest{ScopeID: scope, EvidenceSHA256: sha})
	}
	ledger := selectorinventory.ExternalLedgerEvidence{SchemaVersion: selectorinventory.ExternalLedgerSchemaVersionV2, ObservationID: f.inputs.Observation.RecordID, ControllerSnapshotSHA256: sha, GraphConfigGeneration: 1, ControllerBuild: controller.Build, ExecutionGeneration: "execution-1", Capture: window, Retention: selectorinventory.CaptureWindow{Start: f.now.Add(-24 * time.Hour), End: window.End}, SequenceStart: 1, SequenceEnd: 1, Sequences: []selectorinventory.LedgerSequence{{Sequence: 1, SourceScope: "orders", EntryKind: selectorinventory.LedgerSequenceKindCoverageCheckpoint, CoverageWindow: &window, EntrySHA256: selectorinventory.LedgerSequenceCoverageCheckpointDigest(1, window)}}, ExternalScope: selectorwriter.MandatoryScopes(), SourceCoverage: coverage, CompleteResultAtoms: []selectorinventory.CompleteResultAtom{{Atom: "external-writer-mandatory-scopes.v1", Complete: true}}, InFlightResolutionSHA256: resolution.SHA256}
	ledger, ledgerBytes, err := selectorinventory.CanonicalizeExternalLedger(ledger)
	if err != nil {
		t.Fatal(err)
	}
	w := f.inputs.Observation
	w.SequenceStart = 1
	w.SequenceEnd = 1
	w.ObservationLedgerSHA256 = ledger.DigestSHA256
	w.ExternalWriterInventorySHA256 = ledger.ExternalWriterInventorySHA256
	w.InFlightResolutionSHA256 = resolution.SHA256
	w.ObservationStartedAt = window.Start
	w.ObservationEndedAt = window.End
	claims := selectorattestation.ObservationClaims{SchemaVersion: selectorattestation.ObservationSchemaVersionV1, Purpose: selectorattestation.ObservationKeyPurpose, RecordID: w.RecordID, KeyID: "observation-key", Issuer: "collector", CollectorIdentity: w.CollectorIdentity, Audience: w.Audience, Workspace: w.Workspace, CandidateManifestSHA256: w.CandidateManifestSHA256, SelectorSnapshotSHA256: w.SelectorSnapshotSHA256, ObservationLedgerSHA256: w.ObservationLedgerSHA256, SequenceStart: w.SequenceStart, SequenceEnd: w.SequenceEnd, SequenceCompletenessResult: w.SequenceCompletenessResult, CoverageResult: w.CoverageResult, RuntimeIdentitySHA256: w.RuntimeIdentitySHA256, BuildIdentitySHA256: w.BuildIdentitySHA256, ConfigIdentitySHA256: w.ConfigIdentitySHA256, OrderInventorySHA256: w.OrderInventorySHA256, ExternalWriterInventorySHA256: w.ExternalWriterInventorySHA256, InFlightResolutionSHA256: w.InFlightResolutionSHA256, ObservationStartedAt: window.Start.Format(time.RFC3339), ObservationEndedAt: window.End.Format(time.RFC3339), IssuedAt: window.End.Format(time.RFC3339), ExpiresAt: f.now.Add(time.Minute).Format(time.RFC3339)}
	ob, err := selectorattestation.ObservationSigningBytes(claims)
	if err != nil {
		t.Fatal(err)
	}
	f.inputs.Observation = w
	f.inputs.ObservationToken = signToken(ob, ed25519.NewKeyFromSeed(bytesSeed(1)))
	key := ed25519.NewKeyFromSeed(bytesSeed(4))
	host := selectorwriter.HostRecordClaims{SchemaVersion: selectorwriter.SchemaVersion, KeyPurpose: selectorwriter.KeyPurpose, KeyID: "external-key", ObservationID: w.RecordID, HostFingerprintSHA256: digestBytes([]byte("gascity.selectorwriter.host.v1\x00" + f.inputs.HostID)), BootFingerprintSHA256: digestBytes([]byte("gascity.selectorwriter.boot.v1\x00" + f.inputs.BootID)), BootStartedAt: f.now.Add(-24 * time.Hour), ControllerSnapshotSHA256: sha, ControllerGeneration: 1, ControllerBuild: controller.Build, ExecutionGeneration: "execution-1", Capture: window, LedgerSHA256: ledger.DigestSHA256, ScopeEvidence: scopeEvidence, IssuedAt: window.End, RetainUntil: window.End.Add(selectorwriter.RetentionPeriod)}
	hostBytes, err := json.Marshal(host)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(selectorwriter.SignedRecord{Claims: host, Ledger: base64.StdEncoding.EncodeToString(ledgerBytes), Signature: base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, append([]byte(selectorwriter.SigningDomain), hostBytes...)))})
	if err != nil {
		t.Fatal(err)
	}
	f.inputs.Writers, err = selectorwriter.NewVerifier(host.KeyID, key.Public().(ed25519.PublicKey))
	if err != nil {
		t.Fatal(err)
	}
	f.inputs.Registry = registry
	f.inputs.External = selectorinventory.ExternalLedgerExpectation{Controller: controller, ObservationID: w.RecordID, Audience: w.Audience, Workspace: w.Workspace, CandidateManifestSHA256: w.CandidateManifestSHA256, ExecutionGeneration: "execution-1", BuildIdentitySHA256: w.BuildIdentitySHA256, ConfigIdentitySHA256: w.ConfigIdentitySHA256, MaxObservationAge: 5 * time.Minute, ExternalScope: selectorwriter.MandatoryScopes(), RequiredResultAtoms: []string{"external-writer-mandatory-scopes.v1"}, ResolutionKey: resolutionKey}
	retarget(t, &f, "external-writers", raw)
	a := NewAdapter(Options{Enabled: true, Now: func() time.Time { return f.now }, Compose: func(context.Context, Binding) (TrustedInputs, error) { return f.inputs, nil }})
	v, err := a.Verify(context.Background(), f.request)
	if err != nil || v.Status != "verified" || v.Assurance != "fixture" {
		t.Fatalf("canonical signed join failed: %+v %v", v, err)
	}
	retarget(t, &f, "in-flight-resolution", raw)
	v, err = a.Verify(context.Background(), f.request)
	if err != nil || v.Status != "verified" || v.Assurance != "fixture" || v.Reason != "signed_fenced_in_flight_resolution_join_verified" {
		t.Fatalf("canonical in-flight resolution failed: %+v %v", v, err)
	}
	f.inputs.Registry.EndFence++
	v, err = a.Verify(context.Background(), f.request)
	if err != nil || v.Status != "unavailable" || !strings.Contains(v.Reason, "external_writer_join_unavailable") {
		t.Fatalf("changed fence accepted: %+v %v", v, err)
	}
}

func TestCompatibilityUsesCanonicalAuthorizeAndRevalidate(t *testing.T) {
	f := newSignedFixture(t)
	f.inputs.CompatibilityScope = f.inputs.PermissionScope
	f.inputs.CompatibilityScope.Formula.Name = "release-formula"
	f.inputs.CompatibilityAuthority = f.authority
	f.inputs.CapabilityProver = testProver{}
	retarget(t, &f, "compatibility", []byte("retained-proof-reference"))
	a := NewAdapter(Options{Enabled: true, Now: func() time.Time { return f.now }, Compose: func(context.Context, Binding) (TrustedInputs, error) { return f.inputs, nil }})
	v, err := a.Verify(context.Background(), f.request)
	if err != nil || v.Status != "verified" || v.Assurance != "fixture" || f.authority.resolves != 3 {
		t.Fatalf("canonical compatibility: %+v %v resolves=%d", v, err, f.authority.resolves)
	}
	f.inputs.CompatibilityScope.EffectiveConfigSHA256 = strings.Repeat("0", 64)
	v, err = a.Verify(context.Background(), f.request)
	if err != nil || v.Status != qualification.StatusUnavailable {
		t.Fatalf("compatibility config mismatch accepted: %+v %v", v, err)
	}
}
