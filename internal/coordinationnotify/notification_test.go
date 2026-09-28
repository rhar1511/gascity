package coordinationnotify

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestBuildProjectsRedactedNotificationDeterministically(t *testing.T) {
	policy := testPolicy()
	input := Input{
		Status: "hold", SourceReason: "ambiguous", BeadID: "work-17", HoldEpoch: 2,
		Mode: ModeReplay, Channel: "discord", RecipientRole: "author",
		BindingSource: "attested_forge_author", BindingID: "forge-author",
		PolicyVersion: "policy-v1", EnvelopeHash: strings.Repeat("a", 64),
		RouterConfigHash: strings.Repeat("b", 64), ModelVersions: ModelVersions{JEV: "jev-1", RLCD: "rlcd-1", SemIF: "sem-if-1"},
	}
	first, err := Build(policy, input)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Build(policy, input)
	if err != nil {
		t.Fatal(err)
	}
	if !jsonEqual(first, second) {
		t.Fatal("same replay input produced different projections")
	}
	if first.Envelope.NotificationID != second.Envelope.NotificationID {
		t.Fatal("notification ID changed across replay")
	}
	if first.Assertion.Count != 1 || first.Record.DedupKey != first.Envelope.DedupKey {
		t.Fatalf("projections do not describe one envelope: %#v", first)
	}
	encoded, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"private request text", "private rationale", "member_id", "intent", "evidence"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("projection contains forbidden content %q", secret)
		}
	}
	rendered, err := Render(policy, first.Envelope)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"private request text", "private rationale"} {
		if strings.Contains(rendered, secret) {
			t.Fatalf("render leaked %q", secret)
		}
	}
}

func TestBuildFailsClosedForUnknownReasonsAndSwitchValues(t *testing.T) {
	input := Input{Status: "hold", SourceReason: "new_reason", BeadID: "work-17", HoldEpoch: 1, Mode: ModeReplay}
	if _, err := Build(testPolicy(), input); err == nil {
		t.Fatal("unknown source reason was accepted")
	}
	input.SourceReason = "ambiguous"
	input.Mode = "surprise"
	if _, err := Build(testPolicy(), input); err == nil {
		t.Fatal("unknown dispatch mode was accepted")
	}
}

func TestDisabledDispatchRecordsButNeverDelivers(t *testing.T) {
	policy := testPolicy()
	policy.Switches = Switches{LiveDispatchValue: "off", NotificationDispatchValue: "off"}
	input := validInput()
	input.Mode = ModeReminder
	item, err := Build(policy, input)
	if err != nil {
		t.Fatal(err)
	}
	if item.Decision.RenderAllowed || item.Decision.Deliver {
		t.Fatal("disabled notification dispatch authorized rendering or delivery")
	}
	ledger := NewLedger()
	first := ledger.Record(item.Envelope, "discord")
	second := ledger.Record(item.Envelope, "discord")
	if first.Suppressed || !second.Suppressed {
		t.Fatalf("same-epoch refire suppression mismatch: %#v %#v", first, second)
	}
	if first.Attempt != 1 || second.Attempt != 2 {
		t.Fatalf("same-channel attempt numbering is unstable: %#v %#v", first, second)
	}
	if receipt := ledger.Record(item.Envelope, "pr_update"); receipt.Suppressed || receipt.Attempt != 1 {
		t.Fatal("one channel suppressed an independent channel attempt")
	}
	if receipt := ledger.Record(item.Envelope, "unknown"); !receipt.Suppressed {
		t.Fatal("unknown receipt channel was accepted")
	}
	input.HoldEpoch++
	newEpoch, err := Build(policy, input)
	if err != nil {
		t.Fatal(err)
	}
	if receipt := ledger.Record(newEpoch.Envelope, "discord"); receipt.Suppressed {
		t.Fatal("new hold epoch was suppressed")
	}
	restored := NewLedgerFromSnapshot(ledger.Snapshot())
	if receipt := restored.Record(item.Envelope, "discord"); !receipt.Suppressed {
		t.Fatal("restored ledger forgot the prior epoch receipt")
	}
}

func TestUnknownSwitchValueFailsClosed(t *testing.T) {
	policy := testPolicy()
	policy.Switches.NotificationDispatchValue = "maybe"
	if _, err := Build(policy, validInput()); err == nil {
		t.Fatal("unknown switch value was accepted")
	}
}

func TestDispatchSwitchesAreIndependent(t *testing.T) {
	policy := testPolicy()
	input := validInput()
	input.Mode = ModeReminder
	policy.Switches = Switches{LiveDispatchValue: "off", NotificationDispatchValue: "on"}
	item, err := Build(policy, input)
	if err != nil {
		t.Fatal(err)
	}
	if !item.Decision.RenderAllowed || item.Decision.Deliver {
		t.Fatalf("live switch did not independently block delivery: %#v", item.Decision)
	}
	policy.Switches = Switches{LiveDispatchValue: "on", NotificationDispatchValue: "off"}
	item, err = Build(policy, input)
	if err != nil {
		t.Fatal(err)
	}
	if item.Decision.RenderAllowed || item.Decision.Deliver {
		t.Fatalf("notification switch did not independently block render/delivery: %#v", item.Decision)
	}
}

func TestRouteAndHardFailureDoNotEmitAuthorNotification(t *testing.T) {
	for _, status := range []string{"route", "hard_failure"} {
		input := validInput()
		input.Status = status
		input.SourceReason = "terminal"
		item, err := Build(testPolicy(), input)
		if err != nil {
			t.Fatal(err)
		}
		if item.Envelope != nil {
			t.Fatalf("%s emitted an author envelope", status)
		}
	}
}

func validInput() Input {
	return Input{
		Status: "hold", SourceReason: "ambiguous", BeadID: "work-17", HoldEpoch: 1,
		Mode: ModeReplay, Channel: "discord", RecipientRole: "author", BindingSource: "attested_forge_author",
		BindingID: "forge-author", PolicyVersion: "policy-v1", EnvelopeHash: strings.Repeat("a", 64),
		RouterConfigHash: strings.Repeat("b", 64), ModelVersions: ModelVersions{JEV: "jev-1", RLCD: "rlcd-1", SemIF: "sem-if-1"},
	}
}

func testPolicy() Policy {
	return Policy{
		Version: "policy-v1", NotificationVersion: "coordination-notification/v1", BeadPrefix: "work-", ReplayCommand: "replay-coordination --corpus tests/corpus.json --policy policy-v1 --out replay-report.json", Reasons: map[string]ReasonRule{
			"hold/ambiguous":        {Status: "hold", Code: "hold.ambiguous_rig", Severity: "action_required", Template: "hold.action_required.v1", Sentence: "A human decision is needed before this work can proceed."},
			"route/terminal":        {Status: "route", Code: "route.single_rig", Severity: "info"},
			"hard_failure/terminal": {Status: "hard_failure", Code: "hard_failure.policy_violation", Severity: "info"},
		}, Switches: Switches{LiveDispatchValue: "off", NotificationDispatchValue: "on"},
		Delivery: DeliveryPolicy{MaxAttempts: 3, BackoffSeconds: []int{30, 120, 600}, FailoverChannel: "pr_update", EscalationRecipient: "operator-relay", DeadlineSeconds: 3600},
	}
}

func jsonEqual(a, b any) bool {
	left, _ := json.Marshal(a)
	right, _ := json.Marshal(b)
	return string(left) == string(right)
}
