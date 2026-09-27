package dispatch

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/formula"
	"github.com/gastownhall/gascity/internal/molecule"
	"github.com/gastownhall/gascity/internal/reviewquorum"
	"github.com/gastownhall/gascity/internal/rsipolicy"
)

func TestCompiledRSIFormulaWiresTrustedGateToCandidateAndBothJudges(t *testing.T) {
	recipe := compileRSIRecipe(t)
	gateID := "mol-rsi-candidate.promote-gate"
	want := []string{
		"mol-rsi-candidate.produce-candidate",
		"mol-rsi-candidate.judge-correctness",
		"mol-rsi-candidate.judge-performance",
	}
	got := map[string]bool{}
	for _, dep := range recipe.Deps {
		if dep.StepID == gateID && dep.Type == "blocks" {
			got[dep.DependsOnID] = true
		}
	}
	for _, id := range want {
		if !got[id] {
			t.Errorf("compiled gate lacks direct blocking dependency on %q; deps=%v", id, recipe.Deps)
		}
	}
}

func TestCompiledFormulaRuntimePromotesOnlyFromSignedTrustedEvaluation(t *testing.T) {
	recipe := compileRSIRecipe(t)
	store := beads.NewMemStore()
	result, err := molecule.Instantiate(context.Background(), store, recipe, molecule.Options{Vars: rsiFormulaVars()})
	if err != nil {
		t.Fatalf("instantiate compiled RSI formula: %v", err)
	}
	pair := testBundlePair()
	candidateRaw := forgedRSICandidateOutput(t, pair)
	candidateControlID := result.IDMapping["mol-rsi-candidate.produce-candidate"]
	candidateAttempt1ID := result.IDMapping["mol-rsi-candidate.produce-candidate.attempt.1"]
	judges := []rsipolicy.JudgeRecord{
		{BeadID: result.IDMapping["mol-rsi-candidate.judge-correctness.attempt.1"], ControlBeadID: result.IDMapping["mol-rsi-candidate.judge-correctness"], ActorID: "judge-correctness", SessionID: "session-correctness", Status: "closed", Outcome: "pass", Lane: passingRSILane("correctness", "judge-correctness")},
		{BeadID: result.IDMapping["mol-rsi-candidate.judge-performance.attempt.1"], ControlBeadID: result.IDMapping["mol-rsi-candidate.judge-performance"], ActorID: "judge-performance", SessionID: "session-performance", Status: "closed", Outcome: "pass", Lane: passingRSILane("performance", "judge-performance")},
	}
	for i := range judges {
		judges[i].RawOutput = mustJSON(t, judges[i].Lane)
	}
	setTransientRSIAttempt(t, store, candidateAttempt1ID, "improver", "session-improver")
	for _, judge := range judges {
		setRSIWorkerEvidence(t, store, judge.BeadID, judge.ActorID, judge.SessionID, judge.RawOutput)
	}
	formulaDir, err := filepath.Abs(filepath.Join("..", "bootstrap", "packs", "core", "formulas"))
	if err != nil {
		t.Fatal(err)
	}
	candidateID := processRSIRetryToSecondAttempt(t, store, candidateControlID, formulaDir, "improver", "session-improver", candidateRaw)
	for _, controlID := range []string{judges[0].ControlBeadID, judges[1].ControlBeadID} {
		processPassingRSIRetry(t, store, controlID)
	}

	cityPath := t.TempDir()
	resolver := writeSignedRSIEvaluation(t, cityPath, candidateID, candidateControlID, "improver", "session-improver", candidateRaw, judges, pair, true, func(m *rsipolicy.TrustedEvaluationManifest) { m.Attempt = 2 })
	actualCandidate := mustGet(t, store, candidateID)
	var candidateProposal rsipolicy.CandidateProposal
	if err := json.Unmarshal([]byte(candidateRaw), &candidateProposal); err != nil {
		t.Fatal(err)
	}
	candidateControl := mustGet(t, store, candidateControlID)
	if got := candidateControl.Metadata[beadmeta.ClosedByAttemptMetadataKey]; got != "2" {
		t.Fatalf("candidate retry gc.closed_by_attempt = %q, want successful attempt 2", got)
	}
	wrongClosedBy := candidateControl
	wrongClosedBy.Metadata = make(map[string]string, len(candidateControl.Metadata))
	for key, value := range candidateControl.Metadata {
		wrongClosedBy.Metadata[key] = value
	}
	wrongClosedBy.Metadata[beadmeta.ClosedByAttemptMetadataKey] = "1"
	if _, err := resolveRSIWorkerExecution(store, wrongClosedBy); err == nil {
		t.Fatal("retry control with gc.closed_by_attempt=1 accepted successful attempt 2")
	}
	if _, err := resolver(context.Background(), rsipolicy.ResolveRequest{Candidate: rsipolicy.CandidateRecord{
		BeadID: actualCandidate.ID, ControlBeadID: candidateControl.ID, Attempt: beadmeta.RetryAttemptNumber(actualCandidate.Metadata),
		MaxAttempts: retryMaxAttempts(candidateControl), ActorID: actualCandidate.Assignee,
		SessionID: actualCandidate.Metadata[beadmeta.SessionIDMetadataKey], Status: actualCandidate.Status,
		Outcome: actualCandidate.Metadata[beadmeta.OutcomeMetadataKey], RawOutput: candidateRaw, Proposal: candidateProposal,
	}, Judges: judges}); err != nil {
		t.Fatalf("resolve signed evaluation before dispatch: %v", err)
	}
	gateID := result.IDMapping["mol-rsi-candidate.promote-gate"]
	gate, err := store.Get(gateID)
	if err != nil {
		t.Fatalf("load compiled gate: %v", err)
	}
	processed, err := ProcessControl(store, gate, ProcessOptions{Context: context.Background(), ResolveRSIEvaluation: resolver})
	if err != nil {
		t.Fatalf("ProcessControl: %v", err)
	}
	if !processed.Processed || processed.Action != "rsi-promote" {
		t.Fatalf("ProcessControl = %+v, want rsi-promote; gate=%+v", processed, mustGet(t, store, gateID))
	}
	after, err := store.Get(gateID)
	if err != nil {
		t.Fatalf("reload gate: %v", err)
	}
	if after.Status != "closed" || after.Metadata[beadmeta.RSIPromoteMetadataKey] != "true" {
		t.Fatalf("gate status/promotion = %q/%q, want closed/true", after.Status, after.Metadata[beadmeta.RSIPromoteMetadataKey])
	}
	decision := readRSIDecision(t, after)
	if !decision.Promote || decision.RollbackBundleID != pair.Current.ID {
		t.Fatalf("decision = %+v, want promotion with baseline rollback", decision)
	}
}

func TestRSIGateFailsClosedWithoutControllerResolver(t *testing.T) {
	store, gate := createRSIGateInputs(t, false)
	processed, err := ProcessControl(store, gate, ProcessOptions{})
	if err != nil {
		t.Fatalf("ProcessControl: %v", err)
	}
	if processed.Action != "rsi-reject" {
		t.Fatalf("Action = %q, want fail-closed rejection", processed.Action)
	}
	after := mustGet(t, store, gate.ID)
	if after.Metadata[beadmeta.RSIReasonMetadataKey] != rsiTrustedEvaluationUnavailable || after.Metadata[beadmeta.RSIPromoteMetadataKey] != "false" {
		t.Fatalf("gate evidence = %v, want trusted evaluation unavailable and no promotion", after.Metadata)
	}
}

func TestRSIGateRequiresExactHumanSignatureForTrustedPromotion(t *testing.T) {
	store, gate := createRSIGateInputs(t, false)
	request := rsiRequestFromStore(t, store, gate)
	cityPath := t.TempDir()
	resolver := writeSignedRSIEvaluation(t, cityPath, request.Candidate.BeadID, request.Candidate.ControlBeadID, request.Candidate.ActorID, request.Candidate.SessionID, request.Candidate.RawOutput, request.Judges, testBundlePair(), false, nil)
	processed, err := ProcessControl(store, gate, ProcessOptions{ResolveRSIEvaluation: resolver})
	if !errors.Is(err, ErrControlPending) {
		t.Fatalf("ProcessControl error = %v, want pending human approval", err)
	}
	if processed.Processed {
		t.Fatalf("ProcessControl = %+v, want unprocessed pending gate", processed)
	}
	after := mustGet(t, store, gate.ID)
	if after.Status != "open" {
		t.Fatalf("gate status = %q, want open while approval is pending", after.Status)
	}
}

func TestRSIGateRetriesUntilEvaluationAndApprovalArtifactsArrive(t *testing.T) {
	store, gate := createRSIGateInputs(t, false)
	request := rsiRequestFromStore(t, store, gate)
	cityPath := t.TempDir()
	resolver := writeSignedRSIEvaluation(t, cityPath, request.Candidate.BeadID, request.Candidate.ControlBeadID, request.Candidate.ActorID, request.Candidate.SessionID, request.Candidate.RawOutput, request.Judges, testBundlePair(), true, nil)
	evidenceDir := filepath.Join(cityPath, ".gc", "rsi")
	evaluationPath := filepath.Join(evidenceDir, "evaluation.json")
	approvalPath := filepath.Join(evidenceDir, "approval.json")
	evaluation, err := os.ReadFile(evaluationPath)
	if err != nil {
		t.Fatal(err)
	}
	approval, err := os.ReadFile(approvalPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(evaluationPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(approvalPath); err != nil {
		t.Fatal(err)
	}

	processPending := func(want string) {
		t.Helper()
		result, processErr := ProcessControl(store, mustGet(t, store, gate.ID), ProcessOptions{Context: context.Background(), ResolveRSIEvaluation: resolver})
		if !errors.Is(processErr, ErrControlPending) {
			t.Fatalf("ProcessControl error = %v, want pending %s", processErr, want)
		}
		if result.Processed {
			t.Fatalf("ProcessControl = %+v, want pending %s", result, want)
		}
		if got := mustGet(t, store, gate.ID).Status; got != "open" {
			t.Fatalf("gate status = %q while %s is pending, want open", got, want)
		}
	}
	processPending("evaluation")
	if err := os.WriteFile(evaluationPath, evaluation, 0o600); err != nil {
		t.Fatal(err)
	}
	processPending("human approval")
	if err := os.WriteFile(approvalPath, approval, 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := ProcessControl(store, mustGet(t, store, gate.ID), ProcessOptions{Context: context.Background(), ResolveRSIEvaluation: resolver})
	if err != nil {
		t.Fatalf("ProcessControl after trusted artifacts arrived: %v", err)
	}
	if !result.Processed || result.Action != "rsi-promote" {
		t.Fatalf("ProcessControl after trusted artifacts = %+v, want rsi-promote", result)
	}
	if got := mustGet(t, store, gate.ID).Metadata[beadmeta.RSIPromoteMetadataKey]; got != "true" {
		t.Fatalf("promotion metadata = %q after valid evaluator and approval files, want true", got)
	}
}

func TestRSIGateRejectsCandidatePermissionToReadHeldOutDataOrSigningKey(t *testing.T) {
	for _, test := range []struct {
		name string
		edit func(*rsipolicy.TrustedEvaluationManifest)
	}{
		{name: "held-out evaluation data", edit: func(m *rsipolicy.TrustedEvaluationManifest) { m.CandidateExecution.Permissions.HeldOutDataRead = true }},
		{name: "evaluator signing key", edit: func(m *rsipolicy.TrustedEvaluationManifest) { m.CandidateExecution.Permissions.SigningKeyRead = true }},
		{name: "controller config", edit: func(m *rsipolicy.TrustedEvaluationManifest) {
			m.CandidateExecution.Permissions.ControllerConfigWrite = true
		}},
		{name: "authoritative policy", edit: func(m *rsipolicy.TrustedEvaluationManifest) { m.CandidateExecution.Permissions.PolicyWrite = true }},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, gate := createRSIGateInputs(t, true)
			request := rsiRequestFromStore(t, store, gate)
			cityPath := t.TempDir()
			resolver := writeSignedRSIEvaluation(t, cityPath, request.Candidate.BeadID, request.Candidate.ControlBeadID, request.Candidate.ActorID, request.Candidate.SessionID, request.Candidate.RawOutput, request.Judges, testBundlePair(), true, test.edit)
			processed, err := ProcessControl(store, gate, ProcessOptions{ResolveRSIEvaluation: resolver})
			if err != nil {
				t.Fatalf("ProcessControl: %v", err)
			}
			if processed.Action != "rsi-reject" {
				t.Fatalf("Action = %q, want rejection", processed.Action)
			}
			after := mustGet(t, store, gate.ID)
			if after.Metadata[beadmeta.RSIPromoteMetadataKey] != "false" || after.Metadata[beadmeta.RSIReasonMetadataKey] != rsiTrustedEvaluationUnavailable {
				t.Fatalf("gate decision = %v, want fail-closed rejection", after.Metadata)
			}
		})
	}
}

func TestRSIGateRejectsJudgePermissionToReadHeldOutData(t *testing.T) {
	store, gate := createRSIGateInputs(t, true)
	request := rsiRequestFromStore(t, store, gate)
	resolver := writeSignedRSIEvaluation(t, t.TempDir(), request.Candidate.BeadID, request.Candidate.ControlBeadID, request.Candidate.ActorID, request.Candidate.SessionID, request.Candidate.RawOutput, request.Judges, testBundlePair(), true, func(m *rsipolicy.TrustedEvaluationManifest) {
		m.Judges[0].Permissions.HeldOutDataRead = true
	})
	processed, err := ProcessControl(store, gate, ProcessOptions{ResolveRSIEvaluation: resolver})
	if err != nil {
		t.Fatalf("ProcessControl: %v", err)
	}
	if processed.Action != "rsi-reject" {
		t.Fatalf("Action = %q, want rejection", processed.Action)
	}
	after := mustGet(t, store, gate.ID)
	if after.Metadata[beadmeta.RSIPromoteMetadataKey] != "false" || after.Metadata[beadmeta.RSIReasonMetadataKey] != rsiTrustedEvaluationUnavailable {
		t.Fatalf("gate decision = %v, want reject held-out judge access", after.Metadata)
	}
}

func TestRSIGateRejectsEvaluatorAttemptBudgetNotMatchingControllerState(t *testing.T) {
	for _, test := range []struct {
		name string
		edit func(*rsipolicy.TrustedEvaluationManifest)
	}{
		{name: "attempt", edit: func(m *rsipolicy.TrustedEvaluationManifest) { m.Attempt = 2 }},
		{name: "maximum", edit: func(m *rsipolicy.TrustedEvaluationManifest) { m.MaxAttempts = 4 }},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, gate := createRSIGateInputs(t, true)
			request := rsiRequestFromStore(t, store, gate)
			resolver := writeSignedRSIEvaluation(t, t.TempDir(), request.Candidate.BeadID, request.Candidate.ControlBeadID, request.Candidate.ActorID, request.Candidate.SessionID, request.Candidate.RawOutput, request.Judges, testBundlePair(), true, test.edit)
			result, err := ProcessControl(store, gate, ProcessOptions{ResolveRSIEvaluation: resolver})
			if err != nil {
				t.Fatalf("ProcessControl: %v", err)
			}
			if result.Action != "rsi-reject" {
				t.Fatalf("Action = %q, want rejection", result.Action)
			}
			decision := readRSIDecision(t, mustGet(t, store, gate.ID))
			if decision.Promote || !containsRSIReason(decision.Reasons, rsiTrustedEvaluationUnavailable) {
				t.Fatalf("decision = %+v, want retry-state binding rejection", decision)
			}
		})
	}
}

func TestRSIGateRejectsUnknownEvaluatorAuthorityClass(t *testing.T) {
	store, gate := createRSIGateInputs(t, true)
	request := rsiRequestFromStore(t, store, gate)
	resolver := writeSignedRSIEvaluation(t, t.TempDir(), request.Candidate.BeadID, request.Candidate.ControlBeadID, request.Candidate.ActorID, request.Candidate.SessionID, request.Candidate.RawOutput, request.Judges, testBundlePair(), true, func(m *rsipolicy.TrustedEvaluationManifest) {
		m.AuthorityClass = "unknown-candidate-selected-class"
	})
	processed, err := ProcessControl(store, gate, ProcessOptions{ResolveRSIEvaluation: resolver})
	if err != nil {
		t.Fatalf("ProcessControl: %v", err)
	}
	if processed.Action != "rsi-reject" {
		t.Fatalf("Action = %q, want rejection", processed.Action)
	}
	decision := readRSIDecision(t, mustGet(t, store, gate.ID))
	if decision.Promote || !containsRSIReason(decision.Reasons, rsipolicy.ReasonUnknownAuthorityClass) {
		t.Fatalf("decision = %+v, want unknown authority rejection", decision)
	}
}

func compileRSIRecipe(t *testing.T) *formula.Recipe {
	t.Helper()
	formulaDir, err := filepath.Abs(filepath.Join("..", "bootstrap", "packs", "core", "formulas"))
	if err != nil {
		t.Fatal(err)
	}
	recipe, err := formula.CompileWithoutRuntimeVarValidation(context.Background(), "mol-rsi-candidate", []string{formulaDir}, rsiFormulaVars())
	if err != nil {
		t.Fatalf("compile mol-rsi-candidate: %v", err)
	}
	return recipe
}

func rsiFormulaVars() map[string]string {
	return map[string]string{
		"objective": "improve discovery latency", "work_dir": "/tmp/rsi-worktree", "rig_root": "/tmp/rig-root",
		"current_bundle_id": "baseline-bundle", "eval_suite_hash": strings.Repeat("a", 64),
		"improver_target": "candidate-agent", "correctness_target": "correctness-agent", "performance_target": "performance-agent",
	}
}

func createRSIGateInputs(t *testing.T, authorityEcho bool) (beads.Store, beads.Bead) {
	t.Helper()
	store := beads.NewMemStore()
	pair := testBundlePair()
	raw := forgedRSICandidateOutput(t, pair)
	metadata := map[string]string{
		beadmeta.RSIRoleMetadataKey: beadmeta.RSIRoleImprover, beadmeta.OutputJSONMetadataKey: raw,
		beadmeta.OutcomeMetadataKey: beadmeta.OutcomePass, beadmeta.SessionIDMetadataKey: "session-improver",
		beadmeta.RetryAttemptMetadataKey: "1", beadmeta.MaxAttemptsMetadataKey: "3",
	}
	if authorityEcho {
		metadata[beadmeta.RSIAuthorityClassMetadataKey] = "safety_policy"
	}
	candidate := mustCreate(t, store, beads.Bead{Title: "candidate", Assignee: "improver", Metadata: metadata})
	setRSIWorkerEvidence(t, store, candidate.ID, "improver", "session-improver", raw)
	judgeSpecs := []struct{ lane, actor, session string }{
		{lane: "correctness", actor: "judge-correctness", session: "session-correctness"},
		{lane: "performance", actor: "judge-performance", session: "session-performance"},
	}
	var judges []beads.Bead
	for _, spec := range judgeSpecs {
		output := mustJSON(t, passingRSILane(spec.lane, spec.actor))
		judge := mustCreate(t, store, beads.Bead{Title: spec.lane, Assignee: spec.actor, Metadata: map[string]string{
			beadmeta.RSIRoleMetadataKey: beadmeta.RSIRoleJudge, beadmeta.OutputJSONMetadataKey: output,
			beadmeta.OutcomeMetadataKey: beadmeta.OutcomePass, beadmeta.SessionIDMetadataKey: spec.session,
		}})
		setRSIWorkerEvidence(t, store, judge.ID, spec.actor, spec.session, output)
		judges = append(judges, judge)
	}
	gate := mustCreate(t, store, beads.Bead{Title: "promotion gate", Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindRSIPromotionGate}})
	mustDep(t, store, gate.ID, candidate.ID, "blocks")
	for _, judge := range judges {
		mustDep(t, store, gate.ID, judge.ID, "blocks")
	}
	return store, mustGet(t, store, gate.ID)
}

func rsiRequestFromStore(t *testing.T, store beads.Store, gate beads.Bead) rsipolicy.ResolveRequest {
	t.Helper()
	deps, err := store.DepList(gate.ID, "down")
	if err != nil {
		t.Fatalf("list gate dependencies: %v", err)
	}
	request := rsipolicy.ResolveRequest{}
	for _, dep := range deps {
		worker, err := store.Get(dep.DependsOnID)
		if err != nil {
			t.Fatalf("load worker: %v", err)
		}
		raw := worker.Metadata[beadmeta.OutputJSONMetadataKey]
		switch worker.Metadata[beadmeta.RSIRoleMetadataKey] {
		case beadmeta.RSIRoleImprover:
			var proposal rsipolicy.CandidateProposal
			if err := json.Unmarshal([]byte(raw), &proposal); err != nil {
				t.Fatal(err)
			}
			maxAttempts, _ := strconv.Atoi(worker.Metadata[beadmeta.MaxAttemptsMetadataKey])
			request.Candidate = rsipolicy.CandidateRecord{BeadID: worker.ID, ControlBeadID: worker.ID, Attempt: beadmeta.RetryAttemptNumber(worker.Metadata), MaxAttempts: maxAttempts, ActorID: worker.Assignee, SessionID: worker.Metadata[beadmeta.SessionIDMetadataKey], Status: worker.Status, Outcome: worker.Metadata[beadmeta.OutcomeMetadataKey], RawOutput: raw, Proposal: proposal}
		case beadmeta.RSIRoleJudge:
			var lane reviewquorum.LaneOutput
			if err := json.Unmarshal([]byte(raw), &lane); err != nil {
				t.Fatal(err)
			}
			request.Judges = append(request.Judges, rsipolicy.JudgeRecord{BeadID: worker.ID, ControlBeadID: worker.ID, ActorID: worker.Assignee, SessionID: worker.Metadata[beadmeta.SessionIDMetadataKey], Status: worker.Status, Outcome: worker.Metadata[beadmeta.OutcomeMetadataKey], RawOutput: raw, Lane: lane})
		}
	}
	return request
}

func setRSIWorkerEvidence(t *testing.T, store beads.Store, id, actor, session, output string) {
	t.Helper()
	if err := store.Update(id, beads.UpdateOpts{Status: stringPtr("closed"), Assignee: stringPtr(actor), Metadata: map[string]string{
		beadmeta.SessionIDMetadataKey: session, beadmeta.OutcomeMetadataKey: beadmeta.OutcomePass, beadmeta.OutputJSONMetadataKey: output,
	}}); err != nil {
		t.Fatalf("update worker %s: %v", id, err)
	}
}

func setTransientRSIAttempt(t *testing.T, store beads.Store, attemptID, actorID, sessionID string) {
	t.Helper()
	if err := store.Update(attemptID, beads.UpdateOpts{Status: stringPtr("closed"), Assignee: stringPtr(actorID), Metadata: map[string]string{
		beadmeta.SessionIDMetadataKey: sessionID, beadmeta.OutcomeMetadataKey: beadmeta.OutcomeFail,
		beadmeta.FailureClassMetadataKey: beadmeta.FailureClassTransient, beadmeta.FailureReasonMetadataKey: "retry-test-transient-failure",
	}}); err != nil {
		t.Fatalf("fail first RSI attempt %s: %v", attemptID, err)
	}
}

func processRSIRetryToSecondAttempt(t *testing.T, store beads.Store, controlID, formulaDir, actorID, sessionID, output string) string {
	t.Helper()
	control := mustGet(t, store, controlID)
	result, err := ProcessControl(store, control, ProcessOptions{Context: context.Background(), FormulaSearchPaths: []string{formulaDir}})
	if err != nil {
		t.Fatalf("process first RSI retry attempt %s: %v", controlID, err)
	}
	if !result.Processed || result.Action != "retry" {
		t.Fatalf("first RSI retry result = %+v, want retry", result)
	}
	second, err := findLatestAttempt(store, mustGet(t, store, controlID))
	if err != nil {
		t.Fatalf("find second RSI retry attempt: %v", err)
	}
	if second.ID == "" || second.Metadata[beadmeta.RetryAttemptMetadataKey] != "2" {
		t.Fatalf("second RSI retry attempt = %+v, want retry_attempt=2", second)
	}
	if err := store.Update(second.ID, beads.UpdateOpts{Status: stringPtr("closed"), Assignee: stringPtr(actorID), Metadata: map[string]string{
		beadmeta.SessionIDMetadataKey: sessionID, beadmeta.OutcomeMetadataKey: beadmeta.OutcomePass,
		beadmeta.FailureClassMetadataKey: "", beadmeta.FailureReasonMetadataKey: "", beadmeta.OutputJSONMetadataKey: output,
	}}); err != nil {
		t.Fatalf("close second RSI retry attempt %s: %v", second.ID, err)
	}
	result, err = ProcessControl(store, mustGet(t, store, controlID), ProcessOptions{Context: context.Background(), FormulaSearchPaths: []string{formulaDir}})
	if err != nil {
		t.Fatalf("process second RSI retry attempt %s: %v", controlID, err)
	}
	if !result.Processed || result.Action != "pass" {
		t.Fatalf("second RSI retry result = %+v, want pass", result)
	}
	return second.ID
}

func processPassingRSIRetry(t *testing.T, store beads.Store, controlID string) {
	t.Helper()
	control := mustGet(t, store, controlID)
	result, err := ProcessControl(store, control, ProcessOptions{Context: context.Background()})
	if err != nil {
		t.Fatalf("process RSI retry control %s: %v", controlID, err)
	}
	if !result.Processed || result.Action != "pass" {
		t.Fatalf("retry control %s = %+v, want pass", controlID, result)
	}
}

func testBundlePair() struct{ Current, Candidate rsipolicy.Bundle } {
	suite := strings.Repeat("a", 64)
	return struct{ Current, Candidate rsipolicy.Bundle }{
		Current:   rsipolicy.Bundle{ID: "baseline-bundle", TypesCommit: strings.Repeat("1", 40), AICommit: strings.Repeat("2", 40), FrontendCommit: strings.Repeat("3", 40), InktreeCommit: strings.Repeat("4", 40), EvalSuiteHash: suite},
		Candidate: rsipolicy.Bundle{ID: "candidate-bundle", TypesCommit: strings.Repeat("1", 40), AICommit: strings.Repeat("5", 40), FrontendCommit: strings.Repeat("3", 40), InktreeCommit: strings.Repeat("4", 40), EvalSuiteHash: suite, Parent: "baseline-bundle"},
	}
}

func forgedRSICandidateOutput(t *testing.T, pair struct{ Current, Candidate rsipolicy.Bundle }) string {
	t.Helper()
	bundle, err := json.Marshal(pair.Candidate)
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("{\"candidate\":%s,\"baseline\":{\"score\":999,\"safety_score\":0,\"latency_ms\":0,\"cost_usd\":-1,\"dependency_count\":-1},\"candidate_metrics\":{\"score\":0,\"safety_score\":0,\"latency_ms\":0,\"cost_usd\":0,\"dependency_count\":0},\"limits\":{\"min_safety_score\":0,\"max_latency_ms\":0,\"max_cost_usd\":0,\"max_dependencies\":0},\"attempts\":999,\"max_attempts\":999,\"authority_class\":\"deployment\",\"human_approval_verified\":true,\"judges\":[\"improver\"]}", bundle)
}

func writeSignedRSIEvaluation(t *testing.T, cityPath, candidateBeadID, candidateControlBeadID, candidateActor, candidateSession, candidateRaw string, judges []rsipolicy.JudgeRecord, pair struct{ Current, Candidate rsipolicy.Bundle }, approve bool, edit func(*rsipolicy.TrustedEvaluationManifest)) rsipolicy.ResolveTrustedEvaluationFunc {
	t.Helper()
	evidenceDir := filepath.Join(cityPath, ".gc", "rsi")
	if err := os.MkdirAll(evidenceDir, 0o700); err != nil {
		t.Fatal(err)
	}
	evaluatorPrivate := rsiTestPrivateKey("evaluator-test-key")
	humanPrivate := rsiTestPrivateKey("human-test-key")
	suite := pair.Current.EvalSuiteHash
	workIDs := []string{"unit-01", "unit-02", "unit-03", "unit-04", "unit-05", "unit-06", "unit-07", "unit-08", "unit-09", "unit-10"}
	baselineUseful := workIDs[:8]
	candidateUseful := workIDs[:9]
	work := rsipolicy.WorkAccounting{
		AcceptanceSetSHA256: rsipolicy.HashAcceptanceUnitIDs(workIDs), AcceptanceUnitIDs: workIDs,
		BaselineUsefulUnitIDs: baselineUseful, CandidateUsefulUnitIDs: candidateUseful,
		BaselineWorkerTime:  rsipolicy.WorkerTime{ImplementationHours: 8, RecoveryHours: 1, EvaluationHours: 1},
		CandidateWorkerTime: rsipolicy.WorkerTime{ImplementationHours: 7, RecoveryHours: 1, EvaluationHours: 2},
	}
	manifest := rsipolicy.TrustedEvaluationManifest{
		SchemaVersion: rsipolicy.TrustedEvaluationSchemaV1, ID: "evaluation-1", PolicyVersion: rsipolicy.PolicyVersionV1,
		EvaluatorKeyID: "evaluator-key", IssuedAt: time.Now().UTC().Add(-time.Minute).Format(time.RFC3339),
		ExpiresAt: time.Now().UTC().Add(time.Hour).Format(time.RFC3339), CandidateBeadID: candidateBeadID, CandidateControlBeadID: candidateControlBeadID,
		CandidateOutputSHA256: digestRSI([]byte(candidateRaw)), Objective: "improve discovery latency",
		Current: pair.Current, Candidate: pair.Candidate, EvalSuiteHash: suite,
		Baseline:         rsipolicy.Metrics{Score: 0.8, SafetyScore: 0.96, LatencyMS: 100, CostUSD: 1, DependencyCount: 3},
		CandidateMetrics: rsipolicy.Metrics{Score: 0.9, SafetyScore: 0.97, LatencyMS: 110, CostUSD: 1.1, DependencyCount: 3},
		Limits:           rsipolicy.Limits{MinSafetyScore: 0.95, MaxLatencyMS: 200, MaxCostUSD: 2, MaxDependencies: 4},
		AuthorityClass:   "optimization", Attempt: 1, MaxAttempts: 3, WorkAccounting: work,
		CandidateExecution: rsipolicy.CandidateExecution{ActorID: candidateActor, SessionID: candidateSession, Permissions: rsipolicy.ExecutionPermissions{
			CandidateRead: true, CandidateWrite: true, PolicyRead: true, OwnOutputWrite: true,
		}},
	}
	for _, judge := range judges {
		manifest.Judges = append(manifest.Judges, rsipolicy.JudgeAuthorization{
			LaneID: judge.Lane.LaneID, BeadID: judge.BeadID, ControlBeadID: judge.ControlBeadID, ActorID: judge.ActorID, SessionID: judge.SessionID,
			OutputSHA256: digestRSI([]byte(judge.RawOutput)), Permissions: rsipolicy.ExecutionPermissions{
				CandidateRead: true, PolicyRead: true, OwnOutputWrite: true,
			},
		})
	}
	manifest.Evidence = writeRSIEvidence(t, evidenceDir, manifest.ID, suite, pair, work, candidateActor, candidateSession, judges)
	if edit != nil {
		edit(&manifest)
	}
	evaluationBytes := signRSIManifest(t, evaluatorPrivate, manifest)
	if err := os.WriteFile(filepath.Join(evidenceDir, "evaluation.json"), evaluationBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := rsipolicy.FileResolverConfig{
		EvaluationFile: ".gc/rsi/evaluation.json", EvaluationKeyID: "evaluator-key",
		EvaluationPublicKey: base64.RawURLEncoding.EncodeToString(evaluatorPrivate.Public().(ed25519.PublicKey)),
	}
	cfg.HumanApprovalFile = ".gc/rsi/approval.json"
	cfg.HumanApprovalKeyID = "human-key"
	cfg.HumanApprovalPubKey = base64.RawURLEncoding.EncodeToString(humanPrivate.Public().(ed25519.PublicKey))
	if approve {
		evalDigest := sha256.Sum256(evaluationBytes)
		approval := rsipolicy.HumanApprovalManifest{
			SchemaVersion: rsipolicy.HumanApprovalSchemaV1, Decision: "approve", PolicyVersion: rsipolicy.PolicyVersionV1,
			ApprovalKeyID: "human-key", ApproverID: "human-key", EvaluationID: manifest.ID,
			EvaluationManifestSHA256: hex.EncodeToString(evalDigest[:]), CandidateBeadID: candidateBeadID,
			CandidateBundleID: pair.Candidate.ID, BaselineBundleID: pair.Current.ID, EvalSuiteHash: suite,
			ApprovedAt: time.Now().UTC().Format(time.RFC3339),
		}
		if err := os.WriteFile(filepath.Join(evidenceDir, "approval.json"), signRSIManifest(t, humanPrivate, approval), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return rsipolicy.NewFileResolver(cityPath, cfg).Resolve
}

func writeRSIEvidence(t *testing.T, dir, evalID, suite string, pair struct{ Current, Candidate rsipolicy.Bundle }, work rsipolicy.WorkAccounting, candidateActor, candidateSession string, judges []rsipolicy.JudgeRecord) []rsipolicy.EvidenceReference {
	t.Helper()
	refs := []rsipolicy.EvidenceReference{}
	put := func(kind, filename, bundleID string, value any) {
		t.Helper()
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, filename), data, 0o600); err != nil {
			t.Fatal(err)
		}
		refs = append(refs, rsipolicy.EvidenceReference{Kind: kind, Path: filename, SHA256: digestRSI(data), BundleID: bundleID, EvalSuiteHash: suite})
	}
	put("baseline-artifact", "baseline.json", pair.Current.ID, rsipolicy.ArtifactIdentityEvidence{SchemaVersion: "gc.rsi.artifact-identity.v1", EvaluationID: evalID, Bundle: pair.Current, EvalSuiteHash: suite})
	put("candidate-artifact", "candidate.json", pair.Candidate.ID, rsipolicy.ArtifactIdentityEvidence{SchemaVersion: "gc.rsi.artifact-identity.v1", EvaluationID: evalID, Bundle: pair.Candidate, EvalSuiteHash: suite})
	put("acceptance-ledger", "acceptance.json", "", rsipolicy.AcceptanceLedgerEvidence{SchemaVersion: "gc.rsi.acceptance-ledger.v1", EvaluationID: evalID, EvalSuiteHash: suite, AcceptanceUnitIDs: work.AcceptanceUnitIDs, BaselineUsefulUnitIDs: work.BaselineUsefulUnitIDs, CandidateUsefulUnitIDs: work.CandidateUsefulUnitIDs})
	timeLedger := rsipolicy.WorkerTimeLedgerEvidence{
		SchemaVersion: "gc.rsi.worker-time-ledger.v1", EvaluationID: evalID, EvalSuiteHash: suite,
		BaselineBundleID: pair.Current.ID, CandidateBundleID: pair.Candidate.ID,
		Intervals: []rsipolicy.WorkerInterval{
			{BundleID: pair.Current.ID, ActorID: "baseline-builder", SessionID: "baseline-implementation", Phase: "implementation", StartedAt: "2025-01-01T00:00:00Z", FinishedAt: "2025-01-01T08:00:00Z"},
			{BundleID: pair.Current.ID, ActorID: "baseline-builder", SessionID: "baseline-recovery", Phase: "recovery", StartedAt: "2025-01-01T08:00:00Z", FinishedAt: "2025-01-01T09:00:00Z"},
			{BundleID: pair.Current.ID, ActorID: "baseline-evaluator", SessionID: "baseline-evaluation", Phase: "evaluation", StartedAt: "2025-01-01T09:00:00Z", FinishedAt: "2025-01-01T10:00:00Z"},
			{BundleID: pair.Candidate.ID, ActorID: candidateActor, SessionID: candidateSession, Phase: "implementation", StartedAt: "2025-01-02T00:00:00Z", FinishedAt: "2025-01-02T07:00:00Z"},
			{BundleID: pair.Candidate.ID, ActorID: candidateActor, SessionID: candidateSession, Phase: "recovery", StartedAt: "2025-01-02T07:00:00Z", FinishedAt: "2025-01-02T08:00:00Z"},
			{BundleID: pair.Candidate.ID, ActorID: "judge-correctness", SessionID: "session-correctness", Phase: "evaluation", StartedAt: "2025-01-02T08:00:00Z", FinishedAt: "2025-01-02T09:00:00Z"},
			{BundleID: pair.Candidate.ID, ActorID: "judge-performance", SessionID: "session-performance", Phase: "evaluation", StartedAt: "2025-01-02T09:00:00Z", FinishedAt: "2025-01-02T10:00:00Z"},
		},
	}
	put("worker-time-ledger", "worker-time.json", "", timeLedger)
	for _, judge := range judges {
		put("judge-lane:"+judge.Lane.LaneID, "judge-"+judge.Lane.LaneID+".json", pair.Candidate.ID, json.RawMessage(judge.RawOutput))
	}
	return refs
}

func signRSIManifest(t *testing.T, privateKey ed25519.PrivateKey, payload any) []byte {
	t.Helper()
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	envelope := struct {
		Payload   json.RawMessage "json:\"payload\""
		Signature string          "json:\"signature\""
	}{Payload: payloadBytes, Signature: base64.RawURLEncoding.EncodeToString(ed25519.Sign(privateKey, payloadBytes))}
	data, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func rsiTestPrivateKey(label string) ed25519.PrivateKey {
	seed := sha256.Sum256([]byte(label))
	return ed25519.NewKeyFromSeed(seed[:])
}

func digestRSI(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func readRSIDecision(t *testing.T, bead beads.Bead) rsipolicy.Decision {
	t.Helper()
	var decision rsipolicy.Decision
	if err := json.Unmarshal([]byte(bead.Metadata[beadmeta.OutputJSONMetadataKey]), &decision); err != nil {
		t.Fatal(err)
	}
	return decision
}

func containsRSIReason(reasons []string, want string) bool {
	for _, reason := range reasons {
		if reason == want {
			return true
		}
	}
	return false
}

func passingRSILane(id, provider string) reviewquorum.LaneOutput {
	return reviewquorum.LaneOutput{
		LaneID: id, Provider: provider, Model: "review-model", Verdict: reviewquorum.VerdictPass, FailureClass: reviewquorum.FailureClassNone,
		ReadOnlyEnforcement: reviewquorum.ReadOnlyEnforcement{Observed: true, Enabled: true, Passed: true, BaselineCommand: "git status --porcelain=v1 -z", AfterCommand: "git status --porcelain=v1 -z"},
	}
}

func stringPtr(value string) *string { return &value }

func mustJSON(t *testing.T, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal JSON: %v", err)
	}
	return string(raw)
}
