package main

import (
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/sessionauthority"
)

func TestBdMetadataGuardRefusesSessionAuthorityMutation(t *testing.T) {
	t.Setenv(sessionauthority.HostTrustFileEnv, "/host/session-authority.json")
	cfg := &config.City{}
	tests := [][]string{
		{"update", "gc-session", "--set-metadata", sessionauthority.MetadataProfile + "="},
		{"update", "gc-session", "--unset-metadata", sessionauthority.MetadataAuthorization},
		{"update", "gc-session", "--unset-metadata=" + sessionauthority.MetadataTransitions},
		{"update", "gc-session", "--set-metadata", "template_overrides={}"},
		{"update", "gc-session", "--set-metadata", beadmeta.OptionMetadataPrefix + sessionAuthorityPermissionModeOptionKey + "="},
		{"update", "gc-session", `--metadata={"gc.authority_profile":"","template_overrides":"{}"}`},
	}
	for _, args := range tests {
		if msg, refused := bdRigQualifiedMetadataRefusal(cfg, args); !refused || !strings.Contains(msg, "controller-owned session authority metadata") {
			t.Errorf("args %q: refused=%v msg=%q", args, refused, msg)
		}
	}
}

func TestBdPersistedProtectedMutationRefusal(t *testing.T) {
	t.Setenv(sessionauthority.HostTrustFileEnv, "")
	protected := map[string]beads.Bead{"gc-session": {
		ID: "gc-session",
		Metadata: map[string]string{
			sessionauthority.MetadataTransitions:        `[{"outcome":"accepted"}]`,
			beadmeta.SessionRequestReceiptPrefix + "r1": `{}`,
		},
	}}
	for _, args := range [][]string{
		{"update", "gc-session", "--set-metadata", sessionauthority.MetadataTemplateOverrides + "={}"},
		{"update", "gc-session", "--unset-metadata", sessionauthority.MetadataPermissionModeOption},
		{"update", "gc-session", `--metadata={"template_overrides":"{}"}`},
		{"delete", "gc-session", "--force"},
	} {
		if msg, refused := bdPersistedProtectedMutationRefusal(args, []string{"gc-session"}, protected); !refused || !strings.Contains(msg, "refusing") {
			t.Errorf("args %q: refused=%v msg=%q", args, refused, msg)
		}
	}
	for _, args := range [][]string{
		{"update", "gc-session", "--set-metadata", sessionauthority.MetadataTemplateOverrides + "={}"},
		{"delete", "gc-session", "--force"},
	} {
		if msg, refused := bdPersistedProtectedMutationRefusal(args, []string{"gc-session"}, nil); !refused || !strings.Contains(msg, "could not be verified") {
			t.Errorf("unreadable args %q: refused=%v msg=%q", args, refused, msg)
		}
	}
	for _, flag := range []string{"--cascade", "--cascade=true", "--cascade=1"} {
		if msg, refused := bdPersistedProtectedMutationRefusal(
			[]string{"delete", "gc-root", flag}, []string{"gc-root"}, map[string]beads.Bead{"gc-root": {ID: "gc-root"}},
		); !refused || !strings.Contains(msg, "cascade delete") {
			t.Fatalf("cascade delete %s: refused=%v msg=%q", flag, refused, msg)
		}
	}
	if msg, refused := bdPersistedProtectedMutationRefusal(
		[]string{"delete", "gc-root", "--cascade=false", "--cascade=true"}, []string{"gc-root"}, map[string]beads.Bead{"gc-root": {ID: "gc-root"}},
	); !refused || !strings.Contains(msg, "cascade delete") {
		t.Fatalf("repeated cascade flag: refused=%v msg=%q", refused, msg)
	}
}

func TestBdMetadataGuardRefusesSessionRequestReceiptMutation(t *testing.T) {
	cfg := &config.City{}
	for _, args := range [][]string{
		{"update", "gc-session", "--set-metadata", beadmeta.SessionRequestReceiptPrefix + "r1={}"},
		{"update", "gc-session", "--unset-metadata", beadmeta.SessionRequestReceiptPrefix + "r1"},
		{"update", "gc-session", `--metadata={"gc.session_request.v1.r1":"{}"}`},
	} {
		if msg, refused := bdRigQualifiedMetadataRefusal(cfg, args); !refused || !strings.Contains(msg, "session request receipt") {
			t.Errorf("args %q: refused=%v msg=%q", args, refused, msg)
		}
	}
}

func TestBdMetadataGuardAlwaysProtectsAuthorityProof(t *testing.T) {
	t.Setenv(sessionauthority.HostTrustFileEnv, "")
	if msg, refused := bdRigQualifiedMetadataRefusal(&config.City{}, []string{
		"update", "gc-session", "--set-metadata", sessionauthority.MetadataAuthorization + "=forged",
	}); !refused || !strings.Contains(msg, "controller-owned session authority metadata") {
		t.Fatalf("refused=%v msg=%q", refused, msg)
	}
}
