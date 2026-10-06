package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/decisionfrontier"
	"github.com/gastownhall/gascity/internal/worklifecycle"
)

// Completion requires acceptance of this exact contract and scope. Neither a
// workflow outcome nor another city's otherwise valid receipt can close it.
func TestLifecycleCompletionRequiresScopedAcceptance(t *testing.T) {
	for _, tc := range []struct {
		name       string
		edit       func(*beads.Bead, *config.City)
		wantClosed bool
	}{
		{name: "accepted envelope without attachment proof", wantClosed: false},
		{name: "unsigned workflow success", edit: func(b *beads.Bead, _ *config.City) {
			delete(b.Metadata, beadmeta.LifecycleCompletionReceiptMetadataKey)
			b.Metadata[beadmeta.OutcomeMetadataKey] = beadmeta.OutcomePass
		}},
		{name: "malformed acceptance", edit: func(b *beads.Bead, _ *config.City) {
			b.Metadata[beadmeta.LifecycleCompletionReceiptMetadataKey] = `{"accepted":true}`
		}},
		{name: "another city", edit: func(_ *beads.Bead, cfg *config.City) { cfg.Workspace.Name = "elsewhere" }},
		{name: "disabled", edit: func(_ *beads.Bead, cfg *config.City) { cfg.Lifecycle.AdmissionEnabled = false }},
		{name: "recovery disabled", edit: func(_ *beads.Bead, cfg *config.City) { cfg.Lifecycle.RecoveryEnabled = false }},
		{name: "human hold", edit: func(b *beads.Bead, _ *config.City) { b.Labels = append(b.Labels, "Hold:Mayor") }},
		{name: "external hold", edit: func(b *beads.Bead, _ *config.City) { b.Labels = append(b.Labels, "hold:external") }},
		{name: "blocked status", edit: func(b *beads.Bead, _ *config.City) { b.Status = "blocked" }},
		{name: "deferred", edit: func(b *beads.Bead, _ *config.City) { future := time.Now().Add(time.Hour); b.DeferUntil = &future }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, row := lifecycleCompletionFixture(t)
			if tc.edit != nil {
				tc.edit(&row, cfg)
			}
			store := newLifecycleCompletionStore(t, row)
			var stderr bytes.Buffer
			reconcileLifecycleCompletions(cfg.Workspace.Name, "/fixture/city", cfg, store, nil, nil, &stderr)
			after, err := store.Get(row.ID)
			if err != nil {
				t.Fatal(err)
			}
			if (after.Status == "closed") != tc.wantClosed {
				t.Fatalf("status=%q wantClosed=%v; log=%s", after.Status, tc.wantClosed, &stderr)
			}
			if after.Assignee != row.Assignee || after.Metadata["worktree"] != row.Metadata["worktree"] {
				t.Fatal("completion changed ownership or worktree evidence")
			}
			// Repeated periodic reconciliation does not count or mutate accepted work again.
			reconcileLifecycleCompletions(cfg.Workspace.Name, "/fixture/city", cfg, store, nil, nil, io.Discard)
			repeated, err := store.Get(row.ID)
			if err != nil {
				t.Fatal(err)
			}
			if repeated.Revision != after.Revision {
				t.Fatal("repeated completion mutated the work item")
			}
		})
	}
}

func TestLifecycleCompletionReceiptCannotBeReplayedAfterGenericReopen(t *testing.T) {
	cfg, row := lifecycleCompletionFixture(t)
	store := newLifecycleCompletionStore(t, row)
	reconcileLifecycleCompletions("pilot", "/fixture/city", cfg, store, nil, nil, io.Discard)
	closed, err := store.Get(row.ID)
	if err != nil {
		t.Fatal(err)
	}
	if closed.Status != "in_progress" {
		t.Fatalf("unproved v2 completion status=%q, want source to remain open", closed.Status)
	}
	if err := store.Update(row.ID, beads.UpdateOpts{Metadata: map[string]string{beadmeta.LifecycleCompletionReceiptMetadataKey: ""}}); !errors.Is(err, beads.ErrLifecycleMutationBlocked) {
		t.Fatalf("clearing accepted receipt error = %v, want lifecycle mutation refusal", err)
	}
	open := "open"
	if err := store.Update(row.ID, beads.UpdateOpts{Status: &open}); !errors.Is(err, beads.ErrLifecycleMutationBlocked) {
		t.Fatalf("generic status reopen error = %v, want lifecycle mutation refusal", err)
	}
	if err := store.Reopen(row.ID); !errors.Is(err, beads.ErrLifecycleMutationBlocked) {
		t.Fatalf("Reopen error = %v, want lifecycle mutation refusal", err)
	}
	after, err := store.Get(row.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != "in_progress" || after.Metadata[beadmeta.LifecycleCompletionReceiptMetadataKey] == "" {
		t.Fatalf("refused mutations changed completion evidence: status=%q metadata=%v", after.Status, after.Metadata)
	}
	reconcileLifecycleCompletions("pilot", "/fixture/city", cfg, store, nil, nil, io.Discard)
	repeated, err := store.Get(row.ID)
	if err != nil {
		t.Fatal(err)
	}
	if repeated.Revision != after.Revision {
		t.Fatalf("verified completion replay mutated evidence: revision %d -> %d", after.Revision, repeated.Revision)
	}
}

func TestLifecycleCompletionRefusesUnsupportedOrStaleClose(t *testing.T) {
	for _, stale := range []bool{false, true} {
		cfg, row := lifecycleCompletionFixture(t)
		inner := newLifecycleCompletionStore(t, row)
		var store beads.Store = &lifecycleUnconditionalStore{Store: inner}
		if stale {
			store = &lifecycleRacingCloseStore{MemStore: inner}
		}
		reconcileLifecycleCompletions("pilot", "/fixture/city", cfg, store, nil, nil, io.Discard)
		after, err := inner.Get(row.ID)
		if err != nil {
			t.Fatal(err)
		}
		if after.Status == "closed" {
			t.Fatalf("unsupported/stale close succeeded (stale=%v)", stale)
		}
	}
}

func TestDecisionFrontierAnswerDoesNotBypassV2AdmissionProofForCompletion(t *testing.T) {
	for _, tc := range lifecycleCompletionStores(t) {
		t.Run(tc.name, func(t *testing.T) {
			cfg, row := lifecycleCompletionFixture(t)
			store, closeStore := tc.open(t, row)
			defer func() {
				if err := closeStore(); err != nil {
					t.Errorf("close lifecycle completion store: %v", err)
				}
			}()
			released := resolveLifecycleDecisionFrontier(t, store, row.ID)
			if beads.HasDecisionFrontierHold(released) || released.Metadata[beadmeta.DecisionFrontierRevisionReceiptsMetadataKey] == "" {
				t.Fatalf("frontier hold/receipts after answer: hold=%v receipts=%q",
					beads.HasDecisionFrontierHold(released), released.Metadata[beadmeta.DecisionFrontierRevisionReceiptsMetadataKey])
			}
			if err := store.Close(row.ID); !errors.Is(err, beads.ErrLifecycleCompletionRequired) {
				t.Fatalf("ordinary close error = %v, want lifecycle authorization guard", err)
			}
			if err := reconcileLifecycleCompletion(store, row.ID,
				worklifecycle.ScopeForStore("pilot", "city:pilot"), cfg.Lifecycle); err == nil || !strings.Contains(err.Error(), "verified admission contract") {
				t.Fatalf("completion error = %v, want explicit v2 proof hold", err)
			}
			closed, err := store.Get(row.ID)
			if err != nil {
				t.Fatal(err)
			}
			if closed.Status != "in_progress" || beads.HasDecisionFrontierHold(closed) ||
				closed.Metadata[beadmeta.DecisionFrontierRevisionReceiptsMetadataKey] == "" {
				t.Fatalf("unproved source changed completion state or lost receipts: status=%q metadata=%v", closed.Status, closed.Metadata)
			}
		})
	}
}

func TestDecisionFrontierReleasePreservesUnrelatedHoldDuringLifecycleCompletion(t *testing.T) {
	for _, tc := range lifecycleCompletionStores(t) {
		t.Run(tc.name, func(t *testing.T) {
			cfg, row := lifecycleCompletionFixture(t)
			row.Labels = append(row.Labels, beadmeta.HoldExternalLabel)
			store, closeStore := tc.open(t, row)
			defer func() {
				if err := closeStore(); err != nil {
					t.Errorf("close lifecycle completion store: %v", err)
				}
			}()
			released := resolveLifecycleDecisionFrontier(t, store, row.ID)
			if beads.HasDecisionFrontierHold(released) || !containsLifecycleLabel(released.Labels, beadmeta.HoldExternalLabel) {
				t.Fatalf("answer changed frontier/external holds: %v metadata=%v", released.Labels, released.Metadata)
			}
			if err := reconcileLifecycleCompletion(store, row.ID,
				worklifecycle.ScopeForStore("pilot", "city:pilot"), cfg.Lifecycle); err != nil {
				t.Fatalf("external hold should be an observable skip: %v", err)
			}
			after, err := store.Get(row.ID)
			if err != nil {
				t.Fatal(err)
			}
			if after.Status != "in_progress" || !containsLifecycleLabel(after.Labels, beadmeta.HoldExternalLabel) {
				t.Fatalf("unrelated hold did not keep source open: status=%q labels=%v", after.Status, after.Labels)
			}
		})
	}
}

func resolveLifecycleDecisionFrontier(t *testing.T, store beads.Store, workID string) beads.Bead {
	t.Helper()
	current, err := store.Get(workID)
	if err != nil {
		t.Fatal(err)
	}
	revision, err := decisionfrontier.WorkRevision(current)
	if err != nil {
		t.Fatal(err)
	}
	scope := decisionfrontier.Scope{CityRef: "city:pilot", StoreRef: "city:pilot"}
	service := decisionfrontier.Service{Verifier: lifecycleDecisionAnswerVerifier{}}
	frontier, err := service.Ensure(context.Background(), store, scope, workID, revision,
		decisionfrontier.Proposal{Questions: []decisionfrontier.Question{{
			ID: "complete", Title: "Completion choice", Prompt: "Confirm the completion choice.",
		}}})
	if err != nil {
		t.Fatalf("ensure decision frontier: %v", err)
	}
	if len(frontier.OpenQuestions) != 1 {
		t.Fatalf("open questions = %d, want 1", len(frontier.OpenQuestions))
	}
	question := frontier.OpenQuestions[0]
	resolved, err := service.Answer(context.Background(), store, scope, workID, decisionfrontier.AnswerSubmission{
		TicketID: question.TicketID, WorkRevision: frontier.WorkRevision, QuestionVersion: question.Version,
		Resolution: decisionfrontier.ResolutionAnswered, Text: "The accepted work is complete.", Proof: "test-authority-proof",
	})
	if err != nil {
		t.Fatalf("answer decision frontier: %v", err)
	}
	if resolved.State != decisionfrontier.StateResolved {
		t.Fatalf("frontier state = %q, want resolved", resolved.State)
	}
	released, err := store.Get(workID)
	if err != nil {
		t.Fatal(err)
	}
	return released
}

type lifecycleDecisionAnswerVerifier struct{}

func (lifecycleDecisionAnswerVerifier) VerifyDecisionAnswer(_ context.Context, challenge decisionfrontier.AnswerChallenge,
	submission decisionfrontier.AnswerSubmission,
) (decisionfrontier.VerifiedAnswer, error) {
	if submission.Proof != "test-authority-proof" {
		return decisionfrontier.VerifiedAnswer{}, errors.New("untrusted test proof")
	}
	return decisionfrontier.VerifiedAnswer{
		CityRef: challenge.CityRef, StoreRef: challenge.StoreRef, KeyID: "human-key", Issuer: "human-authority",
		Subject: "authorized-human", WorkID: challenge.WorkID, WorkRevision: challenge.WorkRevision,
		WorkDigest: challenge.WorkDigest, MapID: challenge.MapID, TicketID: challenge.TicketID,
		QuestionID: challenge.QuestionID, QuestionVersion: challenge.QuestionVersion,
		AnswerDigest: challenge.AnswerDigest, Resolution: challenge.Resolution,
	}, nil
}

type lifecycleCompletionStore struct {
	name string
	open func(*testing.T, beads.Bead) (beads.Store, func() error)
}

func lifecycleCompletionStores(t *testing.T) []lifecycleCompletionStore {
	t.Helper()
	memFixture := lifecycleCompletionStore{name: "mem", open: func(t *testing.T, row beads.Bead) (beads.Store, func() error) {
		t.Helper()
		mem := &beads.MemStore{IDPrefix: "work", HonorExplicitIDs: true}
		seedLifecycleCompletionStore(t, mem, row)
		return mem, func() error { return nil }
	}}
	sqliteFixture := lifecycleCompletionStore{name: "sqlite", open: func(t *testing.T, row beads.Bead) (beads.Store, func() error) {
		t.Helper()
		opened, err := beads.OpenSQLiteStore(t.TempDir())
		if err != nil {
			t.Fatalf("OpenSQLiteStore: %v", err)
		}
		sqlite := opened.(*beads.SQLiteStore)
		seedLifecycleCompletionStore(t, sqlite, row)
		return sqlite, sqlite.CloseStore
	}}
	return []lifecycleCompletionStore{memFixture, sqliteFixture}
}

func containsLifecycleLabel(labels []string, want string) bool {
	for _, label := range labels {
		if label == want {
			return true
		}
	}
	return false
}

func seedLifecycleCompletionStore(t *testing.T, store beads.Store, row beads.Bead) {
	t.Helper()
	if _, err := store.Create(row); err != nil {
		t.Fatalf("create lifecycle source: %v", err)
	}
	if err := store.Update(row.ID, beads.UpdateOpts{Status: &row.Status}); err != nil {
		t.Fatalf("set lifecycle source status: %v", err)
	}
}

type lifecycleUnconditionalStore struct{ beads.Store }

func TestLifecycleCompletionBudgetsUnknownOutcomesAndEscalatesOnce(t *testing.T) {
	for _, applied := range []bool{false, true} {
		cfg, row := lifecycleCompletionFixture(t)
		store := &lifecycleUnknownCloseStore{MemStore: newLifecycleCompletionStore(t, row), apply: applied}
		for range 3 {
			reconcileLifecycleCompletions("pilot", "/fixture/city", cfg, store, nil, nil, io.Discard)
		}
		after, err := store.Get(row.ID)
		if err != nil {
			t.Fatal(err)
		}
		if store.calls != 0 || after.Status != "in_progress" || after.Metadata[beadmeta.LifecycleRecoveryStateMetadataKey] != "" {
			t.Fatalf("unproved v2 admission reached completion side effects: calls=%d status=%s state=%q", store.calls, after.Status, after.Metadata[beadmeta.LifecycleRecoveryStateMetadataKey])
		}
		reconcileLifecycleCompletions("pilot", "/fixture/city", cfg, store, nil, nil, io.Discard)
		repeated, err := store.Get(row.ID)
		if err != nil {
			t.Fatal(err)
		}
		if repeated.Revision != after.Revision {
			t.Fatal("repeated exhaustion or verified close mutated durable evidence")
		}
	}
}

type lifecycleUnknownCloseStore struct {
	*beads.MemStore
	apply bool
	calls int
}

func (s *lifecycleUnknownCloseStore) CloseIfMatch(id string, revision int64) error {
	s.calls++
	if s.apply {
		if err := s.MemStore.CloseIfMatch(id, revision); err != nil {
			return err
		}
	}
	return errors.New("completion response lost")
}

type lifecycleRacingCloseStore struct{ *beads.MemStore }

func (s *lifecycleRacingCloseStore) CloseIfMatch(id string, revision int64) error {
	if err := s.Update(id, beads.UpdateOpts{Labels: []string{"hold:mayor"}}); err != nil {
		return err
	}
	return s.MemStore.CloseIfMatch(id, revision)
}

func lifecycleCompletionFixture(t *testing.T) (*config.City, beads.Bead) {
	t.Helper()
	admissionKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{1}, ed25519.SeedSize))
	acceptanceKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{2}, ed25519.SeedSize))
	cfg := &config.City{Workspace: config.Workspace{Name: "pilot"}, Lifecycle: config.LifecycleConfig{
		AdmissionEnabled: true, RecoveryEnabled: true, EscalationTarget: "human",
		AdmissionV2PrimaryAuthority: "triage",
		AdmissionV2Authorities:      map[string]string{"triage": base64.StdEncoding.EncodeToString(admissionKey.Public().(ed25519.PublicKey))},
		AcceptanceAuthorities:       map[string]string{"reviewer": base64.StdEncoding.EncodeToString(acceptanceKey.Public().(ed25519.PublicKey))},
		CompletionReceiptMaxAge:     "168h", CompletionClockSkew: "5m",
	}}
	admission := worklifecycle.AdmissionReceiptV2{
		Version: 2, WorkItemID: "work-1", Scope: worklifecycle.ScopeForStore("pilot", "city:pilot"), ExpectedWorkRevision: 1,
		Route: "pilot/worker", Workflow: "reviewed-change", RoutingPolicyDigest: strings.Repeat("a", 64), MergeStrategy: "mr", Deliverable: "reviewed patch",
		Verification: "acceptance tests", AcceptanceAuthority: "reviewer", AdmittedBy: "triage",
	}
	admissionJSON, err := worklifecycle.SignAdmissionReceiptV2(admission, admissionKey)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := worklifecycle.AdmissionDigestV2(admission)
	if err != nil {
		t.Fatal(err)
	}
	completionJSON, err := worklifecycle.SignCompletionReceipt(worklifecycle.CompletionReceipt{
		Version: 1, WorkItemID: admission.WorkItemID, Scope: admission.Scope, AdmissionDigest: digest,
		DeliverableRef: "commit:reviewed", VerificationRef: "report:acceptance", AcceptedBy: "reviewer",
		AcceptedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}, acceptanceKey)
	if err != nil {
		t.Fatal(err)
	}
	return cfg, beads.Bead{
		ID: admission.WorkItemID, Title: "accepted work", Type: "task", Status: "in_progress", Assignee: "original-owner",
		Metadata: map[string]string{
			beadmeta.LifecycleAdmissionReceiptV2MetadataKey: admissionJSON,
			beadmeta.LifecycleCompletionReceiptMetadataKey:  completionJSON,
			"worktree": "/preserved/worktree",
		},
	}
}

func newLifecycleCompletionStore(t *testing.T, row beads.Bead) *beads.MemStore {
	t.Helper()
	store := &beads.MemStore{IDPrefix: "work", HonorExplicitIDs: true}
	if _, err := store.Create(row); err != nil {
		t.Fatal(err)
	}
	if err := store.Update(row.ID, beads.UpdateOpts{Status: &row.Status}); err != nil {
		t.Fatal(err)
	}
	return store
}
