package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"testing"
)

// This command acknowledges a queued tick; it must never report a successful
// request when the controller is unavailable or returns another protocol reply.
func TestControllerReconcileAcknowledgement(t *testing.T) {
	for _, tc := range []struct {
		name    string
		reply   string
		err     error
		wantErr bool
	}{
		{name: "acknowledged", reply: "ok"},
		{name: "unavailable", err: errors.New("controller unavailable"), wantErr: true},
		{name: "empty reply", wantErr: true},
		{name: "error reply", reply: "error: unavailable", wantErr: true},
		{name: "unrecognized success", reply: `{"ok":true}`, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout bytes.Buffer
			calls := 0
			cmd := newControllerReconcileCmd(&stdout,
				func([]string) (string, error) { return "/fixture/city", nil },
				func(city, command string) ([]byte, error) {
					calls++
					if city != "/fixture/city" || command != "poke" {
						t.Fatalf("request = (%q, %q), want selected city and poke", city, command)
					}
					return []byte(tc.reply), tc.err
				})
			cmd.SetArgs([]string{"--json"})
			err := cmd.Execute()
			if (err != nil) != tc.wantErr {
				t.Fatalf("Execute error = %v, want error=%v", err, tc.wantErr)
			}
			if calls != 1 {
				t.Fatalf("controller requests = %d, want 1", calls)
			}
			if tc.wantErr {
				if stdout.Len() != 0 {
					t.Fatalf("failed request emitted success output: %s", stdout.String())
				}
				return
			}
			validateJSONResultSchema(t, []string{"controller", "reconcile"}, stdout.Bytes())
			var result struct {
				SchemaVersion string `json:"schema_version"`
				OK            bool   `json:"ok"`
				Status        string `json:"status"`
				CityPath      string `json:"city_path"`
			}
			if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if result.SchemaVersion != "1" || !result.OK || result.Status != "requested" || result.CityPath != "/fixture/city" {
				t.Fatalf("unexpected request acknowledgement: %+v", result)
			}
		})
	}
}

func TestControllerReconcileInvalidInvocationDoesNotSend(t *testing.T) {
	for _, args := range [][]string{{"unexpected"}, {"--unknown"}} {
		cmd := newControllerReconcileCmd(io.Discard,
			func([]string) (string, error) { t.Fatal("resolved city for invalid invocation"); return "", nil },
			func(string, string) ([]byte, error) { t.Fatal("sent invalid request"); return nil, nil })
		cmd.SetArgs(args)
		if err := cmd.Execute(); err == nil {
			t.Fatalf("arguments %v were accepted", args)
		}
	}
}

func TestControllerReconcileResolutionFailureDoesNotSend(t *testing.T) {
	want := errors.New("city unresolved")
	cmd := newControllerReconcileCmd(io.Discard,
		func([]string) (string, error) { return "", want },
		func(string, string) ([]byte, error) { t.Fatal("sent without a resolved city"); return nil, nil })
	cmd.SetArgs([]string{"--json"})
	if err := cmd.Execute(); !errors.Is(err, want) {
		t.Fatalf("resolution error = %v, want %v", err, want)
	}
}

func TestControllerReconcileJSONProductionContract(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"controller", "reconcile", "--json-schema"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("schema command = %d: %s", code, stderr.String())
	}
	var manifest jsonSchemaManifest
	if err := json.Unmarshal(stdout.Bytes(), &manifest); err != nil {
		t.Fatal(err)
	}
	if !manifest.JSONSupported || len(manifest.Command) != 2 || manifest.Command[0] != "controller" || manifest.Command[1] != "reconcile" || len(manifest.Schemas["result"]) == 0 {
		t.Fatalf("controller request JSON contract unavailable: %+v", manifest)
	}
}
