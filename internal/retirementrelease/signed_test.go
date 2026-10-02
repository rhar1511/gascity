package retirementrelease

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/qualification"
	"github.com/gastownhall/gascity/internal/selectorattestation"
	"github.com/gastownhall/gascity/internal/selectorinventory"
)

type testAuthority struct {
	deny       bool
	failVerify bool
	resolves   int
}

func (a *testAuthority) Resolve(_ context.Context, s qualification.CompatibilityScope) (qualification.CompatibilityPolicy, error) {
	a.resolves++
	sha, err := qualification.CompatibilityScopeIdentitySHA(s)
	return qualification.CompatibilityPolicy{Status: qualification.StatusAvailable, ScopeSHA256: sha, PolicyReference: "host-read-policy", PolicyVersion: "1", RequiredCapabilities: []string{"exact-source-read"}}, err
}

func (a *testAuthority) Authorize(_ context.Context, r qualification.CompatibilityRequest) (qualification.ActionAuthorization, error) {
	status := qualification.StatusAuthorized
	if a.deny {
		status = qualification.StatusDenied
	}
	return qualification.ActionAuthorization{Status: status, PolicyReference: r.Policy.PolicyReference, PolicyVersion: r.Policy.PolicyVersion, RequestSHA256: r.RequestSHA256, RecordID: "read-grant", Reference: "read-reference"}, nil
}

func (a *testAuthority) Verify(context.Context, qualification.CompatibilityRequest, qualification.ActionAuthorization) error {
	if a.failVerify {
		return errors.New("revoked permission")
	}
	return nil
}

type testProver struct{}

func (testProver) Prove(_ context.Context, _ qualification.CompatibilityScope, p qualification.CompatibilityPolicy, c string) (qualification.CapabilityProof, error) {
	return qualification.CapabilityProof{Status: qualification.StatusAvailable, Capability: c, ScopeSHA256: p.ScopeSHA256, PolicyReference: p.PolicyReference, PolicyVersion: p.PolicyVersion, EvidenceSHA256: strings.Repeat("a", 64)}, nil
}

type testKeys struct {
	bundle selectorattestation.Bundle
	loads  int
}

type testReleaseAuthority struct{ calls int }

func (a *testReleaseAuthority) Authorize(_ context.Context, r qualification.ReleaseRequest) (qualification.Authorization, error) {
	a.calls++
	sha, err := qualification.ReleaseRequestIdentitySHA(r)
	return qualification.Authorization{Status: qualification.StatusAuthorized, IdentitySHA: r.Snapshot.EffectiveConfigIdentitySHA256, ReleaseRequestSHA256: sha, RecordID: "fixture-release-record"}, err
}

func (s *testKeys) Load(context.Context) (selectorattestation.Bundle, error) {
	s.loads++
	return s.bundle, nil
}

type signedFixture struct {
	now       time.Time
	request   Request
	inputs    TrustedInputs
	authority *testAuthority
	keys      *testKeys
}

func canonical(t *testing.T, v any) string {
	t.Helper()
	b, err := qualification.CanonicalJSON(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func jsonDigest(t *testing.T, v any) string {
	t.Helper()
	s, err := qualification.DigestJSON(v)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func signToken(domain []byte, key ed25519.PrivateKey) string {
	prefix := strings.IndexByte(string(domain), '\n') + 1
	return base64.RawURLEncoding.EncodeToString(domain[prefix:]) + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, domain))
}

func newSignedFixture(t *testing.T) signedFixture {
	t.Helper()
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	sha := strings.Repeat("a", 64)
	rev := strings.Repeat("b", 40)
	closure := qualification.InputClosure{SchemaVersion: 1, Status: qualification.StatusAvailable, EnvironmentSHA: sha, Roots: []qualification.InputRoot{{ID: "private-pack", Kind: "pack", PinStatus: "locked", Pin: rev, ResolvedPathSHA256: sha, InputsSHA256: sha, InputCount: 1}}}
	closure.SHA256 = jsonDigest(t, struct {
		SchemaVersion  int                       `json:"schema_version"`
		EnvironmentSHA string                    `json:"environment_sha256"`
		Roots          []qualification.InputRoot `json:"roots"`
	}{1, sha, closure.Roots})
	snapshot, err := qualification.NewSnapshot(struct{ Mode string }{"fixture"}, closure)
	if err != nil || snapshot.Status != qualification.StatusAvailable {
		t.Fatalf("snapshot: %+v %v", snapshot, err)
	}
	build := qualification.BuildIdentity{Status: qualification.StatusAvailable, SourceRevision: rev, BuildID: rev, Version: "fixture", ArtifactStatus: qualification.StatusAvailable, ArtifactSHA256: sha}
	buildSHA := jsonDigest(t, build)
	policy := privateProtocolPolicy()
	source := sourceIdentity{rev, rev, rev, sha, sha, sha, sha, snapshot.EffectiveConfigSHA256}
	c := releaseContext{1, "retirement-source", "workspace-fixture", sha, sha, 1, "execution-1", source, []string{"rig-fixture"}, now.Add(-time.Minute).Format(time.RFC3339), now.Add(time.Minute).Format(time.RFC3339)}
	c.HostSHA256 = digestBytes([]byte("gascity.selectorwriter.host.v1\x00host-fixture"))
	c.BootSHA256 = digestBytes([]byte("gascity.selectorwriter.boot.v1\x00boot-fixture"))
	sourceMap := map[string]any{}
	sourceJSON, _ := json.Marshal(source)
	if err := json.Unmarshal(sourceJSON, &sourceMap); err != nil {
		t.Fatal(err)
	}
	sourceMap["identity_evidence"] = map[string]string{"path": "identity", "sha256": sha}
	candidate := map[string]any{"schema_version": 1, "claim_scope": "selector-retirement-evidence-only", "phases": map[string]any{}, "trial": map[string]any{"id": "trial-fixture", "mode": "isolated-one-rig-observation", "rig": "rig-fixture", "started_at": now.Add(-49 * time.Hour).Format(time.RFC3339), "ended_at": now.Add(-time.Hour).Format(time.RFC3339), "source": sourceMap, "selector_baseline": map[string]any{}, "excluded_orders": []string{}, "occupancy_intervals": []any{}}, "decision_observations": []any{}, "safety_scenarios": []any{}, "completions": []any{}, "human_review": map[string]any{}}
	candidate["human_review"] = map[string]any{"reviewer": "fixture-reviewer", "reviewed_at": now.Add(-30 * time.Minute).Format(time.RFC3339), "decision": "accept-evidence", "evidence": map[string]string{"path": "review", "sha256": sha}, "reviewed_delta_ids": []string{}}
	phases := map[string]any{}
	for _, phase := range []string{"comparison", "preflight", "trial"} {
		phases[phase] = map[string]any{"status": "complete", "started_at": now.Add(-49 * time.Hour).Format(time.RFC3339), "ended_at": now.Add(-time.Hour).Format(time.RFC3339), "evidence": map[string]string{"path": phase, "sha256": sha}}
	}
	candidate["phases"] = phases
	inventory := map[string]any{"writers": []any{}, "baseline_edges": []any{}, "effective_edges": []any{}, "scopes": []string{"cron", "manual", "orders", "services", "startup"}, "in_flight": []any{}}
	candidateSHA := jsonDigest(t, candidate)
	m := manifest{1, candidateSHA, sha, jsonDigest(t, inventory), json.RawMessage(canonical(t, candidate)), json.RawMessage(canonical(t, inventory)), c, json.RawMessage(canonical(t, map[string]any{"pack": policy.Activation.Pack, "workflow": policy.Activation.Workflow, "mayor_session": policy.Activation.Session, "rigs": c.Rigs, "retired_scripts": policy.Activation.RetiredScripts})), now.Format(time.RFC3339), "human-review"}
	manifestJSON := canonical(t, m)
	request := Request{Gate: m.Gate, RequestSHA256: jsonDigest(t, m), ManifestJSON: manifestJSON}
	observationKey := ed25519.NewKeyFromSeed(bytesSeed(1))
	reviewKey := ed25519.NewKeyFromSeed(bytesSeed(2))
	revokeKey := ed25519.NewKeyFromSeed(bytesSeed(3))
	o := selectorattestation.ObservationClaims{SchemaVersion: selectorattestation.ObservationSchemaVersionV1, Purpose: selectorattestation.ObservationKeyPurpose, RecordID: "observation-fixture", KeyID: "observation-key", Issuer: "collector", CollectorIdentity: "fixture-collector", Audience: c.Audience, Workspace: c.Workspace, CandidateManifestSHA256: candidateSHA, SelectorSnapshotSHA256: sha, ObservationLedgerSHA256: sha, SequenceStart: 1, SequenceEnd: 2, SequenceCompletenessResult: "complete", CoverageResult: "complete", RuntimeIdentitySHA256: sha, BuildIdentitySHA256: sha, ConfigIdentitySHA256: snapshot.EffectiveConfigIdentitySHA256, OrderInventorySHA256: sha, ExternalWriterInventorySHA256: sha, InFlightResolutionSHA256: sha, ObservationStartedAt: now.Add(-49 * time.Hour).Format(time.RFC3339), ObservationEndedAt: now.Add(-time.Hour).Format(time.RFC3339), IssuedAt: now.Add(-time.Minute).Format(time.RFC3339), ExpiresAt: now.Add(time.Minute).Format(time.RFC3339)}
	o.BuildIdentitySHA256 = buildSHA
	ob, err := selectorattestation.ObservationSigningBytes(o)
	if err != nil {
		t.Fatal(err)
	}
	observationToken := signToken(ob, observationKey)
	w := selectorattestation.ObservationExpectation{RecordID: o.RecordID, CollectorIdentity: o.CollectorIdentity, Audience: o.Audience, Workspace: o.Workspace, CandidateManifestSHA256: candidateSHA, SelectorSnapshotSHA256: sha, ObservationLedgerSHA256: sha, SequenceStart: 1, SequenceEnd: 2, SequenceCompletenessResult: "complete", CoverageResult: "complete", RuntimeIdentitySHA256: sha, BuildIdentitySHA256: sha, ConfigIdentitySHA256: o.ConfigIdentitySHA256, OrderInventorySHA256: sha, ExternalWriterInventorySHA256: sha, InFlightResolutionSHA256: sha, ObservationStartedAt: now.Add(-49 * time.Hour), ObservationEndedAt: now.Add(-time.Hour)}
	w.BuildIdentitySHA256 = buildSHA
	r := selectorattestation.ReviewClaims{SchemaVersion: selectorattestation.ReviewSchemaVersionV1, Purpose: selectorattestation.ReviewKeyPurpose, RecordID: "review-fixture", KeyID: "review-key", Issuer: "review-authority", ReviewerIdentity: "fixture-reviewer", Audience: c.Audience, Workspace: c.Workspace, CandidateManifestSHA256: candidateSHA, ObservationRecordSHA256: digestBytes([]byte(observationToken)), Decision: selectorattestation.DecisionApproved, ReviewedDeltaSHA256: sha, ReviewedAt: now.Add(-30 * time.Minute).Format(time.RFC3339), IssuedAt: now.Add(-time.Minute).Format(time.RFC3339), ExpiresAt: now.Add(time.Minute).Format(time.RFC3339)}
	rb, err := selectorattestation.ReviewSigningBytes(r)
	if err != nil {
		t.Fatal(err)
	}
	reviewToken := signToken(rb, reviewKey)
	request.RetainedBase64 = base64.StdEncoding.EncodeToString([]byte(reviewToken))
	revocations := selectorattestation.Revocations{SchemaVersion: selectorattestation.RevocationsSchemaVersionV1, Purpose: selectorattestation.RevocationKeyPurpose, KeyID: "revocation-key", Issuer: "revocation-authority", Audience: c.Audience, Workspace: c.Workspace, IssuedAt: now.Add(-time.Minute).Format(time.RFC3339), ExpiresAt: now.Add(time.Minute).Format(time.RFC3339), RevokedRecordIDs: []string{}}
	bytes, err := selectorattestation.RevocationSigningBytes(revocations)
	if err != nil {
		t.Fatal(err)
	}
	payload := bytes[len(selectorattestation.RevocationSigningDomain):]
	keys := &testKeys{bundle: selectorattestation.Bundle{Keys: []selectorattestation.TrustedKey{{KeyID: o.KeyID, Issuer: o.Issuer, Subject: o.CollectorIdentity, Purpose: o.Purpose, PublicKey: base64.StdEncoding.EncodeToString(observationKey.Public().(ed25519.PublicKey))}, {KeyID: r.KeyID, Issuer: r.Issuer, Subject: r.ReviewerIdentity, Purpose: r.Purpose, PublicKey: base64.StdEncoding.EncodeToString(reviewKey.Public().(ed25519.PublicKey))}, {KeyID: revocations.KeyID, Issuer: revocations.Issuer, Purpose: revocations.Purpose, PublicKey: base64.StdEncoding.EncodeToString(revokeKey.Public().(ed25519.PublicKey))}}, Revocations: selectorattestation.SignedRevocations{Payload: payload, Signature: base64.RawURLEncoding.EncodeToString(ed25519.Sign(revokeKey, bytes))}}}
	verifier, err := selectorattestation.NewVerifier(keys, selectorattestation.Options{Audience: c.Audience, RevocationPayloadSHA256: digestBytes(payload), MaxRecordAge: time.Hour, MaxRevocationAge: time.Hour, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	authority := &testAuthority{}
	releaseSHA, err := qualification.ReleaseRequestIdentitySHA(qualification.ReleaseRequest{Snapshot: snapshot, Build: build})
	if err != nil {
		t.Fatal(err)
	}
	permissionScope := qualification.CompatibilityScope{SchemaVersion: 1, CityID: c.Workspace, ServerID: "server-fixture", StoreRef: "store-fixture", EffectiveConfigSHA256: snapshot.EffectiveConfigIdentitySHA256, ReleaseRequestSHA256: releaseSHA, Formula: qualification.CompatibilityFormula{Name: "retirement-source-read/" + m.Gate, ContentSHA256: request.RequestSHA256, SourceSHA256: m.ReleaseBundleSHA256, CompiledSHA256: candidateSHA}, Packs: []qualification.CompatibilityPack{{Name: "private-pack", RootID: "private-pack", Pin: rev, PinStatus: "locked", ManifestSHA256: sha, SourceSubpathSHA256: sha, RequiresGC: "fixture"}}}
	inputs := TrustedInputs{ExpectedManifestJSON: manifestJSON, RetainedSHA256: digestBytes([]byte(reviewToken)), Epoch: "fence-1", Assurance: "fixture", EffectiveConfig: struct{ Mode string }{"fixture"}, Closure: closure, Build: build, PermissionScope: permissionScope, PermissionAuthority: authority, PermissionProver: testProver{}, Attestations: verifier, Observation: w, ObservationToken: observationToken, Review: selectorattestation.ReviewExpectation{RecordID: r.RecordID, ReviewerIdentity: r.ReviewerIdentity, Audience: r.Audience, Workspace: r.Workspace, CandidateManifestSHA256: candidateSHA, ObservationRecordSHA256: r.ObservationRecordSHA256, Decision: r.Decision, ReviewedDeltaSHA256: sha, ReviewedAt: now.Add(-30 * time.Minute)}}
	inputs.Policy = policy
	inputs.CurrentContext = &c
	inputs.CurrentController = selectorinventory.ControllerBinding{SnapshotSHA256: sha, Generation: c.ControllerGeneration, Build: build.BuildID}
	inputs.CurrentBuildIdentitySHA256 = buildSHA
	inputs.HostID = "host-fixture"
	inputs.BootID = "boot-fixture"
	return signedFixture{now, request, inputs, authority, keys}
}

func bytesSeed(n byte) []byte {
	result := make([]byte, 32)
	for i := range result {
		result[i] = n
	}
	return result
}

func TestSignedFixtureReviewAndRevalidation(t *testing.T) {
	f := newSignedFixture(t)
	calls := 0
	a := NewAdapter(Options{Enabled: true, Now: func() time.Time { return f.now }, Compose: func(context.Context, Binding) (TrustedInputs, error) { calls++; return f.inputs, nil }})
	v, err := a.Verify(context.Background(), f.request)
	if err != nil || v.Status != "verified" || v.Assurance != "fixture" || v.RequestSHA256 != f.request.RequestSHA256 || calls != 2 || f.keys.loads != 4 {
		t.Fatalf("fixture review/reloads: %+v err=%v composition=%d authority=%d", v, err, calls, f.keys.loads)
	}
}

func TestExactScopeAndAuthorityFailures(t *testing.T) {
	for _, name := range []string{"permission-denied", "permission-revoked", "permission-wrong-purpose", "permission-mutated-pack", "context-changed", "context-expired", "signature-invalid", "caller-success-json", "revocations-changed", "retention-mismatch", "review-observation-mismatch", "request-mismatch"} {
		t.Run(name, func(t *testing.T) {
			f := newSignedFixture(t)
			calls := 0
			switch name {
			case "permission-denied":
				f.authority.deny = true
			case "permission-revoked":
				f.authority.failVerify = true
			case "permission-wrong-purpose":
				f.inputs.PermissionScope.Formula.Name = "generic-release"
			case "context-expired":
				f.now = f.now.Add(2 * time.Minute)
			case "caller-success-json":
				raw := []byte(`{"status":"verified","assurance":"operational"}`)
				f.request.RetainedBase64 = base64.StdEncoding.EncodeToString(raw)
				f.inputs.RetainedSHA256 = digestBytes(raw)
			case "signature-invalid":
				raw, _ := base64.StdEncoding.DecodeString(f.request.RetainedBase64)
				raw[len(raw)-2] ^= 1
				f.request.RetainedBase64 = base64.StdEncoding.EncodeToString(raw)
				f.inputs.RetainedSHA256 = digestBytes(raw)
			case "revocations-changed":
				f.keys.bundle.Revocations.Payload = []byte(`{}`)
			case "retention-mismatch":
				f.inputs.RetainedSHA256 = strings.Repeat("0", 64)
			case "review-observation-mismatch":
				f.inputs.Review.ObservationRecordSHA256 = strings.Repeat("0", 64)
			case "request-mismatch":
				f.request.RequestSHA256 = strings.Repeat("0", 64)
			}
			a := NewAdapter(Options{Enabled: true, Now: func() time.Time { return f.now }, Compose: func(context.Context, Binding) (TrustedInputs, error) {
				calls++
				inputs := f.inputs
				if name == "context-changed" && calls == 2 {
					inputs.Epoch = "fence-2"
				}
				if name == "permission-mutated-pack" && calls == 2 {
					inputs.PermissionScope.Packs[0].ManifestSHA256 = strings.Repeat("0", 64)
				}
				return inputs, nil
			}})
			v, _ := a.Verify(context.Background(), f.request)
			if v.Status == "verified" || v.Assurance == "operational" {
				t.Fatalf("failure accepted: %+v", v)
			}
		})
	}
}

func TestManifestRejectsNonExactAndMismatchedInputs(t *testing.T) {
	f := newSignedFixture(t)
	for _, name := range []string{"unknown-field", "null-context", "missing-source", "boolean-generation", "candidate-digest", "inventory-digest", "nested-duplicate", "trailing-json", "noncanonical"} {
		t.Run(name, func(t *testing.T) {
			var root map[string]any
			if err := json.Unmarshal([]byte(f.request.ManifestJSON), &root); err != nil {
				t.Fatal(err)
			}
			switch name {
			case "unknown-field":
				root["success"] = true
			case "null-context":
				root["context"] = nil
			case "missing-source":
				delete(root["context"].(map[string]any)["source"].(map[string]any), "backend_identity_sha256")
			case "boolean-generation":
				root["context"].(map[string]any)["controller_generation"] = true
			case "candidate-digest":
				root["candidate_manifest_sha256"] = strings.Repeat("0", 64)
			case "inventory-digest":
				root["inventory_sha256"] = strings.Repeat("0", 64)
			}
			text := canonical(t, root)
			if name == "nested-duplicate" {
				text = strings.Replace(text, `"audience":"retirement-source"`, `"audience":"retirement-source","audience":"retirement-source"`, 1)
			}
			if name == "trailing-json" {
				text += "{}"
			}
			if name == "noncanonical" {
				text = " " + text
			}
			if _, err := parseManifest(text); err == nil {
				t.Fatalf("accepted %s", name)
			}
		})
	}
}

func TestUnsupportedContractsCannotBecomeOperational(t *testing.T) {
	f := newSignedFixture(t)
	for gate := range gates {
		if gate == "human-review" || gate == "external-writers" || gate == "in-flight-resolution" || gate == "compatibility" {
			continue
		}
		b := Binding{Gate: gate, CandidateManifestSHA256: f.inputs.Observation.CandidateManifestSHA256, RequestSHA256: f.request.RequestSHA256}
		snapshot, err := qualification.NewSnapshot(f.inputs.EffectiveConfig, f.inputs.Closure)
		if err != nil {
			t.Fatal(err)
		}
		raw := []byte("retained claims")
		if gate == "trial-ledger" {
			raw = []byte(f.inputs.ObservationToken)
		}
		v, _ := verifyCanonical(context.Background(), f.inputs, b, raw, snapshot, f.now)
		if v.Status != "unavailable" || v.Assurance != "unavailable" {
			t.Fatalf("%s wrongly ready: %+v", gate, v)
		}
	}
}

func TestCanonicalReleaseApprovalStillRequiresPrivateCapabilityContract(t *testing.T) {
	f := newSignedFixture(t)
	authority := &testReleaseAuthority{}
	f.inputs.ReleaseAuthority = authority
	retarget(t, &f, "release-authorization", []byte("retained-release-reference"))
	a := NewAdapter(Options{Enabled: true, Now: func() time.Time { return f.now }, Compose: func(context.Context, Binding) (TrustedInputs, error) { return f.inputs, nil }})
	v, err := a.Verify(context.Background(), f.request)
	if err != nil || v.Status != "unavailable" || v.Assurance != "unavailable" || authority.calls != 2 || v.Reason != "canonical_release_verified_private_release_capability_contract_unavailable" {
		t.Fatalf("generic canonical release wrongly sufficient: %+v %v calls=%d", v, err, authority.calls)
	}
}
