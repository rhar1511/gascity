package attemptevidence

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/fsys"
)

func TestCapturePreservesExactAttemptAfterOwnerDeleteAndStoreRestart(t *testing.T) {
	storePath := filepath.Join(t.TempDir(), "beads.json")
	store, err := beads.OpenFileStore(fsys.OSFS{}, storePath)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := store.Create(beads.Bead{Title: "source work", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	repo, baseSHA := newEvidenceRepo(t)
	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("candidate\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "new.txt"), []byte("untracked bytes\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	spec := CaptureSpec{
		Identity: Identity{
			Kind: KindWorkbench, OwnerBeadID: owner.ID, ExecutionBeadID: owner.ID,
			SessionID: "session-1", SessionGeneration: "4", ClaimGeneration: "9",
		},
		StoreRef: "rig:pilot", WorkDir: repo, BaseSHA: baseSHA, Outcome: "failed",
		Now: func() time.Time { return time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC) },
	}
	first, err := Capture(context.Background(), store, spec)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if first.SourceStatus != StatusAvailable || first.BaseSHA != baseSHA || first.CandidateSHA == "" {
		t.Fatalf("capture revision/status facts = %#v", first)
	}
	tracked, untracked, err := DecodeDiff(first.WorkspaceDiff)
	if err != nil {
		t.Fatalf("DecodeDiff: %v", err)
	}
	if !strings.Contains(string(tracked), "+candidate") || !strings.Contains(string(tracked), "-base") {
		t.Fatalf("tracked patch did not preserve exact change: %s", tracked)
	}
	if len(untracked) != 1 || untracked[0].Path != "new.txt" || string(untracked[0].Bytes) != "untracked bytes\n" || untracked[0].Mode&0o777 != 0o600 {
		t.Fatalf("untracked snapshot = %#v", untracked)
	}
	archives, err := store.ListByMetadata(map[string]string{beadmeta.AttemptEvidenceArchiveOwnerIDMetadataKey: owner.ID}, 0, beads.IncludeClosed)
	if err != nil {
		t.Fatal(err)
	}
	if len(archives) != 1 || !IsArchiveRecord(archives[0]) {
		t.Fatalf("archive rows = %#v", archives)
	}
	ready, err := store.Ready()
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range ready {
		if row.ID == archives[0].ID {
			t.Fatal("attempt archive appeared in actionable Ready work")
		}
	}

	// A later branch/worktree state cannot replace the first attempt snapshot.
	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("later branch\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	second, err := Capture(context.Background(), store, spec)
	if err != nil {
		t.Fatalf("idempotent Capture: %v", err)
	}
	if !samePayload(first, second) {
		t.Fatal("a later workspace state replaced the first sealed attempt")
	}
	if err := store.Delete(owner.ID); err != nil {
		t.Fatalf("delete source owner after archive: %v", err)
	}

	store, err = beads.OpenFileStore(fsys.OSFS{}, storePath)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Read(store, owner.ID, first.AttemptID)
	if err != nil {
		t.Fatalf("Read after owner deletion/reopen: %v", err)
	}
	if !samePayload(first, got) {
		t.Fatal("reopened archive did not return the exact sealed payload")
	}
	if first.Permission.StoreRef != spec.StoreRef || first.Permission.WorkID != owner.ID ||
		first.Permission.RepositoryRoot != filepath.Join(repo, ".git") || first.Permission.WorkspaceRoot != repo {
		t.Fatalf("captured permission scope = %+v, want exact repo/work/store scope", first.Permission)
	}
	backupDir := filepath.Join(t.TempDir(), "backup")
	if err := os.MkdirAll(backupDir, 0o755); err != nil {
		t.Fatal(err)
	}
	backupBytes, err := os.ReadFile(storePath)
	if err != nil {
		t.Fatalf("read quiescent FileStore backup source: %v", err)
	}
	backupPath := filepath.Join(backupDir, "beads.json")
	if err := os.WriteFile(backupPath, backupBytes, 0o600); err != nil {
		t.Fatalf("write FileStore backup copy: %v", err)
	}
	backupStore, err := beads.OpenFileStore(fsys.OSFS{}, backupPath)
	if err != nil {
		t.Fatalf("open FileStore backup copy: %v", err)
	}
	backupEvidence, err := Read(backupStore, owner.ID, first.AttemptID)
	if err != nil {
		t.Fatalf("read evidence from FileStore backup copy: %v", err)
	}
	if !samePayload(first, backupEvidence) || backupEvidence.Diff.SHA256 != first.Diff.SHA256 || backupEvidence.Permission != first.Permission {
		t.Fatal("FileStore backup/restore lost the exact diff digest or original permission scope")
	}
	refs, err := References(store, "rig:pilot", owner.ID)
	if err != nil {
		t.Fatalf("References after owner deletion: %v", err)
	}
	if len(refs) != 1 || refs[0].AttemptID != first.AttemptID || refs[0].WorkID != owner.ID || refs[0].DiffSHA256 != first.Diff.SHA256 {
		t.Fatalf("exact references = %#v", refs)
	}
}

func TestSQLiteColdCopyBackupPreservesExactArchiveAfterOwnerDeletion(t *testing.T) {
	sourceDir := filepath.Join(t.TempDir(), "source")
	opened, err := beads.OpenSQLiteStore(sourceDir)
	if err != nil {
		t.Fatalf("OpenSQLiteStore source: %v", err)
	}
	store := opened.(*beads.SQLiteStore)
	t.Cleanup(func() { _ = store.CloseStore() })
	owner, err := store.Create(beads.Bead{Title: "source work", Type: "task"})
	if err != nil {
		t.Fatalf("Create owner: %v", err)
	}
	repo, baseSHA := newEvidenceRepo(t)
	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("candidate\n"), 0o644); err != nil {
		t.Fatalf("write candidate: %v", err)
	}
	spec := CaptureSpec{
		Identity: Identity{Kind: KindRetry, OwnerBeadID: owner.ID, ExecutionBeadID: "sqlite-attempt"},
		StoreRef: "city:test-city", WorkDir: repo, BaseSHA: baseSHA,
	}
	want, err := Capture(context.Background(), store, spec)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if err := store.Delete(owner.ID); err != nil {
		t.Fatalf("delete source owner after archive: %v", err)
	}
	if err := store.CloseStore(); err != nil {
		t.Fatalf("close source SQLite store before cold copy: %v", err)
	}
	backupDir := filepath.Join(t.TempDir(), "backup")
	if err := os.MkdirAll(backupDir, 0o755); err != nil {
		t.Fatal(err)
	}
	backupBytes, err := os.ReadFile(filepath.Join(sourceDir, "beads.sqlite"))
	if err != nil {
		t.Fatalf("read quiescent SQLite backup source: %v", err)
	}
	if err := os.WriteFile(filepath.Join(backupDir, "beads.sqlite"), backupBytes, 0o600); err != nil {
		t.Fatalf("write SQLite backup copy: %v", err)
	}
	backupOpened, err := beads.OpenSQLiteStore(backupDir)
	if err != nil {
		t.Fatalf("restore SQLite backup copy: %v", err)
	}
	backupStore := backupOpened.(*beads.SQLiteStore)
	t.Cleanup(func() { _ = backupStore.CloseStore() })
	got, err := Read(backupStore, owner.ID, want.AttemptID)
	if err != nil {
		t.Fatalf("read restored archive: %v", err)
	}
	if !samePayload(want, got) || got.Diff.SHA256 != want.Diff.SHA256 || got.Permission != want.Permission {
		t.Fatal("SQLite backup/restore lost the exact diff digest or original permission scope")
	}
}

func TestCaptureMissingSourceIsExplicitButCaptureFailureIsNotSealed(t *testing.T) {
	store := newAttemptEvidenceStore(t)
	owner, err := store.Create(beads.Bead{Title: "source work", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	spec := CaptureSpec{
		Identity: Identity{Kind: KindRetry, OwnerBeadID: owner.ID, ExecutionBeadID: "attempt-1"},
		StoreRef: "rig:pilot", WorkDir: filepath.Join(t.TempDir(), "already-removed"),
	}
	evidence, err := Capture(context.Background(), store, spec)
	if err != nil {
		t.Fatalf("missing source should be explicitly sealed: %v", err)
	}
	if evidence.SourceStatus != StatusMissing || evidence.Diff.Status != StatusUnavailable {
		t.Fatalf("missing source was not explicit: %#v", evidence)
	}

	spec.Identity.ExecutionBeadID = "attempt-2"
	spec.WorkDir = t.TempDir() // Exists but is not a git repository.
	if _, err := Capture(context.Background(), store, spec); err == nil {
		t.Fatal("capture failure was treated as an unavailable source")
	}
	secondID, err := AttemptID(spec.Identity)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Read(store, owner.ID, secondID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("failed capture left an evidence record: %v", err)
	}
}

func TestMissingWorkspaceKeepsOnlyCanonicalRepositoryPermissionScope(t *testing.T) {
	repo, _ := newEvidenceRepo(t)
	store := newAttemptEvidenceStore(t)
	owner, err := store.Create(beads.Bead{Title: "retired worktree", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	missingWorkspace := filepath.Join(t.TempDir(), "removed-worktree")
	evidence, err := Capture(context.Background(), store, CaptureSpec{
		Identity: Identity{
			Kind: KindWorkbench, OwnerBeadID: owner.ID, ExecutionBeadID: owner.ID,
			SessionID: "session-1", SessionGeneration: "3", ClaimGeneration: "5",
		},
		StoreRef: "rig:pilot", WorkDir: missingWorkspace,
		Permission: PermissionScope{StoreRef: "rig:pilot", WorkID: owner.ID, RepositoryRoot: repo},
	})
	if err != nil {
		t.Fatalf("Capture missing worktree: %v", err)
	}
	if evidence.SourceStatus != StatusMissing || evidence.Permission.RepositoryRoot != filepath.Join(repo, ".git") || evidence.Permission.WorkspaceRoot != missingWorkspace {
		t.Fatalf("missing-source status/scope = %q %+v", evidence.SourceStatus, evidence.Permission)
	}
}

func TestCandidateDiffUsesOnlyResolvedCommitTreesWhileWorkspaceDiffPreservesLocalChanges(t *testing.T) {
	repo, baseSHA := newEvidenceRepo(t)
	runEvidenceGit(t, repo, "checkout", "-b", "candidate")
	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("committed candidate\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runEvidenceGit(t, repo, "add", "tracked.txt")
	runEvidenceGit(t, repo, "commit", "-q", "-m", "candidate")
	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("local uncommitted edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "untracked.txt"), []byte("untracked local file\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	store := newAttemptEvidenceStore(t)
	owner, err := store.Create(beads.Bead{Title: "candidate work", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	spec := CaptureSpec{
		Identity: Identity{Kind: KindRetry, OwnerBeadID: owner.ID, ExecutionBeadID: "attempt-commit-delta"},
		StoreRef: "rig:pilot", WorkDir: repo, BaseSHA: baseSHA,
	}
	evidence, err := Capture(context.Background(), store, spec)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if evidence.Diff.Source != DiffSourceCandidateCommitDelta || evidence.WorkingTreeStatus != WorkingTreeDirty {
		t.Fatalf("candidate/worktree provenance = diff %q, worktree %q", evidence.Diff.Source, evidence.WorkingTreeStatus)
	}
	commitPatch, commitFiles, err := DecodeDiff(evidence.Diff)
	if err != nil {
		t.Fatalf("decode candidate commit delta: %v", err)
	}
	if len(commitFiles) != 0 || !strings.Contains(string(commitPatch), "+committed candidate") || strings.Contains(string(commitPatch), "local uncommitted edit") {
		t.Fatalf("candidate diff included mutable worktree content: patch=%q files=%#v", commitPatch, commitFiles)
	}
	workspacePatch, workspaceFiles, err := DecodeDiff(evidence.WorkspaceDiff)
	if err != nil {
		t.Fatalf("decode workspace diff: %v", err)
	}
	if !strings.Contains(string(workspacePatch), "+local uncommitted edit") || len(workspaceFiles) != 1 || workspaceFiles[0].Path != "untracked.txt" {
		t.Fatalf("workspace diff lost local changes: patch=%q files=%#v", workspacePatch, workspaceFiles)
	}
	ref := EvidenceReference(evidence, "rig:pilot")
	if ref.DiffSource != DiffSourceCandidateCommitDelta || ref.DiffSHA256 != evidence.Diff.SHA256 {
		t.Fatalf("reference did not identify immutable candidate diff: %#v", ref)
	}
}

func TestGitCaptureDisablesConfiguredFSMonitorWithoutExecutingIt(t *testing.T) {
	repo, baseSHA := newEvidenceRepo(t)
	marker := filepath.Join(t.TempDir(), "fsmonitor-ran")
	helper := filepath.Join(t.TempDir(), "fsmonitor.sh")
	if err := os.WriteFile(helper, []byte("#!/bin/sh\nprintf ran > \""+marker+"\"\nprintf ''\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	runEvidenceGit(t, repo, "config", "core.fsmonitor", helper)
	store := newAttemptEvidenceStore(t)
	owner, err := store.Create(beads.Bead{Title: "fsmonitor work", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := Capture(context.Background(), store, CaptureSpec{
		Identity: Identity{Kind: KindRetry, OwnerBeadID: owner.ID, ExecutionBeadID: "fsmonitor-attempt"},
		StoreRef: "rig:pilot", WorkDir: repo, BaseSHA: baseSHA,
	})
	if err != nil {
		t.Fatalf("Capture with configured fsmonitor: %v", err)
	}
	if evidence.SourceStatus != StatusAvailable {
		t.Fatalf("capture source status = %q", evidence.SourceStatus)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("configured fsmonitor ran or marker check failed: %v", err)
	}
}

func TestGitCaptureRejectsConfiguredFilterWithoutExecutingIt(t *testing.T) {
	repo, baseSHA := newEvidenceRepo(t)
	marker := filepath.Join(t.TempDir(), "filter-ran")
	helper := filepath.Join(t.TempDir(), "filter.sh")
	if err := os.WriteFile(helper, []byte("#!/bin/sh\nprintf ran > \""+marker+"\"\ncat\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	runEvidenceGit(t, repo, "config", "filter.probe.clean", helper)
	if err := os.WriteFile(filepath.Join(repo, ".gitattributes"), []byte("tracked.txt filter=probe\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	store := newAttemptEvidenceStore(t)
	owner, err := store.Create(beads.Bead{Title: "filter work", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = Capture(context.Background(), store, CaptureSpec{
		Identity: Identity{Kind: KindRetry, OwnerBeadID: owner.ID, ExecutionBeadID: "filter-attempt"},
		StoreRef: "rig:pilot", WorkDir: repo, BaseSHA: baseSHA,
	})
	if err == nil || !strings.Contains(err.Error(), "external content filter") {
		t.Fatalf("Capture with configured content filter = %v, want fail-closed configuration error", err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("configured content filter ran or marker check failed: %v", err)
	}
}

func TestGitCaptureRejectsIncludedConfigBeforeExecutingIncludedHelpers(t *testing.T) {
	repo, baseSHA := newEvidenceRepo(t)
	marker := filepath.Join(t.TempDir(), "included-helper-ran")
	helper := filepath.Join(t.TempDir(), "included-fsmonitor.sh")
	if err := os.WriteFile(helper, []byte("#!/bin/sh\nprintf ran > \""+marker+"\"\nprintf ''\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	included := filepath.Join(t.TempDir(), "included.gitconfig")
	if err := os.WriteFile(included, []byte("[core]\n\tfsmonitor = "+helper+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runEvidenceGit(t, repo, "config", "include.path", included)
	store := newAttemptEvidenceStore(t)
	owner, err := store.Create(beads.Bead{Title: "included config work", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = Capture(context.Background(), store, CaptureSpec{
		Identity: Identity{Kind: KindRetry, OwnerBeadID: owner.ID, ExecutionBeadID: "included-config-attempt"},
		StoreRef: "rig:pilot", WorkDir: repo, BaseSHA: baseSHA,
	})
	if err == nil || !strings.Contains(err.Error(), "included Git configuration") {
		t.Fatalf("Capture with included helper config = %v, want fail-closed config error", err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("helper from included config ran or marker check failed: %v", err)
	}
}

func TestGitCaptureIgnoresReplacementObjectsAndBindsCandidateSHA(t *testing.T) {
	repo, baseSHA := newEvidenceRepo(t)
	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("actual candidate\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runEvidenceGit(t, repo, "add", "tracked.txt")
	runEvidenceGit(t, repo, "commit", "-q", "-m", "actual candidate")
	candidateSHA := runEvidenceGit(t, repo, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("replacement tree\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runEvidenceGit(t, repo, "add", "tracked.txt")
	runEvidenceGit(t, repo, "commit", "-q", "-m", "replacement object")
	replacementSHA := runEvidenceGit(t, repo, "rev-parse", "HEAD")
	runEvidenceGit(t, repo, "replace", candidateSHA, replacementSHA)
	runEvidenceGit(t, repo, "checkout", "--detach", candidateSHA)
	store := newAttemptEvidenceStore(t)
	owner, err := store.Create(beads.Bead{Title: "replacement work", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := Capture(context.Background(), store, CaptureSpec{
		Identity: Identity{Kind: KindRetry, OwnerBeadID: owner.ID, ExecutionBeadID: "replace-attempt"},
		StoreRef: "rig:pilot", WorkDir: repo, BaseSHA: baseSHA,
	})
	if err != nil {
		t.Fatalf("Capture with replacement ref: %v", err)
	}
	if evidence.CandidateSHA != candidateSHA {
		t.Fatalf("candidate SHA = %q, want original commit %q", evidence.CandidateSHA, candidateSHA)
	}
	patch, _, err := DecodeDiff(evidence.Diff)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(patch), "+actual candidate") || strings.Contains(string(patch), "replacement tree") {
		t.Fatalf("commit delta followed replacement object: %s", patch)
	}
}

func TestPermissionScopeUsesCanonicalSeparateGitDirectory(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "workspace")
	commonDir := filepath.Join(t.TempDir(), "git-common")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	runEvidenceGit(t, workspace, "init", "-q", "--separate-git-dir", commonDir)
	runEvidenceGit(t, workspace, "config", "user.email", "evidence-test@example.invalid")
	runEvidenceGit(t, workspace, "config", "user.name", "Evidence Test")
	if err := os.WriteFile(filepath.Join(workspace, "tracked.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runEvidenceGit(t, workspace, "add", "tracked.txt")
	runEvidenceGit(t, workspace, "commit", "-q", "-m", "base")
	baseSHA := runEvidenceGit(t, workspace, "rev-parse", "HEAD")
	store := newAttemptEvidenceStore(t)
	owner, err := store.Create(beads.Bead{Title: "separate git dir work", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := Capture(context.Background(), store, CaptureSpec{
		Identity: Identity{Kind: KindRetry, OwnerBeadID: owner.ID, ExecutionBeadID: "separate-git-dir"},
		StoreRef: "rig:pilot", WorkDir: workspace, BaseSHA: baseSHA,
		Permission: PermissionScope{StoreRef: "rig:pilot", WorkID: owner.ID, RepositoryRoot: workspace},
	})
	if err != nil {
		t.Fatalf("Capture separate git directory: %v", err)
	}
	resolvedCommonDir, err := filepath.EvalSymlinks(commonDir)
	if err != nil {
		t.Fatal(err)
	}
	if evidence.Permission.RepositoryRoot != resolvedCommonDir || evidence.Permission.WorkspaceRoot != workspace {
		t.Fatalf("permission scope = %+v, want canonical common dir %q and worktree %q", evidence.Permission, resolvedCommonDir, workspace)
	}
}

func TestPermissionScopeMapsConfiguredRepositoryRootToLinkedWorktree(t *testing.T) {
	repo, baseSHA := newEvidenceRepo(t)
	workspace := filepath.Join(t.TempDir(), "attempt-worktree")
	runEvidenceGit(t, repo, "worktree", "add", "--detach", workspace, baseSHA)
	store := newAttemptEvidenceStore(t)
	owner, err := store.Create(beads.Bead{Title: "linked worktree scope", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	evidence, err := Capture(context.Background(), store, CaptureSpec{
		Identity: Identity{Kind: KindRetry, OwnerBeadID: owner.ID, ExecutionBeadID: "linked-worktree"},
		StoreRef: "rig:pilot", WorkDir: workspace, BaseSHA: baseSHA,
		Permission: PermissionScope{StoreRef: "rig:pilot", WorkID: owner.ID, RepositoryRoot: repo},
	})
	if err != nil {
		t.Fatalf("Capture linked worktree: %v", err)
	}
	if evidence.Permission.RepositoryRoot != filepath.Join(repo, ".git") || evidence.Permission.WorkspaceRoot != workspace {
		t.Fatalf("permission scope = %+v, want shared common dir %q and linked worktree %q", evidence.Permission, filepath.Join(repo, ".git"), workspace)
	}
}

func TestSnapshotRejectsHeadMovementDuringCapture(t *testing.T) {
	repo, baseSHA := newEvidenceRepo(t)
	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("other commit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runEvidenceGit(t, repo, "add", "tracked.txt")
	runEvidenceGit(t, repo, "commit", "-q", "-m", "other")
	otherSHA := runEvidenceGit(t, repo, "rev-parse", "HEAD")
	runEvidenceGit(t, repo, "checkout", "--detach", baseSHA)
	spec := CaptureSpec{
		Identity: Identity{Kind: KindRetry, OwnerBeadID: "work-1", ExecutionBeadID: "attempt-1"},
		StoreRef: "rig:pilot", WorkDir: repo, BaseSHA: baseSHA,
	}
	_, err := snapshotWithProbe(context.Background(), spec, mustAttemptID(t, spec.Identity), func() error {
		cmd := exec.Command("git", "-C", repo, "checkout", "--detach", otherSHA)
		if output, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("move HEAD in probe: %w: %s", err, output)
		}
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "changed during evidence capture") {
		t.Fatalf("capture after HEAD movement = %v, want fail-closed movement error", err)
	}
}

func TestGitOutputAndUntrackedReadsAreBoundedAndRootConfined(t *testing.T) {
	repo, baseSHA := newEvidenceRepo(t)
	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte(strings.Repeat("oversized patch\n", 40)), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := gitOutputLimit(context.Background(), repo, 32, "diff", "--binary", "--no-ext-diff", "--no-textconv", "--no-color", baseSHA, "--"); !errors.Is(err, ErrCaptureTooLarge) {
		t.Fatalf("oversized git output error = %v, want ErrCaptureTooLarge", err)
	}

	root, err := os.OpenRoot(repo)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close() //nolint:errcheck
	if err := os.WriteFile(filepath.Join(repo, "large.txt"), []byte(strings.Repeat("x", 64)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := snapshotUntrackedPaths(root, [][]byte{[]byte("large.txt")}, 16); !errors.Is(err, ErrCaptureTooLarge) {
		t.Fatalf("oversized untracked read error = %v, want ErrCaptureTooLarge", err)
	}

	outside := t.TempDir()
	secret := "must-not-be-read-from-outside-worktree"
	if err := os.WriteFile(filepath.Join(outside, "secret.txt"), []byte(secret), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(repo, "escape")); err != nil {
		t.Fatal(err)
	}
	if _, err := snapshotUntrackedPaths(root, [][]byte{[]byte("escape/secret.txt")}, maxDiffInputBytes); err == nil {
		t.Fatal("root-confined read through an escaping parent symlink succeeded")
	}
	if err := os.Symlink(filepath.Join(outside, "secret.txt"), filepath.Join(repo, "secret-link")); err != nil {
		t.Fatal(err)
	}
	links, err := snapshotUntrackedPaths(root, [][]byte{[]byte("secret-link")}, maxDiffInputBytes)
	if err != nil {
		t.Fatalf("capture symlink itself: %v", err)
	}
	if len(links) != 1 || !links[0].Symlink || string(links[0].Bytes) == secret || strings.Contains(string(links[0].Bytes), secret) {
		t.Fatalf("symlink capture read or misrepresented outside file: %#v", links)
	}
}

func TestConcurrentSealHasOneImmutableWinner(t *testing.T) {
	store := newAttemptEvidenceStore(t)
	owner, err := store.Create(beads.Bead{Title: "source work", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	identity := Identity{Kind: KindRetry, OwnerBeadID: owner.ID, ExecutionBeadID: "attempt-1"}
	id, err := AttemptID(identity)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	makeEvidence := func(outcome string) Evidence {
		diff, err := compressDiff(diffBundle{TrackedPatch: []byte(outcome)}, DiffSourceWorkingTree)
		if err != nil {
			t.Fatal(err)
		}
		return Evidence{
			SchemaVersion: SchemaVersion, AttemptID: id, Identity: identity,
			StoreRef: "rig:pilot", CapturedAt: now, Outcome: outcome,
			SourceStatus: StatusAvailable,
			BaseStatus:   StatusUnavailable, BaseReason: "base_unknown",
			CandidateStatus: StatusAvailable, CandidateSHA: strings.Repeat("a", 40),
			WorkingTreeStatus: WorkingTreeDirty,
			Diff:              DiffSnapshot{Status: StatusUnavailable, Reason: "base_unknown"},
			WorkspaceDiff:     diff,
			Policy:            unavailableFacet("not_linked"), Actions: unavailableFacet("not_linked"),
			Acknowledgements: unavailableFacet("not_linked"), Redaction: unavailableFacet("not_performed"),
		}
	}
	proposals := []Evidence{makeEvidence("first"), makeEvidence("second")}
	var wg sync.WaitGroup
	results := make(chan Evidence, 2)
	errs := make(chan error, 2)
	for _, proposal := range proposals {
		proposal := proposal
		wg.Add(1)
		go func() {
			defer wg.Done()
			sealed, err := Seal(store, proposal)
			if err != nil {
				errs <- err
				return
			}
			results <- sealed
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent Seal: %v", err)
	}
	var winner Evidence
	for result := range results {
		if winner.AttemptID == "" {
			winner = result
		} else if !samePayload(winner, result) {
			t.Fatal("concurrent seal callers received different winners")
		}
	}
	got, err := Read(store, owner.ID, id)
	if err != nil {
		t.Fatalf("Read winner: %v", err)
	}
	if !samePayload(winner, got) {
		t.Fatal("read returned a payload different from the CAS winner")
	}
	if _, err := store.Create(beads.Bead{Title: "sanity"}); err != nil {
		t.Fatal(err)
	}
}

func TestUnsupportedMetadataCASRefusesToSeal(t *testing.T) {
	base := newAttemptEvidenceStore(t)
	owner, err := base.Create(beads.Bead{Title: "source", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	store := noMetadataCASStore{Store: base}
	identity := Identity{Kind: KindRetry, OwnerBeadID: owner.ID, ExecutionBeadID: "attempt-1"}
	id, err := AttemptID(identity)
	if err != nil {
		t.Fatal(err)
	}
	evidence := MakeUnavailable(CaptureSpec{Identity: identity, StoreRef: "rig:pilot"}, id, "workspace_absent")
	if _, err := Seal(store, evidence); !errors.Is(err, beads.ErrConditionalWriteUnsupported) {
		t.Fatalf("Seal unsupported CAS error = %v", err)
	}
}

func TestUnsafePayloadTransportDoesNotDowngradePresentDiffOrWriteIndex(t *testing.T) {
	base := beads.NewMemStore()
	owner, err := base.Create(beads.Bead{Title: "source", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	repo, baseSHA := newEvidenceRepo(t)
	if err := os.WriteFile(filepath.Join(repo, "tracked.txt"), []byte("private diff\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	store := noMetadataCASStore{Store: base}
	spec := CaptureSpec{
		Identity: Identity{Kind: KindRetry, OwnerBeadID: owner.ID, ExecutionBeadID: "attempt-1"},
		StoreRef: "rig:pilot", WorkDir: repo, BaseSHA: baseSHA,
	}
	if _, err := Capture(context.Background(), store, spec); !errors.Is(err, ErrPrivatePayloadTransportUnsupported) {
		t.Fatalf("Capture on unsafe payload transport = %v, want explicit refusal", err)
	}
	id, err := AttemptID(spec.Identity)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Read(base, owner.ID, id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unsafe capture wrote an evidence index: %v", err)
	}
	archives, err := base.ListByMetadata(map[string]string{beadmeta.AttemptEvidenceArchiveOwnerIDMetadataKey: owner.ID}, 0, beads.IncludeClosed)
	if err != nil {
		t.Fatal(err)
	}
	if len(archives) != 0 {
		t.Fatalf("unsafe capture created archive rows: %#v", archives)
	}
}

func TestArchiveRecordsAreNotReadyOrWorkflowCandidates(t *testing.T) {
	store := newAttemptEvidenceStore(t)
	owner, err := store.Create(beads.Bead{Title: "source", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	repo, baseSHA := newEvidenceRepo(t)
	spec := CaptureSpec{Identity: Identity{Kind: KindRalph, OwnerBeadID: owner.ID, ExecutionBeadID: "iteration-1"}, StoreRef: "city:pilot", WorkDir: repo, BaseSHA: baseSHA}
	evidence, err := Capture(context.Background(), store, spec)
	if err != nil {
		t.Fatal(err)
	}
	archive, found, err := readArchive(store, owner.ID, evidence.AttemptID)
	if err != nil || !found {
		t.Fatalf("archive read = %#v, found=%v, err=%v", archive, found, err)
	}
	rows, err := store.List(beads.ListQuery{AllowScan: true, IncludeClosed: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if IsArchiveRecord(row) && row.Type != "molecule" {
			t.Fatalf("archive type %q can enter normal work routing", row.Type)
		}
	}
	ready, err := store.Ready()
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range ready {
		if IsArchiveRecord(row) {
			t.Fatalf("archive %s entered Ready", row.ID)
		}
	}
}

func newEvidenceRepo(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q")
	git("config", "user.email", "evidence-test@example.invalid")
	git("config", "user.name", "Evidence Test")
	if err := os.WriteFile(filepath.Join(root, "tracked.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "tracked.txt")
	git("commit", "-q", "-m", "base")
	return root, git("rev-parse", "HEAD")
}

func newAttemptEvidenceStore(t *testing.T) *beads.FileStore {
	t.Helper()
	store, err := beads.OpenFileStore(fsys.OSFS{}, filepath.Join(t.TempDir(), "beads.json"))
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func runEvidenceGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
	return strings.TrimSpace(string(output))
}

func mustAttemptID(t *testing.T, identity Identity) string {
	t.Helper()
	id, err := AttemptID(identity)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

type noMetadataCASStore struct{ beads.Store }

func (s noMetadataCASStore) PrivatePayloadValueTransportTarget() beads.Store { return s.Store }
