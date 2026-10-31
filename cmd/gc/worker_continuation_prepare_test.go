package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/continuationprep"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/sessionauthority"
)

func trustedContinuationFixture(t *testing.T) (string, *config.City, session.Info, session.ContinuationExpectation, string) {
	t.Helper()
	city, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	writePhase0InterfaceCity(t, city, `[workspace]
name = "test-city"
[beads]
provider = "file"
[[agent]]
name = "worker"
provider = "stub"
[providers.stub]
command = "/bin/echo"
resume_flag = "resume"
resume_style = "subcommand"
resume_command = "/bin/echo resume {{.SessionKey}}"
[providers.stub.option_defaults]
permission_mode = "plan"
model = "model-pinned"
[[providers.stub.options_schema]]
key = "permission_mode"
type = "select"
[[providers.stub.options_schema.choices]]
value = "plan"
flag_args = ["--approval", "ask"]
[[providers.stub.options_schema.choices]]
value = "other"
flag_args = ["--approval", "other"]
[[providers.stub.options_schema]]
key = "model"
type = "select"
flag_template = ["--model", "{value}"]
`)
	cfg, prov, err := config.LoadWithIncludesOptions(fsys.OSFS{}, filepath.Join(city, "city.toml"), config.LoadOptions{CaptureQualificationInputs: true})
	if err != nil {
		t.Fatal(err)
	}
	resolveRigPathsAndRefreshQualification(city, cfg, prov)
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	trust := sessionauthority.TrustConfig{Keys: []sessionauthority.TrustedKey{{KeyID: "host", PublicKey: base64.StdEncoding.EncodeToString(pub)}}, Authorities: []sessionauthority.Authority{{KeyID: "host", Issuer: "host", Subject: "native-capture", Profiles: []sessionauthority.Profile{sessionauthority.ProfileWorker}}}}
	trustFile := filepath.Join(t.TempDir(), "trust.json")
	raw, err := json.Marshal(trust)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(trustFile, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(sessionauthority.HostTrustFileEnv, trustFile)
	info := session.Info{
		ID: "session-1", Type: session.BeadType, Template: "worker", Provider: "stub", Transport: "tmux", TransportMetadata: "tmux", WorkDir: city, WorkDirCanonical: city,
		Generation: "2", InstanceToken: "incarnation-2", ContinuationEpoch: "3", SessionKey: "11111111-2222-4333-8444-555555555555", AuthorityProfile: string(sessionauthority.ProfileWorker),
	}
	expected := session.ContinuationExpectation{SessionID: info.ID, Revision: -71, Provider: info.Provider, SessionKey: info.SessionKey, Generation: info.Generation, InstanceToken: info.InstanceToken, ContinuationEpoch: info.ContinuationEpoch, WorkDir: city}
	now := time.Now().UTC()
	hash := cfg.QualificationSnapshot().EffectiveConfigIdentitySHA256
	if hash == "" {
		t.Fatalf("qualified fixture unavailable: %s", cfg.QualificationSnapshot().Reason)
	}
	grant := sessionauthority.Claims{SchemaVersion: sessionauthority.SchemaVersionV1, AuthorizationID: "authorization-1", TokenID: "grant-1", KeyID: "host", Issuer: "host", Subject: "native-capture", City: cfg.EffectiveCityName(), SessionID: info.ID, Generation: 2, EffectiveConfigSHA256: hash, FromProfile: sessionauthority.ProfileDesign, ToProfile: sessionauthority.ProfileWorker, PermissionMode: "plan", IssuedAt: now.Add(-time.Minute).Unix(), ExpiresAt: now.Add(time.Minute).Unix()}
	v, err := sessionauthority.NewVerifier(trust, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	auth, err := v.Verify(mintSessionAuthorityLaunchToken(t, priv, grant), sessionauthority.Expectation{City: grant.City, SessionID: info.ID, Generation: 2, EffectiveConfigSHA256: hash, FromProfile: grant.FromProfile, ToProfile: grant.ToProfile, PermissionMode: grant.PermissionMode})
	if err != nil {
		t.Fatal(err)
	}
	info.AuthorityAuthorization, err = sessionauthority.EncodeAuthorization(auth)
	if err != nil {
		t.Fatal(err)
	}
	binding := sessionauthority.NativeBindingClaims{
		SchemaVersion: sessionauthority.NativeBindingSchemaV1, BindingID: "binding-1", AuthorizationID: grant.AuthorizationID, KeyID: grant.KeyID, Issuer: grant.Issuer, Subject: grant.Subject, IssuedAt: now.Add(-time.Second).Unix(), ExpiresAt: now.Add(time.Minute).Unix(),
		Target: sessionauthority.NativeBindingTarget{City: grant.City, SessionID: info.ID, SessionRevision: expected.Revision, Provider: info.Provider, Transport: "tmux", SessionKey: info.SessionKey, Generation: 2, InstanceToken: info.InstanceToken, ContinuationEpoch: 3, WorkDir: city, EffectiveConfigSHA256: hash, PermissionMode: "plan", CaptureSource: "native.session-start"},
	}
	signing, err := sessionauthority.NativeBindingSigningBytes(binding)
	if err != nil {
		t.Fatal(err)
	}
	raw, err = json.Marshal(binding)
	if err != nil {
		t.Fatal(err)
	}
	token := base64.RawURLEncoding.EncodeToString(raw) + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(priv, signing))
	return city, cfg, info, expected, token
}

func TestPrepareTrustedContinuationAdapterPreservesCoreAndCityEnvironment(t *testing.T) {
	city, cfg, info, expected, token := trustedContinuationFixture(t)
	core, err := continuationprep.Prepare(continuationprep.Request{CityPath: city, City: cfg, Session: info, Expected: expected, CaptureSource: "native.session-start", NativeToken: token})
	if err != nil {
		t.Fatal(err)
	}
	before := info
	got, err := prepareTrustedContinuation(city, cfg, info, expected, "native.session-start", token)
	if err != nil {
		t.Fatal(err)
	}
	if got.Command != core.Launch.Command || got.Resume != core.Launch.Resume || got.Hints.WorkDir != expected.WorkDir || got.Hints.Env["GC_CITY_PATH"] != city || !reflect.DeepEqual(before, info) {
		t.Fatalf("adapter changed canonical core or omitted city anchor: %+v", got)
	}
}

func TestPrepareTrustedContinuationAdapterPropagatesCoreRefusal(t *testing.T) {
	city, cfg, info, expected, _ := trustedContinuationFixture(t)
	got, err := prepareTrustedContinuation(city, cfg, info, expected, "native.session-start", "")
	if err == nil || !reflect.DeepEqual(got, session.ContinuationLaunch{}) {
		t.Fatalf("core refusal became fallback: %+v, %v", got, err)
	}
}
