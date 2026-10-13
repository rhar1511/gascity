//go:build darwin

package proctable

import (
	"fmt"

	"golang.org/x/sys/unix"
)

func readTerminationProcess(pid int) (terminationProcess, error) {
	process, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		if darwinKinfoErrorMeansGone(pid, err, func(pid int) error { return unix.Kill(pid, 0) }) {
			return terminationProcess{}, fmt.Errorf("%w: PID %d", ErrProcessGone, pid)
		}
		return terminationProcess{}, err
	}
	if process == nil || int(process.Proc.P_pid) != pid {
		return terminationProcess{}, fmt.Errorf("missing termination identity for PID %d", pid)
	}
	return darwinTerminationProcess(*process)
}

func darwinTerminationProcess(process unix.KinfoProc) (terminationProcess, error) {
	record, ok := darwinProcessRecord(process)
	if !ok || process.Proc.P_stat <= 0 {
		return terminationProcess{}, fmt.Errorf("invalid kernel termination identity")
	}
	// SZOMB = 5 in Darwin's <sys/proc.h>. Use the same kernel record for
	// membership, high-resolution identity and state; no wall-clock ps token.
	return terminationProcess{PID: record.PID, PGID: record.PGID, Start: record.StartTime, Runnable: process.Proc.P_stat != 5}, nil
}

func censusTerminationGroup(pgid int) ([]terminationProcess, error) {
	processes, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return nil, err
	}
	var members []terminationProcess
	for _, process := range processes {
		if int(process.Eproc.Pgid) != pgid {
			continue
		}
		member, err := darwinTerminationProcess(process)
		if err != nil {
			return nil, err
		}
		members = append(members, member)
	}
	return members, nil
}
