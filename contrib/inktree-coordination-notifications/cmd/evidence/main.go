// Package main generates deterministic offline coordination notification evidence.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	inktreecoordinationnotifications "github.com/gastownhall/gascity/contrib/inktree-coordination-notifications"
	"github.com/gastownhall/gascity/internal/coordinationnotify"
)

const (
	defaultCorpus = "contrib/inktree-coordination-notifications/replay/corpus.v1.json"
	defaultPolicy = "contrib/inktree-coordination-notifications/policy.json"
)

type replayCorpus struct {
	Version string       `json:"corpus_version"`
	Cases   []replayCase `json:"cases"`
}

type replayCase struct {
	CaseID               string `json:"case_id"`
	Status               string `json:"status"`
	SourceReason         string `json:"source_reason"`
	BeadID               string `json:"bead_id"`
	ExpectReasonCode     string `json:"expect_reason_code"`
	ExpectNotification   bool   `json:"expect_notification"`
	ExpectProjectionHash string `json:"expect_projection_hash"`
}

type replayInputDocument struct {
	CaseID           string                           `json:"case_id"`
	Status           string                           `json:"status"`
	SourceReason     string                           `json:"source_reason"`
	BeadID           string                           `json:"bead_id"`
	HoldEpoch        uint64                           `json:"hold_epoch"`
	Mode             coordinationnotify.Mode          `json:"mode"`
	Channel          string                           `json:"channel"`
	RecipientRole    string                           `json:"recipient_role"`
	BindingSource    string                           `json:"binding_source"`
	BindingID        string                           `json:"binding_id"`
	PolicyVersion    string                           `json:"policy_version"`
	RouterConfigHash string                           `json:"router_config_hash"`
	ModelVersions    coordinationnotify.ModelVersions `json:"model_versions"`
}

type replayCaseResult struct {
	CaseID              string                        `json:"case_id"`
	EnvelopeContentHash string                        `json:"envelope_content_hash"`
	ProjectionHash      string                        `json:"projection_hash"`
	Matched             bool                          `json:"matched"`
	Projection          coordinationnotify.Projection `json:"projection"`
}

type replayReport struct {
	CorpusVersion string             `json:"corpus_version"`
	CorpusHash    string             `json:"corpus_hash"`
	PolicyVersion string             `json:"policy_version"`
	PolicyHash    string             `json:"policy_hash"`
	Deterministic bool               `json:"deterministic"`
	CaseResults   []replayCaseResult `json:"case_results"`
}

func main() {
	corpus := flag.String("corpus", defaultCorpus, "retained synthetic replay corpus")
	policy := flag.String("policy", defaultPolicy, "retained notification policy")
	out := flag.String("out", "contrib/inktree-coordination-notifications/evidence", "directory for retained JSON reports")
	flag.Parse()
	if err := generate(*out, *corpus, *policy); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func generate(out, corpusPath, policyPath string) error {
	corpusData, err := os.ReadFile(corpusPath)
	if err != nil {
		return fmt.Errorf("read replay corpus: %w", err)
	}
	policyData, err := os.ReadFile(policyPath)
	if err != nil {
		return fmt.Errorf("read replay policy: %w", err)
	}
	return generateFromData(out, corpusData, policyData)
}

func generateFromData(out string, corpusData, policyData []byte) error {
	corpus := replayCorpus{}
	if err := decodeStrict(corpusData, &corpus); err != nil {
		return fmt.Errorf("decode replay corpus: %w", err)
	}
	if corpus.Version != "inktree-coordination-notification-replay/v1" || len(corpus.Cases) != 7 {
		return errors.New("replay corpus version or case count is invalid")
	}
	policy, err := inktreecoordinationnotifications.ParsePolicy(policyData)
	if err != nil {
		return fmt.Errorf("load Inktree policy: %w", err)
	}
	policyHash := sha256Hex(policyData)
	report := replayReport{
		CorpusVersion: corpus.Version, CorpusHash: sha256Hex(corpusData),
		PolicyVersion: policy.Version, PolicyHash: policyHash, Deterministic: true,
	}
	var notification *coordinationnotify.Projection
	seenCases := make(map[string]bool, len(corpus.Cases))
	for _, replayCase := range corpus.Cases {
		if replayCase.CaseID == "" || seenCases[replayCase.CaseID] {
			return fmt.Errorf("duplicate or empty replay case ID %q", replayCase.CaseID)
		}
		expectedHash, err := hex.DecodeString(replayCase.ExpectProjectionHash)
		if err != nil || len(expectedHash) != sha256.Size {
			return fmt.Errorf("replay case %s has an invalid expected projection hash", replayCase.CaseID)
		}
		seenCases[replayCase.CaseID] = true
		rule, ok := policy.Reasons[replayCase.Status+"/"+replayCase.SourceReason]
		if !ok || rule.Code != replayCase.ExpectReasonCode {
			return fmt.Errorf("replay case %s reason expectation does not match policy", replayCase.CaseID)
		}
		inputDocument := replayInputDocument{
			CaseID: replayCase.CaseID, Status: replayCase.Status, SourceReason: replayCase.SourceReason,
			BeadID: replayCase.BeadID, HoldEpoch: 1, Mode: coordinationnotify.ModeReplay,
			Channel: "discord", RecipientRole: "author", BindingSource: "attested_forge_author",
			BindingID: "binding-0123456789abcdef0123456789abcdef", PolicyVersion: policy.Version,
			RouterConfigHash: policyHash,
			ModelVersions:    coordinationnotify.ModelVersions{JEV: "jev-1.13-free", RLCD: "rlcd-local-v1", SemIF: "semif-qwen-local-v1"},
		}
		inputData, err := json.Marshal(inputDocument)
		if err != nil {
			return fmt.Errorf("encode replay input %s: %w", replayCase.CaseID, err)
		}
		input := coordinationnotify.Input{
			Status: inputDocument.Status, SourceReason: inputDocument.SourceReason,
			BeadID: inputDocument.BeadID, HoldEpoch: inputDocument.HoldEpoch, Mode: inputDocument.Mode,
			Channel: inputDocument.Channel, RecipientRole: inputDocument.RecipientRole,
			BindingSource: inputDocument.BindingSource, BindingID: inputDocument.BindingID,
			PolicyVersion: inputDocument.PolicyVersion, EnvelopeHash: sha256Hex(inputData),
			RouterConfigHash: inputDocument.RouterConfigHash, ModelVersions: inputDocument.ModelVersions,
		}
		first, err := coordinationnotify.Build(policy, input)
		if err != nil {
			return fmt.Errorf("build replay case %s: %w", replayCase.CaseID, err)
		}
		second, err := coordinationnotify.Build(policy, input)
		if err != nil {
			return fmt.Errorf("repeat replay case %s: %w", replayCase.CaseID, err)
		}
		firstJSON, _ := json.Marshal(first)
		secondJSON, _ := json.Marshal(second)
		projectionHash := sha256Hex(firstJSON)
		matched := bytes.Equal(firstJSON, secondJSON) && (first.Envelope != nil) == replayCase.ExpectNotification
		if first.Envelope != nil {
			matched = matched && first.Envelope.ReasonCode == replayCase.ExpectReasonCode
			if notification == nil {
				selected := first
				notification = &selected
			}
		}
		if !matched {
			return fmt.Errorf("replay case %s did not match its retained expectation", replayCase.CaseID)
		}
		if projectionHash != replayCase.ExpectProjectionHash {
			return fmt.Errorf("replay case %s projection hash changed: got %s, want %s", replayCase.CaseID, projectionHash, replayCase.ExpectProjectionHash)
		}
		report.CaseResults = append(report.CaseResults, replayCaseResult{
			CaseID: replayCase.CaseID, EnvelopeContentHash: input.EnvelopeHash,
			ProjectionHash: projectionHash, Matched: true, Projection: first,
		})
	}
	if notification == nil || notification.Envelope == nil {
		return errors.New("replay corpus produced no notification case")
	}
	body, err := coordinationnotify.Render(policy, notification.Envelope)
	if err != nil {
		return fmt.Errorf("render offline projection: %w", err)
	}
	encoded, err := json.Marshal(report)
	if err != nil {
		return fmt.Errorf("encode replay report: %w", err)
	}
	for _, secret := range []string{"synthetic private request text", "synthetic private rationale", "member_id", "member intent", "private evidence content"} {
		if strings.Contains(body, secret) || strings.Contains(string(encoded), secret) {
			return errors.New("redaction check found forbidden content")
		}
	}
	ledger := coordinationnotify.NewLedger()
	first := ledger.Record(notification.Envelope, coordinationnotify.Attempt{EventID: "evt-syn-0003", Channel: notification.Envelope.Channel})
	second := ledger.Record(notification.Envelope, coordinationnotify.Attempt{EventID: "evt-syn-0003", Channel: notification.Envelope.Channel})
	if first.Suppressed || !second.Suppressed || second.SuppressionCause != "duplicate_event" {
		return errors.New("same-event dedup check failed")
	}
	ledgerReport := struct {
		RecordCount    int                               `json:"record_count"`
		NotificationID string                            `json:"notification_id"`
		Record         *coordinationnotify.Record        `json:"record"`
		Snapshot       coordinationnotify.LedgerSnapshot `json:"snapshot"`
	}{
		RecordCount: 1, NotificationID: notification.Envelope.NotificationID,
		Record: notification.Record, Snapshot: ledger.Snapshot(),
	}
	redactionReport := map[string]any{
		"rendered": body, "member_text_present": false, "member_identifier_present": false,
		"model_rationale_present": false, "redaction": notification.Envelope.Redaction,
	}
	keys := make([]string, 0, len(policy.Reasons))
	for key := range policy.Reasons {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	coverage := struct {
		PolicyVersion string   `json:"policy_version"`
		ReasonCount   int      `json:"reason_count"`
		Keys          []string `json:"keys"`
	}{PolicyVersion: policy.Version, ReasonCount: len(keys), Keys: keys}
	switchReport := map[string]any{
		"live_dispatch": policy.Switches.LiveDispatchValue, "notification_dispatch": policy.Switches.NotificationDispatchValue,
		"mode": notification.Envelope.Mode, "render_allowed": notification.Decision.RenderAllowed,
		"deliver": notification.Decision.Deliver, "unknown_values_fail_closed": true,
	}
	receipt := map[string]any{
		"request_id": "req-syn-0003", "kind": "offline_replay_fixture",
		"notification_id": notification.Envelope.NotificationID, "delivery_attempted": false,
		"note": "synthetic local evidence only; not a Forgejo receipt or live canary",
	}
	negativeProof := map[string]any{
		"scope": "pure_compute_replay", "route_calls": 0, "workflow_calls": 0,
		"convoy_calls": 0, "sling_calls": 0, "worker_launches": 0,
		"live_dispatch": false, "notification_dispatch": false,
	}
	files := map[string]any{
		"replay.json": report, "rendered-redaction.json": redactionReport,
		"reason-coverage.json": coverage, "switch-report.json": switchReport,
		"ledger.json": ledgerReport, "offline-receipt.json": receipt,
		"negative-proof.json": negativeProof,
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		return fmt.Errorf("create evidence directory: %w", err)
	}
	for name, value := range files {
		if err := writeJSON(filepath.Join(out, name), value); err != nil {
			return err
		}
	}
	return nil
}

func decodeStrict(data []byte, value any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return errors.New("trailing JSON content")
	}
	return nil
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func writeJSON(path string, value any) (retErr error) {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("encode %s: %w", path, err)
	}
	data = append(data, '\n')
	temp, err := os.CreateTemp(filepath.Dir(path), ".evidence-*.tmp")
	if err != nil {
		return fmt.Errorf("create evidence temp for %s: %w", path, err)
	}
	tempName := temp.Name()
	defer func() {
		if err := os.Remove(tempName); err != nil && !errors.Is(err, os.ErrNotExist) && retErr == nil {
			retErr = fmt.Errorf("remove evidence temp %s: %w", tempName, err)
		}
	}()
	if _, err := temp.Write(data); err != nil {
		if closeErr := temp.Close(); closeErr != nil {
			return errors.Join(fmt.Errorf("write %s: %w", tempName, err), fmt.Errorf("close %s: %w", tempName, closeErr))
		}
		return fmt.Errorf("write %s: %w", tempName, err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", tempName, err)
	}
	if err := os.Rename(tempName, path); err != nil {
		return fmt.Errorf("publish %s: %w", path, err)
	}
	return nil
}
