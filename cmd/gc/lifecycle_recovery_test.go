package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worklifecycle"
)

func TestLifecycleRecoveryControllerNudgesOnlyTheCASWinnerAndObservesReplay(t *testing.T) {
	fixture := newLifecycleRecoveryFixture(t)
	request := fixture.newRequest(t, "nudge-1", fixture.work.Revision)
	fixture.persist(t, request)
	var logs bytes.Buffer
	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			reconcileLifecycleRecoveryRequests(context.Background(), fixture.city, fixture.cityPath, fixture.cfg, fixture.store, nil,
				beads.SessionStore{Store: fixture.store}, fixture.provider, &logs)
		}()
	}
	wg.Wait()
	reconcileLifecycleRecoveryRequests(context.Background(), fixture.city, fixture.cityPath, fixture.cfg, fixture.store, nil,
		beads.SessionStore{Store: fixture.store}, fixture.provider, &logs)

	if got := fixture.provider.CountCalls("Nudge", fixture.info.SessionName); got != 1 {
		t.Fatalf("nudge count=%d, want one CAS winner; log=%s", got, logs.String())
	}
	receipt, err := session.NewStore(beads.SessionStore{Store: fixture.store}).GetRequest(fixture.info.ID, request.RequestID)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.Delivery != session.RequestDeliveryAccepted || receipt.AcknowledgedAt != nil || receipt.Effect != "unverified" {
		t.Fatalf("session receipt=%+v, want provider acceptance only", receipt)
	}
	work, err := fixture.store.Get(fixture.work.ID)
	if err != nil {
		t.Fatal(err)
	}
	state := recoveryStateFromWork(t, work)
	if len(state.Attempts) != 1 || state.Attempts[0].RequestID != request.RequestID || state.Attempts[0].RequestDigest == "" || state.Attempts[0].ExpectedRevision != request.ExpectedRevision {
		t.Fatalf("recovery state did not retain the signed work/session request tuple: %+v", state)
	}
	if !bytes.Contains(logs.Bytes(), []byte("not useful-progress evidence")) {
		t.Fatalf("controller log conflated nudge receipt with progress: %s", logs.String())
	}
}

func TestLifecycleRecoveryDoesNotActWhenTargetChangesOrHoldArrives(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*testing.T, *lifecycleRecoveryFixture)
	}{
		{
			name: "hold after accepted intent",
			mutate: func(t *testing.T, f *lifecycleRecoveryFixture) {
				t.Helper()
				if err := f.store.Update(f.work.ID, beads.UpdateOpts{Labels: []string{"hold:external"}}); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "owner change after accepted intent",
			mutate: func(t *testing.T, f *lifecycleRecoveryFixture) {
				t.Helper()
				owner := "other-owner"
				if err := f.store.Update(f.work.ID, beads.UpdateOpts{Assignee: &owner}); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "session generation change after accepted intent",
			mutate: func(t *testing.T, f *lifecycleRecoveryFixture) {
				t.Helper()
				if err := f.store.SetMetadata(f.info.ID, "generation", "18"); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "claim generation change after accepted intent",
			mutate: func(t *testing.T, f *lifecycleRecoveryFixture) {
				t.Helper()
				if err := f.store.SetMetadata(f.work.ID, beadmeta.ClaimGenerationMetadataKey, "18"); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "reciprocal claim changes after accepted intent",
			mutate: func(t *testing.T, f *lifecycleRecoveryFixture) {
				t.Helper()
				other, err := f.store.Create(beads.Bead{ID: "other-work", Title: "other work"})
				if err != nil {
					t.Fatal(err)
				}
				front := session.NewStore(beads.SessionStore{Store: f.store})
				if _, err := front.SetCurrentClaim(f.info.ID, other.ID); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newLifecycleRecoveryFixture(t)
			request := fixture.newRequest(t, "nudge-1", fixture.work.Revision)
			fixture.persist(t, request)
			tc.mutate(t, fixture)
			reconcileLifecycleRecoveryRequests(context.Background(), fixture.city, fixture.cityPath, fixture.cfg, fixture.store, nil,
				beads.SessionStore{Store: fixture.store}, fixture.provider, &bytes.Buffer{})
			if got := fixture.provider.CountCalls("Nudge", fixture.info.SessionName); got != 0 {
				t.Fatalf("nudge count=%d after stale ownership/hold, want 0", got)
			}
			work, err := fixture.store.Get(fixture.work.ID)
			if err != nil {
				t.Fatal(err)
			}
			if raw := work.Metadata[beadmeta.LifecycleRecoveryStateMetadataKey]; raw != "" {
				t.Fatalf("stale request consumed budget before prerequisites passed: %s", raw)
			}
		})
	}
}

func TestLifecycleRecoveryExpiredRequestDoesNotReserveOrSend(t *testing.T) {
	fixture := newLifecycleRecoveryFixture(t)
	request := fixture.newRequest(t, "nudge-expired", fixture.work.Revision)
	issued := time.Now().UTC().Add(-2 * time.Hour)
	request.IssuedAt = issued.Format(time.RFC3339Nano)
	request.ExpiresAt = issued.Add(time.Hour).Format(time.RFC3339Nano)
	request, err := worklifecycle.SignRecoveryRequest(request, fixture.private)
	if err != nil {
		t.Fatal(err)
	}
	fixture.persist(t, request)

	reconcileLifecycleRecoveryRequests(context.Background(), fixture.city, fixture.cityPath, fixture.cfg, fixture.store, nil,
		beads.SessionStore{Store: fixture.store}, fixture.provider, &bytes.Buffer{})
	if got := fixture.provider.CountCalls("Nudge", fixture.info.SessionName); got != 0 {
		t.Fatalf("expired recovery request sent %d nudges, want none", got)
	}
	work, err := fixture.store.Get(fixture.work.ID)
	if err != nil {
		t.Fatal(err)
	}
	if raw := work.Metadata[beadmeta.LifecycleRecoveryStateMetadataKey]; raw != "" {
		t.Fatalf("expired recovery request consumed budget: %s", raw)
	}
}

func TestLifecycleRecoveryConsumesAmbiguousReservationWithoutSending(t *testing.T) {
	fixture := newLifecycleRecoveryFixture(t)
	request := fixture.newRequest(t, "nudge-ambiguous", fixture.work.Revision)
	fixture.persist(t, request)
	innerWriter, ok := beads.ConditionalWriterFor(fixture.store)
	if !ok {
		t.Fatal("fixture store does not support conditional writes")
	}
	store := &lifecycleRecoveryAmbiguousStore{Store: fixture.store, ConditionalWriter: innerWriter}
	var logs bytes.Buffer
	reconcileLifecycleRecoveryRequests(context.Background(), fixture.city, fixture.cityPath, fixture.cfg, store, nil,
		beads.SessionStore{Store: fixture.store}, fixture.provider, &logs)
	if got := fixture.provider.CountCalls("Nudge", fixture.info.SessionName); got != 0 {
		t.Fatalf("ambiguous CAS triggered %d provider nudge(s), want none", got)
	}
	work, err := fixture.store.Get(fixture.work.ID)
	if err != nil {
		t.Fatal(err)
	}
	state := recoveryStateFromWork(t, work)
	if len(state.Attempts) != 1 {
		t.Fatalf("maybe-committed reservation was not consumed: %+v", state)
	}
	reconcileLifecycleRecoveryRequests(context.Background(), fixture.city, fixture.cityPath, fixture.cfg, fixture.store, nil,
		beads.SessionStore{Store: fixture.store}, fixture.provider, &logs)
	if got := fixture.provider.CountCalls("Nudge", fixture.info.SessionName); got != 0 {
		t.Fatalf("replay sent after ambiguous reservation: nudge count=%d", got)
	}
}

func TestLifecycleRecoveryRechecksHoldAfterReservationAndEscalatesAtBudget(t *testing.T) {
	fixture := newLifecycleRecoveryFixture(t)
	first := fixture.newRequest(t, "nudge-first", fixture.work.Revision)
	fixture.persist(t, first)
	innerWriter, ok := beads.ConditionalWriterFor(fixture.store)
	if !ok {
		t.Fatal("fixture store does not support conditional writes")
	}
	store := &lifecycleRecoveryHoldAfterCASStore{Store: fixture.store, ConditionalWriter: innerWriter, target: fixture.work.ID}
	reconcileLifecycleRecoveryRequests(context.Background(), fixture.city, fixture.cityPath, fixture.cfg, store, nil,
		beads.SessionStore{Store: fixture.store}, fixture.provider, &bytes.Buffer{})
	if got := fixture.provider.CountCalls("Nudge", fixture.info.SessionName); got != 0 {
		t.Fatalf("a hold arriving after reservation still allowed %d nudges", got)
	}
	work, err := fixture.store.Get(fixture.work.ID)
	if err != nil {
		t.Fatal(err)
	}
	state := recoveryStateFromWork(t, work)
	if len(state.Attempts) != 1 {
		t.Fatalf("held action did not consume its reserved slot: %+v", state)
	}

	// An operator removes the hold and explicitly signs a second request against
	// the exact revision now visible. The second durable reservation exhausts
	// the fixed budget and leaves one immutable escalation request.
	if err := fixture.store.Update(work.ID, beads.UpdateOpts{RemoveLabels: []string{"hold:external"}}); err != nil {
		t.Fatal(err)
	}
	work, err = fixture.store.Get(work.ID)
	if err != nil {
		t.Fatal(err)
	}
	second := fixture.newRequest(t, "nudge-second", work.Revision)
	fixture.persist(t, second)
	outbox := lifecycleRecoveryOutbox{store: fixture.store, sessionStore: fixture.store}
	reconcileLifecycleRecoveryRequests(context.Background(), fixture.city, fixture.cityPath, fixture.cfg, fixture.store, nil,
		beads.SessionStore{Store: fixture.store}, fixture.provider, &bytes.Buffer{}, outbox)
	work, err = fixture.store.Get(work.ID)
	if err != nil {
		t.Fatal(err)
	}
	state = recoveryStateFromWork(t, work)
	if len(state.Attempts) != worklifecycle.MaxRecoveryAttempts || state.Escalation == nil || state.Escalation.Target != fixture.cfg.Lifecycle.EscalationTarget {
		t.Fatalf("exhaustion did not persist one escalation request: %+v", state)
	}
	if fixture.provider.CountCalls("Nudge", fixture.info.SessionName) != 1 {
		t.Fatal("hold-gated first request should not send; only second explicit request may nudge")
	}
	// Reconciliation replay observes both stored receipts and the same escalation
	// identity; it does not spend budget or send either request again.
	firstStateID := state.Escalation.ID
	reconcileLifecycleRecoveryRequests(context.Background(), fixture.city, fixture.cityPath, fixture.cfg, fixture.store, nil,
		beads.SessionStore{Store: fixture.store}, fixture.provider, &bytes.Buffer{}, outbox)
	work, err = fixture.store.Get(work.ID)
	if err != nil {
		t.Fatal(err)
	}
	state = recoveryStateFromWork(t, work)
	if len(state.Attempts) != worklifecycle.MaxRecoveryAttempts || state.Escalation == nil || state.Escalation.ID != firstStateID {
		t.Fatalf("replay changed exhausted state: %+v", state)
	}
	messageID, ok := recoveryEscalationMessageID(fixture.store, state.Escalation.DedupKey)
	if !ok {
		t.Fatal("fixture store should support stable outbox message IDs")
	}
	message, err := fixture.store.Get(messageID)
	if err != nil {
		t.Fatalf("durable escalation message missing: %v", err)
	}
	if message.Type != "message" || message.Assignee != state.Escalation.Target || message.Ephemeral ||
		message.Metadata["mail.stable_outbox"] != state.Escalation.DedupKey {
		t.Fatalf("escalation outbox row=%+v, want one durable stable-ID message", message)
	}
	rows, err := fixture.store.List(beads.ListQuery{Type: "message", TierMode: beads.TierBoth, AllowScan: true})
	if err != nil || len(rows) != 1 || rows[0].ID != messageID {
		t.Fatalf("replayed outbox rows=%+v err=%v, want one stable message", rows, err)
	}
}

type lifecycleRecoveryFixture struct {
	city     string
	cityPath string
	cfg      *config.City
	store    *beads.MemStore
	provider *runtime.Fake
	info     session.Info
	work     beads.Bead
	private  ed25519.PrivateKey
}

func newLifecycleRecoveryFixture(t *testing.T) *lifecycleRecoveryFixture {
	t.Helper()
	_, admissionPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	_, acceptancePrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	recoveryPublic, recoveryPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	formulaDir := t.TempDir()
	formula := "formula = \"review\"\nversion = 1\n\n[[steps]]\nid = \"work\"\ntitle = \"Review work\"\n"
	if err := os.WriteFile(filepath.Join(formulaDir, "review.toml"), []byte(formula), 0o600); err != nil {
		t.Fatal(err)
	}
	workflow := "review"
	maxSessions := 1
	city := "pilot"
	scope := worklifecycle.ScopeForStore(city, "city:"+city)
	cfg := &config.City{
		Workspace:     config.Workspace{Name: city},
		FormulaLayers: config.FormulaLayers{City: []string{formulaDir}},
		Agents:        []config.Agent{{Name: "worker", MaxActiveSessions: &maxSessions, DefaultSlingFormula: &workflow}},
		Lifecycle: config.LifecycleConfig{
			AdmissionEnabled: true, RecoveryEnabled: true, EscalationTarget: "mayor",
			AdmissionAuthorities:  map[string]string{"triage": base64.StdEncoding.EncodeToString(admissionPrivate.Public().(ed25519.PublicKey))},
			AcceptanceAuthorities: map[string]string{"reviewer": base64.StdEncoding.EncodeToString(acceptancePrivate.Public().(ed25519.PublicKey))},
			RecoveryAuthorities: map[string]config.LifecycleRecoveryAuthority{"recovery": {
				PublicKey: base64.StdEncoding.EncodeToString(recoveryPublic), Actions: []string{"nudge"}, Scopes: []string{scope},
			}},
		},
	}
	admission, err := worklifecycle.SignAdmissionReceipt(worklifecycle.AdmissionReceipt{
		Version: 1, WorkItemID: "work-1", Scope: scope, Route: "worker", Workflow: workflow, MergeStrategy: "mr",
		Deliverable: "reviewed patch", Verification: "acceptance tests", AcceptanceAuthority: "reviewer", AdmittedBy: "triage",
	}, admissionPrivate)
	if err != nil {
		t.Fatal(err)
	}
	store := &beads.MemStore{IDPrefix: "work", HonorExplicitIDs: true}
	work, err := store.Create(beads.Bead{
		ID: "work-1", Title: "authorized recovery fixture", Type: "task", Status: "open",
		Labels:   []string{worklifecycle.AdmissionIntentLabel},
		Metadata: map[string]string{beadmeta.LifecycleAdmissionReceiptMetadataKey: admission},
	})
	if err != nil {
		t.Fatal(err)
	}
	cityPath := t.TempDir()
	reconcileLifecycleAdmission(city, cityPath, cfg, store, nil, nil, &bytes.Buffer{})
	work, err = store.Get(work.ID)
	if err != nil {
		t.Fatal(err)
	}
	provider := runtime.NewFake()
	sessionManager := session.NewManagerWithOptions(store, provider)
	info, err := sessionManager.CreateSession(context.Background(), session.CreateOptions{
		Template: "worker", Command: "claude", WorkDir: t.TempDir(), Provider: "claude",
	})
	if err != nil {
		t.Fatal(err)
	}
	owner := info.ID
	status := "in_progress"
	if err := store.Update(work.ID, beads.UpdateOpts{
		Status: &status, Assignee: &owner,
		Metadata: map[string]string{
			beadmeta.ClaimGenerationMetadataKey: "17",
			beadmeta.SessionIDMetadataKey:       info.ID,
		},
	}); err != nil {
		t.Fatal(err)
	}
	front := session.NewStore(beads.SessionStore{Store: store})
	if _, err := front.SetCurrentClaim(info.ID, work.ID); err != nil {
		t.Fatal(err)
	}
	work, err = store.Get(work.ID)
	if err != nil {
		t.Fatal(err)
	}
	return &lifecycleRecoveryFixture{city: city, cityPath: cityPath, cfg: cfg, store: store, provider: provider, info: info, work: work, private: recoveryPrivate}
}

func (f *lifecycleRecoveryFixture) newRequest(t *testing.T, id string, revision int64) worklifecycle.RecoveryRequest {
	t.Helper()
	now := time.Now().UTC()
	request, err := worklifecycle.SignRecoveryRequest(worklifecycle.RecoveryRequest{
		Version: 1, RequestID: id, Action: "nudge", Scope: worklifecycle.ScopeForStore(f.city, "city:"+f.city),
		WorkItemID: f.work.ID, ExpectedRevision: revision, Owner: f.work.Assignee,
		ClaimGeneration: f.work.Metadata[beadmeta.ClaimGenerationMetadataKey],
		SessionID:       f.info.ID, SessionGeneration: f.info.Generation,
		Message:  "Please report the current task status and a verifiable useful-progress artifact.",
		IssuedAt: now.Add(-time.Minute).Format(time.RFC3339Nano), ExpiresAt: now.Add(time.Hour).Format(time.RFC3339Nano),
		AuthorizedBy: "recovery",
	}, f.private)
	if err != nil {
		t.Fatal(err)
	}
	return request
}

func (f *lifecycleRecoveryFixture) persist(t *testing.T, request worklifecycle.RecoveryRequest) {
	t.Helper()
	digest, err := worklifecycle.RecoveryRequestDigest(request)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := worklifecycle.PersistRecoveryIntent(f.store, "work", request, digest); err != nil {
		t.Fatal(err)
	}
}

func recoveryStateFromWork(t *testing.T, work beads.Bead) worklifecycle.RecoveryState {
	t.Helper()
	var state worklifecycle.RecoveryState
	if err := json.Unmarshal([]byte(work.Metadata[beadmeta.LifecycleRecoveryStateMetadataKey]), &state); err != nil {
		t.Fatal(err)
	}
	return state
}

type lifecycleRecoveryAmbiguousStore struct {
	beads.Store
	beads.ConditionalWriter
}

func (s *lifecycleRecoveryAmbiguousStore) UpdateIfMatch(id string, revision int64, opts beads.UpdateOpts) error {
	if err := s.ConditionalWriter.UpdateIfMatch(id, revision, opts); err != nil {
		return err
	}
	return errors.New("simulated lost conditional-write response")
}

type lifecycleRecoveryHoldAfterCASStore struct {
	beads.Store
	beads.ConditionalWriter
	target string
}

func (s *lifecycleRecoveryHoldAfterCASStore) UpdateIfMatch(id string, revision int64, opts beads.UpdateOpts) error {
	if err := s.ConditionalWriter.UpdateIfMatch(id, revision, opts); err != nil {
		return err
	}
	if id == s.target {
		if err := s.Update(id, beads.UpdateOpts{Labels: []string{"hold:external"}}); err != nil {
			return err
		}
	}
	return nil
}
