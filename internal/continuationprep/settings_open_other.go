//go:build !unix

package continuationprep

import "os"

// Keep the regular-file open semantics on other platforms. The Unix FIFO
// nonblocking guarantee does not establish bounded device opens on every OS.
func openContinuationSettings(path string) (*os.File, error) {
	return os.Open(path)
}
