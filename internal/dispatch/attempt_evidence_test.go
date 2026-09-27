package dispatch

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/attemptevidence"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/formula"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/testutil"
)

func TestRetryEvidenceCaptureFailureBlocksSpawnAndKeepsBothAttempts(t *testing.T) {
	store, err := beads.OpenFileStore(fsys.OSFS{}, filepath.Join(t.TempDir(), "beads.json"))
	if err != nil {
		t.Fatal(err)
	}
	repo, baseSHA := newDispatchEvidenceRepo(t)
	step := &formula.Step{
		ID: "review", Title: "Review", Type: "task",
		Metadata: map[string]string{
			beadmeta.WorkDirMetadataKey:         repo,
			beadmeta.WorktreeBaseSHAMetadataKey: baseSHA,
		},
		Retry: &formula.RetrySpec{MaxAttempts: 3},
	}
	root, control := makeRetryControl(t, store, "mol-evidence.review", step, 3)
	attempt1 := makeAttemptBead(t, store, root.ID, "mol-evidence.review.attempt.1", 1, map[string]string{
		beadmeta.OutcomeMetadataKey:         beadmeta.OutcomeFail,
		beadmeta.FailureClassMetadataKey:    beadmeta.FailureClassTransient,
		beadmeta.WorkDirMetadataKey:         repo,
		beadmeta.WorktreeBaseSHAMetadataKey: baseSHA,
	})
	mustDep(t, store, control.ID, attempt1.ID, "blocks")

	firstFailure := true
	var first attemptevidence.Evidence
	callback := func(ctx context.Context, control, attempt beads.Bead, attemptNum int, outcome string) error {
		if firstFailure {
			firstFailure = false
			return errors.New("archive write failed")
		}
		evidence, err := attemptevidence.Capture(ctx, store, attemptevidence.CaptureSpec{
			Identity: attemptevidence.Identity{Kind: attemptevidence.KindRetry, OwnerBeadID: control.ID, ExecutionBeadID: attempt.ID},
			StoreRef: "rig:tests", WorkDir: attempt.Metadata[beadmeta.WorkDirMetadataKey],
			BaseSHA: attempt.Metadata[beadmeta.WorktreeBaseSHAMetadataKey],
			Outcome: "attempt_" + strconv.Itoa(attemptNum) + ":" + outcome,
		})
		if err == nil && attemptNum == 1 {
			first = evidence
		}
		return err
	}
	opts := ProcessOptions{CaptureAttemptEvidence: callback}
	if _, err := processRetryControl(store, mustGet(t, store, control.ID), opts); !errors.Is(err, ErrControlPending) {
		t.Fatalf("capture failure error = %v, want ErrControlPending", err)
	}
	if got := mustGet(t, store, control.ID); got.Status != "open" || got.Metadata[beadmeta.AttemptLogMetadataKey] != "" {
		t.Fatalf("control advanced after capture failure: status=%s log=%q", got.Status, got.Metadata[beadmeta.AttemptLogMetadataKey])
	}
	if next := findAttemptByRef(t, store, root.ID, "mol-evidence.review.attempt.2"); next.ID != "" {
		t.Fatalf("attempt 2 spawned before attempt 1 evidence persisted: %s", next.ID)
	}

	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("attempt one result\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	result, err := processRetryControl(store, mustGet(t, store, control.ID), opts)
	if err != nil || result.Action != "retry" {
		t.Fatalf("retry after durable capture: result=%#v err=%v", result, err)
	}
	if first.AttemptID == "" {
		t.Fatal("attempt 1 evidence callback did not capture a record")
	}

	attempt2 := findAttemptByRef(t, store, root.ID, "mol-evidence.review.attempt.2")
	if attempt2.ID == "" {
		t.Fatal("attempt 2 was not spawned after successful evidence persistence")
	}
	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("attempt two result\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := store.SetMetadata(attempt2.ID, beadmeta.OutcomeMetadataKey, beadmeta.OutcomePass); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(attempt2.ID); err != nil {
		t.Fatal(err)
	}
	result, err = processRetryControl(store, mustGet(t, store, control.ID), opts)
	if err != nil || result.Action != "pass" {
		t.Fatalf("pass after second capture: result=%#v err=%v", result, err)
	}

	secondID, err := attemptevidence.AttemptID(attemptevidence.Identity{
		Kind: attemptevidence.KindRetry, OwnerBeadID: control.ID, ExecutionBeadID: attempt2.ID,
	})
	if err != nil {
		t.Fatal(err)
	}
	firstRead, err := attemptevidence.Read(store, control.ID, first.AttemptID)
	if err != nil {
		t.Fatalf("read attempt 1: %v", err)
	}
	secondRead, err := attemptevidence.Read(store, control.ID, secondID)
	if err != nil {
		t.Fatalf("read attempt 2: %v", err)
	}
	firstPatch, _, err := attemptevidence.DecodeDiff(firstRead.WorkspaceDiff)
	if err != nil {
		t.Fatal(err)
	}
	secondPatch, _, err := attemptevidence.DecodeDiff(secondRead.WorkspaceDiff)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(firstPatch), "attempt one result") || strings.Contains(string(firstPatch), "attempt two result") {
		t.Fatalf("attempt 1 diff changed after retry: %s", firstPatch)
	}
	if !strings.Contains(string(secondPatch), "attempt two result") {
		t.Fatalf("attempt 2 diff missing its own result: %s", secondPatch)
	}
}

func newDispatchEvidenceRepo(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		return testutil.RunGit(t, root, args...)
	}
	git("init", "-q")
	git("config", "user.email", "dispatch-evidence-test@example.invalid")
	git("config", "user.name", "Dispatch Evidence Test")
	if err := os.WriteFile(filepath.Join(root, "tracked.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "tracked.txt")
	git("commit", "-q", "-m", "base")
	return root, git("rev-parse", "HEAD")
}
