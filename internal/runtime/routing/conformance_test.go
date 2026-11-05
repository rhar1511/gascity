package routing

import (
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/runtimetest"
)

func TestRoutingConformance(t *testing.T) {
	for _, key := range []string{"local", "remote"} {
		t.Run(key, func(t *testing.T) {
			p, err := New("local", map[string]runtime.Provider{"local": runtime.NewFake(), "remote": runtime.NewFake()})
			if err != nil {
				t.Fatal(err)
			}
			// The contract checks idempotent cleanup for a known, never-started
			// identity. Its owner is explicit just like each factory identity.
			for _, name := range []string{"never-started-conformance-session", "nonexistent-conformance-session"} {
				if err := p.Bind(name, key); err != nil {
					t.Fatal(err)
				}
			}
			var counter atomic.Uint64
			runtimetest.RunProviderTests(t, func(t *testing.T) (runtime.Provider, runtime.Config, string) {
				name := fmt.Sprintf("routed-conformance-%d", counter.Add(1))
				if err := p.Bind(name, key); err != nil {
					t.Fatal(err)
				}
				return p, runtime.Config{}, name
			})
		})
	}
}
