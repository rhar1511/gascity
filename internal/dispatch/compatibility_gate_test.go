package dispatch

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/formula"
	"github.com/gastownhall/gascity/internal/formulatest"
	"github.com/gastownhall/gascity/internal/molecule"
)

type dispatchActionGate struct {
	err   error
	calls int
}

func (g *dispatchActionGate) AuthorizeRecipe(context.Context, *formula.Recipe, beads.Store) (molecule.FormulaActionAuthorization, error) {
	return molecule.FormulaActionAuthorization{}, g.err
}

func (g *dispatchActionGate) AuthorizeFragment(context.Context, *formula.FragmentRecipe, beads.Store) (molecule.FormulaActionAuthorization, error) {
	return molecule.FormulaActionAuthorization{}, g.err
}

func (g *dispatchActionGate) RevalidateRecipe(context.Context, *formula.Recipe, beads.Store, molecule.FormulaActionAuthorization) error {
	return g.err
}

func (g *dispatchActionGate) RevalidateFragment(context.Context, *formula.FragmentRecipe, beads.Store, molecule.FormulaActionAuthorization) error {
	return g.err
}

func (g *dispatchActionGate) RevalidateBead(context.Context, beads.Bead, beads.Store) error {
	g.calls++
	return g.err
}

type dispatchFragmentRefusalGate struct {
	dispatchActionGate
	fragmentAuthorization molecule.FormulaActionAuthorization
	fragmentErr           error
	revalidationErr       error
	fragmentAuths         int
	fragmentRevalidations int
}

func (g *dispatchFragmentRefusalGate) AuthorizeFragment(context.Context, *formula.FragmentRecipe, beads.Store) (molecule.FormulaActionAuthorization, error) {
	g.fragmentAuths++
	return g.fragmentAuthorization, g.fragmentErr
}

func (g *dispatchFragmentRefusalGate) RevalidateFragment(context.Context, *formula.FragmentRecipe, beads.Store, molecule.FormulaActionAuthorization) error {
	g.fragmentRevalidations++
	return g.revalidationErr
}

type fanoutAuthorizationFixture struct {
	store    beads.Store
	workflow beads.Bead
	source   beads.Bead
	fanout   beads.Bead
	dir      string
}

func newFanoutAuthorizationFixture(t *testing.T, state string) fanoutAuthorizationFixture {
	t.Helper()
	formulatest.EnableV2ForTest(t)

	dir := t.TempDir()
	expansion := `
formula = "expansion-review"
type = "expansion"
version = 2
contract = "graph.v2"

[[template]]
id = "{target}.review"
title = "Review {reviewer}"
`
	if err := os.WriteFile(filepath.Join(dir, "expansion-review.toml"), []byte(expansion), 0o644); err != nil {
		t.Fatalf("write expansion formula: %v", err)
	}

	store := beads.NewMemStore()
	workflow := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "workflow",
		Type:  "task",
		Metadata: map[string]string{
			beadmeta.KindMetadataKey: beadmeta.KindWorkflow,
		},
	})
	source := mustCreateWorkflowBead(t, store, beads.Bead{
		Title:  "survey",
		Type:   "task",
		Status: "closed",
		Metadata: map[string]string{
			beadmeta.RootBeadIDMetadataKey: workflow.ID,
			beadmeta.StepRefMetadataKey:    "demo.survey",
			beadmeta.OutcomeMetadataKey:    beadmeta.OutcomePass,
			beadmeta.OutputJSONMetadataKey: `{"items":[{"name":"claude"}]}`,
		},
	})
	fanout := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "Expand fanout for survey",
		Type:  "task",
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:            beadmeta.KindFanout,
			beadmeta.RootBeadIDMetadataKey:      workflow.ID,
			beadmeta.ControlForMetadataKey:      "demo.survey",
			beadmeta.ForEachMetadataKey:         "output.items",
			beadmeta.BondMetadataKey:            "expansion-review",
			beadmeta.BondVarsMetadataKey:        `{"reviewer":"{item.name}"}`,
			beadmeta.FanoutModeMetadataKey:      "parallel",
			beadmeta.FanoutStateMetadataKey:     state,
			beadmeta.ControllerErrorMetadataKey: "prior diagnostic",
		},
	})
	mustDepAdd(t, store, fanout.ID, source.ID, "blocks")
	return fanoutAuthorizationFixture{store: store, workflow: workflow, source: source, fanout: fanout, dir: dir}
}

type failFanoutSpawnMarkerStore struct {
	beads.Store
	fanoutID string
	err      error
	fail     bool
}

func (s *failFanoutSpawnMarkerStore) SetMetadataBatch(id string, kvs map[string]string) error {
	if s.fail && id == s.fanoutID && kvs[beadmeta.FanoutStateMetadataKey] == beadmeta.SpawnStateSpawning {
		return s.err
	}
	return s.Store.SetMetadataBatch(id, kvs)
}

func TestProcessControlRevalidatesBeforeOrphanClose(t *testing.T) {
	store := beads.NewMemStore()
	control, err := store.Create(beads.Bead{
		Title: "retry control",
		Type:  "task",
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:         beadmeta.KindRetry,
			beadmeta.RootBeadIDMetadataKey:   "missing-root",
			beadmeta.RootStoreRefMetadataKey: "city:test",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	gate := &dispatchActionGate{err: errors.New("release authorization revoked")}
	_, err = ProcessControl(store, control, ProcessOptions{FormulaActionGate: gate})
	if err == nil {
		t.Fatal("ProcessControl succeeded after formula action authorization was revoked")
	}
	if gate.calls != 1 {
		t.Fatalf("RevalidateBead calls = %d, want 1", gate.calls)
	}
	current, err := store.Get(control.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Status != "open" {
		t.Fatalf("control status after gate refusal = %q, want open", current.Status)
	}
}

func TestProcessControlRequiresConfiguredGateWhenRequested(t *testing.T) {
	store := beads.NewMemStore()
	control, err := store.Create(beads.Bead{Title: "retry control", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = ProcessControl(store, control, ProcessOptions{RequireFormulaActionGate: true})
	if err == nil {
		t.Fatal("ProcessControl succeeded with a required gate missing")
	}
	current, err := store.Get(control.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Status != "open" {
		t.Fatalf("control status after missing gate = %q, want open", current.Status)
	}
}

func TestProcessFanoutFragmentAuthorizationRefusalDoesNotMutateControl(t *testing.T) {
	fixture := newFanoutAuthorizationFixture(t, "")
	before := mustGetBead(t, fixture.store, fixture.fanout.ID)
	beforeMembers := mustDirectMemberIDs(t, fixture.store, fixture.workflow.ID)
	beforeDeps := mustDeps(t, fixture.store, fixture.fanout.ID)

	gate := &dispatchFragmentRefusalGate{fragmentErr: errors.New("descendant compatibility was revoked")}
	_, err := ProcessControl(fixture.store, fixture.fanout, ProcessOptions{
		FormulaSearchPaths:       []string{fixture.dir},
		FormulaActionGate:        gate,
		RequireFormulaActionGate: true,
	})
	if err == nil {
		t.Fatal("ProcessControl(fanout) succeeded after fragment authorization was refused")
	}
	var actionErr *molecule.FormulaActionError
	if !errors.As(err, &actionErr) || !errors.Is(err, gate.fragmentErr) {
		t.Fatalf("ProcessControl(fanout) error = %v, want wrapped FormulaActionError cause", err)
	}
	if gate.calls != 1 {
		t.Fatalf("RevalidateBead calls = %d, want 1 successful parent check", gate.calls)
	}
	if gate.fragmentAuths != 1 {
		t.Fatalf("AuthorizeFragment calls = %d, want 1 refusal", gate.fragmentAuths)
	}
	if gate.fragmentRevalidations != 0 {
		t.Fatalf("RevalidateFragment calls = %d, want 0 after authorization refused", gate.fragmentRevalidations)
	}
	assertFanoutRefusalDidNotMutate(t, fixture, before, beforeMembers, beforeDeps)
}

func TestProcessFanoutRevalidatesReusedFragmentBeforeWiring(t *testing.T) {
	fixture := newFanoutAuthorizationFixture(t, "")
	fragment, err := formula.CompileExpansionFragment(context.Background(), "expansion-review", []string{fixture.dir}, &formula.Step{
		ID:    "demo.survey.item.1",
		Title: fixture.source.Title,
	}, map[string]string{"reviewer": "claude"})
	if err != nil {
		t.Fatalf("CompileExpansionFragment: %v", err)
	}
	if _, err := molecule.InstantiateFragment(context.Background(), fixture.store, fragment, molecule.FragmentOptions{RootID: fixture.workflow.ID}); err != nil {
		t.Fatalf("seed existing fragment: %v", err)
	}
	before := mustGetBead(t, fixture.store, fixture.fanout.ID)
	beforeMembers := mustDirectMemberIDs(t, fixture.store, fixture.workflow.ID)
	beforeDeps := mustDeps(t, fixture.store, fixture.fanout.ID)

	gate := &dispatchFragmentRefusalGate{
		fragmentAuthorization: molecule.FormulaActionAuthorization{
			Required:          true,
			RequestJSON:       `{"formula":"expansion-review"}`,
			AuthorizationJSON: `{"approved":true}`,
		},
		revalidationErr: errors.New("descendant compatibility was revoked"),
	}
	_, err = ProcessControl(fixture.store, fixture.fanout, ProcessOptions{
		FormulaSearchPaths:       []string{fixture.dir},
		FormulaActionGate:        gate,
		RequireFormulaActionGate: true,
	})
	if err == nil {
		t.Fatal("ProcessControl(fanout resume) succeeded after fragment authorization was refused")
	}
	var actionErr *molecule.FormulaActionError
	if !errors.As(err, &actionErr) || !errors.Is(err, gate.revalidationErr) {
		t.Fatalf("ProcessControl(fanout resume) error = %v, want wrapped FormulaActionError cause", err)
	}
	if gate.calls != 1 {
		t.Fatalf("RevalidateBead calls = %d, want 1 successful parent check", gate.calls)
	}
	if gate.fragmentAuths != 1 {
		t.Fatalf("AuthorizeFragment calls = %d, want 1 approval for reused fragment", gate.fragmentAuths)
	}
	if gate.fragmentRevalidations != 1 {
		t.Fatalf("RevalidateFragment calls = %d, want 1 refusal for reused fragment", gate.fragmentRevalidations)
	}
	assertFanoutRefusalDidNotMutate(t, fixture, before, beforeMembers, beforeDeps)
}

func TestProcessFanoutMarkerFailureRetriesExistingFragment(t *testing.T) {
	fixture := newFanoutAuthorizationFixture(t, "")
	before := mustGetBead(t, fixture.store, fixture.fanout.ID)
	beforeMembers := mustDirectMemberIDs(t, fixture.store, fixture.workflow.ID)
	beforeDeps := mustDeps(t, fixture.store, fixture.fanout.ID)
	writeErr := errors.New("spawn marker write failed")
	store := &failFanoutSpawnMarkerStore{Store: fixture.store, fanoutID: fixture.fanout.ID, err: writeErr, fail: true}
	fixture.store = store

	_, err := ProcessControl(fixture.store, fixture.fanout, ProcessOptions{FormulaSearchPaths: []string{fixture.dir}})
	if !errors.Is(err, ErrControlPending) || !errors.Is(err, writeErr) {
		t.Fatalf("ProcessControl after marker failure = %v, want pending with original store error", err)
	}
	afterFailure := mustGetBead(t, fixture.store, fixture.fanout.ID)
	if !reflect.DeepEqual(afterFailure, before) {
		t.Fatalf("fanout changed after marker write failure:\nbefore: %#v\nafter:  %#v", before, afterFailure)
	}
	if !reflect.DeepEqual(mustDeps(t, fixture.store, fixture.fanout.ID), beforeDeps) {
		t.Fatal("fanout dependencies changed before the marker write succeeded")
	}
	afterFailureMembers := mustDirectMemberIDs(t, fixture.store, fixture.workflow.ID)
	if len(afterFailureMembers) != len(beforeMembers)+1 {
		t.Fatalf("workflow members after marker failure = %v, want one created fragment over %v", afterFailureMembers, beforeMembers)
	}
	child := findWorkflowBeadByRef(t, fixture.store, fixture.workflow.ID, "expansion-review.demo.survey.item.1.review")
	if child.ID == "" {
		t.Fatal("missing fragment created before marker failure")
	}

	store.fail = false
	result, err := ProcessControl(fixture.store, fixture.fanout, ProcessOptions{FormulaSearchPaths: []string{fixture.dir}})
	if err != nil {
		t.Fatalf("ProcessControl retry after marker failure: %v", err)
	}
	if !result.Processed || result.Action != "fanout-spawn" {
		t.Fatalf("retry result = %+v, want processed fanout-spawn", result)
	}
	if result.Created != 0 {
		t.Fatalf("retry created %d beads, want reuse of the existing fragment", result.Created)
	}
	reused := findWorkflowBeadByRef(t, fixture.store, fixture.workflow.ID, "expansion-review.demo.survey.item.1.review")
	if reused.ID != child.ID {
		t.Fatalf("retry fragment ID = %q, want existing ID %q", reused.ID, child.ID)
	}
	if got := mustDirectMemberIDs(t, fixture.store, fixture.workflow.ID); !reflect.DeepEqual(got, afterFailureMembers) {
		t.Fatalf("workflow members after retry = %v, want unchanged %v", got, afterFailureMembers)
	}
}

func assertFanoutRefusalDidNotMutate(t *testing.T, fixture fanoutAuthorizationFixture, before beads.Bead, beforeMembers []string, beforeDeps []beads.Dep) {
	t.Helper()
	current := mustGetBead(t, fixture.store, fixture.fanout.ID)
	if !reflect.DeepEqual(current, before) {
		t.Fatalf("fanout changed after descendant refusal:\nbefore: %#v\nafter:  %#v", before, current)
	}
	afterMembers := mustDirectMemberIDs(t, fixture.store, fixture.workflow.ID)
	if !reflect.DeepEqual(afterMembers, beforeMembers) {
		t.Fatalf("workflow members after descendant refusal = %v, want unchanged %v", afterMembers, beforeMembers)
	}
	afterDeps := mustDeps(t, fixture.store, fixture.fanout.ID)
	if !reflect.DeepEqual(afterDeps, beforeDeps) {
		t.Fatalf("fanout dependencies after descendant refusal = %#v, want unchanged %#v", afterDeps, beforeDeps)
	}
}

func mustDirectMemberIDs(t *testing.T, store beads.Store, rootID string) []string {
	t.Helper()
	members, err := beads.DirectMembers(store, rootID)
	if err != nil {
		t.Fatalf("list workflow members: %v", err)
	}
	ids := make([]string, 0, len(members))
	for _, member := range members {
		ids = append(ids, member.ID)
	}
	slices.Sort(ids)
	return ids
}

func mustDeps(t *testing.T, store beads.Store, beadID string) []beads.Dep {
	t.Helper()
	deps, err := store.DepList(beadID, "down")
	if err != nil {
		t.Fatalf("list dependencies for %s: %v", beadID, err)
	}
	return deps
}
