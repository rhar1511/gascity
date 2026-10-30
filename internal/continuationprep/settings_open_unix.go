//go:build unix

package continuationprep

import (
	"os"
	"syscall"
)

func openContinuationSettings(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
}
