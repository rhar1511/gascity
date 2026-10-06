package beads

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type controllerTransitionPatchTestTransport struct {
	handler         http.Handler
	dropFirstPost   bool
	postCount       int
	contextCount    int
	receiptGetCount int
	postBodies      [][]byte
}

func (transport *controllerTransitionPatchTestTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	switch {
	case request.Method == http.MethodPost:
		body, err := io.ReadAll(request.Body)
		if err != nil {
			return nil, err
		}
		transport.postCount++
		transport.postBodies = append(transport.postBodies, append([]byte(nil), body...))
		request.Body = io.NopCloser(bytes.NewReader(body))
	case request.Method == http.MethodGet && request.URL.Path == "/v0/beads/context":
		transport.contextCount++
	case request.Method == http.MethodGet && strings.HasPrefix(request.URL.Path, transitionPatchReceiptPath):
		transport.receiptGetCount++
	}
	recorder := httptest.NewRecorder()
	transport.handler.ServeHTTP(recorder, request)
	if request.Method == http.MethodPost && transport.dropFirstPost && transport.postCount == 1 {
		return nil, errors.New("simulated lost transition response")
	}
	return recorder.Result(), nil
}

func controllerTransitionPatchTestStore(t *testing.T, transport *controllerTransitionPatchTestTransport) *BdStore {
	return controllerTransitionPatchTestStoreWithOptIn(t, transport, true)
}

func controllerTransitionPatchTestStoreWithOptIn(t *testing.T, transport *controllerTransitionPatchTestTransport, enabled bool) *BdStore {
	t.Helper()
	tokenPath := filepath.Join(t.TempDir(), "controller-token")
	if err := os.WriteFile(tokenPath, []byte("controller-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := NewBdStoreWithPrefix(t.TempDir(), func(_, _ string, _ ...string) ([]byte, error) {
		t.Fatal("transition-patch HTTP path invoked bd command runner")
		return nil, nil
	}, "gc", WithBdStorePrivateEvidenceHTTP(PrivateEvidenceHTTPConfig{
		Endpoint: "http://127.0.0.1:1", ProjectID: "project-a", Database: "gc_fixture",
		ScopeRef: "rig:fixture", TokenFile: tokenPath, RevisionTransitionPatches: enabled,
	}))
	store.privateEvidenceHTTP.client.Transport = transport
	return store
}

func controllerTransitionPatchTestStoreWithLifecycleScope(t *testing.T, transport *controllerTransitionPatchTestTransport, scope string) *BdStore {
	t.Helper()
	tokenPath := filepath.Join(t.TempDir(), "controller-token")
	if err := os.WriteFile(tokenPath, []byte("controller-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := NewBdStoreWithPrefix(t.TempDir(), func(_, _ string, _ ...string) ([]byte, error) {
		t.Fatal("transition-patch HTTP path invoked bd command runner")
		return nil, nil
	}, "gc", WithBdStorePrivateEvidenceHTTP(PrivateEvidenceHTTPConfig{
		Endpoint: "http://127.0.0.1:1", ProjectID: "project-a", Database: "gc_fixture",
		ScopeRef: "rig:fixture", LifecycleScope: scope, TokenFile: tokenPath, RevisionTransitions: true,
	}))
	store.privateEvidenceHTTP.client.Transport = transport
	return store
}

func controllerTransitionPatchTestRequest() RevisionTransitionPatchRequest {
	metadataExpected := json.RawMessage(`{"z":0,"a":1}`)
	metadataNext := json.RawMessage(`"worker"`)
	stepNext := json.RawMessage(`true`)
	return RevisionTransitionPatchRequest{
		ReceiptID: "patch/1", Scope: "rig:fixture", Kind: "admission", Actor: "controller",
		ExpectedVersion: 7, PriorReceiptID: "prior/1",
		PriorReceiptDigest: hex.EncodeToString(bytes.Repeat([]byte{0x42}, sha256.Size)),
		Patch: RevisionTransitionIssuePatch{
			Metadata: []RevisionTransitionMetadataPatch{
				{Key: "gc.route", Expected: &metadataExpected, Value: &metadataNext},
				{Key: "gc.step", Value: &stepNext},
			},
			Status:   &RevisionTransitionStringPatch{Expected: "open", Value: "in_progress"},
			Assignee: &RevisionTransitionStringPatch{Expected: "", Value: "worker-1"},
			Labels:   &RevisionTransitionLabelsPatch{Expected: []string{"z", "a"}, Value: []string{"worker", "ready"}},
		},
		ProtectedPermit: "signed-permit:opaque value",
	}
}

func controllerTransitionPatchTestReceipt(t *testing.T, request RevisionTransitionPatchRequest, actor string) []byte {
	t.Helper()
	metadataExpected := json.RawMessage(`{"a":1,"z":0}`)
	metadataNext := json.RawMessage(`"worker"`)
	stepNext := json.RawMessage(`true`)
	metadata := []RevisionTransitionMetadataPatch{
		{Key: "gc.route", Expected: &metadataExpected, Value: &metadataNext},
		{Key: "gc.step", Value: &stepNext},
	}
	patch := RevisionTransitionIssuePatch{
		Metadata: metadata,
		Status:   &RevisionTransitionStringPatch{Expected: "open", Value: "in_progress"},
		Assignee: &RevisionTransitionStringPatch{Expected: "", Value: "worker-1"},
		Labels:   &RevisionTransitionLabelsPatch{Expected: []string{"a", "z"}, Value: []string{"ready", "worker"}},
	}
	receipt := map[string]any{
		"receipt_id": request.ReceiptID, "issue_id": "gc-1", "scope": request.Scope,
		"kind": request.Kind, "actor": actor, "expected_version": "7", "to_version": "8",
		"prior_receipt_id": request.PriorReceiptID, "prior_receipt_digest": request.PriorReceiptDigest,
		"patch": patch,
	}
	data, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func controllerTransitionPatchTestHandler(receipt []byte, postStatus int) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer controller-secret" || r.Header.Get("Bd-Project-Id") != "project-a" {
			http.Error(w, "credential mismatch", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v0/beads/context":
			writeControllerTransitionTestContext(w, "project-a", "gc_fixture", []string{
				"issues.transitionPatch", "issues.transitionPatchReceipt.get", "issues.protectedMutation",
			})
		case r.Method == http.MethodPost && r.URL.EscapedPath() == "/v0/beads/issues/gc-1:transitionPatch":
			if postStatus != http.StatusOK {
				w.Header().Set("Content-Type", "application/problem+json")
				w.WriteHeader(postStatus)
				_, _ = io.WriteString(w, `{"status":409,"code":"transition_receipt_conflict","title":"Conflict"}`)
				return
			}
			_, _ = w.Write(controllerTransitionPatchAppliedResponse(receipt, false))
		case r.Method == http.MethodGet && r.URL.EscapedPath() == transitionPatchReceiptPath+"patch%2F1":
			if receipt == nil {
				w.Header().Set("Content-Type", "application/problem+json")
				w.WriteHeader(http.StatusNotFound)
				_, _ = io.WriteString(w, `{"status":404,"code":"not_found","title":"Not Found"}`)
				return
			}
			_, _ = w.Write(receipt)
		default:
			http.NotFound(w, r)
		}
	}
}

func controllerTransitionPatchAppliedResponse(receipt []byte, replayed bool) []byte {
	var out bytes.Buffer
	_, _ = out.WriteString(`{"applied":true,"replayed":`)
	if replayed {
		_, _ = out.WriteString("true")
	} else {
		_, _ = out.WriteString("false")
	}
	_, _ = out.WriteString(`,"receipt":`)
	_, _ = out.Write(receipt)
	_ = out.WriteByte('}')
	return out.Bytes()
}

func TestControllerTransitionPatchHTTPPostsCanonicalRequestAndReceipt(t *testing.T) {
	request := controllerTransitionPatchTestRequest()
	receipt := controllerTransitionPatchTestReceipt(t, request, request.Actor)
	transport := &controllerTransitionPatchTestTransport{handler: controllerTransitionPatchTestHandler(receipt, http.StatusOK)}
	store := controllerTransitionPatchTestStore(t, transport)
	writer, ok := RevisionTransitionPatchWriterFor(store)
	if !ok {
		t.Fatal("configured transition-patch capability is unavailable")
	}
	result, err := writer.TransitionPatch("gc-1", request)
	if err != nil {
		t.Fatalf("TransitionPatch: %v", err)
	}
	if !result.Applied || result.Replayed || result.Receipt == nil || result.Receipt.ToVersion != 8 {
		t.Fatalf("transition-patch result = %+v, want applied receipt at version 8", result)
	}
	if transport.postCount != 1 || transport.receiptGetCount != 0 {
		t.Fatalf("requests: post=%d receipt-get=%d", transport.postCount, transport.receiptGetCount)
	}
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(transport.postBodies[0], &wire); err != nil {
		t.Fatalf("decode POST body: %v", err)
	}
	if string(wire["expected_version"]) != `"7"` || string(wire["prior_receipt_digest"]) != `"`+request.PriorReceiptDigest+`"` ||
		string(wire["protected_permit"]) != `"signed-permit:opaque value"` {
		t.Fatalf("POST body omitted version, parent digest, or exact permit: %s", transport.postBodies[0])
	}
	var patch struct {
		Metadata []struct {
			Key string `json:"key"`
		} `json:"metadata"`
		Labels RevisionTransitionLabelsPatch `json:"labels"`
	}
	if err := json.Unmarshal(wire["patch"], &patch); err != nil {
		t.Fatalf("decode wire patch: %v", err)
	}
	if len(patch.Metadata) != 2 || patch.Metadata[0].Key != "gc.route" || patch.Metadata[1].Key != "gc.step" ||
		strings.Join(patch.Labels.Expected, ",") != "a,z" || strings.Join(patch.Labels.Value, ",") != "ready,worker" {
		t.Fatalf("wire patch is not canonical: %s", wire["patch"])
	}
}

func TestControllerTransitionPatchLifecycleScopeMatchesQ43ScopeAndRecovers(t *testing.T) {
	request := controllerTransitionPatchTestRequest()
	request.Scope = "city:alpha/rig:fixture"
	request.Kind = "lifecycle_source_reservation_v1"
	request.PriorReceiptID = "q43-attachment-receipt"
	receipt := controllerTransitionPatchTestReceipt(t, request, request.Actor)
	transport := &controllerTransitionPatchTestTransport{
		handler:       controllerTransitionPatchTestHandler(receipt, http.StatusOK),
		dropFirstPost: true,
	}
	store := controllerTransitionPatchTestStoreWithLifecycleScope(t, transport, request.Scope)
	writer, ok := RevisionTransitionPatchWriterFor(store)
	if !ok {
		t.Fatal("Q54 patch capability was not enabled by the trusted lifecycle scope")
	}
	result, err := writer.TransitionPatch("gc-1", request)
	if err != nil {
		t.Fatalf("same-scope Q54 reservation failed after lost response: %v", err)
	}
	if !result.Applied || !result.Replayed || result.Receipt == nil || result.Receipt.Scope != request.Scope {
		t.Fatalf("recovered Q54 result = %+v, want exact same-scope receipt", result)
	}
	if transport.postCount != 1 || transport.receiptGetCount != 1 {
		t.Fatalf("requests: post=%d receipt-get=%d, want one lost POST and exact readback", transport.postCount, transport.receiptGetCount)
	}
	var wire revisionTransitionPatchWireRequest
	if err := json.Unmarshal(transport.postBodies[0], &wire); err != nil {
		t.Fatalf("decode Q54 request: %v", err)
	}
	if wire.Scope != "city:alpha/rig:fixture" || wire.PriorReceiptID != "q43-attachment-receipt" {
		t.Fatalf("Q54 request scope/parent = %q/%q, want same Q43 lifecycle scope and parent", wire.Scope, wire.PriorReceiptID)
	}

	for _, scope := range []string{"city:beta/rig:fixture", "city:alpha/rig:other"} {
		bad := request
		bad.Scope = scope
		if _, err := writer.TransitionPatch("gc-1", bad); err == nil {
			t.Errorf("cross-city or arbitrary patch scope %q was accepted", scope)
		}
	}
	badKind := request
	badKind.Kind = "unrelated_patch"
	if _, err := writer.TransitionPatch("gc-1", badKind); err == nil {
		t.Error("unrelated patch kind was accepted at the lifecycle scope")
	}
	wrongScope := request
	wrongScope.Scope = "rig:fixture"
	if _, err := writer.TransitionPatch("gc-1", wrongScope); err == nil {
		t.Error("Q54 lifecycle patch was accepted at the store-identity scope")
	}
	if transport.postCount != 1 {
		t.Fatalf("rejected patch requests reached HTTP transport: POST count = %d", transport.postCount)
	}
}

func TestControllerTransitionPatchHTTPRecoversLostResponseByExactReceipt(t *testing.T) {
	request := controllerTransitionPatchTestRequest()
	receipt := controllerTransitionPatchTestReceipt(t, request, request.Actor)
	transport := &controllerTransitionPatchTestTransport{
		handler: controllerTransitionPatchTestHandler(receipt, http.StatusOK), dropFirstPost: true,
	}
	store := controllerTransitionPatchTestStore(t, transport)
	result, err := store.TransitionPatch("gc-1", request)
	if err != nil {
		t.Fatalf("TransitionPatch after lost response: %v", err)
	}
	if !result.Applied || !result.Replayed || result.Receipt == nil || result.Receipt.ToVersion != 8 {
		t.Fatalf("recovered result = %+v, want exact replayed receipt", result)
	}
	if transport.postCount != 1 || transport.receiptGetCount != 1 {
		t.Fatalf("lost-response recovery requests: post=%d receipt-get=%d", transport.postCount, transport.receiptGetCount)
	}
}

func TestControllerTransitionPatchHTTPMapsSameIDConflict(t *testing.T) {
	transport := &controllerTransitionPatchTestTransport{handler: controllerTransitionPatchTestHandler(nil, http.StatusConflict)}
	store := controllerTransitionPatchTestStore(t, transport)
	_, err := store.TransitionPatch("gc-1", controllerTransitionPatchTestRequest())
	if !errors.Is(err, ErrRevisionTransitionPatchReceiptConflict) {
		t.Fatalf("TransitionPatch error = %v, want same-ID receipt conflict", err)
	}
	if transport.postCount != 1 || transport.receiptGetCount != 0 {
		t.Fatalf("conflict must not retry or recover: post=%d receipt-get=%d", transport.postCount, transport.receiptGetCount)
	}
}

func TestControllerTransitionPatchHTTPRefusesMissingCapabilityAndInvalidRequest(t *testing.T) {
	transport := &controllerTransitionPatchTestTransport{handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v0/beads/context" {
			t.Fatalf("request escaped readiness gate: %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		writeControllerTransitionTestContext(w, "project-a", "gc_fixture", []string{"issues.transitionPatch", "issues.transitionPatchReceipt.get"})
	})}
	store := controllerTransitionPatchTestStore(t, transport)
	if _, err := store.TransitionPatch("gc-1", controllerTransitionPatchTestRequest()); !errors.Is(err, ErrRevisionTransitionPatchProtocol) {
		t.Fatalf("missing protected-mutation capability error = %v", err)
	}
	if transport.postCount != 0 {
		t.Fatalf("readiness refusal sent %d transition requests", transport.postCount)
	}
	badTransport := &controllerTransitionPatchTestTransport{handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("invalid request reached the server")
	})}
	badStore := controllerTransitionPatchTestStore(t, badTransport)
	bad := controllerTransitionPatchTestRequest()
	bad.Patch.Metadata = append(bad.Patch.Metadata, bad.Patch.Metadata[0])
	if _, err := badStore.TransitionPatch("gc-1", bad); !errors.Is(err, ErrRevisionTransitionPatchProtocol) {
		t.Fatalf("duplicate metadata patch error = %v", err)
	}
	if badTransport.contextCount != 0 || badTransport.postCount != 0 {
		t.Fatalf("invalid request reached readiness or mutation routes: context=%d post=%d", badTransport.contextCount, badTransport.postCount)
	}
}

func TestControllerTransitionPatchHTTPIsOptInAndRedactsProblemDetails(t *testing.T) {
	transport := &controllerTransitionPatchTestTransport{handler: http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		t.Fatalf("disabled patch capability made HTTP request: %s %s", r.Method, r.URL.Path)
	})}
	store := controllerTransitionPatchTestStoreWithOptIn(t, transport, false)
	if _, ok := RevisionTransitionPatchWriterFor(store); ok {
		t.Fatal("patch writer is available while its opt-in is disabled")
	}
	if _, err := store.TransitionPatch("gc-1", controllerTransitionPatchTestRequest()); !errors.Is(err, ErrRevisionTransitionPatchUnavailable) {
		t.Fatalf("disabled patch error = %v, want unavailable", err)
	}
	if transport.postCount != 0 || transport.contextCount != 0 {
		t.Fatalf("disabled capability sent requests: context=%d post=%d", transport.contextCount, transport.postCount)
	}
	protected := &controllerTransitionPatchTestTransport{handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v0/beads/context" {
			writeControllerTransitionTestContext(w, "project-a", "gc_fixture", []string{
				"issues.transitionPatch", "issues.transitionPatchReceipt.get", "issues.protectedMutation",
			})
			return
		}
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"status":403,"code":"protected_record","title":"sensitive permit material"}`)
	})}
	protectedStore := controllerTransitionPatchTestStore(t, protected)
	_, err := protectedStore.TransitionPatch("gc-1", controllerTransitionPatchTestRequest())
	if !errors.Is(err, ErrRevisionTransitionPatchProtected) || strings.Contains(fmt.Sprint(err), "sensitive permit material") {
		t.Fatalf("protected refusal error = %v, want typed redacted refusal", err)
	}
}

func TestControllerTransitionPatchReceiptReadBindsIDAndScope(t *testing.T) {
	request := controllerTransitionPatchTestRequest()
	receipt := controllerTransitionPatchTestReceipt(t, request, request.Actor)
	transport := &controllerTransitionPatchTestTransport{handler: controllerTransitionPatchTestHandler(receipt, http.StatusOK)}
	store := controllerTransitionPatchTestStore(t, transport)
	reader, ok := RevisionTransitionPatchReceiptReaderFor(store)
	if !ok {
		t.Fatal("configured transition-patch receipt reader is unavailable")
	}
	got, found, err := reader.ReadRevisionTransitionPatchReceipt(request.ReceiptID)
	if err != nil || !found || got.ReceiptID != request.ReceiptID || got.IssueID != "gc-1" {
		t.Fatalf("receipt read = (%+v, %t, %v)", got, found, err)
	}
}

func TestRevisionTransitionPatchProtectedMutationDigestMatchesBeadsGolden(t *testing.T) {
	metadataExpected := json.RawMessage(`{"z":0,"a":1}`)
	metadataNext := json.RawMessage(`"worker"`)
	stepNext := json.RawMessage(`true`)
	request := RevisionTransitionPatchRequest{
		ReceiptID: "patch/golden-1", Scope: "rig:golden", Kind: "admission", Actor: "controller",
		ExpectedVersion: 7, PriorReceiptID: "prior/golden-1",
		PriorReceiptDigest: "4242424242424242424242424242424242424242424242424242424242424242",
		Patch: RevisionTransitionIssuePatch{
			Metadata: []RevisionTransitionMetadataPatch{
				{Key: "gc.route", Expected: &metadataExpected, Value: &metadataNext},
				{Key: "gc.step", Value: &stepNext},
			},
			Status:   &RevisionTransitionStringPatch{Expected: "open", Value: "in_progress"},
			Assignee: &RevisionTransitionStringPatch{Expected: "", Value: "worker-1"},
			Labels:   &RevisionTransitionLabelsPatch{Expected: []string{"z", "a"}, Value: []string{"worker", "ready"}},
		},
	}
	digest, err := RevisionTransitionPatchProtectedMutationDigest("gc-42", request)
	if err != nil {
		t.Fatalf("protected mutation digest: %v", err)
	}
	if digest != "bc21c2612b04d72e3f9892f531e170562d48218097546a2c899e5ddf3fa7f97e" {
		t.Fatalf("digest = %s, want pinned Beads digest", digest)
	}
	if RevisionTransitionPatchProtectedMutationOperation != "issue.revision_transition_patch" {
		t.Fatalf("operation = %q, want the Beads protected patch operation", RevisionTransitionPatchProtectedMutationOperation)
	}
	request.ProtectedPermit = "opaque permit one"
	withPermit, err := RevisionTransitionPatchProtectedMutationDigest("gc-42", request)
	if err != nil || withPermit != digest {
		t.Fatalf("permit changed digest: got %q, err=%v; want %q", withPermit, err, digest)
	}
	if _, err := RevisionTransitionPatchProtectedMutationDigest("gc-43", request); err != nil {
		t.Fatalf("digest for second issue: %v", err)
	} else if changed, _ := RevisionTransitionPatchProtectedMutationDigest("gc-43", request); changed == digest {
		t.Fatal("digest did not bind URL issue ID")
	}
}
