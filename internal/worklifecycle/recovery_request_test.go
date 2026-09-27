package worklifecycle

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

func TestRecoveryRequestVerificationBindsAuthorityScopeAndWindow(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	request := signedRecoveryRequest(t, private, now)
	cfg := recoveryRequestConfig(public)
	if _, err := VerifyRecoveryRequest(request, cfg, now); err != nil {
		t.Fatalf("VerifyRecoveryRequest(valid): %v", err)
	}

	bad := request
	bad.Scope = "city:elsewhere/city:elsewhere"
	if _, err := VerifyRecoveryRequest(bad, cfg, now); !errors.Is(err, ErrRecoveryRequestInvalid) {
		t.Fatalf("wrong scope error = %v", err)
	}
	bad = request
	bad.Action = "replace-owner"
	if _, err := VerifyRecoveryRequest(bad, cfg, now); !errors.Is(err, ErrRecoveryRequestInvalid) {
		t.Fatalf("unsupported action error = %v", err)
	}
	if _, err := VerifyRecoveryRequest(request, cfg, requestExpiry(request)); !errors.Is(err, ErrRecoveryRequestInvalid) {
		t.Fatalf("expired request error = %v", err)
	}
	bad = request
	bad.Message += " changed"
	if _, err := VerifyRecoveryRequest(bad, cfg, now); !errors.Is(err, ErrRecoveryRequestInvalid) {
		t.Fatalf("mutated payload error = %v", err)
	}
	wrongPublic, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cfg.RecoveryAuthorities["operator"] = config.LifecycleRecoveryAuthority{PublicKey: base64.StdEncoding.EncodeToString(wrongPublic), Actions: []string{"nudge"}, Scopes: []string{request.Scope}}
	if _, err := VerifyRecoveryRequest(request, cfg, now); !errors.Is(err, ErrRecoveryRequestInvalid) {
		t.Fatalf("wrong signer error = %v", err)
	}
}

func TestPersistRecoveryIntentIsStableHeldAndReplaySafe(t *testing.T) {
	store := &beads.MemStore{HonorExplicitIDs: true, IDPrefix: "gc"}
	work := mustCreateRecoveryWork(t, store)
	if _, err := store.Create(beads.Bead{ID: "gc-open-1", Title: "unrelated open work"}); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	_ = public
	request := signedRecoveryRequest(t, private, now)
	request.WorkItemID = work.ID
	request.ExpectedRevision = work.Revision
	request, err = SignRecoveryRequest(request, private)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := RecoveryRequestDigest(request)
	if err != nil {
		t.Fatal(err)
	}
	first, created, err := PersistRecoveryIntent(store, "gc", request, digest)
	if err != nil || !created {
		t.Fatalf("PersistRecoveryIntent(first) = (created=%v, err=%v)", created, err)
	}
	if first.Type != "lifecycle-intent" || !beads.HasReadyExcludedLabel(first) {
		t.Fatalf("intent type/labels = %q/%v, want held infrastructure intent", first.Type, first.Labels)
	}
	gotIntent, err := DecodeRecoveryIntent(first)
	if err != nil || gotIntent.Digest != digest || gotIntent.Request.RequestID != request.RequestID {
		t.Fatalf("DecodeRecoveryIntent = (%+v, %v)", gotIntent, err)
	}
	second, created, err := PersistRecoveryIntent(store, "gc", request, digest)
	if err != nil || created || second.ID != first.ID {
		t.Fatalf("PersistRecoveryIntent(replay) = (%q, created=%v, err=%v)", second.ID, created, err)
	}
	changed := request
	changed.Message = "different authorized message"
	changed, err = SignRecoveryRequest(changed, private)
	if err != nil {
		t.Fatal(err)
	}
	changedDigest, err := RecoveryRequestDigest(changed)
	if err != nil {
		t.Fatal(err)
	}
	if _, created, err := PersistRecoveryIntent(store, "gc", changed, changedDigest); created || !errors.Is(err, ErrRecoveryRequestConflict) {
		t.Fatalf("changed replay = (created=%v, err=%v), want conflict", created, err)
	}
	ready, err := store.Ready()
	if err != nil {
		t.Fatal(err)
	}
	if len(ready) != 1 || ready[0].ID != "gc-open-1" {
		t.Fatalf("Ready() returned %+v, want only unrelated open work", ready)
	}
}

func TestPersistRecoveryIntentRejectsChangedWorkBeforeIntake(t *testing.T) {
	store := &beads.MemStore{HonorExplicitIDs: true, IDPrefix: "gc"}
	work := mustCreateRecoveryWork(t, store)
	now := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	request := signedRecoveryRequest(t, private, now)
	request.WorkItemID = work.ID
	request.ExpectedRevision = work.Revision
	request, err = SignRecoveryRequest(request, private)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetMetadata(work.ID, "changed", "true"); err != nil {
		t.Fatal(err)
	}
	digest, _ := RecoveryRequestDigest(request)
	if _, created, err := PersistRecoveryIntent(store, "gc", request, digest); created || !errors.Is(err, ErrRecoveryWorkStale) {
		t.Fatalf("stale intake = (created=%v, err=%v), want stale refusal", created, err)
	}
}

func TestReserveRecoveryRequestAttemptConsumesSignedRevisionOnce(t *testing.T) {
	store := &beads.MemStore{HonorExplicitIDs: true, IDPrefix: "gc"}
	work := mustCreateRecoveryWork(t, store)
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	request := signedRecoveryRequest(t, private, time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC))
	request.ExpectedRevision = work.Revision
	request, err = SignRecoveryRequest(request, private)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := RecoveryRequestDigest(request)
	if err != nil {
		t.Fatal(err)
	}
	first, reserved, err := ReserveRecoveryRequestAttempt(store, request, digest)
	if err != nil || !reserved || len(first.Attempts) != 1 {
		t.Fatalf("first reservation = (%+v, %v, %v), want one winning reservation", first, reserved, err)
	}
	replay, reserved, err := ReserveRecoveryRequestAttempt(store, request, digest)
	if err != nil || reserved || len(replay.Attempts) != 1 {
		t.Fatalf("replay = (%+v, %v, %v), want same reservation without a side effect grant", replay, reserved, err)
	}
	changed := request
	changed.Message += " changed"
	changed, err = SignRecoveryRequest(changed, private)
	if err != nil {
		t.Fatal(err)
	}
	changedDigest, _ := RecoveryRequestDigest(changed)
	if _, reserved, err := ReserveRecoveryRequestAttempt(store, changed, changedDigest); reserved || !errors.Is(err, ErrRecoveryRequestConflict) {
		t.Fatalf("changed request replay = (reserved=%v, err=%v), want content conflict", reserved, err)
	}
	state, err := store.Get(work.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := state.Metadata[beadmeta.LifecycleRecoveryStateMetadataKey]; got == "" {
		t.Fatal("recovery reservation was not persisted on the work row")
	}
}

func TestConcurrentRecoveryRequestReservationsHaveOneWinnerAndStaleRevisionHolds(t *testing.T) {
	store := &beads.MemStore{HonorExplicitIDs: true, IDPrefix: "gc"}
	work := mustCreateRecoveryWork(t, store)
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	request := signedRecoveryRequest(t, private, time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC))
	request.ExpectedRevision = work.Revision
	request, err = SignRecoveryRequest(request, private)
	if err != nil {
		t.Fatal(err)
	}
	digest, _ := RecoveryRequestDigest(request)
	type result struct {
		reserved bool
		err      error
	}
	results := make(chan result, 2)
	var start sync.WaitGroup
	start.Add(1)
	var workers sync.WaitGroup
	workers.Add(2)
	for range 2 {
		go func() {
			defer workers.Done()
			start.Wait()
			_, reserved, reserveErr := ReserveRecoveryRequestAttempt(store, request, digest)
			results <- result{reserved: reserved, err: reserveErr}
		}()
	}
	start.Done()
	workers.Wait()
	close(results)
	winners := 0
	for got := range results {
		if got.err != nil {
			t.Fatalf("concurrent reservation error: %v", got.err)
		}
		if got.reserved {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("concurrent winning reservations = %d, want exactly one", winners)
	}
	state, _, err := readRecoveryState(store, work.ID, request.Scope)
	if err != nil || len(state.Attempts) != 1 {
		t.Fatalf("stored state = (%+v, %v), want one attempt", state, err)
	}
	newRequest := request
	newRequest.RequestID = "recover-456"
	newRequest, err = SignRecoveryRequest(newRequest, private)
	if err != nil {
		t.Fatal(err)
	}
	newDigest, _ := RecoveryRequestDigest(newRequest)
	if _, reserved, err := ReserveRecoveryRequestAttempt(store, newRequest, newDigest); reserved || !errors.Is(err, ErrRecoveryWorkStale) {
		t.Fatalf("old revision reservation = (reserved=%v, err=%v), want stale refusal", reserved, err)
	}
}

func TestRecoveryRequestReservationRefusesUnsupportedConditionalStore(t *testing.T) {
	store := &beads.MemStore{HonorExplicitIDs: true, DisableConditionalWrites: true}
	work := mustCreateRecoveryWork(t, store)
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	request := signedRecoveryRequest(t, private, time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC))
	request.ExpectedRevision = work.Revision
	request, err = SignRecoveryRequest(request, private)
	if err != nil {
		t.Fatal(err)
	}
	digest, _ := RecoveryRequestDigest(request)
	if _, reserved, err := ReserveRecoveryRequestAttempt(store, request, digest); reserved || !errors.Is(err, beads.ErrConditionalWriteUnsupported) {
		t.Fatalf("unsupported reservation = (reserved=%v, err=%v), want fail-closed refusal", reserved, err)
	}
	got, err := store.Get(work.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Metadata[beadmeta.LifecycleRecoveryStateMetadataKey] != "" {
		t.Fatal("unsupported store mutated recovery budget")
	}
}

func TestRecoveryRequestReservationRejectsChangedCASReadback(t *testing.T) {
	backing := &beads.MemStore{HonorExplicitIDs: true, IDPrefix: "gc"}
	work := mustCreateRecoveryWork(t, backing)
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	request := signedRecoveryRequest(t, private, time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC))
	request.ExpectedRevision = work.Revision
	request, err = SignRecoveryRequest(request, private)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := RecoveryRequestDigest(request)
	if err != nil {
		t.Fatal(err)
	}
	writer, ok := beads.ConditionalWriterFor(backing)
	if !ok {
		t.Fatal("fixture store does not support conditional writes")
	}
	store := &recoveryStateReadbackRewriteStore{Store: backing, ConditionalWriter: writer}
	state, reserved, _, err := ReserveRecoveryRequestAttemptWithFence(store, request, digest)
	if reserved || !errors.Is(err, ErrRecoveryStateInvalid) {
		t.Fatalf("reservation with altered readback = (%+v, %v, %v), want no authority and ErrRecoveryStateInvalid", state, reserved, err)
	}
}

type recoveryStateReadbackRewriteStore struct {
	beads.Store
	beads.ConditionalWriter
	rewriteReadback bool
}

func (s *recoveryStateReadbackRewriteStore) UpdateIfMatch(id string, revision int64, opts beads.UpdateOpts) error {
	if err := s.ConditionalWriter.UpdateIfMatch(id, revision, opts); err != nil {
		return err
	}
	s.rewriteReadback = true
	return nil
}

func (s *recoveryStateReadbackRewriteStore) Get(id string) (beads.Bead, error) {
	bead, err := s.Store.Get(id)
	if err != nil || !s.rewriteReadback {
		return bead, err
	}
	s.rewriteReadback = false
	var state RecoveryState
	if err := json.Unmarshal([]byte(bead.Metadata[beadmeta.LifecycleRecoveryStateMetadataKey]), &state); err != nil {
		return beads.Bead{}, err
	}
	state.Attempts[len(state.Attempts)-1].ID = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	state.Attempts[len(state.Attempts)-1].ReservedAt = "2026-09-27T12:00:01Z"
	encoded, err := json.Marshal(state)
	if err != nil {
		return beads.Bead{}, err
	}
	bead.Metadata[beadmeta.LifecycleRecoveryStateMetadataKey] = string(encoded)
	return bead, nil
}

func mustCreateRecoveryWork(t *testing.T, store beads.Store) beads.Bead {
	t.Helper()
	work, err := store.Create(beads.Bead{ID: "gc-work-1", Title: "approved work", Status: "in_progress", Assignee: "worker", Metadata: beads.StringMap{
		beadmeta.ClaimGenerationMetadataKey: "7",
		beadmeta.SessionIDMetadataKey:       "gc-session-1",
	}})
	if err != nil {
		t.Fatal(err)
	}
	status := "in_progress"
	if err := store.Update(work.ID, beads.UpdateOpts{Status: &status, Assignee: &work.Assignee}); err != nil {
		t.Fatal(err)
	}
	work, err = store.Get(work.ID)
	if err != nil {
		t.Fatal(err)
	}
	return work
}

func signedRecoveryRequest(t *testing.T, private ed25519.PrivateKey, now time.Time) RecoveryRequest {
	t.Helper()
	request, err := SignRecoveryRequest(RecoveryRequest{
		Version: 1, RequestID: "recover-123", Action: "nudge", Scope: "city:test/city:test",
		WorkItemID: "gc-work-1", ExpectedRevision: 1, Owner: "worker", ClaimGeneration: "7",
		SessionID: "gc-session-1", SessionGeneration: "3", Message: "Please check the assigned task and report verified progress.",
		IssuedAt: now.Format(time.RFC3339Nano), ExpiresAt: now.Add(time.Hour).Format(time.RFC3339Nano), AuthorizedBy: "operator",
	}, private)
	if err != nil {
		t.Fatal(err)
	}
	return request
}

func requestExpiry(request RecoveryRequest) time.Time {
	expiresAt, err := time.Parse(time.RFC3339Nano, request.ExpiresAt)
	if err != nil {
		panic(err)
	}
	return expiresAt
}

func recoveryRequestConfig(public ed25519.PublicKey) config.LifecycleConfig {
	return config.LifecycleConfig{
		AdmissionEnabled: true, RecoveryEnabled: true,
		RecoveryAuthorities: map[string]config.LifecycleRecoveryAuthority{
			"operator": {PublicKey: base64.StdEncoding.EncodeToString(public), Actions: []string{"nudge"}, Scopes: []string{"city:test/city:test"}},
		},
	}
}
