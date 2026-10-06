package herdr

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

// Herdr 0.9 reports a detected kind in agent, not the Gas City session name.
// The peer is deliberately listed first so a name fallback cannot accidentally
// pass by selecting the owned pane.
func ownershipProvider(t *testing.T) (*Provider, string) {
	t.Helper()
	root := t.TempDir()
	calls := filepath.Join(root, "calls")
	script := filepath.Join(root, "herdr")
	body := `#!/bin/sh
shift 2
printf '%s\n' "$*" >> '` + calls + `'
case "$1_$2" in
agent_list) printf '%s' '{"result":{"agents":[{"agent":"codex","pane_id":"w2:peer","agent_status":"working"},{"agent":"codex","pane_id":"w2:owned","agent_status":"idle"}]}}' ;;
agent_get)
 case "$3" in
 w2:owned) printf '%s' '{"result":{"agent":{"agent":"codex","pane_id":"w2:owned","agent_status":"idle"}}}' ;;
 w2:peer) printf '%s' '{"result":{"agent":{"agent":"codex","pane_id":"w2:peer","agent_status":"working"}}}' ;;
 *) printf '%s' '{"error":{"code":"agent_not_found","message":"agent target not found"}}' ;;
 esac ;;
pane_process-info) printf '%s' '{"result":{"process_info":{"shell_pid":10,"foreground_processes":[{"pid":11,"name":"codex"}]}}}' ;;
pane_close) printf '%s' '{"result":{}}' ;;
*) printf '%s' '{"result":{}}' ;;
esac
`
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	p := New("ownership-fixture", filepath.Join(root, "meta"), "", 0, 0)
	p.c.bin = script
	return p, calls
}

func bindOwnedPane(t *testing.T, p *Provider, name string) {
	t.Helper()
	if err := p.bindPlacement(name, agentInfo{PaneID: "w2:owned", TerminalID: "owned-terminal"}, bindModeAgent); err != nil {
		t.Fatal(err)
	}
}

func TestOwnedHerdrListingExcludesDetectedPeerKinds(t *testing.T) {
	p, calls := ownershipProvider(t)
	bindOwnedPane(t, p, "city--worker")
	names, err := p.ListRunning("")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"city--worker"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("ListRunning = %v; want only owned sessions %v", names, want)
	}
	b, _ := os.ReadFile(calls)
	if strings.Contains(string(b), "pane close") {
		t.Fatalf("listing closed a pane: %s", b)
	}
}

func TestOwnedHerdrStopUsesBindingDespiteDetectedKindCollision(t *testing.T) {
	p, calls := ownershipProvider(t)
	bindOwnedPane(t, p, "codex")
	if err := p.Stop("codex"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(calls)
	if !strings.Contains(string(b), "pane close w2:owned\n") || strings.Contains(string(b), "pane close w2:peer\n") {
		t.Fatalf("Stop must close only owned pane; calls:\n%s", b)
	}
}

func TestOwnedHerdrStopCannotCloseUnboundPeer(t *testing.T) {
	p, calls := ownershipProvider(t)
	_ = p.Stop("codex")
	b, _ := os.ReadFile(calls)
	if strings.Contains(string(b), "pane close") {
		t.Fatalf("unowned detected peer was closed:\n%s", b)
	}
}

func TestOwnedHerdrSanitizedAliasCannotControlBoundOwner(t *testing.T) {
	p, calls := ownershipProvider(t)
	bindOwnedPane(t, p, "city/worker")
	if err := p.Interrupt("city_worker"); err == nil {
		t.Error("alias of another session's metadata was allowed to interrupt it")
	}
	if p.IsRunning("city_worker") {
		t.Error("alias was accepted as the bound session")
	}
	if err := p.Stop("city_worker"); err == nil {
		t.Error("alias of another session's metadata was allowed to stop it")
	}
	b, _ := os.ReadFile(calls)
	if strings.Contains(string(b), "pane close") || strings.Contains(string(b), "pane send-keys") {
		t.Fatalf("alias controlled owner's pane:\n%s", b)
	}
	if pane, err := p.GetMeta("city/worker", metaBoundPane); err != nil || pane != "w2:owned" {
		t.Fatalf("alias lost owner's binding=%q,%v", pane, err)
	}
}

func TestOwnedHerdrLivenessUsesBoundPaneStatus(t *testing.T) {
	p, calls := ownershipProvider(t)
	bindOwnedPane(t, p, "city--worker")
	if got := p.ObserveLiveness("city--worker", nil); got != (runtime.Liveness{Running: true, Alive: true}) {
		t.Fatalf("owned idle worker = %+v", got)
	}
	b, _ := os.ReadFile(calls)
	if !strings.Contains(string(b), "agent get w2:owned\n") {
		t.Fatalf("liveness must use owned pane identity, calls:\n%s", b)
	}
	if strings.Contains(string(b), "agent list") || strings.Contains(string(b), "pane close") {
		t.Fatalf("liveness used ambiguous registry or closed a pane:\n%s", b)
	}
}

func TestOwnedHerdrListingSurfacesRegistryFailure(t *testing.T) {
	p, _ := ownershipProvider(t)
	bindOwnedPane(t, p, "worker")
	body, err := os.ReadFile(p.c.bin)
	if err != nil {
		t.Fatal(err)
	}
	body = []byte(strings.Replace(string(body), "agent_get)", "agent_get) printf '%s' '{\"error\":{\"code\":\"transport_failed\",\"message\":\"unavailable\"}}'; exit 0 ;;\nunused_get)", 1))
	if err := os.WriteFile(p.c.bin, body, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := p.ListRunning(""); err == nil {
		t.Fatal("unavailable registry was reported as an empty running-session list")
	}
	if pane, err := p.GetMeta("worker", metaBoundPane); err != nil || pane != "w2:owned" {
		t.Fatalf("registry failure lost binding=%q,%v", pane, err)
	}
}

func TestOwnedHerdrAttachUsesBoundPane(t *testing.T) {
	p, calls := ownershipProvider(t)
	bindOwnedPane(t, p, "city--worker")
	if err := p.Attach("city--worker"); err != nil {
		t.Fatal(err)
	}
	if err := p.Attach("codex"); err == nil {
		t.Error("unbound detected kind was accepted for attachment")
	}
	b, _ := os.ReadFile(calls)
	if !strings.Contains(string(b), "agent attach w2:owned\n") || strings.Contains(string(b), "agent attach codex\n") {
		t.Fatalf("attachment used an unowned target:\n%s", b)
	}
}

func TestOwnedHerdrUnboundKindIsNotAnAddressableSession(t *testing.T) {
	p, calls := ownershipProvider(t)
	if p.IsRunning("codex") {
		t.Error("detected peer kind was accepted as a GC session")
	}
	if err := p.Interrupt("codex"); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(calls)
	if strings.Contains(string(b), "pane send-keys") {
		t.Fatalf("unbound peer was interrupted:\n%s", b)
	}
}

func TestOwnedHerdrConflictingBindingCannotBeStopped(t *testing.T) {
	p, calls := ownershipProvider(t)
	bindOwnedPane(t, p, "owner-a")
	bindOwnedPane(t, p, "owner-b")
	if err := p.Stop("owner-a"); err == nil {
		t.Fatal("conflicting owners accepted")
	}
	names, err := p.ListRunning("")
	if err != nil || len(names) != 0 {
		t.Fatalf("conflicting listing = %v, %v", names, err)
	}
	b, _ := os.ReadFile(calls)
	if strings.Contains(string(b), "pane close") {
		t.Fatalf("conflicting pane closed:\n%s", b)
	}
}

func TestOwnedHerdrNameTakenCannotAdoptForeignPane(t *testing.T) {
	p, calls := ownershipProvider(t)
	bindOwnedPane(t, p, "codex")
	body, err := os.ReadFile(p.c.bin)
	if err != nil {
		t.Fatal(err)
	}
	body = []byte(strings.Replace(string(body), "agent_list)", "agent_start) printf '%s' '{\"error\":{\"code\":\"agent_name_taken\",\"message\":\"name taken\"}}' ;;\nagent_list)", 1))
	if err := os.WriteFile(p.c.bin, body, 0o700); err != nil {
		t.Fatal(err)
	}
	info, adopted, err := p.startAgentAdopting(t.Context(), "codex", "codex", "w2:owned", nil)
	if err == nil || adopted || info.PaneID == "w2:peer" {
		t.Fatalf("foreign collision adopted: %+v,%v,%v", info, adopted, err)
	}
	b, _ := os.ReadFile(calls)
	if strings.Contains(string(b), "pane close w2:peer") {
		t.Fatalf("foreign holder reaped:\n%s", b)
	}
}

func TestOwnedHerdrStopRetainsBindingWhenCloseFails(t *testing.T) {
	p, _ := ownershipProvider(t)
	bindOwnedPane(t, p, "worker")
	body, err := os.ReadFile(p.c.bin)
	if err != nil {
		t.Fatal(err)
	}
	body = []byte(strings.Replace(string(body), "pane_close) printf '%s' '{\"result\":{}}'", "pane_close) printf '%s' '{\"error\":{\"code\":\"transport_failed\",\"message\":\"unavailable\"}}'", 1))
	if err := os.WriteFile(p.c.bin, body, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := p.Stop("worker"); err == nil {
		t.Fatal("failed close was reported as success")
	}
	if pane, err := p.GetMeta("worker", metaBoundPane); err != nil || pane != "w2:owned" {
		t.Fatalf("failed stop lost ownership: %q,%v", pane, err)
	}
}

func TestOwnedHerdrStopClearsConfirmedGoneBinding(t *testing.T) {
	p, _ := ownershipProvider(t)
	bindOwnedPane(t, p, "worker")
	body, err := os.ReadFile(p.c.bin)
	if err != nil {
		t.Fatal(err)
	}
	body = []byte(strings.Replace(string(body), "pane_close) printf '%s' '{\"result\":{}}'", "pane_close) printf '%s' '{\"error\":{\"code\":\"pane_not_found\",\"message\":\"pane gone\"}}'", 1))
	if err := os.WriteFile(p.c.bin, body, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := p.Stop("worker"); err != nil {
		t.Fatalf("confirmed-gone stop not idempotent: %v", err)
	}
	if pane, err := p.GetMeta("worker", metaBoundPane); err != nil || pane != "" {
		t.Fatalf("gone binding retained=%q,%v", pane, err)
	}
}
