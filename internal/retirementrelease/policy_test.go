package retirementrelease

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/selectorattestation"
)

func privateProtocolPolicy() *ProtocolPolicy {
	return &ProtocolPolicy{
		Reference: "configured-private-retirement-protocol", Version: "1",
		Activation: ActivationScope{Pack: "self-healing-rsi", Workflow: "private-matt-mayor", Session: "mayor", RetiredScripts: []string{
			"roles/wip-dispatch/label_ready.sh", "roles/wip-dispatch/dispatch_once.sh", "roles/wip-dispatch/sweep_orphans.sh",
		}},
		MinimumTrialDuration: 48 * time.Hour, MaximumTrialToReview: 7 * 24 * time.Hour, MaximumContextAge: 5 * time.Minute,
	}
}

func TestValidSignedObservationsCannotSelectAnotherContext(t *testing.T) {
	for _, field := range []string{"audience", "workspace", "runtime", "build", "config", "controller"} {
		t.Run(field, func(t *testing.T) {
			f := newSignedFixture(t)
			payload, err := base64.RawURLEncoding.DecodeString(strings.Split(f.inputs.ObservationToken, ".")[0])
			if err != nil {
				t.Fatal(err)
			}
			var claims selectorattestation.ObservationClaims
			if err := json.Unmarshal(payload, &claims); err != nil {
				t.Fatal(err)
			}
			other := strings.Repeat("c", 64)
			switch field {
			case "audience":
				claims.Audience = "another-audience"
				f.inputs.Observation.Audience = claims.Audience
			case "workspace":
				claims.Workspace = "another-workspace"
				f.inputs.Observation.Workspace = claims.Workspace
			case "runtime":
				claims.RuntimeIdentitySHA256 = other
				f.inputs.Observation.RuntimeIdentitySHA256 = other
			case "build":
				claims.BuildIdentitySHA256 = other
				f.inputs.Observation.BuildIdentitySHA256 = other
			case "config":
				claims.ConfigIdentitySHA256 = other
				f.inputs.Observation.ConfigIdentitySHA256 = other
			case "controller":
				claims.SelectorSnapshotSHA256 = other
				f.inputs.Observation.SelectorSnapshotSHA256 = other
			}
			signing, err := selectorattestation.ObservationSigningBytes(claims)
			if err != nil {
				t.Fatal(err)
			}
			f.inputs.ObservationToken = signToken(signing, ed25519.NewKeyFromSeed(bytesSeed(1)))
			retarget(t, &f, "trial-ledger", []byte(f.inputs.ObservationToken))
			a := NewAdapter(Options{Enabled: true, Now: func() time.Time { return f.now }, Compose: func(context.Context, Binding) (TrustedInputs, error) { return f.inputs, nil }})
			v, _ := a.Verify(context.Background(), f.request)
			if v.Status != "unavailable" || f.keys.loads != 0 {
				t.Fatalf("valid token and matching expectation selected %s outside context: %+v", field, v)
			}
		})
	}
}

func TestProtocolBoundsAreConfiguredRatherThanDefaulted(t *testing.T) {
	f := newSignedFixture(t)
	f.inputs.Policy.MinimumTrialDuration = 30 * time.Hour
	f.inputs.Policy.MaximumTrialToReview = 50 * time.Hour
	a := NewAdapter(Options{Enabled: true, Now: func() time.Time { return f.now }, Compose: func(context.Context, Binding) (TrustedInputs, error) { return f.inputs, nil }})
	v, err := a.Verify(context.Background(), f.request)
	if err != nil || v.Status != "verified" || v.Assurance != "fixture" {
		t.Fatalf("explicit configured bounds not respected: %+v %v", v, err)
	}
}

func TestConfiguredProtocolPolicyRejectsExactScopeAndBoundChanges(t *testing.T) {
	for _, name := range []string{"missing-policy", "invalid-policy", "pack", "workflow", "session", "scripts", "extra-script", "trial-bound", "review-bound", "policy-changed", "policy-slice-mutated"} {
		t.Run(name, func(t *testing.T) {
			f := newSignedFixture(t)
			switch name {
			case "missing-policy":
				f.inputs.Policy = nil
			case "invalid-policy":
				f.inputs.Policy.MinimumTrialDuration = 0
			case "trial-bound":
				f.inputs.Policy.MinimumTrialDuration = 49 * time.Hour
			case "review-bound":
				f.inputs.Policy.MaximumTrialToReview = 48 * time.Hour
			case "pack", "workflow", "session", "scripts", "extra-script":
				var m manifest
				if err := json.Unmarshal([]byte(f.request.ManifestJSON), &m); err != nil {
					t.Fatal(err)
				}
				var activation map[string]any
				if err := json.Unmarshal(m.Activation, &activation); err != nil {
					t.Fatal(err)
				}
				switch name {
				case "pack", "workflow":
					activation[name] = "wrong-target"
				case "session":
					activation["mayor_session"] = "wrong-session"
				case "scripts":
					activation["retired_scripts"] = []string{"one", "two", "three"}
				case "extra-script":
					activation["retired_scripts"] = append(append([]string(nil), f.inputs.Policy.Activation.RetiredScripts...), "extra.sh")
				}
				m.Activation = json.RawMessage(canonical(t, activation))
				f.request.ManifestJSON = canonical(t, m)
				f.request.RequestSHA256 = jsonDigest(t, m)
				f.inputs.ExpectedManifestJSON = f.request.ManifestJSON
				f.inputs.PermissionScope.Formula.ContentSHA256 = f.request.RequestSHA256
			}
			calls := 0
			a := NewAdapter(Options{Enabled: true, Now: func() time.Time { return f.now }, Compose: func(context.Context, Binding) (TrustedInputs, error) {
				calls++
				if calls == 2 && name == "policy-changed" {
					f.inputs.Policy.Version = "2"
				}
				if calls == 2 && name == "policy-slice-mutated" {
					f.inputs.Policy.Activation.RetiredScripts[0] = "other.sh"
				}
				return f.inputs, nil
			}})
			v, _ := a.Verify(context.Background(), f.request)
			if v.Status != "unavailable" || v.Assurance != "unavailable" {
				t.Fatalf("%s accepted: %+v", name, v)
			}
			if name == "missing-policy" && v.Reason != "protocol_policy_unavailable" {
				t.Fatalf("missing policy reason: %+v", v)
			}
		})
	}
}

func TestIndependentContextRejectsVerifierExpectationMismatches(t *testing.T) {
	for _, field := range []string{"missing-context", "audience", "workspace", "host", "boot", "controller-generation", "execution-generation", "backend-commit", "backend-binary", "backend-identity", "runtime-identity", "pack-commit", "observation-build", "observation-controller"} {
		t.Run(field, func(t *testing.T) {
			f := newSignedFixture(t)
			switch field {
			case "missing-context":
				f.inputs.CurrentContext = nil
			case "audience":
				f.inputs.CurrentContext.Audience = "other"
			case "workspace":
				f.inputs.CurrentContext.Workspace = "other"
			case "host":
				f.inputs.HostID = "other-host"
			case "boot":
				f.inputs.BootID = "other-boot"
			case "controller-generation":
				f.inputs.CurrentController.Generation++
			case "execution-generation":
				f.inputs.CurrentContext.ExecutionGeneration = "other"
			case "backend-commit":
				f.inputs.CurrentContext.Source.BackendSourceCommit = strings.Repeat("c", 40)
			case "backend-binary":
				f.inputs.CurrentContext.Source.BackendBinarySHA256 = strings.Repeat("c", 64)
			case "backend-identity":
				f.inputs.CurrentContext.Source.BackendIdentitySHA256 = strings.Repeat("c", 64)
			case "runtime-identity":
				f.inputs.CurrentContext.Source.RuntimeIdentitySHA256 = strings.Repeat("c", 64)
			case "pack-commit":
				f.inputs.CurrentContext.Source.PackSourceCommit = strings.Repeat("c", 40)
			case "observation-build":
				f.inputs.CurrentBuildIdentitySHA256 = strings.Repeat("c", 64)
			case "observation-controller":
				f.inputs.CurrentController.SnapshotSHA256 = strings.Repeat("c", 64)
			}
			a := NewAdapter(Options{Enabled: true, Now: func() time.Time { return f.now }, Compose: func(context.Context, Binding) (TrustedInputs, error) { return f.inputs, nil }})
			v, _ := a.Verify(context.Background(), f.request)
			if v.Status != "unavailable" || f.keys.loads != 0 {
				t.Fatalf("%s trusted despite valid signed token: %+v authority loads=%d", field, v, f.keys.loads)
			}
		})
	}
}
