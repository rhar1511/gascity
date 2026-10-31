package main

import (
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/continuationprep"
	"github.com/gastownhall/gascity/internal/session"
)

// prepareTrustedContinuation composes the SDK preparation with canonical
// CLI-owned city, overlay and session-live hints. It remains opt-in and has no
// launch route. Final material/trust/freshness and provider-enforced exclusive
// admission must be verified at consumption, never inferred from this result.
func prepareTrustedContinuation(cityPath string, cfg *config.City, info session.Info, expected session.ContinuationExpectation, captureSource, nativeToken string) (session.ContinuationLaunch, error) {
	prepared, err := continuationprep.Prepare(continuationprep.Request{CityPath: cityPath, City: cfg, Session: info, Expected: expected, CaptureSource: captureSource, NativeToken: nativeToken})
	if err != nil {
		return session.ContinuationLaunch{}, err
	}
	launch := prepared.Launch
	configured, resolved := prepared.Agent, prepared.Provider
	launch.Hints.Env = resolvedWorkerSessionEnvWithConfig(cityPath, cfg, resolved)
	if len(configured.SessionLive) > 0 {
		setup := sessionSetupContextForAgent(cityPath, cfg.EffectiveCityName(), configured.QualifiedName(), &configured, cfg.Rigs)
		setup.Session, setup.WorkDir = info.SessionName, expected.WorkDir
		setup.ConfigDir = resolveConfigDir(cityPath, configured.SourceDir)
		launch.Hints.SessionLive = expandSessionSetup(configured.SessionLive, setup)
	}
	applyWorkerOverlayHints(&launch.Hints, cfg, cityPath, info.Template, resolved)
	return launch, nil
}
