package main

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
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
		{name: "accepted", wantClosed: true},
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
		var state worklifecycle.RecoveryState
		if err := json.Unmarshal([]byte(after.Metadata[beadmeta.LifecycleRecoveryStateMetadataKey]), &state); err != nil {
			t.Fatal(err)
		}
		if applied {
			if store.calls != 1 || len(state.Attempts) != 1 || state.Escalation != nil || after.Status != "closed" {
				t.Fatalf("committed unknown result was repeated: calls=%d state=%+v status=%s", store.calls, state, after.Status)
			}
		} else if store.calls != 2 || len(state.Attempts) != 2 || state.Escalation == nil || state.Escalation.Target != "human" {
			t.Fatalf("unknown failures exceeded budget or did not request escalation: calls=%d state=%+v", store.calls, state)
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
		AdmissionAuthorities:  map[string]string{"triage": base64.StdEncoding.EncodeToString(admissionKey.Public().(ed25519.PublicKey))},
		AcceptanceAuthorities: map[string]string{"reviewer": base64.StdEncoding.EncodeToString(acceptanceKey.Public().(ed25519.PublicKey))},
	}}
	admission := worklifecycle.AdmissionReceipt{
		Version: 1, WorkItemID: "work-1", Scope: worklifecycle.ScopeForStore("pilot", "city:pilot"),
		Route: "worker", Workflow: "reviewed-change", MergeStrategy: "mr", Deliverable: "reviewed patch",
		Verification: "acceptance tests", AcceptanceAuthority: "reviewer", AdmittedBy: "triage",
	}
	admissionJSON, err := worklifecycle.SignAdmissionReceipt(admission, admissionKey)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := worklifecycle.AdmissionDigest(admission)
	if err != nil {
		t.Fatal(err)
	}
	completionJSON, err := worklifecycle.SignCompletionReceipt(worklifecycle.CompletionReceipt{
		Version: 1, WorkItemID: admission.WorkItemID, Scope: admission.Scope, AdmissionDigest: digest,
		DeliverableRef: "commit:reviewed", VerificationRef: "report:acceptance", AcceptedBy: "reviewer",
		AcceptedAt: time.Date(2026, 9, 26, 0, 0, 0, 0, time.UTC).Format(time.RFC3339),
	}, acceptanceKey)
	if err != nil {
		t.Fatal(err)
	}
	return cfg, beads.Bead{
		ID: admission.WorkItemID, Title: "accepted work", Type: "task", Status: "in_progress", Assignee: "original-owner",
		Metadata: map[string]string{
			beadmeta.LifecycleAdmissionReceiptMetadataKey:  admissionJSON,
			beadmeta.LifecycleCompletionReceiptMetadataKey: completionJSON,
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
