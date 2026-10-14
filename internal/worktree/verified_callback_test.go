package worktree

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestWithVerifiedRequiresExactCreationAndReturnsCallbackError(t *testing.T) {
	repo, base := initTestRepo(t)
	root := t.TempDir()
	spec := Spec{
		RepoDir: repo, Root: root, Path: filepath.Join(root, "source"), Branch: "work/source", Base: base,
		BeadID: "source-1", StoreRef: "rig:compute", Creator: "gc-worktree", Owner: "compute/worker", Generation: "1", Lifecycle: LifecycleActive,
	}
	report, err := Ensure(spec)
	if err != nil {
		t.Fatal(err)
	}
	spec.BaseSHA, spec.AttemptID = report.Provenance.BaseSHA, report.Provenance.AttemptID
	sentinel := errors.New("callback refused")
	calls := 0
	err = WithVerified(spec, func(actual Report) error {
		calls++
		if actual.Provenance.AttemptID != spec.AttemptID {
			t.Fatal("wrong native attempt")
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) || calls != 1 {
		t.Fatalf("callback: calls=%d err=%v", calls, err)
	}
	for _, attempt := range []string{"", "different-attempt"} {
		changed := spec
		changed.AttemptID = attempt
		if err := WithVerified(changed, func(Report) error { calls++; return nil }); err == nil {
			t.Fatal("unbound attempt accepted")
		}
	}
	if calls != 1 {
		t.Fatal("invalid attempt reached callback")
	}
	// A callback failure releases custody rather than leaving a stuck lock.
	if err := WithVerified(spec, func(Report) error { return nil }); err != nil {
		t.Fatal(err)
	}
}
