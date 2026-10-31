package session

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/rollout/gate"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/shellquote"
)

func TestStartContinuationMissingNativeKeyDoesNotStartOrRepair(t *testing.T) {
	workDir := t.TempDir()
	raw := beads.NewMemStoreFrom(1, []beads.Bead{{
		ID: "continuation-missing", Revision: 1, Status: "open",
		Labels: []string{LabelSession}, // deliberately repairable, not repaired
		Metadata: map[string]string{
			"state": string(StateSuspended), "session_name": "continuation-missing-runtime",
			"provider": "codex", "work_dir": workDir, "gc.work_dir": workDir,
			"generation": "2", "instance_token": "token-2", "continuation_epoch": "1",
		},
	}}, nil)
	before, err := raw.Get("continuation-missing")
	if err != nil {
		t.Fatal(err)
	}
	sp := runtime.NewFake()
	mgr := NewManagerWithOptions(raw, sp, WithStaleKeyDetectionWaiter(immediateStaleKeyDetectionWaiter))
	prepared := false
	err = mgr.StartContinuation(context.Background(), ContinuationExpectation{
		SessionID: before.ID, Revision: before.Revision, Provider: "codex",
		SessionKey: "11111111-2222-4333-8444-555555555555", Generation: "2",
		InstanceToken: "token-2", ContinuationEpoch: "1", WorkDir: workDir,
	}, func(Info) (ContinuationLaunch, error) {
		prepared = true
		return ContinuationLaunch{Command: "codex --model gpt-6-astra", Hints: runtime.Config{WorkDir: workDir}}, nil
	})
	after, readErr := raw.Get(before.ID)
	if !errors.Is(err, ErrContinuationIdentity) || prepared || sp.CountCalls("Start", "continuation-missing-runtime") != 0 || readErr != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("error=%v prepared=%v starts=%d row_changed=%v read_error=%v", err, prepared, sp.CountCalls("Start", "continuation-missing-runtime"), !reflect.DeepEqual(before, after), readErr)
	}
}

func continuationFixture(t *testing.T) (*beads.MemStore, *runtime.Fake, *Manager, ContinuationExpectation) {
	t.Helper()
	workDir := t.TempDir()
	raw := beads.NewMemStoreFrom(1, []beads.Bead{{
		ID: "continuation-exact", Revision: 1, Type: BeadType, Status: "open", Labels: []string{LabelSession},
		Metadata: map[string]string{
			"state": string(StateSuspended), "session_name": "continuation-exact-runtime",
			"provider": "codex", "session_key": "11111111-2222-4333-8444-555555555555",
			"work_dir": workDir, "gc.work_dir": workDir, "generation": "2",
			"instance_token": "token-2", "continuation_epoch": "1", "resume_flag": "resume", "resume_style": "subcommand",
		},
	}}, nil)
	_, err := beads.OpenStoreAtForCity(context.Background(), beads.StoreOpenOptions{
		ScopeRoot: t.TempDir(), Provider: "file", ConditionalWrites: gate.Require,
		OpenFileStore: func() (beads.Store, error) { return raw, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	row, err := raw.Get("continuation-exact")
	if err != nil {
		t.Fatal(err)
	}
	sp := runtime.NewFake()
	mgr := NewManagerWithOptions(raw, sp, WithStaleKeyDetectionWaiter(immediateStaleKeyDetectionWaiter))
	return raw, sp, mgr, ContinuationExpectation{
		SessionID: row.ID, Revision: row.Revision, Provider: "codex", SessionKey: row.Metadata["session_key"],
		WorkDir: workDir, Generation: "2", InstanceToken: "token-2", ContinuationEpoch: "1",
	}
}

func prepareContinuationFixture(info Info) (ContinuationLaunch, error) {
	return ContinuationLaunch{
		Command: "codex --model gpt-6-astra --cd " + shellquote.Join([]string{info.WorkDir}),
		Resume:  ProviderResume{ResumeFlag: "resume", ResumeStyle: "subcommand"},
		Hints:   runtime.Config{WorkDir: info.WorkDir},
	}, nil
}

func TestStartContinuationStartupDeathNeverStartsFresh(t *testing.T) {
	raw, sp, mgr, expected := continuationFixture(t)
	before, err := raw.Get(expected.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	sp.StartErrors["continuation-exact-runtime"] = runtime.ErrSessionDiedDuringStartup
	err = mgr.StartContinuation(context.Background(), expected, prepareContinuationFixture)
	after, readErr := raw.Get(expected.SessionID)
	if !errors.Is(err, runtime.ErrSessionDiedDuringStartup) || sp.CountCalls("Start", "continuation-exact-runtime") != 1 || readErr != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("error=%v starts=%d row_changed=%v read_error=%v", err, sp.CountCalls("Start", "continuation-exact-runtime"), !reflect.DeepEqual(before, after), readErr)
	}
}

func TestStartContinuationRetainsExactNativeCommandAndContext(t *testing.T) {
	raw, sp, mgr, expected := continuationFixture(t)
	if err := mgr.StartContinuation(context.Background(), expected, prepareContinuationFixture); err != nil {
		t.Fatal(err)
	}
	cfg := sp.LastStartConfig("continuation-exact-runtime")
	want := "codex resume " + expected.SessionKey + " --model gpt-6-astra --cd " + shellquote.Join([]string{expected.WorkDir})
	if cfg == nil || cfg.Command != want || cfg.WorkDir != expected.WorkDir || cfg.Env["GC_SESSION_ID"] != expected.SessionID || cfg.Env["GC_INSTANCE_TOKEN"] != expected.InstanceToken || cfg.Env["GC_CONTINUATION_EPOCH"] != expected.ContinuationEpoch {
		t.Fatalf("config=%+v want_command=%q", cfg, want)
	}
	row, err := raw.Get(expected.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if row.Metadata["state"] != string(StateActive) || row.Metadata["session_key"] != expected.SessionKey || row.Metadata["instance_token"] != expected.InstanceToken || row.Metadata["continuation_epoch"] != expected.ContinuationEpoch || row.Metadata["continuation_reset_pending"] != "" {
		t.Fatalf("metadata=%v", row.Metadata)
	}
}

func TestStartContinuationRejectsChangedExpectationBeforePrepare(t *testing.T) {
	cases := []struct {
		name   string
		change func(*ContinuationExpectation)
	}{
		{"missing-key", func(e *ContinuationExpectation) { e.SessionKey = "" }},
		{"wrong-key", func(e *ContinuationExpectation) { e.SessionKey = "another-thread" }},
		{"wrong-revision", func(e *ContinuationExpectation) { e.Revision++ }},
		{"missing-revision", func(e *ContinuationExpectation) { e.Revision = 0 }},
		{"wrong-provider", func(e *ContinuationExpectation) { e.Provider = "other-provider" }},
		{"missing-provider", func(e *ContinuationExpectation) { e.Provider = "" }},
		{"wrong-generation", func(e *ContinuationExpectation) { e.Generation = "3" }},
		{"noncanonical-generation", func(e *ContinuationExpectation) { e.Generation = "02" }},
		{"wrong-token", func(e *ContinuationExpectation) { e.InstanceToken = "another-token" }},
		{"missing-token", func(e *ContinuationExpectation) { e.InstanceToken = "" }},
		{"wrong-epoch", func(e *ContinuationExpectation) { e.ContinuationEpoch = "2" }},
		{"missing-epoch", func(e *ContinuationExpectation) { e.ContinuationEpoch = "" }},
		{"wrong-worktree", func(e *ContinuationExpectation) { e.WorkDir = "/other/worktree" }},
		{"relative-worktree", func(e *ContinuationExpectation) { e.WorkDir = "native/source" }},
		{"unclean-worktree", func(e *ContinuationExpectation) { e.WorkDir = "/native/../native/source" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, sp, mgr, expected := continuationFixture(t)
			before, err := raw.Get(expected.SessionID)
			if err != nil {
				t.Fatal(err)
			}
			tc.change(&expected)
			prepared := false
			err = mgr.StartContinuation(context.Background(), expected, func(i Info) (ContinuationLaunch, error) {
				prepared = true
				return prepareContinuationFixture(i)
			})
			after, readErr := raw.Get(before.ID)
			if !errors.Is(err, ErrContinuationIdentity) || prepared || sp.CountCalls("Start", "continuation-exact-runtime") != 0 || readErr != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("error=%v prepared=%v starts=%d row_changed=%v read_error=%v", err, prepared, sp.CountCalls("Start", "continuation-exact-runtime"), !reflect.DeepEqual(before, after), readErr)
			}
		})
	}
}

func TestStartContinuationRejectsUnboundResumeCommand(t *testing.T) {
	cases := []struct {
		name   string
		change func(*ContinuationLaunch)
	}{
		{"missing-resume-capability", func(l *ContinuationLaunch) { l.Resume = ProviderResume{} }},
		{"unbound-explicit-template", func(l *ContinuationLaunch) { l.Resume.ResumeCommand = "codex resume --last" }},
		{"prefixed-native-key", func(l *ContinuationLaunch) { l.Resume.ResumeCommand = "codex resume prefix-{{.SessionKey}}" }},
		{"selector-is-model-value", func(l *ContinuationLaunch) { l.Resume.ResumeCommand = "codex --model resume {{.SessionKey}}" }},
		{"generated-flag-after-terminator", func(l *ContinuationLaunch) {
			l.Command = "codex --"
			l.Resume = ProviderResume{ResumeFlag: "--resume", ResumeStyle: "flag"}
		}},
		{"explicit-flag-after-terminator", func(l *ContinuationLaunch) {
			l.Resume = ProviderResume{ResumeFlag: "--resume", ResumeStyle: "flag", ResumeCommand: "codex -- --resume {{.SessionKey}}"}
		}},
		{"explicit-equals-after-terminator", func(l *ContinuationLaunch) {
			l.Resume = ProviderResume{ResumeFlag: "--resume", ResumeStyle: "flag", ResumeCommand: "codex -- --resume={{.SessionKey}}"}
		}},
		{"selector-is-other-option-value", func(l *ContinuationLaunch) {
			l.Resume = ProviderResume{ResumeFlag: "--resume", ResumeStyle: "flag", ResumeCommand: "codex --note --resume {{.SessionKey}}"}
		}},
		{"suffixed-native-key", func(l *ContinuationLaunch) { l.Resume.ResumeCommand = "codex resume {{.SessionKey}}-suffix" }},
		{"key-only-in-unrelated-option", func(l *ContinuationLaunch) {
			l.Resume.ResumeCommand = "codex resume wrong-thread --note {{.SessionKey}}"
		}},
		{"duplicate-key-template", func(l *ContinuationLaunch) { l.Resume.ResumeCommand = "codex resume {{.SessionKey}} {{.SessionKey}}" }},
		{"unresolved-template", func(l *ContinuationLaunch) { l.Resume.ResumeCommand = "codex resume {{.SessionKey}} --cd {{.WorkDir}}" }},
		{"unknown-style", func(l *ContinuationLaunch) { l.Resume.ResumeStyle = "guessed-style" }},
		{"compound-resume-flag", func(l *ContinuationLaunch) { l.Resume.ResumeFlag = "resume --last" }},
		{"missing-command", func(l *ContinuationLaunch) { l.Command = "" }},
		{"conflicting-worktree", func(l *ContinuationLaunch) { l.Hints.WorkDir = "/other/worktree" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, sp, mgr, expected := continuationFixture(t)
			before, err := raw.Get(expected.SessionID)
			if err != nil {
				t.Fatal(err)
			}
			err = mgr.StartContinuation(context.Background(), expected, func(i Info) (ContinuationLaunch, error) {
				l, err := prepareContinuationFixture(i)
				if err != nil {
					return ContinuationLaunch{}, err
				}
				tc.change(&l)
				return l, nil
			})
			after, readErr := raw.Get(before.ID)
			if err == nil || sp.CountCalls("Start", "continuation-exact-runtime") != 0 || readErr != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("error=%v starts=%d row_changed=%v read_error=%v", err, sp.CountCalls("Start", "continuation-exact-runtime"), !reflect.DeepEqual(before, after), readErr)
			}
		})
	}
}

func TestStartContinuationRechecksAfterPreparation(t *testing.T) {
	raw, sp, mgr, expected := continuationFixture(t)
	var externallyChanged beads.Bead
	err := mgr.StartContinuation(context.Background(), expected, func(i Info) (ContinuationLaunch, error) {
		if err := raw.SetMetadata(expected.SessionID, "session_key", "external-successor"); err != nil {
			return ContinuationLaunch{}, err
		}
		var err error
		externallyChanged, err = raw.Get(expected.SessionID)
		if err != nil {
			return ContinuationLaunch{}, err
		}
		return prepareContinuationFixture(i)
	})
	after, readErr := raw.Get(expected.SessionID)
	if !errors.Is(err, ErrContinuationIdentity) || sp.CountCalls("Start", "continuation-exact-runtime") != 0 || readErr != nil || !reflect.DeepEqual(externallyChanged, after) {
		t.Fatalf("error=%v starts=%d external_row_changed=%v read_error=%v", err, sp.CountCalls("Start", "continuation-exact-runtime"), !reflect.DeepEqual(externallyChanged, after), readErr)
	}
}

func TestStartContinuationRefusesIneligibleSessionBeforePrepare(t *testing.T) {
	cases := []struct {
		name  string
		setup func(*beads.MemStore, *runtime.Fake, *ContinuationExpectation) error
	}{
		{"closed", func(r *beads.MemStore, _ *runtime.Fake, e *ContinuationExpectation) error {
			return r.Close(e.SessionID)
		}},
		{"draining", func(r *beads.MemStore, _ *runtime.Fake, e *ContinuationExpectation) error {
			return r.SetMetadata(e.SessionID, "state", string(StateDraining))
		}},
		{"pending-reset", func(r *beads.MemStore, _ *runtime.Fake, e *ContinuationExpectation) error {
			return r.SetMetadata(e.SessionID, "continuation_reset_pending", "true")
		}},
		{"pending-create", func(r *beads.MemStore, _ *runtime.Fake, e *ContinuationExpectation) error {
			return r.SetMetadata(e.SessionID, "pending_create_claim", "true")
		}},
		{"missing-runtime-name", func(r *beads.MemStore, _ *runtime.Fake, e *ContinuationExpectation) error {
			return r.SetMetadata(e.SessionID, "session_name", "")
		}},
		{"malformed-native-key", func(r *beads.MemStore, _ *runtime.Fake, e *ContinuationExpectation) error {
			e.SessionKey = "$(untrusted-command)"
			return r.SetMetadata(e.SessionID, "session_key", e.SessionKey)
		}},
		{"kill-in-progress", func(r *beads.MemStore, _ *runtime.Fake, e *ContinuationExpectation) error {
			return r.SetMetadataBatch(e.SessionID, map[string]string{"state": string(StateAsleep), "state_reason": KillPendingReason, "sleep_reason": string(SleepReasonKilled), "slept_at": time.Now().UTC().Format(time.RFC3339)})
		}},
		{"already-live-without-native-proof", func(_ *beads.MemStore, sp *runtime.Fake, _ *ContinuationExpectation) error {
			return sp.Start(context.Background(), "continuation-exact-runtime", runtime.Config{Command: "unknown-native-process"})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, sp, mgr, expected := continuationFixture(t)
			if err := tc.setup(raw, sp, &expected); err != nil {
				t.Fatal(err)
			}
			before, err := raw.Get(expected.SessionID)
			if err != nil {
				t.Fatal(err)
			}
			expected.Revision = before.Revision
			startsBefore := sp.CountCalls("Start", "continuation-exact-runtime")
			prepared := false
			err = mgr.StartContinuation(context.Background(), expected, func(i Info) (ContinuationLaunch, error) {
				prepared = true
				return prepareContinuationFixture(i)
			})
			after, readErr := raw.Get(before.ID)
			if err == nil || prepared || sp.CountCalls("Start", "continuation-exact-runtime") != startsBefore || readErr != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("error=%v prepared=%v new_starts=%d row_changed=%v read_error=%v", err, prepared, sp.CountCalls("Start", "continuation-exact-runtime")-startsBefore, !reflect.DeepEqual(before, after), readErr)
			}
		})
	}
}

func TestStartContinuationPostStartDeathDoesNotClearOrRetry(t *testing.T) {
	raw, sp, _, expected := continuationFixture(t)
	before, err := raw.Get(expected.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	mgr := NewManagerWithOptions(raw, sp, WithStaleKeyDetectionWaiter(func(_ context.Context, name string) error { return sp.Stop(name) }))
	err = mgr.StartContinuation(context.Background(), expected, prepareContinuationFixture)
	after, readErr := raw.Get(expected.SessionID)
	if !errors.Is(err, runtime.ErrSessionDiedDuringStartup) || sp.CountCalls("Start", "continuation-exact-runtime") != 1 || readErr != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("error=%v starts=%d row_changed=%v read_error=%v", err, sp.CountCalls("Start", "continuation-exact-runtime"), !reflect.DeepEqual(before, after), readErr)
	}
}

type continuationNonConditionalStore struct{ beads.Store }

func TestStartContinuationRequiresConditionalStoreBeforePrepare(t *testing.T) {
	raw, sp, _, expected := continuationFixture(t)
	mgr := NewManagerWithOptions(continuationNonConditionalStore{raw}, sp)
	prepared := false
	err := mgr.StartContinuation(context.Background(), expected, func(i Info) (ContinuationLaunch, error) {
		prepared = true
		return prepareContinuationFixture(i)
	})
	if err == nil || prepared || sp.CountCalls("Start", "continuation-exact-runtime") != 0 {
		t.Fatalf("error=%v prepared=%v calls=%v", err, prepared, sp.Calls)
	}
}

func TestStartContinuationDoesNotReportPostStartRaceAsSuccess(t *testing.T) {
	raw, sp, _, expected := continuationFixture(t)
	var externallyChanged beads.Bead
	mgr := NewManagerWithOptions(raw, sp, WithStaleKeyDetectionWaiter(func(context.Context, string) error {
		if err := raw.SetMetadata(expected.SessionID, "session_key", "external-successor"); err != nil {
			return err
		}
		var err error
		externallyChanged, err = raw.Get(expected.SessionID)
		return err
	}))
	err := mgr.StartContinuation(context.Background(), expected, prepareContinuationFixture)
	after, readErr := raw.Get(expected.SessionID)
	if !beads.IsPreconditionFailed(err) || sp.CountCalls("Start", "continuation-exact-runtime") != 1 || sp.CountCalls("Stop", "continuation-exact-runtime") != 0 || readErr != nil || !reflect.DeepEqual(externallyChanged, after) {
		t.Fatalf("error=%v starts=%d stops=%d external_row_changed=%v read_error=%v", err, sp.CountCalls("Start", "continuation-exact-runtime"), sp.CountCalls("Stop", "continuation-exact-runtime"), !reflect.DeepEqual(externallyChanged, after), readErr)
	}
}

func TestStartContinuationRequiresNoSurvivingRuntime(t *testing.T) {
	cases := []struct {
		name     string
		provider func(*runtime.Fake, ContinuationExpectation) runtime.Provider
	}{
		{"scan-unavailable", func(sp *runtime.Fake, _ ContinuationExpectation) runtime.Provider {
			return providerWithoutProcessScanner{sp}
		}},
		{"scan-error", func(sp *runtime.Fake, _ ContinuationExpectation) runtime.Provider {
			return &orphanScanProvider{Fake: sp, findErr: errors.New("scan unavailable")}
		}},
		{"untracked-survivor", func(sp *runtime.Fake, e ContinuationExpectation) runtime.Provider {
			return &orphanScanProvider{Fake: sp, results: []runtime.LiveRuntime{{SessionID: e.SessionID, PID: 123, IsTracked: false}}}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, sp, _, expected := continuationFixture(t)
			before, err := raw.Get(expected.SessionID)
			if err != nil {
				t.Fatal(err)
			}
			provider := tc.provider(sp, expected)
			mgr := NewManagerWithOptions(raw, provider, WithStaleKeyDetectionWaiter(immediateStaleKeyDetectionWaiter))
			prepared := false
			err = mgr.StartContinuation(context.Background(), expected, func(i Info) (ContinuationLaunch, error) { prepared = true; return prepareContinuationFixture(i) })
			after, readErr := raw.Get(expected.SessionID)
			if err == nil || prepared || sp.CountCalls("Start", "continuation-exact-runtime") != 0 || readErr != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("error=%v prepared=%v calls=%v row_changed=%v", err, prepared, sp.Calls, !reflect.DeepEqual(before, after))
			}
			if scan, ok := provider.(*orphanScanProvider); ok {
				for _, event := range scan.events {
					if strings.HasPrefix(event, "terminate:") {
						t.Fatalf("strict continuation killed an unverified survivor: %s", event)
					}
				}
			}
		})
	}
}

func TestStartContinuationQuotedExecutableKeepsExactNativeArgument(t *testing.T) {
	_, sp, mgr, expected := continuationFixture(t)
	const program = "/Applications/Native Tools/codex"
	err := mgr.StartContinuation(context.Background(), expected, func(i Info) (ContinuationLaunch, error) {
		launch, err := prepareContinuationFixture(i)
		launch.Command = shellquote.Join([]string{program, "--model", "gpt-6-astra", "--cd", i.WorkDir})
		return launch, err
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg := sp.LastStartConfig("continuation-exact-runtime")
	want := []string{program, "resume", expected.SessionKey, "--model", "gpt-6-astra", "--cd", expected.WorkDir}
	if cfg == nil || !reflect.DeepEqual(shellquote.Split(cfg.Command), want) {
		t.Fatalf("config=%+v want_argv=%q", cfg, want)
	}
}

func TestStartContinuationExactTemplateArgumentForms(t *testing.T) {
	for _, form := range []string{"quoted-subcommand", "flag-equals", "generated-flag"} {
		t.Run(form, func(t *testing.T) {
			_, sp, mgr, expected := continuationFixture(t)
			err := mgr.StartContinuation(context.Background(), expected, func(i Info) (ContinuationLaunch, error) {
				launch, err := prepareContinuationFixture(i)
				switch form {
				case "quoted-subcommand":
					launch.Resume.ResumeCommand = "codex resume '{{.SessionKey}}' --cd " + shellquote.Join([]string{i.WorkDir})
				case "flag-equals":
					launch.Resume = ProviderResume{ResumeFlag: "--resume", ResumeStyle: "flag", ResumeCommand: "codex --resume={{.SessionKey}} --cd " + shellquote.Join([]string{i.WorkDir})}
				default:
					launch.Resume = ProviderResume{ResumeFlag: "--resume", ResumeStyle: "flag"}
				}
				return launch, err
			})
			if err != nil {
				t.Fatal(err)
			}
			cfg := sp.LastStartConfig("continuation-exact-runtime")
			want := []string{"codex", "resume", expected.SessionKey, "--cd", expected.WorkDir}
			switch form {
			case "flag-equals":
				want = []string{"codex", "--resume=" + expected.SessionKey, "--cd", expected.WorkDir}
			case "generated-flag":
				want = []string{"codex", "--resume", expected.SessionKey, "--model", "gpt-6-astra", "--cd", expected.WorkDir}
			}
			if cfg == nil || !reflect.DeepEqual(shellquote.Split(cfg.Command), want) {
				t.Fatalf("config=%+v want_argv=%q", cfg, want)
			}
		})
	}
}

func TestStartContinuationRefusesUnsupportedShellComposition(t *testing.T) {
	for _, command := range []string{"env codex", "sh -c codex", "X=1 codex", "codex && other", "codex --model m; other", "codex 'unterminated"} {
		t.Run(command, func(t *testing.T) {
			raw, sp, mgr, expected := continuationFixture(t)
			before, err := raw.Get(expected.SessionID)
			if err != nil {
				t.Fatal(err)
			}
			err = mgr.StartContinuation(context.Background(), expected, func(i Info) (ContinuationLaunch, error) {
				launch, err := prepareContinuationFixture(i)
				launch.Command = command
				return launch, err
			})
			after, readErr := raw.Get(expected.SessionID)
			if err == nil || sp.CountCalls("Start", "continuation-exact-runtime") != 0 || readErr != nil || !reflect.DeepEqual(before, after) {
				t.Fatalf("error=%v starts=%d row_changed=%v", err, sp.CountCalls("Start", "continuation-exact-runtime"), !reflect.DeepEqual(before, after))
			}
		})
	}
}
