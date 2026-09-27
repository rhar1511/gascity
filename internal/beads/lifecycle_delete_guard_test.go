package beads

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/fsys"
)

func TestRecoveryBudgetCannotBeDeletedAcrossStores(t *testing.T) {
	openSQLite := func(t *testing.T) Store {
		t.Helper()
		store, err := OpenSQLiteStore(t.TempDir(), WithSQLiteStoreIDPrefix("gc"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := store.(interface{ CloseStore() error }).CloseStore(); err != nil {
				t.Errorf("close SQLite store: %v", err)
			}
		})
		return store
	}
	openFile := func(t *testing.T) Store {
		t.Helper()
		store, err := OpenFileStore(fsys.OSFS{}, filepath.Join(t.TempDir(), "beads.json"))
		if err != nil {
			t.Fatal(err)
		}
		return store
	}

	for _, tc := range []struct {
		name string
		open func(*testing.T) Store
	}{
		{name: "mem", open: func(*testing.T) Store { return &MemStore{HonorExplicitIDs: true} }},
		{name: "file", open: openFile},
		{name: "sqlite", open: openSQLite},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := tc.open(t)
			work, err := createRecoveryBudgetDeleteFixture(t, store, `{"version":1,"work_item_id":"gc-budget-work","scope":"city:pilot/city:pilot","attempts":[{"id":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","reserved_at":"2026-09-27T12:00:00Z","request_id":"request-1","request_digest":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","expected_revision":1}]}`)
			if err != nil {
				t.Fatal(err)
			}

			if err := store.Delete(work.ID); !errors.Is(err, ErrLifecycleMutationBlocked) {
				t.Fatalf("Delete budget-bearing work = %v, want ErrLifecycleMutationBlocked", err)
			}
			if writer, ok := ConditionalWriterFor(store); ok {
				if err := writer.DeleteIfMatch(work.ID, work.Revision); !errors.Is(err, ErrLifecycleMutationBlocked) {
					t.Fatalf("DeleteIfMatch budget-bearing work = %v, want ErrLifecycleMutationBlocked", err)
				}
			}
			if deleter, ok := store.(BatchDeleter); ok {
				if err := deleter.DeleteBatch([]string{work.ID}); !errors.Is(err, ErrLifecycleMutationBlocked) {
					t.Fatalf("DeleteBatch budget-bearing work = %v, want ErrLifecycleMutationBlocked", err)
				}
			}

			got, err := store.Get(work.ID)
			if err != nil {
				t.Fatalf("Get budget-bearing work after refused deletes: %v", err)
			}
			if got.Metadata[beadmeta.LifecycleRecoveryStateMetadataKey] != work.Metadata[beadmeta.LifecycleRecoveryStateMetadataKey] {
				t.Fatalf("recovery budget after refused deletes = %q, want unchanged %q", got.Metadata[beadmeta.LifecycleRecoveryStateMetadataKey], work.Metadata[beadmeta.LifecycleRecoveryStateMetadataKey])
			}

			ordinary, err := store.Create(Bead{Title: "ordinary deletable work"})
			if err != nil {
				t.Fatal(err)
			}
			if err := store.Delete(ordinary.ID); err != nil {
				t.Fatalf("ordinary Delete = %v", err)
			}
			if _, err := store.Get(ordinary.ID); !errors.Is(err, ErrNotFound) {
				t.Fatalf("Get ordinary bead after Delete = %v, want ErrNotFound", err)
			}
		})
	}
}

func TestSQLiteRetentionSkipsRecoveryBudgetRows(t *testing.T) {
	storeValue, err := OpenSQLiteStore(t.TempDir(), WithSQLiteStoreIDPrefix("gc"))
	if err != nil {
		t.Fatal(err)
	}
	store := storeValue.(*SQLiteStore)
	t.Cleanup(func() {
		if err := store.CloseStore(); err != nil {
			t.Errorf("close SQLite store: %v", err)
		}
	})
	work, err := createRecoveryBudgetDeleteFixture(t, store, `{"version":1,"work_item_id":"gc-retained-budget","scope":"city:pilot/city:pilot","attempts":[{"id":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","reserved_at":"2026-09-27T12:00:00Z","request_id":"request-2","request_digest":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","expected_revision":1}]}`)
	if err != nil {
		t.Fatal(err)
	}
	intent, err := store.Create(Bead{Title: "recovery intent", Type: "lifecycle-intent", Metadata: StringMap{
		beadmeta.LifecycleRecoveryIntentMetadataKey: `{"request_id":"request-3"}`,
	}})
	if err != nil {
		t.Fatal(err)
	}
	ordinary, err := store.Create(Bead{Title: "ordinary terminal row"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`UPDATE beads SET status='closed', updated_at=1 WHERE id IN (?,?,?)`, work.ID, intent.ID, ordinary.ID); err != nil {
		t.Fatalf("age terminal fixture rows: %v", err)
	}

	deleted, err := store.purgeTerminal(context.Background(), time.Second)
	if err != nil {
		t.Fatalf("purgeTerminal: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("purgeTerminal deleted %d row(s), want only ordinary terminal row", deleted)
	}
	for _, protected := range []struct {
		id, key, value string
	}{
		{work.ID, beadmeta.LifecycleRecoveryStateMetadataKey, work.Metadata[beadmeta.LifecycleRecoveryStateMetadataKey]},
		{intent.ID, beadmeta.LifecycleRecoveryIntentMetadataKey, intent.Metadata[beadmeta.LifecycleRecoveryIntentMetadataKey]},
	} {
		got, err := store.Get(protected.id)
		if err != nil || got.Metadata[protected.key] != protected.value {
			t.Fatalf("protected row %s after retention = %+v, %v", protected.id, got, err)
		}
	}
	if _, err := store.Get(ordinary.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ordinary terminal row after retention = %v, want ErrNotFound", err)
	}
}

func TestCachingDeleteBatchRefusesBudgetRowBeforeRemovingDependencies(t *testing.T) {
	backing := &MemStore{HonorExplicitIDs: true}
	work, err := createRecoveryBudgetDeleteFixture(t, backing, `{"version":1,"work_item_id":"gc-cached-budget","scope":"city:pilot/city:pilot","attempts":[]}`)
	if err != nil {
		t.Fatal(err)
	}
	other, err := backing.Create(Bead{ID: "gc-external", Title: "external dependent"})
	if err != nil {
		t.Fatal(err)
	}
	if err := backing.DepAdd(work.ID, other.ID, "blocks"); err != nil {
		t.Fatal(err)
	}
	cached := NewCachingStoreForTest(backing, nil)
	if err := cached.DeleteBatch([]string{work.ID}); !errors.Is(err, ErrLifecycleMutationBlocked) {
		t.Fatalf("cached DeleteBatch = %v, want ErrLifecycleMutationBlocked", err)
	}
	deps, err := backing.DepList(work.ID, "down")
	if err != nil {
		t.Fatal(err)
	}
	if len(deps) != 1 || deps[0].DependsOnID != other.ID {
		t.Fatalf("dependencies after refused batch delete = %+v, want original edge to %s", deps, other.ID)
	}
}

func TestCachingDeleteBatchDoesNotStripEdgesWhenBudgetAppearsAfterPreflight(t *testing.T) {
	backing := &recoveryStateOnDepListStore{MemStore: &MemStore{HonorExplicitIDs: true}}
	work, err := backing.Create(Bead{ID: "gc-cached-race", Title: "work"})
	if err != nil {
		t.Fatal(err)
	}
	other, err := backing.Create(Bead{ID: "gc-race-dependent", Title: "dependent"})
	if err != nil {
		t.Fatal(err)
	}
	if err := backing.DepAdd(work.ID, other.ID, "blocks"); err != nil {
		t.Fatal(err)
	}
	backing.recoveryState = `{"version":1,"work_item_id":"gc-cached-race","scope":"city:pilot/city:pilot","attempts":[]}`
	cached := NewCachingStoreForTest(backing, nil)
	if err := cached.DeleteBatch([]string{work.ID}); !errors.Is(err, ErrLifecycleMutationBlocked) {
		t.Fatalf("cached DeleteBatch after raced budget = %v, want ErrLifecycleMutationBlocked", err)
	}
	if !backing.reserved {
		t.Fatal("dependency snapshot did not install the racing recovery budget")
	}
	deps, err := backing.MemStore.DepList(work.ID, "down")
	if err != nil {
		t.Fatal(err)
	}
	if len(deps) != 1 || deps[0].DependsOnID != other.ID {
		t.Fatalf("dependencies after refused raced delete = %+v, want original edge to %s", deps, other.ID)
	}
	if _, err := backing.Get(work.ID); err != nil {
		t.Fatalf("work row after refused raced delete: %v", err)
	}
}

type recoveryStateOnDepListStore struct {
	*MemStore
	recoveryState string
	reserved      bool
}

func (s *recoveryStateOnDepListStore) DepList(id, direction string) ([]Dep, error) {
	if !s.reserved && s.recoveryState != "" {
		current, err := s.Get(id)
		if err != nil {
			return nil, err
		}
		if err := UpdateLifecycleRecoveryStateIfMatch(s.MemStore, id, current.Revision, "", s.recoveryState); err != nil {
			return nil, err
		}
		s.reserved = true
	}
	return s.MemStore.DepList(id, direction)
}

func createRecoveryBudgetDeleteFixture(t *testing.T, store Store, recoveryState string) (Bead, error) {
	t.Helper()
	work, err := store.Create(Bead{Title: "budget-bearing work", Type: "task", Status: "in_progress"})
	if err != nil {
		return Bead{}, err
	}
	current, err := store.Get(work.ID)
	if err != nil {
		return Bead{}, err
	}
	if err := UpdateLifecycleRecoveryStateIfMatch(store, work.ID, current.Revision, "", recoveryState); err != nil {
		return Bead{}, err
	}
	return store.Get(work.ID)
}
