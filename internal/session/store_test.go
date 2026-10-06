package session

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
)

// recordingStore seeds a session bead into a recording-fake store and
// returns the Store front door plus the recorder, so a test can assert the
// typed write method emitted byte-identical bead writes.
func recordingStore(t *testing.T, b beads.Bead) (*Store, *beadstest.RecordingStore) {
	t.Helper()
	mem := beads.NewMemStoreFrom(1, []beads.Bead{b}, nil)
	rec := beadstest.NewRecordingStore(mem)
	return NewStore(beads.SessionStore{Store: rec}), rec
}

// TestApplyPatchByteIdenticalToSetMetaBatch proves ApplyPatch emits exactly one
// SetMetadataBatch with the patch verbatim — the byte-identical replacement for
// setMetaBatch(store, id, patch).
func TestApplyPatchByteIdenticalToSetMetaBatch(t *testing.T) {
	b := sessionBeadFixture("s-1", "open", map[string]string{"state": "active"})
	is, rec := recordingStore(t, b)

	patch := MetadataPatch{"state": "asleep", "last_woke_at": "", "sleep_reason": "max-age"}
	if err := is.ApplyPatch("s-1", patch); err != nil {
		t.Fatalf("ApplyPatch: %v", err)
	}

	calls := rec.CallsForOp("SetMetadataBatch")
	if len(calls) != 1 {
		t.Fatalf("want 1 SetMetadataBatch, got %d", len(calls))
	}
	if calls[0].ID != "s-1" {
		t.Errorf("target id = %q, want s-1", calls[0].ID)
	}
	want := map[string]string{"state": "asleep", "last_woke_at": "", "sleep_reason": "max-age"}
	if !reflect.DeepEqual(calls[0].Metadata, want) {
		t.Errorf("batch = %#v, want %#v", calls[0].Metadata, want)
	}
}

func TestBackendIdentityPatchIsRevisionFenced(t *testing.T) {
	store := beads.NewMemStore()
	front := NewStore(beads.SessionStore{Store: store})
	info, err := front.CreateSessionInfo(CreateSpec{Title: "worker", AgentName: "worker", Metadata: map[string]string{"generation": "1", "instance_token": "original"}})
	if err != nil {
		t.Fatal(err)
	}
	before, err := store.Get(info.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := front.ApplyBackendIdentityPatchIfMatch(info.ID, before.Revision, MetadataPatch{"generation": "2", "instance_token": "replacement"}); err != nil {
		t.Fatal(err)
	}
	if err := front.ApplyBackendIdentityPatchIfMatch(info.ID, before.Revision, MetadataPatch{"generation": "3"}); !beads.IsPreconditionFailed(err) {
		t.Fatalf("stale internal identity patch was accepted: %v", err)
	}
	current, err := store.Get(info.ID)
	if err != nil || current.Metadata["generation"] != "2" || current.Metadata["instance_token"] != "replacement" {
		t.Fatalf("stale internal identity patch changed the session: %+v, %v", current, err)
	}
}

// TestApplyPatchInfoPersistsAndFoldsEqualsReprojection proves ApplyPatchInfo
// persists the patch byte-identically (one SetMetadataBatch) AND returns the
// LOCAL fold — never a re-Get — and that the folded Info equals a full
// reprojection of the patched bead. This is the write-returns-Info contract the
// reconciler cuts over to in WI-5 W1.
func TestApplyPatchInfoPersistsAndFoldsEqualsReprojection(t *testing.T) {
	b := sessionBeadFixture("s-1", "open", map[string]string{
		"state":                "creating",
		"pending_create_claim": "true",
		"last_woke_at":         "2026-01-01T00:00:00Z",
	})
	is, rec := recordingStore(t, b)

	pre, err := is.Get("s-1")
	if err != nil {
		t.Fatalf("Get (pre): %v", err)
	}

	patch := MetadataPatch{"state": "asleep", "pending_create_claim": "", "last_woke_at": ""}
	got, err := is.ApplyPatchInfo(pre, patch)
	if err != nil {
		t.Fatalf("ApplyPatchInfo: %v", err)
	}

	// The persist must be a single byte-identical SetMetadataBatch.
	calls := rec.CallsForOp("SetMetadataBatch")
	if len(calls) != 1 {
		t.Fatalf("want 1 SetMetadataBatch, got %d", len(calls))
	}
	if calls[0].ID != "s-1" || !reflect.DeepEqual(calls[0].Metadata, map[string]string(patch)) {
		t.Errorf("persist = (%q, %#v), want (s-1, %#v)", calls[0].ID, calls[0].Metadata, map[string]string(patch))
	}

	// The returned Info is the local fold pre.ApplyPatch(patch)...
	if want := pre.ApplyPatch(patch); !reflect.DeepEqual(got, want) {
		t.Errorf("ApplyPatchInfo fold diverged from pre.ApplyPatch\n got=%+v\nwant=%+v", got, want)
	}
	// ...which is byte-identical to a full reprojection of the patched bead.
	if want := infoFromPersistedBead(reprojectBead(b, patch)); !reflect.DeepEqual(got, want) {
		t.Errorf("ApplyPatchInfo fold diverged from full reprojection\n got=%+v\nwant=%+v", got, want)
	}
}

// TestApplyPatchInfoEmptyIsNoOp proves an empty patch persists nothing and
// returns the input Info unchanged (matching ApplyPatch's len==0 short-circuit).
func TestApplyPatchInfoEmptyIsNoOp(t *testing.T) {
	b := sessionBeadFixture("s-1", "open", map[string]string{"state": "active"})
	is, rec := recordingStore(t, b)

	pre, err := is.Get("s-1")
	if err != nil {
		t.Fatalf("Get (pre): %v", err)
	}
	got, err := is.ApplyPatchInfo(pre, MetadataPatch{})
	if err != nil {
		t.Fatalf("ApplyPatchInfo: %v", err)
	}
	if !reflect.DeepEqual(got, pre) {
		t.Errorf("empty patch changed Info\n got=%+v\nwant=%+v", got, pre)
	}
	if n := len(rec.Calls()); n != 0 {
		t.Errorf("empty patch emitted %d calls, want 0", n)
	}
}

// TestApplyPatchInfoWriteErrorReturnsInputUnchanged proves that when the persist
// fails, ApplyPatchInfo returns the INPUT Info unchanged (no fold) plus the
// error — so a caller that ignores the error keeps a snapshot consistent with
// the store, and a caller that checks it can bail.
func TestApplyPatchInfoWriteErrorReturnsInputUnchanged(t *testing.T) {
	// A store with no such bead: SetMetadataBatch on a missing id errors.
	is := NewStore(seedSessionStore(t))
	pre := infoFromPersistedBead(sessionBeadFixture("missing", "open", map[string]string{"state": "active"}))

	got, err := is.ApplyPatchInfo(pre, MetadataPatch{"state": "asleep"})
	if err == nil {
		t.Fatal("ApplyPatchInfo(missing): want store error, got nil")
	}
	if !reflect.DeepEqual(got, pre) {
		t.Errorf("write error must return the input Info unchanged\n got=%+v\nwant=%+v", got, pre)
	}
}

// TestApplyPatchEmptyIsNoOp proves an empty patch emits no write (matching
// setMetaBatch's len==0 short-circuit).
func TestApplyPatchEmptyIsNoOp(t *testing.T) {
	b := sessionBeadFixture("s-1", "open", nil)
	is, rec := recordingStore(t, b)

	if err := is.ApplyPatch("s-1", MetadataPatch{}); err != nil {
		t.Fatalf("ApplyPatch: %v", err)
	}
	if got := len(rec.Calls()); got != 0 {
		t.Errorf("empty patch emitted %d calls, want 0", got)
	}
}

// TestGetReflectsApplyPatch proves the store-authoritative refresh guarantee the
// session reconciler cuts over to in front-door Step 6: after a mutation
// persisted through ApplyPatch, a re-Get returns an Info reflecting that
// mutation. During the lockstep-coexistence phase (Steps 3-5) refreshSessionInfo
// refreshes the snapshot from the raw working copy instead (byte-identical by
// construction, and preserving the reconciler's deliberate intra-tick raw/store
// divergences like the reset_committed_at hiding); Step 6 removes the raw working
// set and makes Get the sole source. This test pins that Get sees a persisted
// write — the guarantee that Step-6 cutover depends on.
func TestGetReflectsApplyPatch(t *testing.T) {
	// A creating, still-claimed pending-create session — the shape whose lease
	// fields (state / pending_create_claim / last_woke_at) a heal-with-rollback
	// flips, and whose staleness would flip pendingCreateSessionStillLeasedInfo
	// in the reconciler's post-heal switch.
	b := sessionBeadFixture("s-1", "open", map[string]string{
		"state":                "creating",
		"pending_create_claim": "true",
		"last_woke_at":         "2026-01-01T00:00:00Z",
	})
	is, _ := recordingStore(t, b)

	pre, err := is.Get("s-1")
	if err != nil {
		t.Fatalf("Get (pre): %v", err)
	}
	if pre.MetadataState != "creating" || !pre.PendingCreateClaim {
		t.Fatalf("pre Get = state %q claim %v, want creating/true", pre.MetadataState, pre.PendingCreateClaim)
	}

	// Persist a heal-shaped rollback: clear the claim and drop the lease markers.
	if err := is.ApplyPatch("s-1", MetadataPatch{
		"state":                "asleep",
		"pending_create_claim": "",
		"last_woke_at":         "",
	}); err != nil {
		t.Fatalf("ApplyPatch: %v", err)
	}

	// The re-Get must reflect the persisted write — the refresh guarantee.
	post, err := is.Get("s-1")
	if err != nil {
		t.Fatalf("Get (post): %v", err)
	}
	if post.MetadataState != "asleep" {
		t.Errorf("post MetadataState = %q, want asleep", post.MetadataState)
	}
	if post.PendingCreateClaim {
		t.Errorf("post PendingCreateClaim = true, want false (cleared)")
	}
	if post.LastWokeAt != "" {
		t.Errorf("post LastWokeAt = %q, want empty (cleared)", post.LastWokeAt)
	}
}

// TestSleepEmitsSleepPatch proves the typed Sleep method emits exactly the bead
// write that SleepPatch produces — the same write the reconciler raw op did.
func TestSleepEmitsSleepPatch(t *testing.T) {
	b := sessionBeadFixture("s-1", "open", map[string]string{"state": "active"})
	is, rec := recordingStore(t, b)

	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	if err := is.Sleep("s-1", "idle-timeout", now); err != nil {
		t.Fatalf("Sleep: %v", err)
	}
	calls := rec.CallsForOp("SetMetadataBatch")
	if len(calls) != 1 {
		t.Fatalf("want 1 SetMetadataBatch, got %d", len(calls))
	}
	want := map[string]string(SleepPatch(now, "idle-timeout"))
	if !reflect.DeepEqual(calls[0].Metadata, want) {
		t.Errorf("Sleep batch = %#v, want %#v", calls[0].Metadata, want)
	}
}

// TestSetWaitHoldClearWritesEmptyStrings proves clearing wait-hold emits the
// empty-string writes the raw cmd_wait.go clear did.
func TestSetWaitHoldClearWritesEmptyStrings(t *testing.T) {
	b := sessionBeadFixture("s-1", "open", map[string]string{"wait_hold": "x", "sleep_intent": "x"})
	is, rec := recordingStore(t, b)

	if err := is.SetWaitHold("s-1", false, ""); err != nil {
		t.Fatalf("SetWaitHold: %v", err)
	}
	calls := rec.CallsForOp("SetMetadataBatch")
	if len(calls) != 1 {
		t.Fatalf("want 1 SetMetadataBatch, got %d", len(calls))
	}
	want := map[string]string{"wait_hold": "", "sleep_intent": ""}
	if !reflect.DeepEqual(calls[0].Metadata, want) {
		t.Errorf("clear batch = %#v, want %#v", calls[0].Metadata, want)
	}
}

// TestCloseEmitsClosePatchThenClose proves Close stamps ClosePatch metadata and
// then closes the bead — the byte-identical replacement for closeBead's
// SetMetadataBatch(ClosePatch)+Close, WITHOUT any work-reassignment side effect
// (that is Phase 6).
func TestCloseEmitsClosePatchThenClose(t *testing.T) {
	b := sessionBeadFixture("s-1", "open", map[string]string{"state": "active"})
	is, rec := recordingStore(t, b)

	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	closed, err := is.Close("s-1", "gc_swept", now)
	if err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !closed {
		t.Fatalf("Close reported not-closed for an open bead")
	}

	gotOps := opsOf(rec.Calls())
	wantOps := []string{"SetMetadataBatch", "Close"}
	if !reflect.DeepEqual(gotOps, wantOps) {
		t.Fatalf("Close ops = %v, want %v", gotOps, wantOps)
	}
	want := map[string]string(ClosePatch(now, "gc_swept"))
	if !reflect.DeepEqual(rec.CallsForOp("SetMetadataBatch")[0].Metadata, want) {
		t.Errorf("close patch = %#v, want %#v", rec.CallsForOp("SetMetadataBatch")[0].Metadata, want)
	}
}

// TestCloseAlreadyClosedIsNoOp proves Close on a closed bead emits no writes.
func TestCloseAlreadyClosedIsNoOp(t *testing.T) {
	b := sessionBeadFixture("s-1", "closed", nil)
	is, rec := recordingStore(t, b)

	closed, err := is.Close("s-1", "gc_swept", time.Now())
	if err != nil {
		t.Fatalf("Close: %v", err)
	}
	if closed {
		t.Errorf("Close reported closed for an already-closed bead")
	}
	if got := len(rec.Calls()); got != 0 {
		t.Errorf("Close on closed bead emitted %d writes, want 0", got)
	}
}

// TestGetStateProjectsState proves GetState returns the persisted state/closed
// without raw beads crossing the boundary.
func TestGetStateProjectsState(t *testing.T) {
	b := sessionBeadFixture("s-1", "open", map[string]string{"state": "asleep"})
	is, _ := recordingStore(t, b)

	state, closed, err := is.GetState("s-1")
	if err != nil {
		t.Fatalf("GetState: %v", err)
	}
	if state != StateAsleep || closed {
		t.Errorf("GetState = (%q, %v), want (asleep, false)", state, closed)
	}
}

// TestSetMarkerEmitsSingleKeySetMetadata proves SetMarker emits exactly one
// single-key SetMetadata op — byte-identical to the raw store.SetMetadata
// single-key write it replaces (stranded marker, sleep_intent clear, cmd_stop
// sleep_reason), NOT a SetMetadataBatch.
func TestSetMarkerEmitsSingleKeySetMetadata(t *testing.T) {
	b := sessionBeadFixture("s-1", "open", map[string]string{"state": "active"})
	is, rec := recordingStore(t, b)

	if err := is.SetMarker("s-1", "sleep_reason", "city-stop"); err != nil {
		t.Fatalf("SetMarker: %v", err)
	}
	gotOps := opsOf(rec.Calls())
	if !reflect.DeepEqual(gotOps, []string{"SetMetadata"}) {
		t.Fatalf("SetMarker ops = %v, want [SetMetadata]", gotOps)
	}
	c := rec.CallsForOp("SetMetadata")[0]
	if c.ID != "s-1" || c.Key != "sleep_reason" || c.Value != "city-stop" {
		t.Errorf("SetMarker call = (%q,%q,%q), want (s-1,sleep_reason,city-stop)", c.ID, c.Key, c.Value)
	}
}

// TestSetMarkerEmptyValueClears proves SetMarker writes an empty string verbatim
// (the empty-string-clear contract) via a single SetMetadata op.
func TestSetMarkerEmptyValueClears(t *testing.T) {
	b := sessionBeadFixture("s-1", "open", map[string]string{"sleep_intent": "idle-stop-pending"})
	is, rec := recordingStore(t, b)

	if err := is.SetMarker("s-1", "sleep_intent", ""); err != nil {
		t.Fatalf("SetMarker: %v", err)
	}
	c := rec.CallsForOp("SetMetadata")
	if len(c) != 1 || c[0].Key != "sleep_intent" || c[0].Value != "" {
		t.Fatalf("SetMarker clear = %#v, want one SetMetadata(sleep_intent,\"\")", c)
	}
}

// TestRecordCurrentBeadEmitsSingleKeySetMetadata proves RecordCurrentBead emits
// a single-key SetMetadata of CurrentBeadIDKey — byte-identical to
// recordCurrentBeadIDOnWake's raw store.SetMetadata write (NOT a batch).
func TestRecordCurrentBeadEmitsSingleKeySetMetadata(t *testing.T) {
	b := sessionBeadFixture("s-1", "open", nil)
	is, rec := recordingStore(t, b)

	if err := is.RecordCurrentBead("s-1", "gcg-42"); err != nil {
		t.Fatalf("RecordCurrentBead: %v", err)
	}
	gotOps := opsOf(rec.Calls())
	if !reflect.DeepEqual(gotOps, []string{"SetMetadata"}) {
		t.Fatalf("RecordCurrentBead ops = %v, want [SetMetadata]", gotOps)
	}
	c := rec.CallsForOp("SetMetadata")[0]
	if c.ID != "s-1" || c.Key != CurrentBeadIDKey || c.Value != "gcg-42" {
		t.Errorf("RecordCurrentBead call = (%q,%q,%q), want (s-1,%q,gcg-42)", c.ID, c.Key, c.Value, CurrentBeadIDKey)
	}
}

// TestSetCurrentClaimWritesTheReciprocalFence proves the claim id and epoch are
// written together, so request readers cannot observe mismatched claim facts.
func TestSetCurrentClaimWritesTheReciprocalFence(t *testing.T) {
	b := sessionBeadFixture("s-1", "open", nil)
	is, rec := recordingStore(t, b)

	wrote, err := is.SetCurrentClaim("s-1", "gcg-42")
	if err != nil {
		t.Fatalf("SetCurrentClaim: %v", err)
	}
	if !wrote {
		t.Fatal("SetCurrentClaim reported no write for a fresh stamp")
	}
	c := rec.CallsForOp("SetMetadataBatch")
	if len(c) != 1 {
		t.Fatalf("SetMetadataBatch calls = %d, want 1 (ops=%v)", len(c), opsOf(rec.Calls()))
	}
	want := map[string]string{
		beadmeta.CurrentClaimBeadIDMetadataKey:     "gcg-42",
		beadmeta.CurrentClaimGenerationMetadataKey: "",
	}
	if c[0].ID != "s-1" || !reflect.DeepEqual(c[0].Metadata, want) {
		t.Errorf("SetCurrentClaim batch = (%q,%#v), want (s-1,%#v)", c[0].ID, c[0].Metadata, want)
	}
	if c[0].Metadata[CurrentBeadIDKey] != "" {
		t.Errorf("SetCurrentClaim wrote the reconciler's %q key", CurrentBeadIDKey)
	}
}

// TestSetCurrentClaimSkipsWhenUnchanged pins the idempotence guard: the claim
// path re-stamps on every hook tick through its adoption branches, so an
// already-current value must issue no write at all.
func TestSetCurrentClaimSkipsWhenUnchanged(t *testing.T) {
	b := sessionBeadFixture("s-1", "open", map[string]string{
		beadmeta.CurrentClaimBeadIDMetadataKey:     "gcg-42",
		beadmeta.CurrentClaimGenerationMetadataKey: "",
	})
	is, rec := recordingStore(t, b)

	wrote, err := is.SetCurrentClaim("s-1", "gcg-42")
	if err != nil {
		t.Fatalf("SetCurrentClaim: %v", err)
	}
	if wrote {
		t.Error("SetCurrentClaim reported a write for an unchanged value")
	}
	if n := len(rec.CallsForOp("SetMetadataBatch")); n != 0 {
		t.Errorf("unchanged stamp emitted %d writes, want 0", n)
	}
}

func TestSetCurrentClaimWritesWhenOnlyTheGenerationChanges(t *testing.T) {
	b := sessionBeadFixture("s-1", "open", map[string]string{
		beadmeta.CurrentClaimBeadIDMetadataKey:     "gcg-42",
		beadmeta.CurrentClaimGenerationMetadataKey: "claim-1",
	})
	is, rec := recordingStore(t, b)

	wrote, err := is.SetCurrentClaimForGeneration("s-1", "gcg-42", "claim-2")
	if err != nil || !wrote {
		t.Fatalf("same-bead generation renewal wrote=%v err=%v, want a write", wrote, err)
	}
	calls := rec.CallsForOp("SetMetadataBatch")
	if len(calls) != 1 || calls[0].Metadata[beadmeta.CurrentClaimBeadIDMetadataKey] != "gcg-42" ||
		calls[0].Metadata[beadmeta.CurrentClaimGenerationMetadataKey] != "claim-2" {
		t.Fatalf("renewal batch = %#v, want same id with claim-2", calls)
	}
}

// TestSetCurrentClaimClearsWithEmptyValue proves the release side of the stamp:
// clearing empties both reciprocal fields so a session no longer names a bead
// it cannot close or a generation it no longer owns.
func TestSetCurrentClaimClearsWithEmptyValue(t *testing.T) {
	b := sessionBeadFixture("s-1", "open", map[string]string{
		beadmeta.CurrentClaimBeadIDMetadataKey:     "gcg-42",
		beadmeta.CurrentClaimGenerationMetadataKey: "claim-1",
	})
	is, rec := recordingStore(t, b)

	if _, err := is.SetCurrentClaim("s-1", ""); err != nil {
		t.Fatalf("SetCurrentClaim clear: %v", err)
	}
	c := rec.CallsForOp("SetMetadataBatch")
	if len(c) != 1 || c[0].Metadata[beadmeta.CurrentClaimBeadIDMetadataKey] != "" ||
		c[0].Metadata[beadmeta.CurrentClaimGenerationMetadataKey] != "" {
		t.Fatalf("clear = %#v, want both claim fields cleared", c)
	}
	got, err := is.CurrentClaimBeadID("s-1")
	if err != nil {
		t.Fatalf("CurrentClaimBeadID: %v", err)
	}
	if got != "" {
		t.Errorf("CurrentClaimBeadID after clear = %q, want empty", got)
	}
}

// TestCurrentClaimBeadIDReadsTheStampedValue proves the read half of the
// back-channel returns the stamped id verbatim, and empty when nothing is
// stamped.
func TestCurrentClaimBeadIDReadsTheStampedValue(t *testing.T) {
	is, _ := recordingStore(t, sessionBeadFixture("s-1", "open",
		map[string]string{beadmeta.CurrentClaimBeadIDMetadataKey: "gcg-42"}))
	got, err := is.CurrentClaimBeadID("s-1")
	if err != nil {
		t.Fatalf("CurrentClaimBeadID: %v", err)
	}
	if got != "gcg-42" {
		t.Errorf("CurrentClaimBeadID = %q, want gcg-42", got)
	}

	bare, _ := recordingStore(t, sessionBeadFixture("s-2", "open", nil))
	got, err = bare.CurrentClaimBeadID("s-2")
	if err != nil {
		t.Fatalf("CurrentClaimBeadID (unstamped): %v", err)
	}
	if got != "" {
		t.Errorf("CurrentClaimBeadID (unstamped) = %q, want empty", got)
	}
}

// TestCurrentClaimRefusesANonSessionBead proves both halves route through
// validatedBead: a present-but-non-session bead is refused with
// ErrSessionNotFound BEFORE any write, so bd's fuzzy id resolver cannot redirect
// the claim stamp onto a foreign bead.
func TestCurrentClaimRefusesANonSessionBead(t *testing.T) {
	mem := beads.NewMemStoreFrom(1, []beads.Bead{{ID: "wb-1", Status: "open", Title: "work"}}, nil)
	rec := beadstest.NewRecordingStore(mem)
	is := NewStore(beads.SessionStore{Store: rec})

	if _, err := is.SetCurrentClaim("wb-1", "gcg-42"); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("SetCurrentClaim on a non-session bead = %v, want ErrSessionNotFound", err)
	}
	if n := len(rec.CallsForOp("SetMetadata")); n != 0 {
		t.Errorf("refused stamp still wrote %d times", n)
	}
	if _, err := is.CurrentClaimBeadID("wb-1"); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("CurrentClaimBeadID on a non-session bead = %v, want ErrSessionNotFound", err)
	}
}

// TestCloseWithoutReasonEmitsSingleClose proves CloseWithoutReason emits exactly
// one Close op and no metadata write — byte-identical to closeBead's raw
// store.Close(id) after it stamps ClosePatch separately.
func TestCloseWithoutReasonEmitsSingleClose(t *testing.T) {
	b := sessionBeadFixture("s-1", "open", map[string]string{"state": "active"})
	is, rec := recordingStore(t, b)

	if err := is.CloseWithoutReason("s-1"); err != nil {
		t.Fatalf("CloseWithoutReason: %v", err)
	}
	gotOps := opsOf(rec.Calls())
	if !reflect.DeepEqual(gotOps, []string{"Close"}) {
		t.Fatalf("CloseWithoutReason ops = %v, want [Close]", gotOps)
	}
	if rec.CallsForOp("Close")[0].ID != "s-1" {
		t.Errorf("Close target = %q, want s-1", rec.CallsForOp("Close")[0].ID)
	}
}

// TestSetStatusOpenEmitsStatusOnlyUpdate proves SetStatusOpen conditionally
// reopens the exact inspected row.
func TestSetStatusOpenEmitsStatusOnlyUpdate(t *testing.T) {
	b := sessionBeadFixture("s-1", "closed", map[string]string{"state": "archived"})
	mem := &beads.MemStore{HonorExplicitIDs: true}
	created, err := mem.Create(b)
	if err != nil {
		t.Fatal(err)
	}
	if err := mem.Close(created.ID); err != nil {
		t.Fatal(err)
	}
	created, err = mem.Get(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	is := NewStore(beads.SessionStore{Store: mem})

	if err := is.SetStatusOpen("s-1"); err != nil {
		t.Fatalf("SetStatusOpen: %v", err)
	}
	got, err := mem.Get("s-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "open" || got.Revision == created.Revision {
		t.Fatalf("SetStatusOpen result = %+v, want open with advanced revision", got)
	}
}

func TestSetStatusOpenRejectsRequestPurgeFence(t *testing.T) {
	b := sessionBeadFixture("s-1", "closed", map[string]string{
		"state": "archived", beadmeta.SessionRequestPurgeFenceMetadataKey: "purge-owner",
	})
	mem := &beads.MemStore{HonorExplicitIDs: true}
	created, err := mem.Create(b)
	if err != nil {
		t.Fatal(err)
	}
	if err := mem.Close(created.ID); err != nil {
		t.Fatal(err)
	}
	is := NewStore(beads.SessionStore{Store: mem})
	if err := is.SetStatusOpen("s-1"); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("SetStatusOpen = %v, want request conflict", err)
	}
	got, err := mem.Get("s-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "closed" || !IsRequestPurgeFenced(got) {
		t.Fatalf("fenced row changed: %+v", got)
	}
}

func TestRepairTypeUsesInspectedRevision(t *testing.T) {
	b := sessionBeadFixture("s-1", "open", nil)
	b.Type = ""
	mem := beads.NewMemStoreFrom(1, []beads.Bead{b}, nil)
	is := NewStore(beads.SessionStore{Store: mem})

	if err := is.RepairType("s-1"); err != nil {
		t.Fatalf("RepairType: %v", err)
	}
	got, err := mem.Get("s-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Type != BeadType || got.Revision == b.Revision {
		t.Fatalf("RepairType result = %+v, want canonical type and advanced revision", got)
	}
}

func TestRepairTypeRejectsRequestPurgeFence(t *testing.T) {
	b := sessionBeadFixture("s-1", "closed", map[string]string{
		beadmeta.SessionRequestPurgeFenceMetadataKey: "purge-owner",
	})
	b.Type = ""
	mem := beads.NewMemStoreFrom(1, []beads.Bead{b}, nil)
	is := NewStore(beads.SessionStore{Store: mem})

	if err := is.RepairType("s-1"); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("RepairType = %v, want request conflict", err)
	}
	got, err := mem.Get("s-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Type != "" || RequestPurgeFence(got) != "purge-owner" {
		t.Fatalf("fenced repair changed row: %+v", got)
	}
}

func TestRepairTypeRejectsRowThatIsNoLongerRepairable(t *testing.T) {
	b := sessionBeadFixture("s-1", "open", nil)
	b.Type = "task"
	mem := beads.NewMemStoreFrom(1, []beads.Bead{b}, nil)

	if err := NewStore(beads.SessionStore{Store: mem}).RepairType(b.ID); !errors.Is(err, ErrRequestConflict) {
		t.Fatalf("RepairType = %v, want request conflict", err)
	}
	got, err := mem.Get(b.ID)
	if err != nil || got.Type != "task" {
		t.Fatalf("legitimate type was overwritten: %+v, %v", got, err)
	}
}

type repairTypeRaceStore struct {
	*beads.MemStore
	beforeUpdate func()
}

func (s *repairTypeRaceStore) UpdateIfMatch(id string, revision int64, opts beads.UpdateOpts) error {
	if s.beforeUpdate != nil {
		before := s.beforeUpdate
		s.beforeUpdate = nil
		before()
	}
	return s.MemStore.UpdateIfMatch(id, revision, opts)
}

func TestRepairTypeDoesNotOverwriteConcurrentPurgeFence(t *testing.T) {
	b := sessionBeadFixture("s-1", "closed", nil)
	b.Type = ""
	mem := beads.NewMemStoreFrom(1, []beads.Bead{b}, nil)
	race := &repairTypeRaceStore{MemStore: mem}
	race.beforeUpdate = func() {
		if ok, err := mem.CompareAndSetMetadataKey("s-1", beadmeta.SessionRequestPurgeFenceMetadataKey, "", "purge-owner"); err != nil || !ok {
			t.Fatalf("install concurrent purge fence = (%v, %v)", ok, err)
		}
	}
	is := NewStore(beads.SessionStore{Store: race})

	if err := is.RepairType("s-1"); err == nil {
		t.Fatal("RepairType succeeded across concurrent purge fence")
	}
	got, err := mem.Get("s-1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Type != "" || RequestPurgeFence(got) != "purge-owner" {
		t.Fatalf("concurrent fence/type result = %+v", got)
	}
}

func TestRepairTypeDoesNotOverwriteConcurrentLegitimateType(t *testing.T) {
	b := sessionBeadFixture("s-1", "open", nil)
	b.Type = ""
	mem := beads.NewMemStoreFrom(1, []beads.Bead{b}, nil)
	race := &repairTypeRaceStore{MemStore: mem}
	race.beforeUpdate = func() {
		legitimate := "task"
		if err := mem.Update("s-1", beads.UpdateOpts{Type: &legitimate}); err != nil {
			t.Fatalf("install concurrent type = %v", err)
		}
	}

	if err := NewStore(beads.SessionStore{Store: race}).RepairType(b.ID); err == nil {
		t.Fatal("RepairType succeeded across concurrent legitimate type change")
	}
	got, err := mem.Get(b.ID)
	if err != nil || got.Type != "task" {
		t.Fatalf("concurrent legitimate type was overwritten: %+v, %v", got, err)
	}
}

// TestCircuitResetGenerationReturnsPersistedValue proves the typed read returns
// the persisted reset-generation metadata value verbatim — equivalent to the raw
// store.Get(id) + read .Metadata[SessionCircuitResetGenerationMetadataKey] that
// loadPersistedSessionCircuitResetGeneration performed.
func TestCircuitResetGenerationReturnsPersistedValue(t *testing.T) {
	b := sessionBeadFixture("s-1", "open", map[string]string{
		SessionCircuitResetGenerationMetadataKey: "7",
	})
	is, rec := recordingStore(t, b)

	got, err := is.CircuitResetGeneration("s-1")
	if err != nil {
		t.Fatalf("CircuitResetGeneration: %v", err)
	}
	if want := b.Metadata[SessionCircuitResetGenerationMetadataKey]; got != want {
		t.Errorf("CircuitResetGeneration = %q, want %q (the raw read value)", got, want)
	}
	// The read confines a single Get; it must not emit any mutating bead op.
	if mutating := opsOf(rec.Calls()); len(mutating) != 0 {
		t.Errorf("CircuitResetGeneration emitted mutating ops %v, want none", mutating)
	}
}

// TestCircuitResetGenerationEmptyWhenUnset proves an unset key reads back as the
// empty string (not an error) — matching the raw map read on a bead that never
// stamped the generation.
func TestCircuitResetGenerationEmptyWhenUnset(t *testing.T) {
	b := sessionBeadFixture("s-1", "open", map[string]string{"state": "active"})
	is, _ := recordingStore(t, b)

	got, err := is.CircuitResetGeneration("s-1")
	if err != nil {
		t.Fatalf("CircuitResetGeneration: %v", err)
	}
	if got != "" {
		t.Errorf("CircuitResetGeneration on unset key = %q, want \"\"", got)
	}
}

// TestCircuitResetGenerationSurfacesStoreError proves a missing bead surfaces the
// bare store error (the caller owns its diagnostic wrapping), matching the raw
// store.Get error path the front door replaces.
func TestCircuitResetGenerationSurfacesStoreError(t *testing.T) {
	store := seedSessionStore(t)
	is := NewStore(store)
	if _, err := is.CircuitResetGeneration("missing"); err == nil {
		t.Fatal("CircuitResetGeneration(missing): want store error, got nil")
	}
}

// TestPersistedMarkersReturnsVerbatimValues proves the typed read returns the
// persisted session_name / continuation_epoch / sleep_reason metadata values
// verbatim — equivalent to the raw store.Get(id) + read .Metadata[...] the
// cmd_wait registration and retry/clear paths performed. It performs no bead
// validation (matching the raw reads) and emits no mutating op.
func TestPersistedMarkersReturnsVerbatimValues(t *testing.T) {
	b := sessionBeadFixture("s-1", "open", map[string]string{
		"__title":            "My Session",
		"session_name":       "polecat-1",
		"continuation_epoch": "4",
		"sleep_reason":       "wait-hold",
	})
	is, rec := recordingStore(t, b)

	got, err := is.PersistedMarkers("s-1")
	if err != nil {
		t.Fatalf("PersistedMarkers: %v", err)
	}
	want := PersistedMarkers{
		Title:             "My Session",
		SessionName:       "polecat-1",
		ContinuationEpoch: "4",
		SleepReason:       "wait-hold",
	}
	if got != want {
		t.Errorf("PersistedMarkers = %#v, want %#v (the raw read values)", got, want)
	}
	// The read confines a single Get; it must not emit any mutating bead op.
	if mutating := opsOf(rec.Calls()); len(mutating) != 0 {
		t.Errorf("PersistedMarkers emitted mutating ops %v, want none", mutating)
	}
}

// TestPersistedMarkersEmptyWhenUnset proves unset keys read back as empty
// strings (not an error), matching the raw map read on a bead that never
// stamped those markers.
func TestPersistedMarkersEmptyWhenUnset(t *testing.T) {
	b := sessionBeadFixture("s-1", "open", map[string]string{"state": "active"})
	is, _ := recordingStore(t, b)

	got, err := is.PersistedMarkers("s-1")
	if err != nil {
		t.Fatalf("PersistedMarkers: %v", err)
	}
	if got != (PersistedMarkers{}) {
		t.Errorf("PersistedMarkers on unset keys = %#v, want zero value", got)
	}
}

// TestPersistedMarkersSurfacesStoreError proves a missing bead surfaces the bare
// store error (the caller owns its diagnostic wrapping), matching the raw
// store.Get error path the front door replaces.
func TestPersistedMarkersSurfacesStoreError(t *testing.T) {
	store := seedSessionStore(t)
	is := NewStore(store)
	if _, err := is.PersistedMarkers("missing"); err == nil {
		t.Fatal("PersistedMarkers(missing): want store error, got nil")
	}
}

func opsOf(calls []beadstest.RecordedCall) []string {
	out := make([]string, 0, len(calls))
	for _, c := range calls {
		out = append(out, c.Op)
	}
	return out
}

// updateFailStore is a beads.Store whose Update always fails; every other op
// delegates to the embedded store. It proves UpdateMetadataInfo's all-or-nothing
// contract: a rejected Update must leave both the durable row and the caller's
// Info untouched.
type updateFailStore struct {
	beads.Store
	err error
}

func (s updateFailStore) Update(string, beads.UpdateOpts) error { return s.err }

// TestUpdateMetadataInfoEmitsSingleUpdateWithFullPatch pins the one-operation
// contract for the pool trigger/provenance cluster (council finding 1): the whole
// patch is written in exactly ONE Store.Update carrying the full metadata map —
// NOT decomposed into per-key SetMetadata / SetMetadataBatch ops, whose per-key
// decomposition on exec:/partial-write backends could commit a mixed provenance
// row. On success the returned Info equals the local fold pre.ApplyPatch(patch).
func TestUpdateMetadataInfoEmitsSingleUpdateWithFullPatch(t *testing.T) {
	b := sessionBeadFixture("s-1", "open", map[string]string{"state": "active"})
	is, rec := recordingStore(t, b)

	pre, err := is.Get("s-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	patch := MetadataPatch{
		beadmeta.TriggerBeadIDMetadataKey:       "gcg-123",
		beadmeta.TriggerBeadStoreRefMetadataKey: "rig-a",
		beadmeta.BrainParentSIDMetadataKey:      "sid-parent",
		beadmeta.PackMetadataKey:                "packs/x",
		beadmeta.PackWorkspaceMetadataKey:       "ws-1",
		beadmeta.WorkDirMetadataKey:             "/work/dir",
	}

	got, err := is.UpdateMetadataInfo(pre, patch)
	if err != nil {
		t.Fatalf("UpdateMetadataInfo: %v", err)
	}

	updates := rec.CallsForOp("Update")
	if len(updates) != 1 {
		t.Fatalf("want exactly 1 Update op, got %d (all ops: %v)", len(updates), opsOf(rec.Calls()))
	}
	if updates[0].ID != "s-1" {
		t.Errorf("Update target id = %q, want s-1", updates[0].ID)
	}
	if !reflect.DeepEqual(updates[0].Opts.Metadata, map[string]string(patch)) {
		t.Errorf("Update metadata = %#v, want the FULL patch %#v", updates[0].Opts.Metadata, map[string]string(patch))
	}
	// One-operation contract: no per-key decomposition.
	if n := len(rec.CallsForOp("SetMetadata")); n != 0 {
		t.Errorf("SetMetadata ops = %d, want 0 (one-Update contract)", n)
	}
	if n := len(rec.CallsForOp("SetMetadataBatch")); n != 0 {
		t.Errorf("SetMetadataBatch ops = %d, want 0 (one-Update contract)", n)
	}
	// Success folds the patch onto Info, byte-identical to a local ApplyPatch.
	if want := pre.ApplyPatch(patch); !reflect.DeepEqual(got, want) {
		t.Errorf("returned Info = %#v, want local fold %#v", got, want)
	}
	if got.TriggerBeadID != "gcg-123" || got.Pack != "packs/x" || got.WorkDirCanonical != "/work/dir" {
		t.Errorf("returned Info did not fold the trigger cluster: %+v", got)
	}
}

// TestUpdateMetadataInfoFailedWritePersistsNothingAndReturnsInputUnchanged proves
// the all-or-nothing guarantee: when the single Update fails, NOTHING is persisted
// (the durable row keeps its pre-write metadata) and the returned Info is the
// INPUT unchanged, so a log-and-continue caller never advances onto a half-applied
// provenance cluster (council finding 1).
func TestUpdateMetadataInfoFailedWritePersistsNothingAndReturnsInputUnchanged(t *testing.T) {
	b := sessionBeadFixture("s-1", "open", map[string]string{"state": "active"})
	mem := beads.NewMemStoreFrom(1, []beads.Bead{b}, nil)
	is := NewStore(beads.SessionStore{Store: updateFailStore{Store: mem, err: errors.New("update rejected")}})

	pre, err := is.Get("s-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	patch := MetadataPatch{
		beadmeta.TriggerBeadIDMetadataKey:       "gcg-123",
		beadmeta.TriggerBeadStoreRefMetadataKey: "rig-a",
		beadmeta.BrainParentSIDMetadataKey:      "sid-parent",
	}

	got, err := is.UpdateMetadataInfo(pre, patch)
	if err == nil {
		t.Fatal("UpdateMetadataInfo: want error on failed Update, got nil")
	}
	// Returned Info is the input UNCHANGED — no partial fold.
	if !reflect.DeepEqual(got, pre) {
		t.Errorf("returned Info = %#v, want INPUT unchanged %#v", got, pre)
	}
	// Nothing persisted: the durable row still has none of the cluster keys.
	after, err := mem.Get("s-1")
	if err != nil {
		t.Fatalf("Get after failed update: %v", err)
	}
	for k := range patch {
		if v := after.Metadata[k]; v != "" {
			t.Errorf("durable row key %q = %q after failed Update, want unset (all-or-nothing)", k, v)
		}
	}
}
