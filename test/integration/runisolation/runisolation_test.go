package runisolation

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestResolveRequiresOwnedProviderAndIdentity(t *testing.T) {
	tests := []struct {
		name     string
		runOwned string
		session  string
		identity string
		want     Mode
		wantErr  bool
	}{
		{name: "legacy defaults", want: Legacy},
		{name: "owned valid", runOwned: "1", session: SubprocessProvider, identity: IsolatedDoltIdentity, want: Owned},
		{name: "owned flag invalid", runOwned: "true", session: SubprocessProvider, identity: IsolatedDoltIdentity, wantErr: true},
		{name: "provider missing", runOwned: "1", identity: IsolatedDoltIdentity, wantErr: true},
		{name: "provider tmux", runOwned: "1", session: "tmux", identity: IsolatedDoltIdentity, wantErr: true},
		{name: "identity missing", runOwned: "1", session: SubprocessProvider, wantErr: true},
		{name: "identity global", runOwned: "1", session: SubprocessProvider, identity: "global", wantErr: true},
		{name: "identity skipped", runOwned: "1", session: SubprocessProvider, identity: "skip", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Resolve(tt.runOwned, tt.session, tt.identity)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Resolve() error = %v, wantErr %v", err, tt.wantErr)
			}
			if err == nil && got != tt.want {
				t.Fatalf("Resolve() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestStartupDispatchUsesPrepareAndLegacySweepHooks(t *testing.T) {
	tests := []struct {
		name       string
		mode       Mode
		wantCalls  []string
		prepareErr error
	}{
		{name: "legacy", mode: Legacy, wantCalls: []string{"prepare", "legacy-sweep"}},
		{name: "owned has no cross-run startup sweep", mode: Owned, wantCalls: []string{"prepare"}},
		{name: "prepare failure stops before sweep", mode: Legacy, wantCalls: []string{"prepare"}, prepareErr: errors.New("prepare failed")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls []string
			err := Startup(tt.mode, func(mode Mode) error {
				calls = append(calls, "prepare")
				if mode != tt.mode {
					t.Fatalf("prepare mode = %v, want %v", mode, tt.mode)
				}
				return tt.prepareErr
			}, func() error {
				calls = append(calls, "legacy-sweep")
				return nil
			})
			if (err != nil) != (tt.prepareErr != nil) {
				t.Fatalf("Startup() error = %v, want prepare error %v", err, tt.prepareErr)
			}
			if !reflect.DeepEqual(calls, tt.wantCalls) {
				t.Fatalf("Startup() calls = %v, want %v", calls, tt.wantCalls)
			}
		})
	}
}

func TestFinishDispatchUsesOnlySelectedCleanup(t *testing.T) {
	tests := []struct {
		name      string
		mode      Mode
		wantCalls []string
	}{
		{name: "legacy", mode: Legacy, wantCalls: []string{"legacy-cleanup"}},
		{name: "owned", mode: Owned, wantCalls: []string{"owned-cleanup"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls []string
			err := Finish(tt.mode, func() error {
				calls = append(calls, "legacy-cleanup")
				return nil
			}, func() error {
				calls = append(calls, "owned-cleanup")
				return nil
			})
			if err != nil {
				t.Fatalf("Finish() error = %v", err)
			}
			if !reflect.DeepEqual(calls, tt.wantCalls) {
				t.Fatalf("Finish() calls = %v, want %v", calls, tt.wantCalls)
			}
		})
	}
}

func TestSignalDispatchSkipsCrossRunSweepForOwnedMode(t *testing.T) {
	for _, tt := range []struct {
		name     string
		mode     Mode
		wantCall bool
	}{
		{name: "legacy", mode: Legacy, wantCall: true},
		{name: "owned", mode: Owned},
	} {
		t.Run(tt.name, func(t *testing.T) {
			called := false
			OnSignal(tt.mode, func() { called = true })
			if called != tt.wantCall {
				t.Fatalf("OnSignal() called legacy sweep = %t, want %t", called, tt.wantCall)
			}
		})
	}
}

func TestCleanupOwnedRootPreservesMarkedRoot(t *testing.T) {
	parent, toolDir := cleanupFixture(t)
	marker := filepath.Join(toolDir, PreserveMarkerName)
	if err := os.WriteFile(marker, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}

	stopCalled := false
	processCheckCalled := false
	err := CleanupOwnedRoot(parent, toolDir, filepath.Join(toolDir, "gc-home"), func() error {
		stopCalled = true
		return nil
	}, func(string, string) ([]int, error) {
		processCheckCalled = true
		return nil, nil
	})
	if err == nil {
		t.Fatal("CleanupOwnedRoot() error = nil, want marked-root refusal")
	}
	if stopCalled || processCheckCalled {
		t.Fatalf("cleanup callbacks called before marker refusal: stop=%t processCheck=%t", stopCalled, processCheckCalled)
	}
	assertPathExists(t, parent)
	assertPathExists(t, marker)
}

func TestCleanupOwnedRootRejectsEmptyRunRoot(t *testing.T) {
	err := CleanupOwnedRoot("", "", "", func() error { return nil }, func(string, string) ([]int, error) {
		return nil, nil
	})
	if err == nil {
		t.Fatal("CleanupOwnedRoot() error = nil, want missing-root refusal")
	}
}

func TestCleanupOwnedRootRequiresStopCallback(t *testing.T) {
	parent, toolDir := cleanupFixture(t)
	processCheckCalled := false
	err := CleanupOwnedRoot(parent, toolDir, "", nil, func(string, string) ([]int, error) {
		processCheckCalled = true
		return nil, nil
	})
	if err == nil {
		t.Fatal("CleanupOwnedRoot() error = nil, want missing-stop-callback refusal")
	}
	if processCheckCalled {
		t.Fatal("process check ran without a supervisor stop callback")
	}
	assertPathExists(t, parent)
	assertPathExists(t, toolDir)
}

func TestCleanupOwnedRootRequiresProcessCheck(t *testing.T) {
	parent, toolDir := cleanupFixture(t)
	stopCalled := false
	err := CleanupOwnedRoot(parent, toolDir, "", func() error {
		stopCalled = true
		return nil
	}, nil)
	if err == nil {
		t.Fatal("CleanupOwnedRoot() error = nil, want missing-process-check refusal")
	}
	if stopCalled {
		t.Fatal("supervisor stop ran without a process check")
	}
	assertPathExists(t, parent)
	assertPathExists(t, toolDir)
}

func TestCleanupOwnedRootPreservesRootWhenSupervisorStopFails(t *testing.T) {
	parent, toolDir := cleanupFixture(t)
	wantErr := errors.New("supervisor stop failed")
	processCheckCalled := false
	err := CleanupOwnedRoot(parent, toolDir, filepath.Join(toolDir, "gc-home"), func() error {
		return wantErr
	}, func(string, string) ([]int, error) {
		processCheckCalled = true
		return nil, nil
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("CleanupOwnedRoot() error = %v, want wrapped supervisor stop error", err)
	}
	if processCheckCalled {
		t.Fatal("process check ran after supervisor stop failed")
	}
	assertPathExists(t, parent)
	assertPathExists(t, toolDir)
}

func TestCleanupOwnedRootPreservesRootWhenProcessSnapshotFails(t *testing.T) {
	parent, toolDir := cleanupFixture(t)
	wantErr := errors.New("process snapshot unavailable")
	err := CleanupOwnedRoot(parent, toolDir, filepath.Join(toolDir, "gc-home"), func() error { return nil }, func(string, string) ([]int, error) {
		return nil, wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("CleanupOwnedRoot() error = %v, want wrapped snapshot error", err)
	}
	assertPathExists(t, parent)
	assertPathExists(t, toolDir)
}

func TestCleanupOwnedRootPreservesRootWhileFixtureProcessRemains(t *testing.T) {
	parent, toolDir := cleanupFixture(t)
	err := CleanupOwnedRoot(parent, toolDir, filepath.Join(toolDir, "gc-home"), func() error { return nil }, func(string, string) ([]int, error) {
		return []int{4321}, nil
	})
	if err == nil {
		t.Fatal("CleanupOwnedRoot() error = nil, want live-process refusal")
	}
	assertPathExists(t, parent)
	assertPathExists(t, toolDir)
}

func TestCleanupOwnedRootRemovesVerifiedEmptyRoot(t *testing.T) {
	parent, toolDir := cleanupFixture(t)
	stopCalled := false
	processCheckCalled := false
	err := CleanupOwnedRoot(parent, toolDir, filepath.Join(toolDir, "gc-home"), func() error {
		stopCalled = true
		return nil
	}, func(root, gcHome string) ([]int, error) {
		processCheckCalled = true
		if root != parent || gcHome != filepath.Join(toolDir, "gc-home") {
			t.Fatalf("process check received root=%q gcHome=%q", root, gcHome)
		}
		return nil, nil
	})
	if err != nil {
		t.Fatalf("CleanupOwnedRoot() error = %v", err)
	}
	if !stopCalled || !processCheckCalled {
		t.Fatalf("cleanup callbacks not called: stop=%t processCheck=%t", stopCalled, processCheckCalled)
	}
	assertPathMissing(t, parent)
}

func TestCleanupOwnedRootUsesEmptyOnlyParentRemoval(t *testing.T) {
	parent, toolDir := cleanupFixture(t)
	evidence := filepath.Join(parent, "unexpected-evidence")
	if err := os.WriteFile(evidence, []byte("preserve"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := CleanupOwnedRoot(parent, toolDir, filepath.Join(toolDir, "gc-home"), func() error { return nil }, func(string, string) ([]int, error) {
		return nil, nil
	})
	if err == nil {
		t.Fatal("CleanupOwnedRoot() error = nil, want non-empty parent refusal")
	}
	assertPathExists(t, parent)
	assertPathExists(t, evidence)
	assertPathMissing(t, toolDir)
}

func TestCleanupOwnedRootRejectsParentAlreadyMissingAtRemoval(t *testing.T) {
	parent, toolDir := cleanupFixture(t)
	err := cleanupOwnedRoot(parent, toolDir, "", func() error { return nil }, func(string, string) ([]int, error) {
		return nil, nil
	}, func(string) error {
		return os.ErrNotExist
	})
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cleanupOwnedRoot() error = %v, want missing-parent removal error", err)
	}
	assertPathExists(t, parent)
	assertPathMissing(t, toolDir)
}

func cleanupFixture(t *testing.T) (string, string) {
	t.Helper()
	parent := filepath.Join(t.TempDir(), "run-parent")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	toolDir := filepath.Join(parent, "tools")
	if err := os.Mkdir(toolDir, 0o700); err != nil {
		t.Fatal(err)
	}
	return parent, toolDir
}

func assertPathExists(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); err != nil {
		t.Fatalf("Lstat(%q): %v", path, err)
	}
}

func assertPathMissing(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Fatalf("Lstat(%q) error = %v, want not-exist", path, err)
	}
}
