// Package runisolation selects the startup and cleanup hooks for integration
// test runs that own a private temporary root.
package runisolation

import "fmt"

const (
	// EnvName enables a private integration run when set to "1".
	EnvName = "GC_INTEGRATION_RUN_OWNED"
	// SubprocessProvider is the only session provider supported by owned runs.
	SubprocessProvider = "subprocess"
	// IsolatedDoltIdentity is the only Dolt identity mode supported by owned runs.
	IsolatedDoltIdentity = "isolated"
)

// Mode identifies whether TestMain owns a private temporary root.
type Mode uint8

const (
	// Legacy keeps the existing shared integration cleanup behavior.
	Legacy Mode = iota
	// Owned uses a private temporary root and skips cross-run cleanup hooks.
	Owned
)

// Resolve validates the explicit run mode and its required isolation settings.
// Empty runOwned keeps the existing integration-test behavior. Owned mode is
// opt-in and requires both the subprocess provider and isolated Dolt identity.
func Resolve(runOwned, sessionProvider, doltIdentity string) (Mode, error) {
	switch runOwned {
	case "":
		return Legacy, nil
	case "1":
		if sessionProvider != SubprocessProvider {
			return Legacy, fmt.Errorf("%s=1 requires GC_SESSION=%s", EnvName, SubprocessProvider)
		}
		if doltIdentity != IsolatedDoltIdentity {
			return Legacy, fmt.Errorf("%s=1 requires GC_INTEGRATION_DOLT_IDENTITY_MODE=%s", EnvName, IsolatedDoltIdentity)
		}
		return Owned, nil
	default:
		return Legacy, fmt.Errorf("%s=%q is invalid; expected empty or 1", EnvName, runOwned)
	}
}

// Startup prepares the selected run and invokes cross-run startup hooks only
// for legacy runs. TestMain and the no-TestMain tests share this dispatcher.
func Startup(mode Mode, prepare func(Mode) error, legacySweeps func() error) error {
	if mode != Legacy && mode != Owned {
		return fmt.Errorf("unknown integration run mode %d", mode)
	}
	if prepare == nil {
		return fmt.Errorf("integration run preparation callback is nil")
	}
	if mode == Legacy && legacySweeps == nil {
		return fmt.Errorf("legacy integration startup callback is nil")
	}
	if err := prepare(mode); err != nil {
		return err
	}
	if mode == Legacy {
		return legacySweeps()
	}
	return nil
}

// Finish selects the run cleanup path. The owned callback must clean only
// resources beneath its private root and remove that root with empty-only
// removal after all preservation and process checks pass.
func Finish(mode Mode, legacyCleanup, ownedCleanup func() error) error {
	switch mode {
	case Legacy:
		if legacyCleanup == nil {
			return fmt.Errorf("legacy integration cleanup callback is nil")
		}
		return legacyCleanup()
	case Owned:
		if ownedCleanup == nil {
			return fmt.Errorf("owned integration cleanup callback is nil")
		}
		return ownedCleanup()
	default:
		return fmt.Errorf("unknown integration run mode %d", mode)
	}
}

// OnSignal runs the legacy cross-run sweep only for legacy mode.
func OnSignal(mode Mode, legacySweep func()) {
	if mode == Legacy && legacySweep != nil {
		legacySweep()
	}
}
