// Package testutil contains helpers shared by tests across platforms.
package testutil

import (
	"os"
	"runtime"
	"testing"

	"github.com/gastownhall/gascity/internal/pathutil"
)

// CanonicalPath returns the production path-normalized form used for
// comparisons. This keeps tests stable on macOS where /tmp and /var can be
// reported through /private aliases.
func CanonicalPath(path string) string {
	return pathutil.NormalizePathForCompare(path)
}

// AssertSamePath compares two filesystem paths after canonicalization.
func AssertSamePath(t *testing.T, got, want string) {
	t.Helper()
	got = CanonicalPath(got)
	want = CanonicalPath(want)
	if got != want {
		t.Fatalf("path = %q, want %q", got, want)
	}
}

// ShortTempDir returns a test-owned temporary directory rooted at a short path
// on Unix so socket paths stay under the platform limit even with a long TMPDIR.
func ShortTempDir(t *testing.T, prefix string) string {
	t.Helper()
	root := os.TempDir()
	switch runtime.GOOS {
	case "darwin":
		root = "/tmp"
	case "linux":
		// Keep socket fixtures on disk; compiler scratch still uses TMPDIR.
		root = "/var/tmp"
	}
	dir, err := os.MkdirTemp(root, prefix)
	if err != nil {
		t.Fatalf("MkdirTemp(%q, %q): %v", root, prefix, err)
	}
	t.Cleanup(func() {
		saveFailureDiagnostics(t, dir)
		_ = os.RemoveAll(dir)
	})
	return dir
}
