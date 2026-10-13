//go:build integration

package integration

import (
	"context"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads/proxyendpoint"
	gopsnet "github.com/shirou/gopsutil/v4/net"
	"github.com/shirou/gopsutil/v4/process"
)

func graphFixtureProcessTable() proxyendpoint.ProcessTable {
	table := proxyendpoint.DefaultProcessTable()
	table.Birth = graphFixtureProcessBirth
	return table
}

func graphFixtureProcessIDs() ([]int32, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return process.PidsWithContext(ctx)
}

func graphProcessExecutableAndCWD(pid int) (string, string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	p, err := process.NewProcessWithContext(ctx, int32(pid))
	if err != nil {
		return "", "", err
	}
	executable, err := p.ExeWithContext(ctx)
	if err != nil {
		return "", "", err
	}
	cwd, err := p.CwdWithContext(ctx)
	return executable, cwd, err
}

func graphDarwinProcessOwnsListeningPort(pid, port int) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	connections, err := gopsnet.ConnectionsPidWithContext(ctx, "tcp4", int32(pid))
	if err != nil {
		return false
	}
	for _, connection := range connections {
		if connection.Pid == int32(pid) && connection.Status == "LISTEN" &&
			connection.Laddr.IP == proxyendpoint.Host && connection.Laddr.Port == uint32(port) {
			return true
		}
	}
	return false
}

func TestGraphFixtureNativeBirthProof(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("graph fixture process proof supports Linux and Darwin")
	}
	table := graphFixtureProcessTable()
	first, err := table.Birth(os.Getpid())
	if err != nil || !strings.HasPrefix(first, runtime.GOOS+"-v1:") {
		t.Fatalf("native fixture birth proof = %q, %v", first, err)
	}
	second, err := table.Birth(os.Getpid())
	if err != nil || first != second {
		t.Fatalf("native fixture birth changed: %q -> %q, %v", first, second, err)
	}
}
