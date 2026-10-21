package contract

import (
	"encoding/base64"
	"errors"
	"strings"

	"golang.org/x/mod/semver"
)

// ErrBDContextProtocol distinguishes a malformed/unsupported response from an
// unreachable command. Protocol failure must never gain identity fallback.
var ErrBDContextProtocol = errors.New("bd context JSON protocol invalid")

// BeadsArtifact is the actual Go module identity of a CLI or linked library.
// It comes from Go build info, never from bd JSON, display versions or metadata.
// Release provenance and canonical module content are qualified before install.
type BeadsArtifact struct {
	ModulePath string
	Version    string
	Sum        string
	Replaced   bool
}

func (a BeadsArtifact) unconfirmedSourceReason() string {
	switch {
	case a.ModulePath != beadsModulePath:
		return "module family is missing or unsupported"
	case a.Replaced:
		return "replaced module"
	case !comparableReleaseVersion(a.Version) || semver.Canonical(a.Version) != a.Version:
		return "module version is not an exact released version"
	}
	if !strings.HasPrefix(a.Sum, "h1:") {
		return "module content checksum is missing"
	}
	digest, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(a.Sum, "h1:"))
	if err != nil || len(digest) != 32 {
		return "module content checksum is invalid"
	}
	return ""
}
