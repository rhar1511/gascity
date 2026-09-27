package beads

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	beadslib "github.com/steveyegge/beads"
)

func TestAbsentMetadataKeyFiltersBeforeLimit(t *testing.T) {
	for _, backend := range []string{"memory", "sqlite"} {
		t.Run(backend, func(t *testing.T) {
			var store Store = NewMemStore()
			if backend == "sqlite" {
				var err error
				store, err = OpenSQLiteStore(filepath.Join(t.TempDir(), "beads"))
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = store.(*SQLiteStore).CloseStore() })
			}
			for i, metadata := range []map[string]string{{"private": "payload"}, {"private": ""}, nil} {
				_, err := store.Create(Bead{Title: "row", Type: "molecule", Metadata: metadata, CreatedAt: time.Unix(int64(100+i), 0)})
				if err != nil {
					t.Fatal(err)
				}
			}
			rows, err := store.List(ListQuery{Type: "molecule", IncludeClosed: true, Sort: SortCreatedAsc, Limit: 1, AbsentMetadataKey: "private"})
			if err != nil || len(rows) != 1 {
				t.Fatalf("List = %+v, %v", rows, err)
			}
			if _, present := rows[0].Metadata["private"]; present {
				t.Fatal("private prefix consumed the bounded public page")
			}
		})
	}
}

func TestNativeCountAbsentMetadataKey(t *testing.T) {
	calls := 0
	store := newNativeDoltStoreForTest(&nativeDoltStorageSpy{
		countIssues: func(_ context.Context, _ string, filter beadslib.IssueFilter) (int64, error) {
			calls++
			if filter.HasMetadataKey == "private" {
				return 2, nil
			}
			return 3, nil
		},
	})
	got, err := store.Count(context.Background(), ListQuery{Type: "molecule", IncludeClosed: true, AbsentMetadataKey: "private"})
	if !errors.Is(err, ErrCountUnsupported) || got != 0 || calls != 0 {
		t.Fatalf("Count=%d calls=%d err=%v", got, calls, err)
	}
}

func TestNativeListAbsentMetadataKeyDoesNotLosePublicRowAtLimit(t *testing.T) {
	store := newNativeDoltStoreForTest(&nativeDoltStorageSpy{
		searchIssues: func(_ context.Context, _ string, filter beadslib.IssueFilter) ([]*beadslib.Issue, error) {
			rows := []*beadslib.Issue{
				{ID: "private", Status: "open", IssueType: "task", CreatedAt: time.Unix(100, 0), Metadata: json.RawMessage(`{"private":"payload"}`)},
				{ID: "public", Status: "open", IssueType: "task", CreatedAt: time.Unix(101, 0)},
			}
			if filter.Limit > 0 && len(rows) > filter.Limit {
				rows = rows[:filter.Limit]
			}
			return rows, nil
		},
	})
	rows, err := store.List(ListQuery{Type: "task", Sort: SortCreatedAsc, Limit: 1, AbsentMetadataKey: "private"})
	if err != nil || len(rows) != 1 || rows[0].ID != "public" {
		t.Fatalf("native prefix lost the public row: %+v err=%v", rows, err)
	}
}
