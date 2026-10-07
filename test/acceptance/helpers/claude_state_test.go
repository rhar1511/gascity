package acceptancehelpers

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
)

func TestEnsureClaudeStateFileCreatesOnboardingState(t *testing.T) {
	home := t.TempDir()
	configDir := filepath.Join(home, "custom-claude")

	if err := EnsureClaudeStateFile(home, configDir); err != nil {
		t.Fatalf("EnsureClaudeStateFile: %v", err)
	}

	for _, statePath := range []string{
		filepath.Join(home, ".claude.json"),
		filepath.Join(configDir, ".claude.json"),
	} {
		state := readClaudeStateForTest(t, statePath)
		if got := state["hasCompletedOnboarding"]; got != true {
			t.Fatalf("%s hasCompletedOnboarding = %#v, want true", statePath, got)
		}
		if got := state["theme"]; got != "light" {
			t.Fatalf("%s theme = %#v, want light", statePath, got)
		}
	}
}

func TestEnsureClaudeProjectStateMergesExistingState(t *testing.T) {
	home := t.TempDir()
	configDir := filepath.Join(home, "isolated-claude")
	projectPath := filepath.Join(t.TempDir(), "city")

	initial := map[string]any{
		"otherSetting":           "keep-me",
		"hasCompletedOnboarding": false,
		"projects": map[string]any{
			projectPath: map[string]any{
				"customFlag": "keep-project-state",
			},
		},
	}
	writeClaudeStateForTest(t, filepath.Join(home, ".claude.json"), initial)
	writeClaudeStateForTest(t, filepath.Join(configDir, ".claude.json"), map[string]any{
		"nestedSetting": "keep-nested-state",
		"theme":         "dark",
	})

	env := &Env{vars: map[string]string{"HOME": home, "CLAUDE_CONFIG_DIR": configDir}}
	if err := EnsureClaudeProjectState(env, projectPath); err != nil {
		t.Fatalf("EnsureClaudeProjectState: %v", err)
	}

	assertClaudeProjectTrustedForTest(t, filepath.Join(home, ".claude.json"), projectPath, map[string]any{
		"otherSetting": "keep-me",
	}, map[string]any{
		"customFlag": "keep-project-state",
	})
	assertClaudeProjectTrustedForTest(t, filepath.Join(configDir, ".claude.json"), projectPath, map[string]any{
		"nestedSetting": "keep-nested-state",
		"theme":         "dark",
	}, nil)
}

func TestEnsureClaudeProjectStateUsesIsolatedGCHome(t *testing.T) {
	hostHome := t.TempDir()
	gcHome := t.TempDir()
	configDir := filepath.Join(gcHome, ".claude")
	hostPath := filepath.Join(hostHome, ".claude.json")
	hostState := []byte(`{"ambientState":"must remain unchanged"}`)
	if err := os.WriteFile(hostPath, hostState, 0o600); err != nil {
		t.Fatal(err)
	}
	env := &Env{vars: map[string]string{
		"HOME": hostHome, "GC_HOME": gcHome, "CLAUDE_CONFIG_DIR": configDir,
	}}
	project := filepath.Join(t.TempDir(), "city")
	if err := EnsureClaudeProjectState(env, project); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(hostPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(hostState) {
		t.Fatalf("fixture seeding changed the simulated operator state: %s", got)
	}
	if env.Get("HOME") != hostHome {
		t.Fatal("fixture seeding changed the supervisor HOME identity")
	}
	for _, path := range claudeStatePaths(gcHome, configDir) {
		assertClaudeProjectTrustedForTest(t, path, project, nil, nil)
	}
}

func TestEnsureClaudeProjectStateConcurrentPreservesProjects(t *testing.T) {
	home := t.TempDir()
	env := &Env{vars: map[string]string{"HOME": home, "GC_HOME": home}}
	if err := EnsureClaudeStateFile(home); err != nil {
		t.Fatal(err)
	}
	const writers = 32
	start := make(chan struct{})
	errors := make(chan error, writers)
	var done sync.WaitGroup
	for i := range writers {
		project := filepath.Join(home, fmt.Sprintf("city-%d", i))
		done.Add(1)
		go func() {
			defer done.Done()
			<-start
			errors <- EnsureClaudeProjectState(env, project)
		}()
	}
	close(start)
	done.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Errorf("concurrent fixture seeding: %v", err)
		}
	}
	for _, path := range claudeStatePaths(home, filepath.Join(home, ".claude")) {
		for i := range writers {
			assertClaudeProjectTrustedForTest(t, path, filepath.Join(home, fmt.Sprintf("city-%d", i)), nil, nil)
		}
	}
}

func TestSaveClaudeStateReadersNeverSeePartialJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".claude.json")
	state := map[string]any{"payload": string(make([]byte, 32*1024))}
	if err := saveClaudeState(path, state); err != nil {
		t.Fatal(err)
	}
	ready := make(chan struct{})
	stop := make(chan struct{})
	result := make(chan error, 1)
	var writing atomic.Bool
	var concurrentReads atomic.Int64
	go func() {
		firstRead := true
		for {
			select {
			case <-stop:
				result <- nil
				return
			default:
			}
			duringWrite := writing.Load()
			data, err := os.ReadFile(path)
			if firstRead {
				close(ready)
				firstRead = false
			}
			if err != nil {
				result <- err
				return
			}
			if !json.Valid(data) {
				result <- fmt.Errorf("reader observed incomplete JSON (%d bytes)", len(data))
				return
			}
			if duringWrite && writing.Load() {
				concurrentReads.Add(1)
			}
		}
	}()
	<-ready
	writing.Store(true)
	for i := range 32 {
		state["iteration"] = i
		if err := saveClaudeState(path, state); err != nil {
			close(stop)
			<-result
			t.Fatal(err)
		}
	}
	writing.Store(false)
	close(stop)
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	if got := concurrentReads.Load(); got == 0 {
		t.Fatal("reader did not observe the concurrent write phase")
	} else {
		t.Logf("validated %d concurrent JSON reads", got)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("state permissions = %o, want 600", got)
	}
}

func TestEnsureClaudeProjectStateRejectsMalformedExistingJSON(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".claude.json")
	malformed := []byte(`{"unfinished":`)
	if err := os.WriteFile(path, malformed, 0o600); err != nil {
		t.Fatal(err)
	}
	env := &Env{vars: map[string]string{"HOME": home, "GC_HOME": home}}
	if err := EnsureClaudeProjectState(env, filepath.Join(home, "city")); err == nil {
		t.Fatal("malformed existing state must remain an error")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(malformed) {
		t.Fatal("malformed existing state was silently replaced")
	}
}

func assertClaudeProjectTrustedForTest(t *testing.T, statePath, projectPath string, preservedState, preservedProject map[string]any) {
	t.Helper()

	state := readClaudeStateForTest(t, statePath)
	if got := state["hasCompletedOnboarding"]; got != true {
		t.Fatalf("%s hasCompletedOnboarding = %#v, want true", statePath, got)
	}
	for key, want := range preservedState {
		if got := state[key]; got != want {
			t.Fatalf("%s %s = %#v, want %#v", statePath, key, got, want)
		}
	}

	projects, ok := state["projects"].(map[string]any)
	if !ok {
		t.Fatalf("%s projects missing or wrong type: %#v", statePath, state["projects"])
	}
	entry, ok := projects[projectPath].(map[string]any)
	if !ok {
		t.Fatalf("%s project entry missing or wrong type: %#v", statePath, projects[projectPath])
	}
	if got := entry["hasCompletedProjectOnboarding"]; got != true {
		t.Fatalf("%s hasCompletedProjectOnboarding = %#v, want true", statePath, got)
	}
	if got := entry["hasTrustDialogAccepted"]; got != true {
		t.Fatalf("%s hasTrustDialogAccepted = %#v, want true", statePath, got)
	}
	if got := entry["projectOnboardingSeenCount"]; got != float64(1) {
		t.Fatalf("%s projectOnboardingSeenCount = %#v, want 1", statePath, got)
	}
	for key, want := range preservedProject {
		if got := entry[key]; got != want {
			t.Fatalf("%s project %s = %#v, want %#v", statePath, key, got, want)
		}
	}
}

func readClaudeStateForTest(t *testing.T, path string) map[string]any {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	var state map[string]any
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatalf("unmarshal %s: %v", path, err)
	}
	return state
}

func writeClaudeStateForTest(t *testing.T, path string, state map[string]any) {
	t.Helper()

	data, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("marshal state: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
