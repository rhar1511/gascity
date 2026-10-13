package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/rollout/gate"
	"github.com/gastownhall/gascity/internal/runtime"
)

func TestControllerBdStoresInstallWriterForExactCityAndRigScopes(t *testing.T) {
	t.Run("city scope", func(t *testing.T) {
		cityDir := t.TempDir()
		server, transport := controllerPermitTestTransport(t)
		defer server.Close()
		entry, key := hostBeadsPermitTestEntry(t, "alpha", "city:alpha", "city-key", "city-key.pem")
		entry.ProjectID, entry.Database = "project-alpha", "db-alpha"
		resolver := controllerPermitTestResolver(t, entry, key)
		cfg := controllerPermitTestConfig(transport)

		leaf := controllerPermitTestBdStore(cityDir, "city:alpha", transport)
		store := beads.NewCachingStore(leaf, nil)
		cs := &controllerState{cityName: "alpha", cityPath: cityDir, cityBeadStore: store}
		if err := configureControllerProtectedDecisionFrontierStores(cs, cfg, resolver); err != nil {
			t.Fatalf("configure controller city BdStore: %v", err)
		}
		assertControllerDecisionFrontierWriter(t, store)
	})

	t.Run("rig scope", func(t *testing.T) {
		cityDir := t.TempDir()
		rigDir := filepath.Join(cityDir, "repo")
		server, transport := controllerPermitTestTransport(t)
		defer server.Close()
		entry, key := hostBeadsPermitTestEntry(t, "alpha", "rig:repo", "rig-key", "rig-key.pem")
		entry.ProjectID, entry.Database = "project-alpha", "db-alpha"
		resolver := controllerPermitTestResolver(t, entry, key)
		cfg := controllerPermitTestConfig(transport)
		cfg.Beads.PrivateEvidence["rig:repo"] = transport
		cfg.Rigs = []config.Rig{{Name: "repo", Path: rigDir, Prefix: "rp"}}

		leaf := controllerPermitTestBdStore(rigDir, "rig:repo", transport)
		store := wrapStoreWithBeadPolicies(beads.NewCachingStore(leaf, nil), cfg)
		cs := &controllerState{cityName: "alpha", cityPath: cityDir, beadStores: map[string]beads.Store{"repo": store}}
		if err := configureControllerProtectedDecisionFrontierStores(cs, cfg, resolver); err != nil {
			t.Fatalf("configure controller rig BdStore: %v", err)
		}
		assertControllerDecisionFrontierWriter(t, store)
	})
}

func TestControllerBdStoreRefusesBrokenMatchingAuthorityAndOmitsUnmatchedAuthority(t *testing.T) {
	cityDir := t.TempDir()
	server, validTransport := controllerPermitTestTransport(t)
	defer server.Close()
	entry, key := hostBeadsPermitTestEntry(t, "alpha", "city:alpha", "city-key", "city-key.pem")
	entry.ProjectID, entry.Database = "project-alpha", "db-alpha"
	resolver := controllerPermitTestResolver(t, entry, key)

	cases := []struct {
		name      string
		transport map[string]config.PrivateEvidenceTransportConfig
		wantError bool
	}{
		{name: "missing entry", transport: nil, wantError: true},
		{name: "revision transitions disabled", transport: controllerPermitTestTransportConfig(validTransport, false), wantError: true},
		{name: "project mismatch", transport: controllerPermitTestTransportConfigWithIdentity(validTransport, "other-project", "db-alpha"), wantError: true},
		{name: "database mismatch", transport: controllerPermitTestTransportConfigWithIdentity(validTransport, "project-alpha", "other-db"), wantError: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.City{Workspace: config.Workspace{Name: "alpha"}, Beads: config.BeadsConfig{PrivateEvidence: tc.transport}}
			leaf := controllerPermitTestBdStore(cityDir, "city:alpha", validTransport)
			cs := &controllerState{cityName: "alpha", cityPath: cityDir, cityBeadStore: leaf}
			err := configureControllerProtectedDecisionFrontierStores(cs, cfg, resolver)
			if tc.wantError {
				if err == nil {
					t.Fatal("configured controller BdStore authority was not refused")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
		})
	}

	unmatchedEntry, unmatchedKey := hostBeadsPermitTestEntry(t, "other-city", "city:other-city", "other-key", "other-key.pem")
	unmatchedResolver := controllerPermitTestResolver(t, unmatchedEntry, unmatchedKey)
	cfg := controllerPermitTestConfig(validTransport)
	store := controllerPermitTestBdStore(cityDir, "city:alpha", validTransport)
	cs := &controllerState{cityName: "alpha", cityPath: cityDir, cityBeadStore: store}
	if err := configureControllerProtectedDecisionFrontierStores(cs, cfg, unmatchedResolver); err != nil {
		t.Fatalf("configure unmatched controller scope: %v", err)
	}
	if writer, ok := beads.DecisionFrontierRecordWriterFor(store); ok || writer != nil {
		t.Fatalf("unmatched host scope advertised a protected writer: (%T, %t)", writer, ok)
	}
}

func controllerPermitTestBdStore(dir, storeRef string, transport config.PrivateEvidenceTransportConfig) *beads.BdStore {
	return beads.NewBdStore(dir, func(string, string, ...string) ([]byte, error) {
		return []byte("[]"), nil
	}, beads.WithBdStorePrivateEvidenceHTTP(beads.PrivateEvidenceHTTPConfig{
		Endpoint: transport.Endpoint, ProjectID: transport.ProjectID, Database: transport.Database,
		ScopeRef: storeRef, TokenFile: transport.TokenFile, RevisionTransitions: transport.RevisionTransitions,
	}))
}

func TestOneShotBdStoreDoesNotInstallHostProtectedWriter(t *testing.T) {
	cityDir := t.TempDir()
	server, transport := controllerPermitTestTransport(t)
	defer server.Close()
	_, key := hostBeadsPermitTestEntry(t, "alpha", "city:alpha", "city-key", "city-key.pem")
	_ = key // The one-shot open receives no host resolver or permit issuer.
	cfg := controllerPermitTestConfig(transport)
	store, err := openBdStoreAtWithConfig(cityDir, cityDir, cfg)
	if err != nil {
		t.Fatalf("open one-shot BdStore: %v", err)
	}
	if writer, ok := beads.DecisionFrontierRecordWriterFor(store); ok || writer != nil {
		t.Fatalf("one-shot BdStore advertised a protected writer: (%T, %t)", writer, ok)
	}
}

func TestControllerBdStoreReloadRevalidatesProtectedAuthorityBeforePublish(t *testing.T) {
	cityDir := t.TempDir()
	server, transport := controllerPermitTestTransport(t)
	defer server.Close()
	entry, key := hostBeadsPermitTestEntry(t, "alpha", "city:alpha", "city-key", "city-key.pem")
	entry.ProjectID, entry.Database = transport.ProjectID, transport.Database
	resolver := controllerPermitTestResolver(t, entry, key)
	current := controllerPermitTestConfig(transport)
	oldLeaf := controllerPermitTestBdStore(cityDir, "city:alpha", transport)
	oldStore := beads.NewCachingStore(oldLeaf, nil)
	cs := &controllerState{
		cfg: current, cacheCtx: t.Context(), cityName: "alpha", cityPath: cityDir,
		cityBeadStore: oldStore, beadStores: map[string]beads.Store{},
		storeMetadataSignature: storeMetadataSignature(cityDir, current),
	}
	if err := configureControllerProtectedDecisionFrontierStores(cs, current, resolver); err != nil {
		t.Fatalf("configure initial protected writer: %v", err)
	}

	previousOpen := newControllerStateOpenCityStore
	t.Cleanup(func() { newControllerStateOpenCityStore = previousOpen })
	var replacement *beads.BdStore
	newControllerStateOpenCityStore = func(string, gate.Mode) (beads.StoreOpenResult, error) {
		return beads.StoreOpenResult{Store: replacement}, nil
	}

	t.Run("valid replacement advertises writer", func(t *testing.T) {
		next := controllerPermitTestConfig(transport)
		nextTransport := next.Beads.PrivateEvidence["city:alpha"]
		nextTransport.TokenFile = filepath.Join(t.TempDir(), "next-token")
		if err := os.WriteFile(nextTransport.TokenFile, []byte("test-token\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		next.Beads.PrivateEvidence["city:alpha"] = nextTransport
		replacement = controllerPermitTestBdStore(cityDir, "city:alpha", nextTransport)

		if _, err := cs.updateFromRuntime(next, runtime.NewFake(), ""); err != nil {
			t.Fatalf("valid protected-authority reload: %v", err)
		}

		if cs.Config() != next {
			t.Fatal("valid protected-authority reload did not publish the new config")
		}
		if got := cs.CityBeadStore(); got == oldStore {
			t.Fatal("valid protected-authority reload retained the old city store")
		} else {
			assertControllerDecisionFrontierWriter(t, got)
		}
	})

	t.Run("mismatched replacement retains live snapshot", func(t *testing.T) {
		liveConfig := cs.Config()
		liveStore := cs.CityBeadStore()
		invalid := controllerPermitTestConfig(transport)
		invalidTransport := invalid.Beads.PrivateEvidence["city:alpha"]
		invalidTransport.Database = "wrong-database"
		invalid.Beads.PrivateEvidence["city:alpha"] = invalidTransport
		replacement = controllerPermitTestBdStore(cityDir, "city:alpha", invalidTransport)

		if _, err := cs.updateFromRuntime(invalid, runtime.NewFake(), ""); err == nil {
			t.Fatal("mismatched protected-authority reload returned success")
		}

		if cs.Config() != liveConfig {
			t.Fatal("mismatched protected-authority reload published its config")
		}
		if got := cs.CityBeadStore(); got != liveStore {
			t.Fatal("mismatched protected-authority reload replaced the live city store")
		}
		assertControllerDecisionFrontierWriter(t, liveStore)
	})
}

func assertControllerDecisionFrontierWriter(t *testing.T, store beads.Store) {
	t.Helper()
	writer, ok := beads.DecisionFrontierRecordWriterFor(store)
	if !ok || writer == nil {
		t.Fatalf("controller store %T did not advertise the complete decision-frontier record writer", store)
	}
}

func controllerPermitTestTransport(t *testing.T) (*httptest.Server, config.PrivateEvidenceTransportConfig) {
	t.Helper()
	const projectID = "project-alpha"
	const database = "db-alpha"
	capabilities := []string{
		"issues.batchApply", "issues.batchApplyReceipt", "issues.casMetadata", "issues.create", "issues.get",
		"issues.protectedMutation", "issues.sourceSnapshot", "issues.transitionMetadata", "issues.transitionReceipt.get", "project.enforce",
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v0/beads/context":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"api_version": "v0", "backend": "dolt", "bd_version": "1.0.4",
				"capabilities": capabilities, "database": database, "dolt_mode": "server", "project_id": projectID,
			})
		case r.Method == http.MethodGet && r.URL.Path == "/v0/beads/issues":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"items":[],"has_more":false,"next_cursor":""}`))
		default:
			http.NotFound(w, r)
		}
	}))
	tokenFile := filepath.Join(t.TempDir(), "beads-token")
	if err := os.WriteFile(tokenFile, []byte("test-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return server, config.PrivateEvidenceTransportConfig{
		Endpoint: server.URL, ProjectID: projectID, Database: database,
		TokenFile: tokenFile, RevisionTransitions: true,
	}
}

func controllerPermitTestTransportConfig(endpoint config.PrivateEvidenceTransportConfig, revisionTransitions bool) map[string]config.PrivateEvidenceTransportConfig {
	endpoint.RevisionTransitions = revisionTransitions
	return map[string]config.PrivateEvidenceTransportConfig{"city:alpha": endpoint}
}

func controllerPermitTestTransportConfigWithIdentity(endpoint config.PrivateEvidenceTransportConfig, projectID, database string) map[string]config.PrivateEvidenceTransportConfig {
	endpoint.ProjectID, endpoint.Database = projectID, database
	return map[string]config.PrivateEvidenceTransportConfig{"city:alpha": endpoint}
}

func controllerPermitTestConfig(transport config.PrivateEvidenceTransportConfig) *config.City {
	const cityName = "alpha"
	return &config.City{
		Workspace: config.Workspace{Name: cityName},
		Beads:     config.BeadsConfig{PrivateEvidence: map[string]config.PrivateEvidenceTransportConfig{"city:" + cityName: transport}},
	}
}

func TestHostProtectedDecisionFrontierWriterRejectsChangedAuthorityRoot(t *testing.T) {
	entry, key := hostBeadsPermitTestEntry(t, "alpha", "city:alpha", "city-key", "city-key.pem")
	resolver := controllerPermitTestResolver(t, entry, key)
	directory := resolver.source.directory
	backup := directory + ".old"
	if err := os.Rename(directory, backup); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, _, err := hostProtectedDecisionFrontierWriterForStore(resolver, nil, "alpha", "city:alpha", nil); err == nil || !strings.Contains(err.Error(), "authority") {
		t.Fatalf("changed host authority error = %v, want unavailable authority", err)
	}
}
