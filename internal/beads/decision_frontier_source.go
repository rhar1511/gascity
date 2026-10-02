package beads

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

var (
	_ DecisionFrontierSourceReader = (*MemStore)(nil)
	_ DecisionFrontierSourceReader = (*SQLiteStore)(nil)
)

// DecisionFrontierSourceSnapshot returns the source row and authoritative
// outgoing dependency edges from one SQLite read transaction.
func (s *SQLiteStore) DecisionFrontierSourceSnapshot(id string) (Bead, error) {
	if err := s.ensureOpen(); err != nil {
		return Bead{}, err
	}
	if !s.hasRevisionColumn {
		return Bead{}, ErrConditionalWriteUnsupported
	}
	var result Bead
	err := retryOnBusy(func() error {
		ctx := context.Background()
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("sqlite decision-frontier source snapshot: begin tx: %w", err)
		}
		defer tx.Rollback() //nolint:errcheck
		bead, err := s.getTx(ctx, tx, id)
		if err != nil {
			return err
		}
		dependencies, err := s.depListTx(ctx, tx, id)
		if err != nil {
			return err
		}
		bead.Dependencies = dependencies
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("sqlite decision-frontier source snapshot: commit: %w", err)
		}
		result = bead
		return nil
	})
	if err != nil {
		return Bead{}, fmt.Errorf("reading decision-frontier source %q: %w", id, err)
	}
	return cloneBead(result), nil
}

func (s *SQLiteStore) depListTx(ctx context.Context, tx *sql.Tx, id string) ([]Dep, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT issue_id, depends_on_id, dep_type FROM deps WHERE issue_id=?`, id)
	if err != nil {
		return nil, fmt.Errorf("listing dependencies for %q in decision-frontier snapshot: %w", id, err)
	}
	defer rows.Close() //nolint:errcheck
	var dependencies []Dep
	for rows.Next() {
		var dep Dep
		if err := rows.Scan(&dep.IssueID, &dep.DependsOnID, &dep.Type); err != nil {
			return nil, fmt.Errorf("scanning dependencies for %q in decision-frontier snapshot: %w", id, err)
		}
		dependencies = append(dependencies, dep)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("reading dependencies for %q in decision-frontier snapshot: %w", id, err)
	}
	return dependencies, nil
}

func (s *SQLiteStore) dependencyStateTx(ctx context.Context, tx *sql.Tx, issueID, dependsOnID string) (string, bool, error) {
	var depType string
	err := tx.QueryRowContext(ctx,
		`SELECT dep_type FROM deps WHERE issue_id=? AND depends_on_id=?`, issueID, dependsOnID).Scan(&depType)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("reading dependency %s -> %s: %w", issueID, dependsOnID, err)
	}
	return depType, true, nil
}

// graphEdgeMetadataMatchesTx reports whether the pair's sidecar is exactly the
// metadata the next add would persist. The sidecar is not part of Dep/WorkDigest,
// but changing it still advances or is blocked by the source-owner fence.
func (s *SQLiteStore) graphEdgeMetadataMatchesTx(ctx context.Context, tx *sql.Tx, issueID, dependsOnID, depType, expected string) (bool, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT key, value FROM kv WHERE key GLOB ?`, sqliteGraphEdgeMetadataPairPrefix(issueID, dependsOnID)+"*")
	if err != nil {
		return false, fmt.Errorf("reading dependency metadata %s -> %s: %w", issueID, dependsOnID, err)
	}
	defer rows.Close() //nolint:errcheck
	wantKey := ""
	if expected != "" {
		wantKey = sqliteGraphEdgeMetadataKey(issueID, dependsOnID, depType)
	}
	seen := false
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return false, fmt.Errorf("scanning dependency metadata %s -> %s: %w", issueID, dependsOnID, err)
		}
		if seen || wantKey == "" || key != wantKey || value != expected {
			return false, nil
		}
		seen = true
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("reading dependency metadata %s -> %s: %w", issueID, dependsOnID, err)
	}
	return seen == (wantKey != ""), nil
}

// guardDependencySourceMutationTx blocks an actual edge mutation when its
// owner row is held or is an immutable decision-frontier record. It returns
// whether that row exists so callers can advance its revision in the same
// SQLite transaction.
func (s *SQLiteStore) guardDependencySourceMutationTx(ctx context.Context, tx *sql.Tx, issueID string) (bool, error) {
	bead, err := s.getTx(ctx, tx, issueID)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if HasDecisionFrontierHold(bead) || IsDecisionFrontierRecord(bead) {
		return false, ErrDecisionFrontierMutationBlocked
	}
	return true, nil
}

func (s *SQLiteStore) bumpDependencySourceRevisionTx(ctx context.Context, tx *sql.Tx, issueID string, exists bool) error {
	if !exists || !s.hasRevisionColumn {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE beads SET revision=revision+1 WHERE id=?`, issueID); err != nil {
		return fmt.Errorf("advancing dependency-source revision for %q: %w", issueID, err)
	}
	return nil
}

// depAddForCreatedBeadTx treats only an edge owned by the just-created row as
// part of that row's initial contents. Structured dependency payloads can
// carry an explicit IssueID, so a caller cannot use Create to bypass the hold
// and revision fence on some other existing source row.
func (s *SQLiteStore) depAddForCreatedBeadTx(ctx context.Context, tx *sql.Tx, createdID string, dep Dep) error {
	if dep.IssueID == "" {
		dep.IssueID = createdID
	}
	if dep.IssueID == createdID {
		return s.depAddInitialTx(ctx, tx, dep.IssueID, dep.DependsOnID, dep.Type)
	}
	return s.depAddWithMetadataTx(ctx, tx, dep.IssueID, dep.DependsOnID, dep.Type, "")
}

// guardAndFenceIncomingDependenciesTx covers cascade deletes that remove an
// incoming edge from a surviving source. Source rows deleted by this same
// operation need no version bump; every other changed owner is hold-checked
// and revision-fenced in this transaction.
func (s *SQLiteStore) guardAndFenceIncomingDependenciesTx(ctx context.Context, tx *sql.Tx, targetIDs, deletedIDs []string, terminalTarget *Bead) error {
	if len(targetIDs) == 0 {
		return nil
	}
	targetArgs := make([]any, len(targetIDs))
	targetMarks := make([]string, len(targetIDs))
	for i, id := range targetIDs {
		targetArgs[i] = id
		targetMarks[i] = "?"
	}
	deleted := make(map[string]struct{}, len(deletedIDs))
	for _, id := range deletedIDs {
		deleted[id] = struct{}{}
	}
	ownerQuery := `SELECT DISTINCT issue_id FROM deps WHERE depends_on_id IN (` + strings.Join(targetMarks, ",") + ")"
	if terminalTarget != nil {
		ownerQuery += ` UNION SELECT id FROM beads WHERE parent_id IN (` + strings.Join(targetMarks, ",") + ")"
		targetArgs = append(targetArgs, targetArgs...)
	}
	rows, err := tx.QueryContext(ctx, ownerQuery, targetArgs...)
	if err != nil {
		return fmt.Errorf("listing dependency owners affected by delete: %w", err)
	}
	var owners []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close() //nolint:errcheck
			return err
		}
		if _, ownerDeleted := deleted[id]; !ownerDeleted {
			owners = append(owners, id)
		}
	}
	rowsErr := rows.Err()
	rows.Close() //nolint:errcheck
	if rowsErr != nil {
		return fmt.Errorf("reading dependency owners affected by delete: %w", rowsErr)
	}
	for _, owner := range owners {
		var parentOwner *Bead
		if terminalTarget != nil {
			row, err := s.getTx(ctx, tx, owner)
			if err != nil && !errors.Is(err, ErrNotFound) {
				return err
			}
			if err == nil {
				if err := validateTerminalDeleteReferenceOwner(*terminalTarget, row); err != nil {
					return err
				}
				if row.ParentID == terminalTarget.ID {
					parentOwner = &row
				}
			}
		}
		exists, err := s.guardDependencySourceMutationTx(ctx, tx, owner)
		if err != nil {
			return fmt.Errorf("deleting dependency target: source %q: %w", owner, err)
		}
		if parentOwner != nil {
			parentOwner.ParentID = ""
			// Upsert updates the physical column, retained JSON and revision
			// together, without an additional version bump for the same owner.
			if err := s.upsertBeadTx(ctx, tx, *parentOwner); err != nil {
				return err
			}
			continue
		}
		if err := s.bumpDependencySourceRevisionTx(ctx, tx, owner, exists); err != nil {
			return err
		}
	}
	return nil
}
