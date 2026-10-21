//go:build linux

package contract

import (
	"debug/buildinfo"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
)

// openBeadsExecutable opens the command runner's already-selected absolute
// executable. The returned one-use finalizer yields its module identity only
// after checking that the opened file and path remained unchanged. Call it after
// the command returns, including when execution or context retrieval fails.
func openBeadsExecutable(path string) (*os.File, func() (BeadsArtifact, error), error) {
	if !filepath.IsAbs(path) {
		return nil, nil, fmt.Errorf("selected bd executable path is not absolute")
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("open selected bd executable: %w", err)
	}
	fail := func(err error) (*os.File, func() (BeadsArtifact, error), error) {
		return nil, nil, errors.Join(err, f.Close())
	}
	before, err := f.Stat()
	if err != nil {
		return fail(err)
	}
	if !before.Mode().IsRegular() {
		return fail(errors.New("selected bd executable is not a regular file"))
	}
	info, err := buildinfo.Read(f)
	if err != nil {
		return fail(fmt.Errorf("read selected bd Go build info: %w", err))
	}
	if info.Main.Path == "" || info.Path != info.Main.Path+"/cmd/bd" {
		return fail(errors.New("selected executable is not its module's cmd/bd command"))
	}
	artifact := BeadsArtifact{ModulePath: info.Main.Path, Version: info.Main.Version, Sum: info.Main.Sum, Replaced: info.Main.Replace != nil}
	consumed := false
	return f, func() (BeadsArtifact, error) {
		if consumed {
			return BeadsArtifact{}, errors.New("bd executable inspection already consumed")
		}
		consumed = true
		after, statErr := f.Stat()
		current, pathErr := os.Stat(path)
		closeErr := f.Close()
		if err := errors.Join(statErr, pathErr, closeErr); err != nil {
			return BeadsArtifact{}, fmt.Errorf("finish bd executable inspection: %w", err)
		}
		unchanged := func(a, b os.FileInfo) bool {
			return os.SameFile(a, b) && a.Size() == b.Size() && a.ModTime().Equal(b.ModTime()) && a.Mode() == b.Mode()
		}
		if !unchanged(before, after) || !unchanged(before, current) {
			return BeadsArtifact{}, errors.New("selected bd executable changed during command")
		}
		return artifact, nil
	}, nil
}

// Observe binds Linux execution to the inspected file, including across an
// atomic replacement and rollback of its original path. Args[0] is unchanged.
func (o *BeadsExecutableObservation) Observe(cmd *exec.Cmd) (func() error, error) {
	generation := o.generation
	f, finish, err := openBeadsExecutable(cmd.Path)
	if err != nil {
		return nil, err
	}
	cmd.Path = "/proc/self/fd/" + strconv.Itoa(3+len(cmd.ExtraFiles))
	cmd.ExtraFiles = append(cmd.ExtraFiles, f)
	return func() error {
		artifact, err := finish()
		if err != nil {
			return err
		}
		if o.generation != generation {
			return errors.New("bd executable observation belongs to an older command attempt")
		}
		o.artifact = artifact
		return nil
	}, nil
}
