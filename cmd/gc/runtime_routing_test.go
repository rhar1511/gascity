package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime/acp"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/rollout/gate"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/routing"
	"github.com/gastownhall/gascity/internal/session"
)

func exactFakeRuntimeRouter(t *testing.T, cfg *config.City) *routing.Provider {
	t.Helper()
	backends := make(map[string]runtime.Provider)
	var base string
	for _, name := range []string{"fake", "capsule"} {
		binding, err := config.RuntimeBindingForName(cfg, name)
		if err != nil {
			t.Fatal(err)
		}
		key, _ := session.RuntimeBackendKey(name, binding.Fingerprint)
		backends[key] = runtime.NewFake()
		if name == "fake" {
			base = key
		}
	}
	p, err := routing.New(base, backends)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRuntimePoolAndNamedCreationBindFinalPersistedKey(t *testing.T) {
	for _, named := range []bool{false, true} {
		t.Run(map[bool]string{false: "pool", true: "named"}[named], func(t *testing.T) {
			cfg := runtimeRoutingConfig()
			cfg.Workspace.Name = "test-city"
			cfg.Agents[0].StartCommand = "true"
			cfg.Agents[0].MaxActiveSessions = intPtr(2)
			sp := exactFakeRuntimeRouter(t, cfg)
			store := beads.NewMemStore()
			city := t.TempDir()
			var info session.Info
			if !named {
				bp := newAgentBuildParams("test-city", city, cfg, sp, time.Now().UTC(), store, io.Discard)
				bp.sessionBeads = newSessionBeadSnapshotFromInfos(nil)
				var err error
				info, err = createPoolSessionBeadWithGuardedAlias(bp, &cfg.Agents[0], "helper", "helper-1", 1, nil)
				if err != nil {
					t.Fatal(err)
				}
			} else {
				cfg.NamedSessions = []config.NamedSession{{Name: "named", Template: "helper", Runtime: "capsule"}}
				sn := config.NamedSessionRuntimeName("test-city", cfg.Workspace, "named")
				tp := TemplateParams{SessionName: sn, TemplateName: "helper", InstanceName: "named", Alias: "named", ConfiguredNamedIdentity: "named", ConfiguredNamedMode: "on_demand", WorkDir: city, Command: "true"}
				var stderr bytes.Buffer
				index := syncSessionBeads(city, store, map[string]TemplateParams{sn: tp}, sp, map[string]bool{sn: true}, cfg, clock.Real{}, &stderr, true)
				id := index[sn]
				if id == "" {
					t.Fatal("no named session:", stderr.String())
				}
				var err error
				info, err = session.NewStore(beads.SessionStore{Store: store}).Get(id)
				if err != nil {
					t.Fatal(err)
				}
			}
			binding, _ := config.RuntimeBindingForName(cfg, "capsule")
			key, _ := session.RuntimeBackendKey(binding.Name, binding.Fingerprint)
			if info.RuntimeBackend != binding.Name || info.RuntimeFingerprint != binding.Fingerprint {
				t.Fatal("creation lost runtime identity")
			}
			if got, err := sp.BoundBackend(info.SessionName); err != nil || got != key {
				t.Fatalf("final key %q unbound: %q %v", info.SessionName, got, err)
			}
		})
	}
}

type creationOrderProvider struct {
	*routing.Provider
	entered chan struct{}
}

func (p *creationOrderProvider) WithBackendCreation(create func() error) error {
	select {
	case p.entered <- struct{}{}:
	default:
	}
	return p.Provider.WithBackendCreation(create)
}

func TestRuntimePoolCreationGateFollowsIdentifierLock(t *testing.T) {
	cfg := runtimeRoutingConfig()
	cfg.Workspace.Name = "test-city"
	cfg.Agents[0].StartCommand = "true"
	cfg.Agents[0].MaxActiveSessions = intPtr(2)
	sp := &creationOrderProvider{Provider: exactFakeRuntimeRouter(t, cfg), entered: make(chan struct{}, 1)}
	store := beads.NewMemStore()
	bp := newAgentBuildParams("test-city", t.TempDir(), cfg, sp, time.Now().UTC(), store, io.Discard)
	bp.sessionBeads = newSessionBeadSnapshotFromInfos(nil)
	lockTaken := false
	info, err := createPoolSessionBeadWithGuardedAliasUsingLock(bp, &cfg.Agents[0], "helper", "helper-1", 1, nil, func(_ string, _ []string, fn func() error) error {
		select {
		case <-sp.entered:
			t.Fatal("runtime gate taken before city identifier lock")
		default:
		}
		lockTaken = true
		return fn()
	})
	if err != nil {
		t.Fatal(err)
	}
	if !lockTaken {
		t.Fatal("city identifier lock skipped")
	}
	select {
	case <-sp.entered:
	default:
		t.Fatal("runtime creation gate skipped")
	}
	binding, _ := config.RuntimeBindingForName(cfg, "capsule")
	if info.RuntimeBackend != binding.Name || info.RuntimeFingerprint != binding.Fingerprint {
		t.Fatal("creation lost runtime identity")
	}
}

func TestRuntimeProspectiveCreationUsesOnlySelectedBackend(t *testing.T) {
	cfg := runtimeRoutingConfig()
	selected, sibling := runtime.NewFake(), runtime.NewFake()
	sibling.ListingUnattested = true
	binding, _ := config.RuntimeBindingForName(cfg, "capsule")
	key, _ := session.RuntimeBackendKey(binding.Name, binding.Fingerprint)
	sp, err := routing.New(key, map[string]runtime.Provider{key: selected, "acp": sibling})
	if err != nil {
		t.Fatal(err)
	}
	if runtime.ListRunningAttested(sp) {
		t.Fatal("incomplete union attested")
	}
	if err := prospectiveRuntimeNameAvailable(sp, "new-key", binding); err != nil {
		t.Fatal(err)
	}
	if len(sibling.SnapshotCalls()) != 0 {
		t.Fatal("prospective reservation contacted sibling")
	}
	if _, err := sp.BoundBackend("new-key"); err == nil {
		t.Fatal("availability check invented binding")
	}
	if err := selected.Start(context.Background(), "existing", runtime.Config{}); err != nil {
		t.Fatal(err)
	}
	if err := prospectiveRuntimeNameAvailable(sp, "existing", binding); !errors.Is(err, runtime.ErrSessionExists) {
		t.Fatal("existing runtime accepted", err)
	}
}

func runtimeRoutingConfig() *config.City {
	return &config.City{Session: config.SessionConfig{Provider: "fake"}, Agents: []config.Agent{{Name: "helper", Runtime: "capsule"}}, Runtimes: map[string]config.DiscoveredRuntime{"capsule": {Name: "capsule", Command: "/test/runtime"}}}
}

func TestPersistedRuntimeCompositionAndReload(t *testing.T) {
	cfg := runtimeRoutingConfig()
	binding, err := config.RuntimeBindingForName(cfg, "capsule")
	if err != nil {
		t.Fatal(err)
	}
	info := session.Info{ID: "held", SessionName: "exact-existing-key", RuntimeBackend: binding.Name, RuntimeFingerprint: binding.Fingerprint}
	snapshot := newSessionBeadSnapshotFromInfos([]session.Info{info})
	ctx := sessionProviderContextForCity(cfg, "", "")
	sp, err := newSessionProviderFromContext(ctx, snapshot)
	if err != nil {
		t.Fatal(err)
	}
	router, ok := runtime.AsBackendBindingProvider(sp)
	if !ok {
		t.Fatal("explicit selection did not construct exact router")
	}
	want, _ := session.RuntimeBackendKey(binding.Name, binding.Fingerprint)
	if got, err := router.BoundBackend(info.SessionName); err != nil || got != want {
		t.Fatalf("binding=%q, %v", got, err)
	}
	// Removing all selectors cannot silently downgrade persisted owners.
	next := *cfg
	next.Agents = []config.Agent{{Name: "helper"}}
	retained, err := rebuildSessionRuntimeRouting(sessionProviderContextForCity(&next, "", ""), snapshot, sp)
	if err != nil {
		t.Fatal(err)
	}
	prior := sp.(interface {
		BackendForKey(string) (runtime.Provider, bool)
	})
	current := retained.(interface {
		BackendForKey(string) (runtime.Provider, bool)
	})
	a, _ := prior.BackendForKey(want)
	b, _ := current.BackendForKey(want)
	if a != b {
		t.Fatal("unchanged owned backend replaced during reload")
	}
	changed := next
	changed.Runtimes = map[string]config.DiscoveredRuntime{"capsule": {Name: "capsule", Command: "/changed/runtime"}}
	if _, err := rebuildSessionRuntimeRouting(sessionProviderContextForCity(&changed, "", ""), snapshot, sp); err == nil {
		t.Fatal("changed declaration accepted for dormant owner")
	}
	removed := next
	removed.Runtimes = nil
	if _, err := rebuildSessionRuntimeRouting(sessionProviderContextForCity(&removed, "", ""), snapshot, sp); err == nil {
		t.Fatal("removed declaration accepted")
	}
}

func TestMixedRuntimeRefusesUnboundAndUnavailableHistory(t *testing.T) {
	cfg := runtimeRoutingConfig()
	for _, snapshot := range []*sessionBeadSnapshot{
		newSessionBeadSnapshotFromInfos([]session.Info{{ID: "legacy-held", SessionName: "legacy"}}),
		newSessionBeadSnapshotWithError(errors.New("unavailable")),
	} {
		if _, err := newSessionProviderFromContext(sessionProviderContextForCity(cfg, "", ""), snapshot); err == nil {
			t.Fatal("guessed missing history")
		}
	}
	for _, base := range []string{"fake", "acp"} {
		cfg := &config.City{Session: config.SessionConfig{Provider: base}}
		if _, err := newSessionProviderFromContext(sessionProviderContextForCity(cfg, "", ""), newSessionBeadSnapshotWithError(errors.New("unavailable"))); err == nil {
			t.Fatal("failed authoritative snapshot treated as empty")
		}
	}
	if _, err := newSessionProviderFromContext(sessionProviderContextForCity(cfg, "", ""), newSessionBeadSnapshotFromInfos(nil)); err != nil {
		t.Fatal(err)
	}
}

func TestRuntimeSnapshotFailureSurvivesRemovedSelector(t *testing.T) {
	prior := openSessionProviderStore
	openSessionProviderStore = func(string) (beads.Store, error) { return nil, errors.New("cannot open authoritative store") }
	t.Cleanup(func() { openSessionProviderStore = prior })
	for _, base := range []string{"fake", "acp"} {
		cfg := &config.City{Session: config.SessionConfig{Provider: base}}
		ctx := sessionProviderContextForCity(cfg, "/not-a-real-city", "")
		if _, err := newSessionProviderFromContext(ctx, loadProviderSessionSnapshot(ctx)); err == nil || !strings.Contains(err.Error(), "authoritative store") {
			t.Fatalf("snapshot failure=%v", err)
		}
	}
}

func TestMixedRuntimeUnknownBaseCannotFallBack(t *testing.T) {
	cfg := runtimeRoutingConfig()
	cfg.Session.Provider = "typo"
	if _, err := newSessionProviderFromContext(sessionProviderContextForCity(cfg, "", ""), newSessionBeadSnapshotFromInfos(nil)); err == nil {
		t.Fatal("unknown base fell back to tmux")
	}
}

func TestStatusRuntimeWrapperRetainsExactBinding(t *testing.T) {
	cfg := runtimeRoutingConfig()
	sp, err := newSessionProviderFromContext(sessionProviderContextForCity(cfg, "", ""), newSessionBeadSnapshotFromInfos(nil))
	if err != nil {
		t.Fatal(err)
	}
	wrapped := newBoundedStatusProvider(sp)
	binder, ok := runtime.AsBackendBindingProvider(wrapped)
	if !ok {
		t.Fatal("status wrapper lost binding capability")
	}
	binding, _ := config.RuntimeBindingForName(cfg, "capsule")
	key, _ := session.RuntimeBackendKey(binding.Name, binding.Fingerprint)
	if err := binder.Bind("exact-status-key", key); err != nil {
		t.Fatal(err)
	}
	if got, err := binder.BoundBackend("exact-status-key"); err != nil || got != key {
		t.Fatalf("binding=%q %v", got, err)
	}
	if _, ok := runtime.AsBackendBindingProvider(newBoundedStatusProvider(runtime.NewFake())); ok {
		t.Fatal("legacy wrapper claimed strict binding support")
	}
}

func TestRuntimeActualACPDoesNotBlockSelectedNamespace(t *testing.T) {
	cfg := runtimeRoutingConfig()
	binding, _ := config.RuntimeBindingForName(cfg, "capsule")
	key, _ := session.RuntimeBackendKey(binding.Name, binding.Fingerprint)
	sp, err := routing.New(key, map[string]runtime.Provider{key: runtime.NewFake(), "acp": acp.NewProviderWithDir(t.TempDir(), acp.Config{})})
	if err != nil {
		t.Fatal(err)
	}
	if runtime.ListRunningAttested(sp) {
		t.Fatal("ACP inventory unexpectedly attested")
	}
	if err := prospectiveRuntimeNameAvailable(sp, "new-owned-key", binding); err != nil {
		t.Fatal(err)
	}
}

func TestRuntimeStandaloneCreationRequiresExclusiveCurrentConfig(t *testing.T) {
	city := t.TempDir()
	if err := os.MkdirAll(filepath.Join(city, ".gc"), 0o700); err != nil {
		t.Fatal(err)
	}
	tomlPath := filepath.Join(city, "city.toml")
	writeCityRuntimeConfig(t, tomlPath, "fake")
	cfg, err := loadCityConfig(city, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	called := false
	err = withStandaloneRuntimeCreation(city, cfg, func() error {
		called = true
		if lock, err := acquireControllerLock(city); err == nil {
			if err := lock.Close(); err != nil {
				t.Fatal(err)
			}
			t.Fatal("creator did not hold exclusive controller lock")
		}
		return nil
	})
	if err != nil || !called {
		t.Fatalf("exclusive current creator refused: %v", err)
	}
	lock, err := acquireControllerLock(city)
	if err != nil {
		t.Fatal(err)
	}
	called = false
	err = withStandaloneRuntimeCreation(city, cfg, func() error { called = true; return nil })
	if !errors.Is(err, runtime.ErrRuntimeUnavailable) || called {
		t.Fatalf("competing creator was admitted: %v", err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	writeCityRuntimeConfig(t, tomlPath, "fail")
	err = withStandaloneRuntimeCreation(city, cfg, func() error { called = true; return nil })
	if !errors.Is(err, runtime.ErrRuntimeUnavailable) || called {
		t.Fatalf("stale config creator was admitted: %v", err)
	}
}

func TestRuntimePersistedOwnerPreservesRigScopeAndWorkDir(t *testing.T) {
	city := t.TempDir()
	writeTemplateResolveCityConfig(t, city, "file")
	rigRoot := filepath.Join(city, "demo")
	workDir := filepath.Join(city, "retained-worktree")
	for _, path := range []string{rigRoot, workDir} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cfg := runtimeRoutingConfig()
	cfg.Workspace = config.Workspace{Name: "test-city", Provider: "test"}
	cfg.Providers = map[string]config.ProviderSpec{"test": {Command: "echo", PromptMode: "none"}}
	cfg.Rigs = []config.Rig{{Name: "demo", Path: rigRoot}}
	cfg.Agents[0].Dir = "demo"
	cfg.Agents[0].WorkDir = rigRoot
	sp := exactFakeRuntimeRouter(t, cfg)
	bp := newAgentBuildParams("test-city", city, cfg, sp, time.Unix(0, 0), beads.NewMemStore(), io.Discard)
	binding, _ := config.RuntimeBindingForName(cfg, "capsule")
	info := session.Info{ID: "held", Template: "demo/helper", SessionName: "demo--held", SessionNameMetadata: "demo--held", AgentName: "demo/held", WorkDir: workDir, RuntimeBackend: binding.Name, RuntimeFingerprint: binding.Fingerprint}
	tp, err := resolveTemplateForSessionBeadInfo(bp, &cfg.Agents[0], "demo/held", nil, info)
	if err != nil {
		t.Fatal(err)
	}
	if tp.WorkDir != workDir || tp.Env["GC_TEMPLATE"] != "demo/helper" || tp.Env["GC_RIG"] != "demo" || tp.Env["GC_RIG_ROOT"] != rigRoot {
		t.Fatalf("persisted owner lost scope/cwd: cwd=%s template=%s rig=%s root=%s", tp.WorkDir, tp.Env["GC_TEMPLATE"], tp.Env["GC_RIG"], tp.Env["GC_RIG_ROOT"])
	}
}

func TestRuntimeActualReloadPreservesOwnersAndRefusesDeclarationDrift(t *testing.T) {
	t.Setenv("GC_SESSION", "")
	city := t.TempDir()
	packDir := filepath.Join(city, "packs", "runtime")
	if err := os.MkdirAll(packDir, 0o755); err != nil {
		t.Fatal(err)
	}
	packPath := filepath.Join(packDir, "pack.toml")
	writePack := func(command string) {
		t.Helper()
		if err := os.WriteFile(packPath, []byte(fmt.Sprintf("[pack]\nname = \"runtime\"\nschema = 1\n[runtimes.capsule]\ncommand = %q\n", command)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writePack("/bin/true")
	tomlPath := filepath.Join(city, "city.toml")
	base := "[workspace]\nname = \"test-city\"\n[beads]\nprovider = \"file\"\n[session]\nprovider = \"fake\"\n[imports.runtime]\nsource = \"packs/runtime\"\n[[agent]]\nname = \"helper\"\nruntime = \"capsule\"\nstart_command = \"true\"\n"
	writeConfig := func(text string) {
		t.Helper()
		if err := os.WriteFile(tomlPath, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	writeConfig(base)
	cfg, rev := loadCityRuntimeControllerConfig(t, city)
	old := exactFakeRuntimeRouter(t, cfg)
	mem, openErr := openCityStoreAt(city)
	if openErr != nil {
		t.Fatal(openErr)
	}
	t.Cleanup(func() {
		if err := closeBeadStoreHandle(mem); err != nil {
			t.Error(err)
		}
	})
	binding, _ := config.RuntimeBindingForName(cfg, "capsule")
	_, err := session.NewStore(beads.SessionStore{Store: mem}).CreateSessionInfo(session.CreateSpec{Title: "held", AgentName: "helper", RuntimeBackend: binding.Name, RuntimeFingerprint: binding.Fingerprint, Metadata: map[string]string{"session_name": "held-exact", "template": "helper", "state": "asleep", "session_key": "retained-native", "work_dir": city}})
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cr := newTestCityRuntime(t, CityRuntimeParams{CityPath: city, CityName: "test-city", TomlPath: tomlPath, ConfigRev: rev, Cfg: cfg, SP: old, Dops: newDrainOps(old), Rec: events.Discard, Stdout: io.Discard, Stderr: &stderr, BuildFn: func(*config.City, runtime.Provider, beads.Store) DesiredStateResult {
		return DesiredStateResult{State: map[string]TemplateParams{}}
	}})
	cr.standaloneCityStore = mem
	writeConfig(base + "\n# accepted unrelated reload\n")
	last := "fake"
	reply := cr.reloadConfigTraced(context.Background(), &last, city, nil, reloadSourceManual)
	if reply.Outcome != reloadOutcomeApplied {
		t.Fatalf("reload refused: %+v %s", reply, stderr.String())
	}
	current, ok := cr.sp.(*routing.Provider)
	if !ok {
		t.Fatalf("strict registry lost: %T", cr.sp)
	}
	key, _ := session.RuntimeBackendKey(binding.Name, binding.Fingerprint)
	originalBackend, _ := old.BackendForKey(key)
	currentBackend, _ := current.BackendForKey(key)
	if originalBackend != currentBackend {
		t.Fatal("unchanged owner backend replaced")
	}
	if owner, err := current.BoundBackend("held-exact"); err != nil || owner != key {
		t.Fatal("persisted owner lost after reload", owner, err)
	}
	if err := old.WithBackendCreation(func() error { return nil }); err == nil {
		t.Fatal("accepted reload left old creator enabled")
	}
	// Removing selectors must preserve explicit persisted ownership.
	writeConfig(strings.Replace(base, "runtime = \"capsule\"\n", "", 1) + "\n# selector removed\n")
	reply = cr.reloadConfigTraced(context.Background(), &last, city, nil, reloadSourceManual)
	if reply.Outcome != reloadOutcomeApplied {
		t.Fatalf("retained owner downgrade: %+v", reply)
	}
	retainedCfg, retainedSP := cr.cfg, cr.sp
	writePack("/bin/false")
	reply = cr.reloadConfigTraced(context.Background(), &last, city, nil, reloadSourceManual)
	if reply.Outcome != reloadOutcomeFailed || cr.cfg != retainedCfg || cr.sp != retainedSP {
		t.Fatal("owned declaration drift published", reply)
	}
	active := retainedSP.(*routing.Provider)
	if err := active.WithBackendCreation(func() error { return nil }); err != nil {
		t.Fatal("rejected publication retired current creation", err)
	}
	for _, provider := range []runtime.Provider{originalBackend, currentBackend} {
		if provider.(*runtime.Fake).CountCalls("Stop", "held-exact") != 0 {
			t.Fatal("reload stopped retained owner")
		}
	}
}

// runtimeStartTestStore exercises the production require-mode store boundary.
func runtimeStartTestStore(t *testing.T, store beads.Store) beads.Store {
	t.Helper()
	opened, err := beads.OpenStoreAtForCity(context.Background(), beads.StoreOpenOptions{ScopeRoot: t.TempDir(), Provider: "file", ConditionalWrites: gate.Require, OpenFileStore: func() (beads.Store, error) { return store, nil }})
	if err != nil {
		t.Fatal(err)
	}
	return opened.Store
}

func TestRuntimeDeferredClaimlessWakeReleasesExactAttempt(t *testing.T) {
	for _, outcome := range []TraceOutcomeCode{TraceOutcomeSessionInitializing, TraceOutcomeDeferred} {
		t.Run(string(outcome), func(t *testing.T) {
			mem := beads.NewMemStore()
			b, err := mem.Create(beads.Bead{Title: "worker", Type: sessionBeadType, Labels: []string{sessionBeadLabel}, Metadata: creatingMeta(map[string]string{"session_name": "worker", "template": "worker", "generation": "2", "instance_token": "token-2", "session_key": "native-key", "last_woke_at": "2026-11-05T00:00:00Z", "gc.runtime_backend": "capsule", "gc.runtime_fingerprint": strings.Repeat("a", 64)})})
			if err != nil {
				t.Fatal(err)
			}
			front := sessionFrontDoor(runtimeStartTestStore(t, mem))
			info, err := front.Get(b.ID)
			if err != nil {
				t.Fatal(err)
			}
			clk := &clock.Fake{Time: time.Date(2026, 11, 5, 0, 0, 1, 0, time.UTC)}
			result := startResult{prepared: preparedStart{candidate: startCandidate{info: info, tp: TemplateParams{SessionName: "worker", TemplateName: "worker"}}}, outcome: outcome, started: clk.Now(), finished: clk.Now()}
			if commitStartResult(result, front, clk, events.Discard, 0, io.Discard, io.Discard) {
				t.Fatal("deferred attempt committed")
			}
			after, err := front.Get(b.ID)
			if err != nil {
				t.Fatal(err)
			}
			if after.LastWokeAt != "" || after.PendingCreateClaim {
				t.Fatalf("completed claimless lease retained: last_woke_at=%q claim=%v", after.LastWokeAt, after.PendingCreateClaim)
			}
			if pendingCreateStartInFlightInfo(after, clk, time.Minute) {
				t.Fatal("completed attempt still blocks the next patrol")
			}
			if after.SessionKey != info.SessionKey || after.InstanceToken != info.InstanceToken || after.RuntimeBackend != info.RuntimeBackend || after.RuntimeFingerprint != info.RuntimeFingerprint {
				t.Fatal("deferred completion changed custody")
			}
		})
	}
}

func TestRuntimePreparationFailureRestoresOwnPreWake(t *testing.T) {
	for _, claim := range []bool{false, true} {
		t.Run(fmt.Sprint(claim), func(t *testing.T) {
			mem := &failSetMetadataStore{MemStore: beads.NewMemStore(), failKey: "session_key"}
			metadata := creatingMeta(map[string]string{"session_name": "worker", "template": "worker", "generation": "1", "continuation_epoch": "1", "instance_token": "token-worker", "gc.runtime_backend": "capsule", "gc.runtime_fingerprint": strings.Repeat("a", 64)})
			if claim {
				metadata["pending_create_claim"] = "true"
			}
			b, err := mem.Create(beads.Bead{Title: "worker", Type: sessionBeadType, Labels: []string{sessionBeadLabel}, Metadata: metadata})
			if err != nil {
				t.Fatal(err)
			}
			store := runtimeStartTestStore(t, mem)
			info, err := sessionFrontDoor(store).Get(b.ID)
			if err != nil {
				t.Fatal(err)
			}
			backend := newGatedStartProvider()
			t.Cleanup(func() { backend.release("worker") })
			key, _ := session.RuntimeBackendKey(info.RuntimeBackend, info.RuntimeFingerprint)
			sp, err := routing.New(key, map[string]runtime.Provider{key: backend})
			if err != nil {
				t.Fatal(err)
			}
			cfg := &config.City{Agents: []config.Agent{{Name: "worker"}}}
			tp := TemplateParams{Command: "worker", SessionName: "worker", TemplateName: "worker", ResolvedProvider: &config.ResolvedProvider{SessionIDFlag: "--session-id"}}
			clk := &clock.Fake{Time: time.Date(2026, 11, 5, 0, 0, 1, 0, time.UTC)}
			if n := executePlannedStartsTraced(context.Background(), []startCandidate{{info: info, tp: tp}}, cfg, map[string]TemplateParams{"worker": tp}, sp, store, "test-city", "", clk, events.Discard, time.Minute, io.Discard, io.Discard, nil, withAsyncStartExecution()); n != 0 {
				t.Fatalf("woken=%d", n)
			}
			backend.ensureNoFurtherStart(t, 100*time.Millisecond)
			after, err := sessionFrontDoor(store).Get(b.ID)
			if err != nil {
				t.Fatal(err)
			}
			if after.LastWokeAt != "" {
				t.Fatalf("failed preparation retained lease %q", after.LastWokeAt)
			}
			if after.PendingCreateClaim != claim || after.RuntimeBackend != info.RuntimeBackend || after.RuntimeFingerprint != info.RuntimeFingerprint || after.SessionKey != info.SessionKey {
				t.Fatal("failed preparation changed runtime or conversation custody")
			}
			// PreWake owns a new incarnation even when preparation later fails;
			// restoring the lease must not resurrect the previous incarnation.
			if after.Generation != "2" || after.InstanceToken == "" || after.InstanceToken == info.InstanceToken {
				t.Fatal("failed preparation lost its allocated incarnation")
			}
		})
	}
}

func TestRuntimePreWakeRestoreRequiresAuthoritativeFence(t *testing.T) {
	for _, disable := range []bool{false, true} {
		t.Run(fmt.Sprint(disable), func(t *testing.T) {
			mem := beads.NewMemStore()
			b, err := mem.Create(beads.Bead{Title: "worker", Type: sessionBeadType, Labels: []string{sessionBeadLabel}, Metadata: creatingMeta(map[string]string{"session_name": "worker", "template": "worker", "generation": "1", "instance_token": "token-worker", "gc.runtime_backend": "capsule", "gc.runtime_fingerprint": strings.Repeat("a", 64)})})
			if err != nil {
				t.Fatal(err)
			}
			var store beads.Store = mem
			if !disable {
				store = runtimeStartTestStore(t, mem)
			}
			front := sessionFrontDoor(store)
			info, persisted, err := front.GetPersistedResponse(b.ID)
			if err != nil {
				t.Fatal(err)
			}
			clk := &clock.Fake{Time: time.Date(2026, 11, 5, 0, 0, 1, 0, time.UTC)}
			_, _, fold, err := preWakeCommit(info, front, clk)
			if err != nil {
				t.Fatal(err)
			}
			attempted := info.ApplyPatch(fold)
			undo := newPreWakeUndo(info, persisted.Metadata, fold)
			undo.token = attempted.InstanceToken
			undo.custody = attempted
			mem.DisableConditionalWrites = disable
			restorePreWakeState(preparedStart{candidate: startCandidate{info: attempted}, preWakeUndo: undo}, front, io.Discard)
			after, err := mem.Get(b.ID)
			if err != nil {
				t.Fatal(err)
			}
			if disable && after.Metadata["last_woke_at"] != attempted.LastWokeAt {
				t.Fatal("unfenced restore wrote metadata")
			}
			if !disable && after.Metadata["last_woke_at"] != "" {
				t.Fatal("owned fenced attempt was not restored")
			}
		})
	}
}
