package beads

import (
	"encoding/json"
	"fmt"
)

// ControllerMetadataTransitionProblem preserves the stable status and code
// from a Q43 problem response without retaining its caller-controlled detail.
type ControllerMetadataTransitionProblem struct {
	Status int
	Code   string
}

func (p *ControllerMetadataTransitionProblem) Error() string {
	if p == nil {
		return "controller metadata transition problem: <nil>"
	}
	return fmt.Sprintf("controller metadata transition refused with HTTP %d (%s)", p.Status, p.Code)
}

// ControllerMetadataTransitionRequest describes one revision-fenced metadata
// transition supported by the controller's configured Beads HTTP transport.
// ReceiptID must stay the same when recovering or retrying this request.
// Expected and Value use nil for an absent metadata key; a raw JSON null is a
// present value. An empty Payload is sent as omitted and means {} to Beads.
// ProtectedPermit is sent opaquely only when the server advertises protected
// mutation support.
type ControllerMetadataTransitionRequest struct {
	ReceiptID       string
	Scope           string
	Kind            string
	Actor           string
	ExpectedVersion int64
	Key             string
	Expected        *json.RawMessage
	Value           *json.RawMessage
	Payload         json.RawMessage
	// ProtectedPermit authorizes a protected marker transition when nonempty.
	ProtectedPermit string
}

// ControllerMetadataTransitionReceipt is the immutable Q43 receipt for an
// applied transition. Expected and Value preserve absent versus JSON null.
type ControllerMetadataTransitionReceipt struct {
	ReceiptID       string
	IssueID         string
	Scope           string
	Kind            string
	Actor           string
	ExpectedVersion int64
	ToVersion       int64
	Key             string
	Expected        json.RawMessage
	Value           json.RawMessage
	Payload         json.RawMessage
}

// ControllerMetadataTransitionResult reports an applied transition or a
// marker refusal. Current is absent when nil and is present JSON null when it
// contains the bytes "null".
type ControllerMetadataTransitionResult struct {
	Applied  bool
	Replayed bool
	Current  json.RawMessage
	Receipt  *ControllerMetadataTransitionReceipt
}

// ControllerMetadataTransitionWriter exposes only the dedicated Q43
// revision-transition HTTP capability. It does not satisfy the separate
// decision-frontier receipt contract.
type ControllerMetadataTransitionWriter interface {
	TransitionMetadata(issueID string, request ControllerMetadataTransitionRequest) (ControllerMetadataTransitionResult, error)
}

// ControllerMetadataTransitionWriterHandleProvider preserves wrapper-owned
// routing, cache invalidation, and mutation-generation checks.
type ControllerMetadataTransitionWriterHandleProvider interface {
	ControllerMetadataTransitionWriterHandle() (ControllerMetadataTransitionWriter, bool)
}

// ControllerMetadataTransitionWriterFor resolves only an explicitly exposed
// Q43 transport handle or a direct implementation. It never unwraps a store.
func ControllerMetadataTransitionWriterFor(store Store) (ControllerMetadataTransitionWriter, bool) {
	if store == nil {
		return nil, false
	}
	if provider, ok := store.(ControllerMetadataTransitionWriterHandleProvider); ok {
		return provider.ControllerMetadataTransitionWriterHandle()
	}
	writer, ok := store.(ControllerMetadataTransitionWriter)
	return writer, ok
}
