//go:build linux || darwin

package proctable

import (
	"errors"
	"os"
	"os/exec"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestTerminateOwnedGroupConfirmsEveryMember(t *testing.T) {
	for _, test := range []struct {
		name                           string
		termExitsChild, killExitsChild bool
		wantSignals                    int
		wantError                      bool
	}{
		{name: "leader exits but child needs KILL", killExitsChild: true, wantSignals: 2},
		{name: "whole group exits on TERM", termExitsChild: true, wantSignals: 1},
		{name: "child survives KILL", wantSignals: 2, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := terminationProcess{PID: 100, PGID: 100, Start: "root", Runnable: true}
			members := map[int]terminationProcess{100: root, 101: {PID: 101, PGID: 100, Start: "child", Runnable: true}}
			group := fakeTerminationGroup(root, members)
			var signals []syscall.Signal
			group.kill = func(pid int, sig syscall.Signal) error {
				if pid != -100 {
					t.Fatalf("signal target = %d, want original group -100", pid)
				}
				signals = append(signals, sig)
				delete(members, 100)
				if test.termExitsChild || sig == syscall.SIGKILL && test.killExitsChild {
					delete(members, 101)
				}
				return nil
			}
			err := terminatePIDWith(100, group.signal, group.live, group.live, 0, 0)
			if (err != nil) != test.wantError || len(signals) != test.wantSignals {
				t.Fatalf("termination = %v, signals = %v; want error=%v, %d signals", err, signals, test.wantError, test.wantSignals)
			}
		})
	}
}

func TestTerminationGroupRefusesLostOrUnreadableIdentity(t *testing.T) {
	root := terminationProcess{PID: 100, PGID: 100, Start: "root", Runnable: true}
	for _, test := range []struct {
		name        string
		members     map[int]terminationProcess
		censusError error
	}{
		{name: "recycled leader", members: map[int]terminationProcess{100: {PID: 100, PGID: 100, Start: "replacement", Runnable: true}}},
		{name: "unknown survivor", members: map[int]terminationProcess{101: {PID: 101, PGID: 100, Start: "unknown", Runnable: true}}},
		{name: "unreadable census", censusError: syscall.EACCES},
	} {
		t.Run(test.name, func(t *testing.T) {
			group := fakeTerminationGroup(root, test.members)
			if test.censusError != nil {
				group.census = func(int) ([]terminationProcess, error) { return nil, test.censusError }
			}
			group.kill = func(int, syscall.Signal) error { t.Fatal("ambiguous group was signaled"); return nil }
			if err := group.signal(syscall.SIGKILL); err == nil {
				t.Fatal("ambiguous group accepted")
			}
		})
	}
}

func TestTerminationGroupAnchoredRefreshAndZombieConfirmation(t *testing.T) {
	root := terminationProcess{PID: 100, PGID: 100, Start: "root", Runnable: true}
	members := map[int]terminationProcess{100: root, 101: {PID: 101, PGID: 100, Start: "child", Runnable: true}}
	group := fakeTerminationGroup(root, members)
	if live, err := group.refresh(); !live || err != nil {
		t.Fatalf("anchored refresh = %v, %v", live, err)
	}
	delete(members, 100)
	members[102] = terminationProcess{PID: 102, PGID: 100, Start: "later child", Runnable: true}
	if live, err := group.refresh(); !live || err != nil || group.known[102] != "later child" {
		t.Fatalf("child-anchored refresh = %v, %v, known %v", live, err, group.known)
	}
	for pid, member := range members {
		member.Runnable = false
		members[pid] = member
	}
	if group.live(100) {
		t.Fatalf("zombies considered runnable: %v", group.err)
	}
}

func TestTerminationGroupNeverFallsBackToReusedRoot(t *testing.T) {
	root := terminationProcess{PID: 100, PGID: 100, Start: "root", Runnable: true}
	members := map[int]terminationProcess{100: root, 101: {PID: 101, PGID: 100, Start: "child", Runnable: true}}
	group := fakeTerminationGroup(root, members)
	if _, err := group.refresh(); err != nil {
		t.Fatal(err)
	}
	members[100] = terminationProcess{PID: 100, PGID: 200, Start: "replacement", Runnable: true}
	group.kill = func(pid int, _ syscall.Signal) error {
		if pid != -100 {
			t.Fatalf("recycled root was signaled: %d", pid)
		}
		return syscall.EPERM
	}
	if err := group.signal(syscall.SIGKILL); !errors.Is(err, syscall.EPERM) {
		t.Fatalf("signal failure = %v, want original EPERM", err)
	}
}

func TestTerminationGroupStaleCensusCannotPromoteForeignWitness(t *testing.T) {
	root := terminationProcess{PID: 100, PGID: 100, Start: "old root", Runnable: true}
	foreign := terminationProcess{PID: 101, PGID: 100, Start: "new unrelated group", Runnable: true}
	group := fakeTerminationGroup(root, map[int]terminationProcess{101: foreign})
	group.census = func(int) ([]terminationProcess, error) { return []terminationProcess{root, foreign}, nil }
	group.kill = func(int, syscall.Signal) error { t.Fatal("stale census promoted a foreign signal witness"); return nil }
	if err := group.signal(syscall.SIGKILL); err == nil {
		t.Fatal("stale census accepted without a fresh previously known witness")
	}
	if _, promoted := group.known[101]; promoted {
		t.Fatal("foreign identity was promoted")
	}
}

func fakeTerminationGroup(root terminationProcess, members map[int]terminationProcess) terminationGroup {
	return terminationGroup{
		root: root, known: map[int]string{root.PID: root.Start},
		read: func(pid int) (terminationProcess, error) {
			member, ok := members[pid]
			if !ok {
				return terminationProcess{}, ErrProcessGone
			}
			return member, nil
		},
		census: func(pgid int) ([]terminationProcess, error) {
			var rows []terminationProcess
			for _, member := range members {
				if member.PGID == pgid {
					rows = append(rows, member)
				}
			}
			return rows, nil
		},
	}
}

func TestKillByPIDRefusesLowPIDs(t *testing.T) {
	for _, pid := range []int{-1, 0, 1} {
		if err := KillByPID(pid); err == nil {
			t.Errorf("KillByPID(%d) succeeded, want error", pid)
		}
	}
}

func TestKillByPIDAlreadyGoneIsSuccess(t *testing.T) {
	// Spawn a short-lived process and wait for it to exit, then try to kill it.
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatalf("spawning test process: %v", err)
	}
	pid := cmd.ProcessState.Pid()
	// Process is already gone. KillByPID should return nil (ESRCH → success).
	if err := KillByPID(pid); err != nil {
		t.Fatalf("KillByPID(%d) for already-dead process: %v", pid, err)
	}
}

func TestSignalPIDGroupThenFallback(t *testing.T) {
	var got []int
	err := signalPIDWith(12345, syscall.SIGTERM, func(pid int, sig syscall.Signal) error {
		if sig != syscall.SIGTERM {
			t.Fatalf("signal = %v, want SIGTERM", sig)
		}
		got = append(got, pid)
		if pid < 0 {
			return syscall.ESRCH
		}
		return nil
	})
	if err != nil {
		t.Fatalf("signalPIDWith(): %v", err)
	}
	want := []int{-12345, 12345}
	if len(got) != len(want) {
		t.Fatalf("signal calls = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("signal calls = %v, want %v", got, want)
		}
	}
}

func TestSignalPIDGroupSuccessSkipsFallback(t *testing.T) {
	var got []int
	err := signalPIDWith(12345, syscall.SIGTERM, func(pid int, sig syscall.Signal) error {
		if sig != syscall.SIGTERM {
			t.Fatalf("signal = %v, want SIGTERM", sig)
		}
		got = append(got, pid)
		return nil
	})
	if err != nil {
		t.Fatalf("signalPIDWith(): %v", err)
	}
	want := []int{-12345}
	if !slices.Equal(got, want) {
		t.Fatalf("signal calls = %v, want %v", got, want)
	}
}

// TestKillByPIDConfirmedDeadBeforeReturn drives the injected core: a process
// still runnable after SIGKILL (e.g. wedged in D-state) must yield an error so
// a caller can refuse to start a racing replacement, while one that becomes
// dead (gone or zombie) after SIGKILL returns nil.
func TestKillByPIDConfirmedDeadBeforeReturn(t *testing.T) {
	t.Run("survives SIGKILL -> error", func(t *testing.T) {
		var signals []syscall.Signal
		kill := func(_ int, sig syscall.Signal) error {
			// Record every delivery attempt. signalPIDWith signals the process
			// group (negative pid) first and returns on success, so with this
			// always-succeeding fake these are the group deliveries; the
			// assertion below only checks the final escalation is SIGKILL.
			signals = append(signals, sig)
			return nil
		}
		termLive := func(int) bool { return true } // never exits on SIGTERM
		runLive := func(int) bool { return true }  // survives SIGKILL too
		err := killByPID(4321, kill, termLive, runLive, 5*time.Millisecond, 5*time.Millisecond)
		if err == nil {
			t.Fatal("killByPID returned nil for a process that survived SIGKILL")
		}
		if !strings.Contains(err.Error(), "not confirmed dead") {
			t.Fatalf("error = %v, want 'not confirmed dead'", err)
		}
		if len(signals) == 0 || signals[len(signals)-1] != syscall.SIGKILL {
			t.Fatalf("signals = %v, want SIGKILL escalation", signals)
		}
	})

	t.Run("dies after SIGKILL -> nil", func(t *testing.T) {
		kill := func(int, syscall.Signal) error { return nil }
		termLive := func(int) bool { return true } // ignores SIGTERM
		var kills int
		runLive := func(int) bool {
			kills++
			return kills <= 1 // alive on first confirm poll, dead after
		}
		if err := killByPID(4321, kill, termLive, runLive, 5*time.Millisecond, time.Second); err != nil {
			t.Fatalf("killByPID: %v", err)
		}
	})

	t.Run("exits during SIGTERM grace -> no SIGKILL", func(t *testing.T) {
		var sawKill bool
		kill := func(_ int, sig syscall.Signal) error {
			if sig == syscall.SIGKILL {
				sawKill = true
			}
			return nil
		}
		var polls int
		termLive := func(int) bool {
			polls++
			return polls <= 1 // alive at entry, exits before grace elapses
		}
		runLive := func(int) bool { return false }
		if err := killByPID(4321, kill, termLive, runLive, time.Second, time.Second); err != nil {
			t.Fatalf("killByPID: %v", err)
		}
		if sawKill {
			t.Fatal("SIGKILL sent even though the process exited during grace")
		}
	})
}

func TestWaitUntilRespectsZeroTimeout(t *testing.T) {
	if !waitUntil(func() bool { return true }, 0) {
		t.Fatal("waitUntil should observe an already-satisfied condition at zero timeout")
	}
	if waitUntil(func() bool { return false }, 0) {
		t.Fatal("waitUntil should report false when the condition never holds at zero timeout")
	}
}

// TestKillLivenessFuncsAreIdentityBound pins the recycled-PID protection at the
// wiring, not just at the helper.
//
// The SIGTERM-grace probe used to be a bare kill(0) existence check, and it is
// what gates the SIGKILL wave: a target that answered SIGTERM and was reaped,
// whose PID was recycled before the grace expired, still read as "alive", so
// SIGKILL was sent to the new owner. signalPIDWith tries kill(-pid) first, so if
// that owner leads a process group — every tmux pane command and every Setpgid'd
// daemon does — the whole unrelated group dies and the call reports success.
//
// Both probes must therefore be bound to the ORIGINAL target's start-time
// identity: asked about a different live PID they must answer false. Restoring
// either to a bare existence check fails this.
func TestKillLivenessFuncsAreIdentityBound(t *testing.T) {
	self := os.Getpid()
	termLive, runLive := killLivenessFuncsForPID(self)

	if !termLive(self) {
		t.Error("termLive(self) = false; the probe must recognize its own target")
	}
	if !runLive(self) {
		t.Error("runLive(self) = false; the probe must recognize its own target")
	}

	// PID 1 is always live and is never us, so it stands in for a recycled PID
	// now owned by an unrelated process.
	const recycled = 1
	if termLive(recycled) {
		t.Error("termLive reported a DIFFERENT live pid as our target: a recycled PID would receive a process-group SIGKILL")
	}
	if runLive(recycled) {
		t.Error("runLive reported a DIFFERENT live pid as our target")
	}
}
