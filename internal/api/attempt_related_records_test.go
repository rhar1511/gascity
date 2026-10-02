package api

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/gastownhall/gascity/internal/attemptevidence"
)

func TestAttemptRelatedRecordsRejectConflictsAndPreserveMissingPolicy(t *testing.T) {
	for _, scenario := range []string{"legacy", "different digest", "different store", "different head", "invalid index", "missing attempt", "missing work"} {
		t.Run(scenario, func(t *testing.T) {
			fx := newPRActionFixture(t, true)
			receipt, err := fx.service.Execute(context.Background(), fx.actionRequest(PRActionQueueReview), fx.workerActor())
			if err != nil {
				t.Fatal(err)
			}
			ref := receipt.AdmissionVerdict.Attempt
			evidence := attemptevidence.Evidence{
				AttemptID: ref.AttemptID, StoreRef: ref.StoreRef,
				Identity: attemptevidence.Identity{OwnerBeadID: ref.WorkID},
				BaseSHA:  ref.BaseSHA, CandidateSHA: ref.CandidateSHA, WorkingTreeStatus: ref.WorkingTreeStatus,
				Diff: attemptevidence.DiffSnapshot{SHA256: ref.DiffSHA256, Source: ref.DiffSource},
			}
			switch scenario {
			case "legacy":
				receipt.AdmissionVerdict, receipt.ExecutionVerdict = nil, nil
			case "different digest":
				receipt.AdmissionVerdict.Attempt.DiffSHA256 = "different"
			case "different store":
				receipt.ExecutionVerdict.StoreRef = "rig:other"
			case "different head":
				receipt.HeadSHA = "different"
			case "missing attempt":
				receipt.AttemptID = ""
			case "missing work":
				receipt.WorkID = ""
			case "invalid index":
				if err := fx.store.SetMetadata(receipt.ID, prActionQueueIndexMetadataKey, "different"); err != nil {
					t.Fatal(err)
				}
			}
			encoded, err := json.Marshal(receipt)
			if err != nil {
				t.Fatal(err)
			}
			if err := fx.store.SetMetadata(receipt.ID, prActionRecordMetadataKey, string(encoded)); err != nil {
				t.Fatal(err)
			}
			fx.service.policy = nil
			fx.forge.pullRequests = nil
			result := New(fx.state).attemptRelatedRecords(evidence)
			if scenario != "legacy" {
				if result.Actions.Status != attemptevidence.StatusUnavailable || len(result.Actions.Records) != 0 {
					t.Fatalf("conflicting receipt exposed: %+v", result.Actions)
				}
				return
			}
			if result.Actions.Status != attemptevidence.StatusAvailable || len(result.Actions.Records) != 1 {
				t.Fatalf("legacy receipt missing: %+v", result.Actions)
			}
			record := result.Actions.Records[0]
			if record.AdmissionPolicy.Status != attemptevidence.StatusMissing || record.ExecutionPolicy.Status != attemptevidence.StatusMissing || record.Receipt.AdmissionVerdict != nil || record.Receipt.ExecutionVerdict != nil {
				t.Fatalf("legacy policy was reconstructed: %+v", record)
			}
		})
	}
}
