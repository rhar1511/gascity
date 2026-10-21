//go:build integration && linux

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/contract"
)

// The deliberately unreleased Go fixture exercises real selection and build
// info. It cannot supply stable-release source compatibility or native DB proof.
func buildPreflightContextFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, "cmd", "bd")
	if err := os.MkdirAll(src, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/contextfixture\n\ngo 1.26.9\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	program := `package main
import ("fmt"; "os"; "strconv")
func main() {
 if len(os.Args)!=3 || os.Args[1]!="context" || os.Args[2]!="--json" { os.Exit(91) }
 out, err := os.ReadFile(os.Getenv("BD_CONTEXT_FIXTURE_OUTPUT")); if err!=nil { fmt.Fprintln(os.Stderr,err); os.Exit(92) }
 fmt.Print(string(out))
 if os.Getenv("BD_CONTEXT_FIXTURE_EXIT")!="" { fmt.Fprintln(os.Stderr,"broken pipe"); n,_:=strconv.Atoi(os.Getenv("BD_CONTEXT_FIXTURE_EXIT")); os.Exit(n) }
}
`
	if err := os.WriteFile(filepath.Join(src, "main.go"), []byte(program), 0o600); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(dir, "bd")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	runner := beads.ExecCommandRunnerWithEnvContext(ctx, nil)
	if out, err := runner(dir, filepath.Join(runtime.GOROOT(), "bin", "go"), "build", "-o", binary, "./cmd/bd"); err != nil {
		t.Fatalf("fixture build: %v\n%s", err, out)
	}
	return binary
}

func writePreflightContextPayload(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "context.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPreflightBDContextObservesSelectedWorkspaceExecutable(t *testing.T) {
	binary := buildPreflightContextFixture(t)
	t.Setenv("BD_BIN", filepath.Join(t.TempDir(), "wrong-ambient-bd"))
	for _, tc := range []struct {
		name, body, exit string
		protocolError    bool
	}{
		{"wire format one", `{"backend":"dolt","dolt_mode":"server","bd_version":"v1.3.2","schema_version":1}`, "", false},
		{"malformed JSON preserves original error", `{`, "", true},
		{"typed JSON preserves original error", `{"schema_version":"1"}`, "", true},
		{"nonzero command retains observation", `{}`, "17", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			city := t.TempDir()
			writeBoundCityFixture(t, city)
			payload := writePreflightContextPayload(t, tc.body)
			// The complete-binding runner projects city identity and the pinned bd;
			// arbitrary workspace.env entries are not a command environment API.
			t.Setenv("BD_CONTEXT_FIXTURE_OUTPUT", payload)
			t.Setenv("BD_CONTEXT_FIXTURE_EXIT", tc.exit)
			cfg := fmt.Sprintf("[workspace]\nname=\"context-test\"\n[workspace.env]\nBD_BIN=%q\n", binary)
			if err := os.WriteFile(filepath.Join(city, "city.toml"), []byte(cfg), 0o600); err != nil {
				t.Fatal(err)
			}
			got, err := preflightBDContextReader(city)(city)
			if got.CLIArtifact.ModulePath != "example.com/contextfixture" || got.CLIArtifact.Version != "(devel)" || got.CLIArtifact.Sum != "" {
				t.Fatalf("actual selected fixture identity missing or fabricated: %+v err=%v", got.CLIArtifact, err)
			}
			if tc.protocolError {
				var syntax *json.SyntaxError
				var typed *json.UnmarshalTypeError
				if !errors.Is(err, contract.ErrBDContextProtocol) || !(errors.As(err, &syntax) || errors.As(err, &typed)) {
					t.Fatalf("protocol/original JSON error lost: %v", err)
				}
			} else if tc.exit != "" {
				var exit *exec.ExitError
				if !errors.As(err, &exit) || exit.ExitCode() != 17 {
					t.Fatalf("original exit lost: %v", err)
				}
			} else if err != nil || got.SchemaVersion != 1 || got.Backend != "dolt" || got.DoltMode != "server" {
				t.Fatalf("wire context=%+v err=%v", got, err)
			}
		})
	}
}

// Bad observer records must fail before environment preparation, never panic
// or begin a partial observation which can leave earlier evidence reusable.
func TestBDRunnerRejectsInvalidObservation(t *testing.T) {
	for _, route := range []string{"complete", "managed"} {
		for _, shape := range []string{"missing begin", "missing observe", "multiple"} {
			t.Run(route+"/"+shape, func(t *testing.T) {
				defer func() {
					if p := recover(); p != nil {
						t.Errorf("invalid observer panicked: %v", p)
					}
				}()
				begins, envCalls := 0, 0
				o := &beads.ExecutableObserver{BeginAttempt: func() { begins++ }, Observe: func(*exec.Cmd) (func() error, error) { return nil, nil }}
				observers := []*beads.ExecutableObserver{o}
				switch shape {
				case "missing begin":
					o.BeginAttempt = nil
				case "missing observe":
					o.Observe = nil
				case "multiple":
					observers = append(observers, o)
				}
				var runner beads.CommandRunner
				if route == "complete" {
					runner = bdContextCommandRunnerForCity("/nonexistent-observer-test-city", observers...)
				} else {
					runner = bdCommandRunnerWithManagedRetryErr("/nonexistent-observer-test-city", func(string) (map[string]string, error) {
						envCalls++
						return nil, errors.New("environment must not be called")
					}, observers...)
				}
				out, err := runner("/nonexistent-observer-test-city", "bd", "context", "--json")
				if err == nil || !strings.Contains(err.Error(), "observer") || len(out) != 0 || begins != 0 || envCalls != 0 {
					t.Fatalf("invalid record side effect: err=%v out=%q begins=%d envCalls=%d", err, out, begins, envCalls)
				}
			})
		}
	}
}

func TestBDManagedRetryClearsObservationBeforeRetryEnvFailure(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	binary := buildPreflightContextFixture(t)
	payload := writePreflightContextPayload(t, `{}`)
	city := t.TempDir()
	oldSleep, oldRecover := bdCommandRetrySleep, recoverManagedBDCommand
	t.Cleanup(func() { bdCommandRetrySleep = oldSleep; recoverManagedBDCommand = oldRecover })
	sleeps, recoveries := 0, 0
	bdCommandRetrySleep = func(time.Duration) { sleeps++ }
	recoverManagedBDCommand = func(string) error { recoveries++; return errors.New("unexpected recovery") }
	observation := &contract.BeadsExecutableObservation{}
	envCalls := 0
	want := errors.New("retry environment unavailable")
	runner := bdCommandRunnerWithManagedRetryErr(city, func(string) (map[string]string, error) {
		envCalls++
		if envCalls == 2 {
			if observation.Artifact() != (contract.BeadsArtifact{}) {
				t.Error("old executable proof survived retry environment preparation")
			}
			return nil, want
		}
		return map[string]string{"BD_BIN": binary, "BD_CONTEXT_FIXTURE_OUTPUT": payload, "BD_CONTEXT_FIXTURE_EXIT": "17"}, nil
	}, &beads.ExecutableObserver{BeginAttempt: observation.BeginAttempt, Observe: observation.Observe})
	out, err := runner(city, "bd", "context", "--json")
	if !errors.Is(err, want) || len(out) != 0 || envCalls != 2 || sleeps != 1 || recoveries != 0 || observation.Artifact() != (contract.BeadsArtifact{}) {
		t.Fatalf("retry not isolated: err=%v out=%q envCalls=%d sleeps=%d recoveries=%d artifact=%+v", err, out, envCalls, sleeps, recoveries, observation.Artifact())
	}
}

func TestBDHostedObserverUsesHermeticEnvironment(t *testing.T) {
	binary := buildPreflightContextFixture(t)
	city := writeHostedBeadsCity(t, "https://beads.example", "gasworks", false)
	t.Setenv("BEADS_PRIVATE_INHERITED", "must-not-leak")
	payload := writePreflightContextPayload(t, `{}`)
	observation := &contract.BeadsExecutableObservation{}
	seen := false
	runner, err := beadsCommandRunnerForHostedCity(city, map[string]string{"BD_BIN": binary, "BD_CONTEXT_FIXTURE_OUTPUT": payload, "BEADS_PRIVATE_EXPLICIT": "bound"}, &beads.ExecutableObserver{BeginAttempt: observation.BeginAttempt, Observe: func(cmd *exec.Cmd) (func() error, error) {
		seen = true
		env := envEntriesMap(cmd.Env)
		if _, present := env["BEADS_PRIVATE_INHERITED"]; present {
			t.Error("hosted command inherited Beads namespace")
		}
		if env["BEADS_PRIVATE_EXPLICIT"] != "bound" || env["BD_BIN"] != binary || cmd.Path != binary || cmd.Dir != city {
			t.Error("selected hosted command lost explicit inputs")
		}
		return observation.Observe(cmd)
	}})
	if err != nil {
		t.Fatal(err)
	}
	out, err := runner(city, "bd", "context", "--json")
	if err != nil || string(out) != "{}" || !seen || observation.Artifact().ModulePath != "example.com/contextfixture" {
		t.Fatalf("hosted selection failed: out=%q err=%v seen=%v artifact=%+v", out, err, seen, observation.Artifact())
	}
}

// The metadata dispatch probe can fail before selecting complete or managed
// routing. It is still a new attempt: old proof must not survive that failure.
func TestBDCommandRunnerClearsObservationBeforeBindingReadError(t *testing.T) {
	city := t.TempDir()
	if err := os.MkdirAll(scopeMetadataJSONPath(city), 0o700); err != nil {
		t.Fatal(err)
	}
	stale := true
	begins, observations := 0, 0
	observer := &beads.ExecutableObserver{
		BeginAttempt: func() { begins++; stale = false },
		Observe: func(*exec.Cmd) (func() error, error) {
			observations++
			return nil, nil
		},
	}
	runner := bdCommandRunnerForCity(city, observer)
	out, err := runner(city, "bd", "context", "--json")
	if !errors.Is(err, syscall.EISDIR) || len(out) != 0 || stale || begins != 1 || observations != 0 {
		t.Fatalf("binding read failure retained proof or lost cause: err=%v out=%q stale=%v begins=%d observations=%d", err, out, stale, begins, observations)
	}
}
