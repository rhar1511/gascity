package api

import (
	"errors"
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/attemptevidence"
	"github.com/gastownhall/gascity/internal/beads"
)

// archivedPRActionEvidenceReader joins the controller's PR policy to sealed
// attempts. It exposes only revision/digest references; public diff reads use
// the separate original-scope authorization boundary.
type archivedPRActionEvidenceReader struct {
	reader attemptevidence.Reader
}

func (r archivedPRActionEvidenceReader) References(store beads.Store, storeRef, workID string) ([]PRActionAttemptReference, error) {
	rows, err := r.reader.List(store, workID)
	if err != nil {
		return nil, err
	}
	refs := make([]PRActionAttemptReference, 0, len(rows))
	for _, evidence := range rows {
		if !prArchiveScopeMatches(evidence, workID) || evidence.StoreRef != storeRef {
			return nil, fmt.Errorf("attempt evidence does not belong to the authoritative work store")
		}
		refs = append(refs, PRActionAttemptReference{
			StoreRef: evidence.StoreRef, WorkID: evidence.Identity.OwnerBeadID, AttemptID: evidence.AttemptID,
			BaseSHA: evidence.BaseSHA, CandidateSHA: evidence.CandidateSHA, DiffSHA256: evidence.Diff.SHA256,
			DiffSource: evidence.Diff.Source, WorkingTreeStatus: evidence.WorkingTreeStatus,
		})
	}
	return refs, nil
}

func (r archivedPRActionEvidenceReader) Read(store beads.Store, workID, attemptID string) (PRActionAttemptEvidence, error) {
	evidence, err := r.reader.Read(store, workID, attemptID)
	if errors.Is(err, attemptevidence.ErrNotFound) {
		return PRActionAttemptEvidence{}, ErrPRActionEvidenceMissing
	}
	if err != nil {
		return PRActionAttemptEvidence{}, err
	}
	if evidence.AttemptID != attemptID || !prArchiveScopeMatches(evidence, workID) {
		return PRActionAttemptEvidence{}, fmt.Errorf("attempt evidence does not match the requested execution")
	}
	return PRActionAttemptEvidence{
		OwnerBeadID: evidence.Identity.OwnerBeadID, ExecutionBeadID: evidence.Identity.ExecutionBeadID,
		BaseSHA: evidence.BaseSHA, CandidateSHA: evidence.CandidateSHA, DiffSHA256: evidence.Diff.SHA256,
		DiffSource: evidence.Diff.Source, WorkingTreeStatus: evidence.WorkingTreeStatus,
	}, nil
}

func prArchiveScopeMatches(evidence attemptevidence.Evidence, workID string) bool {
	return evidence.Identity.OwnerBeadID == workID && strings.TrimSpace(evidence.StoreRef) != "" &&
		evidence.Permission.StoreRef == evidence.StoreRef && evidence.Permission.WorkID == workID &&
		strings.TrimSpace(evidence.Permission.RepositoryRoot) != "" && strings.TrimSpace(evidence.Permission.WorkspaceRoot) != ""
}
