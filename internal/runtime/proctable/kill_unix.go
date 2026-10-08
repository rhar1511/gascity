//go:build linux || darwin

package proctable

import (
	"errors"
	"fmt"
	"syscall"
	"time"

	"github.com/gastownhall/gascity/internal/pidutil"
	"github.com/gastownhall/gascity/internal/runtime"
)

// KillByPID terminates pid with SIGTERM, then SIGKILL after
// runtime.ManagedProcessStopGrace, then waits (bounded by
// runtime.ManagedProcessReapGrace) for the process and its owned process group
// to be confirmed dead — gone or zombies — before returning. Already-gone processes are success. A process
// that survives its own SIGKILL past the reap grace (e.g. wedged in D-state
// under I/O) yields an error so callers can refuse to start a name-reused
// replacement that would race it for the same work.
func KillByPID(pid int) error {
	if pid <= 1 {
		return fmt.Errorf("proctable: refusing to kill PID %d", pid)
	}
	root, err := readTerminationProcess(pid)
	if errors.Is(err, ErrProcessGone) {
		return nil
	}
	if err != nil {
		return err
	}
	if root.PGID == pid {
		group := terminationGroup{root: root, known: map[int]string{pid: root.Start}, read: readTerminationProcess, census: censusTerminationGroup, kill: syscall.Kill}
		return terminatePIDWith(pid, group.signal, group.live, group.live, runtime.ManagedProcessStopGrace, runtime.ManagedProcessReapGrace)
	}
	// An inherited group belongs to somebody else. Only the captured process
	// may receive a signal, even if its numeric PID later names another group.
	termLive, runLive := killLivenessFuncsForPID(pid)
	signal := func(sig syscall.Signal) error {
		current, err := readTerminationProcess(pid)
		if errors.Is(err, ErrProcessGone) {
			return nil
		}
		if err != nil {
			return err
		}
		if current.Start != root.Start {
			return nil
		}
		err = syscall.Kill(pid, sig)
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		return err
	}
	return terminatePIDWith(pid, signal, termLive, runLive, runtime.ManagedProcessStopGrace, runtime.ManagedProcessReapGrace)
}

// killLivenessFuncsForPID builds the two liveness probes KillByPID signals
// against, both bound to the target's start-time identity captured up front.
//
// Extracted so the IDENTITY BINDING itself is testable: both probes must report
// false for a different live PID, which is what stops a recycled PID from
// receiving a process-group SIGKILL. A version that returns bare existence
// checks passes every process-level test and still ships the bug.
func killLivenessFuncsForPID(pid int) (termLive, runLive func(int) bool) {
	startTime, _ := pidutil.StartTime(pid)
	return func(p int) bool { return pidAliveWithIdentity(p, startTime) },
		func(p int) bool { return pidutil.AliveWithStartTime(p, startTime) }
}

// pidAliveWithIdentity is the cheap kill(0) liveness used across the SIGTERM
// grace, plus recycled-PID protection.
//
// The bare kill(0) form was pure existence with zero identity, and it is what
// gates the SIGKILL wave: a target that answered SIGTERM and was reaped, whose
// PID was then recycled by an unrelated process before the grace expired, still
// read as "alive" — so SIGKILL was sent to it. signalPIDWith tries kill(-pid)
// first, so if the recycled PID happens to lead a process group (every tmux pane
// command and every Setpgid'd daemon does) the entire unrelated group dies and
// the call returns success. Validating the start-time identity on every poll
// means a recycled PID reads as dead, waitUntil succeeds, and no second wave is
// ever sent.
//
// Zombie semantics are preserved deliberately: this keeps kill(0)'s view, in
// which a zombie still counts as live, matching the pre-existing SIGTERM-grace
// behavior. Only the identity check is added. An unreadable or absent start time
// keeps the conservative "still alive" answer rather than inventing a death.
func pidAliveWithIdentity(pid int, startTime string) bool {
	if !pidAlive(pid) {
		return false
	}
	if startTime == "" {
		return true
	}
	current, err := pidutil.StartTime(pid)
	if err != nil {
		return true
	}
	return current == startTime
}

// killByPID is the signal/confirm core with its syscalls injected so the
// confirmed-dead-before-return contract can be unit-tested without real
// processes. termLive is the cheap kill(0) liveness used during the SIGTERM
// grace window (a zombie still counts as live here, matching prior behavior).
// runLive reports whether the process is still runnable — false once it is gone
// or a zombie, since a zombie can no longer execute and therefore cannot race a
// replacement.
func killByPID(
	pid int,
	kill func(int, syscall.Signal) error,
	termLive func(int) bool,
	runLive func(int) bool,
	grace, reapGrace time.Duration,
) error {
	return terminatePIDWith(pid, func(sig syscall.Signal) error { return signalPIDWith(pid, sig, kill) }, termLive, runLive, grace, reapGrace)
}

func terminatePIDWith(pid int, signal func(syscall.Signal) error, termLive, runLive func(int) bool, grace, reapGrace time.Duration) error {
	if pid <= 1 {
		return fmt.Errorf("proctable: refusing to kill PID %d", pid)
	}
	if !termLive(pid) {
		return nil
	}
	if err := signal(syscall.SIGTERM); err != nil {
		return fmt.Errorf("signal PID %d with SIGTERM: %w", pid, err)
	}
	if waitUntil(func() bool { return !termLive(pid) }, grace) {
		return nil
	}
	// Re-validate identity immediately before the second wave. waitUntil's last
	// poll already implies this, but the check is written out because it is a
	// contract, not an optimization: every signal wave is preceded by a fresh
	// identity read, so a PID reaped and recycled between the poll and the signal
	// cannot receive a process-group SIGKILL.
	if !termLive(pid) {
		return nil
	}
	if err := signal(syscall.SIGKILL); err != nil {
		return fmt.Errorf("signal PID %d with SIGKILL: %w", pid, err)
	}
	if waitUntil(func() bool { return !runLive(pid) }, reapGrace) {
		return nil
	}
	return fmt.Errorf("proctable: PID %d still runnable %s after SIGKILL (not confirmed dead)", pid, reapGrace)
}

// waitUntil polls done at 25ms until it reports true or timeout elapses,
// returning done's final result. Checked once up front so a zero timeout still
// observes an already-satisfied condition.
func waitUntil(done func() bool, timeout time.Duration) bool {
	if done() {
		return true
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-deadline.C:
			return done()
		case <-ticker.C:
			if done() {
				return true
			}
		}
	}
}

func signalPIDWith(pid int, sig syscall.Signal, kill func(int, syscall.Signal) error) error {
	if err := kill(-pid, sig); err == nil {
		return nil
	}
	err := kill(pid, sig)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}

func pidAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// terminationProcess keeps state and identity from the same kernel record.
// These teardown readers do not enrich records with comm/environment data or
// silently omit unreadable processes as the read-only discovery snapshot does.
type terminationProcess struct {
	PID, PGID int
	Start     string
	Runnable  bool
}

type terminationGroup struct {
	root   terminationProcess
	known  map[int]string
	read   func(int) (terminationProcess, error)
	census func(int) ([]terminationProcess, error)
	kill   func(int, syscall.Signal) error
	err    error
}

// refresh may extend membership only while a previously captured identity
// still belongs to the group. An unfamiliar group after every witness has
// disappeared is ambiguous: refuse rather than claim death or signal it.
func (g *terminationGroup) refresh() (bool, error) {
	members, err := g.census(g.root.PGID)
	if err != nil {
		return false, err
	}
	anchored := false
	for _, member := range members {
		if member.Start != "" && member.PGID == g.root.PGID && g.known[member.PID] == member.Start {
			current, readErr := g.read(member.PID)
			if errors.Is(readErr, ErrProcessGone) {
				continue
			}
			if readErr != nil {
				return false, readErr
			}
			if current.Start == member.Start && current.PGID == g.root.PGID {
				anchored = true
				break
			}
		}
	}
	if len(members) > 0 && !anchored {
		return false, fmt.Errorf("proctable: group %d has no original identity witness", g.root.PGID)
	}
	live := false
	for _, member := range members {
		if member.PID <= 1 || member.PGID != g.root.PGID || member.Start == "" {
			return false, fmt.Errorf("proctable: incomplete identity in group %d", g.root.PGID)
		}
		g.known[member.PID] = member.Start
		live = live || member.Runnable
	}
	return live, nil
}

// The grace polls read only captured PIDs. Before reporting completion, take a
// complete group census; a leader's exit alone can never establish success.
func (g *terminationGroup) live(int) bool {
	if g.err != nil {
		return true
	}
	for pid, start := range g.known {
		member, err := g.read(pid)
		if errors.Is(err, ErrProcessGone) {
			continue
		}
		if err != nil {
			g.err = err
			return true
		}
		if member.Start == start && member.PGID == g.root.PGID && member.Runnable {
			return true
		}
	}
	live, err := g.refresh()
	g.err = err
	return live || err != nil
}

func (g *terminationGroup) signal(sig syscall.Signal) error {
	if g.err != nil {
		return g.err
	}
	live, err := g.refresh()
	if err != nil || !live {
		return err
	}
	// Fence each wave with a fresh identity AND group read. The remaining
	// witness can be a child after its original leader has been reaped.
	for pid, start := range g.known {
		member, err := g.read(pid)
		if errors.Is(err, ErrProcessGone) {
			continue
		}
		if err != nil {
			return err
		}
		if member.Start != start || member.PGID != g.root.PGID {
			continue
		}
		err = g.kill(-g.root.PGID, sig)
		if err == nil || errors.Is(err, syscall.ESRCH) {
			return nil
		}
		// A failed group signal may fall back only to the original root,
		// never the reused numeric PID of a departed leader.
		root, readErr := g.read(g.root.PID)
		if readErr != nil || root.Start != g.root.Start {
			return fmt.Errorf("signal group %d: %w", g.root.PGID, err)
		}
		return g.kill(g.root.PID, sig)
	}
	return fmt.Errorf("proctable: group %d lost every original identity witness before signal", g.root.PGID)
}
