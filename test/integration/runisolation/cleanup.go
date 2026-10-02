package runisolation

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// PreserveMarkerName marks an integration temp root that cleanup must retain.
const PreserveMarkerName = ".gc-integration-preserve"

// ProcessCheck reports process IDs that still use the owned run root. It must
// return an error when it cannot establish that no such process remains.
type ProcessCheck func(runRoot, gcHome string) ([]int, error)

// CleanupOwnedRoot removes a verified run tool directory and its empty parent.
// Any uncertain state returns an error and leaves the run root for review.
func CleanupOwnedRoot(runRoot, toolDir, gcHome string, stopSupervisor func() error, checkProcesses ProcessCheck) error {
	return cleanupOwnedRoot(runRoot, toolDir, gcHome, stopSupervisor, checkProcesses, os.Remove)
}

func cleanupOwnedRoot(runRoot, toolDir, gcHome string, stopSupervisor func() error, checkProcesses ProcessCheck, removeParent func(string) error) error {
	if runRoot == "" {
		return fmt.Errorf("preserving private integration run root: root path is empty")
	}
	if stopSupervisor == nil {
		return fmt.Errorf("preserving private integration run root %s because supervisor stop cannot be checked: callback is nil", runRoot)
	}
	if checkProcesses == nil {
		return fmt.Errorf("preserving private integration run root %s because process absence cannot be checked: callback is nil", runRoot)
	}
	if removeParent == nil {
		return fmt.Errorf("preserving private integration run root %s because empty-only removal cannot be checked: callback is nil", runRoot)
	}
	parent, err := filepath.Abs(runRoot)
	if err != nil {
		return preservingError(runRoot, "resolve path", err)
	}
	info, err := os.Lstat(parent)
	if err != nil {
		return preservingError(parent, "inspect directory", err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("preserving private integration run root because parent is not a private real directory: %s", parent)
	}
	parentReal, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return preservingError(parent, "resolve real path", err)
	}
	if marker, err := preservationPath(parent); err != nil {
		return preservingError(parent, "inspect markers and symlinks", err)
	} else if marker != "" {
		return fmt.Errorf("preserving private integration run root %s because marker or symlink exists at %s", parent, marker)
	}

	if err := stopSupervisor(); err != nil {
		return preservingError(parent, "stop supervisor", err)
	}
	pids, err := checkProcesses(parent, gcHome)
	if err != nil {
		return preservingError(parent, "check fixture processes", err)
	}
	if len(pids) != 0 {
		sort.Ints(pids)
		return fmt.Errorf("preserving private integration run root %s because fixture processes remain: %v", parent, pids)
	}
	if marker, err := preservationPath(parent); err != nil {
		return preservingError(parent, "recheck markers and symlinks", err)
	} else if marker != "" {
		return fmt.Errorf("preserving private integration run root %s because marker or symlink exists at %s", parent, marker)
	}

	if toolDir != "" {
		toolPath, err := filepath.Abs(toolDir)
		if err != nil {
			return preservingError(parent, fmt.Sprintf("resolve tool dir %s", toolDir), err)
		}
		toolInfo, err := os.Lstat(toolPath)
		if err != nil {
			return preservingError(parent, fmt.Sprintf("inspect tool dir %s", toolPath), err)
		}
		if !toolInfo.IsDir() || toolInfo.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("preserving private integration run root %s: tool path is not a real directory: %s", parent, toolPath)
		}
		toolReal, err := filepath.EvalSymlinks(toolPath)
		if err != nil {
			return preservingError(parent, fmt.Sprintf("resolve tool dir %s", toolPath), err)
		}
		rel, err := filepath.Rel(parentReal, toolReal)
		if err != nil || rel == "." || filepath.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) || filepath.Dir(rel) != "." {
			return fmt.Errorf("preserving private integration run root %s: tool dir %s is outside the root", parent, toolReal)
		}
		if err := os.RemoveAll(toolPath); err != nil {
			return preservingError(parent, fmt.Sprintf("remove tool dir %s", toolPath), err)
		}
	}
	if err := removeParent(parent); err != nil {
		return preservingError(parent, "remove non-empty parent", err)
	}
	return nil
}

func preservationPath(root string) (string, error) {
	marker := ""
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.Name() == PreserveMarkerName || entry.Type()&os.ModeSymlink != 0 {
			marker = path
		}
		return nil
	})
	if os.IsNotExist(err) {
		return "", nil
	}
	return marker, err
}

func preservingError(root, action string, err error) error {
	return fmt.Errorf("preserving private integration run root %s: %s: %w", root, action, err)
}
