//go:build !windows

package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/git"
)

func TestLifecycleClaimWorkspaceHoldsNativeLockThroughProtectedCommit(t *testing.T) {
	setup, state, request := lifecycleClaimPublishedWorkspaceFixture(t)
	common, err := git.New(request.Work.Metadata[beadmeta.WorktreeRepoMetadataKey]).CommonDir()
	if err != nil {
		t.Fatal(err)
	}
	paths, err := filepath.Glob(filepath.Join(common, "gc-worktree-locks", "*.lock"))
	if err != nil || len(paths) != 1 {
		t.Fatalf("lock paths=%v error=%v", paths, err)
	}
	lock, err := os.OpenFile(paths[0], os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Close() }()
	called := false
	setup.store.afterNextPatch = func(string) {
		called = true
		err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
			t.Error("native lock absent during protected commit")
		} else if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			t.Fatal(err)
		}
	}
	if _, err := state.ClaimLifecycleWork(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("protected commit not observed")
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("native lock retained after claim: %v", err)
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}
}
