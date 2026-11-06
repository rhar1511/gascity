package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

func TestHookClaimExpectedWorkPreservesEnrolledLifecycleClaim(t *testing.T) {
	for _, wrongTrigger := range []bool{false, true} {
		t.Run(map[bool]string{false: "exact", true: "wrong trigger"}[wrongTrigger], func(t *testing.T) {
			setup, state, request := newLifecycleDescendantClaimProviderFixture(t)
			candidate := request.Work
			candidate.SourceStoreRef, candidate.LifecycleScope = "rig:pilot", request.Scope
			projection, err := json.Marshal(toReadyBeads([]beads.Bead{candidate}, nil,
				map[string]readyLeg{candidate.ID: {sourceStoreRef: "rig:pilot", cityName: "pilot"}}))
			if err != nil {
				t.Fatal(err)
			}
			opts := lifecycleAdmissionV1ClaimOptions(setup)
			opts.Assignee, opts.SessionID, opts.JSON = request.Actor, request.Session.ID, true
			opts.IdentityCandidates = []string{request.Actor}
			opts.ExpectedWorkID, opts.ExpectedWorkStoreRef = candidate.ID, "rig:pilot"
			if wrongTrigger {
				opts.ExpectedWorkID = "other-trigger"
			}
			var writes lifecycleClaimWriteCounters
			ops := lifecycleProjectionClaimOps(string(projection), setup.store, &writes)
			ops.ClaimLifecycle = func(ctx context.Context, row beads.Bead, _ hookClaimOptions) (beads.Bead, hookLifecycleClaimResult, error) {
				writes.lifecycleClaim++
				request.Work, request.ExpectedRevision = row, row.Revision
				result, err := state.ClaimLifecycleWork(ctx, request)
				if err != nil {
					return beads.Bead{}, hookLifecycleClaimResult{}, err
				}
				current, err := setup.store.Get(row.ID)
				return current, hookLifecycleClaimResult{Generation: result.ClaimGeneration, ReceiptID: result.ReceiptID}, err
			}
			ops.PublishRunMap = func(string, string, ...string) error { return nil }
			ops.EmitExecutionStepStarted = func(beads.Bead, string, []string, string) {}
			ops.DrainAck = func(io.Writer) error { t.Fatal("unexpected drain acknowledgment"); return nil }
			var stdout, stderr bytes.Buffer
			code := doHookClaim("gc ready --json", setup.fixture.cityPath, opts, ops, &stdout, &stderr)
			if wrongTrigger {
				writes.assertNoMutation(t)
				if code != 1 || strings.Contains(stdout.String(), `"action":"work"`) {
					t.Fatalf("code=%d stdout=%s stderr=%s", code, &stdout, &stderr)
				}
				return
			}
			if code != 0 || writes.lifecycleClaim != 1 || writes.claim != 0 || writes.workIdentity != 0 || writes.sessionClaim != 0 || writes.continuationList != 0 || writes.continuationAssign != 0 || writes.release != 0 || !strings.Contains(stdout.String(), `"bead_id":"`+candidate.ID+`"`) {
				t.Fatalf("code=%d writes=%+v stdout=%s stderr=%s", code, writes, &stdout, &stderr)
			}
		})
	}
}

func TestHookClaimExpectedWorkRefusesUnrelatedLegacyAdoption(t *testing.T) {
	var stdout, stderr bytes.Buffer
	writes := 0
	ops := hookClaimOps{
		Runner: func(string, string) (string, error) {
			return `[{"id":"old-work","status":"in_progress","assignee":"worker","source_store_ref":"rig:frontend"}]`, nil
		},
		ReadWorkMeta: func(context.Context, string, []string, string, string) (beads.Bead, error) {
			return beads.Bead{ID: "old-work", Status: "in_progress", Assignee: "worker"}, nil
		},
		StampWorkMeta:     func(context.Context, string, []string, string, string, map[string]string) error { writes++; return nil },
		StampSessionClaim: func(string, string, string) error { writes++; return nil },
		PublishRunMap:     func(string, string, ...string) error { writes++; return nil },
	}
	code := doHookClaim("query", "/rig", hookClaimOptions{
		Assignee: "worker", IdentityCandidates: []string{"worker"}, JSON: true,
		ExpectedWorkID: "new-work", ExpectedWorkStoreRef: "rig:frontend",
	}, ops, &stdout, &stderr)
	if code != 1 || writes != 0 || strings.Contains(stdout.String(), `"action":"work"`) {
		t.Fatalf("code=%d writes=%d stdout=%s stderr=%s; expected refusal before any adoption/stamp", code, writes, &stdout, &stderr)
	}
}

func TestHookClaimExpectedWorkRequiresExactAuthoritativeIdentity(t *testing.T) {
	for _, scenario := range []string{"exact", "exact grouped", "wrong store", "missing source", "missing row", "read failure", "wrong readback", "untrusted query"} {
		t.Run(scenario, func(t *testing.T) {
			store := beads.NewMemStore()
			store.HonorExplicitIDs = true
			row, err := store.Create(beads.Bead{ID: "new-work", Type: "task", Status: "in_progress", Assignee: "worker"})
			if err != nil {
				t.Fatal(err)
			}
			status := "in_progress"
			if err := store.Update(row.ID, beads.UpdateOpts{Status: &status}); err != nil {
				t.Fatal(err)
			}
			row, err = store.Get(row.ID)
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "exact grouped" {
				if err := store.SetMetadataBatch(row.ID, map[string]string{"gc.root_bead_id": "root-1", "gc.continuation_group": "review"}); err != nil {
					t.Fatal(err)
				}
				row, err = store.Get(row.ID)
				if err != nil {
					t.Fatal(err)
				}
			}
			projection := `[{"id":"old-work","status":"in_progress","assignee":"worker","source_store_ref":"rig:frontend"},{"id":"new-work","status":"in_progress","assignee":"worker","source_store_ref":"rig:frontend"}]`
			if scenario == "wrong store" {
				projection = strings.ReplaceAll(projection, "rig:frontend", "rig:other")
			}
			if scenario == "missing source" {
				projection = strings.ReplaceAll(projection, `,"source_store_ref":"rig:frontend"`, "")
			}
			writes := 0
			siblings := 0
			ops := hookClaimOps{
				Runner: func(string, string) (string, error) { return projection, nil },
				ListContinuation: func(context.Context, string, []string, string, string) ([]beads.Bead, error) {
					siblings++
					return []beads.Bead{{ID: "sibling", Status: "open"}}, nil
				},
				ReadWorkMeta: func(context.Context, string, []string, string, string) (beads.Bead, error) {
					if scenario == "read failure" {
						return beads.Bead{}, errors.New("unavailable")
					}
					if scenario == "wrong readback" {
						return beads.Bead{ID: "old-work"}, nil
					}
					return row, nil
				},
				StampWorkMeta:     func(context.Context, string, []string, string, string, map[string]string) error { writes++; return nil },
				StampSessionClaim: func(string, string, string) error { writes++; return nil },
				PublishRunMap:     func(string, string, ...string) error { return nil },
			}
			opts := hookClaimOptions{
				Assignee: "worker", IdentityCandidates: []string{"worker"}, JSON: true,
				ExpectedWorkID: "new-work", ExpectedWorkStoreRef: "rig:frontend", TrustedLifecycleScope: scenario != "untrusted query",
				ResolveLifecycleStore: func(ref string) (beads.Store, error) {
					if ref != "rig:frontend" {
						return nil, errors.New("unexpected store")
					}
					if scenario == "missing row" {
						return beads.NewMemStore(), nil
					}
					return store, nil
				},
			}
			var stdout, stderr bytes.Buffer
			code := doHookClaim("query", "/rig", opts, ops, &stdout, &stderr)
			if scenario == "exact" || scenario == "exact grouped" {
				if code != 0 || siblings != 0 || !strings.Contains(stdout.String(), `"bead_id":"new-work"`) || strings.Contains(stdout.String(), `"bead_id":"old-work"`) {
					t.Fatalf("code=%d stdout=%s stderr=%s", code, &stdout, &stderr)
				}
			} else if code != 1 || writes != 0 || strings.Contains(stdout.String(), `"action":"work"`) {
				t.Fatalf("code=%d writes=%d stdout=%s stderr=%s", code, writes, &stdout, &stderr)
			}
		})
	}
}

func TestHookExpectedWorkFlagsRefuseInvalidCombinationsBeforeContext(t *testing.T) {
	for _, args := range [][]string{
		{"--claim", "--expect-work-id", "new-work"},
		{"--expect-work-id", "new-work", "--expect-work-store-ref", "rig:frontend"},
		{"--claim", "--drain-ack", "--expect-work-id", "new-work", "--expect-work-store-ref", "rig:frontend"},
		{"--claim", "--inject", "--expect-work-id", "new-work", "--expect-work-store-ref", "rig:frontend"},
		{"--claim", "--expect-work-id", "new-work", "--expect-work-store-ref", "class:graph"},
	} {
		var stdout, stderr bytes.Buffer
		cmd := newHookCmd(&stdout, &stderr)
		cmd.SetArgs(args)
		if cmd.Execute() == nil || stdout.Len() != 0 {
			t.Fatalf("args=%v stdout=%s stderr=%s", args, &stdout, &stderr)
		}
	}
}

func TestHookClaimExpectedWorkWritesOnlyTheExpectedStoreLeg(t *testing.T) {
	store := beads.NewMemStore()
	store.HonorExplicitIDs = true
	row, err := store.Create(beads.Bead{ID: "duplicate", Type: "task", Assignee: "worker"})
	if err != nil {
		t.Fatal(err)
	}
	status := "in_progress"
	if err := store.Update(row.ID, beads.UpdateOpts{Status: &status}); err != nil {
		t.Fatal(err)
	}
	row, err = store.Get(row.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, expectedLeg := range []string{"rig:pilot", "", "rig:other"} {
		t.Run("leg="+expectedLeg, func(t *testing.T) {
			wrongWrites, rightWrites := 0, 0
			ops := hookClaimOps{
				ReadWorkMeta: func(context.Context, string, []string, string, string) (beads.Bead, error) { return row, nil },
				StampWorkMeta: func(_ context.Context, dir string, _ []string, _ string, _ string, _ map[string]string) error {
					if dir == "/wrong" {
						wrongWrites++
					} else {
						rightWrites++
					}
					return nil
				},
				StampSessionClaim: func(string, string, string) error { return nil },
				PublishRunMap:     func(string, string, ...string) error { return nil },
			}
			opts := hookClaimOptions{
				Assignee: "worker", IdentityCandidates: []string{"worker"}, JSON: true,
				ExpectedWorkID: "duplicate", ExpectedWorkStoreRef: "rig:pilot", TrustedLifecycleScope: true,
				ResolveLifecycleStore: func(string) (beads.Store, error) { return store, nil },
			}
			query := func(string, string, []string) (string, error) {
				return `[{"id":"duplicate","status":"in_progress","assignee":"worker","source_store_ref":"rig:pilot"}]`, nil
			}
			var stdout, stderr bytes.Buffer
			code := claimHookWorkWithRunner("query", "/wrong", nil, []hookStore{
				{dir: "/wrong", sourceStoreRef: "city:pilot"}, {dir: "/right", sourceStoreRef: expectedLeg},
			}, opts, ops, query, func(string, error) { t.Fatal("unexpected query failure") }, &stdout, &stderr)
			wantCode, wantRight := 1, 0
			if expectedLeg == "rig:pilot" {
				wantCode, wantRight = 0, 1
			}
			if code != wantCode || wrongWrites != 0 || rightWrites != wantRight {
				t.Fatalf("code=%d writes wrong=%d right=%d stdout=%s stderr=%s", code, wrongWrites, rightWrites, &stdout, &stderr)
			}
		})
	}
}

func TestHookClaimExpectedWorkNeverEscalatesToClassCopy(t *testing.T) {
	for _, operation := range []string{"claim", "read", "stamp"} {
		t.Run(operation, func(t *testing.T) {
			class := newClaimRouteClassStore(t)
			mintClaimRouteBead(t, class, "duplicate", nil)
			route := newClaimRouteFor(t, class)
			ops := hookClaimOpsForExpectation(hookClaimOps{
				Claim: notFoundClaim(t, "duplicate"),
				ReadWorkMeta: func(context.Context, string, []string, string, string) (beads.Bead, error) {
					return beads.Bead{}, beads.ErrNotFound
				},
				StampWorkMeta: func(context.Context, string, []string, string, string, map[string]string) error {
					return beads.ErrNotFound
				},
			}, route, hookClaimOptions{ExpectedWorkID: "duplicate", ExpectedWorkStoreRef: "rig:pilot"})
			var err error
			switch operation {
			case "claim":
				_, _, err = ops.Claim(context.Background(), "/rig", nil, "duplicate", "worker")
			case "read":
				_, err = ops.ReadWorkMeta(context.Background(), "/rig", nil, "duplicate", "worker")
			case "stamp":
				err = ops.StampWorkMeta(context.Background(), "/rig", nil, "duplicate", "worker", map[string]string{"gc.session_name": "wrong"})
			}
			current, readErr := class.Get("duplicate")
			if !errors.Is(err, beads.ErrNotFound) || readErr != nil || current.Assignee != "" || current.Metadata["gc.session_name"] != "" {
				t.Fatalf("operation=%s err=%v class=%+v readErr=%v", operation, err, current, readErr)
			}
		})
	}
}
