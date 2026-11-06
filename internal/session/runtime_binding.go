package session

import (
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// ErrRuntimeBackendIdentity reports an incomplete or changed runtime binding.
var ErrRuntimeBackendIdentity = errors.New("runtime backend identity is unavailable or invalid")

// RuntimeBackendResolver resolves trusted configuration only for new sessions.
// It cannot retarget existing session rows or infer historical backend identity.
type RuntimeBackendResolver func(CreateOptions) (name, fingerprint string, err error)

// WithRuntimeBackendResolver captures each new session's execution declaration
// before creation. Existing sessions always use their persisted identity.
func WithRuntimeBackendResolver(resolver RuntimeBackendResolver) ManagerOption {
	return func(m *Manager) { m.runtimeBackendResolver = resolver }
}

// RuntimeBackendKey returns the exact registry key for a persisted selection
// and declaration digest. It never resolves a missing selection by fallback.
func RuntimeBackendKey(name, fingerprint string) (string, error) {
	if name == "" || strings.TrimSpace(name) != name || strings.ContainsAny(name, "\x00\r\n") || len(fingerprint) != 64 || strings.ToLower(fingerprint) != fingerprint {
		return "", ErrRuntimeBackendIdentity
	}
	if _, err := hex.DecodeString(fingerprint); err != nil {
		return "", ErrRuntimeBackendIdentity
	}
	return name + "@" + fingerprint, nil
}

// RuntimeBackendResolverForConfig captures new-session selection only when the
// runtime exposes exact backend bindings. Legacy-only factories stay unchanged.
func RuntimeBackendResolverForConfig(cfg *config.City, baseName string, provider runtime.Provider) RuntimeBackendResolver {
	if _, ok := runtime.AsBackendBindingProvider(provider); !ok || cfg == nil {
		return nil
	}
	return func(spec CreateOptions) (string, string, error) {
		binding, err := config.ResolveSessionRuntimeBinding(cfg, baseName, spec.Template, spec.ExtraMeta[NamedSessionIdentityMetadata], spec.Transport)
		if err != nil {
			return "", "", err
		}
		return binding.Name, binding.Fingerprint, nil
	}
}

func (m *Manager) validateCreateRuntime(spec CreateOptions) error {
	for _, key := range []string{beadmeta.RuntimeBackendMetadataKey, beadmeta.RuntimeFingerprintMetadataKey} {
		if _, exists := spec.ExtraMeta[key]; exists {
			return fmt.Errorf("%w: runtime identity must use typed creation fields", ErrRuntimeBackendIdentity)
		}
	}
	if spec.RuntimeBackend == "" && spec.RuntimeFingerprint == "" {
		if _, strict := runtime.AsBackendBindingProvider(m.sp); strict {
			return ErrRuntimeBackendIdentity
		}
		return nil
	}
	key, err := RuntimeBackendKey(spec.RuntimeBackend, spec.RuntimeFingerprint)
	if err != nil {
		return err
	}
	router, ok := runtime.AsBackendBindingProvider(m.sp)
	if !ok {
		return ErrRuntimeBackendIdentity
	}
	return router.ValidateBackend(key)
}

func setCreateRuntimeMetadata(meta map[string]string, spec CreateOptions) {
	if spec.RuntimeBackend != "" {
		meta[beadmeta.RuntimeBackendMetadataKey] = spec.RuntimeBackend
		meta[beadmeta.RuntimeFingerprintMetadataKey] = spec.RuntimeFingerprint
	}
}

func (m *Manager) bindRuntime(info Info) error {
	return BindRuntimeInfo(m.sp, info)
}

// BindRuntimeInfo reconstructs an exact persisted owner without consulting
// templates or probing other runtimes. It does not grant execution authority.
func BindRuntimeInfo(provider runtime.Provider, info Info) error {
	router, ok := runtime.AsBackendBindingProvider(provider)
	if info.RuntimeBackend == "" && info.RuntimeFingerprint == "" {
		if !ok {
			return nil
		}
		return fmt.Errorf("%w: %w", runtime.ErrRuntimeUnavailable, ErrRuntimeBackendIdentity)
	}
	key, err := RuntimeBackendKey(info.RuntimeBackend, info.RuntimeFingerprint)
	if err != nil {
		return fmt.Errorf("%w: %w", runtime.ErrRuntimeUnavailable, err)
	}
	if !ok {
		return fmt.Errorf("%w: %w", runtime.ErrRuntimeUnavailable, ErrRuntimeBackendIdentity)
	}
	if err := router.Bind(info.SessionName, key); err != nil {
		return fmt.Errorf("%w: %w", runtime.ErrRuntimeUnavailable, err)
	}
	return nil
}
