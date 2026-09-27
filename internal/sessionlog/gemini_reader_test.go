package sessionlog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestFindGeminiSessionFileRejectsProjectRootSymlinkEscape(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "tmp")
	workDir := filepath.Join(t.TempDir(), "project")
	projectDir := filepath.Join(root, "project-hash")
	chatsDir := filepath.Join(projectDir, "chats")
	if err := os.MkdirAll(chatsDir, 0o755); err != nil {
		t.Fatal(err)
	}

	outsideProjectRoot := filepath.Join(t.TempDir(), "outside-project-root")
	if err := os.WriteFile(outsideProjectRoot, []byte(workDir), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideProjectRoot, filepath.Join(projectDir, ".project_root")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	sessionPath := filepath.Join(chatsDir, "session-outside-root.json")
	if err := os.WriteFile(sessionPath, []byte(`{"sessionId":"gemini-session","messages":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}

	if got := FindGeminiSessionFile([]string{root}, workDir); got != "" {
		t.Fatalf("FindGeminiSessionFile() = %q, want no match for escaping .project_root symlink", got)
	}
}

func TestFindGeminiSessionFileRejectsProjectsIndexSymlinkEscape(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "tmp")
	workDir := filepath.Join(t.TempDir(), "project")
	projectDir := filepath.Join(root, "project-hash")
	chatsDir := filepath.Join(projectDir, "chats")
	if err := os.MkdirAll(chatsDir, 0o755); err != nil {
		t.Fatal(err)
	}

	outsideIndex := filepath.Join(t.TempDir(), "projects.json")
	indexData, err := json.Marshal(map[string]any{"projects": map[string]string{workDir: "project-hash"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(outsideIndex, indexData, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideIndex, filepath.Join(base, "projects.json")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	sessionPath := filepath.Join(chatsDir, "session-index-escape.json")
	if err := os.WriteFile(sessionPath, []byte(`{"sessionId":"gemini-session","messages":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}

	if got := FindGeminiSessionFile([]string{root}, workDir); got != "" {
		t.Fatalf("FindGeminiSessionFile() = %q, want no match for escaping projects.json symlink", got)
	}
}

func TestFindGeminiSessionFileRejectsCandidateSymlinkEscape(t *testing.T) {
	root := filepath.Join(t.TempDir(), "tmp")
	workDir := filepath.Join(t.TempDir(), "project")
	projectDir := filepath.Join(root, "project-hash")
	chatsDir := filepath.Join(projectDir, "chats")
	if err := os.MkdirAll(chatsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, ".project_root"), []byte(workDir), 0o600); err != nil {
		t.Fatal(err)
	}

	outsideSession := filepath.Join(t.TempDir(), "session-outside.json")
	if err := os.WriteFile(outsideSession, []byte(`{"sessionId":"outside-session","messages":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	candidate := filepath.Join(chatsDir, "session-candidate.json")
	if err := os.Symlink(outsideSession, candidate); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	if got := FindGeminiSessionFile([]string{root}, workDir); got != "" {
		t.Fatalf("FindGeminiSessionFile() = %q, want no match for escaping candidate symlink", got)
	}
	if got := FindGeminiSessionFileByID([]string{root}, workDir, "outside-session"); got != "" {
		t.Fatalf("FindGeminiSessionFileByID() = %q, want no match for escaping candidate symlink", got)
	}
}
