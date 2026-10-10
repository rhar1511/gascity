package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/rogpeppe/go-internal/testscript"
)

func TestMain(m *testing.M) {
	testscript.Main(m, map[string]func(){"toolcallbench": main})
}

func TestCLI(t *testing.T) {
	testscript.Run(t, testscript.Params{
		Dir: "testdata",
		Setup: func(env *testscript.Env) error {
			for _, name := range []string{"suite.json", "baseline.example.json", "candidate.example.json"} {
				raw, err := os.ReadFile(filepath.Join("testdata", name))
				if err != nil {
					return err
				}
				if err := os.WriteFile(filepath.Join(env.WorkDir, name), raw, 0o600); err != nil {
					return err
				}
			}
			return nil
		},
	})
}
