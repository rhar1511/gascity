package session

import (
	"context"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/google/shlex"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/git"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/shellquote"
)

// ErrContinuationIdentity reports an unavailable or changed native identity.
var ErrContinuationIdentity = errors.New("native continuation identity unavailable or changed")

// ErrContinuationCommand reports missing or ambiguous canonical resume config.
var ErrContinuationCommand = errors.New("native continuation command unavailable or ambiguous")

// ErrContinuationRuntime reports that runtime absence cannot be verified.
var ErrContinuationRuntime = errors.New("native continuation runtime absence unverified")

// ContinuationExpectation must come from owner-authenticated evidence. The
// struct itself authenticates nothing and grants no work admission; trusted
// composition must establish those separately before calling this primitive.
type ContinuationExpectation struct {
	SessionID         string
	Revision          int64
	Provider          string
	SessionKey        string
	Generation        string
	InstanceToken     string
	ContinuationEpoch string
	WorkDir           string
}

// ContinuationLaunch carries canonical, resolved provider configuration.
// Prepare must propagate resolution errors instead of using a stored fallback.
type ContinuationLaunch struct {
	Command string
	Resume  ProviderResume
	Hints   runtime.Config
}

// ContinuationPrepare resolves trusted canonical provider configuration; it must
// retain signed authority-profile checks and may not trust client attribution.
type ContinuationPrepare func(Info) (ContinuationLaunch, error)

// StartContinuation starts an existing exact native conversation. It is an
// opt-in boundary; legacy Start/StartRuntimeOnly behavior is unchanged.
// It does not supply an atomic task lease or authenticate native-provider state.
// Any error can follow an attempted Start; callers must reconcile, not retry
// automatically or infer that no process exists.
func (m *Manager) StartContinuation(ctx context.Context, expected ContinuationExpectation, prepare ContinuationPrepare) error {
	if m == nil || m.store == nil || m.sp == nil || !expected.valid() || prepare == nil {
		return ErrContinuationIdentity
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return WithSessionMutationLock(expected.SessionID, func() error {
		row, err := beads.HandlesFor(m.store).Live.Get(expected.SessionID)
		if err != nil {
			return errors.Join(ErrContinuationIdentity, err)
		}
		if !expected.matches(row) || !continuationEligible(row, m.now()) {
			return ErrContinuationIdentity
		}
		writer, _, err := beads.ResolveConditionalWriter(m.store)
		if err != nil {
			return err
		}
		if writer == nil || row.Revision == 0 {
			return beads.ErrConditionalWriteUnsupported
		}
		current := infoFromPersistedBead(row)
		if err := m.continuationRuntimeAbsent(current); err != nil {
			return err
		}
		launch, err := prepare(current)
		if err != nil {
			return err
		}
		current.Command = launch.Command
		current.ResumeFlag = launch.Resume.ResumeFlag
		current.ResumeStyle = launch.Resume.ResumeStyle
		current.ResumeCommand = launch.Resume.ResumeCommand
		command, err := BuildContinuationCommand(current)
		if err != nil {
			return err
		}
		cfg := launch.Hints
		if cfg.WorkDir != "" && cfg.WorkDir != expected.WorkDir {
			return ErrContinuationCommand
		}
		cfg.Command = command
		if cfg.WorkDir == "" {
			cfg.WorkDir = current.WorkDir
		}
		generation, err := strconv.Atoi(current.Generation)
		if err != nil || generation <= 0 {
			return ErrContinuationIdentity
		}
		epoch, err := strconv.Atoi(current.ContinuationEpoch)
		if err != nil || epoch <= 0 {
			return ErrContinuationIdentity
		}
		cfg.Env = mergeEnv(cfg.Env, RuntimeEnvWithSessionContext(current, generation, epoch, current.InstanceToken))
		cfg.Env = git.ApplySSHKeepaliveEnv(cfg.Env)
		cfg = runtime.SyncWorkDirEnv(cfg)
		// Preparation may cross a slow config/settings boundary. Re-read
		// authoritative state before launch; this is not a cross-store fence.
		latest, err := beads.HandlesFor(m.store).Live.Get(expected.SessionID)
		if err != nil {
			return errors.Join(ErrContinuationIdentity, err)
		}
		if !expected.matches(latest) || !continuationEligible(latest, m.now()) {
			return ErrContinuationIdentity
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := m.continuationRuntimeAbsent(current); err != nil {
			return err
		}
		// A strict continuation never routes through legacy fresh recovery.
		if err := m.sp.Start(ctx, current.SessionName, cfg); err != nil {
			return err
		}
		if err := m.staleKeyDetectionWaiter(ctx, current.SessionName); err != nil {
			return err
		}
		if !m.sp.IsRunning(current.SessionName) {
			return runtime.ErrSessionDiedDuringStartup
		}
		// This conditional completion is not an atomic cross-store work lease.
		// A failed completion can follow a real Start; callers must not retry
		// under a replacement identity or report that no process was started.
		return writer.UpdateIfMatch(row.ID, row.Revision, beads.UpdateOpts{Metadata: map[string]string{
			"state": string(StateActive), "state_reason": "creation_complete",
		}})
	})
}

func (e ContinuationExpectation) valid() bool {
	for _, value := range []string{e.SessionID, e.Provider, e.SessionKey, e.InstanceToken} {
		if value == "" || strings.TrimSpace(value) != value {
			return false
		}
	}
	return validContinuationKey(e.SessionKey) && e.Revision != 0 && positiveContinuationInteger(e.Generation) && positiveContinuationInteger(e.ContinuationEpoch) && filepath.IsAbs(e.WorkDir) && filepath.Clean(e.WorkDir) == e.WorkDir
}

func positiveContinuationInteger(value string) bool {
	number, err := strconv.Atoi(value)
	return err == nil && number > 0 && strconv.Itoa(number) == value
}

func (e ContinuationExpectation) matches(row beads.Bead) bool {
	return row.ID == e.SessionID && row.Revision == e.Revision && row.Type == BeadType &&
		row.Metadata["provider"] == e.Provider && row.Metadata["session_key"] == e.SessionKey &&
		row.Metadata["generation"] == e.Generation && row.Metadata["instance_token"] == e.InstanceToken &&
		row.Metadata["continuation_epoch"] == e.ContinuationEpoch &&
		row.Metadata["work_dir"] == e.WorkDir && row.Metadata["gc.work_dir"] == e.WorkDir
}

// BuildContinuationCommand rejects fresh/ambiguous resume shapes instead of
// silently selecting the stored start command. Provider options must already
// have been composed by the canonical provider resolver.
func BuildContinuationCommand(info Info) (string, error) {
	const key = "{{.SessionKey}}"
	if strings.TrimSpace(info.Command) == "" || !validContinuationKey(info.SessionKey) || !validContinuationResumeFlag(info.ResumeFlag) {
		return "", ErrContinuationCommand
	}
	if info.ResumeStyle != "" && info.ResumeStyle != "flag" && info.ResumeStyle != "subcommand" {
		return "", ErrContinuationCommand
	}
	base, err := continuationArgv(info.Command)
	if err != nil {
		return "", err
	}
	if info.ResumeCommand == "" {
		for _, token := range base[1:] {
			if token == info.ResumeFlag || strings.HasPrefix(token, info.ResumeFlag+"=") {
				return "", ErrContinuationCommand
			}
		}
		argv := []string{base[0], info.ResumeFlag, info.SessionKey}
		return shellquote.Join(append(argv, base[1:]...)), nil
	}
	if strings.Count(info.ResumeCommand, key) != 1 {
		return "", ErrContinuationCommand
	}
	rest := strings.Replace(info.ResumeCommand, key, "", 1)
	if strings.Contains(rest, "{{") || strings.Contains(rest, "}}") {
		return "", ErrContinuationCommand
	}
	argv, err := continuationArgv(info.ResumeCommand)
	if err != nil || len(argv) < 2 {
		return "", ErrContinuationCommand
	}
	pair := len(argv) > 2 && argv[1] == info.ResumeFlag && argv[2] == key
	equals := info.ResumeStyle != "subcommand" && argv[1] == info.ResumeFlag+"="+key
	if !pair && !equals {
		return "", ErrContinuationCommand
	}
	bindings, bound := 0, -1
	for i := 1; i < len(argv); i++ {
		switch {
		case argv[i] == info.ResumeFlag:
			if i+1 >= len(argv) || argv[i+1] != key {
				return "", ErrContinuationCommand
			}
			bindings++
			bound = i + 1
		case strings.HasPrefix(argv[i], info.ResumeFlag+"="):
			if info.ResumeStyle == "subcommand" || argv[i] != info.ResumeFlag+"="+key {
				return "", ErrContinuationCommand
			}
			bindings++
			bound = i
		}
	}
	if bindings != 1 {
		return "", ErrContinuationCommand
	}
	argv[bound] = strings.Replace(argv[bound], key, info.SessionKey, 1)
	return shellquote.Join(argv), nil
}

func validContinuationResumeFlag(flag string) bool {
	if flag == "" {
		return false
	}
	for _, r := range flag {
		if !asciiAlphaNumeric(r) && r != '-' && r != '_' {
			return false
		}
	}
	return true
}

func validContinuationKey(key string) bool {
	if key == "" || strings.HasPrefix(key, "-") {
		return false
	}
	for _, r := range key {
		if !asciiAlphaNumeric(r) && !strings.ContainsRune("-_./:", r) {
			return false
		}
	}
	return true
}

func continuationEligible(row beads.Bead, now time.Time) bool {
	if row.Status != "open" || strings.TrimSpace(row.Metadata["session_name"]) == "" ||
		strings.TrimSpace(row.Metadata["continuation_reset_pending"]) != "" ||
		infoFromPersistedBead(row).PendingCreateClaim ||
		KillPendingMetadata(row.Metadata["state"], row.Metadata["state_reason"], row.Metadata["sleep_reason"], row.Metadata["slept_at"], now) {
		return false
	}
	switch State(row.Metadata["state"]) {
	case StateActive, StateAsleep, StateSuspended:
		return true
	default:
		return false
	}
}

func asciiAlphaNumeric(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9'
}

// Strict continuation refuses survivors; it never kills an unverified runtime.
// This local observation is not a distributed work-lease or authority fence.
func (m *Manager) continuationRuntimeAbsent(info Info) error {
	if m.sp.IsRunning(info.SessionName) {
		return ErrContinuationRuntime
	}
	scanner, ok := runtime.AsProcessTableScanner(m.sp)
	if !ok {
		return ErrContinuationRuntime
	}
	found, err := scanner.FindRuntimesBySessionID(info.ID)
	if err != nil {
		return errors.Join(ErrContinuationRuntime, err)
	}
	for _, live := range found {
		if live.SessionID == info.ID {
			return ErrContinuationRuntime
		}
	}
	return nil
}

// The strict grammar is one quoted executable argv, including an opaque
// executable wrapper. The native selector must immediately follow it.
// Shell/interpreter prefixes, assignments, option terminators, selectors and
// metacharacter-bearing arguments require a separately validated provider form;
// they are never guessed or silently converted into a different launch.
func continuationArgv(command string) ([]string, error) {
	argv, err := shlex.Split(command)
	if err != nil || len(argv) == 0 || strings.Contains(argv[0], "=") {
		return nil, ErrContinuationCommand
	}
	program := strings.TrimSuffix(strings.ToLower(filepath.Base(argv[0])), ".exe")
	switch program {
	case "env", "exec", "sh", "bash", "zsh", "fish", "cmd", "powershell", "pwsh", "python", "python2", "python3", "node", "perl", "ruby":
		return nil, ErrContinuationCommand
	}
	for _, token := range argv {
		if strings.ContainsAny(token, "\x00\n\r;&|<>$`") || token == "--" || token == "--last" || token == "--continue" {
			return nil, ErrContinuationCommand
		}
	}
	return argv, nil
}
