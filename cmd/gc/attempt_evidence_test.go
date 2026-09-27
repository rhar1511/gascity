package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/attemptevidence"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

func TestCLICloseGateCapturesWorkbenchAttemptBeforeClose(t *testing.T) {
	repo, baseSHA := newCLIAttemptEvidenceRepo(t)
	store, err := beads.OpenFileStore(fsys.OSFS{}, filepath.Join(t.TempDir(), "beads.json"))
	if err != nil {
		t.Fatal(err)
	}
	sessions := session.NewStore(beads.SessionStore{Store: store})
	sessionInfo, err := sessions.CreateSessionInfo(session.CreateSpec{
		Title: "worker", AgentName: "worker", Metadata: map[string]string{"generation": "3"},
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	owner, err := store.Create(beads.Bead{
		Title: "executed work", Type: "task",
		Metadata: map[string]string{
			beadmeta.SessionIDMetadataKey:       sessionInfo.ID,
			beadmeta.ClaimGenerationMetadataKey: "8",
			beadmeta.WorkDirMetadataKey:         repo,
			beadmeta.WorktreeBaseSHAMetadataKey: baseSHA,
			beadmeta.WorkOutcomeMetadataKey:     beadmeta.OutcomeFail,
		},
	})
	if err != nil {
		t.Fatalf("create work bead: %v", err)
	}
	var stderr strings.Builder
	gateExitCode := runWorkRecordCloseGate([]string{"close", owner.ID}, repo, repo, nil, store,
		map[string]beads.Bead{owner.ID: owner}, &stderr)
	if gateExitCode != 0 {
		t.Fatalf("close was blocked after capture succeeded: %s", stderr.String())
	}
	attemptID, err := attemptevidence.AttemptID(attemptevidence.Identity{
		Kind: attemptevidence.KindWorkbench, OwnerBeadID: owner.ID, ExecutionBeadID: owner.ID,
		SessionID: sessionInfo.ID, SessionGeneration: "3", ClaimGeneration: "8",
	})
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := attemptevidence.Read(store, owner.ID, attemptID)
	if err != nil {
		t.Fatalf("read attempt sealed before close: %v", err)
	}
	if evidence.Identity.Kind != attemptevidence.KindWorkbench || evidence.Identity.SessionGeneration != "3" || evidence.Permission.WorkspaceRoot != repo {
		t.Fatalf("captured workbench identity/scope = %+v %+v", evidence.Identity, evidence.Permission)
	}
}

func TestCLICloseGateRefusesWorkbenchRetirementWhenPayloadTransportIsUnsupported(t *testing.T) {
	repo, baseSHA := newCLIAttemptEvidenceRepo(t)
	store := beads.NewMemStore()
	sessionInfo, err := session.NewStore(beads.SessionStore{Store: store}).CreateSessionInfo(session.CreateSpec{
		Title: "worker", AgentName: "worker", Metadata: map[string]string{"generation": "2"},
	})
	if err != nil {
		t.Fatal(err)
	}
	owner, err := store.Create(beads.Bead{
		Title: "executed work", Type: "task",
		Metadata: map[string]string{
			beadmeta.SessionIDMetadataKey:       sessionInfo.ID,
			beadmeta.ClaimGenerationMetadataKey: "1",
			beadmeta.WorkDirMetadataKey:         repo,
			beadmeta.WorktreeBaseSHAMetadataKey: baseSHA,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var stderr strings.Builder
	if gateExitCode := runWorkRecordCloseGate([]string{"close", owner.ID}, repo, repo, &config.City{}, store,
		map[string]beads.Bead{owner.ID: owner}, &stderr); gateExitCode == 0 {
		t.Fatalf("close was allowed without durable private payload transport; stderr=%s", stderr.String())
	}
	if !strings.Contains(stderr.String(), attemptevidence.ErrPrivatePayloadTransportUnsupported.Error()) {
		t.Fatalf("close refusal did not name unsupported transport: %s", stderr.String())
	}
}

type failingAttemptEvidenceReadStore struct {
	beads.Store
	readErr error
}

func (s failingAttemptEvidenceReadStore) Get(string) (beads.Bead, error) {
	return beads.Bead{}, s.readErr
}

func TestCLICloseGateRefusesWhenSourceRowReadFails(t *testing.T) {
	store := failingAttemptEvidenceReadStore{Store: beads.NewMemStore(), readErr: errors.New("source read unavailable")}
	var stderr strings.Builder
	gateExitCode := runWorkRecordCloseGate([]string{"close", "work-1"}, t.TempDir(), "", nil, store, nil, &stderr)
	if gateExitCode == 0 {
		t.Fatal("close was allowed without reading the source row")
	}
	if !strings.Contains(stderr.String(), "source read unavailable") {
		t.Fatalf("capture refusal did not name the read failure: %s", stderr.String())
	}
}

func TestCLIClassCloseGateCapturesWorkbenchAttemptBeforeClose(t *testing.T) {
	repo, baseSHA := newCLIAttemptEvidenceRepo(t)
	store, err := beads.OpenFileStore(fsys.OSFS{}, filepath.Join(t.TempDir(), "beads.json"))
	if err != nil {
		t.Fatal(err)
	}
	sessionInfo, err := session.NewStore(beads.SessionStore{Store: store}).CreateSessionInfo(session.CreateSpec{
		Title: "worker", AgentName: "worker", Metadata: map[string]string{"generation": "4"},
	})
	if err != nil {
		t.Fatal(err)
	}
	owner, err := store.Create(beads.Bead{
		Title: "class-owned execution", Type: "task",
		Metadata: map[string]string{
			beadmeta.RootStoreRefMetadataKey:    "rig:pilot",
			beadmeta.SessionIDMetadataKey:       sessionInfo.ID,
			beadmeta.ClaimGenerationMetadataKey: "12",
			beadmeta.WorkDirMetadataKey:         repo,
			beadmeta.WorktreeBaseSHAMetadataKey: baseSHA,
			beadmeta.WorkOutcomeMetadataKey:     beadmeta.WorkOutcomeNoOp,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	dirs := workRecordRepoDirs{cityPath: repo, legacy: repo}
	door := bdByIDClassDoor{Store: store, CityPath: repo}
	op := bdByIDOp{Verb: bdByIDClose, ID: owner.ID}
	if gateBdByIDClassClose(door, op, []string{"close", owner.ID}, bdByIDResolution{Bead: owner, Found: true}, dirs, &strings.Builder{}) != 0 {
		t.Fatal("class-owned close was blocked after evidence capture")
	}
	attemptID, err := attemptevidence.AttemptID(attemptevidence.Identity{
		Kind: attemptevidence.KindWorkbench, OwnerBeadID: owner.ID, ExecutionBeadID: owner.ID,
		SessionID: sessionInfo.ID, SessionGeneration: "4", ClaimGeneration: "12",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := attemptevidence.Read(store, owner.ID, attemptID); err != nil {
		t.Fatalf("class close did not seal attempt before write: %v", err)
	}
}

func TestCLIDeleteGateArchivesWorkbenchAttemptBeforeOwnerRemoval(t *testing.T) {
	repo, baseSHA := newCLIAttemptEvidenceRepo(t)
	store, err := beads.OpenFileStore(fsys.OSFS{}, filepath.Join(t.TempDir(), "beads.json"))
	if err != nil {
		t.Fatal(err)
	}
	sessionInfo, err := session.NewStore(beads.SessionStore{Store: store}).CreateSessionInfo(session.CreateSpec{
		Title: "worker", AgentName: "worker", Metadata: map[string]string{"generation": "7"},
	})
	if err != nil {
		t.Fatal(err)
	}
	owner, err := store.Create(beads.Bead{
		Title: "deleted execution", Type: "task",
		Metadata: map[string]string{
			beadmeta.SessionIDMetadataKey:       sessionInfo.ID,
			beadmeta.ClaimGenerationMetadataKey: "13",
			beadmeta.WorkDirMetadataKey:         repo,
			beadmeta.WorktreeBaseSHAMetadataKey: baseSHA,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var stderr strings.Builder
	if runWorkRecordCloseGate([]string{"delete", owner.ID}, repo, repo, nil, store,
		map[string]beads.Bead{owner.ID: owner}, &stderr) != 0 {
		t.Fatalf("delete was blocked after evidence capture: %s", stderr.String())
	}
	identity := attemptevidence.Identity{
		Kind: attemptevidence.KindWorkbench, OwnerBeadID: owner.ID, ExecutionBeadID: owner.ID,
		SessionID: sessionInfo.ID, SessionGeneration: "7", ClaimGeneration: "13",
	}
	attemptID, err := attemptevidence.AttemptID(identity)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(owner.ID); err != nil {
		t.Fatalf("remove owner after pre-delete capture: %v", err)
	}
	if _, err := attemptevidence.Read(store, owner.ID, attemptID); err != nil {
		t.Fatalf("archive did not survive owner removal: %v", err)
	}
}

func TestSessionCloseCapturesAssignedWorkbenchAttemptBeforeRelease(t *testing.T) {
	cityDir := t.TempDir()
	writePhase0InterfaceCity(t, cityDir, `[workspace]
name = "test-city"

[beads]
provider = "file"

[[agent]]
name = "worker"
start_command = "true"
max_active_sessions = 1
`)
	repo, baseSHA := newCLIAttemptEvidenceRepo(t)
	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_DIR", t.TempDir())
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_SESSION", "fake")
	store, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	sessionInfo, err := session.NewStore(beads.SessionStore{Store: store}).CreateSessionInfo(session.CreateSpec{
		Title: "worker", AgentName: "worker", Metadata: map[string]string{"generation": "3", "template": "worker"},
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	work, err := store.Create(beads.Bead{
		Title: "active workbench execution", Type: "task", Assignee: sessionInfo.ID,
		Metadata: map[string]string{
			beadmeta.RootStoreRefMetadataKey:    "city:test-city",
			beadmeta.SessionIDMetadataKey:       sessionInfo.ID,
			beadmeta.ClaimGenerationMetadataKey: "9",
			beadmeta.WorkDirMetadataKey:         repo,
			beadmeta.WorktreeBaseSHAMetadataKey: baseSHA,
		},
	})
	if err != nil {
		t.Fatalf("create execution: %v", err)
	}
	inProgress := "in_progress"
	if err := store.Update(work.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
		t.Fatalf("mark execution in progress: %v", err)
	}

	var stdout, stderr bytes.Buffer
	if code := cmdSessionClose([]string{sessionInfo.ID}, &stdout, &stderr); code != 0 {
		t.Fatalf("cmdSessionClose = %d, want 0; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}

	reopened, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("reopen city store: %v", err)
	}
	retired, err := reopened.Get(work.ID)
	if err != nil {
		t.Fatal(err)
	}
	if retired.Assignee != "" || retired.Status != "open" {
		t.Fatalf("released execution state = assignee %q status %q, want unassigned/open", retired.Assignee, retired.Status)
	}
	identity := attemptevidence.Identity{
		Kind: attemptevidence.KindWorkbench, OwnerBeadID: work.ID, ExecutionBeadID: work.ID,
		SessionID: sessionInfo.ID, SessionGeneration: sessionInfo.Generation, ClaimGeneration: "9",
	}
	attemptID, err := attemptevidence.AttemptID(identity)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := attemptevidence.Read(reopened, work.ID, attemptID); err != nil {
		t.Fatalf("attempt evidence did not survive session close and work release: %v", err)
	}
}

func TestSessionRetirementKeepsAssignedExecutionWhenArchiveTransportFails(t *testing.T) {
	repo, baseSHA := newCLIAttemptEvidenceRepo(t)
	store := beads.NewMemStore()
	sessionInfo, err := session.NewStore(beads.SessionStore{Store: store}).CreateSessionInfo(session.CreateSpec{
		Title: "worker", AgentName: "worker", Metadata: map[string]string{"generation": "5"},
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	sessionBead, err := store.Get(sessionInfo.ID)
	if err != nil {
		t.Fatalf("read session bead: %v", err)
	}
	work, err := store.Create(beads.Bead{
		Title: "execution that must remain assigned", Type: "task", Assignee: sessionInfo.ID,
		Metadata: map[string]string{
			beadmeta.RootStoreRefMetadataKey:    "city:test-city",
			beadmeta.SessionIDMetadataKey:       sessionInfo.ID,
			beadmeta.ClaimGenerationMetadataKey: "11",
			beadmeta.WorkDirMetadataKey:         repo,
			beadmeta.WorktreeBaseSHAMetadataKey: baseSHA,
		},
	})
	if err != nil {
		t.Fatalf("create execution: %v", err)
	}
	inProgress := "in_progress"
	if err := store.Update(work.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
		t.Fatalf("mark execution in progress: %v", err)
	}

	var stderr strings.Builder
	unclaimWorkAssignedToRetiredSessionBead("", nil, store, nil, sessionBead, "", &stderr)
	got, err := store.Get(work.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Assignee != sessionInfo.ID || got.Status != "in_progress" {
		t.Fatalf("failed capture released source execution: assignee=%q status=%q", got.Assignee, got.Status)
	}
	if !strings.Contains(stderr.String(), attemptevidence.ErrPrivatePayloadTransportUnsupported.Error()) {
		t.Fatalf("missing archive transport diagnostic: %s", stderr.String())
	}
}

func TestOrphanReleaseKeepsWorkbenchExecutionWhenArchiveTransportFails(t *testing.T) {
	repo, baseSHA := newCLIAttemptEvidenceRepo(t)
	store := beads.NewMemStore()
	sessionInfo, err := session.NewStore(beads.SessionStore{Store: store}).CreateSessionInfo(session.CreateSpec{
		Title: "worker", AgentName: "worker", Metadata: map[string]string{"generation": "6"},
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if err := store.Close(sessionInfo.ID); err != nil {
		t.Fatalf("retire session: %v", err)
	}
	work, err := store.Create(beads.Bead{
		Title: "orphaned execution", Type: "task", Assignee: sessionInfo.ID,
		Metadata: map[string]string{
			"gc.routed_to":                      "worker",
			beadmeta.RootStoreRefMetadataKey:    "city:test-city",
			beadmeta.SessionIDMetadataKey:       sessionInfo.ID,
			beadmeta.ClaimGenerationMetadataKey: "17",
			beadmeta.WorkDirMetadataKey:         repo,
			beadmeta.WorktreeBaseSHAMetadataKey: baseSHA,
		},
	})
	if err != nil {
		t.Fatalf("create execution: %v", err)
	}
	inProgress := "in_progress"
	if err := store.Update(work.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
		t.Fatalf("mark execution in progress: %v", err)
	}
	work, err = store.Get(work.ID)
	if err != nil {
		t.Fatal(err)
	}

	released := releaseOrphanedPoolAssignments(
		store, beads.SessionStore{Store: store}, testPoolReleaseConfig(), t.TempDir(), nil,
		[]beads.Bead{work}, []beads.Store{store}, []string{""}, nil, nil, nil,
	)
	if len(released) != 0 {
		t.Fatalf("released = %#v, want no release while archive transport is unavailable", released)
	}
	got, err := store.Get(work.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Assignee != sessionInfo.ID || got.Status != "in_progress" {
		t.Fatalf("failed capture released source execution: assignee=%q status=%q", got.Assignee, got.Status)
	}
}

func newCLIAttemptEvidenceRepo(t *testing.T) (string, string) {
	t.Helper()
	repo := t.TempDir()
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, output)
		}
		return strings.TrimSpace(string(output))
	}
	run("init", "-q")
	run("config", "user.email", "attempt-test@example.invalid")
	run("config", "user.name", "Attempt Test")
	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "tracked.txt")
	run("commit", "-q", "-m", "base")
	return repo, run("rev-parse", "HEAD")
}

type sessionReapEvidenceFixture struct {
	cityPath string
	cfg      *config.City
	store    beads.Store
	session  beads.Bead
	work     beads.Bead
	repo     string
	baseSHA  string
}

type evidenceInspectingStopRuntime struct {
	*runtime.Fake
	stopCalls int
	onStop    func(string) error
}

func (p *evidenceInspectingStopRuntime) Stop(name string) error {
	p.stopCalls++
	if p.onStop != nil {
		if err := p.onStop(name); err != nil {
			return err
		}
	}
	return p.Fake.Stop(name)
}

func newSessionReapEvidenceFixture(t *testing.T, durable bool, state string) sessionReapEvidenceFixture {
	t.Helper()
	repo, baseSHA := newCLIAttemptEvidenceRepo(t)
	var store beads.Store
	if durable {
		opened, err := beads.OpenFileStore(fsys.OSFS{}, filepath.Join(t.TempDir(), "beads.json"))
		if err != nil {
			t.Fatalf("open file store: %v", err)
		}
		store = opened
	} else {
		store = beads.NewMemStore()
	}
	sessionBead, err := store.Create(beads.Bead{
		Title: "worker", Type: sessionBeadType, Status: "open", Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name": "worker-1",
			"transport":    config.SessionTransportTmux,
			"state":        state,
			"generation":   "3",
		},
	})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}
	work, err := store.Create(beads.Bead{
		Title: "executed work", Type: "task", Status: "in_progress", Assignee: sessionBead.ID,
		Metadata: map[string]string{
			beadmeta.RootStoreRefMetadataKey:    "city:test-city",
			beadmeta.SessionIDMetadataKey:       sessionBead.ID,
			beadmeta.ClaimGenerationMetadataKey: "8",
			beadmeta.WorkDirMetadataKey:         repo,
			beadmeta.WorktreeBaseSHAMetadataKey: baseSHA,
			beadmeta.WorkOutcomeMetadataKey:     beadmeta.OutcomeFail,
		},
	})
	if err != nil {
		t.Fatalf("create work bead: %v", err)
	}
	inProgress := "in_progress"
	if err := store.Update(work.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
		t.Fatalf("mark work in progress: %v", err)
	}
	work.Status = inProgress
	return sessionReapEvidenceFixture{
		cityPath: t.TempDir(), cfg: &config.City{}, store: store,
		session: sessionBead, work: work, repo: repo, baseSHA: baseSHA,
	}
}

func TestDeadRuntimeCleanupArchivesExecutionInItsRigStoreOnly(t *testing.T) {
	cityPath := t.TempDir()
	repo, baseSHA := newCLIAttemptEvidenceRepo(t)
	cityStore, err := beads.OpenFileStore(fsys.OSFS{}, filepath.Join(t.TempDir(), "city-beads.json"))
	if err != nil {
		t.Fatalf("open city store: %v", err)
	}
	rigStore, err := beads.OpenFileStore(fsys.OSFS{}, filepath.Join(t.TempDir(), "rig-beads.json"))
	if err != nil {
		t.Fatalf("open rig store: %v", err)
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs:      []config.Rig{{Name: "blue", Path: "rig-blue"}},
	}
	sessionBead, err := cityStore.Create(beads.Bead{
		Title: "worker", Type: sessionBeadType, Status: "open", Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name": "worker-1", "transport": config.SessionTransportTmux,
			"state": string(session.StateActive), "generation": "3",
		},
	})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}
	work, err := rigStore.Create(beads.Bead{
		Title: "rig-owned execution", Type: "task", Status: "in_progress", Assignee: sessionBead.ID,
		Metadata: map[string]string{
			beadmeta.RootStoreRefMetadataKey:    "rig:blue",
			beadmeta.SessionIDMetadataKey:       sessionBead.ID,
			beadmeta.ClaimGenerationMetadataKey: "8",
			beadmeta.WorkDirMetadataKey:         repo,
			beadmeta.WorktreeBaseSHAMetadataKey: baseSHA,
			beadmeta.WorkOutcomeMetadataKey:     beadmeta.OutcomeFail,
		},
	})
	if err != nil {
		t.Fatalf("create rig work item: %v", err)
	}
	sp := newDeadRuntimeArtifactProvider()
	sp.visible["worker-1"] = true
	sp.dead["worker-1"] = true
	snapshot := newSessionBeadSnapshot([]beads.Bead{sessionBead})
	var stderr strings.Builder
	reaped := cleanupDeadRuntimeSessionCorpses(cityPath, cityStore, map[string]beads.Store{"blue": rigStore}, cfg, snapshot, nil, sp, nil, &stderr)
	if reaped != 1 {
		t.Fatalf("reaped = %d, want 1 after rig archive capture; stderr=%s", reaped, stderr.String())
	}
	identity := attemptevidence.Identity{
		Kind: attemptevidence.KindWorkbench, OwnerBeadID: work.ID, ExecutionBeadID: work.ID,
		SessionID: sessionBead.ID, SessionGeneration: "3", ClaimGeneration: "8",
	}
	attemptID, err := attemptevidence.AttemptID(identity)
	if err != nil {
		t.Fatalf("AttemptID: %v", err)
	}
	evidence, err := attemptevidence.Read(rigStore, work.ID, attemptID)
	if err != nil {
		t.Fatalf("rig store did not retain attempt evidence: %v", err)
	}
	if evidence.StoreRef != "rig:blue" {
		t.Fatalf("captured StoreRef = %q, want rig:blue", evidence.StoreRef)
	}
	if _, err := attemptevidence.Read(cityStore, work.ID, attemptID); !errors.Is(err, attemptevidence.ErrNotFound) {
		t.Fatalf("city store unexpectedly contains rig-owned evidence: %v", err)
	}
}

func TestSessionReapPathsCaptureBeforeClosingAssignedWorkbenchAttempts(t *testing.T) {
	for _, path := range []string{"stale-creating", "preboot", "dead-runtime"} {
		for _, durable := range []bool{true, false} {
			name := "capture-failure"
			if durable {
				name = "capture-success"
			}
			t.Run(path+"/"+name, func(t *testing.T) {
				state := string(session.StateActive)
				if path == "stale-creating" {
					state = "creating"
				}
				fixture := newSessionReapEvidenceFixture(t, durable, state)
				var stderr strings.Builder
				var reaped int

				switch path {
				case "stale-creating":
					sp := runtime.NewFake()
					now := fixture.session.CreatedAt.Add(staleCreatingStateTimeout + time.Second)
					reaped = reapStaleSessionBeads(fixture.cityPath, fixture.cfg, fixture.store, nil, sp, nil, &clock.Fake{Time: now}, &stderr)
				case "preboot":
					boot := fixture.session.CreatedAt.Add(time.Hour)
					withHostBootTime(t, boot, nil)
					sp := newDeadRuntimeArtifactProvider()
					sp.listErr = serverAbsentListErr()
					snapshot := newSessionBeadSnapshot([]beads.Bead{fixture.session})
					reaped = cleanupDeadRuntimeSessionCorpses(fixture.cityPath, fixture.store, nil, fixture.cfg, snapshot, nil, sp, &clock.Fake{Time: boot}, &stderr)
				case "dead-runtime":
					sp := newDeadRuntimeArtifactProvider()
					sp.visible["worker-1"] = true
					sp.dead["worker-1"] = true
					snapshot := newSessionBeadSnapshot([]beads.Bead{fixture.session})
					reaped = cleanupDeadRuntimeSessionCorpses(fixture.cityPath, fixture.store, nil, fixture.cfg, snapshot, nil, sp, nil, &stderr)
					if durable && sp.stopCalls["worker-1"] != 1 {
						t.Fatalf("Stop calls = %d, want 1 after evidence capture", sp.stopCalls["worker-1"])
					}
					if !durable && sp.stopCalls["worker-1"] != 0 {
						t.Fatalf("Stop calls = %d after capture failure, want 0 so the source stays available", sp.stopCalls["worker-1"])
					}
				}

				if durable {
					if reaped != 1 {
						t.Fatalf("reaped = %d, want 1 after capture; stderr=%s", reaped, stderr.String())
					}
					closed, err := fixture.store.Get(fixture.session.ID)
					if err != nil || closed.Status != "closed" {
						t.Fatalf("session status after capture = %q, err=%v", closed.Status, err)
					}
					released, err := fixture.store.Get(fixture.work.ID)
					if err != nil || released.Status != "open" || released.Assignee != "" {
						t.Fatalf("work after close = status %q assignee %q, err=%v", released.Status, released.Assignee, err)
					}
					attemptID, err := attemptevidence.AttemptID(attemptevidence.Identity{
						Kind: attemptevidence.KindWorkbench, OwnerBeadID: fixture.work.ID, ExecutionBeadID: fixture.work.ID,
						SessionID: fixture.session.ID, SessionGeneration: "3", ClaimGeneration: "8",
					})
					if err != nil {
						t.Fatal(err)
					}
					if _, err := attemptevidence.Read(fixture.store, fixture.work.ID, attemptID); err != nil {
						t.Fatalf("attempt was not archived before close: %v", err)
					}
					return
				}

				if reaped != 0 {
					t.Fatalf("reaped = %d after capture failure, want 0; stderr=%s", reaped, stderr.String())
				}
				openSession, err := fixture.store.Get(fixture.session.ID)
				if err != nil || openSession.Status != "open" {
					t.Fatalf("session was not preserved after capture failure: status=%q err=%v", openSession.Status, err)
				}
				assigned, err := fixture.store.Get(fixture.work.ID)
				if err != nil || assigned.Status != "in_progress" || assigned.Assignee != fixture.session.ID {
					t.Fatalf("assignment changed after capture failure: status=%q assignee=%q err=%v", assigned.Status, assigned.Assignee, err)
				}
				if !strings.Contains(stderr.String(), "attempt evidence capture failed") {
					t.Fatalf("stderr does not report capture refusal: %s", stderr.String())
				}
			})
		}
	}
}

func TestStaleCreatingPoolReapCapturesBeforeRuntimeStop(t *testing.T) {
	for _, durable := range []bool{true, false} {
		name := "capture-failure"
		if durable {
			name = "capture-success"
		}
		t.Run(name, func(t *testing.T) {
			fixture := newSessionReapEvidenceFixture(t, durable, "creating")
			poolName := PoolSessionName("worker", fixture.session.ID)
			for key, value := range map[string]string{
				"session_name":         poolName,
				"template":             "worker",
				"agent_name":           "worker",
				poolManagedMetadataKey: boolMetadata(true),
			} {
				if err := fixture.store.SetMetadata(fixture.session.ID, key, value); err != nil {
					t.Fatalf("set %s: %v", key, err)
				}
			}
			attemptID, err := attemptevidence.AttemptID(attemptevidence.Identity{
				Kind: attemptevidence.KindWorkbench, OwnerBeadID: fixture.work.ID, ExecutionBeadID: fixture.work.ID,
				SessionID: fixture.session.ID, SessionGeneration: "3", ClaimGeneration: "8",
			})
			if err != nil {
				t.Fatal(err)
			}

			sp := &evidenceInspectingStopRuntime{Fake: runtime.NewFake()}
			if durable {
				sp.onStop = func(name string) error {
					if name != poolName {
						return fmt.Errorf("Stop name = %q, want %q", name, poolName)
					}
					if _, err := attemptevidence.Read(fixture.store, fixture.work.ID, attemptID); err != nil {
						return fmt.Errorf("attempt was not archived before Stop: %w", err)
					}
					if err := os.RemoveAll(fixture.repo); err != nil {
						return fmt.Errorf("remove fixture checkout during Stop: %w", err)
					}
					return nil
				}
			}
			now := fixture.session.CreatedAt.Add(staleCreatingStateTimeout + time.Minute)
			var stderr strings.Builder
			got := reapStaleSessionBeads(fixture.cityPath, fixture.cfg, fixture.store, nil, sp, nil, &clock.Fake{Time: now}, &stderr)
			if durable {
				if got != 1 {
					t.Fatalf("reaped = %d, want 1 after archive and teardown; stderr=%s", got, stderr.String())
				}
				if sp.stopCalls != 1 {
					t.Fatalf("Stop calls = %d, want 1", sp.stopCalls)
				}
				if _, err := os.Stat(fixture.repo); !os.IsNotExist(err) {
					t.Fatalf("fixture checkout still exists after fake Stop; stat err=%v", err)
				}
				if _, err := attemptevidence.Read(fixture.store, fixture.work.ID, attemptID); err != nil {
					t.Fatalf("archive did not survive checkout removal: %v", err)
				}
				return
			}

			if got != 0 {
				t.Fatalf("reaped = %d after capture failure, want 0; stderr=%s", got, stderr.String())
			}
			if sp.stopCalls != 0 {
				t.Fatalf("Stop calls = %d after capture failure, want 0", sp.stopCalls)
			}
			openSession, err := fixture.store.Get(fixture.session.ID)
			if err != nil || openSession.Status != "open" {
				t.Fatalf("session was not preserved after failed capture: status=%q err=%v", openSession.Status, err)
			}
			assigned, err := fixture.store.Get(fixture.work.ID)
			if err != nil || assigned.Status != "in_progress" || assigned.Assignee != fixture.session.ID {
				t.Fatalf("assignment changed after failed capture: status=%q assignee=%q err=%v", assigned.Status, assigned.Assignee, err)
			}
			if _, err := os.Stat(fixture.repo); err != nil {
				t.Fatalf("fixture checkout was removed after failed capture: %v", err)
			}
		})
	}
}
