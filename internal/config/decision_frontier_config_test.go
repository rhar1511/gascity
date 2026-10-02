package config

import "testing"

func TestDecisionFrontierPromptTargetDefaultsOffAndParses(t *testing.T) {
	defaults, err := Parse([]byte("[workspace]\nname = \"test-city\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if defaults.DecisionFrontier.PromptTarget != "" {
		t.Fatalf("default decision-frontier prompt target = %q, want disabled", defaults.DecisionFrontier.PromptTarget)
	}

	configured, err := Parse([]byte("[workspace]\nname = \"test-city\"\n[decision_frontier]\nprompt_target = \"mayor\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if configured.DecisionFrontier.PromptTarget != "mayor" {
		t.Fatalf("configured prompt target = %q, want mayor", configured.DecisionFrontier.PromptTarget)
	}
}
