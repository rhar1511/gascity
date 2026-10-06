package dispatch

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
)

// A passing workflow is orchestration evidence, not signed acceptance of the
// deliverable. Its finalizer must leave lifecycle-enrolled source work alone.
func TestWorkflowFinalizeLeavesLifecycleSourceForAcceptance(t *testing.T) {
	for _, key := range []string{
		beadmeta.LifecycleAdmissionReceiptMetadataKey,
		beadmeta.LifecycleMaterializationMetadataKey,
	} {
		t.Run(key, func(t *testing.T) {
			f := newSourceChainFinalizeFixture(t)
			if err := f.cityStore.SetMetadata(f.citySource.ID, key, "controller enrollment evidence"); err != nil {
				t.Fatal(err)
			}
			if _, err := ProcessControl(f.rigStore, f.finalizer, ProcessOptions{ResolveStoreRef: f.resolver}); err != nil {
				t.Fatal(err)
			}
			source := mustGetBead(t, f.cityStore, f.citySource.ID)
			if source.Status != "open" || source.Metadata[beadmeta.OutcomeMetadataKey] != "" {
				t.Fatalf("finalizer changed enrolled source: status=%q outcome=%q", source.Status, source.Metadata[beadmeta.OutcomeMetadataKey])
			}
			if got := mustGetBead(t, f.rigStore, f.workflow.ID).Status; got != "closed" {
				t.Fatalf("workflow status=%q, want its own finalization to finish", got)
			}
		})
	}
}
