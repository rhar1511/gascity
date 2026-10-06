package qualification

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestAuthorizeCompatibilityBindsPackScopePolicyAndRuntimeProofs(t *testing.T) {
	scope := validCompatibilityScope()
	policy := compatibilityPolicyFor(t, scope, "attempt-evidence-private-payload")
	authority := &fakeCompatibilityAuthority{policy: policy}
	prover := fakeCapabilityProver{evidence: map[string]string{
		"attempt-evidence-private-payload": strings.Repeat("d", 64),
	}}

	got, request, err := AuthorizeCompatibility(context.Background(), authority, scope, prover)
	if err != nil || got.Status != StatusAuthorized {
		t.Fatalf("AuthorizeCompatibility = %#v, %v; want authorized", got, err)
	}
	if request.Policy.PolicyReference != policy.PolicyReference || request.Scope.Packs[0].Pin != scope.Packs[0].Pin {
		t.Fatalf("request lost exact policy or pack scope: %#v", request)
	}
	if request.RequestSHA256 == "" || got.RequestSHA256 != request.RequestSHA256 {
		t.Fatalf("authorization/request digest mismatch: auth=%#v request=%#v", got, request)
	}
	if err := RevalidateCompatibility(context.Background(), authority, request, got); err != nil {
		t.Fatalf("RevalidateCompatibility: %v", err)
	}
	if authority.verifyCalls != 1 {
		t.Fatalf("Verify calls = %d, want 1", authority.verifyCalls)
	}
}

func TestAuthorizeCompatibilityFailsClosedWithoutAuthorityOrCapabilityProof(t *testing.T) {
	scope := validCompatibilityScope()
	policy := compatibilityPolicyFor(t, scope, "attempt-evidence-private-payload")

	if got, _, err := AuthorizeCompatibility(context.Background(), nil, scope, fakeCapabilityProver{}); !errors.Is(err, ErrUnavailable) || got.Status != StatusUnavailable {
		t.Fatalf("nil authority = %#v, %v; want unavailable", got, err)
	}

	authority := &fakeCompatibilityAuthority{policy: policy}
	got, _, err := AuthorizeCompatibility(context.Background(), authority, scope, fakeCapabilityProver{})
	if !errors.Is(err, ErrUnavailable) || got.Status != StatusUnavailable || authority.authorizeCalls != 0 {
		t.Fatalf("missing runtime proof = %#v, %v (authorize calls %d), want unavailable before authority action", got, err, authority.authorizeCalls)
	}

	unknown := &fakeCompatibilityAuthority{policy: compatibilityPolicyFor(t, scope, "unrecognized-capability")}
	got, _, err = AuthorizeCompatibility(context.Background(), unknown, scope, fakeCapabilityProver{})
	if !errors.Is(err, ErrUnavailable) || got.Status != StatusUnavailable || unknown.authorizeCalls != 0 {
		t.Fatalf("unknown capability = %#v, %v (authorize calls %d), want unavailable", got, err, unknown.authorizeCalls)
	}
}

func TestAuthorizeCompatibilityRejectsWrongScopeAndAuthorizationReplay(t *testing.T) {
	scope := validCompatibilityScope()
	policy := compatibilityPolicyFor(t, scope, "attempt-evidence-private-payload")
	prover := fakeCapabilityProver{evidence: map[string]string{"attempt-evidence-private-payload": strings.Repeat("d", 64)}}

	wrongPolicy := policy
	wrongPolicy.ScopeSHA256 = strings.Repeat("e", 64)
	authority := &fakeCompatibilityAuthority{policy: wrongPolicy}
	got, _, err := AuthorizeCompatibility(context.Background(), authority, scope, prover)
	if !errors.Is(err, ErrUnavailable) || got.Status != StatusUnavailable || authority.authorizeCalls != 0 {
		t.Fatalf("wrong-scope policy = %#v, %v (authorize calls %d), want unavailable", got, err, authority.authorizeCalls)
	}

	authority = &fakeCompatibilityAuthority{policy: policy, replayRequestSHA: strings.Repeat("f", 64)}
	got, _, err = AuthorizeCompatibility(context.Background(), authority, scope, prover)
	if err != nil || got.Status != StatusDenied || got.Reason != "compatibility_authorization_scope_mismatch" {
		t.Fatalf("replayed action authorization = %#v, %v; want request-scope denial", got, err)
	}
}

func TestCompatibilityRequestIdentityChangesWithExactPackAndBackendEvidence(t *testing.T) {
	scope := validCompatibilityScope()
	policy := compatibilityPolicyFor(t, scope, "attempt-evidence-private-payload")
	prover := fakeCapabilityProver{evidence: map[string]string{"attempt-evidence-private-payload": strings.Repeat("d", 64)}}
	_, first, err := AuthorizeCompatibility(context.Background(), &fakeCompatibilityAuthority{policy: policy}, scope, prover)
	if err != nil {
		t.Fatal(err)
	}

	changedPack := validCompatibilityScope()
	changedPack.Packs[0].Pin = strings.Repeat("b", 40)
	changedPackPolicy := compatibilityPolicyFor(t, changedPack, "attempt-evidence-private-payload")
	_, second, err := AuthorizeCompatibility(context.Background(), &fakeCompatibilityAuthority{policy: changedPackPolicy}, changedPack, prover)
	if err != nil {
		t.Fatal(err)
	}
	if first.RequestSHA256 == second.RequestSHA256 {
		t.Fatal("changing the exact pack pin did not change request identity")
	}

	changedBackend := fakeCapabilityProver{evidence: map[string]string{"attempt-evidence-private-payload": strings.Repeat("c", 64)}}
	_, third, err := AuthorizeCompatibility(context.Background(), &fakeCompatibilityAuthority{policy: policy}, scope, changedBackend)
	if err != nil {
		t.Fatal(err)
	}
	if first.RequestSHA256 == third.RequestSHA256 {
		t.Fatal("changing runtime/backend evidence did not change request identity")
	}

	changedCompiledRecipe := validCompatibilityScope()
	changedCompiledRecipe.Formula.CompiledSHA256 = strings.Repeat("e", 64)
	changedCompiledPolicy := compatibilityPolicyFor(t, changedCompiledRecipe, "attempt-evidence-private-payload")
	_, fourth, err := AuthorizeCompatibility(context.Background(), &fakeCompatibilityAuthority{policy: changedCompiledPolicy}, changedCompiledRecipe, prover)
	if err != nil {
		t.Fatal(err)
	}
	if first.RequestSHA256 == fourth.RequestSHA256 {
		t.Fatal("changing compiled formula action did not change request identity")
	}
}

func TestRevalidateCompatibilityRejectsChangedPolicyOrRequest(t *testing.T) {
	scope := validCompatibilityScope()
	policy := compatibilityPolicyFor(t, scope, "attempt-evidence-private-payload")
	prover := fakeCapabilityProver{evidence: map[string]string{"attempt-evidence-private-payload": strings.Repeat("d", 64)}}
	authority := &fakeCompatibilityAuthority{policy: policy}
	authorization, request, err := AuthorizeCompatibility(context.Background(), authority, scope, prover)
	if err != nil {
		t.Fatal(err)
	}
	request.Scope.CityID = "another-city"
	if err := RevalidateCompatibility(context.Background(), authority, request, authorization); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("RevalidateCompatibility(changed request) = %v, want unavailable", err)
	}
}

func validCompatibilityScope() CompatibilityScope {
	return CompatibilityScope{
		SchemaVersion: SchemaVersion,
		CityID:        "city-alpha", ServerID: "server-1", StoreRef: "city:city-alpha",
		EffectiveConfigSHA256: strings.Repeat("9", 64),
		ReleaseRequestSHA256:  strings.Repeat("a", 64),
		Formula: CompatibilityFormula{
			Name: "review", ContentSHA256: strings.Repeat("b", 64), SourceSHA256: strings.Repeat("c", 64), CompiledSHA256: strings.Repeat("d", 64),
		},
		Packs: []CompatibilityPack{{
			Name: "trusted-pack", RootID: "pack:root-one", Pin: strings.Repeat("1", 40), PinStatus: "locked",
			ManifestSHA256: strings.Repeat("2", 64), SourceSubpathSHA256: strings.Repeat("3", 64), RequiresGC: ">=1.0.0",
		}},
	}
}

func compatibilityPolicyFor(t *testing.T, scope CompatibilityScope, requirements ...string) CompatibilityPolicy {
	t.Helper()
	digest, err := CompatibilityScopeIdentitySHA(scope)
	if err != nil {
		t.Fatal(err)
	}
	return CompatibilityPolicy{
		Status: StatusAvailable, ScopeSHA256: digest,
		PolicyReference: "release-record:revision-12", PolicyVersion: "policy-v3",
		RequiredCapabilities: requirements,
	}
}

type fakeCapabilityProver struct{ evidence map[string]string }

func (p fakeCapabilityProver) Prove(_ context.Context, _ CompatibilityScope, policy CompatibilityPolicy, capability string) (CapabilityProof, error) {
	evidence := p.evidence[capability]
	if evidence == "" {
		return CapabilityProof{}, ErrUnavailable
	}
	return CapabilityProof{
		Status: StatusAvailable, Capability: capability, ScopeSHA256: policy.ScopeSHA256,
		PolicyReference: policy.PolicyReference, PolicyVersion: policy.PolicyVersion,
		EvidenceSHA256: evidence,
	}, nil
}

type fakeCompatibilityAuthority struct {
	policy           CompatibilityPolicy
	replayRequestSHA string
	resolveCalls     int
	authorizeCalls   int
	verifyCalls      int
}

func (a *fakeCompatibilityAuthority) Resolve(_ context.Context, _ CompatibilityScope) (CompatibilityPolicy, error) {
	a.resolveCalls++
	return a.policy, nil
}

func (a *fakeCompatibilityAuthority) Authorize(_ context.Context, request CompatibilityRequest) (ActionAuthorization, error) {
	a.authorizeCalls++
	requestSHA := request.RequestSHA256
	if a.replayRequestSHA != "" {
		requestSHA = a.replayRequestSHA
	}
	return ActionAuthorization{
		Status: StatusAuthorized, PolicyReference: request.Policy.PolicyReference,
		PolicyVersion: request.Policy.PolicyVersion, RequestSHA256: requestSHA,
		RecordID: "authorization:action-7", Reference: "signed:action-7",
	}, nil
}

func (a *fakeCompatibilityAuthority) Verify(_ context.Context, request CompatibilityRequest, authorization ActionAuthorization) error {
	a.verifyCalls++
	if authorization.RequestSHA256 != request.RequestSHA256 || authorization.PolicyReference != request.Policy.PolicyReference || authorization.PolicyVersion != request.Policy.PolicyVersion {
		return ErrUnavailable
	}
	return nil
}
