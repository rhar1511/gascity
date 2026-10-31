package continuationprep

import (
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/google/shlex"

	"github.com/gastownhall/gascity/internal/agent"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/qualification"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/sessionauthority"
	"github.com/gastownhall/gascity/internal/shellquote"
)

// Request contains trusted current observations. CaptureSource is selected by
// the host, never copied from token attribution. City must be a newly loaded,
// qualification-aware config snapshot; preparation does not reread its inputs.
type Request struct {
	CityPath      string
	City          *config.City
	Session       session.Info
	Expected      session.ContinuationExpectation
	CaptureSource string
	NativeToken   string
}

// Prepared contains non-launching composition data for a trusted adapter. The
// adapter still supplies canonical city/session-live/overlay hints. Neither the
// statement receipt nor this result is a consumable launch permit; trust,
// freshness, final material and exclusive ownership must be enforced at launch.
type Prepared struct {
	Launch   session.ContinuationLaunch
	Agent    config.Agent
	Provider *config.ResolvedProvider
}

// Prepare resolves canonical provider/options and verifies the exact fresh
// native statement using host trust. It never uses a stored command fallback,
// creates a session, changes a claim, persists metadata or starts a process.
func Prepare(request Request) (Prepared, error) {
	cityPath, cfg, info, expected, captureSource, nativeToken := request.CityPath, request.City, request.Session, request.Expected, request.CaptureSource, request.NativeToken
	empty := Prepared{}
	if cfg == nil || expected.Revision == 0 || info.Type != session.BeadType || info.Closed || info.PendingCreateClaim ||
		info.ID != expected.SessionID || info.Provider != expected.Provider || info.SessionKey != expected.SessionKey ||
		info.Generation != expected.Generation || info.InstanceToken != expected.InstanceToken || info.ContinuationEpoch != expected.ContinuationEpoch ||
		info.WorkDir != expected.WorkDir || info.WorkDirCanonical != expected.WorkDir || info.Transport != config.SessionTransportTmux || info.TransportMetadata != info.Transport {
		return empty, session.ErrContinuationIdentity
	}
	canonical, err := filepath.EvalSymlinks(expected.WorkDir)
	if err != nil || !filepath.IsAbs(expected.WorkDir) || canonical != expected.WorkDir || filepath.Clean(expected.WorkDir) != expected.WorkDir {
		return empty, errors.Join(session.ErrContinuationIdentity, err)
	}
	cityCanonical, err := filepath.EvalSymlinks(cityPath)
	cityStat, statErr := os.Stat(cityPath)
	if err != nil || statErr != nil || !filepath.IsAbs(cityPath) || filepath.Clean(cityPath) != cityPath || cityCanonical != cityPath || !cityStat.IsDir() {
		return empty, errors.Join(session.ErrContinuationIdentity, err, statErr)
	}
	snapshot := cfg.QualificationSnapshot()
	copyCfg := *cfg
	current := config.RefreshQualificationSnapshot(&copyCfg, nil)
	if snapshot.Status != qualification.StatusAvailable || current.Status != qualification.StatusAvailable || snapshot.EffectiveConfigIdentitySHA256 == "" || snapshot.EffectiveConfigIdentitySHA256 != current.EffectiveConfigIdentitySHA256 {
		return empty, sessionauthority.ErrUnavailable
	}
	configured, err := configuredAgent(cfg, info.Template)
	if err != nil {
		return empty, err
	}
	resolved, err := config.ResolveProvider(&configured, &cfg.Workspace, cfg.Providers, exec.LookPath)
	if err != nil {
		return empty, fmt.Errorf("resolve strict continuation provider: %w", err)
	}
	if resolved == nil || resolved.Name != info.Provider {
		return empty, session.ErrContinuationIdentity
	}
	if transport := config.ResolveSessionCreateTransport(configured.Session, resolved); transport != "" && transport != info.Transport {
		return empty, session.ErrContinuationIdentity
	}
	family := providerFamily(resolved)
	for _, stored := range []string{info.ProviderKind, info.BuiltinAncestor} {
		if stored != "" && stored != family {
			return empty, session.ErrContinuationIdentity
		}
	}
	overrides, err := session.ParseTemplateOverridesFromInfo(info)
	if err != nil {
		return empty, err
	}
	effective := maps.Clone(resolved.EffectiveDefaults)
	if effective == nil {
		effective = map[string]string{}
	}
	for key, value := range overrides {
		if key != "initial_message" {
			effective[key] = value
		}
	}
	optionArgs, err := config.ResolveExplicitOptions(resolved.OptionsSchema, effective)
	if err != nil {
		return empty, err
	}
	mode := effective["permission_mode"]
	if mode == "" || info.AuthorityProfile != string(sessionauthority.ProfileWorker) || info.AuthorityAuthorization == "" {
		return empty, sessionauthority.ErrUnavailable
	}
	generation, err := strictContinuationInteger(info.Generation)
	if err != nil {
		return empty, err
	}
	epoch, err := strictContinuationInteger(info.ContinuationEpoch)
	if err != nil {
		return empty, err
	}
	auth, err := sessionauthority.DecodeAuthorization(info.AuthorityAuthorization)
	if err != nil {
		return empty, err
	}
	verifier, err := sessionauthority.LoadHostVerifier()
	if err != nil {
		return empty, err
	}
	if _, err := verifier.VerifyNativeBinding(nativeToken, sessionauthority.NativeBindingTarget{
		City: cfg.EffectiveCityName(), SessionID: info.ID, SessionRevision: expected.Revision, Provider: info.Provider, Transport: info.Transport,
		SessionKey: info.SessionKey, Generation: generation, InstanceToken: info.InstanceToken, ContinuationEpoch: epoch, WorkDir: expected.WorkDir,
		EffectiveConfigSHA256: snapshot.EffectiveConfigIdentitySHA256, PermissionMode: mode, CaptureSource: captureSource,
	}, auth); err != nil {
		return empty, err
	}
	if err := strictResumeDefaults(resolved, effective, overrides); err != nil {
		return empty, err
	}
	command, err := config.BuildProviderLaunchCommand(cityPath, resolved, effective, info.Transport)
	if err != nil {
		return empty, err
	}
	// This strict path requires an existing readable regular Claude settings
	// projection. It neither generates settings nor uses an implicit fallback.
	if family == "claude" {
		if err := readableContinuationSettings(command.SettingsPath); err != nil {
			return empty, err
		}
	}
	resume, err := config.BuildProviderResumeCommand(resolved, effective)
	if err != nil {
		return empty, err
	}
	resume, err = strictContinuationResumeTemplate(resume, resolved.ResumeFlag, resolved.ResumeStyle, optionArgs)
	if err != nil {
		return empty, err
	}
	// Settings are resolved from the canonical launch builder, never a stored
	// command or an implicit last-known-good fallback. No projection is created.
	if command.SettingsPath != "" {
		command.Command, err = strictContinuationPathOption(command.Command, []string{"--settings"}, command.SettingsPath)
		if err != nil {
			return empty, err
		}
		if resume != "" {
			resume, err = strictContinuationPathOption(resume, []string{"--settings"}, command.SettingsPath)
			if err != nil {
				return empty, err
			}
		}
	}
	if family == "codex" {
		command.Command, err = strictContinuationPathOption(command.Command, []string{"--cd", "-C"}, expected.WorkDir)
		if err != nil {
			return empty, err
		}
		if resume != "" {
			resume, err = strictContinuationPathOption(resume, []string{"--cd", "-C"}, expected.WorkDir)
			if err != nil {
				return empty, err
			}
		}
	}
	resumeInfo := info
	resumeInfo.Command = command.Command
	resumeInfo.ResumeFlag = resolved.ResumeFlag
	resumeInfo.ResumeStyle = resolved.ResumeStyle
	resumeInfo.ResumeCommand = resume
	if _, err := session.BuildContinuationCommand(resumeInfo); err != nil {
		return empty, err
	}
	hints := agent.StartupHints{
		Lifecycle: runtime.Lifecycle(resolved.Lifecycle), ReadyPromptPrefix: resolved.ReadyPromptPrefix, ReadyDelayMs: resolved.ReadyDelayMs,
		ProcessNames: resolved.ProcessNames, EmitsPermissionWarning: resolved.EmitsPermissionWarning, AcceptStartupDialogs: resolved.AcceptStartupDialogs,
	}.ToRuntimeConfig()
	hints.WorkDir = expected.WorkDir
	hints.Env = maps.Clone(resolved.Env)
	return Prepared{Agent: configured, Provider: resolved, Launch: session.ContinuationLaunch{
		Command: command.Command,
		Resume:  session.ProviderResume{ResumeFlag: resolved.ResumeFlag, ResumeStyle: resolved.ResumeStyle, ResumeCommand: resume, SessionIDFlag: resolved.SessionIDFlag},
		Hints:   hints,
	}}, nil
}

func strictContinuationInteger(value string) (uint64, error) {
	n, err := strconv.ParseUint(value, 10, 64)
	if err != nil || n == 0 || strconv.FormatUint(n, 10) != value {
		return 0, session.ErrContinuationIdentity
	}
	return n, nil
}

// The canonical option builder may place its exact schema args between a
// subcommand selector and the native placeholder. Move only that known sequence
// after the placeholder; never reinterpret arbitrary leading options as a key.
func strictContinuationResumeTemplate(command, flag, style string, options []string) (string, error) {
	if command == "" {
		return "", nil
	}
	const key = "{{.SessionKey}}"
	argv, err := shlex.Split(command)
	if err != nil || len(argv) < 2 || strings.Count(command, key) != 1 {
		return "", session.ErrContinuationCommand
	}
	if style != "subcommand" && argv[1] == flag+"="+key {
		return command, nil
	}
	index := slices.Index(argv, key)
	if argv[1] != flag || index < 2 || index > 2 && !slices.Equal(argv[2:index], options) {
		return "", session.ErrContinuationCommand
	}
	normalized := append(slices.Clone(argv[:2]), key)
	normalized = append(normalized, argv[2:index]...)
	normalized = append(normalized, argv[index+1:]...)
	return shellquote.Join(normalized), nil
}

// strictContinuationPathOption refuses conflicting or duplicate path selectors
// instead of relying on a CLI's first/last flag precedence. Its flags are
// provider-owned constants, and value is the canonical host-resolved path.
func strictContinuationPathOption(command string, flags []string, value string) (string, error) {
	argv, err := shlex.Split(command)
	if err != nil || len(argv) == 0 || len(flags) == 0 || value == "" {
		return "", session.ErrContinuationCommand
	}
	seen := false
	for i := 1; i < len(argv); i++ {
		for _, flag := range flags {
			if argv[i] == flag {
				if seen || i+1 >= len(argv) || argv[i+1] != value {
					return "", session.ErrContinuationCommand
				}
				seen = true
				i++
				break
			}
			if strings.HasPrefix(argv[i], flag+"=") {
				if seen || strings.TrimPrefix(argv[i], flag+"=") != value {
					return "", session.ErrContinuationCommand
				}
				seen = true
				break
			}
		}
	}
	if !seen {
		argv = append(argv, flags[0], value)
	}
	return shellquote.Join(argv), nil
}

func configuredAgent(cfg *config.City, identity string) (config.Agent, error) {
	var found *config.Agent
	for _, candidate := range cfg.Agents {
		if candidate.QualifiedName() != identity {
			continue
		}
		if found != nil {
			return config.Agent{}, session.ErrContinuationIdentity
		}
		copyAgent := candidate
		found = &copyAgent
	}
	if found == nil || found.Provider == "" {
		return config.Agent{}, session.ErrContinuationCommand
	}
	return *found, nil
}

func providerFamily(provider *config.ResolvedProvider) string {
	if family := strings.TrimSpace(provider.BuiltinAncestor); family != "" {
		return family
	}
	return strings.TrimSpace(provider.Name)
}

// strictResumeDefaults preserves explicit provider intent by refusing a resume
// option that contradicts its resolved default. A persisted session override
// explicitly authorizes replacement; absent that override, never substitute a
// different model/effort/permission value silently. The config package owns all
// option rendering, aliases and flag-shape stripping.
func strictResumeDefaults(provider *config.ResolvedProvider, effective, overrides map[string]string) error {
	if provider.ResumeCommand == "" {
		return nil
	}
	if _, err := shlex.Split(provider.ResumeCommand); err != nil {
		return session.ErrContinuationCommand
	}
	for _, option := range provider.OptionsSchema {
		if _, explicit := overrides[option.Key]; explicit {
			continue
		}
		value, pinned := effective[option.Key]
		if !pinned {
			continue
		}
		args, ok := config.OptionFlagArgs(option, value)
		if !ok {
			return session.ErrContinuationCommand
		}
		selected := config.OptionChoice{Value: value, FlagArgs: args}
		for _, choice := range option.Choices {
			if choice.Value == value {
				selected.FlagAliases = choice.FlagAliases
			}
		}
		expected := option
		expected.Choices = []config.OptionChoice{selected}
		expected.FlagTemplate = nil
		withoutExpected := config.StripFlags(provider.ResumeCommand, config.CollectAllSchemaFlags([]config.ProviderOption{expected}))
		withoutAny := config.ReplaceSchemaFlags(provider.ResumeCommand, []config.ProviderOption{option}, nil)
		if !slices.Equal(shellquote.Split(withoutExpected), shellquote.Split(withoutAny)) {
			return session.ErrContinuationCommand
		}
	}
	return nil
}

func readableContinuationSettings(path string) (err error) {
	if path == "" {
		return session.ErrContinuationCommand
	}
	before, err := captureContinuationSettingsSnapshot(path)
	if err != nil {
		return errors.Join(session.ErrContinuationCommand, err)
	}
	defer func() {
		if closeErr := before.close(); closeErr != nil {
			err = errors.Join(session.ErrContinuationCommand, err, closeErr)
		}
	}()
	if !before.info.Mode().IsRegular() || before.info.Mode().Perm()&0o444 == 0 {
		return session.ErrContinuationCommand
	}
	return readableContinuationSettingsAfterStat(path, before.info, openContinuationSettings)
}

// Check the opened object against the pre-open path. The production opener is
// nonblocking on Unix; tests use that same opener to observe descriptor cleanup.
func readableContinuationSettingsAfterStat(path string, before os.FileInfo, openFile func(string) (*os.File, error)) error {
	file, err := openFile(path)
	if err != nil {
		return errors.Join(session.ErrContinuationCommand, err)
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o444 == 0 || !os.SameFile(before, info) {
		return errors.Join(session.ErrContinuationCommand, err)
	}
	var probe [1]byte
	if _, err := file.Read(probe[:]); err != nil && !errors.Is(err, io.EOF) {
		return errors.Join(session.ErrContinuationCommand, err)
	}
	return nil
}
