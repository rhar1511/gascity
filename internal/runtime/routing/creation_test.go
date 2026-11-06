package routing

import (
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

func TestRoutingReplacementRetiresOnlyCreation(t *testing.T) {
	backend := runtime.NewFake()
	p, err := New("local", map[string]runtime.Provider{"local": backend})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Bind("retained", "local"); err != nil {
		t.Fatal(err)
	}
	called := 0
	create := func() error { called++; return nil }
	if err := p.WithBackendCreation(create); err != nil {
		t.Fatal(err)
	}
	if err := p.WithRoutingReplacement(func() (bool, error) { return false, nil }); err != nil {
		t.Fatal(err)
	}
	if err := p.WithBackendCreation(create); err != nil {
		t.Fatal("rejected publication retired creation")
	}
	if err := p.WithRoutingReplacement(func() (bool, error) { return true, nil }); err != nil {
		t.Fatal(err)
	}
	if err := p.WithBackendCreation(create); err == nil || called != 2 {
		t.Fatal("captured old factory created after replacement")
	}
	if err := p.Stop("retained"); err != nil {
		t.Fatal("retirement changed existing-owner operation:", err)
	}
	if backend.CountCalls("Stop", "retained") != 1 {
		t.Fatal("retained owner lost backend")
	}
}
