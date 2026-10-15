package api

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// The two ways internal/api reaches BdStore's unread-store notice
// (internal/beads/unread_store_notice.go), and the two things that notice must
// never do here. Both are regressions of the same mistake — a diagnostic
// treating itself as free — measured against this package's real read path
// rather than a model of it.
//
// The API is where it bites hardest: State.ScopedStoreLike hands the status
// handler a BRAND NEW bd-backed store per request (cmd/gc's
// scopedBdStoreForCity/scopedBdStoreForRig construct one every call), and
// statusWorkCounts fans that out over the city plus every rig concurrently,
// all inside statusStoreReadTimeout.

// unreadScope writes the on-disk shape gc's metadata canonicalization leaves
// behind: metadata.json naming the server store, and the embedded database it
// stopped reading still on disk.
func unreadScope(t *testing.T) string {
	t.Helper()
	scope := t.TempDir()
	if err := os.MkdirAll(filepath.Join(scope, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(map[string]string{
		"database": "dolt", "backend": "dolt", "dolt_mode": "server", "dolt_database": "jc",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scope, ".beads", "metadata.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(scope, ".beads", "embeddeddolt", "jc", ".dolt"), 0o755); err != nil {
		t.Fatal(err)
	}
	return scope
}

// statusDeadlineNoticeWriter observes writes while retaining the original sink.
type statusDeadlineNoticeWriter func([]byte) (int, error)

func (w statusDeadlineNoticeWriter) Write(p []byte) (int, error) { return w(p) }

// TestStatusListUnderADeadlineIsUnchangedByTheUnreadStoreNotice is the
// deadline-safety proof, and it fails on any diagnostic that asks bd a question
// of its own inside List or Ready.
//
// Store.List takes no context, so the store cannot see the budget its caller is
// holding — statusListStoreWithTimeout races the read against a 1s per-store
// deadline and reports "list timed out" if it loses. A probe subprocess inside
// List is charged to that budget. Measured before this was removed, with the
// deadline shortened to 250ms and bd answering the probe argv slower than it:
//
//	statusListStoreWithTimeout(ctx, state, store, ListQuery{AllowScan:true})
//	  => rows=0 err="list timed out: context deadline exceeded"   (250ms)
//	control, GC_BD_ALLOW_UNREAD_STORE_READ=1
//	  => rows=0 err=<nil>                                         (72µs)
//
// bd had already answered that List successfully. The whole budget went to the
// diagnostic, which means the notice could only ever turn a successful read
// into an error — the opposite of the claim that it leaves every answer
// byte-identical. Binding the probe child to the caller's context does not help
// either: it kills the child, and the budget is still spent.
//
// This runs both arms, so a regression is visible as a difference rather than
// as a bare timeout.
func TestStatusListUnderADeadlineIsUnchangedByTheUnreadStoreNotice(t *testing.T) {
	old := statusStoreReadTimeout
	statusStoreReadTimeout = 250 * time.Millisecond
	t.Cleanup(func() { statusStoreReadTimeout = old })

	// The caller's context outlives its own read budget: statusListStoreWithTimeout
	// derives a 250ms child from it, so canceling the parent happens only after the
	// read has already given up. bd answers every read empty and immediately, and
	// anything ELSE the store decides to ask returns only when that parent ends —
	// so an unrequested command cannot complete inside the caller's budget, by
	// construction rather than by racing a wall-clock constant.
	caller, endCaller := context.WithCancel(context.Background())
	defer endCaller()

	read := "bd list --json --include-infra --include-gates --limit 0"
	var extra atomic.Int64
	var runner beads.CommandRunner = func(_, name string, args ...string) ([]byte, error) {
		if name+" "+strings.Join(args, " ") != read {
			extra.Add(1)
			<-caller.Done()
		}
		return []byte(`[]`), nil
	}

	for _, silenced := range []bool{false, true} {
		name := "notice active"
		if silenced {
			name = "notice silenced by the override"
		}
		t.Run(name, func(t *testing.T) {
			if silenced {
				t.Setenv(beads.AllowUnreadStoreReadEnvVar, "1")
			}
			// The List goroutine may outlive a timeout. Observe only per-arm atomic
			// phase samples; never inspect the sink or wait for that goroutine.
			origin := time.Now()
			var scopeCalls, scopeEnter, scopeReturn, childDeadline atomic.Int64
			var queryCalls, queryEnter, queryReturn, unexpectedCalls atomic.Int64
			var noticeCalls, noticeEnter, noticeReturn atomic.Int64
			notices := &strings.Builder{}
			sink := statusDeadlineNoticeWriter(func(p []byte) (int, error) {
				noticeCalls.Add(1)
				noticeEnter.Store(time.Since(origin).Nanoseconds())
				n, err := notices.Write(p)
				noticeReturn.Store(time.Since(origin).Nanoseconds())
				return n, err
			})
			observedRunner := func(dir, name string, args ...string) ([]byte, error) {
				queryCalls.Add(1)
				queryEnter.Store(time.Since(origin).Nanoseconds())
				if name+" "+strings.Join(args, " ") != read {
					unexpectedCalls.Add(1)
				}
				out, err := runner(dir, name, args...)
				queryReturn.Store(time.Since(origin).Nanoseconds())
				return out, err
			}
			store := beads.NewBdStore(unreadScope(t), observedRunner, beads.WithBdStoreNoticeSink(sink))
			state := newFakeState(t)
			state.scopedStoreFn = func(ctx context.Context, _ beads.Store) (beads.Store, error) {
				scopeCalls.Add(1)
				scopeEnter.Store(time.Since(origin).Nanoseconds())
				if deadline, ok := ctx.Deadline(); ok {
					childDeadline.Store(deadline.Sub(origin).Nanoseconds())
				}
				scopeReturn.Store(time.Since(origin).Nanoseconds())
				return store, nil
			}

			start := time.Now()
			rows, err := statusListStoreWithTimeout(caller, state, store, beads.ListQuery{AllowScan: true})
			elapsed := time.Since(start)
			defer func() {
				if !t.Failed() {
					return
				}
				// These are independent snapshots, not a completed or coherent trace.
				// Offsets are monotonic since origin; zero means not observed yet.
				t.Logf("[status-unread-deadline] call_start=%s elapsed=%s budget=%s returned_error=%t child_deadline=%s",
					start.Sub(origin), elapsed, statusStoreReadTimeout, err != nil, time.Duration(childDeadline.Load()))
				t.Logf("[status-unread-deadline] scope_calls=%d latest_enter=%s latest_return=%s",
					scopeCalls.Load(), time.Duration(scopeEnter.Load()), time.Duration(scopeReturn.Load()))
				t.Logf("[status-unread-deadline] query_calls=%d unexpected_calls=%d latest_enter=%s latest_return=%s",
					queryCalls.Load(), unexpectedCalls.Load(), time.Duration(queryEnter.Load()), time.Duration(queryReturn.Load()))
				t.Logf("[status-unread-deadline] notice_calls=%d latest_enter=%s latest_return=%s",
					noticeCalls.Load(), time.Duration(noticeEnter.Load()), time.Duration(noticeReturn.Load()))
			}()

			if err != nil {
				t.Fatalf("statusListStoreWithTimeout = (%d rows, %v) after %s; want (0, nil) — bd answered this List successfully and a diagnostic may not spend the caller's deadline",
					len(rows), err, elapsed)
			}
			if len(rows) != 0 {
				t.Fatalf("statusListStoreWithTimeout returned %d rows, want 0", len(rows))
			}
			if elapsed >= statusStoreReadTimeout {
				t.Fatalf("the read took %s against a %s budget; the notice must add no measurable time", elapsed, statusStoreReadTimeout)
			}
		})
	}
	if n := extra.Load(); n != 0 {
		t.Fatalf("the read path ran %d bd command(s) the caller did not ask for; a store read may only invoke bd for its own query", n)
	}
}

// TestStatusRequestsOverOneScopePrintOneNotice pins the "one-time notice"
// wording against the call pattern that broke it.
//
// scopedBdStoreForCity/scopedBdStoreForRig construct a brand new
// beads.NewBdStore on every call, and handler_status reaches them through
// state.ScopedStoreLike on every /status request, so a verdict memoized on the
// store object degrades to once per READ. Measured before the fix, over ten
// requests on one scope: 10 probe subprocesses and 13,000 bytes of notice — a
// log-flood shape at status-rebuild rate, and R+1 extra bd children per request
// once statusWorkCounts fans out over the rigs.
func TestStatusRequestsOverOneScopePrintOneNotice(t *testing.T) {
	scope := unreadScope(t)
	var notices strings.Builder
	var invocations atomic.Int64
	var runner beads.CommandRunner = func(_, _ string, _ ...string) ([]byte, error) {
		invocations.Add(1)
		return []byte(`[]`), nil
	}
	state := newFakeState(t)
	state.scopedStoreFn = func(context.Context, beads.Store) (beads.Store, error) {
		return beads.NewBdStore(scope, runner, beads.WithBdStoreNoticeSink(&notices)), nil
	}
	shared := beads.NewBdStore(scope, runner, beads.WithBdStoreNoticeSink(&notices))

	const requests = 10
	for i := 0; i < requests; i++ {
		rows, err := statusListStoreWithTimeout(context.Background(), state, shared, beads.ListQuery{AllowScan: true})
		if err != nil || len(rows) != 0 {
			t.Fatalf("request #%d = (%d rows, %v), want (0, nil)", i, len(rows), err)
		}
	}
	if n := strings.Count(notices.String(), "does not point at"); n != 1 {
		t.Fatalf("%d requests over one scope printed the notice %d times (%d bytes), want exactly 1", requests, n, notices.Len())
	}
	if n := invocations.Load(); n != requests {
		t.Fatalf("%d requests ran %d bd command(s), want %d — one per request, none for the diagnostic", requests, n, requests)
	}
}
