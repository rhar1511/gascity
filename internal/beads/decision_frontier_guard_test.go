package beads

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
)

type decisionFrontierStoreFixture struct {
	name  string
	store Store
	close func() error
}

func decisionFrontierStores(t *testing.T) []decisionFrontierStoreFixture {
	t.Helper()
	mem := explicitMemStore()
	sqlite := openDecisionFrontierSQLite(t)
	return []decisionFrontierStoreFixture{
		{name: "mem", store: mem, close: func() error { return nil }},
		{name: "sqlite", store: sqlite.store, close: sqlite.close},
	}
}

func TestDecisionFrontierWriterDiscoveryRequiresAnActiveWritableRole(t *testing.T) {
	assertWriterRoles := func(t *testing.T, store Store, wantWriter, wantReader, wantTransition bool) {
		t.Helper()
		_, gotWriter := DecisionFrontierRecordWriterFor(store)
		_, gotReader := DecisionFrontierSourceReaderFor(store)
		_, gotTransition := RevisionTransitionWriterFor(store)
		if gotWriter != wantWriter || gotReader != wantReader || gotTransition != wantTransition {
			t.Fatalf("frontier capabilities = writer:%t reader:%t transition:%t, want %t:%t:%t",
				gotWriter, gotReader, gotTransition, wantWriter, wantReader, wantTransition)
		}
	}

	t.Run("typed nil stores are unavailable", func(t *testing.T) {
		var nilMem *MemStore
		var nilSQLite *SQLiteStore
		assertWriterRoles(t, nilMem, false, false, false)
		assertWriterRoles(t, nilSQLite, false, false, false)
	})

	t.Run("disabled memory writes do not advertise controller writers", func(t *testing.T) {
		store := &MemStore{IDPrefix: "wrk", HonorExplicitIDs: true, DisableConditionalWrites: true}
		assertWriterRoles(t, store, false, true, false)
	})

	for _, tc := range []struct {
		name               string
		revision, readOnly bool
		wantWriter         bool
		wantReader         bool
		wantTransition     bool
	}{
		{name: "writable revisioned sqlite", revision: true, wantWriter: true, wantReader: true, wantTransition: true},
		{name: "revisionless sqlite", revision: false},
		{name: "read-only revisioned sqlite", revision: true, readOnly: true, wantReader: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			createSQLiteSchemaFixture(t, dir, tc.revision, false, nil)
			options := []SQLiteStoreOption{WithSQLiteStoreIDPrefix("gcg")}
			if tc.readOnly {
				options = append(options, WithSQLiteStoreReadOnly())
			}
			opened, err := OpenSQLiteStore(dir, options...)
			if err != nil {
				t.Fatalf("open SQLite fixture: %v", err)
			}
			store := opened.(*SQLiteStore)
			defer func() {
				if err := store.CloseStore(); err != nil {
					t.Errorf("close SQLite fixture: %v", err)
				}
			}()
			assertWriterRoles(t, store, tc.wantWriter, tc.wantReader, tc.wantTransition)
		})
	}
}

func TestDecisionFrontierReservedRowsAndHoldsAreProtectedOnSupportedStores(t *testing.T) {
	for _, tc := range decisionFrontierStores(t) {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if err := tc.close(); err != nil {
					t.Errorf("close decision-frontier test store: %v", err)
				}
			}()
			assertDecisionFrontierGuards(t, tc.store)
		})
	}
}

func TestDecisionFrontierLinksFollowImmutableRecordsAndResistGenericEdits(t *testing.T) {
	for _, tc := range decisionFrontierStores(t) {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if err := tc.close(); err != nil {
					t.Errorf("close decision-frontier test store: %v", err)
				}
			}()
			store := tc.store
			const cityRef = "city:link-test"
			const storeRef = "city:link-test"
			const workID = "work-link-test"
			const revision = "7"
			const workDigest = "source-digest"
			mapID := DecisionFrontierMapRecordID(cityRef, storeRef, workID, revision)
			promptID := DecisionFrontierPromptRecordID(cityRef, storeRef, mapID)
			questions := []decisionFrontierLinkQuestion{
				{ID: "first", Title: "First", Prompt: "Pick first."},
				{ID: "second", Title: "Second", Prompt: "Pick second.", DependsOn: []string{"first"}},
			}
			ticketIDs := []string{
				DecisionFrontierQuestionRecordID(cityRef, storeRef, mapID, questions[0].ID),
				DecisionFrontierQuestionRecordID(cityRef, storeRef, mapID, questions[1].ID),
			}
			identity := decisionFrontierLinkIdentity{
				SchemaVersion: 1, CityRef: cityRef, StoreRef: storeRef, WorkID: workID,
				WorkRevision: revision, WorkDigest: workDigest, MapID: mapID,
			}
			mapDoc := decisionFrontierLinkDocument{
				SchemaVersion: identity.SchemaVersion, CityRef: identity.CityRef, StoreRef: identity.StoreRef,
				WorkID: identity.WorkID, WorkRevision: identity.WorkRevision, WorkDigest: identity.WorkDigest,
				MapID: identity.MapID, PromptID: promptID, Questions: questions,
			}
			mapBead := createDecisionFrontierTestRecord(t, store, mapID, decisionFrontierMapKind, "pending", mapDoc)
			for i, question := range questions {
				doc := decisionFrontierLinkDocument{
					SchemaVersion: identity.SchemaVersion, CityRef: identity.CityRef, StoreRef: identity.StoreRef,
					WorkID: identity.WorkID, WorkRevision: identity.WorkRevision, WorkDigest: identity.WorkDigest,
					MapID: identity.MapID, Question: question,
				}
				createDecisionFrontierTestRecord(t, store, ticketIDs[i], decisionFrontierQuestionKind, "pending", doc)
			}
			promptDoc := decisionFrontierLinkDocument{
				SchemaVersion: identity.SchemaVersion, CityRef: identity.CityRef, StoreRef: identity.StoreRef,
				WorkID: identity.WorkID, WorkRevision: identity.WorkRevision, WorkDigest: identity.WorkDigest,
				MapID: identity.MapID, ID: promptID, TicketIDs: ticketIDs,
			}
			createDecisionFrontierTestRecord(t, store, promptID, decisionFrontierPromptKind, "unconfigured", promptDoc)

			writer, ok := DecisionFrontierRecordWriterFor(store)
			if !ok {
				t.Fatal("store did not expose decision-frontier record writer")
			}
			links := []struct {
				source  string
				target  string
				depType string
			}{
				{ticketIDs[0], mapID, "relates-to"},
				{ticketIDs[1], mapID, "relates-to"},
				{ticketIDs[1], ticketIDs[0], "blocks"},
				{promptID, mapID, "relates-to"},
			}
			for _, link := range links {
				if err := writer.EnsureDecisionFrontierLink(link.source, link.target, link.depType); err != nil {
					t.Fatalf("ensure authorized link %s -> %s (%s): %v", link.source, link.target, link.depType, err)
				}
			}
			before, err := store.Get(ticketIDs[0])
			if err != nil {
				t.Fatal(err)
			}
			if err := writer.EnsureDecisionFrontierLink(ticketIDs[0], mapID, "relates-to"); err != nil {
				t.Fatalf("retry exact authorized link: %v", err)
			}
			after, err := store.Get(ticketIDs[0])
			if err != nil {
				t.Fatal(err)
			}
			if after.Revision != before.Revision {
				t.Fatalf("exact link retry advanced source revision %d -> %d", before.Revision, after.Revision)
			}
			if mem, ok := store.(*MemStore); ok {
				mem.DisableConditionalWrites = true
				if err := writer.EnsureDecisionFrontierLink(ticketIDs[0], mapID, "relates-to"); !errors.Is(err, ErrConditionalWriteUnsupported) {
					t.Fatalf("MemStore link capability with conditional writes disabled = %v, want unsupported", err)
				}
				mem.DisableConditionalWrites = false
			}
			if err := writer.EnsureDecisionFrontierLink(ticketIDs[0], ticketIDs[1], "blocks"); !errors.Is(err, ErrDecisionFrontierLinkConflict) {
				t.Fatalf("undeclared prerequisite link error = %v, want conflict", err)
			}
			otherMapID := DecisionFrontierMapRecordID(cityRef, storeRef, "other-work", "9")
			otherPromptID := DecisionFrontierPromptRecordID(cityRef, storeRef, otherMapID)
			otherMap := decisionFrontierLinkDocument{
				SchemaVersion: 1, CityRef: cityRef, StoreRef: storeRef, WorkID: "other-work", WorkRevision: "9",
				WorkDigest: workDigest, MapID: otherMapID, PromptID: otherPromptID,
				Questions: []decisionFrontierLinkQuestion{{ID: "other", Title: "Other", Prompt: "Other."}},
			}
			createDecisionFrontierTestRecord(t, store, otherMapID, decisionFrontierMapKind, "pending", otherMap)
			if err := writer.EnsureDecisionFrontierLink(ticketIDs[0], otherMapID, "relates-to"); !errors.Is(err, ErrDecisionFrontierLinkConflict) {
				t.Fatalf("cross-map link error = %v, want conflict", err)
			}

			ordinary, err := store.Create(Bead{Type: "task", Title: "ordinary target"})
			if err != nil {
				t.Fatal(err)
			}
			if err := store.DepAdd(ticketIDs[0], ordinary.ID, "blocks"); !errors.Is(err, ErrDecisionFrontierMutationBlocked) {
				t.Fatalf("generic record-edge add error = %v, want refusal", err)
			}
			if err := store.DepAdd(ticketIDs[0], mapID, "blocks"); !errors.Is(err, ErrDecisionFrontierMutationBlocked) {
				t.Fatalf("generic record-edge type change error = %v, want refusal", err)
			}
			if err := store.DepRemove(ticketIDs[0], mapID); !errors.Is(err, ErrDecisionFrontierMutationBlocked) {
				t.Fatalf("generic record-edge removal error = %v, want refusal", err)
			}
			if err := store.DepRemove(ticketIDs[1], ticketIDs[0]); !errors.Is(err, ErrDecisionFrontierMutationBlocked) {
				t.Fatalf("generic prerequisite-edge removal error = %v, want refusal", err)
			}

			if sqlite, ok := store.(*SQLiteStore); ok {
				if _, err := sqlite.db.Exec(`INSERT INTO deps(issue_id, depends_on_id, dep_type) VALUES(?,?,?)`, ticketIDs[0], ordinary.ID, "blocks"); err != nil {
					t.Fatalf("insert cascade fixture edge: %v", err)
				}
				if err := store.Delete(ordinary.ID); !errors.Is(err, ErrDecisionFrontierMutationBlocked) {
					t.Fatalf("delete target with immutable record-owned edge error = %v, want refusal", err)
				}
			} else if mem, ok := store.(*MemStore); ok {
				mem.mu.Lock()
				mem.deps = append(mem.deps, Dep{IssueID: ticketIDs[0], DependsOnID: ordinary.ID, Type: "blocks"})
				mem.mu.Unlock()
				if err := store.Delete(ordinary.ID); err != nil {
					t.Fatalf("MemStore preserves dangling-edge delete behavior: %v", err)
				}
				deps, err := store.DepList(ticketIDs[0], "down")
				if err != nil {
					t.Fatal(err)
				}
				foundDangling := false
				for _, dep := range deps {
					if dep.DependsOnID == ordinary.ID {
						foundDangling = true
					}
				}
				if !foundDangling {
					t.Fatal("MemStore delete removed an edge from an immutable frontier record")
				}
			}

			gotMap, err := store.Get(mapBead.ID)
			if err != nil || gotMap.Metadata[beadmeta.DecisionFrontierRecordMetadataKey] != decisionFrontierMapKind {
				t.Fatalf("map record after link checks = %+v, err=%v", gotMap, err)
			}
		})
	}
}

func createDecisionFrontierTestRecord(t *testing.T, store Store, id, kind, state string, doc any) Bead {
	t.Helper()
	body, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	record, err := mustDecisionFrontierRecordWriter(t, store).CreateDecisionFrontierRecord(Bead{
		ID: id, Type: "gate", Title: "record " + id, Description: string(body),
		Metadata: StringMap{beadmeta.DecisionFrontierRecordMetadataKey: kind, beadmeta.DecisionFrontierStateMetadataKey: state},
	})
	if err != nil {
		t.Fatalf("create test record %q: %v", id, err)
	}
	return record
}

func mustDecisionFrontierRecordWriter(t *testing.T, store Store) DecisionFrontierRecordWriter {
	t.Helper()
	writer, ok := DecisionFrontierRecordWriterFor(store)
	if !ok {
		t.Fatal("store does not expose decision-frontier record writer")
	}
	return writer
}

func explicitMemStore() Store {
	store := NewMemStore()
	store.HonorExplicitIDs = true
	return store
}

func openDecisionFrontierSQLite(t *testing.T) decisionFrontierStoreFixture {
	t.Helper()
	opened, err := OpenSQLiteStore(t.TempDir())
	if err != nil {
		t.Fatalf("OpenSQLiteStore: %v", err)
	}
	sqlite := opened.(*SQLiteStore)
	return decisionFrontierStoreFixture{store: sqlite, close: sqlite.CloseStore}
}

func assertDecisionFrontierGuards(t *testing.T, store Store) {
	t.Helper()
	forged := Bead{
		ID: "gcf-map-forged", Type: "gate", Title: "forged", Description: `{"schema_version":1,"city_ref":"city:test","store_ref":"city:test","work_id":"work","work_revision":"1","map_id":"gcf-map-forged"}`,
		Metadata: StringMap{beadmeta.DecisionFrontierRecordMetadataKey: "decision-frontier/map/v1", beadmeta.DecisionFrontierStateMetadataKey: "pending"},
	}
	if _, err := store.Create(forged); !errors.Is(err, ErrDecisionFrontierMutationBlocked) {
		t.Fatalf("generic Create error = %v, want reserved-metadata refusal", err)
	}

	recordWriter, ok := DecisionFrontierRecordWriterFor(store)
	if !ok {
		t.Fatal("supported store did not expose trusted record writer")
	}
	mapID := "gcf-map-trusted"
	record, err := recordWriter.CreateDecisionFrontierRecord(Bead{
		ID: mapID, Type: "gate", Title: "trusted map",
		Description: `{"schema_version":1,"city_ref":"city:test","store_ref":"city:test","work_id":"work","work_revision":"1","map_id":"gcf-map-trusted"}`,
		Metadata:    StringMap{beadmeta.DecisionFrontierRecordMetadataKey: "decision-frontier/map/v1", beadmeta.DecisionFrontierStateMetadataKey: "pending"},
	})
	if err != nil {
		t.Fatalf("trusted CreateDecisionFrontierRecord: %v", err)
	}
	if err := store.Update(record.ID, UpdateOpts{Title: ptrTo("forged update")}); !errors.Is(err, ErrDecisionFrontierMutationBlocked) {
		t.Fatalf("generic Update error = %v, want reserved-record refusal", err)
	}
	writer, ok := ConditionalWriterFor(store)
	if !ok {
		t.Fatal("supported store did not expose conditional writer")
	}
	if _, err := writer.CompareAndSetMetadataKey(record.ID, beadmeta.DecisionFrontierStateMetadataKey, "pending", "resolved"); !errors.Is(err, ErrDecisionFrontierMutationBlocked) {
		t.Fatalf("generic CAS error = %v, want reserved-record refusal", err)
	}
	if err := writer.CloseIfMatch(record.ID, record.Revision); !errors.Is(err, ErrDecisionFrontierMutationBlocked) {
		t.Fatalf("conditional record close error = %v, want reserved-record refusal", err)
	}
	if err := store.Delete(record.ID); !errors.Is(err, ErrDecisionFrontierRecordProtected) {
		t.Fatalf("record delete error = %v, want protected", err)
	}
	if err := writer.DeleteIfMatch(record.ID, record.Revision); !errors.Is(err, ErrDecisionFrontierRecordProtected) {
		t.Fatalf("conditional record delete error = %v, want protected", err)
	}
	if deleter, ok := store.(BatchDeleter); ok {
		if err := deleter.DeleteBatch([]string{record.ID}); !errors.Is(err, ErrDecisionFrontierRecordProtected) {
			t.Fatalf("batch record delete error = %v, want protected", err)
		}
	}

	work, err := store.Create(Bead{Title: "held source", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	if work.Revision == 0 {
		if err := store.Update(work.ID, UpdateOpts{Title: ptrTo("revisioned source")}); err != nil {
			t.Fatal(err)
		}
		work, err = store.Get(work.ID)
		if err != nil {
			t.Fatal(err)
		}
	}
	cityRef, storeRef, heldMapID := "city:test", "city:test", "gcf-map-held"
	reservationID := "gcf-transition-reserve-held"
	workRevision := strconv.FormatInt(work.Revision, 10)
	hold, err := json.Marshal(struct {
		SchemaVersion int    `json:"schema_version"`
		CityRef       string `json:"city_ref"`
		StoreRef      string `json:"store_ref"`
		WorkID        string `json:"work_id"`
		MapID         string `json:"map_id"`
		WorkRevision  string `json:"work_revision"`
		WorkDigest    string `json:"work_digest"`
		ProposalHash  string `json:"proposal_hash"`
		ReservationID string `json:"reservation_id"`
	}{1, cityRef, storeRef, work.ID, heldMapID, workRevision, "digest", "proposal", reservationID})
	if err != nil {
		t.Fatal(err)
	}
	transition, ok := RevisionTransitionWriterFor(store)
	if !ok {
		t.Fatal("supported store did not expose source transition writer")
	}
	if _, won, err := transition.CompareAndSetMetadataKeyWithReceipt(work.ID, beadmeta.DecisionFrontierHoldMetadataKey, "", string(hold), work.Revision,
		RevisionTransitionReceipt{ID: reservationID, CityRef: cityRef, StoreRef: storeRef, WorkID: work.ID, MapID: heldMapID, Operation: "reserve", FromRevision: work.Revision}); err != nil || !won {
		t.Fatalf("reserve source hold won=%v err=%v", won, err)
	}
	held, err := store.Get(work.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := writer.CloseIfMatch(held.ID, held.Revision); !errors.Is(err, ErrDecisionFrontierMutationBlocked) {
		t.Fatalf("held source conditional close error = %v, want protected", err)
	}
	if err := store.Delete(held.ID); !errors.Is(err, ErrDecisionFrontierRecordProtected) {
		t.Fatalf("held source delete error = %v, want protected", err)
	}
	if err := writer.DeleteIfMatch(held.ID, held.Revision); !errors.Is(err, ErrDecisionFrontierRecordProtected) {
		t.Fatalf("held source conditional delete error = %v, want protected", err)
	}
	if deleter, ok := store.(BatchDeleter); ok {
		if err := deleter.DeleteBatch([]string{held.ID}); !errors.Is(err, ErrDecisionFrontierRecordProtected) {
			t.Fatalf("held source batch delete error = %v, want protected", err)
		}
	}

	release := RevisionTransitionReceipt{
		ID: "gcf-transition-release-held", CityRef: cityRef, StoreRef: storeRef,
		WorkID: work.ID, MapID: heldMapID, Operation: "release", FromRevision: held.Revision,
	}
	if _, won, err := transition.CompareAndSetMetadataKeyWithReceipt(work.ID,
		beadmeta.DecisionFrontierHoldMetadataKey, string(hold), "", held.Revision, release); err != nil || !won {
		t.Fatalf("release source hold won=%v err=%v", won, err)
	}
	released, err := store.Get(work.ID)
	if err != nil {
		t.Fatal(err)
	}
	if released.Metadata[beadmeta.DecisionFrontierHoldMetadataKey] != "" ||
		strings.TrimSpace(released.Metadata[beadmeta.DecisionFrontierRevisionReceiptsMetadataKey]) == "" {
		t.Fatalf("released source does not retain its receipt: metadata=%v", released.Metadata)
	}
	closed := "closed"
	if err := store.Update(released.ID, UpdateOpts{Status: &closed}); err != nil {
		t.Fatalf("released source ordinary close: %v", err)
	}
	completed, err := store.Get(released.ID)
	if err != nil {
		t.Fatal(err)
	}
	if completed.Status != "closed" || completed.Metadata[beadmeta.DecisionFrontierRevisionReceiptsMetadataKey] == "" {
		t.Fatalf("ordinary close lost status or immutable receipts: status=%q metadata=%v", completed.Status, completed.Metadata)
	}
	if err := store.Close(released.ID); err != nil {
		t.Fatalf("released source Close: %v", err)
	}
	if err := writer.CloseIfMatch(released.ID, completed.Revision); err != nil {
		t.Fatalf("released source conditional close: %v", err)
	}
	completed, err = store.Get(released.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(completed.ID); !errors.Is(err, ErrDecisionFrontierRecordProtected) {
		t.Fatalf("released source delete error = %v, want protected", err)
	}
	if err := writer.DeleteIfMatch(completed.ID, completed.Revision); !errors.Is(err, ErrDecisionFrontierRecordProtected) {
		t.Fatalf("released source conditional delete error = %v, want protected", err)
	}
	if deleter, ok := store.(BatchDeleter); ok {
		if err := deleter.DeleteBatch([]string{completed.ID}); !errors.Is(err, ErrDecisionFrontierRecordProtected) {
			t.Fatalf("released source batch delete error = %v, want protected", err)
		}
	}
}

func TestConditionalLifecycleCompletionStillClosesSource(t *testing.T) {
	for _, tc := range decisionFrontierStores(t) {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if err := tc.close(); err != nil {
					t.Errorf("close decision-frontier test store: %v", err)
				}
			}()
			created, err := tc.store.Create(Bead{
				Title: "admitted source", Type: "task",
				Metadata: StringMap{beadmeta.LifecycleAdmissionReceiptMetadataKey: `{"signed":"receipt"}`},
			})
			if err != nil {
				t.Fatal(err)
			}
			inProgress := "in_progress"
			if err := tc.store.Update(created.ID, UpdateOpts{Status: &inProgress}); err != nil {
				t.Fatal(err)
			}
			current, err := tc.store.Get(created.ID)
			if err != nil {
				t.Fatal(err)
			}
			writer, ok := ConditionalWriterFor(tc.store)
			if !ok {
				t.Fatal("store does not expose conditional writer")
			}
			if err := writer.CloseIfMatch(current.ID, current.Revision); err != nil {
				t.Fatalf("controller-verified revision-matched completion: %v", err)
			}
			closed, err := tc.store.Get(created.ID)
			if err != nil || closed.Status != "closed" {
				t.Fatalf("lifecycle close status=%q err=%v", closed.Status, err)
			}
		})
	}
}

func TestDecisionFrontierHoldBlocksOwnershipMutationButKeepsNoOps(t *testing.T) {
	for _, tc := range decisionFrontierStores(t) {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if err := tc.close(); err != nil {
					t.Errorf("close decision-frontier test store: %v", err)
				}
			}()
			releaser, ok := tc.store.(ConditionalAssignmentReleaser)
			if !ok {
				t.Fatal("supported store does not expose conditional assignment release")
			}
			work, err := tc.store.Create(Bead{Title: "owned source"})
			if err != nil {
				t.Fatal(err)
			}
			inProgress, owner := "in_progress", "worker-a"
			if err := tc.store.Update(work.ID, UpdateOpts{Status: &inProgress, Assignee: &owner}); err != nil {
				t.Fatal(err)
			}
			held, marker, transition := reserveGuardTestSource(t, tc.store, work.ID)
			if claimer, ok := tc.store.(interface {
				Claim(string, string) (Bead, bool, error)
			}); ok {
				claimed, won, err := claimer.Claim(work.ID, owner)
				if err != nil || !won || claimed.Revision != held.Revision {
					t.Fatalf("same-owner Claim no-op while held: won=%v err=%v row=%+v", won, err, claimed)
				}
			}
			released, err := releaser.ReleaseIfCurrent(work.ID, owner)
			if !errors.Is(err, ErrDecisionFrontierMutationBlocked) || released {
				t.Fatalf("ReleaseIfCurrent during hold: released=%v err=%v, want blocked", released, err)
			}
			current, err := tc.store.Get(work.ID)
			if err != nil {
				t.Fatal(err)
			}
			if current.Revision != held.Revision || current.Status != "in_progress" || current.Assignee != owner || !HasDecisionFrontierHold(current) {
				t.Fatalf("blocked owner release changed source: held=%+v current=%+v", held, current)
			}
			if _, won, err := transition.CompareAndSetMetadataKeyWithReceipt(work.ID,
				beadmeta.DecisionFrontierHoldMetadataKey, marker, "", held.Revision,
				RevisionTransitionReceipt{
					ID: "gcf-transition-release-" + work.ID, CityRef: "city:test", StoreRef: "city:test",
					WorkID: work.ID, MapID: "gcf-map-" + work.ID, Operation: "release", FromRevision: held.Revision,
				}); err != nil || !won {
				t.Fatalf("release source hold after blocked owner mutation: won=%v err=%v", won, err)
			}
			released, err = releaser.ReleaseIfCurrent(work.ID, owner)
			if err != nil || !released {
				t.Fatalf("ReleaseIfCurrent after frontier release: released=%v err=%v", released, err)
			}
		})
	}
}

func TestDecisionFrontierHoldBlocksClaimButAllowsAfterRelease(t *testing.T) {
	for _, tc := range decisionFrontierStores(t) {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if err := tc.close(); err != nil {
					t.Errorf("close decision-frontier test store: %v", err)
				}
			}()
			claimer, ok := tc.store.(interface {
				Claim(string, string) (Bead, bool, error)
			})
			if !ok {
				t.Skip("store has no direct Claim capability")
			}
			work, err := tc.store.Create(Bead{Title: "unowned held source"})
			if err != nil {
				t.Fatal(err)
			}
			held, marker, transition := reserveGuardTestSource(t, tc.store, work.ID)
			claimed, won, err := claimer.Claim(work.ID, "worker-a")
			if !errors.Is(err, ErrDecisionFrontierMutationBlocked) || won {
				t.Fatalf("Claim during hold: won=%v err=%v row=%+v, want blocked", won, err, claimed)
			}
			current, err := tc.store.Get(work.ID)
			if err != nil {
				t.Fatal(err)
			}
			if current.Revision != held.Revision || current.Status != "open" || current.Assignee != "" || !HasDecisionFrontierHold(current) {
				t.Fatalf("blocked Claim changed source: held=%+v current=%+v", held, current)
			}
			if _, won, err := transition.CompareAndSetMetadataKeyWithReceipt(work.ID,
				beadmeta.DecisionFrontierHoldMetadataKey, marker, "", held.Revision,
				RevisionTransitionReceipt{
					ID: "gcf-transition-release-" + work.ID, CityRef: "city:test", StoreRef: "city:test",
					WorkID: work.ID, MapID: "gcf-map-" + work.ID, Operation: "release", FromRevision: held.Revision,
				}); err != nil || !won {
				t.Fatalf("release claim source hold: won=%v err=%v", won, err)
			}
			claimed, won, err = claimer.Claim(work.ID, "worker-a")
			if err != nil || !won || claimed.Status != "in_progress" || claimed.Assignee != "worker-a" {
				t.Fatalf("Claim after frontier release: won=%v err=%v row=%+v", won, err, claimed)
			}
		})
	}
}

func TestDecisionFrontierStorageAcceptsSignedBackendRevisionTokens(t *testing.T) {
	const from int64 = -868924464739205321
	const to int64 = -4153482142457066826
	receipt := RevisionTransitionReceipt{
		ID: "gcf-transition-signed", CityRef: "city:test", StoreRef: "city:test",
		WorkID: "work", MapID: "gcf-map-signed", Operation: "reserve", FromRevision: from,
	}
	if _, err := appendRevisionTransitionReceipt("", receipt, to); err != nil {
		t.Fatalf("append signed transition receipt: %v", err)
	}
	doc := `{"schema_version":1,"city_ref":"city:test","store_ref":"city:test","work_id":"work","work_revision":"-868924464739205321","map_id":"gcf-map-signed"}`
	if err := validateDecisionFrontierRecordCreate(Bead{
		ID: "gcf-map-signed", Type: "gate", Title: "signed revision map", Description: doc,
		Metadata: StringMap{beadmeta.DecisionFrontierRecordMetadataKey: "decision-frontier/map/v1", beadmeta.DecisionFrontierStateMetadataKey: "pending"},
	}); err != nil {
		t.Fatalf("validate signed revision map record: %v", err)
	}
}

func TestDecisionFrontierSourceSnapshotHydratesAuthoritativeEdges(t *testing.T) {
	for _, tc := range decisionFrontierStores(t) {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if err := tc.close(); err != nil {
					t.Errorf("close decision-frontier test store: %v", err)
				}
			}()
			target, err := tc.store.Create(Bead{Title: "dependency target"})
			if err != nil {
				t.Fatal(err)
			}
			source, err := tc.store.Create(Bead{Title: "source", Needs: []string{"blocks:" + target.ID}})
			if err != nil {
				t.Fatal(err)
			}
			ordinary, err := tc.store.Get(source.ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(ordinary.Dependencies) != 0 {
				t.Fatalf("ordinary Get changed its dependency projection: %+v", ordinary.Dependencies)
			}
			reader, ok := DecisionFrontierSourceReaderFor(tc.store)
			if !ok || reader == nil {
				t.Fatal("supported direct store did not expose authoritative source snapshot")
			}
			snapshot, err := reader.DecisionFrontierSourceSnapshot(source.ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(snapshot.Dependencies) != 1 || snapshot.Dependencies[0] != (Dep{IssueID: source.ID, DependsOnID: target.ID, Type: "blocks"}) {
				t.Fatalf("source snapshot dependencies = %+v, want persisted outgoing edge", snapshot.Dependencies)
			}
			initialRevision := snapshot.Revision
			if err := tc.store.DepAdd(source.ID, target.ID, "blocks"); err != nil {
				t.Fatalf("same-edge DepAdd: %v", err)
			}
			snapshot, err = reader.DecisionFrontierSourceSnapshot(source.ID)
			if err != nil {
				t.Fatal(err)
			}
			if snapshot.Revision != initialRevision {
				t.Fatalf("same-edge DepAdd changed revision %d -> %d", initialRevision, snapshot.Revision)
			}
			if err := tc.store.DepAdd(source.ID, target.ID, "tracks"); err != nil {
				t.Fatalf("dependency type change: %v", err)
			}
			snapshot, err = reader.DecisionFrontierSourceSnapshot(source.ID)
			if err != nil {
				t.Fatal(err)
			}
			if snapshot.Revision != initialRevision+1 || len(snapshot.Dependencies) != 1 || snapshot.Dependencies[0].Type != "tracks" {
				t.Fatalf("type change snapshot = rev %d deps %+v, want revision +1 and tracks", snapshot.Revision, snapshot.Dependencies)
			}
			if err := tc.store.DepRemove(source.ID, target.ID); err != nil {
				t.Fatalf("dependency remove: %v", err)
			}
			snapshot, err = reader.DecisionFrontierSourceSnapshot(source.ID)
			if err != nil {
				t.Fatal(err)
			}
			if snapshot.Revision != initialRevision+2 || len(snapshot.Dependencies) != 0 {
				t.Fatalf("remove snapshot = rev %d deps %+v, want revision +2 and no edges", snapshot.Revision, snapshot.Dependencies)
			}
			if err := tc.store.DepRemove(source.ID, "absent-target"); err != nil {
				t.Fatalf("absent-edge DepRemove: %v", err)
			}
			snapshot, err = reader.DecisionFrontierSourceSnapshot(source.ID)
			if err != nil {
				t.Fatal(err)
			}
			if snapshot.Revision != initialRevision+2 {
				t.Fatalf("absent-edge DepRemove changed revision to %d", snapshot.Revision)
			}
		})
	}
}

func TestDecisionFrontierHeldSourceFencesDependencyMutations(t *testing.T) {
	for _, tc := range decisionFrontierStores(t) {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if err := tc.close(); err != nil {
					t.Errorf("close decision-frontier test store: %v", err)
				}
			}()
			target, err := tc.store.Create(Bead{Title: "target"})
			if err != nil {
				t.Fatal(err)
			}
			extra, err := tc.store.Create(Bead{Title: "extra target"})
			if err != nil {
				t.Fatal(err)
			}
			source, err := tc.store.Create(Bead{Title: "held source", Needs: []string{"blocks:" + target.ID}})
			if err != nil {
				t.Fatal(err)
			}
			unrelated, err := tc.store.Create(Bead{Title: "unrelated source"})
			if err != nil {
				t.Fatal(err)
			}
			held, marker, transition := reserveGuardTestSource(t, tc.store, source.ID)
			if err := tc.store.DepAdd(source.ID, target.ID, "blocks"); err != nil {
				t.Fatalf("same-edge DepAdd on held source: %v", err)
			}
			if err := tc.store.DepRemove(source.ID, "absent-target"); err != nil {
				t.Fatalf("absent-edge DepRemove on held source: %v", err)
			}
			if err := tc.store.DepAdd(source.ID, extra.ID, "blocks"); !errors.Is(err, ErrDecisionFrontierMutationBlocked) {
				t.Fatalf("new edge on held source = %v, want blocked", err)
			}
			if err := tc.store.DepRemove(source.ID, target.ID); !errors.Is(err, ErrDecisionFrontierMutationBlocked) {
				t.Fatalf("edge removal on held source = %v, want blocked", err)
			}
			current, err := tc.store.Get(source.ID)
			if err != nil {
				t.Fatal(err)
			}
			deps, err := tc.store.DepList(source.ID, "down")
			if err != nil {
				t.Fatal(err)
			}
			if current.Revision != held.Revision || len(deps) != 1 || deps[0].DependsOnID != target.ID {
				t.Fatalf("blocked edge edits changed held source: revision=%d deps=%+v held=%d", current.Revision, deps, held.Revision)
			}
			beforeUnrelated, err := tc.store.Get(unrelated.ID)
			if err != nil {
				t.Fatal(err)
			}
			if err := tc.store.DepAdd(unrelated.ID, target.ID, "blocks"); err != nil {
				t.Fatalf("unrelated source edge add: %v", err)
			}
			afterUnrelated, err := tc.store.Get(unrelated.ID)
			if err != nil {
				t.Fatal(err)
			}
			if afterUnrelated.Revision != beforeUnrelated.Revision+1 {
				t.Fatalf("unrelated source revision = %d, want %d", afterUnrelated.Revision, beforeUnrelated.Revision+1)
			}
			release := RevisionTransitionReceipt{
				ID: "gcf-transition-release-" + source.ID, CityRef: "city:test", StoreRef: "city:test",
				WorkID: source.ID, MapID: "gcf-map-" + source.ID, Operation: "release", FromRevision: held.Revision,
			}
			if _, won, err := transition.CompareAndSetMetadataKeyWithReceipt(source.ID,
				beadmeta.DecisionFrontierHoldMetadataKey, marker, "", held.Revision, release); err != nil || !won {
				t.Fatalf("release after blocked source mutation and unrelated edit: won=%v err=%v", won, err)
			}
		})
	}
}

func reserveGuardTestSource(t *testing.T, store Store, sourceID string) (Bead, string, RevisionTransitionWriter) {
	t.Helper()
	work, err := store.Get(sourceID)
	if err != nil {
		t.Fatal(err)
	}
	if work.Revision == 0 {
		if err := store.Update(sourceID, UpdateOpts{Title: ptrTo("revisioned source")}); err != nil {
			t.Fatal(err)
		}
		work, err = store.Get(sourceID)
		if err != nil {
			t.Fatal(err)
		}
	}
	const cityRef, storeRef = "city:test", "city:test"
	mapID := "gcf-map-" + sourceID
	reservationID := "gcf-transition-reserve-" + sourceID
	hold, err := json.Marshal(struct {
		SchemaVersion int    `json:"schema_version"`
		CityRef       string `json:"city_ref"`
		StoreRef      string `json:"store_ref"`
		WorkID        string `json:"work_id"`
		MapID         string `json:"map_id"`
		WorkRevision  string `json:"work_revision"`
		WorkDigest    string `json:"work_digest"`
		ProposalHash  string `json:"proposal_hash"`
		ReservationID string `json:"reservation_id"`
	}{1, cityRef, storeRef, sourceID, mapID, strconv.FormatInt(work.Revision, 10), "test-digest", "test-proposal", reservationID})
	if err != nil {
		t.Fatal(err)
	}
	transition, ok := RevisionTransitionWriterFor(store)
	if !ok || transition == nil {
		t.Fatal("store lacks revision-transition writer")
	}
	if _, won, err := transition.CompareAndSetMetadataKeyWithReceipt(sourceID, beadmeta.DecisionFrontierHoldMetadataKey, "", string(hold), work.Revision,
		RevisionTransitionReceipt{ID: reservationID, CityRef: cityRef, StoreRef: storeRef, WorkID: sourceID, MapID: mapID, Operation: "reserve", FromRevision: work.Revision}); err != nil || !won {
		t.Fatalf("reserve test source hold: won=%v err=%v", won, err)
	}
	held, err := store.Get(sourceID)
	if err != nil {
		t.Fatal(err)
	}
	return held, string(hold), transition
}

func TestMemDeletePreservesDanglingEdgesAndConditionalDeleteCascades(t *testing.T) {
	store := explicitMemStore()
	source, err := store.Create(Bead{Title: "source"})
	if err != nil {
		t.Fatal(err)
	}
	firstTarget, err := store.Create(Bead{Title: "first target"})
	if err != nil {
		t.Fatal(err)
	}
	secondTarget, err := store.Create(Bead{Title: "second target"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.DepAdd(source.ID, firstTarget.ID, "blocks"); err != nil {
		t.Fatal(err)
	}
	if err := store.DepAdd(source.ID, secondTarget.ID, "blocks"); err != nil {
		t.Fatal(err)
	}
	before, err := store.Get(source.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(firstTarget.ID); err != nil {
		t.Fatal(err)
	}
	conditional, ok := ConditionalWriterFor(store)
	if !ok {
		t.Fatal("MemStore did not expose conditional writer")
	}
	if err := conditional.DeleteIfMatch(secondTarget.ID, secondTarget.Revision); err != nil {
		t.Fatal(err)
	}
	after, err := store.Get(source.ID)
	if err != nil {
		t.Fatal(err)
	}
	deps, err := store.DepList(source.ID, "down")
	if err != nil {
		t.Fatal(err)
	}
	if after.Revision == before.Revision || len(deps) != 1 || deps[0].DependsOnID != firstTarget.ID {
		t.Fatalf("Mem target deletion changed source snapshot: revision %d -> %d, deps=%+v", before.Revision, after.Revision, deps)
	}
	reader, ok := DecisionFrontierSourceReaderFor(store)
	if !ok {
		t.Fatal("MemStore lacks source snapshot")
	}
	snapshot, err := reader.DecisionFrontierSourceSnapshot(source.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Dependencies) != 1 || snapshot.Dependencies[0].DependsOnID != firstTarget.ID {
		t.Fatalf("Mem source snapshot lost dangling dependency edges: %+v", snapshot.Dependencies)
	}
}

func TestConditionalDeletePreservesHeldIncomingSource(t *testing.T) {
	for _, native := range []bool{false, true} {
		t.Run(strconv.FormatBool(native), func(t *testing.T) {
			target := Bead{ID: "gc-target", Title: "target", Status: "open", Type: "task", Revision: 1}
			source := Bead{
				ID: "gc-source", Title: "held source", Status: "open", Type: "task", Revision: 1,
				Metadata: map[string]string{beadmeta.DecisionFrontierHoldMetadataKey: "map-1"},
			}
			mem := NewMemStoreFrom(3, []Bead{target, source}, []Dep{{IssueID: source.ID, DependsOnID: target.ID, Type: "blocks"}})
			var store Store = mem
			if native {
				storage := newNativeDoltMemStorage()
				storage.store = mem
				store = newNativeDoltStoreForTest(storage)
			}
			writer, ok := ConditionalWriterFor(store)
			if !ok {
				t.Fatal("conditional writer missing")
			}
			if err := writer.DeleteIfMatch(target.ID, 1); !errors.Is(err, ErrDecisionFrontierMutationBlocked) {
				t.Fatalf("conditional cascade bypassed held source: %v", err)
			}
			if _, err := store.Get(target.ID); err != nil {
				t.Fatalf("refused cascade deleted target: %v", err)
			}
			deps, err := store.DepList(source.ID, "down")
			if err != nil || len(deps) != 1 || deps[0].DependsOnID != target.ID {
				t.Fatalf("refused cascade changed held edge: %+v %v", deps, err)
			}
		})
	}
}

func TestSQLiteDecisionFrontierFencesGraphPlanAndIncomingDeleteCascades(t *testing.T) {
	fixture := openDecisionFrontierSQLite(t)
	defer func() {
		if err := fixture.close(); err != nil {
			t.Errorf("close SQLite decision-frontier fixture: %v", err)
		}
	}()
	sqlite := fixture.store.(*SQLiteStore)
	target, err := sqlite.Create(Bead{Title: "target"})
	if err != nil {
		t.Fatal(err)
	}
	extra, err := sqlite.Create(Bead{Title: "extra target"})
	if err != nil {
		t.Fatal(err)
	}
	source, err := sqlite.Create(Bead{Title: "held source"})
	if err != nil {
		t.Fatal(err)
	}
	if err := sqlite.DepAdd(source.ID, target.ID, "blocks"); err != nil {
		t.Fatal(err)
	}
	held, marker, transition := reserveGuardTestSource(t, sqlite, source.ID)
	if err := sqlite.DepAdd(source.ID, extra.ID, "blocks"); !errors.Is(err, ErrDecisionFrontierMutationBlocked) {
		t.Fatalf("graph-plan source edge addition equivalent = %v, want blocked", err)
	}
	if err := sqlite.DepAdd(source.ID, target.ID, "tracks"); !errors.Is(err, ErrDecisionFrontierMutationBlocked) {
		t.Fatalf("held source dependency type change = %v, want blocked", err)
	}
	if err := sqlite.DepAddWithMetadata(source.ID, target.ID, "blocks", "changed-payload"); !errors.Is(err, ErrDecisionFrontierMutationBlocked) {
		t.Fatalf("held source dependency metadata edit = %v, want blocked", err)
	}
	plan := &GraphApplyPlan{
		Nodes: []GraphApplyNode{{Key: "new-node", Title: "new node"}},
		Edges: []GraphApplyEdge{{FromID: source.ID, ToID: target.ID, Type: "blocks", Metadata: "changed-payload"}},
	}
	if _, err := sqlite.ApplyGraphPlan(context.Background(), plan); !errors.Is(err, ErrDecisionFrontierMutationBlocked) {
		t.Fatalf("GraphApply literal FromID edit to held source = %v, want blocked", err)
	}
	if err := sqlite.DepRemove(source.ID, target.ID); !errors.Is(err, ErrDecisionFrontierMutationBlocked) {
		t.Fatalf("held source dependency removal = %v, want blocked", err)
	}
	conditional, ok := ConditionalWriterFor(sqlite)
	if !ok {
		t.Fatal("SQLiteStore did not expose conditional writer")
	}
	if err := sqlite.Delete(target.ID); !errors.Is(err, ErrDecisionFrontierMutationBlocked) {
		t.Fatalf("target Delete cascade affecting held source = %v, want blocked", err)
	}
	if err := conditional.DeleteIfMatch(target.ID, target.Revision); !errors.Is(err, ErrDecisionFrontierMutationBlocked) {
		t.Fatalf("target DeleteIfMatch cascade affecting held source = %v, want blocked", err)
	}
	if err := sqlite.DeleteBatch([]string{target.ID}); !errors.Is(err, ErrDecisionFrontierMutationBlocked) {
		t.Fatalf("target DeleteBatch cascade affecting held source = %v, want blocked", err)
	}
	current, err := sqlite.Get(source.ID)
	if err != nil {
		t.Fatal(err)
	}
	deps, err := sqlite.DepList(source.ID, "down")
	if err != nil {
		t.Fatal(err)
	}
	if current.Revision != held.Revision || len(deps) != 1 || deps[0].DependsOnID != target.ID {
		t.Fatalf("refused graph edits changed held source: revision %d -> %d deps=%+v", held.Revision, current.Revision, deps)
	}
	if _, err := sqlite.Get(target.ID); err != nil {
		t.Fatalf("refused cascade deleted target: %v", err)
	}
	if _, won, err := transition.CompareAndSetMetadataKeyWithReceipt(source.ID, beadmeta.DecisionFrontierHoldMetadataKey,
		marker, "", held.Revision, RevisionTransitionReceipt{
			ID: "gcf-transition-release-" + source.ID, CityRef: "city:test", StoreRef: "city:test",
			WorkID: source.ID, MapID: "gcf-map-" + source.ID, Operation: "release", FromRevision: held.Revision,
		}); err != nil || !won {
		t.Fatalf("release after refused graph edits: won=%v err=%v", won, err)
	}

	unheldSource, err := sqlite.Create(Bead{Title: "unheld source"})
	if err != nil {
		t.Fatal(err)
	}
	targets := make([]Bead, 3)
	for i := range targets {
		targets[i], err = sqlite.Create(Bead{Title: "unheld target"})
		if err != nil {
			t.Fatal(err)
		}
		if err := sqlite.DepAdd(unheldSource.ID, targets[i].ID, "blocks"); err != nil {
			t.Fatal(err)
		}
	}
	before, err := sqlite.Get(unheldSource.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := sqlite.Delete(targets[0].ID); err != nil {
		t.Fatalf("unheld incoming Delete: %v", err)
	}
	if err := conditional.DeleteIfMatch(targets[1].ID, targets[1].Revision); err != nil {
		t.Fatalf("unheld incoming DeleteIfMatch: %v", err)
	}
	if err := sqlite.DeleteBatch([]string{targets[2].ID}); err != nil {
		t.Fatalf("unheld incoming DeleteBatch: %v", err)
	}
	after, err := sqlite.Get(unheldSource.ID)
	if err != nil {
		t.Fatal(err)
	}
	deps, err = sqlite.DepList(unheldSource.ID, "down")
	if err != nil {
		t.Fatal(err)
	}
	if after.Revision != before.Revision+3 || len(deps) != 0 {
		t.Fatalf("unheld cascade source revision/deps = %d/%+v, want %d/no edges", after.Revision, deps, before.Revision+3)
	}
}

func TestSQLiteCreatePathsFenceStructuredDependencyOwner(t *testing.T) {
	fixture := openDecisionFrontierSQLite(t)
	defer func() {
		if err := fixture.close(); err != nil {
			t.Errorf("close SQLite decision-frontier fixture: %v", err)
		}
	}()
	sqlite := fixture.store.(*SQLiteStore)
	target, err := sqlite.Create(Bead{Title: "target"})
	if err != nil {
		t.Fatal(err)
	}
	heldSource, err := sqlite.Create(Bead{Title: "held source"})
	if err != nil {
		t.Fatal(err)
	}
	held, _, _ := reserveGuardTestSource(t, sqlite, heldSource.ID)
	dependency := Dep{IssueID: heldSource.ID, DependsOnID: target.ID, Type: "blocks"}
	for _, tc := range []struct {
		name string
		id   string
		make func() error
	}{
		{name: "ordinary create", id: "gc-frontier-create-forged", make: func() error {
			_, err := sqlite.Create(Bead{ID: "gc-frontier-create-forged", Title: "forged owner edge", Dependencies: []Dep{dependency}})
			return err
		}},
		{name: "foreign ID create", id: "external-frontier-create-forged", make: func() error {
			_, err := sqlite.CreateWithForeignID(Bead{ID: "external-frontier-create-forged", Title: "forged owner edge", Dependencies: []Dep{dependency}})
			return err
		}},
		{name: "transaction create", id: "gc-frontier-tx-create-forged", make: func() error {
			return sqlite.Tx("create dependency with foreign owner", func(tx Tx) error {
				_, err := tx.Create(Bead{ID: "gc-frontier-tx-create-forged", Title: "forged owner edge", Dependencies: []Dep{dependency}})
				return err
			})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.make(); !errors.Is(err, ErrDecisionFrontierMutationBlocked) {
				t.Fatalf("create with dependency owned by held source = %v, want blocked", err)
			}
			if _, err := sqlite.Get(tc.id); !errors.Is(err, ErrNotFound) {
				t.Fatalf("refused create left row %s: %v", tc.id, err)
			}
			current, err := sqlite.Get(heldSource.ID)
			if err != nil {
				t.Fatal(err)
			}
			deps, err := sqlite.DepList(heldSource.ID, "down")
			if err != nil {
				t.Fatal(err)
			}
			if current.Revision != held.Revision || len(deps) != 0 || !HasDecisionFrontierHold(current) {
				t.Fatalf("refused create changed held owner: row=%+v edges=%+v held=%+v", current, deps, held)
			}
		})
	}

	// A dependency with no explicit owner belongs to the newly created row and
	// stays atomic with that row's initial revision.
	created, err := sqlite.Create(Bead{Title: "initial dependency owner", Dependencies: []Dep{{DependsOnID: target.ID, Type: "blocks"}}})
	if err != nil {
		t.Fatalf("Create with implicit dependency owner: %v", err)
	}
	reader, ok := DecisionFrontierSourceReaderFor(sqlite)
	if !ok {
		t.Fatal("SQLite source snapshot reader unavailable")
	}
	snapshot, err := reader.DecisionFrontierSourceSnapshot(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Revision != created.Revision || len(snapshot.Dependencies) != 1 || snapshot.Dependencies[0] != (Dep{IssueID: created.ID, DependsOnID: target.ID, Type: "blocks"}) {
		t.Fatalf("created source snapshot = %+v, want one initial edge at revision %d", snapshot, created.Revision)
	}
}
