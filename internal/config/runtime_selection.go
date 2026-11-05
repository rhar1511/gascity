package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

// RuntimeBinding is the resolved runtime selection and immutable declaration
// fingerprint. Coding providers, models and session names are separate axes.
type RuntimeBinding struct {
	Name        string
	Fingerprint string
}

// ResolveSessionRuntimeBinding resolves a NEW session's named override,
// template override and city default in that order. Explicit overrides must
// name declared exec runtimes; ACP remains a separate transport selection.
// Existing sessions must use their persisted binding instead of this resolver.
func ResolveSessionRuntimeBinding(cfg *City, baseName, template, namedIdentity, transport string) (RuntimeBinding, error) {
	if cfg == nil {
		return RuntimeBinding{}, fmt.Errorf("runtime selection: city configuration is missing")
	}
	selected := ""
	agent := FindAgent(cfg, template)
	if agent != nil {
		selected = agent.Runtime
	}
	if namedIdentity != "" {
		named := FindNamedSession(cfg, namedIdentity)
		if named == nil || named.QualifiedName() != namedIdentity || named.TemplateQualifiedName() != template {
			return RuntimeBinding{}, fmt.Errorf("runtime selection: named session %q does not match template %q", namedIdentity, template)
		}
		if named.Runtime != "" {
			selected = named.Runtime
		}
	}
	return ResolveSelectedRuntimeBinding(cfg, baseName, selected, transport)
}

// ResolveSelectedRuntimeBinding validates a resolved template/named override.
// It is for new sessions; an existing session supplies its persisted identity.
func ResolveSelectedRuntimeBinding(cfg *City, baseName, selected, transport string) (RuntimeBinding, error) {
	if cfg == nil {
		return RuntimeBinding{}, fmt.Errorf("runtime selection: city configuration is missing")
	}
	switch {
	case selected != "":
		if strings.TrimSpace(selected) != selected {
			return RuntimeBinding{}, fmt.Errorf("runtime selection: noncanonical runtime %q", selected)
		}
		if _, exists := cfg.Runtimes[selected]; !exists {
			return RuntimeBinding{}, fmt.Errorf("runtime selection: unknown named runtime %q", selected)
		}
		if transport == SessionTransportACP {
			return RuntimeBinding{}, fmt.Errorf("runtime selection: explicit runtime %q cannot be combined with ACP transport", selected)
		}
	case transport == SessionTransportACP:
		selected = "acp"
	default:
		selected = baseName
		if selected == "" {
			selected = "tmux"
		}
	}
	return RuntimeBindingForName(cfg, selected)
}

// RuntimeBindingForName fingerprints one resolved execution declaration. The
// registry separately validates builtin/prefix selections; this pure function
// performs no provider construction, observation or runtime ownership inference.
func RuntimeBindingForName(cfg *City, name string) (RuntimeBinding, error) {
	if cfg == nil || name == "" || strings.TrimSpace(name) != name || strings.ContainsAny(name, "\x00\r\n") {
		return RuntimeBinding{}, fmt.Errorf("runtime selection: invalid runtime identity %q", name)
	}
	input := struct {
		Name     string
		Named    *DiscoveredRuntime `json:",omitempty"`
		Session  *SessionConfig     `json:",omitempty"`
		CityName string             `json:",omitempty"`
	}{Name: name}
	if declared, exists := cfg.Runtimes[name]; exists {
		if declared.Name != "" && declared.Name != name || strings.TrimSpace(declared.Command) == "" || declared.Protocol != supportedRuntimeProtocol {
			return RuntimeBinding{}, fmt.Errorf("runtime selection: invalid declaration for %q", name)
		}
		declared.Name = name
		input.Named = &declared
	} else {
		// Retain only settings consumed by the selected builtin constructor.
		// In particular, ACP/K8s changes do not invalidate terminal owners.
		source := cfg.Session
		var settings SessionConfig
		switch name {
		case "herdr":
			settings.SetupTimeout = source.SetupTimeoutDuration().String()
			settings.SetupMaxTimeout = source.SetupMaxTimeoutDuration().String()
			input.CityName = cfg.Workspace.Name
		case "tmux", "hybrid":
			settings.SetupTimeout = source.SetupTimeoutDuration().String()
			settings.SetupMaxTimeout = source.SetupMaxTimeoutDuration().String()
			settings.NudgeReadyTimeout = source.NudgeReadyTimeoutDuration().String()
			settings.NudgeRetryInterval = source.NudgeRetryIntervalDuration().String()
			settings.NudgeLockTimeout = source.NudgeLockTimeoutDuration().String()
			debounce, display := source.DebounceMsOrDefault(), source.DisplayMsOrDefault()
			settings.DebounceMs, settings.DisplayMs = &debounce, &display
			settings.Socket = source.Socket
			if settings.Socket == "" {
				settings.Socket = cfg.Workspace.Name
			}
			if name == "hybrid" {
				settings.RemoteMatch = source.RemoteMatch
				settings.K8s = source.K8s
			}
		case "acp":
			settings.ACP = source.ACP
		case "k8s":
			settings.K8s = source.K8s
		}
		input.Session = &settings
	}
	encoded, err := json.Marshal(input)
	if err != nil {
		return RuntimeBinding{}, fmt.Errorf("runtime selection: fingerprinting %q: %w", name, err)
	}
	digest := sha256.Sum256(append([]byte("gascity.runtime-declaration.v1\n"), encoded...))
	return RuntimeBinding{Name: name, Fingerprint: hex.EncodeToString(digest[:])}, nil
}

// HasSessionRuntimeSelections reports whether config requests explicit named
// runtimes. Persisted session bindings also require strict routing when these
// selectors are later removed.
func HasSessionRuntimeSelections(cfg *City) bool {
	if cfg == nil {
		return false
	}
	for _, a := range cfg.Agents {
		if a.Runtime != "" {
			return true
		}
	}
	for _, s := range cfg.NamedSessions {
		if s.Runtime != "" {
			return true
		}
	}
	return false
}
