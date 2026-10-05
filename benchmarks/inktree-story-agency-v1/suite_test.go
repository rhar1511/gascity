package agencybench

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/gastownhall/gascity/internal/storybench"
)

//go:embed suite.json
var rawSuite []byte

//go:embed suiteV2.json
var rawSuiteV2 []byte

//go:embed baseline.example.json
var rawBaseline []byte

//go:embed candidate.example.json
var rawCandidate []byte

func exampleRuns(t *testing.T) (storybench.Run, storybench.Run) {
	t.Helper()
	var baseline, candidate storybench.Run
	if err := json.Unmarshal(rawBaseline, &baseline); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(rawCandidate, &candidate); err != nil {
		t.Fatal(err)
	}
	return baseline, candidate
}

func TestPilotSuiteAndExampleRuns(t *testing.T) {
	baseline, candidate := exampleRuns(t)
	result, err := storybench.Evaluate(rawSuite, baseline, candidate, "demo-improver")
	if err != nil {
		t.Fatal(err)
	}
	if !result.Eligible || len(result.Cases) != 12 || result.BaselinePassed != 11 || result.CandidatePassed != 12 {
		t.Fatalf("pilot fixture drift: %+v", result)
	}
}

func TestEveryCriticalCaseHasAnEffectiveVeto(t *testing.T) {
	var suite storybench.Suite
	if err := json.Unmarshal(rawSuite, &suite); err != nil {
		t.Fatal(err)
	}
	critical := 0
	for _, c := range suite.Cases {
		if !c.Critical {
			continue
		}
		critical++
		if len(c.ForbiddenActions) == 0 {
			t.Fatalf("critical case %q has no forbidden action", c.ID)
		}
		t.Run(c.ID, func(t *testing.T) {
			baseline, candidate := exampleRuns(t)
			for i := range candidate.Cases {
				if candidate.Cases[i].CaseID == c.ID {
					candidate.Cases[i].Actions = append(candidate.Cases[i].Actions, c.ForbiddenActions[0])
				}
			}
			result, err := storybench.Evaluate(rawSuite, baseline, candidate, "demo-improver")
			if err != nil {
				t.Fatal(err)
			}
			if result.Eligible || result.CriticalFailures == 0 {
				t.Fatalf("critical violation did not veto: %+v", result)
			}
		})
	}
	if critical < 7 {
		t.Fatalf("critical case coverage = %d, want at least 7", critical)
	}
}

func TestV2CoversEveryStoryAndCriticalVetoes(t *testing.T) {
	var suite storybench.Suite
	if err := json.Unmarshal(rawSuiteV2, &suite); err != nil {
		t.Fatal(err)
	}
	if suite.ID != "inktree-story-agency-v2" || len(suite.Cases) != 50 {
		t.Fatalf("V2 suite identity/coverage: id=%q cases=%d", suite.ID, len(suite.Cases))
	}
	hash := storybench.SuiteHash(rawSuiteV2)
	if hash == storybench.SuiteHash(rawSuite) {
		t.Fatal("V2 must have a distinct evaluation identity from the pilot")
	}
	baseline := storybench.Run{SuiteSHA256: hash, BundleID: "synthetic-baseline", CapturedBy: "fixture-harness"}
	candidate := storybench.Run{SuiteSHA256: hash, BundleID: "synthetic-candidate", CapturedBy: "fixture-harness"}
	seen := make(map[string]bool, 50)
	critical := 0
	for _, c := range suite.Cases {
		if seen[c.StoryID] {
			t.Fatalf("duplicate story %q", c.StoryID)
		}
		seen[c.StoryID] = true
		if c.MaxActions < len(c.RequiredActions) || len(c.ForbiddenActions) == 0 {
			t.Fatalf("case %q has ineffective action assertions", c.ID)
		}
		if c.Critical {
			critical++
		}
		trace := storybench.Trace{CaseID: c.ID, Actions: append([]string(nil), c.RequiredActions...), ElapsedMS: 1}
		baseline.Cases = append(baseline.Cases, trace)
		candidate.Cases = append(candidate.Cases, trace)
	}
	for i := 1; i <= 50; i++ {
		if !seen[fmt.Sprintf("INK-%03d", i)] {
			t.Fatalf("missing story INK-%03d", i)
		}
	}
	if critical < 30 {
		t.Fatalf("critical story coverage = %d, want at least 30", critical)
	}
	result, err := storybench.Evaluate(rawSuiteV2, baseline, candidate, "candidate-author")
	if err != nil {
		t.Fatal(err)
	}
	if result.Eligible || result.BaselinePassed != 50 || result.CandidatePassed != 50 || result.SuiteHash != hash {
		t.Fatalf("synthetic equal traces should be valid but ineligible: %+v", result)
	}
	for i, c := range suite.Cases {
		if !c.Critical {
			continue
		}
		t.Run(c.StoryID, func(t *testing.T) {
			violating := candidate
			violating.Cases = append([]storybench.Trace(nil), candidate.Cases...)
			violating.Cases[i].Actions = append(append([]string(nil), candidate.Cases[i].Actions...), c.ForbiddenActions[0])
			result, err := storybench.Evaluate(rawSuiteV2, baseline, violating, "candidate-author")
			if err != nil {
				t.Fatal(err)
			}
			if result.Eligible || result.CriticalFailures != 1 || result.Regressions != 1 {
				t.Fatalf("critical action did not veto: %+v", result)
			}
		})
	}
}
