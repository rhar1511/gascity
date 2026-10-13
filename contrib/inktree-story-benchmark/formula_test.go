package storybenchmark

import (
	"context"
	_ "embed"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/formula"
)

//go:embed formulas/mol-rsi-story-candidate.toml
var storyFormula []byte

//go:embed testdata/mol-rsi-candidate.toml
var baseFormula []byte

func TestStoryFormulaPreservesIndependentBenchmarkAndHumanBoundary(t *testing.T) {
	dir := t.TempDir()
	for name, raw := range map[string][]byte{"mol-rsi-candidate.toml": baseFormula, "mol-rsi-story-candidate.toml": storyFormula} {
		if err := os.WriteFile(filepath.Join(dir, name), raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	parser := formula.NewParser(dir)
	child, err := parser.LoadByName("mol-rsi-story-candidate")
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := parser.Resolve(child)
	if err != nil {
		t.Fatal(err)
	}
	if len(resolved.Steps) != 5 {
		t.Fatalf("resolved steps = %d, want 5", len(resolved.Steps))
	}
	if authority := resolved.Vars["authority_class"]; authority == nil || !authority.Required || authority.Default != nil {
		t.Fatalf("authority must be supplied explicitly: %+v", authority)
	}
	benchmarkFound, gateFound := false, false
	for _, step := range resolved.Steps {
		switch step.ID {
		case "judge-story-benchmark":
			benchmarkFound = true
			for _, phrase := range []string{"harness independent", "{{benchmark_command}}", "candidate-authored scores", "evaluator-signed manifest", "separate human signature"} {
				if !strings.Contains(step.Description, phrase) {
					t.Fatalf("benchmark lacks %q", phrase)
				}
			}
		case "promote-gate":
			gateFound = true
			if step.Metadata["gc.rsi_story_benchmark_required"] != "true" {
				t.Fatalf("gate does not require all independent judges: %+v", step)
			}
			for _, phrase := range []string{"critical case", "story regression", "human approval gate"} {
				if !strings.Contains(step.Description, phrase) {
					t.Fatalf("gate lacks %q", phrase)
				}
			}
		}
	}
	if !benchmarkFound || !gateFound {
		t.Fatal("resolved graph lost benchmark or promotion gate")
	}
	recipe, err := formula.Compile(context.Background(), "mol-rsi-story-candidate", []string{dir}, map[string]string{
		"objective": "improve stopping cues", "work_dir": filepath.Join(dir, "work"),
		"rig_root": filepath.Join(dir, "rig"), "current_bundle_id": "bundle-1",
		"eval_suite_hash": "suite-hash", "improver_target": "improver",
		"correctness_target": "correctness", "performance_target": "performance",
		"story_suite_path": filepath.Join(dir, "suite.json"), "benchmark_target": "benchmark",
		"benchmark_command": filepath.Join(dir, "rsistorybench"), "authority_class": "workflow",
	})
	if err != nil {
		t.Fatalf("compile story formula: %v", err)
	}
	gateID := ""
	for _, step := range recipe.Steps {
		if step.Metadata["gc.kind"] == "rsi-promotion-gate" {
			if gateID != "" {
				t.Fatal("compiled graph has multiple promotion gates")
			}
			gateID = step.ID
		}
	}
	if gateID == "" {
		t.Fatal("compiled graph lost the executable promotion gate")
	}
	want := map[string]bool{}
	for _, id := range []string{"produce-candidate", "judge-correctness", "judge-performance", "judge-story-benchmark"} {
		want[recipe.Name+"."+id] = true
	}
	for _, dep := range recipe.Deps {
		if dep.StepID != gateID || dep.Type != "blocks" {
			continue
		}
		if !want[dep.DependsOnID] {
			t.Fatalf("unexpected gate input: %+v", dep)
		}
		delete(want, dep.DependsOnID)
	}
	if len(want) != 0 {
		t.Fatalf("compiled gate lacks direct evidence dependencies: %v", want)
	}
}
