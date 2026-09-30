package beads

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
)

// ErrProtectedAttemptEvidenceArchive is returned when a direct store delete
// would remove an immutable attempt-evidence archive row.
var ErrProtectedAttemptEvidenceArchive = errors.New("attempt-evidence archive rows cannot be deleted")

// ErrRetainedSessionRequestEvidence is returned when a direct store delete
// would remove durable request receipts or their append-only event ledger.
var ErrRetainedSessionRequestEvidence = errors.New("session request evidence must be retained")

// ErrPrivateEvidenceTransportUnsupported reports that a store cannot safely
// read private attempt-evidence values through its available transport.
var ErrPrivateEvidenceTransportUnsupported = errors.New("private attempt-evidence transport unsupported")

// ErrImmutableAttemptEvidenceArchive is returned when a direct store write
// would alter an attempt-evidence archive after it has been created.
var ErrImmutableAttemptEvidenceArchive = errors.New("attempt-evidence archive rows are immutable")

// ErrImmutableAttemptEvidencePayload is returned when a direct store write
// would mutate a content-addressed evidence payload.
var ErrImmutableAttemptEvidencePayload = errors.New("content-addressed attempt-evidence payloads are immutable")

// ErrPrivatePayloadCreateUnsupported indicates the resolved durable backend
// cannot atomically create a private payload at its deterministic ID.
var ErrPrivatePayloadCreateUnsupported = errors.New("private payload create-if-absent is unsupported")

// IsAttemptEvidenceArchive reports whether b is a fully formed archive row.
// Requiring all identifying and payload fields avoids treating partial legacy
// metadata as a protected evidence record.
func IsAttemptEvidenceArchive(b Bead) bool {
	return b.Metadata[beadmeta.AttemptEvidenceArchiveAttemptIDMetadataKey] != "" &&
		b.Metadata[beadmeta.AttemptEvidenceArchiveOwnerIDMetadataKey] != "" &&
		b.Metadata[beadmeta.AttemptEvidenceArchivePayloadMetadataKey] != ""
}

// HasAttemptEvidenceArchiveMarkers also recognizes incomplete archive rows.
// A damaged archive must remain private, retained, and immutable rather than
// becoming an ordinary mutable row because one marker was lost.
func HasAttemptEvidenceArchiveMarkers(b Bead) bool {
	for _, key := range []string{
		beadmeta.AttemptEvidenceArchiveAttemptIDMetadataKey,
		beadmeta.AttemptEvidenceArchiveOwnerIDMetadataKey,
		beadmeta.AttemptEvidenceArchivePayloadMetadataKey,
		beadmeta.AttemptEvidenceArchiveDigestMetadataKey,
	} {
		if _, ok := b.Metadata[key]; ok {
			return true
		}
	}
	return false
}

// IsAttemptEvidencePayload reports whether b is a complete content-addressed
// evidence payload row. The digest is the row's immutable address; presence of
// the data key (including the empty string for an empty payload) distinguishes
// a complete row from partial metadata.
func IsAttemptEvidencePayload(b Bead) bool {
	digest := b.Metadata[beadmeta.AttemptEvidencePayloadDigestMetadataKey]
	if len(digest) != 64 {
		return false
	}
	if _, err := hex.DecodeString(digest); err != nil {
		return false
	}
	_, hasData := b.Metadata[beadmeta.AttemptEvidencePayloadDataMetadataKey]
	return hasData
}

// HasAttemptEvidencePayloadMarkers reports whether a row contains either
// reserved content-payload marker. It also catches incomplete rows so a
// malformed payload cannot be made mutable or visible through ordinary paths.
func HasAttemptEvidencePayloadMarkers(b Bead) bool {
	_, hasDigest := b.Metadata[beadmeta.AttemptEvidencePayloadDigestMetadataKey]
	_, hasData := b.Metadata[beadmeta.AttemptEvidencePayloadDataMetadataKey]
	return hasDigest || hasData
}

// IsProtectedAttemptEvidenceRecord reports whether b is an archive or a
// content-addressed payload row that must stay private and retained.
func IsProtectedAttemptEvidenceRecord(b Bead) bool {
	return HasAttemptEvidenceArchiveMarkers(b) || HasAttemptEvidencePayloadMarkers(b)
}

func protectAttemptEvidenceDelete(b Bead) error {
	if IsProtectedAttemptEvidenceRecord(b) {
		return fmt.Errorf("deleting bead %q: %w", b.ID, ErrProtectedAttemptEvidenceArchive)
	}
	return nil
}

// HasRetainedSessionRequestEvidence reports whether b owns at least one durable
// request receipt. Receipt rows have no automatic deletion policy.
func HasRetainedSessionRequestEvidence(b Bead) bool {
	for key := range b.Metadata {
		if strings.HasPrefix(key, beadmeta.SessionRequestReceiptPrefix) {
			return true
		}
	}
	return false
}

func protectRetainedEvidenceDelete(b Bead) error {
	if err := protectAttemptEvidenceDelete(b); err != nil {
		return err
	}
	if HasRetainedSessionRequestEvidence(b) {
		return fmt.Errorf("deleting bead %q: %w", b.ID, ErrRetainedSessionRequestEvidence)
	}
	return nil
}

func protectAttemptEvidencePayloadMutation(b Bead) error {
	if HasAttemptEvidencePayloadMarkers(b) {
		return fmt.Errorf("updating bead %q: %w", b.ID, ErrImmutableAttemptEvidencePayload)
	}
	return nil
}

func protectAttemptEvidenceRecordMutation(b Bead) error {
	if err := protectAttemptEvidencePayloadMutation(b); err != nil {
		return err
	}
	if HasAttemptEvidenceArchiveMarkers(b) {
		return fmt.Errorf("updating bead %q: %w", b.ID, ErrImmutableAttemptEvidenceArchive)
	}
	return nil
}

// protectAttemptEvidenceUpdate permits only the terminal open-to-closed style
// update used to make a newly created archive non-actionable. Every other
// whole-row change to an archive, and every change to a payload row, is denied.
func protectAttemptEvidenceUpdate(b Bead, opts UpdateOpts) error {
	if err := protectAttemptEvidencePayloadMutation(b); err != nil {
		return err
	}
	if !HasAttemptEvidenceArchiveMarkers(b) {
		return nil
	}
	closeOnly := opts.Status != nil && *opts.Status == "closed" &&
		opts.Title == nil && opts.Type == nil && opts.Priority == nil && opts.Description == nil &&
		opts.ParentID == nil && opts.Assignee == nil && len(opts.Labels) == 0 &&
		len(opts.RemoveLabels) == 0 && len(opts.Metadata) == 0
	if closeOnly {
		return nil
	}
	return fmt.Errorf("updating bead %q: %w", b.ID, ErrImmutableAttemptEvidenceArchive)
}

func protectAttemptEvidencePayloadMetadataMutation(metadata map[string]string) error {
	return protectAttemptEvidencePayloadMutation(Bead{Metadata: metadata})
}

func rejectAttemptEvidencePayloadMetadataWrite(metadata map[string]string) error {
	if HasAttemptEvidencePayloadMarkers(Bead{Metadata: metadata}) {
		return fmt.Errorf("writing content-addressed payload metadata: %w", ErrImmutableAttemptEvidencePayload)
	}
	return nil
}

func rejectAttemptEvidencePayloadMetadataKeyWrite(key string) error {
	return rejectAttemptEvidencePayloadMetadataWrite(map[string]string{key: ""})
}

// PrivatePayloadValueCreator is the narrow durable-write capability for
// content-addressed attempt payloads. Implementations create at the payload's
// deterministic ID, or return the existing row at that ID for caller-side
// digest verification.
type PrivatePayloadValueCreator interface {
	CreatePrivatePayloadValue(b Bead) (Bead, error)
}

// AttemptEvidencePayloadID returns the deterministic row ID for a SHA-256
// digest. Invalid or non-canonical digest strings return an empty ID.
func AttemptEvidencePayloadID(digest string) string {
	if len(digest) != sha256.Size*2 || strings.ToLower(digest) != digest {
		return ""
	}
	if _, err := hex.DecodeString(digest); err != nil {
		return ""
	}
	return "gc-ep-" + digest
}

// CreatePrivatePayloadValue resolves the durable backend and invokes its
// digest-fenced create-if-absent capability.
func CreatePrivatePayloadValue(store Store, b Bead) (Bead, error) {
	backend, ok := PrivatePayloadValueBackend(store)
	if !ok {
		return Bead{}, ErrPrivatePayloadCreateUnsupported
	}
	creator, ok := backend.(PrivatePayloadValueCreator)
	if !ok {
		return Bead{}, ErrPrivatePayloadCreateUnsupported
	}
	if err := validatePrivatePayloadCreate(b); err != nil {
		return Bead{}, err
	}
	return creator.CreatePrivatePayloadValue(b)
}

func validatePrivatePayloadCreate(b Bead) error {
	digest := b.Metadata[beadmeta.AttemptEvidencePayloadDigestMetadataKey]
	if AttemptEvidencePayloadID(digest) == "" || b.ID != AttemptEvidencePayloadID(digest) {
		return errors.New("private attempt-evidence payload has an invalid deterministic ID")
	}
	encoded, ok := b.Metadata[beadmeta.AttemptEvidencePayloadDataMetadataKey]
	if !ok {
		return errors.New("private attempt-evidence payload is missing its content")
	}
	content, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return errors.New("private attempt-evidence payload content is not valid base64")
	}
	actual := sha256.Sum256(content)
	if hex.EncodeToString(actual[:]) != digest {
		return errors.New("private attempt-evidence payload digest does not match content")
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
// The explicit allow-list is intentional: archive-body transport alone is not
// enough. The backend must also implement deterministic content-payload rows.
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
			return false
		}
	}
	return false
}

// PrivatePayloadValueBackend returns the exact durable backend permitted to
// carry private content-addressed payloads, unwrapping only declared transport
// targets. Callers use the returned leaf for both payload lookup and writes so
// cache wrappers cannot return stale duplicate scans or miss exact readback.
func PrivatePayloadValueBackend(store Store) (Store, bool) {
	for depth := 0; store != nil && depth < 16; depth++ {
		if targeter, ok := store.(PrivatePayloadValueTransportTargeter); ok {
			target := targeter.PrivatePayloadValueTransportTarget()
			if target == nil {
				return nil, false
			}
			store = target
			continue
		}
		switch store.(type) {
		case *FileStore, *SQLiteStore, *NativeDoltStore:
			return store, true
		default:
			// This is an explicit allow-list. In particular, MemStore is not
			// durable, and BdStore, ProxiedStore, BeadsLibStore, and exec-backed
			// stores are unsupported until their persistence and payload transport
			// are independently proven safe.
			return nil, false
		}
	}
	return nil, false
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
