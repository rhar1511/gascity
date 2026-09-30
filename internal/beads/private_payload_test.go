package beads

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/fsys"
	beadslib "github.com/steveyegge/beads"
)

func protectedAttemptEvidenceBead() Bead {
	return Bead{
		Title:  "attempt evidence",
		Type:   "molecule",
		Status: "closed",
		Metadata: StringMap{
			beadmeta.AttemptEvidenceArchiveAttemptIDMetadataKey: "ae-one",
			beadmeta.AttemptEvidenceArchiveOwnerIDMetadataKey:   "work-one",
			beadmeta.AttemptEvidenceArchivePayloadMetadataKey:   "{}",
		},
	}
}

func protectedAttemptEvidencePayload() Bead {
	content := []byte("content")
	digest := fmt.Sprintf("%x", sha256.Sum256(content))
	return Bead{
		ID:     AttemptEvidencePayloadID(digest),
		Title:  "content-addressed attempt evidence payload",
		Type:   "molecule",
		Status: "closed",
		Metadata: StringMap{
			beadmeta.AttemptEvidencePayloadDigestMetadataKey: digest,
			beadmeta.AttemptEvidencePayloadDataMetadataKey:   base64.StdEncoding.EncodeToString(content),
		},
	}
}

func TestFileStoreDeleteProtectsAttemptEvidenceArchive(t *testing.T) {
	store, err := OpenFileStore(fsys.OSFS{}, filepath.Join(t.TempDir(), "beads.json"))
	if err != nil {
		t.Fatalf("OpenFileStore: %v", err)
	}
	archive, err := store.Create(protectedAttemptEvidenceBead())
	if err != nil {
		t.Fatalf("Create archive: %v", err)
	}
	changed := "tampered"
	if err := store.Update(archive.ID, UpdateOpts{Title: &changed}); !errors.Is(err, ErrImmutableAttemptEvidenceArchive) {
		t.Fatalf("Update archive error = %v, want immutable archive", err)
	}
	if err := store.SetMetadata(archive.ID, beadmeta.AttemptEvidenceArchivePayloadMetadataKey, "{}"); !errors.Is(err, ErrImmutableAttemptEvidenceArchive) {
		t.Fatalf("SetMetadata archive error = %v, want immutable archive", err)
	}
	if err := store.Close(archive.ID); err != nil {
		t.Fatalf("Close newly created archive: %v", err)
	}
	if err := store.Reopen(archive.ID); !errors.Is(err, ErrImmutableAttemptEvidenceArchive) {
		t.Fatalf("Reopen archive error = %v, want immutable archive", err)
	}
	if err := store.Delete(archive.ID); !errors.Is(err, ErrProtectedAttemptEvidenceArchive) {
		t.Fatalf("Delete archive error = %v, want protected archive", err)
	}
	stored, err := store.Get(archive.ID)
	if err != nil {
		t.Fatalf("Get archive: %v", err)
	}
	if err := store.DeleteIfMatch(archive.ID, stored.Revision); !errors.Is(err, ErrProtectedAttemptEvidenceArchive) {
		t.Fatalf("DeleteIfMatch archive error = %v, want protected archive", err)
	}
	if _, err := store.Get(archive.ID); err != nil {
		t.Fatalf("archive missing after rejected delete: %v", err)
	}
}

func TestSQLiteStoreDeleteAndBatchDeleteProtectAttemptEvidenceArchive(t *testing.T) {
	opened, err := OpenSQLiteStore(t.TempDir())
	if err != nil {
		t.Fatalf("OpenSQLiteStore: %v", err)
	}
	store := opened.(*SQLiteStore)
	t.Cleanup(func() { _ = store.CloseStore() })
	archive, err := store.Create(protectedAttemptEvidenceBead())
	if err != nil {
		t.Fatalf("Create archive: %v", err)
	}
	changed := "tampered"
	if err := store.Update(archive.ID, UpdateOpts{Title: &changed}); !errors.Is(err, ErrImmutableAttemptEvidenceArchive) {
		t.Fatalf("Update archive error = %v, want immutable archive", err)
	}
	if err := store.SetMetadata(archive.ID, beadmeta.AttemptEvidenceArchiveDigestMetadataKey, "tampered"); !errors.Is(err, ErrImmutableAttemptEvidenceArchive) {
		t.Fatalf("SetMetadata archive error = %v, want immutable archive", err)
	}
	if err := store.Close(archive.ID); err != nil {
		t.Fatalf("Close newly created archive: %v", err)
	}
	if err := store.Reopen(archive.ID); !errors.Is(err, ErrImmutableAttemptEvidenceArchive) {
		t.Fatalf("Reopen archive error = %v, want immutable archive", err)
	}
	if err := store.Delete(archive.ID); !errors.Is(err, ErrProtectedAttemptEvidenceArchive) {
		t.Fatalf("Delete archive error = %v, want protected archive", err)
	}
	stored, err := store.Get(archive.ID)
	if err != nil {
		t.Fatalf("Get archive: %v", err)
	}
	if err := store.DeleteIfMatch(archive.ID, stored.Revision); !errors.Is(err, ErrProtectedAttemptEvidenceArchive) {
		t.Fatalf("DeleteIfMatch archive error = %v, want protected archive", err)
	}
	if err := store.DeleteBatch([]string{archive.ID}); !errors.Is(err, ErrProtectedAttemptEvidenceArchive) {
		t.Fatalf("DeleteBatch archive error = %v, want protected archive", err)
	}
	if _, err := store.Get(archive.ID); err != nil {
		t.Fatalf("archive missing after rejected delete: %v", err)
	}
	payload, err := CreatePrivatePayloadValue(store, protectedAttemptEvidencePayload())
	if err != nil {
		t.Fatalf("Create payload: %v", err)
	}
	if err := store.Delete(payload.ID); !errors.Is(err, ErrProtectedAttemptEvidenceArchive) {
		t.Fatalf("Delete payload error = %v, want protected payload", err)
	}
	if err := store.DeleteIfMatch(payload.ID, payload.Revision); !errors.Is(err, ErrProtectedAttemptEvidenceArchive) {
		t.Fatalf("DeleteIfMatch payload error = %v, want protected payload", err)
	}
	if err := store.DeleteBatch([]string{payload.ID}); !errors.Is(err, ErrProtectedAttemptEvidenceArchive) {
		t.Fatalf("DeleteBatch payload error = %v, want protected payload", err)
	}
	if err := store.Update(payload.ID, UpdateOpts{Metadata: map[string]string{beadmeta.AttemptEvidencePayloadDataMetadataKey: "dGFtcGVyZWQ="}}); !errors.Is(err, ErrImmutableAttemptEvidencePayload) {
		t.Fatalf("Update payload error = %v, want immutable payload", err)
	}
	if err := store.SetMetadata(payload.ID, beadmeta.AttemptEvidencePayloadDataMetadataKey, "dGFtcGVyZWQ="); !errors.Is(err, ErrImmutableAttemptEvidencePayload) {
		t.Fatalf("SetMetadata payload error = %v, want immutable payload", err)
	}
}

func TestStoresRetainSessionRequestEvidenceOnDelete(t *testing.T) {
	openSQLite := func(t *testing.T) Store {
		opened, err := OpenSQLiteStore(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		store := opened.(*SQLiteStore)
		t.Cleanup(func() { _ = store.CloseStore() })
		return store
	}
	for name, open := range map[string]func(*testing.T) Store{
		"memory": func(*testing.T) Store { return NewMemStore() },
		"sqlite": openSQLite,
	} {
		t.Run(name, func(t *testing.T) {
			store := open(t)
			row, err := store.Create(Bead{Title: "session", Metadata: map[string]string{
				beadmeta.SessionRequestReceiptPrefix + "request-1": `{}`,
			}})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			if err := store.Delete(row.ID); !errors.Is(err, ErrRetainedSessionRequestEvidence) {
				t.Fatalf("Delete error = %v, want retained request evidence", err)
			}
			conditional := store.(ConditionalWriter)
			if err := conditional.DeleteIfMatch(row.ID, row.Revision); !errors.Is(err, ErrRetainedSessionRequestEvidence) {
				t.Fatalf("DeleteIfMatch error = %v, want retained request evidence", err)
			}
			if batch, ok := store.(BatchDeleter); ok {
				if err := batch.DeleteBatch([]string{row.ID}); !errors.Is(err, ErrRetainedSessionRequestEvidence) {
					t.Fatalf("DeleteBatch error = %v, want retained request evidence", err)
				}
			}
		})
	}
}

func TestSQLiteRetentionKeepsSessionRequestEvidence(t *testing.T) {
	opened, err := OpenSQLiteStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store := opened.(*SQLiteStore)
	t.Cleanup(func() { _ = store.CloseStore() })
	row, err := store.Create(Bead{Title: "closed session", Status: "closed", Metadata: map[string]string{
		beadmeta.SessionRequestReceiptPrefix + "request-1": `{}`,
	}})
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-48 * time.Hour).UnixNano()
	if _, err := store.db.Exec(`UPDATE beads SET updated_at=?, created_at=? WHERE id=?`, old, old, row.ID); err != nil {
		t.Fatal(err)
	}
	deleted, err := store.purgeTerminal(t.Context(), 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 0 {
		t.Fatalf("purge deleted %d rows, want retained session", deleted)
	}
	if _, err := store.Get(row.ID); err != nil {
		t.Fatalf("retained session missing after purge: %v", err)
	}
}

func TestSQLiteRetentionKeepsArchiveAndPurgesOtherTerminalRows(t *testing.T) {
	opened, err := OpenSQLiteStore(t.TempDir())
	if err != nil {
		t.Fatalf("OpenSQLiteStore: %v", err)
	}
	store := opened.(*SQLiteStore)
	t.Cleanup(func() { _ = store.CloseStore() })
	archive, err := store.Create(protectedAttemptEvidenceBead())
	if err != nil {
		t.Fatalf("Create archive: %v", err)
	}
	ordinary, err := store.Create(Bead{Title: "ordinary terminal row", Status: "closed"})
	if err != nil {
		t.Fatalf("Create ordinary row: %v", err)
	}
	old := time.Now().Add(-48 * time.Hour).UnixNano()
	if _, err := store.db.Exec(`UPDATE beads SET updated_at=?, created_at=? WHERE id IN (?, ?)`, old, old, archive.ID, ordinary.ID); err != nil {
		t.Fatalf("age terminal rows: %v", err)
	}
	deleted, err := store.purgeTerminal(t.Context(), 24*time.Hour)
	if err != nil {
		t.Fatalf("purgeTerminal: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("purge deleted %d rows, want only ordinary row", deleted)
	}
	if _, err := store.Get(archive.ID); err != nil {
		t.Fatalf("archive missing after retention: %v", err)
	}
	if _, err := store.Get(ordinary.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ordinary row after retention error = %v, want not found", err)
	}
}

func TestFileStorePayloadRowsAreImmutableAndProtectedFromDelete(t *testing.T) {
	store, err := OpenFileStore(fsys.OSFS{}, filepath.Join(t.TempDir(), "beads.json"))
	if err != nil {
		t.Fatalf("OpenFileStore: %v", err)
	}
	payload, err := CreatePrivatePayloadValue(store, protectedAttemptEvidencePayload())
	if err != nil {
		t.Fatalf("Create payload: %v", err)
	}
	if !IsAttemptEvidencePayload(payload) || !IsProtectedAttemptEvidenceRecord(payload) {
		t.Fatalf("payload row not recognized as protected: %+v", payload)
	}
	if err := store.Delete(payload.ID); !errors.Is(err, ErrProtectedAttemptEvidenceArchive) {
		t.Fatalf("Delete payload error = %v, want protected payload", err)
	}
	if err := store.Update(payload.ID, UpdateOpts{Metadata: map[string]string{beadmeta.AttemptEvidencePayloadDataMetadataKey: "dGFtcGVyZWQ="}}); !errors.Is(err, ErrImmutableAttemptEvidencePayload) {
		t.Fatalf("Update payload error = %v, want immutable payload", err)
	}
	if err := store.SetMetadata(payload.ID, beadmeta.AttemptEvidencePayloadDataMetadataKey, "dGFtcGVyZWQ="); !errors.Is(err, ErrImmutableAttemptEvidencePayload) {
		t.Fatalf("SetMetadata payload error = %v, want immutable payload", err)
	}
	if _, err := store.Get(payload.ID); err != nil {
		t.Fatalf("payload missing after rejected writes: %v", err)
	}
}

func TestSQLiteRetentionKeepsContentAddressedPayload(t *testing.T) {
	opened, err := OpenSQLiteStore(t.TempDir())
	if err != nil {
		t.Fatalf("OpenSQLiteStore: %v", err)
	}
	store := opened.(*SQLiteStore)
	t.Cleanup(func() { _ = store.CloseStore() })
	payload, err := CreatePrivatePayloadValue(store, protectedAttemptEvidencePayload())
	if err != nil {
		t.Fatalf("Create payload: %v", err)
	}
	ordinary, err := store.Create(Bead{Title: "ordinary terminal row", Status: "closed"})
	if err != nil {
		t.Fatalf("Create ordinary row: %v", err)
	}
	old := time.Now().Add(-48 * time.Hour).UnixNano()
	if _, err := store.db.Exec(`UPDATE beads SET updated_at=?, created_at=? WHERE id IN (?, ?)`, old, old, payload.ID, ordinary.ID); err != nil {
		t.Fatalf("age terminal rows: %v", err)
	}
	deleted, err := store.purgeTerminal(t.Context(), 24*time.Hour)
	if err != nil {
		t.Fatalf("purgeTerminal: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("purge deleted %d rows, want only ordinary row", deleted)
	}
	if _, err := store.Get(payload.ID); err != nil {
		t.Fatalf("payload missing after retention: %v", err)
	}
	if _, err := store.Get(ordinary.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ordinary row after retention error = %v, want not found", err)
	}
}

func TestOrdinaryStoreWritesCannotCreatePayloadMarkers(t *testing.T) {
	openFile := func(t *testing.T) Store {
		t.Helper()
		store, err := OpenFileStore(fsys.OSFS{}, filepath.Join(t.TempDir(), "beads.json"))
		if err != nil {
			t.Fatal(err)
		}
		return store
	}
	openSQLite := func(t *testing.T) Store {
		t.Helper()
		opened, err := OpenSQLiteStore(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		store := opened.(*SQLiteStore)
		t.Cleanup(func() { _ = store.CloseStore() })
		return store
	}

	for name, open := range map[string]func(*testing.T) Store{"file": openFile, "sqlite": openSQLite} {
		t.Run(name, func(t *testing.T) {
			store := open(t)
			marker := map[string]string{beadmeta.AttemptEvidencePayloadDigestMetadataKey: strings.Repeat("a", 64)}
			if _, err := store.Create(Bead{Title: "forged payload", Metadata: marker}); !errors.Is(err, ErrImmutableAttemptEvidencePayload) {
				t.Fatalf("Create marker error = %v, want immutable payload", err)
			}
			ordinary, err := store.Create(Bead{Title: "ordinary"})
			if err != nil {
				t.Fatal(err)
			}
			if err := store.Update(ordinary.ID, UpdateOpts{Metadata: marker}); !errors.Is(err, ErrImmutableAttemptEvidencePayload) {
				t.Fatalf("Update marker error = %v, want immutable payload", err)
			}
			if err := store.SetMetadata(ordinary.ID, beadmeta.AttemptEvidencePayloadDataMetadataKey, ""); !errors.Is(err, ErrImmutableAttemptEvidencePayload) {
				t.Fatalf("SetMetadata marker error = %v, want immutable payload", err)
			}
			if err := store.SetMetadataBatch(ordinary.ID, marker); !errors.Is(err, ErrImmutableAttemptEvidencePayload) {
				t.Fatalf("SetMetadataBatch marker error = %v, want immutable payload", err)
			}
			if _, err := store.CloseAll([]string{ordinary.ID}, marker); !errors.Is(err, ErrImmutableAttemptEvidencePayload) {
				t.Fatalf("CloseAll marker error = %v, want immutable payload", err)
			}
			writer, ok := ConditionalWriterFor(store)
			if !ok {
				t.Fatalf("%T has no conditional writer", store)
			}
			current, err := store.Get(ordinary.ID)
			if err != nil {
				t.Fatal(err)
			}
			if err := writer.UpdateIfMatch(ordinary.ID, current.Revision, UpdateOpts{Metadata: marker}); !errors.Is(err, ErrImmutableAttemptEvidencePayload) {
				t.Fatalf("UpdateIfMatch marker error = %v, want immutable payload", err)
			}
			if _, err := writer.CompareAndSetMetadataKey(ordinary.ID, beadmeta.AttemptEvidencePayloadDataMetadataKey, "", "forged"); !errors.Is(err, ErrImmutableAttemptEvidencePayload) {
				t.Fatalf("metadata CAS marker error = %v, want immutable payload", err)
			}
		})
	}
}

func TestSQLitePartialPayloadMarkersAreProtectedFromDeleteAndRetention(t *testing.T) {
	opened, err := OpenSQLiteStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store := opened.(*SQLiteStore)
	t.Cleanup(func() { _ = store.CloseStore() })

	partial, err := store.CreateWithForeignID(Bead{
		ID: "legacy-partial-payload", Title: "malformed restored payload", Status: "closed",
	})
	if err != nil {
		t.Fatalf("create legacy payload row: %v", err)
	}
	if _, err := store.db.Exec(`INSERT INTO metadata(bead_id,meta_key,meta_value) VALUES(?,?,?)`, partial.ID, beadmeta.AttemptEvidencePayloadDataMetadataKey, ""); err != nil {
		t.Fatalf("inject legacy partial payload marker: %v", err)
	}
	partial.Metadata = StringMap{beadmeta.AttemptEvidencePayloadDataMetadataKey: ""}
	if !IsProtectedAttemptEvidenceRecord(partial) {
		t.Fatal("partial payload marker was not classified as protected")
	}
	if err := store.Delete(partial.ID); !errors.Is(err, ErrProtectedAttemptEvidenceArchive) {
		t.Fatalf("Delete partial payload error = %v, want protected record", err)
	}
	old := time.Now().Add(-48 * time.Hour).UnixNano()
	if _, err := store.db.Exec(`UPDATE beads SET updated_at=?, created_at=? WHERE id=?`, old, old, partial.ID); err != nil {
		t.Fatal(err)
	}
	deleted, err := store.purgeTerminal(t.Context(), 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 0 {
		t.Fatalf("purge deleted %d rows, want partial marker retained", deleted)
	}
	if _, err := store.Get(partial.ID); err != nil {
		t.Fatalf("partial payload missing after retention: %v", err)
	}
}

func TestSQLitePartialArchiveMarkersAreImmutableAndRetained(t *testing.T) {
	opened, err := OpenSQLiteStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	store := opened.(*SQLiteStore)
	t.Cleanup(func() { _ = store.CloseStore() })

	partial, err := store.CreateWithForeignID(Bead{
		ID: "legacy-partial-archive", Title: "malformed restored archive", Status: "closed",
		Metadata: StringMap{beadmeta.AttemptEvidenceArchiveDigestMetadataKey: ""},
	})
	if err != nil {
		t.Fatalf("restore partial archive marker: %v", err)
	}
	if !IsProtectedAttemptEvidenceRecord(partial) {
		t.Fatal("partial archive marker was not classified as protected")
	}
	changed := "tampered"
	if err := store.Update(partial.ID, UpdateOpts{Title: &changed}); !errors.Is(err, ErrImmutableAttemptEvidenceArchive) {
		t.Fatalf("Update partial archive error = %v, want immutable archive", err)
	}
	if err := store.Delete(partial.ID); !errors.Is(err, ErrProtectedAttemptEvidenceArchive) {
		t.Fatalf("Delete partial archive error = %v, want protected record", err)
	}
	old := time.Now().Add(-48 * time.Hour).UnixNano()
	if _, err := store.db.Exec(`UPDATE beads SET updated_at=?, created_at=? WHERE id=?`, old, old, partial.ID); err != nil {
		t.Fatal(err)
	}
	deleted, err := store.purgeTerminal(t.Context(), 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 0 {
		t.Fatalf("purge deleted %d rows, want partial archive retained", deleted)
	}
	if _, err := store.Get(partial.ID); err != nil {
		t.Fatalf("partial archive missing after retention: %v", err)
	}
}

func TestNativeEvidenceStatusMutationGuardsPreserveMalformedLegacyCompatibility(t *testing.T) {
	payload := protectedAttemptEvidencePayload()
	raw, err := metadataRawFromMap(payload.Metadata)
	if err != nil {
		t.Fatal(err)
	}
	partialRaw, err := metadataRawFromMap(StringMap{beadmeta.AttemptEvidencePayloadDataMetadataKey: ""})
	if err != nil {
		t.Fatal(err)
	}
	for _, issue := range []*beadslib.Issue{
		{ID: payload.ID, Metadata: json.RawMessage(`not-json`)},
		{ID: "legacy-payload-copy", Metadata: raw},
		{ID: "partial-payload-copy", Metadata: partialRaw},
	} {
		if err := protectNativeAttemptEvidencePayloadIssue(issue); !errors.Is(err, ErrImmutableAttemptEvidencePayload) {
			t.Fatalf("protect payload issue %q error = %v, want immutable payload", issue.ID, err)
		}
	}
	if err := protectNativeAttemptEvidencePayloadIssue(&beadslib.Issue{ID: "ordinary", Metadata: json.RawMessage(`not-json`)}); err != nil {
		t.Fatalf("ordinary malformed legacy metadata was blocked: %v", err)
	}
	archiveRaw, err := metadataRawFromMap(protectedAttemptEvidenceBead().Metadata)
	if err != nil {
		t.Fatal(err)
	}
	if err := protectNativeAttemptEvidenceRecordIssue(&beadslib.Issue{ID: "archive", Metadata: archiveRaw}); !errors.Is(err, ErrImmutableAttemptEvidenceArchive) {
		t.Fatalf("protect native archive error = %v, want immutable archive", err)
	}
	if err := protectNativeAttemptEvidenceRecordIssue(&beadslib.Issue{ID: "ordinary", Metadata: json.RawMessage(`not-json`)}); err != nil {
		t.Fatalf("ordinary malformed legacy metadata was blocked by record guard: %v", err)
	}
}
