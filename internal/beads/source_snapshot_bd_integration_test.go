//go:build integration && !windows

package beads

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

const bdSourceSnapshotIntegrationTokenPrefix = "source-snapshot-integration-"

func TestBdStoreDecisionFrontierSourceSnapshotExactBinaryIntegration(t *testing.T) {
	bdBinary := bdSourceSnapshotIntegrationBDPath(t)
	doltBinary := bdSourceSnapshotIntegrationDoltPath(t)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Minute)
	defer cancel()
	root := t.TempDir()
	home := filepath.Join(root, "home")
	doltRoot := filepath.Join(root, "dolt-root")
	doltData := filepath.Join(root, "dolt-data")
	workspace := filepath.Join(root, "workspace")
	xdgConfig := filepath.Join(home, "xdg-config")
	for _, dir := range []string{home, doltRoot, doltData, workspace, xdgConfig} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("create private integration directory %s: %v", dir, err)
		}
	}
	t.Setenv("GIT_DIR", filepath.Join(root, "ambient-git-dir-must-not-escape"))
	t.Setenv("GIT_WORK_TREE", filepath.Join(root, "ambient-git-work-tree-must-not-escape"))
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "user.name")
	t.Setenv("GIT_CONFIG_VALUE_0", "ambient config must not escape")
	env := bdSourceSnapshotIntegrationEnv(home, doltRoot)
	for _, item := range env {
		key, _, _ := strings.Cut(item, "=")
		if strings.HasPrefix(key, "GIT_") && key != "GIT_CONFIG_NOSYSTEM" {
			t.Fatalf("private integration environment retained ambient Git override %q", key)
		}
	}
	bdSourceSnapshotIntegrationRun(t, ctx, 30*time.Second, env, doltRoot, doltBinary,
		"config", "--global", "--add", "user.name", "Source snapshot integration")
	bdSourceSnapshotIntegrationRun(t, ctx, 30*time.Second, env, doltRoot, doltBinary,
		"config", "--global", "--add", "user.email", "source-snapshot@example.invalid")
	bdSourceSnapshotIntegrationRun(t, ctx, 2*time.Minute, env, doltData, doltBinary, "init")

	doltPort := bdSourceSnapshotIntegrationFreePort(t)
	dolt := bdSourceSnapshotIntegrationStartDolt(t, env, doltBinary, doltData, doltPort)
	bdSourceSnapshotIntegrationWaitForDolt(t, ctx, doltPort, dolt)

	database := "source_snapshot_" + bdSourceSnapshotIntegrationRandomHex(t, 8)
	bdSourceSnapshotIntegrationRun(t, ctx, 30*time.Second, env, workspace, "git", "init", "--quiet")
	bdSourceSnapshotIntegrationRun(t, ctx, 30*time.Second, env, workspace, "git", "config", "user.name", "Source snapshot integration")
	bdSourceSnapshotIntegrationRun(t, ctx, 30*time.Second, env, workspace, "git", "config", "user.email", "source-snapshot@example.invalid")
	bdSourceSnapshotIntegrationRun(t, ctx, 30*time.Second, env, workspace, "git", "config", "core.hooksPath", ".git/hooks")
	bdSourceSnapshotIntegrationRun(t, ctx, 4*time.Minute, env, workspace, bdBinary,
		"init", "--quiet", "--server", "--server-host", "127.0.0.1", "--server-port", strconv.Itoa(doltPort),
		"--database", database, "--prefix", "snap", "--non-interactive", "--skip-agents", "--skip-hooks")

	identity := bdSourceSnapshotIntegrationReadIdentity(t, workspace)
	if identity.ProjectID == "" {
		t.Fatal("bd init did not persist a project ID for the private workspace")
	}
	if identity.DoltDatabase != database {
		t.Fatalf("configured Dolt database = %q, want %q", identity.DoltDatabase, database)
	}
	token := bdSourceSnapshotIntegrationTokenPrefix + bdSourceSnapshotIntegrationRandomHex(t, 24)
	tokenFile := filepath.Join(root, "serve-token")
	if err := os.WriteFile(tokenFile, []byte(token+"\n"), 0o600); err != nil {
		t.Fatalf("write private serve token: %v", err)
	}

	serve := bdSourceSnapshotIntegrationStartServe(t, bdBinary, workspace, env, tokenFile)
	baseURL := bdSourceSnapshotIntegrationWaitForServe(t, ctx, serve, token, identity.ProjectID, database)
	store := NewBdStore(workspace, bdSourceSnapshotIntegrationRunner(ctx, bdBinary, env),
		WithBdStorePrivateEvidenceHTTP(PrivateEvidenceHTTPConfig{
			Endpoint: baseURL, ProjectID: identity.ProjectID, Database: database,
			ScopeRef: "rig:source-snapshot-integration", TokenFile: tokenFile, RevisionTransitions: true,
		}),
	)
	reader, ok := DecisionFrontierSourceReaderFor(store)
	if !ok || reader == nil {
		t.Fatal("BdStore with revision-transition private HTTP did not expose its source-snapshot reader")
	}
	writer, ok := ControllerMetadataTransitionWriterFor(store)
	if !ok || writer == nil {
		t.Fatal("BdStore with revision-transition private HTTP did not expose its controller metadata transition writer")
	}

	priority := 2
	const sourceTitle = "Exact binary source snapshot task"
	const sourceDescription = "Ordinary source task for the source-snapshot integration row."
	source, err := store.Create(Bead{
		Title: sourceTitle, Type: "task", Priority: &priority, Description: sourceDescription,
		Metadata: map[string]string{
			"integration.q43_once":     "before",
			"integration.q43_recovery": "before",
		},
	})
	if err != nil {
		t.Fatalf("create source task through exact bd binary: %v", err)
	}
	firstTarget, err := store.Create(Bead{Title: "Exact binary first target task", Type: "task"})
	if err != nil {
		t.Fatalf("create first target task through exact bd binary: %v", err)
	}
	secondTarget, err := store.Create(Bead{Title: "Exact binary second target task", Type: "task"})
	if err != nil {
		t.Fatalf("create second target task through exact bd binary: %v", err)
	}
	if source.ID == "" || firstTarget.ID == "" || secondTarget.ID == "" {
		t.Fatalf("bd returned empty task IDs: source=%q first=%q second=%q", source.ID, firstTarget.ID, secondTarget.ID)
	}
	if firstTarget.ID == secondTarget.ID || source.ID == firstTarget.ID || source.ID == secondTarget.ID {
		t.Fatalf("bd returned non-distinct task IDs: source=%q first=%q second=%q", source.ID, firstTarget.ID, secondTarget.ID)
	}
	if err := store.DepAdd(source.ID, firstTarget.ID, "blocks"); err != nil {
		t.Fatalf("add first source dependency through exact bd binary: %v", err)
	}
	if err := store.DepAdd(source.ID, secondTarget.ID, "related"); err != nil {
		t.Fatalf("add second source dependency through exact bd binary: %v", err)
	}

	wantEdges := map[string]string{firstTarget.ID: "blocks", secondTarget.ID: "related"}
	snapshot, err := reader.DecisionFrontierSourceSnapshot(source.ID)
	if err != nil {
		t.Fatalf("read authoritative source snapshot: %v", err)
	}
	bdSourceSnapshotIntegrationAssertCore(t, snapshot, source.ID, sourceTitle, sourceDescription, priority)
	gotEdges := bdSourceSnapshotIntegrationAssertEdges(t, snapshot.ID, snapshot.Dependencies, wantEdges)

	ordinary, err := store.Get(source.ID)
	if err != nil {
		t.Fatalf("read source task through BdStore.Get: %v", err)
	}
	if !bdSourceSnapshotIntegrationSameCore(snapshot, ordinary) {
		t.Fatalf("source snapshot core fields disagree with BdStore.Get:\n snapshot: %+v\n Get: %+v", snapshot, ordinary)
	}

	repeated, err := reader.DecisionFrontierSourceSnapshot(source.ID)
	if err != nil {
		t.Fatalf("repeat authoritative source snapshot read: %v", err)
	}
	if repeated.Revision != snapshot.Revision || !bdSourceSnapshotIntegrationSameCore(snapshot, repeated) {
		t.Fatalf("repeat source snapshot changed row or revision:\n first: %+v\n repeat: %+v", snapshot, repeated)
	}
	repeatedEdges := bdSourceSnapshotIntegrationAssertEdges(t, repeated.ID, repeated.Dependencies, wantEdges)
	if !bdSourceSnapshotIntegrationSameEdges(gotEdges, repeatedEdges) {
		t.Fatalf("repeat source snapshot edge set changed: first=%v repeat=%v", gotEdges, repeatedEdges)
	}
	if snapshot.Metadata["integration.q43_once"] != "before" || snapshot.Metadata["integration.q43_recovery"] != "before" {
		t.Fatalf("source snapshot metadata baseline = %v, want both disposable Q43 markers set to before", snapshot.Metadata)
	}

	const transitionScope = "rig:source-snapshot-integration"
	const transitionKind = "integration-metadata-transition"
	const transitionActor = "gascity-controller-integration"
	firstExpected := json.RawMessage(`"before"`)
	firstValue := json.RawMessage(`"after"`)
	firstRequest := ControllerMetadataTransitionRequest{
		ReceiptID: "q43-exact-binary-" + bdSourceSnapshotIntegrationRandomHex(t, 12),
		Scope:     transitionScope, Kind: transitionKind, Actor: transitionActor,
		ExpectedVersion: snapshot.Revision, Key: "integration.q43_once",
		Expected: &firstExpected, Value: &firstValue,
		Payload: json.RawMessage(`{"purpose":"exact-binary-integration","sequence":1}`),
	}
	priorTransport := store.privateEvidenceHTTP.client.Transport
	if priorTransport == nil {
		priorTransport = http.DefaultTransport
	}
	capturePosts := &bdSourceSnapshotIntegrationCaptureTransitionTransport{
		next:           priorTransport,
		transitionPath: controllerTransitionPath + url.PathEscape(source.ID) + ":transitionMetadata",
	}
	store.privateEvidenceHTTP.client.Transport = capturePosts
	t.Cleanup(func() {
		if store.privateEvidenceHTTP.client.Transport == capturePosts {
			store.privateEvidenceHTTP.client.Transport = priorTransport
		}
	})
	firstResult, err := writer.TransitionMetadata(source.ID, firstRequest)
	if err != nil {
		t.Fatalf("apply controller metadata transition through exact bd server: %v", err)
	}
	if !firstResult.Applied || firstResult.Replayed {
		t.Fatalf("first transition result = %+v, want applied and not replayed", firstResult)
	}
	bdSourceSnapshotIntegrationAssertTransitionReceipt(t, firstResult.Receipt, source.ID, firstRequest)

	afterFirst, err := reader.DecisionFrontierSourceSnapshot(source.ID)
	if err != nil {
		t.Fatalf("read source snapshot after first metadata transition: %v", err)
	}
	bdSourceSnapshotIntegrationAssertTransitionEffect(t, afterFirst, snapshot, firstResult.Receipt, firstRequest, "after")

	replayed, err := writer.TransitionMetadata(source.ID, firstRequest)
	if err != nil {
		t.Fatalf("replay exact controller metadata transition: %v", err)
	}
	if !replayed.Applied || !replayed.Replayed {
		t.Fatalf("exact replay result = %+v, want applied and replayed", replayed)
	}
	bdSourceSnapshotIntegrationAssertTransitionReceipt(t, replayed.Receipt, source.ID, firstRequest)
	store.privateEvidenceHTTP.client.Transport = priorTransport
	capturedPosts := capturePosts.snapshotRequests()
	if len(capturedPosts) != 2 {
		t.Fatalf("captured %d controller transition POSTs, want initial apply and one exact replay", len(capturedPosts))
	}
	wantPath := controllerTransitionPath + url.PathEscape(source.ID) + ":transitionMetadata"
	for index, captured := range capturedPosts {
		bdSourceSnapshotIntegrationAssertTransitionRequest(t, captured, wantPath, token, identity.ProjectID, firstRequest)
		if index > 0 && !bytes.Equal(capturedPosts[0].body, captured.body) {
			t.Fatalf("exact replay POST body differs from initial POST:\n first: %s\n replay: %s", capturedPosts[0].body, captured.body)
		}
	}
	afterReplay, err := reader.DecisionFrontierSourceSnapshot(source.ID)
	if err != nil {
		t.Fatalf("read source snapshot after exact replay: %v", err)
	}
	if afterReplay.Revision != afterFirst.Revision || afterReplay.Revision != replayed.Receipt.ToVersion ||
		afterReplay.Metadata[firstRequest.Key] != "after" ||
		afterReplay.Metadata["integration.q43_recovery"] != "before" {
		t.Fatalf("exact replay changed the source row: after first=%+v after replay=%+v", afterFirst, afterReplay)
	}

	recoveryExpected := json.RawMessage(`"before"`)
	recoveryValue := json.RawMessage(`"recovered"`)
	recoveryRequest := ControllerMetadataTransitionRequest{
		ReceiptID: "q43-lost-post-" + bdSourceSnapshotIntegrationRandomHex(t, 12),
		Scope:     transitionScope, Kind: transitionKind, Actor: transitionActor,
		ExpectedVersion: afterReplay.Revision, Key: "integration.q43_recovery",
		Expected: &recoveryExpected, Value: &recoveryValue,
		Payload: json.RawMessage(`{"purpose":"exact-binary-integration","sequence":2}`),
	}
	lostPost := &bdSourceSnapshotIntegrationLostPostTransport{
		next:           priorTransport,
		transitionPath: controllerTransitionPath + url.PathEscape(source.ID) + ":transitionMetadata",
	}
	store.privateEvidenceHTTP.client.Transport = lostPost
	t.Cleanup(func() { store.privateEvidenceHTTP.client.Transport = priorTransport })
	recovered, err := writer.TransitionMetadata(source.ID, recoveryRequest)
	if err != nil {
		t.Fatalf("recover controller metadata transition after lost successful POST response: %v", err)
	}
	if !recovered.Applied || !recovered.Replayed {
		t.Fatalf("lost-response recovery result = %+v, want applied and replayed", recovered)
	}
	bdSourceSnapshotIntegrationAssertTransitionReceipt(t, recovered.Receipt, source.ID, recoveryRequest)
	if dropped, posts, receiptGets := lostPost.counts(); !dropped || posts != 1 || receiptGets != 1 {
		t.Fatalf("lost-response recovery transport: dropped=%v transition-posts=%d receipt-gets=%d, want one dropped successful POST and one receipt lookup",
			dropped, posts, receiptGets)
	}
	afterRecovery, err := reader.DecisionFrontierSourceSnapshot(source.ID)
	if err != nil {
		t.Fatalf("read source snapshot after lost-response recovery: %v", err)
	}
	bdSourceSnapshotIntegrationAssertTransitionEffect(t, afterRecovery, afterReplay, recovered.Receipt, recoveryRequest, "recovered")
	if afterRecovery.Metadata[firstRequest.Key] != "after" {
		t.Fatalf("lost-response recovery changed the prior metadata effect: metadata=%v", afterRecovery.Metadata)
	}
}

type bdSourceSnapshotIntegrationLostPostTransport struct {
	next           http.RoundTripper
	transitionPath string
	mu             sync.Mutex
	posts          int
	receiptGets    int
	dropped        bool
}

func (transport *bdSourceSnapshotIntegrationLostPostTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.Method == http.MethodPost && request.URL.EscapedPath() == transport.transitionPath {
		transport.mu.Lock()
		transport.posts++
		attempt := transport.posts
		transport.mu.Unlock()
		if attempt > 1 {
			return nil, errors.New("bounded integration transport refused a duplicate transition POST")
		}

		response, err := transport.next.RoundTrip(request)
		if err != nil {
			return nil, err
		}
		if response.StatusCode != http.StatusOK {
			return response, nil
		}
		body, readErr := io.ReadAll(io.LimitReader(response.Body, controllerTransitionMaxSuccessBody+1))
		closeErr := response.Body.Close()
		if readErr != nil {
			return nil, readErr
		}
		if closeErr != nil {
			return nil, closeErr
		}
		if int64(len(body)) > controllerTransitionMaxSuccessBody {
			return nil, errors.New("exact bd server transition response exceeded its configured body cap")
		}
		var result struct {
			Applied  bool `json:"applied"`
			Replayed bool `json:"replayed"`
		}
		if err := json.Unmarshal(body, &result); err != nil || !result.Applied || result.Replayed {
			return nil, fmt.Errorf("exact bd server did not apply a new transition before simulated response loss: %s", body)
		}
		transport.mu.Lock()
		transport.dropped = true
		transport.mu.Unlock()
		return nil, errors.New("simulated loss of successful controller transition response")
	}
	if request.Method == http.MethodGet && strings.HasPrefix(request.URL.EscapedPath(), controllerTransitionReceiptPath) {
		transport.mu.Lock()
		transport.receiptGets++
		transport.mu.Unlock()
	}
	return transport.next.RoundTrip(request)
}

func (transport *bdSourceSnapshotIntegrationLostPostTransport) counts() (dropped bool, posts, receiptGets int) {
	transport.mu.Lock()
	defer transport.mu.Unlock()
	return transport.dropped, transport.posts, transport.receiptGets
}

type bdSourceSnapshotIntegrationCapturedRequest struct {
	method        string
	path          string
	authorization string
	projectID     string
	contentType   string
	body          []byte
}

type bdSourceSnapshotIntegrationCaptureTransitionTransport struct {
	next           http.RoundTripper
	transitionPath string
	mu             sync.Mutex
	captured       []bdSourceSnapshotIntegrationCapturedRequest
}

func (transport *bdSourceSnapshotIntegrationCaptureTransitionTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.Method == http.MethodPost && request.URL.EscapedPath() == transport.transitionPath {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			return nil, err
		}
		request.Body = io.NopCloser(bytes.NewReader(body))
		transport.mu.Lock()
		if len(transport.captured) >= 2 {
			transport.mu.Unlock()
			return nil, errors.New("bounded integration capture refused a third transition POST")
		}
		transport.captured = append(transport.captured, bdSourceSnapshotIntegrationCapturedRequest{
			method: request.Method, path: request.URL.EscapedPath(),
			authorization: request.Header.Get("Authorization"),
			projectID:     request.Header.Get("Bd-Project-Id"),
			contentType:   request.Header.Get("Content-Type"),
			body:          append([]byte(nil), body...),
		})
		transport.mu.Unlock()
	}
	return transport.next.RoundTrip(request)
}

func (transport *bdSourceSnapshotIntegrationCaptureTransitionTransport) snapshotRequests() []bdSourceSnapshotIntegrationCapturedRequest {
	transport.mu.Lock()
	defer transport.mu.Unlock()
	requests := make([]bdSourceSnapshotIntegrationCapturedRequest, len(transport.captured))
	for index, request := range transport.captured {
		requests[index] = request
		requests[index].body = append([]byte(nil), request.body...)
	}
	return requests
}

func bdSourceSnapshotIntegrationAssertTransitionRequest(t *testing.T, got bdSourceSnapshotIntegrationCapturedRequest, wantPath, token, projectID string, request ControllerMetadataTransitionRequest) {
	t.Helper()
	if got.method != http.MethodPost || got.path != wantPath || got.authorization != "Bearer "+token ||
		got.projectID != projectID || got.contentType != "application/json" {
		t.Fatalf("captured transition request = method %q path %q authorization-matches=%t project-matches=%t content type %q; want POST %q with configured identity and JSON content type",
			got.method, got.path, got.authorization == "Bearer "+token, got.projectID == projectID, got.contentType, wantPath)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(got.body, &fields); err != nil || fields == nil {
		t.Fatalf("decode captured transition request body %s: %v", got.body, err)
	}
	wantFields := map[string]bool{
		"receipt_id": true, "scope": true, "kind": true, "actor": true,
		"expected_version": true, "key": true,
	}
	if request.Expected != nil {
		wantFields["expected"] = true
	}
	if request.Value != nil {
		wantFields["value"] = true
	}
	if len(request.Payload) != 0 {
		wantFields["payload"] = true
	}
	if len(fields) != len(wantFields) {
		t.Fatalf("captured transition request fields = %v, want exactly %v", fields, wantFields)
	}
	for name := range fields {
		if !wantFields[name] {
			t.Fatalf("captured transition request has unexpected field %q: %s", name, got.body)
		}
	}
	for name, want := range map[string]string{
		"receipt_id": request.ReceiptID, "scope": request.Scope, "kind": request.Kind,
		"actor": request.Actor, "key": request.Key,
	} {
		var value string
		if err := json.Unmarshal(fields[name], &value); err != nil || value != want {
			t.Fatalf("captured transition field %q = %s, want JSON string %q", name, fields[name], want)
		}
	}
	wantRevision := strconv.FormatInt(request.ExpectedVersion, 10)
	var gotRevision string
	if err := json.Unmarshal(fields["expected_version"], &gotRevision); err != nil || gotRevision != wantRevision {
		t.Fatalf("captured expected_version = %s, want JSON decimal string %q", fields["expected_version"], wantRevision)
	}
	for name, want := range map[string]*json.RawMessage{"expected": request.Expected, "value": request.Value} {
		gotRaw, present := fields[name]
		if want == nil {
			if present {
				t.Fatalf("captured transition field %q = %s, want absent", name, gotRaw)
			}
			continue
		}
		if !present || !bytes.Equal(gotRaw, *want) {
			t.Fatalf("captured transition field %q = %s, want %s", name, gotRaw, *want)
		}
	}
	if len(request.Payload) == 0 {
		if _, present := fields["payload"]; present {
			t.Fatalf("captured transition payload = %s, want omitted", fields["payload"])
		}
	} else if gotPayload, present := fields["payload"]; !present || !bytes.Equal(gotPayload, request.Payload) {
		t.Fatalf("captured transition payload = %s, want %s", gotPayload, request.Payload)
	}
}

func bdSourceSnapshotIntegrationAssertTransitionReceipt(t *testing.T, receipt *ControllerMetadataTransitionReceipt, issueID string, request ControllerMetadataTransitionRequest) {
	t.Helper()
	if receipt == nil {
		t.Fatal("controller metadata transition returned no receipt")
	}
	if receipt.ReceiptID != request.ReceiptID || receipt.IssueID != issueID || receipt.Scope != request.Scope ||
		receipt.Kind != request.Kind || receipt.Actor != request.Actor || receipt.ExpectedVersion != request.ExpectedVersion ||
		receipt.Key != request.Key {
		t.Fatalf("transition receipt bindings = %+v, want receipt ID %q issue %q scope %q kind %q actor %q expected revision %d key %q",
			receipt, request.ReceiptID, issueID, request.Scope, request.Kind, request.Actor, request.ExpectedVersion, request.Key)
	}
	if request.Expected == nil {
		if len(receipt.Expected) != 0 {
			t.Fatalf("receipt expected marker = %s, want absent", receipt.Expected)
		}
	} else if !bytes.Equal(receipt.Expected, *request.Expected) {
		t.Fatalf("receipt expected marker = %s, want %s", receipt.Expected, *request.Expected)
	}
	if request.Value == nil {
		if len(receipt.Value) != 0 {
			t.Fatalf("receipt value marker = %s, want absent", receipt.Value)
		}
	} else if !bytes.Equal(receipt.Value, *request.Value) {
		t.Fatalf("receipt value marker = %s, want %s", receipt.Value, *request.Value)
	}
	if !bytes.Equal(receipt.Payload, request.Payload) {
		t.Fatalf("receipt payload = %s, want %s", receipt.Payload, request.Payload)
	}
	if receipt.ToVersion == 0 || receipt.ToVersion == receipt.ExpectedVersion {
		t.Fatalf("receipt destination revision = %d, expected revision = %d; want a distinct nonzero revision token",
			receipt.ToVersion, receipt.ExpectedVersion)
	}
}

func bdSourceSnapshotIntegrationAssertTransitionEffect(t *testing.T, got, before Bead, receipt *ControllerMetadataTransitionReceipt, request ControllerMetadataTransitionRequest, wantValue string) {
	t.Helper()
	if receipt == nil {
		t.Fatal("cannot verify source effect without a transition receipt")
	}
	if got.ID != before.ID || got.Revision == 0 || got.Revision == before.Revision || got.Revision != receipt.ToVersion {
		t.Fatalf("source snapshot revision/identity = (%q, %d), previous = (%q, %d), receipt destination revision = %d",
			got.ID, got.Revision, before.ID, before.Revision, receipt.ToVersion)
	}
	if got.Metadata[request.Key] != wantValue {
		t.Fatalf("source snapshot metadata[%q] = %q, want %q", request.Key, got.Metadata[request.Key], wantValue)
	}
}

func bdSourceSnapshotIntegrationBDPath(t *testing.T) string {
	t.Helper()
	configured := strings.TrimSpace(os.Getenv("BEADS_TEST_BD_BINARY"))
	if configured == "" {
		t.Fatal("BEADS_TEST_BD_BINARY must name the exact bd executable for this integration test")
	}
	path, err := filepath.Abs(configured)
	if err != nil {
		t.Fatalf("resolve BEADS_TEST_BD_BINARY %q: %v", configured, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("BEADS_TEST_BD_BINARY %q: %v", path, err)
	}
	if !info.Mode().IsRegular() {
		t.Fatalf("BEADS_TEST_BD_BINARY %q is not a regular file", path)
	}
	return path
}

func bdSourceSnapshotIntegrationDoltPath(t *testing.T) string {
	t.Helper()
	configured := strings.TrimSpace(os.Getenv("BEADS_TEST_DOLT_BINARY"))
	if configured == "" {
		configured = "dolt"
	}
	path, err := exec.LookPath(configured)
	if err != nil {
		t.Skipf("source-snapshot integration needs the Dolt CLI: %v", err)
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		t.Fatalf("resolve Dolt executable %q: %v", path, err)
	}
	return absolute
}

func bdSourceSnapshotIntegrationEnv(home, doltRoot string) []string {
	env := make([]string, 0, len(os.Environ())+12)
	xdgConfig := filepath.Join(home, "xdg-config")
	for _, item := range os.Environ() {
		key, _, _ := strings.Cut(item, "=")
		if strings.HasPrefix(key, "BEADS_") || strings.HasPrefix(key, "BD_") || strings.HasPrefix(key, "DOLT_") || strings.HasPrefix(key, "GIT_") ||
			key == "HOME" || key == "USERPROFILE" || key == "APPDATA" || key == "XDG_CONFIG_HOME" ||
			key == "CGO_ENABLED" || key == "GOFLAGS" || key == "GOTOOLCHAIN" {
			continue
		}
		env = append(env, item)
	}
	return append(env,
		"HOME="+home,
		"USERPROFILE="+home,
		"XDG_CONFIG_HOME="+xdgConfig,
		"DOLT_ROOT_PATH="+doltRoot,
		"GIT_CONFIG_NOSYSTEM=1",
		"BEADS_DOLT_AUTO_START=0",
		"BEADS_NO_DAEMON=1",
		"BD_DISABLE_METRICS=1",
		"BD_DISABLE_EVENT_FLUSH=1",
	)
}

func bdSourceSnapshotIntegrationRandomHex(t *testing.T, size int) string {
	t.Helper()
	raw := make([]byte, size)
	if _, err := rand.Read(raw); err != nil {
		t.Fatalf("generate private integration identity: %v", err)
	}
	return hex.EncodeToString(raw)
}

func bdSourceSnapshotIntegrationRun(t *testing.T, parent context.Context, timeout time.Duration, env []string, dir, name string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir, cmd.Env = dir, env
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			t.Fatalf("%s %s timed out after %s: %v\nstdout:\n%s\nstderr:\n%s", name, strings.Join(args, " "), timeout, err, stdout.String(), stderr.String())
		}
		t.Fatalf("%s %s failed: %v\nstdout:\n%s\nstderr:\n%s", name, strings.Join(args, " "), err, stdout.String(), stderr.String())
	}
	return stdout.String()
}

func bdSourceSnapshotIntegrationFreePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve private Dolt TCP port: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatalf("release private Dolt TCP port: %v", err)
	}
	return port
}

type bdSourceSnapshotIntegrationBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *bdSourceSnapshotIntegrationBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *bdSourceSnapshotIntegrationBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

type bdSourceSnapshotIntegrationProcess struct {
	cmd     *exec.Cmd
	label   string
	output  *bdSourceSnapshotIntegrationBuffer
	done    chan struct{}
	waitErr error
}

func bdSourceSnapshotIntegrationTrackProcess(t *testing.T, cmd *exec.Cmd, label string, output *bdSourceSnapshotIntegrationBuffer) *bdSourceSnapshotIntegrationProcess {
	t.Helper()
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %s: %v", label, err)
	}
	process := &bdSourceSnapshotIntegrationProcess{cmd: cmd, label: label, output: output, done: make(chan struct{})}
	go func() {
		process.waitErr = cmd.Wait()
		close(process.done)
	}()
	t.Cleanup(func() {
		if err := process.stop(15 * time.Second); err != nil {
			t.Errorf("stop %s: %v\noutput:\n%s", label, err, output.String())
		}
	})
	return process
}

func (p *bdSourceSnapshotIntegrationProcess) exited() (bool, error) {
	select {
	case <-p.done:
		return true, p.waitErr
	default:
		return false, nil
	}
}

func (p *bdSourceSnapshotIntegrationProcess) stop(timeout time.Duration) error {
	if exited, _ := p.exited(); exited {
		return nil
	}
	if err := p.cmd.Process.Signal(os.Interrupt); err != nil {
		_ = p.cmd.Process.Kill()
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-p.done:
		return nil
	case <-timer.C:
	}
	if err := p.cmd.Process.Kill(); err != nil {
		if exited, _ := p.exited(); !exited {
			return fmt.Errorf("process did not stop within %s and kill failed: %w", timeout, err)
		}
	}
	killTimer := time.NewTimer(10 * time.Second)
	defer killTimer.Stop()
	select {
	case <-p.done:
		return nil
	case <-killTimer.C:
		return fmt.Errorf("process did not exit within 10s after kill")
	}
}

func bdSourceSnapshotIntegrationStartDolt(t *testing.T, env []string, doltBinary, dataDir string, port int) *bdSourceSnapshotIntegrationProcess {
	t.Helper()
	output := &bdSourceSnapshotIntegrationBuffer{}
	cmd := exec.Command(doltBinary, "sql-server", "-H", "127.0.0.1", "-P", strconv.Itoa(port), "--no-auto-commit", "--data-dir", dataDir)
	cmd.Env, cmd.Dir, cmd.Stdout, cmd.Stderr = env, dataDir, output, output
	return bdSourceSnapshotIntegrationTrackProcess(t, cmd, "private Dolt SQL server", output)
}

func bdSourceSnapshotIntegrationWaitForDolt(t *testing.T, ctx context.Context, port int, process *bdSourceSnapshotIntegrationProcess) {
	t.Helper()
	db, err := sql.Open("mysql", fmt.Sprintf("root@tcp(127.0.0.1:%d)/", port))
	if err != nil {
		t.Fatalf("open private Dolt readiness connection: %v", err)
	}
	defer db.Close()
	deadline := time.Now().Add(45 * time.Second)
	for time.Now().Before(deadline) {
		if exited, waitErr := process.exited(); exited {
			t.Fatalf("private Dolt SQL server exited before readiness: %v\noutput:\n%s", waitErr, process.output.String())
		}
		probeCtx, cancel := context.WithTimeout(ctx, time.Second)
		err := db.PingContext(probeCtx)
		var version string
		if err == nil {
			err = db.QueryRowContext(probeCtx, "SELECT DOLT_VERSION()").Scan(&version)
		}
		cancel()
		if err == nil && strings.TrimSpace(version) != "" {
			time.Sleep(100 * time.Millisecond)
			if exited, waitErr := process.exited(); exited {
				t.Fatalf("private Dolt SQL server exited during readiness check: %v\noutput:\n%s", waitErr, process.output.String())
			}
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("private Dolt SQL server readiness exceeded test deadline: %v", ctx.Err())
		case <-time.After(150 * time.Millisecond):
		}
	}
	t.Fatalf("private Dolt SQL server did not become ready on port %d\noutput:\n%s", port, process.output.String())
}

func bdSourceSnapshotIntegrationRunner(ctx context.Context, bdBinary string, env []string) CommandRunner {
	return func(dir, name string, args ...string) ([]byte, error) {
		if name != "bd" {
			return nil, fmt.Errorf("unexpected BdStore command name %q", name)
		}
		commandCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
		defer cancel()
		cmd := exec.CommandContext(commandCtx, bdBinary, args...)
		cmd.Dir, cmd.Env = dir, env
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		if err := cmd.Run(); err != nil {
			if commandCtx.Err() != nil {
				return stdout.Bytes(), fmt.Errorf("exact bd command %q timed out: %w; stderr: %s", strings.Join(args, " "), err, stderr.String())
			}
			return stdout.Bytes(), fmt.Errorf("exact bd command %q failed: %w; stderr: %s", strings.Join(args, " "), err, stderr.String())
		}
		return stdout.Bytes(), nil
	}
}

type bdSourceSnapshotIntegrationIdentity struct {
	ProjectID    string `json:"project_id"`
	DoltDatabase string `json:"dolt_database"`
}

func bdSourceSnapshotIntegrationReadIdentity(t *testing.T, workspace string) bdSourceSnapshotIntegrationIdentity {
	t.Helper()
	path := filepath.Join(workspace, ".beads", "metadata.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read private Beads workspace identity: %v", err)
	}
	var identity bdSourceSnapshotIntegrationIdentity
	if err := json.Unmarshal(data, &identity); err != nil {
		t.Fatalf("decode private Beads workspace identity: %v", err)
	}
	return identity
}

type bdSourceSnapshotIntegrationServe struct {
	process *bdSourceSnapshotIntegrationProcess
	addr    string
}

func bdSourceSnapshotIntegrationStartServe(t *testing.T, bdBinary, workspace string, env []string, tokenFile string) *bdSourceSnapshotIntegrationServe {
	t.Helper()
	cmd := exec.Command(bdBinary, "serve", "--addr", "127.0.0.1:0", "--auth-token-file", tokenFile)
	cmd.Dir, cmd.Env = workspace, env
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("capture exact bd serve address: %v", err)
	}
	stderr := &bdSourceSnapshotIntegrationBuffer{}
	cmd.Stderr = stderr
	process := bdSourceSnapshotIntegrationTrackProcess(t, cmd, "private bd serve", stderr)
	lines := make(chan string, 8)
	go func() {
		scanner := bufio.NewScanner(stdout)
		for scanner.Scan() {
			select {
			case lines <- scanner.Text():
			default:
			}
		}
		close(lines)
	}()
	deadline := time.NewTimer(90 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case line, ok := <-lines:
			if !ok {
				t.Fatalf("exact bd serve exited before announcing its address\noutput:\n%s", stderr.String())
			}
			addr, found := strings.CutPrefix(strings.TrimSpace(line), "bd serve: listening on http://")
			if found && addr != "" {
				return &bdSourceSnapshotIntegrationServe{process: process, addr: addr}
			}
		case <-process.done:
			t.Fatalf("exact bd serve exited before announcing its address: %v\noutput:\n%s", process.waitErr, stderr.String())
		case <-deadline.C:
			t.Fatalf("exact bd serve did not announce its address within 90s\noutput:\n%s", stderr.String())
		}
	}
}

func bdSourceSnapshotIntegrationWaitForServe(t *testing.T, ctx context.Context, serve *bdSourceSnapshotIntegrationServe, token, projectID, database string) string {
	t.Helper()
	baseURL := "http://" + serve.addr
	client := &http.Client{Timeout: 2 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	deadline := time.Now().Add(45 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		if exited, waitErr := serve.process.exited(); exited {
			t.Fatalf("exact bd serve exited before readiness: %v\noutput:\n%s", waitErr, serve.process.output.String())
		}
		requestCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		request, err := http.NewRequestWithContext(requestCtx, http.MethodGet, baseURL+"/v0/beads/context", nil)
		if err == nil {
			request.Header.Set("Authorization", "Bearer "+token)
			request.Header.Set("Bd-Project-Id", projectID)
			var response *http.Response
			response, err = client.Do(request)
			if err == nil {
				body, readErr := io.ReadAll(io.LimitReader(response.Body, 1<<20))
				closeErr := response.Body.Close()
				if readErr != nil {
					err = readErr
				} else if closeErr != nil {
					err = closeErr
				} else if response.StatusCode != http.StatusOK {
					err = fmt.Errorf("context returned HTTP %d", response.StatusCode)
				} else {
					var served struct {
						APIVersion   string   `json:"api_version"`
						Backend      string   `json:"backend"`
						Database     string   `json:"database"`
						DoltMode     string   `json:"dolt_mode"`
						ProjectID    string   `json:"project_id"`
						Capabilities []string `json:"capabilities"`
					}
					if decodeErr := json.Unmarshal(body, &served); decodeErr != nil {
						err = fmt.Errorf("decode context response: %w", decodeErr)
					} else if served.APIVersion != "v0" || served.Backend != "dolt" || served.DoltMode != "server" ||
						served.ProjectID != projectID || served.Database != database {
						cancel()
						t.Fatalf("bd serve identity = api %q backend %q mode %q project %q database %q, want v0 dolt server %q %q",
							served.APIVersion, served.Backend, served.DoltMode, served.ProjectID, served.Database, projectID, database)
					} else {
						for _, required := range []string{
							"issues.casMetadata", "issues.create", "issues.get", "issues.sourceSnapshot",
							"issues.transitionMetadata", "issues.transitionReceipt.get", "project.enforce",
						} {
							if !slices.Contains(served.Capabilities, required) {
								cancel()
								t.Fatalf("bd serve context omits required capability %q: %v", required, served.Capabilities)
							}
						}
						cancel()
						return baseURL
					}
				}
			}
		}
		cancel()
		lastErr = err
		select {
		case <-ctx.Done():
			t.Fatalf("bd serve readiness exceeded test deadline: %v (last response: %v)", ctx.Err(), lastErr)
		case <-time.After(150 * time.Millisecond):
		}
	}
	t.Fatalf("bd serve did not pass authenticated readiness within 45s: %v\noutput:\n%s", lastErr, serve.process.output.String())
	return ""
}

func bdSourceSnapshotIntegrationAssertCore(t *testing.T, got Bead, wantID, wantTitle, wantDescription string, wantPriority int) {
	t.Helper()
	if got.ID != wantID {
		t.Fatalf("source snapshot ID = %q, want %q", got.ID, wantID)
	}
	if got.Revision == 0 {
		t.Fatalf("source snapshot revision = %d, want nonzero", got.Revision)
	}
	if got.Title != wantTitle || got.Status != "open" || got.Type != "task" || got.Priority == nil || *got.Priority != wantPriority ||
		got.Description != wantDescription || got.CreatedAt.IsZero() {
		t.Fatalf("source snapshot core fields = %+v, want title %q, open task, priority %d, description %q, and creation time",
			got, wantTitle, wantPriority, wantDescription)
	}
}

func bdSourceSnapshotIntegrationSameCore(left, right Bead) bool {
	prioritiesMatch := left.Priority == nil && right.Priority == nil ||
		left.Priority != nil && right.Priority != nil && *left.Priority == *right.Priority
	return left.ID == right.ID && left.Title == right.Title && left.Status == right.Status && left.Type == right.Type &&
		left.Revision == right.Revision && prioritiesMatch && left.CreatedAt.Equal(right.CreatedAt) && left.UpdatedAt.Equal(right.UpdatedAt) &&
		left.Assignee == right.Assignee && left.Description == right.Description && left.ParentID == right.ParentID &&
		slices.Equal(left.Labels, right.Labels)
}

func bdSourceSnapshotIntegrationAssertEdges(t *testing.T, sourceID string, edges []Dep, expected map[string]string) map[string]string {
	t.Helper()
	if len(edges) != len(expected) {
		t.Fatalf("source snapshot returned %d outgoing edges, want exactly %d: %+v", len(edges), len(expected), edges)
	}
	seen := make(map[string]string, len(edges))
	for _, edge := range edges {
		wantType, expectedTarget := expected[edge.DependsOnID]
		if edge.IssueID != sourceID || !expectedTarget || edge.Type != wantType {
			t.Fatalf("source snapshot returned unexpected outgoing edge %+v; expected %v", edge, expected)
		}
		if _, duplicate := seen[edge.DependsOnID]; duplicate {
			t.Fatalf("source snapshot returned duplicate outgoing edge to %q: %+v", edge.DependsOnID, edges)
		}
		seen[edge.DependsOnID] = edge.Type
	}
	return seen
}

func bdSourceSnapshotIntegrationSameEdges(left, right map[string]string) bool {
	if len(left) != len(right) {
		return false
	}
	for target, depType := range left {
		if right[target] != depType {
			return false
		}
	}
	return true
}
