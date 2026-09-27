package qualification

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestNewSnapshotHashesEffectiveConfigAndLoadedInputsWithoutDisclosure(t *testing.T) {
	root := InputRoot{ID: "city", Kind: "city", PinStatus: "content", ResolvedPathSHA256: strings.Repeat("a", 64), InputsSHA256: strings.Repeat("b", 64), InputCount: 1}
	closure := InputClosure{
		SchemaVersion:  SchemaVersion,
		Status:         StatusAvailable,
		EnvironmentSHA: strings.Repeat("c", 64),
		Roots:          []InputRoot{root},
	}
	closure.SHA256, _ = InputClosureDigest(closure)
	snapshot, err := NewSnapshot(map[string]any{"credential": "secret-value", "enabled": true}, closure)
	if err != nil {
		t.Fatalf("NewSnapshot: %v", err)
	}
	if snapshot.Status != StatusAvailable || snapshot.EffectiveConfigIdentitySHA256 == "" {
		t.Fatalf("snapshot = %#v, want available identity", snapshot)
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "secret-value") {
		t.Fatalf("snapshot disclosed effective config value: %s", encoded)
	}
}

func TestNewSnapshotKeepsPartialClosureUnavailable(t *testing.T) {
	closureDigest, err := DigestJSON([]InputRoot{{ID: "external:one", Kind: "external", PinStatus: "unbound", UnavailableReason: "unbound_external_input"}})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := NewSnapshot(map[string]string{"name": "city"}, InputClosure{
		SchemaVersion: SchemaVersion,
		Status:        StatusUnavailable,
		Reason:        "unbound_external_input",
		SHA256:        closureDigest,
		Roots:         []InputRoot{{ID: "external:one", Kind: "external", PinStatus: "unbound", UnavailableReason: "unbound_external_input"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Status != StatusUnavailable || snapshot.EffectiveConfigInputClosureSHA256 != closureDigest || snapshot.EffectiveConfigIdentitySHA256 != "" {
		t.Fatalf("snapshot = %#v, want partial closure hash and no identity", snapshot)
	}
}

func TestAuthorizeDefaultsToUnavailableAndRejectsWrongScope(t *testing.T) {
	closure := InputClosure{
		SchemaVersion:  SchemaVersion,
		Status:         StatusAvailable,
		EnvironmentSHA: strings.Repeat("c", 64),
		Roots: []InputRoot{{
			ID: "city", Kind: "city", PinStatus: "content",
			ResolvedPathSHA256: strings.Repeat("a", 64),
			InputsSHA256:       strings.Repeat("b", 64), InputCount: 1,
		}},
	}
	closure.SHA256, _ = InputClosureDigest(closure)
	snapshot, err := NewSnapshot(map[string]string{"name": "city"}, closure)
	if err != nil {
		t.Fatal(err)
	}
	build := BuildIdentity{
		Status: StatusAvailable, SourceRevision: strings.Repeat("1", 40),
		BuildID: strings.Repeat("1", 40), Version: "1.0.0",
		ArtifactStatus: StatusAvailable, ArtifactSHA256: strings.Repeat("2", 64),
	}
	decision, err := Authorize(context.Background(), nil, snapshot, build)
	if !errors.Is(err, ErrUnavailable) || decision.Status != StatusUnavailable || decision.Reason != "release_authorizer_unconfigured" {
		t.Fatalf("Authorize(nil) = %#v, %v; want unavailable", decision, err)
	}
	if decision.ReleaseRequestSHA256 == "" {
		t.Fatalf("Authorize(nil) omitted release request identity: %#v", decision)
	}
	authorizer := fixedReleaseAuthorizer{decision: Authorization{Status: StatusAuthorized, IdentitySHA: strings.Repeat("f", 64)}}
	decision, err = Authorize(context.Background(), authorizer, snapshot, build)
	if err != nil || decision.Status != StatusDenied || decision.Reason != "release_scope_mismatch" {
		t.Fatalf("Authorize(wrong scope) = %#v, %v; want scope denial", decision, err)
	}

	for _, tc := range []struct {
		name       string
		build      BuildIdentity
		decision   Authorization
		wantReason string
	}{
		{
			name:       "dirty source",
			build:      BuildIdentity{Status: StatusAvailable, SourceRevision: strings.Repeat("1", 40), BuildID: strings.Repeat("1", 40), Version: "1.0.0", SourceDirty: true, ArtifactStatus: StatusAvailable, ArtifactSHA256: strings.Repeat("2", 64)},
			decision:   Authorization{Status: StatusAuthorized, IdentitySHA: snapshot.EffectiveConfigIdentitySHA256, RecordID: "release-1"},
			wantReason: "controller_build_dirty",
		},
		{
			name:       "malformed artifact digest",
			build:      BuildIdentity{Status: StatusAvailable, SourceRevision: strings.Repeat("1", 40), BuildID: strings.Repeat("1", 40), Version: "1.0.0", ArtifactStatus: StatusAvailable, ArtifactSHA256: strings.Repeat("2", 63)},
			decision:   Authorization{Status: StatusAuthorized, IdentitySHA: snapshot.EffectiveConfigIdentitySHA256, RecordID: "release-1"},
			wantReason: "controller_build_unavailable",
		},
		{
			name:       "missing release record id",
			build:      build,
			decision:   Authorization{Status: StatusAuthorized, IdentitySHA: snapshot.EffectiveConfigIdentitySHA256},
			wantReason: "release_record_identity_missing",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Authorize(context.Background(), requestBoundAuthorizer{decision: tc.decision, bind: tc.name == "missing release record id"}, snapshot, tc.build)
			if !errors.Is(err, ErrUnavailable) || got.Status != StatusUnavailable || got.Reason != tc.wantReason {
				t.Fatalf("Authorize = %#v, %v; want unavailable: %s", got, err, tc.wantReason)
			}
		})
	}

	buildB := build
	buildB.ArtifactSHA256 = strings.Repeat("3", 64)
	approvedBuildA, err := ReleaseRequestIdentitySHA(ReleaseRequest{Snapshot: snapshot, Build: build})
	if err != nil {
		t.Fatal(err)
	}
	decisionA := Authorization{
		Status: StatusAuthorized, IdentitySHA: snapshot.EffectiveConfigIdentitySHA256,
		ReleaseRequestSHA256: approvedBuildA, RecordID: "release-build-a",
	}
	got, err := Authorize(context.Background(), fixedReleaseAuthorizer{decision: decisionA}, snapshot, build)
	if err != nil || got.Status != StatusAuthorized {
		t.Fatalf("Authorize(original build) = %#v, %v; want authorized", got, err)
	}
	got, err = Authorize(context.Background(), fixedReleaseAuthorizer{decision: decisionA}, snapshot, buildB)
	if err != nil || got.Status != StatusDenied || got.Reason != "release_request_scope_mismatch" {
		t.Fatalf("Authorize(replayed build decision) = %#v, %v; want release request mismatch", got, err)
	}
}

func TestAuthorizeRejectsUnsupportedSnapshotSchemaBeforeAuthority(t *testing.T) {
	authorizer := &callCountingAuthorizer{}
	decision, err := Authorize(context.Background(), authorizer, Snapshot{
		SchemaVersion: SchemaVersion + 1,
		Status:        StatusAvailable,
	}, BuildIdentity{})
	if !errors.Is(err, ErrUnavailable) || decision.Status != StatusUnavailable || decision.Reason != "qualification_snapshot_schema_unsupported" {
		t.Fatalf("Authorize(unsupported schema) = %#v, %v; want schema-unavailable", decision, err)
	}
	if authorizer.calls != 0 {
		t.Fatalf("release authority called %d times for unsupported schema, want zero", authorizer.calls)
	}
}

type fixedReleaseAuthorizer struct {
	decision Authorization
}

func (a fixedReleaseAuthorizer) Authorize(_ context.Context, _ ReleaseRequest) (Authorization, error) {
	return a.decision, nil
}

type callCountingAuthorizer struct {
	calls int
}

func (a *callCountingAuthorizer) Authorize(_ context.Context, _ ReleaseRequest) (Authorization, error) {
	a.calls++
	return Authorization{Status: StatusAuthorized, RecordID: "release-1"}, nil
}

type requestBoundAuthorizer struct {
	decision Authorization
	bind     bool
}

func (a requestBoundAuthorizer) Authorize(_ context.Context, request ReleaseRequest) (Authorization, error) {
	decision := a.decision
	if a.bind {
		requestSHA, err := ReleaseRequestIdentitySHA(request)
		if err != nil {
			return Authorization{}, err
		}
		decision.ReleaseRequestSHA256 = requestSHA
	}
	return decision, nil
}
