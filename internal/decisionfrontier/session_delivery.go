package decisionfrontier

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"

	sessiondomain "github.com/gastownhall/gascity/internal/session"
)

// PromptSessionReader is the persisted session surface used by the prompt
// adapter. Its methods read only session beads and request receipts; they do
// not observe or contact a runtime provider.
type PromptSessionReader interface {
	ResolveID(identifier string) (string, error)
	GetPersistedResponse(id string) (sessiondomain.Info, sessiondomain.PersistedResponse, error)
	GetRequest(sessionID, requestID string) (sessiondomain.RequestReceipt, error)
}

// PromptRequestSubmitter is the live-only request path. The session manager
// persists a receipt and reserves the one allowed send before provider I/O.
type PromptRequestSubmitter interface {
	SubmitRequest(ctx context.Context, sessionID, requestID string, generation int, message string) (sessiondomain.RequestReceipt, error)
}

// SessionPromptDelivery adapts persisted decision prompts to tracked session
// requests. Target is supplied by composition; this package does not choose a
// role or resolve a new target during replay.
type SessionPromptDelivery struct {
	target    string
	sessions  PromptSessionReader
	submitter PromptRequestSubmitter
}

// NewSessionPromptDelivery creates a provider adapter for one configured live
// session target. Target resolution remains a persisted session-store read.
func NewSessionPromptDelivery(target string, sessions PromptSessionReader, submitter PromptRequestSubmitter) (*SessionPromptDelivery, error) {
	if target == "" || strings.TrimSpace(target) != target || target != sessiondomain.NormalizeNamedSessionTarget(target) ||
		strings.HasPrefix(target, "template:") || sessions == nil || submitter == nil {
		return nil, fmt.Errorf("%w: a live session target, session reader, and request submitter are required", ErrUnavailable)
	}
	return &SessionPromptDelivery{target: target, sessions: sessions, submitter: submitter}, nil
}

// ResolveDecisionPrompt binds the request to one open, active persisted
// session execution. ResolveID refuses ambiguous identifiers, while the
// subsequent typed read captures the exact persisted generation without
// observing a runtime provider.
func (d *SessionPromptDelivery) ResolveDecisionPrompt(ctx context.Context, request PromptRequest) (PromptBinding, error) {
	if err := d.validateRequest(ctx, request); err != nil {
		return PromptBinding{}, err
	}
	sessionID, err := d.sessions.ResolveID(d.target)
	if err != nil {
		return PromptBinding{}, fmt.Errorf("resolve configured session target %q: %w", d.target, err)
	}
	info, response, err := d.sessions.GetPersistedResponse(sessionID)
	if err != nil {
		return PromptBinding{}, errors.Join(ErrUnavailable, fmt.Errorf("read resolved session %s: %w", sessionID, err))
	}
	if err := configuredPromptTarget(info, d.target, sessionID); err != nil {
		return PromptBinding{}, err
	}
	generation, err := persistedSessionGeneration(info, response, sessionID, true)
	if err != nil {
		return PromptBinding{}, err
	}
	return PromptBinding{SessionID: sessionID, ExecutionGeneration: int64(generation), RequestID: request.ID}, nil
}

// DeliverDecisionPrompt submits the exact persisted request to its bound
// execution. The canonical JSON body is also the payload hashed into
// PromptRequest.MessageDigest, so the manager receipt has the same digest.
func (d *SessionPromptDelivery) DeliverDecisionPrompt(ctx context.Context, request PromptRequest, binding PromptBinding) (PromptResult, error) {
	if err := d.validateRequest(ctx, request); err != nil {
		return PromptResult{}, err
	}
	generation, err := validPromptDeliveryBinding(binding, request)
	if err != nil {
		return PromptResult{}, err
	}
	info, response, err := d.sessions.GetPersistedResponse(binding.SessionID)
	if err != nil {
		return PromptResult{}, errors.Join(ErrUnavailable, fmt.Errorf("read bound session %s before delivery: %w", binding.SessionID, err))
	}
	if err := configuredPromptTarget(info, d.target, binding.SessionID); err != nil {
		return PromptResult{}, err
	}
	currentGeneration, err := persistedSessionGeneration(info, response, binding.SessionID, true)
	if err != nil {
		return PromptResult{}, err
	}
	if currentGeneration != generation {
		return PromptResult{}, fmt.Errorf("%w: bound session generation changed from %d to %d", ErrConflict, generation, currentGeneration)
	}
	body, err := promptMessageJSON(request)
	if err != nil {
		return PromptResult{}, errors.Join(ErrConflict, fmt.Errorf("encode decision prompt: %w", err))
	}
	if digest(body) != request.MessageDigest {
		return PromptResult{}, fmt.Errorf("%w: encoded decision prompt does not match its message digest", ErrConflict)
	}
	receipt, submitErr := d.submitter.SubmitRequest(ctx, binding.SessionID, binding.RequestID, generation, string(body))
	if errors.Is(submitErr, sessiondomain.ErrRequestConflict) {
		return PromptResult{}, errors.Join(ErrConflict, submitErr)
	}
	if submitErr != nil {
		return PromptResult{Status: "unknown", Reason: "session request submission returned an error"}, errors.Join(ErrPromptDeliveryUnavailable, submitErr)
	}
	result, err := promptReceiptResult(receipt, request, binding)
	if err != nil {
		return PromptResult{}, err
	}
	if result.Status == "unknown" || result.Status == "absent" {
		return result, ErrPromptDeliveryUnavailable
	}
	return result, nil
}

// ReconcileDecisionPrompt reads only the exact bound session, generation, and
// request receipt. It never follows a changed alias or submits another prompt.
func (d *SessionPromptDelivery) ReconcileDecisionPrompt(ctx context.Context, request PromptRequest, binding PromptBinding) (PromptResult, error) {
	if err := d.validateRequest(ctx, request); err != nil {
		return PromptResult{}, err
	}
	if _, err := validPromptDeliveryBinding(binding, request); err != nil {
		return PromptResult{}, err
	}
	var result PromptResult
	err := sessiondomain.WithSessionMutationLock(binding.SessionID, func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		info, response, err := d.sessions.GetPersistedResponse(binding.SessionID)
		if err != nil {
			return errors.Join(ErrUnavailable, fmt.Errorf("read bound session %s: %w", binding.SessionID, err))
		}
		generation, err := persistedSessionGeneration(info, response, binding.SessionID, false)
		if err != nil {
			return err
		}
		if err := configuredPromptTarget(info, d.target, binding.SessionID); err != nil {
			return err
		}
		if int64(generation) != binding.ExecutionGeneration {
			return fmt.Errorf("%w: bound session generation changed from %d to %d", ErrConflict, binding.ExecutionGeneration, generation)
		}
		receipt, err := d.sessions.GetRequest(binding.SessionID, binding.RequestID)
		if errors.Is(err, sessiondomain.ErrRequestNotFound) {
			result = PromptResult{Status: "absent", DefinitivelyAbsent: true}
			return nil
		}
		if err != nil {
			if errors.Is(err, sessiondomain.ErrRequestConflict) {
				return errors.Join(ErrConflict, fmt.Errorf("read exact session request receipt: %w", err))
			}
			return errors.Join(ErrUnavailable, fmt.Errorf("read exact session request receipt: %w", err))
		}
		result, err = promptReceiptResult(receipt, request, binding)
		return err
	})
	if err != nil {
		return PromptResult{}, err
	}
	if result.Status == "unknown" {
		return result, ErrPromptDeliveryUnavailable
	}
	return result, nil
}

func (d *SessionPromptDelivery) validateRequest(ctx context.Context, request PromptRequest) error {
	if d == nil || d.sessions == nil || d.submitter == nil {
		return ErrUnavailable
	}
	if ctx == nil {
		return fmt.Errorf("%w: request context is required", ErrInvalid)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if request.ID == "" || strings.TrimSpace(request.ID) != request.ID ||
		request.PresentationVersion != promptPresentationVersion || request.MessageDigest == "" ||
		request.MessageDigest != promptMessageDigest(request) {
		return fmt.Errorf("%w: persisted decision prompt identity or digest is invalid", ErrConflict)
	}
	return nil
}

func configuredPromptTarget(info sessiondomain.Info, target, sessionID string) error {
	if info.ID != sessionID || !info.ConfiguredNamedSession ||
		info.ConfiguredNamedIdentity != target {
		return fmt.Errorf("%w: session %q is not the configured named target %q", ErrConflict, sessionID, target)
	}
	return nil
}

func validPromptDeliveryBinding(binding PromptBinding, request PromptRequest) (int, error) {
	if !validPromptBinding(binding, request.ID) || int64(int(binding.ExecutionGeneration)) != binding.ExecutionGeneration {
		return 0, fmt.Errorf("%w: decision prompt execution binding is invalid", ErrConflict)
	}
	return int(binding.ExecutionGeneration), nil
}

func persistedSessionGeneration(info sessiondomain.Info, response sessiondomain.PersistedResponse, sessionID string, requireActive bool) (int, error) {
	if info.ID != sessionID ||
		(response.Status != "open" && response.Status != "closed") ||
		(info.Closed != (response.Status == "closed")) {
		return 0, fmt.Errorf("%w: persisted record does not match exact session %q", ErrConflict, sessionID)
	}
	if requireActive && (info.Closed || response.Status != "open") {
		return 0, fmt.Errorf("%w: target did not resolve to the exact open session %q", ErrConflict, sessionID)
	}
	generation, err := strconv.Atoi(info.Generation)
	if err != nil || generation <= 0 || strconv.Itoa(generation) != info.Generation {
		return 0, fmt.Errorf("%w: session %q has an invalid persisted generation", ErrConflict, sessionID)
	}
	if requireActive && sessiondomain.ProjectLifecycle(sessiondomain.LifecycleInputFromInfo(info)).BaseState != sessiondomain.BaseStateActive {
		return 0, fmt.Errorf("%w: target session %q is not persisted active", ErrConflict, sessionID)
	}
	return generation, nil
}

func promptReceiptResult(receipt sessiondomain.RequestReceipt, request PromptRequest, binding PromptBinding) (PromptResult, error) {
	if receipt.RequestID != binding.RequestID || receipt.SessionID != binding.SessionID ||
		receipt.Generation <= 0 || int64(receipt.Generation) != binding.ExecutionGeneration ||
		receipt.MessageDigest != request.MessageDigest || !validPromptDigest(receipt.MessageDigest) ||
		receipt.AcceptedAt.IsZero() || receipt.Effect != "unverified" || receipt.Attempt != nil ||
		(receipt.DeliveryAttemptedAt != nil && receipt.DeliveryAttemptedAt.IsZero()) ||
		(receipt.ProviderResultAt != nil && receipt.ProviderResultAt.IsZero()) ||
		(receipt.AcknowledgedAt != nil && receipt.AcknowledgedAt.IsZero()) {
		return PromptResult{}, fmt.Errorf("%w: session request receipt does not match the persisted prompt binding", ErrConflict)
	}
	if receipt.AcknowledgedAt != nil && receipt.Delivery == sessiondomain.RequestDeliveryPending {
		return PromptResult{}, fmt.Errorf("%w: pending session request has an acknowledgement", ErrConflict)
	}
	switch receipt.Delivery {
	case sessiondomain.RequestDeliveryPending:
		if receipt.DeliveryAttemptedAt != nil || receipt.ProviderResultAt != nil {
			return PromptResult{}, fmt.Errorf("%w: pending session request contains send evidence", ErrConflict)
		}
		return PromptResult{Status: "absent", DefinitivelyAbsent: true}, nil
	case sessiondomain.RequestDeliveryAccepted, sessiondomain.RequestDeliveryQueued:
		if receipt.ProviderResultAt == nil {
			return PromptResult{}, fmt.Errorf("%w: accepted session request lacks provider result time", ErrConflict)
		}
		if receipt.AcknowledgedAt != nil {
			return PromptResult{Status: "acknowledged"}, nil
		}
		return PromptResult{Status: "accepted"}, nil
	case sessiondomain.RequestDeliveryUnknown:
		if receipt.DeliveryAttemptedAt == nil && receipt.ProviderResultAt == nil {
			return PromptResult{}, fmt.Errorf("%w: unknown session request lacks send evidence", ErrConflict)
		}
		if receipt.AcknowledgedAt != nil {
			return PromptResult{Status: "acknowledged"}, nil
		}
		return PromptResult{Status: "unknown"}, nil
	default:
		return PromptResult{}, fmt.Errorf("%w: session request receipt has an unknown delivery stage", ErrConflict)
	}
}

func validPromptDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

var _ PromptDelivery = (*SessionPromptDelivery)(nil)
