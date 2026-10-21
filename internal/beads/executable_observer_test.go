//go:build integration && linux

package beads

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads/contract"
)

type fixtureExecutableObserver struct {
	began, observed, finished int
	path                      string
	beforeErr, afterErr       error
	stale                     bool
}

func (o *fixtureExecutableObserver) BeginAttempt() { o.began++; o.stale = false; o.path = "" }
func (o *fixtureExecutableObserver) Observe(cmd *exec.Cmd) (func() error, error) {
	o.observed++
	o.path = cmd.Path
	if o.beforeErr != nil {
		return nil, o.beforeErr
	}
	return func() error { o.finished++; return o.afterErr }, nil
}

func (o *fixtureExecutableObserver) callbacks() *ExecutableObserver {
	return &ExecutableObserver{BeginAttempt: o.BeginAttempt, Observe: o.Observe}
}

func buildObserverFixture(t *testing.T, output string) string {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, "cmd", "bd")
	if err := os.MkdirAll(src, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/observerfixture\n\ngo 1.26.9\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code := fmt.Sprintf(`package main
import("fmt";"os")
func main(){if len(os.Args)>1&&os.Args[1]=="marker"{if err:=os.WriteFile(os.Args[2],[]byte("started"),0600);err!=nil{panic(err)}};if len(os.Args)>1&&os.Args[1]=="fail"{fmt.Print("fixturefail");os.Exit(7)};fmt.Print(%q)}`, output)
	if err := os.WriteFile(filepath.Join(src, "main.go"), []byte(code), 0o600); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(dir, "bd")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, filepath.Join(runtime.GOROOT(), "bin", "go"), "build", "-o", binary, "./cmd/bd")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fixture build: %v\n%s", err, out)
	}
	return binary
}

func TestRunObservedCommand(t *testing.T) {
	binary := buildObserverFixture(t, "fixture")
	t.Run("default unchanged", func(t *testing.T) {
		out, err := runObservedCommand(exec.Command(binary), nil)
		if err != nil || string(out) != "fixture" {
			t.Fatalf("output=%q err=%v", out, err)
		}
	})
	t.Run("actual selected absolute path", func(t *testing.T) {
		o := &fixtureExecutableObserver{}
		cmd := exec.Command(binary)
		out, err := runObservedCommand(cmd, o.callbacks())
		if err != nil || string(out) != "fixture" || o.path != cmd.Path || o.began != 1 || o.observed != 1 || o.finished != 1 {
			t.Fatalf("out=%q err=%v observer=%+v", out, err, o)
		}
	})
	t.Run("PATH selection", func(t *testing.T) {
		t.Setenv("PATH", filepath.Dir(binary))
		o := &fixtureExecutableObserver{}
		cmd := exec.Command("bd")
		out, err := runObservedCommand(cmd, o.callbacks())
		if err != nil || string(out) != "fixture" || o.path != binary {
			t.Fatalf("out=%q err=%v observed=%q selected=%q", out, err, o.path, cmd.Path)
		}
	})
	t.Run("before failure prevents child", func(t *testing.T) {
		marker := filepath.Join(t.TempDir(), "started")
		sentinel := errors.New("before")
		o := &fixtureExecutableObserver{beforeErr: sentinel}
		_, err := runObservedCommand(exec.Command(binary, "marker", marker), o.callbacks())
		if !errors.Is(err, sentinel) || o.finished != 0 {
			t.Fatal("observation error lost")
		}
		if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("unobserved child started")
		}
	})
	t.Run("after failure retains output", func(t *testing.T) {
		sentinel := errors.New("after")
		o := &fixtureExecutableObserver{afterErr: sentinel}
		out, err := runObservedCommand(exec.Command(binary), o.callbacks())
		if !errors.Is(err, sentinel) || string(out) != "fixture" || o.finished != 1 {
			t.Fatalf("out=%q err=%v observer=%+v", out, err, o)
		}
	})
	t.Run("command and finalizer failures retained", func(t *testing.T) {
		sentinel := errors.New("after")
		o := &fixtureExecutableObserver{afterErr: sentinel}
		out, err := runObservedCommand(exec.Command(binary, "fail"), o.callbacks())
		var exit *exec.ExitError
		if !errors.Is(err, sentinel) || !errors.As(err, &exit) || exit.ExitCode() != 7 || string(out) != "fixturefail" || o.finished != 1 {
			t.Fatalf("out=%q err=%v observer=%+v", out, err, o)
		}
	})
	t.Run("command failure unchanged", func(t *testing.T) {
		o := &fixtureExecutableObserver{}
		_, err := runObservedCommand(exec.Command(binary, "fail"), o.callbacks())
		exit, ok := err.(*exec.ExitError)
		if !ok || exit.ExitCode() != 7 || o.finished != 1 {
			t.Fatalf("err=%v observer=%+v", err, o)
		}
	})
	t.Run("lookup failure resets proof", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		o := &fixtureExecutableObserver{stale: true}
		_, err := runObservedCommand(exec.Command("__gc_missing_observer_fixture__"), o.callbacks())
		if err == nil || o.began != 1 || o.stale || o.observed != 0 || o.finished != 0 {
			t.Fatalf("err=%v observer=%+v", err, o)
		}
	})
	t.Run("actual Go module observation", func(t *testing.T) {
		o := &contract.BeadsExecutableObservation{}
		out, err := runObservedCommand(exec.Command(binary), &ExecutableObserver{BeginAttempt: o.BeginAttempt, Observe: o.Observe})
		if err != nil || string(out) != "fixture" || o.Artifact().ModulePath != "example.com/observerfixture" {
			t.Fatalf("out=%q err=%v actual=%+v", out, err, o.Artifact())
		}
	})
	t.Run("path replacement and restoration executes inspected file", func(t *testing.T) {
		alternate := buildObserverFixture(t, "other")
		saved := binary + ".saved"
		o := &contract.BeadsExecutableObservation{}
		callbacks := &ExecutableObserver{BeginAttempt: o.BeginAttempt, Observe: func(cmd *exec.Cmd) (func() error, error) {
			finish, err := o.Observe(cmd)
			if err != nil {
				return nil, err
			}
			if err := os.Rename(binary, saved); err != nil {
				return nil, errors.Join(err, finish())
			}
			if err := os.Rename(alternate, binary); err != nil {
				return nil, errors.Join(err, os.Rename(saved, binary), finish())
			}
			return func() error { return errors.Join(os.Rename(binary, alternate), os.Rename(saved, binary), finish()) }, nil
		}}
		cmd := exec.Command(binary)
		out, err := runObservedCommand(cmd, callbacks)
		if err != nil || string(out) != "fixture" || cmd.Args[0] != binary || o.Artifact().ModulePath != "example.com/observerfixture" {
			t.Fatalf("out=%q err=%v args=%v artifact=%+v", out, err, cmd.Args, o.Artifact())
		}
	})
	t.Run("existing inherited file retained", func(t *testing.T) {
		f, err := os.Open(binary)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		o := &contract.BeadsExecutableObservation{}
		cmd := exec.Command(binary)
		cmd.ExtraFiles = []*os.File{f}
		out, err := runObservedCommand(cmd, &ExecutableObserver{BeginAttempt: o.BeginAttempt, Observe: o.Observe})
		if err != nil || string(out) != "fixture" || cmd.Path != "/proc/self/fd/4" || len(cmd.ExtraFiles) != 2 || cmd.ExtraFiles[0] != f {
			t.Fatalf("out=%q err=%v path=%q inherited=%v", out, err, cmd.Path, cmd.ExtraFiles)
		}
	})
}
