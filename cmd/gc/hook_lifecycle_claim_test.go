package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"maps"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/worklifecycle"
)

func TestEnrolledHookClaimUsesOnlyLifecycleTransitionPath(t *testing.T) {
	candidate := beads.Bead{
		ID: "work-1", Type: "task", Status: "open",
		Metadata: map[string]string{
			beadmeta.LifecycleAdmissionReceiptV2MetadataKey: "v2-receipt",
			beadmeta.LifecycleTransitionHeadMetadataKey:     "head-1",
			beadmeta.RoutedToMetadataKey:                    "worker",
			beadmeta.RootBeadIDMetadataKey:                  "root-1",
			beadmeta.ContinuationGroupMetadataKey:           "review",
		},
	}
	opts := hookClaimOptions{
		Assignee: "worker", SessionID: "session-1", IdentityCandidates: []string{"worker"},
		RouteTargets: []string{"worker"}, Env: []string{"GC_SESSION_ID=session-1"}, JSON: true,
		Lifecycle: config.LifecycleConfig{AdmissionEnabled: true},
	}
	var lifecycleCalls, genericClaims, workIdentityWrites, sessionClaimWrites int
	var continuationQueries, continuationAssignments, releases int
	ops := hookClaimOps{
		ClaimLifecycle: func(_ context.Context, bead beads.Bead, _ hookClaimOptions) (beads.Bead, hookLifecycleClaimResult, error) {
			lifecycleCalls++
			bead.Status = "in_progress"
			bead.Assignee = "worker"
			bead.Metadata[beadmeta.SessionIDMetadataKey] = "session-1"
			bead.Metadata[beadmeta.ClaimGenerationMetadataKey] = "1"
			bead.Metadata[beadmeta.LifecycleTransitionHeadMetadataKey] = "claim-head-1"
			return bead, hookLifecycleClaimResult{Generation: "1", ReceiptID: "claim-receipt-1"}, nil
		},
		Claim: func(context.Context, string, []string, string, string) (beads.Bead, bool, error) {
			genericClaims++
			return beads.Bead{}, false, nil
		},
		StampWorkMeta: func(context.Context, string, []string, string, string, map[string]string) error {
			workIdentityWrites++
			return nil
		},
		StampSessionClaim: func(string, string, string) error {
			sessionClaimWrites++
			return nil
		},
		ListContinuation: func(context.Context, string, []string, string, string) ([]beads.Bead, error) {
			continuationQueries++
			return nil, nil
		},
		AssignContinuation: func(context.Context, string, []string, string, string) error {
			continuationAssignments++
			return nil
		},
		Release: func(context.Context, string, []string, string, string) (bool, error) {
			releases++
			return true, nil
		},
		PublishRunMap:            func(string, string, ...string) error { return nil },
		EmitExecutionStepStarted: func(beads.Bead, string, []string, string) {},
	}
	var stdout, stderr bytes.Buffer
	result := claimFirstEligibleHookCandidate([]beads.Bead{candidate}, opts, ops, "/work", &stdout, &stderr)
	if !result.terminal || result.code != 0 {
		t.Fatalf("claim result = %+v; stderr %q", result, stderr.String())
	}
	if lifecycleCalls != 1 || genericClaims != 0 || workIdentityWrites != 0 || sessionClaimWrites != 0 ||
		continuationQueries != 0 || continuationAssignments != 0 || releases != 0 {
		t.Fatalf("claim calls lifecycle=%d generic=%d work identity writes=%d session claim writes=%d continuation queries=%d assignments=%d releases=%d",
			lifecycleCalls, genericClaims, workIdentityWrites, sessionClaimWrites, continuationQueries, continuationAssignments, releases)
	}
	if stdout.Len() == 0 {
		t.Fatal("lifecycle claim result was not written")
	}
}

func TestHookClaimUsesCanonicalEnrollmentWhenProjectionOmitsMarkers(t *testing.T) {
	for _, tc := range []struct {
		name              string
		status            string
		assignee          string
		projectedStatus   string
		projectedAssignee string
	}{
		{name: "open", status: "open", projectedStatus: "open"},
		{name: "already assigned", status: "in_progress", assignee: "session-1", projectedStatus: "open"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			baseStore := beads.NewMemStore()
			baseStore.HonorExplicitIDs = true
			canonical, err := baseStore.Create(beads.Bead{
				ID: "work-1", Type: "task", Status: tc.status, Assignee: tc.assignee,
				Metadata: map[string]string{
					beadmeta.RoutedToMetadataKey:          "worker",
					beadmeta.RootBeadIDMetadataKey:        "root-1",
					beadmeta.ContinuationGroupMetadataKey: "review",
				},
			})
			if err != nil {
				t.Fatalf("create source row: %v", err)
			}
			store := &lifecycleProjectionEnrolledReadStore{Store: baseStore}
			// The ready projection keeps routing and exact source provenance but
			// loses admission/Q54 markers and has an older revision. The assigned
			// row also projects as open and unassigned. The canonical read must
			// restore its durable enrollment and owner before selecting any claim path.
			projection, err := json.Marshal([]struct {
				ID             string            `json:"id"`
				Type           string            `json:"type"`
				Revision       int64             `json:"revision"`
				Status         string            `json:"status"`
				Assignee       string            `json:"assignee,omitempty"`
				Metadata       map[string]string `json:"metadata"`
				SourceStoreRef string            `json:"source_store_ref"`
				LifecycleScope string            `json:"lifecycle_scope"`
			}{{
				ID: canonical.ID, Type: canonical.Type, Revision: canonical.Revision - 1,
				Status: tc.projectedStatus, Assignee: tc.projectedAssignee,
				Metadata: map[string]string{
					beadmeta.RoutedToMetadataKey:          "worker",
					beadmeta.RootBeadIDMetadataKey:        "root-1",
					beadmeta.ContinuationGroupMetadataKey: "review",
				},
				SourceStoreRef: "city:pilot",
				LifecycleScope: worklifecycle.ScopeForStore("pilot", "city:pilot"),
			}})
			if err != nil {
				t.Fatalf("encode partial work projection: %v", err)
			}

			var writes lifecycleClaimWriteCounters
			ops := lifecycleProjectionClaimOps(string(projection), store, &writes)
			opts := hookClaimOptions{
				Assignee: "session-1", SessionID: "session-1", IdentityCandidates: []string{"session-1"},
				RouteTargets: []string{"worker"}, Env: []string{"GC_SESSION_ID=session-1"}, JSON: true,
				Lifecycle:                     config.LifecycleConfig{AdmissionEnabled: true},
				LifecycleCity:                 &config.City{Lifecycle: config.LifecycleConfig{AdmissionEnabled: true}},
				TrustedLifecycleScope:         true,
				RequireAuthoritativeClaimRead: true,
				ResolveLifecycleStore: func(ref string) (beads.Store, error) {
					if ref != "city:pilot" {
						return nil, beads.ErrNotFound
					}
					return store, nil
				},
			}
			var stdout, stderr bytes.Buffer
			result := tryHookClaim("gc ready --json", "/city", &opts, &ops, &stdout, &stderr)
			if result.terminal || !bytes.Contains(stderr.Bytes(), []byte("holding lifecycle item work-1")) {
				t.Fatalf("claim result = %+v; stderr=%q stdout=%q", result, stderr.String(), stdout.String())
			}
			writes.assertNoMutation(t)
		})
	}
}

func TestHookFormulaActionProjectionCannotEraseDurableLifecycleMarkers(t *testing.T) {
	baseStore := beads.NewMemStore()
	baseStore.HonorExplicitIDs = true
	canonical, err := baseStore.Create(beads.Bead{ID: "work-1", Type: "task", Status: "open"})
	if err != nil {
		t.Fatalf("create source row: %v", err)
	}
	store := &lifecycleProjectionEnrolledReadStore{Store: baseStore}
	candidate := beads.Bead{
		ID: canonical.ID, Type: canonical.Type, Status: canonical.Status,
		SourceStoreRef: "city:pilot", LifecycleScope: worklifecycle.ScopeForStore("pilot", "city:pilot"),
	}
	opts := hookClaimOptions{
		TrustedLifecycleScope:         true,
		RequireAuthoritativeClaimRead: true,
		ResolveLifecycleStore: func(ref string) (beads.Store, error) {
			if ref != "city:pilot" {
				return nil, beads.ErrNotFound
			}
			return store, nil
		},
		CheckFormulaAction: func(_ context.Context, candidate beads.Bead) (formulaActionCandidate, error) {
			return formulaActionCandidate{Bead: beads.Bead{ID: candidate.ID, Type: candidate.Type, Status: candidate.Status}}, nil
		},
	}
	state, err := checkHookFormulaAction(context.Background(), opts, candidate)
	if err != nil {
		t.Fatalf("formula compatibility projection: %v", err)
	}
	if !lifecycleEnrollmentEvidence(state.Bead) {
		t.Fatalf("canonical formula candidate lost lifecycle enrollment markers: %+v", state.Bead.Metadata)
	}
	if state.Bead.SourceStoreRef != candidate.SourceStoreRef || state.Bead.LifecycleScope != candidate.LifecycleScope {
		t.Fatalf("canonical formula candidate provenance = (%q, %q), want (%q, %q)",
			state.Bead.SourceStoreRef, state.Bead.LifecycleScope, candidate.SourceStoreRef, candidate.LifecycleScope)
	}
}

func TestHookClaimFailsClosedWhenClaimProjectionProvenanceIsUnavailable(t *testing.T) {
	baseStore := beads.NewMemStore()
	baseStore.HonorExplicitIDs = true
	if _, err := baseStore.Create(beads.Bead{
		ID: "work-1", Type: "task", Status: "open",
		Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "worker"},
	}); err != nil {
		t.Fatalf("create source row: %v", err)
	}
	store := &lifecycleProjectionEnrolledReadStore{Store: baseStore}
	for _, tc := range []struct {
		name              string
		candidateID       string
		sourceStoreRef    string
		trusted           bool
		resolutionFails   bool
		wantResolverCalls int
		wantError         string
	}{
		{name: "custom query", candidateID: "work-1", sourceStoreRef: "city:pilot", wantError: "trusted source-store provenance"},
		{name: "missing source ref", candidateID: "work-1", trusted: true, wantError: "exact source-store reference"},
		{name: "store resolution fails", candidateID: "work-1", sourceStoreRef: "city:pilot", trusted: true, resolutionFails: true, wantResolverCalls: 1, wantError: "resolve source store"},
		{name: "source read fails", candidateID: "missing-1", sourceStoreRef: "city:pilot", trusted: true, wantResolverCalls: 1, wantError: "read canonical candidate"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			projection, err := json.Marshal([]map[string]any{{
				"id": tc.candidateID, "type": "task", "status": "open",
				"metadata":         map[string]string{beadmeta.RoutedToMetadataKey: "worker"},
				"source_store_ref": tc.sourceStoreRef,
			}})
			if err != nil {
				t.Fatalf("encode work projection: %v", err)
			}

			var writes lifecycleClaimWriteCounters
			var resolverCalls int
			ops := lifecycleProjectionClaimOps(string(projection), store, &writes)
			opts := hookClaimOptions{
				Assignee: "session-1", SessionID: "session-1", IdentityCandidates: []string{"session-1"},
				RouteTargets: []string{"worker"}, Env: []string{"GC_SESSION_ID=session-1"}, JSON: true,
				Lifecycle:                     config.LifecycleConfig{AdmissionEnabled: true},
				RequireAuthoritativeClaimRead: true,
				TrustedLifecycleScope:         tc.trusted,
				ResolveLifecycleStore: func(string) (beads.Store, error) {
					resolverCalls++
					if tc.resolutionFails {
						return nil, beads.ErrNotFound
					}
					return store, nil
				},
			}
			var stdout, stderr bytes.Buffer
			result := tryHookClaim("candidate query", "/city", &opts, &ops, &stdout, &stderr)
			if !result.terminal || result.code == 0 || !bytes.Contains(stderr.Bytes(), []byte(tc.wantError)) {
				t.Fatalf("projection result = %+v; stderr=%q stdout=%q", result, stderr.String(), stdout.String())
			}
			if resolverCalls != tc.wantResolverCalls {
				t.Fatalf("resolver calls = %d, want %d", resolverCalls, tc.wantResolverCalls)
			}
			writes.assertNoMutation(t)
		})
	}
}

type lifecycleClaimWriteCounters struct {
	claim                 int
	lifecycleClaim        int
	workIdentity          int
	sessionClaim          int
	continuationList      int
	continuationAssign    int
	release               int
	canonicalAdoptionRead int
}

func lifecycleProjectionClaimOps(output string, store beads.Store, writes *lifecycleClaimWriteCounters) hookClaimOps {
	return hookClaimOps{
		Runner: func(string, string) (string, error) { return output, nil },
		Claim: func(_ context.Context, _ string, _ []string, id, actor string) (beads.Bead, bool, error) {
			writes.claim++
			return beads.Bead{ID: id, Type: "task", Status: "in_progress", Assignee: actor}, true, nil
		},
		ClaimLifecycle: func(context.Context, beads.Bead, hookClaimOptions) (beads.Bead, hookLifecycleClaimResult, error) {
			writes.lifecycleClaim++
			return beads.Bead{}, hookLifecycleClaimResult{}, nil
		},
		ReadWorkMeta: func(_ context.Context, _ string, _ []string, id, _ string) (beads.Bead, error) {
			writes.canonicalAdoptionRead++
			return store.Get(id)
		},
		StampWorkMeta: func(context.Context, string, []string, string, string, map[string]string) error {
			writes.workIdentity++
			return nil
		},
		StampSessionClaim: func(string, string, string) error {
			writes.sessionClaim++
			return nil
		},
		ListContinuation: func(context.Context, string, []string, string, string) ([]beads.Bead, error) {
			writes.continuationList++
			return []beads.Bead{{ID: "continuation-1", Type: "task", Status: "open"}}, nil
		},
		AssignContinuation: func(context.Context, string, []string, string, string) error {
			writes.continuationAssign++
			return nil
		},
		Release: func(context.Context, string, []string, string, string) (bool, error) {
			writes.release++
			return true, nil
		},
		DrainPending: func(string) (bool, error) { return false, nil },
		DrainAck:     func(io.Writer) error { return nil },
	}
}

func (writes lifecycleClaimWriteCounters) assertNoMutation(t *testing.T) {
	t.Helper()
	if writes.claim != 0 || writes.lifecycleClaim != 0 || writes.workIdentity != 0 || writes.sessionClaim != 0 ||
		writes.continuationList != 0 || writes.continuationAssign != 0 || writes.release != 0 || writes.canonicalAdoptionRead != 0 {
		t.Fatalf("candidate classification reached claim/stamp/continuation/release operations: %+v", writes)
	}
}

type lifecycleProjectionEnrolledReadStore struct {
	beads.Store
}

func (s *lifecycleProjectionEnrolledReadStore) Get(id string) (beads.Bead, error) {
	row, err := s.Store.Get(id)
	if err != nil {
		return beads.Bead{}, err
	}
	row.Metadata = maps.Clone(row.Metadata)
	if row.Metadata == nil {
		row.Metadata = make(map[string]string)
	}
	row.Metadata[beadmeta.LifecycleAdmissionReceiptV2MetadataKey] = "unverified-v2-receipt"
	row.Metadata[beadmeta.LifecycleTransitionHeadMetadataKey] = "claim-head-1"
	return row, nil
}
