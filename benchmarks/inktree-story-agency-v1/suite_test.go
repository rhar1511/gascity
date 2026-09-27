package agencybench

import (
	_ "embed"
	"encoding/json"
	"testing"

	"github.com/gastownhall/gascity/internal/storybench"
)

//go:embed suite.json
var rawSuite []byte

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
