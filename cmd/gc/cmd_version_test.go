package main

import (
	"context"
	"runtime/debug"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/qualification"
)

func TestNormalizeVersion(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{in: "v0.13.0", want: "0.13.0"},
		{in: "0.13.0", want: "0.13.0"},
		{in: "v0.13.0-rc2.0.20260317225312-41a12e4914cb+dirty", want: "0.13.0-rc2"},
		{in: "v0.0.0-20260317225312-41a12e4914cb", want: "dev"},
		{in: "(devel)", want: "dev"},
		{in: "", want: "dev"},
		// SemVer build metadata must be preserved.
		{in: "1.3.5+ra.1", want: "1.3.5+ra.1"},
		{in: "1.3.5", want: "1.3.5"},
		// Pseudo-version with a newer timestamp still collapses.
		{in: "v0.0.0-20260719191849-4c2927134266", want: "dev"},
		// +incompatible is the one Go-specific suffix we strip.
		{in: "1.2.3+incompatible", want: "1.2.3"},
	}
	for _, tt := range tests {
		if got := normalizeVersion(tt.in); got != tt.want {
			t.Fatalf("normalizeVersion(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestResolveBuildMetadataUsesModuleVersion(t *testing.T) {
	info := &debug.BuildInfo{
		Main: debug.Module{
			Version: "v0.13.0",
		},
	}
	version, commit, date := resolveBuildMetadata("dev", "unknown", "unknown", true, info)
	if version != "0.13.0" {
		t.Fatalf("version = %q, want %q", version, "0.13.0")
	}
	if commit != "unknown" {
		t.Fatalf("commit = %q, want unknown", commit)
	}
	if date != "unknown" {
		t.Fatalf("date = %q, want unknown", date)
	}
}

func TestResolveBuildMetadataUsesVCSSettings(t *testing.T) {
	info := &debug.BuildInfo{
		Settings: []debug.BuildSetting{
			{Key: "vcs.revision", Value: "abc123"},
			{Key: "vcs.time", Value: "2026-03-17T00:00:00Z"},
			{Key: "vcs.modified", Value: "true"},
		},
	}
	version, commit, date := resolveBuildMetadata("dev", "unknown", "unknown", true, info)
	if version != "dev" {
		t.Fatalf("version = %q, want dev", version)
	}
	if commit != "abc123-dirty" {
		t.Fatalf("commit = %q, want %q", commit, "abc123-dirty")
	}
	if date != "2026-03-17T00:00:00Z" {
		t.Fatalf("date = %q, want %q", date, "2026-03-17T00:00:00Z")
	}
}

// TestResolveBuildMetadataDirtinessFollowsCommitIdentity pins the rule that
// keeps a foreign repository's working-tree state out of our build stamp: the
// toolchain's vcs.modified flag is trusted only when its companion
// vcs.revision describes the same commit the binary was linked against.
//
// The concrete failure this guards (ga-u7fb): Go's buildvcs looks for a `.git`
// *directory*, so inside a git worktree — where `.git` is a gitdir *file* — it
// keeps walking up and stamps whichever repository encloses the worktree. A
// polecat worktree sits inside the city directory, itself a git repo, so a
// pristine checkout at dc8327db3 was stamped with the city's 251dc9e0f9 and
// the city's dirtiness, and `gc version --long` reported `dc8327db3-dirty`.
// That lie propagates to the supervisor's /health build_id and to binary-drift
// detection, so it must not survive.
func TestResolveBuildMetadataDirtinessFollowsCommitIdentity(t *testing.T) {
	const (
		worktreeShort = "dc8327db3"
		worktreeFull  = "dc8327db335c0c99ad0be57dca851f51eae2f01b"
		cityFull      = "251dc9e0f9caf16320cda9e02460c9c867057d12"
	)
	tests := []struct {
		name        string
		stamped     string
		vcsRevision string
		vcsModified string
		want        string
	}{
		{
			name:        "foreign revision cannot dirty our stamp",
			stamped:     worktreeShort,
			vcsRevision: cityFull,
			vcsModified: "true",
			want:        worktreeShort,
		},
		{
			name:        "abbreviated stamp matches full revision",
			stamped:     worktreeShort,
			vcsRevision: worktreeFull,
			vcsModified: "true",
			want:        worktreeShort + "-dirty",
		},
		{
			name:        "full stamp matches full revision",
			stamped:     worktreeFull,
			vcsRevision: worktreeFull,
			vcsModified: "true",
			want:        worktreeFull + "-dirty",
		},
		{
			name:        "matching revision on a clean tree stays clean",
			stamped:     worktreeShort,
			vcsRevision: worktreeFull,
			vcsModified: "false",
			want:        worktreeShort,
		},
		{
			name:        "already-stamped dirtiness is not doubled",
			stamped:     worktreeShort + "-dirty",
			vcsRevision: worktreeFull,
			vcsModified: "true",
			want:        worktreeShort + "-dirty",
		},
		{
			name:        "unstamped build still adopts the toolchain revision",
			stamped:     "unknown",
			vcsRevision: worktreeFull,
			vcsModified: "true",
			want:        worktreeFull + "-dirty",
		},
		{
			name:        "abbreviation too short to identify a commit is not trusted",
			stamped:     "dc83",
			vcsRevision: worktreeFull,
			vcsModified: "true",
			want:        "dc83",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info := &debug.BuildInfo{
				Settings: []debug.BuildSetting{
					{Key: "vcs.revision", Value: tt.vcsRevision},
					{Key: "vcs.modified", Value: tt.vcsModified},
				},
			}
			_, commit, _ := resolveBuildMetadata("dev", tt.stamped, "unknown", true, info)
			if commit != tt.want {
				t.Fatalf("commit = %q, want %q", commit, tt.want)
			}
		})
	}
}

func TestBuildIdentityFromStampsRequiresFullKnownSourceIdentity(t *testing.T) {
	const revision = "0123456789abcdef0123456789abcdef01234567"
	clean := buildIdentityFromStamps(revision, "false", "1.2.3")
	if clean.Status != qualification.StatusAvailable || clean.SourceDirty || clean.BuildID != revision {
		t.Fatalf("clean identity = %#v, want exact clean revision", clean)
	}
	dirty := buildIdentityFromStamps(revision, "true", "1.2.3")
	if dirty.Status != qualification.StatusUnavailable || !dirty.SourceDirty || dirty.Reason != "controller_build_dirty" {
		t.Fatalf("dirty identity = %#v, want unavailable identity marked dirty", dirty)
	}
	for _, tc := range []struct {
		name     string
		revision string
		dirty    string
		version  string
	}{
		{name: "abbreviated revision", revision: "0123456", dirty: "false", version: "1.2.3"},
		{name: "missing version", revision: revision, dirty: "false"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			identity := buildIdentityFromStamps(tc.revision, tc.dirty, tc.version)
			if identity.Status != qualification.StatusUnavailable {
				t.Fatalf("identity = %#v, want unavailable", identity)
			}
		})
	}
	unknown := buildIdentityFromStamps(revision, "unknown", "1.2.3")
	if unknown.Status != qualification.StatusUnavailable || unknown.SourceDirty || unknown.Reason != "source_cleanliness_unavailable" {
		t.Fatalf("unknown cleanliness identity = %#v, want unavailable and not clean", unknown)
	}

	invalidArtifact := clean
	invalidArtifact.ArtifactStatus = qualification.StatusAvailable
	invalidArtifact.ArtifactSHA256 = strings.Repeat("a", 63)
	if _, err := qualification.Authorize(context.Background(), staticQualificationAuthorizer{}, validQualificationSnapshot(t), invalidArtifact); err == nil {
		t.Fatal("Authorize accepted a malformed running-artifact digest")
	}
}

type staticQualificationAuthorizer struct{}

func (staticQualificationAuthorizer) Authorize(_ context.Context, _ qualification.ReleaseRequest) (qualification.Authorization, error) {
	return qualification.Authorization{Status: qualification.StatusAuthorized, IdentitySHA: strings.Repeat("b", 64), RecordID: "release-1"}, nil
}

func validQualificationSnapshot(t *testing.T) qualification.Snapshot {
	t.Helper()
	closure := qualification.InputClosure{
		SchemaVersion:  qualification.SchemaVersion,
		Status:         qualification.StatusAvailable,
		EnvironmentSHA: strings.Repeat("c", 64),
		Roots: []qualification.InputRoot{{
			ID: "city", Kind: "city", PinStatus: "content",
			ResolvedPathSHA256: strings.Repeat("a", 64),
			InputsSHA256:       strings.Repeat("b", 64), InputCount: 1,
		}},
	}
	var err error
	closure.SHA256, err = qualification.InputClosureDigest(closure)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := qualification.NewSnapshot(map[string]string{"name": "city"}, closure)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}
