//go:build integration && linux

package acp

import (
	"bufio"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/pidutil"
	"github.com/gastownhall/gascity/internal/runtime"
)

// childPIDs returns pid's direct children from procfs.
func childPIDs(t *testing.T, pid int) []int {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "task", strconv.Itoa(pid), "children"))
	if err != nil {
		t.Fatalf("read children of %d: %v", pid, err)
	}
	var out []int
	for _, field := range strings.Fields(string(data)) {
		child, err := strconv.Atoi(field)
		if err != nil {
			t.Fatalf("parse child pid %q: %v", field, err)
		}
		out = append(out, child)
	}
	return out
}

// TestTerminateRuntimeWaitsForWholeProcessGroup makes the group leader exit on
// SIGTERM while its child ignores that signal. A replacement cannot safely
// start until TerminateRuntime has also confirmed the child dead.
func TestTerminateRuntimeWaitsForWholeProcessGroup(t *testing.T) {
	pidFIFO := filepath.Join(t.TempDir(), "child.pid")
	if err := syscall.Mkfifo(pidFIFO, 0o600); err != nil {
		t.Fatalf("create private child notification: %v", err)
	}
	fifo, err := os.OpenFile(pidFIFO, os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("open private child notification: %v", err)
	}
	t.Cleanup(func() { _ = fifo.Close() })
	if err := fifo.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatalf("bound private child notification: %v", err)
	}
	// Hold the child launch until the leader's identity and cleanup are bound.
	cmd := exec.Command("sh", "-c", `read -r ready || exit; sh -c 'trap "" TERM; printf "%s\n" "$$" > "$1"; exec sleep 600' sh "$1" & wait`, "sh", pidFIFO)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	start, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("create private launch barrier: %v", err)
	}
	t.Cleanup(func() { _ = start.Close() })
	if err := cmd.Start(); err != nil {
		t.Fatalf("start private process group: %v", err)
	}
	rootPID := cmd.Process.Pid
	rootStart, err := pidutil.StartTime(rootPID)
	if err != nil || rootStart == "" {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("read group leader identity: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var childPID int
	var childStart string
	t.Cleanup(func() {
		if pidutil.AliveWithStartTime(rootPID, rootStart) {
			// The identity-bound leader still owns this group, including a
			// child whose notification failed before its PID could be read.
			_ = syscall.Kill(-rootPID, syscall.SIGKILL)
		}
		if childStart != "" && pidutil.AliveWithStartTime(childPID, childStart) {
			_ = syscall.Kill(childPID, syscall.SIGKILL)
		}
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("private group leader was not waited during cleanup")
		}
	})
	if _, err := start.Write([]byte("start\n")); err != nil {
		t.Fatalf("release private launch barrier: %v", err)
	}
	data, err := bufio.NewReader(fifo).ReadString('\n')
	if err != nil {
		t.Fatalf("read private child notification: %v", err)
	}
	childPID, err = strconv.Atoi(strings.TrimSpace(data))
	if err != nil || childPID <= 1 {
		t.Fatalf("parse private child PID %q: %v", data, err)
	}
	childStart, err = pidutil.StartTime(childPID)
	if err != nil || childStart == "" {
		t.Fatalf("read private child identity: %v", err)
	}
	group, err := syscall.Getpgid(childPID)
	if err != nil || group != rootPID {
		t.Fatalf("private child process group = %d, want %d: %v", group, rootPID, err)
	}
	p := NewProviderWithDir(t.TempDir(), Config{})
	if err := p.TerminateRuntime(runtime.LiveRuntime{PID: rootPID, SessionID: "private-group"}); err != nil {
		t.Fatalf("TerminateRuntime: %v", err)
	}
	if pidutil.AliveWithStartTime(childPID, childStart) {
		t.Fatalf("TerminateRuntime returned while original process-group child %d was still runnable", childPID)
	}
}

// TestACPOrphanReapedAfterProviderRestart runs the real fakeacp agent through
// the production (seam-backed) provider, drops the provider's in-process state
// and control-socket listener the way a supervisor death does, and proves a fresh provider on the same
// state directory reports the surviving runtime as an untracked orphan and
// terminates its whole process group.
func TestACPOrphanReapedAfterProviderRestart(t *testing.T) {
	var fixture acpConformanceFixture
	if err := prepareACPConformanceFixture(t, &fixture); err != nil {
		t.Fatal(err)
	}
	raw := NewProviderWithDir(fixture.dir, Config{})
	name := testName()
	sessionID := "sid-" + name
	city := t.TempDir()
	if err := seamBack(raw).Start(context.Background(), name, runtime.Config{
		Command: fixture.command,
		WorkDir: t.TempDir(),
		Env:     map[string]string{"GC_SESSION_ID": sessionID, "GC_CITY_PATH": city},
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = raw.Stop(name) })

	raw.mu.Lock()
	agentPID := raw.conns[name].cmd.Process.Pid
	raw.mu.Unlock()
	pids := append([]int{agentPID}, childPIDs(t, agentPID)...)

	var found []runtime.LiveRuntime
	scanSnapshot(t, func() { found = findOnly(t, seamBack(raw), sessionID) }, pids...)
	if len(found) != 1 || found[0].PID != agentPID || !found[0].IsTracked || found[0].City != city {
		t.Fatalf("live session found = %+v, want tracked root pid %d in city %s", found, agentPID, city)
	}

	simulateOwnerDeath(t, raw, name)
	restarted := NewSeamBackedWithDir(fixture.dir, Config{}).(runtime.ProcessTableScanner)
	scanSnapshot(t, func() { found = findOnly(t, restarted, sessionID) }, pids...)
	if len(found) != 1 || found[0].PID != agentPID || found[0].IsTracked {
		t.Fatalf("after restart found = %+v, want untracked root pid %d", found, agentPID)
	}
	if err := restarted.TerminateRuntime(found[0]); err != nil {
		t.Fatalf("TerminateRuntime: %v", err)
	}
	scanSnapshot(t, func() { found = findOnly(t, restarted, sessionID) }, pids...)
	if len(found) != 0 {
		t.Fatalf("after terminate found = %+v, want the whole process group gone", found)
	}
}
