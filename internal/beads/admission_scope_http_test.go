package beads

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
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

func TestLifecycleReadScopeReadsMetadataReceiptsWithoutWriteAuthority(t *testing.T) {
	const scope = "city:alpha/rig:fixture"
	for _, kind := range []string{"lifecycle_admission_v2_attach", "lifecycle_descendant_anchor_v1"} {
		t.Run(kind, func(t *testing.T) {
			request := controllerTransitionTestRequest()
			request.Scope, request.Kind = scope, kind
			receipt := controllerTransitionTestReceipt(t, request, request.Actor)
			transport := &controllerTransitionTestTransport{handler: controllerTransitionTestHandler(&receipt, false)}
			store := controllerTransitionTestStore(t, transport)
			WithBdStorePrivateEvidenceLifecycleReadScope(scope)(store)
			reader, ok := ControllerMetadataTransitionReceiptReaderFor(store)
			if !ok {
				t.Fatal("read scope did not expose metadata receipts")
			}
			got, found, err := reader.ControllerMetadataTransitionReceipt("gc/one", request.ReceiptID)
			if err != nil || !found || got.Scope != scope || got.Kind != kind || got.ToVersion != 8 {
				t.Fatalf("metadata receipt = %+v, found=%v err=%v", got, found, err)
			}
			if _, err := store.TransitionMetadata("gc/one", request); err == nil {
				t.Fatal("read scope authorized a lifecycle metadata write")
			}
			for _, tc := range []struct{ name, scope, kind, issue, receiptID string }{
				{"foreign city", "city:other/rig:fixture", kind, "gc/one", request.ReceiptID},
				{"foreign store", "city:alpha/rig:other", kind, "gc/one", request.ReceiptID},
				{"unrelated kind", scope, "lease", "gc/one", request.ReceiptID},
				{"wrong issue", scope, kind, "gc/other", request.ReceiptID},
				{"wrong receipt", scope, kind, "gc/one", "other-receipt"},
			} {
				t.Run(tc.name, func(t *testing.T) {
					bad := request
					bad.Scope, bad.Kind, bad.ReceiptID = tc.scope, tc.kind, tc.receiptID
					receipt = controllerTransitionTestReceipt(t, bad, bad.Actor)
					if _, found, err := reader.ControllerMetadataTransitionReceipt(tc.issue, request.ReceiptID); found || !errors.Is(err, ErrDecisionFrontierTransitionReceiptCorrupt) {
						t.Fatalf("invalid receipt: found=%v err=%v", found, err)
					}
				})
			}
			if transport.postCount != 0 {
				t.Fatal("read-only lifecycle operations issued a mutation")
			}
		})
	}
}

func TestLifecycleReadScopeReadsPatchReceiptsWithoutWriteAuthority(t *testing.T) {
	for _, kind := range []string{"lifecycle_descendant_claim_identity_v1", "lifecycle_source_workspace_v1"} {
		t.Run(kind, func(t *testing.T) {
			const scope = "city:alpha/rig:fixture"
			request := controllerTransitionPatchTestRequest()
			request.Scope, request.Kind = scope, kind
			receipt := controllerTransitionPatchTestReceipt(t, request, request.Actor)
			transport := &controllerTransitionTestTransport{handler: controllerTransitionPatchTestHandler(receipt, http.StatusOK)}
			store := controllerTransitionTestStore(t, transport)
			WithBdStorePrivateEvidenceLifecycleReadScope(scope)(store)
			if writer, ok := RevisionTransitionPatchWriterFor(store); ok || writer != nil {
				t.Fatal("read scope exposed a protected patch writer")
			}
			if _, err := store.TransitionPatch("gc-1", request); !errors.Is(err, ErrRevisionTransitionPatchUnavailable) {
				t.Fatalf("direct patch write = %v, want unavailable", err)
			}
			reader, ok := RevisionTransitionPatchReceiptReaderFor(store)
			if !ok {
				t.Fatal("read scope did not expose patch receipts")
			}
			got, found, err := reader.ReadRevisionTransitionPatchReceipt(request.ReceiptID)
			if err != nil || !found || got.Scope != scope || got.Kind != request.Kind || got.ToVersion != 8 {
				t.Fatalf("patch receipt = %+v, found=%v err=%v", got, found, err)
			}
			for _, tc := range []struct{ name, scope, kind, receiptID string }{
				{"physical store only", "rig:fixture", "admission", request.ReceiptID},
				{"foreign city", "city:other/rig:fixture", request.Kind, request.ReceiptID},
				{"foreign store", "city:alpha/rig:other", request.Kind, request.ReceiptID},
				{"unrelated kind", scope, "unrelated_patch", request.ReceiptID},
				{"wrong receipt", scope, request.Kind, "other-receipt"},
			} {
				t.Run(tc.name, func(t *testing.T) {
					bad := request
					bad.Scope, bad.Kind, bad.ReceiptID = tc.scope, tc.kind, tc.receiptID
					transport.handler = controllerTransitionPatchTestHandler(controllerTransitionPatchTestReceipt(t, bad, bad.Actor), http.StatusOK)
					if _, found, err := reader.ReadRevisionTransitionPatchReceipt(request.ReceiptID); found || !errors.Is(err, ErrRevisionTransitionPatchProtocol) {
						t.Fatalf("invalid patch receipt: found=%v err=%v", found, err)
					}
				})
			}
			if transport.postCount != 0 {
				t.Fatal("read-only lifecycle operations issued a mutation")
			}
		})
	}
}

func TestLifecycleReadScopeRequiresConfiguredTransitionTransport(t *testing.T) {
	for _, tc := range []struct {
		name, scope string
		disabled    bool
	}{
		{"disabled transitions", "city:alpha/rig:fixture", true},
		{"wrong physical store", "city:alpha/rig:other", false},
		{"empty scope", "", false},
		{"malformed scope", "alpha/rig:fixture", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			transport := &controllerTransitionTestTransport{handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				t.Fatal("invalid read scope reached HTTP transport")
			})}
			store := controllerTransitionTestStoreWithEnabled(t, transport, !tc.disabled)
			WithBdStorePrivateEvidenceLifecycleReadScope(tc.scope)(store)
			if _, ok := ControllerMetadataTransitionReceiptReaderFor(store); ok {
				t.Fatal("invalid read scope exposed metadata receipt reader")
			}
			if _, ok := RevisionTransitionPatchReceiptReaderFor(store); ok {
				t.Fatal("invalid read scope exposed patch receipt reader")
			}
			if _, _, err := store.ReadRevisionTransitionPatchReceipt("patch/1"); !errors.Is(err, ErrRevisionTransitionPatchUnavailable) {
				t.Fatalf("invalid read scope patch error = %v", err)
			}
		})
	}
}

func TestNativeLifecycleReadScopePreservesAuthorityAndLifetime(t *testing.T) {
	const scope = "city:alpha/rig:fixture"
	t.Run("metadata receipt", func(t *testing.T) {
		request := controllerTransitionTestRequest()
		request.Scope, request.Kind = scope, privateEvidenceLifecycleAdmissionAttachmentKind
		receipt := controllerTransitionTestReceipt(t, request, request.Actor)
		transport := &controllerTransitionTestTransport{handler: controllerTransitionTestHandler(&receipt, false)}
		leaf := controllerTransitionTestStore(t, transport)
		WithBdStorePrivateEvidenceLifecycleReadScope(scope)(leaf)
		native := newNativeDoltStoreForTest(nil, WithNativeControllerTransport(leaf))
		reader, ok := ControllerMetadataTransitionReceiptReaderFor(native)
		if !ok {
			t.Fatal("native store lost its configured lifecycle receipt reader")
		}
		if _, found, err := reader.ControllerMetadataTransitionReceipt("gc/one", request.ReceiptID); err != nil || !found {
			t.Fatalf("read native lifecycle receipt: found=%v err=%v", found, err)
		}
		writer, ok := ControllerMetadataTransitionWriterFor(native)
		if !ok {
			t.Fatal("Q43 handle required by the attachment adapter is unavailable")
		}
		if _, err := writer.TransitionMetadata("gc/one", request); err == nil {
			t.Fatal("native receipt read scope authorized a lifecycle metadata write")
		}
		if transport.postCount != 0 {
			t.Fatal("native receipt scope submitted an HTTP mutation")
		}
		reads := transport.receiptGetCount
		if err := native.CloseStore(); err != nil {
			t.Fatal(err)
		}
		if _, _, err := reader.ControllerMetadataTransitionReceipt("gc/one", request.ReceiptID); !errors.Is(err, ErrStoreClosed) {
			t.Fatalf("retained metadata reader after close: %v", err)
		}
		if transport.receiptGetCount != reads {
			t.Fatal("retained reader reached HTTP after its native owner closed")
		}
	})
	t.Run("patch receipt", func(t *testing.T) {
		request := controllerTransitionPatchTestRequest()
		request.Scope, request.Kind = scope, "lifecycle_source_workspace_v1"
		receipt := controllerTransitionPatchTestReceipt(t, request, request.Actor)
		transport := &controllerTransitionTestTransport{handler: controllerTransitionPatchTestHandler(receipt, http.StatusOK)}
		leaf := controllerTransitionTestStore(t, transport)
		WithBdStorePrivateEvidenceLifecycleReadScope(scope)(leaf)
		native := newNativeDoltStoreForTest(nil, WithNativeControllerTransport(leaf))
		reader, ok := RevisionTransitionPatchReceiptReaderFor(native)
		if !ok {
			t.Fatal("native store lost its configured patch receipt reader")
		}
		if _, found, err := reader.ReadRevisionTransitionPatchReceipt(request.ReceiptID); err != nil || !found {
			t.Fatalf("read native patch receipt: found=%v err=%v", found, err)
		}
		if writer, ok := RevisionTransitionPatchWriterFor(native); ok || writer != nil {
			t.Fatal("native receipt read scope exposed a protected patch writer")
		}
		// A retained forwarding handle also rechecks the transport's authority.
		if _, err := (nativeControllerCapabilities{store: native}).TransitionPatch("gc-1", request); !errors.Is(err, ErrRevisionTransitionPatchUnavailable) {
			t.Fatalf("retained native patch write: %v; want unavailable", err)
		}
		if transport.postCount != 0 {
			t.Fatal("native receipt scope submitted an HTTP mutation")
		}
		if err := native.CloseStore(); err != nil {
			t.Fatal(err)
		}
		if _, _, err := reader.ReadRevisionTransitionPatchReceipt(request.ReceiptID); !errors.Is(err, ErrStoreClosed) {
			t.Fatalf("retained patch reader after close: %v", err)
		}
	})
}
