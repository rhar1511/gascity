package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

type privateEvidenceConfigProbe struct {
	endpoint string
	project  string
	database string
	token    string
	hits     atomic.Int32
}

func (p *privateEvidenceConfigProbe) serve(w http.ResponseWriter, r *http.Request) {
	p.hits.Add(1)
	if r.Header.Get("Authorization") != "Bearer "+p.token || r.Header.Get("Bd-Project-Id") != p.project {
		http.Error(w, "wrong controller binding", http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/v0/beads/context":
		_ = json.NewEncoder(w).Encode(map[string]any{
			"api_version": "v0", "backend": "dolt", "bd_version": "1.3.0",
			"capabilities": []string{"issues.casMetadata", "issues.create", "issues.get", "project.enforce"},
			"database":     p.database, "dolt_mode": "server", "project_id": p.project,
		})
	case "/v0/beads/issues":
		_ = json.NewEncoder(w).Encode(map[string]any{"items": []any{}, "has_more": false})
	default:
		http.NotFound(w, r)
	}
}

func TestBdStoreConstructorsSelectPrivateEvidenceByExactCityAndRigScope(t *testing.T) {
	cityProbe := &privateEvidenceConfigProbe{project: "project-city", database: "city-db", token: "city-controller-token"}
	cityServer := httptest.NewServer(http.HandlerFunc(cityProbe.serve))
	defer cityServer.Close()
	cityProbe.endpoint = cityServer.URL
	rigProbe := &privateEvidenceConfigProbe{project: "project-rig", database: "rig-db", token: "rig-controller-token"}
	rigServer := httptest.NewServer(http.HandlerFunc(rigProbe.serve))
	defer rigServer.Close()
	rigProbe.endpoint = rigServer.URL

	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "rigs", "repo")
	for _, dir := range []string{cityDir, rigDir} {
		if err := os.MkdirAll(filepath.Join(dir, ".beads"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(scopeMetadataJSONPath(dir), []byte(`{"backend":"postgres","storage_endpoint":"opaque-remote","storage_database":"work","dolt_mode":"server"}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cityToken := filepath.Join(t.TempDir(), "city-token")
	rigToken := filepath.Join(t.TempDir(), "rig-token")
	for path, token := range map[string]string{cityToken: cityProbe.token, rigToken: rigProbe.token} {
		if err := os.WriteFile(path, []byte(token+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "demo"},
		Rigs:      []config.Rig{{Name: "repo", Path: "rigs/repo", Prefix: "repo"}},
		Beads: config.BeadsConfig{PrivateEvidence: map[string]config.PrivateEvidenceTransportConfig{
			"city:demo": {Endpoint: cityProbe.endpoint, ProjectID: cityProbe.project, Database: cityProbe.database, TokenFile: cityToken},
			"rig:repo":  {Endpoint: rigProbe.endpoint, ProjectID: rigProbe.project, Database: rigProbe.database, TokenFile: rigToken, RevisionTransitions: true},
		}},
	}

	tests := []struct {
		name            string
		want            *privateEvidenceConfigProbe
		wantTransitions bool
		construct       func() (*beads.BdStore, error)
	}{
		{name: "normal city", want: cityProbe, construct: func() (*beads.BdStore, error) {
			return bdStoreForCityWithConfig(cityDir, cityDir, cfg), nil
		}},
		{name: "control city", want: cityProbe, construct: func() (*beads.BdStore, error) {
			return controlBdStoreForCity(cityDir, cityDir, cfg), nil
		}},
		{name: "scoped city", want: cityProbe, construct: func() (*beads.BdStore, error) {
			return scopedBdStoreForCity(context.Background(), cityDir, cfg)
		}},
		{name: "normal rig", want: rigProbe, wantTransitions: true, construct: func() (*beads.BdStore, error) {
			return bdStoreForRig(rigDir, cityDir, cfg), nil
		}},
		{name: "control rig", want: rigProbe, wantTransitions: true, construct: func() (*beads.BdStore, error) {
			return controlBdStoreForRig(rigDir, cityDir, cfg), nil
		}},
		{name: "scoped rig", want: rigProbe, wantTransitions: true, construct: func() (*beads.BdStore, error) {
			return scopedBdStoreForRig(context.Background(), cityDir, cfg, rigDir)
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cityBefore, rigBefore := cityProbe.hits.Load(), rigProbe.hits.Load()
			store, err := tc.construct()
			if err != nil {
				t.Fatalf("construct store: %v", err)
			}
			if !store.PrivateEvidencePayloadTransportReady() {
				t.Fatal("constructed BdStore did not bind the configured private evidence service")
			}
			writer, transitions := beads.ControllerMetadataTransitionWriterFor(store)
			if transitions != tc.wantTransitions || transitions != (writer != nil) {
				t.Fatalf("revision transition capability = (%T, %v), want enabled %v", writer, transitions, tc.wantTransitions)
			}
			if tc.want == cityProbe {
				if cityProbe.hits.Load() <= cityBefore || rigProbe.hits.Load() != rigBefore {
					t.Fatalf("city probe calls changed from %d to %d; rig probe from %d to %d",
						cityBefore, cityProbe.hits.Load(), rigBefore, rigProbe.hits.Load())
				}
			} else if rigProbe.hits.Load() <= rigBefore || cityProbe.hits.Load() != cityBefore {
				t.Fatalf("rig probe calls changed from %d to %d; city probe from %d to %d",
					rigBefore, rigProbe.hits.Load(), cityBefore, cityProbe.hits.Load())
			}
		})
	}
}
