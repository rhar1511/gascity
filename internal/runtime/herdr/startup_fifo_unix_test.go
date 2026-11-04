//go:build unix

package herdr

import (
	"context"
	"errors"
	"syscall"
	"testing"

	"github.com/gastownhall/gascity/internal/testutil"
)

func makeStartupDetectionFIFO(path string) error {
	return syscall.Mkfifo(path, 0o600)
}

func TestStartupDetectionNoticeCancelBeforeProbe(t *testing.T) {
	_, cancel := context.WithTimeout(context.Background(), testutil.ExecRaceTimeout)
	defer cancel()
	probed := make(chan struct{}, 1)
	stopNotice, done := newStartupDetectionNotice(t, t.TempDir(), func() { probed <- struct{}{}; cancel() })
	cancel()
	if err := stopNotice(); err != nil {
		t.Fatal(err)
	}
	cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), testutil.GoroutineRaceTimeout)
	defer cleanupCancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("reader result = %v, want aborted before notification", err)
		}
	case <-cleanupCtx.Done():
		t.Fatalf("reader stayed blocked after cancellation and abort: %v", cleanupCtx.Err())
	}
	select {
	case <-probed:
		t.Fatal("cancellation was mistaken for a detection notification")
	default:
	}
}
