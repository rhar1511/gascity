package runtimetest

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

func TestConstructorFactoryConformance(t *testing.T) {
	RunProviderTests(t, func(_ *testing.T) (runtime.Provider, error) {
		return runtime.NewFake(), nil
	})
}

func TestConstructorFactoryNamesAndLegacyCompatibility(t *testing.T) {
	p := runtime.NewFake()
	factory := normalizeProviderFactory(ConstructorFactory(func(_ *testing.T) (runtime.Provider, error) {
		return p, nil
	}))
	seen := map[string]bool{}
	for i := 0; i < 20; i++ {
		got, cfg, name := factory(t)
		if got != p || cfg.Command != "" || cfg.WorkDir != "" || !strings.HasPrefix(name, "runtime-conformance-") || seen[name] {
			t.Fatalf("constructor factory lost provider/default config or reused name %q", name)
		}
		seen[name] = true
	}
	legacy := Factory(func(_ *testing.T) (runtime.Provider, runtime.Config, string) {
		return p, runtime.Config{Command: "legacy-command", WorkDir: "legacy-dir"}, "legacy-name"
	})
	got, cfg, name := normalizeProviderFactory(legacy)(t)
	if got != p || cfg.Command != "legacy-command" || cfg.WorkDir != "legacy-dir" || name != "legacy-name" {
		t.Fatal("legacy factory result changed")
	}
}

type constructorFailure struct{}

type constructorFailureRecorder struct{ message string }

func (*constructorFailureRecorder) Helper() {}
func (r *constructorFailureRecorder) Fatalf(format string, args ...any) {
	r.message = fmt.Sprintf(format, args...)
	panic(constructorFailure{})
}

func TestConstructedProviderRejectsErrorsAndNil(t *testing.T) {
	var typedNil *runtime.Fake
	for _, tc := range []struct {
		name     string
		provider runtime.Provider
		err      error
		want     string
	}{
		{"error", runtime.NewFake(), errors.New("construction failed"), "construction failed"},
		{"nil", nil, nil, "nil provider"},
		{"typed nil", typedNil, nil, "nil provider"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := &constructorFailureRecorder{}
			defer func() {
				if _, ok := recover().(constructorFailure); !ok || !strings.Contains(recorder.message, tc.want) {
					t.Fatalf("constructor failure not reported: %q", recorder.message)
				}
			}()
			requireConstructedProvider(recorder, tc.provider, tc.err)
			t.Fatal("invalid construction returned instead of failing")
		})
	}
}
