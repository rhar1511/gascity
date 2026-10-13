//go:build integration

package compatibility

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/qualification"
)

func TestStoreCapabilityProverChecksConfiguredBdStoreThroughGraphStore(t *testing.T) {
	const readyContext = `{"api_version":"v0","backend":"dolt","bd_version":"1.3.1","capabilities":["issues.casMetadata","issues.create","issues.get","project.enforce"],"database":"gc_fixture","dolt_mode":"server","project_id":"project-a"}`
	contextResponse := readyContext
	listReady := true
	var stateMu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		stateMu.Lock()
		defer stateMu.Unlock()
		if r.Method != http.MethodGet {
			t.Errorf("capability proof attempted a mutation: %s %s", r.Method, r.URL.Path)
			http.Error(w, "read-only proof", http.StatusMethodNotAllowed)
			return
		}
		if r.Header.Get("Authorization") != "Bearer controller-secret" || r.Header.Get("Bd-Project-Id") != "project-a" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v0/beads/context":
			_, _ = w.Write([]byte(contextResponse))
		case "/v0/beads/issues":
			if !listReady {
				http.Error(w, "list unavailable", http.StatusServiceUnavailable)
				return
			}
			_, _ = w.Write([]byte(`{"items":[],"has_more":false}`))
		default:
			t.Errorf("unexpected readiness request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	tokenPath := filepath.Join(t.TempDir(), "controller-token")
	if err := os.WriteFile(tokenPath, []byte("controller-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	bdStore := beads.NewBdStore(t.TempDir(), func(_, _ string, _ ...string) ([]byte, error) {
		t.Fatal("private compatibility proof invoked the generic bd runner")
		return nil, nil
	}, beads.WithBdStorePrivateEvidenceHTTP(beads.PrivateEvidenceHTTPConfig{
		Endpoint: server.URL, ProjectID: "project-a", Database: "gc_fixture", ScopeRef: "city:work", TokenFile: tokenPath,
	}))
	if !bdStore.PrivateEvidencePayloadTransportReady() {
		t.Fatal("configured authenticated private-evidence transport was not ready")
	}
	// GraphStore is the actual typed outer wrapper used by controller callers.
	// Its private CAS and archive-reader handles must survive proof resolution.
	outer := beads.GraphStore{Store: bdStore}
	if writer, ok := beads.PrivateEvidenceMetadataCASWriterFor(outer); !ok || writer == nil {
		t.Fatal("outer GraphStore lost its private metadata CAS handle")
	}
	if reader, ok := beads.PrivateEvidenceArchiveReaderFor(outer); !ok || reader == nil {
		t.Fatal("outer GraphStore lost its private archive reader")
	}
	scope := validScopeForProver(t)
	policy := policyForScope(t, scope)
	prover := StoreCapabilityProver{Store: outer, StoreRef: scope.StoreRef}
	proof, err := prover.Prove(
		context.Background(), scope, policy, CapabilityAttemptEvidencePrivatePayload)
	// BdStore implements deterministic content payloads over the stock HTTP
	// routes. Proving that capability must retain the outer store's handles
	// and authenticate its exact configured scope without writing a payload.
	if err != nil || proof.Status != qualification.StatusAvailable || proof.Capability != CapabilityAttemptEvidencePrivatePayload ||
		proof.ScopeSHA256 != policy.ScopeSHA256 || proof.PolicyReference != policy.PolicyReference ||
		proof.PolicyVersion != policy.PolicyVersion || len(proof.EvidenceSHA256) != 64 {
		t.Fatalf("configured GraphStore proof = %+v, %v, want exact authenticated payload capability", proof, err)
	}
	for _, tc := range []struct {
		name    string
		context string
		list    bool
	}{
		{"missing create", strings.Replace(readyContext, `"issues.create",`, "", 1), true},
		{"missing project enforcement", strings.Replace(readyContext, `,"project.enforce"`, "", 1), true},
		{"wrong project", strings.Replace(readyContext, `"project_id":"project-a"`, `"project_id":"another-project"`, 1), true},
		{"unavailable list", readyContext, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stateMu.Lock()
			contextResponse, listReady = tc.context, tc.list
			stateMu.Unlock()
			proof, err := prover.Prove(context.Background(), scope, policy, CapabilityAttemptEvidencePrivatePayload)
			if !errors.Is(err, qualification.ErrUnavailable) || proof != (qualification.CapabilityProof{}) {
				t.Fatalf("unready GraphStore proof = %+v, %v, want unavailable", proof, err)
			}
		})
	}
}
