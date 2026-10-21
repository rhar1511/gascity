package beads

import (
	"errors"
	"os/exec"
	"reflect"
	"testing"
)

// Ordering/error policy uses a scripted output port. Actual Go identity,
// path selection and FD execution remain in the Linux integration owners.
func TestRunObservedCommandPolicy(t *testing.T) {
	sentinel := errors.New("command")
	before := errors.New("before")
	after := errors.New("after")
	lookup := errors.New("lookup")
	for _, tc := range []struct {
		name                                       string
		observer                                   bool
		missingBegin, missingObserve               bool
		lookupErr, beforeErr, commandErr, afterErr error
		wantEvents                                 []string
		wantOutput                                 string
		wantErrors                                 []error
		wantErr                                    bool
	}{
		{name: "default unchanged", wantEvents: []string{"output"}, wantOutput: "fixture"},
		{name: "missing begin refuses before output", observer: true, missingBegin: true, wantErr: true},
		{name: "missing observe refuses before output", observer: true, missingObserve: true, wantErr: true},
		{name: "lookup failure resets before output", observer: true, lookupErr: lookup, wantEvents: []string{"begin", "output"}, wantErrors: []error{lookup}, wantErr: true},
		{name: "observation failure prevents output", observer: true, beforeErr: before, wantEvents: []string{"begin", "observe"}, wantErrors: []error{before}, wantErr: true},
		{name: "success orders callbacks", observer: true, wantEvents: []string{"begin", "observe", "output", "finish"}, wantOutput: "fixture"},
		{name: "finalizer failure retains output", observer: true, afterErr: after, wantEvents: []string{"begin", "observe", "output", "finish"}, wantOutput: "fixture", wantErrors: []error{after}, wantErr: true},
		{name: "command failure identity unchanged", observer: true, commandErr: sentinel, wantEvents: []string{"begin", "observe", "output", "finish"}, wantOutput: "fixture", wantErrors: []error{sentinel}, wantErr: true},
		{name: "both failures retained", observer: true, commandErr: sentinel, afterErr: after, wantEvents: []string{"begin", "observe", "output", "finish"}, wantOutput: "fixture", wantErrors: []error{sentinel, after}, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := &exec.Cmd{Path: "/fixture/bd", Args: []string{"/fixture/bd"}, Err: tc.lookupErr}
			var events []string
			stale := true
			var observer *ExecutableObserver
			if tc.observer {
				observer = &ExecutableObserver{BeginAttempt: func() { events = append(events, "begin"); stale = false }, Observe: func(selected *exec.Cmd) (func() error, error) {
					events = append(events, "observe")
					if selected != cmd {
						t.Fatal("selected command changed")
					}
					if tc.beforeErr != nil {
						return nil, tc.beforeErr
					}
					return func() error { events = append(events, "finish"); return tc.afterErr }, nil
				}}
				if tc.missingBegin {
					observer.BeginAttempt = nil
				}
				if tc.missingObserve {
					observer.Observe = nil
				}
			}
			output := func(selected *exec.Cmd) ([]byte, error) {
				events = append(events, "output")
				if selected != cmd {
					t.Fatal("output selected another command")
				}
				if selected.Err != nil {
					return nil, selected.Err
				}
				return []byte("fixture"), tc.commandErr
			}
			got, err := runObservedCommandOutput(cmd, observer, output)
			if !reflect.DeepEqual(events, tc.wantEvents) || string(got) != tc.wantOutput || (err != nil) != tc.wantErr {
				t.Fatalf("events=%v out=%q err=%v", events, got, err)
			}
			for _, wanted := range tc.wantErrors {
				if !errors.Is(err, wanted) {
					t.Fatalf("missing original error %v in %v", wanted, err)
				}
			}
			// This contract preserves the exact error object; errors.Is also accepts wrappers.
			if tc.name == "command failure identity unchanged" && reflect.ValueOf(err) != reflect.ValueOf(sentinel) {
				t.Fatal("command error identity changed")
			}
			if tc.lookupErr != nil && stale {
				t.Fatal("lookup failure kept stale proof")
			}
		})
	}
}
