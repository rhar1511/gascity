package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/featureflags"
	"github.com/gastownhall/gascity/internal/rollout"
	"github.com/gastownhall/gascity/internal/sourceworkflow"
	"github.com/gastownhall/gascity/internal/worklifecycle"
)

func TestLifecycleAdmissionHoldsSignedV2UntilAttachmentAndPolicyProof(t *testing.T) {
	store, cfg, cityPath := lifecycleAdmissionFixture(t)
	var stderr bytes.Buffer
	reconcileLifecycleAdmission("pilot", cityPath, cfg, store, nil, nil, &stderr)
	work, err := store.Get("work-1")
	if err != nil {
		t.Fatal(err)
	}
	decision := worklifecycle.EvaluateAdmission(work, cfg.Lifecycle, worklifecycle.ScopeForStore("pilot", "city:pilot"))
	if !decision.Requested || decision.Admitted || !strings.Contains(decision.Reason, "attachment and current route-policy proof") {
		t.Fatalf("admission decision = %+v, want fail-closed proof hold", decision)
	}
	if work.Metadata[beadmeta.RoutedToMetadataKey] != "" || work.Metadata[beadmeta.MoleculeIDMetadataKey] != "" || work.Metadata[beadmeta.LifecycleMaterializationMetadataKey] != "" {
		t.Fatalf("unproved v2 receipt caused materialization: %v", work.Metadata)
	}
	if !strings.Contains(stderr.String(), "holding work-1") {
		t.Fatalf("reconciliation did not explain the hold: %s", stderr.String())
	}
}

func TestLifecycleAdmissionConcurrentPassesDoNotMaterializeWithoutProof(t *testing.T) {
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
	if got := lifecycleWorkflowRootCount(store, "review"); got != 0 {
		t.Fatalf("unproved concurrent reconciliations created %d review workflow roots", got)
	}
	work, err := store.Get("work-1")
	if err != nil {
		t.Fatal(err)
	}
	if work.Metadata[beadmeta.RoutedToMetadataKey] != "" || work.Metadata[beadmeta.MoleculeIDMetadataKey] != "" || work.Metadata[beadmeta.LifecycleMaterializationMetadataKey] != "" {
		t.Fatalf("unproved concurrent pass changed source lifecycle state: %v", work.Metadata)
	}
}

func TestLifecycleAdmissionDoesNotResumeReservationWithoutV2Proof(t *testing.T) {
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
	current, err := store.Get(row.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Metadata[beadmeta.LifecycleMaterializationMetadataKey] != `{"version":1,"state":"reserved","scope":"city:pilot/city:pilot","contract":"prior","route":"worker","workflow":"review","merge_strategy":"mr","token":"prior-owner"}` {
		t.Fatalf("unproved pass changed prior reservation: %v", current.Metadata)
	}
	if current.Metadata[beadmeta.RoutedToMetadataKey] != "" || current.Metadata[beadmeta.MoleculeIDMetadataKey] != "" {
		t.Fatalf("unproved prior reservation was resumed: %v", current.Metadata)
	}
	if !strings.Contains(stderr.String(), "attachment and current route-policy proof") {
		t.Fatalf("stderr=%q, want explicit v2 proof hold", stderr.String())
	}
}

func TestLifecycleProtectionSurvivesDisabledAdmissionWithoutChangingLegacyRows(t *testing.T) {
	disabled := &config.City{Lifecycle: config.LifecycleConfig{AdmissionEnabled: false}}
	enrolled := beads.Bead{Metadata: map[string]string{
		beadmeta.LifecycleAdmissionReceiptMetadataKey: "durable signed enrollment",
	}}
	if !lifecycleProtectedWork(enrolled, disabled) {
		t.Fatal("durably enrolled work became unprotected when admission was disabled")
	}
	if lifecycleProtectedWork(beads.Bead{ID: "legacy"}, disabled) {
		t.Fatal("ordinary non-enrolled work entered the lifecycle protection lane")
	}
}

func TestLifecycleGraphV2AdmissionWaitsForAttachmentAndPolicyProof(t *testing.T) {
	store, cfg, cityPath := lifecycleAdmissionFixture(t)
	var stderr bytes.Buffer
	reconcileLifecycleAdmission("pilot", cityPath, cfg, store, nil, nil, &stderr)
	source, err := store.Get("work-1")
	if err != nil {
		t.Fatal(err)
	}
	decision := worklifecycle.EvaluateAdmission(source, cfg.Lifecycle, worklifecycle.ScopeForStore("pilot", "city:pilot"))
	if !decision.Requested || decision.Admitted || !strings.Contains(decision.Reason, "attachment and current route-policy proof") {
		t.Fatalf("graph admission = %+v, want proof hold", decision)
	}
	if source.Metadata[beadmeta.MoleculeIDMetadataKey] != "" || source.Metadata[beadmeta.LifecycleMaterializationMetadataKey] != "" || lifecycleWorkflowRootCount(store, "review") != 0 {
		t.Fatalf("unproved graph admission created lineage: %+v", source.Metadata)
	}
}

func TestLifecycleGraphDescendantAssignmentProtectsLiveSessionFromLegacyRestart(t *testing.T) {
	env, _, _ := newProgressStallTestEnv(t)
	env.cfg.Lifecycle.AdmissionEnabled = true
	workflow := "review"
	env.cfg.Agents[0].DefaultSlingFormula = &workflow
	formulaDir := t.TempDir()
	env.cfg.FormulaLayers.City = []string{formulaDir}
	env.cfg.Daemon.FormulaV2 = boolPtr(true)
	if err := os.WriteFile(filepath.Join(formulaDir, "review.toml"), []byte("formula = \"review\"\nversion = 2\ncontract = \"graph.v2\"\n\n[[steps]]\nid = \"work\"\ntitle = \"Review work\"\ntype = \"task\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	withLifecycleFormulaV2ForTest(t, env.cfg)
	addTestControlDispatcherAgents(env.cfg, "")

	_, admissionKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	_, acceptanceKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	env.cfg.Lifecycle.AdmissionV2Authorities = map[string]string{"triage": base64.StdEncoding.EncodeToString(admissionKey.Public().(ed25519.PublicKey))}
	env.cfg.Lifecycle.AcceptanceAuthorities = map[string]string{"reviewer": base64.StdEncoding.EncodeToString(acceptanceKey.Public().(ed25519.PublicKey))}
	scope := worklifecycle.ScopeForStore("test-city", "city:test-city")
	if len(env.cfg.Agents) == 0 {
		t.Fatal("test city has no worker template")
	}
	env.cfg.Agents[0].Dir = "test-rig"
	source, err := env.store.Create(beads.Bead{Title: "graph lifecycle work", Type: "task", Status: "open", Labels: []string{worklifecycle.AdmissionIntentLabel}})
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := worklifecycle.SignAdmissionReceiptV2(worklifecycle.AdmissionReceiptV2{
		Version: 2, WorkItemID: source.ID, Scope: scope, ExpectedWorkRevision: source.Revision, Route: "test-rig/worker", Workflow: workflow,
		RoutingPolicyDigest: strings.Repeat("a", 64), MergeStrategy: "mr",
		Deliverable: "reviewed patch", Verification: "tests", AcceptanceAuthority: "reviewer", AdmittedBy: "triage",
	}, admissionKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := env.store.SetMetadata(source.ID, beadmeta.LifecycleAdmissionReceiptV2MetadataKey, receipt); err != nil {
		t.Fatal(err)
	}
	cityPath := t.TempDir()
	var stderr bytes.Buffer
	reconcileLifecycleAdmission("test-city", cityPath, env.cfg, env.store, nil, nil, &stderr)
	source, err = env.store.Get(source.ID)
	if err != nil {
		t.Fatal(err)
	}
	decision := worklifecycle.EvaluateAdmission(source, env.cfg.Lifecycle, scope)
	if !decision.Requested || decision.Admitted || !strings.Contains(decision.Reason, "attachment and current route-policy proof") {
		t.Fatalf("graph admission = %+v, want proof hold", decision)
	}
	if source.Metadata[beadmeta.RoutedToMetadataKey] != "" || source.Metadata[beadmeta.MoleculeIDMetadataKey] != "" || source.Metadata[beadmeta.LifecycleMaterializationMetadataKey] != "" {
		t.Fatalf("unproved graph admission changed the source: %+v", source.Metadata)
	}
	rows, err := env.store.ListByMetadata(map[string]string{beadmeta.RootBeadIDMetadataKey: source.ID}, 0, beads.WithBothTiers)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("unproved graph admission created descendants: %+v", rows)
	}
	if err := env.store.Close(source.ID); err == nil {
		t.Fatal("ordinary close accepted durably enrolled source work")
	}
}

func TestLifecycleGraphWorkflowLookupDoesNotFallBackAcrossDuplicateIDs(t *testing.T) {
	store, cfg, _ := lifecycleAdmissionFixture(t)
	source, err := store.Get("work-1")
	if err != nil {
		t.Fatal(err)
	}
	scope := worklifecycle.ScopeForStore("pilot", "city:pilot")
	decision := worklifecycle.EvaluateAdmission(source, cfg.Lifecycle, scope)
	digest, err := worklifecycle.AdmissionDigestV2(decision.Receipt)
	if err != nil {
		t.Fatal(err)
	}
	const rootID = "duplicate-workflow-id"
	reservation, err := encodeLifecycleMaterialization(lifecycleMaterialization{
		Version: 1, State: "reserved", Scope: scope, Contract: digest, Route: "pilot/worker", Workflow: "review", MergeStrategy: "mr",
		Token: "reservation", SourceID: source.ID, SourceStoreRef: "city:pilot", WorkflowStoreRef: "class:graph", AdmissionReceipt: source.Metadata[beadmeta.LifecycleAdmissionReceiptV2MetadataKey],
	})
	if err != nil {
		t.Fatal(err)
	}
	inputConvoy := "input-convoy"
	if err := store.Update(source.ID, beads.UpdateOpts{Metadata: map[string]string{
		beadmeta.LifecycleMaterializationMetadataKey: reservation,
		beadmeta.RoutedToMetadataKey:                 "pilot/worker",
		beadmeta.MergeStrategyMetadataKey:            "mr",
		beadmeta.WorkflowIDMetadataKey:               rootID,
	}, ParentID: &inputConvoy}); err != nil {
		t.Fatal(err)
	}
	current, err := store.Get(source.ID)
	if err != nil {
		t.Fatal(err)
	}
	lineage, err := encodeLifecycleMaterialization(lifecycleMaterialization{
		Version: 1, State: "lineage_pending", Scope: scope, Contract: digest, Route: "pilot/worker", Workflow: "review", MergeStrategy: "mr",
		Token: "reservation", WorkflowID: rootID, SourceID: source.ID, SourceStoreRef: "city:pilot", WorkflowStoreRef: "class:graph",
		AdmissionReceipt: source.Metadata[beadmeta.LifecycleAdmissionReceiptV2MetadataKey],
	})
	if err != nil {
		t.Fatal(err)
	}
	matchingRoot := beads.Bead{
		ID: rootID, Title: "Review workflow", Type: "molecule", Status: "open",
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:                     beadmeta.KindWorkflow,
			beadmeta.FormulaNameMetadataKey:              "review",
			beadmeta.FormulaContractMetadataKey:          beadmeta.FormulaContractGraphV2,
			beadmeta.ExecutionRoutedToMetadataKey:        "pilot/worker",
			beadmeta.MergeStrategyMetadataKey:            "mr",
			beadmeta.InputConvoyIDMetadataKey:            inputConvoy,
			beadmeta.LifecycleMaterializationMetadataKey: lineage,
		},
	}
	// The wrong store has a fully matching root. The source's input convoy is
	// also materialized there, so the old fallback-to-work-store behavior would
	// accept this result if the authoritative graph lookup were absent.
	if _, err := store.Create(matchingRoot); err != nil {
		t.Fatal(err)
	}
	graphStore := &beads.MemStore{IDPrefix: "graph", HonorExplicitIDs: true}
	if lifecycleSlingResultMatches(current, store, graphStore, cfg, scope, digest, "worker", "review", "mr", rootID, true, reservation, "class:graph") {
		t.Fatal("matching row in another store satisfied unavailable authoritative graph lookup")
	}
	if _, err := graphStore.Create(matchingRoot); err != nil {
		t.Fatal(err)
	}
	if !lifecycleSlingResultMatches(current, store, graphStore, cfg, scope, digest, "worker", "review", "mr", rootID, true, reservation, "class:graph") {
		t.Fatal("valid graph.v2 root in the selected authoritative store did not verify")
	}
}

func materializeLifecycleGraphV2(t *testing.T, store beads.Store, cfg *config.City, cityPath, sourceID string) (beads.Bead, beads.Bead) {
	t.Helper()
	if len(cfg.FormulaLayers.City) == 0 {
		cfg.FormulaLayers.City = []string{t.TempDir()}
	}
	formulaDir := cfg.FormulaLayers.City[0]
	if err := os.MkdirAll(formulaDir, 0o700); err != nil {
		t.Fatal(err)
	}
	formula := "formula = \"review\"\nversion = 2\ncontract = \"graph.v2\"\n\n[[steps]]\nid = \"work\"\ntitle = \"Review work\"\ntype = \"task\"\n"
	if err := os.WriteFile(filepath.Join(formulaDir, "review.toml"), []byte(formula), 0o600); err != nil {
		t.Fatal(err)
	}
	workflow := "review"
	cfg.Agents[0].DefaultSlingFormula = &workflow
	cfg.Daemon.FormulaV2 = boolPtr(true)
	withLifecycleFormulaV2ForTest(t, cfg)
	addTestControlDispatcherAgents(cfg, "")
	var stderr bytes.Buffer
	reconcileLifecycleAdmission(censusCityName(cfg), cityPath, cfg, store, nil, nil, &stderr)
	source, err := store.Get(sourceID)
	if err != nil {
		t.Fatal(err)
	}
	lineage, ok := lifecycleMaterializationFor(source)
	if !ok || lineage.State != "attached" || lineage.WorkflowID == "" {
		rows, _ := store.List(beads.ListQuery{AllowScan: true, TierMode: beads.TierBoth})
		t.Fatalf("graph formula did not attach lifecycle lineage to source: %+v; stderr=%s; source=%+v; rows=%+v", lineage, stderr.String(), source, rows)
	}
	root, err := store.Get(lineage.WorkflowID)
	if err != nil {
		t.Fatalf("read graph root %s: %v", lineage.WorkflowID, err)
	}
	if !sourceworkflow.IsWorkflowRoot(root) || root.Metadata[beadmeta.FormulaContractMetadataKey] != beadmeta.FormulaContractGraphV2 {
		t.Fatalf("attached workflow is not a graph.v2 root: %+v", root)
	}
	children, err := store.ListByMetadata(map[string]string{beadmeta.RootBeadIDMetadataKey: root.ID}, 0, beads.WithBothTiers)
	if err != nil {
		t.Fatal(err)
	}
	for _, child := range children {
		if child.ID != root.ID && child.Metadata[beadmeta.RoutedToMetadataKey] == "pilot/worker" && lifecycleMaterializationEvidence(child) {
			return root, child
		}
	}
	t.Fatalf("graph root %s has no routed descendant carrying attached lifecycle lineage: %+v", root.ID, children)
	return beads.Bead{}, beads.Bead{}
}

func withLifecycleFormulaV2ForTest(t *testing.T, cfg *config.City) {
	t.Helper()
	previous := featureflags.Snapshot()
	formulaV2 := rollout.ForTest(rollout.WithFormulaV2(cfg != nil && cfg.Daemon.FormulaV2Enabled())).FormulaV2()
	featureflags.Apply(featureflags.Flags{FormulaV2: formulaV2, GraphApply: formulaV2})
	t.Cleanup(func() { featureflags.Apply(previous) })
}

func lifecycleReadyWireJSON(t *testing.T, candidate beads.Bead, storeRef, scope string) string {
	t.Helper()
	wire := struct {
		beads.Bead
		SourceStoreRef string `json:"source_store_ref"`
		LifecycleScope string `json:"lifecycle_scope"`
	}{Bead: candidate, SourceStoreRef: storeRef, LifecycleScope: scope}
	encoded, err := json.Marshal([]any{wire})
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
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
			Dir:                 "pilot",
			MaxActiveSessions:   &maxSessions,
			DefaultSlingFormula: &workflow,
		}},
		Lifecycle: config.LifecycleConfig{
			AdmissionEnabled:            true,
			AdmissionV2PrimaryAuthority: "triage",
			AdmissionV2Authorities: map[string]string{
				"triage": base64.StdEncoding.EncodeToString(admissionKey.Public().(ed25519.PublicKey)),
			},
			AcceptanceAuthorities: map[string]string{
				"reviewer": base64.StdEncoding.EncodeToString(acceptanceKey.Public().(ed25519.PublicKey)),
			},
		},
	}
	receipt, err := worklifecycle.SignAdmissionReceiptV2(worklifecycle.AdmissionReceiptV2{
		Version: 2, WorkItemID: "work-1", Scope: worklifecycle.ScopeForStore("pilot", "city:pilot"), ExpectedWorkRevision: 1,
		Route: "pilot/worker", Workflow: workflow, RoutingPolicyDigest: strings.Repeat("a", 64), MergeStrategy: "mr", Deliverable: "reviewed patch",
		Verification: "acceptance tests", AcceptanceAuthority: "reviewer", AdmittedBy: "triage",
	}, admissionKey)
	if err != nil {
		t.Fatal(err)
	}
	store := &beads.MemStore{IDPrefix: "work", HonorExplicitIDs: true}
	if _, err := store.Create(beads.Bead{
		ID: "work-1", Title: "lifecycle test", Type: "task", Status: "open",
		Labels:   []string{worklifecycle.AdmissionIntentLabel},
		Metadata: map[string]string{beadmeta.LifecycleAdmissionReceiptV2MetadataKey: receipt},
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
