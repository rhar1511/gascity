package beads

import (
	"errors"
	"fmt"

	"github.com/gastownhall/gascity/internal/beadmeta"
)

// ErrProtectedAttemptEvidenceArchive is returned when a direct store delete
// would remove an immutable attempt-evidence archive row.
var ErrProtectedAttemptEvidenceArchive = errors.New("attempt-evidence archive rows cannot be deleted")

// IsAttemptEvidenceArchive reports whether b is a fully formed archive row.
// Requiring all identifying and payload fields avoids treating partial legacy
// metadata as a protected evidence record.
func IsAttemptEvidenceArchive(b Bead) bool {
	return b.Metadata[beadmeta.AttemptEvidenceArchiveAttemptIDMetadataKey] != "" &&
		b.Metadata[beadmeta.AttemptEvidenceArchiveOwnerIDMetadataKey] != "" &&
		b.Metadata[beadmeta.AttemptEvidenceArchivePayloadMetadataKey] != ""
}

func protectAttemptEvidenceDelete(b Bead) error {
	if IsAttemptEvidenceArchive(b) {
		return fmt.Errorf("deleting bead %q: %w", b.ID, ErrProtectedAttemptEvidenceArchive)
	}
	return nil
}

// PrivatePayloadValueTransportTargeter declares the store beneath a policy or
// cache wrapper. The wrapper is unwrapped to its exact concrete backend; a
// wrapper that cannot prove its target is unsupported.
type PrivatePayloadValueTransportTargeter interface {
	PrivatePayloadValueTransportTarget() Store
}

// SupportsPrivatePayloadValues reports whether evidence bytes can be stored in
// a durable backend without exposing them through an argv-style transport.
// The explicit allow-list is intentional: BdStore currently sends metadata
// values through `bd` command arguments, so it cannot safely persist evidence.
func SupportsPrivatePayloadValues(store Store) bool {
	for depth := 0; store != nil && depth < 16; depth++ {
		if targeter, ok := store.(PrivatePayloadValueTransportTargeter); ok {
			target := targeter.PrivatePayloadValueTransportTarget()
			if target == nil {
				return false
			}
			store = target
			continue
		}
		switch store.(type) {
		case *FileStore, *SQLiteStore, *NativeDoltStore:
			return true
		default:
			// This is an explicit allow-list. In particular, MemStore is not
			// durable, and BdStore, ProxiedStore, BeadsLibStore, and exec-backed
			// stores are unsupported until their persistence and payload transport
			// are independently proven safe.
			return false
		}
	}
	return false
}

// PrivatePayloadValueTransportTarget unwraps the cache to its durable backend.
func (c *CachingStore) PrivatePayloadValueTransportTarget() Store { return c.backing }

// PrivatePayloadValueTransportTarget returns the backend beneath the work view.
func (s WorkStore) PrivatePayloadValueTransportTarget() Store { return s.Store }

// PrivatePayloadValueTransportTarget returns the backend beneath the graph view.
func (s GraphStore) PrivatePayloadValueTransportTarget() Store { return s.Store }

// PrivatePayloadValueTransportTarget returns the backend beneath the session view.
func (s SessionStore) PrivatePayloadValueTransportTarget() Store { return s.Store }

// PrivatePayloadValueTransportTarget returns the backend beneath the mail view.
func (s MailStore) PrivatePayloadValueTransportTarget() Store { return s.Store }

// PrivatePayloadValueTransportTarget returns the backend beneath the orders view.
func (s OrdersStore) PrivatePayloadValueTransportTarget() Store { return s.Store }

// PrivatePayloadValueTransportTarget returns the backend beneath the nudges view.
func (s NudgesStore) PrivatePayloadValueTransportTarget() Store { return s.Store }
