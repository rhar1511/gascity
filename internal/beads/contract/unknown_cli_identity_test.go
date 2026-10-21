package contract

import (
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

// The real bd JSON formatter reports schema_version=1 even when context runs
// without any database. That wire-format number and displayed release strings
// do not establish the actual CLI artifact's source family or writer contract.
func TestUnboundCLIArtifactCannotAdmitNativeFromReleaseStrings(t *testing.T) {
	tests := []struct {
		name, libraryVersion, cliVersion string
		replaced                         bool
	}{
		{"equal displayed release", "1.3.2", "1.3.2", false},
		{"newer displayed CLI", "1.3.2", "1.3.3", false},
		{"same prerelease series", "1.3.2-rc.2", "1.3.2-rc.1", false},
		{"devel library", "(devel)", "1.3.2", false},
		{"pseudo version library", "1.3.2-0.20261013010644-3c3423cf09a9", "1.3.2", false},
		{"replaced library", "1.3.2", "1.3.2", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			checker := testPreflightChecker(`{"backend":"dolt","dolt_mode":"server","dolt_database":"gascity","project_id":"gc-local"}`, PreflightBDContext{Backend: "dolt", DoltMode: "server", BDVersion: tt.cliVersion, SchemaVersion: 1}, "gc-local")
			checker.BeadsLibraryVersion = tt.libraryVersion
			checker.BeadsLibraryReplaced = tt.replaced
			result, err := checker.Check("/city")
			if err != nil {
				t.Fatal(err)
			}
			if result.NativeStoreEligible || result.NativeEligibleViaIdentityFallback || result.Verdict == PreflightVerdictEligible {
				t.Fatalf("unbound CLI source admitted from display versions and wire schema1: %+v", result)
			}
			if check := findPreflightCheck(t, result, PreflightCheckVersionCompat); check.State == PreflightCheckPass {
				t.Fatalf("unbound artifact compatibility falsely PASS: %+v", check)
			}
			assertPreflightReadOnly(t, checker.FS.(*fsys.Fake))
		})
	}
}

func TestUnboundCLIArtifactCannotUseProjectIdentityFallback(t *testing.T) {
	checker := testPreflightChecker(`{"backend":"dolt","dolt_mode":"server","dolt_database":"gascity","project_id":"gc-local"}`, PreflightBDContext{Backend: "dolt", DoltMode: "server", BDVersion: "1.3.2", SchemaVersion: 1}, "gc-local")
	checker.BeadsLibraryVersion = "1.3.2"
	checker.BDContext = func(string) (PreflightBDContext, error) {
		return PreflightBDContext{}, errors.New("context unavailable")
	}
	result, err := checker.Check("/city")
	if err != nil {
		t.Fatal(err)
	}
	if result.NativeStoreEligible || result.NativeEligibleViaIdentityFallback || result.Verdict == PreflightVerdictEligible {
		t.Fatalf("project identity promoted an unbound CLI source: %+v", result)
	}
	if check := findPreflightCheck(t, result, PreflightCheckIdentityMatch); check.State != PreflightCheckPass {
		t.Fatalf("fixture did not independently verify project identity: %+v", check)
	}
	assertPreflightReadOnly(t, checker.FS.(*fsys.Fake))
}
