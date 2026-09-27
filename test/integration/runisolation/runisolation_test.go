package runisolation

import (
	"errors"
	"reflect"
	"testing"
)

func TestResolveRequiresOwnedProviderAndIdentity(t *testing.T) {
	tests := []struct {
		name     string
		runOwned string
		session  string
		identity string
		want     Mode
		wantErr  bool
	}{
		{name: "legacy defaults", want: Legacy},
		{name: "owned valid", runOwned: "1", session: SubprocessProvider, identity: IsolatedDoltIdentity, want: Owned},
		{name: "owned flag invalid", runOwned: "true", session: SubprocessProvider, identity: IsolatedDoltIdentity, wantErr: true},
		{name: "provider missing", runOwned: "1", identity: IsolatedDoltIdentity, wantErr: true},
		{name: "provider tmux", runOwned: "1", session: "tmux", identity: IsolatedDoltIdentity, wantErr: true},
		{name: "identity missing", runOwned: "1", session: SubprocessProvider, wantErr: true},
		{name: "identity global", runOwned: "1", session: SubprocessProvider, identity: "global", wantErr: true},
		{name: "identity skipped", runOwned: "1", session: SubprocessProvider, identity: "skip", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Resolve(tt.runOwned, tt.session, tt.identity)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Resolve() error = %v, wantErr %v", err, tt.wantErr)
			}
			if err == nil && got != tt.want {
				t.Fatalf("Resolve() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestStartupDispatchUsesPrepareAndLegacySweepHooks(t *testing.T) {
	tests := []struct {
		name       string
		mode       Mode
		wantCalls  []string
		prepareErr error
	}{
		{name: "legacy", mode: Legacy, wantCalls: []string{"prepare", "legacy-sweep"}},
		{name: "owned has no cross-run startup sweep", mode: Owned, wantCalls: []string{"prepare"}},
		{name: "prepare failure stops before sweep", mode: Legacy, wantCalls: []string{"prepare"}, prepareErr: errors.New("prepare failed")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls []string
			err := Startup(tt.mode, func(mode Mode) error {
				calls = append(calls, "prepare")
				if mode != tt.mode {
					t.Fatalf("prepare mode = %v, want %v", mode, tt.mode)
				}
				return tt.prepareErr
			}, func() error {
				calls = append(calls, "legacy-sweep")
				return nil
			})
			if (err != nil) != (tt.prepareErr != nil) {
				t.Fatalf("Startup() error = %v, want prepare error %v", err, tt.prepareErr)
			}
			if !reflect.DeepEqual(calls, tt.wantCalls) {
				t.Fatalf("Startup() calls = %v, want %v", calls, tt.wantCalls)
			}
		})
	}
}

func TestFinishDispatchUsesOnlySelectedCleanup(t *testing.T) {
	tests := []struct {
		name      string
		mode      Mode
		wantCalls []string
	}{
		{name: "legacy", mode: Legacy, wantCalls: []string{"legacy-cleanup"}},
		{name: "owned", mode: Owned, wantCalls: []string{"owned-cleanup"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls []string
			err := Finish(tt.mode, func() error {
				calls = append(calls, "legacy-cleanup")
				return nil
			}, func() error {
				calls = append(calls, "owned-cleanup")
				return nil
			})
			if err != nil {
				t.Fatalf("Finish() error = %v", err)
			}
			if !reflect.DeepEqual(calls, tt.wantCalls) {
				t.Fatalf("Finish() calls = %v, want %v", calls, tt.wantCalls)
			}
		})
	}
}

func TestSignalDispatchSkipsCrossRunSweepForOwnedMode(t *testing.T) {
	for _, tt := range []struct {
		name     string
		mode     Mode
		wantCall bool
	}{
		{name: "legacy", mode: Legacy, wantCall: true},
		{name: "owned", mode: Owned},
	} {
		t.Run(tt.name, func(t *testing.T) {
			called := false
			OnSignal(tt.mode, func() { called = true })
			if called != tt.wantCall {
				t.Fatalf("OnSignal() called legacy sweep = %t, want %t", called, tt.wantCall)
			}
		})
	}
}
