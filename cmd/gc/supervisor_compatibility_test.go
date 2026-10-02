package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/compatibility"
	"github.com/gastownhall/gascity/internal/qualification"
)

func TestSupervisorCompatibilityAuthorityRequiresExplicitHostPolicy(t *testing.T) {
	tests := []struct {
		name      string
		directory string
		maxAge    string
		wantErr   bool
	}{
		{name: "disabled by default"},
		{name: "missing directory", maxAge: "1h", wantErr: true},
		{name: "missing max age", directory: "/host/authority", wantErr: true},
		{name: "invalid max age", directory: "/host/authority", maxAge: "tomorrow", wantErr: true},
		{name: "zero max age", directory: "/host/authority", maxAge: "0s", wantErr: true},
		{name: "negative max age", directory: "/host/authority", maxAge: "-1m", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			authority, source, err := composeSupervisorCompatibilityAuthority(tt.directory, tt.maxAge)
			if tt.wantErr {
				if err == nil {
					t.Fatal("composeSupervisorCompatibilityAuthority error = nil, want configuration error")
				}
				if authority != nil || source != nil {
					t.Fatalf("invalid policy returned authority %T and source %T", authority, source)
				}
				return
			}
			if err != nil || authority != nil || source != nil {
				t.Fatalf("disabled composition = (%T, %T, %v), want nil authority and source", authority, source, err)
			}
		})
	}
}

func TestSupervisorCompatibilityAuthorityUsesOnlyPinnedHostSource(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "authority")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	authority, source, err := composeSupervisorCompatibilityAuthority(directory, "15m")
	if err != nil {
		t.Fatal(err)
	}
	if authority == nil || source == nil {
		t.Fatalf("composition = (%T, %T), want authority and owned source", authority, source)
	}
	fileSource, ok := source.(compatibility.FileHostCompatibilityAuthoritySource)
	if !ok {
		t.Fatalf("source = %T, want file host source", source)
	}
	if _, err := fileSource.Load(context.Background()); !errors.Is(err, qualification.ErrUnavailable) {
		t.Fatalf("empty host trust directory load error = %v, want unavailable", err)
	}
	if err := source.Close(); err != nil {
		t.Fatalf("closing host trust source: %v", err)
	}
}

func TestSupervisorCompatibilityAuthorityRejectsUntrustedDirectoryPath(t *testing.T) {
	authority, source, err := composeSupervisorCompatibilityAuthority("relative/authority", "15m")
	if !errors.Is(err, qualification.ErrUnavailable) {
		t.Fatalf("compose error = %v, want unavailable", err)
	}
	if authority != nil || source != nil {
		t.Fatalf("invalid directory returned authority %T and source %T", authority, source)
	}
}
