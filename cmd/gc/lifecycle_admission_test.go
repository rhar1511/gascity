package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
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
	ready, err := beads.HandlesFor(store).Live.Ready(beads.ReadyQuery{TierMode: beads.FederatedReadTier})
	if err != nil {
		t.Fatalf("read ready work after trusted admission: %v", err)
	}
	if !slices.ContainsFunc(ready, func(row beads.Bead) bool { return row.ID == work.ID }) {
		t.Fatalf("trustedly admitted source was routed and materialized but did not become ready: %v", ready)
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

func TestLifecycleGraphV2DescendantClaimRevalidatesLiveSourceAndSkipsLegacyReclaim(t *testing.T) {
	for _, tc := range []struct {
		name        string
		beforeQuery func(t *testing.T, store beads.Store, source beads.Bead)
		staleOwner  string
		wantHold    string
		wantClaim   bool
	}{
		{
			name: "source hold added after ready snapshot",
			beforeQuery: func(t *testing.T, store beads.Store, source beads.Bead) {
				t.Helper()
				if err := store.Update(source.ID, beads.UpdateOpts{Labels: []string{beadmeta.DispatchHoldLabels[0]}}); err != nil {
					t.Fatal(err)
				}
			},
			wantHold: "source admission",
		},
		{
			name: "source materialization becomes incomplete",
			beforeQuery: func(t *testing.T, store beads.Store, source beads.Bead) {
				t.Helper()
				marker, ok := lifecycleMaterializationFor(source)
				if !ok {
					t.Fatal("source has no controller materialization marker")
				}
				marker.State = "reserved"
				value, err := encodeLifecycleMaterialization(marker)
				if err != nil {
					t.Fatal(err)
				}
				if err := store.SetMetadata(source.ID, beadmeta.LifecycleMaterializationMetadataKey, value); err != nil {
					t.Fatal(err)
				}
			},
			wantHold: "graph lineage",
		},
		{
			name:       "legacy stale-owner reclaim is skipped",
			staleOwner: "former-session",
		},
		{
			name:      "fresh graph descendant claim uses lifecycle CAS",
			wantClaim: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, cfg, cityPath := lifecycleAdmissionFixture(t)
			root, descendant := materializeLifecycleGraphV2(t, store, cfg, cityPath, "work-1")
			if !sourceworkflow.IsWorkflowRoot(root) || descendant.ID == "" || !lifecycleMaterializationEvidence(descendant) {
				t.Fatalf("compiled graph lineage missing root/descendant proof: root=%+v descendant=%+v", root, descendant)
			}
			if tc.staleOwner != "" {
				descendant.Assignee = tc.staleOwner
				if err := store.Update(descendant.ID, beads.UpdateOpts{Assignee: &tc.staleOwner}); err != nil {
					t.Fatal(err)
				}
			}
			currentSource, err := store.Get("work-1")
			if err != nil {
				t.Fatal(err)
			}
			readyJSON := lifecycleReadyWireJSON(t, descendant, "city:pilot", worklifecycle.ScopeForStore("pilot", "city:pilot"))
			var claims, reclaims int
			ops := hookClaimOps{
				Runner: func(string, string) (string, error) {
					if tc.beforeQuery != nil {
						tc.beforeQuery(t, store, currentSource)
					}
					return readyJSON, nil
				},
				Claim: func(context.Context, string, []string, string, string) (beads.Bead, bool, error) {
					claims++
					return beads.Bead{}, false, nil
				},
				ReclaimStale: func(context.Context, string, []string, string) (bool, string, error) {
					reclaims++
					return true, "former-session", nil
				},
				DrainAck: func(io.Writer) error { return nil },
			}
			var stdout, stderr bytes.Buffer
			code := doHookClaim("gc ready --json", cityPath, hookClaimOptions{
				Assignee: "worker-session", IdentityCandidates: []string{"worker-session"}, RouteTargets: []string{"worker"},
				Env: nil, DrainAck: true, JSON: true, Lifecycle: cfg.Lifecycle, LifecycleCity: cfg,
				TrustedLifecycleScope: true, AutoReclaimStaleClaims: true,
				ResolveLifecycleStore: func(ref string) (beads.Store, error) {
					if ref != "city:pilot" {
						return nil, beads.ErrNotFound
					}
					return store, nil
				},
			}, ops, &stdout, &stderr)
			if code != 0 {
				t.Fatalf("hook claim = %d, want acknowledged no-work; stderr=%s", code, stderr.String())
			}
			if claims != 0 {
				t.Fatalf("claim path ran %d times although lifecycle authority was absent or reclaim was unsafe", claims)
			}
			if reclaims != 0 {
				t.Fatalf("legacy stale-owner reclaim ran %d times for enrolled descendant", reclaims)
			}
			claimed, err := store.Get(descendant.ID)
			if err != nil {
				t.Fatal(err)
			}
			if (strings.EqualFold(claimed.Status, "in_progress") && claimed.Assignee == "worker-session") != tc.wantClaim {
				t.Fatalf("descendant status/owner = %q/%q, wantClaim=%v; stdout=%q stderr=%q", claimed.Status, claimed.Assignee, tc.wantClaim, stdout.String(), stderr.String())
			}
			if tc.wantHold != "" && !strings.Contains(stderr.String(), tc.wantHold) {
				t.Fatalf("stderr=%q, want explanation containing %q", stderr.String(), tc.wantHold)
			}
		})
	}
}

func TestLifecycleGraphDescendantAssignmentProtectsLiveSessionFromLegacyRestart(t *testing.T) {
	env, session, sessionName := newProgressStallTestEnv(t)
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
	env.cfg.Lifecycle.AdmissionAuthorities = map[string]string{"triage": base64.StdEncoding.EncodeToString(admissionKey.Public().(ed25519.PublicKey))}
	env.cfg.Lifecycle.AcceptanceAuthorities = map[string]string{"reviewer": base64.StdEncoding.EncodeToString(acceptanceKey.Public().(ed25519.PublicKey))}
	scope := worklifecycle.ScopeForStore("test-city", "city:test-city")
	source, err := env.store.Create(beads.Bead{Title: "graph lifecycle work", Type: "task", Status: "open", Labels: []string{worklifecycle.AdmissionIntentLabel}})
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := worklifecycle.SignAdmissionReceipt(worklifecycle.AdmissionReceipt{
		Version: 1, WorkItemID: source.ID, Scope: scope, Route: "worker", Workflow: workflow, MergeStrategy: "mr",
		Deliverable: "reviewed patch", Verification: "tests", AcceptanceAuthority: "reviewer", AdmittedBy: "triage",
	}, admissionKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := env.store.SetMetadata(source.ID, beadmeta.LifecycleAdmissionReceiptMetadataKey, receipt); err != nil {
		t.Fatal(err)
	}
	cityPath := t.TempDir()
	reconcileLifecycleAdmission("test-city", cityPath, env.cfg, env.store, nil, nil, io.Discard)
	source, err = env.store.Get(source.ID)
	if err != nil {
		t.Fatal(err)
	}
	marker, ok := lifecycleMaterializationFor(source)
	if !ok || marker.State != "attached" {
		t.Fatalf("compiled graph admission did not attach source lineage: %+v", marker)
	}
	rows, err := env.store.ListByMetadata(map[string]string{beadmeta.RootBeadIDMetadataKey: marker.WorkflowID}, 0, beads.WithBothTiers)
	if err != nil {
		t.Fatal(err)
	}
	var descendant beads.Bead
	for _, row := range rows {
		if row.ID != marker.WorkflowID && row.Metadata[beadmeta.RoutedToMetadataKey] == "worker" {
			descendant = row
			break
		}
	}
	if descendant.ID == "" || !lifecycleMaterializationEvidence(descendant) {
		t.Fatalf("compiled graph did not carry enrollment onto a runnable descendant: %+v", rows)
	}
	if err := env.store.Update(descendant.ID, beads.UpdateOpts{Assignee: &sessionName}); err != nil {
		t.Fatal(err)
	}
	descendant, err = env.store.Get(descendant.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !lifecycleProtectedWork(descendant, env.cfg) {
		t.Fatal("graph descendant lacks durable protection evidence")
	}
	env.reconcileAtPath(cityPath, []beads.Bead{session})
	if !env.sp.IsRunning(sessionName) {
		t.Fatalf("session %q was stopped while assigned an enrolled graph descendant", sessionName)
	}
	after, err := env.store.Get(descendant.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != "open" || after.Assignee != sessionName {
		t.Fatalf("legacy session restart changed graph ownership: status=%q assignee=%q", after.Status, after.Assignee)
	}
	if strings.Contains(env.stderr.String(), "progress-stalled") || strings.Contains(env.stderr.String(), "restart-requested action") {
		t.Fatalf("stderr=%q, lifecycle-owned graph assignment should suppress legacy restart", env.stderr.String())
	}
	if err := env.store.Close(source.ID); err == nil {
		t.Fatal("ordinary close accepted lifecycle source work without a completion receipt")
	}
	if err := env.store.Close(descendant.ID); err != nil {
		t.Fatalf("ordinary close of an admitted graph descendant must preserve step-completion behavior: %v", err)
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
	digest, err := worklifecycle.AdmissionDigest(decision.Receipt)
	if err != nil {
		t.Fatal(err)
	}
	const rootID = "duplicate-workflow-id"
	reservation, err := encodeLifecycleMaterialization(lifecycleMaterialization{
		Version: 1, State: "reserved", Scope: scope, Contract: digest, Route: "worker", Workflow: "review", MergeStrategy: "mr",
		Token: "reservation", SourceID: source.ID, SourceStoreRef: "city:pilot", WorkflowStoreRef: "class:graph", AdmissionReceipt: source.Metadata[beadmeta.LifecycleAdmissionReceiptMetadataKey],
	})
	if err != nil {
		t.Fatal(err)
	}
	inputConvoy := "input-convoy"
	if err := store.Update(source.ID, beads.UpdateOpts{Metadata: map[string]string{
		beadmeta.LifecycleMaterializationMetadataKey: reservation,
		beadmeta.RoutedToMetadataKey:                 "worker",
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
		Version: 1, State: "lineage_pending", Scope: scope, Contract: digest, Route: "worker", Workflow: "review", MergeStrategy: "mr",
		Token: "reservation", WorkflowID: rootID, SourceID: source.ID, SourceStoreRef: "city:pilot", WorkflowStoreRef: "class:graph",
		AdmissionReceipt: source.Metadata[beadmeta.LifecycleAdmissionReceiptMetadataKey],
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
			beadmeta.ExecutionRoutedToMetadataKey:        "worker",
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
		if child.ID != root.ID && child.Metadata[beadmeta.RoutedToMetadataKey] == "worker" && lifecycleMaterializationEvidence(child) {
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
