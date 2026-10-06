package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	inktreecoordinationnotifications "github.com/gastownhall/gascity/contrib/inktree-coordination-notifications"
)

func TestGenerateRetainsOfflineEvidenceWithoutDispatch(t *testing.T) {
	dir := t.TempDir()
	corpus, err := inktreecoordinationnotifications.ReplayCorpusDocument()
	if err != nil {
		t.Fatal(err)
	}
	policy, err := inktreecoordinationnotifications.PolicyDocument()
	if err != nil {
		t.Fatal(err)
	}
	if err := generateFromData(dir, corpus, policy); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{
		"replay.json", "rendered-redaction.json", "reason-coverage.json",
		"switch-report.json", "ledger.json", "offline-receipt.json", "negative-proof.json",
	} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		var value any
		if err := json.Unmarshal(data, &value); err != nil {
			t.Fatalf("decode %s: %v", name, err)
		}
	}
	var switches struct {
		Live          string `json:"live_dispatch"`
		Notifications string `json:"notification_dispatch"`
		Deliver       bool   `json:"deliver"`
	}
	data, err := os.ReadFile(filepath.Join(dir, "switch-report.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &switches); err != nil {
		t.Fatal(err)
	}
	if switches.Live != "off" || switches.Notifications != "off" || switches.Deliver {
		t.Fatalf("offline evidence authorized delivery: %#v", switches)
	}
	var replay replayReport
	data, err = os.ReadFile(filepath.Join(dir, "replay.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &replay); err != nil {
		t.Fatal(err)
	}
	if len(replay.CaseResults) != 7 || !replay.Deterministic || replay.CorpusHash == strings.Repeat("0", 64) || replay.PolicyHash == strings.Repeat("1", 64) {
		t.Fatalf("replay evidence is not bound to retained inputs: %#v", replay)
	}
}
