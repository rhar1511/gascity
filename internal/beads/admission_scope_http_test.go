package beads

import "testing"

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
