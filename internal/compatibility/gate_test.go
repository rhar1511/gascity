package compatibility

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/formula"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/qualification"
)

func TestGateRequiresTrustedApprovalBeforeRequiredFormulaAction(t *testing.T) {
	cfg, recipe, snapshot := requiredPackFixture(t)
	store, err := beads.OpenFileStore(fsys.OSFS{}, filepath.Join(t.TempDir(), "beads.json"))
	if err != nil {
		t.Fatal(err)
	}
	authority := &fakeCompatibilityAuthority{}
	gate := testGate(snapshot, authority)

	result, err := gate.AuthorizeFormula(context.Background(), cfg, recipe, store)
	if err != nil || !result.Required || result.Authorization.Status != qualification.StatusAuthorized {
		t.Fatalf("AuthorizeFormula = %#v, %v; want required authorized formula", result, err)
	}
	if authority.resolveCalls != 1 || authority.authorizeCalls != 1 {
		t.Fatalf("authority calls = resolve:%d authorize:%d, want one each", authority.resolveCalls, authority.authorizeCalls)
	}
	if err := gate.RevalidateFormula(context.Background(), cfg, recipe, store, result); err != nil {
		t.Fatalf("RevalidateFormula: %v", err)
	}
	if authority.verifyCalls != 1 {
		t.Fatalf("authority Verify calls = %d, want 1", authority.verifyCalls)
	}
}

func TestMaterializationGatePersistsAndRevalidatesExactDecision(t *testing.T) {
	cfg, recipe, snapshot := requiredPackFixture(t)
	store, err := beads.OpenFileStore(fsys.OSFS{}, filepath.Join(t.TempDir(), "beads.json"))
	if err != nil {
		t.Fatal(err)
	}
	authority := &fakeCompatibilityAuthority{}
	gate := MaterializationGate{Gate: testGate(snapshot, authority), Config: cfg}
	metadata, err := gate.AuthorizeRecipe(context.Background(), recipe, store)
	if err != nil || !metadata.Required {
		t.Fatalf("AuthorizeRecipe = %#v, %v; want required authorization", metadata, err)
	}
	if err := gate.RevalidateActionMetadata(context.Background(), metadata.RequestJSON, metadata.AuthorizationJSON, store); err != nil {
		t.Fatalf("RevalidateActionMetadata: %v", err)
	}
	if authority.verifyCalls != 1 {
		t.Fatalf("authority Verify calls = %d, want 1", authority.verifyCalls)
	}
}

func TestMaterializationGateRejectsAuthorizationAfterLoadedConfigChanges(t *testing.T) {
	cfg, recipe, snapshot := requiredPackFixture(t)
	store, err := beads.OpenFileStore(fsys.OSFS{}, filepath.Join(t.TempDir(), "beads.json"))
	if err != nil {
		t.Fatal(err)
	}
	authority := &fakeCompatibilityAuthority{}
	gate := MaterializationGate{Gate: testGate(snapshot, authority), Config: cfg}
	metadata, err := gate.AuthorizeRecipe(context.Background(), recipe, store)
	if err != nil {
		t.Fatal(err)
	}
	// A config reload publishes a new snapshot. The old authorization must not
	// remain valid just because the old pack source path is still on disk.
	changed := *cfg
	changed.Workspace.Name = "city-beta"
	config.RefreshQualificationSnapshot(&changed, nil)
	gate.Config = &changed
	if err := gate.RevalidateActionMetadata(context.Background(), metadata.RequestJSON, metadata.AuthorizationJSON, store); !errors.Is(err, qualification.ErrUnavailable) {
		t.Fatalf("RevalidateActionMetadata after config change = %v, want unavailable", err)
	}
}

func TestMaterializationGateUsesCurrentIdentityDuringRevalidation(t *testing.T) {
	cfg, recipe, snapshot := requiredPackFixture(t)
	store, err := beads.OpenFileStore(fsys.OSFS{}, filepath.Join(t.TempDir(), "beads.json"))
	if err != nil {
		t.Fatal(err)
	}
	authority := &fakeCompatibilityAuthority{}
	currentConfig := cfg
	currentSnapshot := snapshot
	currentBuild := testGate(snapshot, authority).Build
	currentAuthority := qualification.CompatibilityAuthority(authority)
	currentCalls := 0
	gate := MaterializationGate{
		Gate:   testGate(snapshot, authority),
		Config: cfg,
		Current: func() (*config.City, qualification.Snapshot, qualification.BuildIdentity, qualification.CompatibilityAuthority, error) {
			currentCalls++
			return currentConfig, currentSnapshot, currentBuild, currentAuthority, nil
		},
	}
	active, _, err := gate.activeGate()
	if err != nil || active.Authority != currentAuthority {
		t.Fatalf("activeGate authority = %T, %v; want current authority %T", active.Authority, err, currentAuthority)
	}
	metadata, err := gate.AuthorizeRecipe(context.Background(), recipe, store)
	if err != nil || !metadata.Required {
		t.Fatalf("AuthorizeRecipe = %#v, %v; want required authorization", metadata, err)
	}

	changed := *cfg
	changed.Workspace.Name = "city-beta"
	config.RefreshQualificationSnapshot(&changed, nil)
	currentConfig = &changed
	currentSnapshot = changed.QualificationSnapshot()
	replacementAuthority := &fakeCompatibilityAuthority{}
	currentAuthority = replacementAuthority
	currentBuild = qualification.BuildIdentity{BuildID: "replacement-build"}
	active, _, err = gate.activeGate()
	if err != nil {
		t.Fatalf("activeGate after reload = %v", err)
	}
	if active.Authority != replacementAuthority || active.Build.BuildID != "replacement-build" ||
		active.Snapshot.EffectiveConfigIdentitySHA256 != currentSnapshot.EffectiveConfigIdentitySHA256 {
		t.Fatalf("activeGate after reload mixed identity: authority=%T build=%+v snapshot=%+v", active.Authority, active.Build, active.Snapshot)
	}
	if err := gate.RevalidateActionMetadata(context.Background(), metadata.RequestJSON, metadata.AuthorizationJSON, store); !errors.Is(err, qualification.ErrUnavailable) {
		t.Fatalf("RevalidateActionMetadata after Current changed = %v, want unavailable", err)
	}
	if currentCalls < 2 {
		t.Fatalf("Current calls = %d, want authorization and revalidation to each read current identity", currentCalls)
	}
}

func TestMaterializationGateRejectsChangedCompiledRecipeBeforeWrite(t *testing.T) {
	cfg, recipe, snapshot := requiredPackFixture(t)
	store, err := beads.OpenFileStore(fsys.OSFS{}, filepath.Join(t.TempDir(), "beads.json"))
	if err != nil {
		t.Fatal(err)
	}
	authority := &fakeCompatibilityAuthority{}
	gate := MaterializationGate{Gate: testGate(snapshot, authority), Config: cfg}
	metadata, err := gate.AuthorizeRecipe(context.Background(), recipe, store)
	if err != nil {
		t.Fatal(err)
	}
	recipe.Steps = []formula.RecipeStep{{ID: "review.changed", Title: "different effect", Type: "task", IsRoot: true}}
	if err := gate.RevalidateRecipe(context.Background(), recipe, store, metadata); !errors.Is(err, qualification.ErrUnavailable) {
		t.Fatalf("RevalidateRecipe after compiled recipe change = %v, want unavailable", err)
	}
}

func TestGateDeniesRequiredFormulaWhenAuthorityOrExactStoreCapabilityIsUnavailable(t *testing.T) {
	cfg, recipe, snapshot := requiredPackFixture(t)
	mem := beads.NewMemStore()
	gate := testGate(snapshot, nil)
	if got, err := gate.AuthorizeFormula(context.Background(), cfg, recipe, mem); !errors.Is(err, qualification.ErrUnavailable) || !got.Required {
		t.Fatalf("nil authority result = %#v, %v; want required unavailable", got, err)
	}

	authority := &fakeCompatibilityAuthority{}
	gate = testGate(snapshot, authority)
	gate.ProverForStore = nil // exercise the production default over unsupported MemStore.
	got, err := gate.AuthorizeFormula(context.Background(), cfg, recipe, mem)
	if !errors.Is(err, qualification.ErrUnavailable) || !got.Required || got.Authorization.Status != qualification.StatusUnavailable {
		t.Fatalf("unsupported MemStore result = %#v, %v; want unavailable before action", got, err)
	}
	if authority.authorizeCalls != 0 {
		t.Fatalf("authority received %d action authorization calls; unsupported store must stop before action", authority.authorizeCalls)
	}
}

func TestGateLeavesOrdinaryFormulaOutsideCompatibilityAuthority(t *testing.T) {
	dir := t.TempDir()
	formulaPath := filepath.Join(dir, "formulas", "ordinary.toml")
	if err := os.MkdirAll(filepath.Dir(formulaPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(formulaPath, []byte("formula = \"ordinary\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := (Gate{}).AuthorizeFormula(context.Background(), &config.City{}, &formula.Recipe{
		Name:          "ordinary",
		FormulaSource: formulaPath,
		FormulaSources: []formula.SourceIdentity{{
			Path:          formulaPath,
			ContentSHA256: digest([]byte("formula = \"ordinary\"\n")),
		}},
	}, beads.NewMemStore())
	if err != nil || got.Required {
		t.Fatalf("ordinary formula result = %#v, %v; want ungated success", got, err)
	}
}

func TestGateFailsClosedWhenSourceIdentityIsMissing(t *testing.T) {
	got, err := (Gate{}).AuthorizeFormula(context.Background(), &config.City{}, &formula.Recipe{Name: "maybe-required"}, beads.NewMemStore())
	if !errors.Is(err, qualification.ErrUnavailable) || !got.Required {
		t.Fatalf("missing formula source = %#v, %v; want fail-closed unavailable", got, err)
	}
}

func requiredPackFixture(t *testing.T) (*config.City, *formula.Recipe, qualification.Snapshot) {
	t.Helper()
	cityDir := t.TempDir()
	t.Setenv("GC_HOME", filepath.Join(t.TempDir(), "gc-home"))
	write := func(path, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(cityDir, "city.toml"), "[workspace]\nname = \"city-alpha\"\n")
	write(filepath.Join(cityDir, "pack.toml"), "[pack]\nname = \"city-root\"\nschema = 2\nincludes = [\"packs/required\"]\n")
	write(filepath.Join(cityDir, "packs", "required", "pack.toml"), "[pack]\nname = \"trusted-pack\"\nschema = 2\nrequires_gc = \">=0.14.0\"\n")
	formulaPath := filepath.Join(cityDir, "packs", "required", "formulas", "review.toml")
	formulaBody := "formula = \"review\"\n"
	write(formulaPath, formulaBody)
	cfg, prov, err := config.LoadWithIncludesOptions(fsys.OSFS{}, filepath.Join(cityDir, "city.toml"), config.LoadOptions{CaptureQualificationInputs: true})
	if err != nil {
		t.Fatalf("load required pack config: %v", err)
	}
	recipe := &formula.Recipe{
		Name:          "review",
		FormulaSource: formulaPath,
		ContentHash:   digest([]byte(formulaBody)),
		FormulaSources: []formula.SourceIdentity{{
			Path:          formulaPath,
			ContentSHA256: digest([]byte(formulaBody)),
		}},
	}
	return cfg, recipe, config.RefreshQualificationSnapshot(cfg, prov)
}

func testGate(snapshot qualification.Snapshot, authority qualification.CompatibilityAuthority) Gate {
	return Gate{
		Authority: authority,
		ProverForStore: func(beads.Store, string) qualification.CapabilityProver {
			return testCapabilityProver{}
		},
		CityID:   "city-alpha",
		ServerID: "server-test",
		StoreRef: "city:work",
		Snapshot: snapshot,
		Build: qualification.BuildIdentity{
			Status:         qualification.StatusAvailable,
			SourceRevision: strings.Repeat("1", 40),
			BuildID:        strings.Repeat("1", 40),
			Version:        "0.14.1",
			ArtifactStatus: qualification.StatusAvailable,
			ArtifactSHA256: strings.Repeat("b", 64),
		},
	}
}

type testCapabilityProver struct{}

func (testCapabilityProver) Prove(_ context.Context, scope qualification.CompatibilityScope, policy qualification.CompatibilityPolicy, capability string) (qualification.CapabilityProof, error) {
	scopeSHA, err := qualification.CompatibilityScopeIdentitySHA(scope)
	if err != nil || policy.Status != qualification.StatusAvailable || policy.ScopeSHA256 != scopeSHA || capability == "" {
		return qualification.CapabilityProof{}, qualification.ErrUnavailable
	}
	return qualification.CapabilityProof{
		Status: qualification.StatusAvailable, Capability: capability,
		ScopeSHA256: scopeSHA, PolicyReference: policy.PolicyReference,
		PolicyVersion: policy.PolicyVersion, EvidenceSHA256: strings.Repeat("c", 64),
	}, nil
}

type fakeCompatibilityAuthority struct {
	policy         qualification.CompatibilityPolicy
	resolveCalls   int
	authorizeCalls int
	verifyCalls    int
}

func (a *fakeCompatibilityAuthority) Resolve(_ context.Context, scope qualification.CompatibilityScope) (qualification.CompatibilityPolicy, error) {
	a.resolveCalls++
	scopeSHA, err := qualification.CompatibilityScopeIdentitySHA(scope)
	if err != nil {
		return qualification.CompatibilityPolicy{}, err
	}
	a.policy = qualification.CompatibilityPolicy{
		Status:               qualification.StatusAvailable,
		ScopeSHA256:          scopeSHA,
		PolicyReference:      "test-policy",
		PolicyVersion:        "version-1",
		RequiredCapabilities: []string{CapabilityAttemptEvidencePrivatePayload},
	}
	return a.policy, nil
}

func (a *fakeCompatibilityAuthority) Authorize(_ context.Context, request qualification.CompatibilityRequest) (qualification.ActionAuthorization, error) {
	a.authorizeCalls++
	return qualification.ActionAuthorization{
		Status:          qualification.StatusAuthorized,
		PolicyReference: request.Policy.PolicyReference,
		PolicyVersion:   request.Policy.PolicyVersion,
		RequestSHA256:   request.RequestSHA256,
		RecordID:        "record-1",
		Reference:       "opaque-ref-1",
	}, nil
}

func (a *fakeCompatibilityAuthority) Verify(_ context.Context, request qualification.CompatibilityRequest, auth qualification.ActionAuthorization) error {
	a.verifyCalls++
	if request.Policy.PolicyReference != auth.PolicyReference || request.RequestSHA256 != auth.RequestSHA256 {
		return qualification.ErrUnavailable
	}
	return nil
}

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
