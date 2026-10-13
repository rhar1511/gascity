package beads

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
)

func TestControllerReceiptReaderAcceptsAdmissionWithoutFrontierProjection(t *testing.T) {
	const scope = "city:alpha/rig:fixture"
	payload := json.RawMessage(`{"from_revision":7,"receipt_digest":"digest","schema_version":1,"scope":"city:alpha/rig:fixture","work_item_id":"gc/one"}`)
	value := json.RawMessage(`"signed receipt"`)
	wire, err := json.Marshal(map[string]any{
		"receipt_id": "receipt/1", "issue_id": "gc/one", "scope": scope,
		"kind": privateEvidenceLifecycleAdmissionAttachmentKind, "actor": "controller",
		"expected_version": "7", "to_version": "8",
		"key": beadmeta.LifecycleAdmissionReceiptV2MetadataKey, "value": value, "payload": payload,
	})
	if err != nil {
		t.Fatal(err)
	}
	receipt := json.RawMessage(wire)
	transport := &controllerTransitionTestTransport{handler: controllerTransitionTestHandler(&receipt, false)}
	store := controllerTransitionTestStore(t, transport)
	WithBdStorePrivateEvidenceAdmissionScope(scope)(store)
	reader, ok := ControllerMetadataTransitionReceiptReaderFor(store)
	if !ok {
		t.Fatal("configured store did not expose its complete receipt reader")
	}
	got, found, err := reader.ControllerMetadataTransitionReceipt("gc/one", "receipt/1")
	if err != nil || !found {
		t.Fatalf("read committed admission receipt: found=%v err=%v", found, err)
	}
	if got.ReceiptID != "receipt/1" || got.IssueID != "gc/one" || got.Scope != scope ||
		got.Kind != privateEvidenceLifecycleAdmissionAttachmentKind || got.Actor != "controller" ||
		got.ExpectedVersion != 7 || got.ToVersion != 8 || got.Key != beadmeta.LifecycleAdmissionReceiptV2MetadataKey ||
		len(got.Expected) != 0 || !bytes.Equal(got.Value, value) || !bytes.Equal(got.Payload, payload) {
		t.Fatalf("admission envelope changed on readback: %+v", got)
	}
	if _, found, err := store.DecisionFrontierRevisionTransitionReceipt("gc/one", "receipt/1"); found || !errors.Is(err, ErrDecisionFrontierTransitionReceiptCorrupt) {
		t.Fatalf("frontier reader accepted admission receipt: found=%v err=%v", found, err)
	}
	WithBdStorePrivateEvidenceAdmissionScope("city:other/rig:fixture")(store)
	if _, found, err := reader.ControllerMetadataTransitionReceipt("gc/one", "receipt/1"); found || !errors.Is(err, ErrDecisionFrontierTransitionReceiptCorrupt) {
		t.Fatalf("reader accepted admission from another scope: found=%v err=%v", found, err)
	}
	if transport.postCount != 0 {
		t.Fatalf("receipt lookup performed %d writes", transport.postCount)
	}
}

func TestAdmissionScopeAllowsReceiptAttachmentWithoutSourcePatchAuthority(t *testing.T) {
	request := controllerTransitionTestRequest()
	request.Scope = "city:alpha/rig:fixture"
	request.Kind = privateEvidenceLifecycleAdmissionAttachmentKind
	receipt := controllerTransitionTestReceipt(t, request, request.Actor)
	transport := &controllerTransitionTestTransport{handler: controllerTransitionTestHandler(&receipt, false)}
	store := controllerTransitionTestStore(t, transport)
	WithBdStorePrivateEvidenceAdmissionScope(request.Scope)(store)

	writer, ok := ControllerMetadataTransitionWriterFor(store)
	if !ok {
		t.Fatal("admission attachment capability unavailable")
	}
	if _, err := writer.TransitionMetadata("gc/one", request); err != nil {
		t.Fatalf("attach signed admission receipt: %v", err)
	}
	if _, ok := RevisionTransitionPatchWriterFor(store); ok {
		t.Fatal("operator admission scope granted protected source-patch capability")
	}
	for _, change := range []func(*ControllerMetadataTransitionRequest){
		func(r *ControllerMetadataTransitionRequest) { r.Scope = "city:other/rig:fixture" },
		func(r *ControllerMetadataTransitionRequest) { r.Scope = "city:alpha/rig:other" },
		func(r *ControllerMetadataTransitionRequest) { r.Scope = "rig:fixture" },
		func(r *ControllerMetadataTransitionRequest) { r.Kind = "lease" },
	} {
		bad := request
		change(&bad)
		if _, err := writer.TransitionMetadata("gc/one", bad); err == nil {
			t.Errorf("accepted unrelated scope/kind %q/%q", bad.Scope, bad.Kind)
		}
	}
	if transport.postCount != 1 {
		t.Fatalf("HTTP writes = %d, want only the exact admission attachment", transport.postCount)
	}
}
