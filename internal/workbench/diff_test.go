package workbench

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func gitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init"},
		{"config", "user.email", "t@example.com"},
		{"config", "user.name", "t"},
	} {
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Skipf("git unavailable: %v %s", err, out)
		}
	}
	return dir
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestWorktreeDiff_MissingWorktree(t *testing.T) {
	if got := WorktreeDiff(context.Background(), "", 0); got.State != DiffMissingWorktree {
		t.Fatalf("empty dir state = %q, want %q", got.State, DiffMissingWorktree)
	}
	if got := WorktreeDiff(context.Background(), filepath.Join(t.TempDir(), "nope"), 0); got.State != DiffMissingWorktree {
		t.Fatalf("absent dir state = %q, want %q", got.State, DiffMissingWorktree)
	}
}

func TestWorktreeDiff_Empty(t *testing.T) {
	dir := gitRepo(t)
	if got := WorktreeDiff(context.Background(), dir, 0); got.State != DiffEmpty {
		t.Fatalf("clean repo state = %q, want %q", got.State, DiffEmpty)
	}
}

func TestWorktreeDiff_OK(t *testing.T) {
	dir := gitRepo(t)
	write(t, filepath.Join(dir, "a.txt"), "one\n")
	if out, err := exec.Command("git", "-C", dir, "add", "a.txt").CombinedOutput(); err != nil {
		t.Fatalf("add: %v %s", err, out)
	}
	if out, err := exec.Command("git", "-C", dir, "commit", "-m", "init").CombinedOutput(); err != nil {
		t.Fatalf("commit: %v %s", err, out)
	}
	write(t, filepath.Join(dir, "a.txt"), "two\n")

	got := WorktreeDiff(context.Background(), dir, 0)
	if got.State != DiffOK {
		t.Fatalf("state = %q, want %q", got.State, DiffOK)
	}
	if !strings.Contains(got.Text, "-one") || !strings.Contains(got.Text, "+two") {
		t.Fatalf("diff missing change:\n%s", got.Text)
	}
}

func TestWorktreeDiff_Truncated(t *testing.T) {
	dir := gitRepo(t)
	write(t, filepath.Join(dir, "a.txt"), strings.Repeat("x\n", 100))
	if out, err := exec.Command("git", "-C", dir, "add", "a.txt").CombinedOutput(); err != nil {
		t.Fatalf("add: %v %s", err, out)
	}
	if out, err := exec.Command("git", "-C", dir, "commit", "-m", "init").CombinedOutput(); err != nil {
		t.Fatalf("commit: %v %s", err, out)
	}
	write(t, filepath.Join(dir, "a.txt"), strings.Repeat("y\n", 100))

	got := WorktreeDiff(context.Background(), dir, 32)
	if got.State != DiffOK || !got.Truncated {
		t.Fatalf("state=%q truncated=%v, want ok/truncated", got.State, got.Truncated)
	}
	if len(got.Text) > 32 {
		t.Fatalf("text len %d exceeds bound 32", len(got.Text))
	}
}
