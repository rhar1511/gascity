// Package subprocesssweep selects integration-test subprocesses that belong to
// an owned gc-integration temp root based on their command lines. It does not
// verify /proc/<pid>/exe or signal processes.
package subprocesssweep

import (
	"path/filepath"
	"strconv"
	"strings"
)

const integrationRunPrefix = "gc-integration-"

// Process contains the parent PID and command line needed to select an owned
// subprocess tree from a process-table snapshot.
type Process struct {
	PPID int
	Cmd  string
}

// KillSet returns PIDs in stale or current integration-owned supervisor and
// control-dispatcher trees. A root is eligible only when its command line has
// exactly <tempParent>/gc-integration-<ownerPID>-<suffix>/bin/gc as argv[0] and
// its first arguments identify a supported root command. This is command-line
// evidence, not verification of the process executable. Unknown ownership is
// refused. Children are included only by their parent relationship to an
// eligible root; command names alone never make a process eligible.
func KillSet(procs map[int]Process, tempParent string, selfPID int, ownerAlive func(int) bool) map[int]bool {
	killSet := make(map[int]bool)
	if len(procs) == 0 || selfPID <= 0 || ownerAlive == nil {
		return killSet
	}
	tempParent = filepath.Clean(tempParent)
	if !filepath.IsAbs(tempParent) {
		return killSet
	}

	roots := make(map[int]bool)
	foreignRoots := make(map[int]bool)
	children := make(map[int][]int, len(procs))
	for pid, info := range procs {
		ownerPID, ok := ownedRootOwnerPID(info.Cmd, tempParent)
		if ok {
			if ownerPID == selfPID || !ownerAlive(ownerPID) {
				roots[pid] = true
			} else {
				foreignRoots[pid] = true
			}
		}
		children[info.PPID] = append(children[info.PPID], pid)
	}

	// A live foreign root protects its entire current descendant closure.
	// Filtering just the root while later independently seeding an eligible
	// nested root would still let the sweep cross into that live run.
	protected := make(map[int]bool)
	protectedQueue := make([]int, 0, len(foreignRoots))
	for pid := range foreignRoots {
		protectedQueue = append(protectedQueue, pid)
	}
	for len(protectedQueue) > 0 {
		pid := protectedQueue[0]
		protectedQueue = protectedQueue[1:]
		if protected[pid] {
			continue
		}
		protected[pid] = true
		protectedQueue = append(protectedQueue, children[pid]...)
	}

	queue := make([]int, 0, len(roots))
	for pid := range roots {
		if !protected[pid] {
			queue = append(queue, pid)
		}
	}
	for len(queue) > 0 {
		pid := queue[0]
		queue = queue[1:]
		if killSet[pid] {
			continue
		}
		if protected[pid] {
			continue
		}
		killSet[pid] = true
		queue = append(queue, children[pid]...)
	}
	return killSet
}

func ownedRootOwnerPID(cmd, tempParent string) (int, bool) {
	fields := strings.Fields(cmd)
	if len(fields) < 3 || !isOwnedRootCommand(fields[1:]) {
		return 0, false
	}
	argv0 := fields[0]
	if !filepath.IsAbs(argv0) || filepath.Clean(argv0) != argv0 {
		return 0, false
	}
	rel, err := filepath.Rel(tempParent, argv0)
	if err != nil {
		return 0, false
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) != 3 || parts[1] != "bin" || parts[2] != "gc" {
		return 0, false
	}
	return ownerPIDFromRunDir(parts[0])
}

func isOwnedRootCommand(args []string) bool {
	if len(args) >= 2 && args[0] == "supervisor" && args[1] == "run" {
		return true
	}
	return len(args) >= 5 &&
		args[0] == "convoy" &&
		args[1] == "control" &&
		args[2] == "--serve" &&
		args[3] == "--follow" &&
		args[4] == "control-dispatcher"
}

func ownerPIDFromRunDir(name string) (int, bool) {
	if !strings.HasPrefix(name, integrationRunPrefix) {
		return 0, false
	}
	suffix := strings.TrimPrefix(name, integrationRunPrefix)
	dash := strings.IndexByte(suffix, '-')
	if dash <= 0 || dash == len(suffix)-1 {
		return 0, false
	}
	pid, err := strconv.Atoi(suffix[:dash])
	if err != nil || pid <= 0 || !allASCIIDigits(suffix[:dash]) {
		return 0, false
	}
	return pid, true
}

func allASCIIDigits(value string) bool {
	if value == "" {
		return false
	}
	for i := 0; i < len(value); i++ {
		if value[i] < '0' || value[i] > '9' {
			return false
		}
	}
	return true
}
