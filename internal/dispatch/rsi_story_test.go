package dispatch

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/rsipolicy"
	"github.com/gastownhall/gascity/internal/storybench"
)

const signedStorySuite = `{"version":1,"id":"signed-story-v1","cases":[
 {"id":"critical","story_id":"TEST-1","objective":"retain an exit","prompt":"exit requested","critical":true,"required_actions":["exit"],"forbidden_actions":["trap"],"max_actions":3},
 {"id":"ordinary","story_id":"TEST-2","objective":"retain choice","prompt":"choice requested","critical":false,"required_actions":["choice"],"forbidden_actions":[],"max_actions":3}
]}`

func TestRSIGateSignedStoryEvidence(t *testing.T) {
	for _, kind := range []string{"valid", "critical failure", "regression", "no improvement", "partial references", "missing references", "missing file", "tampered file", "wrong suite", "wrong bundle", "wrong capturer", "execution mismatch", "output mismatch", "write permission", "unknown trace field", "policy disagreement", "human pending"} {
		t.Run(kind, func(t *testing.T) {
			store, gate := createRSIGateInputs(t, true)
			pair := testBundlePair()
			pair.Current.EvalSuiteHash = storybench.SuiteHash([]byte(signedStorySuite))
			pair.Candidate.EvalSuiteHash = pair.Current.EvalSuiteHash
			prior := rsiRequestFromStore(t, store, gate)
			setRSIWorkerEvidence(t, store, prior.Candidate.BeadID, "improver", "session-improver", mustJSON(t, rsipolicy.CandidateProposal{Candidate: pair.Candidate}))
			baseline := storybench.Run{SuiteSHA256: pair.Current.EvalSuiteHash, BundleID: pair.Current.ID, CapturedBy: "capture-actor", Cases: []storybench.Trace{{CaseID: "critical", Actions: []string{"exit"}}, {CaseID: "ordinary", Actions: []string{}}}}
			candidate := storybench.Run{SuiteSHA256: pair.Current.EvalSuiteHash, BundleID: pair.Candidate.ID, CapturedBy: "capture-actor", Cases: []storybench.Trace{{CaseID: "critical", Actions: []string{"exit"}}, {CaseID: "ordinary", Actions: []string{"choice"}}}}
			switch kind {
			case "critical failure":
				candidate.Cases[0].Actions = []string{"exit", "trap"}
			case "regression":
				baseline.Cases[1].Actions = []string{"choice"}
				candidate.Cases[1].Actions = []string{}
			case "no improvement":
				baseline.Cases[1].Actions = []string{"choice"}
			case "wrong bundle":
				candidate.BundleID = "unrelated-bundle"
			case "wrong capturer":
				candidate.CapturedBy = "unbound-actor"
			}
			raw := mustJSON(t, storybench.Evidence{Baseline: baseline, Candidate: candidate})
			if kind == "unknown trace field" {
				raw = raw[:len(raw)-1] + `,"unexpected":true}`
			}
			benchmark := mustCreate(t, store, beads.Bead{Title: "benchmark", Metadata: map[string]string{beadmeta.RSIRoleMetadataKey: beadmeta.RSIRoleBenchmark}})
			setRSIWorkerEvidence(t, store, benchmark.ID, "capture-actor", "capture-session", raw)
			mustDep(t, store, gate.ID, benchmark.ID, "blocks")
			// Mutable metadata cannot waive evidence required by the signed evaluator.
			if err := store.Update(gate.ID, beads.UpdateOpts{Metadata: map[string]string{beadmeta.RSIStoryRequiredMetadataKey: "false"}}); err != nil {
				t.Fatal(err)
			}
			gate = mustGet(t, store, gate.ID)
			request := rsiRequestFromStore(t, store, gate)
			city := t.TempDir()
			dir := rsiGateEvidenceDir(city, gate.ID)
			resolver := writeSignedRSIEvaluation(t, city, request, pair, kind != "human pending", func(m *rsipolicy.TrustedEvaluationManifest) {
				m.StoryBenchmarkRequired = true
				m.AuthorityClass = "safety_policy"
				record := request.Benchmark
				m.Benchmark = &rsipolicy.BenchmarkAuthorization{BeadID: record.BeadID, BeadRevision: record.BeadRevision, ControlBeadID: record.ControlBeadID, ControlRevision: record.ControlRevision, ActorID: record.ActorID, SessionID: record.SessionID, OutputSHA256: digestRSI([]byte(raw)), Permissions: rsipolicy.ExecutionPermissions{CandidateRead: true, PolicyRead: true, OwnOutputWrite: true}}
				publishRSIFile(t, filepath.Join(dir, "story-suite.json"), []byte(signedStorySuite))
				publishRSIFile(t, filepath.Join(dir, "story-traces.json"), []byte(raw))
				suiteRef := rsipolicy.EvidenceReference{Kind: "story-suite", Path: "story-suite.json", SHA256: digestRSI([]byte(signedStorySuite)), EvalSuiteHash: pair.Current.EvalSuiteHash}
				traceRef := rsipolicy.EvidenceReference{Kind: "story-traces", Path: "story-traces.json", SHA256: digestRSI([]byte(raw)), BundleID: pair.Candidate.ID, EvalSuiteHash: pair.Current.EvalSuiteHash}
				if kind != "missing references" && kind != "policy disagreement" {
					m.Evidence = append(m.Evidence, suiteRef)
				}
				if kind != "partial references" && kind != "missing references" && kind != "policy disagreement" {
					m.Evidence = append(m.Evidence, traceRef)
				}
				switch kind {
				case "execution mismatch":
					m.Benchmark.SessionID = "other-session"
				case "output mismatch":
					m.Benchmark.OutputSHA256 = digestRSI([]byte("different"))
				case "write permission":
					m.Benchmark.Permissions.CandidateWrite = true
				case "wrong suite":
					m.Evidence[len(m.Evidence)-2].SHA256 = digestRSI([]byte("wrong-suite"))
				case "policy disagreement":
					m.StoryBenchmarkRequired = false
					m.Benchmark = nil
				}
			})
			if kind == "missing file" {
				if err := os.Remove(filepath.Join(dir, "story-traces.json")); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "tampered file" {
				publishRSIFile(t, filepath.Join(dir, "story-traces.json"), []byte("{}"))
			}
			result, err := ProcessControl(store, gate, rsiProcessOptions(gate, resolver))
			if kind == "human pending" {
				if !errors.Is(err, ErrControlPending) || result.Processed || mustGet(t, store, gate.ID).Status != "open" {
					t.Fatalf("human gate = %+v, %v", result, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			expected := "rsi-reject"
			if kind == "valid" {
				expected = "rsi-promote"
			}
			if result.Action != expected {
				t.Fatalf("action = %q, want %q", result.Action, expected)
			}
			if kind == "valid" {
				// The signed request cannot be replayed after a benchmark revision changes.
				request.Benchmark.BeadRevision++
				request.Context.InputSHA256 = rsipolicy.ResolveRequestInputSHA256(request)
				if _, err := resolver(context.Background(), request); err == nil {
					t.Fatal("changed request identity accepted")
				}
			}
		})
	}
}

func TestRSIStoryInputHashBindsExecutionAndRequirement(t *testing.T) {
	request := rsipolicy.ResolveRequest{Benchmark: &rsipolicy.BenchmarkRecord{BeadID: "benchmark", BeadRevision: 1, RawOutput: "traces"}}
	original := rsipolicy.ResolveRequestInputSHA256(request)
	request.Benchmark.RawOutput = "changed"
	if rsipolicy.ResolveRequestInputSHA256(request) == original {
		t.Fatal("benchmark output omitted from request identity")
	}
	request.Benchmark.RawOutput = "traces"
	request.StoryBenchmarkRequired = true
	if rsipolicy.ResolveRequestInputSHA256(request) == original {
		t.Fatal("story requirement omitted from request identity")
	}
}
