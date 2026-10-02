package sessionlog

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestClaudeDiscoveryRejectsTranscriptSymlinksOutsideSearchRoot(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.jsonl")
	if err := os.WriteFile(outside, []byte("outside transcript\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	const slug = "work-project"
	projectDir := filepath.Join(root, slug)
	if err := os.MkdirAll(projectDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"session-123.jsonl", "latest-session.jsonl", "listed-session.jsonl"} {
		if err := os.Symlink(outside, filepath.Join(projectDir, name)); err != nil {
			t.Fatal(err)
		}
	}

	if got := findSessionFileByIDForCandidates([]string{root}, []string{slug}, "session-123.jsonl"); got != "" {
		t.Fatalf("Claude ID discovery returned escaping transcript %q", got)
	}
	if got := findClaudeLatestSessionFileForCandidates([]string{root}, []string{slug}); got != "" {
		t.Fatalf("Claude latest discovery returned escaping transcript %q", got)
	}
	if got := findSlugSessionFileForCandidates([]string{root}, []string{slug}); got != "" {
		t.Fatalf("Claude slug discovery returned escaping transcript %q", got)
	}
}

func TestClaudeDiscoveryRejectsEscapingProjectDirectoryAndSupportsRootSymlink(t *testing.T) {
	actualRoot := t.TempDir()
	configuredRoot := filepath.Join(t.TempDir(), "projects-alias")
	if err := os.Symlink(actualRoot, configuredRoot); err != nil {
		t.Fatal(err)
	}

	const slug = "work-project"
	outsideProject := t.TempDir()
	if err := os.WriteFile(filepath.Join(outsideProject, "outside.jsonl"), []byte("outside transcript\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideProject, filepath.Join(configuredRoot, slug)); err != nil {
		t.Fatal(err)
	}
	if got := findSlugSessionFileForCandidates([]string{configuredRoot}, []string{slug}); got != "" {
		t.Fatalf("Claude slug discovery traversed an escaping project directory: %q", got)
	}

	if err := os.Remove(filepath.Join(configuredRoot, slug)); err != nil {
		t.Fatal(err)
	}
	projectDir := filepath.Join(actualRoot, slug)
	if err := os.MkdirAll(projectDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "session-123.jsonl"), []byte("inside transcript\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(configuredRoot, slug, "session-123.jsonl")
	if got := findSessionFileByIDForCandidates([]string{configuredRoot}, []string{slug}, "session-123.jsonl"); got != want {
		t.Fatalf("Claude ID discovery = %q, want configured-root path %q", got, want)
	}
}

func TestCodexDiscoveryRejectsTranscriptSymlinkOutsideSearchRoot(t *testing.T) {
	root := t.TempDir()
	workDir := filepath.Join(t.TempDir(), "project")
	dayDir := filepath.Join(root, "2026", "01", "02")
	if err := os.MkdirAll(dayDir, 0o700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.jsonl")
	content := fmt.Sprintf(`{"type":"session_meta","payload":{"cwd":%q}}`+"\n", workDir)
	if err := os.WriteFile(outside, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	candidate := filepath.Join(dayDir, "rollout-2026-01-02T03-04-05-security-fixture.jsonl")
	if err := os.Symlink(outside, candidate); err != nil {
		t.Fatal(err)
	}

	if got := FindCodexSessionFile([]string{root}, workDir); got != "" {
		t.Fatalf("Codex discovery returned escaping transcript %q", got)
	}
	if got := codexSessionCWD(candidate); got != "" {
		t.Fatalf("Codex CWD probe read an escaping transcript: %q", got)
	}
}

func TestCodexDiscoverySupportsConfiguredSymlinkRoot(t *testing.T) {
	actualRoot := t.TempDir()
	configuredRoot := filepath.Join(t.TempDir(), "sessions-alias")
	if err := os.Symlink(actualRoot, configuredRoot); err != nil {
		t.Fatal(err)
	}
	workDir := filepath.Join(t.TempDir(), "project")
	dayDir := filepath.Join(actualRoot, "2026", "01", "02")
	if err := os.MkdirAll(dayDir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dayDir, "rollout-2026-01-02T03-04-05-configured-root.jsonl")
	content := fmt.Sprintf(`{"type":"session_meta","payload":{"cwd":%q}}`+"\n", workDir)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	want := filepath.Join(configuredRoot, "2026", "01", "02", filepath.Base(path))
	if got := FindCodexSessionFile([]string{configuredRoot}, workDir); got != want {
		t.Fatalf("Codex discovery = %q, want path under configured symlink root %q", got, want)
	}
}
