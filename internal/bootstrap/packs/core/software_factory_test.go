package core

import (
	"errors"
	"io/fs"
	"testing"
)

// Private workflows are supplied only by an explicit consuming-city import.
func TestFactoryWorkflowsRequireExplicitPackImport(t *testing.T) {
	for _, path := range []string{
		"formulas/mol-software-factory.toml",
		"formulas/mol-rsi-candidate.toml",
		"skills/gc-factory/SKILL.md",
	} {
		t.Run(path, func(t *testing.T) {
			if _, err := fs.Stat(PackFS, path); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("embedded private workflow %q: got %v, want missing until its pack is imported", path, err)
			}
		})
	}
}
