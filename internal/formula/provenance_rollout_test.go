package formula_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/featureflags"
	"github.com/gastownhall/gascity/internal/formula"
	"github.com/gastownhall/gascity/internal/rollout"
)

func TestCompileImplementationVersionIsSeparateFromRequirementCapability(t *testing.T) {
	t.Setenv("GC_FORMULA_REF", "")
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "legacy.toml"), []byte(`formula = "legacy"
version = 1

[[steps]]
id = "task"
title = "Task"
`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name       string
		enabled    bool
		capability string
	}{
		{"disabled", false, "1.0.0"},
		{"enabled", true, "2.0.0"},
	} {
		t.Run(test.name, func(t *testing.T) {
			flags := rollout.ForTest(rollout.WithFormulaV2(test.enabled))
			featureflags.WithScoped(featureflags.Flags{FormulaV2: flags.FormulaV2(), GraphApply: flags.FormulaV2()}, func() {
				_, provenance, err := formula.CompileWithProvenance(context.Background(), "legacy", []string{dir}, nil)
				if err != nil {
					t.Fatal(err)
				}
				if provenance.FormulaV2Enabled != test.enabled || provenance.CompilerCapability != test.capability {
					t.Fatalf("compiler mode/capability = %t/%q, want %t/%q", provenance.FormulaV2Enabled, provenance.CompilerCapability, test.enabled, test.capability)
				}
				if provenance.CompilerImplementationVersion != formula.FormulaCompilerImplementationVersion {
					t.Fatalf("implementation version = %q, want %q", provenance.CompilerImplementationVersion, formula.FormulaCompilerImplementationVersion)
				}
				if provenance.CompilerImplementationVersion == provenance.CompilerCapability {
					t.Fatalf("implementation version and requirement capability must be distinct: %q", provenance.CompilerCapability)
				}
			})
		})
	}
}
