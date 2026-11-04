//go:build unix

package continuationprep

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"syscall"
	"testing"

	"github.com/gastownhall/gascity/internal/session"
)

// The isolated test runner bounds this regression with -timeout. Calling
// Prepare directly avoids leaking a blocked helper goroutine or adding another
// subprocess to the repository's checked source-resource census.
func TestPrepareRejectsSettingsFIFOWithoutBlocking(t *testing.T) {
	setup := func(city string) {
		path := filepath.Join(city, ".gc", "settings.json")
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := syscall.Mkfifo(path, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	city, cfg, info, expected, token := configuredContinuationFixture(t, preparationFixtureOptions{family: "claude", setup: setup})
	got, err := Prepare(Request{CityPath: city, City: cfg, Session: info, Expected: expected, CaptureSource: "native.session-start", NativeToken: token})
	if !errors.Is(err, session.ErrContinuationCommand) || !reflect.DeepEqual(got, Prepared{}) {
		t.Fatalf("FIFO settings accepted: %+v, %v", got, err)
	}
}

func TestSettingsOpenRejectsFIFOReplacementAndClosesDescriptor(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(path, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path, path+".original"); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	assertSettingsReplacementRefusedAndClosed(t, path, before)
}
