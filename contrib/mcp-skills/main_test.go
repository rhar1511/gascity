package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/materialize"
)

// The opt-in example must pass the same loader as real agent projections.
func TestExampleLoadsThroughGasCityMCPCatalog(t *testing.T) {
	data, err := os.ReadFile("gascity-skills.toml.example")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "gascity-skills.toml"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	servers, err := materialize.LoadMCPDir(dir, "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(servers) != 1 || servers[0].Name != "gascity-skills" || servers[0].Transport != materialize.MCPTransportStdio || len(servers[0].Args) != 2 {
		t.Fatalf("unexpected projection: %+v", servers)
	}
}
