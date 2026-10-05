package main

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/splittest"
	"github.com/gastownhall/gascity/internal/config"
)

// The CLI delegates repair preparation to the central API in both storage
// layouts. Workflow placement is decided later by lifecycle admission.
func TestGitHubPRPreparePreservesLocalWorkAndGraphStores(t *testing.T) {
	for _, split := range []bool{false, true} {
		name := "single"
		if split {
			name = "split"
		}
		t.Run(name, func(t *testing.T) {
			cityDir := oneShotCookCity(t)
			var graph beads.Store
			if split {
				graph = splittest.NewClassStore(t, config.BeadClassGraph)
				seedCLIStorageRoutes(t, cityDir, messagingSplitRoutes(graph))
			} else {
				seedCLIStorageRoutes(t, cityDir, nil)
			}
			work, err := openStoreAtForCity(cityDir, cityDir)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := work.Create(beads.Bead{Title: "existing PR work", Type: "task"}); err != nil {
				t.Fatal(err)
			}
			beforeWork := allBeads(t, work)
			var beforeGraph []beads.Bead
			if split {
				beforeGraph = allBeads(t, graph)
			}
			marker := filepath.Join(t.TempDir(), "child.env")
			t.Setenv(productMetricsDirectChildEnvSpyPath, marker)
			installProductMetricsDirectChildSpyCommand(t, "gc")
			client := &fakeGitHubPRActionClient{queue: centralPRTestQueue()}
			useCentralPRTestClientAt(t, cityDir, client)
			var out, errOut bytes.Buffer
			if code := run([]string{"--city", cityDir, "github", "pr", "backfill", "--create-repair-beads", "--json"}, &out, &errOut); code != 0 {
				t.Fatalf("exit=%d stderr=%s", code, &errOut)
			}
			if len(client.actionRequests) != 1 {
				t.Fatalf("prepare actions=%d, want one", len(client.actionRequests))
			}
			if !reflect.DeepEqual(allBeads(t, work), beforeWork) {
				t.Fatal("CLI changed the local work store")
			}
			if split && !reflect.DeepEqual(allBeads(t, graph), beforeGraph) {
				t.Fatal("CLI materialized a local graph workflow")
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("CLI launched a local gc child: stat=%v", err)
			}
		})
	}
}
