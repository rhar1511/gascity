package beads

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
)

func recordingBdRunner(calls *[][]string) CommandRunner {
	return func(_, name string, args ...string) ([]byte, error) {
		*calls = append(*calls, append([]string{name}, args...))
		if len(args) > 0 && (args[0] == "list" || args[0] == "query" || args[0] == "show") {
			return []byte("[]"), nil
		}
		return []byte("{}"), nil
	}
}

func deleteCallCount(calls [][]string) int {
	count := 0
	for _, call := range calls {
		if len(call) > 1 && call[1] == "delete" {
			count++
		}
	}
	return count
}

func TestBdStoreDeletePreflightProtectsRecoveryEvidence(t *testing.T) {
	for _, tc := range []struct {
		name     string
		metadata StringMap
		wantErr  error
	}{
		{
			name: "budget",
			metadata: StringMap{
				beadmeta.LifecycleRecoveryStateMetadataKey: `{"version":1,"work_item_id":"gc-budget","attempts":[]}`,
			},
			wantErr: ErrLifecycleMutationBlocked,
		},
		{
			name: "signed intent",
			metadata: StringMap{
				beadmeta.LifecycleRecoveryIntentMetadataKey: `{"request_id":"request-1"}`,
				beadmeta.LifecycleRecoveryIntentDigestKey:   "digest",
			},
			wantErr: ErrLifecycleIntentImmutable,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls [][]string
			row, err := json.Marshal([]map[string]any{{"id": "gc-protected", "status": "in_progress", "metadata": tc.metadata}})
			if err != nil {
				t.Fatal(err)
			}
			runner := func(_, name string, args ...string) ([]byte, error) {
				calls = append(calls, append([]string{name}, args...))
				if len(args) > 0 && args[0] == "show" {
					return row, nil
				}
				return []byte("{}"), nil
			}
			store := NewBdStore("/city", runner)
			if err := store.Delete("gc-protected"); !errors.Is(err, tc.wantErr) {
				t.Fatalf("Delete protected row = %v, want %v", err, tc.wantErr)
			}
			if deletes := deleteCallCount(calls); deletes != 0 {
				t.Fatalf("preflight refusal invoked %d delete command(s): %v", deletes, calls)
			}
		})
	}
}

func TestBdStoreDeletePreflightProtectsAttemptArchive(t *testing.T) {
	for _, batch := range []bool{false, true} {
		t.Run(strconv.FormatBool(batch), func(t *testing.T) {
			var calls [][]string
			archive := protectedAttemptEvidenceBead()
			row, err := json.Marshal([]map[string]any{{"id": "gc-archive", "status": "closed", "metadata": archive.Metadata}})
			if err != nil {
				t.Fatal(err)
			}
			store := NewBdStore("/city", func(_, name string, args ...string) ([]byte, error) {
				calls = append(calls, append([]string{name}, args...))
				if len(args) > 0 && args[0] == "show" {
					if args[len(args)-1] == "gc-archive" {
						return row, nil
					}
					return []byte("[]"), nil
				}
				return []byte("{}"), nil
			})
			if batch {
				err = store.DeleteBatch([]string{"gc-ordinary", "gc-archive"})
				var batchErr *BatchDeleteError
				if !errors.As(err, &batchErr) || len(batchErr.Committed) != 0 {
					t.Fatalf("DeleteBatch = %v, want no-commit BatchDeleteError", err)
				}
			} else {
				err = store.Delete("gc-archive")
			}
			if !errors.Is(err, ErrProtectedAttemptEvidenceArchive) {
				t.Fatalf("archive delete = %v, want protected archive", err)
			}
			if deletes := deleteCallCount(calls); deletes != 0 {
				t.Fatalf("archive preflight invoked %d delete commands", deletes)
			}
		})
	}
}

func TestBdStoreDeleteBatchPreflightsProtectedRowsBeforeChunkDelete(t *testing.T) {
	var calls [][]string
	protected, err := json.Marshal([]map[string]any{{"id": "gc-protected", "status": "open", "metadata": StringMap{
		beadmeta.LifecycleRecoveryStateMetadataKey: `{"version":1,"attempts":[{"request_id":"request-1"}]}`,
	}}})
	if err != nil {
		t.Fatal(err)
	}
	runner := func(_, name string, args ...string) ([]byte, error) {
		calls = append(calls, append([]string{name}, args...))
		if len(args) > 0 && args[0] == "show" {
			if args[len(args)-1] == "gc-protected" {
				return protected, nil
			}
			return []byte("[]"), nil
		}
		return []byte("{}"), nil
	}
	store := NewBdStore("/city", runner)
	err = store.DeleteBatch([]string{"gc-ordinary", "gc-protected"})
	if !errors.Is(err, ErrLifecycleMutationBlocked) {
		t.Fatalf("DeleteBatch protected row = %v, want ErrLifecycleMutationBlocked", err)
	}
	var batchErr *BatchDeleteError
	if !errors.As(err, &batchErr) || len(batchErr.Committed) != 0 {
		t.Fatalf("DeleteBatch error = %#v, want no-commit BatchDeleteError", err)
	}
	if deletes := deleteCallCount(calls); deletes != 0 {
		t.Fatalf("preflight refusal invoked %d delete command(s): %v", deletes, calls)
	}
}

func TestBdStoreDeletePreflightLeavesOrdinaryDeleteAvailable(t *testing.T) {
	var calls [][]string
	ordinary, err := json.Marshal([]map[string]any{{"id": "gc-ordinary", "status": "open"}})
	if err != nil {
		t.Fatal(err)
	}
	runner := func(_, name string, args ...string) ([]byte, error) {
		calls = append(calls, append([]string{name}, args...))
		if len(args) > 0 && args[0] == "show" {
			return ordinary, nil
		}
		return []byte("{}"), nil
	}
	store := NewBdStore("/city", runner)
	if err := store.Delete("gc-ordinary"); err != nil {
		t.Fatalf("Delete ordinary row = %v", err)
	}
	if deletes := deleteCallCount(calls); deletes != 1 {
		t.Fatalf("ordinary delete commands = %d, want 1: %v", deletes, calls)
	}
}

func TestBdStoreDeleteBatchBatchesInOneCall(t *testing.T) {
	var calls [][]string
	s := NewBdStore("/city", recordingBdRunner(&calls))
	if err := s.DeleteBatch([]string{"a", "b", "c"}); err != nil {
		t.Fatalf("DeleteBatch: %v", err)
	}
	if deleteCallCount(calls) != 1 {
		t.Fatalf("want 1 batched delete command, got calls=%v", calls)
	}
	var got string
	for _, call := range calls {
		if len(call) > 1 && call[1] == "delete" {
			got = strings.Join(call, " ")
		}
	}
	for _, want := range []string{"bd", "delete", "a", "b", "c", "--force"} {
		if !strings.Contains(got, want) {
			t.Errorf("batched call %q missing %q", got, want)
		}
	}
	// The batch delete must use --force (orphan external dependents), never
	// --cascade (recursively delete dependents outside the collected closure).
	// Passing --cascade here is the data-loss regression this test guards.
	if strings.Contains(got, "--cascade") {
		t.Errorf("batched call %q must not pass --cascade (would recursively delete external dependents)", got)
	}
}

func TestBdStoreDeleteBatchChunksLargeSets(t *testing.T) {
	var calls [][]string
	s := NewBdStore("/city", recordingBdRunner(&calls))
	n := bdDeleteBatchChunk + 5
	ids := make([]string, n)
	for i := range ids {
		ids[i] = "id" + strconv.Itoa(i)
	}
	if err := s.DeleteBatch(ids); err != nil {
		t.Fatalf("DeleteBatch: %v", err)
	}
	if deleteCallCount(calls) != 2 {
		t.Fatalf("want 2 chunked delete commands for %d ids (chunk=%d), got calls=%d: %v", n, bdDeleteBatchChunk, deleteCallCount(calls), calls)
	}
}

func TestBdStoreDeleteBatchEmptyIsNoop(t *testing.T) {
	called := false
	s := NewBdStore("/city", func(_, _ string, _ ...string) ([]byte, error) {
		called = true
		return nil, nil
	})
	if err := s.DeleteBatch(nil); err != nil {
		t.Fatalf("DeleteBatch(nil): %v", err)
	}
	if called {
		t.Fatalf("DeleteBatch(nil) should not invoke bd")
	}
}

// A later chunk failing after earlier chunks committed must report the
// committed ids so a caching layer can reconcile the partial success instead of
// treating the whole batch as untouched.
func TestBdStoreDeleteBatchReportsCommittedOnLaterChunkFailure(t *testing.T) {
	var call int
	runner := func(_, _ string, args ...string) ([]byte, error) {
		if len(args) > 0 && (args[0] == "list" || args[0] == "query" || args[0] == "show") {
			return []byte("[]"), nil
		}
		if len(args) == 0 || args[0] != "delete" {
			return []byte("{}"), nil
		}
		call++
		if call == 2 { // second chunk fails after the first committed
			return nil, errors.New("bd delete: backend unavailable")
		}
		return []byte("{}"), nil
	}
	s := NewBdStore("/city", runner)

	n := bdDeleteBatchChunk + 5 // two chunks: [0:chunk] then [chunk:chunk+5]
	ids := make([]string, n)
	for i := range ids {
		ids[i] = "id" + strconv.Itoa(i)
	}

	err := s.DeleteBatch(ids)
	var batchErr *BatchDeleteError
	if !errors.As(err, &batchErr) {
		t.Fatalf("DeleteBatch err = %v, want *BatchDeleteError", err)
	}
	if len(batchErr.Committed) != bdDeleteBatchChunk {
		t.Fatalf("Committed len = %d, want %d (the fully-applied first chunk)", len(batchErr.Committed), bdDeleteBatchChunk)
	}
	for i := 0; i < bdDeleteBatchChunk; i++ {
		if batchErr.Committed[i] != ids[i] {
			t.Fatalf("Committed[%d] = %q, want %q", i, batchErr.Committed[i], ids[i])
		}
	}
}

// A first-chunk failure has committed nothing, so the reported committed set is
// empty and a caching layer leaves the cache untouched.
func TestBdStoreDeleteBatchReportsNoCommittedOnFirstChunkFailure(t *testing.T) {
	runner := func(_, _ string, args ...string) ([]byte, error) {
		if len(args) > 0 && (args[0] == "list" || args[0] == "query" || args[0] == "show") {
			return []byte("[]"), nil
		}
		if len(args) == 0 || args[0] != "delete" {
			return []byte("{}"), nil
		}
		return nil, errors.New("bd delete: backend unavailable")
	}
	s := NewBdStore("/city", runner)

	err := s.DeleteBatch([]string{"a", "b", "c"})
	var batchErr *BatchDeleteError
	if !errors.As(err, &batchErr) {
		t.Fatalf("DeleteBatch err = %v, want *BatchDeleteError", err)
	}
	if len(batchErr.Committed) != 0 {
		t.Fatalf("Committed = %v, want empty on first-chunk failure", batchErr.Committed)
	}
}

// BdStore must advertise the batched delete capability so the wisp GC discovers
// it by interface assertion.
var _ BatchDeleter = (*BdStore)(nil)
