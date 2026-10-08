//go:build linux

package proctable

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

func readTerminationProcess(pid int) (terminationProcess, error) {
	return readTerminationProcessWith(pid, os.ReadFile)
}

func readTerminationProcessWith(pid int, read func(string) ([]byte, error)) (terminationProcess, error) {
	data, err := read(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
			return terminationProcess{}, fmt.Errorf("%w: PID %d", ErrProcessGone, pid)
		}
		return terminationProcess{}, fmt.Errorf("reading termination identity for PID %d: %w", pid, err)
	}
	_, pgid, start, ok, err := parseProcStatIdentity(string(data))
	opening := strings.IndexByte(string(data), '(')
	var recordedPID int
	if opening > 0 {
		recordedPID, _ = strconv.Atoi(strings.TrimSpace(string(data[:opening])))
	}
	fields := strings.Fields(string(data)[strings.LastIndexByte(string(data), ')')+1:])
	if err != nil || !ok || recordedPID != pid || start == "" || len(fields) == 0 || len(fields[0]) != 1 {
		return terminationProcess{}, fmt.Errorf("invalid termination identity for PID %d", pid)
	}
	return terminationProcess{PID: pid, PGID: pgid, Start: start, Runnable: fields[0] != "Z"}, nil
}

// Read only stat: comm/environ permissions must not hide a surviving member.
// An unreadable or malformed entry makes group completion uncertain, even if
// its membership cannot be determined. Vanished entries are the sole omission.
func censusTerminationGroup(pgid int) ([]terminationProcess, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	var members []terminationProcess
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 0 {
			continue
		}
		member, err := readTerminationProcess(pid)
		if errors.Is(err, ErrProcessGone) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if member.PGID == pgid {
			members = append(members, member)
		}
	}
	return members, nil
}
