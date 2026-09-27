package config

import (
	"fmt"
	"net"
	"net/url"
	"path/filepath"
	"strings"
)

// PrivateEvidenceTransportConfig opts one canonical Beads store scope into the
// body-only HTTP transport for immutable attempt evidence. The credential is a
// protected file path; the token value is never part of city.toml.
type PrivateEvidenceTransportConfig struct {
	Endpoint  string `toml:"endpoint" json:"endpoint"`
	ProjectID string `toml:"project_id" json:"project_id"`
	Database  string `toml:"database" json:"database"`
	TokenFile string `toml:"token_file" json:"token_file"`
}

func validatePrivateEvidenceTransports(transports map[string]PrivateEvidenceTransportConfig) error {
	for scope, transport := range transports {
		if !validPrivateEvidenceScope(scope) {
			return fmt.Errorf("beads.private_evidence: %q is not a canonical city:<name> or rig:<name> scope", scope)
		}
		if strings.TrimSpace(transport.ProjectID) == "" || strings.TrimSpace(transport.ProjectID) != transport.ProjectID ||
			strings.TrimSpace(transport.Database) == "" || strings.TrimSpace(transport.Database) != transport.Database ||
			strings.ContainsAny(transport.ProjectID+transport.Database, "\r\n\t") {
			return fmt.Errorf("beads.private_evidence.%s: project_id and database are required and must be trimmed", scope)
		}
		if strings.TrimSpace(transport.TokenFile) != transport.TokenFile || !filepath.IsAbs(transport.TokenFile) {
			return fmt.Errorf("beads.private_evidence.%s: token_file must be an absolute path", scope)
		}
		if !validPrivateEvidenceEndpoint(transport.Endpoint) {
			return fmt.Errorf("beads.private_evidence.%s: endpoint must be an HTTPS origin or literal loopback HTTP origin", scope)
		}
	}
	return nil
}

func validPrivateEvidenceScope(scope string) bool {
	if strings.TrimSpace(scope) != scope || strings.ContainsAny(scope, "\r\n\t") {
		return false
	}
	for _, prefix := range []string{"city:", "rig:"} {
		if strings.HasPrefix(scope, prefix) && strings.TrimSpace(strings.TrimPrefix(scope, prefix)) != "" {
			return true
		}
	}
	return false
}

func validPrivateEvidenceEndpoint(endpoint string) bool {
	if strings.TrimSpace(endpoint) == "" || strings.TrimSpace(endpoint) != endpoint || len(endpoint) > 2048 {
		return false
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed == nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return false
	}
	switch strings.ToLower(parsed.Scheme) {
	case "https":
		return true
	case "http":
		ip := net.ParseIP(parsed.Hostname())
		return ip != nil && ip.IsLoopback()
	default:
		return false
	}
}
