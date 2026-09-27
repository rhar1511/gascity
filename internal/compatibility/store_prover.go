package compatibility

import (
	"context"
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/qualification"
)

// CapabilityAttemptEvidencePrivatePayload is the controller capability name
// for a durable store that can safely persist private attempt-evidence payloads
// and revision-CAS the metadata used to seal those payloads. It intentionally
// says nothing about backup coverage, restore procedures, read authorization,
// or production deployment.
const CapabilityAttemptEvidencePrivatePayload = "attempt-evidence-private-payload"

// StoreCapabilityProver proves only the explicitly named, implemented store
// capabilities. Its StoreRef must be the exact configured store reference in
// the requested action scope; a caller cannot use one store's proof for another.
type StoreCapabilityProver struct {
	Store    beads.Store
	StoreRef string
}

// Prove returns an unavailable error for an unknown capability or when the
// selected store lacks safe payload transport or metadata CAS.
func (p StoreCapabilityProver) Prove(_ context.Context, scope qualification.CompatibilityScope, policy qualification.CompatibilityPolicy, capability string) (qualification.CapabilityProof, error) {
	if strings.TrimSpace(p.StoreRef) == "" || p.StoreRef != scope.StoreRef {
		return qualification.CapabilityProof{}, fmt.Errorf("store capability scope mismatch: %w", qualification.ErrUnavailable)
	}
	if capability != CapabilityAttemptEvidencePrivatePayload {
		return qualification.CapabilityProof{}, fmt.Errorf("capability %q is not implemented: %w", capability, qualification.ErrUnavailable)
	}
	scopeSHA, err := qualification.CompatibilityScopeIdentitySHA(scope)
	if err != nil || policy.Status != qualification.StatusAvailable || policy.ScopeSHA256 != scopeSHA || strings.TrimSpace(policy.PolicyReference) == "" || strings.TrimSpace(policy.PolicyVersion) == "" {
		return qualification.CapabilityProof{}, fmt.Errorf("store capability policy scope is unavailable: %w", qualification.ErrUnavailable)
	}
	if !beads.SupportsPrivatePayloadValues(p.Store) {
		return qualification.CapabilityProof{}, fmt.Errorf("store does not support private attempt-evidence payload transport: %w", qualification.ErrUnavailable)
	}
	backend, ok := privatePayloadBackend(p.Store)
	if !ok {
		return qualification.CapabilityProof{}, fmt.Errorf("private attempt-evidence payload backend is unavailable: %w", qualification.ErrUnavailable)
	}
	// These are resolved on the exact outer store. Generic target-unwrapping
	// would bypass wrapper-owned mutation fencing, cache invalidation, or read
	// routing even if the leaf itself advertises a comparable primitive.
	metadataCAS, ok := beads.PrivateEvidenceMetadataCASWriterFor(p.Store)
	if !ok {
		return qualification.CapabilityProof{}, fmt.Errorf("outer store lacks private attempt-evidence metadata CAS: %w", qualification.ErrUnavailable)
	}
	archiveReader, ok := beads.PrivateEvidenceArchiveReaderFor(p.Store)
	if !ok {
		return qualification.CapabilityProof{}, fmt.Errorf("outer store lacks private attempt-evidence archive reads: %w", qualification.ErrUnavailable)
	}
	evidence, err := qualification.DigestJSON(struct {
		SchemaVersion     int    `json:"schema_version"`
		Capability        string `json:"capability"`
		ScopeSHA256       string `json:"scope_sha256"`
		PolicyReference   string `json:"policy_reference"`
		PolicyVersion     string `json:"policy_version"`
		StoreRef          string `json:"store_ref"`
		OuterStore        string `json:"outer_store"`
		PayloadBackend    string `json:"payload_backend"`
		PayloadReady      bool   `json:"private_payload_transport_ready"`
		MetadataCASHandle string `json:"private_metadata_cas_handle"`
		ArchiveReadHandle string `json:"private_archive_read_handle"`
	}{
		SchemaVersion:     2,
		Capability:        capability,
		ScopeSHA256:       scopeSHA,
		PolicyReference:   policy.PolicyReference,
		PolicyVersion:     policy.PolicyVersion,
		StoreRef:          p.StoreRef,
		OuterStore:        fmt.Sprintf("%T", p.Store),
		PayloadBackend:    fmt.Sprintf("%T", backend),
		PayloadReady:      true,
		MetadataCASHandle: fmt.Sprintf("%T", metadataCAS),
		ArchiveReadHandle: fmt.Sprintf("%T", archiveReader),
	})
	if err != nil {
		return qualification.CapabilityProof{}, err
	}
	return qualification.CapabilityProof{
		Status:          qualification.StatusAvailable,
		Capability:      capability,
		ScopeSHA256:     scopeSHA,
		PolicyReference: policy.PolicyReference,
		PolicyVersion:   policy.PolicyVersion,
		EvidenceSHA256:  evidence,
	}, nil
}

func privatePayloadBackend(store beads.Store) (beads.Store, bool) {
	for depth := 0; store != nil && depth < 16; depth++ {
		targeter, ok := store.(beads.PrivatePayloadValueTransportTargeter)
		if !ok {
			return store, true
		}
		target := targeter.PrivatePayloadValueTransportTarget()
		if target == nil || target == store {
			return nil, false
		}
		store = target
	}
	return nil, false
}
