package beads

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
)

const (
	decisionFrontierMapKind      = "decision-frontier/map/v1"
	decisionFrontierQuestionKind = "decision-frontier/question/v1"
	decisionFrontierPromptKind   = "decision-frontier/prompt/v1"
)

// DecisionFrontierMapRecordID returns the stable ID used for a map record.
func DecisionFrontierMapRecordID(cityRef, storeRef, workID, workRevision string) string {
	return decisionFrontierStableID("map", cityRef, storeRef, workID, workRevision)
}

// DecisionFrontierQuestionRecordID returns the stable ID used for a question
// ticket in one exact map.
func DecisionFrontierQuestionRecordID(cityRef, storeRef, mapID, questionID string) string {
	return decisionFrontierStableID("q", cityRef, storeRef, mapID, questionID)
}

// DecisionFrontierPromptRecordID returns the stable ID used for a prompt
// intent in one exact map.
func DecisionFrontierPromptRecordID(cityRef, storeRef, mapID string) string {
	return decisionFrontierStableID("prompt", cityRef, storeRef, mapID)
}

func decisionFrontierStableID(kind string, parts ...string) string {
	joined := kind
	for _, part := range parts {
		joined += "\x00" + part
	}
	sum := sha256.Sum256([]byte(joined))
	return "gcf-" + kind + "-" + hex.EncodeToString(sum[:16])
}

type decisionFrontierLinkIdentity struct {
	SchemaVersion int    `json:"schema_version"`
	CityRef       string `json:"city_ref"`
	StoreRef      string `json:"store_ref"`
	WorkID        string `json:"work_id"`
	WorkRevision  string `json:"work_revision"`
	WorkDigest    string `json:"work_digest"`
	MapID         string `json:"map_id"`
}

type decisionFrontierLinkQuestion struct {
	ID              string   `json:"id"`
	Title           string   `json:"title"`
	Prompt          string   `json:"prompt"`
	Recommendations []string `json:"recommendations,omitempty"`
	DependsOn       []string `json:"depends_on,omitempty"`
	SourceLinks     []string `json:"source_links,omitempty"`
}

type decisionFrontierLinkDocument struct {
	SchemaVersion int                            `json:"schema_version"`
	CityRef       string                         `json:"city_ref"`
	StoreRef      string                         `json:"store_ref"`
	WorkID        string                         `json:"work_id"`
	WorkRevision  string                         `json:"work_revision"`
	WorkDigest    string                         `json:"work_digest"`
	MapID         string                         `json:"map_id"`
	ID            string                         `json:"id"`
	PromptID      string                         `json:"prompt_id"`
	Question      decisionFrontierLinkQuestion   `json:"question"`
	Questions     []decisionFrontierLinkQuestion `json:"questions"`
	TicketIDs     []string                       `json:"ticket_ids"`
}

func (doc decisionFrontierLinkDocument) identity() decisionFrontierLinkIdentity {
	return decisionFrontierLinkIdentity{
		SchemaVersion: doc.SchemaVersion, CityRef: doc.CityRef, StoreRef: doc.StoreRef,
		WorkID: doc.WorkID, WorkRevision: doc.WorkRevision, WorkDigest: doc.WorkDigest, MapID: doc.MapID,
	}
}

// decisionFrontierLinkRecord reads and validates the immutable record
// envelope before a trusted edge operation uses its document as authority.
func decisionFrontierLinkRecord(bead Bead) (decisionFrontierLinkDocument, string, error) {
	var doc decisionFrontierLinkDocument
	kind := bead.Metadata[beadmeta.DecisionFrontierRecordMetadataKey]
	if bead.Type != "gate" || (kind != decisionFrontierMapKind && kind != decisionFrontierQuestionKind && kind != decisionFrontierPromptKind) {
		return doc, "", ErrDecisionFrontierLinkConflict
	}
	if bead.Title == "" || strings.TrimSpace(bead.Title) != bead.Title {
		return doc, "", ErrDecisionFrontierLinkConflict
	}
	for key := range bead.Metadata {
		if key == beadmeta.DecisionFrontierRecordMetadataKey || key == beadmeta.DecisionFrontierStateMetadataKey {
			continue
		}
		if key == beadmeta.DecisionFrontierReasonMetadataKey && kind == decisionFrontierPromptKind {
			continue
		}
		return doc, "", ErrDecisionFrontierLinkConflict
	}
	if err := json.Unmarshal([]byte(bead.Description), &doc); err != nil {
		return doc, "", fmt.Errorf("%w: decode %q: %w", ErrDecisionFrontierLinkConflict, bead.ID, err)
	}
	revision, err := strconv.ParseInt(doc.WorkRevision, 10, 64)
	if doc.SchemaVersion != 1 || doc.CityRef == "" || doc.StoreRef == "" || doc.WorkID == "" ||
		doc.WorkDigest == "" || doc.MapID == "" || err != nil || revision == 0 || strconv.FormatInt(revision, 10) != doc.WorkRevision ||
		doc.MapID != DecisionFrontierMapRecordID(doc.CityRef, doc.StoreRef, doc.WorkID, doc.WorkRevision) {
		return doc, "", ErrDecisionFrontierLinkConflict
	}
	state := bead.Metadata[beadmeta.DecisionFrontierStateMetadataKey]
	initial := map[string]string{
		decisionFrontierMapKind:      "pending",
		decisionFrontierQuestionKind: "pending",
		decisionFrontierPromptKind:   "unconfigured",
	}[kind]
	if !validDecisionFrontierLinkState(kind, initial, state) {
		return doc, "", ErrDecisionFrontierLinkConflict
	}
	switch kind {
	case decisionFrontierMapKind:
		if bead.ID != doc.MapID || doc.PromptID != DecisionFrontierPromptRecordID(doc.CityRef, doc.StoreRef, doc.MapID) || len(doc.Questions) == 0 {
			return doc, "", ErrDecisionFrontierLinkConflict
		}
		seen := make(map[string]struct{}, len(doc.Questions))
		for _, question := range doc.Questions {
			if question.ID == "" {
				return doc, "", ErrDecisionFrontierLinkConflict
			}
			if _, ok := seen[question.ID]; ok {
				return doc, "", ErrDecisionFrontierLinkConflict
			}
			seen[question.ID] = struct{}{}
		}
	case decisionFrontierQuestionKind:
		if doc.Question.ID == "" || bead.ID != DecisionFrontierQuestionRecordID(doc.CityRef, doc.StoreRef, doc.MapID, doc.Question.ID) {
			return doc, "", ErrDecisionFrontierLinkConflict
		}
	case decisionFrontierPromptKind:
		if doc.ID != bead.ID || bead.ID != DecisionFrontierPromptRecordID(doc.CityRef, doc.StoreRef, doc.MapID) {
			return doc, "", ErrDecisionFrontierLinkConflict
		}
	default:
		return doc, "", ErrDecisionFrontierLinkConflict
	}
	return doc, kind, nil
}

// validDecisionFrontierLinkState mirrors the decisionfrontier record-state
// validator. Keep the accepted immutable record states aligned here so the
// storage capability can validate endpoints without importing its caller.
func validDecisionFrontierLinkState(kind, initial, state string) bool {
	switch kind {
	case decisionFrontierMapKind:
		return state == initial || state == "resolved"
	case decisionFrontierQuestionKind:
		if state == initial {
			return true
		}
		for _, prefix := range []string{"answering:", "answered:", "unresolved:"} {
			if strings.HasPrefix(state, prefix) && strings.TrimPrefix(state, prefix) != "" {
				return true
			}
		}
		return false
	case decisionFrontierPromptKind:
		switch state {
		case initial, "pending", "submitting", "accepted", "delivered", "acknowledged", "unknown", "failed":
			return true
		default:
			return false
		}
	default:
		return false
	}
}

func sameDecisionFrontierLinkIdentity(a, b decisionFrontierLinkIdentity) bool {
	return a.SchemaVersion == b.SchemaVersion && a.CityRef == b.CityRef && a.StoreRef == b.StoreRef &&
		a.WorkID == b.WorkID && a.WorkRevision == b.WorkRevision && a.WorkDigest == b.WorkDigest && a.MapID == b.MapID
}

func decisionFrontierQuestionEqual(a, b decisionFrontierLinkQuestion) bool {
	return a.ID == b.ID && a.Title == b.Title && a.Prompt == b.Prompt &&
		slices.Equal(a.Recommendations, b.Recommendations) && slices.Equal(a.DependsOn, b.DependsOn) &&
		slices.Equal(a.SourceLinks, b.SourceLinks)
}

func decisionFrontierQuestionInMap(question decisionFrontierLinkQuestion, mapDoc decisionFrontierLinkDocument) bool {
	for _, candidate := range mapDoc.Questions {
		if decisionFrontierQuestionEqual(question, candidate) {
			return true
		}
	}
	return false
}

func decisionFrontierPromptTicketsMatch(prompt decisionFrontierLinkDocument, mapDoc decisionFrontierLinkDocument) bool {
	if len(prompt.TicketIDs) != len(mapDoc.Questions) {
		return false
	}
	for i, question := range mapDoc.Questions {
		if prompt.TicketIDs[i] != DecisionFrontierQuestionRecordID(mapDoc.CityRef, mapDoc.StoreRef, mapDoc.MapID, question.ID) {
			return false
		}
	}
	return true
}

// validateDecisionFrontierLink permits only the exact graph relationships
// represented by immutable map, question, and prompt documents.
func validateDecisionFrontierLink(source, target, mapBead Bead, depType string) error {
	sourceDoc, sourceKind, err := decisionFrontierLinkRecord(source)
	if err != nil {
		return err
	}
	targetDoc, targetKind, err := decisionFrontierLinkRecord(target)
	if err != nil {
		return err
	}
	mapDoc, mapKind, err := decisionFrontierLinkRecord(mapBead)
	if err != nil {
		return err
	}
	if mapKind != decisionFrontierMapKind || mapBead.ID != sourceDoc.MapID ||
		!sameDecisionFrontierLinkIdentity(sourceDoc.identity(), mapDoc.identity()) ||
		!sameDecisionFrontierLinkIdentity(targetDoc.identity(), mapDoc.identity()) {
		return ErrDecisionFrontierLinkConflict
	}
	switch depType {
	case "relates-to":
		if targetKind != decisionFrontierMapKind || target.ID != mapBead.ID {
			return ErrDecisionFrontierLinkConflict
		}
		switch sourceKind {
		case decisionFrontierQuestionKind:
			if !decisionFrontierQuestionInMap(sourceDoc.Question, mapDoc) {
				return ErrDecisionFrontierLinkConflict
			}
		case decisionFrontierPromptKind:
			if sourceDoc.ID != mapDoc.PromptID || !decisionFrontierPromptTicketsMatch(sourceDoc, mapDoc) {
				return ErrDecisionFrontierLinkConflict
			}
		default:
			return ErrDecisionFrontierLinkConflict
		}
		return nil
	case "blocks":
		if sourceKind != decisionFrontierQuestionKind || targetKind != decisionFrontierQuestionKind || source.ID == target.ID ||
			!decisionFrontierQuestionInMap(sourceDoc.Question, mapDoc) || !decisionFrontierQuestionInMap(targetDoc.Question, mapDoc) ||
			!slices.Contains(sourceDoc.Question.DependsOn, targetDoc.Question.ID) {
			return ErrDecisionFrontierLinkConflict
		}
		return nil
	default:
		return ErrDecisionFrontierLinkConflict
	}
}
