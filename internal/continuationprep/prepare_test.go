package continuationprep

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/sessionauthority"
	"github.com/gastownhall/gascity/internal/shellquote"
)

type preparationFixtureOptions struct {
	family     string
	args       []string
	resume     string
	overrides  string
	mode       string
	captureAge time.Duration
	setup      func(string)
}

func trustedContinuationFixture(t *testing.T) (string, *config.City, session.Info, session.ContinuationExpectation, string) {
	t.Helper()
	return configuredContinuationFixture(t, preparationFixtureOptions{})
}

func configuredContinuationFixture(t *testing.T, options preparationFixtureOptions) (string, *config.City, session.Info, session.ContinuationExpectation, string) {
	t.Helper()
	city, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	document := `[workspace]
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
[[providers.stub.options_schema]]
key = "effort"
type = "select"
flag_template = ["--effort", "{value}"]
`
	executable := preparationTestExecutable(t)
	document = strings.ReplaceAll(document, "/bin/echo", shellquote.Join([]string{executable}))
	if options.family != "" {
		document = strings.Replace(document, "[providers.stub]", "[providers.stub]\nbase = \"builtin:"+options.family+"\"", 1)
	}
	args := append([]string{}, options.args...)
	for i, a := range args {
		args[i] = strings.ReplaceAll(a, "{{WORKDIR}}", city)
	}
	rawArgs, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	document = strings.Replace(document, "[providers.stub]", "[providers.stub]\nargs = "+string(rawArgs), 1)
	if options.resume != "" {
		resume := strings.ReplaceAll(strings.ReplaceAll(options.resume, "{{EXE}}", shellquote.Join([]string{executable})), "{{WORKDIR}}", city)
		raw, err := json.Marshal(resume)
		if err != nil {
			t.Fatal(err)
		}
		document = strings.Replace(document, `resume_command = "`+shellquote.Join([]string{executable})+` resume {{.SessionKey}}"`, "resume_command = "+string(raw), 1)
	}
	if options.setup != nil {
		options.setup(city)
	}
	writePreparationCity(t, city, document)
	cfg, prov, err := config.LoadWithIncludesOptions(fsys.OSFS{}, filepath.Join(city, "city.toml"), config.LoadOptions{CaptureQualificationInputs: true})
	if err != nil {
		t.Fatal(err)
	}
	config.RefreshQualificationSnapshot(cfg, prov)
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
		TemplateOverrides: options.overrides, ProviderKind: options.family, BuiltinAncestor: options.family,
	}
	expected := session.ContinuationExpectation{SessionID: info.ID, Revision: -71, Provider: info.Provider, SessionKey: info.SessionKey, Generation: info.Generation, InstanceToken: info.InstanceToken, ContinuationEpoch: info.ContinuationEpoch, WorkDir: city}
	mode := options.mode
	if mode == "" {
		mode = "plan"
	}
	now := time.Now().UTC()
	hash := cfg.QualificationSnapshot().EffectiveConfigIdentitySHA256
	if hash == "" {
		t.Fatalf("qualified fixture unavailable: %s", cfg.QualificationSnapshot().Reason)
	}
	grant := sessionauthority.Claims{SchemaVersion: sessionauthority.SchemaVersionV1, AuthorizationID: "authorization-1", TokenID: "grant-1", KeyID: "host", Issuer: "host", Subject: "native-capture", City: cfg.EffectiveCityName(), SessionID: info.ID, Generation: 2, EffectiveConfigSHA256: hash, FromProfile: sessionauthority.ProfileDesign, ToProfile: sessionauthority.ProfileWorker, PermissionMode: mode, IssuedAt: now.Add(-time.Minute).Unix(), ExpiresAt: now.Add(time.Minute).Unix()}
	v, err := sessionauthority.NewVerifier(trust, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	auth, err := v.Verify(mintPreparationLaunchToken(t, priv, grant), sessionauthority.Expectation{City: grant.City, SessionID: info.ID, Generation: 2, EffectiveConfigSHA256: hash, FromProfile: grant.FromProfile, ToProfile: grant.ToProfile, PermissionMode: grant.PermissionMode})
	if err != nil {
		t.Fatal(err)
	}
	info.AuthorityAuthorization, err = sessionauthority.EncodeAuthorization(auth)
	if err != nil {
		t.Fatal(err)
	}
	binding := sessionauthority.NativeBindingClaims{
		SchemaVersion: sessionauthority.NativeBindingSchemaV1, BindingID: "binding-1", AuthorizationID: grant.AuthorizationID, KeyID: grant.KeyID, Issuer: grant.Issuer, Subject: grant.Subject, IssuedAt: now.Add(-time.Second - options.captureAge).Unix(), ExpiresAt: now.Add(time.Minute - options.captureAge).Unix(),
		Target: sessionauthority.NativeBindingTarget{City: grant.City, SessionID: info.ID, SessionRevision: expected.Revision, Provider: info.Provider, Transport: "tmux", SessionKey: info.SessionKey, Generation: 2, InstanceToken: info.InstanceToken, ContinuationEpoch: 3, WorkDir: city, EffectiveConfigSHA256: hash, PermissionMode: mode, CaptureSource: "native.session-start"},
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

func TestPrepareTrustedContinuationUsesDefaultPermissionAndExactCanonicalCommand(t *testing.T) {
	city, cfg, info, expected, token := trustedContinuationFixture(t)
	before := info
	launch, err := prepareForTest(city, cfg, info, expected, "native.session-start", token)
	if err != nil {
		t.Fatal(err)
	}
	info.Command = launch.Command
	info.ResumeFlag = launch.Resume.ResumeFlag
	info.ResumeStyle = launch.Resume.ResumeStyle
	info.ResumeCommand = launch.Resume.ResumeCommand
	command, err := session.BuildContinuationCommand(info)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{preparationTestExecutable(t), "resume", expected.SessionKey, "--approval", "ask", "--model", "model-pinned"}
	if !reflect.DeepEqual(shellquote.Split(command), want) || launch.Hints.WorkDir != expected.WorkDir {
		t.Fatalf("command=%q hints=%+v", command, launch.Hints)
	}
	if before.AuthorityAuthorization != info.AuthorityAuthorization {
		t.Fatal("preparation changed authority")
	}
}

func TestPrepareTrustedContinuationRejectsUntrustedOrDriftingInputs(t *testing.T) {
	cases := []struct {
		name   string
		change func(*config.City, *session.Info, *session.ContinuationExpectation, *string, *string)
	}{
		{"native-proof-missing", func(_ *config.City, _ *session.Info, _ *session.ContinuationExpectation, _ *string, token *string) {
			*token = ""
		}},
		{"correlation-is-not-proof", func(_ *config.City, _ *session.Info, _ *session.ContinuationExpectation, _ *string, token *string) {
			*token = `{"native":"app-thread"}`
		}},
		{"closed", func(_ *config.City, i *session.Info, _ *session.ContinuationExpectation, _ *string, _ *string) {
			i.Closed = true
		}},
		{"pending-create", func(_ *config.City, i *session.Info, _ *session.ContinuationExpectation, _ *string, _ *string) {
			i.PendingCreateClaim = true
		}},
		{"missing-authority", func(_ *config.City, i *session.Info, _ *session.ContinuationExpectation, _ *string, _ *string) {
			i.AuthorityAuthorization = ""
		}},
		{"missing-profile", func(_ *config.City, i *session.Info, _ *session.ContinuationExpectation, _ *string, _ *string) {
			i.AuthorityProfile = ""
		}},
		{"wrong-profile", func(_ *config.City, i *session.Info, _ *session.ContinuationExpectation, _ *string, _ *string) {
			i.AuthorityProfile = "operator"
		}},
		{"malformed-options", func(_ *config.City, i *session.Info, _ *session.ContinuationExpectation, _ *string, _ *string) {
			i.TemplateOverrides = "{"
		}},
		{"unknown-options", func(_ *config.City, i *session.Info, _ *session.ContinuationExpectation, _ *string, _ *string) {
			i.TemplateOverrides = `{"unknown":"value"}`
		}},
		{"changed-effective-permission", func(_ *config.City, i *session.Info, _ *session.ContinuationExpectation, _ *string, _ *string) {
			i.TemplateOverrides = `{"permission_mode":"other"}`
		}},
		{"missing-provider", func(_ *config.City, i *session.Info, _ *session.ContinuationExpectation, _ *string, _ *string) {
			i.Provider = ""
		}},
		{"missing-template", func(_ *config.City, i *session.Info, _ *session.ContinuationExpectation, _ *string, _ *string) {
			i.Template = "unknown"
		}},
		{"provider-drift", func(_ *config.City, i *session.Info, e *session.ContinuationExpectation, _ *string, _ *string) {
			i.Provider = "other"
			e.Provider = "other"
		}},
		{"transport-drift", func(_ *config.City, i *session.Info, _ *session.ContinuationExpectation, _ *string, _ *string) {
			i.Transport = "acp"
			i.TransportMetadata = "acp"
		}},
		{"inferred-transport", func(_ *config.City, i *session.Info, _ *session.ContinuationExpectation, _ *string, _ *string) {
			i.TransportMetadata = ""
		}},
		{"native-key-drift", func(_ *config.City, i *session.Info, e *session.ContinuationExpectation, _ *string, _ *string) {
			i.SessionKey = "other-thread"
			e.SessionKey = "other-thread"
		}},
		{"revision-drift", func(_ *config.City, _ *session.Info, e *session.ContinuationExpectation, _ *string, _ *string) {
			e.Revision--
		}},
		{"cwd-mismatch", func(_ *config.City, i *session.Info, _ *session.ContinuationExpectation, _ *string, _ *string) {
			i.WorkDirCanonical = "/other"
		}},
		{"source-mismatch", func(_ *config.City, _ *session.Info, _ *session.ContinuationExpectation, source *string, _ *string) {
			*source = "stdin-correlation"
		}},
		{"config-drift", func(c *config.City, _ *session.Info, _ *session.ContinuationExpectation, _ *string, _ *string) {
			c.Workspace.Name = "changed"
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			city, cfg, info, expected, token := trustedContinuationFixture(t)
			source := "native.session-start"
			tc.change(cfg, &info, &expected, &source, &token)
			before := info
			launch, err := prepareForTest(city, cfg, info, expected, source, token)
			if err == nil || !reflect.DeepEqual(launch, session.ContinuationLaunch{}) || !reflect.DeepEqual(before, info) {
				t.Fatalf("accepted %s: launch=%+v error=%v", tc.name, launch, err)
			}
		})
	}
}

func TestPrepareTrustedContinuationRefusesUnavailableConfigAndTrust(t *testing.T) {
	city, cfg, info, expected, token := trustedContinuationFixture(t)
	for _, bad := range []*config.City{nil, {}} {
		if _, err := prepareForTest(city, bad, info, expected, "native.session-start", token); err == nil {
			t.Fatal("unqualified config accepted")
		}
	}
	t.Setenv(sessionauthority.HostTrustFileEnv, "")
	if _, err := prepareForTest(city, cfg, info, expected, "native.session-start", token); err == nil {
		t.Fatal("missing host trust accepted")
	}
}

func TestPrepareTrustedContinuationCannotFallBackToStoredCommand(t *testing.T) {
	city, cfg, info, expected, token := trustedContinuationFixture(t)
	info.Command = "unsafe-stale --model wrong"
	info.ResumeCommand = "unsafe-stale resume {{.SessionKey}}"
	launch, err := prepareForTest(city, cfg, info, expected, "native.session-start", token)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(launch.Command, "unsafe-stale") || strings.Contains(launch.Resume.ResumeCommand, "unsafe-stale") {
		t.Fatal("stored fallback survived")
	}
}

func TestPrepareTrustedContinuationPathOptionsRefuseContradictoryCLIWorkspaces(t *testing.T) {
	for _, command := range []string{
		"codex --cd /other", "codex -C /other", "codex --cd=/other",
		"codex --cd /worktree -C /worktree", "codex --cd /worktree --cd=/worktree", "codex --cd",
	} {
		if _, err := strictContinuationPathOption(command, []string{"--cd", "-C"}, "/worktree"); err == nil {
			t.Fatalf("conflicting command accepted: %s", command)
		}
	}
	for _, command := range []string{"codex", "codex --cd /worktree", "codex -C /worktree", "codex --cd=/worktree"} {
		got, err := strictContinuationPathOption(command, []string{"--cd", "-C"}, "/worktree")
		if err != nil || !strings.Contains(got, "/worktree") {
			t.Fatalf("command=%q result=%q error=%v", command, got, err)
		}
	}
	if _, err := strictContinuationPathOption("claude --settings /canonical --settings /old", []string{"--settings"}, "/canonical"); err == nil {
		t.Fatal("duplicate settings accepted")
	}
}

func writePreparationCity(t *testing.T, city, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(city, ".gc"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(city, "city.toml"), []byte(strings.TrimSpace(content)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func mintPreparationLaunchToken(t *testing.T, key ed25519.PrivateKey, c sessionauthority.Claims) string {
	t.Helper()
	b, err := sessionauthority.SigningBytes(c)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(raw) + "." + base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, b))
}

func prepareForTest(city string, cfg *config.City, info session.Info, expected session.ContinuationExpectation, source, token string) (session.ContinuationLaunch, error) {
	prepared, err := Prepare(Request{CityPath: city, City: cfg, Session: info, Expected: expected, CaptureSource: source, NativeToken: token})
	return prepared.Launch, err
}

func preparationTestExecutable(t *testing.T) string {
	t.Helper()
	p, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	p, err = filepath.EvalSymlinks(p)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPreparePinsOptionsOnCanonicalResume(t *testing.T) {
	city, cfg, info, expected, token := configuredContinuationFixture(t, preparationFixtureOptions{
		overrides: `{"model":"new-model","effort":"high","permission_mode":"other"}`, mode: "other",
	})
	launch, err := prepareForTest(city, cfg, info, expected, "native.session-start", token)
	if err != nil {
		t.Fatal(err)
	}
	argv := preparedCommandArgs(t, info, launch)
	want := []string{preparationTestExecutable(t), "resume", expected.SessionKey, "--approval", "other", "--model", "new-model", "--effort", "high"}
	if !reflect.DeepEqual(argv, want) {
		t.Fatalf("argv=%q want=%q", argv, want)
	}
}

func TestPrepareCodexFamilyUsesCanonicalWorkspaceOnBothCommands(t *testing.T) {
	for i, args := range [][]string{nil, {"--cd", "{{WORKDIR}}"}, {"-C", "{{WORKDIR}}"}, {"--cd={{WORKDIR}}"}} {
		t.Run(fmt.Sprintf("variant-%d", i), func(t *testing.T) {
			city, cfg, info, expected, token := configuredContinuationFixture(t, preparationFixtureOptions{family: "codex", args: args})
			launch, err := prepareForTest(city, cfg, info, expected, "native.session-start", token)
			if err != nil {
				t.Fatal(err)
			}
			for _, argv := range [][]string{shellquote.Split(launch.Command), preparedCommandArgs(t, info, launch)} {
				assertPathArgument(t, argv, []string{"--cd", "-C"}, city)
			}
			if launch.Hints.WorkDir != city {
				t.Fatalf("cwd=%q", launch.Hints.WorkDir)
			}
		})
	}
	for _, args := range [][]string{{"--cd", "/other"}, {"-C", "/other"}, {"--cd", "{{WORKDIR}}", "-C", "{{WORKDIR}}"}} {
		city, cfg, info, expected, token := configuredContinuationFixture(t, preparationFixtureOptions{family: "codex", args: args})
		if _, err := prepareForTest(city, cfg, info, expected, "native.session-start", token); err == nil {
			t.Fatalf("conflicting configured workspace accepted: %q", args)
		}
	}
}

func TestPrepareClaudeSettingsAreRequiredReadableAndUnambiguous(t *testing.T) {
	setup := func(city string) { writePreparationSettings(t, city, 0o600) }
	city, cfg, info, expected, token := configuredContinuationFixture(t, preparationFixtureOptions{family: "claude", setup: setup})
	launch, err := prepareForTest(city, cfg, info, expected, "native.session-start", token)
	if err != nil {
		t.Fatal(err)
	}
	for _, argv := range [][]string{shellquote.Split(launch.Command), preparedCommandArgs(t, info, launch)} {
		assertPathArgument(t, argv, []string{"--settings"}, filepath.Join(city, ".gc", "settings.json"))
	}
	for _, tc := range []struct {
		name  string
		setup func(string)
		args  []string
	}{
		{name: "absent"},
		{name: "unreadable", setup: func(city string) { writeUnreadablePreparationSettings(t, city) }},
		{name: "directory", setup: func(city string) {
			if err := os.MkdirAll(filepath.Join(city, ".gc", "settings.json"), 0o700); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "conflicting", setup: setup, args: []string{"--settings", "/other"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			city, cfg, info, expected, token := configuredContinuationFixture(t, preparationFixtureOptions{family: "claude", setup: tc.setup, args: tc.args})
			got, err := prepareForTest(city, cfg, info, expected, "native.session-start", token)
			if err == nil || !reflect.DeepEqual(got, session.ContinuationLaunch{}) {
				t.Fatalf("invalid settings accepted: %+v, %v", got, err)
			}
		})
	}
}

func TestPrepareRejectsExpiredAndRevokedCapture(t *testing.T) {
	city, cfg, info, expected, token := configuredContinuationFixture(t, preparationFixtureOptions{captureAge: 2 * time.Minute})
	if _, err := prepareForTest(city, cfg, info, expected, "native.session-start", token); !errors.Is(err, sessionauthority.ErrExpired) {
		t.Fatalf("expired capture: %v", err)
	}
	city, cfg, info, expected, token = trustedContinuationFixture(t)
	file := os.Getenv(sessionauthority.HostTrustFileEnv)
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var trust sessionauthority.TrustConfig
	if err := json.Unmarshal(raw, &trust); err != nil {
		t.Fatal(err)
	}
	trust.RevokedAuthorizationIDs = []string{"authorization-1"}
	raw, err = json.Marshal(trust)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareForTest(city, cfg, info, expected, "native.session-start", token); !errors.Is(err, sessionauthority.ErrUnauthorized) {
		t.Fatalf("revoked capture: %v", err)
	}
}

func TestPreparePreservesSelectorGrammarAndQuotedExecutable(t *testing.T) {
	for _, resume := range []string{
		"{{EXE}} resume prefix-{{.SessionKey}}", "{{EXE}} resume {{.SessionKey}}-suffix",
		"{{EXE}} resume wrong --note {{.SessionKey}}", "{{EXE}} --note resume {{.SessionKey}}",
		"{{EXE}} -- resume {{.SessionKey}}", "{{EXE}} resume {{.SessionKey}} --",
		"{{EXE}} resume {{.SessionKey}} {{.SessionKey}}",
	} {
		city, cfg, info, expected, token := configuredContinuationFixture(t, preparationFixtureOptions{resume: resume})
		if _, err := prepareForTest(city, cfg, info, expected, "native.session-start", token); err == nil {
			t.Fatalf("ambiguous selector accepted: %s", resume)
		}
	}
	city, cfg, info, expected, token := configuredContinuationFixture(t, preparationFixtureOptions{
		resume: "'{{WORKDIR}}/resume client' resume {{.SessionKey}}",
		setup: func(city string) {
			if err := os.Symlink(preparationTestExecutable(t), filepath.Join(city, "resume client")); err != nil {
				t.Fatal(err)
			}
		},
	})
	launch, err := prepareForTest(city, cfg, info, expected, "native.session-start", token)
	if err != nil {
		t.Fatal(err)
	}
	argv := preparedCommandArgs(t, info, launch)
	if len(argv) < 3 || argv[0] != filepath.Join(city, "resume client") || argv[1] != "resume" || argv[2] != expected.SessionKey {
		t.Fatalf("quoted executable/binding changed: %q", argv)
	}
}

func TestPrepareDoesNotSilentlyReplaceConflictingResumeDefaults(t *testing.T) {
	city, cfg, info, expected, token := configuredContinuationFixture(t, preparationFixtureOptions{resume: "{{EXE}} resume {{.SessionKey}} --model other-model"})
	if _, err := prepareForTest(city, cfg, info, expected, "native.session-start", token); err == nil {
		t.Fatal("explicit resume model silently replaced by unrelated default")
	}
	city, cfg, info, expected, token = configuredContinuationFixture(t, preparationFixtureOptions{
		resume: "{{EXE}} resume {{.SessionKey}} --model other-model", overrides: `{"model":"authorized-model"}`,
	})
	launch, err := prepareForTest(city, cfg, info, expected, "native.session-start", token)
	if err != nil {
		t.Fatal(err)
	}
	argv := preparedCommandArgs(t, info, launch)
	assertPathArgument(t, argv, []string{"--model"}, "authorized-model")
}

func TestPreparedSnapshotRequiresFreshConfigAtLaterBoundary(t *testing.T) {
	city, cfg, info, expected, token := trustedContinuationFixture(t)
	file := filepath.Join(city, "city.toml")
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	raw = []byte(strings.Replace(string(raw), `model = "model-pinned"`, `model = "changed-model"`, 1))
	if err := os.WriteFile(file, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	// Preparation uses the supplied qualified snapshot. It is not a filesystem
	// freshness proof or an admission permit and must not be cached as one.
	if _, err := prepareForTest(city, cfg, info, expected, "native.session-start", token); err != nil {
		t.Fatalf("snapshot contract changed: %v", err)
	}
	fresh, prov, err := config.LoadWithIncludesOptions(fsys.OSFS{}, file, config.LoadOptions{CaptureQualificationInputs: true})
	if err != nil {
		t.Fatal(err)
	}
	config.RefreshQualificationSnapshot(fresh, prov)
	if _, err := prepareForTest(city, fresh, info, expected, "native.session-start", token); err == nil {
		t.Fatal("fresh config incorrectly accepted old native/config scope")
	}
}

func preparedCommandArgs(t *testing.T, info session.Info, launch session.ContinuationLaunch) []string {
	t.Helper()
	info.Command = launch.Command
	info.ResumeFlag = launch.Resume.ResumeFlag
	info.ResumeStyle = launch.Resume.ResumeStyle
	info.ResumeCommand = launch.Resume.ResumeCommand
	command, err := session.BuildContinuationCommand(info)
	if err != nil {
		t.Fatal(err)
	}
	return shellquote.Split(command)
}

func assertPathArgument(t *testing.T, argv, flags []string, want string) {
	t.Helper()
	count := 0
	for i, token := range argv {
		for _, flag := range flags {
			if token == flag {
				count++
				if i+1 >= len(argv) || argv[i+1] != want {
					t.Fatalf("argument mismatch: %q", argv)
				}
			}
			if strings.HasPrefix(token, flag+"=") {
				count++
				if strings.TrimPrefix(token, flag+"=") != want {
					t.Fatalf("argument mismatch: %q", argv)
				}
			}
		}
	}
	if count != 1 {
		t.Fatalf("expected exactly one %q, got %q", flags, argv)
	}
}

func TestPrepareRequiresCanonicalCityRoot(t *testing.T) {
	city, cfg, info, expected, token := trustedContinuationFixture(t)
	for _, root := range []string{"relative", filepath.Join(city, "missing"), city + "/../" + filepath.Base(city)} {
		if _, err := Prepare(Request{CityPath: root, City: cfg, Session: info, Expected: expected, CaptureSource: "native.session-start", NativeToken: token}); err == nil {
			t.Fatalf("noncanonical/unavailable city root accepted: %q", root)
		}
	}
}

func TestSettingsOpenRejectsDifferentRegularFileAndClosesDescriptor(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(path, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := captureContinuationSettingsSnapshot(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := before.close(); err != nil {
			t.Error(err)
		}
	}()
	// Retain the original inode so replacement cannot reuse its identity.
	if err := os.Rename(path, path+".original"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	assertSettingsReplacementRefusedAndClosed(t, path, before.info)
}

func assertSettingsReplacementRefusedAndClosed(t *testing.T, path string, before os.FileInfo) {
	t.Helper()
	var opened *os.File
	err := readableContinuationSettingsAfterStat(path, before, func(path string) (*os.File, error) {
		file, err := openContinuationSettings(path)
		opened = file
		return file, err
	})
	if !errors.Is(err, session.ErrContinuationCommand) {
		t.Fatalf("replaced settings accepted: %v", err)
	}
	if opened == nil {
		t.Fatal("replacement did not reach descriptor validation")
	}
	if _, err := opened.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("settings descriptor not closed: %v", err)
	}
}

func TestPrepareClaudeAcceptsEmptyRegularSettings(t *testing.T) {
	setup := func(city string) {
		path := filepath.Join(city, ".gc", "settings.json")
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	city, cfg, info, expected, token := configuredContinuationFixture(t, preparationFixtureOptions{family: "claude", setup: setup})
	got, err := Prepare(Request{CityPath: city, City: cfg, Session: info, Expected: expected, CaptureSource: "native.session-start", NativeToken: token})
	if err != nil || got.Provider == nil {
		t.Fatalf("empty regular settings refused: %+v, %v", got, err)
	}
}

func TestSettingsSnapshotRetainsIdentityAndClosesOwnedHandle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	snapshot, err := captureContinuationSettingsSnapshot(path)
	if err != nil {
		t.Fatal(err)
	}
	closed := false
	t.Cleanup(func() {
		if !closed {
			if err := snapshot.close(); err != nil {
				t.Error(err)
			}
		}
	})
	if runtime.GOOS == "windows" && snapshot.handle == nil {
		t.Fatal("Windows snapshot did not retain a coherent metadata handle")
	}
	if !snapshot.info.Mode().IsRegular() {
		t.Fatal("snapshot lost regular-file attributes")
	}
	if snapshot.handle != nil {
		current, err := snapshot.handle.Stat()
		if err != nil || !os.SameFile(snapshot.info, current) {
			t.Fatalf("snapshot handle does not retain captured identity: %v", err)
		}
	}
	if err := snapshot.close(); err != nil {
		t.Fatal(err)
	}
	closed = true
	if snapshot.handle != nil {
		if _, err := snapshot.handle.Stat(); !errors.Is(err, os.ErrClosed) {
			t.Fatalf("metadata handle not closed: %v", err)
		}
	}
}
