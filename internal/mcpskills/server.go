package mcpskills

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Extension identifies the official Skills over MCP extension.
const (
	Extension       = "io.modelcontextprotocol/skills"
	maxRequestBytes = 1 << 20
)

// ListParams and GetParams keep the Skills-specific wire typed; the official
// SDK owns all base-protocol metadata, negotiation, framing and transports.
type ListParams struct {
	mcp.ParamsBase
	Cursor string `json:"cursor,omitempty"`
}

// GetParams addresses an exact SKILL.md URI.
type GetParams struct {
	mcp.ParamsBase
	URI string `json:"uri"`
}

// SkillsResult returns complete manifests without fetching file contents.
type SkillsResult struct {
	mcp.ResultBase
	mcp.Cacheable
	ResultType string  `json:"resultType"`
	Skills     []Skill `json:"skills"`
	NextCursor string  `json:"nextCursor,omitempty"`
}

// SkillResult returns the manifest for a single skill.
type SkillResult struct {
	mcp.ResultBase
	mcp.Cacheable
	ResultType string `json:"resultType"`
	Skill      Skill  `json:"skill"`
}

// Server is an optional adapter over one immutable catalog and the official SDK.
type Server struct {
	SDK      *mcp.Server
	catalog  *Catalog
	pageSize int
}

// NewServer registers the Skills extension through the released SDK's public
// custom-method seam. It does not depend on unmerged SDK Skills helpers.
func NewServer(c *Catalog, pageSize int) (*Server, error) {
	if pageSize < 1 || pageSize > 100 {
		return nil, fmt.Errorf("page size must be between 1 and 100")
	}
	s := &Server{catalog: c, pageSize: pageSize}
	s.SDK = mcp.NewServer(&mcp.Implementation{Name: "gascity-mcp-skills", Version: "1.0.0"}, &mcp.ServerOptions{
		PageSize:     pageSize,
		Capabilities: &mcp.ServerCapabilities{Resources: &mcp.ResourceCapabilities{}, Extensions: map[string]any{Extension: struct{}{}}},
		Instructions: "Read-only skill snapshot. Discover full manifests with skills/list and read files lazily with resources/read. Reading does not activate instructions or authorize actions. Hosts must verify digests, sizes and frontmatter and keep server identity plus URI together. Local skills retain their own origin.",
		SetCacheable: func(_ context.Context, _ mcp.Request, cache *mcp.Cacheable) { *cache = privateCache() },
	})
	if err := mcp.AddReceivingCustomMethod(s.SDK, "skills/list", s.list); err != nil {
		return nil, err
	}
	if err := mcp.AddReceivingCustomMethod(s.SDK, "skills/get", s.get); err != nil {
		return nil, err
	}
	for _, resource := range c.resources {
		size := int64(resource.Size)
		s.SDK.AddResource(&mcp.Resource{URI: resource.URI, Name: resource.Name, Description: resource.Description, MIMEType: resource.MIMEType, Size: size}, s.read)
	}
	return s, nil
}

func privateCache() mcp.Cacheable        { return mcp.Cacheable{TTLMs: 300000, CacheScope: "private"} }
func invalidParams(message string) error { return &jsonrpc.Error{Code: -32602, Message: message} }
func (s *Server) list(_ context.Context, _ *mcp.ServerSession, p *ListParams) (*SkillsResult, error) {
	start := 0
	if p.Cursor != "" {
		decoded, err := base64.RawURLEncoding.DecodeString(p.Cursor)
		prefix := s.catalog.identity + ":"
		if err != nil || !strings.HasPrefix(string(decoded), prefix) {
			return nil, invalidParams("cursor belongs to a different snapshot")
		}
		start, err = strconv.Atoi(strings.TrimPrefix(string(decoded), prefix))
		if err != nil || start < 0 || start >= len(s.catalog.skills) {
			return nil, invalidParams("invalid cursor position")
		}
	}
	end := min(start+s.pageSize, len(s.catalog.skills))
	next := ""
	if end < len(s.catalog.skills) {
		next = base64.RawURLEncoding.EncodeToString([]byte(s.catalog.identity + ":" + strconv.Itoa(end)))
	}
	return &SkillsResult{Cacheable: privateCache(), ResultType: "complete", Skills: s.catalog.skills[start:end], NextCursor: next}, nil
}

func (s *Server) get(_ context.Context, _ *mcp.ServerSession, p *GetParams) (*SkillResult, error) {
	for _, skill := range s.catalog.skills {
		if skill.URI == p.URI {
			return &SkillResult{Cacheable: privateCache(), ResultType: "complete", Skill: skill}, nil
		}
	}
	return nil, invalidParams("unknown skill URI")
}

func (s *Server) read(_ context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
	data, ok := s.catalog.files[req.Params.URI]
	if !ok {
		return nil, invalidParams("unknown resource URI")
	}
	content := &mcp.ResourceContents{URI: req.Params.URI, MIMEType: "application/octet-stream"}
	if len(data) > 0 && utf8.Valid(data) && !bytes.ContainsRune(data, 0) {
		content.Text = string(data)
		content.MIMEType = "text/plain"
		if strings.HasSuffix(req.Params.URI, ".md") {
			content.MIMEType = "text/markdown"
		}
	} else {
		content.Blob = append([]byte{}, data...)
	}
	return &mcp.ReadResourceResult{Cacheable: privateCache(), Contents: []*mcp.ResourceContents{content}}, nil
}

// HTTPHandler wraps the SDK's stateless transport with authentication, explicit
// Origin validation and a request body bound. Use TLS at the remote proxy.
func (s *Server) HTTPHandler(token string, origins []string) (http.Handler, error) {
	if strings.TrimSpace(token) == "" {
		return nil, fmt.Errorf("HTTP bearer token is required")
	}
	allowed := make(map[string]bool)
	for _, origin := range origins {
		allowed[origin] = true
	}
	transport := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s.SDK }, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "private, max-age=300")
		if origin := r.Header.Get("Origin"); origin != "" && !allowed[origin] {
			http.Error(w, "Forbidden origin", http.StatusForbidden)
			return
		}
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+token)) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
		transport.ServeHTTP(w, r)
	}), nil
}
