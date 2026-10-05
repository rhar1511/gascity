package main

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/gastownhall/gascity/internal/storybench"
	"github.com/rogpeppe/go-internal/testscript"
)

func TestMain(m *testing.M) {
	testscript.Main(m, map[string]func(){"rsistorybench": main})
}

func TestCLI(t *testing.T) {
	testscript.Run(t, testscript.Params{
		Dir: "testdata",
		Setup: func(env *testscript.Env) error {
			suite := []byte(`{"version":1,"id":"cli-suite","cases":[{"id":"choice","story_id":"CLI-1","objective":"retain choice","prompt":"choose","critical":true,"required_actions":["choice"],"forbidden_actions":[],"max_actions":3}]}`)
			baseline := storybench.Run{SuiteSHA256: storybench.SuiteHash(suite), BundleID: "accepted", CapturedBy: "harness", Cases: []storybench.Trace{{CaseID: "choice", Actions: []string{}}}}
			candidate := baseline
			candidate.BundleID = "candidate"
			candidate.Cases = []storybench.Trace{{CaseID: "choice", Actions: []string{"choice"}}}
			veto := candidate
			veto.Cases = baseline.Cases
			files := map[string][]byte{"suite.json": suite, "malformed.json": []byte("{"), "result.json": []byte("old result"), "evidence.json": []byte("old evidence")}
			for name, value := range map[string]storybench.Run{"baseline.json": baseline, "candidate.json": candidate, "veto.json": veto} {
				raw, err := json.Marshal(value)
				if err != nil {
					return err
				}
				files[name] = raw
			}
			for name, raw := range files {
				path := filepath.Join(env.WorkDir, name)
				if err := os.WriteFile(path, raw, 0o600); err != nil {
					return err
				}
				if name == "result.json" || name == "evidence.json" {
					if err := os.Chmod(path, 0o644); err != nil {
						return err
					}
				}
			}
			return nil
		},
		Cmds: map[string]func(*testscript.TestScript, bool, []string){
			"exitcode": func(ts *testscript.TestScript, neg bool, args []string) {
				if neg || len(args) == 0 {
					ts.Fatalf("usage: exitcode <code> [benchmark flags]")
				}
				want, err := strconv.Atoi(args[0])
				ts.Check(err)
				got := 0
				if err := ts.Exec("rsistorybench", args[1:]...); err != nil {
					var exit *exec.ExitError
					if !errors.As(err, &exit) {
						ts.Fatalf("running benchmark: %v", err)
					}
					got = exit.ExitCode()
				}
				if got != want {
					ts.Fatalf("exit code = %d, want %d", got, want)
				}
			},
			"private": func(ts *testscript.TestScript, neg bool, args []string) {
				if neg || len(args) == 0 {
					ts.Fatalf("usage: private <artifact>...")
				}
				for _, path := range args {
					info, err := os.Stat(ts.MkAbs(path))
					ts.Check(err)
					if info.Mode().Perm() != 0o600 {
						ts.Fatalf("%s permissions = %o, want 600", path, info.Mode().Perm())
					}
				}
			},
		},
	})
}
