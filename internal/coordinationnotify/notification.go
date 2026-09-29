// Package coordinationnotify contains role-neutral coordination notification
// projections. Consumers provide their own reason table and fixed copy.
package coordinationnotify

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Mode controls whether a notification is previewed, observed, or deliverable.
type Mode string

const (
	// ModeReminder allows reminder-mode render/delivery when its switches permit.
	ModeReminder Mode = "reminder"
	// ModeReplay records offline evidence without author-facing delivery.
	ModeReplay Mode = "replay"
	// ModeShadow records shadow observations without author-facing delivery.
	ModeShadow Mode = "shadow"
)

const redaction = "no_member_content"

// ReasonRule is consumer-supplied policy for one source reason.
type ReasonRule struct {
	Status   string `json:"status"`
	Code     string `json:"code"`
	Severity string `json:"severity"`
	Template string `json:"template,omitempty"`
	Sentence string `json:"sentence,omitempty"`
}

// Switches keeps notification rendering and live transport independently gated.
// Values are strings so unknown configuration fails closed instead of being
// silently coerced to a zero-value boolean.
type Switches struct {
	LiveDispatchValue         string `json:"live_dispatch"`
	NotificationDispatchValue string `json:"notification_dispatch"`
}

// Policy carries consumer-owned reason mappings and fixed templates.
type Policy struct {
	Version             string                `json:"version"`
	NotificationVersion string                `json:"notification_version"`
	BeadPrefix          string                `json:"bead_prefix"`
	ReplayCommand       string                `json:"replay_command"`
	Reasons             map[string]ReasonRule `json:"reasons"`
	Switches            Switches              `json:"switches"`
	Delivery            DeliveryPolicy        `json:"delivery"`
}

// DeliveryPolicy bounds attempts in consumer-owned policy data.
type DeliveryPolicy struct {
	MaxAttempts         int    `json:"max_attempts"`
	BackoffSeconds      []int  `json:"backoff_seconds"`
	FailoverChannel     string `json:"failover_channel"`
	EscalationRecipient string `json:"escalation_recipient"`
	DeadlineSeconds     int    `json:"deadline_seconds"`
}

// Input is the allowlisted decision data accepted by Build. Its closed field
// set has no slot for member content or model rationale.
type Input struct {
	Status           string
	SourceReason     string
	BeadID           string
	HoldEpoch        uint64
	Mode             Mode
	Channel          string
	RecipientRole    string
	BindingSource    string
	BindingID        string
	PolicyVersion    string
	EnvelopeHash     string
	RouterConfigHash string
	ModelVersions    ModelVersions
}

// Envelope is the fixed, member-content-free notification record.
type Envelope struct {
	NotificationVersion string      `json:"notification_version"`
	NotificationID      string      `json:"notification_id"`
	Status              string      `json:"status"`
	ReasonCode          string      `json:"reason_code"`
	Severity            string      `json:"severity"`
	Redaction           string      `json:"redaction"`
	Mode                Mode        `json:"mode"`
	Channel             string      `json:"channel"`
	RecipientRole       string      `json:"recipient_role"`
	DedupKey            string      `json:"dedup_key"`
	HoldEpoch           uint64      `json:"hold_epoch"`
	Template            string      `json:"template"`
	BeadRef             string      `json:"bead_ref"`
	Links               Links       `json:"links"`
	DecisionRef         DecisionRef `json:"decision_ref"`
	MustInclude         []string    `json:"must_include"`
	Identity            Identity    `json:"identity"`
	Delivery            Delivery    `json:"delivery"`
}

// Links contains the durable bead lookup command and optional external link.
type Links struct {
	Bead        string `json:"bead,omitempty"`
	BeadCommand string `json:"bead_command"`
}

// DecisionRef lets a reviewer reproduce the routing decision offline.
type DecisionRef struct {
	EnvelopeContentHash string        `json:"envelope_content_hash"`
	PolicyVersion       string        `json:"policy_version"`
	ModelVersions       ModelVersions `json:"model_versions"`
	RouterConfigHash    string        `json:"router_config_hash"`
	ReplayCommand       string        `json:"replay_command"`
}

// ModelVersions identifies the three decision-model builds without carrying
// their rationale or output text.
type ModelVersions struct {
	JEV   string `json:"jev"`
	RLCD  string `json:"rlcd"`
	SemIF string `json:"sem_if"`
}

// Identity records the trust source of the configured recipient binding.
type Identity struct {
	Resolution    string `json:"resolution"`
	BindingSource string `json:"binding_source"`
	BindingID     string `json:"binding_id"`
}

// Delivery contains bounded retry and escalation settings copied from policy.
type Delivery struct {
	MaxAttempts         int    `json:"max_attempts"`
	BackoffSeconds      []int  `json:"backoff_seconds"`
	FailoverChannel     string `json:"failover_channel"`
	EscalationRecipient string `json:"escalation_recipient"`
	DeadlineSeconds     int    `json:"deadline_seconds"`
}

// Assertion is the replay-corpus projection.
type Assertion struct {
	Channel       string   `json:"channel"`
	RecipientRole string   `json:"recipient_role"`
	Severity      string   `json:"severity"`
	Redaction     string   `json:"redaction"`
	DedupKey      string   `json:"dedup_key"`
	MustInclude   []string `json:"must_include"`
	Count         int      `json:"count"`
}

// Record is the notification-ledger projection.
type Record struct {
	Channel       string `json:"channel"`
	RecipientRole string `json:"recipient_role"`
	Severity      string `json:"severity"`
	Redaction     string `json:"redaction"`
	DedupKey      string `json:"dedup_key"`
}

// Decision captures independent kill-switch outcomes. Delivery is never
// authorized by replay or shadow mode.
type Decision struct {
	LiveDispatch         bool `json:"live_dispatch"`
	NotificationDispatch bool `json:"notification_dispatch"`
	RenderAllowed        bool `json:"render_allowed"`
	Deliver              bool `json:"deliver"`
}

// Projection contains the one envelope and its two contract projections.
type Projection struct {
	Envelope  *Envelope  `json:"envelope,omitempty"`
	Assertion *Assertion `json:"assertion,omitempty"`
	Record    *Record    `json:"record,omitempty"`
	Decision  Decision   `json:"decision"`
}

var (
	beadIDPattern        = regexp.MustCompile(`^[a-z][a-z0-9]*(?:-[a-z0-9]+)*(?:\.[a-z0-9]+)*$`)
	bindingIDPattern     = regexp.MustCompile(`^binding-[0-9a-f]{32}$`)
	hashPattern          = regexp.MustCompile(`^[0-9a-f]{64}$`)
	versionPattern       = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)
	replayCommandPattern = regexp.MustCompile(`^go run \./[A-Za-z0-9._/-]+ --corpus [A-Za-z0-9._/-]+ --policy [A-Za-z0-9._/-]+ --out [A-Za-z0-9._/-]+$`)
)

// Build maps a consumer source reason, validates closed vocabularies, and
// returns deterministic envelope, assertion, and ledger projections. Route
// and hard_failure decisions return no author-facing envelope.
func Build(policy Policy, input Input) (Projection, error) {
	if err := validateSwitches(policy.Switches); err != nil {
		return Projection{}, err
	}
	if err := validateDelivery(policy.Delivery); err != nil {
		return Projection{}, err
	}
	if policy.Version == "" || input.PolicyVersion == "" || input.PolicyVersion != policy.Version {
		return Projection{}, errors.New("notification policy version is missing or mismatched")
	}
	if !validModelVersion(policy.NotificationVersion) || !validBeadPrefix(policy.BeadPrefix) {
		return Projection{}, errors.New("notification version or bead prefix is invalid")
	}
	if !replayCommandPattern.MatchString(policy.ReplayCommand) || strings.Contains(policy.ReplayCommand, "..") {
		return Projection{}, errors.New("policy replay command is missing or malformed")
	}
	if input.Mode != ModeReminder && input.Mode != ModeReplay && input.Mode != ModeShadow {
		return Projection{}, fmt.Errorf("unknown notification mode %q", input.Mode)
	}
	rule, ok := policy.Reasons[input.Status+"/"+input.SourceReason]
	if !ok {
		return Projection{}, fmt.Errorf("unmapped coordination reason %q/%q", input.Status, input.SourceReason)
	}
	if rule.Status != input.Status || !validReasonCode(rule.Code, input.Status) {
		return Projection{}, fmt.Errorf("invalid reason mapping for %q/%q", input.Status, input.SourceReason)
	}
	if input.Status == "route" || input.Status == "hard_failure" {
		outcome := decision(policy.Switches, input.Mode)
		outcome.RenderAllowed = false
		outcome.Deliver = false
		return Projection{Decision: outcome}, nil
	}
	if input.Status != "hold" && input.Status != "abstain" {
		return Projection{}, fmt.Errorf("unknown coordination status %q", input.Status)
	}
	if !beadIDPattern.MatchString(input.BeadID) || !strings.HasPrefix(input.BeadID, policy.BeadPrefix) || input.HoldEpoch == 0 {
		return Projection{}, errors.New("invalid bead ID or hold epoch")
	}
	if input.Channel != "discord" && input.Channel != "pr_update" {
		return Projection{}, fmt.Errorf("unknown notification channel %q", input.Channel)
	}
	if input.Channel == policy.Delivery.FailoverChannel {
		return Projection{}, errors.New("notification primary and failover channels must differ")
	}
	if input.RecipientRole != "author" && input.RecipientRole != "maintainer" && input.RecipientRole != "operator" {
		return Projection{}, fmt.Errorf("unknown recipient role %q", input.RecipientRole)
	}
	if input.BindingSource != "attested_forge_author" && input.BindingSource != "configured" && input.BindingSource != "none" {
		return Projection{}, fmt.Errorf("unknown binding source %q", input.BindingSource)
	}
	if input.BindingSource == "none" && (input.RecipientRole != "operator" || input.Channel != "pr_update") {
		return Projection{}, errors.New("unbound notification must use operator role on pr_update")
	}
	if input.BindingSource == "attested_forge_author" && input.RecipientRole != "author" {
		return Projection{}, errors.New("attested binding must resolve to author")
	}
	if input.BindingSource == "configured" && input.RecipientRole != "maintainer" {
		return Projection{}, errors.New("configured binding must resolve to maintainer")
	}
	if !bindingIDPattern.MatchString(input.BindingID) {
		return Projection{}, errors.New("binding ID must be an opaque binding record key")
	}
	if !hashPattern.MatchString(input.EnvelopeHash) || !hashPattern.MatchString(input.RouterConfigHash) {
		return Projection{}, errors.New("decision hashes must be lowercase sha256 values")
	}
	if !validModelVersion(input.ModelVersions.JEV) || !validModelVersion(input.ModelVersions.RLCD) || !validModelVersion(input.ModelVersions.SemIF) {
		return Projection{}, errors.New("decision model versions are missing or malformed")
	}
	if rule.Severity != "action_required" && (input.Status != "abstain" || rule.Severity != "warning") {
		return Projection{}, fmt.Errorf("severity %q is not author-facing for %q", rule.Severity, input.Status)
	}
	if rule.Template == "" || rule.Sentence == "" {
		return Projection{}, errors.New("fixed notification template is missing")
	}
	dedupKey := "coord-" + input.Status + "-" + input.BeadID
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s:%d", dedupKey, input.HoldEpoch)))
	resolution := map[string]string{"attested_forge_author": "resolved", "configured": "unverified_author_downgraded", "none": "unresolved_operator_relay"}[input.BindingSource]
	envelope := &Envelope{
		NotificationVersion: policy.NotificationVersion,
		NotificationID:      "notif-" + hex.EncodeToString(sum[:8]),
		Status:              input.Status, ReasonCode: rule.Code, Severity: rule.Severity,
		Redaction: redaction, Mode: input.Mode, Channel: input.Channel,
		RecipientRole: input.RecipientRole, DedupKey: dedupKey, HoldEpoch: input.HoldEpoch,
		Template: rule.Template, BeadRef: input.BeadID,
		Links: Links{BeadCommand: "gc bd show " + input.BeadID},
		DecisionRef: DecisionRef{
			EnvelopeContentHash: input.EnvelopeHash, PolicyVersion: policy.Version,
			ModelVersions:    input.ModelVersions,
			RouterConfigHash: input.RouterConfigHash, ReplayCommand: policy.ReplayCommand,
		},
		MustInclude: []string{"bead_link", "decision_link", "reason_code"},
		Identity:    Identity{Resolution: resolution, BindingSource: input.BindingSource, BindingID: input.BindingID},
		Delivery: Delivery{
			MaxAttempts:         policy.Delivery.MaxAttempts,
			BackoffSeconds:      append([]int(nil), policy.Delivery.BackoffSeconds...),
			FailoverChannel:     policy.Delivery.FailoverChannel,
			EscalationRecipient: policy.Delivery.EscalationRecipient,
			DeadlineSeconds:     policy.Delivery.DeadlineSeconds,
		},
	}
	assertion := &Assertion{
		Channel: input.Channel, RecipientRole: input.RecipientRole, Severity: rule.Severity,
		Redaction: redaction, DedupKey: dedupKey, MustInclude: append([]string(nil), envelope.MustInclude...), Count: 1,
	}
	record := &Record{
		Channel: input.Channel, RecipientRole: input.RecipientRole, Severity: rule.Severity,
		Redaction: redaction, DedupKey: dedupKey,
	}
	return Projection{
		Envelope: envelope, Assertion: assertion, Record: record,
		Decision: decision(policy.Switches, input.Mode),
	}, nil
}

// Render produces a fixed sentence and allowlisted anchors. It never reads
// request text or model rationale.
func Render(policy Policy, envelope *Envelope) (string, error) {
	if envelope == nil {
		return "", errors.New("notification envelope is required")
	}
	if envelope.NotificationVersion != policy.NotificationVersion || envelope.Redaction != redaction ||
		!beadIDPattern.MatchString(envelope.BeadRef) || !strings.HasPrefix(envelope.BeadRef, policy.BeadPrefix) ||
		envelope.Links.BeadCommand != "gc bd show "+envelope.BeadRef ||
		!hashPattern.MatchString(envelope.DecisionRef.EnvelopeContentHash) ||
		!hashPattern.MatchString(envelope.DecisionRef.RouterConfigHash) ||
		!validModelVersion(envelope.DecisionRef.ModelVersions.JEV) ||
		!validModelVersion(envelope.DecisionRef.ModelVersions.RLCD) ||
		!validModelVersion(envelope.DecisionRef.ModelVersions.SemIF) ||
		!replayCommandPattern.MatchString(envelope.DecisionRef.ReplayCommand) ||
		strings.Contains(envelope.DecisionRef.ReplayCommand, "..") {
		return "", errors.New("notification contains an unsafe render field")
	}
	var rule ReasonRule
	found := false
	for _, candidate := range policy.Reasons {
		if candidate.Status == envelope.Status && candidate.Code == envelope.ReasonCode && candidate.Template == envelope.Template {
			rule, found = candidate, true
			break
		}
	}
	if !found || rule.Sentence == "" {
		return "", errors.New("notification template is not in policy")
	}
	return strings.Join([]string{
		rule.Sentence, "Reason: " + envelope.ReasonCode,
		"Bead: " + envelope.Links.BeadCommand, "Decision: " + envelope.DecisionRef.ReplayCommand,
	}, "\n"), nil
}

func validReasonCode(code, status string) bool {
	return strings.HasPrefix(code, status+".") && len(code) > len(status)+1
}

func validModelVersion(value string) bool {
	return value != "" && versionPattern.MatchString(value)
}

func validBeadPrefix(value string) bool {
	return value != "" && beadIDPattern.MatchString(strings.TrimSuffix(value, "-")) && strings.HasSuffix(value, "-")
}

func validateSwitches(s Switches) error {
	if s.LiveDispatchValue != "on" && s.LiveDispatchValue != "off" {
		return fmt.Errorf("unknown live_dispatch switch value %q", s.LiveDispatchValue)
	}
	if s.NotificationDispatchValue != "on" && s.NotificationDispatchValue != "off" {
		return fmt.Errorf("unknown notification_dispatch switch value %q", s.NotificationDispatchValue)
	}
	return nil
}

func validateDelivery(d DeliveryPolicy) error {
	if d.MaxAttempts < 1 || d.MaxAttempts > 5 || d.DeadlineSeconds < 1 || d.DeadlineSeconds > 86400 || d.EscalationRecipient == "" {
		return errors.New("notification delivery bounds are invalid")
	}
	if d.FailoverChannel != "discord" && d.FailoverChannel != "pr_update" {
		return fmt.Errorf("unknown failover channel %q", d.FailoverChannel)
	}
	if len(d.BackoffSeconds) < d.MaxAttempts-1 || len(d.BackoffSeconds) > d.MaxAttempts {
		return errors.New("notification backoff schedule is invalid")
	}
	elapsed := 0
	for _, delay := range d.BackoffSeconds {
		if delay < 0 || delay > d.DeadlineSeconds-elapsed {
			return errors.New("notification backoff schedule is invalid")
		}
		elapsed += delay
	}
	return nil
}

func decision(s Switches, mode Mode) Decision {
	live := s.LiveDispatchValue == "on"
	notify := s.NotificationDispatchValue == "on"
	return Decision{
		LiveDispatch: live, NotificationDispatch: notify,
		RenderAllowed: notify && mode == ModeReminder,
		Deliver:       live && notify && mode == ModeReminder,
	}
}
