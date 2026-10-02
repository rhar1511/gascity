//go:build linux

// gc-protected-authority-launcher is a root-run, host-only entry point. It
// launches exactly one non-root `gc supervisor run` under a pidfd-backed key
// broker.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/gastownhall/gascity/internal/beadspermitbroker"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "--internal-protected-authority-exec-child" {
		os.Exit(beadspermitbroker.ExecChild())
	}
	os.Exit(run(os.Args[1:], os.Stderr))
}

func run(args []string, stderr io.Writer) int {
	flags := flag.NewFlagSet("gc-protected-authority-launcher", flag.ContinueOnError)
	flags.SetOutput(stderr)
	authorityDirectory := flags.String("authority-dir", "", "root-owned protected authority directory")
	privateKeyDirectory := flags.String("key-dir", "", "root-owned directory containing <key-handle>.pem files")
	controllerEnvironmentFile := flags.String("environment-file", "", "optional root-owned mode-0600 JSON environment for the controller")
	gcPath := flags.String("gc", "", "root-owned gc executable to run")
	controllerPath := flags.String("path", "/usr/local/bin:/usr/bin:/bin", "explicit controller PATH")
	uid := flags.Uint("uid", 0, "non-root controller user ID")
	gid := flags.Uint("gid", 0, "non-root controller primary group ID")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 || *authorityDirectory == "" || *privateKeyDirectory == "" || *gcPath == "" ||
		*uid == 0 || *gid == 0 || uint64(*uid) > uint64(^uint32(0)) || uint64(*gid) > uint64(^uint32(0)) {
		fmt.Fprintln(stderr, "usage: gc-protected-authority-launcher --authority-dir DIR --key-dir DIR --gc PATH --uid UID --gid GID [--environment-file FILE]") //nolint:errcheck
		return 2
	}
	return beadspermitbroker.LaunchSupervisor(beadspermitbroker.LaunchConfig{
		AuthorityDirectory: *authorityDirectory, PrivateKeyDirectory: *privateKeyDirectory,
		ControllerEnvironmentFile: *controllerEnvironmentFile,
		GCExecutable:              *gcPath, ControllerPath: *controllerPath, ControllerUID: uint32(*uid), ControllerGID: uint32(*gid),
	})
}
