package api

import (
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/attemptevidence"
	"github.com/gastownhall/gascity/internal/beads"
)

// AttemptEvidenceRead retains the sealed snapshot and separately reports
// durable records read after capture. The combined response is not an atomic
// snapshot across the archive and action ledger.
type AttemptEvidenceRead struct {
	attemptevidence.Evidence
	RelatedRecords AttemptRelatedRecords `json:"related_records"`
}

// AttemptRelatedRecords contains only records attributed to this exact attempt.
type AttemptRelatedRecords struct {
	Actions          AttemptActionRecords  `json:"actions"`
	Acknowledgements AttemptRequestRecords `json:"acknowledgements"`
}

// AttemptActionRecords makes incomplete ledger reads distinguishable from
// missing records. Neither state proves that no action occurred.
type AttemptActionRecords struct {
	Status  string                     `json:"status"`
	Reason  string                     `json:"reason,omitempty"`
	Records []HistoricalPRActionRecord `json:"records"`
}

// HistoricalPRActionRecord reports the captured policy availability explicitly
// for older receipts that predate verdict persistence.
type HistoricalPRActionRecord struct {
	Receipt         PRActionResult        `json:"receipt"`
	AdmissionPolicy attemptevidence.Facet `json:"admission_policy"`
	ExecutionPolicy attemptevidence.Facet `json:"execution_policy"`
}

// Called only after the original archive scope has been authorized. A current
// Queue read would lose closed PRs and depend on mutable policy and forge state.
func (s *Server) attemptRelatedRecords(evidence attemptevidence.Evidence) AttemptRelatedRecords {
	result := AttemptRelatedRecords{
		Actions:          AttemptActionRecords{Status: attemptevidence.StatusUnavailable, Reason: "action_ledger_unavailable", Records: []HistoricalPRActionRecord{}},
		Acknowledgements: s.attemptAcknowledgements(evidence),
	}
	var store beads.Store
	switch {
	case evidence.StoreRef == "city:"+s.state.CityName():
		store = s.state.CityBeadStore()
	case strings.HasPrefix(evidence.StoreRef, "rig:"):
		store = s.state.BeadStore(strings.TrimPrefix(evidence.StoreRef, "rig:"))
	}
	if store == nil {
		return result
	}
	rows, err := store.List(beads.ListQuery{Metadata: map[string]string{prActionSourceMetadataKey: prActionRecordSource}, IncludeClosed: true})
	if err != nil {
		return result
	}
	records := []HistoricalPRActionRecord{}
	seen := make(map[string]bool)
	for _, row := range rows {
		receipt, err := decodePRActionReceipt(row)
		if err != nil || seen[receipt.ID] {
			result.Actions.Reason = "action_ledger_validation_failed"
			return result
		}
		seen[receipt.ID] = true
		if receipt.WorkID != evidence.Identity.OwnerBeadID || receipt.AttemptID != evidence.AttemptID {
			continue
		}
		if receipt.HeadSHA != evidence.CandidateSHA || receipt.BaseSHA != evidence.BaseSHA ||
			!historicalVerdictMatches(receipt.AdmissionVerdict, receipt, evidence) ||
			!historicalVerdictMatches(receipt.ExecutionVerdict, receipt, evidence) {
			result.Actions.Reason = "action_attempt_binding_conflict"
			return result
		}
		records = append(records, HistoricalPRActionRecord{
			Receipt: receipt, AdmissionPolicy: historicalPolicyFacet(receipt.AdmissionVerdict), ExecutionPolicy: historicalPolicyFacet(receipt.ExecutionVerdict),
		})
	}
	sort.Slice(records, func(i, j int) bool {
		if !records[i].Receipt.CreatedAt.Equal(records[j].Receipt.CreatedAt) {
			return records[i].Receipt.CreatedAt.Before(records[j].Receipt.CreatedAt)
		}
		return records[i].Receipt.ID < records[j].Receipt.ID
	})
	result.Actions = AttemptActionRecords{Status: attemptevidence.StatusAvailable, Records: records}
	if len(records) == 0 {
		result.Actions.Status = attemptevidence.StatusMissing
		result.Actions.Reason = "no_attributed_pr_action_records"
	}
	return result
}

func historicalPolicyFacet(verdict *PRActionPolicyVerdict) attemptevidence.Facet {
	if verdict == nil {
		return attemptevidence.Facet{Status: attemptevidence.StatusMissing, Reason: "policy_verdict_not_captured"}
	}
	return attemptevidence.Facet{Status: attemptevidence.StatusAvailable}
}

func historicalVerdictMatches(verdict *PRActionPolicyVerdict, receipt PRActionResult, evidence attemptevidence.Evidence) bool {
	if verdict == nil {
		return true // Legacy receipts retain explicitly missing policy evidence.
	}
	ref := verdict.Attempt
	return verdict.PolicyVersion == receipt.PolicyVersion && verdict.Monitor == receipt.Monitor &&
		verdict.Owner == receipt.Owner && verdict.Repo == receipt.Repo && verdict.PullRequest == receipt.PullRequest &&
		verdict.StoreRef == evidence.StoreRef && verdict.HeadSHA == evidence.CandidateSHA && verdict.BaseSHA == evidence.BaseSHA &&
		verdict.Action.Action == receipt.Action && ref != nil && ref.StoreRef == evidence.StoreRef &&
		ref.WorkID == evidence.Identity.OwnerBeadID && ref.AttemptID == evidence.AttemptID &&
		ref.BaseSHA == evidence.BaseSHA && ref.CandidateSHA == evidence.CandidateSHA &&
		ref.DiffSHA256 == evidence.Diff.SHA256 && ref.DiffSource == evidence.Diff.Source && ref.WorkingTreeStatus == evidence.WorkingTreeStatus
}
