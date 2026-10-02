package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

func TestOrderIdentityRegistrySurvivesDispatcherReplacement(t *testing.T) {
	registry := newOrderDispatchIdentityRegistryWithGeneration("execution-replacement")
	previous := &memoryOrderDispatcher{inflightIdentityRegistry: registry}
	next := &memoryOrderDispatcher{inflightIdentityRegistry: newOrderDispatchIdentityRegistryWithGeneration("other-execution")}
	runtime := &CityRuntime{
		od:                            previous,
		orderDispatchIdentityRegistry: registry,
	}

	runtime.replaceOrderDispatcher(next)
	if runtime.od != next {
		t.Fatal("replacement dispatcher was not installed")
	}
	if next.inflightIdentityRegistry != registry {
		t.Fatal("replacement dispatcher did not retain the controller execution registry")
	}
}

func TestWebhookDispatcherUsesControllerIdentityRegistry(t *testing.T) {
	registry := newOrderDispatchIdentityRegistryWithGeneration("execution-webhook")
	state := &controllerState{
		cfg:                           &config.City{Workspace: config.Workspace{Name: "test-city"}},
		cityPath:                      t.TempDir(),
		orderDispatchIdentityRegistry: registry,
	}
	dispatcher := (controllerWebhookDispatcher{cs: state}).dispatcher()
	if dispatcher.inflightIdentityRegistry != registry {
		t.Fatal("webhook dispatcher did not share the controller execution registry")
	}
	dispatcher.cancel()
}
