package config

import (
	"reflect"
	"strings"
	"testing"
)

func TestPrivateEvidenceTransportConfigRoundTripDefaultsDisabled(t *testing.T) {
	defaultConfig, err := Parse([]byte("[workspace]\nname = \"test\"\n"))
	if err != nil {
		t.Fatalf("Parse default config: %v", err)
	}
	if len(defaultConfig.Beads.PrivateEvidence) != 0 {
		t.Fatalf("default private evidence transports = %#v, want disabled", defaultConfig.Beads.PrivateEvidence)
	}

	want := City{
		Workspace: Workspace{Name: "test"},
		Beads: BeadsConfig{PrivateEvidence: map[string]PrivateEvidenceTransportConfig{
			"city:test": {
				Endpoint: "http://127.0.0.1:9865", ProjectID: "city-project", Database: "city_db", TokenFile: "/run/gc/private-evidence.token",
			},
			"rig:api": {
				Endpoint: "https://beads.example.test", ProjectID: "api-project", Database: "api_db", TokenFile: "/run/gc/api-evidence.token",
			},
		}},
	}
	encoded, err := want.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	got, err := Parse(encoded)
	if err != nil {
		t.Fatalf("Parse round-trip: %v\n%s", err, encoded)
	}
	if !reflect.DeepEqual(got.Beads.PrivateEvidence, want.Beads.PrivateEvidence) {
		t.Fatalf("private evidence transports = %#v, want %#v", got.Beads.PrivateEvidence, want.Beads.PrivateEvidence)
	}
}

func TestPrivateEvidenceTransportConfigRejectsUnsafeOrIncompleteEntries(t *testing.T) {
	valid := PrivateEvidenceTransportConfig{
		Endpoint: "https://beads.example.test", ProjectID: "project", Database: "gc", TokenFile: "/run/gc/token",
	}
	tests := []struct {
		name  string
		scope string
		value PrivateEvidenceTransportConfig
	}{
		{name: "noncanonical scope", scope: "pilot", value: valid},
		{name: "cleartext non-loopback", scope: "city:test", value: PrivateEvidenceTransportConfig{Endpoint: "http://beads.example.test", ProjectID: "project", Database: "gc", TokenFile: "/run/gc/token"}},
		{name: "host name loopback", scope: "city:test", value: PrivateEvidenceTransportConfig{Endpoint: "http://localhost:9865", ProjectID: "project", Database: "gc", TokenFile: "/run/gc/token"}},
		{name: "endpoint path", scope: "city:test", value: PrivateEvidenceTransportConfig{Endpoint: "https://beads.example.test/v0", ProjectID: "project", Database: "gc", TokenFile: "/run/gc/token"}},
		{name: "missing project", scope: "city:test", value: PrivateEvidenceTransportConfig{Endpoint: valid.Endpoint, Database: valid.Database, TokenFile: valid.TokenFile}},
		{name: "relative token file", scope: "city:test", value: PrivateEvidenceTransportConfig{Endpoint: valid.Endpoint, ProjectID: valid.ProjectID, Database: valid.Database, TokenFile: "token"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validatePrivateEvidenceTransports(map[string]PrivateEvidenceTransportConfig{tt.scope: tt.value})
			if err == nil || !strings.Contains(err.Error(), "beads.private_evidence") {
				t.Fatalf("validation error = %v, want a beads.private_evidence error", err)
			}
		})
	}
}

func TestPrivateEvidenceTransportConfigValidatesDuringParse(t *testing.T) {
	_, err := Parse([]byte(`[workspace]
name = "test"

[beads.private_evidence."city:test"]
endpoint = "http://localhost:9865"
project_id = "project"
database = "gc"
token_file = "/run/gc/token"
`))
	if err == nil || !strings.Contains(err.Error(), "beads.private_evidence") {
		t.Fatalf("Parse error = %v, want private evidence transport validation error", err)
	}
}
