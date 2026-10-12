//go:build integration && darwin

package integration

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// Match stock Beads' procid.Capture on Darwin exactly; millisecond or ps
// timestamps lose the microseconds needed to compare its persisted token.
func graphFixtureProcessBirth(pid int) (string, error) {
	info, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return "", err
	}
	started := info.Proc.P_starttime
	if started.Sec <= 0 || started.Usec < 0 || started.Usec >= 1_000_000 {
		return "", fmt.Errorf("invalid Darwin process birth for pid %d", pid)
	}
	return fmt.Sprintf("darwin-v1:%d.%d", started.Sec, started.Usec), nil
}
