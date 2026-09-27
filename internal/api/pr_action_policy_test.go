package api

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestResolvePRActionPolicyRequiresExactHumanPolicySigner(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	verifier := policyTestHumanVerifier(t, pub, []string{PRActionScopePolicyWrite})
	policy := PRActionPolicyDocument{Monitors: []PRActionPolicyMonitor{{
		Name: "pilot", Owner: "Acme", Repo: "Widget", Rig: "myrig",
		BaseBranches: []string{"Release"}, RepairRoute: "myrig/worker", RequiredChecks: []string{"ci"},
	}}}
	raw := signTestPRPolicy(t, priv, "test-city", "human", "governance", "ricky", policy)
	resolved, err := ResolvePRActionPolicy(raw, "test-city", verifier)
	if err != nil {
		t.Fatalf("ResolvePRActionPolicy: %v", err)
	}
	if resolved == nil || resolved.City != "test-city" || !strings.HasPrefix(resolved.Version, "pr-actions-") || len(resolved.Monitors) != 1 {
		t.Fatalf("resolved policy = %+v", resolved)
	}
	if resolved.Monitors[0].Owner != "acme" || resolved.Monitors[0].Repo != "widget" || resolved.Monitors[0].BaseBranches[0] != "Release" || resolved.Monitors[0].RequiredChecks[0] != "ci" {
		t.Fatalf("policy was not normalized before activation: %+v", resolved.Monitors[0])
	}

	if absent, err := ResolvePRActionPolicy("", "test-city", verifier); err != nil || absent != nil {
		t.Fatalf("absent policy = %v, %v; want disabled", absent, err)
	}
	if otherCity, err := ResolvePRActionPolicy(raw, "other-city", verifier); err != nil || otherCity != nil {
		t.Fatalf("policy for another city = %v, %v; want disabled", otherCity, err)
	}
}

func TestResolvePRActionPolicyRejectsChangedContentAndUnknownAuthority(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	policy := PRActionPolicyDocument{Monitors: []PRActionPolicyMonitor{{
		Name: "pilot", Owner: "acme", Repo: "widget", Rig: "myrig",
		BaseBranches: []string{"main"}, RepairRoute: "myrig/worker", RequiredChecks: []string{"ci"},
	}}}
	raw := signTestPRPolicy(t, priv, "test-city", "human", "governance", "ricky", policy)
	var configured prActionPolicySet
	if err := json.Unmarshal([]byte(raw), &configured); err != nil {
		t.Fatal(err)
	}
	envelope := configured.Policies["test-city"]
	envelope.Policy.Monitors[0].BaseBranches = []string{"release"}
	configured.Policies["test-city"] = envelope
	tampered, err := json.Marshal(configured)
	if err != nil {
		t.Fatal(err)
	}
	verifier := policyTestHumanVerifier(t, pub, []string{PRActionScopePolicyWrite})
	if _, err := ResolvePRActionPolicy(string(tampered), "test-city", verifier); !errors.Is(err, ErrPRActionPolicyInvalid) {
		t.Fatalf("tampered policy error = %v, want invalid", err)
	}
	wrongAuthority := policyTestHumanVerifier(t, pub, []string{PRActionScopeMerge})
	if _, err := ResolvePRActionPolicy(raw, "test-city", wrongAuthority); !errors.Is(err, ErrPRActionPolicyInvalid) {
		t.Fatalf("wrong authority error = %v, want invalid", err)
	}
}

func TestPRActionPolicySigningRejectsUnboundedMergeCheckPolicy(t *testing.T) {
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	policy := PRActionPolicyDocument{Monitors: []PRActionPolicyMonitor{{
		Name: "pilot", Owner: "acme", Repo: "widget", Rig: "myrig",
		BaseBranches: []string{"main"}, RepairRoute: "myrig/worker",
	}}}
	if _, err := PRActionPolicySigningBytes("test-city", policy); err == nil {
		t.Fatal("policy without required checks was signable")
	}
	_ = priv
}

func policyTestHumanVerifier(t *testing.T, public ed25519.PublicKey, scopes []string) *PRHumanGrantVerifier {
	t.Helper()
	verifier, err := NewPRHumanGrantVerifier(PRHumanTrustConfig{
		Keys:        []PRHumanGrantKey{{KeyID: "human", PublicKey: base64.StdEncoding.EncodeToString(public)}},
		Authorities: []PRHumanAuthority{{KeyID: "human", Issuer: "governance", Subject: "ricky", Scopes: scopes}},
	}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return verifier
}

func signTestPRPolicy(t *testing.T, private ed25519.PrivateKey, city, keyID, issuer, subject string, policy PRActionPolicyDocument) string {
	t.Helper()
	signingBytes, err := PRActionPolicySigningBytes(city, policy)
	if err != nil {
		t.Fatal(err)
	}
	envelope := prActionPolicyEnvelope{
		City: city, KeyID: keyID, Issuer: issuer, Subject: subject, Policy: policy,
		Signature: base64.RawURLEncoding.EncodeToString(ed25519.Sign(private, signingBytes)),
	}
	configured, err := json.Marshal(prActionPolicySet{Policies: map[string]prActionPolicyEnvelope{city: envelope}})
	if err != nil {
		t.Fatal(err)
	}
	return string(configured)
}
