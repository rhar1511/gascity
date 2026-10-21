package contract

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestBoundCLIArtifactIdentityAndWireProtocol(t *testing.T) {
	// Synthetic checksums exercise the pure comparison only, not release authenticity.
	a := BeadsArtifact{ModulePath: beadsModulePath, Version: "v1.3.2", Sum: "h1:" + strings.Repeat("A", 43) + "="}
	tests := []struct {
		name               string
		change             func(*BeadsArtifact, *BeadsArtifact, *PreflightBDContext, *error)
		state              PreflightCheckState
		eligible, fallback bool
	}{
		{"matched release", nil, PreflightCheckPass, true, false},
		{"short major", func(l, c *BeadsArtifact, _ *PreflightBDContext, _ *error) { l.Version = "v1"; c.Version = "v1" }, PreflightCheckWarn, false, false},
		{"short minor", func(l, c *BeadsArtifact, _ *PreflightBDContext, _ *error) { l.Version = "v1.3"; c.Version = "v1.3" }, PreflightCheckWarn, false, false},
		{"unprefixed", func(l, c *BeadsArtifact, _ *PreflightBDContext, _ *error) { l.Version = "1.3.2"; c.Version = "1.3.2" }, PreflightCheckWarn, false, false},
		{"build metadata", func(l, c *BeadsArtifact, _ *PreflightBDContext, _ *error) {
			l.Version = "v1.3.2+example"
			c.Version = "v1.3.2+example"
		}, PreflightCheckWarn, false, false},
		{"display string is not source", func(_, _ *BeadsArtifact, ctx *PreflightBDContext, _ *error) { ctx.BDVersion = "99.0.0" }, PreflightCheckPass, true, false},
		{"different family same version", func(_, c *BeadsArtifact, _ *PreflightBDContext, _ *error) {
			c.ModulePath = "example.com/unqualified/beads"
		}, PreflightCheckWarn, false, false},
		{"different family newer display", func(_, c *BeadsArtifact, ctx *PreflightBDContext, _ *error) {
			c.ModulePath = "example.com/unqualified/beads"
			ctx.BDVersion = "1.3.3"
		}, PreflightCheckWarn, false, false},
		{"different exact release", func(_, c *BeadsArtifact, _ *PreflightBDContext, _ *error) { c.Version = "v1.3.3" }, PreflightCheckFail, false, false},
		{"different content", func(_, c *BeadsArtifact, _ *PreflightBDContext, _ *error) {
			c.Sum = "h1:" + strings.Repeat("B", 43) + "="
		}, PreflightCheckFail, false, false},
		{"missing CLI source", func(_, c *BeadsArtifact, _ *PreflightBDContext, _ *error) { *c = BeadsArtifact{} }, PreflightCheckWarn, false, false},
		{"missing library source", func(l, _ *BeadsArtifact, _ *PreflightBDContext, _ *error) { *l = BeadsArtifact{} }, PreflightCheckWarn, false, false},
		{"CLI replacement", func(_, c *BeadsArtifact, _ *PreflightBDContext, _ *error) { c.Replaced = true }, PreflightCheckWarn, false, false},
		{"library replacement", func(l, _ *BeadsArtifact, _ *PreflightBDContext, _ *error) { l.Replaced = true }, PreflightCheckWarn, false, false},
		{"devel", func(l, _ *BeadsArtifact, _ *PreflightBDContext, _ *error) { l.Version = "(devel)" }, PreflightCheckWarn, false, false},
		{"pseudo", func(l, _ *BeadsArtifact, _ *PreflightBDContext, _ *error) {
			l.Version = "v1.3.2-0.20261013010644-3c3423cf09a9"
		}, PreflightCheckWarn, false, false},
		{"invalid checksum", func(_, c *BeadsArtifact, _ *PreflightBDContext, _ *error) { c.Sum = "h1:fake" }, PreflightCheckWarn, false, false},
		{"positive wire is not enough", func(_, _ *BeadsArtifact, ctx *PreflightBDContext, _ *error) { ctx.SchemaVersion = 73 }, PreflightCheckFail, false, false},
		{"missing wire", func(_, _ *BeadsArtifact, ctx *PreflightBDContext, _ *error) { ctx.SchemaVersion = 0 }, PreflightCheckFail, false, false},
		{"matched source context unavailable", func(_, _ *BeadsArtifact, _ *PreflightBDContext, e *error) { *e = errors.New("context unavailable") }, PreflightCheckPass, true, true},
		{"unknown source context unavailable", func(_, c *BeadsArtifact, _ *PreflightBDContext, e *error) {
			*c = BeadsArtifact{}
			*e = errors.New("context unavailable")
		}, PreflightCheckWarn, false, false},
		{"malformed protocol", func(_, _ *BeadsArtifact, _ *PreflightBDContext, e *error) {
			*e = fmt.Errorf("%w: invalid JSON", ErrBDContextProtocol)
		}, PreflightCheckFail, false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			linked, cli := a, a
			ctx := PreflightBDContext{Backend: "dolt", DoltMode: "server", BDVersion: "1.3.2", SchemaVersion: 1}
			var contextErr error
			if tt.change != nil {
				tt.change(&linked, &cli, &ctx, &contextErr)
			}
			ctx.CLIArtifact = cli
			checker := testPreflightChecker(`{"backend":"dolt","dolt_mode":"server","dolt_database":"gascity","project_id":"gc-local"}`, ctx, "gc-local")
			checker.BeadsLibraryArtifact = &linked
			checker.BDContext = func(string) (PreflightBDContext, error) { return ctx, contextErr }
			r, err := checker.Check("/city")
			if err != nil {
				t.Fatal(err)
			}
			if c := findPreflightCheck(t, r, PreflightCheckVersionCompat); c.State != tt.state {
				t.Fatalf("compatibility=%s want%s: %+v", c.State, tt.state, r)
			}
			if r.NativeStoreEligible != tt.eligible || r.NativeEligibleViaIdentityFallback != tt.fallback {
				t.Fatalf("unexpected eligibility/fallback: %+v", r)
			}
		})
	}
}
