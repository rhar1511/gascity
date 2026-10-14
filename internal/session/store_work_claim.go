package session

import (
	"errors"
	"fmt"
	"maps"
	"reflect"
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// ErrWorkClaimSessionChanged means the original managed incarnation, native
// conversation, workspace, or eligible lifecycle state is no longer current.
var ErrWorkClaimSessionChanged = errors.New("work claim session changed")

// CanClaimWork reports the existing managed lifecycle states permitted to
// claim work. It grants no source, route, signature, or native workspace proof.
func CanClaimWork(info Info) bool {
	if !IsSessionBeadOrRepairableInfo(info) || info.Closed {
		return false
	}
	switch ProjectLifecycle(LifecycleInputFromInfo(info)).BaseState {
	case BaseStateNone, BaseStateStartPending, BaseStateCreating, BaseStateActive:
		return true
	default:
		return false
	}
}

// CommitWorkClaim retains the process-local session mutation lock across a
// fresh identity read, the trusted durable work transition, and its reciprocal
// claim stamp. transition must not acquire the same session lock or start a
// runtime provider. Its work-store commit is separate: a lost response or an
// external session change after commit requires exact receipt replay, not
// rollback or a replacement claim. Reads bypass caches, and the reciprocal
// stamp requires a conditional writer. External writers do not honor the lock;
// the exact row revision fences the stamp, not the separate work transition.
func (s *Store) CommitWorkClaim(expected Info, transition func(Info, PersistedResponse) (string, string, error)) error {
	if expected.ID == "" || expected.InstanceToken == "" || expected.Generation == "" || transition == nil {
		return ErrWorkClaimSessionChanged
	}
	if s == nil || s.store.Store == nil {
		return ErrWorkClaimSessionChanged
	}
	writer, _, err := beads.ResolveConditionalWriter(s.store.Store)
	if err != nil {
		return err
	}
	if writer == nil {
		return beads.ErrConditionalWriteUnsupported
	}
	return WithSessionMutationLock(expected.ID, func() error {
		readCurrent := func() (beads.Bead, Info, error) {
			row, err := beads.HandlesFor(s.store.Store).Live.Get(expected.ID)
			if err != nil {
				return beads.Bead{}, Info{}, err
			}
			current := infoFromPersistedBead(row)
			if row.Revision == 0 || !sameWorkClaimSession(expected, current) {
				return beads.Bead{}, Info{}, ErrWorkClaimSessionChanged
			}
			return row, current, nil
		}
		original, current, err := readCurrent()
		if err != nil {
			return err
		}
		workID, generation, err := transition(current, PersistedResponseFromBead(original))
		if err != nil {
			return err
		}
		parsed, parseErr := strconv.ParseInt(generation, 10, 64)
		if workID == "" || strings.TrimSpace(workID) != workID || parseErr != nil || parsed <= 0 || strconv.FormatInt(parsed, 10) != generation {
			return fmt.Errorf("work transition returned an invalid reciprocal claim")
		}
		row, _, err := readCurrent()
		if err != nil {
			return err
		}
		// Do not merge an external session/custody update into this attempt. The
		// work transition may already be durable, so retain it for exact replay.
		if row.Revision != original.Revision || row.Status != original.Status || !reflect.DeepEqual(row.Metadata, original.Metadata) {
			return ErrWorkClaimSessionChanged
		}
		expectedMetadata := maps.Clone(row.Metadata)
		if expectedMetadata == nil {
			expectedMetadata = map[string]string{}
		}
		expectedMetadata[beadmeta.CurrentClaimBeadIDMetadataKey] = workID
		expectedMetadata[beadmeta.CurrentClaimGenerationMetadataKey] = generation
		if !reflect.DeepEqual(row.Metadata, expectedMetadata) {
			err := writer.UpdateIfMatch(row.ID, row.Revision, beads.UpdateOpts{Metadata: map[string]string{
				beadmeta.CurrentClaimBeadIDMetadataKey: workID, beadmeta.CurrentClaimGenerationMetadataKey: generation,
			}})
			if beads.IsPreconditionFailed(err) {
				return errors.Join(ErrWorkClaimSessionChanged, err)
			}
			if err != nil {
				return err
			}
		}
		readback, _, err := readCurrent()
		if err != nil {
			return err
		}
		if readback.Status != row.Status || !reflect.DeepEqual(readback.Metadata, expectedMetadata) {
			return ErrWorkClaimSessionChanged
		}
		return nil
	})
}

// ReadWorkClaimSession authoritatively reads the exact managed incarnation and
// its persisted custody. It takes no lock; callers retaining custody across a
// transition must already hold WithSessionMutationLock.
func (s *Store) ReadWorkClaimSession(expected Info) (Info, PersistedResponse, error) {
	if s == nil || s.store.Store == nil {
		return Info{}, PersistedResponse{}, ErrWorkClaimSessionChanged
	}
	row, err := beads.HandlesFor(s.store.Store).Live.Get(expected.ID)
	if err != nil {
		return Info{}, PersistedResponse{}, err
	}
	current := infoFromPersistedBead(row)
	if !sameWorkClaimSession(expected, current) {
		return Info{}, PersistedResponse{}, ErrWorkClaimSessionChanged
	}
	return current, PersistedResponseFromBead(row), nil
}

func sameWorkClaimSession(expected, current Info) bool {
	return CanClaimWork(current) && current.ID == expected.ID &&
		current.InstanceToken == expected.InstanceToken && current.Generation == expected.Generation &&
		AssigneeIdentifier(current) == AssigneeIdentifier(expected) && current.Template == expected.Template &&
		current.Provider == expected.Provider && current.SessionKey == expected.SessionKey &&
		current.WorkDir == expected.WorkDir && current.WorkDirCanonical == expected.WorkDirCanonical
}
