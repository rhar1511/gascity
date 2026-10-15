package mcpskills

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func serverFixture(t *testing.T) *Server {
	t.Helper()
	c, err := Load(map[string]string{"team": fixture(t)})
	if err != nil {
		t.Fatal(err)
	}
	s, err := NewServer(c, 1)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// One SDK client/server boundary proves wire registration, pagination and raw
// resource delivery; catalog tests own filesystem/manifest edge cases.
func TestSDKClientDiscoversAndReadsCatalog(t *testing.T) {
	s := serverFixture(t)
	ctx := context.Background()
	ct, st := mcp.NewInMemoryTransports()
	ss, err := s.SDK.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := ss.Close(); err != nil {
			t.Errorf("close server session: %v", err)
		}
	})
	client := mcp.NewClient(&mcp.Implementation{Name: "skills-test", Version: "1"}, nil)
	if err := mcp.AddSendingCustomMethod[*ListParams, *SkillsResult](client, "skills/list"); err != nil {
		t.Fatal(err)
	}
	if err := mcp.AddSendingCustomMethod[*GetParams, *SkillResult](client, "skills/get"); err != nil {
		t.Fatal(err)
	}
	cs, err := client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := cs.Close(); err != nil {
			t.Errorf("close client session: %v", err)
		}
	})
	if _, ok := cs.InitializeResult().Capabilities.Extensions[Extension]; !ok {
		t.Fatal("extension not declared")
	}
	listing, err := mcp.CallCustomMethod[*ListParams, *SkillsResult](ctx, cs, "skills/list", &ListParams{})
	if err != nil {
		t.Fatal(err)
	}
	if len(listing.Skills) != 1 || listing.NextCursor == "" || listing.CacheScope != "private" || listing.ResultType != "complete" {
		t.Fatalf("list: %+v", listing)
	}
	next, err := mcp.CallCustomMethod[*ListParams, *SkillsResult](ctx, cs, "skills/list", &ListParams{Cursor: listing.NextCursor})
	if err != nil || len(next.Skills) != 1 || next.NextCursor != "" {
		t.Fatal("second page", err)
	}
	got, err := mcp.CallCustomMethod[*GetParams, *SkillResult](ctx, cs, "skills/get", &GetParams{URI: "skill://team/review/SKILL.md"})
	if err != nil || len(got.Skill.Resources) != 5 {
		t.Fatal("get manifest", err)
	}
	read, err := cs.ReadResource(ctx, &mcp.ReadResourceParams{URI: "skill://team/review/asset.bin"})
	if err != nil || !bytes.Equal(read.Contents[0].Blob, []byte{255, 0, 1}) {
		t.Fatal("binary roundtrip", err)
	}
	if _, err := mcp.CallCustomMethod[*GetParams, *SkillResult](ctx, cs, "skills/get", &GetParams{URI: "file:///etc/passwd"}); err == nil {
		t.Fatal("unlisted URI accepted")
	}
	if _, err := mcp.CallCustomMethod[*ListParams, *SkillsResult](ctx, cs, "skills/list", &ListParams{Cursor: "bad"}); err == nil {
		t.Fatal("bad cursor accepted")
	}
}

func modernRequest(method, uri string) []byte {
	type meta struct {
		Version      string   `json:"io.modelcontextprotocol/protocolVersion"`
		Capabilities struct{} `json:"io.modelcontextprotocol/clientCapabilities"`
	}
	type params struct {
		URI  string `json:"uri,omitempty"`
		Meta meta   `json:"_meta"`
	}
	b, _ := json.Marshal(struct {
		JSONRPC string `json:"jsonrpc"`
		ID      int    `json:"id"`
		Method  string `json:"method"`
		Params  params `json:"params"`
	}{"2.0", 1, method, params{URI: uri, Meta: meta{Version: "2026-07-28"}}})
	return b
}

func TestModernHTTPDiscoveryAndProtection(t *testing.T) {
	s := serverFixture(t)
	handler, err := s.HTTPHandler("test-token", []string{"https://trusted.example"})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, token, origin, version, method, verb string
		status                                     int
	}{
		{"valid", "test-token", "", "2026-07-28", "server/discover", "POST", 200},
		{"no auth", "", "", "2026-07-28", "server/discover", "POST", 401},
		{"bad origin", "test-token", "https://evil.example", "2026-07-28", "server/discover", "POST", 403},
		{"missing version", "test-token", "", "", "server/discover", "POST", 400},
		{"mismatch", "test-token", "", "2026-07-28", "resources/read", "POST", 400},
		{"get", "test-token", "", "2026-07-28", "server/discover", "GET", 405},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(tc.verb, "http://localhost/mcp", bytes.NewReader(modernRequest("server/discover", "")))
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("Accept", "application/json, text/event-stream")
			r.Header.Set("Authorization", "Bearer "+tc.token)
			r.Header.Set("Origin", tc.origin)
			r.Header.Set("MCP-Protocol-Version", tc.version)
			r.Header.Set("Mcp-Method", tc.method)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != tc.status {
				t.Fatalf("%d: %s", w.Code, w.Body.String())
			}
			if tc.status == 200 {
				var response struct {
					Result struct {
						Capabilities mcp.ServerCapabilities
						CacheScope   string
					}
				}
				if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
					t.Fatal(err)
				}
				if _, ok := response.Result.Capabilities.Extensions[Extension]; !ok || response.Result.CacheScope != "private" {
					t.Fatalf("discovery: %s", w.Body.String())
				}
			}
		})
	}
	if _, err := s.HTTPHandler("", nil); err == nil {
		t.Fatal("unauthenticated HTTP allowed")
	}
}

func TestModernHTTPEmptyResourceAndFullManifest(t *testing.T) {
	s := serverFixture(t)
	handler, err := s.HTTPHandler("test-token", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, method := range []string{"skills/list", "skills/get", "resources/read"} {
		uri := "skill://team/review/SKILL.md"
		if method == "resources/read" {
			uri = "skill://team/review/empty.txt"
		}
		r := httptest.NewRequest("POST", "http://localhost/mcp", bytes.NewReader(modernRequest(method, uri)))
		r.Header.Set("Authorization", "Bearer test-token")
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Accept", "application/json, text/event-stream")
		r.Header.Set("MCP-Protocol-Version", "2026-07-28")
		r.Header.Set("Mcp-Method", method)
		if method == "resources/read" {
			r.Header.Set("Mcp-Name", uri)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != 200 {
			t.Fatalf("%s: %d %s", method, w.Code, w.Body.String())
		}
		if method == "resources/read" && !bytes.Contains(w.Body.Bytes(), []byte(`"blob":""`)) {
			t.Fatalf("empty bytes omitted on wire: %s", w.Body.String())
		}
	}
}
