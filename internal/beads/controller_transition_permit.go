package beads

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// ControllerMetadataTransitionProtectedMutationOperation is the Beads operation
// used when a Q43 metadata transition requires a host-issued mutation permit.
const ControllerMetadataTransitionProtectedMutationOperation = "issue.revision_transition"

// ControllerMetadataTransitionProtectedMutationDigest binds a permit to the
// exact request sent by the metadata-transition client, including the URL issue
// ID. It clears the permit itself and preserves absent versus JSON null values.
// The digest grants no authority; a trusted issuer must authorize the operation.
func ControllerMetadataTransitionProtectedMutationDigest(issueID string, request ControllerMetadataTransitionRequest) (string, error) {
	plan, err := planControllerMetadataTransition(issueID, request)
	if err != nil {
		return "", fmt.Errorf("invalid metadata transition for protected permit digest: %w", err)
	}
	// Beads hashes issueops.RevisionTransitionRequest, whose fields have no
	// JSON tags. This is intentionally different from its lower-case HTTP wire.
	type backendRequest struct {
		ProtectedPermit string
		ReceiptID       string
		IssueID         string
		Scope           string
		Kind            string
		Actor           string
		ExpectedVersion int64
		Key             string
		Expected        *json.RawMessage
		Value           *json.RawMessage
		Payload         json.RawMessage
	}
	canonical := backendRequest{
		ReceiptID: plan.request.ReceiptID, IssueID: plan.issueID,
		Scope: plan.request.Scope, Kind: plan.request.Kind, Actor: plan.request.Actor,
		ExpectedVersion: plan.request.ExpectedVersion, Key: plan.request.Key,
		Expected: plan.request.Expected, Value: plan.request.Value, Payload: plan.request.Payload,
	}
	encoded, err := json.Marshal(canonical)
	if err != nil {
		return "", fmt.Errorf("encode metadata transition permit request: %w", err)
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}
