//go:build !unix

package herdr

import "errors"

func makeStartupDetectionFIFO(string) error {
	return errors.New("startup detection FIFO requires a Unix platform")
}
