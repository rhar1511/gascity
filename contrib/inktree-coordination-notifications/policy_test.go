package inktreecoordinationnotifications

import (
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/coordinationnotify"
)

func TestReasonMapIsExhaustiveAndFailsClosed(t *testing.T) {
	policy, err := Policy()
	if err != nil {
		t.Fatal(err)
	}
	for key, rule := range policy.Reasons {
		if !strings.HasPrefix(rule.Code, rule.Status+".") {
			t.Errorf("%s maps across status namespace: %#v", key, rule)
		}
		if rule.Status == "hold" || rule.Status == "abstain" {
			if rule.Template == "" || rule.Sentence == "" {
				t.Errorf("%s has no fixed author copy", key)
			}
		}
	}
	want := []string{
		"abstain/unsupported_class",
		"hard_failure/deterministic_control_plane_guard",
		"hard_failure/policy_violation",
		"hold/__insufficient_evidence__",
		"hold/ambiguous_rig",
		"hold/irreversible_risk",
		"hold/jev_selected_hold",
		"hold/missing_evidence",
		"hold/safety_class_security",
		"route/multi_rig_dag",
		"route/single_rig",
	}
	got, err := ReasonCoverage()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("reason map has %d entries, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("reason map entry %d = %q, want %q", i, got[i], want[i])
		}
	}
	input := coordinationnotify.Input{Status: "hold", SourceReason: "not-in-policy", PolicyVersion: policy.Version}
	if _, err := coordinationnotify.Build(policy, input); err == nil {
		t.Fatal("unknown reason was accepted")
	}
}

func TestIndependentDispatchSwitchesDefaultOff(t *testing.T) {
	policy, err := Policy()
	if err != nil {
		t.Fatal(err)
	}
	switches := policy.Switches
	if switches.LiveDispatchValue != "off" || switches.NotificationDispatchValue != "off" {
		t.Fatalf("unsafe defaults: %#v", switches)
	}
	switches.LiveDispatchValue = "maybe"
	policy.Switches = switches
	if _, err := coordinationnotify.Build(policy, coordinationnotify.Input{PolicyVersion: policy.Version}); err == nil {
		t.Fatal("unknown live dispatch value was accepted")
	}
}

func TestSeededReasonMapping(t *testing.T) {
	policy, err := Policy()
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct{ status, source, code string }{
		{"hold", "jev_selected_hold", "hold.ambiguous_rig"},
		{"hold", "__insufficient_evidence__", "hold.missing_evidence"},
		{"hold", "safety_class_security", "hold.irreversible_risk"},
		{"abstain", "unsupported_class", "abstain.unsupported_class"},
		{"hard_failure", "deterministic_control_plane_guard", "hard_failure.policy_violation"},
	}
	for _, tc := range cases {
		rule, ok := policy.Reasons[tc.status+"/"+tc.source]
		if !ok || rule.Code != tc.code {
			t.Errorf("%s/%s mapped to %#v, want %s", tc.status, tc.source, rule, tc.code)
		}
	}
}
