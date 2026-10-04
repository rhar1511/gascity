package api

import (
	"context"
	"errors"
	"strings"

	"github.com/gastownhall/gascity/internal/api/apierr"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/workbench"
)

// humaHandleBeadAttemptsDiff is the Huma-typed handler for
// GET /v0/city/{cityName}/bead/{id}/attempts/diff.
//
// It returns the READ-ONLY diff of the selected Bead's Execution-Attempt
// worktree. The worktree is Gas City's: this reads gc.work_dir off the Bead and
// runs `git diff` there without staging, discarding, committing, or otherwise
// mutating the worktree. States (empty / missing_worktree / unavailable) are
// explicit so the Workbench can render them without treating them as failures.
func (s *Server) humaHandleBeadAttemptsDiff(ctx context.Context, input *BeadAttemptsDiffInput) (*IndexOutput[workbench.Diff], error) {
	_, bead, err := s.resolveBeadOwner(input.ID)
	if err != nil {
		return nil, err
	}
	dir := strings.TrimSpace(bead.Metadata[beadmeta.WorkDirMetadataKey])
	if dir == "" {
		dir = strings.TrimSpace(bead.Metadata[beadmeta.LegacyWorkDirMetadataKey])
	}
	diff := workbench.WorktreeDiff(ctx, dir, workbench.DefaultMaxDiffBytes)
	return &IndexOutput[workbench.Diff]{
		Index: s.latestIndex(),
		Body:  diff,
	}, nil
}

// humaHandleBeadAttemptHistory reads attempt-scoped artifacts from Gas City's
// durable bead/session records. Session work_dir is intentionally not passed to
// WorktreeDiff: it is a mutable location, not a historical snapshot.
func (s *Server) humaHandleBeadAttemptHistory(_ context.Context, input *BeadAttemptHistoryInput) (*IndexOutput[workbench.AttemptInspection], error) {
	_, bead, err := s.resolveBeadOwner(input.ID)
	if err != nil {
		return nil, err
	}

	store := s.state.SessionsBeadStore().Store
	if store == nil {
		return nil, apierr.ServiceUnavailable.Msg("session bead store unavailable")
	}
	sessionInfo, _, err := session.ResolveSessionRecordByExactID(store, input.SessionID)
	if err != nil {
		switch {
		case errors.Is(err, session.ErrSessionNotFound), errors.Is(err, beads.ErrNotFound):
			return nil, apierr.SessionNotFound.Msg("not_found: " + err.Error())
		default:
			return nil, humaStoreError(err)
		}
	}

	linked := strings.TrimSpace(bead.Metadata[beadmeta.SessionIDMetadataKey]) == sessionInfo.ID ||
		strings.TrimSpace(bead.Metadata[beadmeta.SessionIDCamelMetadataKey]) == sessionInfo.ID ||
		strings.TrimSpace(bead.Metadata["isolation.session"]) == sessionInfo.ID ||
		strings.TrimSpace(sessionInfo.CurrentlyProcessingBeadID) == bead.ID
	inspection := workbench.NewAttemptInspection(bead.ID, sessionInfo.ID, linked)
	return &IndexOutput[workbench.AttemptInspection]{
		Index: s.latestIndex(),
		Body:  inspection,
	}, nil
}
