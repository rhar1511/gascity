package rsipolicy

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/gastownhall/gascity/internal/storybench"
)

func validateStoryEvidence(contents map[string][]byte, manifest TrustedEvaluationManifest, request ResolveRequest) (*storybench.Result, error) {
	record, auth := request.Benchmark, manifest.Benchmark
	if record == nil || auth == nil || record.BeadID == "" || record.ControlBeadID == "" || record.ActorID == "" || record.SessionID == "" ||
		record.Status != "closed" || record.Outcome != "pass" || record.ActorID == request.Candidate.ActorID || record.SessionID == request.Candidate.SessionID ||
		record.BeadID != auth.BeadID || record.BeadRevision != auth.BeadRevision || record.ControlBeadID != auth.ControlBeadID || record.ControlRevision != auth.ControlRevision ||
		record.ActorID != auth.ActorID || record.SessionID != auth.SessionID || digestText(record.RawOutput) != auth.OutputSHA256 || !judgePermissionsValid(auth.Permissions) {
		return nil, errors.New("story benchmark execution does not match independent signed capture")
	}
	rawSuite, rawTraces := contents["story-suite"], contents["story-traces"]
	if storybench.SuiteHash(rawSuite) != manifest.EvalSuiteHash || !bytes.Equal(rawTraces, []byte(record.RawOutput)) {
		return nil, errors.New("story suite or traces differ from the bound suite and execution")
	}
	var evidence storybench.Evidence
	if err := decodeStrictJSON(rawTraces, &evidence); err != nil {
		return nil, fmt.Errorf("story traces malformed: %w", err)
	}
	if evidence.Baseline.BundleID != manifest.Current.ID || evidence.Candidate.BundleID != manifest.Candidate.ID ||
		evidence.Baseline.CapturedBy != record.ActorID || evidence.Candidate.CapturedBy != record.ActorID {
		return nil, errors.New("story traces do not identify the signed bundles and capturer")
	}
	result, err := storybench.Evaluate(rawSuite, evidence.Baseline, evidence.Candidate, request.Candidate.ActorID)
	if err != nil {
		return nil, fmt.Errorf("story benchmark: %w", err)
	}
	return &result, nil
}
