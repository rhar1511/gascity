package herdr

import (
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

func TestProviderLiveSharedKindPreservesForeignPane(t *testing.T) {
	requireLiveHerdr(t)
	session := fmt.Sprintf("gctest-ownership-%d-%d", os.Getpid(), time.Now().UnixNano())
	p := New(session, t.TempDir(), t.TempDir(), 0, 0)
	t.Cleanup(func() { _ = p.Stop("worker"); _ = p.TeardownServer() })
	ctx := t.Context()
	if err := p.Start(ctx, "worker", runtime.Config{WorkDir: t.TempDir(), Command: "exec sleep 120"}); err != nil {
		t.Fatal(err)
	}
	owned, err := p.GetMeta("worker", metaBoundPane)
	if err != nil || owned == "" {
		t.Fatalf("owned binding=%q,%v", owned, err)
	}
	_, peer, err := p.c.ensurePlacement(ctx, "peer", "external-peer", t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	p.waitPaneShellReady(ctx, peer)
	if err := p.c.paneRunCommand(ctx, peer, "exec sleep 120"); err != nil {
		t.Fatal(err)
	}
	p.waitPaneLaunched(ctx, peer, "exec sleep 120")
	if probe, err := p.probePane(ctx, peer); err != nil || !probe.Exists || !probe.Busy {
		t.Fatalf("peer fixture did not launch=%+v,%v", probe, err)
	}
	for _, pane := range []string{peer, owned} {
		output, err := exec.CommandContext(ctx, "herdr", "--session", session, "pane", "report-agent", pane, "--source", "gctest", "--agent", "codex", "--state", "idle").CombinedOutput()
		if err != nil {
			t.Fatalf("report shared kind: %v: %s", err, output)
		}
	}
	for _, pane := range []string{peer, owned} {
		a, present, err := p.c.getAgent(ctx, pane)
		if err != nil || !present || a.Name != "codex" {
			t.Fatalf("shared-kind fixture=%+v,%v,%v", a, present, err)
		}
	}
	names, err := p.ListRunning("")
	if err != nil || !reflect.DeepEqual(names, []string{"worker"}) {
		t.Fatalf("owned listing=%v,%v", names, err)
	}
	if err := p.Stop("codex"); err != nil {
		t.Fatal(err)
	}
	if err := p.Interrupt("codex"); err != nil {
		t.Fatal(err)
	}
	if probe, err := p.probePane(ctx, peer); err != nil || !probe.Exists || !probe.Busy {
		t.Fatalf("foreign peer changed=%+v,%v", probe, err)
	}
	if live := p.ObserveLiveness("worker", nil); !live.Running || !live.Alive {
		t.Fatalf("owned live=%+v", live)
	}
	if err := p.Stop("worker"); err != nil {
		t.Fatal(err)
	}
	if probe, err := p.probePane(ctx, peer); err != nil || !probe.Exists || !probe.Busy {
		t.Fatalf("owned Stop affected peer=%+v,%v", probe, err)
	}
	if probe, err := p.probePane(ctx, owned); err != nil || probe.Exists {
		t.Fatalf("owned pane was not closed=%+v,%v", probe, err)
	}
}
