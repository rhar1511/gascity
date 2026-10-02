package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/session/sessiontest"
	"github.com/gastownhall/gascity/internal/sessionauthority"
	"github.com/gastownhall/gascity/internal/supervisor"
)

func TestResolvedWorkerRuntimeRequiresExactSessionAuthority(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	trust := sessionauthority.TrustConfig{
		Keys:        []sessionauthority.TrustedKey{{KeyID: "key", PublicKey: base64.StdEncoding.EncodeToString(pub)}},
		Authorities: []sessionauthority.Authority{{KeyID: "key", Issuer: "operations", Subject: "ricky", Profiles: []sessionauthority.Profile{sessionauthority.ProfileWorker}}},
	}
	trustRaw, _ := json.Marshal(trust)
	trustPath := filepath.Join(t.TempDir(), "trust.json")
	if err := os.WriteFile(trustPath, trustRaw, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(sessionauthority.HostTrustFileEnv, trustPath)
	cityDir, cfg := loadSessionAuthorityTestCity(t)

	snapshot := cfg.QualificationSnapshot()
	if snapshot.EffectiveConfigIdentitySHA256 == "" {
		t.Fatal("loaded config has no effective identity")
	}
	info := session.Info{ID: "gc-session", Template: "worker", Provider: "stub", WorkDir: cityDir, Generation: "2"}
	now := time.Now().UTC()
	claims := sessionauthority.Claims{
		SchemaVersion: sessionauthority.SchemaVersionV1, AuthorizationID: "auth-1", TokenID: "token-1",
		KeyID: "key", Issuer: "operations", Subject: "ricky", City: config.EffectiveCityName(cfg, ""),
		SessionID: info.ID, Generation: 2, EffectiveConfigSHA256: snapshot.EffectiveConfigIdentitySHA256,
		FromProfile: sessionauthority.ProfileDesign, ToProfile: sessionauthority.ProfileWorker,
		PermissionMode: "plan", IssuedAt: now.Add(-time.Minute).Unix(), ExpiresAt: now.Add(time.Minute).Unix(),
	}
	verifier, err := sessionauthority.NewVerifier(trust, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	want := sessionauthority.Expectation{
		City: claims.City, SessionID: claims.SessionID, Generation: claims.Generation,
		EffectiveConfigSHA256: claims.EffectiveConfigSHA256, FromProfile: claims.FromProfile,
		ToProfile: claims.ToProfile, PermissionMode: claims.PermissionMode,
	}
	token := mintSessionAuthorityLaunchToken(t, priv, claims)
	auth, err := verifier.Verify(token, want)
	if err != nil {
		t.Fatal(err)
	}
	rawAuth, err := sessionauthority.EncodeAuthorization(auth)
	if err != nil {
		t.Fatal(err)
	}
	metadata := map[string]string{
		"generation":                           "2",
		"template_overrides":                   `{"permission_mode":"plan"}`,
		sessionauthority.MetadataProfile:       string(sessionauthority.ProfileWorker),
		sessionauthority.MetadataAuthorization: rawAuth,
	}
	resolved, err := resolvedWorkerRuntimeWithConfigAndMetadata(cityDir, cfg, info, "", metadata)
	if err != nil {
		t.Fatalf("authorized resume: %v", err)
	}
	if resolved == nil || !strings.Contains(resolved.Command, "--approval never") {
		t.Fatalf("authorized command = %#v, want plan permission mode", resolved)
	}

	metadata[sessionauthority.MetadataTransitions] = `[{"accepted":"retained"}]`
	delete(metadata, sessionauthority.MetadataProfile)
	delete(metadata, sessionauthority.MetadataAuthorization)
	delete(metadata, "template_overrides")
	if _, err := resolvedWorkerRuntimeWithConfigAndMetadata(cityDir, cfg, info, "", metadata); !errors.Is(err, sessionauthority.ErrUnavailable) {
		t.Fatalf("cleared protected resume error = %v, want authority unavailable", err)
	}
	t.Setenv(sessionauthority.HostTrustFileEnv, "")
	if _, err := resolvedWorkerRuntimeWithConfigAndMetadata(cityDir, cfg, info, "", metadata); !errors.Is(err, sessionauthority.ErrUnavailable) {
		t.Fatalf("cleared protected resume without host trust error = %v, want authority unavailable", err)
	}
}

func TestPreparedStartRejectsWorkBeadPermissionModeButKeepsOtherOptions(t *testing.T) {
	t.Setenv(sessionauthority.HostTrustFileEnv, filepath.Join(t.TempDir(), "missing-trust.json"))
	_, cfg := loadSessionAuthorityTestCity(t)
	resolved, err := config.ResolveProvider(&cfg.Agents[0], &cfg.Workspace, cfg.Providers, func(name string) (string, error) { return name, nil })
	if err != nil {
		t.Fatal(err)
	}
	store := beads.NewMemStore()
	sessionBead, err := store.Create(beads.Bead{
		Title: "worker", Type: sessionBeadType, Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{"template": "worker", "session_name": "worker", "state": "asleep", "generation": "1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	work, err := store.Create(beads.Bead{
		Title: "work", Type: "task", Status: "in_progress", Assignee: sessionBead.ID,
		Metadata: map[string]string{"opt_permission_mode": "plan", "opt_effort": "high"},
	})
	if err != nil {
		t.Fatal(err)
	}
	status, assignee := "in_progress", sessionBead.ID
	if err := store.Update(work.ID, beads.UpdateOpts{Status: &status, Assignee: &assignee}); err != nil {
		t.Fatal(err)
	}
	candidate := startCandidate{
		info: sessiontest.SeedBead(t, sessionBead),
		tp:   TemplateParams{TemplateName: "worker", SessionName: "worker", Command: resolved.CommandString(), ResolvedProvider: resolved},
	}
	if _, _, err := buildPreparedStart(candidate, cfg, store); !errors.Is(err, sessionauthority.ErrUnavailable) {
		t.Fatalf("unsigned work permission_mode error = %v, want authority unavailable", err)
	}
	if err := store.SetMetadata(work.ID, "opt_permission_mode", ""); err != nil {
		t.Fatal(err)
	}
	prepared, _, err := buildPreparedStart(candidate, cfg, store)
	if err != nil {
		t.Fatalf("non-authority work option: %v", err)
	}
	if !strings.Contains(prepared.cfg.Command, "--effort high") {
		t.Fatalf("command = %q, want unrelated work option", prepared.cfg.Command)
	}
}

func TestLoadSessionAuthorityCityConfigUsesRegisteredNormalizedIdentity(t *testing.T) {
	clearGCEnv(t)
	t.Setenv(sessionauthority.HostTrustFileEnv, filepath.Join(t.TempDir(), "missing-trust.json"))
	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "rigs", "relative")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writePhase0InterfaceCity(t, cityDir, `[workspace]
name = "configured-name"

[beads]
provider = "file"

[[rigs]]
name = "relative"
path = "rigs/relative"

[[agent]]
name = "worker"
provider = "stub"

[providers.stub]
command = "/bin/echo"
`)
	if err := supervisor.NewRegistry(supervisor.RegistryPath()).Register(cityDir, "registered-alias"); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadCityConfig(cityDir)
	if err != nil {
		t.Fatal(err)
	}
	if got := config.EffectiveCityName(cfg, ""); got != "registered-alias" {
		t.Fatalf("effective city name = %q, want registered-alias", got)
	}
	if len(cfg.Rigs) != 1 || cfg.Rigs[0].Path != rigDir {
		t.Fatalf("normalized rigs = %#v, want path %q", cfg.Rigs, rigDir)
	}
	if got := cfg.QualificationSnapshot().EffectiveConfigIdentitySHA256; got == "" {
		t.Fatal("normalized config has no effective identity")
	}
}

func loadSessionAuthorityTestCity(t *testing.T) (string, *config.City) {
	t.Helper()
	cityDir := t.TempDir()
	writePhase0InterfaceCity(t, cityDir, `[workspace]
name = "test-city"

[beads]
provider = "file"

[[agent]]
name = "worker"
provider = "stub"

[providers.stub]
command = "/bin/echo"

[[providers.stub.options_schema]]
key = "permission_mode"
type = "select"

  [[providers.stub.options_schema.choices]]
  value = "default"
  flag_args = ["--approval", "ask"]

  [[providers.stub.options_schema.choices]]
  value = "plan"
  flag_args = ["--approval", "never"]

[[providers.stub.options_schema]]
key = "effort"
type = "select"

  [[providers.stub.options_schema.choices]]
  value = "low"
  flag_args = ["--effort", "low"]

  [[providers.stub.options_schema.choices]]
  value = "high"
  flag_args = ["--effort", "high"]
`)
	cfg, err := loadCityConfig(cityDir)
	if err != nil {
		t.Fatal(err)
	}
	return cityDir, cfg
}

func mintSessionAuthorityLaunchToken(t *testing.T, key ed25519.PrivateKey, claims sessionauthority.Claims) string {
	t.Helper()
	signing, err := sessionauthority.SigningBytes(claims)
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(claims)
	return base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, signing))
}
