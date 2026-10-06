package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

type controllerTransitionWrapperProbe struct {
	beads.Store
	calls int
}

func (p *controllerTransitionWrapperProbe) TransitionMetadata(string, beads.ControllerMetadataTransitionRequest) (beads.ControllerMetadataTransitionResult, error) {
	p.calls++
	return beads.ControllerMetadataTransitionResult{Applied: true}, nil
}

func TestControllerMetadataTransitionSurvivesProductionWrappers(t *testing.T) {
	probe := &controllerTransitionWrapperProbe{Store: beads.NewMemStore()}
	wrappers := []struct {
		name  string
		store beads.Store
	}{
		{name: "policy", store: wrapStoreWithBeadPolicies(probe, &config.City{})},
		{name: "emitting class", store: &emittingClassStore{Store: probe, cityPath: t.TempDir()}},
	}

	for _, wrapper := range wrappers {
		t.Run(wrapper.name, func(t *testing.T) {
			writer, ok := beads.ControllerMetadataTransitionWriterFor(wrapper.store)
			if !ok || writer == nil {
				t.Fatal("wrapper hid the controller metadata transition capability")
			}
			before := probe.calls
			if _, err := writer.TransitionMetadata("work-1", beads.ControllerMetadataTransitionRequest{}); err != nil {
				t.Fatalf("TransitionMetadata: %v", err)
			}
			if probe.calls != before+1 {
				t.Fatalf("transition calls = %d, want %d", probe.calls, before+1)
			}
		})
	}
}

var _ beads.ControllerMetadataTransitionWriter = (*controllerTransitionWrapperProbe)(nil)
