//go:build !linux

package contract

import "os/exec"

// Observe executes normally on other platforms but cannot claim descriptor-bound identity.
// The zero artifact keeps native eligibility unconfirmed rather than deriving
// proof from a mutable pathname or from the command's version string.
func (o *BeadsExecutableObservation) Observe(_ *exec.Cmd) (func() error, error) {
	return nil, nil
}
