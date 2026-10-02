package sessionlog

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFindCapturedACPSessionFileByIDRejectsEscapingSymlink(t *testing.T) {
	root := t.TempDir()
	workDir := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatalf("mkdir workdir: %v", err)
	}
	outside := filepath.Join(t.TempDir(), "outside.jsonl")
	if err := os.WriteFile(outside, []byte(`{"cwd":`+jsonString(workDir)+`}`+"\n"), 0o600); err != nil {
		t.Fatalf("write outside capture: %v", err)
	}
	candidate := filepath.Join(root, "session-1.jsonl")
	if err := os.Symlink(outside, candidate); err != nil {
		t.Fatalf("symlink outside capture: %v", err)
	}

	if got := FindGrokSessionFileByID([]string{root}, workDir, "session-1"); got != "" {
		t.Fatalf("FindGrokSessionFileByID() = %q, want escaping symlink rejected", got)
	}
}

func TestFindCapturedACPSessionFileRejectsEscapingSymlinkCandidate(t *testing.T) {
	root := t.TempDir()
	workDir := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatalf("mkdir workdir: %v", err)
	}
	outside := filepath.Join(t.TempDir(), "outside.jsonl")
	if err := os.WriteFile(outside, []byte(`{"cwd":`+jsonString(workDir)+`}`+"\n"), 0o600); err != nil {
		t.Fatalf("write outside capture: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "session-1.jsonl")); err != nil {
		t.Fatalf("symlink outside capture: %v", err)
	}

	if got := FindGrokSessionFile([]string{root}, workDir); got != "" {
		t.Fatalf("FindGrokSessionFile() = %q, want escaping symlink rejected", got)
	}
}

func TestFindCapturedACPSessionFileSupportsConfiguredSymlinkRoot(t *testing.T) {
	actualRoot := t.TempDir()
	configuredRoot := filepath.Join(t.TempDir(), "capture-alias")
	if err := os.Symlink(actualRoot, configuredRoot); err != nil {
		t.Fatalf("symlink configured root: %v", err)
	}
	workDir := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatalf("mkdir workdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(actualRoot, "session-1.jsonl"), []byte(`{"cwd":`+jsonString(workDir)+`}`+"\n"), 0o600); err != nil {
		t.Fatalf("write configured capture: %v", err)
	}

	want := filepath.Join(configuredRoot, "session-1.jsonl")
	if got := FindGrokSessionFile([]string{configuredRoot}, workDir); got != want {
		t.Fatalf("FindGrokSessionFile() = %q, want configured-root path %q", got, want)
	}
}
