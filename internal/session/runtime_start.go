package session

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
)

// Runtime operations and metadata transactions have different lifetimes. A
// provider may wait for its child to claim work before reporting ready, so that
// wait cannot own the metadata lock used by CommitWorkClaim. This process-wide
// reservation still excludes another manager's runtime operation or corpse
// cleanup for the same session while metadata transactions can make progress.
type runtimeStartReservation struct {
	available chan struct{}
	refs      int
}

var runtimeStarts = struct {
	sync.Mutex
	entries map[string]*runtimeStartReservation
}{entries: make(map[string]*runtimeStartReservation)}

func referenceRuntimeStart(id string) *runtimeStartReservation {
	runtimeStarts.Lock()
	defer runtimeStarts.Unlock()
	entry := runtimeStarts.entries[id]
	if entry == nil {
		entry = &runtimeStartReservation{available: make(chan struct{}, 1)}
		entry.available <- struct{}{}
		runtimeStarts.entries[id] = entry
	}
	entry.refs++
	return entry
}

func unreferenceRuntimeStart(id string, entry *runtimeStartReservation) {
	runtimeStarts.Lock()
	defer runtimeStarts.Unlock()
	entry.refs--
	if entry.refs == 0 {
		delete(runtimeStarts.entries, id)
	}
}

func withRuntimeStart(ctx context.Context, id string, fn func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	entry := referenceRuntimeStart(id)
	defer unreferenceRuntimeStart(id, entry)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-entry.available:
		defer func() { entry.available <- struct{}{} }()
		if err := ctx.Err(); err != nil {
			return err
		}
		return fn()
	}
}

func (m *Manager) runtimeStartRunning(ctx context.Context, expected Info, name string) (bool, error) {
	var running bool
	err := m.runtimeStartIO(ctx, expected, func() error {
		running = m.sp.IsRunning(name)
		return nil
	})
	return running, err
}

func tryWithRuntimeStart(id string, fn func() error) (bool, error) {
	entry := referenceRuntimeStart(id)
	defer unreferenceRuntimeStart(id, entry)
	select {
	case <-entry.available:
		defer func() { entry.available <- struct{}{} }()
		return true, fn()
	default:
		return false, nil
	}
}

// ErrRuntimeStartSuperseded refuses further effects for a start whose session
// changed while provider I/O was in flight. The controller retains ownership
// of its existing async commit and attempted-runtime cleanup protocol.
var ErrRuntimeStartSuperseded = errors.New("runtime start superseded")

// runtimeStartAcceptedError retains a completed provider Start observation
// while the lifecycle result still refuses every effect on the superseded row.
type runtimeStartAcceptedError struct{ cause error }

func (e *runtimeStartAcceptedError) Error() string { return e.cause.Error() }
func (e *runtimeStartAcceptedError) Unwrap() error { return e.cause }

// RuntimeStartProviderAccepted reports that a runtime-only start called the
// provider successfully before an authoritative post-call supersession. This
// proves endpoint acceptance only; it never permits a lifecycle commit or retry.
func RuntimeStartProviderAccepted(err error) bool {
	var accepted *runtimeStartAcceptedError
	return errors.As(err, &accepted)
}

// runtimeProviderStartIO is reserved for actual provider Start calls. Generic
// observations, orphan cleanup and readiness waiters cannot prove acceptance.
func (m *Manager) runtimeProviderStartIO(ctx context.Context, expected Info, effect func() error) error {
	called, accepted := false, false
	err := m.runtimeStartIO(ctx, expected, func() error {
		called = true
		providerErr := effect()
		accepted = providerErr == nil
		return providerErr
	})
	if called && accepted && ctx.Err() == nil && errors.Is(err, ErrRuntimeStartSuperseded) {
		return &runtimeStartAcceptedError{cause: err}
	}
	return err
}

type runtimeStartGuardKey struct{}

type runtimeStartGuard struct {
	id   string
	lock *sessionMutationLockEntry
}

// runtimeStartIO is used only by the runtime-only bridge. Other manager entry
// points retain their existing metadata transaction behavior. The guard keeps
// its reference to the metadata mutex while unlocked, so a concurrent claim
// uses that same mutex rather than a newly allocated map entry.
func (m *Manager) runtimeStartIO(ctx context.Context, expected Info, effect func() error) error {
	guard, _ := ctx.Value(runtimeStartGuardKey{}).(*runtimeStartGuard)
	if guard == nil {
		return effect()
	}
	if guard.id != expected.ID {
		return ErrRuntimeStartSuperseded
	}
	check := func() error {
		// The cached session projection can remain stale after another writer
		// changes the incarnation. This fence must read the authoritative
		// handle and must not repair types or route ACP as a read side effect.
		row, err := NewStore(beads.SessionStore{Store: m.store}).validatedBeadForRead(expected.ID, true)
		if err != nil {
			return runtimeStartReadError(err)
		}

		if !runtimeStartStillCurrent(expected, infoFromPersistedBead(row)) {
			return ErrRuntimeStartSuperseded
		}
		return nil
	}
	if err := check(); err != nil {
		return err
	}
	providerErr := func() error {
		guard.lock.mu.Unlock()
		defer guard.lock.mu.Lock()
		return effect()
	}()
	// A stale verdict must not retain ErrSessionDiedDuringStartup: that error
	// authorizes stale-key recovery, which must never mutate a newer execution.
	if err := check(); err != nil {
		if errors.Is(err, ErrRuntimeStartSuperseded) && runtime.IsProviderCapacity(providerErr) {
			// A refusal permits only fenced PreWake recovery, never stale-key
			// fallback. Keep its typed capacity details without its cause chain.
			capacity := &runtime.CapacityError{Err: errors.New(providerErr.Error())}
			var original *runtime.CapacityError
			if errors.As(providerErr, &original) {
				capacity.ExitCode, capacity.Source = original.ExitCode, original.Source
			}
			return fmt.Errorf("%w: %w", err, capacity)
		}
		return err
	}
	return providerErr
}

// Failure to read identity is uncertainty, not a failed provider launch. Only
// authoritative absence or a different session class proves supersession.
func runtimeStartReadError(err error) error {
	if errors.Is(err, beads.ErrNotFound) || errors.Is(err, ErrSessionNotFound) ||
		errors.Is(err, ErrNotSession) || errors.Is(err, ErrSessionClosed) {
		return fmt.Errorf("%w: %w", ErrRuntimeStartSuperseded, err)
	}
	return fmt.Errorf("validating runtime start identity: %w: %w", runtime.ErrRuntimeUnavailable, err)
}

// RuntimeStartRefusalRestoreAllowed reports whether a refused launch still owns
// its execution's PreWake values. Concurrent dormant holds may survive recovery;
// changed custody, claim ownership or live-state progress forbids all restoration.
func RuntimeStartRefusalRestoreAllowed(expected, current Info) bool {
	if !runtimeStartSameCustody(expected, current) || expected.PendingCreateClaim != current.PendingCreateClaim ||
		current.State == StateActive || current.State == StateAwake {
		return false
	}
	return expected.State == current.State || current.State == StateSuspended || current.State == StateAsleep
}

func runtimeStartSameCustody(expected, current Info) bool {
	if current.Closed || current.ID != expected.ID ||
		!LeaseFromInfo(expected).SameIdentity(LeaseFromInfo(current)) ||
		AssigneeIdentifier(expected) != AssigneeIdentifier(current) ||
		expected.SessionNameMetadata != current.SessionNameMetadata ||
		expected.Provider != current.Provider || expected.ProviderKind != current.ProviderKind ||
		expected.RuntimeBackend != current.RuntimeBackend || expected.RuntimeFingerprint != current.RuntimeFingerprint ||
		expected.Template != current.Template || expected.Command != current.Command ||
		expected.Transport != current.Transport || expected.WorkDir != current.WorkDir ||
		expected.WorkDirCanonical != current.WorkDirCanonical ||
		expected.WorktreeSourceBeadID != current.WorktreeSourceBeadID ||
		expected.WorktreeSourceStoreRef != current.WorktreeSourceStoreRef ||
		expected.WorktreeAttemptID != current.WorktreeAttemptID ||
		expected.TriggerBeadID != current.TriggerBeadID || expected.TriggerBeadStoreRef != current.TriggerBeadStoreRef ||
		expected.ContinuationEpoch != current.ContinuationEpoch ||
		expected.ContinuationResetPending != current.ContinuationResetPending || expected.WakeMode != current.WakeMode {
		return false
	}
	// An initially unkeyed provider may publish its discovered key, but a
	// known conversation must not be silently exchanged during a resume.
	return expected.SessionKey == "" || expected.SessionKey == current.SessionKey
}

func runtimeStartStillCurrent(expected, current Info) bool {
	if !runtimeStartSameCustody(expected, current) ||
		expected.HeldUntil != current.HeldUntil || expected.QuarantinedUntil != current.QuarantinedUntil ||
		expected.SleepIntent != current.SleepIntent {
		return false
	}
	if expected.State == current.State {
		return !expected.PendingCreateClaim || current.PendingCreateClaim ||
			current.State == StateActive || current.State == StateAwake
	}
	// The reconciler may heal a starting row to awake/active and clear its
	// pending-create claim before Start returns. Preserve that existing lease
	// behavior, including generation drift when the token still matches.
	return (current.State == StateActive || current.State == StateAwake) &&
		LeaseFromInfo(expected).CommitVerdict(LeaseFromInfo(current)) == LeaseCommit
}

// SourceWorkspaceDir preserves the exact controller-published native ownership
// tuple. It is a consistency check, not admission or native ownership proof.
// Partial custody must not fall back to legacy assignee-based cwd discovery.
func (i Info) SourceWorkspaceDir() (string, bool, error) {
	bound := i.WorktreeSourceBeadID != "" || i.WorktreeSourceStoreRef != "" || i.WorktreeAttemptID != ""
	if !bound {
		return "", false, nil
	}
	path := i.WorkDirCanonical
	if i.WorktreeSourceBeadID == "" || i.WorktreeSourceStoreRef == "" || i.WorktreeAttemptID == "" ||
		i.TriggerBeadID == "" || i.TriggerBeadID == i.WorktreeSourceBeadID || i.TriggerBeadStoreRef != i.WorktreeSourceStoreRef ||
		!strings.HasPrefix(i.WorktreeSourceStoreRef, "rig:") || strings.TrimPrefix(i.WorktreeSourceStoreRef, "rig:") == "" ||
		path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path || i.WorkDir != path {
		return "", true, fmt.Errorf("session %s: incomplete or conflicting published source workspace custody", i.ID)
	}
	return path, true, nil
}

func validateSourceWorkspaceRuntimeConfig(info Info, cfg runtime.Config) error {
	path, bound, err := info.SourceWorkspaceDir()
	if err != nil {
		return err
	}
	if bound && (cfg.WorkDir != path || cfg.Env["GC_DIR"] != path ||
		cfg.Env["GC_TRIGGER_WORK_BEAD_ID"] != info.TriggerBeadID ||
		cfg.Env["GC_TRIGGER_WORK_STORE_REF"] != info.WorktreeSourceStoreRef) {
		return fmt.Errorf("session %s: prepared runtime differs from published source workspace or trigger", info.ID)
	}
	return nil
}
