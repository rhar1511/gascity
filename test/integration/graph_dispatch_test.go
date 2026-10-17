//go:build integration

package integration

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/attemptevidence"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/proxyendpoint"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/pathutil"
	"github.com/gastownhall/gascity/test/integration/runisolation"
	"github.com/gastownhall/gascity/test/tmuxtest"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

	workflow := waitForWorkflowAndTeardown(t, cityDir, transport, workflowID, graphWorkflowCloseTimeout())
	if got := metaValue(workflow, "gc.outcome"); got != "pass" {
		t.Fatalf("workflow outcome = %q, want pass; fixture diagnostics:\n%s", got, graphWorkflowFailedOutcomeDiagnostics(cityDir, transport, workflow))
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
	var sealedRetries, repositorySnapshots int
	for _, control := range controls {
		attempts, err := attemptevidence.List(beadStore, control.ID)
		if err != nil {
			t.Fatalf("read retry evidence for control %s: %v", control.ID, err)
		}
		for _, attempt := range attempts {
			if attempt.Identity.Kind != attemptevidence.KindRetry {
				continue
			}
			compact, err := attemptevidence.ReadCompact(beadStore, control.ID, attempt.AttemptID)
			if err != nil {
				t.Fatalf("compact retry evidence readback for control %s failed: %v", control.ID, err)
			}
			readback, err := attemptevidence.Read(beadStore, control.ID, attempt.AttemptID)
			if err != nil {
				t.Fatalf("hydrated retry evidence readback for control %s failed: %v", control.ID, err)
			}
			if err := validateGraphAttemptReadback(attempt, compact, readback); err != nil {
				t.Fatalf("exact retry evidence readback for control %s failed: %v", control.ID, err)
			}
			if attempt.CandidateStatus == attemptevidence.StatusAvailable {
				workspace := attempt.Permission.WorkspaceRoot
				// This is the recorded identity of an already-removed workspace.
				// The fixture city path is canonical; resolving the deleted leaf
				// here would reject successful teardown.
				implementationWorkspace := workspace == filepath.Join(cityDir, "worktrees", convoyID)
				if len(attempt.CandidateSHA) != 40 || (!implementationWorkspace && workspace != cityDir) {
					t.Fatalf("retry evidence has the wrong committed workspace identity: candidate=%q workspace=%q", attempt.CandidateSHA, workspace)
				}
				if implementationWorkspace {
					repositorySnapshots++
				}
			}
			sealedRetries++
		}
	}
	if sealedRetries == 0 {
		t.Fatal("graph workflow passed without an exact retry-attempt evidence archive")
	}
	if repositorySnapshots == 0 {
		t.Fatal("graph workflow passed without committed attempt workspace evidence")
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
	cityDir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve test-owned city directory: %v", err)
	}
	beadsDir := filepath.Join(cityDir, ".beads")
	if err := os.MkdirAll(beadsDir, 0o700); err != nil {
		t.Fatalf("create fixture Beads directory: %v", err)
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen on disposable loopback port: %v", err)
	}
	t.Cleanup(func() {
		if err := listener.Close(); err != nil {
			t.Errorf("close disposable loopback listener: %v", err)
		}
	})
	port := strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)
	if !graphProcessOwnsListeningPort(os.Getpid(), listener.Addr().(*net.TCPAddr).Port) {
		t.Fatal("native process proof did not recognize this process's loopback listener")
	}
	if graphProcessOwnsListeningPort(os.Getppid(), listener.Addr().(*net.TCPAddr).Port) {
		t.Fatal("native process proof accepted the listener's parent as its owner")
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "dolt-server.port"), []byte(port+"\n"), 0o600); err != nil {
		t.Fatalf("write reachable port mirror: %v", err)
	}
	if got, ok := currentManagedDoltPortForTest(cityDir); !ok || got != port {
		t.Fatalf("generic port mirror probe = %q, %v; want reachable fixture port %q", got, ok, port)
	}
	if got, ok := graphFixtureProxyPortForTest(cityDir); ok {
		t.Fatalf("graph proxy proof accepted unowned reachable port %q", got)
	}
	if runtime.GOOS == "linux" || runtime.GOOS == "darwin" {
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

func TestGraphFixtureRecognizesItsRenamedSupervisorBinary(t *testing.T) {
	t.Setenv(integrationGCBinaryEnv, "")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	home, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	home, err = filepath.EvalSymlinks(home)
	if err != nil {
		t.Fatal(err)
	}
	previous := gcBinary
	gcBinary = executable
	t.Cleanup(func() { gcBinary = previous })
	argv := []string{executable, "supervisor", "run"}
	if !graphSupervisorUsesFixtureHome(os.Getpid(), argv, home) {
		t.Fatal("fixture cleanup failed to recognize its exact renamed supervisor executable")
	}
	if graphSupervisorUsesFixtureHome(os.Getpid(), argv, t.TempDir()) {
		t.Fatal("fixture cleanup accepted a supervisor in another home")
	}
	t.Run("pinned executable behind exec shim", func(t *testing.T) {
		t.Setenv(integrationGCBinaryEnv, executable)
		gcBinary = filepath.Join(t.TempDir(), "gc")
		if err := writeExecShim(gcBinary, executable); err != nil {
			t.Fatal(err)
		}
		if !graphSupervisorUsesFixtureHome(os.Getpid(), argv, home) {
			t.Fatal("fixture cleanup failed to recognize the pinned executable behind its shim")
		}
	})
	t.Run("different executable", func(t *testing.T) {
		t.Setenv(integrationGCBinaryEnv, "")
		gcBinary = filepath.Join(t.TempDir(), "unrelated-gc")
		if graphSupervisorUsesFixtureHome(os.Getpid(), argv, home) {
			t.Fatal("fixture cleanup accepted another executable")
		}
	})
}

func TestGraphFixtureCanonicalizesControllerHome(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "controller")
	cityRoot := filepath.Join(root, "city-root")
	cityDir := filepath.Join(cityRoot, "city")
	for _, dir := range []string{home, cityDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	alias := filepath.Join(root, "controller-alias")
	if err := os.Symlink(home, alias); err != nil {
		t.Fatal(err)
	}
	want, err := filepath.EvalSymlinks(home)
	if err != nil {
		t.Fatal(err)
	}
	env := prepareGraphWorkflowCityEnv(t, []string{"GC_HOME=" + alias, "DOLT_ROOT_PATH=" + alias}, alias, cityDir, cityRoot)
	values := parseEnvList(env)
	if values["GC_HOME"] != want || values["DOLT_ROOT_PATH"] != want {
		t.Fatal("fixture home identity differs from the native process working directory")
	}
}

func TestGraphFixtureBDLauncherAcceptsOnlyBoundPathSpellings(t *testing.T) {
	root := t.TempDir()
	physicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	cityRoot := filepath.Join(physicalRoot, "fixture")
	cityDir := filepath.Join(cityRoot, "city")
	if err := os.MkdirAll(filepath.Join(cityDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	aliasRoot := filepath.Join(physicalRoot, "fixture-alias")
	otherAlias := filepath.Join(physicalRoot, "unbound-alias")
	for _, alias := range []string{aliasRoot, otherAlias} {
		if err := os.Symlink(cityRoot, alias); err != nil {
			t.Fatal(err)
		}
	}
	boundCity := filepath.Join(aliasRoot, "city")
	beadsDir := filepath.Join(boundCity, ".beads")
	proxyRoot := filepath.Join(beadsDir, proxyendpoint.DefaultRootDirName)
	recorder := filepath.Join(physicalRoot, "record-bd")
	if err := os.WriteFile(recorder, []byte("#!/bin/sh\nprintf '%s\\n%s\\n' \"$BEADS_DIR\" \"$GC_INTEGRATION_DISPOSABLE_CITY_DIR\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	previousBD, previousRealBD := bdBinary, realBDBinary
	bdBinary, realBDBinary = recorder, recorder
	t.Cleanup(func() { bdBinary, realBDBinary = previousBD, previousRealBD })
	launcher := writeGraphFixtureBDLauncher(t, aliasRoot, boundCity, graphBeadsIdentity{database: "hq", projectID: "fixture-project"})
	for _, tc := range []struct {
		name, key, value string
		wantPass         bool
	}{
		{"bound spelling", "BEADS_DIR", beadsDir, true},
		{"GC canonical spelling", "BEADS_DIR", pathutil.NormalizePathForCompare(beadsDir), true},
		{"GC canonical city", integrationDisposableCityDirEnv, pathutil.NormalizePathForCompare(boundCity), true},
		{"GC canonical proxy root", "BEADS_PROXIED_SERVER_ROOT_PATH", pathutil.NormalizePathForCompare(proxyRoot), true},
		{"foreign path", "BEADS_DIR", filepath.Join(physicalRoot, "foreign", ".beads"), false},
		{"unbound alias", "BEADS_DIR", filepath.Join(otherAlias, "city", ".beads"), false},
		{"foreign database", "BEADS_DOLT_DATABASE", "other", false},
		{"foreign project", integrationDisposableProjectEnv, "other", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := runCommand(physicalRoot, []string{"PATH=/usr/bin:/bin", tc.key + "=" + tc.value}, 10*time.Second, launcher, "ping")
			if tc.wantPass {
				if err != nil {
					t.Fatalf("bound fixture path refused: %v: %s", err, out)
				}
				if want := beadsDir + "\n" + boundCity + "\n"; out != want {
					t.Fatalf("launcher changed bound output tuple: got %q, want %q", out, want)
				}
			} else if err == nil || out != "" {
				t.Fatalf("conflicting input reached recorder: err=%v out=%q", err, out)
			}
		})
	}
}

func assertGraphFixturePinsBoundBDLauncher(t *testing.T, cityDir string) {
	t.Helper()
	cfg, err := config.Load(fsys.OSFS{}, filepath.Join(cityDir, "city.toml"))
	if err != nil {
		t.Fatalf("load graph fixture config: %v", err)
	}
	want := filepath.Join(filepath.Dir(cityDir), "fixture-bin", "bd")
	if got := cfg.Workspace.Env["BD_BIN"]; got != want {
		t.Fatalf("graph fixture workspace BD_BIN = %q, want bound launcher %q", got, want)
	}
	info, err := os.Lstat(want)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o111 == 0 {
		t.Fatal("graph fixture bd pin must be a regular executable owned by its fixture")
	}
	if resolved, err := filepath.EvalSymlinks(want); err != nil || resolved != want || !graphPathWithin(filepath.Dir(cityDir), want) {
		t.Fatal("graph fixture bd pin must retain its exact physical fixture path")
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

func TestGraphFixtureListenerOwnershipAllowsSeparateProxyAndDoltPorts(t *testing.T) {
	const (
		proxyPID  = 101
		proxyPort = 33001
		doltPID   = 202
		doltPort  = 33002
	)
	owned := map[int]int{proxyPID: proxyPort, doltPID: doltPort}
	owns := func(pid, port int) bool { return owned[pid] == port }

	if !graphFixtureListenersOwnedWith(proxyPID, proxyPort, doltPID, doltPort, owns) {
		t.Fatal("fixture listener proof rejected separate proxy and Dolt listener owners")
	}
	if graphFixtureListenersOwnedWith(proxyPID, doltPort, doltPID, proxyPort, owns) {
		t.Fatal("fixture listener proof accepted swapped proxy and Dolt port ownership")
	}
	if graphFixtureListenersOwnedWith(proxyPID, 0, doltPID, doltPort, owns) {
		t.Fatal("fixture listener proof accepted an invalid proxy port")
	}
	if graphFixtureListenersOwnedWith(proxyPID, proxyPort, doltPID, 65536, owns) {
		t.Fatal("fixture listener proof accepted an invalid Dolt port")
	}
	if graphFixtureListenersOwnedWith(proxyPID, proxyPort, proxyPID, doltPort, owns) {
		t.Fatal("fixture listener proof accepted one PID for the proxy and Dolt listeners")
	}
}

func TestGraphWorkflowFailureRunsCleanup(t *testing.T) {
	cityDir, transport := setupGraphWorkflowCityWithPrivateEvidence(t, "fail-preflight")
	convoyID, workflowID := startScopedWorkflowWithPrivateEvidenceHTTP(t, cityDir, transport)

	workflow := waitForWorkflowAndTeardown(t, cityDir, transport, workflowID, graphWorkflowCloseTimeout())
	if got := metaValue(workflow, "gc.outcome"); got != "fail" {
		t.Fatalf("workflow outcome = %q, want fail", got)
	}

	body := findGraphWorkflowBeadByRefSuffixOverHTTP(t, transport, workflowID, ".body")
	if got := metaValue(body, "gc.outcome"); got != "fail" {
		t.Fatalf("body outcome = %q, want fail", got)
	}

	convoy, err := readGraphBeadOverHTTP(t, transport, convoyID)
	if err != nil {
		t.Fatalf("read graph convoy through installed service: %v", err)
	}
	if got := metaValue(convoy, "work_dir"); got != "" {
		t.Fatalf("convoy work_dir = %q, want unset after cleanup", got)
	}
	if got := metaValue(convoy, "submitted"); got != "" {
		t.Fatalf("convoy submitted = %q, want unset on failed workflow", got)
	}

	for _, suffix := range []string{".implement", ".self-review", ".submit"} {
		bead := findGraphWorkflowBeadByRefSuffixOverHTTP(t, transport, workflowID, suffix)
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

type graphPrivateEvidenceTransport struct {
	endpoint           string
	projectID          string
	database           string
	scopeRef           string
	tokenFile          string
	token              string
	configRevision     string
	bdLauncher         string
	client             *http.Client
	serviceDone        <-chan struct{}
	serviceDiagnostics func() string
}

func newGraphPrivateEvidenceHTTPClient() *http.Client {
	return &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{Proxy: nil}}
}

const graphFixtureCityDatabase = "hq"

type graphBeadsIdentity struct {
	projectID string
	database  string
}

type boundedTailBuffer struct {
	mu  sync.Mutex
	buf []byte
	max int
}

func (b *boundedTailBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
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

func (b *boundedTailBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.buf)
}

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

func TestBoundedTailBufferConcurrentReadAndWrite(t *testing.T) {
	var output boundedTailBuffer
	output.max = 128
	var workers sync.WaitGroup
	workers.Add(2)
	go func() {
		defer workers.Done()
		for range 1000 {
			_, _ = output.Write([]byte(strings.Repeat("x", 32)))
		}
	}()
	go func() {
		defer workers.Done()
		for range 1000 {
			_ = output.String()
		}
	}()
	workers.Wait()
	if got := len(output.String()); got > output.max {
		t.Fatalf("tail buffer size = %d, exceeds bound %d", got, output.max)
	}
}

func TestGraphIssueReadsReuseHTTPConnection(t *testing.T) {
	var connections atomic.Int64
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintln(w, `{"id":"gc-test","status":"open","metadata":{}}`)
	}))
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			connections.Add(1)
		}
	}
	server.Start()
	t.Cleanup(server.Close)

	client := newGraphPrivateEvidenceHTTPClient()
	t.Cleanup(client.CloseIdleConnections)
	transport := graphPrivateEvidenceTransport{
		endpoint: server.URL,
		client:   client,
		serviceDiagnostics: func() string {
			return `service=running stdout="" stderr=""`
		},
	}
	for range 4 {
		if _, err := readGraphBeadOverHTTPContext(context.Background(), transport, "gc-test"); err != nil {
			t.Fatalf("read graph issue: %v", err)
		}
	}
	if got := connections.Load(); got != 1 {
		t.Fatalf("issue reads opened %d HTTP connections, want one reused connection", got)
	}
}

func TestGraphIssueReadTimeoutReportsHTTPProgress(t *testing.T) {
	const token = "private-test-token"
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		close(started)
		<-request.Context().Done()
	}))
	t.Cleanup(server.Close)

	client := &http.Client{Timeout: 25 * time.Millisecond, Transport: &http.Transport{Proxy: nil}}
	t.Cleanup(client.CloseIdleConnections)
	transport := graphPrivateEvidenceTransport{
		endpoint: server.URL,
		client:   client,
		token:    token,
		serviceDiagnostics: func() string {
			return redactGraphPrivateEvidenceToken(`service=running stdout="" stderr="private-test-token"`, token)
		},
	}
	_, err := readGraphBeadOverHTTPContext(context.Background(), transport, "gc-test")
	if err == nil {
		t.Fatal("stalled issue read unexpectedly succeeded")
	}
	select {
	case <-started:
	default:
		t.Fatal("stalled server never received the issue request")
	}
	for _, evidence := range []string{"request failed after", "got_connection=true", "first_response_byte=false", "service=running"} {
		if !strings.Contains(err.Error(), evidence) {
			t.Fatalf("timeout diagnostic %q does not include %q", err, evidence)
		}
	}
	if strings.Contains(err.Error(), token) {
		t.Fatalf("timeout diagnostic exposed the service token: %q", err)
	}
}

func setupGraphWorkflowCityWithPrivateEvidence(t *testing.T, mode string) (string, graphPrivateEvidenceTransport) {
	t.Helper()
	return setupWorkflowCityWithPrivateEvidence(t, func(cityName string) string {
		startCommand := "GC_GRAPH_MODE=" + mode + " bash " + agentScript("graph-dispatch.sh")
		cityToml := fmt.Sprintf(
			"[workspace]\nname = %q\n\n[session]\nprovider = \"subprocess\"\n\n[orders]\nskip = [\"beads-health\", \"dolt-health\"]\n\n[daemon]\nformula_v2 = true\npatrol_interval = \"100ms\"\n\n[[agent]]\nname = \"worker\"\nmax_active_sessions = 1\nstart_command = %q\n\n[[named_session]]\ntemplate = \"worker\"\nmode = \"always\"\n",
			cityName, startCommand,
		)

		return cityToml
	}, nil)
}

// setupWorkflowCityWithPrivateEvidence binds every retry-capable fixture to
// its own stock Beads HTTP service and verifies process absence on cleanup.
func setupWorkflowCityWithPrivateEvidence(t *testing.T, config func(string) string, prepare func(string)) (string, graphPrivateEvidenceTransport) {
	t.Helper()
	gcHome, env := newIsolatedCommandEnvWithoutSupervisor(t, true)
	// Bound transient Beads probes during pooled worker recovery.
	env = append(env, "GC_BD_PROBE_TIMEOUT=30s")

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
	gcHome = parseEnvList(env)["GC_HOME"]
	launcher := filepath.Join(cityRoot, "fixture-bin", "bd")
	preserveGraphEnvRoot := func() error {
		gcRoot := filepath.Dir(gcHome)
		if !filepath.IsAbs(gcRoot) || !graphPathWithin(gcRoot, gcHome) {
			return errors.New("isolated GC_HOME is not contained by its fixture root")
		}
		marker := filepath.Join(gcRoot, runisolation.PreserveMarkerName)
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
		// Stop the controller before its store so a final reconciliation cannot
		// restart the fixture proxy while cleanup is retiring it.
		if supervisorStarted {
			if _, err := runGCDoltWithEnv(env, "", "supervisor", "stop", "--wait"); err != nil {
				cleanupFailures = append(cleanupFailures, "gc supervisor stop")
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

	cityToml := config(cityName)
	// Controller store calls must use the same authority-restoring launcher
	// as PATH-based fixture commands.
	cityToml += fmt.Sprintf("\n[workspace.env]\nBD_BIN = %q\n", launcher)
	configPath := filepath.Join(t.TempDir(), "graph-workflow.toml")
	if err := os.WriteFile(configPath, []byte(cityToml), 0o644); err != nil {
		t.Fatalf("writing graph workflow config: %v", err)
	}

	if prepare != nil {
		prepare(cityDir)
	}

	// gc init can start the BD-owned proxy even with --no-start. The cleanup
	// above is registered before this first command so partial initialization
	// cannot leave a detached proxy outside the disposable city.
	out, err := runGCDoltWithEnv(env, cityDir, "init", "--skip-provider-readiness", "--no-start", "--file", configPath, cityDir)
	if err != nil {
		t.Fatalf("gc init --file failed: %v\noutput: %s", err, out)
	}
	assertGraphFixturePinsBoundBDLauncher(t, cityDir)
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
	transport := startGraphPrivateEvidenceService(t, env, cityDir, cityName, identity)
	cityConfigPath := filepath.Join(cityDir, "city.toml")
	contents, err := os.ReadFile(cityConfigPath)
	if err != nil {
		t.Fatalf("read initialized graph city config: %v", err)
	}
	privateEvidenceConfig := fmt.Sprintf(
		"\n[beads.private_evidence.%q]\nendpoint = %q\nproject_id = %q\ndatabase = %q\ntoken_file = %q\nrevision_transitions = true\n",
		transport.scopeRef, transport.endpoint, transport.projectID, transport.database, transport.tokenFile,
	)
	if err := os.WriteFile(cityConfigPath, append(contents, []byte(privateEvidenceConfig)...), 0o600); err != nil {
		t.Fatalf("enable disposable graph private-evidence transport: %v", err)
	}
	reloadOut, reloadErr := runGCDoltWithEnv(env, cityDir, "reload", "--json", "--timeout", "45s", cityDir)
	if reloadErr != nil {
		t.Fatalf("reload graph city after enabling private-evidence transport: %v\noutput: %s\nfixture diagnostics:\n%s", reloadErr, reloadOut, readGraphFailureDiagnostics(cityDir, transport.token))
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
		!graphFixtureReloadPathEquals(reload.CityPath, cityDir) ||
		(reload.Outcome != "applied" && reload.Outcome != "no_change") || strings.TrimSpace(reload.Revision) == "" {
		t.Fatalf("controller did not confirm loading the private-evidence configuration: output=%q", strings.TrimSpace(reloadOut))
	}
	transport.configRevision = reload.Revision
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
	env = replaceEnv(env, "GC_HOME", resolvedGCHome)
	env = replaceEnv(env, "DOLT_ROOT_PATH", resolvedGCHome)
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
	for _, check := range []struct {
		name, value string
		path        bool
	}{
		{"BEADS_DIR", beadsDir, true},
		{integrationDisposableBeadsDirEnv, beadsDir, true},
		{integrationDisposableCityDirEnv, cityDir, true},
		{integrationDisposableDatabaseEnv, identity.database, false},
		{integrationDisposableProjectEnv, identity.projectID, false},
		{"BEADS_PROXIED_SERVER_ROOT_PATH", proxyRoot, true},
		{"BEADS_DOLT_DATA_DIR", proxyRoot, true},
		{"BEADS_DOLT_DATABASE", identity.database, false},
		{"BEADS_PROXIED_SERVER_CONFIG", filepath.Join(proxyRoot, "config.yaml"), true},
		{"BEADS_PROXIED_SERVER_LOG", filepath.Join(proxyRoot, "server.log"), true},
	} {
		if check.value == "" {
			content += "if [ -n \"${" + check.name + ":-}\" ]; then exit 1; fi\n"
		} else {
			condition := "[ -n \"${" + check.name + ":-}\" ] && [ \"${" + check.name + "}\" != " + singleQuoteShell(check.value) + " ]"
			if check.path {
				// GC collapses Darwin's /private alias; accept only that exact
				// spelling of this bound path. Never resolve caller-supplied paths.
				canonical := pathutil.NormalizePathForCompare(check.value)
				if canonical != check.value {
					condition += " && [ \"${" + check.name + "}\" != " + singleQuoteShell(canonical) + " ]"
				}
			}
			content += "if " + condition + "; then exit 1; fi\n"
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
// owns the root and identifies the proxy listener. The child pidfile and native
// listener ownership independently prove the Dolt backend listener in that
// same root; those two listeners intentionally have distinct ports.
func graphFixtureProxyPortForTest(cityDir string) (string, bool) {
	port, err := graphFixtureProxyPortForTestWithDiagnostic(cityDir)
	return port, err == nil
}

func waitForGraphFixtureProxyForTest(cityDir string, timeout time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var port string
	_, err := pollUntilContext(ctx, "graph fixture proxy ownership", 100*time.Millisecond,
		func(context.Context) (bool, string, error) {
			observedPort, checkErr := graphFixtureProxyPortForTestWithDiagnostic(cityDir)
			if checkErr != nil {
				return false, checkErr.Error(), nil
			}
			port = observedPort
			return true, "proxy and Dolt listeners have verified owners", nil
		})
	if err != nil {
		return "", err
	}
	return port, nil
}

func graphFixtureProxyPortForTestWithDiagnostic(cityDir string) (string, error) {
	if (runtime.GOOS != "linux" && runtime.GOOS != "darwin") || cityDir == "" || !filepath.IsAbs(cityDir) {
		return "", errors.New("proof requires an absolute city path on Linux or Darwin")
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
	endpoint := proxyendpoint.Inspect(root, graphFixtureProcessTable())
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
	if child.PID == endpoint.Record.PID {
		return "", errors.New("Dolt child and proxy must be distinct processes")
	}
	processes := graphFixtureProcessTable()
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
	if !graphFixtureListenersOwned(endpoint.Record.PID, endpoint.Record.Port, child.PID, child.Port) {
		return "", errors.New("verified proxy and Dolt child do not own their respective listening sockets")
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
	if (runtime.GOOS != "linux" && runtime.GOOS != "darwin") || cityDir == "" || gcHome == "" || !filepath.IsAbs(cityDir) || !filepath.IsAbs(gcHome) {
		return true, errors.New("fixture process shutdown requires absolute Linux or Darwin city and controller paths")
	}
	root := filepath.Join(filepath.Clean(cityDir), ".beads", proxyendpoint.DefaultRootDirName)
	endpoint := proxyendpoint.Inspect(root, graphFixtureProcessTable())
	switch endpoint.Verdict {
	case proxyendpoint.VerdictNoRecord, proxyendpoint.VerdictDead:
	case proxyendpoint.VerdictLive:
		return true, nil
	default:
		return true, fmt.Errorf("proxy endpoint state is %s", endpoint.Verdict)
	}
	processes := graphFixtureProcessTable()
	pids, err := graphFixtureProcessIDs()
	if err != nil {
		return true, fmt.Errorf("read process table: %w", err)
	}
	for _, rawPID := range pids {
		pid := int(rawPID)
		if pid <= 0 {
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
	executable, cwd, err := graphProcessExecutableAndCWD(pid)
	if err != nil || strings.TrimSuffix(filepath.Base(executable), " (deleted)") != "dolt" {
		return false
	}
	if !graphCleanupPathEquals(cwd, root) {
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
	executable, cwd, err := graphProcessExecutableAndCWD(pid)
	if err != nil || strings.TrimSuffix(filepath.Base(executable), " (deleted)") != "bd" {
		return false
	}
	if !graphCleanupPathEquals(cwd, cityDir) {
		return false
	}
	return graphCleanupArgvMentionsPath(argv, "--auth-token-file", filepath.Join(filepath.Dir(cityDir), "controller-token"))
}

func graphSupervisorUsesFixtureHome(pid int, argv []string, gcHome string) bool {
	if pid <= 0 || len(argv) < 3 || argv[1] != "supervisor" || argv[2] != "run" {
		return false
	}
	// TestMain may invoke gc through an exec shim; compare the native
	// executable to the pinned target, not the shell script used to launch it.
	expected := gcBinary
	if override, ok, err := binaryOverride(integrationGCBinaryEnv); err != nil {
		return false
	} else if ok {
		expected = override
	}
	expected, err := filepath.EvalSymlinks(expected)
	if err != nil {
		return false
	}
	executable, cwd, err := graphProcessExecutableAndCWD(pid)
	if err != nil || !graphCleanupPathEquals(executable, expected) {
		return false
	}
	return graphCleanupPathEquals(cwd, gcHome)
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

// Reload JSON names the city using GC's canonical spelling. Derive that
// spelling only from the existing real fixture root; never normalize or adopt
// an arbitrary response path. Proxy/process guards retain strict physical paths.
func graphFixtureReloadPathEquals(path, expected string) bool {
	return graphFixturePathIsReal(expected) && path == pathutil.NormalizePathForCompare(expected)
}

func TestGraphFixtureReloadPathMatchesOnlyCanonicalBoundCity(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	canonical := pathutil.NormalizePathForCompare(root)
	assert.True(t, graphFixtureReloadPathEquals(canonical, root))
	assert.True(t, graphFixturePathEquals(root, root), "physical guard unchanged")
	t.Logf("controller_alias_distinct=%t", canonical != root)
	if canonical != root {
		assert.False(t, graphFixturePathEquals(canonical, root), "original reload predicate rejects the controller alias")
	}
	alias := root + "-alias"
	require.NoError(t, os.Symlink(root, alias))
	t.Cleanup(func() { require.NoError(t, os.Remove(alias)) })
	foreign, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	for _, tc := range []struct{ name, response, expected string }{
		{"relative response", ".", root},
		{"foreign response", pathutil.NormalizePathForCompare(foreign), root},
		{"arbitrary same-inode alias", alias, root},
		{"symlink bound root", canonical, alias},
		{"missing bound root", canonical, filepath.Join(root, "missing")},
		{"relative bound root", canonical, "."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.False(t, graphFixtureReloadPathEquals(tc.response, tc.expected))
		})
	}
	assert.False(t, graphFixturePathEquals(alias, root), "strict proxy/process authority must still reject aliases")
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
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == flag {
			if found || i+1 >= len(args) || args[i+1] == "" {
				return "", false, false
			}
			found = true
			value = args[i+1]
			i++
			continue
		}
		if strings.HasPrefix(arg, flag+"=") {
			if found || strings.TrimPrefix(arg, flag+"=") == "" {
				return "", false, false
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
	executable, cwd, err := graphProcessExecutableAndCWD(pid)
	if err != nil || strings.TrimSuffix(filepath.Base(executable), " (deleted)") != "dolt" {
		return false
	}
	if !graphFixturePathEquals(cwd, root) {
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

func graphFixtureListenersOwned(proxyPID, proxyPort, doltPID, doltPort int) bool {
	return graphFixtureListenersOwnedWith(proxyPID, proxyPort, doltPID, doltPort, graphProcessOwnsListeningPort)
}

func graphFixtureListenersOwnedWith(proxyPID, proxyPort, doltPID, doltPort int, owns func(pid, port int) bool) bool {
	if proxyPID <= 0 || doltPID <= 0 || proxyPort < 1 || proxyPort > 65535 || doltPort < 1 || doltPort > 65535 {
		return false
	}
	if proxyPID == doltPID || owns == nil {
		return false
	}
	return owns(proxyPID, proxyPort) && owns(doltPID, doltPort)
}

func graphProcessOwnsListeningPort(pid, port int) bool {
	if pid <= 0 || port < 1 || port > 65535 {
		return false
	}
	if runtime.GOOS == "darwin" {
		return graphDarwinProcessOwnsListeningPort(pid, port)
	}
	if runtime.GOOS != "linux" {
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

func TestGraphPrivateEvidenceServiceEnvBindsFixtureTempAuthority(t *testing.T) {
	cityRoot := filepath.Join(string(filepath.Separator), "private", "fixture-owned")
	cityDir := filepath.Join(cityRoot, "city")
	identity := graphBeadsIdentity{projectID: "fixture-project", database: graphFixtureCityDatabase}
	for _, inheritedTemp := range []string{"", filepath.Join(string(filepath.Separator), "foreign-temp")} {
		t.Run("inherited_temp="+inheritedTemp, func(t *testing.T) {
			cityValues := map[string]string{
				"PATH":                   "/fixture-tools:/usr/bin:/bin",
				"TMPDIR":                 inheritedTemp,
				"HOME":                   "/foreign-home",
				"GC_DOLT_HOST":           "foreign.invalid",
				"BEADS_SERVE_TOKEN_FILE": "/foreign-token",
			}
			env := graphPrivateEvidenceServiceEnv(cityDir, identity, cityValues)
			values := parseEnvList(env)
			if values["TMPDIR"] != cityRoot || !filepath.IsAbs(values["TMPDIR"]) {
				t.Fatalf("service temp authority = %q, want exact owned root %q", values["TMPDIR"], cityRoot)
			}
			if !graphPathWithin(values["TMPDIR"], values["BEADS_DIR"]) || values["BEADS_DIR"] != filepath.Join(cityDir, ".beads") {
				t.Fatal("service temp authority does not contain its exact Beads directory")
			}
			if values["HOME"] != filepath.Join(cityRoot, "bd-home") || values["PATH"] != cityValues["PATH"] || values["BEADS_DOLT_DATABASE"] != identity.database {
				t.Fatal("service environment lost its owned home, selected tool path, or database")
			}
			for _, key := range []string{"GC_DOLT_HOST", "BEADS_SERVE_TOKEN_FILE"} {
				if _, ok := values[key]; ok {
					t.Fatalf("service environment inherited ambient %s", key)
				}
			}
		})
	}
}

func graphPrivateEvidenceServiceEnv(cityDir string, expected graphBeadsIdentity, cityValues map[string]string) []string {
	cityRoot := filepath.Dir(cityDir)
	wantBeadsDir := filepath.Join(cityDir, ".beads")
	proxyRoot := filepath.Join(wantBeadsDir, proxyendpoint.DefaultRootDirName)
	return []string{
		"TMPDIR=" + cityRoot,
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
	serviceEnv := graphPrivateEvidenceServiceEnv(cityDir, expected, cityValues)
	for _, dir := range []string{filepath.Join(cityRoot, "bd-home"), filepath.Join(cityRoot, "bd-config"), filepath.Join(cityRoot, "bd-data")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("create isolated Beads service config directory: %v", err)
		}
	}
	serviceValues := parseEnvList(serviceEnv)
	for _, name := range []string{"TMPDIR", "HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME", "BEADS_DIR", "BEADS_PROXIED_SERVER_ROOT_PATH", "BEADS_DOLT_DATA_DIR", integrationDisposableBeadsDirEnv} {
		if !filepath.IsAbs(serviceValues[name]) {
			t.Fatalf("graph Beads service %s must be an absolute fixture path", name)
		}
	}
	if serviceValues["TMPDIR"] != cityRoot || serviceValues["BEADS_DIR"] != wantBeadsDir || serviceValues[integrationDisposableBeadsDirEnv] != wantBeadsDir ||
		serviceValues["BEADS_PROXIED_SERVER_ROOT_PATH"] != proxyRoot || serviceValues["BEADS_DOLT_DATA_DIR"] != proxyRoot {
		t.Fatal("graph Beads service environment does not match its disposable proxied root")
	}
	for _, name := range integrationGitRepositoryVars {
		if _, ok := serviceValues[name]; ok {
			t.Fatalf("graph Beads service environment contains %s", name)
		}
	}
	for _, name := range []string{"TMPDIR", "HOME", "XDG_CONFIG_HOME", "XDG_DATA_HOME"} {
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
	initializeGraphFixtureRepository(t, cityDir, serviceEnv)
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
		return redactGraphPrivateEvidenceToken(strings.TrimSpace(output.String()), token)
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
	issueClient := newGraphPrivateEvidenceHTTPClient()
	t.Cleanup(issueClient.CloseIdleConnections)
	serviceDiagnostics := func() string {
		state := "running"
		select {
		case <-done:
			state = "exited"
		default:
		}
		return fmt.Sprintf("service=%s stdout=%q stderr=%q", state, diagnosticOutput(serviceStdout), diagnosticOutput(serviceStderr))
	}
	return graphPrivateEvidenceTransport{
		endpoint: endpoint, projectID: loadedIdentity.projectID, database: loadedIdentity.database,
		scopeRef: "city:" + cityName, tokenFile: tokenFile, token: token, client: issueClient,
		serviceDone: done, serviceDiagnostics: serviceDiagnostics,
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
	if transport.client == nil {
		return nil, errors.New("graph issue transport has no HTTP client")
	}
	query := url.Values{
		"all":               {"true"},
		"include_templates": {"true"},
		"include_gates":     {"true"},
		"include_infra":     {"true"},
		"limit":             {"100"},
	}
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
		response, err := transport.client.Do(request)
		if err != nil {
			return nil, fmt.Errorf("could not read graph issues through installed service: %s", safeGraphHTTPTransportError(err, transport.token))
		}
		var page struct {
			Items      *[]graphBead `json:"items"`
			HasMore    *bool        `json:"has_more"`
			NextCursor string       `json:"next_cursor"`
		}
		decodeErr := json.NewDecoder(response.Body).Decode(&page)
		closeErr := response.Body.Close()
		if decodeErr == nil && closeErr != nil {
			decodeErr = closeErr
		}
		if response.StatusCode != http.StatusOK || decodeErr != nil || page.Items == nil || page.HasMore == nil {
			return nil, fmt.Errorf("installed service graph issue-list response was invalid (HTTP %d)", response.StatusCode)
		}
		for _, item := range *page.Items {
			for key := range item.Metadata {
				if isGraphPrivateEvidenceMetadataKey(key) {
					delete(item.Metadata, key)
				}
			}
			issues = append(issues, item)
		}
		if !*page.HasMore {
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

func waitForWorkflowAndTeardown(t *testing.T, cityDir string, transport graphPrivateEvidenceTransport, beadID string, timeout time.Duration) graphBead {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var closed graphBead
	_, err := pollUntilContext(ctx, "private-evidence workflow close state", 250*time.Millisecond,
		func(ctx context.Context) (bool, string, error) {
			bead, err := readGraphBeadOverHTTPContext(ctx, transport, beadID)
			if err != nil {
				// The error contains token-redacted transport details or an HTTP
				// status, never the private response body. Preserve it as the last
				// observation so CI distinguishes an unreachable service from a
				// missing route or issue.
				return false, "workflow bead read is unavailable: " + err.Error(), nil
			}
			if bead.Status == "closed" {
				// A settled root releases its teardown tail. Root closure alone
				// cannot establish that cleanup has executed.
				issues, listErr := readGraphIssuesOverHTTP(ctx, transport)
				if listErr != nil {
					return false, "workflow teardown state is unavailable: " + listErr.Error(), nil
				}
				for _, issue := range issues {
					if metaValue(issue, beadmeta.RootBeadIDMetadataKey) == beadID &&
						metaValue(issue, beadmeta.ScopeRoleMetadataKey) == beadmeta.ScopeRoleTeardown && issue.Status != "closed" {
						return false, "workflow teardown " + issue.ID + " is " + issue.Status, nil
					}
				}
				closed = bead
				return true, "workflow and teardown are closed", nil
			}
			return false, "workflow bead status is " + bead.Status, nil
		})
	if err != nil {
		t.Fatalf("timed out waiting for workflow bead %s to close: %v; fixture diagnostics:\n%s", beadID, err, readGraphFailureDiagnostics(cityDir, transport.token))
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
	if transport.client == nil {
		return graphBead{}, errors.New("graph issue transport has no HTTP client")
	}
	if transport.serviceDone != nil {
		select {
		case <-transport.serviceDone:
			return graphBead{}, fmt.Errorf("installed Beads service exited before issue read: %s", graphServiceDiagnosticText(transport))
		default:
		}
	}
	endpoint := transport.endpoint + "/v0/beads/issues/" + url.PathEscape(beadID)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return graphBead{}, errors.New("could not form graph issue-read request")
	}
	request.Header.Set("Authorization", "Bearer "+transport.token)
	request.Header.Set("Bd-Project-Id", transport.projectID)
	var gotConnection, reusedConnection, firstResponseByte atomic.Bool
	trace := &httptrace.ClientTrace{
		GotConn: func(info httptrace.GotConnInfo) {
			gotConnection.Store(true)
			reusedConnection.Store(info.Reused)
		},
		GotFirstResponseByte: func() { firstResponseByte.Store(true) },
	}
	request = request.WithContext(httptrace.WithClientTrace(request.Context(), trace))
	started := time.Now()
	response, err := transport.client.Do(request)
	if err != nil {
		observation := fmt.Sprintf(
			"request failed after %s (got_connection=%t connection_reused=%t first_response_byte=%t)",
			time.Since(started).Round(time.Millisecond), gotConnection.Load(), reusedConnection.Load(), firstResponseByte.Load(),
		)
		if diagnostic := graphServiceDiagnosticText(transport); diagnostic != "" {
			observation += "; " + diagnostic
		}
		return graphBead{}, fmt.Errorf("could not read graph issue through installed service: %s; %s", safeGraphHTTPTransportError(err, transport.token), observation)
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

func graphServiceDiagnosticText(transport graphPrivateEvidenceTransport) string {
	if transport.serviceDiagnostics == nil {
		return ""
	}
	return transport.serviceDiagnostics()
}

func safeGraphHTTPTransportError(err error, token string) string {
	return redactGraphPrivateEvidenceToken(err.Error(), token)
}

func redactGraphPrivateEvidenceToken(message, token string) string {
	if token != "" {
		message = strings.ReplaceAll(message, token, "[redacted]")
	}
	return message
}

func isGraphPrivateEvidenceMetadataKey(key string) bool {
	return key == beadmeta.AttemptEvidenceArchiveAttemptIDMetadataKey ||
		key == beadmeta.AttemptEvidenceArchiveOwnerIDMetadataKey ||
		key == beadmeta.AttemptEvidenceArchivePayloadMetadataKey ||
		key == beadmeta.AttemptEvidenceArchiveDigestMetadataKey ||
		strings.HasPrefix(key, beadmeta.AttemptEvidenceIndexPrefix)
}

func startScopedWorkflowWithPrivateEvidenceHTTP(t *testing.T, cityDir string, transport graphPrivateEvidenceTransport) (string, string) {
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
	workflowID := waitForGraphWorkflowRootForInputConvoyOverHTTP(t, cityDir, transport, convoyID, 10*time.Second)
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
		t.Fatalf("timed out waiting for graph.v2 workflow root for input convoy %s: %v; fixture diagnostics:\n%s", convoyID, err, readGraphFailureDiagnostics(cityDir, transport.token))
	}
	return workflowID
}

// graphWorkflowFailedOutcomeDiagnostics runs only after a terminal outcome failed
// its assertion, while the fixture service and files still exist.
func graphWorkflowFailedOutcomeDiagnostics(cityDir string, transport graphPrivateEvidenceTransport, root graphBead) string {
	const maxOutputBytes = 256 << 10
	const maxIssueBytes = 64 << 10
	const maxIssues = 128
	const maxFieldBytes = 512
	field := func(value string) string {
		value = redactGraphPrivateEvidenceToken(value, transport.token)
		if len(value) > maxFieldBytes {
			return value[:maxFieldBytes] + "[truncated]"
		}
		return value
	}
	var out strings.Builder
	appendIssue := func(issue graphBead) {
		selected := map[string]string{"id": field(issue.ID), "ref": field(issue.Ref), "status": field(issue.Status)}
		for _, key := range []string{
			beadmeta.RootBeadIDMetadataKey, beadmeta.ScopeRoleMetadataKey,
			beadmeta.KindMetadataKey, beadmeta.StepRefMetadataKey,
			"gc.outcome", "gc.failure_class", "gc.failure_reason",
			"review.verdict", "design_review.verdict", "code_review.verdict",
		} {
			selected[key] = field(metaValue(issue, key))
		}
		// A string-only map cannot fail JSON encoding. Quoting also prevents
		// control characters in a metadata value from forging log lines.
		data, _ := json.Marshal(selected)
		out.Write(data)
		out.WriteByte('\n')
	}
	appendIssue(root)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	issues, err := readGraphIssuesOverHTTP(ctx, transport)
	if err != nil {
		fmt.Fprintf(&out, "issue verdict read unavailable: %s\n", field(err.Error()))
	} else {
		for i, issue := range issues {
			if i >= maxIssues || out.Len() >= maxIssueBytes {
				out.WriteString("[issue verdicts truncated]\n")
				break
			}
			appendIssue(issue)
		}
	}
	out.WriteString(readGraphFailureDiagnostics(cityDir, transport.token))
	// Redact before truncation so clipping cannot expose a token prefix.
	text := redactGraphPrivateEvidenceToken(out.String(), transport.token)
	if len(text) > maxOutputBytes {
		return text[:maxOutputBytes-len("\n[diagnostics truncated]")] + "\n[diagnostics truncated]"
	}
	return text
}

// readGraphFailureDiagnostics only reads fixture-owned logs after a failure.
// Keep both the number of startup logs and each log tail bounded for CI output.
func readGraphFailureDiagnostics(cityDir, token string) string {
	const maxStartupLogs = 16
	paths := []string{
		citylayout.ControlDispatcherTraceDefaultPath(cityDir),
		citylayout.ControlDispatcherTraceDefaultPathFor(cityDir, "core.control-dispatcher"),
		filepath.Join(cityDir, "graph-workflow-trace.log"),
		filepath.Join(cityDir, "graph-workflow-steps.log"),
	}
	if env, ok := cityCommandEnv.Load(cityDir); ok {
		if gcHome := parseEnvList(env.([]string))["GC_HOME"]; filepath.IsAbs(gcHome) {
			paths = append(paths, filepath.Join(gcHome, "supervisor.log"))
		}
	}
	startupLogs, err := filepath.Glob(filepath.Join(citylayout.SessionDiagnosticsDir(cityDir), "*", "start-stderr.log"))
	if err != nil {
		return redactGraphPrivateEvidenceToken(fmt.Sprintf("enumerate fixture startup logs: %v", err), token)
	}
	var diagnostics []string
	if len(startupLogs) > maxStartupLogs {
		diagnostics = append(diagnostics, fmt.Sprintf("startup logs truncated: showing %d of %d", maxStartupLogs, len(startupLogs)))
		startupLogs = startupLogs[:maxStartupLogs]
	}
	paths = append(paths, startupLogs...)
	for _, path := range paths {
		diagnostics = append(diagnostics, fmt.Sprintf("%s:\n%s", path, readGraphDiagnosticTail(path)))
	}
	return redactGraphPrivateEvidenceToken(strings.Join(diagnostics, "\n\n"), token)
}

func readGraphDiagnosticTail(path string) string {
	const maxBytes = 16 << 10
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Sprintf("inspect %s failed: %v", path, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Sprintf("refusing non-regular diagnostic file %s", path)
	}
	file, err := os.Open(path)
	if err != nil {
		return fmt.Sprintf("open %s failed: %v", path, err)
	}
	defer file.Close() //nolint:errcheck
	offset := max(int64(0), info.Size()-maxBytes)
	data, err := io.ReadAll(io.NewSectionReader(file, offset, maxBytes))
	if err != nil {
		return fmt.Sprintf("read %s failed: %v", path, err)
	}
	if offset > 0 {
		// Discard a partial first line, so token redaction never starts mid-token.
		if newline := bytes.IndexByte(data, '\n'); newline >= 0 {
			data = data[newline+1:]
		} else {
			data = nil
		}
		return "[earlier log bytes omitted]\n" + string(data)
	}
	return string(data)
}

func readOptionalFile(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Sprintf("read %s failed: %v", path, err)
	}
	return string(data)
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

func initializeGraphFixtureRepository(t *testing.T, cityDir string, serviceEnv []string) {
	t.Helper()
	if output, err := runCommand(cityDir, serviceEnv, 10*time.Second, "git", "init", "--quiet", cityDir); err != nil {
		t.Fatalf("initialize private-evidence fixture repository: %v\noutput: %s", err, output)
	}
	root, err := runCommand(cityDir, serviceEnv, 10*time.Second, "git", "rev-parse", "--show-toplevel")
	if err != nil || filepath.Clean(strings.TrimSpace(root)) != filepath.Clean(cityDir) {
		t.Fatalf("private-evidence fixture repository root = %q, want %q (err=%v)", strings.TrimSpace(root), cityDir, err)
	}
	// The city's repository is a default execution workspace, separate from
	// the simulated implementation repository. Keep mutable fixture state
	// (including its ledger) out of snapshots and never commit staged data.
	excludePath := filepath.Join(cityDir, ".git", "info", "exclude")
	excludes, err := os.ReadFile(excludePath)
	if err != nil {
		t.Fatalf("read fixture repository exclusions: %v", err)
	}
	if err := os.WriteFile(excludePath, append(excludes, []byte("\n/*\n")...), 0o600); err != nil {
		t.Fatalf("exclude mutable fixture state from repository captures: %v", err)
	}
	if output, err := runCommand(cityDir, serviceEnv, 10*time.Second, "git",
		"-c", "user.name=Graph fixture", "-c", "user.email=graph-fixture@example.invalid",
		"-c", "commit.gpgsign=false", "-c", "core.hooksPath=/dev/null",
		"commit", "--quiet", "--allow-empty", "--only", "-m", "Initialize graph city workspace"); err != nil {
		t.Fatalf("commit empty fixture workspace: %v\noutput: %s", err, output)
	}
}

// Settlement closes the root before its teardown tail becomes executable.
// The integration completion gate must observe both phases before checking
// worktree cleanup, even after a failed body.
func TestGraphWorkflowWaitIncludesTeardownAfterSettlement(t *testing.T) {
	for _, outcome := range []string{"pass", "fail"} {
		t.Run(outcome, func(t *testing.T) {
			var listCalls atomic.Int64
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/v0/beads/issues/root":
					_ = json.NewEncoder(w).Encode(graphBead{ID: "root", Status: "closed", Metadata: map[string]any{"gc.outcome": outcome}})
				case "/v0/beads/issues":
					state := "open"
					if listCalls.Add(1) >= 2 {
						state = "closed"
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"items": []graphBead{
						{ID: "cleanup", Status: state, Metadata: map[string]any{"gc.root_bead_id": "root", "gc.scope_role": "teardown", "gc.kind": "retry", "gc.outcome": "pass"}},
						{ID: "other-workflow", Status: "open", Metadata: map[string]any{"gc.root_bead_id": "other-root", "gc.scope_role": "teardown"}},
						{ID: "non-teardown", Status: "open", Metadata: map[string]any{"gc.root_bead_id": "root", "gc.scope_role": "member"}},
					}, "has_more": false})
				default:
					http.NotFound(w, r)
				}
			}))
			t.Cleanup(server.Close)
			client := newGraphPrivateEvidenceHTTPClient()
			t.Cleanup(client.CloseIdleConnections)
			transport := graphPrivateEvidenceTransport{endpoint: server.URL, client: client}
			root := waitForWorkflowAndTeardown(t, t.TempDir(), transport, "root", 2*time.Second)
			if root.Status != "closed" || metaValue(root, "gc.outcome") != outcome {
				t.Fatalf("workflow state changed: %+v", root)
			}
			if got := listCalls.Load(); got != 2 {
				t.Fatalf("teardown observations=%d, want open then closed before returning", got)
			}
		})
	}
}

func TestGraphIssueListRejectsMissingCompletionFields(t *testing.T) {
	for _, body := range []string{`{}`, `{"items":[]}`, `{"has_more":false}`, `{"items":null,"has_more":false}`} {
		t.Run(body, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, body)
			}))
			t.Cleanup(server.Close)
			client := newGraphPrivateEvidenceHTTPClient()
			t.Cleanup(client.CloseIdleConnections)
			if _, err := readGraphIssuesOverHTTP(t.Context(), graphPrivateEvidenceTransport{endpoint: server.URL, client: client}); err == nil {
				t.Fatal("incomplete issue list could falsely prove teardown completion")
			}
		})
	}
}

func TestGraphFixtureRepositoryStartsWithEmptyCommit(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	env := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + t.TempDir(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null"}
	runGit := func(args ...string) string {
		t.Helper()
		out, err := runCommand(root, env, 10*time.Second, "git", args...)
		if err != nil {
			t.Fatalf("fixture git command: %v: %s", err, out)
		}
		return strings.TrimSpace(out)
	}
	runGit("init", "--quiet")
	const privateFile = "private-fixture-state"
	if err := os.WriteFile(filepath.Join(root, privateFile), []byte("fixture-only private state\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit("add", privateFile)
	initializeGraphFixtureRepository(t, root, env)
	if err := os.WriteFile(filepath.Join(root, "runtime-artifact"), []byte("mutable fixture state"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := runGit("rev-parse", "--verify", "HEAD^{commit}"); len(got) != 40 {
		t.Fatalf("fixture HEAD=%q, want a real commit", got)
	}
	if got := runGit("ls-tree", "--name-only", "HEAD"); got != "" {
		t.Fatalf("fixture committed data: %q", got)
	}
	if got := runGit("diff", "--cached", "--name-only"); got != privateFile {
		t.Fatalf("fixture changed staged private data: %q", got)
	}
	if got := runGit("ls-files", "--others", "--exclude-standard"); got != "" {
		t.Fatalf("mutable fixture state remains in the capture workspace: %q", got)
	}
}

// Compact reads retain the manifest; full reads additionally hydrate diff
// bytes. Verify both contracts without discarding identity or status fields.
func validateGraphAttemptReadback(listed, compact, hydrated attemptevidence.Evidence) error {
	if !reflect.DeepEqual(compact, listed) {
		return errors.New("compact attempt readback differs from its listed manifest")
	}
	for _, diff := range []attemptevidence.DiffSnapshot{hydrated.Diff, hydrated.WorkspaceDiff} {
		if diff.Status == attemptevidence.StatusAvailable {
			if _, _, err := attemptevidence.DecodeDiff(diff); err != nil {
				return fmt.Errorf("hydrated attempt diff verification: %w", err)
			}
		}
	}
	hydrated.Diff.Payload = nil
	hydrated.WorkspaceDiff.Payload = nil
	if !reflect.DeepEqual(hydrated, compact) {
		return errors.New("hydrated attempt readback changed its compact manifest")
	}
	return nil
}

func TestGraphAttemptReadbackVerifiesCompactManifestAndHydratedDiffs(t *testing.T) {
	const bundle = `{"tracked_patch":"Zml4dHVyZSBwYXRjaA=="}`
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	if _, err := io.WriteString(writer, bundle); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(compressed.Bytes())
	diff := attemptevidence.DiffSnapshot{
		Status: attemptevidence.StatusAvailable, Encoding: attemptevidence.DiffEncoding,
		SHA256: fmt.Sprintf("%x", digest), UncompressedBytes: int64(len(bundle)),
	}
	listed := attemptevidence.Evidence{AttemptID: "exact-attempt", Diff: diff, WorkspaceDiff: diff}
	hydrated := listed
	hydrated.Diff.Payload = append([]byte(nil), compressed.Bytes()...)
	hydrated.WorkspaceDiff.Payload = append([]byte(nil), compressed.Bytes()...)
	if err := validateGraphAttemptReadback(listed, listed, hydrated); err != nil {
		t.Fatalf("valid hydrated diff rejected: %v", err)
	}
	for _, tc := range []struct {
		name   string
		change func(*attemptevidence.Evidence, *attemptevidence.Evidence)
	}{
		{"different compact attempt", func(c, _ *attemptevidence.Evidence) { c.AttemptID = "other-attempt" }},
		{"different hydrated owner", func(_, h *attemptevidence.Evidence) { h.Identity.OwnerBeadID = "other-owner" }},
		{"missing candidate bytes", func(_, h *attemptevidence.Evidence) { h.Diff.Payload = nil }},
		{"missing workspace bytes", func(_, h *attemptevidence.Evidence) { h.WorkspaceDiff.Payload = nil }},
		{"corrupt candidate bytes", func(_, h *attemptevidence.Evidence) { h.Diff.Payload = []byte("corrupt") }},
		{"corrupt workspace bytes", func(_, h *attemptevidence.Evidence) { h.WorkspaceDiff.Payload = []byte("corrupt") }},
		{"downgraded candidate", func(_, h *attemptevidence.Evidence) { h.Diff.Status = "unavailable" }},
		{"downgraded workspace", func(_, h *attemptevidence.Evidence) { h.WorkspaceDiff.Status = "unavailable" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			compact, full := listed, hydrated
			tc.change(&compact, &full)
			if err := validateGraphAttemptReadback(listed, compact, full); err == nil {
				t.Fatal("invalid attempt readback accepted")
			}
		})
	}
}

func TestGraphWorkflowFailedOutcomeDiagnostics(t *testing.T) {
	root := graphBead{ID: "root", Ref: "graph.v2", Status: "closed", Metadata: map[string]any{"gc.outcome": "fail"}}
	newTransport := func(t *testing.T, issues []graphBead, status int, token string) graphPrivateEvidenceTransport {
		t.Helper()
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/v0/beads/issues" || r.Method != http.MethodGet {
				t.Errorf("unexpected diagnostic request: %s %s", r.Method, r.URL.Path)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(map[string]any{"items": issues, "has_more": false})
		}))
		t.Cleanup(server.Close)
		client := newGraphPrivateEvidenceHTTPClient()
		t.Cleanup(client.CloseIdleConnections)
		return graphPrivateEvidenceTransport{endpoint: server.URL, client: client, token: token}
	}
	t.Run("root and child verdicts", func(t *testing.T) {
		child := graphBead{ID: "child", Ref: "graph.body.preflight", Status: "closed", Metadata: map[string]any{
			"gc.root_bead_id": "root", "gc.scope_role": "member", "gc.outcome": "fail",
			"gc.failure_class": "hard", "gc.failure_reason": "preflight_failed", "review.verdict": "iterate",
		}}
		got := graphWorkflowFailedOutcomeDiagnostics(t.TempDir(), newTransport(t, []graphBead{child}, http.StatusOK, ""), root)
		for _, want := range []string{`"id":"root"`, `"id":"child"`, `"gc.outcome":"fail"`, `"gc.root_bead_id":"root"`, `"gc.failure_class":"hard"`, `"gc.failure_reason":"preflight_failed"`, `"review.verdict":"iterate"`} {
			if !strings.Contains(got, want) {
				t.Errorf("missing selected verdict field %q", want)
			}
		}
	})
	t.Run("arbitrary metadata excluded", func(t *testing.T) {
		child := graphBead{ID: "child", Title: "private-title-sentinel", Metadata: map[string]any{
			"private.payload": "private-payload-sentinel", "gc.failure_reason": "safe-selected-reason",
		}}
		got := graphWorkflowFailedOutcomeDiagnostics(t.TempDir(), newTransport(t, []graphBead{child}, http.StatusOK, ""), root)
		if !strings.Contains(got, "safe-selected-reason") {
			t.Fatal("selected reason was lost with excluded fields")
		}
		for _, forbidden := range []string{"private.payload", "private-payload-sentinel", "private-title-sentinel"} {
			if strings.Contains(got, forbidden) {
				t.Errorf("diagnostic disclosed unselected data %q", forbidden)
			}
		}
	})
	t.Run("redact before field clipping", func(t *testing.T) {
		const token = "SECRET_TOKEN_CROSSING_FIELD_BOUNDARY"
		child := graphBead{ID: "child", Metadata: map[string]any{"gc.failure_reason": strings.Repeat("x", 500) + token + strings.Repeat("z", 100)}}
		got := graphWorkflowFailedOutcomeDiagnostics(t.TempDir(), newTransport(t, []graphBead{child}, http.StatusOK, token), root)
		if strings.Contains(got, "SECRET") || !strings.Contains(got, "[redacted]") || !strings.Contains(got, "[truncated]") {
			t.Fatal("field was not redacted before clipping at the boundary")
		}
	})
	t.Run("redact log tail boundary", func(t *testing.T) {
		const token = "SECRET_TOKEN_CROSSING_LOG_TAIL_BOUNDARY"
		cityDir := t.TempDir()
		// Put the retained tail's first byte inside the token. The existing
		// log reader must discard that partial line, then redact complete lines.
		content := "discard-prefix-" + token + "\ncomplete " + token + "\n" + strings.Repeat("x", (16<<10)-len(token)-len("\ncomplete ")-len("\n")-8)
		if err := os.WriteFile(filepath.Join(cityDir, "graph-workflow-trace.log"), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		got := graphWorkflowFailedOutcomeDiagnostics(cityDir, newTransport(t, nil, http.StatusOK, token), root)
		if strings.Contains(got, "SECRET") || strings.Contains(got, "BOUNDARY") || !strings.Contains(got, "complete [redacted]") {
			t.Fatal("log tail leaked a token fragment or lost redacted retained evidence")
		}
	})
	t.Run("hard output cap with marker", func(t *testing.T) {
		cityDir := t.TempDir()
		for i := 0; i < 16; i++ {
			path := filepath.Join(citylayout.SessionDiagnosticsDir(cityDir), fmt.Sprintf("%02d", i), "start-stderr.log")
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(strings.Repeat("x", 16<<10)), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		got := graphWorkflowFailedOutcomeDiagnostics(cityDir, newTransport(t, nil, http.StatusOK, ""), root)
		if len(got) > 256<<10 || !strings.HasSuffix(got, "[diagnostics truncated]") {
			t.Fatalf("diagnostic bytes=%d, want <=256KiB with explicit truncation marker", len(got))
		}
	})
	t.Run("unavailable HTTP retains root failure", func(t *testing.T) {
		got := graphWorkflowFailedOutcomeDiagnostics(t.TempDir(), newTransport(t, nil, http.StatusServiceUnavailable, ""), root)
		if !strings.Contains(got, `"id":"root"`) || !strings.Contains(got, `"gc.outcome":"fail"`) || !strings.Contains(got, "issue verdict read unavailable:") || !strings.Contains(got, "HTTP 503") {
			t.Fatal("unavailable observation hid the original terminal root failure or HTTP error")
		}
		if root.Status != "closed" || metaValue(root, "gc.outcome") != "fail" {
			t.Fatal("diagnostic read changed the original workflow state")
		}
	})
}
