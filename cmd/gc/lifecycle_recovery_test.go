package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/attemptevidence"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worklifecycle"
)

func TestLifecycleRecoveryRequiresV2AttachmentAndPolicyProof(t *testing.T) {
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

	if got := fixture.provider.CountCalls("Nudge", fixture.info.SessionName); got != 0 {
		t.Fatalf("v2 receipt without attachment/policy proof sent %d nudges", got)
	}
	work, err := fixture.store.Get(fixture.work.ID)
	if err != nil {
		t.Fatal(err)
	}
	if work.Metadata[beadmeta.LifecycleRecoveryStateMetadataKey] != "" {
		t.Fatalf("unproved v2 admission consumed recovery budget: %s", work.Metadata[beadmeta.LifecycleRecoveryStateMetadataKey])
	}
	if !bytes.Contains(logs.Bytes(), []byte("no attempt reserved")) {
		t.Fatalf("controller did not explain the proof hold: %s", logs.String())
	}
}

func TestLifecycleRecoveryAttemptBindingPreservesSignedRevisionToken(t *testing.T) {
	fixture := newLifecycleRecoveryFixture(t)
	request := fixture.newRequest(t, "nudge-negative-revision", fixture.work.Revision)
	request.ExpectedRevision = -37
	binding, err := lifecycleRecoveryAttemptBinding(request, "city:"+fixture.city)
	if err != nil || binding.WorkRevision != "-37" {
		t.Fatalf("negative recovery revision binding=%+v err=%v", binding, err)
	}
	request.ExpectedRevision = 0
	if _, err := lifecycleRecoveryAttemptBinding(request, "city:"+fixture.city); err == nil {
		t.Fatal("zero revision sentinel produced an attempt binding")
	}
}

func TestLifecycleRecoveryDoesNotTrustSessionReceiptsWithoutAdmissionProof(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(session.RequestAttemptBinding, worklifecycle.RecoveryRequest) *session.RequestAttemptBinding
	}{
		{name: "legacy unbound", mutate: func(session.RequestAttemptBinding, worklifecycle.RecoveryRequest) *session.RequestAttemptBinding {
			return nil
		}},
		{name: "wrong physical store", mutate: func(binding session.RequestAttemptBinding, _ worklifecycle.RecoveryRequest) *session.RequestAttemptBinding {
			binding.StoreRef = "city:other"
			return &binding
		}},
		{name: "wrong signed work revision", mutate: func(binding session.RequestAttemptBinding, request worklifecycle.RecoveryRequest) *session.RequestAttemptBinding {
			binding.WorkRevision = strconv.FormatInt(request.ExpectedRevision+1, 10)
			return &binding
		}},
		{name: "wrong execution identity", mutate: func(binding session.RequestAttemptBinding, _ worklifecycle.RecoveryRequest) *session.RequestAttemptBinding {
			binding.Identity.ClaimGeneration = "other-claim-generation"
			attemptID, err := attemptevidence.AttemptID(binding.Identity)
			if err != nil {
				t.Fatal(err)
			}
			binding.AttemptID = attemptID
			return &binding
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newLifecycleRecoveryFixture(t)
			request := fixture.newRequest(t, "nudge-preexisting", fixture.work.Revision)
			fixture.persist(t, request)
			front := session.NewStore(beads.SessionStore{Store: fixture.store})
			generation, err := strconv.Atoi(request.SessionGeneration)
			if err != nil {
				t.Fatal(err)
			}
			binding := tc.mutate(lifecycleRecoveryBindingForTest(t, fixture, request), request)
			if binding == nil {
				if _, err := front.AcceptRequest(request.SessionID, request.RequestID, generation, request.Message, time.Now()); err != nil {
					t.Fatalf("seed legacy request receipt: %v", err)
				}
			} else {
				if _, err := front.SetCurrentClaimForGeneration(request.SessionID, binding.Identity.ExecutionBeadID, binding.Identity.ClaimGeneration); err != nil {
					t.Fatalf("seed mismatched reciprocal claim: %v", err)
				}
				if _, err := front.AcceptRequestForAttempt(request.SessionID, request.RequestID, generation, request.Message, *binding, time.Now()); err != nil {
					t.Fatalf("seed mismatched attempt receipt: %v", err)
				}
				if _, err := front.SetCurrentClaimForGeneration(request.SessionID, fixture.work.ID, request.ClaimGeneration); err != nil {
					t.Fatalf("restore authoritative reciprocal claim: %v", err)
				}
			}

			var logs bytes.Buffer
			reconcileLifecycleRecoveryRequests(context.Background(), fixture.city, fixture.cityPath, fixture.cfg, fixture.store, nil,
				beads.SessionStore{Store: fixture.store}, fixture.provider, &logs)
			if got := fixture.provider.CountCalls("Nudge", fixture.info.SessionName); got != 0 {
				t.Fatalf("mismatched receipt triggered %d provider send(s)", got)
			}
			if !bytes.Contains(logs.Bytes(), []byte("no attempt reserved")) {
				t.Fatalf("controller did not fail closed before session receipt reconciliation: %s", logs.String())
			}
			work, err := fixture.store.Get(fixture.work.ID)
			if err != nil {
				t.Fatal(err)
			}
			if raw := work.Metadata[beadmeta.LifecycleRecoveryStateMetadataKey]; raw != "" {
				t.Fatalf("mismatched receipt consumed recovery budget: %s", raw)
			}
		})
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

func TestLifecycleRecoveryDoesNotReserveBeforeAdmissionProof(t *testing.T) {
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
	if work.Metadata[beadmeta.LifecycleRecoveryStateMetadataKey] != "" {
		t.Fatalf("unproved admission consumed ambiguous recovery reservation: %s", work.Metadata[beadmeta.LifecycleRecoveryStateMetadataKey])
	}
	revision := work.Revision
	reconcileLifecycleRecoveryRequests(context.Background(), fixture.city, fixture.cityPath, fixture.cfg, fixture.store, nil,
		beads.SessionStore{Store: fixture.store}, fixture.provider, &logs)
	if got := fixture.provider.CountCalls("Nudge", fixture.info.SessionName); got != 0 {
		t.Fatalf("replay sent before admission proof: nudge count=%d", got)
	}
	work, err = fixture.store.Get(fixture.work.ID)
	if err != nil {
		t.Fatal(err)
	}
	if work.Revision != revision || work.Metadata[beadmeta.LifecycleRecoveryStateMetadataKey] != "" {
		t.Fatalf("unproved replay changed source revision/state: revision=%d state=%q", work.Revision, work.Metadata[beadmeta.LifecycleRecoveryStateMetadataKey])
	}
}

func TestLifecycleRecoveryRequiresProofBeforeBudgetOrEscalation(t *testing.T) {
	fixture := newLifecycleRecoveryFixture(t)
	request := fixture.newRequest(t, "nudge-first", fixture.work.Revision)
	fixture.persist(t, request)
	var logs bytes.Buffer
	reconcileLifecycleRecoveryRequests(context.Background(), fixture.city, fixture.cityPath, fixture.cfg, fixture.store, nil,
		beads.SessionStore{Store: fixture.store}, fixture.provider, &logs)
	if got := fixture.provider.CountCalls("Nudge", fixture.info.SessionName); got != 0 {
		t.Fatalf("unproved admission sent %d nudges", got)
	}
	work, err := fixture.store.Get(fixture.work.ID)
	if err != nil {
		t.Fatal(err)
	}
	if work.Metadata[beadmeta.LifecycleRecoveryStateMetadataKey] != "" {
		t.Fatalf("unproved admission consumed a recovery slot: %s", work.Metadata[beadmeta.LifecycleRecoveryStateMetadataKey])
	}
	if !bytes.Contains(logs.Bytes(), []byte("no attempt reserved")) {
		t.Fatalf("controller did not report the proof hold: %s", logs.String())
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
		Agents:        []config.Agent{{Name: "worker", Dir: "pilot", MaxActiveSessions: &maxSessions, DefaultSlingFormula: &workflow}},
		Lifecycle: config.LifecycleConfig{
			AdmissionEnabled: true, RecoveryEnabled: true, EscalationTarget: "mayor",
			AdmissionV2PrimaryAuthority: "triage",
			AdmissionV2Authorities:      map[string]string{"triage": base64.StdEncoding.EncodeToString(admissionPrivate.Public().(ed25519.PublicKey))},
			AcceptanceAuthorities:       map[string]string{"reviewer": base64.StdEncoding.EncodeToString(acceptancePrivate.Public().(ed25519.PublicKey))},
			RecoveryAuthorities: map[string]config.LifecycleRecoveryAuthority{"recovery": {
				PublicKey: base64.StdEncoding.EncodeToString(recoveryPublic), Actions: []string{"nudge"}, Scopes: []string{scope},
			}},
		},
	}
	admission, err := worklifecycle.SignAdmissionReceiptV2(worklifecycle.AdmissionReceiptV2{
		Version: 2, WorkItemID: "work-1", Scope: scope, ExpectedWorkRevision: 1, Route: "pilot/worker", Workflow: workflow,
		RoutingPolicyDigest: strings.Repeat("a", 64), MergeStrategy: "mr",
		Deliverable: "reviewed patch", Verification: "acceptance tests", AcceptanceAuthority: "reviewer", AdmittedBy: "triage",
	}, admissionPrivate)
	if err != nil {
		t.Fatal(err)
	}
	store := &beads.MemStore{IDPrefix: "work", HonorExplicitIDs: true}
	work, err := store.Create(beads.Bead{
		ID: "work-1", Title: "authorized recovery fixture", Type: "task", Status: "open",
		Labels:   []string{worklifecycle.AdmissionIntentLabel},
		Metadata: map[string]string{beadmeta.LifecycleAdmissionReceiptV2MetadataKey: admission},
	})
	if err != nil {
		t.Fatal(err)
	}
	cityPath := t.TempDir()
	reconcileLifecycleAdmission(city, cityPath, cfg, store, &bytes.Buffer{})
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
	if _, err := front.SetCurrentClaimForGeneration(info.ID, work.ID, "17"); err != nil {
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

func lifecycleRecoveryBindingForTest(t *testing.T, fixture *lifecycleRecoveryFixture, request worklifecycle.RecoveryRequest) session.RequestAttemptBinding {
	t.Helper()
	identity := attemptevidence.Identity{
		Kind: attemptevidence.KindWorkbench, OwnerBeadID: request.WorkItemID,
		ExecutionBeadID: request.WorkItemID, SessionID: request.SessionID,
		SessionGeneration: request.SessionGeneration, ClaimGeneration: request.ClaimGeneration,
	}
	attemptID, err := attemptevidence.AttemptID(identity)
	if err != nil {
		t.Fatal(err)
	}
	return session.RequestAttemptBinding{
		StoreRef: "city:" + fixture.city, AttemptID: attemptID,
		WorkRevision: strconv.FormatInt(request.ExpectedRevision, 10), Identity: identity,
	}
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
