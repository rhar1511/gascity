package contract

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

// TestPreflightBlocksNativeOnABackendGCDoesNotImplement pins the native-store
// gate for the shape that replaced postgres: the metadata names a backend gc
// has no vocabulary for, and every check that can decide says so by name.
//
// The connection keys in the fixture are deliberate. They are the shape a
// linked beads library reads and gc does not, and their presence must change
// nothing here — the verdict comes from the backend name alone.
func TestPreflightBlocksNativeOnABackendGCDoesNotImplement(t *testing.T) {
	scope := "/city"
	checker := testConfirmedSourcePreflightChecker(preflightMetadataJSON(`{
		"backend": "postgres",
		"postgres_host": "db.example.com",
		"postgres_port": "5432",
		"postgres_user": "operator",
		"postgres_database": "gascity",
		"project_id": "gc-local"
	}`), PreflightBDContext{Backend: "postgres"}, "gc-local")

	result, err := checker.Check(scope)
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}

	assertPreflightVerdict(t, result, PreflightVerdictBlocked, false)
	assertCheckOrder(t, result)
	assertCheckState(t, result, PreflightCheckMetadataBackend, PreflightCheckFail)
	// bd context is not consulted once metadata_backend has blocked: the
	// bd-context checks report that, as a WARN, instead of a cross-check.
	assertCheckState(t, result, PreflightCheckBDContextAgreement, PreflightCheckWarn)
	assertCheckState(t, result, PreflightCheckContractShape, PreflightCheckFail)
	for _, id := range []PreflightCheckID{PreflightCheckMetadataBackend, PreflightCheckContractShape} {
		if summary := findPreflightCheck(t, result, id).Summary; !strings.Contains(summary, `"postgres"`) {
			t.Errorf("%s summary = %q, want it to name the backend", id, summary)
		}
	}
	assertPreflightReadOnly(t, checker.FS.(*fsys.Fake))
}

// TestPreflightRedactsADSNDiagnostic keeps the redaction rule keyed on the
// shape of the value rather than on any one backend's name: a *_dsn diagnostic
// is a connection string with a userinfo section whoever wrote it.
func TestPreflightRedactsADSNDiagnostic(t *testing.T) {
	details := PreflightDetails{
		MetadataBackend: "postgres",
		AdditionalDiagnostics: []PreflightDetailField{
			{Key: "storage_dsn", Value: "postgres://operator:swordfish@db.example.com/gascity"},
		},
	}
	check := NewPreflightCheckResult(PreflightCheckContractShape, PreflightCheckFail, "shape refused", details)
	if got := check.Details.AdditionalDiagnostics[0].Value; got != "postgres://[REDACTED]" {
		t.Fatalf("dsn diagnostic = %q, want %q", got, "postgres://[REDACTED]")
	}
	data, err := json.Marshal(check)
	if err != nil {
		t.Fatalf("MarshalJSON: %v", err)
	}
	if strings.Contains(string(data), "swordfish") {
		t.Fatalf("serialized check leaked the DSN secret: %s", data)
	}
}

func TestPreflightBlocksNativeOnContextDisagreement(t *testing.T) {
	scope := "/city"
	checker := testConfirmedSourcePreflightChecker(preflightMetadataJSON(`{
		"backend": "dolt",
		"dolt_mode": "server",
		"dolt_database": "gascity",
		"project_id": "gc-local"
	}`), PreflightBDContext{Backend: "postgres"}, "gc-local")

	result, err := checker.Check(scope)
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}

	assertPreflightVerdict(t, result, PreflightVerdictBlocked, false)
	assertCheckState(t, result, PreflightCheckBDContextAgreement, PreflightCheckFail)
}

// TestPreflightEligibleOnUnreachableBDContextWhenIdentityVerified keeps project identity independent from command availability,
// with an explicitly synthetic matched-source fixture (not release authentication).
func TestPreflightEligibleOnUnreachableBDContextWhenIdentityVerified(t *testing.T) {
	scope := "/city"
	fs := fsys.NewFake()
	fs.Dirs[filepath.Join(scope, ".beads")] = true
	fs.Files[filepath.Join(scope, ".beads", "metadata.json")] = []byte(`{
		"backend": "dolt",
		"dolt_mode": "server",
		"dolt_database": "gascity",
		"project_id": "gc-local"
	}`)
	checker := PreflightChecker{
		FS:                  fs,
		Provider:            "bd",
		BeadsLibraryVersion: "1.0.4",
		BDContext: func(string) (PreflightBDContext, error) {
			return PreflightBDContext{}, errors.New("bd context unavailable: not a git repository")
		},
		DatabaseProjectID: func(string) (string, bool, error) {
			return "gc-local", true, nil
		},
	}

	bindSyntheticPreflightSource(&checker)
	result, err := checker.Check(scope)
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}

	// Confirmed source pair + unavailable context + independent identity proof => ELIGIBLE.
	assertPreflightVerdict(t, result, PreflightVerdictEligible, true)
	// The bd-context cross-checks still report WARN; they are informational —
	// the verdict is upgraded on the strength of the independent identity match.
	assertCheckState(t, result, PreflightCheckBDContextAgreement, PreflightCheckWarn)
	assertCheckState(t, result, PreflightCheckDoltModeSafe, PreflightCheckWarn)
	assertCheckState(t, result, PreflightCheckVersionCompat, PreflightCheckPass)
	assertCheckState(t, result, PreflightCheckIdentityMatch, PreflightCheckPass)
	// The result flags that eligibility came via the identity-fallback path.
	if !result.NativeEligibleViaIdentityFallback {
		t.Errorf("NativeEligibleViaIdentityFallback = false, want true on the identity-verified upgrade")
	}
}

// TestPreflightDegradesOnUnreachableBDContextWithoutIdentityProof keeps project identity independent from command availability,
// with an explicitly synthetic matched-source fixture (not release authentication).
func TestPreflightDegradesOnUnreachableBDContextWithoutIdentityProof(t *testing.T) {
	scope := "/city"
	fs := fsys.NewFake()
	fs.Dirs[filepath.Join(scope, ".beads")] = true
	fs.Files[filepath.Join(scope, ".beads", "metadata.json")] = []byte(`{
		"backend": "dolt",
		"dolt_mode": "server",
		"dolt_database": "gascity",
		"project_id": "gc-local"
	}`)
	checker := PreflightChecker{
		FS:                  fs,
		Provider:            "bd",
		BeadsLibraryVersion: "1.0.4",
		BDContext: func(string) (PreflightBDContext, error) {
			return PreflightBDContext{}, errors.New("bd context unavailable: not a git repository")
		},
		DatabaseProjectID: func(string) (string, bool, error) {
			return "", false, nil
		},
	}

	bindSyntheticPreflightSource(&checker)
	result, err := checker.Check(scope)
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}

	// Unreachable bd context, no independent proof => DEGRADED, never BLOCKED.
	assertPreflightVerdict(t, result, PreflightVerdictDegraded, false)
	assertCheckState(t, result, PreflightCheckBDContextAgreement, PreflightCheckWarn)
	assertCheckState(t, result, PreflightCheckDoltModeSafe, PreflightCheckWarn)
	assertCheckState(t, result, PreflightCheckVersionCompat, PreflightCheckPass)
	assertCheckState(t, result, PreflightCheckIdentityMatch, PreflightCheckWarn)
	// No upgrade happened, so the identity-fallback flag stays false.
	if result.NativeEligibleViaIdentityFallback {
		t.Errorf("NativeEligibleViaIdentityFallback = true, want false when the verdict stays DEGRADED")
	}
}

func TestPreflightBlocksNativeOnIdentityMismatch(t *testing.T) {
	scope := "/city"
	checker := testConfirmedSourcePreflightChecker(preflightMetadataJSON(`{
		"backend": "dolt",
		"dolt_mode": "server",
		"dolt_database": "gascity",
		"project_id": "metadata-id"
	}`), PreflightBDContext{Backend: "dolt", DoltMode: "server"}, "database-id")

	result, err := checker.Check(scope)
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}

	assertPreflightVerdict(t, result, PreflightVerdictBlocked, false)
	assertCheckState(t, result, PreflightCheckIdentityMatch, PreflightCheckFail)
	check := findPreflightCheck(t, result, PreflightCheckIdentityMatch)
	if check.Details.MetadataProjectID != "metadata-id" || check.Details.DBProjectID != "database-id" {
		t.Fatalf("identity details = %+v, want both project ids visible", check.Details)
	}
}

func TestPreflightPassesOnHealthyDolt(t *testing.T) {
	scope := "/city"
	checker := testConfirmedSourcePreflightChecker(preflightMetadataJSON(`{
		"backend": "dolt",
		"dolt_mode": "server",
		"dolt_database": "gascity",
		"project_id": "gc-local"
	}`), PreflightBDContext{Backend: "dolt", DoltMode: "server"}, "gc-local")

	result, err := checker.Check(scope)
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}

	assertPreflightVerdict(t, result, PreflightVerdictEligible, true)
	for _, check := range result.Checks {
		if check.State != PreflightCheckPass {
			t.Fatalf("check %s state = %s, want PASS in healthy case: %+v", check.ID, check.State, result.Checks)
		}
	}
	if result.Fallback != PreflightFallbackNone {
		t.Fatalf("Fallback = %q, want none", result.Fallback)
	}
}

func TestPreflightAcceptsExecGcBeadsBdProviderPath(t *testing.T) {
	scope := "/city"
	checker := testConfirmedSourcePreflightChecker(preflightMetadataJSON(`{
		"backend": "dolt",
		"dolt_mode": "server",
		"dolt_database": "gascity",
		"project_id": "gc-local"
	}`), PreflightBDContext{Backend: "dolt", DoltMode: "server"}, "gc-local")
	checker.Provider = "exec:/tmp/gc-beads-bd.sh"

	result, err := checker.Check(scope)
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}

	assertPreflightVerdict(t, result, PreflightVerdictEligible, true)
	assertCheckState(t, result, PreflightCheckProviderContract, PreflightCheckPass)
}

func TestProviderUsesBDContract(t *testing.T) {
	tests := []struct {
		provider string
		want     bool
	}{
		{provider: "", want: true},
		{provider: "bd", want: true},
		{provider: " file ", want: false},
		{provider: "exec:gc-beads-bd", want: true},
		{provider: "exec:/tmp/gc-beads-bd", want: true},
		{provider: "exec:/tmp/gc-beads-bd.sh", want: true},
		{provider: "exec:/tmp/gc-beads-k8s", want: false},
		{provider: "exec:/tmp/custom", want: false},
	}
	for _, tt := range tests {
		if got := ProviderUsesBDContract(tt.provider); got != tt.want {
			t.Fatalf("ProviderUsesBDContract(%q) = %v, want %v", tt.provider, got, tt.want)
		}
	}
}

func TestPreflightRespectsSkipOverrideAsRecoveryOnly(t *testing.T) {
	t.Setenv("BEADS_SKIP_IDENTITY_CHECK", "1")
	scope := "/city"
	checker := testConfirmedSourcePreflightChecker(preflightMetadataJSON(`{
		"backend": "dolt",
		"dolt_mode": "server",
		"dolt_database": "gascity",
		"project_id": "metadata-id"
	}`), PreflightBDContext{Backend: "dolt", DoltMode: "server"}, "database-id")

	result, err := checker.Check(scope)
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}

	assertPreflightVerdict(t, result, PreflightVerdictBlocked, false)
	assertCheckState(t, result, PreflightCheckIdentityMatch, PreflightCheckFail)
}

func TestPreflightWarnsWhenDatabaseIdentityUnavailable(t *testing.T) {
	scope := "/city"
	checker := testConfirmedSourcePreflightChecker(preflightMetadataJSON(`{
		"backend": "dolt",
		"dolt_mode": "server",
		"dolt_database": "gascity",
		"project_id": "metadata-id"
	}`), PreflightBDContext{Backend: "dolt", DoltMode: "server"}, "")
	checker.DatabaseProjectID = func(string) (string, bool, error) {
		return "", false, errors.New("dial dolt")
	}

	result, err := checker.Check(scope)
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}

	assertPreflightVerdict(t, result, PreflightVerdictDegraded, false)
	assertCheckState(t, result, PreflightCheckIdentityMatch, PreflightCheckWarn)
}

// TestPreflightDefersIdentityToNativeOpenForExternalEndpoint covers hosted
// beads-gateway endpoints: the direct project_id SQL probe (managedDoltOpenDatabase)
// connects as root over plaintext and cannot authenticate the EIA-as-username +
// TLS gateway, so it never confirms project_id. For an external endpoint the
// authoritative database _project_id is verified by beadslib at native-open time
// (verifyProjectIdentity over the authenticated connection), so the identity
// check defers to that gate and keeps the scope native-eligible instead of
// degrading to the shell BdStore — without claiming a control-plane confirmation
// it cannot make.
func TestPreflightDefersIdentityToNativeOpenForExternalEndpoint(t *testing.T) {
	scope := "/city"
	checker := testConfirmedSourcePreflightChecker(preflightMetadataJSON(`{
		"backend": "dolt",
		"dolt_mode": "server",
		"dolt_database": "bd_prj_c069247fbac36e2b",
		"project_id": "prj_c069247fbac36e2b"
	}`), PreflightBDContext{Backend: "dolt", DoltMode: "server"}, "")
	// Direct DB probe fails to authenticate the hosted gateway (root/plaintext)...
	checker.DatabaseProjectID = func(string) (string, bool, error) {
		return "", false, errors.New("dial hosted gateway: access denied")
	}
	// ...and the scope resolves to an external endpoint, so identity is deferred
	// to beadslib's native-open verification rather than degraded.
	checker.DeferIdentityToNativeOpen = func(string) bool { return true }

	result, err := checker.Check(scope)
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}

	assertPreflightVerdict(t, result, PreflightVerdictEligible, true)
	assertCheckState(t, result, PreflightCheckIdentityMatch, PreflightCheckPass)
}

// TestPreflightExternalEndpointStillBlocksOnProbeMismatch guards the deferral:
// deferring to native-open verification only applies when the direct probe is
// UNAVAILABLE. If the probe does reach the database and reports a project_id that
// disagrees with metadata, that is a genuine cross-project mismatch and must
// still block native activation even for an external endpoint.
func TestPreflightExternalEndpointStillBlocksOnProbeMismatch(t *testing.T) {
	scope := "/city"
	checker := testConfirmedSourcePreflightChecker(preflightMetadataJSON(`{
		"backend": "dolt",
		"dolt_mode": "server",
		"dolt_database": "gascity",
		"project_id": "metadata-id"
	}`), PreflightBDContext{Backend: "dolt", DoltMode: "server"}, "database-id")
	checker.DeferIdentityToNativeOpen = func(string) bool { return true }

	result, err := checker.Check(scope)
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}

	assertPreflightVerdict(t, result, PreflightVerdictBlocked, false)
	assertCheckState(t, result, PreflightCheckIdentityMatch, PreflightCheckFail)
}

func TestPreflightUnreadableScopeReturnsError(t *testing.T) {
	scope := "/city"
	fs := fsys.NewFake()
	fs.Errors[filepath.Join(scope, ".beads", "metadata.json")] = os.ErrPermission
	checker := PreflightChecker{
		FS:                  fs,
		Provider:            "bd",
		BeadsLibraryVersion: "1.0.4",
		BDContext: func(string) (PreflightBDContext, error) {
			return PreflightBDContext{Backend: "dolt", DoltMode: "server", BDVersion: "1.0.4", SchemaVersion: 1}, nil
		},
		DatabaseProjectID: func(string) (string, bool, error) {
			return "gc-local", true, nil
		},
	}

	if _, err := checker.Check(scope); err == nil || !strings.Contains(err.Error(), "read preflight metadata") {
		t.Fatalf("Check() error = %v, want unreadable metadata error", err)
	}
	assertPreflightReadOnly(t, fs)
}

func testPreflightChecker(metadata string, ctx PreflightBDContext, dbProjectID string) PreflightChecker {
	scope := "/city"
	fs := fsys.NewFake()
	fs.Dirs[filepath.Join(scope, ".beads")] = true
	fs.Files[filepath.Join(scope, ".beads", "metadata.json")] = []byte(metadata)
	if ctx.BDVersion == "" {
		ctx.BDVersion = "1.0.4"
	}
	if ctx.SchemaVersion == 0 {
		ctx.SchemaVersion = 1
	}
	return PreflightChecker{
		FS:                  fs,
		Provider:            "bd",
		BeadsLibraryVersion: "1.0.4",
		BDContext: func(string) (PreflightBDContext, error) {
			return ctx, nil
		},
		DatabaseProjectID: func(string) (string, bool, error) {
			return dbProjectID, dbProjectID != "", nil
		},
	}
}

func preflightMetadataJSON(body string) string {
	return strings.ReplaceAll(body, "\t", "")
}

func assertPreflightVerdict(t *testing.T, result PreflightResult, want PreflightVerdict, wantEligible bool) {
	t.Helper()
	if result.Verdict != want {
		t.Fatalf("Verdict = %q, want %q; checks=%+v", result.Verdict, want, result.Checks)
	}
	if result.NativeStoreEligible != wantEligible {
		t.Fatalf("NativeStoreEligible = %v, want %v", result.NativeStoreEligible, wantEligible)
	}
}

func assertCheckOrder(t *testing.T, result PreflightResult) {
	t.Helper()
	want := []PreflightCheckID{
		PreflightCheckProviderContract,
		PreflightCheckMetadataBackend,
		PreflightCheckBDContextAgreement,
		PreflightCheckDoltModeSafe,
		PreflightCheckIdentityMatch,
		PreflightCheckVersionCompat,
		PreflightCheckContractShape,
	}
	if len(result.Checks) != len(want) {
		t.Fatalf("Checks len = %d, want %d: %+v", len(result.Checks), len(want), result.Checks)
	}
	for i, id := range want {
		if result.Checks[i].ID != id {
			t.Fatalf("Checks[%d].ID = %q, want %q; checks=%+v", i, result.Checks[i].ID, id, result.Checks)
		}
	}
}

func assertCheckState(t *testing.T, result PreflightResult, id PreflightCheckID, want PreflightCheckState) {
	t.Helper()
	check := findPreflightCheck(t, result, id)
	if check.State != want {
		t.Fatalf("check %s state = %q, want %q; check=%+v", id, check.State, want, check)
	}
}

func findPreflightCheck(t *testing.T, result PreflightResult, id PreflightCheckID) PreflightCheckResult {
	t.Helper()
	for _, check := range result.Checks {
		if check.ID == id {
			return check
		}
	}
	t.Fatalf("missing check %s in %+v", id, result.Checks)
	return PreflightCheckResult{}
}

func assertPreflightReadOnly(t *testing.T, fs *fsys.Fake) {
	t.Helper()
	for _, call := range fs.Calls {
		switch call.Method {
		case "WriteFile", "MkdirAll", "Rename", "Remove", "Chmod":
			t.Fatalf("preflight checker must be read-only; saw %s on %s", call.Method, call.Path)
		}
	}
}

// TestUnboundSourceBuildsDoNotEstablishCompatibility retains the legacy version inputs while refusing
// to mistake display strings or JSON wire format for authenticated source identity.
func TestUnboundSourceBuildsDoNotEstablishCompatibility(t *testing.T) {
	validCtx := func(bdVersion string) PreflightBDContext {
		return PreflightBDContext{Backend: "dolt", DoltMode: "server", BDVersion: bdVersion, SchemaVersion: 1}
	}
	tests := []struct {
		name       string
		libVersion string
		replaced   bool
		ctx        PreflightBDContext
		want       PreflightCheckState
	}{
		{"source build reports (devel) — source unconfirmed", "(devel)", false, validCtx("1.0.5"), PreflightCheckWarn},
		{"confirmed version mismatch display alone is unconfirmed", "1.0.5", false, validCtx("1.0.4"), PreflightCheckWarn},
		{"matching displayed versions remain unconfirmed", "1.0.5", false, validCtx("1.0.5"), PreflightCheckWarn},
		{"missing bd version is unconfirmable — warn", "1.0.5", false, validCtx(""), PreflightCheckWarn},

		{"enterprise replacement pseudo-version vs bd release — source unconfirmed", "0.0.0-20260810084121-1aa7bf160786", true, validCtx("1.1.0"), PreflightCheckWarn},
		{"pseudo-version without a replace — source unconfirmed", "1.1.1-0.20260805093327-bf97b73749ac", false, validCtx("1.1.0"), PreflightCheckWarn},
		{"tagged replacement module — source unconfirmed", "2.4.0", true, validCtx("1.1.0"), PreflightCheckWarn},
		{"replaced with no recorded version — source unconfirmed", "", true, validCtx("1.1.0"), PreflightCheckWarn},

		{"unreplaced release skew display alone is unconfirmed", "1.1.1", false, validCtx("1.1.0"), PreflightCheckWarn},
		{"unreplaced exact match display alone is unconfirmed", "1.1.0", false, validCtx("1.1.0"), PreflightCheckWarn},
		{"v-prefixed unreplaced release skew display alone is unconfirmed", "v1.2.0", false, validCtx("v1.1.0"), PreflightCheckWarn},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := PreflightChecker{BeadsLibraryVersion: tt.libVersion, BeadsLibraryReplaced: tt.replaced}
			got := c.checkVersionCompat(tt.ctx, nil)
			if got.ID != PreflightCheckVersionCompat {
				t.Fatalf("ID = %q, want %q", got.ID, PreflightCheckVersionCompat)
			}
			if got.State != tt.want {
				t.Fatalf("state = %q, want %q (summary: %q)", got.State, tt.want, got.Summary)
			}
		})
	}
}

// TestUnboundCompatibilityDiagnosticsDoNotClaimSchemaProof preserves the six
// legacy diagnostic inputs without presenting unbound version strings as schema proof.
func TestUnboundCompatibilityDiagnosticsDoNotClaimSchemaProof(t *testing.T) {
	validCtx := func(bdVersion string) PreflightBDContext {
		return PreflightBDContext{Backend: "dolt", DoltMode: "server", BDVersion: bdVersion, SchemaVersion: 1}
	}
	tests := []struct {
		name       string
		libVersion string
		ctx        PreflightBDContext
		err        error
		want       string
	}{
		{"unreachable bd context", "1.0.5", PreflightBDContext{}, errors.New("not a git repository"), "linked Beads source is unconfirmed: module family is missing or unsupported"},
		{"missing JSON wire version", "1.0.5", PreflightBDContext{BDVersion: "1.0.5"}, nil, "bd context JSON wire-format version is missing or unsupported"},
		{"missing bd version", "1.0.5", validCtx(""), nil, "linked Beads source is unconfirmed: module family is missing or unsupported"},
		{"source build", "(devel)", validCtx("1.0.5"), nil, "linked Beads source is unconfirmed: module family is missing or unsupported"},
		{"matching releases", "1.0.5", validCtx("1.0.5"), nil, "linked Beads source is unconfirmed: module family is missing or unsupported"},
		{"confirmed mismatch", "1.0.5", validCtx("1.0.4"), nil, "linked Beads source is unconfirmed: module family is missing or unsupported"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := PreflightChecker{BeadsLibraryVersion: tt.libVersion}
			if got := c.checkVersionCompat(tt.ctx, tt.err); got.Summary != tt.want {
				t.Fatalf("summary = %q, want %q", got.Summary, tt.want)
			}
		})
	}
}

// TestPreflightDegradesOnUnconfirmedReplacedModule refuses the former
// readable-context false positive without pretending a replacement is a released source.
func TestPreflightDegradesOnUnconfirmedReplacedModule(t *testing.T) {
	scope := "/city/rigs/gascity"
	fs := fsys.NewFake()
	fs.Dirs[filepath.Join(scope, ".beads")] = true
	fs.Files[filepath.Join(scope, ".beads", "metadata.json")] = []byte(`{
		"backend": "dolt",
		"dolt_mode": "server",
		"dolt_database": "gascity",
		"project_id": "gc-local"
	}`)
	checker := PreflightChecker{
		FS:                   fs,
		Provider:             "bd",
		BeadsLibraryVersion:  "0.0.0-20260810084121-1aa7bf160786",
		BeadsLibraryReplaced: true,
		BDContext: func(string) (PreflightBDContext, error) {
			return PreflightBDContext{Backend: "dolt", DoltMode: "server", BDVersion: "1.1.0", SchemaVersion: 1}, nil
		},
		DatabaseProjectID: func(string) (string, bool, error) {
			return "gc-local", true, nil
		},
	}

	result, err := checker.Check(scope)
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}

	assertPreflightVerdict(t, result, PreflightVerdictDegraded, false)
	assertCheckState(t, result, PreflightCheckVersionCompat, PreflightCheckWarn)
	// Readable context and independent project identity cannot qualify unknown source.
	if result.NativeEligibleViaIdentityFallback {
		t.Errorf("NativeEligibleViaIdentityFallback = true, want false when bd context is readable")
	}
	if result.Fallback != PreflightFallbackBdStore {
		t.Errorf("Fallback = %q, want bd fallback for unconfirmed replaced module", result.Fallback)
	}
}

// TestPreflightBlocksOnRealVersionSkew requires actual differing source
// descriptors; a display string alone is not a confirmed mismatch.
func TestPreflightBlocksOnRealVersionSkew(t *testing.T) {
	scope := "/city/rigs/gascity"
	fs := fsys.NewFake()
	fs.Dirs[filepath.Join(scope, ".beads")] = true
	fs.Files[filepath.Join(scope, ".beads", "metadata.json")] = []byte(`{
		"backend": "dolt",
		"dolt_mode": "server",
		"dolt_database": "gascity",
		"project_id": "gc-local"
	}`)
	checker := PreflightChecker{
		FS:                  fs,
		Provider:            "bd",
		BeadsLibraryVersion: "1.2.0",
		BDContext: func(string) (PreflightBDContext, error) {
			return PreflightBDContext{Backend: "dolt", DoltMode: "server", BDVersion: "1.1.0", SchemaVersion: 1}, nil
		},
		DatabaseProjectID: func(string) (string, bool, error) {
			return "gc-local", true, nil
		},
	}

	linked := syntheticPreflightArtifact("v1.2.0")
	checker.BeadsLibraryArtifact = &linked
	reader := checker.BDContext
	checker.BDContext = func(scope string) (PreflightBDContext, error) {
		ctx, err := reader(scope)
		ctx.CLIArtifact = syntheticPreflightArtifact("v1.1.0")
		return ctx, err
	}
	result, err := checker.Check(scope)
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}

	assertPreflightVerdict(t, result, PreflightVerdictBlocked, false)
	assertCheckState(t, result, PreflightCheckVersionCompat, PreflightCheckFail)
	if result.Fallback != PreflightFallbackBdStore {
		t.Errorf("Fallback = %q, want %q", result.Fallback, PreflightFallbackBdStore)
	}
}

// TestUnboundSemverDisplaysDoNotEstablishCompatibility retains the legacy version inputs while refusing
// to mistake display strings or JSON wire format for authenticated source identity.
func TestUnboundSemverDisplaysDoNotEstablishCompatibility(t *testing.T) {
	validCtx := func(bdVersion string) PreflightBDContext {
		return PreflightBDContext{Backend: "dolt", DoltMode: "server", BDVersion: bdVersion, SchemaVersion: 1}
	}
	tests := []struct {
		name       string
		libVersion string
		ctx        PreflightBDContext
		want       PreflightCheckState
	}{
		{"newer patch, same major.minor — source unconfirmed", "1.1.0", validCtx("1.1.2"), PreflightCheckWarn},
		{"newer minor, same major — source unconfirmed", "1.1.0", validCtx("1.2.0"), PreflightCheckWarn},
		{"newer major — display alone is unconfirmed, majors are not semver-compatible", "1.1.0", validCtx("2.0.0"), PreflightCheckWarn},
		{"older patch — display alone is unconfirmed", "1.1.2", validCtx("1.1.0"), PreflightCheckWarn},
		{"older major — display alone is unconfirmed", "2.0.0", validCtx("1.9.9"), PreflightCheckWarn},
		{"v-prefixed newer patch — source unconfirmed", "v1.1.0", validCtx("v1.1.2"), PreflightCheckWarn},
		{"non-semver CLI display remains unconfirmed", "1.1.0", validCtx("not-a-version"), PreflightCheckWarn},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := PreflightChecker{BeadsLibraryVersion: tt.libVersion}
			got := c.checkVersionCompat(tt.ctx, nil)
			if got.State != tt.want {
				t.Fatalf("state = %q, want %q (summary: %q)", got.State, tt.want, got.Summary)
			}
		})
	}
}

// TestUnboundPrereleaseDisplaysDoNotEstablishCompatibility retains the legacy version inputs while refusing
// to mistake display strings or JSON wire format for authenticated source identity.
func TestUnboundPrereleaseDisplaysDoNotEstablishCompatibility(t *testing.T) {
	validCtx := func(bdVersion string) PreflightBDContext {
		return PreflightBDContext{Backend: "dolt", DoltMode: "server", BDVersion: bdVersion, SchemaVersion: 1}
	}
	tests := []struct {
		name       string
		libVersion string
		ctx        PreflightBDContext
		want       PreflightCheckState
	}{
		{"older RC against newer RC of the same release — source unconfirmed", "v1.3.0-rc.2", validCtx("1.3.0-rc.1"), PreflightCheckWarn},
		{"newer RC against older RC of the same release — source unconfirmed", "v1.3.0-rc.1", validCtx("1.3.0-rc.2"), PreflightCheckWarn},
		{"RC against the final release it precedes — display alone is unconfirmed", "v1.3.0", validCtx("1.3.0-rc.1"), PreflightCheckWarn},
		{"prereleases of different releases — display alone is unconfirmed", "v1.4.0-rc.1", validCtx("1.3.0-rc.1"), PreflightCheckWarn},
		{"prereleases across majors — display alone is unconfirmed", "v2.0.0-rc.1", validCtx("1.3.0-rc.1"), PreflightCheckWarn},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := PreflightChecker{BeadsLibraryVersion: tt.libVersion}
			got := c.checkVersionCompat(tt.ctx, nil)
			if got.State != tt.want {
				t.Fatalf("state = %q, want %q (summary: %q)", got.State, tt.want, got.Summary)
			}
		})
	}
}

// TestPreflightBlocksOnDifferentReleasedSourceDespiteNewerDisplay keeps
// semver ordering from replacing the exact released-source requirement.
func TestPreflightBlocksOnDifferentReleasedSourceDespiteNewerDisplay(t *testing.T) {
	scope := "/city/rigs/gascity"
	fs := fsys.NewFake()
	fs.Dirs[filepath.Join(scope, ".beads")] = true
	fs.Files[filepath.Join(scope, ".beads", "metadata.json")] = []byte(`{
		"backend": "dolt",
		"dolt_mode": "server",
		"dolt_database": "gascity",
		"project_id": "gc-local"
	}`)
	checker := PreflightChecker{
		FS:                  fs,
		Provider:            "bd",
		BeadsLibraryVersion: "1.1.0",
		BDContext: func(string) (PreflightBDContext, error) {
			return PreflightBDContext{Backend: "dolt", DoltMode: "server", BDVersion: "1.1.2", SchemaVersion: 1}, nil
		},
		DatabaseProjectID: func(string) (string, bool, error) {
			return "gc-local", true, nil
		},
	}

	linked := syntheticPreflightArtifact("v1.1.0")
	checker.BeadsLibraryArtifact = &linked
	reader := checker.BDContext
	checker.BDContext = func(scope string) (PreflightBDContext, error) {
		ctx, err := reader(scope)
		ctx.CLIArtifact = syntheticPreflightArtifact("v1.1.2")
		return ctx, err
	}
	result, err := checker.Check(scope)
	if err != nil {
		t.Fatalf("Check() error = %v", err)
	}

	assertPreflightVerdict(t, result, PreflightVerdictBlocked, false)
	assertCheckState(t, result, PreflightCheckVersionCompat, PreflightCheckFail)
	if result.Fallback != PreflightFallbackBdStore {
		t.Errorf("Fallback = %q, want bd fallback for different released sources", result.Fallback)
	}
}

// TestLinkedBeadsLibraryFromBuildInfo covers the build-info reader that feeds
// checkVersionCompat in production. A replace directive — of either form — means
// the recorded version does not describe the code that is actually linked.
func TestLinkedBeadsLibraryFromBuildInfo(t *testing.T) {
	dep := func(version string, replace *debug.Module) *debug.BuildInfo {
		return &debug.BuildInfo{Deps: []*debug.Module{
			{Path: "github.com/spf13/cobra", Version: "v1.10.2"},
			{Path: beadsModulePath, Version: version, Replace: replace},
		}}
	}
	tests := []struct {
		name string
		info *debug.BuildInfo
		want beadsLibrary
	}{
		{"nil build info", nil, beadsLibrary{}},
		{"beads is not linked", &debug.BuildInfo{Deps: []*debug.Module{{Path: "github.com/spf13/cobra", Version: "v1.10.2"}}}, beadsLibrary{}},
		{"plain require", dep("v1.1.0", nil), beadsLibrary{ModulePath: beadsModulePath, Version: "v1.1.0"}},
		{
			"module replace records the replacement's version",
			dep("v1.1.1-0.20260805093327-bf97b73749ac", &debug.Module{Path: "github.com/gascity/bd-enterprise", Version: "v0.0.0-20260810084121-1aa7bf160786"}),
			beadsLibrary{ModulePath: "github.com/gascity/bd-enterprise", Version: "v0.0.0-20260810084121-1aa7bf160786", Replaced: true},
		},
		{
			"local-path replace records no version",
			dep("v1.1.0", &debug.Module{Path: "../beads"}),
			beadsLibrary{ModulePath: "../beads", Replaced: true},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := linkedBeadsLibraryFrom(tt.info); got != tt.want {
				t.Fatalf("linkedBeadsLibraryFrom() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// TestComparableReleaseVersion pins the classifier that decides whether the two
// reported versions name the same kind of thing. Only a released semver tag can
// be compared to bd's self-reported release.
func TestComparableReleaseVersion(t *testing.T) {
	tests := []struct {
		version string
		want    bool
	}{
		{"", false},
		{"(devel)", false},
		{"devel", false},
		{"unknown", false},
		{"1.1.0", true},
		{"v1.1.0", true},
		{"1.1.0-rc.1", true},
		{"0.0.0-20260810084121-1aa7bf160786", false},
		{"1.1.1-0.20260805093327-bf97b73749ac", false},
		{"1.2.0-pre.0.20260805093327-bf97b73749ac", false},
	}
	for _, tt := range tests {
		t.Run(tt.version, func(t *testing.T) {
			if got := comparableReleaseVersion(tt.version); got != tt.want {
				t.Fatalf("comparableReleaseVersion(%q) = %v, want %v", tt.version, got, tt.want)
			}
		})
	}
}

// syntheticPreflightArtifact is comparison fixture data only; it authenticates
// no real module, executable, release or database.
func syntheticPreflightArtifact(version string) BeadsArtifact {
	return BeadsArtifact{ModulePath: beadsModulePath, Version: version, Sum: "h1:" + strings.Repeat("A", 43) + "="}
}

func bindSyntheticPreflightSource(checker *PreflightChecker) {
	a := syntheticPreflightArtifact("v1.3.2")
	checker.BeadsLibraryArtifact = &a
	reader := checker.BDContext
	if reader != nil {
		checker.BDContext = func(scope string) (PreflightBDContext, error) {
			ctx, err := reader(scope)
			ctx.CLIArtifact = a
			return ctx, err
		}
	}
}

func testConfirmedSourcePreflightChecker(metadata string, ctx PreflightBDContext, dbProjectID string) PreflightChecker {
	checker := testPreflightChecker(metadata, ctx, dbProjectID)
	bindSyntheticPreflightSource(&checker)
	return checker
}
