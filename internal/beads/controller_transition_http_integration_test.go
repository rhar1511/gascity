//go:build integration

package beads

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
