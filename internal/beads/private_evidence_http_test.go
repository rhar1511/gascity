package beads

import (
	"context"
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
	"sync/atomic"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/fsys"
)

func privateEvidenceTokenFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "controller-token")
	if err := os.WriteFile(path, []byte("controller-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func privateEvidenceHTTPStore(t *testing.T) *BdStore {
	t.Helper()
	return NewBdStore(t.TempDir(), func(_, _ string, _ ...string) ([]byte, error) {
		t.Fatal("private evidence HTTP path invoked bd command runner")
		return nil, nil
	}, WithBdStorePrivateEvidenceHTTP(PrivateEvidenceHTTPConfig{
		Endpoint:  "http://127.0.0.1:1",
		ProjectID: "project-a",
		Database:  "gc_fixture",
		ScopeRef:  "rig:fixture",
		TokenFile: privateEvidenceTokenFile(t),
	}))
}

type privateEvidenceHandlerTransport struct {
	handler http.Handler
}

func (transport privateEvidenceHandlerTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	recorder := httptest.NewRecorder()
	transport.handler.ServeHTTP(recorder, request)
	return recorder.Result(), nil
}

func TestReadPrivateEvidenceMetadataKeyRefusesUnsupportedBdStoresBeforeRunner(t *testing.T) {
	for _, config := range []struct {
		name   string
		option BdStoreOption
	}{
		{name: "unconfigured"},
		{
			name: "misconfigured",
			option: WithBdStorePrivateEvidenceHTTP(PrivateEvidenceHTTPConfig{
				Endpoint: "http://127.0.0.1:1", ProjectID: "project-a", Database: "gc_fixture",
				ScopeRef: "rig:fixture", TokenFile: filepath.Join(t.TempDir(), "missing-token"),
			}),
		},
	} {
		for _, wrap := range []struct {
			name string
			fn   func(Store) Store
		}{
			{name: "direct", fn: func(store Store) Store { return store }},
			{name: "typed", fn: func(store Store) Store { return WorkStore{Store: store} }},
			{name: "cached", fn: func(store Store) Store { return NewCachingStore(store, nil) }},
		} {
			t.Run(config.name+"/"+wrap.name, func(t *testing.T) {
				var calls atomic.Int32
				base := NewBdStore(t.TempDir(), func(_, _ string, _ ...string) ([]byte, error) {
					calls.Add(1)
					return []byte(`{"id":"gc-1","metadata":{"gc.attempt_evidence.index.a1":"private-value"}}`), nil
				}, config.option)
				store := wrap.fn(base)
				value, present, err := ReadPrivateEvidenceMetadataKey(store, "gc-1", "gc.attempt_evidence.index.a1")
				if !errors.Is(err, ErrPrivateEvidenceTransportUnsupported) {
					t.Fatalf("read = (%q, %v, %v), want unsupported private transport", value, present, err)
				}
				if calls.Load() != 0 {
					t.Fatalf("private evidence read invoked bd runner %d times", calls.Load())
				}
			})
		}
	}
}

func TestReadPrivateEvidenceMetadataKeyKeepsPrivateSafeDirectStoresReadable(t *testing.T) {
	store, err := OpenFileStore(fsys.OSFS{}, filepath.Join(t.TempDir(), "store.json"))
	if err != nil {
		t.Fatal(err)
	}
	owner, err := store.Create(Bead{Title: "private evidence owner", Type: "task", Metadata: map[string]string{
		"gc.attempt_evidence.index.a1": "body-safe-value",
	}})
	if err != nil {
		t.Fatal(err)
	}
	value, present, err := ReadPrivateEvidenceMetadataKey(store, owner.ID, "gc.attempt_evidence.index.a1")
	if err != nil {
		t.Fatalf("ReadPrivateEvidenceMetadataKey: %v", err)
	}
	if !present || value != "body-safe-value" {
		t.Fatalf("read = (%q, %v), want present private value", value, present)
	}
}

func TestReadPrivateEvidenceTokenRejectsOversizeAndUnsafeFiles(t *testing.T) {
	oversized := filepath.Join(t.TempDir(), "oversized-token")
	if err := os.WriteFile(oversized, []byte(strings.Repeat("x", 4097)), 0o600); err != nil {
		t.Fatal(err)
	}
	unsafe := filepath.Join(t.TempDir(), "unsafe-token")
	if err := os.WriteFile(unsafe, []byte("private-token"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(unsafe, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{oversized, unsafe} {
		t.Run(filepath.Base(path), func(t *testing.T) {
			if token, err := readPrivateEvidenceToken(path); err == nil || token != "" {
				t.Fatalf("readPrivateEvidenceToken = (%q, %v), want bounded refusal", token, err)
			}
		})
	}
}

func TestBdStorePrivateEvidenceCASPreservesAbsentEmptyAndNull(t *testing.T) {
	tests := []struct {
		name           string
		metadata       map[string]json.RawMessage
		wantCAS        bool
		wantExpected   string
		wantExpectedIn bool
	}{
		{name: "absent", metadata: map[string]json.RawMessage{}, wantCAS: true},
		{name: "empty string", metadata: map[string]json.RawMessage{"gc.attempt_evidence.index.a1": json.RawMessage(`""`)}, wantCAS: true, wantExpected: `""`, wantExpectedIn: true},
		{name: "null is not absent", metadata: map[string]json.RawMessage{"gc.attempt_evidence.index.a1": json.RawMessage(`null`)}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var casCalls atomic.Int32
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer controller-secret" {
					http.Error(w, "unauthorized", http.StatusUnauthorized)
					return
				}
				if r.Header.Get("Bd-Project-Id") != "project-a" {
					http.Error(w, "wrong project", http.StatusBadRequest)
					return
				}
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/v0/beads/context":
					_, _ = io.WriteString(w, `{"api_version":"v0","backend":"dolt","bd_version":"1.3.0","capabilities":["issues.casMetadata","issues.create","issues.get","project.enforce"],"database":"gc_fixture","dolt_mode":"server","project_id":"project-a","schema_version":1}`)
				case r.Method == http.MethodGet && r.URL.Path == "/v0/beads/issues/gc-1":
					body, _ := json.Marshal(map[string]any{"id": "gc-1", "metadata": tt.metadata})
					_, _ = w.Write(body)
				case r.Method == http.MethodPost && r.URL.Path == "/v0/beads/issues/gc-1:casMetadata":
					casCalls.Add(1)
					var body map[string]json.RawMessage
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Errorf("decode CAS body: %v", err)
						http.Error(w, "bad body", http.StatusBadRequest)
						return
					}
					if string(body["value"]) != `"private-evidence"` {
						t.Errorf("CAS value JSON = %s, want quoted opaque payload", body["value"])
					}
					gotExpected, hasExpected := body["expected"]
					if hasExpected != tt.wantExpectedIn || string(gotExpected) != tt.wantExpected {
						t.Errorf("CAS expected = %s, present=%v; want %s, present=%v", gotExpected, hasExpected, tt.wantExpected, tt.wantExpectedIn)
					}
					_, _ = io.WriteString(w, `{"swapped":true}`)
				default:
					t.Errorf("unexpected HTTP request %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
				}
			})

			store := privateEvidenceHTTPStore(t)
			store.privateEvidenceHTTP.client.Transport = privateEvidenceHandlerTransport{handler: handler}
			swapped, err := store.CompareAndSetPrivateEvidenceMetadataKey("gc-1", "gc.attempt_evidence.index.a1", "", "private-evidence")
			if err != nil {
				t.Fatalf("CompareAndSetPrivateEvidenceMetadataKey: %v", err)
			}
			if swapped != tt.wantCAS {
				t.Errorf("swapped = %v, want %v", swapped, tt.wantCAS)
			}
			if got := casCalls.Load(); got != int32(btoi(tt.wantCAS)) {
				t.Errorf("CAS requests = %d, want %d", got, btoi(tt.wantCAS))
			}
			value, present, readErr := store.ReadPrivateEvidenceMetadataKey("gc-1", "gc.attempt_evidence.index.a1")
			if tt.name == "null is not absent" {
				if readErr == nil || !present {
					t.Errorf("null metadata read = (%q, present=%v, err=%v), want present malformed value", value, present, readErr)
				}
				return
			}
			if readErr != nil {
				t.Fatalf("ReadPrivateEvidenceMetadataKey: %v", readErr)
			}
			wantPresent := tt.name == "empty string"
			if present != wantPresent || (present && value != "") {
				t.Errorf("metadata read = (%q, present=%v), want empty string present=%v", value, present, wantPresent)
			}
		})
	}
}

func TestBdStorePrivateEvidenceCASRejectsWrongContextBeforeWrite(t *testing.T) {
	var casCalls atomic.Int32
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer controller-secret" {
			t.Errorf("Authorization header missing")
		}
		if r.URL.Path == "/v0/beads/context" {
			_, _ = io.WriteString(w, `{"api_version":"v0","backend":"dolt","bd_version":"1.3.0","capabilities":["issues.casMetadata","issues.create","issues.get","project.enforce"],"database":"other_db","dolt_mode":"server","project_id":"project-a","schema_version":1}`)
			return
		}
		if strings.HasSuffix(r.URL.Path, ":casMetadata") {
			casCalls.Add(1)
		}
		http.Error(w, "private-evidence-must-not-appear-here", http.StatusBadRequest)
	})
	store := privateEvidenceHTTPStore(t)
	store.privateEvidenceHTTP.client.Transport = privateEvidenceHandlerTransport{handler: handler}
	_, err := store.CompareAndSetPrivateEvidenceMetadataKey("gc-1", "gc.attempt_evidence.index.a1", "", "private-evidence")
	if err == nil {
		t.Fatal("wrong database identity was accepted")
	}
	if errors.Is(err, ErrNotFound) {
		t.Fatalf("identity error was misclassified: %v", err)
	}
	if strings.Contains(err.Error(), "private-evidence") || strings.Contains(err.Error(), "controller-secret") {
		t.Fatalf("private payload or credential leaked in error: %v", err)
	}
	if got := casCalls.Load(); got != 0 {
		t.Fatalf("CAS calls = %d after identity mismatch, want 0", got)
	}
}

func TestBdStorePrivateEvidenceOwnerIndexReadSupportsLargeHistory(t *testing.T) {
	const attempts = 16
	const valueBytes = 64 << 10
	metadata := make(map[string]string, attempts)
	for i := 0; i < attempts; i++ {
		metadata[beadmeta.AttemptEvidenceIndexPrefix+fmt.Sprintf("%064x", i)] = strings.Repeat("\x01", valueBytes)
	}
	store := newBdStoreWithPrivateEvidenceHTTPForTest(t, func(_, _ string, _ ...string) ([]byte, error) {
		t.Fatal("private evidence HTTP path invoked bd command runner")
		return nil, nil
	}, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v0/beads/context":
			writePrivateEvidenceTestContext(w)
		case "/v0/beads/issues/gc-owner":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "gc-owner", "metadata": metadata})
		default:
			t.Errorf("unexpected HTTP request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	})
	indexes, err := store.ListPrivateEvidenceOwnerIndexes("gc-owner")
	if err != nil {
		t.Fatalf("read %d historical owner indexes: %v", attempts, err)
	}
	if len(indexes) != attempts {
		t.Fatalf("owner indexes = %d, want %d", len(indexes), attempts)
	}
	for key, value := range indexes {
		if len(value) != valueBytes {
			t.Fatalf("owner index %q length = %d, want %d", key, len(value), valueBytes)
		}
	}
}

func TestBdStorePrivateEvidenceGetUsesHTTPBodyReader(t *testing.T) {
	var runnerCalls atomic.Int32
	store := newBdStoreWithPrivateEvidenceHTTPForTest(t, func(_, _ string, _ ...string) ([]byte, error) {
		runnerCalls.Add(1)
		return nil, errors.New("unexpected bd invocation")
	}, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v0/beads/context":
			writePrivateEvidenceTestContext(w)
		case "/v0/beads/issues/gc-owner":
			_, _ = io.WriteString(w, `{"id":"gc-owner","title":"owner","status":"in_progress","issue_type":"task","metadata":{"gc.attempt_evidence.index.a1":"private-index"}}`)
		default:
			t.Errorf("unexpected HTTP request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	})
	bead, err := store.Get("gc-owner")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if bead.ID != "gc-owner" || bead.Metadata["gc.attempt_evidence.index.a1"] != "private-index" {
		t.Fatalf("Get bead = %#v", bead)
	}
	if got := runnerCalls.Load(); got != 0 {
		t.Fatalf("bd runner calls = %d, want 0", got)
	}
}

func TestPrivateEvidenceHTTPAcceptsOnlyDurableDoltServerModes(t *testing.T) {
	for _, tt := range []struct {
		mode string
		want bool
	}{
		{mode: "server", want: true},
		{mode: "proxied-server", want: true},
		{mode: "embedded"},
		{mode: "unknown"},
	} {
		t.Run(tt.mode, func(t *testing.T) {
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v0/beads/context" {
					http.NotFound(w, r)
					return
				}
				_, _ = fmt.Fprintf(w, `{"api_version":"v0","backend":"dolt","bd_version":"1.3.0","capabilities":["issues.casMetadata","issues.create","issues.get","project.enforce"],"database":"gc_fixture","dolt_mode":%q,"project_id":"project-a","schema_version":1}`, tt.mode)
			})
			store := privateEvidenceHTTPStore(t)
			store.privateEvidenceHTTP.client.Transport = privateEvidenceHandlerTransport{handler: handler}
			err := store.privateEvidenceHTTP.verifyContext(context.Background())
			if (err == nil) != tt.want {
				t.Fatalf("verifyContext() error = %v, want success = %v", err, tt.want)
			}
		})
	}
}

func TestBdStorePrivateEvidenceCreateUsesHTTPBody(t *testing.T) {
	const payloadPrefix = `{"attempt_id":"attempt-1","identity":{"owner_bead_id":"gc-owner"},"padding":"`
	const payloadSuffix = `"}`
	const maxPayloadBytes = 64 << 10
	payload := payloadPrefix + strings.Repeat(`\u0001`, (maxPayloadBytes-len(payloadPrefix)-len(payloadSuffix))/len(`\u0001`)) + payloadSuffix
	if len(payload) > maxPayloadBytes {
		t.Fatalf("test archive payload = %d bytes, want at most %d", len(payload), maxPayloadBytes)
	}
	digest := sha256.Sum256([]byte(payload))
	metadata := map[string]string{
		beadmeta.GCExemptMetadataKey:                        "true",
		beadmeta.AttemptEvidenceArchiveOwnerIDMetadataKey:   "gc-owner",
		beadmeta.AttemptEvidenceArchiveAttemptIDMetadataKey: "attempt-1",
		beadmeta.AttemptEvidenceArchiveDigestMetadataKey:    hex.EncodeToString(digest[:]),
		beadmeta.AttemptEvidenceArchivePayloadMetadataKey:   payload,
	}
	var runnerCalls atomic.Int32
	store := newBdStoreWithPrivateEvidenceHTTPForTest(t, func(_, _ string, _ ...string) ([]byte, error) {
		runnerCalls.Add(1)
		return nil, errors.New("unexpected bd invocation")
	}, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v0/beads/context":
			writePrivateEvidenceTestContext(w)
		case "/v0/beads/issues":
			if r.Method != http.MethodPost {
				t.Errorf("create method = %s, want POST", r.Method)
				http.Error(w, "wrong method", http.StatusMethodNotAllowed)
				return
			}
			if r.URL.RawQuery != "" {
				t.Errorf("create URL contains query: %q", r.URL.RawQuery)
			}
			var request struct {
				Actor     string            `json:"actor"`
				Title     string            `json:"title"`
				IssueType string            `json:"issue_type"`
				Status    string            `json:"status"`
				Labels    []string          `json:"labels"`
				Metadata  map[string]string `json:"metadata"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Errorf("decode create body: %v", err)
				http.Error(w, "bad body", http.StatusBadRequest)
				return
			}
			if request.Actor != privateEvidenceActor || request.Title != "Immutable execution attempt evidence" ||
				request.IssueType != "molecule" || request.Status != "closed" || request.Metadata[beadmeta.AttemptEvidenceArchivePayloadMetadataKey] != payload {
				t.Errorf("unexpected archive create request: %#v", request)
				http.Error(w, "bad create", http.StatusBadRequest)
				return
			}
			if r.ContentLength <= int64(len(payload)+8192) {
				t.Errorf("JSON body size = %d, want escaping overhead beyond %d raw payload bytes", r.ContentLength, len(payload))
			}
			response := map[string]any{
				"id": "gc-archive", "title": request.Title, "status": request.Status,
				"issue_type": request.IssueType, "labels": request.Labels, "metadata": request.Metadata,
			}
			_ = json.NewEncoder(w).Encode(response)
		default:
			t.Errorf("unexpected HTTP request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	})
	created, err := store.Create(Bead{
		Title:    "Immutable execution attempt evidence",
		Type:     "molecule",
		Status:   "open",
		Labels:   []string{"gc:attempt-evidence"},
		Metadata: StringMap(metadata),
	})
	if err != nil {
		t.Fatalf("Create archive: %v", err)
	}
	if created.ID != "gc-archive" || created.Status != "closed" || created.Metadata[beadmeta.AttemptEvidenceArchivePayloadMetadataKey] != payload {
		t.Fatalf("created archive = %#v", created)
	}
	if got := runnerCalls.Load(); got != 0 {
		t.Fatalf("bd runner calls = %d, want 0", got)
	}
}

func TestBdStorePrivateEvidenceArchiveRejectsEphemeralStorageBeforeHTTPWrite(t *testing.T) {
	for _, storage := range []StorageClass{StorageEphemeral, StorageNoHistory} {
		t.Run(string(storage), func(t *testing.T) {
			var requests atomic.Int32
			store := newBdStoreWithPrivateEvidenceHTTPForTest(t, func(_, _ string, _ ...string) ([]byte, error) {
				t.Fatal("private evidence HTTP path invoked bd command runner")
				return nil, nil
			}, func(w http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				http.Error(w, "unexpected request", http.StatusInternalServerError)
			})
			_, err := store.CreateWithStorage(Bead{
				Title: "Immutable execution attempt evidence",
				Type:  "molecule",
				Metadata: StringMap(map[string]string{
					beadmeta.AttemptEvidenceArchivePayloadMetadataKey: "private payload",
				}),
			}, storage)
			if err == nil {
				t.Fatal("CreateWithStorage succeeded for non-durable evidence archive")
			}
			if got := requests.Load(); got != 0 {
				t.Fatalf("HTTP requests = %d, want 0 before archive durability validation", got)
			}
		})
	}
}

func TestBdStorePrivateEvidenceArchiveRejectsNonDurableStorageBeforeHTTPWrite(t *testing.T) {
	for _, storage := range []StorageClass{StorageEphemeral, StorageNoHistory} {
		t.Run(string(storage), func(t *testing.T) {
			var requests atomic.Int32
			store := newBdStoreWithPrivateEvidenceHTTPForTest(t, func(_, _ string, _ ...string) ([]byte, error) {
				t.Fatal("private evidence HTTP path invoked bd command runner")
				return nil, nil
			}, func(w http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				http.Error(w, "unexpected request", http.StatusInternalServerError)
			})
			_, err := store.CreateWithStorage(Bead{
				Title: "Immutable execution attempt evidence",
				Type:  "molecule",
				Metadata: StringMap(map[string]string{
					beadmeta.AttemptEvidenceArchivePayloadMetadataKey: "private payload",
				}),
			}, storage)
			if err == nil {
				t.Fatal("CreateWithStorage succeeded for non-durable evidence archive")
			}
			if got := requests.Load(); got != 0 {
				t.Fatalf("HTTP requests = %d, want 0 before archive durability validation", got)
			}
		})
	}
}

func TestBdStorePrivateEvidenceArchiveListingReadsBodyPages(t *testing.T) {
	var pageCalls atomic.Int32
	store := newBdStoreWithPrivateEvidenceHTTPForTest(t, func(_, _ string, _ ...string) ([]byte, error) {
		t.Fatal("private evidence HTTP path invoked bd command runner")
		return nil, nil
	}, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v0/beads/context":
			writePrivateEvidenceTestContext(w)
		case "/v0/beads/issues":
			if r.URL.Query().Get("metadata_field") != beadmeta.AttemptEvidenceArchiveOwnerIDMetadataKey+"=gc-owner" {
				t.Errorf("archive owner filter = %q", r.URL.Query().Get("metadata_field"))
			}
			if !r.URL.Query().Has("all") {
				t.Errorf("closed archive listing omitted all=true")
			}
			if pageCalls.Add(1) == 1 {
				_, _ = io.WriteString(w, `{"items":[{"id":"gc-archive-1","title":"archive","status":"closed","issue_type":"molecule","metadata":{"gc.attempt_evidence.archive_owner_id":"gc-owner","gc.attempt_evidence.archive_attempt_id":"attempt-1","gc.attempt_evidence.archive_payload.v1":"one"}}],"has_more":true,"next_cursor":"next"}`)
				return
			}
			if r.URL.Query().Get("cursor") != "next" {
				t.Errorf("second-page cursor = %q", r.URL.Query().Get("cursor"))
			}
			_, _ = io.WriteString(w, `{"items":[{"id":"gc-archive-2","title":"archive","status":"closed","issue_type":"molecule","metadata":{"gc.attempt_evidence.archive_owner_id":"gc-owner","gc.attempt_evidence.archive_attempt_id":"attempt-2","gc.attempt_evidence.archive_payload.v1":"two"}}],"has_more":false}`)
		default:
			t.Errorf("unexpected HTTP request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	})
	archives, err := store.ListPrivateEvidenceArchives("gc-owner", "")
	if err != nil {
		t.Fatalf("ListPrivateEvidenceArchives: %v", err)
	}
	if len(archives) != 2 || archives[0].Metadata[beadmeta.AttemptEvidenceArchivePayloadMetadataKey] != "one" || archives[1].Metadata[beadmeta.AttemptEvidenceArchivePayloadMetadataKey] != "two" {
		t.Fatalf("archives = %#v", archives)
	}
}

func TestBdStorePrivateEvidenceReadinessProbesUnadvertisedListRoute(t *testing.T) {
	store := newBdStoreWithPrivateEvidenceHTTPForTest(t, func(_, _ string, _ ...string) ([]byte, error) {
		t.Fatal("private evidence HTTP path invoked bd command runner")
		return nil, nil
	}, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v0/beads/context" {
			writePrivateEvidenceTestContext(w)
			return
		}
		if r.URL.Path == "/v0/beads/issues" {
			_, _ = io.WriteString(w, `{"items":[],"has_more":false}`)
			return
		}
		http.NotFound(w, r)
	})
	if !store.PrivateEvidencePayloadTransportReady() {
		t.Fatal("transport readiness rejected the working list route")
	}
}

func TestBdStoreContentPayloadCapabilityAndArgvFailClosed(t *testing.T) {
	var calls atomic.Int32
	store := NewBdStore(t.TempDir(), func(_, _ string, _ ...string) ([]byte, error) {
		calls.Add(1)
		return nil, errors.New("runner must not receive private payload content")
	}, WithBdStorePrivateEvidenceHTTP(PrivateEvidenceHTTPConfig{
		Endpoint: "http://127.0.0.1:1", ProjectID: "project-a", Database: "gc_fixture",
		ScopeRef: "rig:fixture", TokenFile: privateEvidenceTokenFile(t),
	}))
	if SupportsPrivatePayloadValues(store) {
		t.Fatal("BdStore advertised content-payload support without a reachable verified HTTP transport")
	}
	metadata := map[string]string{
		beadmeta.AttemptEvidencePayloadDigestMetadataKey: strings.Repeat("a", 64),
		beadmeta.AttemptEvidencePayloadDataMetadataKey:   "cHJpdmF0ZQ==",
	}
	if _, err := store.Create(Bead{ID: "gc-private", Title: "private", Metadata: metadata}); err == nil {
		t.Fatal("BdStore Create accepted content-payload metadata")
	}
	if err := store.Update("gc-private", UpdateOpts{Metadata: metadata}); err == nil {
		t.Fatal("BdStore Update accepted content-payload metadata")
	}
	if err := store.SetMetadata("gc-private", beadmeta.AttemptEvidencePayloadDataMetadataKey, "cHJpdmF0ZQ=="); err == nil {
		t.Fatal("BdStore SetMetadata accepted content-payload metadata")
	}
	if calls.Load() != 0 {
		t.Fatalf("private content payload invoked bd runner %d times", calls.Load())
	}
}

func TestBdFailureDetailRedactsPrivateEvidenceOutput(t *testing.T) {
	for _, output := range []string{
		`{"metadata":{"gc.attempt_evidence.archive_payload.v1":"opaque evidence"}}`,
		`malformed {"gc.attempt_evidence.index.a1":"opaque evidence"`,
	} {
		got := bdFailureDetail("bd", []byte(output), "")
		if strings.Contains(got, "opaque evidence") || got != redactedPrivateEvidenceDiagnostic {
			t.Fatalf("bdFailureDetail = %q, want redaction marker", got)
		}
	}
	stderr := "Error: rejected key gc.attempt_evidence.archive_payload.v1 value=opaque evidence"
	if got := bdFailureDetail("bd", nil, stderr); strings.Contains(got, "opaque evidence") || got != redactedPrivateEvidenceDiagnostic {
		t.Fatalf("bdFailureDetail stderr = %q, want redaction marker", got)
	}
}

func newBdStoreWithPrivateEvidenceHTTPForTest(t *testing.T, runner CommandRunner, handler http.HandlerFunc) *BdStore {
	t.Helper()
	store := NewBdStoreWithPrefix(t.TempDir(), runner, "gc", WithBdStorePrivateEvidenceHTTP(PrivateEvidenceHTTPConfig{
		Endpoint: "http://127.0.0.1:1", ProjectID: "project-a", Database: "gc_fixture", ScopeRef: "rig:fixture", TokenFile: privateEvidenceTokenFile(t),
	}))
	store.privateEvidenceHTTP.client.Transport = privateEvidenceHandlerTransport{handler: handler}
	return store
}

func writePrivateEvidenceTestContext(w http.ResponseWriter) {
	_, _ = io.WriteString(w, `{"api_version":"v0","backend":"dolt","bd_version":"1.3.0","capabilities":["issues.casMetadata","issues.create","issues.get","project.enforce"],"database":"gc_fixture","dolt_mode":"server","project_id":"project-a","schema_version":1}`)
}

func btoi(value bool) int {
	if value {
		return 1
	}
	return 0
}

// This uses the public lifecycle-sensitive Update path that GraphWorkflow calls.
// The CLI advertises guards but refuses them for a proxied store; the configured
// authenticated HTTP transport must carry the same observed aggregate token.
func TestBdStoreLifecycleUpdateUsesGuardedHTTP(t *testing.T) {
	for _, mode := range []string{"server", "proxied-server"} {
		for _, tc := range []struct {
			name, observed, expected, next                      string
			conditional, changed, missingToken                  bool
			status                                              int
			problem, missingCapability, wrongProject, malformed string
			want                                                string
		}{
			{name: "zero", observed: "0", expected: "0", next: "-8", changed: true},
			{name: "negative", observed: "-7", expected: "-7", next: "-8", changed: true},
			{name: "large", observed: "9007199254740993", expected: "9007199254740993", next: "9007199254740995", changed: true},
			{name: "zero noop", observed: "0", expected: "0", next: "0"},
			{name: "caller guard is not renewed", observed: "9", expected: "7", conditional: true, status: http.StatusConflict, problem: `{"code":"precondition_failed","param":"expected_version","expected_version":"7"}`, want: "stale"},
			{name: "missing detail token", missingToken: true, want: "unsupported"},
			{name: "missing workspace enforcement", observed: "7", missingCapability: "project.enforce", want: "protocol"},
			{name: "missing update capability", observed: "7", missingCapability: "issues.update", want: "unsupported"},
			{name: "wrong workspace", observed: "7", wrongProject: "project-b", want: "error"},
			{name: "malformed success", observed: "7", expected: "7", malformed: `{}`, want: "error"},
			{name: "changed token unchanged", observed: "7", expected: "7", next: "7", changed: true, want: "error"},
			{name: "no-op token changed", observed: "7", expected: "7", next: "8", want: "error"},
			{name: "runtime route unsupported", observed: "7", expected: "7", status: http.StatusNotImplemented, want: "unsupported"},
			{name: "protected refusal", observed: "7", expected: "7", status: http.StatusConflict, problem: `{"code":"protected_record","param":"patch"}`, want: "gate"},
			{name: "service failure", observed: "7", expected: "7", status: http.StatusServiceUnavailable, want: "error"},
		} {
			t.Run(mode+"/"+tc.name, func(t *testing.T) {
				store := privateEvidenceHTTPStore(t)
				patches := 0
				store.privateEvidenceHTTP.client.Transport = privateEvidenceHandlerTransport{handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Header.Get("Authorization") != "Bearer controller-secret" {
						t.Fatal("missing configured authentication")
					}
					w.Header().Set("Content-Type", "application/json")
					switch {
					case r.Method == http.MethodGet && r.URL.Path == "/v0/beads/context":
						caps := []string{"issues.casMetadata", "issues.create", "issues.get", "project.enforce", "issues.update"}
						var actual []string
						for _, cap := range caps {
							if cap != tc.missingCapability {
								actual = append(actual, cap)
							}
						}
						project := "project-a"
						if tc.wrongProject != "" {
							project = tc.wrongProject
						}
						_ = json.NewEncoder(w).Encode(map[string]any{"api_version": "v0", "bd_version": "fixture", "backend": "dolt", "dolt_mode": mode, "database": "gc_fixture", "project_id": project, "capabilities": actual})
					case r.Method == http.MethodGet && r.URL.Path == "/v0/beads/issues/gc-1":
						row := map[string]any{"id": "gc-1", "title": "workflow", "status": "open"}
						if !tc.missingToken {
							row["revision"] = tc.observed
						}
						_ = json.NewEncoder(w).Encode(row)
					case r.Method == http.MethodPatch && r.URL.Path == "/v0/beads/issues/gc-1":
						patches++
						var body struct {
							Expected string `json:"expected_version"`
							Patch    struct {
								Status string `json:"status"`
							} `json:"patch"`
						}
						if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
							t.Fatal(err)
						}
						if body.Expected != tc.expected || body.Patch.Status != "in_progress" {
							t.Fatalf("guarded PATCH = %+v", body)
						}
						if tc.status != 0 {
							w.WriteHeader(tc.status)
							_, _ = io.WriteString(w, tc.problem)
							return
						}
						if tc.malformed != "" {
							_, _ = io.WriteString(w, tc.malformed)
							return
						}
						_ = json.NewEncoder(w).Encode(map[string]any{"issue": map[string]any{"id": "gc-1", "title": "workflow", "priority": 2, "created_at": "2026-10-01T00:00:00Z", "updated_at": "2026-10-01T00:00:00Z"}, "changed": tc.changed, "revision": tc.next})
					default:
						t.Fatalf("unexpected HTTP request: %s %s", r.Method, r.URL.Path)
					}
				})}
				status := "in_progress"
				var err error
				if tc.conditional {
					err = store.UpdateIfMatch("gc-1", 7, UpdateOpts{Status: &status})
				} else {
					err = store.Update("gc-1", UpdateOpts{Status: &status})
				}
				switch tc.want {
				case "":
					if err != nil {
						t.Fatalf("lifecycle update: %v", err)
					}
				case "unsupported":
					if !errors.Is(err, ErrConditionalWriteUnsupported) {
						t.Fatalf("error = %v, want unsupported", err)
					}
				case "protocol":
					if !errors.Is(err, ErrPrivateEvidenceHTTPProtocol) {
						t.Fatalf("error = %v, want protocol refusal", err)
					}
				case "stale":
					var stale *PreconditionFailedError
					if !errors.As(err, &stale) || stale.Expected != 7 || stale.ID != "gc-1" {
						t.Fatalf("stale result = %v", err)
					}
				case "gate":
					var gate *GateRefusalError
					if !errors.As(err, &gate) || gate.Code != "protected_record" {
						t.Fatalf("gate result = %v", err)
					}
				default:
					if err == nil {
						t.Fatal("expected refusal")
					}
				}
				wantPatches := 1
				if tc.missingToken || tc.missingCapability != "" || tc.wrongProject != "" {
					wantPatches = 0
				}
				if patches != wantPatches {
					t.Fatalf("PATCH count = %d, want %d", patches, wantPatches)
				}
			})
		}
	}
}

// Public attempt references use the generic metadata CAS after private payload
// sealing. Stock Beads must retain the value comparison and aggregate fence
// without requiring the fork-only four-verb CLI capability.
func TestBdStoreMetadataCASUsesGuardedHTTP(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		conflicts              int
		competingValue         bool
		missingRevision        bool
		status                 int
		wantSwapped            bool
		wantReads, wantPatches int
		wantError              string
	}{
		{name: "seal public reference", wantSwapped: true, wantReads: 1, wantPatches: 1},
		{name: "unrelated write retries with fresh snapshot", conflicts: 1, wantSwapped: true, wantReads: 2, wantPatches: 2},
		{name: "competing reference wins", conflicts: 1, competingValue: true, wantReads: 2, wantPatches: 1},
		{name: "bounded contention", conflicts: casEmulationMaxAttempts, wantReads: casEmulationMaxAttempts + 1, wantPatches: casEmulationMaxAttempts, wantError: "exhausted"},
		{name: "missing revision refuses", missingRevision: true, wantReads: 1, wantError: "unsupported"},
		{name: "unavailable transport never falls back", status: http.StatusServiceUnavailable, wantReads: 1, wantPatches: 1, wantError: "unavailable"},
		{name: "unsupported route never falls back", status: http.StatusNotImplemented, wantReads: 1, wantPatches: 1, wantError: "unsupported"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := privateEvidenceHTTPStore(t)
			reads, patches := 0, 0
			key := beadmeta.AttemptEvidenceReferenceMetadataKey
			const next = `{"work_id":"gc-1","attempt_id":"a-1","digest":"public-digest"}`
			store.privateEvidenceHTTP.client.Transport = privateEvidenceHandlerTransport{handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer controller-secret" {
					t.Fatal("missing configured authentication")
				}
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/v0/beads/context":
					_, _ = io.WriteString(w, `{"api_version":"v0","backend":"dolt","bd_version":"1.3.1","capabilities":["issues.casMetadata","issues.create","issues.get","issues.update","project.enforce"],"database":"gc_fixture","dolt_mode":"proxied-server","project_id":"project-a"}`)
				case r.Method == http.MethodGet && r.URL.Path == "/v0/beads/issues/gc-1":
					reads++
					metadata := map[string]string{}
					if tc.competingValue && patches > 0 {
						metadata[key] = "competitor"
					}
					row := map[string]any{"id": "gc-1", "title": "attempt", "status": "closed", "metadata": metadata}
					if !tc.missingRevision {
						row["revision"] = fmt.Sprint(-reads)
					}
					_ = json.NewEncoder(w).Encode(row)
				case r.Method == http.MethodPatch && r.URL.Path == "/v0/beads/issues/gc-1":
					patches++
					var body conditionalIssueUpdateRequest
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Fatal(err)
					}
					if body.ExpectedVersion != fmt.Sprint(-reads) || body.Patch.Metadata == nil || len(body.Patch.Metadata.Set) != 1 || body.Patch.Metadata.Set[key] != next {
						t.Fatalf("CAS did not preserve the observed guard and exact public reference: %+v", body)
					}
					if tc.status != 0 {
						w.WriteHeader(tc.status)
						return
					}
					if patches <= tc.conflicts {
						w.WriteHeader(http.StatusConflict)
						_, _ = io.WriteString(w, `{"code":"precondition_failed","param":"expected_version"}`)
						return
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"issue": map[string]any{"id": "gc-1", "title": "attempt", "priority": 2, "created_at": "2026-10-01T00:00:00Z", "updated_at": "2026-10-01T00:00:00Z"}, "changed": true, "revision": fmt.Sprint(-reads - 1)})
				default:
					t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
				}
			})}
			swapped, err := store.CompareAndSetMetadataKey("gc-1", key, "", next)
			if swapped != tc.wantSwapped {
				t.Fatalf("swapped=%v, error=%v", swapped, err)
			}
			switch tc.wantError {
			case "":
				if err != nil {
					t.Fatal(err)
				}
			case "exhausted":
				var exhausted *CASRetriesExhaustedError
				if !errors.As(err, &exhausted) || exhausted.Attempts != casEmulationMaxAttempts {
					t.Fatalf("error=%v, want bounded exhaustion", err)
				}
			case "unsupported":
				if !errors.Is(err, ErrConditionalWriteUnsupported) {
					t.Fatalf("error=%v, want unsupported", err)
				}
			case "unavailable":
				if !errors.Is(err, ErrConditionalIssueUpdateHTTPUnavailable) {
					t.Fatalf("error=%v, want unavailable", err)
				}
			}
			if reads != tc.wantReads || patches != tc.wantPatches {
				t.Fatalf("reads/patches=%d/%d, want %d/%d", reads, patches, tc.wantReads, tc.wantPatches)
			}
		})
	}
}
