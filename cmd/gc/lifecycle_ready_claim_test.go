package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/worklifecycle"
)

func TestReadyJSONCarriesTrustedLifecycleScopeIntoHookClaim(t *testing.T) {
	_, admissionKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	_, acceptanceKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	lifecycleCfg := config.LifecycleConfig{
		AdmissionEnabled:            true,
		AdmissionV2PrimaryAuthority: "triage",
		AdmissionV2Authorities: map[string]string{
			"triage": base64.StdEncoding.EncodeToString(admissionKey.Public().(ed25519.PublicKey)),
		},
		AcceptanceAuthorities: map[string]string{
			"reviewer": base64.StdEncoding.EncodeToString(acceptanceKey.Public().(ed25519.PublicKey)),
		},
	}
	workflow := "mol-polecat-work"
	cityCfg := &config.City{Lifecycle: lifecycleCfg, Agents: []config.Agent{{Name: "worker", Dir: "pilot", DefaultSlingFormula: &workflow}}}
	makeCandidate := func(scope string, controllerVerified, intent, receiptPresent bool, reviewedRevision int64) beads.Bead {
		receipt := worklifecycle.AdmissionReceiptV2{
			Version:              2,
			WorkItemID:           "work-1",
			Scope:                scope,
			ExpectedWorkRevision: reviewedRevision,
			Route:                "pilot/worker",
			Workflow:             "mol-polecat-work",
			RoutingPolicyDigest:  strings.Repeat("a", 64),
			MergeStrategy:        "mr",
			Deliverable:          "reviewed code change",
			Verification:         "tests and merge request",
			AcceptanceAuthority:  "reviewer",
			AdmittedBy:           "triage",
		}
		encoded, err := worklifecycle.SignAdmissionReceiptV2(receipt, admissionKey)
		if err != nil {
			t.Fatal(err)
		}
		digest, err := worklifecycle.AdmissionDigestV2(receipt)
		if err != nil {
			t.Fatal(err)
		}
		metadata := map[string]string{
			beadmeta.LifecycleAdmissionReceiptV2MetadataKey: encoded,
			beadmeta.RoutedToMetadataKey:                    "pilot/worker",
			beadmeta.ExecutionRoutedToMetadataKey:           "pilot/worker",
			beadmeta.MergeStrategyMetadataKey:               "mr",
			"workflow_id":                                   "wf-1",
		}
		if controllerVerified {
			metadata[beadmeta.LifecycleMaterializationMetadataKey], err = encodeLifecycleMaterialization(lifecycleMaterialization{
				Version: 1, State: "attached", Scope: scope, Contract: digest, Route: "pilot/worker",
				Workflow: "mol-polecat-work", MergeStrategy: "mr", Token: "controller-token", WorkflowID: "wf-1",
				SourceID: "work-1", SourceStoreRef: "city:pilot", WorkflowStoreRef: "city:pilot", AdmissionReceipt: encoded,
			})
			if err != nil {
				t.Fatal(err)
			}
		}
		bead := beads.Bead{
			ID:       "work-1",
			Title:    "implement lifecycle fix",
			Type:     "task",
			Status:   "open",
			Labels:   nil,
			Metadata: metadata,
		}
		if intent {
			bead.Labels = []string{worklifecycle.AdmissionIntentLabel}
		}
		if !receiptPresent {
			delete(bead.Metadata, beadmeta.LifecycleAdmissionReceiptV2MetadataKey)
		}
		return bead
	}

	for _, tc := range []struct {
		name           string
		scope          string
		verified       bool
		intent         bool
		receipt        bool
		wantClaim      bool
		noCAS          bool
		raceHold       bool
		signedRevision bool
		wantError      bool
	}{
		{name: "local receipt with workflow waits for Q43 and policy proof", scope: worklifecycle.ScopeForStore("pilot", "city:pilot"), verified: true, intent: true, receipt: true},
		{name: "negative signed revision still waits for Q43 and policy proof", scope: worklifecycle.ScopeForStore("pilot", "city:pilot"), verified: true, intent: true, receipt: true, signedRevision: true},
		{name: "receipt replayed from another city", scope: worklifecycle.ScopeForStore("elsewhere", "city:elsewhere"), verified: true, intent: true, receipt: true, wantClaim: false},
		{name: "worker workflow metadata without controller verification", scope: worklifecycle.ScopeForStore("pilot", "city:pilot"), intent: true, receipt: true, wantClaim: false},
		{name: "removed intent and receipt after durable enrollment", scope: worklifecycle.ScopeForStore("pilot", "city:pilot"), verified: true, wantClaim: false},
		{name: "unsupported CAS remains unclaimed before proof integration", scope: worklifecycle.ScopeForStore("pilot", "city:pilot"), verified: true, intent: true, receipt: true, noCAS: true},
		{name: "hold race is not reached before proof integration", scope: worklifecycle.ScopeForStore("pilot", "city:pilot"), verified: true, intent: true, receipt: true, raceHold: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mem := &beads.MemStore{IDPrefix: "work", HonorExplicitIDs: true}
			reviewedRevision := int64(1)
			if tc.signedRevision {
				reviewedRevision = -1
			}
			if _, err := mem.Create(makeCandidate(tc.scope, tc.verified, tc.intent, tc.receipt, reviewedRevision)); err != nil {
				t.Fatal(err)
			}
			var store beads.Store = mem
			if tc.noCAS {
				mem.DisableConditionalWrites = true
			}
			if tc.signedRevision {
				store = &lifecycleSignedRevisionStore{MemStore: mem}
			}
			if tc.raceHold {
				store = &lifecycleHoldDuringClaimStore{MemStore: mem}
			}
			leg := readyLeg{
				label:          "city",
				store:          store,
				ref:            "city:pilot",
				sourceStoreRef: "city:pilot",
				cityName:       "pilot",
			}
			items, owners, err := federateReadyBeadsWithOwner([]readyLeg{leg}, beads.ReadyQuery{TierMode: beads.FederatedReadTier})
			if err != nil {
				t.Fatalf("federate ready rows: %v", err)
			}
			readyJSON, err := json.Marshal(toReadyBeads(items, nil, owners))
			if err != nil {
				t.Fatal(err)
			}
			decoded, _, err := decodeHookClaimBeads(string(readyJSON))
			if err != nil {
				t.Fatalf("decode gc ready output: %v", err)
			}
			if len(decoded) != 1 || decoded[0].LifecycleScope != worklifecycle.ScopeForStore("pilot", "city:pilot") || decoded[0].SourceStoreRef != "city:pilot" {
				t.Fatalf("decoded ready row = %+v, want city-scoped provenance", decoded)
			}
			if !tc.verified {
				if decoded[0].Metadata[beadmeta.LifecycleMaterializationMetadataKey] != "" {
					t.Fatalf("unverified ready row unexpectedly has controller materialization: %+v", decoded[0].Metadata)
				}
				decision := worklifecycle.EvaluateAdmission(decoded[0], lifecycleCfg, decoded[0].LifecycleScope)
				if !decision.Requested || decision.Admitted || lifecycleAdmissionRouteMatches(cityCfg, decoded[0], decoded[0].LifecycleScope) {
					t.Fatalf("unverified admission state: requested=%v admitted=%v routeMatches=%v reason=%q", decision.Requested, decision.Admitted, lifecycleAdmissionRouteMatches(cityCfg, decoded[0], decoded[0].LifecycleScope), decision.Reason)
				}
				filtered := filterHookLifecycleCandidates(decoded, hookClaimOptions{Lifecycle: lifecycleCfg, LifecycleCity: cityCfg, TrustedLifecycleScope: true, RouteTargets: []string{"pilot/worker"}}, io.Discard)
				if len(filtered) != 0 {
					t.Fatalf("worker-authored workflow metadata passed lifecycle filter: %+v", filtered)
				}
			}

			legacyClaimAttempts := 0
			ops := hookClaimOps{
				Runner: func(string, string) (string, error) { return string(readyJSON), nil },
				Claim: func(_ context.Context, _ string, _ []string, id, assignee string) (beads.Bead, bool, error) {
					legacyClaimAttempts++
					row, err := store.Get(id)
					if err != nil {
						return beads.Bead{}, false, err
					}
					row.Status = "in_progress"
					row.Assignee = assignee
					return row, true, nil
				},
				DrainAck: func(io.Writer) error { return nil },
			}
			var stdout, stderr bytes.Buffer
			code := doHookClaim("gc ready --json", "/city", hookClaimOptions{
				Assignee:              "worker-session",
				IdentityCandidates:    []string{"worker-session"},
				RouteTargets:          []string{"pilot/worker"},
				Lifecycle:             lifecycleCfg,
				LifecycleCity:         cityCfg,
				TrustedLifecycleScope: true,
				ResolveLifecycleStore: func(ref string) (beads.Store, error) {
					if ref != "city:pilot" {
						return nil, beads.ErrNotFound
					}
					return store, nil
				},
				JSON: true,
			}, ops, &stdout, &stderr)
			current, err := store.Get("work-1")
			if err != nil {
				t.Fatal(err)
			}
			if (strings.EqualFold(current.Status, "in_progress") && current.Assignee == "worker-session") != tc.wantClaim {
				t.Fatalf("canonical status/owner = %q/%q, wantClaim=%v; stdout=%q stderr=%q", current.Status, current.Assignee, tc.wantClaim, stdout.String(), stderr.String())
			}
			if legacyClaimAttempts != 0 {
				t.Fatalf("legacy Claim called %d times for lifecycle work", legacyClaimAttempts)
			}
			if tc.wantError && code == 0 {
				t.Fatalf("hook claim code = 0, want refusal; stdout=%q stderr=%q", stdout.String(), stderr.String())
			}
			if !tc.wantClaim && !strings.Contains(stderr.String(), "holding lifecycle item work-1") {
				if !tc.wantError {
					t.Fatalf("stderr = %q, want a lifecycle hold explanation", stderr.String())
				}
			}
			if tc.raceHold {
				current, err := mem.Get("work-1")
				if err != nil {
					t.Fatal(err)
				}
				if strings.EqualFold(current.Status, "in_progress") || current.Assignee != "" {
					t.Fatalf("unproved claim changed owner/status: status=%q owner=%q labels=%v", current.Status, current.Assignee, current.Labels)
				}
			}
		})
	}
}

func TestLifecycleGraphV2ClassStoreDescendantIsHeldBeforeClaim(t *testing.T) {
	const (
		classRef = "class:graph"
		rootID   = "gcg-root"
		childID  = "gcg-step"
	)
	lineage, err := encodeLifecycleMaterialization(lifecycleMaterialization{
		Version: 1, State: "lineage_pending", Scope: worklifecycle.ScopeForStore("pilot", "city:pilot"),
		Contract: "admission-digest", Route: "pilot/worker", Workflow: "review", MergeStrategy: "mr",
		Token: "controller-token", SourceID: "work-1", SourceStoreRef: "city:pilot", WorkflowStoreRef: classRef,
		AdmissionReceipt: "signed-v2-admission",
	})
	if err != nil {
		t.Fatalf("encode lifecycle lineage: %v", err)
	}
	store := beads.NewMemStore()
	store.HonorExplicitIDs = true
	root, err := store.Create(beads.Bead{
		ID: rootID, Type: "molecule", Status: "open",
		Metadata: map[string]string{
			beadmeta.FormulaContractMetadataKey:          beadmeta.FormulaContractGraphV2,
			beadmeta.FormulaNameMetadataKey:              "review",
			beadmeta.LifecycleMaterializationMetadataKey: lineage,
		},
	})
	if err != nil {
		t.Fatalf("create relocated graph root: %v", err)
	}
	child, err := store.Create(beads.Bead{
		ID: childID, Type: "task", Status: "open", ParentID: root.ID,
		Metadata: map[string]string{
			beadmeta.RootBeadIDMetadataKey: root.ID,
			beadmeta.RoutedToMetadataKey:   "pilot/worker",
		},
	})
	if err != nil {
		t.Fatalf("create relocated graph descendant: %v", err)
	}
	readyJSON, err := json.Marshal(toReadyBeads([]beads.Bead{child}, nil, map[string]readyLeg{
		child.ID: {sourceStoreRef: classRef, cityName: "pilot"},
	}))
	if err != nil {
		t.Fatalf("encode ready projection: %v", err)
	}

	var writes lifecycleClaimWriteCounters
	ops := lifecycleProjectionClaimOps(string(readyJSON), store, &writes)
	opts := hookClaimOptions{
		Assignee: "session-1", SessionID: "session-1", IdentityCandidates: []string{"session-1"},
		RouteTargets: []string{"pilot/worker"}, Env: []string{"GC_SESSION_ID=session-1"}, JSON: true,
		Lifecycle: config.LifecycleConfig{AdmissionEnabled: true}, TrustedLifecycleScope: true,
		RequireAuthoritativeClaimRead: true,
		ResolveLifecycleStore: func(ref string) (beads.Store, error) {
			if ref != classRef {
				return nil, beads.ErrNotFound
			}
			return store, nil
		},
	}
	var stdout, stderr bytes.Buffer
	code := doHookClaim("gc ready --json", "/city", opts, ops, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("hook claim exit = %d, want 1 for an unacknowledged no-work drain; stderr=%q stdout=%q", code, stderr.String(), stdout.String())
	}
	writes.assertNoMutation(t)
	if !strings.Contains(stderr.String(), "holding lifecycle descendant gcg-step") ||
		!strings.Contains(stderr.String(), "graph.v2 lifecycle descendant has no verified own Q43/Q54 transition head") {
		t.Fatalf("stderr = %q, want an explicit graph.v2 descendant hold reason", stderr.String())
	}

	// A raw metadata string is not proof of a descendant Q54 transition and
	// must not open the generic claim path either.
	child.Metadata[beadmeta.LifecycleTransitionHeadMetadataKey] = "unverified-q54-head"
	readyJSON, err = json.Marshal(toReadyBeads([]beads.Bead{child}, nil, map[string]readyLeg{
		child.ID: {sourceStoreRef: classRef, cityName: "pilot"},
	}))
	if err != nil {
		t.Fatalf("encode ready projection with unverified head: %v", err)
	}
	ops = lifecycleProjectionClaimOps(string(readyJSON), store, &writes)
	stdout.Reset()
	stderr.Reset()
	code = doHookClaim("gc ready --json", "/city", opts, ops, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("hook claim with unverified head exit = %d, want 1 for an unacknowledged no-work drain; stderr=%q stdout=%q", code, stderr.String(), stdout.String())
	}
	writes.assertNoMutation(t)
	if !strings.Contains(stderr.String(), "graph.v2 lifecycle descendant has no verified own Q43/Q54 transition head") {
		t.Fatalf("stderr = %q, want the graph.v2 descendant hold despite an unverified head", stderr.String())
	}
}

func TestLifecycleGraphV2ClassDescendantFailsClosedWithoutRootEvidence(t *testing.T) {
	const (
		classRef = "class:graph"
		rootID   = "gcg-root"
		childID  = "gcg-step"
	)
	store := beads.NewMemStore()
	store.HonorExplicitIDs = true
	if _, err := store.Create(beads.Bead{
		ID: rootID, Type: "molecule", Status: "open",
		Metadata: map[string]string{
			beadmeta.FormulaContractMetadataKey:          beadmeta.FormulaContractGraphV2,
			beadmeta.LifecycleMaterializationMetadataKey: "controller-lineage",
		},
	}); err != nil {
		t.Fatalf("create graph-only lifecycle root: %v", err)
	}
	child, err := store.Create(beads.Bead{
		ID: childID, Type: "task", Status: "open", ParentID: rootID,
		Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: rootID},
	})
	if err != nil {
		t.Fatalf("create graph descendant: %v", err)
	}
	child.SourceStoreRef = classRef

	rootReadErrorStore := lifecycleRootReadErrorStore{Store: store, rootID: rootID}
	for _, tc := range []struct {
		name    string
		resolve func() func(string) (beads.Store, error)
	}{
		{
			name:    "missing resolver",
			resolve: func() func(string) (beads.Store, error) { return nil },
		},
		{
			name: "unavailable store",
			resolve: func() func(string) (beads.Store, error) {
				return func(string) (beads.Store, error) { return nil, errors.New("store unavailable") }
			},
		},
		{
			name: "root read failure",
			resolve: func() func(string) (beads.Store, error) {
				return func(string) (beads.Store, error) { return rootReadErrorStore, nil }
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resolve := tc.resolve()
			opts := hookClaimOptions{
				Lifecycle:             config.LifecycleConfig{AdmissionEnabled: true},
				TrustedLifecycleScope: true,
				ResolveLifecycleStore: resolve,
			}
			var filterErr strings.Builder
			if got := filterHookLifecycleCandidates([]beads.Bead{child}, opts, &filterErr); len(got) != 0 {
				t.Fatalf("descendant passed lifecycle hold with unavailable root evidence: %+v", got)
			}
			if !strings.Contains(filterErr.String(), "workflow root evidence is unavailable") {
				t.Fatalf("hold reason = %q, want explicit unavailable-root evidence", filterErr.String())
			}
		})
	}
}

type lifecycleRootReadErrorStore struct {
	beads.Store
	rootID string
}

func (s lifecycleRootReadErrorStore) Get(id string) (beads.Bead, error) {
	if id == s.rootID {
		return beads.Bead{}, errors.New("root read failed")
	}
	return s.Store.Get(id)
}

type lifecycleHoldDuringClaimStore struct {
	*beads.MemStore
}

type lifecycleSignedRevisionStore struct {
	*beads.MemStore
}

func (s *lifecycleSignedRevisionStore) Get(id string) (beads.Bead, error) {
	bead, err := s.MemStore.Get(id)
	if err == nil {
		bead.Revision = -bead.Revision
	}
	return bead, err
}

func (s *lifecycleSignedRevisionStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	rows, err := s.MemStore.List(query)
	if err == nil {
		for index := range rows {
			rows[index].Revision = -rows[index].Revision
		}
	}
	return rows, err
}

func (s *lifecycleSignedRevisionStore) UpdateIfMatch(id string, revision int64, opts beads.UpdateOpts) error {
	return s.MemStore.UpdateIfMatch(id, -revision, opts)
}

func (s *lifecycleHoldDuringClaimStore) UpdateIfMatch(id string, revision int64, opts beads.UpdateOpts) error {
	if err := s.Update(id, beads.UpdateOpts{Labels: []string{beadmeta.DispatchHoldLabels[0]}}); err != nil {
		return err
	}
	return s.MemStore.UpdateIfMatch(id, revision, opts)
}
