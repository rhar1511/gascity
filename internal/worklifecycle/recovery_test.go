package worklifecycle

import (
	"errors"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/fsys"
)

const recoveryTestScope = "city:pilot/rig:worker"

func recoveryWorkItem(t *testing.T, store beads.Store) string {
	t.Helper()
	bead, err := store.Create(beads.Bead{Title: "recovery budget fixture"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return bead.ID
}

func readStoredRecoveryState(t *testing.T, store beads.Store, beadID string) (RecoveryState, string) {
	t.Helper()
	bead, err := store.Get(beadID)
	if err != nil {
		t.Fatalf("Get(%q): %v", beadID, err)
	}
	raw := bead.Metadata[beadmeta.LifecycleRecoveryStateMetadataKey]
	if raw == "" {
		return RecoveryState{}, raw
	}
	state, err := decodeRecoveryState(raw, beadID, recoveryTestScope)
	if err != nil {
		t.Fatalf("decode stored recovery state: %v", err)
	}
	return state, raw
}

func TestReserveRecoveryAttemptConsumesFixedBudget(t *testing.T) {
	store := beads.NewMemStore()
	beadID := recoveryWorkItem(t, store)

	first, reserved, err := ReserveRecoveryAttempt(store, beadID, recoveryTestScope)
	if err != nil || !reserved {
		t.Fatalf("first reservation = (%+v, %v, %v), want reserved", first, reserved, err)
	}
	second, reserved, err := ReserveRecoveryAttempt(store, beadID, recoveryTestScope)
	if err != nil || !reserved {
		t.Fatalf("second reservation = (%+v, %v, %v), want reserved", second, reserved, err)
	}
	if len(first.Attempts) != 1 || len(second.Attempts) != 2 {
		t.Fatalf("reservation counts = %d then %d, want 1 then 2", len(first.Attempts), len(second.Attempts))
	}
	if first.Attempts[0].ID == second.Attempts[1].ID || first.Attempts[0].ID != second.Attempts[0].ID {
		t.Fatalf("second reservation must preserve the first ID and add a unique ID: first=%+v second=%+v", first.Attempts, second.Attempts)
	}

	_, before := readStoredRecoveryState(t, store, beadID)
	third, reserved, err := ReserveRecoveryAttempt(store, beadID, recoveryTestScope)
	if err != nil || reserved {
		t.Fatalf("third reservation = (%+v, %v, %v), want budget refusal", third, reserved, err)
	}
	_, after := readStoredRecoveryState(t, store, beadID)
	if before != after {
		t.Fatalf("refused reservation changed metadata: before=%s after=%s", before, after)
	}
}

func TestRecoveryBudgetPersistsAcrossFileStoreReopenAndUnknownOutcome(t *testing.T) {
	path := filepath.Join(t.TempDir(), "recovery.json")
	store, err := beads.OpenFileStore(fsys.OSFS{}, path)
	if err != nil {
		t.Fatalf("OpenFileStore: %v", err)
	}
	beadID := recoveryWorkItem(t, store)

	first, reserved, err := ReserveRecoveryAttempt(store, beadID, recoveryTestScope)
	if err != nil || !reserved || len(first.Attempts) != 1 {
		t.Fatalf("first reservation = (%+v, %v, %v), want one durable reservation", first, reserved, err)
	}
	// Model a process restart after the effect's outcome is unknown. The durable
	// reservation remains consumed even though no success is reported here.
	store, err = beads.OpenFileStore(fsys.OSFS{}, path)
	if err != nil {
		t.Fatalf("reopen FileStore: %v", err)
	}
	second, reserved, err := ReserveRecoveryAttempt(store, beadID, recoveryTestScope)
	if err != nil || !reserved || len(second.Attempts) != 2 {
		t.Fatalf("second reservation after reopen = (%+v, %v, %v), want second durable reservation", second, reserved, err)
	}
	// A later status transition that might look like recovery does not refund
	// either reservation if the work item is reopened.
	if err := store.Close(beadID); err != nil {
		t.Fatalf("Close after apparent recovery: %v", err)
	}
	if err := store.Reopen(beadID); err != nil {
		t.Fatalf("Reopen after apparent recovery: %v", err)
	}

	store, err = beads.OpenFileStore(fsys.OSFS{}, path)
	if err != nil {
		t.Fatalf("reopen FileStore after second reservation: %v", err)
	}
	third, reserved, err := ReserveRecoveryAttempt(store, beadID, recoveryTestScope)
	if err != nil || reserved || len(third.Attempts) != MaxRecoveryAttempts {
		t.Fatalf("third reservation after reopen = (%+v, %v, %v), want exhausted persisted budget", third, reserved, err)
	}
}

func TestAmbiguousRecoveryReservationConsumesBudgetWithoutAuthorizingEffect(t *testing.T) {
	store := beads.NewMemStore()
	beadID := recoveryWorkItem(t, store)
	ambiguous := errors.New("CAS committed but response was lost")
	wrapped := &ambiguousRecoveryCASStore{Store: store, writer: store, err: ambiguous}

	state, reserved, err := ReserveRecoveryAttempt(wrapped, beadID, recoveryTestScope)
	if reserved || !errors.Is(err, ambiguous) {
		t.Fatalf("ambiguous reservation = (%+v, %v, %v), want error and no effect authorization", state, reserved, err)
	}

	second, reserved, err := ReserveRecoveryAttempt(store, beadID, recoveryTestScope)
	if err != nil || !reserved || len(second.Attempts) != MaxRecoveryAttempts {
		t.Fatalf("reservation after ambiguous commit = (%+v, %v, %v), want remaining slot", second, reserved, err)
	}
	third, reserved, err := ReserveRecoveryAttempt(store, beadID, recoveryTestScope)
	if err != nil || reserved || len(third.Attempts) != MaxRecoveryAttempts {
		t.Fatalf("reservation after uncertain slot consumed = (%+v, %v, %v), want exhausted", third, reserved, err)
	}
}

func TestConcurrentRecoveryReservationsHaveOnlyTwoWinners(t *testing.T) {
	store := beads.NewMemStore()
	beadID := recoveryWorkItem(t, store)

	const contenders = 32
	start := make(chan struct{})
	results := make(chan reservationResult, contenders)
	var wg sync.WaitGroup
	for range contenders {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			state, reserved, err := ReserveRecoveryAttempt(store, beadID, recoveryTestScope)
			results <- reservationResult{state: state, reserved: reserved, err: err}
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	wins := 0
	for result := range results {
		if result.err != nil {
			t.Errorf("concurrent reservation: %v", result.err)
			continue
		}
		if result.reserved {
			wins++
		}
	}
	if wins != MaxRecoveryAttempts {
		t.Fatalf("successful concurrent reservations = %d, want exactly %d", wins, MaxRecoveryAttempts)
	}
	state, _ := readStoredRecoveryState(t, store, beadID)
	if len(state.Attempts) != MaxRecoveryAttempts {
		t.Fatalf("persisted concurrent reservations = %d, want %d", len(state.Attempts), MaxRecoveryAttempts)
	}
}

func TestRecoveryEscalationIsOneDurableRequestAfterExhaustion(t *testing.T) {
	store := beads.NewMemStore()
	beadID := recoveryWorkItem(t, store)
	first, reserved, err := ReserveRecoveryAttempt(store, beadID, recoveryTestScope)
	if err != nil || !reserved {
		t.Fatalf("first reservation = (%+v, %v, %v)", first, reserved, err)
	}
	before, rawBefore := readStoredRecoveryState(t, store, beadID)
	if len(before.Attempts) != 1 {
		t.Fatalf("pre-exhaustion recovery state has %d attempts, want 1", len(before.Attempts))
	}
	state, requested, err := RequestRecoveryEscalation(store, beadID, recoveryTestScope, "human-ops")
	if err != nil || requested || len(state.Attempts) != 1 || state.Escalation != nil {
		t.Fatalf("early escalation = (%+v, %v, %v), want read-only refusal", state, requested, err)
	}
	_, rawAfter := readStoredRecoveryState(t, store, beadID)
	if rawBefore != rawAfter {
		t.Fatalf("early escalation changed metadata: before=%s after=%s", rawBefore, rawAfter)
	}

	second, reserved, err := ReserveRecoveryAttempt(store, beadID, recoveryTestScope)
	if err != nil || !reserved || len(second.Attempts) != MaxRecoveryAttempts {
		t.Fatalf("second reservation = (%+v, %v, %v)", second, reserved, err)
	}

	const contenders = 24
	start := make(chan struct{})
	results := make(chan escalationResult, contenders)
	var wg sync.WaitGroup
	for range contenders {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			state, requested, err := RequestRecoveryEscalation(store, beadID, recoveryTestScope, "human-ops")
			results <- escalationResult{state: state, newlyRequested: requested, err: err}
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	winners := 0
	var persisted RecoveryState
	for result := range results {
		if result.err != nil {
			t.Errorf("concurrent escalation: %v", result.err)
			continue
		}
		if result.newlyRequested {
			winners++
		}
		if result.state.Escalation == nil || result.state.Escalation.Target != "human-ops" {
			t.Errorf("concurrent escalation state = %+v, want durable human-ops request", result.state)
			continue
		}
		if persisted.Escalation == nil {
			persisted = result.state
		} else if persisted.Escalation.ID != result.state.Escalation.ID || persisted.Escalation.DedupKey != result.state.Escalation.DedupKey {
			t.Errorf("contenders observed different escalation identities: %+v and %+v", persisted.Escalation, result.state.Escalation)
		}
	}
	if winners != 1 {
		t.Fatalf("durable escalation winners = %d, want exactly one", winners)
	}

	replay, requested, err := RequestRecoveryEscalation(store, beadID, recoveryTestScope, "human-ops")
	if err != nil || requested || replay.Escalation == nil || replay.Escalation.ID != persisted.Escalation.ID || replay.Escalation.DedupKey != persisted.Escalation.DedupKey {
		t.Fatalf("same-target replay = (%+v, %v, %v), want same durable request without new transition", replay, requested, err)
	}
	_, beforeConflict := readStoredRecoveryState(t, store, beadID)
	conflict, requested, err := RequestRecoveryEscalation(store, beadID, recoveryTestScope, "different-target")
	if !errors.Is(err, ErrRecoveryEscalationTargetConflict) || requested || conflict.Escalation == nil {
		t.Fatalf("changed-target replay = (%+v, %v, %v), want immutable-target error", conflict, requested, err)
	}
	_, afterConflict := readStoredRecoveryState(t, store, beadID)
	if beforeConflict != afterConflict {
		t.Fatalf("target conflict changed persisted request: before=%s after=%s", beforeConflict, afterConflict)
	}
}

func TestRecoveryCASUnsupportedFailsClosed(t *testing.T) {
	backing := beads.NewMemStore()
	beadID := recoveryWorkItem(t, backing)
	wrapped := recoveryNoCASStore{Store: backing}

	state, reserved, err := ReserveRecoveryAttempt(wrapped, beadID, recoveryTestScope)
	if reserved || !errors.Is(err, beads.ErrConditionalWriteUnsupported) {
		t.Fatalf("unsupported reservation = (%+v, %v, %v), want conditional-write refusal", state, reserved, err)
	}
	_, raw := readStoredRecoveryState(t, backing, beadID)
	if raw != "" {
		t.Fatalf("unsupported store wrote recovery metadata: %s", raw)
	}

	if _, ok, err := ReserveRecoveryAttempt(backing, beadID, recoveryTestScope); err != nil || !ok {
		t.Fatalf("setup first reservation = (%v, %v)", ok, err)
	}
	if _, ok, err := ReserveRecoveryAttempt(backing, beadID, recoveryTestScope); err != nil || !ok {
		t.Fatalf("setup second reservation = (%v, %v)", ok, err)
	}
	state, requested, err := RequestRecoveryEscalation(wrapped, beadID, recoveryTestScope, "human-ops")
	if requested || !errors.Is(err, beads.ErrConditionalWriteUnsupported) {
		t.Fatalf("unsupported escalation = (%+v, %v, %v), want conditional-write refusal", state, requested, err)
	}
	stored, _ := readStoredRecoveryState(t, backing, beadID)
	if stored.Escalation != nil {
		t.Fatalf("unsupported store wrote escalation: %+v", stored.Escalation)
	}

	disabled := beads.NewMemStore()
	disabledID := recoveryWorkItem(t, disabled)
	disabled.DisableConditionalWrites = true
	state, reserved, err = ReserveRecoveryAttempt(disabled, disabledID, recoveryTestScope)
	if reserved || !errors.Is(err, beads.ErrConditionalWriteUnsupported) {
		t.Fatalf("disabled CAS reservation = (%+v, %v, %v), want conditional-write refusal", state, reserved, err)
	}
	_, raw = readStoredRecoveryState(t, disabled, disabledID)
	if raw != "" {
		t.Fatalf("disabled store wrote recovery metadata: %s", raw)
	}
}

func TestMalformedRecoveryStateFailsClosed(t *testing.T) {
	store := beads.NewMemStore()
	beadID := recoveryWorkItem(t, store)
	if swapped, err := store.CompareAndSetMetadataKey(beadID, beadmeta.LifecycleRecoveryStateMetadataKey, "", `{"version":1,"work_item_id":"wrong-item","attempts":[]}`); err != nil || !swapped {
		t.Fatalf("seed malformed state = (%v, %v)", swapped, err)
	}
	state, reserved, err := ReserveRecoveryAttempt(store, beadID, recoveryTestScope)
	if reserved || !errors.Is(err, ErrRecoveryStateInvalid) {
		t.Fatalf("reservation from mismatched state = (%+v, %v, %v), want fail-closed invalid-state error", state, reserved, err)
	}
}

func TestRecoveryStateRejectsAnotherCanonicalStoreScope(t *testing.T) {
	store := beads.NewMemStore()
	beadID := recoveryWorkItem(t, store)
	if _, reserved, err := ReserveRecoveryAttempt(store, beadID, recoveryTestScope); err != nil || !reserved {
		t.Fatalf("initial reservation = (%v, %v), want reserved", reserved, err)
	}
	_, before := readStoredRecoveryState(t, store, beadID)
	otherScope := ScopeForStore("another-city", "rig:worker")
	state, reserved, err := ReserveRecoveryAttempt(store, beadID, otherScope)
	if reserved || !errors.Is(err, ErrRecoveryScopeMismatch) {
		t.Fatalf("cross-scope reservation = (%+v, %v, %v), want scope mismatch", state, reserved, err)
	}
	state, requested, err := RequestRecoveryEscalation(store, beadID, otherScope, "human-ops")
	if requested || !errors.Is(err, ErrRecoveryScopeMismatch) {
		t.Fatalf("cross-scope escalation = (%+v, %v, %v), want scope mismatch", state, requested, err)
	}
	_, after := readStoredRecoveryState(t, store, beadID)
	if before != after {
		t.Fatalf("cross-scope call changed metadata: before=%s after=%s", before, after)
	}
}

func TestRecoveryStateReadDoesNotChangeBackingRow(t *testing.T) {
	store := beads.NewMemStore()
	beadID := recoveryWorkItem(t, store)
	if _, reserved, err := ReserveRecoveryAttempt(store, beadID, recoveryTestScope); err != nil || !reserved {
		t.Fatalf("ReserveRecoveryAttempt: reserved=%v err=%v", reserved, err)
	}
	before, rawBefore := readStoredRecoveryState(t, store, beadID)
	state, requested, err := RequestRecoveryEscalation(store, beadID, recoveryTestScope, "human-ops")
	if err != nil || requested || !reflect.DeepEqual(state, before) {
		t.Fatalf("read-only escalation state = (%+v, %v, %v), want unchanged %+v", state, requested, err, before)
	}
	_, rawAfter := readStoredRecoveryState(t, store, beadID)
	if rawBefore != rawAfter {
		t.Fatalf("read-only call changed metadata: before=%s after=%s", rawBefore, rawAfter)
	}
}

type reservationResult struct {
	state    RecoveryState
	reserved bool
	err      error
}

type escalationResult struct {
	state          RecoveryState
	newlyRequested bool
	err            error
}

// Embedding Store as an interface prevents optional CAS methods on the backing
// concrete store from being promoted through this wrapper.
type recoveryNoCASStore struct{ beads.Store }

type ambiguousRecoveryCASStore struct {
	beads.Store
	writer beads.MetadataCASWriter
	err    error
	once   sync.Once
}

func (s *ambiguousRecoveryCASStore) CompareAndSetMetadataKey(id, key, expected, next string) (bool, error) {
	swapped, err := s.writer.CompareAndSetMetadataKey(id, key, expected, next)
	if err != nil || !swapped {
		return swapped, err
	}
	var result error
	s.once.Do(func() { result = s.err })
	if result != nil {
		return false, result
	}
	return swapped, nil
}
