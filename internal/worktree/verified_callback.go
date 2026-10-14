package worktree

import "errors"

// WithVerified verifies one exact native creation while holding the same path
// lock as Ensure and Cleanup, then keeps that lock through the callback. It
// never creates a workspace or changes provenance. The callback must not call
// Ensure, Cleanup, or WithVerified for the same workspace, which would deadlock.
// Advisory custody excludes cooperating native operations, not arbitrary git
// or filesystem mutation outside this interface.
func WithVerified(spec Spec, callback func(Report) error) error {
	if err := spec.validate(); err != nil {
		return err
	}
	if !spec.managed() || spec.DryRun || spec.AttemptID == "" || spec.BaseSHA == "" || callback == nil {
		return errors.New("verified workspace callback requires complete native creation identity")
	}
	lock, err := lockPath(spec.RepoDir, spec.Path)
	if err != nil {
		return err
	}
	defer lock.unlock()
	report, err := Verify(spec)
	if err != nil {
		return err
	}
	if report.Provenance == nil || report.Provenance.AttemptID != spec.AttemptID {
		return errors.New("verified workspace creation attempt differs")
	}
	return callback(report)
}
