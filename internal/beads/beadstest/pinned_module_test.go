package beadstest

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/mod/module"
)

// A populated upstream cache must not hide the replacement actually linked by
// Go: the drift checks must inspect that replacement's migration sources.
func TestPinnedBeadsModuleDirResolvesEffectiveReplacement(t *testing.T) {
	const (
		upstreamVersion = "v1.3.1"
		forkVersion     = "v1.3.2-0.20261013010644-3c3423cf09a9"
		forkPath        = "example.com/fixture-beads-fork"
	)
	cases := []struct {
		name         string
		replacements string
		moduleDir    string
		version      string // empty for a local directory
	}{
		{
			name:      "required version without replacement",
			moduleDir: "cache/" + PinnedBeadsModulePath + "@" + upstreamVersion,
			version:   upstreamVersion,
		},
		{
			name:         "remote version",
			replacements: "replace github.com/rhar1511/beads => " + forkPath + " " + forkVersion,
			moduleDir:    "cache/" + forkPath + "@" + forkVersion,
			version:      forkVersion,
		},
		{
			name: "exact version takes precedence over wildcard",
			replacements: "replace (\n" +
				"github.com/rhar1511/beads => example.com/wildcard v1.0.0\n" +
				"github.com/rhar1511/beads " + upstreamVersion + " => " + forkPath + " " + forkVersion + "\n)",
			moduleDir: "cache/" + forkPath + "@" + forkVersion,
			version:   forkVersion,
		},
		{
			name: "wildcard does not override earlier exact version",
			replacements: "replace (\n" +
				"github.com/rhar1511/beads " + upstreamVersion + " => " + forkPath + " " + forkVersion + "\n" +
				"github.com/rhar1511/beads => example.com/wildcard v1.0.0\n)",
			moduleDir: "cache/" + forkPath + "@" + forkVersion,
			version:   forkVersion,
		},
		{
			name:         "unmatched exact version leaves required version",
			replacements: "replace github.com/rhar1511/beads v1.3.0 => " + forkPath + " " + forkVersion,
			moduleDir:    "cache/" + PinnedBeadsModulePath + "@" + upstreamVersion,
			version:      upstreamVersion,
		},
		{
			name: "other version does not replace required version",
			replacements: "replace (\n" +
				"github.com/rhar1511/beads v1.3.0 => example.com/other v1.0.0\n" +
				"github.com/rhar1511/beads => " + forkPath + " " + forkVersion + "\n)",
			moduleDir: "cache/" + forkPath + "@" + forkVersion,
			version:   forkVersion,
		},
		{
			name:         "escaped remote cache path and version",
			replacements: "replace github.com/rhar1511/beads => example.com/Team/Beads v1.3.2-RC1",
			moduleDir:    "cache/example.com/!team/!beads@v1.3.2-!r!c1",
			version:      "v1.3.2-RC1",
		},
		{
			name:         "relative local directory",
			replacements: "replace github.com/rhar1511/beads => ../local-beads",
			moduleDir:    "local-beads",
		},
		{
			name:         "absolute local directory",
			replacements: "replace github.com/rhar1511/beads => LOCAL_BEADS_DIR",
			moduleDir:    "local-beads",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			cache := filepath.Join(root, "cache")
			stale := filepath.Join(cache, filepath.FromSlash(PinnedBeadsModulePath)+"@"+upstreamVersion)
			want := filepath.Join(root, filepath.FromSlash(tc.moduleDir))
			fixture := filepath.Join(root, "fixture")
			for _, dir := range []string{stale, want, fixture} {
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			replacements := strings.ReplaceAll(tc.replacements, "LOCAL_BEADS_DIR", fmt.Sprintf("%q", filepath.ToSlash(want)))
			mod := "module example.com/fixture\n\ngo 1.26.0\n\nrequire github.com/rhar1511/beads " + upstreamVersion + "\n\n" + replacements + "\n"
			if err := os.WriteFile(filepath.Join(fixture, "go.mod"), []byte(mod), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("RUNFILES_DIR", "")
			t.Setenv("TEST_SRCDIR", "")
			t.Setenv("GOENV", "off")
			t.Setenv("GOMODCACHE", cache)
			t.Chdir(fixture)
			if got := PinnedBeadsModuleDir(t); got != want {
				t.Fatalf("PinnedBeadsModuleDir = %q, want effective replacement %q (stale upstream cache: %q)", got, want, stale)
			}
			wantVersion := tc.version
			if wantVersion == "" {
				wantVersion = want
			}
			if got := PinnedBeadsVersion(t); got != wantVersion {
				t.Fatalf("PinnedBeadsVersion = %q, want effective source version %q", got, wantVersion)
			}
		})
	}
}

// TestPinnedBeadsModuleDirRefusesAnUnresolvedCache is the fence under the drift
// check's one silent-green path.
//
// The resolver is a hand-rolled copy of cmd/go's GOMODCACHE -> go env file ->
// GOPATH[0]/pkg/mod chain, kept inlined because shelling out to `go env` would
// grow the repo's shrink-only subprocess census. An inlined copy is only
// defensible if disagreeing with cmd/go is fatal, so that is asserted here
// rather than left to the one caller.
func TestPinnedBeadsModuleDirRefusesAnUnresolvedCache(t *testing.T) {
	t.Run("a cache that does not hold the module", func(t *testing.T) {
		if _, err := pinnedBeadsModuleDir(filepath.Join(t.TempDir(), "empty"), module.Version{Path: PinnedBeadsModulePath, Version: "v1.3.0"}); err == nil {
			t.Fatal("pinnedBeadsModuleDir accepted a cache with no pinned module in it; a skip here lets a resolver bug read as green")
		}
	})

	t.Run("a path that is not a directory", func(t *testing.T) {
		cache := t.TempDir()
		path := filepath.Join(cache, filepath.FromSlash(PinnedBeadsModulePath)+"@v1.3.0")
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("not a module"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := pinnedBeadsModuleDir(cache, module.Version{Path: PinnedBeadsModulePath, Version: "v1.3.0"}); err == nil {
			t.Fatal("pinnedBeadsModuleDir accepted a file where the module source should be")
		}
	})

	t.Run("the unpacked module", func(t *testing.T) {
		cache := t.TempDir()
		want := filepath.Join(cache, filepath.FromSlash(PinnedBeadsModulePath)+"@v1.3.0")
		if err := os.MkdirAll(want, 0o755); err != nil {
			t.Fatal(err)
		}
		got, err := pinnedBeadsModuleDir(cache, module.Version{Path: PinnedBeadsModulePath, Version: "v1.3.0"})
		if err != nil {
			t.Fatalf("pinnedBeadsModuleDir: %v", err)
		}
		if got != want {
			t.Fatalf("pinnedBeadsModuleDir = %q, want %q", got, want)
		}
	})
}

// TestPinnedBeadsModuleDirFailsRatherThanSkips pins the seam itself.
//
// The subtests above prove the resolver returns an error; this proves what the
// caller does with it. A revert of the t.Fatalf to a t.Skipf would leave every
// other test in this package green while the pinned-cursor drift check silently
// stopped running — which is the failure mode it was written against, and it was
// reproduced with nothing more than GOMODCACHE pointed somewhere else.
func TestPinnedBeadsModuleDirFailsRatherThanSkips(t *testing.T) {
	reporter := &recordingModuleDirReporter{}
	if dir := pinnedBeadsModuleDirOrFatal(reporter, filepath.Join(t.TempDir(), "empty"), module.Version{Path: PinnedBeadsModulePath, Version: "v1.3.0"}); dir != "" {
		t.Fatalf("an unresolved cache produced the directory %q", dir)
	}
	if len(reporter.skips) != 0 {
		t.Fatalf("an unresolved module cache was SKIPPED (%q); a skip lets the drift check go quiet and read as green", reporter.skips)
	}
	if len(reporter.fatals) != 1 {
		t.Fatalf("an unresolved module cache reported %d fatal(s), want exactly 1: %q", len(reporter.fatals), reporter.fatals)
	}
	for _, want := range []string{"GOMODCACHE", PinnedBeadsModulePath} {
		if !strings.Contains(reporter.fatals[0], want) {
			t.Errorf("the failure message does not name %q, so the operator cannot act on it:\n%s", want, reporter.fatals[0])
		}
	}
}

func TestPinnedBeadsModuleDirMissingReplacementDoesNotFallBack(t *testing.T) {
	cache := t.TempDir()
	stale := filepath.Join(cache, filepath.FromSlash(PinnedBeadsModulePath)+"@v1.3.1")
	if err := os.MkdirAll(stale, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, pinned := range []module.Version{
		{Path: "github.com/rhar1511/beads", Version: "v1.3.2-0.20261013010644-3c3423cf09a9"},
		{Path: filepath.Join(cache, "missing-local-replacement")},
	} {
		t.Run(pinned.String(), func(t *testing.T) {
			reporter := &recordingModuleDirReporter{}
			if got := pinnedBeadsModuleDirOrFatal(reporter, cache, pinned); got != "" {
				t.Fatalf("missing replacement resolved to %q (stale upstream: %q)", got, stale)
			}
			if len(reporter.fatals) != 1 || len(reporter.skips) != 0 {
				t.Fatalf("missing replacement: fatals %q, skips %q; want one fatal and no skips", reporter.fatals, reporter.skips)
			}
			if !strings.Contains(reporter.fatals[0], pinned.String()) {
				t.Fatalf("failure does not identify the effective module %s: %s", pinned, reporter.fatals[0])
			}
		})
	}
}

// recordingModuleDirReporter records what the resolver reports instead of
// failing or skipping the test that drives it. It does not call runtime.Goexit
// on Fatalf, so the caller returns normally and the zero value it hands back is
// asserted too.
type recordingModuleDirReporter struct {
	fatals []string
	skips  []string
}

func (r *recordingModuleDirReporter) Helper() {}

func (r *recordingModuleDirReporter) Fatalf(format string, args ...any) {
	r.fatals = append(r.fatals, fmt.Sprintf(format, args...))
}

func (r *recordingModuleDirReporter) Skipf(format string, args ...any) {
	r.skips = append(r.skips, fmt.Sprintf(format, args...))
}
