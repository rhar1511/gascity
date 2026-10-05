package contract

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

func configOverlayFixture(t *testing.T, portable, local string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	base := filepath.Join(dir, "config.yaml")
	overlay := filepath.Join(dir, "config.local.yaml")
	for path, data := range map[string]string{base: portable, overlay: local} {
		if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return base, overlay
}

func TestCanonicalConfigLocalOverlayPreservesPortableBytes(t *testing.T) {
	const portable = "# Team defaults stay portable.\nissue-prefix: inktree\nsync.remote: git+https://github.com/rhar1511/inktree.git\ndolt:\n  disable-event-flush: false\ntypes.custom: portable-extension\n"
	base, overlay := configOverlayFixture(t, portable, "# Host runtime state.\nlocal-extension: keep\ntypes.custom: local-extension\n")
	fs := fsys.OSFS{}
	state := ConfigState{IssuePrefix: "inktree", DoltHost: "127.0.0.1", DoltPort: "3309", DoltMode: "server", EndpointOrigin: EndpointOriginInheritedCity, EndpointStatus: EndpointStatusVerified, CustomTypes: []string{"workflow"}}
	changed, err := EnsureCanonicalConfig(fs, base, state)
	if err != nil || !changed {
		t.Fatalf("canonicalize local state: changed=%v err=%v", changed, err)
	}
	got, err := os.ReadFile(base)
	if err != nil || !bytes.Equal(got, []byte(portable)) {
		t.Fatalf("portable config changed: err=%v\n%s", err, got)
	}
	cfg, ok, err := ReadConfigState(fs, base)
	if err != nil || !ok || cfg.DoltHost != "127.0.0.1" || cfg.DoltPort != "3309" || cfg.DoltMode != "server" || cfg.EndpointStatus != EndpointStatusVerified {
		t.Fatalf("effective runtime config=%+v present=%v err=%v", cfg, ok, err)
	}
	doc, err := readConfigDoc(fs, base)
	if err != nil {
		t.Fatal(err)
	}
	types := parseCustomTypesValue(configValue(mappingRoot(doc), "types.custom"))
	for _, name := range []string{"workflow", "portable-extension", "local-extension"} {
		if !slices.Contains(types, name) {
			t.Fatalf("effective custom types %q lost %q", types, name)
		}
	}
	dolt, ok, err := ReadDoltConfig(fs, base)
	if err != nil || !ok || dolt.DisableEventFlush == nil || *dolt.DisableEventFlush {
		t.Fatalf("portable event-flush opt-out lost: cfg=%+v present=%v err=%v", dolt, ok, err)
	}
	local, err := os.ReadFile(overlay)
	if err != nil || !bytes.Contains(local, []byte("local-extension: keep")) {
		t.Fatalf("local extension lost: err=%v\n%s", err, local)
	}
	changed, err = EnsureCanonicalConfig(fs, base, state)
	if err != nil || changed {
		t.Fatalf("repeat canonicalization: changed=%v err=%v", changed, err)
	}
}

func TestReadConfigStateLocalOverlayWinsAndInherits(t *testing.T) {
	base, _ := configOverlayFixture(t, "issue-prefix: inktree\ndolt.host: team-default.invalid\ndolt.port: 3306\ndolt:\n  disable-event-flush: false\n", "dolt.host: 127.0.0.1\ndolt.port: 3309\ngc.endpoint_origin: inherited_city\ngc.endpoint_status: verified\ndolt.mode: server\n")
	cfg, ok, err := ReadConfigState(fsys.OSFS{}, base)
	if err != nil || !ok || cfg.IssuePrefix != "inktree" || cfg.DoltHost != "127.0.0.1" || cfg.DoltPort != "3309" || cfg.EndpointStatus != EndpointStatusVerified {
		t.Fatalf("merged config=%+v present=%v err=%v", cfg, ok, err)
	}
	dolt, ok, err := ReadDoltConfig(fsys.OSFS{}, base)
	if err != nil || !ok || dolt.DisableEventFlush == nil || *dolt.DisableEventFlush {
		t.Fatalf("inherited Dolt config=%+v present=%v err=%v", dolt, ok, err)
	}
}

func TestCanonicalConfigRejectsMalformedLocalOverlayWithoutWriting(t *testing.T) {
	const portable = "issue-prefix: inktree\n"
	const local = "dolt.host: [\n"
	base, overlay := configOverlayFixture(t, portable, local)
	if _, err := EnsureCanonicalConfig(fsys.OSFS{}, base, ConfigState{IssuePrefix: "inktree"}); err == nil {
		t.Fatal("malformed explicit local config must fail instead of rewriting either file")
	}
	if _, _, err := ReadConfigState(fsys.OSFS{}, base); err == nil {
		t.Fatal("malformed local config must not silently fall back to portable endpoint")
	}
	if _, _, err := ReadDoltConfig(fsys.OSFS{}, base); err == nil {
		t.Fatal("malformed local config must not silently fall back to portable Dolt settings")
	}
	for path, want := range map[string]string{base: portable, overlay: local} {
		got, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(got, []byte(want)) {
			t.Fatalf("config changed after error: path=%s err=%v", path, err)
		}
	}
}

func TestLocalConfigOverridesAliasAndCleanupReaders(t *testing.T) {
	base, _ := configOverlayFixture(t, "issue_prefix: portable\ndolt.auto-start: true\nexport.auto: true\ngc.endpoint_status: unverified\n", "issue-prefix: local\ndolt.auto-start: false\nexport.auto: false\ngc.endpoint_status: verified\n")
	fs := fsys.OSFS{}
	if prefix, ok, err := ReadIssuePrefix(fs, base); err != nil || !ok || prefix != "local" {
		t.Fatalf("prefix=%q present=%v err=%v", prefix, ok, err)
	}
	if disabled, err := ReadAutoStartDisabled(fs, base); err != nil || !disabled {
		t.Fatalf("auto-start disabled=%v err=%v", disabled, err)
	}
	if enabled, ok, err := ReadExportAuto(fs, base); err != nil || !ok || enabled {
		t.Fatalf("export enabled=%v present=%v err=%v", enabled, ok, err)
	}
	if status, ok, err := ReadEndpointStatus(fs, base); err != nil || !ok || status != EndpointStatusVerified {
		t.Fatalf("endpoint status=%q present=%v err=%v", status, ok, err)
	}
}

func TestLocalConfigDoltSettingAliasesOverridePortable(t *testing.T) {
	for _, tc := range []struct{ name, portable, local string }{
		{"flat overrides nested", "dolt:\n  disable-event-flush: true\n", "dolt.disable_event_flush: false\n"},
		{"nested overrides flat", "dolt.disable-event-flush: true\n", "dolt:\n  disable_event_flush: false\n"},
		{"nested alias overrides nested", "dolt:\n  disable-event-flush: true\n", "dolt:\n  disable_event_flush: false\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base, _ := configOverlayFixture(t, tc.portable, tc.local)
			cfg, ok, err := ReadDoltConfig(fsys.OSFS{}, base)
			if err != nil || !ok || cfg.DisableEventFlush == nil || *cfg.DisableEventFlush {
				t.Fatalf("local opt-out lost: cfg=%+v present=%v err=%v", cfg, ok, err)
			}
		})
	}
}

func TestCanonicalConfigLocalLayerDoesNotResurrectPortableEndpoint(t *testing.T) {
	const portable = "issue-prefix: inktree\ndolt.host: old.invalid\ndolt.port: 3306\ndolt.socket: /old/socket\ndolt.user: old-user\ndolt.mode: server\n"
	base, _ := configOverlayFixture(t, portable, "local-extension: keep\n")
	fs := fsys.OSFS{}
	state := ConfigState{IssuePrefix: "inktree", EndpointOrigin: EndpointOriginManagedCity}
	for attempt := 0; attempt < 2; attempt++ {
		changed, err := EnsureCanonicalConfig(fs, base, state)
		if err != nil || changed != (attempt == 0) {
			t.Fatalf("attempt=%d changed=%v err=%v", attempt, changed, err)
		}
		cfg, ok, err := ReadConfigState(fs, base)
		if err != nil || !ok || cfg.DoltHost != "" || cfg.DoltPort != "" || cfg.DoltSocket != "" || cfg.DoltUser != "" || cfg.DoltMode != "" {
			t.Fatalf("portable endpoint resurrected: cfg=%+v present=%v err=%v", cfg, ok, err)
		}
	}
	got, err := os.ReadFile(base)
	if err != nil || string(got) != portable {
		t.Fatalf("portable endpoint changed: err=%v\n%s", err, got)
	}
}

func TestLocalConfigRequiresPortableProjectFile(t *testing.T) {
	const local = "issue-prefix: inktree\ndolt.host: 127.0.0.1\ndolt.port: 3309\n"
	base, overlay := configOverlayFixture(t, "", local)
	if err := os.Remove(base); err != nil {
		t.Fatal(err)
	}
	fs := fsys.OSFS{}
	if _, _, err := ReadConfigState(fs, base); err == nil {
		t.Fatal("local overrides need the project config Beads loads first")
	}
	if _, err := EnsureCanonicalConfig(fs, base, ConfigState{IssuePrefix: "inktree"}); err == nil {
		t.Fatal("missing portable config must fail without writing the local file")
	}
	if _, err := os.Stat(base); !os.IsNotExist(err) {
		t.Fatalf("canonicalization created portable file: %v", err)
	}
	if got, err := os.ReadFile(overlay); err != nil || string(got) != local {
		t.Fatalf("local file changed: err=%v\n%s", err, got)
	}
}
