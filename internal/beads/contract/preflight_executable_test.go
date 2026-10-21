//go:build integration && linux

package contract

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"
)

func inspectBeadsExecutable(path string) (func() (BeadsArtifact, error), error) {
	_, finish, err := openBeadsExecutable(path)
	return finish, err
}

func buildInspectionFixture(t *testing.T, command string) string {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, "cmd", command)
	if err := os.MkdirAll(src, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module "+beadsModulePath+"\n\ngo 1.26.9\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "main.go"), []byte("package main\nfunc main(){}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(dir, command)
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, filepath.Join(runtime.GOROOT(), "bin", "go"), "build", "-o", binary, "./cmd/"+command)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fixture build: %v\n%s", err, out)
	}
	return binary
}

func TestBeadsExecutableInspection(t *testing.T) {
	binary := buildInspectionFixture(t, "bd")
	t.Run("actual local source is not a released module", func(t *testing.T) {
		finish, err := inspectBeadsExecutable(binary)
		if err != nil {
			t.Fatal(err)
		}
		a, err := finish()
		if err != nil {
			t.Fatal(err)
		}
		if a.ModulePath != beadsModulePath || a.Version == "" || a.unconfirmedSourceReason() == "" {
			t.Fatalf("local source falsely release-qualified: %+v", a)
		}
	})
	t.Run("FIFO refuses without a writer", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "fifo")
		if err := syscall.Mkfifo(p, 0o700); err != nil {
			t.Fatal(err)
		}
		if f, err := inspectBeadsExecutable(p); err == nil || f != nil {
			t.Fatal("FIFO accepted")
		}
	})
	t.Run("one use", func(t *testing.T) {
		finish, err := inspectBeadsExecutable(binary)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := finish(); err != nil {
			t.Fatal(err)
		}
		if a, err := finish(); err == nil || a != (BeadsArtifact{}) {
			t.Fatal("consumed inspection reused")
		}
	})
	t.Run("relative path", func(t *testing.T) {
		if f, err := inspectBeadsExecutable("bd"); err == nil || f != nil {
			t.Fatal("relative selection accepted")
		}
	})
	t.Run("not a Go executable", func(t *testing.T) {
		p := filepath.Join(t.TempDir(), "plain")
		if err := os.WriteFile(p, []byte("not a Go binary"), 0o700); err != nil {
			t.Fatal(err)
		}
		if f, err := inspectBeadsExecutable(p); err == nil || f != nil {
			t.Fatal("unrecognized binary accepted")
		}
	})
	t.Run("older attempt cannot publish", func(t *testing.T) {
		o := &BeadsExecutableObservation{}
		o.BeginAttempt()
		finish, err := o.Observe(exec.Command(binary))
		if err != nil {
			t.Fatal(err)
		}
		o.BeginAttempt()
		if err := finish(); err == nil || o.Artifact() != (BeadsArtifact{}) {
			t.Fatal("older attempt published")
		}
	})
	t.Run("next attempt clears proof", func(t *testing.T) {
		o := &BeadsExecutableObservation{}
		o.BeginAttempt()
		finish, err := o.Observe(exec.Command(binary))
		if err != nil {
			t.Fatal(err)
		}
		if err := finish(); err != nil {
			t.Fatal(err)
		}
		if o.Artifact() == (BeadsArtifact{}) {
			t.Fatal("actual identity not read")
		}
		o.BeginAttempt()
		if o.Artifact() != (BeadsArtifact{}) {
			t.Fatal("prior artifact retained")
		}
	})
	t.Run("wrong command in same module", func(t *testing.T) {
		wrong := buildInspectionFixture(t, "other")
		if f, err := inspectBeadsExecutable(wrong); err == nil || f != nil {
			t.Fatal("other command accepted as bd")
		}
	})
	t.Run("changed opened file refuses", func(t *testing.T) {
		finish, err := inspectBeadsExecutable(binary)
		if err != nil {
			t.Fatal(err)
		}
		f, err := os.OpenFile(binary, os.O_WRONLY|os.O_APPEND, 0)
		if err != nil {
			t.Fatal(err)
		}
		_, writeErr := f.Write([]byte{0})
		closeErr := f.Close()
		if writeErr != nil || closeErr != nil {
			t.Fatalf("private fixture append: %v %v", writeErr, closeErr)
		}
		if a, err := finish(); err == nil || a != (BeadsArtifact{}) {
			t.Fatal("changed artifact accepted")
		}
	})
}
