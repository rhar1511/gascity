package continuationprep

import (
	"os"
	"path/filepath"
	"testing"
)

func writePreparationSettings(t *testing.T, city string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(city, ".gc"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(city, ".gc", "settings.json"), []byte("{}\n"), mode); err != nil {
		t.Fatal(err)
	}
}
