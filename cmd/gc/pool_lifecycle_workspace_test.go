package main

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/rollout/gate"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worktree"
)

// Seed ownership before admission is signed. This fixture never patches a
// protected source through generic writes to make its proof pass.
type poolWorkspaceSeedStore struct {
	*beads.MemStore
	metadata map[string]string
}

func (s *poolWorkspaceSeedStore) Create(row beads.Bead) (beads.Bead, error) {
	if row.ID == "pilot-rig-work-1" {
		for key, value := range s.metadata {
			row.Metadata[key] = value
		}
	}
	return s.MemStore.Create(row)
}

func newPoolLifecycleWorkspaceFixture(t *testing.T, change func(map[string]string)) (lifecycleAdmissionTransitionSetup, *agentBuildParams, SessionRequest, worktree.Spec) {
	t.Helper()
	repo, base := worktreeTestRepo(t)
	root := t.TempDir()
	spec := worktree.Spec{
		RepoDir: repo, Root: root, Path: filepath.Join(root, "source"),
		Branch: "work/source", Base: base, BeadID: "pilot-rig-work-1", StoreRef: "rig:pilot",
		Creator: "gc-worktree", Owner: "pilot/worker", Generation: "1", Lifecycle: worktree.LifecycleActive,
	}
	report, err := worktree.Ensure(spec)
	if err != nil {
		t.Fatal(err)
	}
	spec.BaseSHA, spec.AttemptID = report.Provenance.BaseSHA, report.Provenance.AttemptID
	metadata := map[string]string{
		beadmeta.WorkDirMetadataKey: spec.Path, beadmeta.LegacyWorkDirMetadataKey: spec.Path,
		beadmeta.WorktreeSourceBeadMetadataKey: spec.BeadID, beadmeta.WorktreeSourceStoreMetadataKey: spec.StoreRef,
		beadmeta.WorkBranchMetadataKey: spec.Branch, beadmeta.WorktreeRepoMetadataKey: spec.RepoDir,
		beadmeta.WorktreeRootMetadataKey: spec.Root, beadmeta.WorktreeBaseRefMetadataKey: spec.Base,
		beadmeta.WorktreeBaseSHAMetadataKey: spec.BaseSHA, beadmeta.WorktreeCreatorMetadataKey: spec.Creator,
		beadmeta.WorktreeOwnerMetadataKey: spec.Owner, beadmeta.WorktreeGenerationMetadataKey: spec.Generation,
		beadmeta.WorktreeLifecycleMetadataKey: spec.Lifecycle, "gc.worktree_attempt_id": spec.AttemptID,
	}
	if change != nil {
		change(metadata)
	}
	fixture := newLifecycleAdmissionPolicyFixture(t, "formula = \"review\"\nversion = 1\n\n[[steps]]\nid = \"work\"\ntitle = \"Review work\"\n")
	fixture.cfg.Rigs[0].Path = repo
	backing := &poolWorkspaceSeedStore{MemStore: &beads.MemStore{IDPrefix: "pilot-rig", HonorExplicitIDs: true}, metadata: metadata}
	setup := newLifecycleAdmissionTransitionSetupWithBacking(t, fixture, backing, false)
	var stderr strings.Builder
	reconcileLifecycleAdmissionWithPermitResolver("pilot", fixture.cityPath, fixture.cfg, setup.cityStore, setup.rigStores, nil, &stderr, setup.permitResolver)
	source, err := setup.store.Get(setup.source.ID)
	if err != nil {
		t.Fatal(err)
	}
	marker, ok := lifecycleMaterializationFor(source)
	if !ok || marker.State != "attached" {
		t.Fatalf("source not attached: %+v: %s", marker, stderr.String())
	}
	step := lifecycleAdmissionV1StepCandidate(t, setup, marker)
	opened, err := beads.OpenStoreAtForCity(context.Background(), beads.StoreOpenOptions{
		ScopeRoot: t.TempDir(), Provider: "file", ConditionalWrites: gate.Require,
		OpenFileStore: func() (beads.Store, error) { return setup.cityStore, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	setup.cityStore = opened.Store
	bp := &agentBuildParams{
		city: fixture.cfg, cityName: "pilot", cityPath: fixture.cityPath,
		beadStore: setup.cityStore, sessionCensusRigStores: setup.rigStores,
	}
	return setup, bp, SessionRequest{WorkBeadID: step.ID, WorkStoreRef: "rig:pilot"}, spec
}

func TestPoolLifecycleWorkspaceBindsSourceProvenanceToExecutionStep(t *testing.T) {
	setup, bp, request, spec := newPoolLifecycleWorkspaceFixture(t, nil)
	// The demand collector may have copied the source tuple onto the step.
	// That must neither rename durable provenance nor become its authority.
	stepSpec := spec
	stepSpec.BeadID = request.WorkBeadID
	request.WorktreeSpec = &stepSpec
	row, err := setup.cityStore.Create(beads.Bead{Type: "session"})
	if err != nil {
		t.Fatal(err)
	}
	info, err := sessionFrontDoor(setup.cityStore).Get(row.ID)
	if err != nil {
		t.Fatal(err)
	}
	bound, err := bindPoolSessionTriggerBead(bp, &setup.fixture.cfg.Agents[0], "pilot/worker", info, request)
	if err != nil {
		t.Fatal(err)
	}
	persisted, err := setup.cityStore.Get(row.ID)
	if err != nil {
		t.Fatal(err)
	}
	if bound.WorkDir != spec.Path || bound.WorkDirCanonical != spec.Path ||
		persisted.Metadata[beadmeta.TriggerBeadIDMetadataKey] != request.WorkBeadID ||
		persisted.Metadata["gc.worktree_source_bead_id"] != spec.BeadID ||
		persisted.Metadata["gc.worktree_source_store_ref"] != spec.StoreRef ||
		persisted.Metadata["gc.worktree_attempt_id"] != spec.AttemptID {
		t.Fatalf("source workspace / execution binding not published together: %+v", persisted.Metadata)
	}
	if setup.store.sourceGenericWrites != 0 {
		t.Fatal("binding performed a generic protected source write")
	}
}

func TestPoolLifecycleWorkspacePreservesExistingOwnerAndNativeResume(t *testing.T) {
	for _, test := range []struct {
		name     string
		metadata map[string]string
	}{
		{"different workspace", map[string]string{beadmeta.WorkDirMetadataKey: "/original/worktree", beadmeta.LegacyWorkDirMetadataKey: "/original/worktree", "session_key": "original-native-key"}},
		{"different source", map[string]string{"gc.worktree_source_bead_id": "original-source", "session_key": "original-native-key"}},
		{"different attempt", map[string]string{"gc.worktree_attempt_id": "original-attempt", "session_key": "original-native-key"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			setup, bp, request, _ := newPoolLifecycleWorkspaceFixture(t, nil)
			row, err := setup.cityStore.Create(beads.Bead{Type: "session", Metadata: test.metadata})
			if err != nil {
				t.Fatal(err)
			}
			info, err := sessionFrontDoor(setup.cityStore).Get(row.ID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := bindPoolSessionTriggerBead(bp, &setup.fixture.cfg.Agents[0], "pilot/worker", info, request); err == nil {
				t.Fatal("ordinary binding accepted an implicit owner transfer")
			}
			after, err := setup.cityStore.Get(row.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(after.Metadata, row.Metadata) {
				t.Fatalf("refused binding changed existing identity: before=%+v after=%+v", row.Metadata, after.Metadata)
			}
		})
	}
}

func TestPoolLifecycleWorkspaceRefusesForeignStoreAndBrokenLineage(t *testing.T) {
	setup, bp, request, _ := newPoolLifecycleWorkspaceFixture(t, nil)
	wrongStore := request
	wrongStore.WorkStoreRef = "rig:other"
	if _, err := verifiedPoolTriggerWorkDir(bp, &setup.fixture.cfg.Agents[0], "pilot/worker", wrongStore); err == nil {
		t.Fatal("accepted foreign store")
	}
	step, err := setup.store.Get(request.WorkBeadID)
	if err != nil {
		t.Fatal(err)
	}
	step.Metadata = cloneLifecycleAdmissionMetadata(step.Metadata)
	step.Metadata[beadmeta.LifecycleMaterializationMetadataKey] = "malformed"
	setup.store.mu.Lock()
	setup.store.workflowReadOverrides[step.ID] = step
	setup.store.mu.Unlock()
	if _, err := verifiedPoolTriggerWorkDir(bp, &setup.fixture.cfg.Agents[0], "pilot/worker", request); err == nil {
		t.Fatal("accepted unverified descendant lineage")
	}
}

func TestPoolLifecycleWorkspaceRefusesMissingOrStaleOwnership(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(map[string]string)
	}{
		{"missing path", func(m map[string]string) {
			delete(m, beadmeta.WorkDirMetadataKey)
			delete(m, beadmeta.LegacyWorkDirMetadataKey)
		}},
		{"missing source", func(m map[string]string) { delete(m, beadmeta.WorktreeSourceBeadMetadataKey) }},
		{"different source", func(m map[string]string) { m[beadmeta.WorktreeSourceBeadMetadataKey] = "other-source" }},
		{"different source store", func(m map[string]string) { m[beadmeta.WorktreeSourceStoreMetadataKey] = "rig:other" }},
		{"missing owner", func(m map[string]string) { delete(m, beadmeta.WorktreeOwnerMetadataKey) }},
		{"missing attempt", func(m map[string]string) { delete(m, "gc.worktree_attempt_id") }},
		{"stale attempt", func(m map[string]string) { m["gc.worktree_attempt_id"] = "earlier-attempt" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			setup, bp, request, _ := newPoolLifecycleWorkspaceFixture(t, test.change)
			if _, err := verifiedPoolTriggerWorkDir(bp, &setup.fixture.cfg.Agents[0], "pilot/worker", request); err == nil {
				t.Fatal("protected descendant fell back to unverified workspace")
			}
		})
	}
}

func TestPoolLifecycleWorkspaceRefusesExplicitForeignRootStore(t *testing.T) {
	setup, bp, request, _ := newPoolLifecycleWorkspaceFixture(t, nil)
	step, err := setup.store.Get(request.WorkBeadID)
	if err != nil {
		t.Fatal(err)
	}
	if step.Metadata[beadmeta.RootStoreRefMetadataKey] != "" {
		t.Fatal("fixture must exercise ordinary descendant without optional root-store metadata")
	}
	step.Metadata = cloneLifecycleAdmissionMetadata(step.Metadata)
	step.Metadata[beadmeta.RootStoreRefMetadataKey] = "rig:other"
	setup.store.mu.Lock()
	setup.store.workflowReadOverrides[step.ID] = step
	setup.store.mu.Unlock()
	if _, err := verifiedPoolTriggerWorkDir(bp, &setup.fixture.cfg.Agents[0], "pilot/worker", request); err == nil {
		t.Fatal("accepted explicitly conflicting root store")
	}
}

// Exercise the ordinary sequence with a typed publication after admission,
// rather than relying only on the pre-admission custody seed fixtures.
func TestPoolLifecycleWorkspaceBindsPublishedSourceThenClaimsDescendant(t *testing.T) {
	setup, state, publication := newLifecycleWorkspaceProviderFixture(t, "bug")
	if _, err := state.PublishLifecycleWorkspace(context.Background(), publication); err != nil {
		t.Fatal(err)
	}
	source, err := setup.store.Get(publication.Work.ID)
	if err != nil {
		t.Fatal(err)
	}
	marker, ok := lifecycleMaterializationFor(source)
	if !ok {
		t.Fatal("published source lost attached workflow")
	}
	candidate := lifecycleAdmissionV1StepCandidate(t, setup, marker)
	step, err := beads.HandlesFor(setup.store).Live.Get(candidate.ID)
	if err != nil || step.Revision == 0 {
		t.Fatalf("authoritative descendant snapshot: revision=%d error=%v", step.Revision, err)
	}
	opened, err := beads.OpenStoreAtForCity(context.Background(), beads.StoreOpenOptions{
		ScopeRoot: t.TempDir(), Provider: "file", ConditionalWrites: gate.Require,
		OpenFileStore: func() (beads.Store, error) { return setup.cityStore, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	info, err := sessionFrontDoor(opened.Store).CreateSessionInfo(session.CreateSpec{
		Title: "pool workspace worker", AgentName: "pilot/worker", Metadata: map[string]string{
			"template": "pilot/worker", "agent_name": "pilot/worker", "generation": "1",
			"instance_token": "published-pool-claim", "session_key": "original-pool-thread",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	bp := &agentBuildParams{
		city: setup.fixture.cfg, cityName: "pilot", cityPath: setup.fixture.cityPath,
		beadStore: opened.Store, sessionCensusRigStores: setup.rigStores,
	}
	bound, err := bindPoolSessionTriggerBead(bp, &setup.fixture.cfg.Agents[0], "pilot/worker", info,
		SessionRequest{WorkBeadID: step.ID, WorkStoreRef: "rig:pilot", SessionBeadID: info.ID})
	if err != nil {
		t.Fatal(err)
	}
	if bound.WorkDir != publication.Submission.Workspace.Path || bound.SessionKey != info.SessionKey {
		t.Fatal("binding lost original source workspace or native conversation")
	}
	request := api.LifecycleClaimTransitionRequest{
		Work: step, WorkStore: setup.store, WorkStoreRef: publication.WorkStoreRef,
		Scope: publication.Scope, Session: bound, Actor: session.AssigneeIdentifier(bound),
		ExpectedRevision: step.Revision, ExpectedTransitionHead: step.Metadata[beadmeta.LifecycleTransitionHeadMetadataKey],
	}
	first, err := state.ClaimLifecycleWork(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := state.ClaimLifecycleWork(context.Background(), request)
	if err != nil || !replayed.Replayed || replayed.ReceiptID != first.ReceiptID {
		t.Fatalf("exact bound descendant replay: %+v %v", replayed, err)
	}
	after, err := setup.store.Get(source.ID)
	if err != nil || after.Revision != source.Revision || !reflect.DeepEqual(after.Metadata, source.Metadata) {
		t.Fatal("descendant claim rewrote source publication")
	}
	if setup.store.sourceGenericWrites != 0 {
		t.Fatal("ordinary publication/bind/claim performed generic source writes")
	}
}

type poolWorkspaceSessionListProjection struct {
	*beads.MemStore
}

func (s *poolWorkspaceSessionListProjection) List(query beads.ListQuery) ([]beads.Bead, error) {
	rows, err := s.MemStore.List(query)
	for i := range rows {
		rows[i].Revision = 0
	}
	return rows, err
}

func TestPoolLifecycleWorkspaceBindingUsesAuthoritativeCachedSession(t *testing.T) {
	for _, changedIdentity := range []bool{false, true} {
		name := "current identity"
		if changedIdentity {
			name = "changed incarnation behind cache"
		}
		t.Run(name, func(t *testing.T) {
			setup, bp, request, spec := newPoolLifecycleWorkspaceFixture(t, nil)
			memory, ok := setup.cityStore.(*beads.MemStore)
			if !ok {
				t.Fatalf("fixture session store type %T", setup.cityStore)
			}
			backing := &poolWorkspaceSessionListProjection{MemStore: memory}
			row, err := backing.Create(beads.Bead{Type: "session", Metadata: map[string]string{
				"template": "pilot/worker", "instance_token": "original-incarnation",
				"generation": "1", "session_key": "original-conversation",
			}})
			if err != nil {
				t.Fatal(err)
			}
			info, err := sessionFrontDoor(backing).Get(row.ID)
			if err != nil {
				t.Fatal(err)
			}
			cache := beads.NewCachingStoreForTest(backing, nil)
			if err := cache.Prime(context.Background()); err != nil {
				t.Fatal(err)
			}
			bp.beadStore = cache
			cached, err := cache.Get(row.ID)
			if err != nil || cached.Revision != 0 {
				t.Fatalf("fixture must expose list-backed revision zero: revision=%d error=%v", cached.Revision, err)
			}
			if changedIdentity {
				if err := backing.SetMetadata(row.ID, "instance_token", "replacement-incarnation"); err != nil {
					t.Fatal(err)
				}
			}
			before, err := backing.Get(row.ID)
			if err != nil || before.Revision == 0 {
				t.Fatalf("authoritative fixture revision unavailable: %v", err)
			}
			bound, bindErr := bindPoolSessionTriggerBead(bp, &setup.fixture.cfg.Agents[0], "pilot/worker", info, request)
			after, err := backing.Get(row.ID)
			if err != nil {
				t.Fatal(err)
			}
			if changedIdentity {
				if bindErr == nil || !reflect.DeepEqual(bound, info) || !reflect.DeepEqual(before, after) {
					t.Fatalf("stale cached incarnation was not refused without mutation: %v", bindErr)
				}
				return
			}
			if bindErr != nil || bound.WorkDir != spec.Path || after.Revision == before.Revision {
				t.Fatalf("authoritative cached-session binding failed: %v", bindErr)
			}
			refreshed, err := cache.Get(row.ID)
			if err != nil || !reflect.DeepEqual(refreshed, after) {
				t.Fatalf("binding bypassed original cache writer: %v", err)
			}
			if after.Metadata[beadmeta.TriggerBeadIDMetadataKey] != request.WorkBeadID ||
				after.Metadata[beadmeta.WorktreeSourceBeadMetadataKey] != spec.BeadID ||
				after.Metadata["instance_token"] != "original-incarnation" ||
				after.Metadata["session_key"] != "original-conversation" {
				t.Fatal("binding lost original identity or source custody")
			}
		})
	}
}
