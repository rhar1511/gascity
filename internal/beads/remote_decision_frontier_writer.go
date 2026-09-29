package beads

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"
)

const (
	controllerDecisionFrontierReceiptPrefix       = "gc-df-record-v1-"
	controllerDecisionFrontierPermitReplayPrefix  = "gc-df-replay-v1-"
	controllerDecisionFrontierDefaultBatchTimeout = controllerBatchApplyTimeout
	controllerDecisionFrontierMaxBatchTimeout     = time.Minute
)

var (
	// ErrRemoteDecisionFrontierWriterUnavailable reports an incomplete or
	// invalid set of injected remote writer dependencies.
	ErrRemoteDecisionFrontierWriterUnavailable = errors.New("remote decision-frontier record writer unavailable")
	// ErrRemoteDecisionFrontierRecordShape reports fields the protected remote
	// create contract cannot preserve exactly.
	ErrRemoteDecisionFrontierRecordShape = errors.New("remote decision-frontier record shape unsupported")
	// ErrRemoteDecisionFrontierCASUnsupported records that the protected
	// metadata-CAS dependencies were not explicitly configured.
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
	// EnsureDecisionFrontierLink owns any I/O timeout needed by its transport.
	EnsureDecisionFrontierLink(sourceID, targetID, depType string) error
}

// DecisionFrontierMetadataRecordReader reads one exact record snapshot from
// the same authoritative Beads workspace as the configured Q43 writer. Get
// must reject a response whose returned ID differs from the requested ID and
// include the authoritative revision and metadata values.
type DecisionFrontierMetadataRecordReader interface {
	Get(id string) (Bead, error)
}

// RemoteDecisionFrontierRecordWriterConfig injects the external capabilities
// used by RemoteDecisionFrontierRecordWriter. Actor and ProtectionClass are
// controller-authored policy and must be configured by trusted controller
// code. Metadata transition scope, kind, reader, issuer, and writer are an
// all-or-none opt-in for the inert remote CAS slice.
type RemoteDecisionFrontierRecordWriterConfig struct {
	Actor                    string
	ProtectionClass          string
	BatchTimeout             time.Duration
	PermitIssuer             ControllerProtectedCreateAndLinkPermitIssuer
	BatchWriter              ControllerProtectedCreateAndLinkWriter
	LinkWriter               DecisionFrontierLinkWriter
	MetadataTransitionScope  string
	MetadataTransitionKind   string
	MetadataRecordReader     DecisionFrontierMetadataRecordReader
	MetadataPermitIssuer     ControllerProtectedMutationPermitIssuer
	MetadataTransitionWriter ControllerMetadataTransitionWriter
	MetadataReceiptReader    ControllerMetadataTransitionReceiptReader
}

// RemoteDecisionFrontierRecordWriter adapts remote protected record creation,
// metadata CAS, and decision-frontier linking to the local record-writer
// surface. Its external capabilities are injected; this type performs no
// startup, credential, or store wiring.
type RemoteDecisionFrontierRecordWriter struct {
	actor                    string
	protectionClass          string
	batchTimeout             time.Duration
	permitIssuer             ControllerProtectedCreateAndLinkPermitIssuer
	batchWriter              ControllerProtectedCreateAndLinkWriter
	linkWriter               DecisionFrontierLinkWriter
	metadataTransitionScope  string
	metadataTransitionKind   string
	metadataRecordReader     DecisionFrontierMetadataRecordReader
	metadataPermitIssuer     ControllerProtectedMutationPermitIssuer
	metadataTransitionWriter ControllerMetadataTransitionWriter
	metadataReceiptReader    ControllerMetadataTransitionReceiptReader
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
		controllerBatchApplyHasControl(config.ProtectionClass) || !capabilityValuePresent(config.PermitIssuer) ||
		!capabilityValuePresent(config.BatchWriter) || !capabilityValuePresent(config.LinkWriter) {
		return nil, ErrRemoteDecisionFrontierWriterUnavailable
	}
	if config.BatchTimeout == 0 {
		config.BatchTimeout = controllerDecisionFrontierDefaultBatchTimeout
	}
	if config.BatchTimeout < 0 || config.BatchTimeout > controllerDecisionFrontierMaxBatchTimeout {
		return nil, ErrRemoteDecisionFrontierWriterUnavailable
	}
	if err := validateRemoteDecisionFrontierMetadataCASConfig(config); err != nil {
		return nil, err
	}
	return &RemoteDecisionFrontierRecordWriter{
		actor: config.Actor, protectionClass: config.ProtectionClass,
		batchTimeout: config.BatchTimeout, permitIssuer: config.PermitIssuer,
		batchWriter: config.BatchWriter, linkWriter: config.LinkWriter,
		metadataTransitionScope:  config.MetadataTransitionScope,
		metadataTransitionKind:   config.MetadataTransitionKind,
		metadataRecordReader:     config.MetadataRecordReader,
		metadataPermitIssuer:     config.MetadataPermitIssuer,
		metadataTransitionWriter: config.MetadataTransitionWriter,
		metadataReceiptReader:    config.MetadataReceiptReader,
	}, nil
}

// DecisionFrontierRecordWriterHandle exposes the adapter only when the full
// create, link, exact-record-read, metadata-CAS, and durable-receipt path is
// configured. The constructor validates all-or-none metadata-CAS setup; this
// second check keeps a zero value or a future partially initialized adapter
// from making the capability resolver overclaim support.
func (w *RemoteDecisionFrontierRecordWriter) DecisionFrontierRecordWriterHandle() (DecisionFrontierRecordWriter, bool) {
	if w == nil || w.actor == "" || w.protectionClass == "" || w.batchTimeout <= 0 ||
		!capabilityValuePresent(w.permitIssuer) || !capabilityValuePresent(w.batchWriter) || !capabilityValuePresent(w.linkWriter) ||
		w.metadataTransitionScope == "" || w.metadataTransitionKind == "" ||
		!capabilityValuePresent(w.metadataRecordReader) || !capabilityValuePresent(w.metadataPermitIssuer) ||
		!capabilityValuePresent(w.metadataTransitionWriter) || !capabilityValuePresent(w.metadataReceiptReader) {
		return nil, false
	}
	return w, true
}

// CreateDecisionFrontierRecord submits exactly one validated immutable record
// through protected batchApply. The deterministic digest-derived receipt and
// replay IDs keep exact retries stable while binding changed content to a new
// protected request. The batch API does not return the stored row, so the
// returned bead is a detached projection of the caller's exact create input.
func (w *RemoteDecisionFrontierRecordWriter) CreateDecisionFrontierRecord(record Bead) (Bead, error) {
	if w == nil || !capabilityValuePresent(w.permitIssuer) || !capabilityValuePresent(w.batchWriter) {
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
			Description: record.Description, Labels: cloneControllerDecisionFrontierLabels(record.Labels),
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
	ctx, cancel := context.WithTimeout(context.Background(), w.batchTimeout)
	defer cancel()
	if _, err := w.batchWriter.ApplyProtectedCreateAndLink(ctx, request); err != nil {
		return Bead{}, fmt.Errorf("create protected decision-frontier record %q: %w", record.ID, err)
	}
	return cloneBead(record), nil
}

// CompareAndSetDecisionFrontierRecordMetadataKey uses the configured exact
// record reader, generic Beads permit issuer, and Q43 metadata transition
// transport. The operation refuses when any required CAS dependency is absent.
func (w *RemoteDecisionFrontierRecordWriter) CompareAndSetDecisionFrontierRecordMetadataKey(id, key, expected, next string) (bool, error) {
	if w == nil || !capabilityValuePresent(w.metadataRecordReader) || !capabilityValuePresent(w.metadataPermitIssuer) ||
		!capabilityValuePresent(w.metadataTransitionWriter) || !capabilityValuePresent(w.metadataReceiptReader) {
		return false, ErrRemoteDecisionFrontierCASUnsupported
	}
	return w.compareAndSetDecisionFrontierRecordMetadataKey(id, key, expected, next)
}

// EnsureDecisionFrontierLink delegates to the explicitly injected narrow link
// contract. It does not fold links into arbitrary record create requests.
func (w *RemoteDecisionFrontierRecordWriter) EnsureDecisionFrontierLink(sourceID, targetID, depType string) error {
	if w == nil || !capabilityValuePresent(w.linkWriter) {
		return ErrRemoteDecisionFrontierWriterUnavailable
	}
	return w.linkWriter.EnsureDecisionFrontierLink(sourceID, targetID, depType)
}

// capabilityValuePresent treats an interface containing a typed nil as absent.
// Otherwise a handle resolver could report support and panic only when the
// first request reaches a nil receiver.
func capabilityValuePresent(capability any) bool {
	if capability == nil {
		return false
	}
	value := reflect.ValueOf(capability)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return !value.IsNil()
	default:
		return true
	}
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

func cloneControllerDecisionFrontierLabels(labels []string) []string {
	if labels == nil {
		return nil
	}
	return append([]string{}, labels...)
}

func remoteDecisionFrontierRecordProjectionSupported(record Bead) bool {
	return record.SourceStoreRef == "" && record.LifecycleScope == "" && record.Status == "" &&
		record.Priority == nil && record.CreatedAt.IsZero() && record.UpdatedAt.IsZero() &&
		record.Assignee == "" && record.From == "" && record.ParentID == "" && record.Ref == "" &&
		len(record.Needs) == 0 && len(record.Dependencies) == 0 && !record.Ephemeral && !record.NoHistory &&
		record.DeferUntil == nil && record.IsBlocked == nil && !record.IndefinitelyDeferred &&
		record.Revision == 0 && record.ClaimFence == 0
}
