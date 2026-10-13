package worklifecycle

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"strings"

	"github.com/gastownhall/gascity/internal/attemptevidence"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// BuildAttemptEvidencePatch binds one compact Workbench capture to its current
// owner and store. The caller must commit this first-write index and reference
// through its protected transition, then finish the archive before closing work.
func BuildAttemptEvidencePatch(source beads.Bead, storeRef, key, value string) (map[string]MetadataStringPatch, error) {
	capture, err := attemptevidence.DecodeCompact([]byte(value))
	if err != nil {
		return nil, fmt.Errorf("invalid compact attempt evidence: %w", ErrTransitionChainEvidence)
	}
	canonical, err := json.Marshal(capture)
	if err != nil || !bytes.Equal(canonical, []byte(value)) || !attemptevidence.IsExecutionRecord(source) ||
		capture.Identity.Kind != attemptevidence.KindWorkbench || capture.Identity.OwnerBeadID != source.ID ||
		capture.Identity.ExecutionBeadID != source.ID || capture.Identity.SessionID != source.Metadata[beadmeta.SessionIDMetadataKey] ||
		capture.Identity.ClaimGeneration != source.Metadata[beadmeta.ClaimGenerationMetadataKey] ||
		storeRef == "" || capture.StoreRef != storeRef || capture.Permission.StoreRef != storeRef || capture.Permission.WorkID != source.ID ||
		capture.Outcome != source.Metadata[beadmeta.WorkOutcomeMetadataKey] || key != attemptevidence.OwnerIndexKey(capture.AttemptID) {
		return nil, fmt.Errorf("attempt evidence differs from the current execution identity, store or outcome: %w", ErrTransitionChainEvidence)
	}
	reference, err := json.Marshal(attemptevidence.EvidenceReference(capture, storeRef))
	if err != nil {
		return nil, fmt.Errorf("encode attempt evidence reference: %w", ErrTransitionChainEvidence)
	}
	return map[string]MetadataStringPatch{
		key: {Value: value}, beadmeta.AttemptEvidenceReferenceMetadataKey: {Value: string(reference)},
	}, nil
}

func validateCompletionAttemptEvidencePatch(patch SourceWorkPatch, source beads.Bead) error {
	var indexKey string
	for key := range patch.Metadata {
		if strings.HasPrefix(key, beadmeta.AttemptEvidenceIndexPrefix) {
			if indexKey != "" {
				return fmt.Errorf("completion has multiple attempt indexes: %w", ErrTransitionChainEvidence)
			}
			indexKey = key
		}
	}
	reference, hasReference := patch.Metadata[beadmeta.AttemptEvidenceReferenceMetadataKey]
	if indexKey == "" && !hasReference {
		return nil // Historical completion receipts may predate attempt capture.
	}
	index := patch.Metadata[indexKey]
	if indexKey == "" || !hasReference || index.Expected != nil || reference.Expected != nil {
		return fmt.Errorf("completion requires a first-write attempt index and reference pair: %w", ErrTransitionChainEvidence)
	}
	marker, err := parseTransitionMaterialization(source.Metadata[beadmeta.LifecycleMaterializationMetadataKey])
	if err != nil {
		return fmt.Errorf("attempt evidence has no verified source store: %w", ErrTransitionChainEvidence)
	}
	prospective := source
	prospective.Metadata = maps.Clone(source.Metadata)
	if outcome, exists := patch.Metadata[beadmeta.WorkOutcomeMetadataKey]; exists {
		prospective.Metadata[beadmeta.WorkOutcomeMetadataKey] = outcome.Value
	}
	want, err := BuildAttemptEvidencePatch(prospective, marker.SourceStoreRef, indexKey, index.Value)
	if err != nil {
		return err
	}
	if reference.Value != want[beadmeta.AttemptEvidenceReferenceMetadataKey].Value {
		return fmt.Errorf("completion reference differs from its sealed attempt: %w", ErrTransitionChainEvidence)
	}
	return nil
}
