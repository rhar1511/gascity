package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/compatibility"
	"github.com/gastownhall/gascity/internal/qualification"
)

// composeSupervisorCompatibilityAuthority composes the optional trusted host
// authority from values captured by supervisor startup. Both the source path
// and revocation freshness bound are required; there is no local default.
func composeSupervisorCompatibilityAuthority(directory, maxRevocationAge string) (qualification.CompatibilityAuthority, io.Closer, error) {
	directory = strings.TrimSpace(directory)
	maxRevocationAge = strings.TrimSpace(maxRevocationAge)
	if directory == "" && maxRevocationAge == "" {
		return nil, nil, nil
	}
	if directory == "" || maxRevocationAge == "" {
		return nil, nil, fmt.Errorf("%s and %s must both be set by trusted supervisor startup: %w",
			compatibility.HostCompatibilityAuthorityDirectoryEnv,
			compatibility.HostCompatibilityAuthorityMaxRevocationAgeEnv,
			qualification.ErrUnavailable)
	}
	maxAge, err := time.ParseDuration(maxRevocationAge)
	if err != nil || maxAge <= 0 {
		return nil, nil, fmt.Errorf("%s must be a positive duration: %w",
			compatibility.HostCompatibilityAuthorityMaxRevocationAgeEnv,
			qualification.ErrUnavailable)
	}
	source, err := compatibility.NewFileHostCompatibilityAuthoritySource(directory)
	if err != nil {
		return nil, nil, fmt.Errorf("opening trusted host compatibility source: %w", err)
	}
	return compatibility.NewHostCompatibilityAuthority(source, maxAge, time.Now), source, nil
}

// supervisorCompatibilityAuthorityFromEnvironment is called only by the
// trusted machine-wide supervisor startup path. No city, pack, provider, or
// worker command reads these settings, and the returned source is owned by the
// supervisor until its shutdown completes.
func supervisorCompatibilityAuthorityFromEnvironment(stderr io.Writer) (qualification.CompatibilityAuthority, io.Closer) {
	authority, source, err := composeSupervisorCompatibilityAuthority(
		os.Getenv(compatibility.HostCompatibilityAuthorityDirectoryEnv),
		os.Getenv(compatibility.HostCompatibilityAuthorityMaxRevocationAgeEnv),
	)
	if err != nil {
		fmt.Fprintf(stderr, "gc supervisor: compatibility authority unavailable: %v\n", err) //nolint:errcheck
		return nil, nil
	}
	return authority, source
}
