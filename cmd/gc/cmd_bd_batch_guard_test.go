package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// writeBatchGuardBdStub replaces the fixture's bd with one that logs every
// call and answers `show` from the case arms given.
func writeBatchGuardBdStub(t *testing.T, showArms string) string {
	t.Helper()
	binDir := t.TempDir()
	logPath := filepath.Join(t.TempDir(), "bd-calls.log")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + logPath + "\n" +
		"if [ \"$1\" = show ]; then\n  case \"$*\" in\n" + showArms + "\n  esac\n  exit 0\nfi\nexit 0\n"
	if err := os.WriteFile(filepath.Join(binDir, "bd"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return logPath
}

// A bulk close (the maintenance orders close stale wisps in batches) verifies
// every id with one bd show. An unrevisioned backend cannot safely close the
// observed rows: a concurrent purge fence must never be overwritten.
func TestGcBdBulkCloseVerifiesIDsWithOneShow(t *testing.T) {
	_, cityDir := bdSQLRefusalCityDir(t, "")
	setCwd(t, cityDir)
	calls := writeBatchGuardBdStub(t, `    "show --json demo-a demo-wisp-b demo-c")
      printf '[{"id":"demo-a","status":"open"},{"id":"demo-wisp-b","status":"open"},{"id":"demo-c","status":"open"}]\n' ;;`)

	var stdout, stderr bytes.Buffer
	if code := doBd([]string{"close", "demo-a", "demo-wisp-b", "demo-c", "--reason", "stale"}, &stdout, &stderr); code != 1 {
		t.Fatalf("doBd close = %d, stderr=%q", code, stderr.String())
	}
	data, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	log := string(data)
	if got := strings.Count(log, "show "); got != 1 {
		t.Fatalf("bd show ran %d times, want 1 batched verification:\n%s", got, log)
	}
	if strings.Contains(log, "close ") || strings.Contains(log, "update ") {
		t.Fatalf("unrevisioned batch was mutated:\n%s", log)
	}
}

func TestFencedBatchClosePreservesReasonAndRejectsStaleRevision(t *testing.T) {
	store := beads.NewMemStore()
	observed := map[string]beads.Bead{}
	var ids []string
	for i := 0; i < 2; i++ {
		b, err := store.Create(beads.Bead{Title: "stale work", Type: "task"})
		if err != nil {
			t.Fatal(err)
		}
		observed[b.ID] = b
		ids = append(ids, b.ID)
	}
	// Another writer wins the second row's fence. The first close stands,
	// but the second must retain its current state and expose partial success.
	if err := store.SetMetadata(ids[1], "new_owner", "current"); err != nil {
		t.Fatal(err)
	}
	written, err := closeBdBatchAtRevisions(store, observed, ids, "stale")
	if !beads.IsPreconditionFailed(err) || len(written) != 1 {
		t.Fatalf("batch=%v, %v", written, err)
	}
	first, _ := store.Get(ids[0])
	second, _ := store.Get(ids[1])
	if first.Status != "closed" || first.Metadata["close_reason"] != "stale" || second.Status != "open" || second.Metadata["new_owner"] != "current" {
		t.Fatalf("first=%+v second=%+v", first, second)
	}
}

func TestFencedBatchCloseChecksEveryPurgeFenceBeforeWriting(t *testing.T) {
	store := beads.NewMemStore()
	observed := map[string]beads.Bead{}
	var ids []string
	for i := 0; i < 2; i++ {
		b, err := store.Create(beads.Bead{Title: "work", Type: "task"})
		if err != nil {
			t.Fatal(err)
		}
		if i == 1 {
			if err := store.SetMetadata(b.ID, beadmeta.SessionRequestPurgeFenceMetadataKey, "purging"); err != nil {
				t.Fatal(err)
			}
			b, err = store.Get(b.ID)
			if err != nil {
				t.Fatal(err)
			}
		}
		observed[b.ID] = b
		ids = append(ids, b.ID)
	}
	if written, err := closeBdBatchAtRevisions(store, observed, ids, "stale"); err == nil || len(written) != 0 {
		t.Fatalf("batch=%v error=%v", written, err)
	}
	for _, id := range ids {
		b, err := store.Get(id)
		if err != nil || b.Status != "open" {
			t.Fatalf("%s=%+v %v", id, b, err)
		}
	}
}

// The batch read never weakens the exact-ID guard (gcy-g4o): an id bd did not
// answer verbatim is resolved on its own, and a substring collision still
// refuses the whole write.
func TestGcBdBulkCloseStillRefusesASubstringCollision(t *testing.T) {
	_, cityDir := bdSQLRefusalCityDir(t, "")
	setCwd(t, cityDir)
	calls := writeBatchGuardBdStub(t, `    "show --json demo-a demo-dv7")
      printf '[{"id":"demo-a","status":"open"},{"id":"demo-wisp-dv78","status":"open"}]\n' ;;
    "show --json demo-dv7")
      printf '[{"id":"demo-wisp-dv78","status":"open"}]\n' ;;`)

	var stdout, stderr bytes.Buffer
	if code := doBd([]string{"close", "demo-a", "demo-dv7"}, &stdout, &stderr); code != 1 {
		t.Fatalf("doBd close = %d, want 1 for a substring collision; stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "substring collision") {
		t.Fatalf("stderr = %q, want the collision refusal", stderr.String())
	}
	data, err := os.ReadFile(calls)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "close ") {
		t.Fatalf("bd close ran despite the collision:\n%s", data)
	}
}
