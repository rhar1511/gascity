package beads

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	controllerDecisionFrontierReceiptPrefix      = "gc-df-record-v1-"
	controllerDecisionFrontierPermitReplayPrefix = "gc-df-replay-v1-"
	controllerDecisionFrontierDefaultTimeout     = controllerBatchApplyTimeout
	controllerDecisionFrontierMaxTimeout         = time.Minute
)

var (
	// ErrRemoteDecisionFrontierWriterUnavailable reports an incomplete or
	// invalid set of injected remote writer dependencies.
	ErrRemoteDecisionFrontierWriterUnavailable = errors.New("remote decision-frontier record writer unavailable")
	// ErrRemoteDecisionFrontierRecordShape reports fields the protected remote
	// create contract cannot preserve exactly.
	ErrRemoteDecisionFrontierRecordShape = errors.New("remote decision-frontier record shape unsupported")
	// ErrRemoteDecisionFrontierCASUnsupported records the adapter's deliberate
	// fail-closed boundary until a protected remote record-CAS transport exists.
	ErrRemoteDecisionFrontierCASUnsupported = fmt.Errorf("remote decision-frontier record CAS unavailable: %w", ErrConditionalWriteUnsupported)
)

// ControllerProtectedCreateAndLinkPermitIssuer issues the opaque permit that
// authorizes one exact controller-owned protected batch request.
type ControllerProtectedCreateAndLinkPermitIssuer interface {
	IssueProtectedCreateAndLink(request ControllerProtectedCreateAndLinkRequest, replayID string) (string, error)
}

// DecisionFrontierLinkWriter exposes only the graph mutation this adapter
// needs. Implementations must authorize the relationship from the immutable
// decision-frontier endpoint documents and make exact retries safe.
type DecisionFrontierLinkWriter interface {
	EnsureDecisionFrontierLink(sourceID, targetID, depType string) error
}

// RemoteDecisionFrontierRecordWriterConfig injects the only external
// capabilities used by RemoteDecisionFrontierRecordWriter. The actor is
// controller-authored request identity and must be configured by trusted
// controller code.
type RemoteDecisionFrontierRecordWriterConfig struct {
	Actor           string
	ProtectionClass string
	RequestTimeout  time.Duration
	PermitIssuer    ControllerProtectedCreateAndLinkPermitIssuer
	BatchWriter     ControllerProtectedCreateAndLinkWriter
	LinkWriter      DecisionFrontierLinkWriter
}

// RemoteDecisionFrontierRecordWriter adapts remote protected record creation
// and decision-frontier linking to the local record-writer surface. The
// controller permit issuer, protected batch writer, and narrow link writer are
// injected; this type performs no startup, credential, or store wiring.
//
// This type deliberately does not expose itself as a usable
// DecisionFrontierRecordWriter capability handle. Its record CAS method is
// unavailable, and Service's current capability check treats that handle as
// complete even though Answer needs CAS before it can safely advance a ticket.
// A future integration must add and verify remote CAS before exposing this
// adapter through a store's DecisionFrontierRecordWriterHandle.
type RemoteDecisionFrontierRecordWriter struct {
	actor           string
	protectionClass string
	requestTimeout  time.Duration
	permitIssuer    ControllerProtectedCreateAndLinkPermitIssuer
	batchWriter     ControllerProtectedCreateAndLinkWriter
	linkWriter      DecisionFrontierLinkWriter
}

var (
	_ DecisionFrontierRecordWriter               = (*RemoteDecisionFrontierRecordWriter)(nil)
	_ DecisionFrontierRecordWriterHandleProvider = (*RemoteDecisionFrontierRecordWriter)(nil)
)

// NewRemoteDecisionFrontierRecordWriter constructs an inert adapter from
// caller-owned capabilities. It validates configured request identity,
// protection class, and timeout without reading credentials or contacting the
// remote service.
func NewRemoteDecisionFrontierRecordWriter(config RemoteDecisionFrontierRecordWriterConfig) (*RemoteDecisionFrontierRecordWriter, error) {
	if !validControllerBatchApplyText(config.Actor, controllerBatchApplyMaxActorBytes) ||
		len(config.Actor) > controllerBatchApplyMaxActorBytes || strings.TrimSpace(config.Actor) != config.Actor ||
		controllerBatchApplyHasControl(config.Actor) ||
		!validControllerBatchApplyText(config.ProtectionClass, controllerBatchApplyMaxTextRunes) ||
		len(config.ProtectionClass) > controllerBatchApplyMaxClassBytes || strings.TrimSpace(config.ProtectionClass) != config.ProtectionClass ||
		controllerBatchApplyHasControl(config.ProtectionClass) || config.PermitIssuer == nil ||
		config.BatchWriter == nil || config.LinkWriter == nil {
		return nil, ErrRemoteDecisionFrontierWriterUnavailable
	}
	if config.RequestTimeout == 0 {
		config.RequestTimeout = controllerDecisionFrontierDefaultTimeout
	}
	if config.RequestTimeout < 0 || config.RequestTimeout > controllerDecisionFrontierMaxTimeout {
		return nil, ErrRemoteDecisionFrontierWriterUnavailable
	}
	return &RemoteDecisionFrontierRecordWriter{
		actor: config.Actor, protectionClass: config.ProtectionClass,
		requestTimeout: config.RequestTimeout, permitIssuer: config.PermitIssuer,
		batchWriter: config.BatchWriter, linkWriter: config.LinkWriter,
	}, nil
}

// DecisionFrontierRecordWriterHandle intentionally refuses to advertise the
// adapter through the store capability resolver. The current service checks
// the combined interface before Ensure/Answer, so publishing a writer whose
// CAS always fails would let those flows start with incomplete authority.
func (*RemoteDecisionFrontierRecordWriter) DecisionFrontierRecordWriterHandle() (DecisionFrontierRecordWriter, bool) {
	return nil, false
}

// CreateDecisionFrontierRecord submits exactly one validated immutable record
// through protected batchApply. The deterministic digest-derived receipt and
// replay IDs keep exact retries stable while binding changed content to a new
// protected request. The batch API does not return the stored row, so the
// returned bead is a detached projection of the caller's exact create input.
func (w *RemoteDecisionFrontierRecordWriter) CreateDecisionFrontierRecord(record Bead) (Bead, error) {
	if w == nil || w.permitIssuer == nil || w.batchWriter == nil {
		return Bead{}, ErrRemoteDecisionFrontierWriterUnavailable
	}
	if err := validateDecisionFrontierRecordCreate(record); err != nil {
		return Bead{}, err
	}
	if !remoteDecisionFrontierRecordProjectionSupported(record) {
		return Bead{}, ErrRemoteDecisionFrontierRecordShape
	}

	request := ControllerProtectedCreateAndLinkRequest{
		Actor: w.actor,
		Record: ControllerProtectedRecord{
			ID: record.ID, Type: record.Type, Title: record.Title,
			Description: record.Description, Labels: append([]string(nil), record.Labels...),
			Metadata:        cloneControllerDecisionFrontierMetadata(record.Metadata),
			ProtectionClass: w.protectionClass,
		},
	}
	digest, err := controllerBeadsProtectedBatchDigest(request)
	if err != nil {
		return Bead{}, fmt.Errorf("prepare protected decision-frontier create: %w", err)
	}
	request.ReceiptID = controllerDecisionFrontierReceiptPrefix + digest
	replayID := controllerDecisionFrontierPermitReplayPrefix + digest
	permit, err := w.permitIssuer.IssueProtectedCreateAndLink(request, replayID)
	if err != nil {
		return Bead{}, fmt.Errorf("issue protected decision-frontier create permit: %w", err)
	}
	if permit == "" || strings.TrimSpace(permit) != permit || controllerBatchApplyHasControl(permit) {
		return Bead{}, ErrControllerBeadsPermitRequest
	}
	request.ProtectedPermit = permit
	ctx, cancel := context.WithTimeout(context.Background(), w.requestTimeout)
	defer cancel()
	if _, err := w.batchWriter.ApplyProtectedCreateAndLink(ctx, request); err != nil {
		return Bead{}, fmt.Errorf("create protected decision-frontier record %q: %w", record.ID, err)
	}
	return cloneBead(record), nil
}

// CompareAndSetDecisionFrontierRecordMetadataKey always fails closed. The
// available protected batchApply contract has no remote decision-record CAS.
func (*RemoteDecisionFrontierRecordWriter) CompareAndSetDecisionFrontierRecordMetadataKey(string, string, string, string) (bool, error) {
	return false, ErrRemoteDecisionFrontierCASUnsupported
}

// EnsureDecisionFrontierLink delegates to the explicitly injected narrow link
// contract. It does not fold links into arbitrary record create requests.
func (w *RemoteDecisionFrontierRecordWriter) EnsureDecisionFrontierLink(sourceID, targetID, depType string) error {
	if w == nil || w.linkWriter == nil {
		return ErrRemoteDecisionFrontierWriterUnavailable
	}
	return w.linkWriter.EnsureDecisionFrontierLink(sourceID, targetID, depType)
}

func cloneControllerDecisionFrontierMetadata(metadata StringMap) map[string]string {
	if metadata == nil {
		return nil
	}
	cloned := make(map[string]string, len(metadata))
	for key, value := range metadata {
		cloned[key] = value
	}
	return cloned
}

func remoteDecisionFrontierRecordProjectionSupported(record Bead) bool {
	return record.SourceStoreRef == "" && record.LifecycleScope == "" && record.Status == "" &&
		record.Priority == nil && record.CreatedAt.IsZero() && record.UpdatedAt.IsZero() &&
		record.Assignee == "" && record.From == "" && record.ParentID == "" && record.Ref == "" &&
		len(record.Needs) == 0 && len(record.Dependencies) == 0 && !record.Ephemeral && !record.NoHistory &&
		record.DeferUntil == nil && record.IsBlocked == nil && !record.IndefinitelyDeferred &&
		record.Revision == 0 && record.ClaimFence == 0
}
