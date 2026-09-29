//go:build integration

package integration

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/attemptevidence"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/proxyendpoint"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/test/tmuxtest"
	"gopkg.in/yaml.v3"
)

type graphBead struct {
	ID        string         `json:"id"`
	Title     string         `json:"title"`
	Ref       string         `json:"ref"`
	Status    string         `json:"status"`
	Type      string         `json:"type"`
	IssueType string         `json:"issue_type"`
	Metadata  map[string]any `json:"metadata"`
}

type graphConvoyCreateResult struct {
	ConvoyID string `json:"convoy_id"`
}

func beadType(bead graphBead) string {
	if bead.Type != "" {
		return bead.Type
	}
	return bead.IssueType
}

func metaValue(bead graphBead, key string) string {
	if bead.Metadata == nil {
		return ""
	}
	raw, ok := bead.Metadata[key]
	if !ok || raw == nil {
		return ""
	}
	switch v := raw.(type) {
	case string:
		return v
	case bool:
		return strconv.FormatBool(v)
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	default:
		return fmt.Sprint(v)
	}
}

// TestGraphWorkflowSuccessPath is the E1 Large owner for the installed
// private-evidence service and graph-workflow composition boundary.
func TestGraphWorkflowSuccessPath(t *testing.T) {
	cityDir, transport := setupGraphWorkflowCityWithPrivateEvidence(t, "success")
	convoyID, workflowID := startScopedWorkflowWithPrivateEvidenceHTTP(t, cityDir, transport)

	workflow := waitForBeadClosedWithoutPrivatePayloadDiagnostics(t, cityDir, transport, workflowID, graphWorkflowCloseTimeout())
	if got := metaValue(workflow, "gc.outcome"); got != "pass" {
		t.Fatalf("workflow outcome = %q, want pass", got)
	}

	body := findGraphWorkflowBeadByRefSuffixOverHTTP(t, transport, workflowID, ".body")
	if got := metaValue(body, "gc.outcome"); got != "pass" {
		t.Fatalf("body outcome = %q, want pass", got)
	}

	convoy, err := readGraphBeadOverHTTP(t, transport, convoyID)
	if err != nil {
		t.Fatalf("read graph convoy through installed service: %v", err)
	}
	if got := metaValue(convoy, "work_dir"); got != "" {
		t.Fatalf("convoy work_dir = %q, want unset after cleanup", got)
	}
	if got := metaValue(convoy, "submitted"); got != "true" {
		t.Fatalf("convoy submitted = %q, want true", got)
	}

	beadStore := beads.NewBdStoreWithPrefix(cityDir, func(string, string, ...string) ([]byte, error) {
		return nil, errors.New("private evidence read unexpectedly used the bd command runner")
	}, "gc", beads.WithBdStorePrivateEvidenceHTTP(beads.PrivateEvidenceHTTPConfig{
		Endpoint: transport.endpoint, ProjectID: transport.projectID, Database: transport.database,
		ScopeRef: transport.scopeRef, TokenFile: transport.tokenFile,
	}))
	if !beadStore.PrivateEvidencePayloadTransportReady() {
		t.Fatal("actual graph city did not bind the configured private-evidence transport")
	}
	controls := listGraphRetryControlsOverHTTP(t, transport)
	var sealedRetries int
	for _, control := range controls {
		attempts, err := attemptevidence.List(beadStore, control.ID)
		if err != nil {
			t.Fatalf("read retry evidence for control %s: %v", control.ID, err)
		}
		for _, attempt := range attempts {
			if attempt.Identity.Kind != attemptevidence.KindRetry {
				continue
			}
			readback, err := attemptevidence.Read(beadStore, control.ID, attempt.AttemptID)
			if err != nil || !reflect.DeepEqual(readback, attempt) {
				t.Fatalf("exact retry evidence readback for control %s failed: %v", control.ID, err)
			}
			sealedRetries++
		}
	}
	if sealedRetries == 0 {
		t.Fatal("graph workflow passed without an exact retry-attempt evidence archive")
	}

	worktreePath := filepath.Join(cityDir, "worktrees", convoyID)
	if _, err := os.Stat(worktreePath); !os.IsNotExist(err) {
		t.Fatalf("worktree path %s should be removed, stat err=%v", worktreePath, err)
	}

	report := readWorkflowReport(t, cityDir)
	for _, suffix := range []string{
		".load-context",
		".workspace-setup",
		".preflight-tests",
		".implement",
		".self-review",
		".submit",
		".cleanup-worktree",
	} {
		if !strings.Contains(report, suffix) {
			t.Fatalf("report missing %s:\n%s", suffix, report)
		}
	}

	assertControlDispatcherLane(t, cityDir)
}

func TestGraphProxyProofRejectsReachableUnownedPortMirror(t *testing.T) {
	cityDir := t.TempDir()
	beadsDir := filepath.Join(cityDir, ".beads")
	if err := os.MkdirAll(beadsDir, 0o700); err != nil {
		t.Fatalf("create fixture Beads directory: %v", err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen on disposable loopback port: %v", err)
	}
	t.Cleanup(func() {
		if err := listener.Close(); err != nil {
			t.Errorf("close disposable loopback listener: %v", err)
		}
	})
	port := strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)
	if err := os.WriteFile(filepath.Join(beadsDir, "dolt-server.port"), []byte(port+"\n"), 0o600); err != nil {
		t.Fatalf("write reachable port mirror: %v", err)
	}
	if got, ok := currentManagedDoltPortForTest(cityDir); !ok || got != port {
		t.Fatalf("generic port mirror probe = %q, %v; want reachable fixture port %q", got, ok, port)
	}
	if got, ok := graphFixtureProxyPortForTest(cityDir); ok {
		t.Fatalf("graph proxy proof accepted unowned reachable port %q", got)
	}
	if runtime.GOOS == "linux" {
		if _, err := graphFixtureProxyPortForTestWithDiagnostic(cityDir); err == nil || !strings.Contains(err.Error(), "proxy root is absent") {
			t.Fatalf("detailed graph proxy proof error = %v, want the missing fixture-root reason", err)
		}
	}
}

func TestGraphProcessFlagRejectsDuplicatesAndMissingValues(t *testing.T) {
	for _, tc := range []struct {
		name  string
		args  []string
		want  string
		found bool
		valid bool
	}{
		{name: "absent", args: []string{"dolt", "sql-server"}, valid: true},
		{name: "single separated", args: []string{"dolt", "sql-server", "--config", "/city/.beads/dolt/config.yaml"}, want: "/city/.beads/dolt/config.yaml", found: true, valid: true},
		{name: "single inline", args: []string{"dolt", "sql-server", "--config=/city/.beads/dolt/config.yaml"}, want: "/city/.beads/dolt/config.yaml", found: true, valid: true},
		{name: "duplicate same", args: []string{"dolt", "sql-server", "--config", "/city/.beads/dolt/config.yaml", "--config", "/city/.beads/dolt/config.yaml"}, valid: false},
		{name: "duplicate conflicting", args: []string{"dolt", "sql-server", "--config=/city/.beads/dolt/config.yaml", "--config=/host/config.yaml"}, valid: false},
		{name: "missing value", args: []string{"dolt", "sql-server", "--config"}, valid: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, found, valid := graphProcessFlag(tc.args, "--config")
			if got != tc.want || found != tc.found || valid != tc.valid {
				t.Fatalf("graphProcessFlag() = (%q, %v, %v), want (%q, %v, %v)", got, found, valid, tc.want, tc.found, tc.valid)
			}
		})
	}
}

func TestGraphCleanupProcessPathsMatchAfterFixtureFilesAreRemoved(t *testing.T) {
	root := filepath.Join(t.TempDir(), "removed-city", ".beads", "dolt")
	configPath := filepath.Join(root, proxyendpoint.ConfigFileName)
	if graphCleanupPathEquals(root+" (deleted) extra", root) {
		t.Fatal("cleanup matcher accepted a deleted process cwd with an unrelated suffix")
	}
	if !graphCleanupPathEquals(root, root) || !graphCleanupPathEquals(root+" (deleted)", root) {
		t.Fatal("cleanup matcher required a fixture path to remain on disk")
	}
	if !graphCleanupArgvMentionsPath([]string{"dolt", "sql-server", "--config", configPath}, "--config", configPath) {
		t.Fatal("cleanup matcher did not recognize the exact process-owned config path")
	}
}

func TestGraphProxyConfigMatchesSemanticListenerFields(t *testing.T) {
	for _, tc := range []struct {
		name   string
		config string
		want   bool
	}{
		{name: "field order is irrelevant", config: "listener:\n  port: 43123\n  host: 127.0.0.1\n", want: true},
		{name: "non-loopback host", config: "listener:\n  host: 0.0.0.0\n  port: 43123\n"},
		{name: "wrong port", config: "listener:\n  host: 127.0.0.1\n  port: 3307\n"},
		{name: "unix socket", config: "listener:\n  host: 127.0.0.1\n  port: 43123\n  socket: /tmp/beads.sock\n"},
		{name: "duplicate host key", config: "listener:\n  host: 127.0.0.1\n  host: 0.0.0.0\n  port: 43123\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := graphProxyConfigMatches([]byte(tc.config), 43123); got != tc.want {
				t.Fatalf("graphProxyConfigMatches() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestGraphWorkflowFailureRunsCleanup(t *testing.T) {
	cityDir := setupGraphWorkflowCity(t, "fail-preflight")
	convoyID, workflowID := startScopedWorkflow(t, cityDir)

	workflow := waitForBeadClosed(t, cityDir, workflowID, graphWorkflowCloseTimeout())
	if got := metaValue(workflow, "gc.outcome"); got != "fail" {
		t.Fatalf("workflow outcome = %q, want fail", got)
	}

	body := mustFindWorkflowBeadByRefSuffix(t, cityDir, workflowID, ".body")
	if got := metaValue(body, "gc.outcome"); got != "fail" {
		t.Fatalf("body outcome = %q, want fail", got)
	}

	convoy := showBead(t, cityDir, convoyID)
	if got := metaValue(convoy, "work_dir"); got != "" {
		t.Fatalf("convoy work_dir = %q, want unset after cleanup", got)
	}
	if got := metaValue(convoy, "submitted"); got != "" {
		t.Fatalf("convoy submitted = %q, want unset on failed workflow", got)
	}

	for _, suffix := range []string{".implement", ".self-review", ".submit"} {
		bead := mustFindWorkflowBeadByRefSuffix(t, cityDir, workflowID, suffix)
		if bead.Status != "closed" {
			t.Fatalf("%s status = %q, want closed", suffix, bead.Status)
		}
		if got := metaValue(bead, "gc.outcome"); got != "skipped" {
			t.Fatalf("%s outcome = %q, want skipped", suffix, got)
		}
	}

	report := readWorkflowReport(t, cityDir)
	for _, suffix := range []string{".load-context", ".workspace-setup", ".preflight-tests", ".cleanup-worktree"} {
		if !strings.Contains(report, suffix) {
			t.Fatalf("report missing %s:\n%s", suffix, report)
		}
	}
	for _, suffix := range []string{".implement", ".self-review", ".submit"} {
		if strings.Contains(report, suffix) {
			t.Fatalf("report should not include %s after abort:\n%s", suffix, report)
		}
	}

	assertControlDispatcherLane(t, cityDir)
}

func assertControlDispatcherLane(t *testing.T, cityDir string) {
	t.Helper()

	tracePaths := []string{
		citylayout.ControlDispatcherTraceDefaultPath(cityDir),
		citylayout.ControlDispatcherTraceDefaultPathFor(cityDir, "core.control-dispatcher"),
	}
	var traces []string
	foundControlTrace := false
	for _, path := range tracePaths {
		trace := readOptionalFile(path)
		if strings.Contains(trace, "serve process bead=") {
			foundControlTrace = true
			break
		}
		traces = append(traces, fmt.Sprintf("%s:\n%s", path, trace))
	}
	if !foundControlTrace {
		t.Fatalf("control-dispatcher trace missing processed control bead evidence:\n%s", strings.Join(traces, "\n\n"))
	}

	workerTrace := readOptionalFile(filepath.Join(cityDir, "graph-workflow-trace.log"))
	if strings.Contains(workerTrace, "unexpected-control") {
		t.Fatalf("worker should not receive control beads:\n%s", workerTrace)
	}
}

func graphWorkflowCloseTimeout() time.Duration {
	return 6 * time.Minute
}

func setupGraphWorkflowCity(t *testing.T, mode string) string {
	cityDir, _ := setupGraphWorkflowCityWithOptions(t, mode, false)
	return cityDir
}

type graphPrivateEvidenceTransport struct {
	endpoint       string
	projectID      string
	database       string
	scopeRef       string
	tokenFile      string
	token          string
	configRevision string
	bdLauncher     string
}

const graphFixtureCityDatabase = "hq"

type graphBeadsIdentity struct {
	projectID string
	database  string
}

type boundedTailBuffer struct {
	buf []byte
	max int
}

func (b *boundedTailBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if b.max <= 0 {
		return n, nil
	}
	b.buf = append(b.buf, p...)
	if len(b.buf) > b.max {
		copy(b.buf, b.buf[len(b.buf)-b.max:])
		b.buf = b.buf[:b.max]
	}
	return n, nil
}

func (b *boundedTailBuffer) String() string { return string(b.buf) }

func TestBoundedTailBufferKeepsRecentOutput(t *testing.T) {
	var output boundedTailBuffer
	output.max = 5
	if _, err := output.Write([]byte("first")); err != nil {
		t.Fatal(err)
	}
	if _, err := output.Write([]byte(" second")); err != nil {
		t.Fatal(err)
	}
	if got, want := output.String(), "econd"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

func setupGraphWorkflowCityWithPrivateEvidence(t *testing.T, mode string) (string, graphPrivateEvidenceTransport) {
	return setupGraphWorkflowCityWithOptions(t, mode, true)
}

func setupGraphWorkflowCityWithOptions(t *testing.T, mode string, privateEvidence bool) (string, graphPrivateEvidenceTransport) {
	t.Helper()
	gcHome, env := newIsolatedCommandEnvWithoutSupervisor(t, true)

	var cityName string
	if usingSubprocess() {
		cityName = uniqueCityName()
	} else {
		cityName = tmuxtest.NewGuard(t).CityName()
	}
	createdCityRoot, err := os.MkdirTemp("", "gc-graph-")
	if err != nil {
		t.Fatalf("create disposable graph city root: %v", err)
	}
	preserveCityRoot := false
	t.Cleanup(func() {
		if preserveCityRoot {
			return
		}
		if err := os.RemoveAll(createdCityRoot); err != nil {
			t.Errorf("remove disposable graph city root: %v", err)
		}
	})
	cityRoot, err := filepath.EvalSymlinks(createdCityRoot)
	if err != nil {
		t.Fatalf("resolve disposable graph city root: %v", err)
	}
	cityDir := filepath.Join(cityRoot, cityName)
	if filepath.Base(cityName) != cityName || !graphPathWithin(cityRoot, cityDir) {
		t.Fatalf("graph city path escaped its disposable root")
	}
	if err := os.MkdirAll(cityDir, 0o700); err != nil {
		t.Fatalf("create disposable graph city directory: %v", err)
	}
	env = prepareGraphWorkflowCityEnv(t, env, gcHome, cityDir, cityRoot)
	launcher := filepath.Join(cityRoot, "fixture-bin", "bd")
	preserveGraphEnvRoot := func() error {
		gcRoot := filepath.Dir(gcHome)
		if !filepath.IsAbs(gcRoot) || !graphPathWithin(gcRoot, gcHome) {
			return errors.New("isolated GC_HOME is not contained by its fixture root")
		}
		marker := filepath.Join(gcRoot, integrationPreserveRootMarker)
		return os.WriteFile(marker, []byte("graph fixture process shutdown was not proved\n"), 0o600)
	}
	registerCityCommandEnv(cityDir, env)
	var supervisorStarted, cityStartAttempted bool
	cleanupDone := false
	cleanupFixture := func() {
		if cleanupDone {
			return
		}
		cleanupDone = true
		var cleanupFailures []string
		if cityStartAttempted {
			if _, err := runGCDoltWithEnv(env, "", "stop", cityDir); err != nil {
				cleanupFailures = append(cleanupFailures, "gc stop")
			}
		}
		proxyRoot := filepath.Join(cityDir, ".beads", proxyendpoint.DefaultRootDirName)
		unsafeProxyRoot := false
		if info, err := os.Lstat(proxyRoot); err == nil {
			if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || !graphFixturePathIsReal(proxyRoot) {
				unsafeProxyRoot = true
				cleanupFailures = append(cleanupFailures, "proxy root is not a real fixture directory; bd stop refused")
			} else if _, stopErr := runCommand(cityDir, env, integrationGCLifecycleTimeout, launcher, "dolt", "stop"); stopErr != nil {
				cleanupFailures = append(cleanupFailures, "bd dolt stop")
			}
		} else if !os.IsNotExist(err) {
			unsafeProxyRoot = true
			cleanupFailures = append(cleanupFailures, "proxy root could not be inspected; bd stop refused")
		}
		if supervisorStarted {
			if _, err := runGCDoltWithEnv(env, "", "supervisor", "stop", "--wait"); err != nil {
				cleanupFailures = append(cleanupFailures, "gc supervisor stop")
			}
		}
		unregisterCityCommandEnv(cityDir)
		if remains, err := graphFixtureProcessesMayRemain(cityDir, gcHome); unsafeProxyRoot || remains || err != nil {
			preserveCityRoot = true
			preserveErr := preserveGraphEnvRoot()
			detail := "a fixture-owned Beads proxy or Dolt process is still present"
			if unsafeProxyRoot {
				detail = "fixture proxy root was unsafe or could not be inspected"
			} else if err != nil {
				detail = "fixture process shutdown could not be proved: " + err.Error()
			}
			if preserveErr != nil {
				detail += "; could not mark isolated GC_HOME for preservation: " + preserveErr.Error()
			}
			t.Errorf("%s; preserving disposable graph fixture roots at %s and %s (cleanup commands: %v)", detail, cityRoot, filepath.Dir(gcHome), cleanupFailures)
			return
		}
		if len(cleanupFailures) != 0 {
			t.Logf("fixture stop command(s) returned errors after process absence was independently verified: %v", cleanupFailures)
		}
		cleanupTestCityDir(cityDir)
		if _, err := os.Stat(cityDir); os.IsNotExist(err) {
			return
		}
		preserveCityRoot = true
		if err := preserveGraphEnvRoot(); err != nil {
			t.Errorf("mark isolated GC_HOME for preservation: %v", err)
		}
		beadsEntries, _ := os.ReadDir(filepath.Join(cityDir, ".beads"))
		t.Errorf("graph workflow city cleanup did not quiesce; preserving fixture roots at %s and %s with .beads entries=%v", cityRoot, filepath.Dir(gcHome), beadsEntries)
	}
	t.Cleanup(cleanupFixture)

	startCommand := "GC_GRAPH_MODE=" + mode + " bash " + agentScript("graph-dispatch.sh")
	cityToml := fmt.Sprintf(
		"[workspace]\nname = %q\n\n[session]\nprovider = \"subprocess\"\n\n[daemon]\nformula_v2 = true\npatrol_interval = \"100ms\"\n\n[[agent]]\nname = \"worker\"\nmax_active_sessions = 1\nstart_command = %q\n\n[[named_session]]\ntemplate = \"worker\"\nmode = \"always\"\n",
		cityName, startCommand,
	)
	configPath := filepath.Join(t.TempDir(), "graph-workflow.toml")
	if err := os.WriteFile(configPath, []byte(cityToml), 0o644); err != nil {
		t.Fatalf("writing graph workflow config: %v", err)
	}

	// gc init can start the BD-owned proxy even with --no-start. The cleanup
	// above is registered before this first command so partial initialization
	// cannot leave a detached proxy outside the disposable city.
	out, err := runGCDoltWithEnv(env, cityDir, "init", "--skip-provider-readiness", "--no-start", "--file", configPath, cityDir)
	if err != nil {
		t.Fatalf("gc init --file failed: %v\noutput: %s", err, out)
	}
	identity := readGraphFixtureBeadsIdentity(t, cityDir)
	if identity.database != graphFixtureCityDatabase {
		t.Fatalf("graph fixture database = %q, want the init-bound city database %q", identity.database, graphFixtureCityDatabase)
	}
	if got := writeGraphFixtureBDLauncher(t, cityRoot, cityDir, identity); got != launcher {
		t.Fatalf("graph fixture bd launcher path changed after initialization: %q", got)
	}
	env = replaceEnv(env, integrationDisposableDatabaseEnv, identity.database)
	env = replaceEnv(env, integrationDisposableProjectEnv, identity.projectID)
	registerCityCommandEnv(cityDir, env)
	startIsolatedSupervisor(t, env, gcHome)
	supervisorStarted = true
	// Register again after startIsolatedSupervisor so this cleanup stops the
	// city and its proxy before that helper's later-registered supervisor stop.
	t.Cleanup(cleanupFixture)
	cityStartAttempted = true
	startOut, startErr := runGCDoltWithEnv(env, cityDir, "start", cityDir)
	if startErr != nil {
		t.Fatalf("start isolated graph city after fixture preflight: %v\noutput: %s", startErr, startOut)
	}
	cityCommand := commandEnvForDir(cityDir, true)
	assertGraphWorkflowCityEnv(t, cityCommand, gcHome, cityDir, cityRoot)
	if _, err := waitForGraphFixtureProxyForTest(cityDir, 30*time.Second); err != nil {
		t.Fatalf("graph city did not establish a verified fixture-owned Beads proxy and Dolt listener after isolated startup: %v", err)
	}
	var transport graphPrivateEvidenceTransport
	if privateEvidence {
		transport = startGraphPrivateEvidenceService(t, env, cityDir, cityName, identity)
		cityConfigPath := filepath.Join(cityDir, "city.toml")
		contents, err := os.ReadFile(cityConfigPath)
		if err != nil {
			t.Fatalf("read initialized graph city config: %v", err)
		}
		privateEvidenceConfig := fmt.Sprintf(
			"\n[beads.private_evidence.%q]\nendpoint = %q\nproject_id = %q\ndatabase = %q\ntoken_file = %q\n",
			transport.scopeRef, transport.endpoint, transport.projectID, transport.database, transport.tokenFile,
		)
		if err := os.WriteFile(cityConfigPath, append(contents, []byte(privateEvidenceConfig)...), 0o600); err != nil {
			t.Fatalf("enable disposable graph private-evidence transport: %v", err)
		}
		reloadOut, reloadErr := runGCDoltWithEnv(env, cityDir, "reload", "--json", "--timeout", "45s", cityDir)
		if reloadErr != nil {
			t.Fatalf("reload graph city after enabling private-evidence transport: %v\noutput: %s", reloadErr, reloadOut)
		}
		var reload struct {
			SchemaVersion string `json:"schema_version"`
			OK            bool   `json:"ok"`
			Command       string `json:"command"`
			Action        string `json:"action"`
			CityPath      string `json:"city_path"`
			Outcome       string `json:"outcome"`
			Revision      string `json:"revision"`
		}
		if err := json.Unmarshal([]byte(strings.TrimSpace(reloadOut)), &reload); err != nil ||
			reload.SchemaVersion != "1" || !reload.OK || reload.Command != "reload" || reload.Action != "reload" ||
			!graphFixturePathEquals(reload.CityPath, cityDir) ||
			(reload.Outcome != "applied" && reload.Outcome != "no_change") || strings.TrimSpace(reload.Revision) == "" {
			t.Fatalf("controller did not confirm loading the private-evidence configuration: output=%q", strings.TrimSpace(reloadOut))
		}
		transport.configRevision = reload.Revision
	}
	transport.bdLauncher = launcher
	return cityDir, transport
}

func prepareGraphWorkflowCityEnv(t *testing.T, env []string, gcHome, cityDir, cityRoot string) []string {
	t.Helper()
	resolvedGCHome, err := filepath.EvalSymlinks(gcHome)
	if err != nil {
		t.Fatalf("resolve disposable graph GC_HOME: %v", err)
	}
	cityRoot, err = filepath.EvalSymlinks(cityRoot)
	if err != nil {
		t.Fatalf("resolve disposable graph city root: %v", err)
	}
	cityDir, err = filepath.EvalSymlinks(cityDir)
	if err != nil {
		t.Fatalf("resolve disposable graph city directory: %v", err)
	}
	for _, dir := range []string{
		filepath.Join(cityRoot, "bd-home"),
		filepath.Join(cityRoot, "bd-config"),
		filepath.Join(cityRoot, "bd-data"),
	} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("create disposable Beads home/config directory: %v", err)
		}
	}
	if !graphPathWithin(cityRoot, cityDir) || graphPathWithin(cityRoot, resolvedGCHome) || graphPathWithin(resolvedGCHome, cityRoot) {
		t.Fatal("graph city and controller paths are not separate disposable roots")
	}
	env = replaceEnv(env, "BEADS_DIR", filepath.Join(cityDir, ".beads"))
	env = replaceEnv(env, "XDG_CONFIG_HOME", filepath.Join(cityRoot, "bd-config"))
	env = replaceEnv(env, "XDG_DATA_HOME", filepath.Join(cityRoot, "bd-data"))
	env = replaceEnv(env, integrationDisposableBeadsDirEnv, filepath.Join(cityDir, ".beads"))
	env = replaceEnv(env, integrationDisposableCityDirEnv, cityDir)
	env = replaceEnv(env, integrationDisposableDatabaseEnv, graphFixtureCityDatabase)
	env = replaceEnv(env, integrationRequireDisposableEnv, "1")
	launcher := writeGraphFixtureBDLauncher(t, cityRoot, cityDir, graphBeadsIdentity{database: graphFixtureCityDatabase})
	values := parseEnvList(env)
	env = replaceEnv(env, "PATH", prependPath(filepath.Dir(launcher), values["PATH"]))
	assertGraphWorkflowCityEnv(t, env, parseEnvList(env)["GC_HOME"], cityDir, cityRoot)
	return env
}

func assertGraphWorkflowCityEnv(t *testing.T, env []string, gcHome, cityDir, cityRoot string) {
	t.Helper()
	values := parseEnvList(env)
	wantBeadsDir := filepath.Join(cityDir, ".beads")
	if values["GC_HOME"] != gcHome || values["DOLT_ROOT_PATH"] != gcHome {
		t.Fatal("graph fixture GC_HOME and Dolt root must match its disposable controller root")
	}
	if values["BEADS_DIR"] != wantBeadsDir || values[integrationDisposableBeadsDirEnv] != wantBeadsDir {
		t.Fatal("graph fixture Beads path must match its exact disposable city")
	}
	if values[integrationRequireDisposableEnv] != "1" || values[integrationDisposableCityDirEnv] != cityDir {
		t.Fatal("graph fixture did not require the launcher's bound disposable city target")
	}
	for _, name := range integrationGitRepositoryVars {
		if _, ok := values[name]; ok {
			t.Fatalf("graph fixture environment contains %s", name)
		}
	}
	for _, name := range integrationBeadsSelectorVars {
		if _, ok := values[name]; ok {
			t.Fatalf("graph fixture init environment contains ambient Beads selector %s", name)
		}
	}
	for _, name := range []string{"XDG_CONFIG_HOME", "XDG_DATA_HOME"} {
		if !filepath.IsAbs(values[name]) || !graphPathWithin(cityRoot, values[name]) {
			t.Fatalf("graph fixture %s is outside its disposable city root", name)
		}
	}
	if !filepath.IsAbs(gcHome) || !filepath.IsAbs(cityDir) || !filepath.IsAbs(cityRoot) || !graphPathWithin(cityRoot, cityDir) {
		t.Fatal("graph fixture paths must be absolute and contained by their disposable city root")
	}
}

func readGraphFixtureBeadsIdentity(t *testing.T, cityDir string) graphBeadsIdentity {
	t.Helper()
	metadataPath := filepath.Join(cityDir, ".beads", "metadata.json")
	info, err := os.Lstat(metadataPath)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		t.Fatal("graph fixture did not create a regular Beads identity file inside its disposable city")
	}
	data, err := os.ReadFile(metadataPath)
	if err != nil {
		t.Fatal("read graph fixture Beads identity")
	}
	var metadata struct {
		ProjectID    string `json:"project_id"`
		DoltDatabase string `json:"dolt_database"`
	}
	if err := json.Unmarshal(data, &metadata); err != nil || strings.TrimSpace(metadata.ProjectID) == "" || strings.TrimSpace(metadata.DoltDatabase) == "" {
		t.Fatal("graph fixture Beads identity is malformed or incomplete")
	}
	return graphBeadsIdentity{projectID: strings.TrimSpace(metadata.ProjectID), database: strings.TrimSpace(metadata.DoltDatabase)}
}

func writeGraphFixtureBDLauncher(t *testing.T, cityRoot, cityDir string, identity graphBeadsIdentity) string {
	t.Helper()
	binDir := filepath.Join(cityRoot, "fixture-bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatalf("create graph fixture bd launcher directory: %v", err)
	}
	launcher := filepath.Join(binDir, "bd")
	beadsDir := filepath.Join(cityDir, ".beads")
	proxyRoot := filepath.Join(beadsDir, proxyendpoint.DefaultRootDirName)
	bdHome := filepath.Join(cityRoot, "bd-home")
	bdConfig := filepath.Join(cityRoot, "bd-config")
	bdData := filepath.Join(cityRoot, "bd-data")
	authorityArg, err := integrationFixtureAuthorityArg(cityDir, identity)
	if err != nil {
		t.Fatalf("encode graph fixture bd launcher target: %v", err)
	}
	content := "#!/bin/sh\n"
	for _, check := range []struct{ name, value string }{
		{"BEADS_DIR", beadsDir},
		{integrationDisposableBeadsDirEnv, beadsDir},
		{integrationDisposableCityDirEnv, cityDir},
		{integrationDisposableDatabaseEnv, identity.database},
		{integrationDisposableProjectEnv, identity.projectID},
		{"BEADS_PROXIED_SERVER_ROOT_PATH", proxyRoot},
		{"BEADS_DOLT_DATA_DIR", proxyRoot},
		{"BEADS_DOLT_DATABASE", identity.database},
		{"BEADS_PROXIED_SERVER_CONFIG", filepath.Join(proxyRoot, "config.yaml")},
		{"BEADS_PROXIED_SERVER_LOG", filepath.Join(proxyRoot, "server.log")},
	} {
		if check.value == "" {
			content += "if [ -n \"${" + check.name + ":-}\" ]; then exit 1; fi\n"
		} else {
			content += "if [ -n \"${" + check.name + ":-}\" ] && [ \"${" + check.name + "}\" != " + singleQuoteShell(check.value) + " ]; then exit 1; fi\n"
		}
	}
	for _, name := range []string{
		"BEADS_SHARED_SERVER_DIR", "BEADS_DOLT_SERVER_DATABASE", "BEADS_DOLT_SERVER_SOCKET",
		"BEADS_DOLT_SERVER_HOST", "BEADS_DOLT_SERVER_PORT", "BEADS_DOLT_SERVER_USER", "BEADS_DOLT_SERVER_PASSWORD",
		"BEADS_DOLT_SERVER_TLS", "BEADS_DOLT_REMOTESAPI_PORT", "BEADS_DOLT_CREDENTIAL_COMMAND",
		"BEADS_DOLT_HOST", "BEADS_DOLT_PORT", "BEADS_DOLT_SOCKET", "BEADS_DOLT_USER", "BEADS_DOLT_PASSWORD",
		"BEADS_PROXIED_SERVER_PORT", "BEADS_PROXIED_SERVER_EXTERNAL_HOST", "BEADS_PROXIED_SERVER_EXTERNAL_PORT", "BEADS_PROXIED_SERVER_EXTERNAL_SOCKET_PATH",
		"BD_DB", "BEADS_DB", "BEADS_CENTRAL_CONFIG",
		"GC_BEADS_PROXY_EXTERNAL_HOST", "GC_BEADS_PROXY_EXTERNAL_PORT", "GC_BEADS_PROXY_EXTERNAL_SOCKET",
		"GC_DOLT_HOST", "GC_DOLT_PORT", "GC_DOLT_USER", "GC_DOLT_PASSWORD", "GC_DOLT_DATA_DIR",
		"GC_DOLT_DATABASE", "GC_DOLT_CONFIG_FILE", "GC_DOLT_LOG_FILE", "GC_DOLT_PID_FILE", "GC_DOLT_LOCK_FILE", "GC_DOLT_STATE_FILE",
	} {
		content += "if [ -n \"${" + name + ":-}\" ]; then exit 1; fi\n"
	}
	content += "if [ -n \"${BEADS_DOLT_SHARED_SERVER:-}\" ] && [ \"$BEADS_DOLT_SHARED_SERVER\" != '0' ] && [ \"$BEADS_DOLT_SHARED_SERVER\" != 'false' ]; then exit 1; fi\n"
	content += "if [ -n \"${BEADS_DOLT_SERVER_MODE:-}\" ] && [ \"$BEADS_DOLT_SERVER_MODE\" != '0' ] && [ \"$BEADS_DOLT_SERVER_MODE\" != 'false' ]; then exit 1; fi\n"
	content += "if [ -n \"${GC_BEADS_TRANSPORT:-}\" ] && [ \"$GC_BEADS_TRANSPORT\" != 'proxied' ]; then exit 1; fi\n"
	content += "if [ -n \"${GC_BEADS_TARGET:-}\" ] && [ \"$GC_BEADS_TARGET\" != 'local' ]; then exit 1; fi\n"
	content += "if [ -n \"${GC_BEADS_BACKEND:-}\" ] && [ \"$GC_BEADS_BACKEND\" != 'dolt' ]; then exit 1; fi\n"
	content += "if [ -n \"${BEADS_BACKEND:-}\" ] && [ \"$BEADS_BACKEND\" != 'dolt' ]; then exit 1; fi\n"
	content += "if [ -n \"${BEADS_DOLT_PROXIED_SERVER:-}\" ] && [ \"$BEADS_DOLT_PROXIED_SERVER\" != '1' ]; then exit 1; fi\n"
	content += "if [ -n \"${GC_BEADS:-}\" ] && [ \"$GC_BEADS\" != 'bd' ]; then exit 1; fi\n"
	content += "unset BEADS_DIR GC_SESSION_NAME GC_AGENT\n" +
		"export GC_INTEGRATION_REQUIRE_DISPOSABLE_BEADS_DIR='1'\n" +
		"export GC_INTEGRATION_DISPOSABLE_BEADS_DIR=" + singleQuoteShell(beadsDir) + "\n" +
		"export GC_INTEGRATION_DISPOSABLE_CITY_DIR=" + singleQuoteShell(cityDir) + "\n" +
		"export GC_INTEGRATION_DISPOSABLE_DATABASE=" + singleQuoteShell(identity.database) + "\n" +
		"export GC_INTEGRATION_DISPOSABLE_PROJECT_ID=" + singleQuoteShell(identity.projectID) + "\n" +
		"export BEADS_DIR=" + singleQuoteShell(beadsDir) + "\n" +
		"export BEADS_DOLT_DATABASE=" + singleQuoteShell(identity.database) + "\n" +
		"export BEADS_PROXIED_SERVER_ROOT_PATH=" + singleQuoteShell(proxyRoot) + "\n" +
		"export BEADS_DOLT_DATA_DIR=" + singleQuoteShell(proxyRoot) + "\n" +
		"export BEADS_DOLT_SHARED_SERVER='0'\n" +
		"export BEADS_SHARED_SERVER_DIR=''\n" +
		"export BEADS_DOLT_SERVER_MODE=''\n" +
		"export BEADS_DOLT_SERVER_SOCKET=''\n" +
		"export BEADS_DOLT_SERVER_DATABASE=''\n" +
		"export BEADS_DOLT_SERVER_HOST=''\n" +
		"export BEADS_DOLT_SERVER_PORT=''\n" +
		"export BEADS_PROXIED_SERVER_CONFIG=''\n" +
		"export BEADS_PROXIED_SERVER_LOG=''\n" +
		"export BEADS_DOLT_PROXIED_SERVER='1'\n" +
		"export GC_DOLT_HOST='' GC_DOLT_PORT='' BEADS_DOLT_HOST='' BEADS_DOLT_PORT=''\n" +
		"export HOME=" + singleQuoteShell(bdHome) + "\n" +
		"export XDG_CONFIG_HOME=" + singleQuoteShell(bdConfig) + "\n" +
		"export XDG_DATA_HOME=" + singleQuoteShell(bdData) + "\n" +
		"export GC_BEADS='bd'\n" +
		"export GC_INTEGRATION_REAL_BD=" + singleQuoteShell(realBDBinary) + "\n" +
		"exec " + singleQuoteShell(bdBinary) + " " + singleQuoteShell(authorityArg) + ` "$@"` + "\n"
	if err := os.WriteFile(launcher, []byte(content), 0o700); err != nil {
		t.Fatalf("write graph fixture bd launcher: %v", err)
	}
	if err := os.Chmod(launcher, 0o700); err != nil {
		t.Fatalf("protect graph fixture bd launcher: %v", err)
	}
	return launcher
}

func graphPathWithin(root, path string) bool {
	root, rootErr := filepath.Abs(root)
	path, pathErr := filepath.Abs(path)
	if rootErr != nil || pathErr != nil {
		return false
	}
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	if err != nil || filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false
	}
	return true
}

// graphFixtureProxyPortForTest accepts only the BD-owned proxied lifecycle
// initialized for this graph city. The proxy record proves which supervisor
// owns the root and identifies the proxy listener. The child pidfile and /proc
// listener ownership independently prove the Dolt backend listener in that
// same root; those two listeners intentionally have distinct ports.
func graphFixtureProxyPortForTest(cityDir string) (string, bool) {
	port, err := graphFixtureProxyPortForTestWithDiagnostic(cityDir)
	return port, err == nil
}

func waitForGraphFixtureProxyForTest(cityDir string, timeout time.Duration) (string, error) {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	timeoutTimer := time.NewTimer(timeout)
	defer timeoutTimer.Stop()
	for {
		port, err := graphFixtureProxyPortForTestWithDiagnostic(cityDir)
		if err == nil {
			return port, nil
		}
		select {
		case <-timeoutTimer.C:
			return "", fmt.Errorf("timed out after %s; last ownership check: %w", timeout, err)
		case <-ticker.C:
		}
	}
}

func graphFixtureProxyPortForTestWithDiagnostic(cityDir string) (string, error) {
	if runtime.GOOS != "linux" || cityDir == "" || !filepath.IsAbs(cityDir) {
		return "", errors.New("proof requires an absolute city path on Linux")
	}
	cityDir = filepath.Clean(cityDir)
	resolvedCity, err := filepath.EvalSymlinks(cityDir)
	if err != nil || filepath.Clean(resolvedCity) != cityDir {
		return "", errors.New("city path is missing or resolves through a symlink")
	}
	beadsDir := filepath.Join(cityDir, ".beads")
	root := filepath.Join(beadsDir, proxyendpoint.DefaultRootDirName)
	if !graphFixturePathIsReal(root) {
		return "", errors.New("proxy root is absent or not a real fixture directory")
	}
	metadataData, ok := graphReadFixtureRegularFile(filepath.Join(beadsDir, proxyendpoint.MetadataFileName), 1<<20)
	if !ok {
		return "", errors.New("Beads metadata is absent or not a regular file")
	}
	var metadata struct {
		Backend      string `json:"backend"`
		DoltMode     string `json:"dolt_mode"`
		DoltDatabase string `json:"dolt_database"`
		ProjectID    string `json:"project_id"`
		DoltDataDir  string `json:"dolt_data_dir"`
	}
	if err := json.Unmarshal(metadataData, &metadata); err != nil {
		return "", fmt.Errorf("decode Beads metadata: %w", err)
	}
	if metadata.Backend != "dolt" || metadata.DoltMode != "proxied-server" || metadata.DoltDatabase != graphFixtureCityDatabase || strings.TrimSpace(metadata.ProjectID) == "" {
		return "", fmt.Errorf("Beads metadata does not describe the fixture's proxied Dolt database (backend=%q mode=%q database=%q project_id_present=%t)", metadata.Backend, metadata.DoltMode, metadata.DoltDatabase, strings.TrimSpace(metadata.ProjectID) != "")
	}
	dataDir := root
	if metadata.DoltDataDir != "" {
		dataDir = metadata.DoltDataDir
		if !filepath.IsAbs(dataDir) {
			dataDir = filepath.Join(beadsDir, dataDir)
		}
	}
	if !graphFixturePathEquals(dataDir, root) {
		return "", errors.New("Dolt data directory does not match the fixture proxy root")
	}
	sidecar, err := proxyendpoint.ReadSidecar(beadsDir)
	if err != nil || !sidecar.Present {
		if err != nil {
			return "", fmt.Errorf("read proxy endpoint sidecar: %w", err)
		}
		return "", errors.New("proxy endpoint sidecar has not been published")
	}
	if sidecar.RootPath != "" && !graphFixturePathEquals(sidecar.ResolvedRootPath(beadsDir), root) {
		return "", errors.New("proxy endpoint sidecar names a different root")
	}
	configPath := filepath.Join(root, proxyendpoint.ConfigFileName)
	if sidecar.ConfigPath != "" {
		resolved := sidecar.ConfigPath
		if !filepath.IsAbs(resolved) {
			resolved = filepath.Join(beadsDir, resolved)
		}
		if !graphFixturePathEquals(resolved, configPath) {
			return "", errors.New("proxy endpoint sidecar names a different config")
		}
	}
	if sidecar.LogPath != "" {
		resolved := sidecar.LogPath
		if !filepath.IsAbs(resolved) {
			resolved = filepath.Join(beadsDir, resolved)
		}
		if !graphFixturePathEquals(resolved, filepath.Join(root, "server.log")) {
			return "", errors.New("proxy endpoint sidecar names a different log")
		}
	}
	if _, ok := graphReadFixtureRegularFile(configPath, 1<<20); !ok {
		return "", errors.New("proxy config is absent or not a regular file")
	}
	if _, ok := graphReadFixtureRegularFile(proxyendpoint.PIDPath(root), 16<<10); !ok {
		return "", errors.New("proxy PID record is absent or not a regular file")
	}
	endpoint := proxyendpoint.Inspect(root, proxyendpoint.DefaultProcessTable())
	if endpoint.Verdict != proxyendpoint.VerdictLive || endpoint.Liveness.Evidence != proxyendpoint.EvidenceArgvBirth ||
		endpoint.Record.Kind != proxyendpoint.RecordKind || endpoint.Record.Port <= 0 || endpoint.Record.Port > 65535 {
		return "", fmt.Errorf("proxy endpoint is not verified live (verdict=%s evidence=%s record_kind=%q port=%d)", endpoint.Verdict, endpoint.Liveness.Evidence, endpoint.Record.Kind, endpoint.Record.Port)
	}
	proxyRootArg, proxyRootFound, proxyRootValid := graphProcessFlag(endpoint.Liveness.Argv, proxyendpoint.RootFlag)
	if !proxyRootFound || !proxyRootValid || !graphFixturePathEquals(proxyRootArg, root) || !proxyendpoint.ArgvNamesRoot(endpoint.Liveness.Argv, root) {
		return "", errors.New("verified proxy process argv does not name the fixture root exactly once")
	}
	childData, ok := graphReadFixtureRegularFile(filepath.Join(root, graphProxyChildPIDFileName), 16<<10)
	if !ok {
		return "", errors.New("Dolt child PID record has not been published")
	}
	var child graphProxyProcessRecord
	if err := json.Unmarshal(childData, &child); err != nil {
		return "", fmt.Errorf("decode Dolt child PID record: %w", err)
	}
	if child.PID <= 0 || child.Port <= 0 || child.Port > 65535 ||
		child.Kind != graphDoltBackendRecordKind || child.Schema < proxyendpoint.SchemaV2 || child.Birth == "" || child.RootID != endpoint.RootID {
		return "", fmt.Errorf(
			"Dolt child identity is invalid or belongs to another proxy root (pid=%d backend_port=%d kind=%q schema=%d birth_present=%t root_id_matches=%t)",
			child.PID, child.Port, child.Kind, child.Schema, child.Birth != "", child.RootID == endpoint.RootID,
		)
	}
	processes := proxyendpoint.DefaultProcessTable()
	if !processes.Alive(child.PID) {
		return "", errors.New("Dolt child process is not alive")
	}
	childBirth, err := processes.Birth(child.PID)
	if err != nil {
		return "", fmt.Errorf("read Dolt child process birth identity: %w", err)
	}
	if childBirth != child.Birth {
		return "", errors.New("Dolt child process birth identity does not match its PID record")
	}
	argv, err := processes.Argv(child.PID)
	if err != nil {
		return "", fmt.Errorf("read Dolt child process argv: %w", err)
	}
	if !graphDoltChildCommandMatches(child.PID, argv, configPath, root, child.Port) {
		return "", errors.New("Dolt child process argv does not match the fixture config and root")
	}
	configData, ok := graphReadFixtureRegularFile(configPath, 1<<20)
	if !ok {
		return "", errors.New("proxy config is absent or not a regular file")
	}
	if !graphProxyConfigMatches(configData, child.Port) {
		return "", errors.New("proxy config does not publish the verified Dolt child port")
	}
	if !graphProcessOwnsListeningPort(endpoint.Record.PID, endpoint.Record.Port) {
		return "", errors.New("verified proxy process does not own its published listener")
	}
	if !graphProcessOwnsListeningPort(child.PID, child.Port) {
		return "", errors.New("Dolt child does not own the listening socket on its backend port")
	}
	port := strconv.Itoa(endpoint.Record.Port)
	if !testPortReachable(port) {
		return "", errors.New("published Dolt listener is not reachable on loopback")
	}
	return port, nil
}

// graphFixtureProcessesMayRemain is a cleanup-only proof. The city root is
// removed only after bd's endpoint record is absent/dead and a process-table
// scan finds neither a proxy nor a Dolt child whose argv names this exact
// disposable root. Ambiguous endpoint evidence preserves the root for review.
func graphFixtureProcessesMayRemain(cityDir, gcHome string) (bool, error) {
	if runtime.GOOS != "linux" || cityDir == "" || gcHome == "" || !filepath.IsAbs(cityDir) || !filepath.IsAbs(gcHome) {
		return true, errors.New("fixture process shutdown requires absolute Linux city and controller paths")
	}
	root := filepath.Join(filepath.Clean(cityDir), ".beads", proxyendpoint.DefaultRootDirName)
	endpoint := proxyendpoint.Inspect(root, proxyendpoint.DefaultProcessTable())
	switch endpoint.Verdict {
	case proxyendpoint.VerdictNoRecord, proxyendpoint.VerdictDead:
	case proxyendpoint.VerdictLive:
		return true, nil
	default:
		return true, fmt.Errorf("proxy endpoint state is %s", endpoint.Verdict)
	}
	processes := proxyendpoint.DefaultProcessTable()
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return true, fmt.Errorf("read Linux process table: %w", err)
	}
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 0 || !entry.IsDir() {
			continue
		}
		argv, err := processes.Argv(pid)
		if err != nil {
			continue // the process exited while the bounded snapshot was read
		}
		if proxyendpoint.ArgvRunsChild(argv) && graphCleanupArgvNamesProxyRoot(argv, root) {
			return true, nil
		}
		if graphDoltProcessUsesFixtureRoot(pid, argv, root) {
			return true, nil
		}
		if graphBeadsServiceUsesFixtureCity(pid, argv, cityDir) {
			return true, nil
		}
		if graphSupervisorUsesFixtureHome(pid, argv, gcHome) {
			return true, nil
		}
	}
	return false, nil
}

func graphDoltProcessUsesFixtureRoot(pid int, argv []string, root string) bool {
	if pid <= 0 || len(argv) < 2 || argv[1] != "sql-server" {
		return false
	}
	executable, err := os.Readlink(filepath.Join("/proc", strconv.Itoa(pid), "exe"))
	if err != nil || strings.TrimSuffix(filepath.Base(executable), " (deleted)") != "dolt" {
		return false
	}
	cwd, err := os.Readlink(filepath.Join("/proc", strconv.Itoa(pid), "cwd"))
	if err != nil || !graphCleanupPathEquals(cwd, root) {
		return false
	}
	if !graphCleanupArgvMentionsPath(argv, "--config", filepath.Join(root, proxyendpoint.ConfigFileName)) {
		return false
	}
	return true
}

func graphBeadsServiceUsesFixtureCity(pid int, argv []string, cityDir string) bool {
	if pid <= 0 || len(argv) < 2 || argv[1] != "serve" {
		return false
	}
	executable, err := os.Readlink(filepath.Join("/proc", strconv.Itoa(pid), "exe"))
	if err != nil || strings.TrimSuffix(filepath.Base(executable), " (deleted)") != "bd" {
		return false
	}
	cwd, err := os.Readlink(filepath.Join("/proc", strconv.Itoa(pid), "cwd"))
	if err != nil || !graphCleanupPathEquals(cwd, cityDir) {
		return false
	}
	return graphCleanupArgvMentionsPath(argv, "--auth-token-file", filepath.Join(filepath.Dir(cityDir), "controller-token"))
}

func graphSupervisorUsesFixtureHome(pid int, argv []string, gcHome string) bool {
	if pid <= 0 || len(argv) < 3 || argv[1] != "supervisor" || argv[2] != "run" {
		return false
	}
	executable, err := os.Readlink(filepath.Join("/proc", strconv.Itoa(pid), "exe"))
	if err != nil || strings.TrimSuffix(filepath.Base(executable), " (deleted)") != "gc" {
		return false
	}
	cwd, err := os.Readlink(filepath.Join("/proc", strconv.Itoa(pid), "cwd"))
	return err == nil && graphCleanupPathEquals(cwd, gcHome)
}

func graphCleanupPathEquals(path, expected string) bool {
	if !filepath.IsAbs(path) || !filepath.IsAbs(expected) {
		return false
	}
	path = strings.TrimSuffix(path, " (deleted)")
	return filepath.Clean(path) == filepath.Clean(expected)
}

func graphCleanupArgvMentionsPath(argv []string, flag, expected string) bool {
	for i, arg := range argv {
		if arg == flag && i+1 < len(argv) && graphCleanupPathEquals(argv[i+1], expected) {
			return true
		}
		if strings.HasPrefix(arg, flag+"=") && graphCleanupPathEquals(strings.TrimPrefix(arg, flag+"="), expected) {
			return true
		}
	}
	return false
}

func graphCleanupArgvNamesProxyRoot(argv []string, root string) bool {
	return graphCleanupArgvMentionsPath(argv, proxyendpoint.RootFlag, root)
}

const (
	graphProxyChildPIDFileName = "proxy-child.pid"
	graphDoltBackendRecordKind = "dolt-backend"
)

type graphProxyProcessRecord struct {
	PID    int    `json:"pid"`
	Port   int    `json:"port"`
	Schema int    `json:"schema"`
	Kind   string `json:"kind"`
	Birth  string `json:"birth"`
	RootID string `json:"root_id"`
}

func graphReadFixtureRegularFile(path string, maxSize int64) ([]byte, bool) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() < 0 || info.Size() > maxSize {
		return nil, false
	}
	data, err := os.ReadFile(path)
	return data, err == nil && int64(len(data)) <= maxSize
}

func graphFixturePathIsReal(path string) bool {
	if !filepath.IsAbs(path) {
		return false
	}
	resolved, err := filepath.EvalSymlinks(filepath.Clean(path))
	return err == nil && filepath.Clean(resolved) == filepath.Clean(path)
}

func graphFixturePathEquals(path, expected string) bool {
	if !filepath.IsAbs(path) || !filepath.IsAbs(expected) || filepath.Clean(path) != filepath.Clean(expected) {
		return false
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return false
	}
	return filepath.Clean(resolved) == filepath.Clean(expected)
}

func graphProcessFlag(args []string, flag string) (value string, found, valid bool) {
	valid = true
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == flag {
			if found || i+1 >= len(args) || args[i+1] == "" {
				return "", found, false
			}
			found = true
			value = args[i+1]
			i++
			continue
		}
		if strings.HasPrefix(arg, flag+"=") {
			if found || strings.TrimPrefix(arg, flag+"=") == "" {
				return "", found, false
			}
			found = true
			value = strings.TrimPrefix(arg, flag+"=")
		}
	}
	return value, found, true
}

func graphDoltChildCommandMatches(pid int, args []string, configPath, root string, port int) bool {
	if pid <= 0 || len(args) < 2 || filepath.Base(args[0]) != "dolt" || args[1] != "sql-server" {
		return false
	}
	executable, err := os.Readlink(filepath.Join("/proc", strconv.Itoa(pid), "exe"))
	if err != nil || strings.TrimSuffix(filepath.Base(executable), " (deleted)") != "dolt" {
		return false
	}
	cwd, err := os.Readlink(filepath.Join("/proc", strconv.Itoa(pid), "cwd"))
	if err != nil || !graphFixturePathEquals(cwd, root) {
		return false
	}
	config, found, valid := graphProcessFlag(args, "--config")
	if !found || !valid || !graphFixturePathEquals(config, configPath) {
		return false
	}
	for _, expected := range []struct {
		flag  string
		value string
	}{{"--host", proxyendpoint.Host}, {"--port", strconv.Itoa(port)}, {"--data-dir", root}} {
		value, found, valid := graphProcessFlag(args, expected.flag)
		if !valid || found && value != expected.value {
			return false
		}
	}
	return true
}

func graphProxyConfigMatches(contents []byte, port int) bool {
	var config struct {
		Listener struct {
			Host   string `yaml:"host"`
			Port   int    `yaml:"port"`
			Socket string `yaml:"socket"`
		} `yaml:"listener"`
	}
	if err := yaml.Unmarshal(contents, &config); err != nil {
		return false
	}
	return config.Listener.Host == proxyendpoint.Host && config.Listener.Port == port && config.Listener.Socket == ""
}

func graphProcessOwnsListeningPort(pid, port int) bool {
	if runtime.GOOS != "linux" || pid <= 0 || port < 1 || port > 65535 {
		return false
	}
	data, err := os.ReadFile("/proc/net/tcp")
	if err != nil {
		return false
	}
	wantLocal := fmt.Sprintf("0100007F:%04X", port)
	var inode string
	for _, line := range strings.Split(string(data), "\n")[1:] {
		fields := strings.Fields(line)
		if len(fields) < 10 || fields[1] != wantLocal || fields[3] != "0A" {
			continue
		}
		if inode != "" && inode != fields[9] {
			return false
		}
		inode = fields[9]
	}
	if inode == "" {
		return false
	}
	wanted := "socket:[" + inode + "]"
	fds, err := os.ReadDir(filepath.Join("/proc", strconv.Itoa(pid), "fd"))
	if err != nil {
		return false
	}
	for _, fd := range fds {
		target, err := os.Readlink(filepath.Join("/proc", strconv.Itoa(pid), "fd", fd.Name()))
		if err == nil && target == wanted {
			return true
		}
	}
	return false
}

func startGraphPrivateEvidenceService(t *testing.T, cityEnv []string, cityDir, cityName string, expected graphBeadsIdentity) graphPrivateEvidenceTransport {
	t.Helper()
	cityRoot := filepath.Dir(cityDir)
	cityValues := parseEnvList(cityEnv)
	assertGraphWorkflowCityEnv(t, cityEnv, cityValues["GC_HOME"], cityDir, cityRoot)
	if actual := readGraphFixtureBeadsIdentity(t, cityDir); actual != expected {
		t.Fatal("graph fixture Beads identity changed before service startup")
	}
	wantBeadsDir := filepath.Join(cityDir, ".beads")
	port, err := reserveLoopbackPort()
	if err != nil {
		t.Fatalf("reserve graph Beads HTTP port: %v", err)
	}
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		t.Fatalf("generate disposable graph controller token: %v", err)
	}
	token := fmt.Sprintf("%x", tokenBytes)
	tokenFile := filepath.Join(cityRoot, "controller-token")
	if err := os.WriteFile(tokenFile, []byte(token+"\n"), 0o600); err != nil {
		t.Fatalf("write disposable graph controller token: %v", err)
	}
	if err := os.Chmod(tokenFile, 0o600); err != nil {
		t.Fatalf("protect disposable graph controller token: %v", err)
	}
	proxyRoot := filepath.Join(wantBeadsDir, proxyendpoint.DefaultRootDirName)
	serviceEnv := []string{
		"HOME=" + filepath.Join(cityRoot, "bd-home"),
		"XDG_CONFIG_HOME=" + filepath.Join(cityRoot, "bd-config"),
		"XDG_DATA_HOME=" + filepath.Join(cityRoot, "bd-data"),
		"BEADS_DIR=" + wantBeadsDir,
		"BEADS_PROXIED_SERVER_ROOT_PATH=" + proxyRoot,
		"BEADS_DOLT_DATA_DIR=" + proxyRoot,
		"BEADS_DOLT_SHARED_SERVER=0",
		"BEADS_SHARED_SERVER_DIR=",
		"BEADS_DOLT_PROXIED_SERVER=1",
		"BEADS_DOLT_SERVER_MODE=",
		"BEADS_DOLT_SERVER_SOCKET=",
		"BEADS_PROXIED_SERVER_CONFIG=",
		"BEADS_PROXIED_SERVER_LOG=",
		"BEADS_DOLT_DATABASE=" + expected.database,
		integrationDisposableBeadsDirEnv + "=" + wantBeadsDir,
		"BEADS_DOLT_AUTO_START=0",
		"BD_EXPORT_AUTO=false",
		"GC_BEADS_TRANSPORT=proxied",
		"GC_BEADS_TARGET=local",
		"PATH=" + cityValues["PATH"],
		"GOMAXPROCS=2",
	}
	for _, dir := range []string{filepath.Join(cityRoot, "bd-home"), filepath.Join(cityRoot, "bd-config"), filepath.Join(cityRoot, "bd-data")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("create isolated Beads service config directory: %v", err)
		}
	}
	serviceValues := parseEnvList(serviceEnv)
	for _, name := range []string{"HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "BEADS_DIR", "BEADS_PROXIED_SERVER_ROOT_PATH", "BEADS_DOLT_DATA_DIR", integrationDisposableBeadsDirEnv} {
		if !filepath.IsAbs(serviceValues[name]) {
			t.Fatalf("graph Beads service %s must be an absolute fixture path", name)
		}
	}
	if serviceValues["BEADS_DIR"] != wantBeadsDir || serviceValues[integrationDisposableBeadsDirEnv] != wantBeadsDir ||
		serviceValues["BEADS_PROXIED_SERVER_ROOT_PATH"] != proxyRoot || serviceValues["BEADS_DOLT_DATA_DIR"] != proxyRoot {
		t.Fatal("graph Beads service environment does not match its disposable proxied root")
	}
	for _, name := range integrationGitRepositoryVars {
		if _, ok := serviceValues[name]; ok {
			t.Fatalf("graph Beads service environment contains %s", name)
		}
	}
	for _, name := range []string{"HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME"} {
		if !graphPathWithin(cityRoot, serviceValues[name]) {
			t.Fatalf("graph Beads service %s escaped its disposable fixture root", name)
		}
	}
	if _, ok := graphFixtureProxyPortForTest(cityDir); !ok {
		t.Fatal("graph Beads service has no verified fixture-owned proxied Dolt listener")
	}
	for _, name := range []string{
		"GC_DOLT_HOST", "GC_DOLT_PORT", "BEADS_DOLT_HOST", "BEADS_DOLT_PORT",
		"BEADS_DOLT_SERVER_HOST", "BEADS_DOLT_SERVER_PORT", "BEADS_DOLT_SERVER_DATABASE", "BEADS_DOLT_SERVER_SOCKET",
		"GC_BEADS_PROXY_EXTERNAL_HOST", "GC_BEADS_PROXY_EXTERNAL_PORT", "GC_BEADS_PROXY_EXTERNAL_SOCKET",
	} {
		if value := serviceValues[name]; value != "" {
			t.Fatalf("graph Beads service has an unexpected endpoint override %s", name)
		}
	}
	// bd serve resolves its project context from Git. Give the disposable city
	// an explicitly bounded repository instead of letting Git discover the
	// ambient checkout (the service environment intentionally strips Git vars).
	if output, err := runCommand(cityDir, serviceEnv, 10*time.Second, "git", "init", "--quiet", cityDir); err != nil {
		t.Fatalf("initialize private-evidence fixture repository: %v\noutput: %s", err, output)
	}
	root, err := runCommand(cityDir, serviceEnv, 10*time.Second, "git", "rev-parse", "--show-toplevel")
	if err != nil || filepath.Clean(strings.TrimSpace(root)) != filepath.Clean(cityDir) {
		t.Fatalf("private-evidence fixture repository root = %q, want %q (err=%v)", strings.TrimSpace(root), cityDir, err)
	}
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	serviceCtx, stopService := context.WithCancel(context.Background())
	cmd := buildCommand(serviceCtx, cityDir, serviceEnv, realBDBinary, "serve", "--addr", addr, "--auth-token-file", tokenFile)
	serviceStdout := &boundedTailBuffer{max: 8 << 10}
	serviceStderr := &boundedTailBuffer{max: 8 << 10}
	cmd.Stdout = serviceStdout
	cmd.Stderr = serviceStderr
	if err := cmd.Start(); err != nil {
		stopService()
		t.Fatalf("start installed Beads service for graph city: %v", err)
	}
	done := make(chan struct{})
	var processErr error
	go func() {
		processErr = cmd.Wait()
		close(done)
	}()
	t.Cleanup(func() {
		select {
		case <-done:
			stopService()
			return
		default:
		}
		stopService()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Errorf("installed Beads service did not exit after kill; fixture root will be preserved if its process remains")
		}
	})
	diagnosticOutput := func(output *boundedTailBuffer) string {
		text := output.String()
		text = strings.TrimSpace(text)
		if token != "" {
			text = strings.ReplaceAll(text, token, "[redacted]")
		}
		return text
	}

	endpoint := "http://" + addr
	client := &http.Client{Timeout: time.Second, Transport: &http.Transport{Proxy: nil}}
	readyCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var loadedIdentity graphBeadsIdentity
	_, err = pollUntilContext(readyCtx, "installed Beads authenticated context endpoint", 100*time.Millisecond,
		func(ctx context.Context) (bool, string, error) {
			select {
			case <-done:
				diagnostics := fmt.Sprintf("stdout=%q stderr=%q", diagnosticOutput(serviceStdout), diagnosticOutput(serviceStderr))
				if processErr != nil {
					return false, "service process exited", fmt.Errorf("installed Beads service exited before graph readiness: %w; %s", processErr, diagnostics)
				}
				return false, "service process exited", fmt.Errorf("installed Beads service exited before graph readiness; %s", diagnostics)
			default:
			}
			request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"/v0/beads/context", nil)
			if err != nil {
				return false, "context request could not be formed", errors.New("could not form graph context request")
			}
			request.Header.Set("Authorization", "Bearer "+token)
			response, err := client.Do(request)
			if err != nil {
				return false, "context endpoint is not ready", nil
			}
			var observed struct {
				ProjectID string `json:"project_id"`
				Database  string `json:"database"`
			}
			decodeErr := json.NewDecoder(response.Body).Decode(&observed)
			closeErr := response.Body.Close()
			if closeErr != nil {
				return false, "context response did not close cleanly", errors.New("could not close graph context response")
			}
			if response.StatusCode != http.StatusOK || decodeErr != nil {
				return false, fmt.Sprintf("context endpoint returned HTTP %d or malformed JSON", response.StatusCode), nil
			}
			if observed.ProjectID != expected.projectID || observed.Database != expected.database {
				return false, "context identity disagrees with fixture metadata", errors.New("installed Beads service identity disagrees with fixture metadata")
			}
			loadedIdentity = graphBeadsIdentity{projectID: observed.ProjectID, database: observed.Database}
			return true, "authenticated context matches fixture project and database", nil
		})
	if err != nil {
		t.Fatalf("installed Beads service did not expose its authenticated graph context: %v", err)
	}
	return graphPrivateEvidenceTransport{
		endpoint: endpoint, projectID: loadedIdentity.projectID, database: loadedIdentity.database,
		scopeRef: "city:" + cityName, tokenFile: tokenFile, token: token,
	}
}

func listGraphRetryControlsOverHTTP(t *testing.T, transport graphPrivateEvidenceTransport) []graphBead {
	t.Helper()
	var controls []graphBead
	for _, bead := range listGraphIssuesOverHTTP(t, transport) {
		if metaValue(bead, beadmeta.KindMetadataKey) == "retry" {
			controls = append(controls, bead)
		}
	}
	return controls
}

func listGraphIssuesOverHTTP(t *testing.T, transport graphPrivateEvidenceTransport) []graphBead {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	issues, err := readGraphIssuesOverHTTP(ctx, transport)
	if err != nil {
		t.Fatal(err)
	}
	return issues
}

func readGraphIssuesOverHTTP(ctx context.Context, transport graphPrivateEvidenceTransport) ([]graphBead, error) {
	query := url.Values{
		"all":               {"true"},
		"include_templates": {"true"},
		"include_gates":     {"true"},
		"include_infra":     {"true"},
		"limit":             {"100"},
	}
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{Proxy: nil}}
	var issues []graphBead
	cursor := ""
	for pageNumber := 0; pageNumber < 16; pageNumber++ {
		if cursor != "" {
			query.Set("cursor", cursor)
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, transport.endpoint+"/v0/beads/issues?"+query.Encode(), nil)
		if err != nil {
			return nil, errors.New("could not form graph issue-list request")
		}
		request.Header.Set("Authorization", "Bearer "+transport.token)
		request.Header.Set("Bd-Project-Id", transport.projectID)
		response, err := client.Do(request)
		if err != nil {
			return nil, errors.New("could not read graph issues through installed service")
		}
		var page struct {
			Items      []graphBead `json:"items"`
			HasMore    bool        `json:"has_more"`
			NextCursor string      `json:"next_cursor"`
		}
		decodeErr := json.NewDecoder(response.Body).Decode(&page)
		closeErr := response.Body.Close()
		if decodeErr == nil && closeErr != nil {
			decodeErr = closeErr
		}
		if response.StatusCode != http.StatusOK || decodeErr != nil {
			return nil, fmt.Errorf("installed service graph issue-list response was invalid (HTTP %d)", response.StatusCode)
		}
		for _, item := range page.Items {
			for key := range item.Metadata {
				if isGraphPrivateEvidenceMetadataKey(key) {
					delete(item.Metadata, key)
				}
			}
			issues = append(issues, item)
		}
		if !page.HasMore {
			return issues, nil
		}
		if strings.TrimSpace(page.NextCursor) == "" {
			return nil, errors.New("installed service graph issue list omitted its next cursor")
		}
		cursor = page.NextCursor
	}
	return nil, errors.New("installed service graph issue list exceeded the test page bound")
}

func findGraphWorkflowBeadByRefSuffixOverHTTP(t *testing.T, transport graphPrivateEvidenceTransport, workflowID, suffix string) graphBead {
	t.Helper()
	for _, bead := range listGraphIssuesOverHTTP(t, transport) {
		ref := bead.Ref
		if ref == "" {
			ref = metaValue(bead, "gc.step_ref")
		}
		if metaValue(bead, "gc.root_bead_id") == workflowID && strings.HasSuffix(ref, suffix) {
			return bead
		}
	}
	t.Fatalf("no graph issue with ref suffix %s found for workflow %s", suffix, workflowID)
	return graphBead{}
}

func waitForBeadClosedWithoutPrivatePayloadDiagnostics(t *testing.T, cityDir string, transport graphPrivateEvidenceTransport, beadID string, timeout time.Duration) graphBead {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var closed graphBead
	_, err := pollUntilContext(ctx, "private-evidence workflow close state", 250*time.Millisecond,
		func(ctx context.Context) (bool, string, error) {
			bead, err := readGraphBeadOverHTTPContext(ctx, transport, beadID)
			if err != nil {
				// The error contains only a generic transport/protocol message or
				// HTTP status. Preserve it as the last observation so CI can
				// distinguish a missing route/issue from an unreachable service.
				return false, "workflow bead read is unavailable: " + err.Error(), nil
			}
			if bead.Status == "closed" {
				closed = bead
				return true, "workflow bead is closed", nil
			}
			return false, "workflow bead status is " + bead.Status, nil
		})
	if err != nil {
		t.Fatalf("timed out waiting for workflow bead %s to close: %v; control trace:\n%s", beadID, err, readGraphWorkflowTraceFiles(cityDir))
	}
	return closed
}

func readGraphBeadOverHTTP(t *testing.T, transport graphPrivateEvidenceTransport, beadID string) (graphBead, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return readGraphBeadOverHTTPContext(ctx, transport, beadID)
}

func readGraphBeadOverHTTPContext(ctx context.Context, transport graphPrivateEvidenceTransport, beadID string) (graphBead, error) {
	endpoint := transport.endpoint + "/v0/beads/issues/" + url.PathEscape(beadID)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return graphBead{}, errors.New("could not form graph issue-read request")
	}
	request.Header.Set("Authorization", "Bearer "+transport.token)
	request.Header.Set("Bd-Project-Id", transport.projectID)
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{Proxy: nil}}
	response, err := client.Do(request)
	if err != nil {
		return graphBead{}, errors.New("could not read graph issue through installed service")
	}
	if response.StatusCode != http.StatusOK {
		if err := response.Body.Close(); err != nil {
			return graphBead{}, errors.New("could not close graph issue-read response")
		}
		return graphBead{}, fmt.Errorf("installed service returned HTTP %d", response.StatusCode)
	}
	var bead graphBead
	decodeErr := json.NewDecoder(response.Body).Decode(&bead)
	closeErr := response.Body.Close()
	if decodeErr != nil {
		return graphBead{}, errors.New("installed service returned malformed issue JSON")
	}
	if closeErr != nil {
		return graphBead{}, errors.New("could not close graph issue-read response")
	}
	for key := range bead.Metadata {
		if isGraphPrivateEvidenceMetadataKey(key) {
			delete(bead.Metadata, key)
		}
	}
	return bead, nil
}

func isGraphPrivateEvidenceMetadataKey(key string) bool {
	return key == beadmeta.AttemptEvidenceArchiveAttemptIDMetadataKey ||
		key == beadmeta.AttemptEvidenceArchiveOwnerIDMetadataKey ||
		key == beadmeta.AttemptEvidenceArchivePayloadMetadataKey ||
		key == beadmeta.AttemptEvidenceArchiveDigestMetadataKey ||
		strings.HasPrefix(key, beadmeta.AttemptEvidenceIndexPrefix)
}

func startScopedWorkflow(t *testing.T, cityDir string) (string, string) {
	return startScopedWorkflowWithLookup(t, cityDir, nil)
}

func startScopedWorkflowWithPrivateEvidenceHTTP(t *testing.T, cityDir string, transport graphPrivateEvidenceTransport) (string, string) {
	return startScopedWorkflowWithLookup(t, cityDir, &transport)
}

func startScopedWorkflowWithLookup(t *testing.T, cityDir string, transport *graphPrivateEvidenceTransport) (string, string) {
	t.Helper()

	out, err := bdDolt(cityDir, "create", "--json", "Run built-in scoped workflow part one")
	if err != nil {
		t.Fatalf("bd create failed: %v\noutput: %s", err, out)
	}
	var first graphBead
	if err := json.Unmarshal([]byte(strings.TrimSpace(extractJSONPayload(out))), &first); err != nil {
		t.Fatalf("unmarshal first issue: %v\njson: %s", err, out)
	}
	if first.ID == "" {
		t.Fatalf("bd create returned empty first issue id\njson: %s", out)
	}

	out, err = bdDolt(cityDir, "create", "--json", "Run built-in scoped workflow part two")
	if err != nil {
		t.Fatalf("bd create failed: %v\noutput: %s", err, out)
	}
	var second graphBead
	if err := json.Unmarshal([]byte(strings.TrimSpace(extractJSONPayload(out))), &second); err != nil {
		t.Fatalf("unmarshal second issue: %v\njson: %s", err, out)
	}
	if second.ID == "" {
		t.Fatalf("bd create returned empty second issue id\njson: %s", out)
	}

	out, err = gcDolt(cityDir, "convoy", "create", "Run built-in scoped workflow", first.ID, second.ID, "--json")
	if err != nil {
		t.Fatalf("gc convoy create failed: %v\noutput: %s", err, out)
	}
	var created graphConvoyCreateResult
	if err := json.Unmarshal([]byte(strings.TrimSpace(extractJSONPayload(out))), &created); err != nil {
		t.Fatalf("unmarshal created convoy: %v\njson: %s", err, out)
	}
	convoyID := created.ConvoyID
	if convoyID == "" {
		t.Fatalf("gc convoy create returned empty convoy id\njson: %s", out)
	}

	out, err = gcDolt(cityDir, "sling", "worker", convoyID, "--on=mol-scoped-work")
	if err != nil {
		t.Fatalf("gc sling failed: %v\noutput: %s", err, out)
	}
	slingOutput := out

	var workflowID string
	if transport == nil {
		workflowID = waitForGraphWorkflowRootForInputConvoy(t, cityDir, convoyID, slingOutput, 10*time.Second)
	} else {
		workflowID = waitForGraphWorkflowRootForInputConvoyOverHTTP(t, cityDir, *transport, convoyID, 10*time.Second)
	}
	return convoyID, workflowID
}

func waitForGraphWorkflowRootForInputConvoyOverHTTP(t *testing.T, cityDir string, transport graphPrivateEvidenceTransport, convoyID string, timeout time.Duration) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var workflowID string
	_, err := pollUntilContext(ctx, "installed Beads workflow-root issue list", 200*time.Millisecond,
		func(ctx context.Context) (bool, string, error) {
			beads, err := readGraphIssuesOverHTTP(ctx, transport)
			if err != nil {
				return false, "workflow root is not listed yet", nil
			}
			for _, bead := range beads {
				if metaValue(bead, "gc.kind") == "workflow" && metaValue(bead, "gc.input_convoy_id") == convoyID {
					workflowID = bead.ID
					return true, "workflow root is listed", nil
				}
			}
			return false, "workflow root is not listed yet", nil
		})
	if err != nil {
		t.Fatalf("timed out waiting for graph.v2 workflow root for input convoy %s: %v; control trace:\n%s", convoyID, err, readGraphWorkflowTraceFiles(cityDir))
	}
	return workflowID
}

func waitForGraphWorkflowRootForInputConvoy(t *testing.T, cityDir, inputConvoyID, slingOutput string, timeout time.Duration) string {
	t.Helper()

	deadline := time.Now().Add(timeout)
	var lastErr error
	var lastList string
	for time.Now().Before(deadline) {
		workflowID, rawList, err := findGraphWorkflowRootForInputConvoy(cityDir, inputConvoyID)
		if err == nil && workflowID != "" {
			return workflowID
		}
		lastErr = err
		lastList = rawList
		time.Sleep(200 * time.Millisecond)
	}
	inputConvoy := showBead(t, cityDir, inputConvoyID)
	t.Fatalf("timed out waiting for graph.v2 workflow root for input convoy %s: %v\ngc sling output:\n%s\ninput convoy:\n%+v\nbd list:\n%s", inputConvoyID, lastErr, slingOutput, inputConvoy, lastList)
	return ""
}

func findGraphWorkflowRootForInputConvoy(cityDir, inputConvoyID string) (string, string, error) {
	out, err := bdDolt(cityDir, "list", "--json", "--all", "--limit=0")
	if err != nil {
		return "", out, fmt.Errorf("bd list --json failed: %w", err)
	}
	var beads []graphBead
	if err := json.Unmarshal([]byte(strings.TrimSpace(extractJSONPayload(out))), &beads); err != nil {
		return "", out, fmt.Errorf("unmarshal bead list: %w", err)
	}

	for _, bead := range beads {
		if bead.ID == inputConvoyID && beadType(bead) != "convoy" {
			return "", out, fmt.Errorf("input target %s is type %q, want convoy", inputConvoyID, beadType(bead))
		}
	}
	for _, bead := range beads {
		if metaValue(bead, "gc.kind") != "workflow" {
			continue
		}
		if metaValue(bead, "gc.input_convoy_id") == inputConvoyID {
			return bead.ID, out, nil
		}
	}
	return "", out, fmt.Errorf("no workflow root found for input convoy %s", inputConvoyID)
}

func waitForBeadClosed(t *testing.T, cityDir, beadID string, timeout time.Duration) graphBead {
	t.Helper()

	var waitErr error
	if bead, err := waitForBeadCondition(t, cityDir, beadID, timeout, func(bead graphBead) bool {
		return bead.Status == "closed"
	}); err == nil {
		return bead
	} else {
		waitErr = err
		t.Logf("waitForBeadClosed(%s) ended with %v; collecting diagnostics", beadID, err)
	}

	out, err := bdDolt(cityDir, "list", "--json", "--all", "--limit=0")
	if err != nil {
		t.Fatalf("timed out waiting for bead %s to close; bd list failed: %v\noutput: %s", beadID, err, out)
	}
	readyOut, readyErr := bdDolt(cityDir, "ready", "--json", "--limit=0")
	if readyErr != nil {
		readyOut = fmt.Sprintf("bd ready failed: %v\noutput: %s", readyErr, readyOut)
	}
	readyAssigneeOut, readyAssigneeErr := bdDolt(cityDir, "ready", "--json", "--limit=0", "--assignee=worker")
	if readyAssigneeErr != nil {
		readyAssigneeOut = fmt.Sprintf("bd ready --assignee failed: %v\noutput: %s", readyAssigneeErr, readyAssigneeOut)
	}
	sessionListOut, sessionListErr := gcDolt(cityDir, "session", "list")
	if sessionListErr != nil {
		sessionListOut = fmt.Sprintf("gc session list failed: %v\noutput: %s", sessionListErr, sessionListOut)
	}
	sessionPeekOut, sessionPeekErr := gcDolt(cityDir, "session", "peek", "worker")
	if sessionPeekErr != nil {
		sessionPeekOut = fmt.Sprintf("gc session peek worker failed: %v\noutput: %s", sessionPeekErr, sessionPeekOut)
	}
	traceOut := readOptionalFile(filepath.Join(cityDir, "graph-workflow-trace.log"))
	workflowTraceOut := readGraphWorkflowTraceFiles(cityDir)
	t.Fatalf("waiting for bead %s to close failed: %v\nready:\n%s\nready worker:\n%s\nsessions:\n%s\nworker peek:\n%s\ntrace:\n%s\nworkflow trace:\n%s\nbeads:\n%s",
		beadID, waitErr, readyOut, readyAssigneeOut, sessionListOut, sessionPeekOut, traceOut, workflowTraceOut, out)
	return graphBead{}
}

func readGraphWorkflowTraceFiles(cityDir string) string {
	tracePaths := []string{
		citylayout.ControlDispatcherTraceDefaultPath(cityDir),
		citylayout.ControlDispatcherTraceDefaultPathFor(cityDir, "core.control-dispatcher"),
	}
	var traces []string
	for _, path := range tracePaths {
		traces = append(traces, fmt.Sprintf("%s:\n%s", path, readOptionalFile(path)))
	}
	return strings.Join(traces, "\n\n")
}

func readOptionalFile(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Sprintf("read %s failed: %v", path, err)
	}
	return string(data)
}

func showBead(t *testing.T, cityDir, beadID string) graphBead {
	t.Helper()

	bead, err := tryShowBead(cityDir, beadID)
	if err != nil {
		t.Fatal(err)
	}
	return bead
}

func tryShowBead(cityDir, beadID string) (graphBead, error) {
	out, err := bdDolt(cityDir, "show", beadID, "--json")
	if err != nil {
		return graphBead{}, fmt.Errorf("bd show --json %s failed: %v\noutput: %s", beadID, err, out)
	}
	var bead graphBead
	trimmed := strings.TrimSpace(extractJSONPayload(out))
	if err := json.Unmarshal([]byte(trimmed), &bead); err == nil {
		return bead, nil
	}
	var beads []graphBead
	if err := json.Unmarshal([]byte(trimmed), &beads); err != nil {
		return graphBead{}, fmt.Errorf("unmarshal bead %s: %v\njson: %s", beadID, err, out)
	}
	if len(beads) == 0 {
		return graphBead{}, fmt.Errorf("bd show --json %s returned no beads\njson: %s", beadID, out)
	}
	return beads[0], nil
}

func mustFindWorkflowBeadByRefSuffix(t *testing.T, cityDir, workflowID, suffix string) graphBead {
	t.Helper()

	out, err := bdDolt(cityDir, "list", "--json", "--all", "--limit=0")
	if err != nil {
		t.Fatalf("bd list --json failed: %v\noutput: %s", err, out)
	}
	var beads []graphBead
	if err := json.Unmarshal([]byte(strings.TrimSpace(extractJSONPayload(out))), &beads); err != nil {
		t.Fatalf("unmarshal bead list: %v\njson: %s", err, out)
	}
	for _, bead := range beads {
		ref := bead.Ref
		if ref == "" {
			ref = metaValue(bead, "gc.step_ref")
		}
		if metaValue(bead, "gc.root_bead_id") == workflowID && strings.HasSuffix(ref, suffix) {
			return bead
		}
	}
	t.Fatalf("no bead with ref suffix %s found for workflow %s", suffix, workflowID)
	return graphBead{}
}

func extractJSONPayload(raw string) string {
	data := []byte(raw)
	for i, b := range data {
		if b != '{' && b != '[' {
			continue
		}
		candidate := bytes.TrimSpace(data[i:])
		if json.Valid(candidate) {
			return string(candidate)
		}
	}
	return raw
}

func readWorkflowReport(t *testing.T, cityDir string) string {
	t.Helper()

	path := filepath.Join(cityDir, "graph-workflow-steps.log")
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil && len(strings.TrimSpace(string(data))) > 0 {
			return string(data)
		}
		time.Sleep(100 * time.Millisecond)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading workflow report: %v", err)
	}
	return string(data)
}
