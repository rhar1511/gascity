package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/worklifecycle"
)

func TestBeadsAdmissionNativeSourceUsesScopedAuthoritativeSnapshot(t *testing.T) {
	fixture := newBeadsAdmissionCommandFixture(t)
	t.Setenv("GC_CITY", fixture.cityPath)
	var mu sync.Mutex
	var requests []string
	server, transport := privateEvidenceHTTPTestTransport(t, "native-project", "native-database", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests = append(requests, r.Method+" "+r.URL.Path)
		mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer test-token" || r.Header.Get("Bd-Project-Id") != "native-project" {
			t.Error("native source request lost its configured transport identity")
			http.Error(w, "unexpected transport identity", http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v0/beads/context":
			_ = json.NewEncoder(w).Encode(struct {
				APIVersion   string   `json:"api_version"`
				Backend      string   `json:"backend"`
				BDVersion    string   `json:"bd_version"`
				Capabilities []string `json:"capabilities"`
				Database     string   `json:"database"`
				DoltMode     string   `json:"dolt_mode"`
				ProjectID    string   `json:"project_id"`
			}{"v0", "dolt", "1.3.2", []string{"issues.sourceSnapshot", "project.enforce"}, "native-database", "server", "native-project"})
		case r.Method == http.MethodGet && r.URL.Path == "/v0/beads/issues/"+fixture.source.ID+"/sourceSnapshot":
			// The authoritative HTTP wire encodes revisions as decimal strings.
			type sourceWire struct {
				ID        string `json:"id"`
				Title     string `json:"title"`
				Status    string `json:"status"`
				IssueType string `json:"issue_type"`
				Revision  string `json:"revision"`
			}
			_ = json.NewEncoder(w).Encode(struct {
				Issue        sourceWire  `json:"issue"`
				Dependencies []beads.Dep `json:"outgoing_dependencies"`
			}{sourceWire{fixture.source.ID, fixture.source.Title, fixture.source.Status, fixture.source.Type, strconv.FormatInt(fixture.source.Revision, 10)}, []beads.Dep{{IssueID: fixture.source.ID, DependsOnID: "dependency-1", Type: "blocks"}}})
		default:
			t.Errorf("source preparation attempted an unexpected request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	configPath := filepath.Join(fixture.cityPath, "city.toml")
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	data = []byte(strings.Replace(string(data), `provider = "file"`, `provider = "bd"`, 1))
	data = append(data, []byte(fmt.Sprintf(`
[beads.private_evidence."rig:pilot"]
revision_transitions = %t
endpoint = %q
project_id = %q
database = %q
token_file = %q
`, transport.RevisionTransitions, transport.Endpoint, transport.ProjectID, transport.Database, transport.TokenFile))...)
	if err := os.WriteFile(configPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	native := beads.NewNativeDoltStoreOverStorageForTest(nil)
	previous := openBeadsAdmissionStore
	openBeadsAdmissionStore = func(scopeRoot, cityRoot string, cfg *config.City) (beads.Store, error) {
		if resolveStoreScopeRoot(cityRoot, scopeRoot) == filepath.Join(fixture.cityPath, "pilot") {
			return native, nil
		}
		return previous(scopeRoot, cityRoot, cfg)
	}
	t.Cleanup(func() { openBeadsAdmissionStore = previous })
	ctx, err := openBeadsAdmissionContext("rig:pilot", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	defer ctx.close()
	reader, ok := beads.DecisionFrontierSourceReaderFor(ctx.sourceLeg.store)
	if !ok {
		t.Fatal("one-shot native admission omitted its scoped authoritative source reader")
	}
	got, err := reader.DecisionFrontierSourceSnapshot(fixture.source.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != fixture.source.ID || got.Revision != fixture.source.Revision || got.Title != fixture.source.Title ||
		!reflect.DeepEqual(got.Dependencies, []beads.Dep{{IssueID: fixture.source.ID, DependsOnID: "dependency-1", Type: "blocks"}}) {
		t.Fatalf("native admission lost the authoritative source revision or outgoing edges: %+v", got)
	}
	draft, err := prepareBeadsAdmission("rig:pilot", worklifecycle.AdmissionReceiptV2{
		WorkItemID: fixture.source.ID, Route: "pilot/worker", Deliverable: "reviewed patch",
		Verification: "tests", AcceptanceAuthority: "reviewer", MergeStrategy: "mr",
	}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if draft.Receipt.ExpectedWorkRevision != fixture.source.Revision || draft.Receipt.Scope != "city:pilot/rig:pilot" || draft.Receipt.Signature != "" {
		t.Fatalf("native preparation lost its exact unsigned source binding: %+v", draft.Receipt)
	}
	if err := native.CloseStore(); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.DecisionFrontierSourceSnapshot(fixture.source.ID); !errors.Is(err, beads.ErrStoreClosed) {
		t.Fatalf("retained source reader survived native close: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(requests) != 4 {
		t.Fatalf("preparation wrote or repeated a source read: %v", requests)
	}
}

// Exercise the native constructor through the actual prepare/sign/attach commands.
// The loopback backend uses the existing revision-fenced attachment fixture;
// it is not a replacement for the NativeDoltStore capability being tested.
func TestBeadsAdmissionNativeAttachBindsOnlyVerifiedAdmissionScope(t *testing.T) {
	for _, scenario := range []string{"valid", "wrong-key", "wrong-scope"} {
		t.Run(scenario, func(t *testing.T) {
			f := newBeadsAdmissionCommandFixture(t)
			t.Setenv("GC_CITY", f.cityPath)
			var mu sync.Mutex
			posts := 0
			receiptReads := 0
			var opened []*beads.NativeDoltStore
			type transitionWire struct {
				ReceiptID       string           `json:"receipt_id"`
				Scope           string           `json:"scope"`
				Kind            string           `json:"kind"`
				Actor           string           `json:"actor"`
				ExpectedVersion string           `json:"expected_version"`
				Key             string           `json:"key"`
				Expected        *json.RawMessage `json:"expected,omitempty"`
				Value           *json.RawMessage `json:"value,omitempty"`
				Payload         json.RawMessage  `json:"payload"`
				ProtectedPermit string           `json:"protected_permit,omitempty"`
			}
			type receiptWire struct {
				transitionWire
				IssueID   string `json:"issue_id"`
				ToVersion string `json:"to_version"`
			}
			var committed *receiptWire
			server, transport := privateEvidenceHTTPTestTransport(t, "native-project", "native-database", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				if r.Header.Get("Authorization") != "Bearer test-token" || r.Header.Get("Bd-Project-Id") != "native-project" {
					t.Error("native attachment lost its exact configured transport identity")
					http.Error(w, "unexpected transport identity", http.StatusForbidden)
					return
				}
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/v0/beads/context":
					_, _ = io.WriteString(w, `{"api_version":"v0","backend":"dolt","bd_version":"1.3.2","project_id":"native-project","database":"native-database","dolt_mode":"server","capabilities":["issues.sourceSnapshot","issues.transitionMetadata","issues.transitionReceipt.get","project.enforce"]}`)
				case r.Method == http.MethodGet && r.URL.Path == "/v0/beads/issues/"+f.source.ID+"/sourceSnapshot":
					current, err := f.store.Get(f.source.ID)
					if err != nil {
						t.Error(err)
						http.Error(w, "snapshot failed", 500)
						return
					}
					type issueWire struct {
						ID       string            `json:"id"`
						Title    string            `json:"title"`
						Status   string            `json:"status"`
						Type     string            `json:"issue_type"`
						Revision string            `json:"revision"`
						Metadata map[string]string `json:"metadata"`
					}
					_ = json.NewEncoder(w).Encode(struct {
						Issue        issueWire   `json:"issue"`
						Dependencies []beads.Dep `json:"outgoing_dependencies"`
					}{issueWire{current.ID, current.Title, current.Status, current.Type, strconv.FormatInt(current.Revision, 10), current.Metadata}, []beads.Dep{}})
				case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v0/beads/issue-transition-receipts/"):
					receiptReads++
					if committed == nil {
						w.WriteHeader(http.StatusNotFound)
						_, _ = io.WriteString(w, `{"code":"not_found"}`)
						return
					}
					if r.URL.Path != "/v0/beads/issue-transition-receipts/"+committed.ReceiptID {
						t.Error("lookup used a different receipt")
						http.Error(w, "unexpected receipt", 400)
						return
					}
					_ = json.NewEncoder(w).Encode(committed)
				case r.Method == http.MethodPost && r.URL.Path == "/v0/beads/issues/"+f.source.ID+":transitionMetadata":
					posts++
					for _, native := range opened {
						if _, ok := beads.RevisionTransitionPatchWriterFor(native); ok {
							t.Error("admission granted host protected patch authority")
							http.Error(w, "unexpected patch authority", http.StatusForbidden)
							return
						}
					}
					var request transitionWire
					decoder := json.NewDecoder(r.Body)
					decoder.DisallowUnknownFields()
					if err := decoder.Decode(&request); err != nil {
						t.Error(err)
						http.Error(w, "invalid request", 400)
						return
					}
					if request.Scope != "city:pilot/rig:pilot" || request.Kind != "lifecycle_admission_v2_attach" || request.Actor != "triage" || request.ExpectedVersion != strconv.FormatInt(f.source.Revision, 10) || request.Key != beadmeta.LifecycleAdmissionReceiptV2MetadataKey || request.ProtectedPermit != "" || request.Expected != nil || request.Value == nil {
						t.Errorf("attachment granted unexpected scope or authority: %+v", request)
						http.Error(w, "unexpected authority", http.StatusForbidden)
						return
					}
					result, err := f.store.TransitionMetadata(f.source.ID, beads.ControllerMetadataTransitionRequest{
						ReceiptID: request.ReceiptID, Scope: request.Scope, Kind: request.Kind, Actor: request.Actor, ExpectedVersion: f.source.Revision, Key: request.Key, Value: request.Value, Payload: request.Payload,
					})
					if err != nil || result.Receipt == nil {
						t.Errorf("attachment backend: %v", err)
						http.Error(w, "transition failed", 500)
						return
					}
					committed = &receiptWire{request, f.source.ID, strconv.FormatInt(result.Receipt.ToVersion, 10)}
					_ = json.NewEncoder(w).Encode(struct {
						Applied  bool         `json:"applied"`
						Replayed bool         `json:"replayed"`
						Receipt  *receiptWire `json:"receipt"`
					}{true, false, committed})
				default:
					t.Errorf("native admission attempted unrelated HTTP operation: %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			configPath := filepath.Join(f.cityPath, "city.toml")
			data, err := os.ReadFile(configPath)
			if err != nil {
				t.Fatal(err)
			}
			data = []byte(strings.Replace(string(data), `provider = "file"`, `provider = "bd"`, 1))
			data = append(data, []byte(fmt.Sprintf(`
[beads.private_evidence."rig:pilot"]
revision_transitions = %t
endpoint = %q
project_id = %q
database = %q
token_file = %q
`, transport.RevisionTransitions, transport.Endpoint, transport.ProjectID, transport.Database, transport.TokenFile))...)
			if err := os.WriteFile(configPath, data, 0o600); err != nil {
				t.Fatal(err)
			}
			previous := openBeadsAdmissionStore
			openBeadsAdmissionStore = func(scopeRoot, cityRoot string, cfg *config.City) (beads.Store, error) {
				if resolveStoreScopeRoot(cityRoot, scopeRoot) == filepath.Join(f.cityPath, "pilot") {
					native := beads.NewNativeDoltStoreOverStorageForTest(nil)
					mu.Lock()
					opened = append(opened, native)
					mu.Unlock()
					return beads.NewCachingStore(native, nil), nil
				}
				return previous(scopeRoot, cityRoot, cfg)
			}
			t.Cleanup(func() { openBeadsAdmissionStore = previous })
			draft, err := prepareBeadsAdmission("rig:pilot", worklifecycle.AdmissionReceiptV2{WorkItemID: f.source.ID, Route: "pilot/worker", Deliverable: "reviewed patch", Verification: "tests", AcceptanceAuthority: "reviewer", MergeStrategy: "mr"}, io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "wrong-scope" {
				draft.Receipt.Scope = "city:other/rig:pilot"
			}
			if scenario == "wrong-key" {
				key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{9}, ed25519.SeedSize))
				der, err := x509.MarshalPKCS8PrivateKey(key)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(f.keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			draftPath := filepath.Join(f.cityPath, "reviewed.json")
			encoded, err := json.Marshal(draft)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(draftPath, encoded, 0o600); err != nil {
				t.Fatal(err)
			}
			proof, err := attachBeadsAdmission(draftPath, "rig:pilot", f.keyPath, io.Discard)
			mu.Lock()
			defer mu.Unlock()
			if scenario == "valid" {
				if err != nil {
					t.Fatal(err)
				}
				if posts != 1 || receiptReads < 2 || proof.Scope != "city:pilot/rig:pilot" || proof.FromRevision != f.source.Revision || proof.ToRevision == 0 || proof.ToRevision == proof.FromRevision {
					t.Fatalf("native attachment lost its bounded proof: %+v posts=%d reads=%d", proof, posts, receiptReads)
				}
			} else {
				want := "key does not match"
				if scenario == "wrong-scope" {
					want = "draft scope or primary authority differs"
				}
				if err == nil || !strings.Contains(err.Error(), want) || posts != 0 {
					t.Fatalf("%s did not refuse at its exact guard: posts=%d err=%v", scenario, posts, err)
				}
			}
		})
	}
}
