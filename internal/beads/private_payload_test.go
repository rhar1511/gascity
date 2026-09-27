package beads

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/fsys"
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

func TestFileStoreDeleteProtectsAttemptEvidenceArchive(t *testing.T) {
	store, err := OpenFileStore(fsys.OSFS{}, filepath.Join(t.TempDir(), "beads.json"))
	if err != nil {
		t.Fatalf("OpenFileStore: %v", err)
	}
	archive, err := store.Create(protectedAttemptEvidenceBead())
	if err != nil {
		t.Fatalf("Create archive: %v", err)
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
