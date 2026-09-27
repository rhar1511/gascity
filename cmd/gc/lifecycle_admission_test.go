package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/worklifecycle"
)

func TestLifecycleAdmissionMaterializesConfiguredDefaultWorkflowOnce(t *testing.T) {
	store, cfg, cityPath := lifecycleAdmissionFixture(t)
	var stderr bytes.Buffer
	reconcileLifecycleAdmission("pilot", cityPath, cfg, store, nil, nil, &stderr)
	if stderr.Len() != 0 {
		t.Fatalf("first reconciliation logged an error: %s", stderr.String())
	}
	work, err := store.Get("work-1")
	if err != nil {
		t.Fatal(err)
	}
	if work.Metadata[beadmeta.RoutedToMetadataKey] != "worker" || work.Metadata[beadmeta.MergeStrategyMetadataKey] != "mr" {
		t.Fatalf("source route/merge = %q/%q, want worker/mr; metadata=%v", work.Metadata[beadmeta.RoutedToMetadataKey], work.Metadata[beadmeta.MergeStrategyMetadataKey], work.Metadata)
	}
	if work.Metadata[beadmeta.MoleculeIDMetadataKey] == "" {
		t.Fatalf("source has no attached workflow root: %v", work.Metadata)
	}
	root, err := store.Get(work.Metadata[beadmeta.MoleculeIDMetadataKey])
	if err != nil {
		t.Fatalf("get attached workflow root: %v", err)
	}
	if root.Metadata[beadmeta.FormulaNameMetadataKey] != "review" {
		t.Fatalf("attached formula = %q, want configured review workflow; root=%+v", root.Metadata[beadmeta.FormulaNameMetadataKey], root)
	}
	if !lifecycleAdmissionRouteMatches(cfg, work, worklifecycle.ScopeForStore("pilot", "city:pilot")) {
		t.Fatal("materialized source does not satisfy its signed lifecycle contract")
	}

	// A later controller pass recognizes the same attached formula and leaves
	// both the existing route and workflow intact.
	firstRootID := root.ID
	reconcileLifecycleAdmission("pilot", cityPath, cfg, store, nil, nil, &stderr)
	if got := lifecycleWorkflowRootCount(store, "review"); got != 1 {
		t.Fatalf("second pass left %d review workflow roots, want one", got)
	}
	work, err = store.Get("work-1")
	if err != nil {
		t.Fatal(err)
	}
	if work.Metadata[beadmeta.MoleculeIDMetadataKey] != firstRootID {
		t.Fatalf("second pass replaced workflow %q with %q", firstRootID, work.Metadata[beadmeta.MoleculeIDMetadataKey])
	}
}

func TestLifecycleAdmissionReservationHasOneConcurrentMaterializer(t *testing.T) {
	store, cfg, cityPath := lifecycleAdmissionFixture(t)
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			reconcileLifecycleAdmission("pilot", cityPath, cfg, store, nil, nil, io.Discard)
		}()
	}
	wg.Wait()
	if got := lifecycleWorkflowRootCount(store, "review"); got != 1 {
		t.Fatalf("concurrent reconciliations left %d review workflow roots, want one CAS winner", got)
	}
	work, err := store.Get("work-1")
	if err != nil {
		t.Fatal(err)
	}
	if work.Metadata[beadmeta.MoleculeIDMetadataKey] == "" || !lifecycleMaterializationEvidence(work) {
		t.Fatalf("concurrent materialization did not persist one verified workflow: %v", work.Metadata)
	}
}

func TestLifecycleAdmissionHoldsAnIncompleteReservationAfterRestart(t *testing.T) {
	store, cfg, cityPath := lifecycleAdmissionFixture(t)
	row, err := store.Get("work-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetMetadata(row.ID, beadmeta.LifecycleMaterializationMetadataKey, `{"version":1,"state":"reserved","scope":"city:pilot/city:pilot","contract":"prior","route":"worker","workflow":"review","merge_strategy":"mr","token":"prior-owner"}`); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	reconcileLifecycleAdmission("pilot", cityPath, cfg, store, nil, nil, &stderr)
	if !strings.Contains(stderr.String(), "incomplete prior materialization reservation") {
		t.Fatalf("stderr=%q, want explicit held-reservation evidence", stderr.String())
	}
}

func lifecycleAdmissionFixture(t *testing.T) (beads.Store, *config.City, string) {
	t.Helper()
	_, admissionKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	_, acceptanceKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	formulaDir := t.TempDir()
	formula := "formula = \"review\"\nversion = 1\n\n[[steps]]\nid = \"work\"\ntitle = \"Review work\"\n"
	if err := os.WriteFile(filepath.Join(formulaDir, "review.toml"), []byte(formula), 0o600); err != nil {
		t.Fatal(err)
	}
	workflow := "review"
	maxSessions := 1
	cfg := &config.City{
		Workspace:     config.Workspace{Name: "pilot"},
		FormulaLayers: config.FormulaLayers{City: []string{formulaDir}},
		Agents: []config.Agent{{
			Name:                "worker",
			MaxActiveSessions:   &maxSessions,
			DefaultSlingFormula: &workflow,
		}},
		Lifecycle: config.LifecycleConfig{
			AdmissionEnabled: true,
			AdmissionAuthorities: map[string]string{
				"triage": base64.StdEncoding.EncodeToString(admissionKey.Public().(ed25519.PublicKey)),
			},
			AcceptanceAuthorities: map[string]string{
				"reviewer": base64.StdEncoding.EncodeToString(acceptanceKey.Public().(ed25519.PublicKey)),
			},
		},
	}
	receipt, err := worklifecycle.SignAdmissionReceipt(worklifecycle.AdmissionReceipt{
		Version: 1, WorkItemID: "work-1", Scope: worklifecycle.ScopeForStore("pilot", "city:pilot"),
		Route: "worker", Workflow: workflow, MergeStrategy: "mr", Deliverable: "reviewed patch",
		Verification: "acceptance tests", AcceptanceAuthority: "reviewer", AdmittedBy: "triage",
	}, admissionKey)
	if err != nil {
		t.Fatal(err)
	}
	store := &beads.MemStore{IDPrefix: "work", HonorExplicitIDs: true}
	if _, err := store.Create(beads.Bead{
		ID: "work-1", Title: "lifecycle test", Type: "task", Status: "open",
		Labels:   []string{worklifecycle.AdmissionIntentLabel},
		Metadata: map[string]string{beadmeta.LifecycleAdmissionReceiptMetadataKey: receipt},
	}); err != nil {
		t.Fatal(err)
	}
	return store, cfg, t.TempDir()
}

func lifecycleWorkflowRootCount(store beads.Store, formula string) int {
	rows, err := beads.HandlesFor(store).Live.List(beads.ListQuery{Metadata: map[string]string{beadmeta.FormulaNameMetadataKey: formula}})
	if err != nil {
		return -1
	}
	return len(rows)
}
