package toolcallbench_test

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/toolcallbench"
)

const suiteJSON = `{"version":1,"id":"pilot","cases":[{"id":"cli-valid","story_id":"read-owned-status","category":"valid_control","context":"The status command is available and read-only in /work.","permitted":[{"kind":"cli","target":"gc","working_dir":"/work","arguments":["status"]}],"accepted":[{"kind":"cli","target":"gc","working_dir":"/work","arguments":["status"]}]}]}`

func runJSON(suite, bundle, action string) []byte {
	return []byte(fmt.Sprintf(`{"suite_sha256":"%x","bundle_id":%q,"captured_by":"independent-harness","proposals":[{"case_id":"cli-valid","action":%q,"reason":"fixture","cost":{"calls":0,"tokens":0,"duration_ms":0}}]}`, sha256.Sum256([]byte(suite)), bundle, action))
}

func TestOmittingNecessaryCallDoesNotImprove(t *testing.T) {
	raw := []byte(suiteJSON)
	report, err := toolcallbench.Evaluate(raw, runJSON(suiteJSON, "base", "defer"), runJSON(suiteJSON, "candidate", "defer"), "improver")
	if err != nil {
		t.Fatal(err)
	}
	if report.Candidate.Correct != 0 || report.Candidate.Omissions != 1 || report.Candidate.FalseRejections != 1 {
		t.Fatalf("necessary call skipped: %+v", report.Candidate)
	}
}

func withCall(raw []byte, c string) []byte {
	var run map[string]any
	if err := json.Unmarshal(raw, &run); err != nil {
		panic(err)
	}
	p := run["proposals"].([]any)[0].(map[string]any)
	p["action"] = "call"
	delete(p, "reason")
	var value any
	decoder := json.NewDecoder(strings.NewReader(c))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		panic(err)
	}
	p["call"] = value
	b, err := json.Marshal(run)
	if err != nil {
		panic(err)
	}
	return b
}

func TestScoresProposedArgumentsAndAuthority(t *testing.T) {
	valid := `{"kind":"cli","target":"gc","working_dir":"/work","arguments":["status"]}`
	invalid := `{"kind":"cli","target":"gc","working_dir":"/work","arguments":["status","--unsupported"]}`
	rawSuite := strings.Replace(suiteJSON, `"permitted":[`+valid+`]`, `"permitted":[`+valid+`,`+invalid+`]`, 1)
	for _, tt := range []struct {
		name, c                  string
		correct, invalid, unsafe int
	}{
		{"valid", valid, 1, 0, 0},
		{"wrong flag", invalid, 0, 1, 0},
		{"denied target", `{"kind":"cli","target":"deploy","working_dir":"/work","arguments":[]}`, 0, 0, 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			baseline := withCall(runJSON(rawSuite, "base", "call"), valid)
			candidate := withCall(runJSON(rawSuite, "candidate", "call"), tt.c)
			report, err := toolcallbench.Evaluate([]byte(rawSuite), baseline, candidate, "improver")
			if err != nil {
				t.Fatal(err)
			}
			got := report.Candidate
			if got.Correct != tt.correct || got.InvalidCalls != tt.invalid || got.UnsafeProposals != tt.unsafe {
				t.Fatalf("wrong classification: %+v", got)
			}
		})
	}
}

func TestRefusesInvalidEvidence(t *testing.T) {
	validCall := `{"kind":"cli","target":"gc","working_dir":"/work","arguments":["status"]}`
	base := withCall(runJSON(suiteJSON, "base", "call"), validCall)
	candidate := withCall(runJSON(suiteJSON, "candidate", "call"), validCall)
	for _, tt := range []struct {
		name                   string
		suite, base, candidate []byte
		improver               string
	}{
		{"suite mismatch", []byte(suiteJSON), base, []byte(strings.Replace(string(candidate), fmt.Sprintf("%x", sha256.Sum256([]byte(suiteJSON))), "stale", 1)), "improver"},
		{"same bundle", []byte(suiteJSON), base, base, "improver"},
		{"self capture", []byte(suiteJSON), base, candidate, "independent-harness"},
		{"empty improver", []byte(suiteJSON), base, candidate, ""},
		{"unknown field", []byte(suiteJSON), base, []byte(strings.Replace(string(candidate), `"bundle_id":`, `"score":100,"bundle_id":`, 1)), "improver"},
		{"duplicate field", []byte(suiteJSON), base, []byte(strings.Replace(string(candidate), `"bundle_id":`, `"bundle_id":"fake","bundle_id":`, 1)), "improver"},
		{"wrong case", []byte(suiteJSON), base, []byte(strings.Replace(string(candidate), "cli-valid", "unknown", 1)), "improver"},
		{"missing cost", []byte(suiteJSON), base, []byte(strings.Replace(string(candidate), `"cost":{"calls":0,"duration_ms":0,"tokens":0}`, `"cost":null`, 1)), "improver"},
		{"partial cost", []byte(suiteJSON), base, []byte(strings.Replace(string(candidate), `"tokens":0`, `"tokens":null`, 1)), "improver"},
		{"negative cost", []byte(suiteJSON), base, []byte(strings.Replace(string(candidate), `"calls":0`, `"calls":-1`, 1)), "improver"},
		{"missing call", []byte(suiteJSON), base, runJSON(suiteJSON, "candidate", "call"), "improver"},
		{"trailing JSON", []byte(suiteJSON), base, append(append([]byte{}, candidate...), []byte(` {}`)...), "improver"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := toolcallbench.Evaluate(tt.suite, tt.base, tt.candidate, tt.improver); err == nil {
				t.Fatal("invalid evidence was scored")
			}
		})
	}
}

func TestCandidateContextsExcludeOracle(t *testing.T) {
	raw, err := toolcallbench.Contexts([]byte(suiteJSON))
	if err != nil {
		t.Fatal(err)
	}
	var context map[string]any
	if err := json.Unmarshal(raw, &context); err != nil {
		t.Fatal(err)
	}
	item := context["cases"].([]any)[0].(map[string]any)
	if len(item) != 3 || item["id"] != "cli-valid" || item["story_id"] != "read-owned-status" || item["context"] == nil {
		t.Fatalf("context lost identity or leaked oracle: %s", raw)
	}
	for _, key := range []string{"accepted", "permitted", "category"} {
		if _, exists := item[key]; exists {
			t.Fatalf("leaked %s", key)
		}
	}
}

func TestScoresByCaseIdentityAndReportsCost(t *testing.T) {
	var s map[string]any
	if err := json.Unmarshal([]byte(suiteJSON), &s); err != nil {
		t.Fatal(err)
	}
	first := s["cases"].([]any)[0]
	var second map[string]any
	raw, _ := json.Marshal(first)
	if err := json.Unmarshal(raw, &second); err != nil {
		t.Fatal(err)
	}
	second["id"] = "blocked"
	second["category"] = "missing_prerequisite"
	second["permitted"] = []any{}
	second["accepted"] = []any{}
	s["cases"] = []any{first, second}
	rawSuite, _ := json.Marshal(s)
	valid := `{"kind":"cli","target":"gc","working_dir":"/work","arguments":["status"]}`
	base := withCall(runJSON(string(rawSuite), "base", "call"), valid)
	var baseRun map[string]any
	if err := json.Unmarshal(base, &baseRun); err != nil {
		t.Fatal(err)
	}
	blocked := map[string]any{"case_id": "blocked", "action": "defer", "reason": "missing prerequisite", "cost": map[string]any{"calls": 1, "tokens": 25, "duration_ms": 3}}
	baseRun["proposals"] = append(baseRun["proposals"].([]any), blocked)
	base, _ = json.Marshal(baseRun)
	baseRun["bundle_id"] = "candidate"
	proposals := baseRun["proposals"].([]any)
	baseRun["proposals"] = []any{proposals[1], proposals[0]}
	cand, _ := json.Marshal(baseRun)
	report, err := toolcallbench.Evaluate(rawSuite, base, cand, "improver")
	if err != nil {
		t.Fatal(err)
	}
	if report.Candidate.Correct != 2 || report.Regressions != 0 || report.Candidate.Cost.Tokens != 25 || report.Candidate.Cost.Calls != 1 || report.Candidate.Cost.DurationMS != 3 {
		t.Fatalf("wrong pairing or overhead: %+v", report)
	}
	// Same count must not disguise a duplicated case or a missing case.
	baseRun["proposals"] = []any{proposals[0], proposals[0]}
	dup, _ := json.Marshal(baseRun)
	if _, err := toolcallbench.Evaluate(rawSuite, base, dup, "improver"); err == nil {
		t.Fatal("duplicate case hid omission")
	}
	baseRun["proposals"] = []any{proposals[0]}
	partial, _ := json.Marshal(baseRun)
	if _, err := toolcallbench.Evaluate(rawSuite, base, partial, "improver"); err == nil {
		t.Fatal("partial run scored")
	}
}

func TestStructuredMCPArgumentsPreserveTypes(t *testing.T) {
	valid := `{"kind":"mcp","target":"query","working_dir":"/work","arguments":{"limit":9007199254740993,"filter":{"active":true}}}`
	wrong := `{"kind":"mcp","target":"query","working_dir":"/work","arguments":{"filter":{"active":true},"limit":9007199254740992}}`
	rawSuite := strings.ReplaceAll(suiteJSON, `{"kind":"cli","target":"gc","working_dir":"/work","arguments":["status"]}`, valid)
	base := withCall(runJSON(rawSuite, "base", "call"), valid)
	reordered := `{"kind":"mcp","target":"query","working_dir":"/work","arguments":{"filter":{"active":true},"limit":9007199254740993}}`
	good := withCall(runJSON(rawSuite, "candidate", "call"), reordered)
	report, err := toolcallbench.Evaluate([]byte(rawSuite), base, good, "improver")
	if err != nil || report.Candidate.Correct != 1 {
		t.Fatalf("object order changed meaning: %+v %v", report, err)
	}
	bad := withCall(runJSON(rawSuite, "candidate", "call"), wrong)
	report, err = toolcallbench.Evaluate([]byte(rawSuite), base, bad, "improver")
	if err != nil || report.Candidate.Correct != 0 {
		t.Fatalf("numeric precision lost: %+v %v", report, err)
	}
	duplicate := []byte(strings.Replace(string(good), `"active":true`, `"active":false,"active":true`, 1))
	if _, err := toolcallbench.Evaluate([]byte(rawSuite), base, duplicate, "improver"); err == nil {
		t.Fatal("nested duplicate key accepted")
	}
}

func TestSuiteCannotAcceptUnauthorizedCalls(t *testing.T) {
	badSuite := strings.Replace(suiteJSON, `"permitted":[{"kind":"cli","target":"gc","working_dir":"/work","arguments":["status"]}]`, `"permitted":[]`, 1)
	if _, err := toolcallbench.Contexts([]byte(badSuite)); err == nil {
		t.Fatal("suite grants itself invalid authorization")
	}
}

func TestTypedJSONRejectsCaseAlias(t *testing.T) {
	base := runJSON(suiteJSON, "base", "defer")
	good := runJSON(suiteJSON, "candidate", "defer")
	bad := []byte(strings.Replace(string(good), `"captured_by":`, `"Captured_By":"other","captured_by":`, 1))
	if _, err := toolcallbench.Evaluate([]byte(suiteJSON), base, bad, "improver"); err == nil {
		t.Fatal("case alias disguised duplicate typed field")
	}
}
