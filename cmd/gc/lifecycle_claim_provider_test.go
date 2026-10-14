package main

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/attemptevidence"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/storeref"
	"github.com/gastownhall/gascity/internal/worklifecycle"
)

func TestLifecycleClaimProviderRechecksCanonicalReadiness(t *testing.T) {
	for _, tc := range []struct {
		name    string
		prepare func(t *testing.T, setup lifecycleAdmissionTransitionSetup, store *lifecycleClaimReadinessTestStore)
	}{
		{
			name: "dispatch hold",
			prepare: func(_ *testing.T, _ lifecycleAdmissionTransitionSetup, store *lifecycleClaimReadinessTestStore) {
				store.overlay = func(current beads.Bead) beads.Bead {
					current.Labels = append(current.Labels, beadmeta.DispatchHoldLabels[0])
					return current
				}
			},
		},
		{
			name: "deferred source",
			prepare: func(_ *testing.T, _ lifecycleAdmissionTransitionSetup, store *lifecycleClaimReadinessTestStore) {
				deferUntil := time.Now().UTC().Add(time.Hour)
				store.overlay = func(current beads.Bead) beads.Bead {
					current.DeferUntil = &deferUntil
					return current
				}
			},
		},
		{
			name: "blocked dependency",
			prepare: func(t *testing.T, setup lifecycleAdmissionTransitionSetup, _ *lifecycleClaimReadinessTestStore) {
				seedLifecycleClaimBlocker(t, setup)
			},
		},
		{
			name: "dependency blocks after proof and before live readiness read",
			prepare: func(t *testing.T, setup lifecycleAdmissionTransitionSetup, store *lifecycleClaimReadinessTestStore) {
				store.beforeReady = func() error {
					seedLifecycleClaimBlocker(t, setup)
					return nil
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setup, state, request := newLifecycleClaimProviderFixture(t)
			readinessStore := &lifecycleClaimReadinessTestStore{lifecycleAdmissionTransitionTestStore: setup.store}
			tc.prepare(t, setup, readinessStore)
			setup.rigStores["pilot"] = readinessStore
			state.beadStores = setup.rigStores
			request.WorkStore = readinessStore

			beforeRequests := len(setup.store.patchRequests)
			beforeReceipts := len(setup.store.patchReceipts)
			beforeGenericWrites := setup.store.sourceGenericWrites
			if _, err := state.ClaimLifecycleWork(context.Background(), request); err == nil {
				t.Fatal("provider claimed work that was no longer canonically ready")
			} else if !errors.Is(err, worklifecycle.ErrTransitionChainStale) {
				t.Fatalf("claim error = %v, want a stale-readiness refusal", err)
			}
			if len(setup.store.patchRequests) != beforeRequests || len(setup.store.patchReceipts) != beforeReceipts || setup.store.sourceGenericWrites != beforeGenericWrites {
				t.Fatalf("readiness refusal wrote source state: patches=%d/%d receipts=%d/%d generic=%d/%d",
					len(setup.store.patchRequests), beforeRequests, len(setup.store.patchReceipts), beforeReceipts,
					setup.store.sourceGenericWrites, beforeGenericWrites)
			}
			current, err := setup.store.Get(request.Work.ID)
			if err != nil {
				t.Fatal(err)
			}
			if current.Status != "open" || current.Assignee != "" {
				t.Fatalf("readiness refusal changed source owner/status to %q/%q", current.Status, current.Assignee)
			}
			if tc.name == "dependency blocks after proof and before live readiness read" && !readinessStore.readinessRaceRan {
				t.Fatal("test did not change dependency readiness at the final live-read boundary")
			}
		})
	}
}

// Persist the intended route before invoking the provider: its request is a
// snapshot of a real session, not a replacement identity supplied by the caller.
func persistLifecycleClaimFixtureRoute(t *testing.T, setup lifecycleAdmissionTransitionSetup, request *api.LifecycleClaimTransitionRequest) {
	t.Helper()
	store := sessionFrontDoor(setup.cityStore)
	current, err := store.Get(request.Session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpdateMetadataInfo(current, session.MetadataPatch{
		"template": request.Session.Template, "agent_name": request.Session.AgentName,
		"pool_slot": request.Session.PoolSlot, "common_name": request.Session.CommonName,
	}); err != nil {
		t.Fatal(err)
	}
	// The missing-route fixture also removes the persisted identity labels.
	if request.Session.Labels == nil {
		if err := setup.cityStore.Update(current.ID, beads.UpdateOpts{RemoveLabels: current.Labels}); err != nil {
			t.Fatal(err)
		}
	}
	request.Session, err = store.Get(current.ID)
	if err != nil {
		t.Fatal(err)
	}
}

func TestLifecycleClaimProviderClaimsReadyWorkAndReplaysExactClaim(t *testing.T) {
	for _, tc := range []struct {
		name         string
		template     string
		agentName    string
		poolSlot     string
		nextRevision int64
		descendant   bool
	}{
		{name: "configured base", template: "pilot/worker", agentName: "pilot/worker"},
		{name: "generated workflow step", template: "pilot/worker", agentName: "pilot/worker", descendant: true},
		{name: "negative revision", template: "pilot/worker", agentName: "pilot/worker", nextRevision: -1000},
		{name: "canonical pool slot", template: "pilot/worker-2", agentName: "pilot/worker-2", poolSlot: "2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var setup lifecycleAdmissionTransitionSetup
			if tc.descendant {
				fixture := newLifecycleAdmissionPolicyFixture(t, "formula = \"review\"\nversion = 1\n\n[[steps]]\nid = \"work\"\ntitle = \"Review work\"\ntype = \"bug\"\n")
				setup = newLifecycleAdmissionTransitionSetupWithFixture(t, fixture, false)
			} else {
				setup = newLifecycleAdmissionTransitionSetup(t)
			}
			setup, state, request := newLifecycleClaimProviderFixtureForSetup(t, setup, tc.nextRevision)
			request.Session.Template = tc.template
			request.Session.AgentName = tc.agentName
			request.Session.PoolSlot = tc.poolSlot
			persistLifecycleClaimFixtureRoute(t, setup, &request)
			if tc.descendant {
				marker, ok := lifecycleMaterializationFor(request.Work)
				if !ok {
					t.Fatal("source has no attached workflow")
				}
				request.Work = lifecycleAdmissionV1StepCandidate(t, setup, marker)
				if request.Work.ID == marker.WorkflowID {
					t.Fatal("fixture selected the workflow root instead of a step")
				}
				request.ExpectedRevision = request.Work.Revision
				request.ExpectedTransitionHead = request.Work.Metadata[beadmeta.LifecycleTransitionHeadMetadataKey]
			}
			beforeRequests := len(setup.store.patchRequests)
			claimed, err := state.ClaimLifecycleWork(context.Background(), request)
			if err != nil {
				t.Fatalf("claim currently ready admitted work: %v (patches=%d, anchors=%d)", err, len(setup.store.patchRequests)-beforeRequests, len(setup.store.q43Receipts))
			}
			if claimed.ClaimGeneration != "1" || claimed.ReceiptID == "" || claimed.Replayed || claimed.Recovered {
				t.Fatalf("claim result = %+v, want a fresh generation-1 transition", claimed)
			}
			current, err := setup.store.Get(request.Work.ID)
			if err != nil {
				t.Fatal(err)
			}
			if current.Status != "in_progress" || current.Assignee != request.Actor || len(setup.store.patchRequests) != beforeRequests+1 {
				t.Fatalf("source after claim = %q/%q with %d transition writes, want claimed once", current.Status, current.Assignee, len(setup.store.patchRequests)-beforeRequests)
			}

			replayed, err := state.ClaimLifecycleWork(context.Background(), request)
			if err != nil {
				t.Fatalf("replay exact claim after a lost response: %v", err)
			}
			if !replayed.Replayed || replayed.ReceiptID != claimed.ReceiptID || replayed.ClaimGeneration != claimed.ClaimGeneration || len(setup.store.patchRequests) != beforeRequests+1 {
				t.Fatalf("claim replay = %+v with %d transition writes, want the original receipt without another write", replayed, len(setup.store.patchRequests)-beforeRequests)
			}
		})
	}
}

func TestLifecycleClaimProviderRejectsSessionOnDifferentConfiguredRouteBeforeWrites(t *testing.T) {
	for _, tc := range []struct {
		name    string
		prepare func(*lifecycleAdmissionTransitionSetup, *api.LifecycleClaimTransitionRequest)
	}{
		{
			name: "wrong configured route",
			prepare: func(setup *lifecycleAdmissionTransitionSetup, request *api.LifecycleClaimTransitionRequest) {
				setup.fixture.cfg.Agents = append(setup.fixture.cfg.Agents, config.Agent{Name: "other-worker", Dir: "pilot"})
				request.Session.Template = "pilot/other-worker"
				request.Session.AgentName = "pilot/other-worker"
			},
		},
		{
			name: "missing configured route",
			prepare: func(_ *lifecycleAdmissionTransitionSetup, request *api.LifecycleClaimTransitionRequest) {
				request.Session.Template = ""
				request.Session.AgentName = ""
				request.Session.CommonName = ""
				request.Session.Labels = nil
			},
		},
		{
			name: "ambiguous pool slot identity",
			prepare: func(setup *lifecycleAdmissionTransitionSetup, request *api.LifecycleClaimTransitionRequest) {
				setup.fixture.cfg.Agents = append(setup.fixture.cfg.Agents, config.Agent{Name: "worker", Dir: "other"})
				request.Session.Template = "worker-2"
				request.Session.AgentName = "worker-2"
				request.Session.PoolSlot = "2"
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setup, state, request := newLifecycleClaimProviderFixture(t)
			tc.prepare(&setup, &request)
			persistLifecycleClaimFixtureRoute(t, setup, &request)
			beforeRequests := len(setup.store.patchRequests)
			beforeReceipts := len(setup.store.patchReceipts)
			beforeGenericWrites := setup.store.sourceGenericWrites
			if _, err := state.ClaimLifecycleWork(context.Background(), request); err == nil {
				t.Fatal("provider accepted a session that does not resolve to the signed admission route")
			} else if !errors.Is(err, worklifecycle.ErrTransitionChainEvidence) {
				t.Fatalf("claim error = %v, want a route-evidence refusal", err)
			}
			if len(setup.store.patchRequests) != beforeRequests || len(setup.store.patchReceipts) != beforeReceipts || setup.store.sourceGenericWrites != beforeGenericWrites {
				t.Fatalf("route refusal wrote source state: patches=%d/%d receipts=%d/%d generic=%d/%d",
					len(setup.store.patchRequests), beforeRequests, len(setup.store.patchReceipts), beforeReceipts,
					setup.store.sourceGenericWrites, beforeGenericWrites)
			}
			current, err := setup.store.Get(request.Work.ID)
			if err != nil {
				t.Fatal(err)
			}
			if current.Status != "open" || current.Assignee != "" {
				t.Fatalf("route refusal changed source owner/status to %q/%q", current.Status, current.Assignee)
			}
		})
	}
}

func TestLifecycleClaimProviderRejectsWrongRouteBeforeExactReplay(t *testing.T) {
	setup, state, request := newLifecycleClaimProviderFixture(t)
	if _, err := state.ClaimLifecycleWork(context.Background(), request); err != nil {
		t.Fatalf("initial claim on the signed route: %v", err)
	}
	setup.fixture.cfg.Agents = append(setup.fixture.cfg.Agents, config.Agent{Name: "other-worker", Dir: "pilot"})
	request.Session.Template = "pilot/other-worker"
	request.Session.AgentName = "pilot/other-worker"
	persistLifecycleClaimFixtureRoute(t, setup, &request)
	beforeRequests := len(setup.store.patchRequests)
	beforeReceipts := len(setup.store.patchReceipts)
	beforeGenericWrites := setup.store.sourceGenericWrites
	if _, err := state.ClaimLifecycleWork(context.Background(), request); err == nil {
		t.Fatal("provider replayed an exact claim through a session on another configured route")
	} else if !errors.Is(err, worklifecycle.ErrTransitionChainEvidence) {
		t.Fatalf("claim replay error = %v, want a route-evidence refusal", err)
	}
	if len(setup.store.patchRequests) != beforeRequests || len(setup.store.patchReceipts) != beforeReceipts || setup.store.sourceGenericWrites != beforeGenericWrites {
		t.Fatalf("route refusal added writes: patches=%d/%d receipts=%d/%d generic=%d/%d",
			len(setup.store.patchRequests), beforeRequests, len(setup.store.patchReceipts), beforeReceipts,
			setup.store.sourceGenericWrites, beforeGenericWrites)
	}
}

type lifecycleClaimReadinessTestStore struct {
	*lifecycleAdmissionTransitionTestStore
	overlay          func(beads.Bead) beads.Bead
	beforeReady      func() error
	readinessRaceRan bool
}

func (s *lifecycleClaimReadinessTestStore) Get(id string) (beads.Bead, error) {
	current, err := s.lifecycleAdmissionTransitionTestStore.Get(id)
	if err == nil && s.overlay != nil {
		current = s.overlay(current)
	}
	return current, err
}

func (s *lifecycleClaimReadinessTestStore) Ready(queries ...beads.ReadyQuery) ([]beads.Bead, error) {
	if s.beforeReady != nil {
		beforeReady := s.beforeReady
		s.beforeReady = nil
		s.readinessRaceRan = true
		if err := beforeReady(); err != nil {
			return nil, err
		}
	}
	return s.lifecycleAdmissionTransitionTestStore.Ready(queries...)
}

func newLifecycleClaimProviderFixture(t *testing.T) (lifecycleAdmissionTransitionSetup, *controllerState, api.LifecycleClaimTransitionRequest) {
	t.Helper()
	return newLifecycleClaimProviderFixtureWithRevision(t, 0)
}

func newLifecycleClaimProviderFixtureWithRevision(t *testing.T, revision int64) (lifecycleAdmissionTransitionSetup, *controllerState, api.LifecycleClaimTransitionRequest) {
	t.Helper()
	return newLifecycleClaimProviderFixtureForSetup(t, newLifecycleAdmissionTransitionSetup(t), revision)
}

func newLifecycleClaimProviderFixtureForSetup(t *testing.T, setup lifecycleAdmissionTransitionSetup, revision int64) (lifecycleAdmissionTransitionSetup, *controllerState, api.LifecycleClaimTransitionRequest) {
	t.Helper()
	setup.store.nextPatchRevision = revision
	var stderr strings.Builder
	reconcileLifecycleAdmissionWithPermitResolver("pilot", setup.fixture.cityPath, setup.fixture.cfg, setup.cityStore, setup.rigStores, nil, &stderr, setup.permitResolver)
	current, err := setup.store.Get(setup.source.ID)
	if err != nil {
		t.Fatalf("read admitted lifecycle source: %v", err)
	}
	marker, ok := lifecycleMaterializationFor(current)
	if !ok || marker.State != "attached" || strings.TrimSpace(current.Metadata[beadmeta.LifecycleTransitionHeadMetadataKey]) == "" {
		t.Fatalf("lifecycle source was not attached before claim test: marker=%+v stderr=%s", marker, stderr.String())
	}
	state := &controllerState{
		cfg: setup.fixture.cfg, cityName: "pilot", cityPath: setup.fixture.cityPath,
		cityBeadStore: setup.cityStore, beadStores: setup.rigStores,
		beadsPermitResolver: setup.permitResolver, graphStoreGeneration: 1,
	}
	info, err := session.NewStore(beads.SessionStore{Store: setup.cityStore}).CreateSessionInfo(session.CreateSpec{
		Title: "worker", AgentName: "pilot/worker", Metadata: map[string]string{
			"template": "pilot/worker", "agent_name": "pilot/worker", "generation": "1", "instance_token": "instance-token",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	actor := session.AssigneeIdentifier(info)
	request := api.LifecycleClaimTransitionRequest{
		Work: current, WorkStore: setup.store, WorkStoreRef: storeref.RigRef("pilot"),
		Scope:   worklifecycle.ScopeForStore("pilot", string(storeref.RigRef("pilot"))),
		Session: info, Actor: actor, ExpectedRevision: current.Revision,
		ExpectedTransitionHead: current.Metadata[beadmeta.LifecycleTransitionHeadMetadataKey],
	}
	return setup, state, request
}

func seedLifecycleClaimBlocker(t *testing.T, setup lifecycleAdmissionTransitionSetup) {
	t.Helper()
	blocker, err := setup.store.Create(beads.Bead{
		ID: "pilot-rig-claim-blocker", Title: "open dependency", Type: "task", Status: "open",
	})
	if err != nil {
		t.Fatalf("create open claim blocker: %v", err)
	}
	if err := setup.store.lifecycleAdmissionBackingStore.DepAdd(setup.source.ID, blocker.ID, "blocks"); err != nil {
		t.Fatalf("block admitted source: %v", err)
	}
}

func newLifecycleDescendantClaimProviderFixture(t *testing.T) (lifecycleAdmissionTransitionSetup, *controllerState, api.LifecycleClaimTransitionRequest) {
	t.Helper()
	fixture := newLifecycleAdmissionPolicyFixture(t, "formula = \"review\"\nversion = 1\n\n[[steps]]\nid = \"work\"\ntitle = \"Review work\"\ntype = \"bug\"\n")
	setup, state, request := newLifecycleClaimProviderFixtureForSetup(t, newLifecycleAdmissionTransitionSetupWithFixture(t, fixture, false), 0)
	marker, ok := lifecycleMaterializationFor(request.Work)
	if !ok {
		t.Fatal("source has no workflow")
	}
	request.Work = lifecycleAdmissionV1StepCandidate(t, setup, marker)
	request.ExpectedRevision = request.Work.Revision
	request.ExpectedTransitionHead = ""
	return setup, state, request
}

func TestLifecycleDescendantClaimRefusesInvalidAuthorityBeforeWrites(t *testing.T) {
	for _, scenario := range []string{"missing parent attachment", "missing parent transition", "changed policy", "wrong session route", "stale step revision", "forged parent", "cross store", "held step", "v1 scaffolding"} {
		t.Run(scenario, func(t *testing.T) {
			setup, state, request := newLifecycleDescendantClaimProviderFixture(t)
			switch scenario {
			case "missing parent attachment":
				clear(setup.store.q43Receipts)
			case "missing parent transition":
				clear(setup.store.patchReceipts)
			case "changed policy":
				workflow := "other-workflow"
				setup.fixture.cfg.Agents[0].DefaultSlingFormula = &workflow
			case "wrong session route":
				request.Session.Template, request.Session.AgentName = "pilot/other", "pilot/other"
			case "stale step revision":
				request.ExpectedRevision--
			case "forged parent", "cross store":
				marker, ok := lifecycleMaterializationFor(request.Work)
				if !ok {
					t.Fatal("step has no lineage")
				}
				if scenario == "forged parent" {
					marker.SourceID = "other-source"
				} else {
					marker.WorkflowStoreRef = "rig:other"
				}
				encoded, err := encodeLifecycleMaterialization(marker)
				if err != nil {
					t.Fatal(err)
				}
				row := request.Work
				row.Metadata = cloneLifecycleAdmissionMetadata(row.Metadata)
				row.Metadata[beadmeta.LifecycleMaterializationMetadataKey] = encoded
				setup.store.rows[row.ID] = row
			case "held step":
				row := request.Work
				row.Labels = append(row.Labels, beadmeta.DispatchHoldLabels[0])
				setup.store.rows[row.ID] = row
			case "v1 scaffolding":
				setup, state, request = newLifecycleClaimProviderFixture(t)
				marker, _ := lifecycleMaterializationFor(request.Work)
				request.Work = lifecycleAdmissionV1StepCandidate(t, setup, marker)
				request.ExpectedRevision, request.ExpectedTransitionHead = request.Work.Revision, ""
			}
			anchors, patches := len(setup.store.q43Receipts), len(setup.store.patchRequests)
			if _, err := state.ClaimLifecycleWork(context.Background(), request); err == nil {
				t.Fatal("invalid descendant authority claimed work")
			}
			if len(setup.store.q43Receipts) != anchors || len(setup.store.patchRequests) != patches {
				t.Fatal("refused descendant wrote an anchor or claim")
			}
		})
	}
}

func TestLifecycleDescendantClaimRecoversInterruptedWrites(t *testing.T) {
	for _, scenario := range []string{"claim interrupted after anchor", "claim response lost", "negative step revision"} {
		t.Run(scenario, func(t *testing.T) {
			setup, state, request := newLifecycleDescendantClaimProviderFixture(t)
			beforeAnchors, beforeClaims := len(setup.store.q43Receipts), len(setup.store.patchReceipts)
			switch scenario {
			case "claim interrupted after anchor":
				setup.store.failNextPatchKind = "lifecycle_descendant_claim_identity_v1"
				if _, err := state.ClaimLifecycleWork(context.Background(), request); err == nil {
					t.Fatal("interrupted claim returned success")
				}
				if len(setup.store.q43Receipts) != beforeAnchors+1 || len(setup.store.patchReceipts) != beforeClaims {
					t.Fatal("interruption did not leave exactly the committed anchor")
				}
			case "claim response lost":
				setup.store.loseNextPatchKind = "lifecycle_descendant_claim_identity_v1"
			case "negative step revision":
				row := request.Work
				row.Revision = -3000
				setup.store.rows[row.ID] = row
				request.Work, request.ExpectedRevision = row, row.Revision
			}
			claimed, err := state.ClaimLifecycleWork(context.Background(), request)
			if err != nil {
				t.Fatalf("recover descendant claim: %v", err)
			}
			if claimed.Recovered != (scenario == "claim response lost") || claimed.ClaimGeneration != "1" {
				t.Fatalf("unexpected recovered claim: %+v", claimed)
			}
			if len(setup.store.q43Receipts) != beforeAnchors+1 || len(setup.store.patchReceipts) != beforeClaims+1 {
				t.Fatal("claim recovery duplicated durable mutations")
			}
			writes := len(setup.store.patchRequests)
			replay, err := state.ClaimLifecycleWork(context.Background(), request)
			if err != nil || !replay.Replayed || replay.ReceiptID != claimed.ReceiptID || len(setup.store.patchRequests) != writes {
				t.Fatalf("exact replay: %+v, %v", replay, err)
			}
			request.Session.InstanceToken = "different-incarnation"
			if _, err := state.ClaimLifecycleWork(context.Background(), request); err == nil {
				t.Fatal("another incarnation replayed the old claim")
			}
			if len(setup.store.q43Receipts) != beforeAnchors+1 || len(setup.store.patchRequests) != writes {
				t.Fatal("wrong incarnation wrote another transition")
			}
		})
	}
}

func newLifecycleCoreGraphClaimProviderFixture(t *testing.T) (lifecycleAdmissionTransitionSetup, *controllerState, api.LifecycleClaimTransitionRequest) {
	t.Helper()
	setup, state, request := newLifecycleClaimProviderFixtureForSetup(t, newLifecycleAdmissionCoreGraphSetup(t), 0)
	ready, err := setup.store.Ready(beads.ReadyQuery{TierMode: beads.FederatedReadTier})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, row := range ready {
		if row.Metadata[beadmeta.StepRefMetadataKey] == "mol-do-work.do-work" {
			request.Work, found = row, true
			break
		}
	}
	if !found {
		t.Fatal("configured core graph has no ready worker step")
	}
	request.ExpectedRevision, request.ExpectedTransitionHead = request.Work.Revision, ""
	return setup, state, request
}

func TestLifecycleDescendantClaimCoreGraphWorkerStep(t *testing.T) {
	setup, state, request := newLifecycleCoreGraphClaimProviderFixture(t)
	before := len(setup.store.patchRequests)
	claimed, err := state.ClaimLifecycleWork(context.Background(), request)
	if err != nil {
		t.Fatalf("claim configured graph worker step: %v", err)
	}
	if claimed.ClaimGeneration != "1" || claimed.ReceiptID == "" || len(setup.store.patchRequests) != before+1 {
		t.Fatalf("graph worker claim: %+v", claimed)
	}
	replay, err := state.ClaimLifecycleWork(context.Background(), request)
	if err != nil || !replay.Replayed || replay.ReceiptID != claimed.ReceiptID || len(setup.store.patchRequests) != before+1 {
		t.Fatalf("graph worker replay: %+v, %v", replay, err)
	}
}

func TestLifecycleDescendantClaimRejectsUnprovenGraphStep(t *testing.T) {
	for _, scenario := range []string{"changed description", "unknown step", "duplicate step", "duplicate closed step", "wrong parent", "missing dependency"} {
		t.Run(scenario, func(t *testing.T) {
			setup, state, request := newLifecycleCoreGraphClaimProviderFixture(t)
			row := request.Work
			row.Metadata = cloneLifecycleAdmissionMetadata(row.Metadata)
			switch scenario {
			case "changed description":
				row.Description += " changed worker instructions"
			case "unknown step":
				row.Metadata[beadmeta.StepRefMetadataKey] = "mol-do-work.unsigned"
			case "wrong parent":
				row.ParentID = "other-workflow"
			case "duplicate step", "duplicate closed step":
				duplicate := row
				duplicate.ID = ""
				if scenario == "duplicate closed step" {
					duplicate.Status = "closed"
				}
				if _, err := setup.store.Create(duplicate); err != nil {
					t.Fatal(err)
				}
			case "missing dependency":
				deps, err := setup.store.DepList(row.ID, "down")
				if err != nil || len(deps) == 0 {
					t.Fatalf("graph has no dependency to remove: %v", err)
				}
				if err := setup.store.DepRemove(row.ID, deps[0].DependsOnID); err != nil {
					t.Fatal(err)
				}
				row, err = setup.store.Get(row.ID)
				if err != nil {
					t.Fatal(err)
				}
				request.ExpectedRevision = row.Revision
			}
			setup.store.rows[row.ID] = row
			anchors, patches := len(setup.store.q43Receipts), len(setup.store.patchRequests)
			if _, err := state.ClaimLifecycleWork(context.Background(), request); err == nil {
				t.Fatal("unproven graph worker step was claimed")
			}
			if len(setup.store.q43Receipts) != anchors || len(setup.store.patchRequests) != patches {
				t.Fatal("unproven graph step wrote an anchor or claim")
			}
		})
	}
}

func TestLifecycleSourceClaimRejectsUnverifiedExecutionBeforeWrites(t *testing.T) {
	setup, state, request := newLifecycleClaimProviderFixture(t)
	request.ExecutionWork = beads.Bead{ID: "unrelated-step", Status: "in_progress", Assignee: request.Actor, Revision: 1}
	request.ExecutionClaimGeneration = "1"
	before := len(setup.store.patchRequests)
	if _, err := state.ClaimLifecycleWork(context.Background(), request); err == nil {
		t.Fatal("source was claimed using an unverified execution step")
	}
	if len(setup.store.patchRequests) != before {
		t.Fatal("refusal changed protected source")
	}
}

func TestLifecycleSourceClaimAfterExecutionClaimAndReplay(t *testing.T) {
	setup, state, stepRequest := newLifecycleCoreGraphClaimProviderFixture(t)
	stepClaim, err := state.ClaimLifecycleWork(context.Background(), stepRequest)
	if err != nil {
		t.Fatal(err)
	}
	step, err := setup.store.Get(stepRequest.Work.ID)
	if err != nil {
		t.Fatal(err)
	}
	source, err := setup.store.Get(setup.source.ID)
	if err != nil {
		t.Fatal(err)
	}
	request := stepRequest
	request.Work = source
	request.ExpectedRevision = source.Revision
	request.ExpectedTransitionHead = source.Metadata[beadmeta.LifecycleTransitionHeadMetadataKey]
	request.ExecutionWork = step
	request.ExecutionClaimGeneration = stepClaim.ClaimGeneration
	before := len(setup.store.patchRequests)
	sourceClaim, err := state.ClaimLifecycleWork(context.Background(), request)
	if err != nil {
		t.Fatalf("begin source after claiming execution step: %v", err)
	}
	if sourceClaim.Replayed || sourceClaim.ClaimGeneration != "1" || len(setup.store.patchRequests) != before+1 {
		t.Fatalf("source claim = %+v with %d new transitions", sourceClaim, len(setup.store.patchRequests)-before)
	}
	replay, err := state.ClaimLifecycleWork(context.Background(), request)
	if err != nil || !replay.Replayed || replay.ReceiptID != sourceClaim.ReceiptID || len(setup.store.patchRequests) != before+1 {
		t.Fatalf("source replay = %+v, %v", replay, err)
	}
	unchanged, err := setup.store.Get(step.ID)
	if err != nil || unchanged.Revision != step.Revision || unchanged.Metadata[beadmeta.LifecycleTransitionHeadMetadataKey] != stepClaim.ReceiptID {
		t.Fatalf("source claim changed execution step: %+v, %v", unchanged, err)
	}
}

func TestLifecycleSourceClaimRefusesChangedExecutionAuthority(t *testing.T) {
	for _, scenario := range []string{"missing child receipt", "changed incarnation", "changed generation", "changed step revision", "changed parent", "changed instructions", "held step", "changed policy"} {
		t.Run(scenario, func(t *testing.T) {
			setup, state, request := newLifecycleCoreGraphClaimProviderFixture(t)
			claim, err := state.ClaimLifecycleWork(context.Background(), request)
			if err != nil {
				t.Fatal(err)
			}
			step, err := setup.store.Get(request.Work.ID)
			if err != nil {
				t.Fatal(err)
			}
			source, err := setup.store.Get(setup.source.ID)
			if err != nil {
				t.Fatal(err)
			}
			request.Work, request.ExpectedRevision, request.ExpectedTransitionHead = source, source.Revision, source.Metadata[beadmeta.LifecycleTransitionHeadMetadataKey]
			request.ExecutionWork, request.ExecutionClaimGeneration = step, claim.ClaimGeneration
			switch scenario {
			case "missing child receipt":
				delete(setup.store.patchReceipts, claim.ReceiptID)
			case "changed incarnation":
				request.Session.InstanceToken = "another-incarnation"
			case "changed generation":
				request.ExecutionClaimGeneration = "2"
			case "changed step revision":
				request.ExecutionWork.Revision--
			case "changed parent":
				marker, _ := lifecycleMaterializationFor(step)
				marker.SourceID = "another-source"
				value, err := encodeLifecycleMaterialization(marker)
				if err != nil {
					t.Fatal(err)
				}
				step.Metadata = cloneLifecycleAdmissionMetadata(step.Metadata)
				step.Metadata[beadmeta.LifecycleMaterializationMetadataKey] = value
				setup.store.rows[step.ID] = step
				request.ExecutionWork = step
			case "changed instructions":
				step.Description += " altered instructions"
				setup.store.rows[step.ID] = step
				request.ExecutionWork = step
			case "held step":
				step.Labels = append(step.Labels, beadmeta.DispatchHoldLabels[0])
				setup.store.rows[step.ID] = step
				request.ExecutionWork = step
			case "changed policy":
				workflow := "another-workflow"
				setup.fixture.cfg.Agents[0].DefaultSlingFormula = &workflow
			}
			anchors, patches := len(setup.store.q43Receipts), len(setup.store.patchRequests)
			if _, err := state.ClaimLifecycleWork(context.Background(), request); err == nil {
				t.Fatalf("accepted %s", scenario)
			}
			if len(setup.store.q43Receipts) != anchors || len(setup.store.patchRequests) != patches {
				t.Fatal("refused source claim wrote protected work")
			}
			after, err := setup.store.Get(source.ID)
			if err != nil || after.Revision != source.Revision || after.Status != source.Status || after.Assignee != source.Assignee {
				t.Fatal("refusal changed source")
			}
		})
	}
}

func TestLifecycleExecutionCompletionPreservesSourceAndRecovers(t *testing.T) {
	for _, lostKind := range []string{"", "lifecycle_descendant_completion_v1", "lifecycle_descendant_attempt_evidence_v1"} {
		t.Run(map[string]string{"": "normal", "lifecycle_descendant_completion_v1": "lost close response", "lifecycle_descendant_attempt_evidence_v1": "lost evidence response"}[lostKind], func(t *testing.T) {
			setup, state, request := newLifecycleCoreGraphClaimProviderFixture(t)
			claim, err := state.ClaimLifecycleWork(context.Background(), request)
			if err != nil {
				t.Fatal(err)
			}
			step, err := setup.store.Get(request.Work.ID)
			if err != nil {
				t.Fatal(err)
			}
			source, err := setup.store.Get(setup.source.ID)
			if err != nil {
				t.Fatal(err)
			}
			request.Work, request.ExpectedRevision, request.ExpectedTransitionHead = step, step.Revision, claim.ReceiptID
			finish := api.LifecycleExecutionCompletionTransitionRequest{Claim: request, Intent: api.LifecycleExecutionCompletionIntent{ClaimGeneration: claim.ClaimGeneration, Outcome: "pass"}}
			setup.store.loseNextPatchKind = lostKind
			before := len(setup.store.patchRequests)
			completed, err := state.CompleteLifecycleExecution(context.Background(), finish)
			if err != nil {
				t.Fatalf("complete current execution: %v", err)
			}
			current, err := setup.store.Get(step.ID)
			var reference attemptevidence.Reference
			if err != nil || json.Unmarshal([]byte(current.Metadata[beadmeta.AttemptEvidenceReferenceMetadataKey]), &reference) != nil {
				t.Fatal("completed step has no sealed attempt reference")
			}
			evidence, err := attemptevidence.Read(setup.store, step.ID, reference.AttemptID)
			if err != nil || evidence.Identity.SessionID != request.Session.ID || evidence.Identity.SessionGeneration != request.Session.Generation || evidence.Identity.ClaimGeneration != claim.ClaimGeneration || evidence.Outcome != "pass" || evidence.StoreRef != "rig:pilot" {
				t.Fatalf("step completion lost its exact attempt evidence: %v", err)
			}
			if current.Status != "closed" || current.Metadata[beadmeta.OutcomeMetadataKey] != "pass" || current.Revision != completed.Revision || completed.ReceiptID == claim.ReceiptID || completed.Replayed != (lostKind != "") || len(setup.store.patchRequests) != before+2 {
				t.Fatalf("completion result %+v, readback error %v", completed, err)
			}
			// Exact terminal replay must not need the original runtime or capture again.
			if err := setup.cityStore.Delete(request.Session.ID); err != nil {
				t.Fatal(err)
			}
			for _, fresh := range []bool{false, true} {
				if fresh {
					finish.Claim.Work = current
					finish.Claim.ExpectedRevision = current.Revision
					finish.Claim.ExpectedTransitionHead = completed.ReceiptID
				}
				replay, err := state.CompleteLifecycleExecution(context.Background(), finish)
				if err != nil || !replay.Replayed || replay.ReceiptID != completed.ReceiptID || replay.Revision != completed.Revision || len(setup.store.patchRequests) != before+2 {
					t.Fatalf("completion retry: %+v, %v", replay, err)
				}
			}
			finish.Intent.Outcome = "fail"
			if _, err := state.CompleteLifecycleExecution(context.Background(), finish); err == nil {
				t.Fatal("changed outcome replaced completed execution")
			}
			after, err := setup.store.Get(step.ID)
			if err != nil || !reflect.DeepEqual(current, after) || len(setup.store.patchRequests) != before+2 {
				t.Fatal("completion retry changed step")
			}
			afterSource, err := setup.store.Get(source.ID)
			if err != nil || !reflect.DeepEqual(source, afterSource) {
				t.Fatal("execution completion changed source")
			}
		})
	}
}

func TestLifecycleExecutionCompletionRepairsSealedAttemptBeforeClosing(t *testing.T) {
	for _, interruption := range []string{"archive", "close"} {
		for _, observation := range []string{"claim", "evidence"} {
			t.Run(interruption+"/"+observation, func(t *testing.T) {
				setup, state, request := newLifecycleCoreGraphClaimProviderFixture(t)
				claim, err := state.ClaimLifecycleWork(context.Background(), request)
				if err != nil {
					t.Fatal(err)
				}
				step, err := setup.store.Get(request.Work.ID)
				if err != nil {
					t.Fatal(err)
				}
				request.Work, request.ExpectedRevision, request.ExpectedTransitionHead = step, step.Revision, claim.ReceiptID
				finish := api.LifecycleExecutionCompletionTransitionRequest{Claim: request, Intent: api.LifecycleExecutionCompletionIntent{ClaimGeneration: claim.ClaimGeneration, Outcome: "pass"}}
				if interruption == "archive" {
					setup.store.failNextEvidenceArchive = true
				} else {
					setup.store.failNextPatchKind = "lifecycle_descendant_completion_v1"
				}
				if _, err := state.CompleteLifecycleExecution(context.Background(), finish); err == nil {
					t.Fatal("interrupted completion reported success")
				}
				pending, err := setup.store.Get(step.ID)
				var reference attemptevidence.Reference
				if err != nil || pending.Status != "in_progress" || pending.Metadata[beadmeta.OutcomeMetadataKey] != "" || json.Unmarshal([]byte(pending.Metadata[beadmeta.AttemptEvidenceReferenceMetadataKey]), &reference) != nil {
					t.Fatal("interruption closed the step or lost its sealed attempt")
				}
				archives, err := setup.store.ListByMetadata(map[string]string{beadmeta.AttemptEvidenceArchiveOwnerIDMetadataKey: step.ID}, 0, beads.IncludeClosed)
				if err != nil || (interruption == "archive" && len(archives) != 0) || (interruption == "close" && len(archives) != 1) {
					t.Fatal("fixture did not interrupt the requested phase")
				}
				if observation == "evidence" {
					finish.Claim.Work, finish.Claim.ExpectedRevision, finish.Claim.ExpectedTransitionHead = pending, pending.Revision, pending.Metadata[beadmeta.LifecycleTransitionHeadMetadataKey]
				}
				writes := len(setup.store.patchRequests)
				finish.Intent.Outcome = "fail"
				if _, err := state.CompleteLifecycleExecution(context.Background(), finish); err == nil {
					t.Fatal("changed outcome replaced sealed attempt")
				}
				afterRefusal, err := setup.store.Get(step.ID)
				if err != nil || !reflect.DeepEqual(pending, afterRefusal) || len(setup.store.patchRequests) != writes {
					t.Fatal("changed-outcome refusal mutated the step")
				}
				finish.Intent.Outcome = "pass"
				completed, err := state.CompleteLifecycleExecution(context.Background(), finish)
				if err != nil || !completed.Replayed {
					t.Fatalf("retry did not finish sealed attempt: %+v, %v", completed, err)
				}
				closed, err := setup.store.Get(step.ID)
				evidence, readErr := attemptevidence.Read(setup.store, step.ID, reference.AttemptID)
				archives, archiveErr := setup.store.ListByMetadata(map[string]string{beadmeta.AttemptEvidenceArchiveOwnerIDMetadataKey: step.ID}, 0, beads.IncludeClosed)
				if err != nil || readErr != nil || archiveErr != nil || closed.Status != "closed" || evidence.Outcome != "pass" || len(archives) != 1 || closed.Metadata[beadmeta.AttemptEvidenceReferenceMetadataKey] != pending.Metadata[beadmeta.AttemptEvidenceReferenceMetadataKey] || len(setup.store.patchRequests) != writes+1 {
					t.Fatal("retry failed to archive the exact sealed attempt before close")
				}
			})
		}
	}
}

func TestLifecycleExecutionCompletionRefusesChangedAuthorityBeforeWrites(t *testing.T) {
	for _, scenario := range []string{"missing anchor", "missing claim", "changed incarnation", "replaced persisted incarnation", "replaced persisted generation", "changed generation", "stale revision", "stale head", "changed instructions", "held step", "changed policy", "source as step", "operator outcome"} {
		t.Run(scenario, func(t *testing.T) {
			setup, state, request := newLifecycleCoreGraphClaimProviderFixture(t)
			claim, err := state.ClaimLifecycleWork(context.Background(), request)
			if err != nil {
				t.Fatal(err)
			}
			step, err := setup.store.Get(request.Work.ID)
			if err != nil {
				t.Fatal(err)
			}
			request.Work, request.ExpectedRevision, request.ExpectedTransitionHead = step, step.Revision, claim.ReceiptID
			finish := api.LifecycleExecutionCompletionTransitionRequest{Claim: request, Intent: api.LifecycleExecutionCompletionIntent{ClaimGeneration: claim.ClaimGeneration, Outcome: "pass"}}
			switch scenario {
			case "missing anchor":
				delete(setup.store.q43Receipts, setup.store.patchReceipts[claim.ReceiptID].PriorReceiptID)
			case "missing claim":
				delete(setup.store.patchReceipts, claim.ReceiptID)
			case "changed incarnation":
				finish.Claim.Session.InstanceToken = "other-runtime"
			case "replaced persisted incarnation", "replaced persisted generation":
				metadata := map[string]string{"instance_token": "replacement"}
				if scenario == "replaced persisted generation" {
					metadata = map[string]string{"generation": "2"}
				}
				if err := setup.cityStore.Update(request.Session.ID, beads.UpdateOpts{Metadata: metadata}); err != nil {
					t.Fatal(err)
				}
			case "changed generation":
				finish.Intent.ClaimGeneration = "2"
			case "stale revision":
				finish.Claim.ExpectedRevision--
			case "stale head":
				finish.Claim.ExpectedTransitionHead = "unrelated-head"
			case "changed instructions":
				step.Description += " altered"
				setup.store.rows[step.ID] = step
			case "held step":
				step.Labels = append(step.Labels, beadmeta.DispatchHoldLabels[0])
				setup.store.rows[step.ID] = step
			case "changed policy":
				workflow := "other-workflow"
				setup.fixture.cfg.Agents[0].DefaultSlingFormula = &workflow
			case "source as step":
				source, err := setup.store.Get(setup.source.ID)
				if err != nil {
					t.Fatal(err)
				}
				finish.Claim.Work = source
				finish.Claim.ExpectedRevision = source.Revision
				finish.Claim.ExpectedTransitionHead = source.Metadata[beadmeta.LifecycleTransitionHeadMetadataKey]
			case "operator outcome":
				finish.Intent.Outcome = "canceled"
			}
			source, err := setup.store.Get(setup.source.ID)
			if err != nil {
				t.Fatal(err)
			}
			beforeStep, err := setup.store.Get(step.ID)
			if err != nil {
				t.Fatal(err)
			}
			patches, anchors := len(setup.store.patchRequests), len(setup.store.q43Receipts)
			if _, err := state.CompleteLifecycleExecution(context.Background(), finish); err == nil {
				t.Fatalf("accepted %s", scenario)
			}
			afterStep, err := setup.store.Get(step.ID)
			if err != nil || !reflect.DeepEqual(beforeStep, afterStep) {
				t.Fatal("refused completion changed step")
			}
			afterSource, err := setup.store.Get(source.ID)
			if err != nil || !reflect.DeepEqual(source, afterSource) {
				t.Fatal("refused completion changed source")
			}
			if len(setup.store.patchRequests) != patches || len(setup.store.q43Receipts) != anchors {
				t.Fatal("refused completion wrote a protected transition")
			}
		})
	}
}

func TestLifecycleDescendantClaimAfterAcceptedSourceClose(t *testing.T) {
	for _, scenario := range []string{"valid closure", "historical acceptance", "unaccepted closure", "missing close receipt", "missing budget receipt", "missing acceptance", "altered acceptance", "untrusted authority", "changed source revision", "changed source head", "held source", "changed owner", "closed workflow", "changed policy"} {
		t.Run(scenario, func(t *testing.T) {
			setup, state, request := newLifecycleCoreGraphClaimProviderFixture(t)
			setup.fixture.cfg.Lifecycle.CompletionReceiptMaxAge = "24h"
			setup.fixture.cfg.Lifecycle.CompletionClockSkew = "2m"
			source, err := setup.store.Get(setup.source.ID)
			if err != nil {
				t.Fatal(err)
			}
			sourceRequest := request
			sourceRequest.Work = source
			sourceRequest.ExpectedRevision = source.Revision
			sourceRequest.ExpectedTransitionHead = source.Metadata[beadmeta.LifecycleTransitionHeadMetadataKey]
			if _, err := state.ClaimLifecycleWork(context.Background(), sourceRequest); err != nil {
				t.Fatal(err)
			}
			source, err = setup.store.Get(source.ID)
			if err != nil {
				t.Fatal(err)
			}
			digest, err := worklifecycle.AdmissionDigestV2(setup.receipt)
			if err != nil {
				t.Fatal(err)
			}
			signed, err := worklifecycle.SignCompletionReceipt(worklifecycle.CompletionReceipt{
				Version: 1, WorkItemID: source.ID, Scope: request.Scope, AdmissionDigest: digest,
				DeliverableRef: "commit:reviewed", VerificationRef: "checks:passed", AcceptedBy: "reviewer",
				AcceptedAt: time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano),
			}, setup.acceptancePrivate)
			if err != nil {
				t.Fatal(err)
			}
			beforeAcceptance := source
			completion, err := state.CompleteLifecycleWork(context.Background(), api.LifecycleCompletionTransitionRequest{
				Work: source, WorkStore: setup.store, WorkStoreRef: request.WorkStoreRef, Scope: request.Scope,
				Submission: api.LifecycleCompletionSubmitRequest{
					WorkID: source.ID, SourceStoreRef: "rig:pilot", ExpectedRevision: source.Revision,
					ExpectedTransitionHead: source.Metadata[beadmeta.LifecycleTransitionHeadMetadataKey], CompletionReceipt: signed,
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			source, err = setup.store.Get(source.ID)
			if err != nil || source.Status != "closed" {
				t.Fatalf("source close: %v", err)
			}
			source.Metadata = cloneLifecycleAdmissionMetadata(source.Metadata)
			switch scenario {
			case "historical acceptance":
				// Changing the new-intake window does not invalidate an already
				// committed close. Trust and the signed route policy still apply.
				setup.fixture.cfg.Lifecycle.CompletionReceiptMaxAge = "30m"
			case "unaccepted closure":
				source = beforeAcceptance
				source.Status = "closed"
			case "missing close receipt":
				delete(setup.store.patchReceipts, completion.ReceiptID)
			case "missing budget receipt":
				delete(setup.store.patchReceipts, setup.store.patchReceipts[completion.ReceiptID].PriorReceiptID)
			case "missing acceptance":
				delete(source.Metadata, beadmeta.LifecycleCompletionReceiptMetadataKey)
			case "altered acceptance":
				source.Metadata[beadmeta.LifecycleCompletionReceiptMetadataKey] = strings.Replace(signed, "commit:reviewed", "commit:unreviewed", 1)
			case "untrusted authority":
				setup.fixture.cfg.Lifecycle.AcceptanceAuthorities = nil
			case "changed source revision":
				source.Revision++
			case "changed source head":
				source.Metadata[beadmeta.LifecycleTransitionHeadMetadataKey] = "other-head"
			case "held source":
				source.Labels = append(source.Labels, beadmeta.DispatchHoldLabels[0])
			case "changed owner":
				source.Assignee = "another-worker"
			case "closed workflow":
				marker, _ := lifecycleMaterializationFor(source)
				root, err := setup.store.Get(marker.WorkflowID)
				if err != nil {
					t.Fatal(err)
				}
				root.Status = "closed"
				setup.store.rows[root.ID] = root
			case "changed policy":
				workflow := "another-workflow"
				setup.fixture.cfg.Agents[0].DefaultSlingFormula = &workflow
			}
			setup.store.rows[source.ID] = source
			step, err := setup.store.Get(request.Work.ID)
			if err != nil {
				t.Fatal(err)
			}
			patches, anchors := len(setup.store.patchRequests), len(setup.store.q43Receipts)
			claim, err := state.ClaimLifecycleWork(context.Background(), request)
			if scenario == "valid closure" || scenario == "historical acceptance" {
				if err != nil || claim.ReceiptID == "" {
					t.Fatalf("accepted-source descendant claim: %+v, %v", claim, err)
				}
				claimed, err := setup.store.Get(step.ID)
				if err != nil {
					t.Fatal(err)
				}
				request.Work, request.ExpectedRevision, request.ExpectedTransitionHead = claimed, claimed.Revision, claim.ReceiptID
				if _, err := state.CompleteLifecycleExecution(context.Background(), api.LifecycleExecutionCompletionTransitionRequest{
					Claim: request, Intent: api.LifecycleExecutionCompletionIntent{ClaimGeneration: claim.ClaimGeneration, Outcome: "pass"},
				}); err != nil {
					t.Fatalf("complete accepted-source descendant: %v", err)
				}
			} else {
				if err == nil {
					t.Fatalf("accepted %s", scenario)
				}
				after, err := setup.store.Get(step.ID)
				if err != nil || !reflect.DeepEqual(step, after) || len(setup.store.patchRequests) != patches || len(setup.store.q43Receipts) != anchors {
					t.Fatal("refused tail claim changed step or wrote protected evidence")
				}
			}
			after, err := setup.store.Get(source.ID)
			if err != nil || !reflect.DeepEqual(source, after) {
				t.Fatal("tail operation changed accepted source")
			}
		})
	}
}
