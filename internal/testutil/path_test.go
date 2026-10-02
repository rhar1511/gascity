package testutil

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	_ "github.com/gastownhall/gascity/internal/testenv"
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
