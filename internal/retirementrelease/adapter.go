// Package retirementrelease composes read-only canonical retirement verifiers.
// It neither collects evidence nor grants permission to retire or activate work.
package retirementrelease

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/qualification"
	"github.com/gastownhall/gascity/internal/selectorattestation"
	"github.com/gastownhall/gascity/internal/selectorinventory"
	"github.com/gastownhall/gascity/internal/selectorwriter"
)

// Request transports the exact canonical checker request and retained proof.
// JSON text is a bounded string, not a caller-selectable verifier or verdict.
type Request struct {
	Gate           string `json:"gate"`
	RequestSHA256  string `json:"request_sha256"`
	ManifestJSON   string `json:"manifest_json"`
	RetainedBase64 string `json:"retained_base64"`
}

// Verdict is the retained checker's in-memory verifier result contract.
type Verdict struct {
	Status        string `json:"status"`
	RequestSHA256 string `json:"request_sha256"`
	Assurance     string `json:"assurance"`
	Reason        string `json:"reason"`
}

// Binding contains immutable addresses, never success assertions.
type Binding struct {
	Gate                    string
	RequestSHA256           string
	CandidateManifestSHA256 string
	ReleaseBundleSHA256     string
	InventorySHA256         string
}

// TrustedInputs is supplied only by host application composition. ExpectedManifestJSON
// must be independently reconstructed, including backend, host, boot, generations,
// rig scope and current loaded inputs. RetainedSHA256 comes from trusted retention.
// Policy comes from the owning trusted protocol configuration. CurrentContext,
// CurrentController, HostID and BootID must be independently derived from current
// host/runtime state. CurrentBuildIdentitySHA256 is DigestJSON(Build).
// Permission authority must resolve the authenticated principal from ctx and bind
// the exact read purpose and request; a generic release approval is insufficient.
type TrustedInputs struct {
	ExpectedManifestJSON       string
	RetainedSHA256             string
	Epoch                      string
	Assurance                  string
	Policy                     *ProtocolPolicy
	CurrentContext             *Context
	CurrentController          selectorinventory.ControllerBinding
	CurrentBuildIdentitySHA256 string
	EffectiveConfig            any
	Closure                    qualification.InputClosure
	Build                      qualification.BuildIdentity
	PermissionScope            qualification.CompatibilityScope
	PermissionAuthority        qualification.CompatibilityAuthority
	PermissionProver           qualification.CapabilityProver
	CompatibilityScope         qualification.CompatibilityScope
	CompatibilityAuthority     qualification.CompatibilityAuthority
	CapabilityProver           qualification.CapabilityProver
	ReleaseAuthority           qualification.ReleaseAuthorizer
	Attestations               *selectorattestation.Verifier
	Observation                selectorattestation.ObservationExpectation
	ObservationToken           string
	Review                     selectorattestation.ReviewExpectation
	Writers                    *selectorwriter.Verifier
	External                   selectorinventory.ExternalLedgerExpectation
	Registry                   selectorinventory.RegistrySnapshotInput
	HostID                     string
	BootID                     string
}

// Options configures a default-off trusted source adapter. Compose must derive
// current expectations without copying caller context or accepting saved verdicts.
type Options struct {
	Enabled bool
	Now     func() time.Time
	Compose func(context.Context, Binding) (TrustedInputs, error)
}

// Adapter owns no persistence, signing keys, collectors, or operational actions.
type Adapter struct{ options Options }

// NewAdapter creates a read-only adapter; zero options leave verification off.
func NewAdapter(options Options) *Adapter { return &Adapter{options: options} }

// Verify checks exact request bytes, independently derived context, scoped read
// permission, canonical evidence, and a second current composition before return.
func (a *Adapter) Verify(ctx context.Context, request Request) (Verdict, error) {
	v := Verdict{Status: "unavailable", RequestSHA256: request.RequestSHA256, Assurance: "unavailable", Reason: "source_adapter_disabled"}
	if a == nil || !a.options.Enabled {
		return v, nil
	}
	if ctx == nil || a.options.Compose == nil || a.options.Now == nil {
		v.Reason = "trusted_composition_unavailable"
		return v, nil
	}
	if err := ctx.Err(); err != nil {
		return v, err
	}
	m, err := parseManifest(request.ManifestJSON)
	if err != nil {
		v.Status = "rejected"
		v.Reason = "manifest_invalid"
		return v, err
	}
	sha, err := qualification.DigestJSON(m)
	if err != nil || sha != request.RequestSHA256 || m.Gate != request.Gate {
		v.Status = "rejected"
		v.Reason = "request_binding_mismatch"
		return v, errors.New(v.Reason)
	}
	if len(request.RetainedBase64) > 24<<20 {
		v.Status = "rejected"
		v.Reason = "proof_too_large"
		return v, errors.New(v.Reason)
	}
	raw, err := base64.StdEncoding.DecodeString(request.RetainedBase64)
	if err != nil || len(raw) == 0 || len(raw) > 16<<20 || base64.StdEncoding.EncodeToString(raw) != request.RetainedBase64 {
		v.Status = "rejected"
		v.Reason = "proof_encoding_invalid"
		return v, errors.New(v.Reason)
	}
	b := Binding{request.Gate, sha, m.CandidateManifestSHA256, m.ReleaseBundleSHA256, m.InventorySHA256}
	first, err := a.options.Compose(ctx, b)
	if err != nil {
		v.Reason = "trusted_composition_failed"
		return v, err
	}
	first = isolate(first)
	if err := first.Policy.validate(); err != nil {
		v.Reason = "protocol_policy_unavailable"
		return v, err
	}
	if first.CurrentContext == nil {
		v.Reason = "independent_context_unavailable"
		return v, errors.New(v.Reason)
	}
	snapshot, err := validateTrusted(first, request, raw, m, a.options.Now())
	if err != nil {
		v.Reason = "trusted_context_mismatch"
		return v, err
	}
	permission, permissionRequest, err := authorizePermission(ctx, first, b, snapshot)
	if err != nil {
		v.Reason = "scoped_permission_unavailable"
		if errors.Is(err, qualification.ErrCompatibilityDenied) {
			v.Reason = "scoped_permission_denied"
		}
		return v, err
	}
	v, err = verifyCanonical(ctx, first, b, raw, snapshot, a.options.Now())
	if err != nil {
		return v, err
	}
	second, err := a.options.Compose(ctx, b)
	if err != nil {
		return unavailable(b, "context_revalidation_failed"), err
	}
	second = isolate(second)
	secondSnapshot, err := validateTrusted(second, request, raw, m, a.options.Now())
	if err != nil || !sameComposition(first, second) || !reflect.DeepEqual(snapshot, secondSnapshot) {
		return unavailable(b, "context_changed"), errors.New("trusted context changed during verification")
	}
	if err := qualification.RevalidateCompatibility(ctx, second.PermissionAuthority, permissionRequest, permission); err != nil {
		return unavailable(b, "permission_revalidation_failed"), err
	}
	// Reload canonical authorities and revocations through the newly derived ports.
	v, err = verifyCanonical(ctx, second, b, raw, secondSnapshot, a.options.Now())
	if err != nil {
		return v, err
	}
	if err := ctx.Err(); err != nil {
		return unavailable(b, "verification_canceled"), err
	}
	return v, nil
}

func validateTrusted(t TrustedInputs, r Request, raw []byte, m manifest, now time.Time) (qualification.Snapshot, error) {
	if err := t.Policy.validate(); err != nil {
		return qualification.Snapshot{}, err
	}
	if err := t.Policy.validateActivation(m.Activation); err != nil {
		return qualification.Snapshot{}, err
	}
	if t.CurrentContext == nil || !reflect.DeepEqual(*t.CurrentContext, m.Context) {
		return qualification.Snapshot{}, errors.New("independent context differs from expected context")
	}
	if t.ExpectedManifestJSON != r.ManifestJSON || t.Epoch == "" || (t.Assurance != "fixture" && t.Assurance != "operational") || digestBytes(raw) != t.RetainedSHA256 {
		return qualification.Snapshot{}, errors.New("independent request or retention binding unavailable")
	}
	if err := m.Context.validate(now, t.Policy.MaximumContextAge); err != nil {
		return qualification.Snapshot{}, err
	}
	requestTime, err := time.Parse(time.RFC3339, m.TrustedNow)
	if err != nil || requestTime.After(now) || now.Sub(requestTime) > t.Policy.MaximumContextAge {
		return qualification.Snapshot{}, errors.New("request trusted time is future or stale")
	}
	if err := m.Context.validate(requestTime, t.Policy.MaximumContextAge); err != nil {
		return qualification.Snapshot{}, err
	}
	snapshot, err := qualification.NewSnapshot(t.EffectiveConfig, t.Closure)
	if err != nil {
		return snapshot, fmt.Errorf("loaded qualification unavailable: %w", err)
	}
	if snapshot.Status != qualification.StatusAvailable {
		return snapshot, errors.New("loaded qualification unavailable")
	}
	if snapshot.EffectiveConfigSHA256 != m.Context.Source.EffectiveConfigSHA256 || t.Build.SourceRevision != m.Context.Source.ControllerSourceCommit || t.Build.ArtifactSHA256 != m.Context.Source.ControllerBinarySHA256 {
		return snapshot, errors.New("loaded config or build differs from context")
	}
	if t.Build.Status != qualification.StatusAvailable || t.Build.ArtifactStatus != qualification.StatusAvailable || t.Build.SourceDirty || t.Build.BuildID != t.Build.SourceRevision || strings.TrimSpace(t.Build.Version) == "" {
		return snapshot, errors.New("running build unavailable")
	}
	buildSHA, err := qualification.DigestJSON(t.Build)
	if err != nil || buildSHA != t.CurrentBuildIdentitySHA256 {
		return snapshot, errors.New("independent build digest differs from running build")
	}
	if !validAtom(t.HostID) || !validAtom(t.BootID) || digestBytes([]byte("gascity.selectorwriter.host.v1\x00"+t.HostID)) != m.Context.HostSHA256 || digestBytes([]byte("gascity.selectorwriter.boot.v1\x00"+t.BootID)) != m.Context.BootSHA256 || t.CurrentController.Generation != m.Context.ControllerGeneration || t.CurrentController.Build != t.Build.BuildID || !validDigest(t.CurrentController.SnapshotSHA256) || !validDigest(t.CurrentBuildIdentitySHA256) {
		return snapshot, errors.New("independent host, boot or controller binding differs from context")
	}
	if t.Observation.RecordID != "" && (t.Observation.Audience != m.Context.Audience || t.Observation.Workspace != m.Context.Workspace || t.Observation.RuntimeIdentitySHA256 != m.Context.Source.RuntimeIdentitySHA256 || t.Observation.BuildIdentitySHA256 != t.CurrentBuildIdentitySHA256 || t.Observation.SelectorSnapshotSHA256 != t.CurrentController.SnapshotSHA256 || t.Observation.ConfigIdentitySHA256 != snapshot.EffectiveConfigIdentitySHA256) {
		return snapshot, errors.New("observation context scope mismatch")
	}
	var candidate struct {
		Trial struct {
			StartedAt string `json:"started_at"`
			EndedAt   string `json:"ended_at"`
			Rig       string `json:"rig"`
		} `json:"trial"`
		Review struct {
			Reviewer   string `json:"reviewer"`
			ReviewedAt string `json:"reviewed_at"`
			Decision   string `json:"decision"`
		} `json:"human_review"`
	}
	if err := json.Unmarshal(m.Candidate, &candidate); err != nil {
		return snapshot, err
	}
	started, startErr := time.Parse(time.RFC3339, candidate.Trial.StartedAt)
	ended, endErr := time.Parse(time.RFC3339, candidate.Trial.EndedAt)
	reviewed, reviewErr := time.Parse(time.RFC3339, candidate.Review.ReviewedAt)
	if startErr != nil || endErr != nil || reviewErr != nil || !ended.After(started) || ended.Sub(started) < t.Policy.MinimumTrialDuration || reviewed.Before(ended) || reviewed.After(now) || reviewed.Sub(started) > t.Policy.MaximumTrialToReview || !slices.Contains(m.Context.Rigs, candidate.Trial.Rig) {
		return snapshot, errors.New("candidate chronology or rig differs from configured protocol")
	}
	if (m.Gate == "human-review" || m.Gate == "trial-ledger") && (!started.Equal(t.Observation.ObservationStartedAt) || !ended.Equal(t.Observation.ObservationEndedAt)) {
		return snapshot, errors.New("observation does not bind complete candidate trial window")
	}
	if m.Gate == "human-review" {
		if reviewErr != nil || !reviewed.Equal(t.Review.ReviewedAt) || candidate.Review.Reviewer != t.Review.ReviewerIdentity || candidate.Review.Decision != "accept-evidence" {
			return snapshot, errors.New("review does not bind candidate human decision")
		}
	}
	if t.External.ObservationID != "" && (t.External.Controller != t.CurrentController || t.External.ExecutionGeneration != m.Context.ExecutionGeneration || t.Registry.ExecutionGeneration != m.Context.ExecutionGeneration || t.External.Audience != m.Context.Audience || t.External.Workspace != m.Context.Workspace || t.External.BuildIdentitySHA256 != t.CurrentBuildIdentitySHA256 || t.External.ConfigIdentitySHA256 != snapshot.EffectiveConfigIdentitySHA256) {
		return snapshot, errors.New("external host or generation differs from context")
	}
	return snapshot, nil
}

func authorizePermission(ctx context.Context, t TrustedInputs, b Binding, s qualification.Snapshot) (qualification.ActionAuthorization, qualification.CompatibilityRequest, error) {
	scope := t.PermissionScope
	releaseSHA, err := qualification.ReleaseRequestIdentitySHA(qualification.ReleaseRequest{Snapshot: s, Build: t.Build})
	if err != nil || scope.Formula.Name != "retirement-source-read/"+b.Gate || scope.Formula.ContentSHA256 != b.RequestSHA256 || scope.Formula.SourceSHA256 != b.ReleaseBundleSHA256 || scope.Formula.CompiledSHA256 != b.CandidateManifestSHA256 || scope.EffectiveConfigSHA256 != s.EffectiveConfigIdentitySHA256 || scope.ReleaseRequestSHA256 != releaseSHA {
		return qualification.ActionAuthorization{}, qualification.CompatibilityRequest{}, errors.New("permission does not bind exact source read")
	}
	a, r, err := qualification.AuthorizeCompatibility(ctx, t.PermissionAuthority, scope, t.PermissionProver)
	if err != nil {
		return a, r, err
	}
	if a.Status == qualification.StatusDenied {
		return a, r, qualification.ErrCompatibilityDenied
	}
	if a.Status != qualification.StatusAuthorized {
		return a, r, errors.New("source read permission unavailable")
	}
	return a, r, qualification.RevalidateCompatibility(ctx, t.PermissionAuthority, r, a)
}

func verifyCanonical(ctx context.Context, t TrustedInputs, b Binding, raw []byte, snapshot qualification.Snapshot, now time.Time) (Verdict, error) {
	v := unavailable(b, "gate_contract_unavailable")
	verified := func(reason string) Verdict { return Verdict{"verified", b.RequestSHA256, t.Assurance, reason} }
	switch b.Gate {
	case "human-review", "trial-ledger", "external-writers", "in-flight-resolution":
		w := t.Observation
		if t.Attestations == nil || w.CandidateManifestSHA256 != b.CandidateManifestSHA256 || w.ConfigIdentitySHA256 != snapshot.EffectiveConfigIdentitySHA256 {
			return unavailable(b, "observation_expectation_unavailable"), nil
		}
		token := t.ObservationToken
		if b.Gate == "trial-ledger" {
			token = string(raw)
		}
		o, err := t.Attestations.VerifyObservation(ctx, token, w)
		if err != nil {
			return unavailable(b, "observation_verification_failed"), err
		}
		if !o.Valid() || o.SequenceCompletenessResult != "complete" || o.CoverageResult != "complete" || o.ObservationEndedAt.After(now) {
			return unavailable(b, "observation_not_complete"), nil
		}
		if b.Gate == "trial-ledger" {
			return unavailable(b, "signed_observation_verified_useful_acceptance_trial_contract_unavailable"), nil
		}
		if b.Gate == "human-review" {
			want := t.Review
			if t.Policy == nil || want.CandidateManifestSHA256 != b.CandidateManifestSHA256 || want.ObservationRecordSHA256 != o.SignedRecordSHA256 || want.Audience != o.Audience || want.Workspace != o.Workspace || want.Decision != selectorattestation.DecisionApproved || want.ReviewedAt.Before(o.ObservationEndedAt) || want.ReviewedAt.After(now) || want.ReviewedAt.Sub(o.ObservationStartedAt) > t.Policy.MaximumTrialToReview {
				return unavailable(b, "review_expectation_mismatch"), nil
			}
			review, err := t.Attestations.VerifyReview(ctx, string(raw), want)
			if err != nil {
				return unavailable(b, "review_verification_failed"), err
			}
			if !review.Valid() {
				return unavailable(b, "review_seal_invalid"), nil
			}
			return verified("exact_signed_review_verified"), nil
		}
		expected := t.External
		expected.Attestation = o
		expected.TrustedNow = now.UTC()
		if t.Writers == nil || expected.CandidateManifestSHA256 != b.CandidateManifestSHA256 || expected.ConfigIdentitySHA256 != snapshot.EffectiveConfigIdentitySHA256 {
			return unavailable(b, "external_writer_expectation_unavailable"), nil
		}
		joined := t.Writers.VerifyAndJoin(ctx, raw, expected, t.Registry, t.HostID, t.BootID)
		if joined.Status != selectorinventory.StatusAvailable || joined.Evidence == nil || !joined.Evidence.Valid() {
			return unavailable(b, "external_writer_join_unavailable:"+joined.IssueCode), nil
		}
		if b.Gate == "in-flight-resolution" {
			resolution := selectorinventory.DigestInFlightResolution(t.Registry, expected.ExecutionGeneration, expected.ResolutionKey)
			if resolution.Status != selectorinventory.StatusAvailable || resolution.SHA256 != joined.Evidence.InFlightResolutionDigestSHA256() {
				return unavailable(b, "joined_in_flight_resolution_mismatch"), nil
			}
			return verified("signed_fenced_in_flight_resolution_join_verified"), nil
		}
		return verified("signed_fenced_external_ledger_join_verified"), nil
	case "compatibility":
		scope := t.CompatibilityScope
		releaseSHA, _ := qualification.ReleaseRequestIdentitySHA(qualification.ReleaseRequest{Snapshot: snapshot, Build: t.Build})
		if scope.EffectiveConfigSHA256 != snapshot.EffectiveConfigIdentitySHA256 || scope.ReleaseRequestSHA256 != releaseSHA {
			return unavailable(b, "compatibility_loaded_scope_mismatch"), nil
		}
		a, r, err := qualification.AuthorizeCompatibility(ctx, t.CompatibilityAuthority, scope, t.CapabilityProver)
		if err != nil {
			return unavailable(b, "compatibility_unavailable"), err
		}
		if a.Status != qualification.StatusAuthorized {
			return unavailable(b, "compatibility_not_authorized"), nil
		}
		if err := qualification.RevalidateCompatibility(ctx, t.CompatibilityAuthority, r, a); err != nil {
			return unavailable(b, "compatibility_revalidation_failed"), err
		}
		return verified("canonical_compatibility_verified"), nil
	case "release-authorization":
		a, err := qualification.Authorize(ctx, t.ReleaseAuthority, snapshot, t.Build)
		if err != nil {
			return unavailable(b, "release_authority_unavailable"), err
		}
		if a.Status != qualification.StatusAuthorized {
			return unavailable(b, "release_not_authorized"), nil
		}
		return unavailable(b, "canonical_release_verified_private_release_capability_contract_unavailable"), nil
	case "runtime-identities":
		return unavailable(b, "loaded_snapshot_verified_reviewed_backend_runtime_mapping_unavailable"), nil
	}
	return v, nil
}

func sameComposition(a, b TrustedInputs) bool {
	return reflect.DeepEqual(a.Policy, b.Policy) && reflect.DeepEqual(a.CurrentContext, b.CurrentContext) && a.CurrentController == b.CurrentController && a.CurrentBuildIdentitySHA256 == b.CurrentBuildIdentitySHA256 && a.Epoch == b.Epoch && a.Assurance == b.Assurance && a.HostID == b.HostID && a.BootID == b.BootID && reflect.DeepEqual(a.Build, b.Build) && reflect.DeepEqual(a.PermissionScope, b.PermissionScope) && reflect.DeepEqual(a.CompatibilityScope, b.CompatibilityScope) && reflect.DeepEqual(a.Observation, b.Observation) && a.ObservationToken == b.ObservationToken && reflect.DeepEqual(a.Review, b.Review) && reflect.DeepEqual(a.Registry, b.Registry) && reflect.DeepEqual(a.External, b.External)
}

func isolate(t TrustedInputs) TrustedInputs {
	if t.Policy != nil {
		policy := *t.Policy
		policy.Activation.RetiredScripts = slices.Clone(policy.Activation.RetiredScripts)
		t.Policy = &policy
	}
	if t.CurrentContext != nil {
		context := *t.CurrentContext
		context.Rigs = slices.Clone(context.Rigs)
		t.CurrentContext = &context
	}
	t.Closure.Roots = append([]qualification.InputRoot(nil), t.Closure.Roots...)
	t.PermissionScope.Packs = append([]qualification.CompatibilityPack(nil), t.PermissionScope.Packs...)
	t.CompatibilityScope.Packs = append([]qualification.CompatibilityPack(nil), t.CompatibilityScope.Packs...)
	t.Registry.Identities = slices.Clone(t.Registry.Identities)
	t.External.ExternalScope = append([]string(nil), t.External.ExternalScope...)
	t.External.RequiredResultAtoms = append([]string(nil), t.External.RequiredResultAtoms...)
	t.External.ResolutionKey = append([]byte(nil), t.External.ResolutionKey...)
	return t
}

func unavailable(b Binding, reason string) Verdict {
	return Verdict{"unavailable", b.RequestSHA256, "unavailable", reason}
}
