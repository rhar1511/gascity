package api

import (
	"context"
	"encoding/json"
	"testing"
)

// Admission history must retain the actual permission and freshness inputs
// after the current policy and forge state change.
func TestPRActionPreservesAdmissionVerdict(t *testing.T) {
	fx := newPRActionFixture(t, true)
	request := fx.actionRequest(PRActionQueueReview)
	result, err := fx.service.Execute(context.Background(), request, fx.workerActor())
	if err != nil {
		t.Fatal(err)
	}
	fx.policy.Version = "later-policy"
	fx.policy.Monitors[0].RequiredChecks = []string{"different-check"}
	fx.forge.pullRequests = nil
	stored, found, err := findPRActionRecord(fx.store, request.IdempotencyKey)
	if err != nil || !found || stored.ID != result.ID {
		t.Fatalf("stored action=%+v found=%v err=%v", stored, found, err)
	}
	encoded, err := json.Marshal(stored)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &body); err != nil {
		t.Fatal(err)
	}
	if len(body["admission_verdict"]) == 0 || len(body["execution_verdict"]) == 0 {
		t.Fatal("durable action has no historical admission verdict")
	}
	var verdict struct {
		PolicyVersion  string                   `json:"policy_version"`
		HeadSHA        string                   `json:"head_sha"`
		BaseSHA        string                   `json:"base_sha"`
		StoreRef       string                   `json:"store_ref"`
		ObservedAt     string                   `json:"observed_at"`
		FreshUntil     string                   `json:"fresh_until"`
		Action         PRActionOption           `json:"action"`
		Attempt        PRActionAttemptReference `json:"attempt"`
		RequiredChecks []string                 `json:"required_checks"`
	}
	if err := json.Unmarshal(body["admission_verdict"], &verdict); err != nil {
		t.Fatal(err)
	}
	if verdict.PolicyVersion != request.PolicyVersion || verdict.HeadSHA != request.HeadSHA || verdict.BaseSHA != request.BaseSHA || verdict.StoreRef != "rig:myrig" || verdict.ObservedAt == "" || verdict.FreshUntil == "" {
		t.Fatalf("historical verdict lost its original scope or freshness: %+v", verdict)
	}
	if verdict.Action.Action != request.Action || !verdict.Action.Available || verdict.Action.Reason == "" || verdict.Attempt.WorkID != request.WorkID || verdict.Attempt.AttemptID != request.AttemptID || verdict.Attempt.DiffSHA256 == "" {
		t.Fatalf("historical verdict lost the exact permission/evidence: %+v", verdict)
	}
	if len(verdict.RequiredChecks) != 1 || verdict.RequiredChecks[0] != "ci" {
		t.Fatalf("historical checks changed with current policy: %+v", verdict.RequiredChecks)
	}
}

func TestPRActionVerdictRecordsUnavailablePrepareOnRecovery(t *testing.T) {
	verdict := capturePRActionVerdict(PRActionQueueItem{}, PRActionRequest{Action: PRActionPrepare}, PRActionPolicyMonitor{Rig: "myrig"})
	if verdict.Action.Action != PRActionPrepare || verdict.Action.Available || verdict.Action.Reason == "" {
		t.Fatalf("recovery snapshot must explicitly record that prepare was not offered: %+v", verdict.Action)
	}
}
