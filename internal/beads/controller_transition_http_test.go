package beads

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type controllerTransitionTestTransport struct {
	handler         http.Handler
	dropFirstPost   bool
	dropPosts       map[int]bool
	postCount       int
	contextCount    int
	receiptGetCount int
	postBodies      [][]byte
}

func (transport *controllerTransitionTestTransport) RoundTrip(request *http.Request) (*http.Response, error) {
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
	case request.Method == http.MethodGet && strings.HasPrefix(request.URL.Path, controllerTransitionReceiptPath):
		transport.receiptGetCount++
	}
	recorder := httptest.NewRecorder()
	transport.handler.ServeHTTP(recorder, request)
	if request.Method == http.MethodPost && (transport.dropFirstPost && transport.postCount == 1 || transport.dropPosts[transport.postCount]) {
		return nil, errors.New("simulated lost controller response")
	}
	return recorder.Result(), nil
}

func controllerTransitionTestStore(t *testing.T, transport *controllerTransitionTestTransport) *BdStore {
	t.Helper()
	tokenPath := filepath.Join(t.TempDir(), "controller-token")
	if err := os.WriteFile(tokenPath, []byte("controller-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := NewBdStoreWithPrefix(t.TempDir(), func(_, _ string, _ ...string) ([]byte, error) {
		t.Fatal("controller transition HTTP path invoked bd command runner")
		return nil, nil
	}, "gc", WithBdStorePrivateEvidenceHTTP(PrivateEvidenceHTTPConfig{
		Endpoint: "http://127.0.0.1:1", ProjectID: "project-a", Database: "gc_fixture",
		ScopeRef: "rig:fixture", TokenFile: tokenPath, RevisionTransitions: true,
	}))
	store.privateEvidenceHTTP.client.Transport = transport
	return store
}

func controllerTransitionTestStoreWithEnabled(t *testing.T, transport *controllerTransitionTestTransport, enabled bool) *BdStore {
	t.Helper()
	store := controllerTransitionTestStore(t, transport)
	store.privateEvidenceHTTP.revisionTransitions = enabled
	return store
}

func controllerTransitionTestRequest() ControllerMetadataTransitionRequest {
	expected := json.RawMessage(`{"z":0,"a":1}`)
	value := json.RawMessage(`{"next":true}`)
	return ControllerMetadataTransitionRequest{
		ReceiptID: "receipt/1", Scope: "rig:fixture", Kind: "lease", Actor: "controller",
		ExpectedVersion: 7, Key: "gc.lease", Expected: &expected, Value: &value,
		Payload: json.RawMessage(`{"ticket":"private-marker"}`),
	}
}

func controllerTransitionTestHandler(receipt *json.RawMessage, missingReceipt bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer controller-secret" || r.Header.Get("Bd-Project-Id") != "project-a" {
			http.Error(w, "credential mismatch", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v0/beads/context":
			writeControllerTransitionTestContext(w, "project-a", "gc_fixture", []string{
				"issues.transitionMetadata", "issues.transitionReceipt.get", "project.enforce",
			})
		case r.Method == http.MethodPost && r.URL.EscapedPath() == "/v0/beads/issues/gc%2Fone:transitionMetadata":
			if receipt == nil {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			_, _ = w.Write(controllerTransitionAppliedResponse(*receipt))
		case r.Method == http.MethodGet && r.URL.EscapedPath() == controllerTransitionReceiptPath+"receipt%2F1":
			if missingReceipt || receipt == nil {
				w.Header().Set("Content-Type", "application/problem+json")
				w.WriteHeader(http.StatusNotFound)
				_, _ = io.WriteString(w, `{"status":404,"code":"not_found","title":"Not Found"}`)
				return
			}
			_, _ = w.Write(*receipt)
		default:
			http.NotFound(w, r)
		}
	}
}

func writeControllerTransitionTestContext(w http.ResponseWriter, project, database string, capabilities []string) {
	_ = json.NewEncoder(w).Encode(map[string]any{
		"api_version": "v0", "backend": "dolt", "bd_version": "1.3.0",
		"capabilities": capabilities, "database": database, "dolt_mode": "server", "project_id": project,
	})
}

func controllerTransitionTestReceipt(t *testing.T, request ControllerMetadataTransitionRequest, actor string) json.RawMessage {
	t.Helper()
	receipt := map[string]any{
		"receipt_id": request.ReceiptID, "issue_id": "gc/one", "scope": request.Scope,
		"kind": request.Kind, "actor": actor, "expected_version": "7", "to_version": "8",
		"key": request.Key, "expected": json.RawMessage(`{"a":1,"z":0}`),
		"value": json.RawMessage(`{"next":true}`), "payload": json.RawMessage(`{"ticket":"private-marker"}`),
	}
	data, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	return json.RawMessage(data)
}

func controllerTransitionAppliedResponse(receipt []byte) []byte {
	var out bytes.Buffer
	_, _ = out.WriteString(`{"applied":true,"replayed":false,"receipt":`)
	_, _ = out.Write(receipt)
	_, _ = out.WriteString(`}`)
	return out.Bytes()
}

func TestControllerMetadataTransitionHTTPPostsCanonicalWireAndExactReceipt(t *testing.T) {
	request := controllerTransitionTestRequest()
	receipt := controllerTransitionTestReceipt(t, request, "controller")
	transport := &controllerTransitionTestTransport{}
	transport.handler = controllerTransitionTestHandler(&receipt, false)
	store := controllerTransitionTestStore(t, transport)

	writer, ok := ControllerMetadataTransitionWriterFor(store)
	if !ok {
		t.Fatal("configured revision transition capability is unavailable")
	}
	result, err := writer.TransitionMetadata("gc/one", request)
	if err != nil {
		t.Fatalf("TransitionMetadata: %v", err)
	}
	if !result.Applied || result.Replayed || result.Receipt == nil || result.Receipt.ToVersion != 8 {
		t.Fatalf("transition result = %+v, want applied receipt at revision 8", result)
	}
	if transport.contextCount != 1 || transport.postCount != 1 || transport.receiptGetCount != 0 {
		t.Fatalf("requests: context=%d post=%d receipt-get=%d", transport.contextCount, transport.postCount, transport.receiptGetCount)
	}
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(transport.postBodies[0], &wire); err != nil {
		t.Fatalf("decode POST body: %v", err)
	}
	if string(wire["expected_version"]) != `"7"` || string(wire["expected"]) != `{"a":1,"z":0}` ||
		string(wire["payload"]) != `{"ticket":"private-marker"}` {
		t.Fatalf("POST body did not preserve canonical Q43 fields: %s", transport.postBodies[0])
	}
	wantBody := `{"receipt_id":"receipt/1","scope":"rig:fixture","kind":"lease","actor":"controller","expected_version":"7","key":"gc.lease","expected":{"a":1,"z":0},"value":{"next":true},"payload":{"ticket":"private-marker"}}`
	if string(transport.postBodies[0]) != wantBody {
		t.Fatalf("POST body = %s, want %s", transport.postBodies[0], wantBody)
	}
}

func TestControllerMetadataTransitionHTTPDefaultsDisabled(t *testing.T) {
	transport := &controllerTransitionTestTransport{handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("disabled transition transport sent a request")
	})}
	store := controllerTransitionTestStoreWithEnabled(t, transport, false)
	if writer, ok := ControllerMetadataTransitionWriterFor(store); ok || writer != nil {
		t.Fatal("disabled transition transport exposed a writer")
	}
	if _, err := store.TransitionMetadata("gc/one", controllerTransitionTestRequest()); !errors.Is(err, ErrControllerMetadataTransitionUnavailable) {
		t.Fatalf("TransitionMetadata error = %v, want unavailable", err)
	}
	if transport.contextCount != 0 || transport.postCount != 0 || transport.receiptGetCount != 0 {
		t.Fatalf("disabled transport sent requests: context=%d post=%d receipt=%d", transport.contextCount, transport.postCount, transport.receiptGetCount)
	}
}

func TestControllerMetadataTransitionHTTPRejectsWrongConfiguredScopeBeforeHandshake(t *testing.T) {
	transport := &controllerTransitionTestTransport{handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("wrong transition scope reached the server")
	})}
	store := controllerTransitionTestStore(t, transport)
	request := controllerTransitionTestRequest()
	request.Scope = "rig:other"
	if _, err := store.TransitionMetadata("gc/one", request); !errors.Is(err, ErrControllerMetadataTransitionProtocol) {
		t.Fatalf("TransitionMetadata error = %v, want local protocol refusal", err)
	}
	if transport.contextCount != 0 || transport.postCount != 0 || transport.receiptGetCount != 0 {
		t.Fatalf("wrong scope sent requests: context=%d post=%d receipt=%d", transport.contextCount, transport.postCount, transport.receiptGetCount)
	}
}

func TestControllerMetadataTransitionHTTPRecoversExactReceiptAfterLostPost(t *testing.T) {
	request := controllerTransitionTestRequest()
	receipt := controllerTransitionTestReceipt(t, request, "controller")
	transport := &controllerTransitionTestTransport{dropFirstPost: true}
	transport.handler = controllerTransitionTestHandler(&receipt, false)
	store := controllerTransitionTestStore(t, transport)

	result, err := store.TransitionMetadata("gc/one", request)
	if err != nil {
		t.Fatalf("TransitionMetadata: %v", err)
	}
	if !result.Applied || !result.Replayed || result.Receipt == nil || result.Receipt.ReceiptID != request.ReceiptID {
		t.Fatalf("recovered result = %+v, want exact applied receipt", result)
	}
	if transport.postCount != 1 || transport.receiptGetCount != 1 {
		t.Fatalf("requests: post=%d receipt-get=%d, want 1 each", transport.postCount, transport.receiptGetCount)
	}
}

func TestControllerMetadataTransitionHTTPRetriesOnlyAfterExactReceiptMiss(t *testing.T) {
	request := controllerTransitionTestRequest()
	receipt := controllerTransitionTestReceipt(t, request, "controller")
	transport := &controllerTransitionTestTransport{dropFirstPost: true}
	postAttempt := 0
	transport.handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			postAttempt++
			if postAttempt == 1 {
				if _, err := io.Copy(io.Discard, r.Body); err != nil {
					t.Errorf("read first POST body: %v", err)
				}
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			if _, err := io.Copy(io.Discard, r.Body); err != nil {
				t.Errorf("read retry POST body: %v", err)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(controllerTransitionAppliedResponse(receipt))
			return
		}
		controllerTransitionTestHandler(nil, true).ServeHTTP(w, r)
	})
	store := controllerTransitionTestStore(t, transport)
	result, err := store.TransitionMetadata("gc/one", request)
	if err != nil {
		t.Fatalf("TransitionMetadata: %v", err)
	}
	if !result.Applied || result.Receipt == nil || transport.postCount != 2 || transport.receiptGetCount != 1 {
		t.Fatalf("result=%+v requests post=%d receipt-get=%d, want one bounded retry", result, transport.postCount, transport.receiptGetCount)
	}
	if len(transport.postBodies) != 2 || !bytes.Equal(transport.postBodies[0], transport.postBodies[1]) {
		t.Fatal("retry did not reuse the exact original POST body")
	}
}

func TestControllerMetadataTransitionHTTPChecksReceiptAfterDefinitiveRetry(t *testing.T) {
	request := controllerTransitionTestRequest()
	receipt := controllerTransitionTestReceipt(t, request, "controller")
	transport := &controllerTransitionTestTransport{dropFirstPost: true}
	receiptReads := 0
	transport.handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v0/beads/context":
			writeControllerTransitionTestContext(w, "project-a", "gc_fixture", []string{
				"issues.transitionMetadata", "issues.transitionReceipt.get", "project.enforce",
			})
		case r.Method == http.MethodPost && transport.postCount == 1:
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"code":"internal"}`)
		case r.Method == http.MethodPost:
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusConflict)
			_, _ = io.WriteString(w, `{"code":"precondition_failed"}`)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, controllerTransitionReceiptPath):
			receiptReads++
			if receiptReads == 1 {
				w.Header().Set("Content-Type", "application/problem+json")
				w.WriteHeader(http.StatusNotFound)
				_, _ = io.WriteString(w, `{"code":"not_found"}`)
				return
			}
			_, _ = w.Write(receipt)
		default:
			http.NotFound(w, r)
		}
	})
	store := controllerTransitionTestStore(t, transport)
	result, err := store.TransitionMetadata("gc/one", request)
	if err != nil {
		t.Fatalf("TransitionMetadata: %v", err)
	}
	if !result.Applied || !result.Replayed || result.Receipt == nil || transport.postCount != 2 || receiptReads != 2 {
		t.Fatalf("result=%+v posts=%d receipt-reads=%d, want recovered first POST", result, transport.postCount, receiptReads)
	}
}

func TestControllerMetadataTransitionHTTPChecksReceiptAfterMarkerMissRetry(t *testing.T) {
	request := controllerTransitionTestRequest()
	receipt := controllerTransitionTestReceipt(t, request, "controller")
	transport := &controllerTransitionTestTransport{dropFirstPost: true}
	receiptReads := 0
	transport.handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v0/beads/context":
			writeControllerTransitionTestContext(w, "project-a", "gc_fixture", []string{
				"issues.transitionMetadata", "issues.transitionReceipt.get", "project.enforce",
			})
		case r.Method == http.MethodPost && transport.postCount == 1:
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"code":"internal"}`)
		case r.Method == http.MethodPost:
			_, _ = io.WriteString(w, `{"applied":false,"replayed":false,"current":null}`)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, controllerTransitionReceiptPath):
			receiptReads++
			if receiptReads == 1 {
				w.Header().Set("Content-Type", "application/problem+json")
				w.WriteHeader(http.StatusNotFound)
				_, _ = io.WriteString(w, `{"code":"not_found"}`)
				return
			}
			_, _ = w.Write(receipt)
		default:
			http.NotFound(w, r)
		}
	})
	store := controllerTransitionTestStore(t, transport)
	result, err := store.TransitionMetadata("gc/one", request)
	if err != nil {
		t.Fatalf("TransitionMetadata: %v", err)
	}
	if !result.Applied || !result.Replayed || result.Receipt == nil || transport.postCount != 2 || receiptReads != 2 {
		t.Fatalf("result=%+v posts=%d receipt-reads=%d, want recovered first POST", result, transport.postCount, receiptReads)
	}
}

func TestControllerMetadataTransitionHTTPRejectsMismatchedRecoveredReceipt(t *testing.T) {
	request := controllerTransitionTestRequest()
	receipt := controllerTransitionTestReceipt(t, request, "other-actor")
	transport := &controllerTransitionTestTransport{dropFirstPost: true}
	transport.handler = controllerTransitionTestHandler(&receipt, false)
	store := controllerTransitionTestStore(t, transport)

	_, err := store.TransitionMetadata("gc/one", request)
	if err == nil || !errors.Is(err, ErrControllerMetadataTransitionProtocol) {
		t.Fatalf("TransitionMetadata error = %v, want protocol refusal", err)
	}
	if strings.Contains(err.Error(), "private-marker") || strings.Contains(err.Error(), "controller-secret") || transport.postCount != 1 {
		t.Fatalf("recovery error leaked data or retried a conflicting receipt: %v posts=%d", err, transport.postCount)
	}
}

func TestControllerMetadataTransitionHTTPDoesNotRetryUnrecognizedReceiptMiss(t *testing.T) {
	transport := &controllerTransitionTestTransport{dropFirstPost: true}
	transport.handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v0/beads/context":
			writeControllerTransitionTestContext(w, "project-a", "gc_fixture", []string{
				"issues.transitionMetadata", "issues.transitionReceipt.get", "project.enforce",
			})
		case r.Method == http.MethodPost:
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"code":"internal"}`)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, controllerTransitionReceiptPath):
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"code":"wrong_not_found","detail":"private-marker"}`)
		default:
			http.NotFound(w, r)
		}
	})
	store := controllerTransitionTestStore(t, transport)
	result, err := store.TransitionMetadata("gc/one", controllerTransitionTestRequest())
	if err == nil || result.Applied || result.Receipt != nil {
		t.Fatalf("unrecognized receipt miss result = %+v, error %v; want no success claim", result, err)
	}
	if strings.Contains(err.Error(), "private-marker") || transport.postCount != 1 || transport.receiptGetCount != 1 {
		t.Fatalf("unrecognized receipt miss leaked data or retried: error=%v post=%d receipt=%d", err, transport.postCount, transport.receiptGetCount)
	}
}

func TestControllerMetadataTransitionHTTPWithholdsWriteWithoutReceiptCapability(t *testing.T) {
	transport := &controllerTransitionTestTransport{}
	transport.handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/v0/beads/context" {
			w.Header().Set("Content-Type", "application/json")
			writeControllerTransitionTestContext(w, "project-a", "gc_fixture", []string{"issues.transitionMetadata", "project.enforce"})
			return
		}
		http.Error(w, "unexpected request", http.StatusInternalServerError)
	})
	store := controllerTransitionTestStore(t, transport)
	if _, err := store.TransitionMetadata("gc/one", controllerTransitionTestRequest()); err == nil {
		t.Fatal("transition succeeded without the receipt capability")
	}
	if transport.postCount != 0 || transport.receiptGetCount != 0 {
		t.Fatalf("missing-capability handshake allowed writes or recovery: post=%d receipt-get=%d", transport.postCount, transport.receiptGetCount)
	}
}

func TestControllerMetadataTransitionHTTPWithholdsWriteOnHandshakeMismatch(t *testing.T) {
	tests := []struct {
		name         string
		project      string
		database     string
		capabilities []string
	}{
		{name: "wrong project", project: "project-b", database: "gc_fixture", capabilities: []string{"issues.transitionMetadata", "issues.transitionReceipt.get", "project.enforce"}},
		{name: "wrong database", project: "project-a", database: "other", capabilities: []string{"issues.transitionMetadata", "issues.transitionReceipt.get", "project.enforce"}},
		{name: "missing transition", project: "project-a", database: "gc_fixture", capabilities: []string{"issues.transitionReceipt.get", "project.enforce"}},
		{name: "missing project enforcement", project: "project-a", database: "gc_fixture", capabilities: []string{"issues.transitionMetadata", "issues.transitionReceipt.get"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			transport := &controllerTransitionTestTransport{}
			transport.handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.Path != "/v0/beads/context" {
					t.Fatalf("handshake refusal allowed request %s %s", r.Method, r.URL.Path)
				}
				w.Header().Set("Content-Type", "application/json")
				writeControllerTransitionTestContext(w, tc.project, tc.database, tc.capabilities)
			})
			store := controllerTransitionTestStore(t, transport)
			if _, err := store.TransitionMetadata("gc/one", controllerTransitionTestRequest()); err == nil {
				t.Fatal("transition succeeded after handshake mismatch")
			}
			if transport.postCount != 0 || transport.receiptGetCount != 0 {
				t.Fatalf("handshake mismatch allowed write/recovery: post=%d receipt=%d", transport.postCount, transport.receiptGetCount)
			}
		})
	}
}

func TestControllerMetadataTransitionHTTPUsesQ43ResponseBodyCaps(t *testing.T) {
	tests := []struct {
		name      string
		status    int
		bodyLimit int
	}{
		{name: "success", status: http.StatusOK, bodyLimit: controllerTransitionMaxSuccessBody},
		{name: "problem", status: http.StatusBadRequest, bodyLimit: controllerTransitionMaxProblemBody},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			transport := &controllerTransitionTestTransport{}
			transport.handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write(bytes.Repeat([]byte("x"), tc.bodyLimit+1))
			})
			store := controllerTransitionTestStore(t, transport)
			_, status, err := store.privateEvidenceHTTP.requestWithResponseCaps(context.Background(), http.MethodGet, "/body-cap", nil,
				controllerTransitionMaxSuccessBody, controllerTransitionMaxProblemBody)
			if !errors.Is(err, ErrPrivateEvidenceHTTPProtocol) || status != tc.status {
				t.Fatalf("bounded request = (status %d, error %v), want protocol refusal at status %d", status, err, tc.status)
			}
		})
	}
}

func TestControllerMetadataTransitionHTTPRefusesRedirectWithoutForwardingCredential(t *testing.T) {
	targetHits := 0
	targetAuthorization := ""
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetHits++
		targetAuthorization = r.Header.Get("Authorization")
		http.Error(w, "unexpected redirect target", http.StatusInternalServerError)
	}))
	defer target.Close()

	postHits := 0
	receiptHits := 0
	redirector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v0/beads/context":
			writeControllerTransitionTestContext(w, "project-a", "gc_fixture", []string{
				"issues.transitionMetadata", "issues.transitionReceipt.get", "project.enforce",
			})
		case r.Method == http.MethodPost:
			postHits++
			http.Redirect(w, r, target.URL+"/stolen", http.StatusTemporaryRedirect)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, controllerTransitionReceiptPath):
			receiptHits++
			http.NotFound(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	defer redirector.Close()

	tokenPath := filepath.Join(t.TempDir(), "controller-token")
	if err := os.WriteFile(tokenPath, []byte("controller-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := NewBdStoreWithPrefix(t.TempDir(), func(_, _ string, _ ...string) ([]byte, error) {
		t.Fatal("controller transition HTTP path invoked bd command runner")
		return nil, nil
	}, "gc", WithBdStorePrivateEvidenceHTTP(PrivateEvidenceHTTPConfig{
		Endpoint: redirector.URL, ProjectID: "project-a", Database: "gc_fixture",
		ScopeRef: "rig:fixture", TokenFile: tokenPath, RevisionTransitions: true,
	}))
	if _, err := store.TransitionMetadata("gc/one", controllerTransitionTestRequest()); !errors.Is(err, ErrControllerMetadataTransitionProtocol) {
		t.Fatalf("TransitionMetadata error = %v, want redirect refusal", err)
	}
	if postHits != 1 || receiptHits != 0 || targetHits != 0 || targetAuthorization != "" {
		t.Fatalf("redirect behavior: posts=%d receipts=%d target-hits=%d target-auth=%q", postHits, receiptHits, targetHits, targetAuthorization)
	}
}

func TestControllerMetadataTransitionHTTPDoesNotRecoverDefinitiveStatuses(t *testing.T) {
	for _, status := range []int{
		http.StatusBadRequest,
		http.StatusUnauthorized,
		http.StatusNotFound,
		http.StatusConflict,
		http.StatusPreconditionFailed,
		http.StatusNotImplemented,
	} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			transport := &controllerTransitionTestTransport{}
			transport.handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/problem+json")
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/v0/beads/context":
					w.Header().Set("Content-Type", "application/json")
					writeControllerTransitionTestContext(w, "project-a", "gc_fixture", []string{
						"issues.transitionMetadata", "issues.transitionReceipt.get", "project.enforce",
					})
				case r.Method == http.MethodPost:
					w.WriteHeader(status)
					_, _ = io.WriteString(w, `{"status":400,"code":"refused"}`)
				default:
					http.Error(w, "unexpected request", http.StatusInternalServerError)
				}
			})
			store := controllerTransitionTestStore(t, transport)
			_, err := store.TransitionMetadata("gc/one", controllerTransitionTestRequest())
			var problem *ControllerMetadataTransitionProblem
			if !errors.As(err, &problem) || problem.Status != status || problem.Code != "refused" {
				t.Fatalf("TransitionMetadata error = %v, want sanitized HTTP %d problem", err, status)
			}
			if transport.postCount != 1 || transport.receiptGetCount != 0 {
				t.Fatalf("definitive HTTP %d triggered recovery: post=%d receipt-get=%d", status, transport.postCount, transport.receiptGetCount)
			}
		})
	}
}

func TestControllerMetadataTransitionHTTPRejectsActorOver256BytesBeforeHandshake(t *testing.T) {
	transport := &controllerTransitionTestTransport{handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("invalid actor reached the server")
	})}
	store := controllerTransitionTestStore(t, transport)
	request := controllerTransitionTestRequest()
	request.Actor = strings.Repeat("é", 129)
	if _, err := store.TransitionMetadata("gc/one", request); !errors.Is(err, ErrControllerMetadataTransitionProtocol) {
		t.Fatalf("TransitionMetadata error = %v, want local protocol refusal", err)
	}
	if transport.contextCount != 0 || transport.postCount != 0 {
		t.Fatalf("invalid actor sent requests: context=%d post=%d", transport.contextCount, transport.postCount)
	}
}

func TestControllerMetadataTransitionHTTPRejectsUnicodeLineSeparatorsBeforeHandshake(t *testing.T) {
	tests := []struct {
		name    string
		issueID string
		actor   string
	}{
		{name: "issue line separator", issueID: "work\u2028forged", actor: "controller"},
		{name: "issue paragraph separator", issueID: "work\u2029forged", actor: "controller"},
		{name: "actor line separator", issueID: "work-1", actor: "controller\u2028forged"},
		{name: "actor paragraph separator", issueID: "work-1", actor: "controller\u2029forged"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			transport := &controllerTransitionTestTransport{handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
				t.Fatal("invalid text reached the server")
			})}
			store := controllerTransitionTestStore(t, transport)
			request := controllerTransitionTestRequest()
			request.Actor = tc.actor
			if _, err := store.TransitionMetadata(tc.issueID, request); !errors.Is(err, ErrControllerMetadataTransitionProtocol) {
				t.Fatalf("TransitionMetadata error = %v, want local protocol refusal", err)
			}
			if transport.contextCount != 0 || transport.postCount != 0 {
				t.Fatalf("invalid text sent requests: context=%d post=%d", transport.contextCount, transport.postCount)
			}
		})
	}
}

func TestControllerMetadataTransitionWirePreservesAbsentAndNull(t *testing.T) {
	nullValue := json.RawMessage(`null`)
	request := ControllerMetadataTransitionRequest{
		ReceiptID: "receipt-null", Scope: "rig:fixture", Kind: "lease", Actor: "controller",
		ExpectedVersion: -9, Key: "gc.lease", Value: &nullValue,
	}
	plan, err := planControllerMetadataTransition("work-1", request)
	if err != nil {
		t.Fatalf("planControllerMetadataTransition: %v", err)
	}
	body, err := encodeControllerMetadataTransition(plan)
	if err != nil {
		t.Fatalf("encodeControllerMetadataTransition: %v", err)
	}
	var members map[string]json.RawMessage
	if err := json.Unmarshal(body, &members); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	if _, present := members["expected"]; present {
		t.Fatalf("absent expected marker was serialized: %s", body)
	}
	if string(members["value"]) != "null" {
		t.Fatalf("explicit-null next marker = %s, want null", members["value"])
	}
	if _, present := members["payload"]; present {
		t.Fatalf("default payload was serialized: %s", body)
	}
	if string(plan.payload) != `{}` {
		t.Fatalf("bound default payload = %s, want {}", plan.payload)
	}
}

func TestControllerMetadataTransitionReceiptBindingCoversEveryRequestField(t *testing.T) {
	request := controllerTransitionTestRequest()
	plan, err := planControllerMetadataTransition("gc/one", request)
	if err != nil {
		t.Fatal(err)
	}
	baseRaw := controllerTransitionTestReceipt(t, request, "controller")
	base, err := decodeControllerMetadataTransitionReceipt(baseRaw)
	if err != nil || !controllerTransitionReceiptMatchesPlan(base, plan) {
		t.Fatalf("base receipt does not match: receipt=%+v err=%v", base, err)
	}
	changedJSON := json.RawMessage(`{"changed":true}`)
	tests := []struct {
		name   string
		mutate func(*ControllerMetadataTransitionReceipt)
	}{
		{"receipt ID", func(r *ControllerMetadataTransitionReceipt) { r.ReceiptID = "other" }},
		{"issue ID", func(r *ControllerMetadataTransitionReceipt) { r.IssueID = "other" }},
		{"scope", func(r *ControllerMetadataTransitionReceipt) { r.Scope = "rig:other" }},
		{"kind", func(r *ControllerMetadataTransitionReceipt) { r.Kind = "other" }},
		{"actor", func(r *ControllerMetadataTransitionReceipt) { r.Actor = "other" }},
		{"expected version", func(r *ControllerMetadataTransitionReceipt) { r.ExpectedVersion = 6 }},
		{"key", func(r *ControllerMetadataTransitionReceipt) { r.Key = "gc.other" }},
		{"expected", func(r *ControllerMetadataTransitionReceipt) { r.Expected = changedJSON }},
		{"value", func(r *ControllerMetadataTransitionReceipt) { r.Value = changedJSON }},
		{"payload", func(r *ControllerMetadataTransitionReceipt) { r.Payload = changedJSON }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			receipt := base
			receipt.Expected = append(json.RawMessage(nil), base.Expected...)
			receipt.Value = append(json.RawMessage(nil), base.Value...)
			receipt.Payload = append(json.RawMessage(nil), base.Payload...)
			tc.mutate(&receipt)
			if controllerTransitionReceiptMatchesPlan(receipt, plan) {
				t.Fatal("changed receipt remained bound to the request")
			}
		})
	}
}

func TestControllerMetadataTransitionHTTPTreatsFailedReceiptReadAsUnknown(t *testing.T) {
	transport := &controllerTransitionTestTransport{dropFirstPost: true}
	transport.handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v0/beads/context":
			writeControllerTransitionTestContext(w, "project-a", "gc_fixture", []string{
				"issues.transitionMetadata", "issues.transitionReceipt.get", "project.enforce",
			})
		case r.Method == http.MethodPost:
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"code":"internal"}`)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, controllerTransitionReceiptPath):
			w.Header().Set("Content-Type", "application/problem+json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"code":"db_unavailable","detail":"private-marker"}`)
		default:
			http.NotFound(w, r)
		}
	})
	store := controllerTransitionTestStore(t, transport)
	result, err := store.TransitionMetadata("gc/one", controllerTransitionTestRequest())
	if err == nil || result.Applied || result.Receipt != nil {
		t.Fatalf("unknown transition result = %+v, error %v; want no success claim", result, err)
	}
	if strings.Contains(err.Error(), "private-marker") || transport.postCount != 1 || transport.receiptGetCount != 1 {
		t.Fatalf("unknown result leaked data or retried: error=%v post=%d receipt=%d", err, transport.postCount, transport.receiptGetCount)
	}
}

func TestControllerMetadataTransitionHTTPStopsAfterLostRetryAndFailedFinalRecovery(t *testing.T) {
	transport := &controllerTransitionTestTransport{dropPosts: map[int]bool{1: true, 2: true}}
	receiptReads := 0
	transport.handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v0/beads/context":
			writeControllerTransitionTestContext(w, "project-a", "gc_fixture", []string{
				"issues.transitionMetadata", "issues.transitionReceipt.get", "project.enforce",
			})
		case r.Method == http.MethodPost:
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"code":"internal"}`)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, controllerTransitionReceiptPath):
			receiptReads++
			w.Header().Set("Content-Type", "application/problem+json")
			if receiptReads == 1 {
				w.WriteHeader(http.StatusNotFound)
				_, _ = io.WriteString(w, `{"code":"not_found"}`)
				return
			}
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"code":"db_unavailable","detail":"private-marker"}`)
		default:
			http.NotFound(w, r)
		}
	})
	store := controllerTransitionTestStore(t, transport)
	result, err := store.TransitionMetadata("gc/one", controllerTransitionTestRequest())
	if err == nil || result.Applied || result.Receipt != nil {
		t.Fatalf("lost retry result = %+v, error %v; want unknown outcome", result, err)
	}
	if strings.Contains(err.Error(), "private-marker") || transport.postCount != 2 || receiptReads != 2 {
		t.Fatalf("lost retry escaped bounds or leaked data: error=%v posts=%d receipts=%d", err, transport.postCount, receiptReads)
	}
}
