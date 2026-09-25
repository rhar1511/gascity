package factory

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func validConfig() Config {
	return Config{
		Version:  ConfigVersion,
		Timezone: "Australia/Sydney",
		Repositories: []Repository{{
			ID: "app", Provider: "github", Remote: "rhar1511/app",
			Worktree: WorktreePolicy{Mode: "fresh-per-run", Base: "origin/main"},
		}},
		Sources: []Source{{ID: "issues", Provider: "github", Type: "issue", Scope: "rhar1511/app"}},
		Workflows: Workflows{Collect: CollectWorkflow{
			Enabled: true, Sources: []string{"issues"},
			Implement: ActionPolicy{Mode: "criteria", Allow: []string{"verified defects"}},
			Reply:     ActionPolicy{Mode: "never"}, Close: ActionPolicy{Mode: "never"},
		}},
	}
}

func TestValidateAcceptsBoundedFactoryPolicy(t *testing.T) {
	if err := validConfig().Validate(); err != nil {
		t.Fatalf("Validate() = %v", err)
	}
}

func TestValidateRejectsUnknownSourceAndUnsafePolicyMode(t *testing.T) {
	cfg := validConfig()
	cfg.Workflows.Collect.Sources = []string{"missing"}
	cfg.Workflows.Collect.Implement.Mode = "automatic"
	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate() = nil, want error")
	}
	for _, want := range []string{"unknown source missing", "unsupported mode automatic"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Validate() error %q does not contain %q", err, want)
		}
	}
}

func TestValidateRequiresSourcesForEnabledWorkflows(t *testing.T) {
	cfg := validConfig()
	cfg.Workflows.Collect.Sources = nil
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "collect.sources is required") {
		t.Fatalf("Validate() = %v, want enabled collect source error", err)
	}
}

func TestValidateRequiresFreshWorktreeBase(t *testing.T) {
	cfg := validConfig()
	cfg.Repositories[0].Worktree.Base = ""
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "worktree.base is required") {
		t.Fatalf("Validate() = %v, want missing base error", err)
	}
}

func TestLoadProjectDistinguishesMissingConfig(t *testing.T) {
	root := t.TempDir()
	_, err := LoadProject(root)
	if !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("LoadProject() error = %v, want ErrNotConfigured", err)
	}
}

func TestLoadFileParsesBuilderShape(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, ConfigRelativePath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	data := `version: 1
timezone: UTC
repositories:
  - id: app
    provider: github
    remote: rhar1511/app
    worktree:
      mode: fresh-per-run
      base: origin/main
sources:
  - id: issues
    provider: github
    type: issue
    scope: rhar1511/app
workflows:
  collect:
    enabled: true
    sources: [issues]
    implement:
      mode: criteria
      allow: [verified defects in owned code]
    reply:
      mode: never
    close:
      mode: never
skill_prompts:
  factory: Keep changes bounded.
`
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadProject(root)
	if err != nil {
		t.Fatalf("LoadProject() = %v", err)
	}
	if cfg.Repositories[0].Remote != "rhar1511/app" || cfg.Workflows.Collect.Sources[0] != "issues" {
		t.Fatalf("parsed config = %+v", cfg)
	}
	if cfg.SkillPrompts["factory"] != "Keep changes bounded." {
		t.Fatalf("skill prompt = %#v", cfg.SkillPrompts)
	}
}
