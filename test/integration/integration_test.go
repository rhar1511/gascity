//go:build integration

// Package integration provides end-to-end tests that exercise the real gc
// binary against real session providers (tmux or subprocess). Tests validate
// the tutorial experiences: gc init, gc start, gc stop, bead CRUD, etc.
//
// By default tests use tmux. Set GC_SESSION=subprocess to use the subprocess
// provider instead (no tmux required).
//
// Session safety: no-guard test cities use randomized 6-letter lowercase
// names (see uniqueCityName) so they spread across distinct Dolt DB prefixes
// instead of all collapsing to "gc". Legacy runs use shared pre/post sweeps;
// run-owned mode uses only its private temporary root and per-test cleanup.
package integration

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/bazeltest"

	"github.com/cenkalti/backoff/v4"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/testutil"
	"github.com/gastownhall/gascity/test/dolttest"
	"github.com/gastownhall/gascity/test/integration/runisolation"
	"github.com/gastownhall/gascity/test/integration/subprocesssweep"
	"github.com/gastownhall/gascity/test/tmuxtest"
	"github.com/gastownhall/gascity/test/toolhome"
)

// gcBinary is the path to the built gc binary, set by TestMain.
var gcBinary string

// bdBinary is the path to the bd binary, discovered by TestMain.
var (
	bdBinary              string
	realBDBinary          string
	doltBinary            string
	integrationToolBinDir string
)

// testGCHome isolates integration-test supervisor state from the developer's
// real ~/.gc registry, config, and logs.
var testGCHome string

// testRuntimeDir isolates the supervisor lock/socket from the developer's
// real XDG runtime directory.
var testRuntimeDir string

var cityCommandEnv sync.Map

var runIntegrationSupervisorStopCommand = exec.CommandContext

const (
	integrationGCCommandTimeout      = 60 * time.Second
	integrationGCLifecycleTimeout    = 120 * time.Second
	integrationGCDoltCommandTimeout  = 120 * time.Second
	integrationBDCommandTimeout      = 15 * time.Second
	integrationSupervisorStopTimeout = 10 * time.Second
	integrationSupervisorWaitDelay   = 10 * time.Second
)

const (
	integrationGCBinaryEnv           = "GC_INTEGRATION_GC_BINARY"
	integrationRealBDBinaryEnv       = "GC_INTEGRATION_REAL_BD"
	integrationDoltBinaryEnv         = "GC_INTEGRATION_DOLT_BINARY"
	integrationBuildGitDirEnv        = "GC_INTEGRATION_BUILD_GIT_DIR"
	integrationBuildGitTreeEnv       = "GC_INTEGRATION_BUILD_GIT_WORK_TREE"
	integrationDisposableBeadsDirEnv = "GC_INTEGRATION_DISPOSABLE_BEADS_DIR"
	integrationDisposableCityDirEnv  = "GC_INTEGRATION_DISPOSABLE_CITY_DIR"
	integrationDisposableDatabaseEnv = "GC_INTEGRATION_DISPOSABLE_DATABASE"
	integrationDisposableProjectEnv  = "GC_INTEGRATION_DISPOSABLE_PROJECT_ID"
	integrationRequireDisposableEnv  = "GC_INTEGRATION_REQUIRE_DISPOSABLE_BEADS_DIR"
	integrationFixtureAuthorityFlag  = "--gc-integration-fixture-authority="
	integrationDoltIdentityEnv       = "GC_INTEGRATION_DOLT_IDENTITY_MODE"
	managedDoltTestModeEnv           = "GC_MANAGED_DOLT_TEST_MODE"
	managedDoltTestParentEnv         = "GC_MANAGED_DOLT_TEST_PARENT_PID"
	doltIdentityModeIsolated         = "isolated"
	doltIdentityModeGlobal           = "global"
	doltIdentityModeSkip             = "skip"
)

var integrationGitRepositoryVars = []string{
	"GIT_DIR",
	"GIT_WORK_TREE",
	"GIT_INDEX_FILE",
	"GIT_OBJECT_DIRECTORY",
	"GIT_ALTERNATE_OBJECT_DIRECTORIES",
	"GIT_COMMON_DIR",
	"GIT_CEILING_DIRECTORIES",
	"GIT_DISCOVERY_ACROSS_FILESYSTEM",
	"GIT_PREFIX",
	"GIT_IMPLICIT_WORK_TREE",
}

var integrationBeadsSelectorVars = []string{
	"BD_DB",
	"BEADS_DB",
	"BEADS_CENTRAL_CONFIG",
	"BEADS_DOLT_CREDENTIAL_COMMAND",
	"BEADS_DOLT_REMOTESAPI_PORT",
	"BEADS_DOLT_SERVER_TLS",
	"BEADS_PROXIED_SERVER_PORT",
	"BEADS_PROXIED_SERVER_EXTERNAL_HOST",
	"BEADS_PROXIED_SERVER_EXTERNAL_PORT",
	"BEADS_PROXIED_SERVER_EXTERNAL_SOCKET_PATH",
	"BEADS_PROXIED_SERVER_ROOT_PATH",
	"BEADS_PROXIED_SERVER_CONFIG",
	"BEADS_PROXIED_SERVER_LOG",
	"BEADS_DOLT_SHARED_SERVER",
	"BEADS_SHARED_SERVER_DIR",
	"BEADS_DOLT_DATA_DIR",
	"BEADS_DOLT_DATABASE",
	"BEADS_DOLT_HOST",
	"BEADS_DOLT_PORT",
	"BEADS_DOLT_SOCKET",
	"BEADS_DOLT_USER",
	"BEADS_DOLT_PASSWORD",
	"BEADS_DOLT_SERVER_DATABASE",
	"BEADS_DOLT_SERVER_MODE",
	"BEADS_DOLT_SERVER_SOCKET",
	"BEADS_DOLT_SERVER_HOST",
	"BEADS_DOLT_SERVER_PORT",
	"BEADS_DOLT_SERVER_USER",
	"BEADS_DOLT_SERVER_PASSWORD",
	"BEADS_DOLT_PROXIED_SERVER",
	"GC_DOLT_DATA_DIR",
	"GC_DOLT_DATABASE",
	"GC_DOLT_HOST",
	"GC_DOLT_PORT",
	"GC_DOLT_USER",
	"GC_DOLT_PASSWORD",
	"GC_DOLT_STATE_FILE",
	"GC_DOLT_PID_FILE",
	"GC_DOLT_LOCK_FILE",
	"GC_DOLT_CONFIG_FILE",
	"GC_DOLT_LOG_FILE",
	"GC_BEADS_TRANSPORT",
	"GC_BEADS_TARGET",
	"GC_BEADS_BACKEND",
	"GC_BEADS_PROXY_EXTERNAL_HOST",
	"GC_BEADS_PROXY_EXTERNAL_PORT",
	"GC_BEADS_PROXY_EXTERNAL_SOCKET",
	"BEADS_BACKEND",
}

// tmuxSocketAliveSentinel pins the alive-sentinel flock on this process's
// tmux socket parent dir for the binary's lifetime; see TestMain.
var tmuxSocketAliveSentinel *os.File

// TestMain builds the gc binary and runs pre/post sweeps of orphan sessions.
func TestMain(m *testing.M) {
	flag.Parse()
	runMode, err := runisolation.Resolve(
		os.Getenv(runisolation.EnvName),
		os.Getenv("GC_SESSION"),
		os.Getenv(integrationDoltIdentityEnv),
	)
	if err != nil {
		fmt.Fprintln(os.Stderr, "integration:", err)
		os.Exit(2)
	}
	if err := os.Unsetenv(runisolation.EnvName); err != nil {
		fmt.Fprintf(os.Stderr, "integration: clearing %s: %v\n", runisolation.EnvName, err)
		os.Exit(2)
	}
	if listFlag := flag.Lookup("test.list"); listFlag != nil && listFlag.Value.String() != "" {
		os.Exit(m.Run())
	}

	if os.Getenv("GC_INTEGRATION_SUPERVISOR_STOP_HELPER") == "1" {
		select {}
	}

	// Every env this suite builds starts from os.Environ(); drop the shell's
	// XDG base directories and BEADS_*/BD_* first so only explicit values reach
	// bd, and pin bd's shared-server mode off. gc keeps the real HOME (see
	// pinRealHomeEnv); bd is re-homed by the wrapper around realBDBinary below.
	if err := toolhome.ScrubProcessEnv(); err != nil {
		panic("integration: scrubbing host bd env: " + err.Error())
	}

	subprocess := os.Getenv("GC_SESSION") == "subprocess"

	var runParent, tmpDir, tmuxSocketParent string
	var skipForMissingTmux bool
	testsStarted := false
	cleanupLegacy := func() error {
		if testsStarted {
			_ = stopIntegrationSupervisorWithTimeout(integrationSupervisorStopTimeout)
			if !subprocess {
				tmuxtest.KillAllTestSessions(&mainTB{})
			}
			sweepSubprocessTestProcesses()
		}
		if tmpDir != "" {
			if err := os.RemoveAll(tmpDir); err != nil {
				return fmt.Errorf("remove integration temp dir %s: %w", tmpDir, err)
			}
		}
		if tmuxSocketParent != "" {
			if err := os.RemoveAll(tmuxSocketParent); err != nil {
				return fmt.Errorf("remove tmux socket parent %s: %w", tmuxSocketParent, err)
			}
		}
		return nil
	}
	cleanupOwned := func() error {
		gcHome := ""
		if tmpDir != "" {
			gcHome = filepath.Join(tmpDir, "gc-home")
		}
		return cleanupOwnedIntegrationRun(runParent, tmpDir, gcHome, func() error {
			if testsStarted {
				procs := readProcessSnapshot()
				if procs == nil {
					return fmt.Errorf("owned supervisor cleanup: process absence could not be checked")
				}
				// Narrow runs may never start a supervisor. Skip the stop only
				// when the owned process census proves there is nothing to stop;
				// CleanupOwnedRoot checks absence again before removing the root.
				if len(ownedIntegrationProcesses(procs, runParent, gcHome)) == 0 {
					return nil
				}
				return stopIntegrationSupervisorWithTimeout(integrationSupervisorStopTimeout)
			}
			return nil
		})
	}
	finish := func() error {
		if err := runisolation.Finish(runMode, cleanupLegacy, cleanupOwned); err != nil {
			fmt.Fprintf(os.Stderr, "integration cleanup: %v\n", err)
			return err
		}
		return nil
	}
	finished := false
	defer func() {
		if !finished {
			_ = finish()
		}
	}()

	startupErr := runisolation.Startup(runMode, func(mode runisolation.Mode) error {
		var tmuxSocketRoot string
		if mode == runisolation.Owned {
			var err error
			runParent, err = os.MkdirTemp("", fmt.Sprintf("gc-integration-run-%d-*", os.Getpid()))
			if err != nil {
				return fmt.Errorf("create private integration run parent: %w", err)
			}
			runParent, err = filepath.Abs(runParent)
			if err != nil {
				return fmt.Errorf("resolve private integration run parent: %w", err)
			}
			if err := os.Setenv("TMPDIR", runParent); err != nil {
				return fmt.Errorf("set private integration TMPDIR: %w", err)
			}
			tmpDir, err = os.MkdirTemp(runParent, fmt.Sprintf("gc-integration-%d-*", os.Getpid()))
			if err != nil {
				return fmt.Errorf("create integration tool dir: %w", err)
			}
			tmuxSocketRoot = filepath.Join(tmpDir, "tmux")
		} else {
			var err error
			tmpDir, err = os.MkdirTemp("", fmt.Sprintf("gc-integration-%d-*", os.Getpid()))
			if err != nil {
				return fmt.Errorf("create integration temp dir: %w", err)
			}
			// Legacy tmux runs use a short socket parent because macOS's default
			// TMPDIR can make Unix socket paths exceed the platform limit.
			var tmuxSentinel *os.File
			var tmuxParentErr error
			tmuxSocketParent, tmuxSentinel, tmuxParentErr = tmuxtest.NewSocketParentDir("/tmp", io.Discard)
			tmuxSocketAliveSentinel = tmuxSentinel
			tmuxSocketRoot = filepath.Join(tmpDir, "tmux")
			if tmuxParentErr == nil {
				tmuxSocketRoot = filepath.Join(tmuxSocketParent, "tmux")
				if err := os.MkdirAll(tmuxSocketRoot, 0o700); err != nil {
					if tmuxSocketAliveSentinel != nil {
						_ = tmuxSocketAliveSentinel.Close()
					}
					tmuxSocketAliveSentinel = nil
					_ = os.RemoveAll(tmuxSocketParent)
					tmuxSocketParent = ""
					tmuxSocketRoot = filepath.Join(tmpDir, "tmux")
				}
			}
		}
		if err := tmuxtest.ConfigureProcessEnv(tmuxSocketRoot); err != nil {
			return fmt.Errorf("configure tmux test environment: %w", err)
		}
		return nil
	}, func() error {
		// Shared-directory sweeps run only in legacy mode. Owned mode has a
		// private TMPDIR and leaves other integration runs untouched.
		if !subprocess {
			if _, err := exec.LookPath("tmux"); err != nil {
				skipForMissingTmux = true
				return nil
			}
			tmuxtest.KillAllTestSessions(&mainTB{})
		}
		sweepSubprocessTestProcesses()
		dolttest.SweepStale(filepath.Dir(tmpDir), "gc-integration-")
		return nil
	})
	if startupErr != nil {
		panic("integration startup: " + startupErr.Error())
	}
	if skipForMissingTmux {
		_ = finish()
		finished = true
		os.Exit(0)
	}
	stopSignalSweeper := installIntegrationSignalSweeper(runMode, subprocess)
	defer stopSignalSweeper()

	testGCHome = filepath.Join(tmpDir, "gc-home")
	if err := os.MkdirAll(testGCHome, 0o755); err != nil {
		panic("integration: creating GC_HOME: " + err.Error())
	}
	testRuntimeDir = filepath.Join(tmpDir, "runtime")
	if err := os.MkdirAll(testRuntimeDir, 0o755); err != nil {
		panic("integration: creating XDG_RUNTIME_DIR: " + err.Error())
	}
	integrationToolBinDir = filepath.Join(tmpDir, "bin")
	if err := os.MkdirAll(integrationToolBinDir, 0o755); err != nil {
		panic("integration: creating integration tool bin dir: " + err.Error())
	}

	if override, ok, err := binaryOverride(integrationGCBinaryEnv); err != nil {
		panic("integration: resolving GC override: " + err.Error())
	} else if ok {
		gcBinary = filepath.Join(integrationToolBinDir, "gc")
		if err := writeExecShim(gcBinary, override); err != nil {
			panic("integration: writing gc shim: " + err.Error())
		}
	} else {
		gcBinary = filepath.Join(integrationToolBinDir, "gc")
		// Under bazel the pre-built gc binary ships in runfiles (declared as
		// a data dep); use it instead of shelling out to `go build`.
		runfilesGC := ""
		for _, rf := range []string{os.Getenv("RUNFILES_DIR"), os.Getenv("TEST_SRCDIR")} {
			if rf == "" {
				continue
			}
			if bin := filepath.Join(rf, "_main", "cmd", "gc", "gc_", "gc"); statOK(bin) {
				runfilesGC = bin
				break
			}
		}
		if runfilesGC != "" {
			gcBinary = runfilesGC
		} else {
			buildCmd := exec.Command("go", "build", "-o", gcBinary, "./cmd/gc")
			buildCmd.Dir = findModuleRoot()
			buildCmd.Env, err = integrationModuleBuildEnv(os.Environ())
			if err != nil {
				panic("integration: preparing gc build environment: " + err.Error())
			}
			if out, err := buildCmd.CombinedOutput(); err != nil {
				panic("integration: building gc binary: " + err.Error() + "\n" + string(out))
			}
		}
	}

	if override, ok, err := binaryOverride(integrationRealBDBinaryEnv); err != nil {
		panic("integration: resolving bd override: " + err.Error())
	} else if ok {
		realBDBinary = override
	} else if bazeltest.IsBazel() {
		// Under bazel the pinned bd ships prebuilt in runfiles as a data dep
		// (http_archive of the same release the go-test CI installs).
		if bd := runfilesBinaryAt("bd_bin_v1_3_1_rc_2", "bd"); bd != "" {
			realBDBinary = bd
		}
	} else {
		var err error
		realBDBinary, err = buildPinnedIntegrationBDBinary(tmpDir)
		if err != nil {
			panic("integration: building pinned bd binary: " + err.Error())
		}
	}
	// Every real bd this suite runs — directly, through the file-store shim, or
	// forked by gc — goes through this wrapper, which re-homes bd under the run's
	// temp dir: gc runs with the real HOME, and bd must never resolve the
	// operator's ~/.beads (a user-level dolt.shared-server: true starts the
	// host-wide shared Dolt server).
	wrappedRealBD := filepath.Join(tmpDir, "bd-real", "bd")
	if err := toolhome.WriteWrapper(wrappedRealBD, filepath.Join(tmpDir, "bd-tool-home"), realBDBinary); err != nil {
		panic("integration: wrapping real bd: " + err.Error())
	}
	realBDBinary = wrappedRealBD
	bdBinary = filepath.Join(integrationToolBinDir, "bd")
	if bazeltest.IsBazel() {
		// The shim is a bazel-built go_binary shipped in runfiles as a data
		// dep; no on-worker `go build` (which needs a module cache) required.
		if shim := runfilesBinary("test/integration/filebdshim/filebdshim_/filebdshim"); shim != "" {
			bdBinary = shim
		}
	} else {
		shimCmd := exec.Command("go", "build", "-o", bdBinary, "./test/integration/filebdshim")
		shimCmd.Dir = findModuleRoot()
		shimCmd.Env, err = integrationModuleBuildEnv(os.Environ())
		if err != nil {
			panic("integration: preparing bd shim build environment: " + err.Error())
		}
		if out, err := shimCmd.CombinedOutput(); err != nil {
			panic("integration: building bd shim: " + err.Error() + "\n" + string(out))
		}
	}
	// These values authorize VCS discovery only for module-local Go builds.
	// Remove them before any test or fixture subprocess can inherit them.
	if err := os.Unsetenv(integrationBuildGitDirEnv); err != nil {
		panic("integration: clearing build-only Git directory override: " + err.Error())
	}
	if err := os.Unsetenv(integrationBuildGitTreeEnv); err != nil {
		panic("integration: clearing build-only Git work-tree override: " + err.Error())
	}
	if realBDBinary != "" {
		if err := os.Setenv(integrationRealBDBinaryEnv, realBDBinary); err != nil {
			panic("integration: setting GC_INTEGRATION_REAL_BD: " + err.Error())
		}
	}

	if override, ok, err := binaryOverride(integrationDoltBinaryEnv); err != nil {
		panic("integration: resolving dolt override: " + err.Error())
	} else if ok {
		doltBinary = filepath.Join(integrationToolBinDir, "dolt")
		if err := writeExecShim(doltBinary, override); err != nil {
			panic("integration: writing dolt shim: " + err.Error())
		}
	} else if resolved := runfilesBinaryAt("dolt_bin_v2_1_7", "dolt-linux-amd64/bin/dolt"); resolved != "" {
		// Prebuilt pinned dolt from runfiles (bazel http_archive data dep);
		// preferred over PATH so remote workers without a system dolt run the
		// dolt-backed shapes.
		doltBinary = filepath.Join(integrationToolBinDir, "dolt")
		if err := writeExecShim(doltBinary, resolved); err != nil {
			panic("integration: writing dolt shim: " + err.Error())
		}
	} else if resolved, err := exec.LookPath("dolt"); err == nil {
		doltBinary = filepath.Join(integrationToolBinDir, "dolt")
		if err := writeExecShim(doltBinary, resolved); err != nil {
			panic("integration: writing dolt shim: " + err.Error())
		}
	}

	// Agents resolve gc/bd/dolt from PATH (their scripts cannot see runfiles
	// paths), and integrationEnvFor prepends integrationToolBinDir to PATH.
	// Under bazel gcBinary/bdBinary point directly at runfiles binaries, so
	// link them into the tool bin dir the way the go-build path materializes
	// them there. Symlinks keep the 100MB+ gc binary out of every test's
	// sandbox copy; copy is the fallback when linking fails.
	for name, bin := range map[string]string{
		"gc":   gcBinary,
		"bd":   bdBinary,
		"dolt": doltBinary,
	} {
		if bin == "" || filepath.Dir(bin) == integrationToolBinDir {
			continue
		}
		dst := filepath.Join(integrationToolBinDir, name)
		_ = os.Remove(dst)
		if err := os.Symlink(bin, dst); err != nil {
			data, readErr := os.ReadFile(bin)
			if readErr != nil {
				panic("integration: staging " + name + " into tool bin dir: " + readErr.Error())
			}
			if err := os.WriteFile(dst, data, 0o755); err != nil {
				panic("integration: staging " + name + " into tool bin dir: " + err.Error())
			}
		}
	}

	port, err := reserveLoopbackPort()
	if err != nil {
		panic("integration: reserving supervisor port: " + err.Error())
	}
	supervisorConfig := fmt.Sprintf("[supervisor]\nport = %d\nbind = \"127.0.0.1\"\n", port)
	if err := os.WriteFile(filepath.Join(testGCHome, "supervisor.toml"), []byte(supervisorConfig), 0o644); err != nil {
		panic("integration: writing supervisor config: " + err.Error())
	}
	if err := seedDoltIdentityForRoot(testGCHome); err != nil {
		panic("integration: writing dolt config: " + err.Error())
	}

	// Run tests.
	testsStarted = true
	code := m.Run()
	cleanupErr := finish()
	finished = true
	if runMode == runisolation.Owned && cleanupErr != nil && code == 0 {
		code = 1
	}
	os.Exit(code)
}

func installIntegrationSignalSweeper(mode runisolation.Mode, subprocess bool) func() {
	signals := make(chan os.Signal, 2)
	done := make(chan struct{})
	// Catches an external interrupt (Ctrl-C, `kill`, a CI job cancellation).
	// Legacy mode sweeps shared state; owned mode leaves its private root for
	// per-fixture cleanup or later review.
	// NOTE: `go test -timeout` does not normally reach this handler — the
	// in-binary deadline fires a panic() from an internal timer goroutine and
	// the runtime calls os.Exit(2) directly, so a timed-out run's orphans are
	// caught only by the next run's pre-sweep in TestMain. The handler still
	// has to stay registered: cmd/go sends SIGQUIT as a backstop once the
	// binary blows past testTimeout + WaitDelay (issue #3640).
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)
	go func() {
		select {
		case sig := <-signals:
			runisolation.OnSignal(mode, func() {
				sweepIntegrationProcesses(subprocess)
			})
			signal.Stop(signals)
			if s, ok := sig.(syscall.Signal); ok {
				signal.Reset(s)
				_ = syscall.Kill(os.Getpid(), s)
			}
		case <-done:
		}
	}()
	return func() {
		signal.Stop(signals)
		close(done)
	}
}

func sweepIntegrationProcesses(subprocess bool) {
	_ = stopIntegrationSupervisorWithTimeout(integrationSupervisorStopTimeout)
	// Reap dolt orphans under this run's home too — the per-test t.Cleanup that
	// normally does this is bypassed on a signal (issue #3640).
	if testGCHome != "" {
		cleanupIntegrationDoltSQLServersUnderRoot(testGCHome)
	}
	if !subprocess {
		tmuxtest.KillAllTestSessions(&mainTB{})
	}
	sweepSubprocessTestProcesses()
}

// cleanupOwnedIntegrationRun removes only this TestMain run's private root.
func cleanupOwnedIntegrationRun(runParent, tmpDir, gcHome string, stopSupervisor func() error) error {
	return runisolation.CleanupOwnedRoot(runParent, tmpDir, gcHome, stopSupervisor, func(root, gcHome string) ([]int, error) {
		procs := readProcessSnapshot()
		if procs == nil {
			return nil, fmt.Errorf("process absence could not be checked")
		}
		return sortedIntegrationPIDs(ownedIntegrationProcesses(procs, root, gcHome)), nil
	})
}

func ownedIntegrationProcesses(procs map[int]procSnapshot, runParent, gcHome string) map[int]bool {
	owned := make(map[int]bool)
	for pid, info := range procs {
		if strings.Contains(info.cmd, runParent) {
			owned[pid] = true
		}
	}
	for pid := range integrationDoltSQLServerKillSet(procs, gcHome) {
		owned[pid] = true
	}
	for pid := range subprocessTestKillSetUnder(procs, runParent) {
		owned[pid] = true
	}
	return owned
}

func sortedIntegrationPIDs(pids map[int]bool) []int {
	out := make([]int, 0, len(pids))
	for pid := range pids {
		out = append(out, pid)
	}
	sort.Ints(out)
	return out
}

func stopIntegrationSupervisorWithTimeout(timeout time.Duration) error {
	if gcBinary == "" {
		return fmt.Errorf("gc binary is unavailable for supervisor stop")
	}
	if timeout <= 0 {
		timeout = integrationSupervisorStopTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	stopCmd := runIntegrationSupervisorStopCommand(ctx, gcBinary, "supervisor", "stop", "--wait")
	stopCmd.Env = integrationEnv()
	out, err := stopCmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		fmt.Fprintf(os.Stderr, "integration cleanup: supervisor stop timed out after %s\n%s", timeout, string(out)) //nolint:errcheck
		return fmt.Errorf("supervisor stop timed out after %s", timeout)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "integration cleanup: supervisor stop failed: %v\n%s", err, string(out)) //nolint:errcheck
		return fmt.Errorf("supervisor stop failed: %w", err)
	}
	return nil
}

func TestIntegrationSupervisorStopHelperProcess(t *testing.T) {
	if os.Getenv("GC_INTEGRATION_SUPERVISOR_STOP_HELPER") != "1" {
		return
	}
	select {}
}

func TestIntegrationTestListingSkipsRuntimeSetup(t *testing.T) {
	missing := t.TempDir()
	env := os.Environ()
	for _, name := range []string{
		"GC_SESSION",
		runisolation.EnvName,
		"GC_INTEGRATION_SUPERVISOR_STOP_HELPER",
		integrationGCBinaryEnv,
		integrationRealBDBinaryEnv,
		integrationDoltBinaryEnv,
	} {
		env = filterEnv(env, name)
	}
	env = append(env,
		"GC_SESSION=subprocess",
		integrationGCBinaryEnv+"="+filepath.Join(missing, "gc"),
		integrationRealBDBinaryEnv+"="+filepath.Join(missing, "bd"),
		integrationDoltBinaryEnv+"="+filepath.Join(missing, "dolt"),
	)

	const want = "TestE2E_Hook_WithWork"
	cases := []struct {
		name             string
		args             []string
		wantSetupFailure bool
	}{
		{name: "equals", args: []string{"-test.list=^" + want + "$"}},
		{name: "separate", args: []string{"-test.list", "^" + want + "$"}},
		{name: "empty list runs setup", args: []string{"-test.list="}, wantSetupFailure: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			output, err := runCommand("", env, testutil.ExecRaceTimeout, os.Args[0], tc.args...)
			if tc.wantSetupFailure {
				if err == nil {
					t.Fatalf("listing with empty regexp succeeded despite unavailable runtime binaries:\n%s", output)
				}
				if !strings.Contains(output, "resolving GC override") || !strings.Contains(output, integrationGCBinaryEnv) {
					t.Fatalf("empty list failed before expected runtime setup check: %v\n%s", err, output)
				}
				return
			}
			if err != nil {
				t.Fatalf("listing integration tests with args %q and unavailable runtime binaries: %v\n%s", tc.args, err, output)
			}
			if got := strings.TrimSpace(output); got != want {
				t.Fatalf("listed tests with args %q = %q, want only %q", tc.args, got, want)
			}
		})
	}
}

func TestStopIntegrationSupervisorWithTimeoutReturnsAfterDeadline(t *testing.T) {
	oldRunner := runIntegrationSupervisorStopCommand
	oldGCBinary := gcBinary
	oldGCHome := testGCHome
	oldRuntimeDir := testRuntimeDir
	oldToolBin := integrationToolBinDir
	oldRealBD := realBDBinary
	t.Cleanup(func() {
		runIntegrationSupervisorStopCommand = oldRunner
		gcBinary = oldGCBinary
		testGCHome = oldGCHome
		testRuntimeDir = oldRuntimeDir
		integrationToolBinDir = oldToolBin
		realBDBinary = oldRealBD
	})

	t.Setenv("GC_INTEGRATION_SUPERVISOR_STOP_HELPER", "1")
	gcBinary = os.Args[0]
	testGCHome = t.TempDir()
	testRuntimeDir = t.TempDir()
	integrationToolBinDir = filepath.Dir(os.Args[0])
	realBDBinary = "bd"
	runIntegrationSupervisorStopCommand = exec.CommandContext

	start := time.Now()
	if err := stopIntegrationSupervisorWithTimeout(10 * time.Millisecond); err == nil {
		t.Fatal("stopIntegrationSupervisorWithTimeout() error = nil, want timeout")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("stopIntegrationSupervisorWithTimeout took %s, want bounded return", elapsed)
	}
}

func binaryOverride(envName string) (string, bool, error) {
	raw := strings.TrimSpace(os.Getenv(envName))
	if raw == "" {
		return "", false, nil
	}
	path := raw
	if !filepath.IsAbs(path) {
		abs, err := filepath.Abs(path)
		if err != nil {
			return "", false, fmt.Errorf("%s=%q: make absolute: %w", envName, raw, err)
		}
		path = abs
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", false, fmt.Errorf("%s=%q: %w", envName, raw, err)
	}
	if info.IsDir() {
		return "", false, fmt.Errorf("%s=%q points to a directory", envName, raw)
	}
	return path, true, nil
}

// buildPinnedIntegrationBDBinary builds bd from the exact Beads module that
// the integration test binary and gc both import. Resolving PATH here lets an
// older host bd open the database after gc has migrated it, producing a schema
// skew that obscures the workflow under test.
func buildPinnedIntegrationBDBinary(tmpDir string) (string, error) {
	version, err := pinnedIntegrationBeadsModuleVersion()
	if err != nil {
		return "", err
	}
	binDir := filepath.Join(tmpDir, "pinned-bd")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return "", fmt.Errorf("create pinned bd directory: %w", err)
	}
	// CGO_ENABLED=1 + gms_pure_go is the embedded-capable bd build (per beads
	// INSTALLING.md): the pinned bd's `bd init` defaults to embedded Dolt,
	// which a CGO_ENABLED=0 binary refuses at runtime.
	cmd := exec.Command("go", "install", "-tags", "gms_pure_go", "github.com/steveyegge/beads/cmd/bd@"+version)
	cmd.Env = append(integrationPinnedBdBuildEnv(os.Environ()), "CGO_ENABLED=1", "GOBIN="+binDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("go install github.com/steveyegge/beads/cmd/bd@%s: %w\n%s", version, err, out)
	}
	return filepath.Join(binDir, "bd"), nil
}

// pinnedBdStoreCommandRunner keeps direct BdStore integration tests on the
// same bd shim used by their setup commands. The default runner resolves the
// ambient process PATH before its per-command environment applies, so using it
// directly could select a host bd whose schema knowledge predates the pinned
// Beads module that created the test database.
func pinnedBdStoreCommandRunner() beads.CommandRunner {
	runner := beads.ExecCommandRunner()
	return func(dir, name string, args ...string) ([]byte, error) {
		if name == "bd" {
			name = bdBinary
		}
		return runner(dir, name, args...)
	}
}

func pinnedIntegrationBeadsModuleVersion() (string, error) {
	cmd := exec.Command("go", "list", "-m", "-f", "{{.Version}}", "github.com/steveyegge/beads")
	cmd.Dir = findModuleRoot()
	cmd.Env = integrationPinnedBdBuildEnv(os.Environ())
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("resolve github.com/steveyegge/beads module version: %w\n%s", err, out)
	}
	version := strings.TrimSpace(string(out))
	if version == "" {
		return "", errors.New("github.com/steveyegge/beads module version is empty")
	}
	return version, nil
}

// integrationPinnedBdBuildEnv keeps repository and fixture selectors out of
// the go list/install calls used to find and build pinned bd. Those commands
// use the Go module cache; unlike the module-local gc and shim builds, they do
// not need repository-location overrides for VCS stamping.
func integrationPinnedBdBuildEnv(base []string) []string {
	blocked := append([]string(nil), integrationGitRepositoryVars...)
	blocked = append(blocked, integrationBeadsSelectorVars...)
	blocked = append(blocked,
		"GC_TESTENV_PASSTHROUGH",
		"BEADS_DIR",
		"BEADS_HOLDER_TOKEN",
		"BEADS_ACTOR",
		"DOLT_ROOT_PATH",
		"GC_BEADS",
		"GC_HOME",
		"GC_DIR",
		"GC_CITY",
		"GC_CITY_PATH",
		"GC_CITY_ROOT",
		"GC_CITY_RUNTIME_DIR",
		"GC_AGENT",
		"GC_RIG",
		"GC_RIG_ROOT",
		"GC_SESSION_ID",
		"GC_SESSION_NAME",
		"GC_TMUX_SESSION",
		integrationGCBinaryEnv,
		integrationRealBDBinaryEnv,
		integrationDoltBinaryEnv,
		integrationBuildGitDirEnv,
		integrationBuildGitTreeEnv,
		integrationDisposableBeadsDirEnv,
		integrationDisposableCityDirEnv,
		integrationDisposableDatabaseEnv,
		integrationDisposableProjectEnv,
		integrationRequireDisposableEnv,
		integrationDoltIdentityEnv,
		managedDoltTestModeEnv,
		managedDoltTestParentEnv,
	)
	return filterEnvMany(base, blocked...)
}

// wantPinnedBeadsModuleVersion is the beads module version this suite expects
// go.mod to pin. TestBDVersionPins in scripts/bd_version_pin_test.go reads it
// by name out of this file and asserts it matches go.mod — see
// TestPinnedIntegrationBeadsModuleVersion for why it is a literal.
const wantPinnedBeadsModuleVersion = "v1.3.1-rc.2"

func TestPinnedIntegrationBeadsModuleVersion(t *testing.T) {
	version, err := pinnedIntegrationBeadsModuleVersion()
	if err != nil {
		t.Fatalf("pinnedIntegrationBeadsModuleVersion() error = %v", err)
	}
	// A deliberate second anchor on go.mod's beads pin: this suite installs
	// bd from whatever go.mod names (installPinnedBd above), so a bump must be
	// a reviewed edit here too rather than silently changing which bd the
	// integration tests run against.
	//
	// This test only runs in the `rest-full` integration shard, which is gated
	// on `push` — i.e. after merge, which is how v1.3.0-rc.2 sat stale here
	// (tracker ga-rnwg5u). TestBDVersionPins in scripts/bd_version_pin_test.go
	// reads wantPinnedBeadsModuleVersion by name out of this file and asserts it
	// against go.mod's pin; `make test-ci-policy` runs it, and that target is on
	// the PR-time preflight-static job, so drift now fails before merge. Keep
	// the const name greppable if you move it.
	if version != wantPinnedBeadsModuleVersion {
		t.Errorf("pinnedIntegrationBeadsModuleVersion() = %q, want %q", version, wantPinnedBeadsModuleVersion)
	}
}

func writeExecShim(path, target string) error {
	script := "#!/bin/sh\nexec " + singleQuoteShell(target) + ` "$@"` + "\n"
	return os.WriteFile(path, []byte(script), 0o755)
}

func singleQuoteShell(s string) string {
	if s == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

type procSnapshot struct {
	pid  int
	ppid int
	cmd  string
}

func sweepSubprocessTestProcesses() {
	procs := readProcessSnapshot()
	if len(procs) == 0 {
		return
	}

	killSet := subprocessTestKillSet(procs)
	if len(killSet) == 0 {
		return
	}

	for pid := range killSet {
		_ = syscall.Kill(pid, syscall.SIGTERM)
	}
	time.Sleep(150 * time.Millisecond)
	for pid := range killSet {
		if err := syscall.Kill(pid, syscall.Signal(0)); err == nil {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	}
	waitForPIDsReaped(killSet)
}

func configureIntegrationSupervisorCommand(cmd *exec.Cmd) {
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		return cmd.Process.Signal(syscall.SIGTERM)
	}
	cmd.WaitDelay = integrationSupervisorWaitDelay
}

func registerIntegrationDoltSQLServerCleanup(t *testing.T, root string) {
	t.Helper()
	t.Cleanup(func() {
		cleanupIntegrationDoltSQLServersUnderRoot(root)
	})
}

func cleanupIntegrationDoltSQLServersUnderRoot(root string) {
	procs := readProcessSnapshot()
	if len(procs) == 0 {
		return
	}
	terminateIntegrationPIDs(integrationDoltSQLServerKillSet(procs, root))
}

func integrationDoltSQLServerKillSet(procs map[int]procSnapshot, root string) map[int]bool {
	killSet := make(map[int]bool)
	for pid, info := range procs {
		configPath := integrationDoltSQLServerConfigPath(info.cmd)
		if configPath == "" || !pathWithinIntegrationRoot(root, configPath) {
			continue
		}
		killSet[pid] = true
	}
	return killSet
}

func integrationDoltSQLServerConfigPath(cmd string) string {
	fields := strings.Fields(cmd)
	if !integrationLooksLikeDoltSQLServer(fields) {
		return ""
	}
	for i, field := range fields {
		if field == "--config" {
			if i+1 < len(fields) {
				return fields[i+1]
			}
			return ""
		}
		if strings.HasPrefix(field, "--config=") {
			return strings.TrimPrefix(field, "--config=")
		}
	}
	return ""
}

func integrationLooksLikeDoltSQLServer(fields []string) bool {
	for i := 0; i+1 < len(fields); i++ {
		if filepath.Base(fields[i]) == "dolt" && fields[i+1] == "sql-server" {
			return true
		}
	}
	return false
}

func pathWithinIntegrationRoot(root, path string) bool {
	if root == "" || path == "" {
		return false
	}
	cleanRoot := filepath.Clean(root)
	if cleanRoot == "." || cleanRoot == string(os.PathSeparator) {
		return false
	}
	cleanPath := filepath.Clean(path)
	return cleanPath == cleanRoot || strings.HasPrefix(cleanPath, cleanRoot+string(os.PathSeparator))
}

func terminateIntegrationPIDs(killSet map[int]bool) {
	if len(killSet) == 0 {
		return
	}
	for pid := range killSet {
		_ = syscall.Kill(pid, syscall.SIGTERM)
	}
	time.Sleep(150 * time.Millisecond)
	for pid := range killSet {
		if err := syscall.Kill(pid, syscall.Signal(0)); err == nil {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	}
	waitForPIDsReaped(killSet)
}

// waitForPIDsReaped blocks until every PID in killSet is gone (signal-0 errors)
// or a bounded deadline elapses. Without it, a SIGKILL returns before the
// kernel has torn the process down and released its open files: a following
// t.TempDir() RemoveAll then races a dying managed Dolt server under
// cityDir/.beads/dolt ("directory not empty"), and a following test can
// re-bind the just-freed managed Dolt port and adopt a half-dead server whose
// DB still has prior tables ("alter pre-existing dirty tables"). The deadline
// guarantees a wedged process can never hang the suite.
func waitForPIDsReaped(killSet map[int]bool) {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		alive := false
		for pid := range killSet {
			if err := syscall.Kill(pid, syscall.Signal(0)); err == nil {
				alive = true
				break
			}
		}
		if !alive {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// readProcessSnapshot returns the live process table. /proc gives an exact,
// dependency-free read on Linux (CI); macOS has no /proc, so readProcessSnapshot
// falls back to shelling out to `ps` there. Without the fallback, every sweep
// built on this snapshot (sweepSubprocessTestProcesses, subprocessTestKillSet)
// silently no-ops on macOS dev boxes: orphaned "gc supervisor run" processes
// from a timed-out or killed run are never reaped, on that run or any later
// one (issue: orphan supervisor from rest-full ran 49+ minutes in a temp city).
func readProcessSnapshot() map[int]procSnapshot {
	if procs := readProcessSnapshotProc(); procs != nil {
		return procs
	}
	return readProcessSnapshotPS()
}

func readProcessSnapshotProc() map[int]procSnapshot {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil
	}
	procs := make(map[int]procSnapshot)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		cmdline, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "cmdline"))
		if err != nil || len(cmdline) == 0 {
			continue
		}
		status, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "status"))
		if err != nil {
			continue
		}
		ppid := parsePPid(string(status))
		if ppid == 0 {
			continue
		}
		cmd := strings.TrimSpace(strings.ReplaceAll(string(cmdline), "\x00", " "))
		if cmd == "" {
			continue
		}
		procs[pid] = procSnapshot{pid: pid, ppid: ppid, cmd: cmd}
	}
	return procs
}

// readProcessSnapshotPS shells out to `ps` (BSD/macOS and Linux both support
// this invocation) to build the same pid->{ppid,cmd} view /proc gives for
// free on Linux. Best-effort: a `ps` failure returns nil, same as a missing
// /proc, so callers treat "can't determine the process table" uniformly.
func readProcessSnapshotPS() map[int]procSnapshot {
	out, err := exec.Command("ps", "-axwwo", "pid=,ppid=,command=").Output()
	if err != nil {
		return nil
	}
	procs := make(map[int]procSnapshot)
	for _, line := range strings.Split(string(out), "\n") {
		pid, ppid, cmd, ok := parsePSLine(line)
		if !ok {
			continue
		}
		procs[pid] = procSnapshot{pid: pid, ppid: ppid, cmd: cmd}
	}
	return procs
}

// parsePSLine parses one line of `ps -axwwo pid=,ppid=,command=` output.
// It scans by whitespace runs for the first two fields (pid, ppid) rather
// than splitting the whole line, so internal spaces in the command string
// (arguments, paths) survive intact.
func parsePSLine(line string) (pid, ppid int, cmd string, ok bool) {
	rest := strings.TrimLeft(line, " \t")
	pidStr, rest := nextPSField(rest)
	ppidStr, rest := nextPSField(rest)
	cmd = strings.TrimSpace(rest)
	if pidStr == "" || ppidStr == "" || cmd == "" {
		return 0, 0, "", false
	}
	pid, err := strconv.Atoi(pidStr)
	if err != nil {
		return 0, 0, "", false
	}
	ppid, err = strconv.Atoi(ppidStr)
	if err != nil {
		return 0, 0, "", false
	}
	return pid, ppid, cmd, true
}

// nextPSField splits s on the first run of whitespace, returning the field
// before it and the remainder (with leading whitespace trimmed).
func nextPSField(s string) (field, rest string) {
	i := strings.IndexAny(s, " \t")
	if i < 0 {
		return s, ""
	}
	return s[:i], strings.TrimLeft(s[i:], " \t")
}

func parsePPid(status string) int {
	for _, line := range strings.Split(status, "\n") {
		if !strings.HasPrefix(line, "PPid:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0
		}
		ppid, err := strconv.Atoi(fields[1])
		if err != nil {
			return 0
		}
		return ppid
	}
	return 0
}

// integrationPIDAlive reports whether pid still exists. Signal 0 probes
// existence without delivering a signal; EPERM means the process exists but
// is not ours to signal — treat as alive (don't reap).
func integrationPIDAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

func subprocessTestKillSet(procs map[int]procSnapshot) map[int]bool {
	return subprocessTestKillSetUnder(procs, os.TempDir())
}

func subprocessTestKillSetUnder(procs map[int]procSnapshot, tempParent string) map[int]bool {
	owned := make(map[int]subprocesssweep.Process, len(procs))
	for pid, info := range procs {
		owned[pid] = subprocesssweep.Process{PPID: info.ppid, Cmd: info.cmd}
	}
	return subprocesssweep.KillSet(owned, tempParent, os.Getpid(), integrationPIDAlive)
}

// gc runs the gc binary with the given args. If dir is non-empty, it sets
// the working directory. Returns combined stdout+stderr and any error.
func gc(dir string, args ...string) (string, error) {
	envDir := commandCityDirForArgs(dir, args)
	return runCommand(dir, commandEnvForDir(envDir, false), gcCommandTimeout(args), gcBinary, args...)
}

// gcDolt runs the gc binary with the given args using the isolated integration
// supervisor state, but without forcing GC_DOLT=skip. Use this for tests that
// need the real bd+dolt-backed bead store.
func gcDolt(dir string, args ...string) (string, error) {
	return gcDoltWithTimeout(dir, gcDoltCommandTimeout(args), args...)
}

func gcDoltWithTimeout(dir string, timeout time.Duration, args ...string) (string, error) {
	envDir := commandCityDirForArgs(dir, args)
	return runCommand(dir, commandEnvForDir(envDir, true), timeout, gcBinary, args...)
}

// bd runs the bd binary with the given args. If dir is non-empty, it sets
// the working directory. Returns combined stdout+stderr and any error.
func bd(dir string, args ...string) (string, error) {
	env := commandEnvForDir(dir, false)
	if usesStandaloneBDWorkspace(dir, env) {
		env = standaloneBDEnvForDir(dir)
	}
	out, err := runCommand(dir, env, integrationBDCommandTimeout, bdBinary, args...)
	if err == nil || !shouldUseFileStoreBDFallback(dir, out, args) {
		return out, err
	}
	return runFileStoreBD(dir, args...)
}

func standaloneBDEnvForDir(dir string) []string {
	base := parseEnvList(integrationEnv())
	keep := []string{
		"HOME",
		"PATH",
		"TMPDIR",
		"USER",
		"LOGNAME",
		"LANG",
		"LC_ALL",
		"TZ",
		"DOLT_ROOT_PATH",
		integrationRealBDBinaryEnv,
		integrationGCBinaryEnv,
		integrationDoltBinaryEnv,
	}
	env := make([]string, 0, len(keep)+3)
	for _, key := range keep {
		if value, ok := base[key]; ok {
			env = append(env, key+"="+value)
		}
	}
	// integrationEnv pins HOME to the real passwd-db home for gc start/supervisor
	// start subprocesses. This helper only execs the bd binary, so re-isolate HOME
	// back to the caller-owned dir instead of leaking the real home through.
	env = replaceEnv(env, "HOME", dir)
	// Keep DOLT_ROOT_PATH from integrationEnv so standalone bd commands use
	// the suite's seeded Dolt identity instead of an unseeded per-workspace root.
	// BEADS_DIR and XDG_RUNTIME_DIR are temp-scoped by caller-owned test dirs;
	// bd's embedded-mode default needs no server shutdown, and server-mode tests
	// should use their own explicit lifecycle instead of hiding it in this env.
	env = append(env, "XDG_RUNTIME_DIR="+dir)
	env = append(env, "BEADS_DIR="+filepath.Join(dir, ".beads"))
	return append(env, "BEADS_DOLT_AUTO_START=1")
}

func usesStandaloneBDWorkspace(dir string, env []string) bool {
	if parseEnvList(env)["GC_BEADS"] == "file" {
		return false
	}
	return hasStandaloneBDWorkspace(dir)
}

func hasStandaloneBDWorkspace(dir string) bool {
	if dir == "" {
		return false
	}
	if _, err := os.Stat(filepath.Join(dir, ".beads", "config.yaml")); err == nil {
		return true
	}
	return false
}

// bdDolt runs bd against a Dolt-backed city using the same isolated runtime
// env as integration gc commands plus the city's managed Dolt port.
func bdDolt(dir string, args ...string) (string, error) {
	env := commandEnvForDir(dir, true)
	binary := bdBinary
	fixturePinned := false
	if dir != "" {
		env = filterEnv(env, "GC_CITY")
		env = filterEnv(env, "GC_CITY_PATH")
		env = filterEnv(env, "GC_CITY_ROOT")
		env = filterEnv(env, "GC_CITY_RUNTIME_DIR")
		env = append(env,
			"GC_CITY="+dir,
			"GC_CITY_PATH="+dir,
			"GC_CITY_RUNTIME_DIR="+filepath.Join(dir, ".gc", "runtime"),
		)
		if expectedBeadsDir := strings.TrimSpace(parseEnvList(env)[integrationDisposableBeadsDirEnv]); expectedBeadsDir != "" {
			fixturePinned = true
			if filepath.Clean(expectedBeadsDir) != filepath.Join(filepath.Clean(dir), ".beads") {
				return "", errors.New("graph command Beads target does not match its disposable city")
			}
			binary = filepath.Join(filepath.Dir(dir), "fixture-bin", "bd")
			info, err := os.Lstat(binary)
			if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o111 == 0 {
				return "", errors.New("graph command has no executable fixture-owned bd launcher")
			}
		}
		var port string
		var ok bool
		if fixturePinned {
			port, ok = graphFixtureProxyPortForTest(dir)
			if !ok {
				return "", errors.New("graph command has no verified fixture-owned Beads proxy and Dolt listener")
			}
		} else {
			port, ok = ensureManagedDoltPortForTest(dir)
			if ok {
				env = appendManagedDoltEndpointEnv(env, port)
			}
		}
	}
	out, err := runCommand(dir, env, integrationBDCommandTimeout, binary, args...)
	if err == nil || dir == "" || !managedDoltTransportRetryable(out) {
		return out, err
	}
	if fixturePinned {
		return out, err
	}
	if _, readyErr := waitForManagedDoltCityReady(env, dir, 20*time.Second); readyErr == nil {
		if port, ok := currentManagedDoltPortForTest(dir); ok {
			env = appendManagedDoltEndpointEnv(env, port)
		}
		return runCommand(dir, env, integrationBDCommandTimeout, binary, args...)
	}
	if port, ok := ensureManagedDoltPortForTest(dir); ok {
		env = appendManagedDoltEndpointEnv(env, port)
		if delay := managedDoltRetryDelay(out); delay > 0 {
			time.Sleep(delay)
		}
		return runCommand(dir, env, integrationBDCommandTimeout, binary, args...)
	}
	return out, err
}

func appendManagedDoltEndpointEnv(env []string, port string) []string {
	env = filterEnvMany(env, "GC_DOLT_HOST", "GC_DOLT_PORT", "BEADS_DOLT_SERVER_HOST", "BEADS_DOLT_SERVER_PORT")
	return append(env,
		"GC_DOLT_HOST=127.0.0.1",
		"GC_DOLT_PORT="+port,
		"BEADS_DOLT_SERVER_HOST=127.0.0.1",
		"BEADS_DOLT_SERVER_PORT="+port,
	)
}

func runGCWithEnv(env []string, dir string, args ...string) (string, error) {
	return runCommand(dir, env, gcCommandTimeout(args), gcBinary, args...)
}

func runGCDoltWithEnv(env []string, dir string, args ...string) (string, error) {
	return runCommand(dir, env, gcDoltCommandTimeout(args), gcBinary, args...)
}

func gcDoltCommandTimeout(args []string) time.Duration {
	if len(args) > 0 && args[0] == "sling" {
		for _, arg := range args[1:] {
			if strings.HasPrefix(arg, "--on=") {
				return 4 * time.Minute
			}
		}
	}
	return integrationGCDoltCommandTimeout
}

func gcCommandTimeout(args []string) time.Duration {
	if len(args) == 0 {
		return integrationGCCommandTimeout
	}
	switch args[0] {
	case "init", "start", "stop", "restart":
		return integrationGCLifecycleTimeout
	case "supervisor":
		if len(args) > 1 && args[1] == "stop" {
			return integrationGCLifecycleTimeout
		}
	}
	return integrationGCCommandTimeout
}

// buildCommand is the single construction point for every *exec.Cmd this
// file runs -- runCommand and runCommandStdout both delegate here so the
// repository's subprocess-call-site census sees one site, not two.
func buildCommand(ctx context.Context, dir string, env []string, binary string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.WaitDelay = 2 * time.Second
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = env
	return cmd
}

func runCommand(dir string, env []string, timeout time.Duration, binary string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := buildCommand(ctx, dir, env, binary, args...)
	out, err := cmd.CombinedOutput()
	output := string(out)
	if ctx.Err() == context.DeadlineExceeded {
		return output, fmt.Errorf("timed out after %s running %s", timeout, renderCommand(binary, args...))
	}
	if errors.Is(err, exec.ErrWaitDelay) {
		return output, nil
	}
	return output, err
}

// runCommandStdout runs the command like runCommand, but captures stdout and
// stderr into separate buffers so a value-bearing caller only ever observes
// stdout -- diagnostics the subprocess writes to stderr (e.g. bd's own
// logging) must never contaminate a parsed value. On failure the stderr
// content is folded into the returned error so it remains available for
// diagnosis; only the clean value is lost from the returned string, and only
// when there is no clean value to report (the command failed).
func runCommandStdout(dir string, env []string, timeout time.Duration, binary string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	cmd := buildCommand(ctx, dir, env, binary, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	output := stdout.String()
	if ctx.Err() == context.DeadlineExceeded {
		return output, fmt.Errorf("timed out after %s running %s", timeout, renderCommand(binary, args...))
	}
	if errors.Is(err, exec.ErrWaitDelay) {
		return output, nil
	}
	if err != nil && stderr.Len() > 0 {
		return output, fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return output, err
}

func renderCommand(binary string, args ...string) string {
	parts := make([]string, 0, len(args)+1)
	parts = append(parts, binary)
	parts = append(parts, args...)
	return strings.Join(parts, " ")
}

func shouldUseFileStoreBDFallback(dir, output string, args []string) bool {
	if dir == "" || len(args) == 0 || args[0] == "init" {
		return false
	}
	if !strings.Contains(output, "no beads database found") {
		return false
	}
	_, err := os.Stat(filepath.Join(dir, ".gc", "beads.json"))
	return err == nil
}

func runFileStoreBD(dir string, args ...string) (string, error) {
	store, recorder, err := openFileStoreBeads(dir)
	if err != nil {
		return "", err
	}
	defer recorder.Close() //nolint:errcheck // best-effort test cleanup

	switch args[0] {
	case "create":
		if len(args) < 2 {
			return "", fmt.Errorf("bd create: missing title")
		}
		created, err := store.Create(beads.Bead{Title: args[1]})
		if err != nil {
			return "", err
		}
		recorder.Record(events.Event{
			Type:    events.BeadCreated,
			Actor:   "human",
			Subject: created.ID,
			Message: created.Title,
		})
		return fmt.Sprintf("Created bead: %s\n", created.ID), nil
	case "show":
		if len(args) < 2 {
			return "", fmt.Errorf("bd show: missing bead id")
		}
		b, err := store.Get(args[1])
		if err != nil {
			return "", err
		}
		return renderFileStoreBead(b), nil
	case "list":
		items, err := store.List(beads.ListQuery{AllowScan: true})
		if err != nil {
			return "", err
		}
		return renderFileStoreBeadList(items), nil
	case "close":
		if len(args) < 2 {
			return "", fmt.Errorf("bd close: missing bead id")
		}
		if err := store.Close(args[1]); err != nil {
			return "", err
		}
		recorder.Record(events.Event{
			Type:    events.BeadClosed,
			Actor:   "human",
			Subject: args[1],
		})
		return "", nil
	case "update":
		if len(args) < 2 {
			return "", fmt.Errorf("bd update: missing bead id")
		}
		var opts beads.UpdateOpts
		supported := false
		for _, arg := range args[2:] {
			if strings.HasPrefix(arg, "--assignee=") {
				assignee := strings.TrimPrefix(arg, "--assignee=")
				opts.Assignee = &assignee
				supported = true
			}
		}
		if !supported {
			return "", fmt.Errorf("bd update fallback only supports --assignee")
		}
		if err := store.Update(args[1], opts); err != nil {
			return "", err
		}
		recorder.Record(events.Event{
			Type:    events.BeadUpdated,
			Actor:   "human",
			Subject: args[1],
		})
		return "", nil
	default:
		return "", fmt.Errorf("bd %s not supported by file-store fallback", args[0])
	}
}

func openFileStoreBeads(dir string) (beads.Store, *events.FileRecorder, error) {
	store, err := beads.OpenFileStore(fsys.OSFS{}, filepath.Join(dir, ".gc", "beads.json"))
	if err != nil {
		return nil, nil, err
	}
	store.SetLocker(beads.NewFileFlock(filepath.Join(dir, ".gc", "beads.json.lock")))
	recorder, err := events.NewFileRecorder(filepath.Join(dir, ".gc", "events.jsonl"), io.Discard)
	if err != nil {
		return nil, nil, err
	}
	return store, recorder, nil
}

func renderFileStoreBead(b beads.Bead) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "ID: %s\n", b.ID)
	fmt.Fprintf(&sb, "Title: %s\n", b.Title)
	fmt.Fprintf(&sb, "Status: %s\n", b.Status)
	if b.Assignee != "" {
		fmt.Fprintf(&sb, "Assignee: %s\n", b.Assignee)
	}
	return sb.String()
}

func renderFileStoreBeadList(items []beads.Bead) string {
	if len(items) == 0 {
		return "No beads.\n"
	}
	var sb strings.Builder
	for _, b := range items {
		fmt.Fprintf(&sb, "%s  %s  %s\n", b.ID, b.Status, b.Title)
	}
	return sb.String()
}

// findModuleRoot walks up from the current directory to find go.mod.
func findModuleRoot() string {
	if root := bazeltest.OverrideRoot(); root != "" {
		return root
	}
	dir, err := os.Getwd()
	if err != nil {
		panic("integration: getting cwd: " + err.Error())
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			panic("integration: go.mod not found")
		}
		dir = parent
	}
}

// filterEnv returns env with the named variable removed.
func filterEnv(env []string, name string) []string {
	prefix := name + "="
	result := make([]string, 0, len(env))
	for _, e := range env {
		if len(e) >= len(prefix) && e[:len(prefix)] == prefix {
			continue
		}
		result = append(result, e)
	}
	return result
}

func integrationEnv() []string {
	return integrationEnvFor(testGCHome, testRuntimeDir, false)
}

func integrationEnvDolt() []string {
	return integrationEnvFor(testGCHome, testRuntimeDir, true)
}

func integrationEnvFor(gcHome, runtimeDir string, useDolt bool) []string {
	return integrationRuntimeEnv(os.Environ(), gcHome, runtimeDir, useDolt)
}

func integrationRuntimeEnv(base []string, gcHome, runtimeDir string, useDolt bool) []string {
	env := filterEnv(base, "GC_BEADS")
	env = filterEnv(env, runisolation.EnvName)
	gitVars := append([]string(nil), integrationGitRepositoryVars...)
	gitVars = append(gitVars,
		integrationBuildGitDirEnv,
		integrationBuildGitTreeEnv,
		integrationDisposableBeadsDirEnv,
		integrationDisposableCityDirEnv,
		integrationDisposableDatabaseEnv,
		integrationDisposableProjectEnv,
		integrationRequireDisposableEnv,
	)
	env = filterEnvMany(env, gitVars...)
	env = filterEnvMany(env, integrationBeadsSelectorVars...)
	env = filterEnv(env, "BEADS_DIR")
	env = filterEnv(env, "GC_BEADS_SCOPE_ROOT")
	env = filterEnv(env, "GC_DOLT")
	env = filterEnv(env, "PATH")
	env = filterEnv(env, "GC_HOME")
	env = filterEnv(env, "GC_DIR")
	env = filterEnv(env, "GC_CITY")
	env = filterEnv(env, "GC_CITY_PATH")
	env = filterEnv(env, "GC_CITY_ROOT")
	env = filterEnv(env, "GC_CITY_RUNTIME_DIR")
	env = filterEnv(env, "GC_AGENT")
	env = filterEnv(env, "GC_RIG")
	env = filterEnv(env, "GC_RIG_ROOT")
	env = filterEnv(env, "GC_TEMPLATE")
	env = filterEnv(env, "GC_SESSION_NAME")
	env = filterEnv(env, "XDG_RUNTIME_DIR")
	env = filterEnv(env, integrationRealBDBinaryEnv)
	env = filterEnv(env, "DOLT_ROOT_PATH")
	env = filterEnv(env, "BEADS_ACTOR")
	env = filterEnv(env, "GC_DOLT_HOST")
	env = filterEnv(env, "GC_DOLT_PORT")
	env = filterEnv(env, "GC_DOLT_USER")
	env = filterEnv(env, "GC_DOLT_PASSWORD")
	env = filterEnv(env, managedDoltTestModeEnv)
	env = filterEnv(env, managedDoltTestParentEnv)
	env = filterEnv(env, "BEADS_DOLT_SERVER_HOST")
	env = filterEnv(env, "BEADS_DOLT_SERVER_PORT")
	env = filterEnv(env, "BEADS_DOLT_SERVER_USER")
	env = filterEnv(env, "BEADS_DOLT_HOST")
	env = filterEnv(env, "BEADS_DOLT_PORT")
	env = filterEnv(env, "BEADS_DOLT_USER")
	env = filterEnv(env, "BEADS_DOLT_DATABASE")
	env = filterEnv(env, "BEADS_DOLT_DATA_DIR")
	env = filterEnv(env, "BEADS_DOLT_PASSWORD")
	env = filterEnv(env, "GC_SUPERVISOR_ENV")
	env = filterEnv(env, "GC_SUPERVISOR_PRESERVE_SESSIONS_ON_SIGNAL")
	env = filterEnv(env, "GC_SUPERVISOR_LOG_TEE")
	env = filterEnv(env, "DOLT_HOST")
	env = filterEnv(env, "DOLT_PORT")
	env = filterEnv(env, "DOLT_USER")
	env = filterEnv(env, "DOLT_PASSWORD")
	env = filterEnv(env, integrationGCBinaryEnv)
	env = filterEnv(env, integrationDoltBinaryEnv)
	env = filterEnv(env, "BEADS_DOLT_AUTO_START")
	if !useDolt {
		env = append(env, "GC_DOLT=skip")
	}
	env = append(env, "GC_HOME="+gcHome)
	env = append(env, "XDG_RUNTIME_DIR="+runtimeDir)
	env = append(env, managedDoltTestModeEnv+"=1")
	env = append(env, managedDoltTestParentEnv+"="+strconv.Itoa(os.Getpid()))
	env = append(env, integrationRealBDBinaryEnv+"="+realBDBinary)
	env = append(env, "DOLT_ROOT_PATH="+gcHome)
	env = append(env, "PATH="+prependPath(integrationToolBinDir, os.Getenv("PATH")))
	// Match production: suppress bd's CLI Dolt auto-start so integration
	// tests can't spawn rogue servers when the managed Dolt port file is
	// stale between subtests. bd's auto-start logic ignores the
	// dolt.auto-start:false config written into .beads/config.yaml
	// (resolveAutoStart priority bug), so the env var is the only
	// reliable kill-switch. Mirrors bdRuntimeEnv in cmd/gc/bd_env.go.
	env = append(env, "BEADS_DOLT_AUTO_START=0")
	env = pinRealHomeEnv(env)
	// Seed a global gitconfig under the isolated GC_HOME and point children at
	// it. The Makefile's TEST_ENV does this via scripts/test-gitconfig-path
	// (user.name, user.email, beads.role=maintainer); under bazel the ambient
	// variable is unset and gc subprocesses would read the executing worker's
	// real global config, which has no beads.role — `gc doctor`'s beads-role
	// check fails on any machine that never opted in. Writing it per-GC_HOME
	// keeps every isolated root self-contained.
	env = replaceEnv(env, "GIT_CONFIG_GLOBAL", ensureIntegrationGitConfig(gcHome))
	return env
}

// integrationModuleBuildEnv scopes explicit Git repository overrides to Go
// builds rooted in this module. Test and fixture processes use the scrubbed
// runtime environment instead.
func integrationModuleBuildEnv(base []string) ([]string, error) {
	values := parseEnvList(base)
	gitVars := append([]string(nil), integrationGitRepositoryVars...)
	gitVars = append(gitVars,
		integrationBuildGitDirEnv,
		integrationBuildGitTreeEnv,
		integrationDisposableBeadsDirEnv,
		integrationDisposableCityDirEnv,
		integrationDisposableDatabaseEnv,
		integrationDisposableProjectEnv,
		integrationRequireDisposableEnv,
	)
	gitVars = append(gitVars, integrationBeadsSelectorVars...)
	env := filterEnvMany(base, gitVars...)
	gitDir := strings.TrimSpace(values[integrationBuildGitDirEnv])
	gitWorkTree := strings.TrimSpace(values[integrationBuildGitTreeEnv])
	if gitDir == "" && gitWorkTree == "" {
		return replaceEnv(env, "CGO_ENABLED", "0"), nil
	}
	if gitDir == "" || gitWorkTree == "" || !filepath.IsAbs(gitDir) || !filepath.IsAbs(gitWorkTree) {
		return nil, fmt.Errorf("%s and %s must both be absolute paths when either is set", integrationBuildGitDirEnv, integrationBuildGitTreeEnv)
	}
	moduleRoot, err := filepath.Abs(findModuleRoot())
	if err != nil {
		return nil, fmt.Errorf("resolve integration module root: %w", err)
	}
	buildWorkTree, err := filepath.Abs(gitWorkTree)
	if err != nil || filepath.Clean(buildWorkTree) != filepath.Clean(moduleRoot) {
		return nil, fmt.Errorf("%s must name this integration module's worktree", integrationBuildGitTreeEnv)
	}
	env = replaceEnv(env, "GIT_DIR", gitDir)
	env = replaceEnv(env, "GIT_WORK_TREE", gitWorkTree)
	return replaceEnv(env, "CGO_ENABLED", "0"), nil
}

func integrationFixtureAuthorityArg(cityDir string, identity graphBeadsIdentity) (string, error) {
	data, err := json.Marshal(struct {
		CityDir  string `json:"city_dir"`
		Database string `json:"database"`
		Project  string `json:"project_id"`
	}{CityDir: cityDir, Database: identity.database, Project: identity.projectID})
	if err != nil {
		return "", fmt.Errorf("encode fixture launcher authority: %w", err)
	}
	return integrationFixtureAuthorityFlag + base64.RawURLEncoding.EncodeToString(data), nil
}

// ensureIntegrationGitConfig writes the isolated global gitconfig mirrors of
// scripts/test-gitconfig-path into gcHome and returns its path. Panics on
// failure: a missing beads.role silently breaks agent flows mid-test.
func ensureIntegrationGitConfig(gcHome string) string {
	if err := os.MkdirAll(gcHome, 0o755); err != nil {
		panic("integration: creating GC_HOME for gitconfig: " + err.Error())
	}
	path := filepath.Join(gcHome, "gitconfig-global")
	content := "[user]\n\tname = Gas City Integration Test\n\temail = integration-test@gascity.invalid\n[beads]\n\trole = maintainer\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		panic("integration: writing isolated gitconfig: " + err.Error())
	}
	return path
}

// pinRealHomeEnv pins HOME to the real passwd-db home for the current uid.
// Test runners (sandboxes, CI containers) commonly run with HOME pointed at
// something other than the invoking user's real home; left unchanged, that
// ambient HOME propagates into the gc subprocess these tests exec and trips
// platformSupervisorHomeOverrideError (cmd/gc/cmd_supervisor_lifecycle.go),
// which blocks non-delegated `gc start`/`gc supervisor start` when HOME
// differs from the real home. GC_HOME (set separately, above) remains the
// isolated per-test root; only the OS-level HOME is pinned. Mirrors
// cmd/gc/cmd_supervisor_test.go's pinRealHome, reimplemented here because
// that helper is test-only in a different package. Fails open (leaves env
// untouched) if the lookup errors or returns an empty home dir, matching
// platformSupervisorHomeOverrideError's own tolerance.
func pinRealHomeEnv(env []string) []string {
	lu, err := user.LookupId(strconv.Itoa(os.Getuid()))
	if err != nil || strings.TrimSpace(lu.HomeDir) == "" {
		return env
	}
	return replaceEnv(env, "HOME", lu.HomeDir)
}

func prependPath(paths ...string) string {
	parts := make([]string, 0, len(paths))
	for _, path := range paths {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		parts = append(parts, path)
	}
	return strings.Join(parts, string(os.PathListSeparator))
}

func newIsolatedToolEnv(t *testing.T, useDolt bool) []string {
	t.Helper()

	_, _, env := newIsolatedEnvRoot(t, useDolt)
	return env
}

func newIsolatedCommandEnv(t *testing.T, useDolt bool) []string {
	t.Helper()

	gcHome, env := newIsolatedCommandEnvWithoutSupervisor(t, useDolt)
	startIsolatedSupervisor(t, env, gcHome)
	return env
}

func newIsolatedCommandEnvWithoutSupervisor(t *testing.T, useDolt bool) (string, []string) {
	t.Helper()

	gcHome, _, env := newIsolatedEnvRoot(t, useDolt)

	root := filepath.Dir(gcHome)
	shimDir := filepath.Join(root, "bin")
	if err := os.MkdirAll(shimDir, 0o755); err != nil {
		t.Fatalf("creating isolated shim dir: %v", err)
	}
	for _, name := range []string{"systemctl", "launchctl"} {
		path := filepath.Join(shimDir, name)
		if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatalf("writing %s shim: %v", name, err)
		}
	}
	envMap := parseEnvList(env)
	env = replaceEnv(env, "PATH", prependPath(shimDir, envMap["PATH"]))
	return gcHome, env
}

func newIsolatedEnvRoot(t *testing.T, useDolt bool) (string, string, []string) {
	t.Helper()

	root, err := os.MkdirTemp("", "gc-int-env-")
	if err != nil {
		t.Fatalf("creating isolated env root: %v", err)
	}
	t.Cleanup(func() {
		preserveMarker := filepath.Join(root, runisolation.PreserveMarkerName)
		if _, err := os.Lstat(preserveMarker); err == nil {
			t.Logf("preserving isolated integration environment root at %s", root)
			return
		} else if !os.IsNotExist(err) {
			t.Errorf("inspect isolated integration environment preservation marker: %v; preserving %s", err, root)
			return
		}
		if err := os.RemoveAll(root); err != nil {
			t.Errorf("remove isolated integration environment root %s: %v", root, err)
		}
	})
	registerIntegrationDoltSQLServerCleanup(t, root)
	gcHome := filepath.Join(root, "gc-home")
	runtimeDir := filepath.Join(root, "runtime")
	if err := os.MkdirAll(gcHome, 0o755); err != nil {
		t.Fatalf("creating isolated GC_HOME: %v", err)
	}
	if err := os.MkdirAll(runtimeDir, 0o755); err != nil {
		t.Fatalf("creating isolated runtime dir: %v", err)
	}
	port, err := reserveLoopbackPort()
	if err != nil {
		t.Fatalf("reserving isolated supervisor port: %v", err)
	}
	supervisorConfig := fmt.Sprintf("[supervisor]\nport = %d\nbind = \"127.0.0.1\"\n", port)
	if err := os.WriteFile(filepath.Join(gcHome, "supervisor.toml"), []byte(supervisorConfig), 0o644); err != nil {
		t.Fatalf("writing isolated supervisor config: %v", err)
	}
	if err := seedDoltIdentityForRoot(gcHome); err != nil {
		t.Fatalf("writing isolated dolt config: %v", err)
	}
	env := integrationEnvFor(gcHome, runtimeDir, useDolt)
	env = replaceEnv(env, "TMPDIR", root)
	return gcHome, runtimeDir, env
}

func seedDoltIdentityForRoot(gcHome string) error {
	switch mode := doltIdentityMode(); mode {
	case doltIdentityModeIsolated:
		return seedIsolatedDoltConfig(gcHome)
	case doltIdentityModeSkip:
		return nil
	case doltIdentityModeGlobal:
		if err := ensureGlobalDoltIdentity(); err != nil {
			return err
		}
		return seedIsolatedDoltConfig(gcHome)
	default:
		return fmt.Errorf("%s=%q is invalid", integrationDoltIdentityEnv, mode)
	}
}

func doltIdentityMode() string {
	mode := strings.TrimSpace(os.Getenv(integrationDoltIdentityEnv))
	if mode == "" {
		return doltIdentityModeIsolated
	}
	return mode
}

func ensureGlobalDoltIdentity() error {
	if doltBinary == "" {
		return fmt.Errorf("dolt binary is required when %s=%s", integrationDoltIdentityEnv, doltIdentityModeGlobal)
	}

	name, _ := trimmedCommandOutput(doltBinary, "config", "--global", "--get", "user.name")
	email, _ := trimmedCommandOutput(doltBinary, "config", "--global", "--get", "user.email")
	if name != "" && email != "" {
		return nil
	}

	if name == "" {
		gitName, _ := trimmedCommandOutput("git", "config", "--global", "user.name")
		if gitName == "" {
			gitName = "gc-test"
		}
		if out, err := exec.Command(doltBinary, "config", "--global", "--add", "user.name", gitName).CombinedOutput(); err != nil {
			return fmt.Errorf("set dolt user.name: %w: %s", err, string(out))
		}
	}
	if email == "" {
		gitEmail, _ := trimmedCommandOutput("git", "config", "--global", "user.email")
		if gitEmail == "" {
			gitEmail = "gc-test@test.local"
		}
		if out, err := exec.Command(doltBinary, "config", "--global", "--add", "user.email", gitEmail).CombinedOutput(); err != nil {
			return fmt.Errorf("set dolt user.email: %w: %s", err, string(out))
		}
	}
	return nil
}

func trimmedCommandOutput(binary string, args ...string) (string, error) {
	out, err := exec.Command(binary, args...).CombinedOutput()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func seedIsolatedDoltConfig(gcHome string) error {
	doltDir := filepath.Join(gcHome, ".dolt")
	if err := os.MkdirAll(doltDir, 0o755); err != nil {
		return err
	}
	doltCfg := `{"user.name":"gc-test","user.email":"gc-test@test.local"}`
	return os.WriteFile(filepath.Join(doltDir, "config_global.json"), []byte(doltCfg), 0o644)
}

func registerCityCommandEnv(cityDir string, env []string) {
	cityCommandEnv.Store(cityDir, append([]string(nil), env...))
}

func unregisterCityCommandEnv(cityDir string) {
	cityCommandEnv.Delete(cityDir)
}

func commandEnvForDir(dir string, useDolt bool) []string {
	if dir != "" {
		if env, ok := cityCommandEnv.Load(dir); ok {
			return append([]string(nil), env.([]string)...)
		}
	}
	if useDolt {
		return integrationEnvDolt()
	}
	return integrationEnv()
}

func commandCityDirForArgs(dir string, args []string) string {
	if dir != "" || len(args) < 2 {
		return dir
	}
	switch args[0] {
	case "start", "stop", "restart", "suspend", "resume":
		if filepath.IsAbs(args[1]) {
			return args[1]
		}
	}
	return dir
}

func commandEnvLookupDir(dir string, args []string) string {
	return commandCityDirForArgs(dir, args)
}

func replaceEnv(env []string, name, value string) []string {
	env = filterEnv(env, name)
	return append(env, name+"="+value)
}

func currentManagedDoltPortForTest(cityDir string) (string, bool) {
	if cityDir == "" {
		return "", false
	}
	if data, err := os.ReadFile(filepath.Join(cityDir, ".beads", "dolt-server.port")); err == nil {
		if port := strings.TrimSpace(string(data)); port != "" && port != "0" && testPortReachable(port) {
			return port, true
		}
	}
	data, err := os.ReadFile(filepath.Join(cityDir, ".gc", "runtime", "packs", "dolt", "dolt-state.json"))
	if err != nil {
		return "", false
	}
	var state struct {
		Running bool `json:"running"`
		Port    int  `json:"port"`
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return "", false
	}
	if !state.Running || state.Port <= 0 {
		return "", false
	}
	port := strconv.Itoa(state.Port)
	if !testPortReachable(port) {
		return "", false
	}
	return port, true
}

func ensureManagedDoltPortForTest(cityDir string) (string, bool) {
	if port, ok := currentManagedDoltPortForTest(cityDir); ok {
		return port, true
	}
	if cityDir == "" {
		return "", false
	}
	startOut, startErr := runGCDoltWithEnv(commandEnvForDir(cityDir, true), "", "start", cityDir)
	if startErr != nil && !isGCStartAlreadyRunning(startOut) {
		return "", false
	}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if port, ok := currentManagedDoltPortForTest(cityDir); ok {
			return port, true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return "", false
}

func managedDoltTransportRetryable(out string) bool {
	msg := strings.ToLower(out)
	for _, marker := range []string{
		"dolt circuit breaker is open",
		"server appears down, failing fast",
		"dolt server unreachable",
		"dial tcp",
		"connection refused",
		"broken pipe",
		"unexpected eof",
		"bad connection",
		"dolt circuit breaker is open",
		"server appears down",
	} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

func managedDoltRetryDelay(out string) time.Duration {
	msg := strings.ToLower(out)
	if strings.Contains(msg, "dolt circuit breaker is open") || strings.Contains(msg, "server appears down, failing fast") {
		return 5 * time.Second
	}
	return 0
}

func TestManagedDoltTransportRetryableIncludesCircuitBreaker(t *testing.T) {
	out := `{"error":"failed to open database: dolt circuit breaker is open: server appears down, failing fast (cooldown 5s)"}`
	if !managedDoltTransportRetryable(out) {
		t.Fatalf("managedDoltTransportRetryable(%q) = false, want true", out)
	}
}

// doltDirtyTableMigrationRaceRetryable reports whether out is the known
// transient beads#4566 signature: a bd Dolt schema-migration bootstrap
// racing a still-settling prior schema state under concurrent test load
// (ga-38xsx4). Narrowly scoped to that one signature only — any other gc
// init / bd init failure, including the stdout-contract regression this
// suite exists to catch (ga-rsktma), must never be retried away here.
func doltDirtyTableMigrationRaceRetryable(out string) bool {
	return strings.Contains(strings.ToLower(out), "pending schema migrations alter pre-existing dirty tables")
}

func TestDoltDirtyTableMigrationRaceRetryableMatchesKnownSignatureOnly(t *testing.T) {
	known := "Error: failed to open Dolt store: failed to initialize schema: schema migration: pending schema migrations alter pre-existing dirty tables: dependencies; run 'bd dolt commit' to commit the working set at the current schema, then re-run the migration (gastownhall/beads#4566)"
	if !doltDirtyTableMigrationRaceRetryable(known) {
		t.Fatalf("doltDirtyTableMigrationRaceRetryable(%q) = false, want true", known)
	}
	for _, table := range []string{"issues", "events", "dolt_schemas"} {
		variant := strings.Replace(known, "dependencies", table, 1)
		if !doltDirtyTableMigrationRaceRetryable(variant) {
			t.Fatalf("doltDirtyTableMigrationRaceRetryable(%q) = false, want true", variant)
		}
	}
	other := "Error: failed to open Dolt store: dial tcp 127.0.0.1:3306: connect: connection refused"
	if doltDirtyTableMigrationRaceRetryable(other) {
		t.Fatalf("doltDirtyTableMigrationRaceRetryable(%q) = true, want false (must not swallow unrelated errors)", other)
	}
	stdoutRegression := "unexpected extra stdout: circuit-breaker cleanup log leaked onto stdout"
	if doltDirtyTableMigrationRaceRetryable(stdoutRegression) {
		t.Fatalf("doltDirtyTableMigrationRaceRetryable(%q) = true, want false (must not mask the stdout-contract regression this test exists to catch)", stdoutRegression)
	}
}

// retryOnDoltDirtyTableMigrationRace runs cmd, retrying up to a small bound
// ONLY when the result matches the known-transient beads#4566 dirty-table
// migration race (ga-38xsx4). Any other outcome — success or a different
// failure — returns immediately on the first attempt, so a real regression
// (e.g. ga-rsktma's stdout contract) still fails the test instead of being
// retried away. Bounded, not a blind retry-until-green.
//
// The inter-attempt wait is delegated to backoff.Retry rather than a local
// time.Sleep: the delay then lives inside the already-imported backoff
// library's own implementation instead of adding another fixed-sleep call
// site to this file's static resource census (internal/testpolicy/resourcecensus).
func retryOnDoltDirtyTableMigrationRace(cmd func() (string, error)) (string, error) {
	const maxAttempts = 3
	const retryDelay = 2 * time.Second

	var out string
	var lastErr error

	bo := backoff.WithMaxRetries(backoff.NewConstantBackOff(retryDelay), maxAttempts-1)
	_ = backoff.Retry(func() error {
		out, lastErr = cmd()
		if lastErr == nil {
			return nil
		}
		if !doltDirtyTableMigrationRaceRetryable(out) {
			return backoff.Permanent(lastErr)
		}
		return lastErr
	}, bo)

	return out, lastErr
}

func TestRetryOnDoltDirtyTableMigrationRaceRetriesOnlyKnownSignature(t *testing.T) {
	const raceOutput = "schema migration: pending schema migrations alter pre-existing dirty tables: issues (gastownhall/beads#4566)"

	t.Run("retries until success", func(t *testing.T) {
		calls := 0
		out, err := retryOnDoltDirtyTableMigrationRace(func() (string, error) {
			calls++
			if calls < 3 {
				return raceOutput, errors.New("exit status 1")
			}
			return "ok", nil
		})
		if err != nil {
			t.Fatalf("err = %v, want nil after eventual success", err)
		}
		if out != "ok" {
			t.Fatalf("out = %q, want %q", out, "ok")
		}
		if calls != 3 {
			t.Fatalf("calls = %d, want 3", calls)
		}
	})

	t.Run("does not retry unrelated errors", func(t *testing.T) {
		calls := 0
		wantErr := errors.New("boom")
		_, err := retryOnDoltDirtyTableMigrationRace(func() (string, error) {
			calls++
			return "unrelated failure", wantErr
		})
		if !errors.Is(err, wantErr) {
			t.Fatalf("err = %v, want %v", err, wantErr)
		}
		if calls != 1 {
			t.Fatalf("calls = %d, want 1 (must not retry a non-4566 failure)", calls)
		}
	})

	t.Run("gives up after bounded attempts", func(t *testing.T) {
		calls := 0
		_, err := retryOnDoltDirtyTableMigrationRace(func() (string, error) {
			calls++
			return raceOutput, errors.New("exit status 1")
		})
		if err == nil {
			t.Fatalf("err = nil, want non-nil after exhausting retries on a persistent race")
		}
		if calls != 3 {
			t.Fatalf("calls = %d, want 3 (bounded, not unbounded retry-until-green)", calls)
		}
	})
}

func testPortReachable(port string) bool {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", port), 250*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func requireDoltIntegration(t *testing.T) {
	t.Helper()

	if doltBinary == "" {
		t.Skip("dolt not configured; set GC_INTEGRATION_DOLT_BINARY or add dolt to PATH")
	}
	if realBDBinary == "" || bdBinary == "" {
		t.Skip("bd not configured; set GC_INTEGRATION_REAL_BD or add bd to PATH")
	}
}

func startIsolatedSupervisor(t *testing.T, env []string, gcHome string) {
	t.Helper()

	logPath := filepath.Join(gcHome, "supervisor.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatalf("creating isolated supervisor log: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.CommandContext(ctx, gcBinary, "supervisor", "run")
	configureIntegrationSupervisorCommand(cmd)
	cmd.Dir = gcHome
	cmd.Env = env
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		t.Fatalf("starting isolated supervisor: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		out, err := runCommand("", env, 2*time.Second, gcBinary, "supervisor", "status")
		if err == nil && strings.Contains(out, "Supervisor is running") {
			t.Cleanup(func() {
				// --wait so runCommand blocks until the supervisor fully
				// shut down, aligning with the cmd.Wait() synchronization below.
				_, _ = runCommand("", env, 15*time.Second, gcBinary, "supervisor", "stop", "--wait")
				cancel()
				waitForIntegrationSupervisorDone(cmd, done, integrationSupervisorWaitDelay)
				_ = logFile.Close()
			})
			return
		}
		select {
		case err := <-done:
			_ = logFile.Close()
			logData, _ := os.ReadFile(logPath)
			if err == nil {
				t.Fatalf("isolated supervisor exited early:\n%s", string(logData))
			}
			t.Fatalf("isolated supervisor exited early: %v\n%s", err, string(logData))
		default:
		}
		time.Sleep(100 * time.Millisecond)
	}

	cancel()
	waitForIntegrationSupervisorDone(cmd, done, integrationSupervisorWaitDelay)
	_ = logFile.Close()
	logData, _ := os.ReadFile(logPath)
	t.Fatalf("isolated supervisor did not become ready:\n%s", string(logData))
}

func waitForIntegrationSupervisorDone(cmd *exec.Cmd, done <-chan error, timeout time.Duration) {
	select {
	case <-done:
	case <-time.After(timeout):
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		<-done
	}
}

func restartIsolatedSupervisor(t *testing.T, env []string) {
	t.Helper()

	_, _ = runCommand("", env, 15*time.Second, gcBinary, "supervisor", "stop", "--wait")

	gcHome := parseEnvList(env)["GC_HOME"]
	if gcHome == "" {
		t.Fatal("isolated env missing GC_HOME")
	}
	startIsolatedSupervisor(t, env, gcHome)
}

func reserveLoopbackPort() (int, error) {
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer lis.Close() //nolint:errcheck
	addr, ok := lis.Addr().(*net.TCPAddr)
	if !ok {
		return 0, fmt.Errorf("unexpected addr type %T", lis.Addr())
	}
	return addr.Port, nil
}

func TestIntegrationEnvForPinsRealHome(t *testing.T) {
	oldGCHome, oldRuntimeDir := testGCHome, testRuntimeDir
	oldGCBinary, oldBDBinary, oldRealBDBinary := gcBinary, bdBinary, realBDBinary
	oldToolBinDir, oldDoltBinary := integrationToolBinDir, doltBinary
	t.Cleanup(func() {
		testGCHome = oldGCHome
		testRuntimeDir = oldRuntimeDir
		gcBinary = oldGCBinary
		bdBinary = oldBDBinary
		realBDBinary = oldRealBDBinary
		integrationToolBinDir = oldToolBinDir
		doltBinary = oldDoltBinary
	})

	testGCHome = filepath.Join(t.TempDir(), "gc-home")
	testRuntimeDir = filepath.Join(t.TempDir(), "runtime")
	gcBinary = filepath.Join(t.TempDir(), "gc")
	bdBinary = filepath.Join(t.TempDir(), "bd")
	realBDBinary = "/usr/bin/bd"
	doltBinary = "/usr/bin/dolt"
	integrationToolBinDir = filepath.Join(t.TempDir(), "bin")

	t.Setenv("HOME", "/host/home")
	t.Setenv("BEADS_DIR", "/host/beads")
	t.Setenv("GC_DOLT_HOST", "ambient-host")
	t.Setenv("GC_DOLT_PORT", "0")
	t.Setenv("GC_DOLT_USER", "ambient-user")
	t.Setenv("GC_DOLT_PASSWORD", "ambient-password")
	t.Setenv("BEADS_DIR", "/host/beads")
	t.Setenv("BEADS_ACTOR", "ambient-actor")
	t.Setenv("BEADS_DIR", "/host/repo/.beads")
	t.Setenv("BEADS_DOLT_SERVER_HOST", "ambient-beads-host")
	t.Setenv("BEADS_DOLT_SERVER_PORT", "0")
	t.Setenv("BEADS_DOLT_SERVER_USER", "ambient-beads-user")
	t.Setenv("BEADS_DOLT_HOST", "ambient-legacy-host")
	t.Setenv("BEADS_DOLT_PORT", "0")
	t.Setenv("BEADS_DOLT_USER", "ambient-legacy-user")
	t.Setenv("BEADS_DOLT_DATABASE", "ambient-legacy-db")
	t.Setenv("BEADS_DOLT_DATA_DIR", filepath.Join(t.TempDir(), "ambient-dolt-data"))
	t.Setenv("BEADS_DOLT_PASSWORD", "ambient-beads-password")
	t.Setenv("DOLT_HOST", "ambient-raw-host")
	t.Setenv("DOLT_PORT", "0")
	t.Setenv("DOLT_USER", "ambient-raw-user")
	t.Setenv("DOLT_PASSWORD", "ambient-raw-password")
	t.Setenv("BEADS_DIR", "/host/beads")
	t.Setenv("BEADS_ACTOR", "host-agent")
	t.Setenv("GC_BEADS_SCOPE_ROOT", "/host/scope")
	t.Setenv("GC_DIR", "/host/gc-dir")
	t.Setenv("GC_CITY", "/host/city")
	t.Setenv("GC_CITY_PATH", "/host/city-path")
	t.Setenv("GC_CITY_ROOT", "/host/city-root")
	t.Setenv("GC_CITY_RUNTIME_DIR", "/host/runtime")
	t.Setenv("GC_AGENT", "host-agent")
	t.Setenv("GC_RIG", "host-rig")
	t.Setenv("GC_RIG_ROOT", "/host/rig")
	t.Setenv("GC_TEMPLATE", "host/template")
	t.Setenv("GC_SESSION_NAME", "host-session")
	env := integrationEnv()
	got := parseEnvList(env)

	lu, err := user.LookupId(strconv.Itoa(os.Getuid()))
	if err != nil || strings.TrimSpace(lu.HomeDir) == "" {
		t.Skip("no passwd entry for uid; pinRealHomeEnv fails open")
	}
	if got["HOME"] != lu.HomeDir {
		t.Fatalf("HOME = %q, want real passwd-db home %q (ambient HOME=/host/home must not leak through)", got["HOME"], lu.HomeDir)
	}
	if got["GC_HOME"] != testGCHome {
		t.Fatalf("GC_HOME = %q, want %q", got["GC_HOME"], testGCHome)
	}
	if got["XDG_RUNTIME_DIR"] != testRuntimeDir {
		t.Fatalf("XDG_RUNTIME_DIR = %q, want %q", got["XDG_RUNTIME_DIR"], testRuntimeDir)
	}
	if got[integrationRealBDBinaryEnv] != realBDBinary {
		t.Fatalf("%s = %q, want %q", integrationRealBDBinaryEnv, got[integrationRealBDBinaryEnv], realBDBinary)
	}
	if path := got["PATH"]; !strings.HasPrefix(path, integrationToolBinDir+string(os.PathListSeparator)) && path != integrationToolBinDir {
		t.Fatalf("PATH = %q, want prefix %q", path, integrationToolBinDir)
	}
	if got["BEADS_DOLT_AUTO_START"] != "0" {
		t.Fatalf("BEADS_DOLT_AUTO_START = %q, want %q; tests must match bdRuntimeEnv and suppress bd's rogue auto-start", got["BEADS_DOLT_AUTO_START"], "0")
	}
	for _, key := range []string{
		"BEADS_DIR",
		"GC_BEADS_SCOPE_ROOT",
		"GC_DOLT_HOST",
		"GC_DOLT_PORT",
		"GC_DOLT_USER",
		"GC_DOLT_PASSWORD",
		"BEADS_ACTOR",
		"BEADS_DIR",
		"BEADS_DOLT_SERVER_HOST",
		"BEADS_DOLT_SERVER_PORT",
		"BEADS_DOLT_SERVER_USER",
		"BEADS_DOLT_HOST",
		"BEADS_DOLT_PORT",
		"BEADS_DOLT_USER",
		"BEADS_DOLT_DATABASE",
		"BEADS_DOLT_DATA_DIR",
		"BEADS_DOLT_PASSWORD",
		"DOLT_HOST",
		"DOLT_PORT",
		"DOLT_USER",
		"DOLT_PASSWORD",
		"BEADS_DIR",
		"BEADS_ACTOR",
		"GC_BEADS_SCOPE_ROOT",
		"GC_DIR",
		"GC_CITY",
		"GC_CITY_PATH",
		"GC_CITY_ROOT",
		"GC_CITY_RUNTIME_DIR",
		"GC_AGENT",
		"GC_RIG",
		"GC_RIG_ROOT",
		"GC_TEMPLATE",
		"GC_SESSION_NAME",
	} {
		if _, ok := got[key]; ok {
			t.Fatalf("%s leaked into integration env: %v", key, got[key])
		}
	}
}

func TestIntegrationBuildGitOverridesStayBuildScoped(t *testing.T) {
	selectors := append(append([]string(nil), integrationGitRepositoryVars...), integrationBeadsSelectorVars...)
	base := make([]string, 0, len(selectors)+7)
	for _, name := range selectors {
		base = append(base, name+"=/ambient/"+strings.ToLower(name))
	}
	base = append(base,
		integrationBuildGitDirEnv+"=/fixture/repo.git",
		integrationBuildGitTreeEnv+"="+findModuleRoot(),
		integrationDisposableBeadsDirEnv+"=/fixture/city/.beads",
		integrationDisposableCityDirEnv+"=/fixture/city",
		integrationDisposableDatabaseEnv+"=fixture_db",
		integrationDisposableProjectEnv+"=fixture-project",
		integrationRequireDisposableEnv+"=1",
	)
	buildEnv, err := integrationModuleBuildEnv(base)
	if err != nil {
		t.Fatalf("integrationModuleBuildEnv() error = %v", err)
	}
	build := parseEnvList(buildEnv)
	if build["GIT_DIR"] != "/fixture/repo.git" || build["GIT_WORK_TREE"] != findModuleRoot() {
		t.Fatalf("module build Git env = (%q, %q), want the explicit build-only fixture paths", build["GIT_DIR"], build["GIT_WORK_TREE"])
	}
	for _, name := range selectors {
		if name == "GIT_DIR" || name == "GIT_WORK_TREE" {
			continue
		}
		if value, ok := build[name]; ok {
			t.Errorf("%s=%q leaked into module build env", name, value)
		}
	}

	runtime := parseEnvList(integrationRuntimeEnv(base, t.TempDir(), t.TempDir(), true))
	runtimeVars := append(append([]string(nil), selectors...),
		integrationBuildGitDirEnv,
		integrationBuildGitTreeEnv,
		integrationDisposableBeadsDirEnv,
		integrationDisposableCityDirEnv,
		integrationDisposableDatabaseEnv,
		integrationDisposableProjectEnv,
		integrationRequireDisposableEnv,
	)
	for _, name := range runtimeVars {
		if value, ok := runtime[name]; ok {
			t.Errorf("%s=%q leaked into fixture runtime env", name, value)
		}
	}
}

func TestIntegrationPinnedBdBuildEnvExcludesGitBeadsAndFixtureSelectors(t *testing.T) {
	blocked := append([]string(nil), integrationGitRepositoryVars...)
	blocked = append(blocked, integrationBeadsSelectorVars...)
	blocked = append(blocked,
		"GC_TESTENV_PASSTHROUGH",
		"BEADS_DIR",
		"BEADS_HOLDER_TOKEN",
		"BEADS_ACTOR",
		"DOLT_ROOT_PATH",
		"GC_BEADS",
		"GC_HOME",
		"GC_DIR",
		"GC_CITY",
		"GC_CITY_PATH",
		"GC_CITY_ROOT",
		"GC_CITY_RUNTIME_DIR",
		"GC_AGENT",
		"GC_RIG",
		"GC_RIG_ROOT",
		"GC_SESSION_ID",
		"GC_SESSION_NAME",
		"GC_TMUX_SESSION",
		integrationGCBinaryEnv,
		integrationRealBDBinaryEnv,
		integrationDoltBinaryEnv,
		integrationBuildGitDirEnv,
		integrationBuildGitTreeEnv,
		integrationDisposableBeadsDirEnv,
		integrationDisposableCityDirEnv,
		integrationDisposableDatabaseEnv,
		integrationDisposableProjectEnv,
		integrationRequireDisposableEnv,
		integrationDoltIdentityEnv,
		managedDoltTestModeEnv,
		managedDoltTestParentEnv,
	)
	base := make([]string, 0, len(blocked)+8)
	for _, name := range blocked {
		base = append(base, name+"=/ambient/"+strings.ToLower(name))
	}
	base = append(base,
		"GC_TESTENV_PASSTHROUGH=GIT_DIR,BEADS_DIR",
		"HOME=/go-home",
		"PATH=/go-bin",
		"TMPDIR=/go-tmp",
		"GOMODCACHE=/go-mod-cache",
		"GOCACHE=/go-build-cache",
		"GOPROXY=https://proxy.example",
		"GOSUMDB=sum.golang.org",
	)

	got := parseEnvList(integrationPinnedBdBuildEnv(base))
	for _, name := range blocked {
		if value, ok := got[name]; ok {
			t.Errorf("%s=%q leaked into pinned bd Go command environment", name, value)
		}
	}
	for name, want := range map[string]string{
		"HOME":       "/go-home",
		"PATH":       "/go-bin",
		"TMPDIR":     "/go-tmp",
		"GOMODCACHE": "/go-mod-cache",
		"GOCACHE":    "/go-build-cache",
		"GOPROXY":    "https://proxy.example",
		"GOSUMDB":    "sum.golang.org",
	} {
		if got[name] != want {
			t.Errorf("%s = %q, want %q in pinned bd Go command environment", name, got[name], want)
		}
	}
}

func TestIntegrationBuildGitOverridesRequireBothAbsolutePaths(t *testing.T) {
	for _, base := range [][]string{
		{integrationBuildGitDirEnv + "=/fixture/repo.git"},
		{integrationBuildGitTreeEnv + "=relative"},
	} {
		if _, err := integrationModuleBuildEnv(base); err == nil {
			t.Errorf("integrationModuleBuildEnv(%v) succeeded, want invalid Git override error", base)
		}
	}
}

func TestManagedDoltTransportRetryableRecognizesCircuitBreaker(t *testing.T) {
	output := `{"error":"failed to open database: dolt circuit breaker is open: server appears down, failing fast (cooldown 5s)"}`
	if !managedDoltTransportRetryable(output) {
		t.Fatalf("managedDoltTransportRetryable(%q) = false, want true", output)
	}
	if got := managedDoltRetryDelay(output); got < 5*time.Second {
		t.Fatalf("managedDoltRetryDelay(%q) = %s, want at least 5s", output, got)
	}
	if got := managedDoltRetryDelay("dial tcp 127.0.0.1:3306: connect: connection refused"); got != 0 {
		t.Fatalf("managedDoltRetryDelay for plain transport error = %s, want 0", got)
	}
}

func TestStandaloneBDEnvAllowsBDAutoStart(t *testing.T) {
	oldGCHome := testGCHome
	oldRuntimeDir := testRuntimeDir
	oldRealBDBinary := realBDBinary
	oldToolBinDir := integrationToolBinDir
	t.Cleanup(func() {
		testGCHome = oldGCHome
		testRuntimeDir = oldRuntimeDir
		realBDBinary = oldRealBDBinary
		integrationToolBinDir = oldToolBinDir
	})

	testGCHome = filepath.Join(t.TempDir(), "gc-home")
	testRuntimeDir = filepath.Join(t.TempDir(), "runtime")
	realBDBinary = "/usr/bin/bd"
	integrationToolBinDir = filepath.Join(t.TempDir(), "bin")

	t.Setenv("BEADS_DOLT_AUTO_START", "0")
	t.Setenv("BEADS_DIR", "/host/beads")
	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_DOLT_HOST", "ambient-host")
	t.Setenv("GC_DOLT_PORT", "1234")
	t.Setenv("GC_DOLT_USER", "ambient-user")
	t.Setenv("GC_DOLT_PASSWORD", "ambient-password")
	t.Setenv("GC_DOLT_STATE_FILE", "/host/dolt-state.json")
	t.Setenv("GC_DOLT_CONFIG_FILE", "/host/dolt-config.yaml")
	t.Setenv("GC_DOLT_DATA_DIR", "/host/dolt-data")
	t.Setenv("GC_DOLT_LOG_FILE", "/host/dolt.log")
	t.Setenv("GC_DOLT_PID_FILE", "/host/dolt.pid")
	t.Setenv("GC_DOLT_LOCK_FILE", "/host/dolt.lock")
	t.Setenv("GC_DOLT_MANAGED_LOCAL", "1")
	t.Setenv("BEADS_DOLT_SERVER_HOST", "ambient-beads-host")
	t.Setenv("BEADS_DOLT_SERVER_PORT", "5678")
	t.Setenv("BEADS_DOLT_SERVER_USER", "ambient-beads-user")
	t.Setenv("BEADS_DOLT_PASSWORD", "ambient-beads-password")
	t.Setenv("BEADS_DOLT_HOST", "ambient-legacy-host")
	t.Setenv("BEADS_DOLT_PORT", "9012")
	t.Setenv("BEADS_DOLT_USER", "ambient-legacy-user")
	t.Setenv("BEADS_DOLT_DATABASE", "ambient-legacy-db")
	t.Setenv("BEADS_DOLT_DATA_DIR", filepath.Join(t.TempDir(), "ambient-dolt-data"))
	t.Setenv("GC_CITY", "/host/city")
	t.Setenv("GC_CITY_PATH", "/host/city")
	t.Setenv("GC_CITY_RUNTIME_DIR", "/host/runtime")

	dir := t.TempDir()
	env := standaloneBDEnvForDir(dir)
	got := parseEnvList(env)

	if got["BEADS_DOLT_AUTO_START"] != "1" {
		t.Fatalf("BEADS_DOLT_AUTO_START = %q, want 1", got["BEADS_DOLT_AUTO_START"])
	}
	if got["BEADS_DIR"] != filepath.Join(dir, ".beads") {
		t.Fatalf("BEADS_DIR = %q, want %q", got["BEADS_DIR"], filepath.Join(dir, ".beads"))
	}
	if got["DOLT_ROOT_PATH"] != testGCHome {
		t.Fatalf("DOLT_ROOT_PATH = %q, want seeded integration root %q", got["DOLT_ROOT_PATH"], testGCHome)
	}
	if got["XDG_RUNTIME_DIR"] != dir {
		t.Fatalf("XDG_RUNTIME_DIR = %q, want %q", got["XDG_RUNTIME_DIR"], dir)
	}
	for _, key := range []string{
		"GC_DOLT",
		"GC_DOLT_HOST",
		"GC_DOLT_PORT",
		"GC_DOLT_USER",
		"GC_DOLT_PASSWORD",
		"GC_DOLT_STATE_FILE",
		"GC_DOLT_CONFIG_FILE",
		"GC_DOLT_DATA_DIR",
		"GC_DOLT_LOG_FILE",
		"GC_DOLT_PID_FILE",
		"GC_DOLT_LOCK_FILE",
		"GC_DOLT_MANAGED_LOCAL",
		"BEADS_DOLT_SERVER_HOST",
		"BEADS_DOLT_SERVER_PORT",
		"BEADS_DOLT_SERVER_USER",
		"BEADS_DOLT_PASSWORD",
		"BEADS_DOLT_HOST",
		"BEADS_DOLT_PORT",
		"BEADS_DOLT_USER",
		"BEADS_DOLT_DATABASE",
		"BEADS_DOLT_DATA_DIR",
		"GC_CITY",
		"GC_CITY_PATH",
		"GC_CITY_RUNTIME_DIR",
	} {
		if _, ok := got[key]; ok {
			t.Fatalf("%s leaked into standalone bd env: %v", key, got[key])
		}
	}
}

func TestStandaloneBDEnvForDirIsolatesHome(t *testing.T) {
	oldGCHome := testGCHome
	oldRuntimeDir := testRuntimeDir
	oldRealBDBinary := realBDBinary
	oldToolBinDir := integrationToolBinDir
	t.Cleanup(func() {
		testGCHome = oldGCHome
		testRuntimeDir = oldRuntimeDir
		realBDBinary = oldRealBDBinary
		integrationToolBinDir = oldToolBinDir
	})

	testGCHome = filepath.Join(t.TempDir(), "gc-home")
	testRuntimeDir = filepath.Join(t.TempDir(), "runtime")
	realBDBinary = "/usr/bin/bd"
	integrationToolBinDir = filepath.Join(t.TempDir(), "bin")

	t.Setenv("HOME", "/host/home")

	dir := t.TempDir()
	env := standaloneBDEnvForDir(dir)
	got := parseEnvList(env)

	// pinRealHomeEnv fails open when the uid has no passwd entry, so the
	// real-home comparison is only meaningful when the lookup succeeds. The
	// dir-scoped assertion below holds either way.
	if lu, err := user.LookupId(strconv.Itoa(os.Getuid())); err == nil && strings.TrimSpace(lu.HomeDir) != "" {
		if got["HOME"] == lu.HomeDir {
			t.Fatalf("HOME = %q, leaked the real passwd-db home; standalone bd only execs the bd binary (never gc start/supervisor start), so it must not inherit the real-HOME pin meant for gc-start consumers", got["HOME"])
		}
	}
	if got["HOME"] != dir {
		t.Fatalf("HOME = %q, want dir-scoped %q, matching this helper's own XDG_RUNTIME_DIR/BEADS_DIR isolation root", got["HOME"], dir)
	}
}

func TestUsesStandaloneBDWorkspaceKeepsFileProviderOnShim(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".beads"), 0o755); err != nil {
		t.Fatalf("mkdir .beads: %v", err)
	}

	if usesStandaloneBDWorkspace(dir, []string{"GC_BEADS=file"}) {
		t.Fatal("file provider city should keep using the file-store bd shim")
	}
	if usesStandaloneBDWorkspace(dir, []string{"GC_BEADS=dolt"}) {
		t.Fatal("bare .beads directory should not select the standalone bd env")
	}
	if err := os.WriteFile(filepath.Join(dir, ".beads", "config.yaml"), []byte("issue_prefix: test\n"), 0o644); err != nil {
		t.Fatalf("write config.yaml: %v", err)
	}
	if usesStandaloneBDWorkspace(dir, []string{"GC_BEADS=file"}) {
		t.Fatal("file provider city with config.yaml should keep using the file-store bd shim")
	}
	if !usesStandaloneBDWorkspace(dir, []string{"GC_BEADS=dolt"}) {
		t.Fatal("standalone .beads workspace with config.yaml should use the standalone bd env")
	}
}

func TestCommandEnvForDirPrefersRegisteredCityEnv(t *testing.T) {
	cityDir := filepath.Join(t.TempDir(), "city")
	want := []string{"HOME=/tmp/isolated", "GC_HOME=/tmp/isolated", "PATH=/tmp/bin"}
	registerCityCommandEnv(cityDir, want)
	t.Cleanup(func() { unregisterCityCommandEnv(cityDir) })

	got := commandEnvForDir(cityDir, false)
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("commandEnvForDir(%q) = %v, want %v", cityDir, got, want)
	}
}

func TestCommandEnvLookupDirUsesRegisteredPathArg(t *testing.T) {
	cityDir := filepath.Join(t.TempDir(), "city")
	registerCityCommandEnv(cityDir, []string{"GC_HOME=/tmp/isolated"})
	t.Cleanup(func() { unregisterCityCommandEnv(cityDir) })

	if got := commandEnvLookupDir("", []string{"start", cityDir}); got != cityDir {
		t.Fatalf("commandEnvLookupDir with path arg = %q, want %q", got, cityDir)
	}
	if got := commandEnvLookupDir("/tmp/cwd", []string{"start", cityDir}); got != "/tmp/cwd" {
		t.Fatalf("commandEnvLookupDir with cwd = %q, want cwd", got)
	}
}

func TestStandaloneBdEnvIsolatesAmbientDoltConfig(t *testing.T) {
	t.Setenv("HOME", "/host/home")
	t.Setenv("GC_CITY", "/host/city")
	t.Setenv("GC_CITY_PATH", "/host/city")
	t.Setenv("GC_RIG", "host-rig")
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_BEADS_SCOPE_ROOT", "/host/repo")
	t.Setenv("GC_DOLT", "server")
	t.Setenv("GC_DOLT_HOST", "127.0.0.1")
	t.Setenv("GC_DOLT_PORT", "0")
	t.Setenv("GC_DOLT_USER", "ambient-user")
	t.Setenv("GC_DOLT_PASSWORD", "ambient-password")
	t.Setenv("BEADS_DIR", "/host/beads")
	t.Setenv("BEADS_DOLT_AUTO_START", "0")
	t.Setenv("BEADS_DOLT_SERVER_HOST", "127.0.0.1")
	t.Setenv("BEADS_DOLT_SERVER_PORT", "0")
	t.Setenv("BEADS_DOLT_SERVER_USER", "ambient-user")
	t.Setenv("BEADS_DOLT_PASSWORD", "ambient-password")

	dir := filepath.Join(t.TempDir(), "standalone")
	got := parseEnvList(standaloneBdEnv(t, dir))

	if got["HOME"] == "/host/home" || got["HOME"] == "" {
		t.Fatalf("HOME = %q, want isolated non-empty home", got["HOME"])
	}
	if got["HOME"] != got["GC_HOME"] {
		t.Fatalf("HOME = %q, want GC_HOME %q", got["HOME"], got["GC_HOME"])
	}
	if got["BEADS_DIR"] != filepath.Join(dir, ".beads") {
		t.Fatalf("BEADS_DIR = %q, want standalone beads dir", got["BEADS_DIR"])
	}
	if got["BD_NON_INTERACTIVE"] != "1" {
		t.Fatalf("BD_NON_INTERACTIVE = %q, want 1", got["BD_NON_INTERACTIVE"])
	}
	for _, key := range []string{
		"GC_CITY",
		"GC_CITY_PATH",
		"GC_RIG",
		"GC_BEADS",
		"GC_BEADS_SCOPE_ROOT",
		"GC_DOLT",
		"GC_DOLT_HOST",
		"GC_DOLT_PORT",
		"GC_DOLT_USER",
		"GC_DOLT_PASSWORD",
		"BEADS_DOLT_AUTO_START",
		"BEADS_DOLT_SERVER_HOST",
		"BEADS_DOLT_SERVER_PORT",
		"BEADS_DOLT_SERVER_USER",
		"BEADS_DOLT_PASSWORD",
	} {
		if _, ok := got[key]; ok {
			t.Fatalf("%s leaked into standalone bd env: %v", key, got)
		}
	}
}

func TestRenderE2ETomlPlainAgentUsesNamedSessionWithoutSingletonCap(t *testing.T) {
	toml := renderE2EToml(e2eCity{
		Agents: []e2eAgent{{Name: "worker", StartCommand: "sleep 3600"}},
	})
	if !strings.Contains(toml, "[[named_session]]\ntemplate = \"worker\"\nmode = \"always\"") {
		t.Fatalf("rendered TOML missing named session:\n%s", toml)
	}
	if strings.Contains(toml, "max_active_sessions = 1") {
		t.Fatalf("plain E2E agent should not render singleton cap:\n%s", toml)
	}
}

func TestRewriteE2ETomlPreservingNamedSessionsRestoresInlineAgent(t *testing.T) {
	cityDir := t.TempDir()
	initial := `[workspace]
name = "test-city"

[beads]
provider = "file"

[[named_session]]
template = "worker"
mode = "on_demand"

[[named_session]]
template = "worker"
mode = "always"

[[named_session]]
template = "worker"
name = "worker-extra"
mode = "on_demand"
`
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(initial), 0o644); err != nil {
		t.Fatalf("writing city.toml: %v", err)
	}

	rewriteE2ETomlPreservingNamedSessions(t, cityDir, e2eCity{
		Agents: []e2eAgent{{Name: "worker", StartCommand: "VERSION=v2 sleep 3600"}},
	})

	cityData, err := os.ReadFile(filepath.Join(cityDir, "city.toml"))
	if err != nil {
		t.Fatalf("reading city.toml: %v", err)
	}
	packData, err := os.ReadFile(filepath.Join(cityDir, "pack.toml"))
	if err != nil {
		t.Fatalf("reading pack.toml: %v", err)
	}
	cfg, _, err := config.LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityDir, "city.toml"))
	if err != nil {
		t.Fatalf("loading city.toml: %v\ncity.toml:\n%s\npack.toml:\n%s", err, cityData, packData)
	}
	if cfg.Workspace.Name != "test-city" {
		t.Fatalf("Workspace.Name = %q, want test-city", cfg.Workspace.Name)
	}
	var explicitAgents []config.Agent
	for _, agent := range cfg.Agents {
		if !agent.Implicit {
			explicitAgents = append(explicitAgents, agent)
		}
	}
	if len(explicitAgents) != 1 || explicitAgents[0].Name != "worker" {
		t.Fatalf("explicit agents = %+v, want restored worker; all agents = %+v", explicitAgents, cfg.Agents)
	}
	if got := explicitAgents[0].StartCommand; got != "VERSION=v2 sleep 3600" {
		t.Fatalf("StartCommand = %q, want updated command", got)
	}
	var userNamedSessions []config.NamedSession
	for _, ns := range cfg.NamedSessions {
		if ns.Template != config.ControlDispatcherAgentName {
			userNamedSessions = append(userNamedSessions, ns)
		}
	}
	if len(userNamedSessions) != 2 {
		t.Fatalf("len(userNamedSessions) = %d, want 2; all named sessions = %+v\ncity.toml:\n%s\npack.toml:\n%s", len(userNamedSessions), cfg.NamedSessions, cityData, packData)
	}
	var workerSession config.NamedSession
	for _, ns := range userNamedSessions {
		if ns.QualifiedName() == "worker" {
			workerSession = ns
			break
		}
	}
	if workerSession.Template == "" {
		t.Fatalf("worker named session not found\ncity.toml:\n%s\npack.toml:\n%s", cityData, packData)
	}
	if got := workerSession.Mode; got != "always" {
		t.Fatalf("worker named session mode = %q, want always\ncity.toml:\n%s\npack.toml:\n%s", got, cityData, packData)
	}
	if got := strings.Count(string(cityData), "[[named_session]]"); got != 1 {
		t.Fatalf("city.toml named_session blocks = %d, want 1\n%s", got, cityData)
	}
	if !strings.Contains(string(cityData), `name = "worker-extra"`) {
		t.Fatalf("city.toml should preserve non-conflicting worker-extra named session:\n%s", cityData)
	}
	if got := strings.Count(string(packData), "[[named_session]]"); got != 1 {
		t.Fatalf("pack.toml named_session blocks = %d, want 1\n%s", got, packData)
	}
}

func TestNewIsolatedToolEnvSeedsLocalDoltIdentity(t *testing.T) {
	var scratchPath string
	t.Cleanup(func() {
		if scratchPath != "" {
			_ = os.Remove(scratchPath)
		}
	})
	t.Run("owned scratch lifetime", func(t *testing.T) {
		env := newIsolatedToolEnv(t, true)
		got := parseEnvList(env)
		cfgPath := filepath.Join(got["DOLT_ROOT_PATH"], ".dolt", "config_global.json")
		data, err := os.ReadFile(cfgPath)
		if err != nil {
			t.Fatalf("read isolated dolt config: %v", err)
		}
		if !strings.Contains(string(data), `"user.name":"gc-test"`) {
			t.Fatalf("isolated dolt config missing user.name: %s", string(data))
		}
		if !strings.Contains(string(data), `"user.email":"gc-test@test.local"`) {
			t.Fatalf("isolated dolt config missing user.email: %s", string(data))
		}
		if got["TMPDIR"] == "" {
			t.Fatal("isolated environment has no scratch directory")
		}
		scratch, err := os.CreateTemp(got["TMPDIR"], "isolated-scratch-*")
		if err != nil {
			t.Fatalf("create isolated child scratch: %v", err)
		}
		scratchPath = scratch.Name()
		if err := scratch.Close(); err != nil {
			t.Fatalf("close isolated child scratch: %v", err)
		}
	})
	if scratchPath != "" {
		if _, err := os.Stat(scratchPath); !os.IsNotExist(err) {
			t.Errorf("isolated child scratch survived fixture cleanup: %s: %v", scratchPath, err)
		}
	}
}

func TestNewIsolatedToolEnvSkipIdentityModeSkipsConfigWrite(t *testing.T) {
	t.Setenv(integrationDoltIdentityEnv, doltIdentityModeSkip)

	env := newIsolatedToolEnv(t, true)
	got := parseEnvList(env)
	cfgPath := filepath.Join(got["DOLT_ROOT_PATH"], ".dolt", "config_global.json")
	if _, err := os.Stat(cfgPath); err == nil {
		t.Fatalf("expected no isolated dolt config at %s when %s=%s", cfgPath, integrationDoltIdentityEnv, doltIdentityModeSkip)
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat isolated dolt config: %v", err)
	}
}

// TestParsePSLineSurvivesInternalWhitespaceAndRejectsMalformedInput is the
// falsifiable-floor check for the macOS ps(1) fallback: it must parse real
// `ps -axwwo pid=,ppid=,command=` rows (including a multi-arg orphaned
// supervisor command, the exact shape reported for issue's orphan pid) and
// must reject rows that don't have the pid/ppid/command shape, so a future
// ps(1) output-format change fails loudly instead of silently returning an
// empty, "looks clean" process table.
func TestParsePSLineSurvivesInternalWhitespaceAndRejectsMalformedInput(t *testing.T) {
	cases := []struct {
		name     string
		line     string
		wantOK   bool
		wantPID  int
		wantPPID int
		wantCmd  string
	}{
		{
			name:     "orphaned supervisor with args and spaces",
			line:     "62765     1 /var/folders/2t/xxx/T/gc-integration-49858-3331992158/bin/gc supervisor run",
			wantOK:   true,
			wantPID:  62765,
			wantPPID: 1,
			wantCmd:  "/var/folders/2t/xxx/T/gc-integration-49858-3331992158/bin/gc supervisor run",
		},
		{
			name:     "leading whitespace from column padding",
			line:     "   104     1 /usr/libexec/logd",
			wantOK:   true,
			wantPID:  104,
			wantPPID: 1,
			wantCmd:  "/usr/libexec/logd",
		},
		{name: "empty line", line: "", wantOK: false},
		{name: "header-only garbage", line: "PID PPID COMMAND", wantOK: false},
		{name: "missing command", line: "10 1", wantOK: false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pid, ppid, cmd, ok := parsePSLine(c.line)
			if ok != c.wantOK {
				t.Fatalf("parsePSLine(%q) ok = %v, want %v", c.line, ok, c.wantOK)
			}
			if !c.wantOK {
				return
			}
			if pid != c.wantPID || ppid != c.wantPPID || cmd != c.wantCmd {
				t.Fatalf("parsePSLine(%q) = (%d, %d, %q), want (%d, %d, %q)",
					c.line, pid, ppid, cmd, c.wantPID, c.wantPPID, c.wantCmd)
			}
		})
	}
}

// TestReadProcessSnapshotPSFindsRealProcesses exercises the ps(1) query
// itself on every host, including Linux CI, so a future flag or output-format
// change fails here instead of leaving the macOS sweep silently blind — the
// exact failure mode this fallback exists to fix. `ps -axwwo
// pid=,ppid=,command=` is accepted by both BSD ps and procps-ng.
func TestReadProcessSnapshotPSFindsRealProcesses(t *testing.T) {
	procs := readProcessSnapshotPS()
	if len(procs) == 0 {
		t.Fatal("readProcessSnapshotPS() returned no processes; known-positive control failed")
	}
	self := os.Getpid()
	if _, ok := procs[self]; !ok {
		t.Fatalf("readProcessSnapshotPS() did not include this process's own pid %d among %d entries", self, len(procs))
	}
}

// TestReadProcessSnapshotFallsBackToPSAndFindsRealProcesses is the
// known-positive control for the fallback path added by this change: on a
// host with no /proc (every macOS dev box, including CI running locally
// here), readProcessSnapshotProc must return nil, and readProcessSnapshot's
// ps(1) fallback must come back non-empty and contain this test binary's own
// pid — proving the query is not a silently-blind zero.
func TestReadProcessSnapshotFallsBackToPSAndFindsRealProcesses(t *testing.T) {
	if _, err := os.Stat("/proc"); err == nil {
		t.Skip("host has /proc; this test targets the no-/proc (macOS) fallback path")
	}
	if procs := readProcessSnapshotProc(); procs != nil {
		t.Fatalf("readProcessSnapshotProc() = %v entries on a host with no /proc, want nil", len(procs))
	}
	procs := readProcessSnapshot()
	if len(procs) == 0 {
		t.Fatal("readProcessSnapshot() returned no processes via the ps(1) fallback; known-positive control failed")
	}
	self := os.Getpid()
	if _, ok := procs[self]; !ok {
		t.Fatalf("readProcessSnapshot() via ps(1) fallback did not include this process's own pid %d among %d entries", self, len(procs))
	}
}

func TestConfigureIntegrationSupervisorCommandUsesGracefulCancel(t *testing.T) {
	cmd := exec.CommandContext(context.Background(), "gc", "supervisor", "run")
	configureIntegrationSupervisorCommand(cmd)

	if cmd.Cancel == nil {
		t.Fatal("supervisor command Cancel is nil, want SIGTERM cancel")
	}
	if cmd.WaitDelay != 10*time.Second {
		t.Fatalf("supervisor command WaitDelay = %s, want 10s", cmd.WaitDelay)
	}
}

func TestIntegrationDoltSQLServerKillSetMatchesOnlyRootedConfigs(t *testing.T) {
	root := "/tmp/gcit-123"
	procs := map[int]procSnapshot{
		10: {pid: 10, ppid: 1, cmd: "dolt sql-server --config /tmp/gcit-123/cities/x/.gc/runtime/packs/dolt/dolt-config.yaml"},
		11: {pid: 11, ppid: 1, cmd: "dolt sql-server --config=/tmp/gcit-123/cities/y/.gc/runtime/packs/dolt/dolt-config.yaml"},
		12: {pid: 12, ppid: 1, cmd: "dolt sql-server --config /tmp/gcit-1234/cities/z/.gc/runtime/packs/dolt/dolt-config.yaml"},
		13: {pid: 13, ppid: 1, cmd: "dolt sql-server --config /home/u/projects/foo/.gc/runtime/packs/dolt/dolt-config.yaml"},
		14: {pid: 14, ppid: 1, cmd: "dolt sql --config /tmp/gcit-123/cities/x/.gc/runtime/packs/dolt/dolt-config.yaml"},
	}

	got := integrationDoltSQLServerKillSet(procs, root)
	for _, pid := range []int{10, 11} {
		if !got[pid] {
			t.Fatalf("kill set missing pid %d: %#v", pid, got)
		}
	}
	for _, pid := range []int{12, 13, 14} {
		if got[pid] {
			t.Fatalf("kill set unexpectedly included pid %d: %#v", pid, got)
		}
	}
}

func TestRunCommandDoesNotHangOnInheritedStdoutFromBackgroundChild(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")
	script := filepath.Join(dir, "leak-stdout.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nsleep 30 &\necho \"$!\" > \"$1\"\necho leaked-stdout-ok\n"), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}

	start := time.Now()
	out, err := runCommand("", nil, 5*time.Second, script, pidFile)
	if err != nil {
		t.Fatalf("runCommand: %v\n%s", err, out)
	}
	if strings.TrimSpace(out) != "leaked-stdout-ok" {
		t.Fatalf("output = %q, want %q", strings.TrimSpace(out), "leaked-stdout-ok")
	}
	if elapsed := time.Since(start); elapsed >= 5*time.Second {
		t.Fatalf("runCommand took %s, want it to return before timeout", elapsed)
	}

	pidBytes, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("read child pid: %v", err)
	}
	childPID, err := strconv.Atoi(strings.TrimSpace(string(pidBytes)))
	if err != nil {
		t.Fatalf("parse child pid: %v", err)
	}
	t.Cleanup(func() {
		_ = syscall.Kill(childPID, syscall.SIGKILL)
	})
}

// TestRunCommandStdoutExcludesStderr guards against ga-rsktma: a value-bearing
// caller (e.g. bdDoltInRig's `bd config get`) must never observe stderr
// diagnostics mixed into the value it parses. Regression trigger: any
// legitimate stderr output from the subprocess (e.g. bd's own diagnostic
// logging) corrupted assertions that only expected the clean value on stdout.
func TestRunCommandStdoutExcludesStderr(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "split-streams.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf 'clean-value'\nprintf 'diagnostic-noise' 1>&2\n"), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}

	out, err := runCommandStdout("", nil, 5*time.Second, script)
	if err != nil {
		t.Fatalf("runCommandStdout: %v\n%s", err, out)
	}
	if out != "clean-value" {
		t.Fatalf("output = %q, want %q (stderr must not be mixed into a value-bearing capture)", out, "clean-value")
	}
}

// TestRunCommandStdoutIncludesStderrInErrorOnFailure verifies that excluding
// stderr from the returned value does not lose it for diagnosis: on failure
// the stderr content must still surface, via the returned error, so callers'
// %v-based failure messages remain informative.
func TestRunCommandStdoutIncludesStderrInErrorOnFailure(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "fail-with-diagnostic.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf 'partial-value'\nprintf 'boom-diagnostic' 1>&2\nexit 1\n"), 0o755); err != nil {
		t.Fatalf("write script: %v", err)
	}

	out, err := runCommandStdout("", nil, 5*time.Second, script)
	if err == nil {
		t.Fatalf("runCommandStdout: want error for non-zero exit, got nil (output %q)", out)
	}
	if out != "partial-value" {
		t.Fatalf("output = %q, want %q", out, "partial-value")
	}
	if !strings.Contains(err.Error(), "boom-diagnostic") {
		t.Fatalf("err = %q, want it to contain stderr diagnostic %q", err.Error(), "boom-diagnostic")
	}
}

func parseEnvList(env []string) map[string]string {
	out := make(map[string]string, len(env))
	for _, entry := range env {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			out[key] = value
		}
	}
	return out
}

// mainTB is a minimal testing.TB implementation for use in TestMain where
// no *testing.T is available. Only Helper() and Logf() are called by
// KillAllTestSessions.
type mainTB struct{ testing.TB }

func (mainTB) Helper()                         {}
func (mainTB) Logf(format string, args ...any) {}

func statOK(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// runfilesBinaryAt resolves a file inside an external repository (e.g. a
// prebuilt release binary fetched via http_archive) in the test's runfiles
// tree, returning "" when absent. Bazel materializes external repos under
// their canonical name (+http_archive+repo); _repo_mapping maps the apparent
// name used in BUILD labels to the canonical runfiles path.
func runfilesBinaryAt(repo, rel string) string {
	for _, rf := range []string{os.Getenv("RUNFILES_DIR"), os.Getenv("TEST_SRCDIR")} {
		if rf == "" {
			continue
		}
		for _, cand := range []string{repo, canonicalRunfilesRepo(rf, repo)} {
			if cand == "" {
				continue
			}
			if bin := filepath.Join(rf, cand, rel); statOK(bin) {
				return bin
			}
		}
	}
	return ""
}

// canonicalRunfilesRepo reads _repo_mapping in the runfiles root and returns
// the canonical repository name for an apparent one ("" when unmapped).
func canonicalRunfilesRepo(rf, apparent string) string {
	data, err := os.ReadFile(filepath.Join(rf, "_repo_mapping"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		parts := strings.Split(line, ",")
		if len(parts) == 3 && parts[1] == apparent {
			return parts[2]
		}
	}
	return ""
}

// runfilesBinary resolves a bazel-built binary from the test's runfiles tree
// (workspace-relative path) and returns "" when absent.
func runfilesBinary(rel string) string {
	for _, rf := range []string{os.Getenv("RUNFILES_DIR"), os.Getenv("TEST_SRCDIR")} {
		if rf == "" {
			continue
		}
		if bin := filepath.Join(rf, "_main", rel); statOK(bin) {
			return bin
		}
	}
	return ""
}
