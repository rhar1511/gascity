package session

import (
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

func TestStoreCreateSessionRuntimeIdentity(t *testing.T) {
	store := beads.NewMemStore()
	front := NewStore(beads.SessionStore{Store: store})
	fingerprint := strings.Repeat("b", 64)
	meta := map[string]string{"session_name": "exact-key"}
	info, err := front.CreateSessionInfo(CreateSpec{AgentName: "helper", Metadata: meta, RuntimeBackend: "capsule", RuntimeFingerprint: fingerprint})
	if err != nil || info.RuntimeBackend != "capsule" || info.RuntimeFingerprint != fingerprint {
		t.Fatalf("created binding = %#v, %v", info, err)
	}
	if _, changed := meta[beadmeta.RuntimeBackendMetadataKey]; changed {
		t.Fatal("creation mutated caller metadata")
	}
	for _, spec := range []CreateSpec{
		{RuntimeBackend: "capsule"},
		{RuntimeFingerprint: fingerprint},
		{RuntimeBackend: "capsule", RuntimeFingerprint: "invalid"},
		{Metadata: map[string]string{beadmeta.RuntimeBackendMetadataKey: "forged"}},
	} {
		if _, err := front.CreateSessionInfo(spec); err == nil {
			t.Fatalf("accepted invalid binding: %#v", spec)
		}
	}
	rows, err := store.List(beads.ListQuery{Label: LabelSession})
	if err != nil || len(rows) != 1 {
		t.Fatalf("invalid inputs mutated store: %d rows, %v", len(rows), err)
	}
}
