//go:build !windows

package continuationprep

import (
	"os"
	"path/filepath"
	"testing"
)

func writeUnreadablePreparationSettings(t *testing.T, city string) {
	t.Helper()
	writePreparationSettings(t, city, 0o600)
	path := filepath.Join(city, ".gc", "settings.json")
	if _, err := os.ReadFile(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0); err != nil {
		t.Fatal(err)
	}
}
