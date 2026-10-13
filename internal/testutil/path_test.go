package testutil

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestShortTempDirRetainsUnixSocketHeadroomWithLongTMPDIR(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("Unix socket path bound is specific to the supported Unix roots")
	}
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), strings.Repeat("long-path-", 16)))
	dir := ShortTempDir(t, "gc-socket-")
	if path := filepath.Join(dir, "controller.sock"); len(path) >= 100 {
		t.Fatalf("socket path has insufficient platform headroom: %q", path)
	}
}

func TestTempFallbackOnlyForMissingRoot(t *testing.T) {
	primary := t.TempDir()
	fallback := t.TempDir()
	dir, err := mkdirTempWithFallback(primary, fallback, "gc-test-")
	if err != nil || filepath.Dir(dir) != primary {
		t.Fatalf("primary dir=%q err=%v", dir, err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	missing := filepath.Join(primary, "absent")
	other, err := mkdirTempWithFallback(missing, fallback, "gc-test-")
	if err != nil || filepath.Dir(other) != fallback {
		t.Fatalf("fallback dir=%q err=%v", other, err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(other) })
	file := filepath.Join(primary, "not-a-directory")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if dir, err := mkdirTempWithFallback(file, fallback, "gc-test-"); err == nil || dir != "" {
		t.Fatalf("non-missing root error must refuse fallback: dir=%q err=%v", dir, err)
	}
}
