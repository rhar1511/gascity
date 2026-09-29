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
		BindingSource: "attested_forge_author", BindingID: "binding-0123456789abcdef0123456789abcdef",
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
	for _, secret := range []string{"private request text", "private rationale", "member_id", "member intent", "private evidence content"} {
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
	first := ledger.Record(item.Envelope, Attempt{EventID: "evt-1", Channel: "discord"})
	second := ledger.Record(item.Envelope, Attempt{EventID: "evt-1", Channel: "discord"})
	if first.Suppressed || !second.Suppressed {
		t.Fatalf("event replay suppression mismatch: %#v %#v", first, second)
	}
	if first.Attempt != 1 || second.Attempt != 1 || second.SuppressionCause != "duplicate_event" {
		t.Fatalf("same-channel attempt numbering is unstable: %#v %#v", first, second)
	}
	if receipt := ledger.Record(item.Envelope, Attempt{EventID: "evt-failover-1", Channel: "pr_update"}); receipt.Suppressed || receipt.Attempt != 1 {
		t.Fatal("one channel suppressed an independent channel attempt")
	}
	if receipt := ledger.Record(item.Envelope, Attempt{EventID: "evt-failover-2", Channel: "pr_update"}); receipt.SuppressionCause != "max_attempts" {
		t.Fatal("failover channel was not bounded to one attempt")
	}
	if receipt := ledger.Record(item.Envelope, Attempt{EventID: "evt-unknown", Channel: "unknown"}); !receipt.Suppressed {
		t.Fatal("unknown receipt channel was accepted")
	}
	input.HoldEpoch++
	newEpoch, err := Build(policy, input)
	if err != nil {
		t.Fatal(err)
	}
	if receipt := ledger.Record(newEpoch.Envelope, Attempt{EventID: "evt-new-epoch", Channel: "discord"}); receipt.Suppressed {
		t.Fatal("new hold epoch was suppressed")
	}
	restored := NewLedgerFromSnapshot(ledger.Snapshot())
	if receipt := restored.Record(item.Envelope, Attempt{EventID: "evt-1", Channel: "discord"}); !receipt.Suppressed {
		t.Fatal("restored ledger forgot the prior epoch receipt")
	}
}

func TestLedgerEnforcesRetryIdentityBackoffAttemptsAndDeadline(t *testing.T) {
	item, err := Build(testPolicy(), validInput())
	if err != nil {
		t.Fatal(err)
	}
	ledger := NewLedger()
	if got := ledger.Record(item.Envelope, Attempt{EventID: "evt-1", Channel: "discord"}); got.Suppressed {
		t.Fatalf("first attempt suppressed: %#v", got)
	}
	if got := ledger.Record(item.Envelope, Attempt{EventID: "evt-2", Channel: "discord", ElapsedSeconds: 29}); got.SuppressionCause != "backoff" {
		t.Fatalf("early retry not suppressed by backoff: %#v", got)
	}
	if got := ledger.Record(item.Envelope, Attempt{EventID: "evt-2", Channel: "discord", ElapsedSeconds: 30}); got.Suppressed || got.Attempt != 2 {
		t.Fatalf("eligible retry suppressed: %#v", got)
	}
	if got := ledger.Record(item.Envelope, Attempt{EventID: "evt-3", Channel: "discord", ElapsedSeconds: 150}); got.Suppressed || got.Attempt != 3 {
		t.Fatalf("third attempt suppressed: %#v", got)
	}
	if got := ledger.Record(item.Envelope, Attempt{EventID: "evt-4", Channel: "discord", ElapsedSeconds: 750}); got.SuppressionCause != "max_attempts" {
		t.Fatalf("attempt bound not enforced: %#v", got)
	}
	newEpoch := *item.Envelope
	newEpoch.HoldEpoch++
	if got := ledger.Record(&newEpoch, Attempt{EventID: "evt-deadline", Channel: "discord", ElapsedSeconds: 3601}); got.SuppressionCause != "deadline" {
		t.Fatalf("deadline not enforced: %#v", got)
	}
}

func TestSuppressedOnlySnapshotDoesNotRestoreAcceptedState(t *testing.T) {
	item, err := Build(testPolicy(), validInput())
	if err != nil {
		t.Fatal(err)
	}
	snapshot := LedgerSnapshot{Receipts: []Receipt{{
		DedupKey: item.Envelope.DedupKey, HoldEpoch: item.Envelope.HoldEpoch,
		EventID: "evt-1", Channel: "discord", Attempt: 1, Suppressed: true,
	}}}
	ledger := NewLedgerFromSnapshot(snapshot)
	if got := ledger.Snapshot(); len(got.Receipts) != 1 || !got.Receipts[0].Suppressed {
		t.Fatalf("suppressed audit receipt was not retained: %#v", got)
	}
	if got := ledger.Record(item.Envelope, Attempt{EventID: "evt-1", Channel: "discord"}); got.Suppressed {
		t.Fatalf("suppressed-only snapshot blocked a valid attempt: %#v", got)
	}
}

func TestInvalidAttemptRemainsInAuditSnapshot(t *testing.T) {
	item, err := Build(testPolicy(), validInput())
	if err != nil {
		t.Fatal(err)
	}
	ledger := NewLedger()
	if got := ledger.Record(item.Envelope, Attempt{EventID: "member text", Channel: "unknown", ElapsedSeconds: -1}); got.SuppressionCause != "invalid_attempt" {
		t.Fatalf("invalid attempt was not suppressed: %#v", got)
	}
	snapshot := ledger.Snapshot()
	if len(snapshot.Receipts) != 1 || !snapshot.Receipts[0].Suppressed {
		t.Fatalf("invalid attempt was omitted from audit snapshot: %#v", snapshot)
	}
	if snapshot.Receipts[0].EventID != "invalid-event" || snapshot.Receipts[0].Channel != "invalid-channel" {
		t.Fatalf("invalid attempt retained caller-controlled content: %#v", snapshot)
	}
	restored := NewLedgerFromSnapshot(snapshot).Snapshot()
	if len(restored.Receipts) != 1 || restored.Receipts[0].SuppressionCause != "invalid_attempt" {
		t.Fatalf("invalid attempt audit was lost on restore: %#v", restored)
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
	if !item.Decision.RenderAllowed || !item.Decision.Deliver || item.Decision.LiveDispatch {
		t.Fatalf("route dispatch switch incorrectly blocked notification delivery: %#v", item.Decision)
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
		policy := testPolicy()
		policy.Switches = Switches{LiveDispatchValue: "on", NotificationDispatchValue: "on"}
		input := validInput()
		input.Status = status
		input.SourceReason = "terminal"
		input.Mode = ModeReminder
		item, err := Build(policy, input)
		if err != nil {
			t.Fatal(err)
		}
		if item.Envelope != nil {
			t.Fatalf("%s emitted an author envelope", status)
		}
		if item.Decision.RenderAllowed || item.Decision.Deliver {
			t.Fatalf("%s authorized terminal render/delivery: %#v", status, item.Decision)
		}
	}
}

func TestBindingIDRejectsMemberContentShapes(t *testing.T) {
	for _, bindingID := range []string{
		"member name", "member-private-medical-note", "member@example.test", "member/id", "member\ntext", "MIXEDCASE", "mémber", strings.Repeat("a", 129),
	} {
		input := validInput()
		input.BindingID = bindingID
		if _, err := Build(testPolicy(), input); err == nil {
			t.Errorf("unsafe binding ID %q was accepted", bindingID)
		}
	}
}

func TestPrimaryAndFailoverChannelsMustDiffer(t *testing.T) {
	policy := testPolicy()
	policy.Delivery.FailoverChannel = "discord"
	if _, err := Build(policy, validInput()); err == nil {
		t.Fatal("primary channel was accepted as its own failover")
	}
}

func validInput() Input {
	return Input{
		Status: "hold", SourceReason: "ambiguous", BeadID: "work-17", HoldEpoch: 1,
		Mode: ModeReplay, Channel: "discord", RecipientRole: "author", BindingSource: "attested_forge_author",
		BindingID: "binding-0123456789abcdef0123456789abcdef", PolicyVersion: "policy-v1", EnvelopeHash: strings.Repeat("a", 64),
		RouterConfigHash: strings.Repeat("b", 64), ModelVersions: ModelVersions{JEV: "jev-1", RLCD: "rlcd-1", SemIF: "sem-if-1"},
	}
}

func testPolicy() Policy {
	return Policy{
		Version: "policy-v1", NotificationVersion: "coordination-notification/v1", BeadPrefix: "work-", ReplayCommand: "go run ./contrib/inktree-coordination-notifications/cmd/evidence --corpus tests/corpus.json --policy tests/policy.json --out replay-report.json", Reasons: map[string]ReasonRule{
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
