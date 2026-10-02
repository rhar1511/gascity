package worklifecycle

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

func TestAdmissionAttachmentUsesExactQ43TransitionAndCurrentProof(t *testing.T) {
	store, private, cfg, source, encoded := admissionAttachmentFixture(t)
	adapter, err := NewAdmissionAttachmentAdapter(store)
	if err != nil {
		t.Fatal(err)
	}

	attached, proof, err := adapter.Attach(encoded, cfg, "rig:pilot")
	if err != nil {
		t.Fatal(err)
	}
	if proof.SchemaVersion != 1 || proof.WorkItemID != source.ID || proof.Scope != "rig:pilot" ||
		proof.FromRevision != source.Revision || proof.ToRevision == 0 || proof.ToRevision != attached.Revision ||
		attached.Metadata[beadmeta.LifecycleAdmissionReceiptV2MetadataKey] != encoded {
		t.Fatalf("attachment = row %+v proof %+v, want exact atomic receipt transition", attached, proof)
	}
	request := store.lastRequest
	if request.ExpectedVersion != source.Revision || request.Expected != nil || request.Key != beadmeta.LifecycleAdmissionReceiptV2MetadataKey ||
		request.ReceiptID != proof.ReceiptID || request.Kind != admissionAttachmentKind || request.Actor != "triage" {
		t.Fatalf("Q43 request = %+v, want absent-key v2 attach at reviewed revision", request)
	}
	var next string
	if request.Value == nil || json.Unmarshal(*request.Value, &next) != nil || next != encoded {
		t.Fatalf("Q43 value = %s, want exact signed receipt bytes %q", request.Value, encoded)
	}
	if got := store.transitionCalls; got != 1 {
		t.Fatalf("Q43 transition calls = %d, want one", got)
	}

	verified, currentProof, err := adapter.VerifyCurrent(source.ID, cfg, "rig:pilot")
	if err != nil || verified.Revision != proof.ToRevision || currentProof != proof {
		t.Fatalf("VerifyCurrent() = row %+v proof %+v err %v, want current exact proof", verified, currentProof, err)
	}

	// Exact retries recover the same immutable receipt; they do not create a
	// second transition or infer a revision.
	replayed, replayProof, err := adapter.Attach(encoded, cfg, "rig:pilot")
	if err != nil || replayProof != proof || replayed.Revision != proof.ToRevision || store.transitionCalls != 1 {
		t.Fatalf("idempotent Attach() = row %+v proof %+v err %v calls %d", replayed, replayProof, err, store.transitionCalls)
	}
	revoked := cfg
	revoked.AdmissionV2Authorities = nil
	if _, _, err := adapter.Attach(encoded, revoked, "rig:pilot"); err == nil {
		t.Fatal("exact retry succeeded after the receipt signer was removed from current authority")
	}
	_ = private
}

func TestAdmissionAttachmentRecoversLostTransitionResponseFromExactReceipt(t *testing.T) {
	store, _, cfg, source, encoded := admissionAttachmentFixture(t)
	store.returnAfterCommitErr = errors.New("lost response")
	adapter, err := NewAdmissionAttachmentAdapter(store)
	if err != nil {
		t.Fatal(err)
	}
	attached, proof, err := adapter.Attach(encoded, cfg, "rig:pilot")
	if err != nil {
		t.Fatal(err)
	}
	if attached.Revision != proof.ToRevision || proof.FromRevision != source.Revision || store.transitionCalls != 1 {
		t.Fatalf("recovered attach row %+v proof %+v calls %d", attached, proof, store.transitionCalls)
	}
}

func TestAdmissionAttachmentTransitionVerifierAllowsOnlyExactQ54AdvancedSource(t *testing.T) {
	for _, state := range []string{"reserved", "attached"} {
		t.Run(state, func(t *testing.T) {
			store, _, cfg, source, encoded := admissionAttachmentFixture(t)
			adapter, err := NewAdmissionAttachmentAdapter(store)
			if err != nil {
				t.Fatal(err)
			}
			_, proof, err := adapter.Attach(encoded, cfg, "rig:pilot")
			if err != nil {
				t.Fatal(err)
			}
			marker := transitionMaterialization{
				Version: 1, State: state, Scope: proof.Scope, Contract: proof.ReceiptDigest,
				Route: "pilot/worker", Workflow: "review", MergeStrategy: "mr", Token: "stable-token",
				SourceID: source.ID, SourceStoreRef: "rig:pilot", WorkflowStoreRef: "rig:pilot",
				AdmissionReceipt: encoded,
			}
			metadata := map[string]string{beadmeta.LifecycleMaterializationMetadataKey: encodeAttachmentTestValue(t, marker)}
			if state == "attached" {
				marker.WorkflowID = "workflow-root"
				metadata[beadmeta.LifecycleMaterializationMetadataKey] = encodeAttachmentTestValue(t, marker)
				metadata[beadmeta.RoutedToMetadataKey] = marker.Route
				metadata[beadmeta.MoleculeIDMetadataKey] = marker.WorkflowID
				metadata[beadmeta.MergeStrategyMetadataKey] = marker.MergeStrategy
			}
			setAdmissionAttachmentSnapshotOverride(t, store, source.ID, metadata)
			if _, _, err := adapter.VerifyCurrent(source.ID, cfg, proof.Scope); err == nil {
				t.Fatal("strict VerifyCurrent accepted a source revision advanced by Q54")
			}
			current, got, err := adapter.VerifyForTransition(source.ID, cfg, proof.Scope)
			if err != nil || got != proof || current.Revision <= proof.ToRevision {
				t.Fatalf("VerifyForTransition() = row revision %d proof %+v err %v, want exact historical Q43 proof", current.Revision, got, err)
			}
		})
	}

	t.Run("advanced source without matching marker", func(t *testing.T) {
		store, _, cfg, source, encoded := admissionAttachmentFixture(t)
		adapter, err := NewAdmissionAttachmentAdapter(store)
		if err != nil {
			t.Fatal(err)
		}
		_, _, err = adapter.Attach(encoded, cfg, "rig:pilot")
		if err != nil {
			t.Fatal(err)
		}
		setAdmissionAttachmentSnapshotOverride(t, store, source.ID, nil)
		if _, _, err := adapter.VerifyForTransition(source.ID, cfg, "rig:pilot"); err == nil {
			t.Fatal("advanced source without Q54 marker verified")
		}
	})

	t.Run("advanced source with mismatched marker", func(t *testing.T) {
		store, _, cfg, source, encoded := admissionAttachmentFixture(t)
		adapter, err := NewAdmissionAttachmentAdapter(store)
		if err != nil {
			t.Fatal(err)
		}
		_, proof, err := adapter.Attach(encoded, cfg, "rig:pilot")
		if err != nil {
			t.Fatal(err)
		}
		marker := transitionMaterialization{
			Version: 1, State: "reserved", Scope: proof.Scope, Contract: "wrong-digest",
			Route: "pilot/worker", Workflow: "review", MergeStrategy: "mr", Token: "stable-token",
			SourceID: source.ID, SourceStoreRef: "rig:pilot", WorkflowStoreRef: "rig:pilot", AdmissionReceipt: encoded,
		}
		setAdmissionAttachmentSnapshotOverride(t, store, source.ID, map[string]string{
			beadmeta.LifecycleMaterializationMetadataKey: encodeAttachmentTestValue(t, marker),
		})
		if _, _, err := adapter.VerifyForTransition(source.ID, cfg, proof.Scope); err == nil {
			t.Fatal("advanced source with a mismatched Q54 marker verified")
		}
	})
}

func encodeAttachmentTestValue(t *testing.T, value any) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func setAdmissionAttachmentSnapshotOverride(t *testing.T, store *admissionAttachmentTestStore, sourceID string, metadata map[string]string) {
	t.Helper()
	current, err := store.MemStore.Get(sourceID)
	if err != nil {
		t.Fatal(err)
	}
	current.Revision++
	current.Metadata = cloneStringMap(current.Metadata)
	for key, value := range metadata {
		current.Metadata[key] = value
	}
	store.sourceOverride = &current
}

func TestAdmissionAttachmentRejectsStaleUnsupportedAndCorruptProofs(t *testing.T) {
	t.Run("stale source revision", func(t *testing.T) {
		store, _, cfg, source, encoded := admissionAttachmentFixture(t)
		if err := store.Update(source.ID, beads.UpdateOpts{Title: stringPtr("edited after review")}); err != nil {
			t.Fatal(err)
		}
		adapter, err := NewAdmissionAttachmentAdapter(store)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := adapter.Attach(encoded, cfg, "rig:pilot"); err == nil {
			t.Fatal("stale reviewed revision attached")
		}
		if store.transitionCalls != 0 {
			t.Fatalf("stale attach called Q43 %d times, want zero", store.transitionCalls)
		}
	})

	t.Run("unsupported store", func(t *testing.T) {
		store := beads.NewMemStore()
		if _, err := NewAdmissionAttachmentAdapter(store); err == nil {
			t.Fatal("adapter accepted a store without Q43 transition and receipt capabilities")
		}
	})

	t.Run("corrupt durable receipt", func(t *testing.T) {
		store, _, cfg, source, encoded := admissionAttachmentFixture(t)
		adapter, err := NewAdmissionAttachmentAdapter(store)
		if err != nil {
			t.Fatal(err)
		}
		_, proof, err := adapter.Attach(encoded, cfg, "rig:pilot")
		if err != nil {
			t.Fatal(err)
		}
		actual := store.receipts[proof.ReceiptID]
		actual.Scope = "rig:other"
		store.receipts[proof.ReceiptID] = actual
		if _, _, err := adapter.VerifyCurrent(source.ID, cfg, "rig:pilot"); err == nil {
			t.Fatal("corrupt durable Q43 receipt verified")
		}
	})

	t.Run("source changed after attach", func(t *testing.T) {
		store, _, cfg, source, encoded := admissionAttachmentFixture(t)
		adapter, err := NewAdmissionAttachmentAdapter(store)
		if err != nil {
			t.Fatal(err)
		}
		_, _, err = adapter.Attach(encoded, cfg, "rig:pilot")
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Update(source.ID, beads.UpdateOpts{Title: stringPtr("later edit")}); err != nil {
			t.Fatal(err)
		}
		if _, _, err := adapter.VerifyCurrent(source.ID, cfg, "rig:pilot"); err == nil {
			t.Fatal("attachment proof verified after source revision changed")
		}
		if _, _, err := adapter.Attach(encoded, cfg, "rig:pilot"); err == nil {
			t.Fatal("exact retry succeeded after the current source changed")
		}
	})

	t.Run("matching-looking response without durable receipt", func(t *testing.T) {
		store, _, cfg, _, encoded := admissionAttachmentFixture(t)
		store.returnWithoutPersistingReceipt = true
		adapter, err := NewAdmissionAttachmentAdapter(store)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := adapter.Attach(encoded, cfg, "rig:pilot"); err == nil {
			t.Fatal("matching-looking transition response without durable receipt was accepted")
		}
	})

	for _, tc := range []struct {
		name   string
		mutate func(*beads.ControllerMetadataTransitionReceipt)
	}{
		{name: "wrong value", mutate: func(receipt *beads.ControllerMetadataTransitionReceipt) {
			receipt.Value = json.RawMessage(`"different receipt"`)
		}},
		{name: "wrong receipt digest payload", mutate: func(receipt *beads.ControllerMetadataTransitionReceipt) {
			receipt.Payload = json.RawMessage(`{"schema_version":1,"receipt_digest":"wrong"}`)
		}},
		{name: "wrong from revision", mutate: func(receipt *beads.ControllerMetadataTransitionReceipt) { receipt.ExpectedVersion++ }},
		{name: "wrong to revision", mutate: func(receipt *beads.ControllerMetadataTransitionReceipt) { receipt.ToVersion++ }},
		{name: "wrong actor", mutate: func(receipt *beads.ControllerMetadataTransitionReceipt) { receipt.Actor = "worker" }},
		{name: "wrong key", mutate: func(receipt *beads.ControllerMetadataTransitionReceipt) { receipt.Key = "gc.other" }},
		{name: "wrong kind", mutate: func(receipt *beads.ControllerMetadataTransitionReceipt) { receipt.Kind = "other" }},
	} {
		t.Run("durable receipt "+tc.name, func(t *testing.T) {
			store, _, cfg, source, encoded := admissionAttachmentFixture(t)
			adapter, err := NewAdmissionAttachmentAdapter(store)
			if err != nil {
				t.Fatal(err)
			}
			_, proof, err := adapter.Attach(encoded, cfg, "rig:pilot")
			if err != nil {
				t.Fatal(err)
			}
			actual := store.receipts[proof.ReceiptID]
			tc.mutate(&actual)
			store.receipts[proof.ReceiptID] = actual
			if _, _, err := adapter.VerifyCurrent(source.ID, cfg, "rig:pilot"); err == nil {
				t.Fatalf("corrupt %s receipt verified", tc.name)
			}
		})
	}
}

func admissionAttachmentFixture(t *testing.T) (*admissionAttachmentTestStore, ed25519.PrivateKey, config.LifecycleConfig, beads.Bead, string) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	acceptancePublic, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cfg := config.LifecycleConfig{
		AdmissionEnabled:            true,
		AdmissionV2PrimaryAuthority: "triage",
		AdmissionV2Authorities:      map[string]string{"triage": base64.StdEncoding.EncodeToString(public)},
		AcceptanceAuthorities:       map[string]string{"reviewer": base64.StdEncoding.EncodeToString(acceptancePublic)},
	}
	store := &admissionAttachmentTestStore{MemStore: &beads.MemStore{IDPrefix: "work", HonorExplicitIDs: true}, receipts: map[string]beads.ControllerMetadataTransitionReceipt{}}
	source, err := store.Create(beads.Bead{ID: "work-1", Title: "reviewed title", Description: "reviewed detail", Type: "task", Status: "open", Labels: []string{AdmissionIntentLabel}})
	if err != nil {
		t.Fatal(err)
	}
	receipt := AdmissionReceiptV2{
		Version: 2, WorkItemID: source.ID, Scope: "rig:pilot", ExpectedWorkRevision: source.Revision,
		Route: "pilot/worker", Workflow: "review", RoutingPolicyDigest: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		MergeStrategy: "mr", Deliverable: "reviewed patch", Verification: "acceptance tests",
		AcceptanceAuthority: "reviewer", AdmittedBy: "triage",
	}
	encoded, err := SignAdmissionReceiptV2(receipt, private)
	if err != nil {
		t.Fatal(err)
	}
	return store, private, cfg, source, encoded
}

type admissionAttachmentTestStore struct {
	*beads.MemStore
	sourceOverride                 *beads.Bead
	receipts                       map[string]beads.ControllerMetadataTransitionReceipt
	lastRequest                    beads.ControllerMetadataTransitionRequest
	transitionCalls                int
	returnAfterCommitErr           error
	returnWithoutPersistingReceipt bool
}

var _ beads.DecisionFrontierSourceReaderHandleProvider = (*admissionAttachmentTestStore)(nil)
var _ beads.ControllerMetadataTransitionWriterHandleProvider = (*admissionAttachmentTestStore)(nil)
var _ beads.ControllerMetadataTransitionReceiptReaderHandleProvider = (*admissionAttachmentTestStore)(nil)

func (s *admissionAttachmentTestStore) DecisionFrontierSourceReaderHandle() (beads.DecisionFrontierSourceReader, bool) {
	return s, true
}

func (s *admissionAttachmentTestStore) DecisionFrontierSourceSnapshot(id string) (beads.Bead, error) {
	if s.sourceOverride != nil && s.sourceOverride.ID == id {
		current := *s.sourceOverride
		current.Metadata = cloneStringMap(current.Metadata)
		return current, nil
	}
	return s.MemStore.Get(id)
}

func (s *admissionAttachmentTestStore) ControllerMetadataTransitionWriterHandle() (beads.ControllerMetadataTransitionWriter, bool) {
	return s, true
}

func (s *admissionAttachmentTestStore) ControllerMetadataTransitionReceiptReaderHandle() (beads.ControllerMetadataTransitionReceiptReader, bool) {
	return s, true
}

func (s *admissionAttachmentTestStore) TransitionMetadata(issueID string, request beads.ControllerMetadataTransitionRequest) (beads.ControllerMetadataTransitionResult, error) {
	s.transitionCalls++
	s.lastRequest = request
	if prior, ok := s.receipts[request.ReceiptID]; ok {
		return beads.ControllerMetadataTransitionResult{Applied: true, Replayed: true, Receipt: &prior}, nil
	}
	current, err := s.MemStore.Get(issueID)
	if err != nil {
		return beads.ControllerMetadataTransitionResult{}, err
	}
	if request.ExpectedVersion != current.Revision || request.Expected != nil || request.Key != beadmeta.LifecycleAdmissionReceiptV2MetadataKey || current.Metadata[request.Key] != "" {
		return beads.ControllerMetadataTransitionResult{}, errors.New("transition compare failed")
	}
	var next string
	if request.Value == nil || json.Unmarshal(*request.Value, &next) != nil {
		return beads.ControllerMetadataTransitionResult{}, errors.New("invalid transition value")
	}
	if err := s.MemStore.UpdateIfMatch(issueID, request.ExpectedVersion, beads.UpdateOpts{Metadata: map[string]string{request.Key: next}}); err != nil {
		return beads.ControllerMetadataTransitionResult{}, err
	}
	committed, err := s.MemStore.Get(issueID)
	if err != nil {
		return beads.ControllerMetadataTransitionResult{}, err
	}
	receipt := beads.ControllerMetadataTransitionReceipt{
		ReceiptID: request.ReceiptID, IssueID: issueID, Scope: request.Scope, Kind: request.Kind, Actor: request.Actor,
		ExpectedVersion: request.ExpectedVersion, ToVersion: committed.Revision, Key: request.Key,
		Value: append(json.RawMessage(nil), (*request.Value)...), Payload: append(json.RawMessage(nil), request.Payload...),
	}
	if s.returnWithoutPersistingReceipt {
		return beads.ControllerMetadataTransitionResult{Applied: true, Receipt: &receipt}, nil
	}
	s.receipts[request.ReceiptID] = receipt
	if s.returnAfterCommitErr != nil {
		return beads.ControllerMetadataTransitionResult{}, s.returnAfterCommitErr
	}
	return beads.ControllerMetadataTransitionResult{Applied: true, Receipt: &receipt}, nil
}

func (s *admissionAttachmentTestStore) ControllerMetadataTransitionReceipt(issueID, receiptID string) (beads.ControllerMetadataTransitionReceipt, bool, error) {
	receipt, ok := s.receipts[receiptID]
	if !ok {
		return beads.ControllerMetadataTransitionReceipt{}, false, nil
	}
	if receipt.IssueID != issueID {
		return beads.ControllerMetadataTransitionReceipt{}, true, errors.New("receipt issue mismatch")
	}
	return receipt, true, nil
}

func stringPtr(value string) *string { return &value }
