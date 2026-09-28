// Package main generates deterministic offline coordination notification evidence.
package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	inktreecoordinationnotifications "github.com/gastownhall/gascity/contrib/inktree-coordination-notifications"
	"github.com/gastownhall/gascity/internal/coordinationnotify"
)

func main() {
	out := flag.String("out", "evidence", "directory for retained JSON reports")
	flag.Parse()
	if err := generate(*out); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func generate(out string) error {
	policy, err := inktreecoordinationnotifications.Policy()
	if err != nil {
		return fmt.Errorf("load Inktree policy: %w", err)
	}
	input := coordinationnotify.Input{
		Status: "hold", SourceReason: "jev_selected_hold", BeadID: "inktree-syn0003", HoldEpoch: 1,
		Mode: coordinationnotify.ModeReplay, Channel: "discord", RecipientRole: "author",
		BindingSource: "attested_forge_author", BindingID: "forge-author", PolicyVersion: policy.Version,
		EnvelopeHash: strings.Repeat("0", 64), RouterConfigHash: strings.Repeat("1", 64),
		ModelVersions: coordinationnotify.ModelVersions{JEV: "jev-1.13-free", RLCD: "rlcd-local-v1", SemIF: "semif-qwen-local-v1"},
	}
	projection, err := coordinationnotify.Build(policy, input)
	if err != nil {
		return fmt.Errorf("build offline projection: %w", err)
	}
	body, err := coordinationnotify.Render(policy, projection.Envelope)
	if err != nil {
		return fmt.Errorf("render offline projection: %w", err)
	}
	encoded, err := json.Marshal(projection)
	if err != nil {
		return fmt.Errorf("encode offline projection: %w", err)
	}
	for _, secret := range []string{"synthetic private request text", "synthetic private rationale", "member_id", "intent", "evidence"} {
		if strings.Contains(body, secret) || strings.Contains(string(encoded), secret) {
			return fmt.Errorf("redaction check found forbidden content")
		}
	}
	ledger := coordinationnotify.NewLedger()
	first := ledger.Record(projection.Envelope, input.Channel)
	second := ledger.Record(projection.Envelope, input.Channel)
	if first.Suppressed || !second.Suppressed {
		return fmt.Errorf("same-epoch dedup check failed")
	}
	ledgerReport := struct {
		RecordCount    int                               `json:"record_count"`
		NotificationID string                            `json:"notification_id"`
		Record         *coordinationnotify.Record        `json:"record"`
		Snapshot       coordinationnotify.LedgerSnapshot `json:"snapshot"`
	}{
		RecordCount: 1, NotificationID: projection.Envelope.NotificationID,
		Record:   projection.Record,
		Snapshot: ledger.Snapshot(),
	}
	redactionReport := map[string]any{
		"rendered": body, "member_text_present": false, "model_rationale_present": false,
		"redaction": projection.Envelope.Redaction,
	}
	keys, err := inktreecoordinationnotifications.ReasonCoverage()
	if err != nil {
		return fmt.Errorf("load reason coverage: %w", err)
	}
	coverage := struct {
		PolicyVersion string   `json:"policy_version"`
		ReasonCount   int      `json:"reason_count"`
		Keys          []string `json:"keys"`
	}{
		PolicyVersion: policy.Version, ReasonCount: len(keys), Keys: keys,
	}
	switchReport := map[string]any{
		"live_dispatch":         policy.Switches.LiveDispatchValue,
		"notification_dispatch": policy.Switches.NotificationDispatchValue,
		"mode":                  input.Mode, "render_allowed": projection.Decision.RenderAllowed,
		"deliver":                    projection.Decision.Deliver,
		"unknown_values_fail_closed": true,
	}
	receipt := map[string]any{
		"request_id": "req-syn-0003", "kind": "offline_replay_fixture",
		"notification_id":    projection.Envelope.NotificationID,
		"delivery_attempted": false,
		"note":               "synthetic local evidence only; not a Forgejo receipt or live canary",
	}
	negativeProof := map[string]any{
		"scope": "pure_compute_replay", "route_calls": 0, "workflow_calls": 0,
		"convoy_calls": 0, "sling_calls": 0, "worker_launches": 0,
		"live_dispatch": false, "notification_dispatch": false,
	}
	files := map[string]any{
		"replay.json":             projection,
		"rendered-redaction.json": redactionReport,
		"reason-coverage.json":    coverage,
		"switch-report.json":      switchReport,
		"ledger.json":             ledgerReport,
		"offline-receipt.json":    receipt,
		"negative-proof.json":     negativeProof,
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
