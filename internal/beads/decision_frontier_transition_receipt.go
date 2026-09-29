package beads

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
)

var (
	_ RevisionTransitionReceiptReader = (*MemStore)(nil)
	_ RevisionTransitionReceiptReader = (*SQLiteStore)(nil)
)

// DecisionFrontierRevisionTransitionReceipt reads one persisted receipt from
// the same memory-store row that owns the source transition.
func (m *MemStore) DecisionFrontierRevisionTransitionReceipt(issueID, receiptID string) (RevisionTransitionReceipt, bool, error) {
	bead, err := m.DecisionFrontierSourceSnapshot(issueID)
	if err != nil {
		return RevisionTransitionReceipt{}, false, err
	}
	return decisionFrontierRevisionTransitionReceipt(bead, issueID, receiptID)
}

// DecisionFrontierRevisionTransitionReceipt reads one persisted receipt from
// the authoritative SQLite source snapshot.
func (s *SQLiteStore) DecisionFrontierRevisionTransitionReceipt(issueID, receiptID string) (RevisionTransitionReceipt, bool, error) {
	bead, err := s.DecisionFrontierSourceSnapshot(issueID)
	if err != nil {
		return RevisionTransitionReceipt{}, false, err
	}
	return decisionFrontierRevisionTransitionReceipt(bead, issueID, receiptID)
}

func decisionFrontierRevisionTransitionReceipt(bead Bead, issueID, receiptID string) (RevisionTransitionReceipt, bool, error) {
	if issueID == "" || receiptID == "" || bead.ID != issueID {
		return RevisionTransitionReceipt{}, false, fmt.Errorf("%w: source identity does not match the receipt query", ErrDecisionFrontierTransitionReceiptCorrupt)
	}
	raw := bead.Metadata[beadmeta.DecisionFrontierRevisionReceiptsMetadataKey]
	if strings.TrimSpace(raw) == "" {
		return RevisionTransitionReceipt{}, false, nil
	}
	var receipts []RevisionTransitionReceipt
	if err := json.Unmarshal([]byte(raw), &receipts); err != nil || receipts == nil {
		return RevisionTransitionReceipt{}, false, fmt.Errorf("%w: decode source receipts", ErrDecisionFrontierTransitionReceiptCorrupt)
	}
	var found *RevisionTransitionReceipt
	seen := make(map[string]struct{}, len(receipts))
	for _, receipt := range receipts {
		if receipt.ID == "" || receipt.CityRef == "" || receipt.StoreRef == "" || receipt.WorkID != issueID ||
			receipt.MapID == "" || (receipt.Operation != "reserve" && receipt.Operation != "release") ||
			receipt.FromRevision == 0 || receipt.ToRevision == 0 || receipt.ToRevision == receipt.FromRevision {
			return RevisionTransitionReceipt{}, false, fmt.Errorf("%w: invalid source receipt", ErrDecisionFrontierTransitionReceiptCorrupt)
		}
		if _, duplicate := seen[receipt.ID]; duplicate {
			return RevisionTransitionReceipt{}, false, fmt.Errorf("%w: duplicate source receipt ID", ErrDecisionFrontierTransitionReceiptCorrupt)
		}
		seen[receipt.ID] = struct{}{}
		if receipt.ID == receiptID {
			copy := receipt
			found = &copy
		}
	}
	if found == nil {
		return RevisionTransitionReceipt{}, false, nil
	}
	return *found, true, nil
}
