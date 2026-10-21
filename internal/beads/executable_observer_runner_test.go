//go:build integration && linux

package beads

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads/contract"
)

// This is the one composition proof for the real BDStore runner constructor.
// The module fixture is deliberately unreleased; it proves execution plumbing,
// never actual released-source compatibility or database eligibility.
func TestObservedBDRunnerUsesSelectedEnvironment(t *testing.T) {
	binary := buildObserverFixture(t, "fixture")
	for _, hosted := range []bool{false, true} {
		name := "ordinary"
		if hosted {
			name = "without ambient Beads namespace"
		}
		t.Run(name, func(t *testing.T) {
			t.Setenv("BD_BIN", filepath.Join(t.TempDir(), "wrong-bd"))
			t.Setenv("BEADS_PRIVATE_INHERITED", "ambient")
			scope := t.TempDir()
			observation := &contract.BeadsExecutableObservation{}
			seen := false
			observer := &ExecutableObserver{BeginAttempt: observation.BeginAttempt, Observe: func(cmd *exec.Cmd) (func() error, error) {
				seen = true
				if cmd.Path != binary || cmd.Dir != scope || len(cmd.Args) != 3 || cmd.Args[0] != binary || cmd.Args[1] != "context" || cmd.Args[2] != "--json" {
					t.Errorf("selected command changed: path=%q dir=%q args=%v", cmd.Path, cmd.Dir, cmd.Args)
				}
				env := map[string]string{}
				for _, entry := range cmd.Env {
					k, v, ok := strings.Cut(entry, "=")
					if ok {
						env[k] = v
					}
				}
				if env["BD_BIN"] != binary || env["BEADS_PRIVATE_EXPLICIT"] != "bound" {
					t.Errorf("explicit command environment lost")
				}
				if hosted {
					if _, ok := env["BEADS_PRIVATE_INHERITED"]; ok {
						t.Error("hosted command inherited ambient Beads namespace")
					}
				} else if env["BEADS_PRIVATE_INHERITED"] != "ambient" {
					t.Error("ordinary command lost allowed inherited namespace")
				}
				return observation.Observe(cmd)
			}}
			runner := ExecCommandRunnerWithExecutableObserver(map[string]string{"BD_BIN": binary, "BEADS_PRIVATE_EXPLICIT": "bound"}, hosted, observer)
			out, err := runner(scope, "bd", "context", "--json")
			if err != nil || string(out) != "fixture" || !seen || observation.Artifact().ModulePath != "example.com/observerfixture" {
				t.Fatalf("out=%q err=%v observed=%v artifact=%+v", out, err, seen, observation.Artifact())
			}
		})
	}
}
