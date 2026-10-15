//go:build !windows

package main

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"syscall"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/git"
)

type poolWorkspaceObservedCityStore struct {
	*beads.MemStore
	beforeWrite func() error
}

func (s *poolWorkspaceObservedCityStore) UpdateIfMatch(id string, revision int64, opts beads.UpdateOpts) error {
	if s.beforeWrite != nil {
		if err := s.beforeWrite(); err != nil {
			return err
		}
	}
	return s.MemStore.UpdateIfMatch(id, revision, opts)
}

func TestPoolLifecycleWorkspaceRetainsNativeLockThroughConditionalBinding(t *testing.T) {
	for _, reject := range []bool{false, true} {
		name := "success"
		if reject {
			name = "conditional refusal"
		}
		t.Run(name, func(t *testing.T) {
			setup, bp, request, spec := newPoolLifecycleWorkspaceFixture(t, nil)
			backing, ok := setup.cityStore.(*beads.MemStore)
			if !ok {
				t.Fatalf("fixture session store has changed type: %T", setup.cityStore)
			}
			watched := &poolWorkspaceObservedCityStore{MemStore: backing}
			bp.beadStore = watched
			row, err := watched.Create(beads.Bead{Type: "session"})
			if err != nil {
				t.Fatal(err)
			}
			info, err := sessionFrontDoor(watched).Get(row.ID)
			if err != nil {
				t.Fatal(err)
			}
			common, err := git.New(spec.RepoDir).CommonDir()
			if err != nil {
				t.Fatal(err)
			}
			paths, err := filepath.Glob(filepath.Join(common, "gc-worktree-locks", "*.lock"))
			if err != nil || len(paths) != 1 {
				t.Fatalf("native fixture lock paths=%v error=%v", paths, err)
			}
			lock, err := os.OpenFile(paths[0], os.O_RDWR, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = lock.Close() }()
			calls := 0
			sentinel := errors.New("conditional store refused")
			watched.beforeWrite = func() error {
				calls++
				err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
				if err == nil {
					_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
					t.Error("native Ensure/Cleanup lock was released before session CAS")
					return errors.New("missing native custody")
				}
				if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
					t.Fatalf("unexpected lock probe error: %v", err)
				}
				if reject {
					return sentinel
				}
				return nil
			}
			bound, err := bindPoolSessionTriggerBead(bp, &setup.fixture.cfg.Agents[0], "pilot/worker", info, request)
			if calls != 1 {
				t.Fatalf("conditional write calls=%d", calls)
			}
			if reject {
				if !errors.Is(err, sentinel) || !reflect.DeepEqual(bound, info) {
					t.Fatalf("refusal changed caller identity: bound=%+v error=%v", bound, err)
				}
				current, getErr := watched.Get(row.ID)
				if getErr != nil || !reflect.DeepEqual(current.Metadata, row.Metadata) {
					t.Fatalf("refused CAS changed persisted binding: %+v %v", current, getErr)
				}
			} else if err != nil || bound.WorkDir != spec.Path {
				t.Fatalf("binding failed: %+v %v", bound, err)
			}
			if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
				t.Fatalf("native lock retained after return: %v", err)
			}
			if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_UN); err != nil {
				t.Fatal(err)
			}
		})
	}
}
