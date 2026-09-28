package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestGenerateRetainsOfflineEvidenceWithoutDispatch(t *testing.T) {
	dir := t.TempDir()
	if err := generate(dir); err != nil {
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
}
