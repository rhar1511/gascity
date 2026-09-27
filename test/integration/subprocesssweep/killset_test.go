package subprocesssweep

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func TestKillSetSelectsOwnedRootsAndTheirDescendants(t *testing.T) {
	const tempParent = "/tmp"
	const selfPID = 777
	const deadOwnerPID = 123
	const liveOwnerPID = 999
	agentScript := "/repo/test/agents/graph-dispatch.sh"

	procs := map[int]Process{
		10: {PPID: 1, Cmd: gcCommand(deadOwnerPID, "stale", "supervisor", "run")},
		11: {PPID: 10, Cmd: "owned supervisor child"},
		12: {PPID: 11, Cmd: "owned supervisor grandchild"},
		20: {PPID: 1, Cmd: gcCommand(selfPID, "current", "supervisor", "run")},
		21: {PPID: 20, Cmd: "current supervisor child"},
		30: {PPID: 1, Cmd: gcCommand(liveOwnerPID, "foreign", "supervisor", "run")},
		31: {PPID: 30, Cmd: "live foreign supervisor child"},
		40: {PPID: 1, Cmd: "bash " + agentScript},
		41: {PPID: 40, Cmd: "graph dispatch child"},
		50: {PPID: 1, Cmd: "bd ready --label=pool:polecat --unassigned --json --limit=1"},
		51: {PPID: 1, Cmd: "bd ready --assignee=worker --json --limit=1"},
		60: {PPID: 1, Cmd: "ordinary unrelated process"},
	}

	got := KillSet(procs, tempParent, selfPID, func(pid int) bool {
		return pid == liveOwnerPID || pid == selfPID
	})

	for _, pid := range []int{10, 11, 12, 20, 21} {
		if !got[pid] {
			t.Errorf("kill set missing owned pid %d: %#v", pid, got)
		}
	}
	for _, pid := range []int{30, 31, 40, 41, 50, 51, 60} {
		if got[pid] {
			t.Errorf("kill set included unowned or live-foreign pid %d: %#v", pid, got)
		}
	}
}

func TestKillSetRecognizesOwnedControlDispatcherRoot(t *testing.T) {
	const tempParent = "/tmp"
	procs := map[int]Process{
		10: {PPID: 1, Cmd: gcCommand(123, "stale", "convoy", "control", "--serve", "--follow", "control-dispatcher")},
		11: {PPID: 10, Cmd: "owned control dispatcher child"},
	}

	got := KillSet(procs, tempParent, 777, func(int) bool { return false })
	if !got[10] || !got[11] {
		t.Fatalf("kill set = %#v, want owned control root and child", got)
	}
}

func TestKillSetPreservesLiveForeignRootNestedUnderOwnedTree(t *testing.T) {
	const tempParent = "/tmp"
	procs := map[int]Process{
		10: {PPID: 1, Cmd: gcCommand(123, "stale", "supervisor", "run")},
		11: {PPID: 10, Cmd: gcCommand(999, "foreign", "supervisor", "run")},
		12: {PPID: 11, Cmd: gcCommand(321, "nested-stale", "supervisor", "run")},
		13: {PPID: 12, Cmd: "nested stale supervisor child"},
	}

	got := KillSet(procs, tempParent, 777, func(pid int) bool { return pid == 999 })
	if !got[10] {
		t.Fatalf("kill set = %#v, want stale owned root", got)
	}
	for _, pid := range []int{11, 12, 13} {
		if got[pid] {
			t.Errorf("kill set crossed live foreign subtree at pid %d: %#v", pid, got)
		}
	}
}

func TestKillSetFailsClosedOnUnownedOrMalformedRoot(t *testing.T) {
	const tempParent = "/tmp"
	procs := map[int]Process{
		10: {PPID: 1, Cmd: "/tmp/gc-integration-123/bin/gc supervisor run"},
		11: {PPID: 10, Cmd: "child of suffixless root"},
		20: {PPID: 1, Cmd: "/tmp/gc-integration-abc-stale/bin/gc supervisor run"},
		21: {PPID: 20, Cmd: "child of invalid-owner root"},
		30: {PPID: 1, Cmd: "/var/tmp/gc-integration-123-stale/bin/gc supervisor run"},
		31: {PPID: 30, Cmd: "child of outside-parent root"},
		40: {PPID: 1, Cmd: "/tmp/nested/gc-integration-123-stale/bin/gc supervisor run"},
		41: {PPID: 40, Cmd: "child of nested root"},
		50: {PPID: 1, Cmd: "/usr/bin/gc supervisor run --note=/tmp/gc-integration-123-stale/bin/gc"},
		51: {PPID: 50, Cmd: "child of marker-in-argument root"},
		60: {PPID: 1, Cmd: gcCommand(123, "stale", "wrapper", "supervisor", "run")},
		61: {PPID: 60, Cmd: "child of misplaced-command root"},
		70: {PPID: 1, Cmd: filepath.Join(tempParent, "gc-integration-123-stale", "bin", "not-gc") + " supervisor run"},
		71: {PPID: 70, Cmd: "child of wrong-binary root"},
		80: {PPID: 1, Cmd: "/tmp/gc-integration-+123-stale/bin/gc supervisor run"},
		81: {PPID: 80, Cmd: "child of signed-owner root"},
	}

	got := KillSet(procs, tempParent, 777, func(int) bool { return false })
	if len(got) != 0 {
		t.Fatalf("kill set = %#v, want empty for roots without exact fixture ownership", got)
	}
}

func TestKillSetRequiresOwnerLivenessEvidence(t *testing.T) {
	procs := map[int]Process{
		10: {PPID: 1, Cmd: gcCommand(123, "stale", "supervisor", "run")},
	}
	if got := KillSet(procs, "/tmp", 777, nil); len(got) != 0 {
		t.Fatalf("KillSet with unknown owner status = %#v, want empty", got)
	}
}

func gcCommand(ownerPID int, suffix string, args ...string) string {
	const tempParent = "/tmp"
	bin := filepath.Join(tempParent, fmt.Sprintf("gc-integration-%d-%s", ownerPID, suffix), "bin", "gc")
	return bin + " " + strings.Join(args, " ")
}
