package beads

import (
	"errors"
	"fmt"
	"os/exec"
)

// ExecutableObserver records one command attempt. BeginAttempt invalidates any
// earlier evidence before lookup or execution can fail. Observe receives the
// selected command; its finalizer runs even when the command fails.
// Both callbacks are required when an observer is supplied.
type ExecutableObserver struct {
	BeginAttempt func()
	Observe      func(cmd *exec.Cmd) (finish func() error, err error)
}

func runObservedCommand(cmd *exec.Cmd, observe *ExecutableObserver) ([]byte, error) {
	return runObservedCommandOutput(cmd, observe, (*exec.Cmd).Output)
}

// runObservedCommandOutput owns callback ordering and error composition. The
// production adapter supplies exec.Cmd.Output; policy tests script only that port.
func runObservedCommandOutput(cmd *exec.Cmd, observe *ExecutableObserver, output func(*exec.Cmd) ([]byte, error)) ([]byte, error) {
	if observe == nil {
		return output(cmd)
	}
	if observe.BeginAttempt == nil || observe.Observe == nil {
		return nil, errors.New("executable observer requires both callbacks")
	}
	observe.BeginAttempt()
	if cmd.Err != nil {
		return output(cmd)
	}
	finish, err := observe.Observe(cmd)
	if err != nil {
		return nil, fmt.Errorf("observe selected executable: %w", err)
	}
	out, commandErr := output(cmd)
	if finish != nil {
		if err := finish(); err != nil {
			return out, errors.Join(commandErr, fmt.Errorf("finish selected executable observation: %w", err))
		}
	}
	return out, commandErr
}
