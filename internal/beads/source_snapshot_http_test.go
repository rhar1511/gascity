package beads

import (
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
	"time"
)

type sourceSnapshotHTTPTestTransport struct {
	handler       http.Handler
	contextCount  int
	snapshotCount int
	snapshotPath  string
}

func (transport *sourceSnapshotHTTPTestTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.Header.Get("Authorization") != "Bearer source-secret" || request.Header.Get("Bd-Project-Id") != "project-a" {
		return nil, errors.New("source snapshot test request omitted its configured identity")
	}
	switch {
	case request.Method == http.MethodGet && request.URL.Path == "/v0/beads/context":
		transport.contextCount++
	case request.Method == http.MethodGet && strings.HasSuffix(request.URL.Path, "/sourceSnapshot"):
		transport.snapshotCount++
		transport.snapshotPath = request.URL.EscapedPath()
	}
	recorder := httptest.NewRecorder()
	transport.handler.ServeHTTP(recorder, request)
	return recorder.Result(), nil
}

func sourceSnapshotHTTPTestStore(t *testing.T, transport *sourceSnapshotHTTPTestTransport, enabled bool) *BdStore {
	t.Helper()
	tokenPath := filepath.Join(t.TempDir(), "source-token")
	if err := os.WriteFile(tokenPath, []byte("source-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := NewBdStoreWithPrefix(t.TempDir(), func(_, _ string, _ ...string) ([]byte, error) {
		t.Fatal("source snapshot used the ordinary bd command path")
		return nil, nil
	}, "gc", WithBdStorePrivateEvidenceHTTP(PrivateEvidenceHTTPConfig{
		Endpoint: "http://127.0.0.1:1", ProjectID: "project-a", Database: "gc_fixture",
		ScopeRef: "rig:fixture", TokenFile: tokenPath, RevisionTransitions: enabled,
	}))
	if store.privateEvidenceHTTPInitErr != nil {
		t.Fatalf("initialize source snapshot HTTP store: %v", store.privateEvidenceHTTPInitErr)
	}
	store.privateEvidenceHTTP.client.Transport = transport
	return store
}

func sourceSnapshotHTTPTestContext(apiVersion, projectID, database, backend, doltMode string, capabilities []string) []byte {
	body, _ := json.Marshal(map[string]any{
		"api_version": apiVersion, "backend": backend, "bd_version": "1.3.0",
		"capabilities": capabilities, "database": database, "dolt_mode": doltMode, "project_id": projectID,
	})
	return body
}

func sourceSnapshotHTTPTestHandler(contextBody, snapshotBody []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v0/beads/context":
			_, _ = w.Write(contextBody)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/sourceSnapshot"):
			_, _ = w.Write(snapshotBody)
		default:
			http.NotFound(w, r)
		}
	}
}

func sourceSnapshotHTTPTestIssue(id, revision string) map[string]any {
	return map[string]any{
		"id": id, "title": "source title", "status": "open", "issue_type": "task",
		"priority": 2, "created_at": "2026-09-01T12:00:00Z", "updated_at": "2026-09-02T12:00:00Z",
		"assignee": "agent-a", "from": "operator", "parent": "gc-parent", "ref": "step-1",
		"needs": []string{"step-0"}, "description": "source detail", "labels": []string{"source", "decision"},
		"metadata": map[string]any{"custom": "value", "enabled": true, "count": 3},
		"revision": revision,
	}
}

func sourceSnapshotHTTPTestBody(t *testing.T, issue map[string]any, edges any) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{"issue": issue, "outgoing_dependencies": edges})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func TestBdStoreDecisionFrontierSourceSnapshotUsesBoundedHTTPAndExactEdges(t *testing.T) {
	issueID := "bd/one%two?x"
	issue := sourceSnapshotHTTPTestIssue(issueID, "-9223372036854775808")
	delete(issue, "parent")
	edges := []map[string]string{
		{"issue_id": issueID, "depends_on_id": "bd/target-a", "type": "blocks"},
		{"issue_id": issueID, "depends_on_id": "bd/target-b", "type": "tracks"},
		{"issue_id": issueID, "depends_on_id": "gc-parent", "type": "parent-child"},
	}
	contextBody := sourceSnapshotHTTPTestContext("v0", "project-a", "gc_fixture", "dolt", "server", []string{"issues.sourceSnapshot", "project.enforce"})
	transport := &sourceSnapshotHTTPTestTransport{handler: sourceSnapshotHTTPTestHandler(contextBody, sourceSnapshotHTTPTestBody(t, issue, edges))}
	store := sourceSnapshotHTTPTestStore(t, transport, true)

	reader, ok := DecisionFrontierSourceReaderFor(store)
	if !ok || reader == nil {
		t.Fatal("configured revision-transition store did not expose its source-snapshot reader")
	}
	got, err := reader.DecisionFrontierSourceSnapshot(issueID)
	if err != nil {
		t.Fatalf("DecisionFrontierSourceSnapshot: %v", err)
	}
	if got.ID != issueID || got.Title != "source title" || got.Type != "task" || got.Status != "open" || got.Revision != -1<<63 {
		t.Fatalf("source bead core fields = %+v", got)
	}
	if got.Assignee != "agent-a" || got.From != "operator" || got.ParentID != "gc-parent" || got.Ref != "step-1" ||
		got.Description != "source detail" || len(got.Needs) != 1 || got.Needs[0] != "step-0" {
		t.Fatalf("source bead detail fields = %+v", got)
	}
	if got.Priority == nil || *got.Priority != 2 || !got.CreatedAt.Equal(time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)) ||
		!got.UpdatedAt.Equal(time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("source bead priority/timestamps = %+v", got)
	}
	if len(got.Labels) != 2 || got.Labels[0] != "source" || got.Labels[1] != "decision" ||
		got.Metadata["custom"] != "value" || got.Metadata["enabled"] != "true" || got.Metadata["count"] != "3" {
		t.Fatalf("source bead labels/metadata = labels:%v metadata:%v", got.Labels, got.Metadata)
	}
	wantDeps := []Dep{
		{IssueID: issueID, DependsOnID: "bd/target-a", Type: "blocks"},
		{IssueID: issueID, DependsOnID: "bd/target-b", Type: "tracks"},
		{IssueID: issueID, DependsOnID: "gc-parent", Type: "parent-child"},
	}
	if fmt.Sprint(got.Dependencies) != fmt.Sprint(wantDeps) {
		t.Fatalf("source bead dependencies = %v, want exact outgoing edges %v", got.Dependencies, wantDeps)
	}
	if transport.contextCount != 1 || transport.snapshotCount != 1 {
		t.Fatalf("HTTP requests: context=%d sourceSnapshot=%d, want one each", transport.contextCount, transport.snapshotCount)
	}
	if wantPath := "/v0/beads/issues/bd%2Fone%25two%3Fx/sourceSnapshot"; transport.snapshotPath != wantPath {
		t.Fatalf("source snapshot path = %q, want %q", transport.snapshotPath, wantPath)
	}
}

func TestBdStoreDecisionFrontierSourceSnapshotRequiresExplicitCapabilityAndInitializedClient(t *testing.T) {
	transport := &sourceSnapshotHTTPTestTransport{handler: http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("unavailable source snapshot transport sent an HTTP request")
	})}
	store := sourceSnapshotHTTPTestStore(t, transport, false)
	if reader, ok := DecisionFrontierSourceReaderFor(store); ok || reader != nil {
		t.Fatal("revision-transition-disabled store exposed a source-snapshot reader")
	}

	badStore := NewBdStoreWithPrefix(t.TempDir(), nil, "gc", WithBdStorePrivateEvidenceHTTP(PrivateEvidenceHTTPConfig{
		Endpoint: "http://127.0.0.1:1", ProjectID: "project-a", Database: "gc_fixture",
		ScopeRef: "rig:fixture", TokenFile: filepath.Join(t.TempDir(), "missing-token"), RevisionTransitions: true,
	}))
	if reader, ok := DecisionFrontierSourceReaderFor(badStore); ok || reader != nil {
		t.Fatal("store with an uninitialized HTTP client exposed a source-snapshot reader")
	}
	if transport.contextCount != 0 || transport.snapshotCount != 0 {
		t.Fatalf("disabled/uninitialized capability sent requests: context=%d sourceSnapshot=%d", transport.contextCount, transport.snapshotCount)
	}
}

func TestBdStoreDecisionFrontierSourceSnapshotVerifiesExactContext(t *testing.T) {
	validCaps := []string{"issues.sourceSnapshot", "project.enforce"}
	cases := []struct {
		name         string
		apiVersion   string
		projectID    string
		database     string
		backend      string
		doltMode     string
		capabilities []string
	}{
		{name: "wrong API version", apiVersion: "v1", projectID: "project-a", database: "gc_fixture", backend: "dolt", doltMode: "server", capabilities: validCaps},
		{name: "wrong project", apiVersion: "v0", projectID: "project-b", database: "gc_fixture", backend: "dolt", doltMode: "server", capabilities: validCaps},
		{name: "wrong database", apiVersion: "v0", projectID: "project-a", database: "other", backend: "dolt", doltMode: "server", capabilities: validCaps},
		{name: "wrong backend", apiVersion: "v0", projectID: "project-a", database: "gc_fixture", backend: "sqlite", doltMode: "server", capabilities: validCaps},
		{name: "wrong Dolt mode", apiVersion: "v0", projectID: "project-a", database: "gc_fixture", backend: "dolt", doltMode: "embedded", capabilities: validCaps},
		{name: "missing source snapshot", apiVersion: "v0", projectID: "project-a", database: "gc_fixture", backend: "dolt", doltMode: "server", capabilities: []string{"project.enforce"}},
		{name: "missing project enforcement", apiVersion: "v0", projectID: "project-a", database: "gc_fixture", backend: "dolt", doltMode: "server", capabilities: []string{"issues.sourceSnapshot"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			contextBody := sourceSnapshotHTTPTestContext(tc.apiVersion, tc.projectID, tc.database, tc.backend, tc.doltMode, tc.capabilities)
			transport := &sourceSnapshotHTTPTestTransport{handler: sourceSnapshotHTTPTestHandler(contextBody, []byte(`{}`))}
			store := sourceSnapshotHTTPTestStore(t, transport, true)
			if _, err := store.DecisionFrontierSourceSnapshot("bd-1"); !errors.Is(err, ErrDecisionFrontierSourceSnapshotProtocol) &&
				!errors.Is(err, ErrPrivateEvidenceHTTPIdentity) {
				t.Fatalf("DecisionFrontierSourceSnapshot error = %v, want context refusal", err)
			}
			if transport.contextCount != 1 || transport.snapshotCount != 0 {
				t.Fatalf("context mismatch sent requests: context=%d sourceSnapshot=%d", transport.contextCount, transport.snapshotCount)
			}
		})
	}
}

func TestBdStoreDecisionFrontierSourceSnapshotRejectsIncompleteWireResponses(t *testing.T) {
	issueID := "bd-1"
	baseIssue := sourceSnapshotHTTPTestIssue(issueID, "-1")
	validEdges := []map[string]string{{"issue_id": issueID, "depends_on_id": "bd-2", "type": "blocks"}}
	issueBody := func(revision any, id string) []byte {
		issue := sourceSnapshotHTTPTestIssue(id, "-1")
		issue["revision"] = revision
		return sourceSnapshotHTTPTestBody(t, issue, validEdges)
	}
	validContext := sourceSnapshotHTTPTestContext("v0", "project-a", "gc_fixture", "dolt", "server", []string{"issues.sourceSnapshot", "project.enforce"})
	edgesJSON, _ := json.Marshal(validEdges)
	baseIssueJSON, _ := json.Marshal(baseIssue)
	validBody := []byte(`{"issue":` + string(baseIssueJSON) + `,"outgoing_dependencies":` + string(edgesJSON) + `}`)

	cases := []struct {
		name string
		body []byte
	}{
		{name: "mismatched issue ID", body: issueBody("-1", "bd-other")},
		{name: "revision missing", body: []byte(`{"issue":{"id":"bd-1"},"outgoing_dependencies":[]}`)},
		{name: "revision zero", body: issueBody("0", issueID)},
		{name: "revision plus sign", body: issueBody("+1", issueID)},
		{name: "revision leading zero", body: issueBody("-01", issueID)},
		{name: "revision number", body: []byte(strings.Replace(string(validBody), `"revision":"-1"`, `"revision":-1`, 1))},
		{name: "revision overflow", body: issueBody("9223372036854775808", issueID)},
		{name: "outgoing dependencies missing", body: []byte(`{"issue":{"id":"bd-1","revision":"-1"}}`)},
		{name: "outgoing dependencies null", body: []byte(`{"issue":{"id":"bd-1","revision":"-1"},"outgoing_dependencies":null}`)},
		{name: "outgoing dependencies object", body: []byte(`{"issue":{"id":"bd-1","revision":"-1"},"outgoing_dependencies":{}}`)},
		{name: "edge has wrong source", body: sourceSnapshotHTTPTestBody(t, baseIssue, []map[string]string{{"issue_id": "bd-other", "depends_on_id": "bd-2", "type": "blocks"}})},
		{name: "edge has empty target", body: sourceSnapshotHTTPTestBody(t, baseIssue, []map[string]string{{"issue_id": issueID, "depends_on_id": "", "type": "blocks"}})},
		{name: "edge has empty type", body: sourceSnapshotHTTPTestBody(t, baseIssue, []map[string]string{{"issue_id": issueID, "depends_on_id": "bd-2", "type": ""}})},
		{name: "edge is null", body: []byte(`{"issue":{"id":"bd-1","revision":"-1"},"outgoing_dependencies":[null]}`)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			transport := &sourceSnapshotHTTPTestTransport{handler: sourceSnapshotHTTPTestHandler(validContext, tc.body)}
			store := sourceSnapshotHTTPTestStore(t, transport, true)
			if _, err := store.DecisionFrontierSourceSnapshot(issueID); !errors.Is(err, ErrDecisionFrontierSourceSnapshotProtocol) {
				t.Fatalf("DecisionFrontierSourceSnapshot error = %v, want source-snapshot protocol error", err)
			}
		})
	}
}

func TestBdStoreDecisionFrontierSourceSnapshotMapsNotFound(t *testing.T) {
	contextBody := sourceSnapshotHTTPTestContext("v0", "project-a", "gc_fixture", "dolt", "server", []string{"issues.sourceSnapshot", "project.enforce"})
	transport := &sourceSnapshotHTTPTestTransport{handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v0/beads/context" {
			_, _ = w.Write(contextBody)
			return
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"status":404,"code":"not_found","title":"Not Found"}`)
	})}
	store := sourceSnapshotHTTPTestStore(t, transport, true)
	if _, err := store.DecisionFrontierSourceSnapshot("missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("DecisionFrontierSourceSnapshot error = %v, want ErrNotFound", err)
	}
}
