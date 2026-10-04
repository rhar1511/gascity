package scripts_test

import (
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

func TestCmdGCIntegrationShardRunsOnlyIntegrationManifest(t *testing.T) {
	repo := repoRoot(t)
	manifest := parseCmdGCIntegrationManifest(t, filepath.Join(repo, "scripts", "test-integration-shard"))
	fixture := newIntegrationShardFixture(t)

	out, err := fixture.run(t)
	if err != nil {
		t.Fatalf("test-integration-shard failed: %v\n%s", err, out)
	}

	captured, err := os.ReadFile(fixture.capturePath)
	if err != nil {
		t.Fatalf("read captured go invocation: %v", err)
	}
	encodedInvocations := strings.TrimSuffix(string(captured), "\x00\x00")
	invocations := strings.Split(encodedInvocations, "\x00\x00")
	if len(invocations) != 1 {
		t.Fatalf("go test invoked %d times, want only the final tagged manifest command:\n%s", len(invocations), captured)
	}
	got := strings.Split(invocations[0], "\x00")
	want := []string{
		"test",
		"-tags", "integration",
		"-timeout", "17s",
		"./cmd/gc",
		"-run", "^(" + strings.Join(manifest, "|") + ")$",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("go test argv = %q, want exact tagged manifest command %q", got, want)
	}
}

func TestIntegrationShardPreservesQualifiedBeadsExecutables(t *testing.T) {
	fixture := newIntegrationShardFixture(t)
	bdPath := filepath.Join(fixture.binDir, "qualified-bd")
	doltPath := filepath.Join(fixture.binDir, "qualified-dolt")
	out, err := fixture.runShardWithEnv(t, "bdstore",
		"BEADS_TEST_BD_BINARY="+bdPath,
		"BEADS_TEST_DOLT_BINARY="+doltPath,
		"GC_DOLT_PORT=3329",
		"BEADS_DOLT_SERVER_PORT=3329",
	)
	if err != nil {
		t.Fatalf("integration shard failed: %v\n%s", err, out)
	}
	got := readCapturedGoEnv(t, fixture.envCapturePath)
	for key, want := range map[string]string{
		"BEADS_TEST_BD_BINARY":   bdPath,
		"BEADS_TEST_DOLT_BINARY": doltPath,
		"GC_DOLT_PORT":           "unset",
		"BEADS_DOLT_SERVER_PORT": "unset",
	} {
		if got[key] != want {
			t.Errorf("child %s = %q, want %q", key, got[key], want)
		}
	}
}

func TestNestedIntegrationShardPreservesQualifiedBeadsExecutables(t *testing.T) {
	fixture := newIntegrationShardFixture(t)
	bdPath := filepath.Join(fixture.binDir, "qualified-bd")
	doltPath := filepath.Join(fixture.binDir, "qualified-dolt")
	out, err := fixture.runShardWithEnv(t, "packages-cmd-gc-1-of-2",
		"BEADS_TEST_BD_BINARY="+bdPath,
		"BEADS_TEST_DOLT_BINARY="+doltPath,
		"GC_DOLT_PORT=3329",
		"BEADS_DOLT_SERVER_PORT=3329",
	)
	if err != nil {
		t.Fatalf("nested integration shard failed: %v\n%s", err, out)
	}
	got := readCapturedGoEnv(t, fixture.envCapturePath)
	for key, want := range map[string]string{
		"BEADS_TEST_BD_BINARY":   bdPath,
		"BEADS_TEST_DOLT_BINARY": doltPath,
		"GC_DOLT_PORT":           "unset",
		"BEADS_DOLT_SERVER_PORT": "unset",
	} {
		if got[key] != want {
			t.Errorf("nested child %s = %q, want %q", key, got[key], want)
		}
	}
}

func TestNestedIntegrationShardPreservesCGOMode(t *testing.T) {
	for _, mode := range []string{"0", "1", ""} {
		t.Run(mode, func(t *testing.T) {
			fixture := newIntegrationShardFixture(t)
			out, err := fixture.runShardWithEnv(t, "packages-cmd-gc-1-of-2",
				"CGO_ENABLED="+mode,
				"GOMEMLIMIT=1GiB",
				"GOMAXPROCS=2",
				"GC_DOLT_PORT=3329",
				"BEADS_DOLT_SERVER_PORT=3329",
			)
			if err != nil {
				t.Fatalf("nested integration shard failed: %v\n%s", err, out)
			}
			got := readCapturedGoEnv(t, fixture.envCapturePath)
			wantMode := mode
			if wantMode == "" {
				wantMode = "0" // The recording Go tool models this resolved default.
			}
			for key, want := range map[string]string{
				"CGO_ENABLED":            wantMode,
				"GC_DOLT_PORT":           "unset",
				"BEADS_DOLT_SERVER_PORT": "unset",
			} {
				if got[key] != want {
					t.Errorf("nested child %s = %q, want %q", key, got[key], want)
				}
			}
			for key, want := range map[string]string{"GOMEMLIMIT": "1GiB", "GOMAXPROCS": "2"} {
				if got[key] != want {
					t.Errorf("nested child %s = %q, want selected runtime limit %q", key, got[key], want)
				}
			}
		})
	}
}

func TestMakefileIntegrationShardPreservesQualifiedBeadsExecutables(t *testing.T) {
	for _, mode := range []string{"0", "1", ""} {
		t.Run(mode, func(t *testing.T) {
			fixture := newIntegrationShardFixture(t)
			bdPath := filepath.Join(fixture.binDir, "qualified-bd")
			doltPath := filepath.Join(fixture.binDir, "qualified-dolt")
			makefile := filepath.Join(t.TempDir(), "qualified-beads.mk")
			writeTestFile(t, makefile, `
.PHONY: qualified-beads-shard
qualified-beads-shard:
	@$(TEST_ENV) scripts/test-integration-shard bdstore
`)
			cmd := makeCommand("--no-print-directory", "-f", "Makefile", "-f", makefile, "qualified-beads-shard")
			cmd.Dir = repoRoot(t)
			cmd.Env = []string{
				"PATH=" + fixture.binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
				"HOME=" + fixture.homeDir,
				"SHELL=/bin/sh",
				"GC_TEST_NO_SLICE=1",
				"SYS_USR_CGO_FALLBACK=0",
				"CGO_ENABLED=" + mode,
				"GOMEMLIMIT=1GiB",
				"GOMAXPROCS=2",
				"BEADS_TEST_BD_BINARY=" + bdPath,
				"BEADS_TEST_DOLT_BINARY=" + doltPath,
				"GC_DOLT_PORT=3329",
				"BEADS_DOLT_SERVER_PORT=3329",
			}
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("make integration shard failed: %v\n%s", err, out)
			}
			got := readCapturedGoEnv(t, fixture.envCapturePath)
			wantMode := mode
			if wantMode == "" {
				wantMode = "0" // The recording Go tool models this resolved default.
			}
			for key, want := range map[string]string{
				"CGO_ENABLED":            wantMode,
				"BEADS_TEST_BD_BINARY":   bdPath,
				"BEADS_TEST_DOLT_BINARY": doltPath,
				"GC_DOLT_PORT":           "unset",
				"BEADS_DOLT_SERVER_PORT": "unset",
			} {
				if got[key] != want {
					t.Errorf("make child %s = %q, want %q", key, got[key], want)
				}
			}
			for key, want := range map[string]string{"GOMEMLIMIT": "1GiB", "GOMAXPROCS": "2"} {
				if got[key] != want {
					t.Errorf("make child %s = %q, want selected runtime limit %q", key, got[key], want)
				}
			}
		})
	}
}

func TestLocalParallelPreservesQualifiedBeadsExecutables(t *testing.T) {
	for _, tc := range []struct {
		name, mode, discovery string
	}{
		{"cgo-disabled", "0", ""},
		{"cgo-enabled", "1", ""},
		{"cgo-default", "", ""},
		{"failed-discovery", "0", "failed"},
		{"partial-failed-discovery", "0", "partial"},
		{"empty-discovery", "0", "empty"},
		{"valid-discovery", "0", "valid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mode := tc.mode
			fixture := newIntegrationShardFixture(t)
			runnerMode := "cmd-gc-process"
			if tc.discovery != "" {
				runnerMode = "fast"
				writeTestFile(t, filepath.Join(fixture.binDir, "discovery-mode"), tc.discovery)
			}
			bdPath := filepath.Join(fixture.binDir, "qualified-bd")
			doltPath := filepath.Join(fixture.binDir, "qualified-dolt")
			cityDir := t.TempDir()
			writeTestFile(t, filepath.Join(cityDir, "city.toml"), "# Disposable runner coordination root.\n")
			logDir := filepath.Join(cityDir, "logs")
			if err := os.Mkdir(logDir, 0o755); err != nil {
				t.Fatalf("create runner log directory: %v", err)
			}
			cmd := shardTestCommand(filepath.Join(repoRoot(t), "scripts", "test-local-parallel"), runnerMode)
			cmd.Dir = repoRoot(t)
			cmd.Env = []string{
				"PATH=" + fixture.binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
				"HOME=" + fixture.homeDir,
				"SHELL=/bin/sh",
				// Scratch must not sit beneath the coordination city's config:
				// the runner's real self-tests exercise no-city Git repositories.
				"TMPDIR=" + filepath.Dir(fixture.binDir),
				"GC_CITY_PATH=" + cityDir,
				"GC_TEST_NO_SLICE=1",
				// Recording-tool plumbing does not exercise host orphan cleanup.
				"GC_TEST_NO_ORPHAN_SWEEP=1",
				"SYS_USR_CGO_FALLBACK=0",
				"CGO_ENABLED=" + mode,
				"LOCAL_TEST_JOBS=1",
				"GOMEMLIMIT=1GiB",
				"GOMAXPROCS=2",
				"GC_TEST_INNER_P=1",
				"CMD_GC_PROCESS_TOTAL=1",
				"GO_TEST_TIMEOUT=17s",
				"LOCAL_TEST_LOG_DIR=" + logDir,
				"BEADS_TEST_BD_BINARY=" + bdPath,
				"BEADS_TEST_DOLT_BINARY=" + doltPath,
				"GC_DOLT_PORT=3329",
				"BEADS_DOLT_SERVER_PORT=3329",
			}
			out, err := cmd.CombinedOutput()
			if tc.discovery != "" {
				captured, readErr := os.ReadFile(fixture.capturePath)
				if readErr != nil {
					t.Fatalf("read product commands: %v", readErr)
				}
				coreInvoked := strings.Contains(string(captured), "github.com/gastownhall/gascity/internal/config\x00")
				if tc.discovery != "valid" {
					if err == nil || !strings.Contains(string(out), "[unit-core] failed") {
						t.Fatalf("invalid discovery reported success: %v\n%s", err, out)
					}
					if coreInvoked || strings.Contains(string(captured), "-timeout\x0017s\x00\x00") {
						t.Fatalf("invalid discovery invoked partial or implicit-root tests: %q", captured)
					}
					return
				}
				if !coreInvoked || strings.Contains(string(captured), "github.com/gastownhall/gascity/cmd/gc\x00") {
					t.Fatalf("core did not use the discovered non-CLI packages: %q", captured)
				}
			}
			if err != nil {
				t.Fatalf("local parallel runner failed: %v\n%s", err, out)
			}
			got := readCapturedGoEnv(t, fixture.envCapturePath)
			wantMode := mode
			if wantMode == "" {
				wantMode = "0" // The recording Go tool models this resolved default.
			}
			for key, want := range map[string]string{
				"CGO_ENABLED":            wantMode,
				"BEADS_TEST_BD_BINARY":   bdPath,
				"BEADS_TEST_DOLT_BINARY": doltPath,
				"GC_DOLT_PORT":           "unset",
				"BEADS_DOLT_SERVER_PORT": "unset",
			} {
				if got[key] != want {
					t.Errorf("outer runner child %s = %q, want %q", key, got[key], want)
				}
			}
			for key, want := range map[string]string{"GOMEMLIMIT": "1GiB", "GOMAXPROCS": "2"} {
				if got[key] != want {
					t.Errorf("outer runner child %s = %q, want selected runtime limit %q", key, got[key], want)
				}
			}
		})
	}
}

func TestRuntimeTmuxIntegrationShardUsesCheckedManifestOnLinux(t *testing.T) {
	repo := repoRoot(t)
	manifest := parseRuntimeTmuxManifest(t, filepath.Join(repo, runtimeTmuxManifestRelativePath))
	fixture := newIntegrationShardFixtureForPlatform(t, "linux", "amd64")

	out, err := fixture.runShard(t, "packages-runtime-tmux-2-of-6")
	if err != nil {
		t.Fatalf("runtime-tmux integration shard failed: %v\n%s", err, out)
	}

	captured, err := os.ReadFile(fixture.capturePath)
	if err != nil {
		t.Fatalf("read captured go invocation: %v", err)
	}
	encodedInvocations := strings.TrimSuffix(string(captured), "\x00\x00")
	invocations := strings.Split(encodedInvocations, "\x00\x00")
	if len(invocations) != 1 {
		t.Fatalf("go test invoked %d times, want only the final runtime-tmux shard command:\n%s", len(invocations), captured)
	}

	var selected []string
	for index, testName := range manifest {
		if index%6 == 1 {
			selected = append(selected, testName)
		}
	}
	got := strings.Split(invocations[0], "\x00")
	want := []string{
		"test",
		"-tags", "integration",
		"-timeout", "17s",
		"./internal/runtime/tmux",
		"-run", "^(" + strings.Join(selected, "|") + ")$",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("runtime-tmux go test argv = %q, want exact checked-manifest shard command %q", got, want)
	}
}

func TestRuntimeTmuxIntegrationShardPreservesDynamicDiscoveryOnDarwin(t *testing.T) {
	assertRuntimeTmuxIntegrationShardUsesDynamicDiscovery(t, "Darwin", newIntegrationShardFixtureForPlatform(t, "darwin", "amd64"))
}

func TestRuntimeTmuxIntegrationShardPreservesDynamicDiscoveryOnLinuxArm64(t *testing.T) {
	assertRuntimeTmuxIntegrationShardUsesDynamicDiscovery(t, "Linux/arm64", newIntegrationShardFixtureForPlatform(t, "linux", "arm64"))
}

func TestRuntimeTmuxIntegrationShardSelectsUsingSanitizedTarget(t *testing.T) {
	fixture := newIntegrationShardFixtureForPlatform(t, "darwin", "arm64")
	assertRuntimeTmuxIntegrationShardUsesDynamicDiscovery(
		t,
		"Darwin/arm64 with ambient linux/amd64 override",
		fixture,
		"GOOS=linux",
		"GOARCH=amd64",
	)
}

func assertRuntimeTmuxIntegrationShardUsesDynamicDiscovery(t *testing.T, platform string, fixture integrationShardFixture, extraEnv ...string) {
	t.Helper()
	assertRuntimeTmuxIntegrationShardUsesDynamicDiscoveryOnShard(
		t,
		platform,
		fixture,
		"2",
		"TestDarwinBeta",
		extraEnv...,
	)
}

func assertRuntimeTmuxIntegrationShardUsesDynamicDiscoveryOnShard(t *testing.T, platform string, fixture integrationShardFixture, shardIndex, selectedTest string, extraEnv ...string) {
	t.Helper()

	out, err := fixture.runShardWithEnv(t, "packages-runtime-tmux-"+shardIndex+"-of-6", extraEnv...)
	if err != nil {
		t.Fatalf("%s runtime-tmux integration shard failed: %v\n%s", platform, err, out)
	}

	captured, err := os.ReadFile(fixture.capturePath)
	if err != nil {
		t.Fatalf("read captured %s go invocations: %v", platform, err)
	}
	encodedInvocations := strings.TrimSuffix(string(captured), "\x00\x00")
	encoded := strings.Split(encodedInvocations, "\x00\x00")
	var invocations [][]string
	for _, invocation := range encoded {
		invocations = append(invocations, strings.Split(invocation, "\x00"))
	}
	want := [][]string{
		{
			"test",
			"-tags", "integration",
			"-timeout", "17s",
			"./internal/runtime/tmux",
			"-list", "^Test",
		},
		{
			"test",
			"-tags", "integration",
			"-timeout", "17s",
			"./internal/runtime/tmux",
			"-run", "^(" + selectedTest + ")$",
		},
	}
	if !slices.EqualFunc(invocations, want, slices.Equal) {
		t.Fatalf("%s runtime-tmux go test invocations = %q, want discovery plus final shard %q", platform, invocations, want)
	}
}

func TestRuntimeTmuxIntegrationShardClearsAmbientManifestOnDynamicFallback(t *testing.T) {
	fixture := newIntegrationShardFixtureForPlatform(t, "darwin", "amd64")
	manifest := writeGoTestManifest(t, t.TempDir(), "TestForcedByAmbientManifest")
	assertRuntimeTmuxIntegrationShardUsesDynamicDiscoveryOnShard(
		t,
		"Darwin/amd64 with ambient one-entry manifest",
		fixture,
		"1",
		"TestDarwinAlpha",
		"GO_TEST_MANIFEST="+manifest,
	)
}

func TestCmdGCIntegrationManifestMatchesTaggedDeclarations(t *testing.T) {
	repo := repoRoot(t)
	manifest := parseCmdGCIntegrationManifest(t, filepath.Join(repo, "scripts", "test-integration-shard"))
	declared := discoverCmdGCIntegrationTests(t, filepath.Join(repo, "cmd", "gc"))

	if drift := cmdGCIntegrationManifestDrift(manifest, declared); len(drift) != 0 {
		t.Fatalf("cmd/gc integration manifest drift:\n%s\nupdate cmd_gc_integration_tests in scripts/test-integration-shard", strings.Join(drift, "\n"))
	}
}

func TestCmdGCIntegrationManifestDriftDiagnosesBothDirections(t *testing.T) {
	manifest := []string{"TestKept", "TestStale", "TestStale"}
	declared := []string{"TestKept", "TestNew"}
	want := []string{
		"duplicate cmd/gc integration manifest entry: TestStale",
		"unassigned cmd/gc integration test: TestNew",
		"cmd/gc integration manifest entry is not integration-only: TestStale",
	}

	if got := cmdGCIntegrationManifestDrift(manifest, declared); !slices.Equal(got, want) {
		t.Fatalf("manifest drift diagnostics = %q, want %q", got, want)
	}
}

func TestCmdGCIntegrationDiscoveryUsesCanonicalLinuxPlatform(t *testing.T) {
	context := canonicalCmdGCIntegrationBuildContext()
	if context.GOOS != "linux" || context.GOARCH != "amd64" {
		t.Fatalf("cmd/gc integration build target = %s/%s, want linux/amd64", context.GOOS, context.GOARCH)
	}

	dir := t.TempDir()
	writeTestFile(t, filepath.Join(dir, "linux_integration_test.go"), `//go:build integration && linux

package fixture

import "testing"

func TestLinux(t *testing.T) {}
`)
	writeTestFile(t, filepath.Join(dir, "darwin_integration_test.go"), `//go:build integration && darwin

package fixture

import "testing"

func TestDarwin(t *testing.T) {}
`)

	if got, want := discoverCmdGCIntegrationTests(t, dir), []string{"TestLinux"}; !slices.Equal(got, want) {
		t.Fatalf("canonical linux/amd64 integration tests = %q, want %q", got, want)
	}
}

func TestCmdGCIntegrationDiscoveryDistinguishesTestMainHarness(t *testing.T) {
	t.Run("ordinary test", func(t *testing.T) {
		dir := t.TempDir()
		writeTestFile(t, filepath.Join(dir, "ordinary_integration_test.go"), `//go:build integration

package fixture

import "testing"

func TestMain(*testing.T) {}
`)

		if got, want := discoverCmdGCIntegrationTests(t, dir), []string{"TestMain"}; !slices.Equal(got, want) {
			t.Fatalf("integration tests = %q, want ordinary TestMain included as %q", got, want)
		}
	})

	t.Run("test harness", func(t *testing.T) {
		dir := t.TempDir()
		writeTestFile(t, filepath.Join(dir, "harness_integration_test.go"), `//go:build integration

package fixture

import "testing"

func TestMain(m *testing.M) {}
func TestOrdinary(t *testing.T) {}
`)

		if got, want := discoverCmdGCIntegrationTests(t, dir), []string{"TestOrdinary"}; !slices.Equal(got, want) {
			t.Fatalf("integration tests = %q, want harness excluded and ordinary test %q", got, want)
		}
	})
}

func parseCmdGCIntegrationManifest(t *testing.T, path string) []string {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read cmd/gc integration manifest: %v", err)
	}

	const declaration = "cmd_gc_integration_tests=("
	inManifest := false
	var tests []string
	for _, rawLine := range strings.Split(string(content), "\n") {
		line := strings.TrimSpace(rawLine)
		if !inManifest {
			if line == declaration {
				inManifest = true
			}
			continue
		}
		if line == ")" {
			if len(tests) == 0 {
				t.Fatal("cmd_gc_integration_tests is empty")
			}
			return tests
		}
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 1 {
			t.Fatalf("unsupported cmd_gc_integration_tests entry %q", rawLine)
		}
		testName := strings.Trim(fields[0], "'\"")
		if !isGoTestName(testName) {
			t.Fatalf("invalid cmd_gc_integration_tests entry %q", testName)
		}
		tests = append(tests, testName)
	}
	if !inManifest {
		t.Fatalf("%s not found in %s", declaration, path)
	}
	t.Fatalf("unterminated %s in %s", declaration, path)
	return nil
}

func discoverCmdGCIntegrationTests(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read cmd/gc directory: %v", err)
	}

	withoutIntegration := canonicalCmdGCIntegrationBuildContext()
	withIntegration := withoutIntegration
	withIntegration.BuildTags = []string{"integration"}

	fileSet := token.NewFileSet()
	var tests []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, "_test.go") {
			continue
		}
		tagged, err := withIntegration.MatchFile(dir, name)
		if err != nil {
			t.Fatalf("match tagged cmd/gc file %s: %v", name, err)
		}
		untagged, err := withoutIntegration.MatchFile(dir, name)
		if err != nil {
			t.Fatalf("match untagged cmd/gc file %s: %v", name, err)
		}
		if !tagged || untagged {
			continue
		}

		path := filepath.Join(dir, name)
		parsed, err := parser.ParseFile(fileSet, path, nil, 0)
		if err != nil {
			t.Fatalf("parse integration-only cmd/gc file %s: %v", name, err)
		}
		for _, declaration := range parsed.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if ok && function.Recv == nil && isGoTestName(function.Name.Name) && isGoTestFunc(function, "T") {
				tests = append(tests, function.Name.Name)
			}
		}
	}
	slices.Sort(tests)
	return tests
}

func canonicalCmdGCIntegrationBuildContext() build.Context {
	context := build.Default
	context.GOOS = "linux"
	context.GOARCH = "amd64"
	context.Compiler = "gc"
	context.CgoEnabled = true
	context.BuildTags = nil
	context.ToolTags = nil
	return context
}

func isGoTestName(name string) bool {
	if !strings.HasPrefix(name, "Test") {
		return false
	}
	runeAfterPrefix, _ := utf8.DecodeRuneInString(strings.TrimPrefix(name, "Test"))
	return !unicode.IsLower(runeAfterPrefix)
}

func isGoTestFunc(function *ast.FuncDecl, parameterType string) bool {
	if function.Type.TypeParams != nil && len(function.Type.TypeParams.List) != 0 {
		return false
	}
	if function.Type.Results != nil && len(function.Type.Results.List) != 0 {
		return false
	}
	if function.Type.Params == nil || len(function.Type.Params.List) != 1 {
		return false
	}
	parameter := function.Type.Params.List[0]
	if len(parameter.Names) > 1 {
		return false
	}
	pointer, ok := parameter.Type.(*ast.StarExpr)
	if !ok {
		return false
	}
	switch target := pointer.X.(type) {
	case *ast.Ident:
		return target.Name == parameterType
	case *ast.SelectorExpr:
		return target.Sel.Name == parameterType
	default:
		return false
	}
}

func cmdGCIntegrationManifestDrift(manifest, declared []string) []string {
	manifestSet := make(map[string]struct{}, len(manifest))
	var duplicates []string
	for _, testName := range manifest {
		if _, exists := manifestSet[testName]; exists {
			if !slices.Contains(duplicates, testName) {
				duplicates = append(duplicates, testName)
			}
			continue
		}
		manifestSet[testName] = struct{}{}
	}
	slices.Sort(duplicates)
	declaredSet := make(map[string]struct{}, len(declared))
	for _, testName := range declared {
		declaredSet[testName] = struct{}{}
	}

	var unassigned []string
	for testName := range declaredSet {
		if _, ok := manifestSet[testName]; !ok {
			unassigned = append(unassigned, testName)
		}
	}
	slices.Sort(unassigned)
	var stale []string
	for testName := range manifestSet {
		if _, ok := declaredSet[testName]; !ok {
			stale = append(stale, testName)
		}
	}
	slices.Sort(stale)

	drift := make([]string, 0, len(duplicates)+len(unassigned)+len(stale))
	for _, testName := range duplicates {
		drift = append(drift, "duplicate cmd/gc integration manifest entry: "+testName)
	}
	for _, testName := range unassigned {
		drift = append(drift, "unassigned cmd/gc integration test: "+testName)
	}
	for _, testName := range stale {
		drift = append(drift, "cmd/gc integration manifest entry is not integration-only: "+testName)
	}
	return drift
}

type integrationShardFixture struct {
	binDir         string
	homeDir        string
	capturePath    string
	envCapturePath string
}

func newIntegrationShardFixture(t *testing.T) integrationShardFixture {
	return newIntegrationShardFixtureForPlatform(t, "linux", "amd64")
}

func newIntegrationShardFixtureForPlatform(t *testing.T, goos, goarch string) integrationShardFixture {
	t.Helper()

	tmp := t.TempDir()
	binDir := filepath.Join(tmp, "bin")
	if err := os.Mkdir(binDir, 0o755); err != nil {
		t.Fatalf("mkdir fake bin: %v", err)
	}
	capturePath := filepath.Join(tmp, "go-test.capture")
	envCapturePath := filepath.Join(tmp, "go-test.env")

	writeExecutable(t, filepath.Join(binDir, "go"), `#!/usr/bin/env bash
set -euo pipefail

capture_path=`+shellQuote(capturePath)+`
env_capture_path=`+shellQuote(envCapturePath)+`
modeled_goos=`+shellQuote(goos)+`
modeled_goarch=`+shellQuote(goarch)+`

case "$1" in
  list)
    case "$(cat "$(dirname "$0")/discovery-mode")" in
      failed) echo 'fixture package discovery failed' >&2; exit 42 ;;
      partial) echo github.com/gastownhall/gascity/internal/config; exit 42 ;;
      empty) echo github.com/gastownhall/gascity/cmd/gc ;;
      valid) printf '%s\n' github.com/gastownhall/gascity/internal/config github.com/gastownhall/gascity/cmd/gc ;;
      *) exit 1 ;;
    esac
    ;;
  env)
    case "$2" in
      GOPATH) echo /tmp/fake-gopath ;;
      GOCACHE) echo /tmp/fake-gocache ;;
      GOMODCACHE) echo /tmp/fake-gomodcache ;;
      GOTMPDIR) echo "" ;;
      GOROOT) echo /tmp/fake-goroot ;;
      CGO_ENABLED) echo "${CGO_ENABLED:-0}" ;;
      GOOS) echo "${GOOS:-$modeled_goos}" ;;
      GOARCH) echo "${GOARCH:-$modeled_goarch}" ;;
      *) echo "unexpected go env key: $2" >&2; exit 1 ;;
    esac
    ;;
  test)
    {
      printf 'CGO_ENABLED=%s\n' "${CGO_ENABLED-unset}"
      printf 'GOMEMLIMIT=%s\n' "${GOMEMLIMIT-unset}"
      printf 'GOMAXPROCS=%s\n' "${GOMAXPROCS-unset}"
      printf 'BEADS_TEST_BD_BINARY=%s\n' "${BEADS_TEST_BD_BINARY-unset}"
      printf 'BEADS_TEST_DOLT_BINARY=%s\n' "${BEADS_TEST_DOLT_BINARY-unset}"
      printf 'GC_DOLT_PORT=%s\n' "${GC_DOLT_PORT-unset}"
      printf 'BEADS_DOLT_SERVER_PORT=%s\n' "${BEADS_DOLT_SERVER_PORT-unset}"
    } > "$env_capture_path"
    is_list=0
    for arg in "$@"; do
      if [[ "$arg" == "-list" ]]; then
        is_list=1
      fi
    done
    printf '%s\0' "$@" >> "$capture_path"
    printf '\0' >> "$capture_path"
    if [[ "$is_list" == "1" ]]; then
      printf '%s\n' TestDarwinAlpha TestDarwinBeta TestDarwinGamma 'ok  github.com/gastownhall/gascity/internal/runtime/tmux  0.001s'
    fi
    ;;
  *)
    echo "unexpected go command: $*" >&2
    exit 1
    ;;
esac
`)

	return integrationShardFixture{
		binDir:         binDir,
		homeDir:        filepath.Join(tmp, "home"),
		capturePath:    capturePath,
		envCapturePath: envCapturePath,
	}
}

func (f integrationShardFixture) run(t *testing.T) ([]byte, error) {
	return f.runShard(t, "packages-cmd-gc-integration")
}

func (f integrationShardFixture) runShard(t *testing.T, shard string) ([]byte, error) {
	return f.runShardWithEnv(t, shard)
}

func (f integrationShardFixture) runShardWithEnv(t *testing.T, shard string, extraEnv ...string) ([]byte, error) {
	t.Helper()
	repo := repoRoot(t)
	cmd := exec.Command(
		filepath.Join(repo, "scripts", "test-integration-shard"),
		shard,
	)
	cmd.Dir = repo
	cmd.Env = append([]string{
		"PATH=" + f.binDir + string(os.PathListSeparator) + os.Getenv("PATH"),
		"HOME=" + f.homeDir,
		"GC_TEST_NO_SLICE=1",
		"SYS_USR_CGO_FALLBACK=0",
		"GO_TEST_TIMEOUT=17s",
	}, extraEnv...)
	return cmd.CombinedOutput()
}
