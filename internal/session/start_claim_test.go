package session

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/testutil"
)

// Model the host wrapper claiming work before the runtime can report its
// agent ready. Both startup and claim use the actual session APIs.
type runtimeOnlyClaimBeforeReady struct {
	*runtime.Fake
	store     *Store
	expected  Info
	claimDone chan error
}

func (p *runtimeOnlyClaimBeforeReady) Start(ctx context.Context, name string, cfg runtime.Config) error {
	go func() {
		p.claimDone <- p.store.CommitWorkClaim(p.expected, func(Info, PersistedResponse) (string, string, error) {
			return "diagnostic-step", "1", nil
		})
	}()
	select {
	case err := <-p.claimDone:
		p.claimDone <- err
		if err != nil {
			return err
		}
		return p.Fake.Start(ctx, name, cfg)
	case <-ctx.Done():
		return errors.New("provider readiness blocked waiting for startup work claim")
	}
}

func TestRuntimeOnlyStartAllowsClaimBeforeProviderReady(t *testing.T) {
	for _, state := range []string{"active", "creating", "start-pending"} {
		t.Run(state, func(t *testing.T) {
			store, raw, initial := workClaimSessionFixture(t)
			if err := raw.SetMetadataBatch(initial.ID, map[string]string{"state": state, "session_key": ""}); err != nil {
				t.Fatal(err)
			}
			expected, err := store.Get(initial.ID)
			if err != nil {
				t.Fatal(err)
			}
			p := &runtimeOnlyClaimBeforeReady{Fake: runtime.NewFake(), store: store, expected: expected, claimDone: make(chan error, 1)}
			m := NewManagerWithOptions(raw, p, WithStaleKeyDetectionWaiter(immediateStaleKeyDetectionWaiter))
			ctx, cancel := context.WithTimeout(context.Background(), testutil.GoroutineRaceTimeout)
			defer cancel()
			err = m.StartRuntimeOnly(ctx, expected.ID, "diagnostic-wrapper", runtime.Config{})
			// Ensure a blocked claim is released when Start returns; leave no goroutine.
			select {
			case claimErr := <-p.claimDone:
				if claimErr != nil {
					t.Fatalf("claim: %v", claimErr)
				}
			case <-time.After(goroutineHangBudget):
				t.Fatal("claim did not finish after startup returned")
			}
			if err != nil {
				t.Fatalf("startup could not complete its pre-ready claim: %v", err)
			}
		})
	}
}
