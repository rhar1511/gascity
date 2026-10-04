package api

import (
	"strings"

	"github.com/gastownhall/gascity/internal/api/apierr"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// rejectPRActionLedgerMutation keeps controller-owned idempotency receipts
// immutable through ordinary city bead mutation routes. Direct backend access
// remains a deployment boundary and is not protected by this API check.
func rejectPRActionLedgerMutation(b beads.Bead) error {
	if strings.HasPrefix(b.ID, "gc-pr-action-") && b.Metadata[prActionSourceMetadataKey] == prActionRecordSource {
		return apierr.Forbidden.Msg("PR action ledger records are controller managed")
	}
	return nil
}

// validatePRActionMetadata prevents generic writes from manufacturing ledger authority.
func validatePRActionMetadata(metadata map[string]string) error {
	for key := range metadata {
		if strings.HasPrefix(key, beadmeta.PRActionMetadataPrefix) {
			return apierr.Forbidden.Msg("PR action ledger metadata is reserved for the controller")
		}
	}
	return nil
}
