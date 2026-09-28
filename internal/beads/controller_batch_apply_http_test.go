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
	"reflect"
	"strconv"
	"strings"
	"testing"
)

type controllerBatchApplyTestTransport struct {
	handler       http.Handler
	dropFirstPost bool
	postCount     int
	contextCount  int
	requestCount  int
	postBodies    [][]byte
}

func (transport *controllerBatchApplyTestTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	transport.requestCount++
	if request.Method == http.MethodPost {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			return nil, err
		}
		transport.postCount++
		transport.postBodies = append(transport.postBodies, append([]byte(nil), body...))
		request.Body = io.NopCloser(bytes.NewReader(body))
	}
	if request.Method == http.MethodGet && request.URL.Path == "/v0/beads/context" {
		transport.contextCount++
	}
	recorder := httptest.NewRecorder()
	transport.handler.ServeHTTP(recorder, request)
	if request.Method == http.MethodPost && transport.dropFirstPost && transport.postCount == 1 {
		return nil, errors.New("simulated lost batchApply response")
	}
	return recorder.Result(), nil
}

func controllerBatchApplyTestClient(t *testing.T, transport *controllerBatchApplyTestTransport) *controllerBatchApplyHTTPClient {
	t.Helper()
	tokenPath := filepath.Join(t.TempDir(), "controller-token")
	if err := os.WriteFile(tokenPath, []byte("controller-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	client, err := newControllerBatchApplyHTTPClient(ControllerBatchApplyHTTPConfig{
		Endpoint: "http://127.0.0.1:1", ProjectID: "project-a", Database: "gc_fixture", TokenFile: tokenPath,
	})
	if err != nil {
		t.Fatalf("new controller batchApply client: %v", err)
	}
	client.client.Transport = transport
	return client
}

func controllerBatchApplyTestRequest() ControllerProtectedCreateAndLinkRequest {
	return ControllerProtectedCreateAndLinkRequest{
		Actor: "gascity-controller",
		Record: ControllerProtectedRecord{
			ID: "gc-protected-1", Title: "Protected record", ProtectionClass: "gc-policy",
		},
		Links: []ControllerDependencyLink{
			{SourceID: "gc-protected-1", TargetID: "gc-map", Type: "relates-to"},
			{SourceID: "gc-prerequisite", TargetID: "gc-protected-1", Type: "blocks"},
		},
		ReceiptID:       "receipt-1",
		ProtectedPermit: "opaque-permit",
	}
}

func controllerBatchApplyTestCapabilities() []string {
	return []string{"issues.batchApply", "issues.batchApplyReceipt", "issues.protectedMutation", "project.enforce"}
}

func controllerBatchApplyTestHandler(project string, capabilities []string, batch http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer controller-secret" || r.Header.Get("Bd-Project-Id") != "project-a" {
			http.Error(w, "credential mismatch", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet && r.URL.Path == "/v0/beads/context" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"api_version": "v0", "backend": "dolt", "bd_version": "1.3.0",
				"capabilities": capabilities, "database": "gc_fixture", "dolt_mode": "server", "project_id": project,
			})
			return
		}
		if r.Method == http.MethodPost && r.URL.Path == controllerBatchApplyPath {
			batch(w, r)
			return
		}
		http.NotFound(w, r)
	})
}

func controllerBatchApplyTestResponse(request ControllerProtectedCreateAndLinkRequest, replayed bool) []byte {
	items := []map[string]any{{
		"kind": "create", "issue_id": request.Record.ID, "changed": true, "revision": "-3912",
	}}
	for _, link := range request.Links {
		items = append(items, map[string]any{
			"kind": "dep_add", "issue_id": link.SourceID, "depends_on_id": link.TargetID,
			"changed": true, "revision": "-9223372036854775808",
		})
	}
	body, err := json.Marshal(map[string]any{"keys": map[string]string{}, "items": items, "replayed": replayed})
	if err != nil {
		panic(err)
	}
	return body
}

func TestControllerBatchApplyHTTPPostsTypedCreateAndOrderedLinks(t *testing.T) {
	request := controllerBatchApplyTestRequest()
	transport := &controllerBatchApplyTestTransport{}
	transport.handler = controllerBatchApplyTestHandler("project-a", controllerBatchApplyTestCapabilities(), func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", r.Header.Get("Content-Type"))
		}
		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if len(body) != 4 || body["actor"] == nil || body["items"] == nil || body["receipt_id"] == nil || body["protected_permit"] == nil {
			t.Fatalf("request members = %v, want only actor/items/receipt_id/protected_permit", body)
		}
		var gotActor, gotReceipt, gotPermit string
		_ = json.Unmarshal(body["actor"], &gotActor)
		_ = json.Unmarshal(body["receipt_id"], &gotReceipt)
		_ = json.Unmarshal(body["protected_permit"], &gotPermit)
		if gotActor != request.Actor || gotReceipt != request.ReceiptID || gotPermit != request.ProtectedPermit {
			t.Fatalf("request identity = (%q, %q, %q), want (%q, %q, %q)", gotActor, gotReceipt, gotPermit, request.Actor, request.ReceiptID, request.ProtectedPermit)
		}
		var items []map[string]json.RawMessage
		if err := json.Unmarshal(body["items"], &items); err != nil {
			t.Fatalf("decode items: %v", err)
		}
		if len(items) != 1+len(request.Links) {
			t.Fatalf("item count = %d, want %d", len(items), 1+len(request.Links))
		}
		var create map[string]json.RawMessage
		if len(items[0]) != 2 || json.Unmarshal(items[0]["kind"], new(string)) != nil || json.Unmarshal(items[0]["create"], &create) != nil {
			t.Fatalf("create item = %v", items[0])
		}
		var createKind string
		_ = json.Unmarshal(items[0]["kind"], &createKind)
		if createKind != "create" || len(create) != 3 {
			t.Fatalf("create kind/payload = %q / %v", createKind, create)
		}
		var createID, title, protectionClass string
		_ = json.Unmarshal(create["id"], &createID)
		_ = json.Unmarshal(create["title"], &title)
		_ = json.Unmarshal(create["protection_class"], &protectionClass)
		if createID != request.Record.ID || title != request.Record.Title || protectionClass != request.Record.ProtectionClass {
			t.Fatalf("create = (%q, %q, %q), want (%q, %q, %q)", createID, title, protectionClass, request.Record.ID, request.Record.Title, request.Record.ProtectionClass)
		}
		for i, want := range request.Links {
			var item map[string]json.RawMessage
			if len(items[i+1]) != 2 || json.Unmarshal(items[i+1]["kind"], new(string)) != nil || json.Unmarshal(items[i+1]["dep_add"], &item) != nil {
				t.Fatalf("link item %d = %v", i, items[i+1])
			}
			var kind string
			_ = json.Unmarshal(items[i+1]["kind"], &kind)
			if kind != "dep_add" || len(item) != 3 {
				t.Fatalf("link item %d kind/payload = %q / %v", i, kind, item)
			}
			var source, target map[string]string
			_ = json.Unmarshal(item["source"], &source)
			_ = json.Unmarshal(item["target"], &target)
			var linkType string
			_ = json.Unmarshal(item["type"], &linkType)
			if !reflect.DeepEqual(source, map[string]string{"id": want.SourceID}) || !reflect.DeepEqual(target, map[string]string{"id": want.TargetID}) || linkType != want.Type {
				t.Fatalf("link item %d = (%v, %v, %q), want (%q, %q, %q)", i, source, target, linkType, want.SourceID, want.TargetID, want.Type)
			}
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(controllerBatchApplyTestResponse(request, true))
	})
	transport.dropFirstPost = false
	client := controllerBatchApplyTestClient(t, transport)

	result, err := client.ApplyProtectedCreateAndLink(context.Background(), request)
	if err != nil {
		t.Fatalf("ApplyProtectedCreateAndLink: %v", err)
	}
	if !result.Replayed {
		t.Fatal("Replayed = false, want true")
	}
	if transport.postCount != 1 || transport.contextCount != 1 {
		t.Fatalf("POST/context calls = %d/%d, want 1/1", transport.postCount, transport.contextCount)
	}
}

func TestControllerBatchApplyHTTPPreservesDependencyTypeWhitespace(t *testing.T) {
	request := controllerBatchApplyTestRequest()
	request.Links[0].Type = " custom-type "
	transport := &controllerBatchApplyTestTransport{}
	transport.handler = controllerBatchApplyTestHandler("project-a", controllerBatchApplyTestCapabilities(), func(w http.ResponseWriter, r *http.Request) {
		var body controllerBatchApplyWireRequest
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if got := body.Items[1].DepAdd.Type; got != request.Links[0].Type {
			t.Fatalf("dependency type = %q, want exact %q", got, request.Links[0].Type)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(controllerBatchApplyTestResponse(request, false))
	})
	client := controllerBatchApplyTestClient(t, transport)
	if _, err := client.ApplyProtectedCreateAndLink(context.Background(), request); err != nil {
		t.Fatalf("ApplyProtectedCreateAndLink: %v", err)
	}
}

func TestControllerBatchApplyHTTPRejectsProtectedCreateWithoutLinks(t *testing.T) {
	request := controllerBatchApplyTestRequest()
	request.Links = nil
	transport := &controllerBatchApplyTestTransport{}
	transport.handler = controllerBatchApplyTestHandler("project-a", controllerBatchApplyTestCapabilities(), func(_ http.ResponseWriter, _ *http.Request) {
		t.Fatal("create without a dependency link reached the server")
	})
	client := controllerBatchApplyTestClient(t, transport)
	if _, err := client.ApplyProtectedCreateAndLink(context.Background(), request); !errors.Is(err, ErrControllerBatchApplyProtocol) {
		t.Fatalf("ApplyProtectedCreateAndLink error = %v, want local protocol refusal", err)
	}
	if transport.contextCount != 0 || transport.postCount != 0 {
		t.Fatalf("HTTP calls = context %d, POST %d; want none", transport.contextCount, transport.postCount)
	}
}

func TestControllerBatchApplyHTTPAllowsNinetyNineLinks(t *testing.T) {
	request := controllerBatchApplyTestRequest()
	request.Links = make([]ControllerDependencyLink, controllerBatchApplyMaxItems-1)
	for i := range request.Links {
		request.Links[i] = ControllerDependencyLink{
			SourceID: request.Record.ID,
			TargetID: "gc-target-" + strconv.Itoa(i),
			Type:     "relates-to",
		}
	}
	transport := &controllerBatchApplyTestTransport{}
	transport.handler = controllerBatchApplyTestHandler("project-a", controllerBatchApplyTestCapabilities(), func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(controllerBatchApplyTestResponse(request, false))
	})
	client := controllerBatchApplyTestClient(t, transport)
	if _, err := client.ApplyProtectedCreateAndLink(context.Background(), request); err != nil {
		t.Fatalf("ApplyProtectedCreateAndLink: %v", err)
	}
	if transport.postCount != 1 {
		t.Fatalf("POST count = %d, want 1", transport.postCount)
	}
}

func TestControllerBatchApplyHTTPRequiresCapabilitiesBeforePOST(t *testing.T) {
	for _, tc := range []struct {
		name         string
		request      ControllerProtectedCreateAndLinkRequest
		capabilities []string
	}{
		{name: "batchApply", request: controllerBatchApplyTestRequest(), capabilities: []string{"issues.batchApplyReceipt", "issues.protectedMutation", "project.enforce"}},
		{name: "receipt", request: controllerBatchApplyTestRequest(), capabilities: []string{"issues.batchApply", "issues.protectedMutation", "project.enforce"}},
		{name: "protected mutation", request: controllerBatchApplyTestRequest(), capabilities: []string{"issues.batchApply", "issues.batchApplyReceipt", "project.enforce"}},
		{name: "project enforcement", request: controllerBatchApplyTestRequest(), capabilities: []string{"issues.batchApply", "issues.batchApplyReceipt", "issues.protectedMutation"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			transport := &controllerBatchApplyTestTransport{}
			transport.handler = controllerBatchApplyTestHandler("project-a", tc.capabilities, func(_ http.ResponseWriter, _ *http.Request) {
				t.Fatal("batchApply POST reached without required capabilities")
			})
			client := controllerBatchApplyTestClient(t, transport)
			if _, err := client.ApplyProtectedCreateAndLink(context.Background(), tc.request); !errors.Is(err, ErrControllerBatchApplyProtocol) {
				t.Fatalf("ApplyProtectedCreateAndLink error = %v, want protocol refusal", err)
			}
			if transport.postCount != 0 {
				t.Fatalf("POST count = %d, want 0", transport.postCount)
			}
		})
	}
}

func TestControllerBatchApplyHTTPRefusesWrongWorkspaceBeforePOST(t *testing.T) {
	transport := &controllerBatchApplyTestTransport{}
	transport.handler = controllerBatchApplyTestHandler("project-other", controllerBatchApplyTestCapabilities(), func(_ http.ResponseWriter, _ *http.Request) {
		t.Fatal("batchApply POST reached for another project")
	})
	client := controllerBatchApplyTestClient(t, transport)
	if _, err := client.ApplyProtectedCreateAndLink(context.Background(), controllerBatchApplyTestRequest()); !errors.Is(err, ErrControllerBatchApplyIdentity) {
		t.Fatalf("ApplyProtectedCreateAndLink error = %v, want identity refusal", err)
	}
	if transport.postCount != 0 {
		t.Fatalf("POST count = %d, want 0", transport.postCount)
	}
}

func TestControllerBatchApplyHTTPRecoversAmbiguousReceiptWithSamePOST(t *testing.T) {
	request := controllerBatchApplyTestRequest()
	transport := &controllerBatchApplyTestTransport{dropFirstPost: true}
	transport.handler = controllerBatchApplyTestHandler("project-a", controllerBatchApplyTestCapabilities(), func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		if transport.postCount == 1 {
			_, _ = w.Write(controllerBatchApplyTestResponse(request, false))
			return
		}
		_, _ = w.Write(controllerBatchApplyTestResponse(request, true))
	})
	client := controllerBatchApplyTestClient(t, transport)

	result, err := client.ApplyProtectedCreateAndLink(context.Background(), request)
	if err != nil {
		t.Fatalf("ApplyProtectedCreateAndLink: %v", err)
	}
	if !result.Replayed {
		t.Fatal("Replayed = false after receipt recovery, want true")
	}
	if transport.postCount != 2 || transport.contextCount != 2 {
		t.Fatalf("POST/context calls = %d/%d, want one bounded recovery POST and handshake", transport.postCount, transport.contextCount)
	}
	if !bytes.Equal(transport.postBodies[0], transport.postBodies[1]) {
		t.Fatalf("recovery body changed:\nfirst:  %s\nsecond: %s", transport.postBodies[0], transport.postBodies[1])
	}
}

func TestControllerBatchApplyHTTPRecovery4xxLeavesOutcomeUnknown(t *testing.T) {
	request := controllerBatchApplyTestRequest()
	transport := &controllerBatchApplyTestTransport{dropFirstPost: true}
	transport.handler = controllerBatchApplyTestHandler("project-a", controllerBatchApplyTestCapabilities(), func(w http.ResponseWriter, _ *http.Request) {
		if transport.postCount == 1 {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(controllerBatchApplyTestResponse(request, false))
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"status":401,"code":"unauthenticated"}`)
	})
	client := controllerBatchApplyTestClient(t, transport)

	_, err := client.ApplyProtectedCreateAndLink(context.Background(), request)
	if !errors.Is(err, ErrControllerBatchApplyOutcomeUnknown) {
		t.Fatalf("ApplyProtectedCreateAndLink error = %v, want unknown outcome", err)
	}
	var problem *ControllerBatchApplyProblem
	if !errors.As(err, &problem) || problem.Status != http.StatusUnauthorized || problem.Code != "unauthenticated" {
		t.Fatalf("ApplyProtectedCreateAndLink cause = %v, want preserved 401 problem", err)
	}
	if transport.postCount != 2 {
		t.Fatalf("POST count = %d, want one bounded recovery POST", transport.postCount)
	}
}

func TestControllerBatchApplyHTTPDoesNotRetryAmbiguousRequestWithoutReceipt(t *testing.T) {
	request := controllerBatchApplyTestRequest()
	request.ReceiptID = ""
	transport := &controllerBatchApplyTestTransport{dropFirstPost: true}
	transport.handler = controllerBatchApplyTestHandler("project-a", controllerBatchApplyTestCapabilities(), func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(controllerBatchApplyTestResponse(request, false))
	})
	client := controllerBatchApplyTestClient(t, transport)
	if _, err := client.ApplyProtectedCreateAndLink(context.Background(), request); !errors.Is(err, ErrControllerBatchApplyOutcomeUnknown) {
		t.Fatalf("ApplyProtectedCreateAndLink error = %v, want unknown outcome", err)
	}
	if transport.postCount != 1 {
		t.Fatalf("POST count = %d, want no unsafe retry", transport.postCount)
	}
}

func TestControllerBatchApplyHTTPReturnsSecretFreeProblem(t *testing.T) {
	request := controllerBatchApplyTestRequest()
	transport := &controllerBatchApplyTestTransport{}
	transport.handler = controllerBatchApplyTestHandler("project-a", controllerBatchApplyTestCapabilities(), func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusConflict)
		_, _ = io.WriteString(w, `{"status":409,"code":"protected_record","detail":"`+request.ProtectedPermit+`"}`)
	})
	client := controllerBatchApplyTestClient(t, transport)
	_, err := client.ApplyProtectedCreateAndLink(context.Background(), request)
	var problem *ControllerBatchApplyProblem
	if !errors.As(err, &problem) || problem.Status != http.StatusConflict || problem.Code != "protected_record" {
		t.Fatalf("ApplyProtectedCreateAndLink error = %v, want protected_record 409", err)
	}
	if strings.Contains(err.Error(), request.ProtectedPermit) {
		t.Fatalf("error leaked the permit: %v", err)
	}
	if transport.postCount != 1 {
		t.Fatalf("POST count = %d, want 1", transport.postCount)
	}
}

func TestControllerBatchApplyHTTPDoesNotRetryDefinitive4xxWithOversizedBody(t *testing.T) {
	transport := &controllerBatchApplyTestTransport{}
	transport.handler = controllerBatchApplyTestHandler("project-a", controllerBatchApplyTestCapabilities(), func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = io.WriteString(w, strings.Repeat("x", controllerBatchApplyMaxProblem+1))
	})
	client := controllerBatchApplyTestClient(t, transport)
	if _, err := client.ApplyProtectedCreateAndLink(context.Background(), controllerBatchApplyTestRequest()); !errors.Is(err, ErrControllerBatchApplyProtocol) {
		t.Fatalf("ApplyProtectedCreateAndLink error = %v, want bounded problem response error", err)
	}
	if transport.postCount != 1 {
		t.Fatalf("POST count = %d, want no retry after definitive 4xx", transport.postCount)
	}
}

func TestControllerBatchApplyHTTPRejectsMalformedRequestsBeforeHandshake(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*ControllerProtectedCreateAndLinkRequest)
	}{
		{name: "link omits protected record", mutate: func(request *ControllerProtectedCreateAndLinkRequest) { request.Links[0].SourceID = "gc-other" }},
		{name: "self link", mutate: func(request *ControllerProtectedCreateAndLinkRequest) { request.Links[0].TargetID = request.Record.ID }},
		{name: "over item limit", mutate: func(request *ControllerProtectedCreateAndLinkRequest) {
			request.Links = make([]ControllerDependencyLink, controllerBatchApplyMaxItems)
			for i := range request.Links {
				request.Links[i] = ControllerDependencyLink{SourceID: request.Record.ID, TargetID: "gc-target-" + strconv.Itoa(i), Type: "relates-to"}
			}
		}},
		{name: "empty permit", mutate: func(request *ControllerProtectedCreateAndLinkRequest) { request.ProtectedPermit = " " }},
		{name: "empty receipt", mutate: func(request *ControllerProtectedCreateAndLinkRequest) { request.ReceiptID = "\x00" }},
		{name: "class without permit", mutate: func(request *ControllerProtectedCreateAndLinkRequest) { request.ProtectedPermit = "" }},
		{name: "permit without class", mutate: func(request *ControllerProtectedCreateAndLinkRequest) { request.Record.ProtectionClass = "" }},
		{name: "overlong dependency type", mutate: func(request *ControllerProtectedCreateAndLinkRequest) {
			request.Links[0].Type = strings.Repeat("x", controllerBatchApplyMaxDepTypeBytes+1)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := controllerBatchApplyTestRequest()
			tc.mutate(&request)
			transport := &controllerBatchApplyTestTransport{}
			transport.handler = controllerBatchApplyTestHandler("project-a", controllerBatchApplyTestCapabilities(), func(_ http.ResponseWriter, _ *http.Request) {
				t.Fatal("invalid request reached the server")
			})
			client := controllerBatchApplyTestClient(t, transport)
			if _, err := client.ApplyProtectedCreateAndLink(context.Background(), request); !errors.Is(err, ErrControllerBatchApplyProtocol) {
				t.Fatalf("ApplyProtectedCreateAndLink error = %v, want local protocol refusal", err)
			}
			if transport.contextCount != 0 || transport.postCount != 0 {
				t.Fatalf("HTTP calls = context %d, POST %d; want none", transport.contextCount, transport.postCount)
			}
		})
	}
}

func TestControllerBatchApplyHTTPOptionalFields(t *testing.T) {
	request := controllerBatchApplyTestRequest()
	request.ReceiptID = ""
	request.ProtectedPermit = ""
	request.Record.ProtectionClass = ""
	transport := &controllerBatchApplyTestTransport{}
	transport.handler = controllerBatchApplyTestHandler("project-a", []string{"issues.batchApply", "project.enforce"}, func(w http.ResponseWriter, r *http.Request) {
		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if len(body) != 2 || body["receipt_id"] != nil || body["protected_permit"] != nil {
			t.Fatalf("request members = %v, want only actor/items", body)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(controllerBatchApplyTestResponse(request, false))
	})
	client := controllerBatchApplyTestClient(t, transport)
	result, err := client.ApplyProtectedCreateAndLink(context.Background(), request)
	if err != nil {
		t.Fatalf("ApplyProtectedCreateAndLink: %v", err)
	}
	if result.Replayed {
		t.Fatal("Replayed = true without a receipt, want false")
	}
	if transport.postCount != 1 {
		t.Fatalf("POST count = %d, want 1", transport.postCount)
	}
}

func TestControllerBatchApplyHTTPRejectsReplayWithoutReceipt(t *testing.T) {
	request := controllerBatchApplyTestRequest()
	request.ReceiptID = ""
	transport := &controllerBatchApplyTestTransport{}
	transport.handler = controllerBatchApplyTestHandler("project-a", controllerBatchApplyTestCapabilities(), func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(controllerBatchApplyTestResponse(request, true))
	})
	client := controllerBatchApplyTestClient(t, transport)
	_, err := client.ApplyProtectedCreateAndLink(context.Background(), request)
	if !errors.Is(err, ErrControllerBatchApplyOutcomeUnknown) || !errors.Is(err, ErrControllerBatchApplyProtocol) {
		t.Fatalf("ApplyProtectedCreateAndLink error = %v, want unknown outcome with protocol cause", err)
	}
	if transport.postCount != 1 {
		t.Fatalf("POST count = %d, want no unsafe retry", transport.postCount)
	}
}

func TestControllerBatchApplyHTTPRefusesRedirect(t *testing.T) {
	transport := &controllerBatchApplyTestTransport{}
	transport.handler = controllerBatchApplyTestHandler("project-a", controllerBatchApplyTestCapabilities(), func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/must-not-follow" {
			t.Fatal("HTTP client followed a redirect")
		}
		w.Header().Set("Location", "/must-not-follow")
		w.WriteHeader(http.StatusTemporaryRedirect)
	})
	client := controllerBatchApplyTestClient(t, transport)
	if _, err := client.ApplyProtectedCreateAndLink(context.Background(), controllerBatchApplyTestRequest()); !errors.Is(err, ErrControllerBatchApplyOutcomeUnknown) || !errors.Is(err, ErrControllerBatchApplyProtocol) {
		t.Fatalf("ApplyProtectedCreateAndLink error = %v, want unknown outcome with redirect cause", err)
	}
	if transport.postCount != 2 {
		t.Fatalf("POST count = %d, want one same-endpoint receipt recovery without following the redirect", transport.postCount)
	}
	if transport.requestCount != 4 {
		t.Fatalf("HTTP request count = %d, want two context handshakes and two POSTs only", transport.requestCount)
	}
	if !bytes.Equal(transport.postBodies[0], transport.postBodies[1]) {
		t.Fatalf("recovery body changed:\nfirst:  %s\nsecond: %s", transport.postBodies[0], transport.postBodies[1])
	}
}

func TestControllerBatchApplyHTTPRetriesMalformedSuccessOnlyWithReceipt(t *testing.T) {
	request := controllerBatchApplyTestRequest()
	transport := &controllerBatchApplyTestTransport{}
	transport.handler = controllerBatchApplyTestHandler("project-a", controllerBatchApplyTestCapabilities(), func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"keys":{},"items":[],"replayed":false,"unexpected":true}`)
	})
	client := controllerBatchApplyTestClient(t, transport)
	if _, err := client.ApplyProtectedCreateAndLink(context.Background(), request); !errors.Is(err, ErrControllerBatchApplyOutcomeUnknown) || !errors.Is(err, ErrControllerBatchApplyProtocol) {
		t.Fatalf("ApplyProtectedCreateAndLink error = %v, want unknown outcome with protocol cause after bounded replay", err)
	}
	if transport.postCount != 2 {
		t.Fatalf("POST count = %d, want one receipt recovery POST", transport.postCount)
	}
	if !bytes.Equal(transport.postBodies[0], transport.postBodies[1]) {
		t.Fatalf("recovery body changed:\nfirst:  %s\nsecond: %s", transport.postBodies[0], transport.postBodies[1])
	}
}
