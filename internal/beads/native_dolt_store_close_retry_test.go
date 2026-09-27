package beads

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	beadslib "github.com/steveyegge/beads"
	beadops "github.com/steveyegge/beads/issueops"
)

// closeReopenSpy serves an issue whose status can change while a checked native
// write is attempted. The mutate callback models either a lost row-version race
// or a successful state change observed by a later read.
type closeReopenSpy struct {
	*nativeDoltStorageSpy
	gets    int32
	mutates int32
}

// newCloseSpy returns a spy whose issue starts open. closeIssue runs mutate,
// which decides what that attempt does to the stored status and what it
// returns, so a test can model "a concurrent actor closed it before the replay"
// as easily as "this attempt lost the race and nothing landed".
func newCloseSpy(mutate func(attempt int32, markClosed func()) error) *closeReopenSpy {
	spy := &closeReopenSpy{}
	closed := int32(0)
	spy.nativeDoltStorageSpy = &nativeDoltStorageSpy{
		getIssue: func(_ context.Context, id string) (*beadslib.Issue, error) {
			atomic.AddInt32(&spy.gets, 1)
			status := beadslib.StatusOpen
			if atomic.LoadInt32(&closed) == 1 {
				status = beadslib.StatusClosed
			}
			version := int64(1 + atomic.LoadInt32(&closed))
			return &beadslib.Issue{ID: id, Status: status, IssueType: beadslib.TypeTask, Priority: 2, RowVersion: version}, nil
		},
		closeIssueChecked: func(_ context.Context, _ string, _ string, opts beadslib.CloseIssueOptions) (beadslib.CloseIssueResult, error) {
			if opts.ExpectedVersion == nil {
				return beadslib.CloseIssueResult{}, errors.New("CloseIssueChecked omitted expected revision")
			}
			n := atomic.AddInt32(&spy.mutates, 1)
			err := mutate(n, func() { atomic.StoreInt32(&closed, 1) })
			return beadslib.CloseIssueResult{}, err
		},
	}
	return spy
}

func TestNativeDoltStoreCloseRetriesSerializationConflict(t *testing.T) {
	spy := newCloseSpy(func(attempt int32, markClosed func()) error {
		if attempt == 1 {
			// Lost the race: nothing landed.
			return errors.New(serializationConflictErr)
		}
		markClosed()
		return nil
	})
	store := newNativeDoltStoreForTest(spy)

	if err := store.Close("gc-1"); err != nil {
		t.Fatalf("Close after one serialization conflict: got %v, want nil", err)
	}
	if got := atomic.LoadInt32(&spy.mutates); got != 2 {
		t.Fatalf("CloseIssue attempts = %d, want 2 (one conflict, one retry)", got)
	}
}

// TestNativeDoltStoreCloseReplayShortCircuitsWhenAnotherActorClosedTheBead
// covers why the re-read has to sit INSIDE the retried unit. This store loses
// the race and a different writer closes the bead before the replay runs. The
// replay must notice that and not issue a second close.
//
// The assertion that carries the weight is the checked CloseIssue count. The
// replay must re-read and short-circuit if another writer already closed it.
func TestNativeDoltStoreCloseReplayShortCircuitsWhenAnotherActorClosedTheBead(t *testing.T) {
	spy := newCloseSpy(func(attempt int32, markClosed func()) error {
		if attempt == 1 {
			// This attempt lost the race and wrote nothing; a concurrent
			// actor closed the bead before the replay reads again. The
			// flip is synchronous here, so this models ordering, not
			// backoff timing.
			markClosed()
			return errors.New(serializationConflictErr)
		}
		t.Errorf("CloseIssueChecked called %d times; the replay must short-circuit on the already-closed bead", attempt)
		return nil
	})
	store := newNativeDoltStoreForTest(spy)

	if err := store.Close("gc-1"); err != nil {
		t.Fatalf("Close when another actor closed the bead mid-retry: got %v, want nil", err)
	}
	if got := atomic.LoadInt32(&spy.mutates); got != 1 {
		t.Fatalf("CloseIssueChecked attempts = %d, want 1 (the replay must not re-close)", got)
	}
	if got := atomic.LoadInt32(&spy.gets); got != 2 {
		t.Fatalf("GetIssue reads = %d, want 2 (the replay must re-read, not reuse the first snapshot)", got)
	}
}

func TestNativeDoltStoreCloseStopsAtAttemptLimit(t *testing.T) {
	spy := newCloseSpy(func(int32, func()) error {
		return errors.New(serializationConflictErr)
	})
	store := newNativeDoltStoreForTest(spy)

	err := store.Close("gc-1")
	if err == nil {
		t.Fatal("Close with unrelenting conflicts: got nil, want the serialization error")
	}
	if !isNativeDoltSerializationConflict(err) {
		t.Fatalf("returned error lost its serialization-conflict identity: %v", err)
	}
	if got := atomic.LoadInt32(&spy.mutates); got != int32(nativeWriteAttempts) {
		t.Fatalf("CloseIssueChecked attempts = %d, want %d", got, nativeWriteAttempts)
	}
}

func TestNativeDoltStoreCloseDoesNotRetryNonConflictErrors(t *testing.T) {
	spy := newCloseSpy(func(int32, func()) error {
		return errors.New("Error 1062 (23000): duplicate entry")
	})
	store := newNativeDoltStoreForTest(spy)

	err := store.Close("gc-1")
	if err == nil || !strings.Contains(err.Error(), "duplicate entry") {
		t.Fatalf("Close error = %v, want the duplicate-entry error", err)
	}
	if got := atomic.LoadInt32(&spy.mutates); got != 1 {
		t.Fatalf("CloseIssueChecked attempts = %d, want 1 (non-conflict errors must not retry)", got)
	}
}

func TestNativeDoltCloseCheckedRejectsAdmissionRacingValidation(t *testing.T) {
	admitted := false
	closed := false
	rowVersion := int64(7)
	checkedCalls := 0
	uncheckedCalls := 0
	metadataFor := func() json.RawMessage {
		if !admitted {
			return json.RawMessage(`{}`)
		}
		raw, _ := json.Marshal(map[string]string{beadmeta.LifecycleAdmissionReceiptMetadataKey: "signed-admission"})
		return raw
	}
	storage := &nativeDoltStorageSpy{
		getIssue: func(_ context.Context, id string) (*beadslib.Issue, error) {
			status := beadslib.StatusOpen
			if closed {
				status = beadslib.StatusClosed
			}
			return &beadslib.Issue{ID: id, Status: status, IssueType: beadslib.TypeTask, Priority: 2, RowVersion: rowVersion, Metadata: metadataFor()}, nil
		},
		closeIssueChecked: func(_ context.Context, _ string, _ string, opts beadslib.CloseIssueOptions) (beadslib.CloseIssueResult, error) {
			checkedCalls++
			if opts.ExpectedVersion == nil || *opts.ExpectedVersion != 7 {
				t.Fatalf("checked close expected version = %v, want 7", opts.ExpectedVersion)
			}
			// Admission wins after the controller's read but before the close CAS.
			admitted = true
			rowVersion++
			return beadslib.CloseIssueResult{}, beadslib.ErrVersionMismatch
		},
		closeIssue: func(context.Context, string, string, string, string) error {
			uncheckedCalls++
			closed = true
			return nil
		},
	}
	store := newNativeDoltStoreForTest(storage)

	err := store.Close("gc-racing-admission")
	if !errors.Is(err, ErrLifecycleCompletionRequired) {
		t.Fatalf("Close error = %v, want verified-completion refusal after re-read", err)
	}
	if checkedCalls != 1 {
		t.Fatalf("checked close calls = %d, want 1", checkedCalls)
	}
	if uncheckedCalls != 0 || closed {
		t.Fatalf("unchecked close calls = %d, closed = %v; stale validation must not close enrolled work", uncheckedCalls, closed)
	}
}

func TestNativeDoltSetMetadataBatchAdmissionRaceCannotClearEvidence(t *testing.T) {
	rowVersion := int64(11)
	metadata := json.RawMessage(`{}`)
	updateCalls := 0
	storage := &nativeDoltStorageSpy{
		getIssue: func(_ context.Context, id string) (*beadslib.Issue, error) {
			return &beadslib.Issue{
				ID:         id,
				Status:     beadslib.StatusOpen,
				IssueType:  beadslib.TypeTask,
				Priority:   2,
				RowVersion: rowVersion,
				Metadata:   slices.Clone(metadata),
			}, nil
		},
		updateIssueChecked: func(_ context.Context, _ string, updates map[string]interface{}, _ string, opts beadslib.UpdateIssueOptions) error {
			updateCalls++
			if opts.ExpectedVersion == nil {
				t.Fatal("metadata update omitted expected revision")
			}
			if *opts.ExpectedVersion != rowVersion {
				return beadslib.ErrVersionMismatch
			}
			raw, ok := updates["metadata"].(json.RawMessage)
			if !ok {
				t.Fatalf("metadata update type = %T, want json.RawMessage", updates["metadata"])
			}
			metadata = slices.Clone(raw)
			rowVersion++
			return nil
		},
	}
	store := newNativeDoltStoreForTest(storage)
	store.afterMetadataMergeRead = func(string) {
		// Simulate an admission receipt committing after the guard read but
		// before its checked metadata write.
		if rowVersion == 11 {
			metadata, _ = json.Marshal(map[string]string{beadmeta.LifecycleAdmissionReceiptMetadataKey: "signed-admission"})
			rowVersion++
		}
	}

	err := store.SetMetadataBatch("gc-metadata-race", map[string]string{beadmeta.LifecycleAdmissionReceiptMetadataKey: ""})
	if !errors.Is(err, ErrLifecycleMutationBlocked) {
		t.Fatalf("SetMetadataBatch error = %v, want enrolled-evidence refusal after retry", err)
	}
	if updateCalls != 1 {
		t.Fatalf("UpdateIssueChecked calls = %d, want one stale CAS refusal", updateCalls)
	}
	var got map[string]string
	if err := json.Unmarshal(metadata, &got); err != nil {
		t.Fatalf("decode committed metadata: %v", err)
	}
	if got[beadmeta.LifecycleAdmissionReceiptMetadataKey] != "signed-admission" {
		t.Fatalf("admission receipt after concurrent clear attempt = %q, want preserved receipt", got[beadmeta.LifecycleAdmissionReceiptMetadataKey])
	}
}

// TestNativeDoltStoreCloseAllRecoversWhenTheCloseConflictsAfterMetadataLanded
// is the originally reported failure, at the level it was reported. CloseAll
// calls the retried SetMetadataBatch and then Close. While Close was unretried,
// a conflict there returned an error with the metadata already written, leaving
// a bead carrying close metadata that was never closed. That reached users as
// an HTTP 500 on session suspend.
//
// This test drives CloseAll rather than Close so the asymmetry itself is
// guarded. Reverting the Close wrap alone leaves every direct-Close test in
// this file failing, but nothing would record that the pairing is the point.
func TestNativeDoltStoreCloseAllRecoversWhenTheCloseConflictsAfterMetadataLanded(t *testing.T) {
	var (
		closed     int32
		metadata   int32
		closes     int32
		rowVersion int64 = 1
	)
	issue := func(id string) *beadslib.Issue {
		status := beadslib.StatusOpen
		if atomic.LoadInt32(&closed) == 1 {
			status = beadslib.StatusClosed
		}
		return &beadslib.Issue{ID: id, Status: status, IssueType: beadslib.TypeTask, Priority: 2, RowVersion: rowVersion}
	}
	spy := &nativeDoltStorageSpy{
		getIssue: func(_ context.Context, id string) (*beadslib.Issue, error) {
			return issue(id), nil
		},
		// CloseAll's own precondition read goes through Get, which uses
		// SearchIssues rather than GetIssue.
		searchIssues: func(_ context.Context, _ string, filter beadslib.IssueFilter) ([]*beadslib.Issue, error) {
			out := make([]*beadslib.Issue, 0, len(filter.IDs))
			for _, id := range filter.IDs {
				out = append(out, issue(id))
			}
			return out, nil
		},
		updateIssueChecked: func(context.Context, string, map[string]interface{}, string, beadslib.UpdateIssueOptions) error {
			atomic.AddInt32(&metadata, 1)
			rowVersion++
			return nil
		},
		closeIssueChecked: func(_ context.Context, _ string, _ string, opts beadslib.CloseIssueOptions) (beadslib.CloseIssueResult, error) {
			if opts.ExpectedVersion == nil || *opts.ExpectedVersion != rowVersion {
				return beadslib.CloseIssueResult{}, beadslib.ErrVersionMismatch
			}
			if atomic.AddInt32(&closes, 1) == 1 {
				return beadslib.CloseIssueResult{}, errors.New(serializationConflictErr)
			}
			atomic.StoreInt32(&closed, 1)
			rowVersion++
			return beadslib.CloseIssueResult{}, nil
		},
	}
	store := newNativeDoltStoreForTest(spy)

	n, err := store.CloseAll([]string{"gc-1"}, map[string]string{"gc.close_reason": "suspended"})
	if err != nil {
		t.Fatalf("CloseAll with one conflicting close: got %v, want nil", err)
	}
	if n != 1 {
		t.Fatalf("CloseAll closed = %d, want 1", n)
	}
	if got := atomic.LoadInt32(&closes); got != 2 {
		t.Fatalf("CloseIssueChecked attempts = %d, want 2 (one conflict, one retry)", got)
	}
	// The metadata write must not be replayed by the Close retry: the two are
	// separately retried units, and only the inner one lost its race.
	if got := atomic.LoadInt32(&metadata); got != 1 {
		t.Fatalf("metadata writes = %d, want 1 (the Close retry must not re-run SetMetadataBatch)", got)
	}
}

type nativeDoltLifecycleSpy struct {
	beadops.Lifecycle
	reopen func(context.Context, beadops.ReopenRequest) (beadops.ReopenResult, error)
}

func (s nativeDoltLifecycleSpy) Reopen(ctx context.Context, request beadops.ReopenRequest) (beadops.ReopenResult, error) {
	if s.reopen == nil {
		return beadops.ReopenResult{}, errors.New("lifecycle Reopen test seam not configured")
	}
	return s.reopen(ctx, request)
}

// newReopenSpy mirrors newCloseSpy for the opposite transition: it requires
// the expected revision on the public lifecycle Reopen operation.
func newReopenSpy(mutate func(attempt int32, markOpen func()) error) *closeReopenSpy {
	spy := &closeReopenSpy{}
	opened := int32(0)
	spy.nativeDoltStorageSpy = &nativeDoltStorageSpy{
		getIssue: func(_ context.Context, id string) (*beadslib.Issue, error) {
			atomic.AddInt32(&spy.gets, 1)
			status := beadslib.StatusClosed
			if atomic.LoadInt32(&opened) == 1 {
				status = beadslib.StatusOpen
			}
			version := int64(1 + atomic.LoadInt32(&opened))
			return &beadslib.Issue{ID: id, Status: status, IssueType: beadslib.TypeTask, Priority: 2, RowVersion: version}, nil
		},
		issueLifecycle: func() (beadops.Lifecycle, error) {
			return nativeDoltLifecycleSpy{
				reopen: func(_ context.Context, request beadops.ReopenRequest) (beadops.ReopenResult, error) {
					if request.ExpectedVersion == nil || *request.ExpectedVersion != 1 {
						return beadops.ReopenResult{}, errors.New("lifecycle Reopen omitted expected revision")
					}
					n := atomic.AddInt32(&spy.mutates, 1)
					err := mutate(n, func() { atomic.StoreInt32(&opened, 1) })
					return beadops.ReopenResult{Changed: err == nil}, err
				},
			}, nil
		},
	}
	return spy
}

func TestNativeDoltStoreReopenRetriesSerializationConflict(t *testing.T) {
	spy := newReopenSpy(func(attempt int32, markOpen func()) error {
		if attempt == 1 {
			return errors.New(serializationConflictErr)
		}
		markOpen()
		return nil
	})
	store := newNativeDoltStoreForTest(spy)

	if err := store.Reopen("gc-1"); err != nil {
		t.Fatalf("Reopen after one serialization conflict: got %v, want nil", err)
	}
	if got := atomic.LoadInt32(&spy.mutates); got != 2 {
		t.Fatalf("checked reopen updates = %d, want 2 (one conflict, one retry)", got)
	}
}

func TestNativeDoltStoreReopenReplayShortCircuitsWhenAnotherActorReopenedTheBead(t *testing.T) {
	spy := newReopenSpy(func(attempt int32, markOpen func()) error {
		if attempt == 1 {
			markOpen()
			return errors.New(serializationConflictErr)
		}
		t.Errorf("lifecycle Reopen called %d times; replay must short-circuit on the already-open bead", attempt)
		return nil
	})
	store := newNativeDoltStoreForTest(spy)

	if err := store.Reopen("gc-1"); err != nil {
		t.Fatalf("Reopen when another actor reopened the bead mid-retry: got %v, want nil", err)
	}
	if got := atomic.LoadInt32(&spy.mutates); got != 1 {
		t.Fatalf("lifecycle Reopen calls = %d, want 1 (the replay must not re-open)", got)
	}
	if got := atomic.LoadInt32(&spy.gets); got != 2 {
		t.Fatalf("GetIssue reads = %d, want 2 (the replay must re-read)", got)
	}
}

func TestNativeDoltReopenCASRejectsAdmissionRacingValidation(t *testing.T) {
	rowVersion := int64(23)
	status := beadslib.StatusClosed
	metadata := json.RawMessage(`{}`)
	updateCalls := 0
	storage := &nativeDoltStorageSpy{
		getIssue: func(_ context.Context, id string) (*beadslib.Issue, error) {
			return &beadslib.Issue{ID: id, Status: status, IssueType: beadslib.TypeTask, Priority: 2, RowVersion: rowVersion, Metadata: slices.Clone(metadata)}, nil
		},
		issueLifecycle: func() (beadops.Lifecycle, error) {
			return nativeDoltLifecycleSpy{reopen: func(_ context.Context, request beadops.ReopenRequest) (beadops.ReopenResult, error) {
				updateCalls++
				if request.ExpectedVersion == nil || *request.ExpectedVersion != 23 {
					t.Fatalf("lifecycle Reopen expected version = %v, want 23", request.ExpectedVersion)
				}
				// Admission commits after validation but before the checked
				// status change. Its row-version bump refuses the stale reopen.
				metadata, _ = json.Marshal(map[string]string{beadmeta.LifecycleAdmissionReceiptMetadataKey: "signed-admission"})
				rowVersion++
				return beadops.ReopenResult{}, beadslib.ErrVersionMismatch
			}}, nil
		},
	}
	store := newNativeDoltStoreForTest(storage)

	err := store.Reopen("gc-reopen-admission-race")
	if !errors.Is(err, ErrLifecycleMutationBlocked) {
		t.Fatalf("Reopen error = %v, want enrollment refusal after checked retry", err)
	}
	if updateCalls != 1 {
		t.Fatalf("UpdateIssueChecked calls = %d, want one stale CAS refusal", updateCalls)
	}
	if status != beadslib.StatusClosed {
		t.Fatalf("durable status = %q, want closed", status)
	}
	var got map[string]string
	if err := json.Unmarshal(metadata, &got); err != nil {
		t.Fatalf("decode concurrent metadata: %v", err)
	}
	if got[beadmeta.LifecycleAdmissionReceiptMetadataKey] != "signed-admission" {
		t.Fatalf("concurrent admission receipt = %q, want preserved receipt", got[beadmeta.LifecycleAdmissionReceiptMetadataKey])
	}
}

func TestNativeDoltReopenDelegatesDoneCategoryHandlingToLifecycleRole(t *testing.T) {
	status := beadslib.StatusInProgress
	rowVersion := int64(31)
	called := 0
	storage := &nativeDoltStorageSpy{
		getIssue: func(_ context.Context, id string) (*beadslib.Issue, error) {
			return &beadslib.Issue{ID: id, Status: status, IssueType: beadslib.TypeTask, Priority: 2, RowVersion: rowVersion}, nil
		},
		issueLifecycle: func() (beadops.Lifecycle, error) {
			return nativeDoltLifecycleSpy{reopen: func(_ context.Context, request beadops.ReopenRequest) (beadops.ReopenResult, error) {
				called++
				if request.ExpectedVersion == nil || *request.ExpectedVersion != rowVersion {
					t.Fatalf("Reopen expected version = %v, want %d", request.ExpectedVersion, rowVersion)
				}
				// The public contract leaves non-done statuses unchanged.
				return beadops.ReopenResult{Changed: false}, nil
			}}, nil
		},
		updateIssueChecked: func(context.Context, string, map[string]interface{}, string, beadslib.UpdateIssueOptions) error {
			return errors.New("generic update must not replace lifecycle Reopen")
		},
	}
	store := newNativeDoltStoreForTest(storage)

	if err := store.Reopen("gc-active"); err != nil {
		t.Fatalf("Reopen(in_progress): %v", err)
	}
	if called != 1 {
		t.Fatalf("lifecycle Reopen calls = %d, want 1", called)
	}
	if status != beadslib.StatusInProgress {
		t.Fatalf("status = %q, want in_progress; public lifecycle role owns done-category behavior", status)
	}
}
