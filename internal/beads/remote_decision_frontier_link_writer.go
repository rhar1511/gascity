package beads

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	controllerDecisionFrontierLinkReceiptPrefix       = "gc-df-link-v1-"
	controllerDecisionFrontierLinkPermitReplayPrefix  = "gc-df-link-replay-v1-"
	controllerDecisionFrontierLinkDefaultBatchTimeout = controllerBatchApplyTimeout
	controllerDecisionFrontierLinkMaxBatchTimeout     = time.Minute
)

var (
	// ErrRemoteDecisionFrontierLinkWriterUnavailable reports missing or
	// invalid configured capabilities for protected remote decision links.
	ErrRemoteDecisionFrontierLinkWriterUnavailable = errors.New("remote decision-frontier link writer unavailable")
	// ErrRemoteDecisionFrontierLinkPolicy reports a link type not explicitly
	// allowed by the configured decision-frontier link policy.
	ErrRemoteDecisionFrontierLinkPolicy = errors.New("remote decision-frontier link is outside configured policy")
)

// ControllerProtectedLinkPermitIssuer issues the opaque permit for one exact
// protected dependency-link request.
type ControllerProtectedLinkPermitIssuer interface {
	IssueProtectedLink(request ControllerProtectedLinkRequest, replayID string) (string, error)
}

// DecisionFrontierLinkRecordReader reads the authoritative immutable endpoint
// documents used to validate a requested decision-frontier relationship.
type DecisionFrontierLinkRecordReader interface {
	Get(id string) (Bead, error)
}

// RemoteDecisionFrontierLinkWriterConfig injects the only external
// capabilities used by RemoteDecisionFrontierLinkWriter. Actor must be
// controller-authored identity. AllowedDependencyTypes is an explicit local
// policy gate. RecordReader supplies authoritative endpoint documents so the
// adapter can validate the exact relationship before issuing a permit.
type RemoteDecisionFrontierLinkWriterConfig struct {
	Actor                  string
	AllowedDependencyTypes []string
	BatchTimeout           time.Duration
	RecordReader           DecisionFrontierLinkRecordReader
	PermitIssuer           ControllerProtectedLinkPermitIssuer
	ProtectedLinkWriter    ControllerProtectedLinkWriter
}

// RemoteDecisionFrontierLinkWriter adapts the decision-frontier link surface
// to protected exact-ID Beads links. It has no credential, store, or startup
// wiring and can only issue link-only protected requests.
type RemoteDecisionFrontierLinkWriter struct {
	actor                  string
	allowedDependencyTypes map[string]struct{}
	batchTimeout           time.Duration
	recordReader           DecisionFrontierLinkRecordReader
	permitIssuer           ControllerProtectedLinkPermitIssuer
	linkWriter             ControllerProtectedLinkWriter
}

var _ DecisionFrontierLinkWriter = (*RemoteDecisionFrontierLinkWriter)(nil)

// NewRemoteDecisionFrontierLinkWriter constructs an inert adapter from
// caller-owned capabilities. It requires explicit link-type policy and
// validates the trusted actor and request timeout without contacting Beads.
func NewRemoteDecisionFrontierLinkWriter(config RemoteDecisionFrontierLinkWriterConfig) (*RemoteDecisionFrontierLinkWriter, error) {
	if !validControllerBatchApplyText(config.Actor, controllerBatchApplyMaxActorBytes) ||
		len(config.Actor) > controllerBatchApplyMaxActorBytes || strings.TrimSpace(config.Actor) != config.Actor ||
		controllerBatchApplyHasControl(config.Actor) || config.RecordReader == nil ||
		config.PermitIssuer == nil || config.ProtectedLinkWriter == nil ||
		len(config.AllowedDependencyTypes) == 0 {
		return nil, ErrRemoteDecisionFrontierLinkWriterUnavailable
	}
	if config.BatchTimeout == 0 {
		config.BatchTimeout = controllerDecisionFrontierLinkDefaultBatchTimeout
	}
	if config.BatchTimeout < 0 || config.BatchTimeout > controllerDecisionFrontierLinkMaxBatchTimeout {
		return nil, ErrRemoteDecisionFrontierLinkWriterUnavailable
	}
	allowed := make(map[string]struct{}, len(config.AllowedDependencyTypes))
	for _, depType := range config.AllowedDependencyTypes {
		if !validControllerBatchApplyText(depType, controllerBatchApplyMaxTextRunes) ||
			len(depType) > controllerBatchApplyMaxDepTypeBytes || strings.TrimSpace(depType) != depType ||
			controllerBatchApplyHasControl(depType) {
			return nil, ErrRemoteDecisionFrontierLinkWriterUnavailable
		}
		if _, exists := allowed[depType]; exists {
			return nil, ErrRemoteDecisionFrontierLinkWriterUnavailable
		}
		allowed[depType] = struct{}{}
	}
	return &RemoteDecisionFrontierLinkWriter{
		actor: config.Actor, allowedDependencyTypes: allowed, batchTimeout: config.BatchTimeout,
		recordReader: config.RecordReader,
		permitIssuer: config.PermitIssuer, linkWriter: config.ProtectedLinkWriter,
	}, nil
}

// EnsureDecisionFrontierLink sends one protected exact-ID dependency edge.
// The digest-derived receipt and replay IDs make a retry of the same actor,
// endpoints, and relationship converge on the same durable Beads receipt.
func (w *RemoteDecisionFrontierLinkWriter) EnsureDecisionFrontierLink(sourceID, targetID, depType string) error {
	if w == nil || w.recordReader == nil || w.permitIssuer == nil || w.linkWriter == nil {
		return ErrRemoteDecisionFrontierLinkWriterUnavailable
	}
	if _, allowed := w.allowedDependencyTypes[depType]; !allowed {
		return fmt.Errorf("%w: %q", ErrRemoteDecisionFrontierLinkPolicy, depType)
	}

	request := ControllerProtectedLinkRequest{
		Actor: w.actor,
		Link:  ControllerDependencyLink{SourceID: sourceID, TargetID: targetID, Type: depType},
	}
	digest, err := controllerBeadsProtectedLinkDigest(request)
	if err != nil {
		return fmt.Errorf("prepare protected decision-frontier link: %w", err)
	}
	if err := w.authorizeLink(request.Link, depType); err != nil {
		return err
	}
	request.ReceiptID = controllerDecisionFrontierLinkReceiptPrefix + digest
	replayID := controllerDecisionFrontierLinkPermitReplayPrefix + digest
	permit, err := w.permitIssuer.IssueProtectedLink(request, replayID)
	if err != nil {
		return fmt.Errorf("issue protected decision-frontier link permit: %w", err)
	}
	if !validControllerBatchApplyText(permit, controllerBatchApplyMaxPermitBytes) ||
		len(permit) > controllerBatchApplyMaxPermitBytes || strings.TrimSpace(permit) != permit ||
		controllerBatchApplyHasControl(permit) {
		return ErrControllerBeadsPermitRequest
	}
	request.ProtectedPermit = permit
	ctx, cancel := context.WithTimeout(context.Background(), w.batchTimeout)
	defer cancel()
	if _, err := w.linkWriter.ApplyProtectedLink(ctx, request); err != nil {
		return fmt.Errorf("link decision-frontier records %q and %q: %w", sourceID, targetID, err)
	}
	return nil
}

func (w *RemoteDecisionFrontierLinkWriter) authorizeLink(link ControllerDependencyLink, depType string) error {
	source, err := w.recordReader.Get(link.SourceID)
	if err != nil {
		return fmt.Errorf("read decision-frontier link source %q: %w", link.SourceID, err)
	}
	target, err := w.recordReader.Get(link.TargetID)
	if err != nil {
		return fmt.Errorf("read decision-frontier link target %q: %w", link.TargetID, err)
	}
	if source.ID != link.SourceID || target.ID != link.TargetID {
		return ErrDecisionFrontierLinkConflict
	}
	sourceDoc, _, err := decisionFrontierLinkRecord(source)
	if err != nil {
		return err
	}
	mapBead := target
	if target.ID != sourceDoc.MapID {
		mapBead, err = w.recordReader.Get(sourceDoc.MapID)
		if err != nil {
			return fmt.Errorf("read decision-frontier link map %q: %w", sourceDoc.MapID, err)
		}
	}
	if err := validateDecisionFrontierLink(source, target, mapBead, depType); err != nil {
		return err
	}
	return nil
}
