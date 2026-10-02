//go:build cross_language_harness

package api

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/qualification"
	"github.com/gastownhall/gascity/internal/retirementrelease"
	"github.com/gastownhall/gascity/internal/selectorattestation"
	"github.com/gastownhall/gascity/internal/selectorinventory"
)

// Fixture construction follows retirementrelease/signed_test.go. Canonical
// serialization, manifest parsing and signed observation/review/revocation
// verification remain the actual production implementations.
type crossLanguageAuthority struct {
	scope qualification.CompatibilityScope
}

func (a crossLanguageAuthority) Resolve(ctx context.Context, s qualification.CompatibilityScope) (qualification.CompatibilityPolicy, error) {
	if _, ok := verifiedCityWritePrincipal(ctx); !ok {
		return qualification.CompatibilityPolicy{}, errors.New("fixture authenticated city principal required")
	}
	want, err := qualification.CompatibilityScopeIdentitySHA(a.scope)
	if err != nil {
		return qualification.CompatibilityPolicy{}, err
	}
	got, err := qualification.CompatibilityScopeIdentitySHA(s)
	if err != nil || got != want {
		return qualification.CompatibilityPolicy{}, errors.New("fixture permission scope mismatch")
	}
	return qualification.CompatibilityPolicy{Status: qualification.StatusAvailable, ScopeSHA256: got, PolicyReference: "fixture-exact-source-read", PolicyVersion: "1", RequiredCapabilities: []string{"exact-source-read"}}, nil
}

func (a crossLanguageAuthority) Authorize(ctx context.Context, r qualification.CompatibilityRequest) (qualification.ActionAuthorization, error) {
	p, err := a.Resolve(ctx, r.Scope)
	if err != nil {
		return qualification.ActionAuthorization{}, err
	}
	if r.Policy.PolicyReference != p.PolicyReference || r.Policy.ScopeSHA256 != p.ScopeSHA256 {
		return qualification.ActionAuthorization{}, errors.New("fixture policy mismatch")
	}
	return qualification.ActionAuthorization{Status: qualification.StatusAuthorized, PolicyReference: p.PolicyReference, PolicyVersion: p.PolicyVersion, RequestSHA256: r.RequestSHA256, RecordID: "fixture-source-read", Reference: "fixture-read-only"}, nil
}

func (a crossLanguageAuthority) Verify(ctx context.Context, r qualification.CompatibilityRequest, auth qualification.ActionAuthorization) error {
	want, err := a.Authorize(ctx, r)
	if err != nil {
		return err
	}
	if auth != want {
		return errors.New("fixture read authorization mismatch")
	}
	return nil
}

type crossLanguageProver struct{}

func (crossLanguageProver) Prove(_ context.Context, _ qualification.CompatibilityScope, p qualification.CompatibilityPolicy, c string) (qualification.CapabilityProof, error) {
	if c != "exact-source-read" {
		return qualification.CapabilityProof{}, errors.New("fixture capability unavailable")
	}
	return qualification.CapabilityProof{Status: qualification.StatusAvailable, Capability: c, ScopeSHA256: p.ScopeSHA256, PolicyReference: p.PolicyReference, PolicyVersion: p.PolicyVersion, EvidenceSHA256: strings.Repeat("a", 64)}, nil
}

type crossLanguageKeys struct{ bundle selectorattestation.Bundle }

func (k crossLanguageKeys) Load(context.Context) (selectorattestation.Bundle, error) {
	return k.bundle, nil
}

func crossLanguageDigest(b []byte) string { sum := sha256.Sum256(b); return hex.EncodeToString(sum[:]) }
func crossLanguageCanonical(t *testing.T, v any) string {
	t.Helper()
	b, err := qualification.CanonicalJSON(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func crossLanguageJSONDigest(t *testing.T, v any) string {
	t.Helper()
	s, err := qualification.DigestJSON(v)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func crossLanguageToken(t *testing.T, domain []byte, err error, seed byte) string {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	prefix := bytes.IndexByte(domain, '\n') + 1
	if prefix == 0 {
		t.Fatal("missing canonical signing domain")
	}
	return base64.RawURLEncoding.EncodeToString(domain[prefix:]) + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(ed25519.NewKeyFromSeed(bytes.Repeat([]byte{seed}, 32)), domain))
}

func crossLanguageRetirementFixture(t *testing.T, now time.Time) (retirementrelease.Request, *retirementrelease.Adapter) {
	f := newCrossLanguageRetirementFixture(t, now)
	return f.request, f.adapter(t)
}

type crossLanguageRetirementSource struct {
	request retirementrelease.Request
	inputs  retirementrelease.TrustedInputs
	keys    *crossLanguageKeys
	now     time.Time
}

func newCrossLanguageRetirementFixture(t *testing.T, now time.Time) *crossLanguageRetirementSource {
	t.Helper()
	sha := strings.Repeat("a", 64)
	rev := strings.Repeat("b", 40)
	closure := qualification.InputClosure{SchemaVersion: 1, Status: qualification.StatusAvailable, EnvironmentSHA: sha, Roots: []qualification.InputRoot{{ID: "fixture-pack", Kind: "pack", PinStatus: "locked", Pin: rev, ResolvedPathSHA256: sha, InputsSHA256: sha, InputCount: 1}}}
	closure.SHA256 = crossLanguageJSONDigest(t, struct {
		SchemaVersion  int                       `json:"schema_version"`
		EnvironmentSHA string                    `json:"environment_sha256"`
		Roots          []qualification.InputRoot `json:"roots"`
	}{1, sha, closure.Roots})
	config := struct{ Mode string }{"fixture"}
	snapshot, err := qualification.NewSnapshot(config, closure)
	if err != nil {
		t.Fatal(err)
	}
	build := qualification.BuildIdentity{Status: qualification.StatusAvailable, SourceRevision: rev, BuildID: rev, Version: "fixture", ArtifactStatus: qualification.StatusAvailable, ArtifactSHA256: sha}
	buildSHA := crossLanguageJSONDigest(t, build)
	policy := &retirementrelease.ProtocolPolicy{Reference: "fixture-retirement-protocol", Version: "1", Activation: retirementrelease.ActivationScope{Pack: "fixture-pack", Workflow: "fixture-workflow", Session: "fixture-review", RetiredScripts: []string{"fixture/retired.sh"}}, MinimumTrialDuration: 48 * time.Hour, MaximumTrialToReview: 7 * 24 * time.Hour, MaximumContextAge: 5 * time.Minute}
	source := retirementrelease.SourceIdentity{ControllerSourceCommit: rev, PackSourceCommit: rev, BackendSourceCommit: rev, ControllerBinarySHA256: sha, BackendBinarySHA256: sha, RuntimeIdentitySHA256: sha, BackendIdentitySHA256: sha, EffectiveConfigSHA256: snapshot.EffectiveConfigSHA256}
	c := retirementrelease.Context{SchemaVersion: 1, Audience: "retirement-source", Workspace: "workspace-fixture", HostSHA256: crossLanguageDigest([]byte("gascity.selectorwriter.host.v1\x00host-fixture")), BootSHA256: crossLanguageDigest([]byte("gascity.selectorwriter.boot.v1\x00boot-fixture")), ControllerGeneration: 1, ExecutionGeneration: "execution-1", Source: source, Rigs: []string{"rig-fixture"}, ObservedAt: now.Add(-time.Minute).Format(time.RFC3339), ExpiresAt: now.Add(time.Minute).Format(time.RFC3339)}
	// These maps are canonical protocol fixture inputs, never HTTP endpoint wire
	// types or a replacement manifest codec. Adapter.Verify validates every field.
	ref := map[string]string{"path": "fixture", "sha256": sha}
	sourceFields := map[string]any{"controller_source_commit": rev, "pack_source_commit": rev, "backend_source_commit": rev, "controller_binary_sha256": sha, "backend_binary_sha256": sha, "runtime_identity_sha256": sha, "backend_identity_sha256": sha, "effective_config_sha256": snapshot.EffectiveConfigSHA256, "identity_evidence": ref}
	start := now.Add(-49 * time.Hour)
	end := now.Add(-time.Hour)
	reviewed := now.Add(-30 * time.Minute)
	phases := map[string]any{}
	for _, p := range []string{"comparison", "preflight", "trial"} {
		phases[p] = map[string]any{"status": "complete", "started_at": start.Format(time.RFC3339), "ended_at": end.Format(time.RFC3339), "evidence": ref}
	}
	candidate := map[string]any{"schema_version": 1, "claim_scope": "selector-retirement-evidence-only", "phases": phases, "trial": map[string]any{"id": "trial-fixture", "mode": "isolated-one-rig-observation", "rig": "rig-fixture", "started_at": start.Format(time.RFC3339), "ended_at": end.Format(time.RFC3339), "source": sourceFields, "selector_baseline": map[string]any{}, "excluded_orders": []string{}, "occupancy_intervals": []any{}}, "decision_observations": []any{}, "safety_scenarios": []any{}, "completions": []any{}, "human_review": map[string]any{"reviewer": "fixture-reviewer", "reviewed_at": reviewed.Format(time.RFC3339), "decision": "accept-evidence", "evidence": ref, "reviewed_delta_ids": []string{}}}
	inventory := map[string]any{"writers": []any{}, "baseline_edges": []any{}, "effective_edges": []any{}, "scopes": []string{"cron", "manual", "orders", "services", "startup"}, "in_flight": []any{}}
	candidateSHA := crossLanguageJSONDigest(t, candidate)
	manifest := map[string]any{"schema_version": 1, "candidate_manifest_sha256": candidateSHA, "release_bundle_sha256": sha, "inventory_sha256": crossLanguageJSONDigest(t, inventory), "candidate": candidate, "inventory": inventory, "context": c, "activation": map[string]any{"pack": policy.Activation.Pack, "workflow": policy.Activation.Workflow, "mayor_session": policy.Activation.Session, "rigs": c.Rigs, "retired_scripts": policy.Activation.RetiredScripts}, "trusted_now": now.Format(time.RFC3339), "gate": "human-review"}
	r := retirementrelease.Request{Gate: "human-review", RequestSHA256: crossLanguageJSONDigest(t, manifest), ManifestJSON: crossLanguageCanonical(t, manifest)}
	o := selectorattestation.ObservationClaims{SchemaVersion: selectorattestation.ObservationSchemaVersionV1, Purpose: selectorattestation.ObservationKeyPurpose, RecordID: "observation-fixture", KeyID: "observation-key", Issuer: "collector", CollectorIdentity: "fixture-collector", Audience: c.Audience, Workspace: c.Workspace, CandidateManifestSHA256: candidateSHA, SelectorSnapshotSHA256: sha, ObservationLedgerSHA256: sha, SequenceStart: 1, SequenceEnd: 2, SequenceCompletenessResult: "complete", CoverageResult: "complete", RuntimeIdentitySHA256: sha, BuildIdentitySHA256: buildSHA, ConfigIdentitySHA256: snapshot.EffectiveConfigIdentitySHA256, OrderInventorySHA256: sha, ExternalWriterInventorySHA256: sha, InFlightResolutionSHA256: sha, ObservationStartedAt: start.Format(time.RFC3339), ObservationEndedAt: end.Format(time.RFC3339), IssuedAt: now.Add(-time.Minute).Format(time.RFC3339), ExpiresAt: now.Add(time.Minute).Format(time.RFC3339)}
	b, e := selectorattestation.ObservationSigningBytes(o)
	observationToken := crossLanguageToken(t, b, e, 1)
	review := selectorattestation.ReviewClaims{SchemaVersion: selectorattestation.ReviewSchemaVersionV1, Purpose: selectorattestation.ReviewKeyPurpose, RecordID: "review-fixture", KeyID: "review-key", Issuer: "review-authority", ReviewerIdentity: "fixture-reviewer", Audience: c.Audience, Workspace: c.Workspace, CandidateManifestSHA256: candidateSHA, ObservationRecordSHA256: crossLanguageDigest([]byte(observationToken)), Decision: selectorattestation.DecisionApproved, ReviewedDeltaSHA256: sha, ReviewedAt: reviewed.Format(time.RFC3339), IssuedAt: now.Add(-time.Minute).Format(time.RFC3339), ExpiresAt: now.Add(time.Minute).Format(time.RFC3339)}
	b, e = selectorattestation.ReviewSigningBytes(review)
	reviewToken := crossLanguageToken(t, b, e, 2)
	r.RetainedBase64 = base64.StdEncoding.EncodeToString([]byte(reviewToken))
	revocations := selectorattestation.Revocations{SchemaVersion: selectorattestation.RevocationsSchemaVersionV1, Purpose: selectorattestation.RevocationKeyPurpose, KeyID: "revocation-key", Issuer: "revocation-authority", Audience: c.Audience, Workspace: c.Workspace, IssuedAt: now.Add(-time.Minute).Format(time.RFC3339), ExpiresAt: now.Add(time.Minute).Format(time.RFC3339), RevokedRecordIDs: []string{}}
	b, e = selectorattestation.RevocationSigningBytes(revocations)
	if e != nil {
		t.Fatal(e)
	}
	payload := b[len(selectorattestation.RevocationSigningDomain):]
	public := func(n byte) string {
		return base64.StdEncoding.EncodeToString(ed25519.NewKeyFromSeed(bytes.Repeat([]byte{n}, 32)).Public().(ed25519.PublicKey))
	}
	keys := crossLanguageKeys{selectorattestation.Bundle{Keys: []selectorattestation.TrustedKey{{KeyID: o.KeyID, Issuer: o.Issuer, Subject: o.CollectorIdentity, Purpose: o.Purpose, PublicKey: public(1)}, {KeyID: review.KeyID, Issuer: review.Issuer, Subject: review.ReviewerIdentity, Purpose: review.Purpose, PublicKey: public(2)}, {KeyID: revocations.KeyID, Issuer: revocations.Issuer, Purpose: revocations.Purpose, PublicKey: public(3)}}, Revocations: selectorattestation.SignedRevocations{Payload: payload, Signature: base64.RawURLEncoding.EncodeToString(ed25519.Sign(ed25519.NewKeyFromSeed(bytes.Repeat([]byte{3}, 32)), b))}}}
	verifier, err := selectorattestation.NewVerifier(keys, selectorattestation.Options{Audience: c.Audience, RevocationPayloadSHA256: crossLanguageDigest(payload), MaxRecordAge: time.Hour, MaxRevocationAge: time.Hour, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	releaseSHA, err := qualification.ReleaseRequestIdentitySHA(qualification.ReleaseRequest{Snapshot: snapshot, Build: build})
	if err != nil {
		t.Fatal(err)
	}
	scope := qualification.CompatibilityScope{SchemaVersion: 1, CityID: c.Workspace, ServerID: "server-fixture", StoreRef: "store-fixture", EffectiveConfigSHA256: snapshot.EffectiveConfigIdentitySHA256, ReleaseRequestSHA256: releaseSHA, Formula: qualification.CompatibilityFormula{Name: "retirement-source-read/human-review", ContentSHA256: r.RequestSHA256, SourceSHA256: sha, CompiledSHA256: candidateSHA}, Packs: []qualification.CompatibilityPack{{Name: "fixture-pack", RootID: "fixture-pack", Pin: rev, PinStatus: "locked", ManifestSHA256: sha, SourceSubpathSHA256: sha, RequiresGC: "fixture"}}}
	inputs := retirementrelease.TrustedInputs{ExpectedManifestJSON: r.ManifestJSON, RetainedSHA256: crossLanguageDigest([]byte(reviewToken)), Epoch: "fixture-1", Assurance: "fixture", Policy: policy, CurrentContext: &c, CurrentController: selectorinventory.ControllerBinding{SnapshotSHA256: sha, Generation: 1, Build: rev}, CurrentBuildIdentitySHA256: buildSHA, EffectiveConfig: config, Closure: closure, Build: build, PermissionScope: scope, PermissionAuthority: crossLanguageAuthority{scope}, PermissionProver: crossLanguageProver{}, Attestations: verifier, ObservationToken: observationToken, Observation: selectorattestation.ObservationExpectation{RecordID: o.RecordID, CollectorIdentity: o.CollectorIdentity, Audience: c.Audience, Workspace: c.Workspace, CandidateManifestSHA256: candidateSHA, SelectorSnapshotSHA256: sha, ObservationLedgerSHA256: sha, SequenceStart: 1, SequenceEnd: 2, SequenceCompletenessResult: "complete", CoverageResult: "complete", RuntimeIdentitySHA256: sha, BuildIdentitySHA256: buildSHA, ConfigIdentitySHA256: o.ConfigIdentitySHA256, OrderInventorySHA256: sha, ExternalWriterInventorySHA256: sha, InFlightResolutionSHA256: sha, ObservationStartedAt: start, ObservationEndedAt: end}, Review: selectorattestation.ReviewExpectation{RecordID: review.RecordID, ReviewerIdentity: review.ReviewerIdentity, Audience: c.Audience, Workspace: c.Workspace, CandidateManifestSHA256: candidateSHA, ObservationRecordSHA256: review.ObservationRecordSHA256, Decision: review.Decision, ReviewedDeltaSHA256: sha, ReviewedAt: reviewed}, HostID: "host-fixture", BootID: "boot-fixture"}
	return &crossLanguageRetirementSource{r, inputs, &keys, now}
}

func (f *crossLanguageRetirementSource) adapter(t *testing.T) *retirementrelease.Adapter {
	t.Helper()
	var addresses struct {
		Release   string `json:"release_bundle_sha256"`
		Inventory string `json:"inventory_sha256"`
	}
	if err := json.Unmarshal([]byte(f.request.ManifestJSON), &addresses); err != nil {
		t.Fatal(err)
	}
	return retirementrelease.NewAdapter(retirementrelease.Options{Enabled: true, Now: func() time.Time { return f.now }, Compose: func(ctx context.Context, b retirementrelease.Binding) (retirementrelease.TrustedInputs, error) {
		if _, ok := verifiedCityWritePrincipal(ctx); !ok || b.Gate != f.request.Gate || b.RequestSHA256 != f.request.RequestSHA256 || b.CandidateManifestSHA256 != f.inputs.Observation.CandidateManifestSHA256 || b.ReleaseBundleSHA256 != addresses.Release || b.InventorySHA256 != addresses.Inventory {
			return retirementrelease.TrustedInputs{}, errors.New("fixture trusted binding mismatch")
		}
		return f.inputs, nil
	}})
}
