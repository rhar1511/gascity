//go:build integration

package compatibility

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/qualification"
)

func TestStoreCapabilityProverKeepsConfiguredBdStoreUnavailableWithoutContentPayloads(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer controller-secret" || r.Header.Get("Bd-Project-Id") != "project-a" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v0/beads/context":
			_, _ = w.Write([]byte(`{"api_version":"v0","backend":"dolt","bd_version":"1.3.0","capabilities":["issues.casMetadata","issues.create","issues.get","project.enforce"],"database":"gc_fixture","dolt_mode":"server","project_id":"project-a"}`))
		case "/v0/beads/issues":
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
	proof, err := (StoreCapabilityProver{Store: outer, StoreRef: scope.StoreRef}).Prove(
		context.Background(), scope, policy, CapabilityAttemptEvidencePrivatePayload)
	// Archive-body readiness and outer handle forwarding do not prove the
	// separate durable content-addressed payload-row contract.
	if !errors.Is(err, qualification.ErrUnavailable) || proof != (qualification.CapabilityProof{}) {
		t.Fatalf("configured GraphStore proof = %+v, %v, want unavailable without content payloads", proof, err)
	}
}
