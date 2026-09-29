package decisionfrontier

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"testing"
	"time"

	sessiondomain "github.com/gastownhall/gascity/internal/session"
)

func TestSessionPromptDeliveryResolvesInjectedTargetFromPersistedSession(t *testing.T) {
	request := promptRequestForDeliveryTest()
	reader := &promptSessionReaderFake{
		resolvedID: "session-42",
		info:       sessiondomain.Info{ID: "session-42", State: sessiondomain.StateActive, MetadataState: string(sessiondomain.StateActive), Generation: "7"},
		response:   sessiondomain.PersistedResponse{Status: "open"},
	}
	submitter := &promptSessionSubmitterFake{}
	delivery := newPromptDeliveryForTest(t, "ops", reader, submitter)

	binding, err := delivery.ResolveDecisionPrompt(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if binding != (PromptBinding{SessionID: "session-42", ExecutionGeneration: 7, RequestID: request.ID}) {
		t.Fatalf("binding = %+v", binding)
	}
	if reader.resolveTarget != "ops" || reader.resolveCalls != 1 || reader.persistedCalls != 1 {
		t.Fatalf("target resolution = target %q, resolve calls %d, persisted reads %d", reader.resolveTarget, reader.resolveCalls, reader.persistedCalls)
	}
	if submitter.calls != 0 || reader.receiptCalls != 0 {
		t.Fatalf("resolution performed delivery or receipt I/O: submissions=%d receipt reads=%d", submitter.calls, reader.receiptCalls)
	}
}

func TestSessionPromptDeliveryResolveRejectsAmbiguityAndInvalidExecution(t *testing.T) {
	request := promptRequestForDeliveryTest()
	tests := []struct {
		name      string
		target    string
		reader    promptSessionReaderFake
		wantErrIs error
	}{
		{
			name:      "ambiguous target",
			target:    "ops",
			reader:    promptSessionReaderFake{resolveErr: fmt.Errorf("%w: ops", sessiondomain.ErrAmbiguous)},
			wantErrIs: sessiondomain.ErrAmbiguous,
		},
		{
			name:   "generation missing",
			target: "ops",
			reader: promptSessionReaderFake{resolvedID: "session-42", info: sessiondomain.Info{ID: "session-42", State: sessiondomain.StateActive, MetadataState: string(sessiondomain.StateActive)}, response: sessiondomain.PersistedResponse{Status: "open"}},
		},
		{
			name:   "closed session",
			target: "ops",
			reader: promptSessionReaderFake{resolvedID: "session-42", info: sessiondomain.Info{ID: "session-42", State: sessiondomain.StateActive, MetadataState: string(sessiondomain.StateActive), Generation: "7", Closed: true}, response: sessiondomain.PersistedResponse{Status: "closed"}},
		},
		{
			name:   "inactive session",
			target: "ops",
			reader: promptSessionReaderFake{resolvedID: "session-42", info: sessiondomain.Info{ID: "session-42", State: sessiondomain.StateAsleep, MetadataState: string(sessiondomain.StateAsleep), Generation: "7"}, response: sessiondomain.PersistedResponse{Status: "open"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reader := &tt.reader
			submitter := &promptSessionSubmitterFake{}
			delivery := newPromptDeliveryForTest(t, tt.target, reader, submitter)
			_, err := delivery.ResolveDecisionPrompt(context.Background(), request)
			if err == nil {
				t.Fatal("ResolveDecisionPrompt() error = nil, want rejection")
			}
			if tt.wantErrIs != nil && !errors.Is(err, tt.wantErrIs) {
				t.Fatalf("ResolveDecisionPrompt() error = %v, want %v", err, tt.wantErrIs)
			}
			if submitter.calls != 0 {
				t.Fatalf("resolve rejection submitted %d requests", submitter.calls)
			}
		})
	}

	badRequest := request
	badRequest.MessageDigest = "bad"
	reader := &promptSessionReaderFake{resolvedID: "session-42", info: sessiondomain.Info{ID: "session-42", State: sessiondomain.StateActive, MetadataState: string(sessiondomain.StateActive), Generation: "7"}, response: sessiondomain.PersistedResponse{Status: "open"}}
	if _, err := newPromptDeliveryForTest(t, "ops", reader, &promptSessionSubmitterFake{}).ResolveDecisionPrompt(context.Background(), badRequest); !errors.Is(err, ErrConflict) {
		t.Fatalf("digest mismatch error = %v, want ErrConflict", err)
	}
}

func TestNewSessionPromptDeliveryRejectsFactoryTargetsAndMissingPorts(t *testing.T) {
	reader := &promptSessionReaderFake{}
	submitter := &promptSessionSubmitterFake{}
	for _, tt := range []struct {
		name      string
		target    string
		reader    PromptSessionReader
		submitter PromptRequestSubmitter
	}{
		{name: "empty target", target: " ", reader: reader, submitter: submitter},
		{name: "template target", target: "template:operator", reader: reader, submitter: submitter},
		{name: "missing reader", target: "ops", submitter: submitter},
		{name: "missing submitter", target: "ops", reader: reader},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := NewSessionPromptDelivery(tt.target, tt.reader, tt.submitter); !errors.Is(err, ErrUnavailable) {
				t.Fatalf("NewSessionPromptDelivery() error = %v, want ErrUnavailable", err)
			}
		})
	}
}

func TestSessionPromptDeliverySubmitsCanonicalDigestBoundMessage(t *testing.T) {
	request := promptRequestForDeliveryTest()
	binding := PromptBinding{SessionID: "session-42", ExecutionGeneration: 7, RequestID: request.ID}
	reader := &promptSessionReaderFake{}
	submitter := &promptSessionSubmitterFake{receipt: promptReceiptForTest(request, binding, sessiondomain.RequestDeliveryAccepted)}
	delivery := newPromptDeliveryForTest(t, "ops", reader, submitter)

	result, err := delivery.DeliverDecisionPrompt(context.Background(), request, binding)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "accepted" {
		t.Fatalf("result = %+v, want accepted", result)
	}
	if submitter.calls != 1 || submitter.sessionID != binding.SessionID || submitter.requestID != binding.RequestID || submitter.generation != 7 {
		t.Fatalf("submit arguments = calls %d, session %q, request %q, generation %d", submitter.calls, submitter.sessionID, submitter.requestID, submitter.generation)
	}
	digest := sha256.Sum256([]byte(submitter.message))
	if got := hex.EncodeToString(digest[:]); got != request.MessageDigest {
		t.Fatalf("submitted message digest = %s, want persisted MessageDigest %s", got, request.MessageDigest)
	}
}

func TestSessionPromptDeliveryKeepsAcknowledgementSeparateAndFailsClosed(t *testing.T) {
	request := promptRequestForDeliveryTest()
	binding := PromptBinding{SessionID: "session-42", ExecutionGeneration: 7, RequestID: request.ID}
	stamp := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name       string
		receipt    sessiondomain.RequestReceipt
		submitErr  error
		wantStatus string
		wantErrIs  error
	}{
		{
			name:       "accepted remains unacknowledged",
			receipt:    promptReceiptForTest(request, binding, sessiondomain.RequestDeliveryAccepted),
			wantStatus: "accepted",
		},
		{
			name:       "queued is only accepted",
			receipt:    promptReceiptForTest(request, binding, sessiondomain.RequestDeliveryQueued),
			wantStatus: "accepted",
		},
		{
			name: "acknowledged is its own stage",
			receipt: func() sessiondomain.RequestReceipt {
				r := promptReceiptForTest(request, binding, sessiondomain.RequestDeliveryAccepted)
				r.AcknowledgedAt = &stamp
				return r
			}(),
			wantStatus: "acknowledged",
		},
		{
			name:       "unknown receipt is not success",
			receipt:    promptReceiptForTest(request, binding, sessiondomain.RequestDeliveryUnknown),
			wantStatus: "unknown",
			wantErrIs:  ErrPromptDeliveryUnavailable,
		},
		{
			name:       "submit error stays uncertain",
			receipt:    promptReceiptForTest(request, binding, sessiondomain.RequestDeliveryAccepted),
			submitErr:  errors.New("provider result write failed"),
			wantStatus: "unknown",
			wantErrIs:  ErrPromptDeliveryUnavailable,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			submitter := &promptSessionSubmitterFake{receipt: tt.receipt, err: tt.submitErr}
			delivery := newPromptDeliveryForTest(t, "ops", &promptSessionReaderFake{}, submitter)
			result, err := delivery.DeliverDecisionPrompt(context.Background(), request, binding)
			if tt.wantErrIs == nil && err != nil {
				t.Fatalf("DeliverDecisionPrompt() error = %v", err)
			}
			if tt.wantErrIs != nil && !errors.Is(err, tt.wantErrIs) {
				t.Fatalf("DeliverDecisionPrompt() error = %v, want %v", err, tt.wantErrIs)
			}
			if result.Status != tt.wantStatus {
				t.Fatalf("result = %+v, want status %q", result, tt.wantStatus)
			}
		})
	}
}

func TestSessionPromptDeliveryRejectsBindingMismatchBeforeSubmit(t *testing.T) {
	request := promptRequestForDeliveryTest()
	submitter := &promptSessionSubmitterFake{}
	delivery := newPromptDeliveryForTest(t, "ops", &promptSessionReaderFake{}, submitter)
	for _, binding := range []PromptBinding{
		{SessionID: " ", ExecutionGeneration: 7, RequestID: request.ID},
		{SessionID: "session-42", ExecutionGeneration: 0, RequestID: request.ID},
		{SessionID: "session-42", ExecutionGeneration: 7, RequestID: "different"},
	} {
		if _, err := delivery.DeliverDecisionPrompt(context.Background(), request, binding); !errors.Is(err, ErrConflict) {
			t.Errorf("binding %+v error = %v, want ErrConflict", binding, err)
		}
	}
	if submitter.calls != 0 {
		t.Fatalf("invalid binding reached submitter %d times", submitter.calls)
	}
}

func TestSessionPromptDeliveryReconcilesExactReceiptStages(t *testing.T) {
	request := promptRequestForDeliveryTest()
	binding := PromptBinding{SessionID: "session-42", ExecutionGeneration: 7, RequestID: request.ID}
	stamp := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name       string
		receipt    sessiondomain.RequestReceipt
		receiptErr error
		wantStatus string
		wantAbsent bool
		wantErrIs  error
	}{
		{
			name:       "missing is definitive absence",
			receiptErr: sessiondomain.ErrRequestNotFound,
			wantStatus: "absent",
			wantAbsent: true,
		},
		{
			name:       "pending has not been submitted",
			receipt:    promptReceiptForTest(request, binding, sessiondomain.RequestDeliveryPending),
			wantStatus: "absent",
			wantAbsent: true,
		},
		{
			name:       "accepted is not acknowledgement",
			receipt:    promptReceiptForTest(request, binding, sessiondomain.RequestDeliveryAccepted),
			wantStatus: "accepted",
		},
		{
			name:       "queued is not acknowledgement",
			receipt:    promptReceiptForTest(request, binding, sessiondomain.RequestDeliveryQueued),
			wantStatus: "accepted",
		},
		{
			name: "acknowledgement remains separate",
			receipt: func() sessiondomain.RequestReceipt {
				r := promptReceiptForTest(request, binding, sessiondomain.RequestDeliveryAccepted)
				r.AcknowledgedAt = &stamp
				return r
			}(),
			wantStatus: "acknowledged",
		},
		{
			name:       "unknown cannot authorize retry",
			receipt:    promptReceiptForTest(request, binding, sessiondomain.RequestDeliveryUnknown),
			wantStatus: "unknown",
			wantErrIs:  ErrPromptDeliveryUnavailable,
		},
		{
			name:       "storage error is unavailable",
			receiptErr: errors.New("store offline"),
			wantErrIs:  ErrUnavailable,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reader := &promptSessionReaderFake{
				resolvedID: binding.SessionID,
				info:       sessiondomain.Info{ID: binding.SessionID, State: sessiondomain.StateActive, MetadataState: string(sessiondomain.StateActive), Generation: "7"},
				response:   sessiondomain.PersistedResponse{Status: "open"},
				receipt:    tt.receipt,
				receiptErr: tt.receiptErr,
			}
			delivery := newPromptDeliveryForTest(t, "ops", reader, &promptSessionSubmitterFake{})
			result, err := delivery.ReconcileDecisionPrompt(context.Background(), request, binding)
			if tt.wantErrIs == nil && err != nil {
				t.Fatalf("ReconcileDecisionPrompt() error = %v", err)
			}
			if tt.wantErrIs != nil && !errors.Is(err, tt.wantErrIs) {
				t.Fatalf("ReconcileDecisionPrompt() error = %v, want %v", err, tt.wantErrIs)
			}
			if result.Status != tt.wantStatus || result.DefinitivelyAbsent != tt.wantAbsent {
				t.Fatalf("result = %+v, want status %q definitive-absence %t", result, tt.wantStatus, tt.wantAbsent)
			}
			if reader.receiptSessionID != binding.SessionID || reader.receiptRequestID != request.ID {
				t.Fatalf("receipt lookup = %q/%q, want exact %q/%q", reader.receiptSessionID, reader.receiptRequestID, binding.SessionID, request.ID)
			}
		})
	}
}

func TestSessionPromptDeliveryReconcileRejectsGenerationAndReceiptMismatch(t *testing.T) {
	request := promptRequestForDeliveryTest()
	binding := PromptBinding{SessionID: "session-42", ExecutionGeneration: 7, RequestID: request.ID}
	tests := []struct {
		name    string
		info    sessiondomain.Info
		receipt sessiondomain.RequestReceipt
	}{
		{
			name:    "current generation changed",
			info:    sessiondomain.Info{ID: binding.SessionID, State: sessiondomain.StateActive, MetadataState: string(sessiondomain.StateActive), Generation: "8"},
			receipt: promptReceiptForTest(request, binding, sessiondomain.RequestDeliveryAccepted),
		},
		{
			name: "receipt session mismatch",
			info: sessiondomain.Info{ID: binding.SessionID, State: sessiondomain.StateActive, MetadataState: string(sessiondomain.StateActive), Generation: "7"},
			receipt: func() sessiondomain.RequestReceipt {
				r := promptReceiptForTest(request, binding, sessiondomain.RequestDeliveryAccepted)
				r.SessionID = "other"
				return r
			}(),
		},
		{
			name: "receipt request mismatch",
			info: sessiondomain.Info{ID: binding.SessionID, State: sessiondomain.StateActive, MetadataState: string(sessiondomain.StateActive), Generation: "7"},
			receipt: func() sessiondomain.RequestReceipt {
				r := promptReceiptForTest(request, binding, sessiondomain.RequestDeliveryAccepted)
				r.RequestID = "other"
				return r
			}(),
		},
		{
			name: "receipt generation mismatch",
			info: sessiondomain.Info{ID: binding.SessionID, State: sessiondomain.StateActive, MetadataState: string(sessiondomain.StateActive), Generation: "7"},
			receipt: func() sessiondomain.RequestReceipt {
				r := promptReceiptForTest(request, binding, sessiondomain.RequestDeliveryAccepted)
				r.Generation = 6
				return r
			}(),
		},
		{
			name: "receipt digest mismatch",
			info: sessiondomain.Info{ID: binding.SessionID, State: sessiondomain.StateActive, MetadataState: string(sessiondomain.StateActive), Generation: "7"},
			receipt: func() sessiondomain.RequestReceipt {
				r := promptReceiptForTest(request, binding, sessiondomain.RequestDeliveryAccepted)
				r.MessageDigest = "bad"
				return r
			}(),
		},
		{
			name: "corrupt accepted stage",
			info: sessiondomain.Info{ID: binding.SessionID, State: sessiondomain.StateActive, MetadataState: string(sessiondomain.StateActive), Generation: "7"},
			receipt: func() sessiondomain.RequestReceipt {
				r := promptReceiptForTest(request, binding, sessiondomain.RequestDeliveryAccepted)
				r.ProviderResultAt = nil
				return r
			}(),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reader := &promptSessionReaderFake{info: tt.info, response: sessiondomain.PersistedResponse{Status: "open"}, receipt: tt.receipt}
			delivery := newPromptDeliveryForTest(t, "ops", reader, &promptSessionSubmitterFake{})
			if _, err := delivery.ReconcileDecisionPrompt(context.Background(), request, binding); !errors.Is(err, ErrConflict) {
				t.Fatalf("ReconcileDecisionPrompt() error = %v, want ErrConflict", err)
			}
		})
	}
}

func TestSessionPromptDeliveryRejectsDigestMismatchBeforeSubmit(t *testing.T) {
	request := promptRequestForDeliveryTest()
	request.MessageDigest = "0000000000000000000000000000000000000000000000000000000000000000"
	submitter := &promptSessionSubmitterFake{}
	delivery := newPromptDeliveryForTest(t, "ops", &promptSessionReaderFake{}, submitter)
	if _, err := delivery.DeliverDecisionPrompt(context.Background(), request, PromptBinding{SessionID: "session-42", ExecutionGeneration: 7, RequestID: request.ID}); !errors.Is(err, ErrConflict) {
		t.Fatalf("DeliverDecisionPrompt() error = %v, want ErrConflict", err)
	}
	if submitter.calls != 0 {
		t.Fatalf("digest mismatch sent %d requests", submitter.calls)
	}
}

func promptRequestForDeliveryTest() PromptRequest {
	request := PromptRequest{
		CityRef: "city:test-city", StoreRef: "city:test-city", ID: "decision-prompt:test-map",
		WorkID: "work-1", WorkRevision: "3", WorkDigest: "work-digest", MapID: "map-1",
		PresentationVersion: promptPresentationVersion, TicketIDs: []string{"ticket-1"},
		Questions: []PromptQuestionPresentation{{ID: "q1", TicketID: "ticket-1", Version: "v1", Title: "Choose", Prompt: "Which option?"}},
	}
	request.MessageDigest = promptMessageDigest(request)
	return request
}

func promptReceiptForTest(request PromptRequest, binding PromptBinding, delivery sessiondomain.RequestDelivery) sessiondomain.RequestReceipt {
	now := time.Date(2026, 9, 29, 11, 0, 0, 0, time.UTC)
	receipt := sessiondomain.RequestReceipt{
		RequestID: binding.RequestID, SessionID: binding.SessionID, Generation: int(binding.ExecutionGeneration),
		MessageDigest: request.MessageDigest, AcceptedAt: now, Delivery: delivery, Effect: "unverified",
	}
	switch delivery {
	case sessiondomain.RequestDeliveryAccepted, sessiondomain.RequestDeliveryQueued:
		receipt.ProviderResultAt = &now
	case sessiondomain.RequestDeliveryUnknown:
		receipt.DeliveryAttemptedAt = &now
	}
	return receipt
}

func newPromptDeliveryForTest(t *testing.T, target string, reader *promptSessionReaderFake, submitter *promptSessionSubmitterFake) *SessionPromptDelivery {
	t.Helper()
	delivery, err := NewSessionPromptDelivery(target, reader, submitter)
	if err != nil {
		t.Fatal(err)
	}
	return delivery
}

type promptSessionReaderFake struct {
	resolvedID       string
	resolveErr       error
	resolveTarget    string
	resolveCalls     int
	info             sessiondomain.Info
	response         sessiondomain.PersistedResponse
	persistedErr     error
	persistedCalls   int
	receipt          sessiondomain.RequestReceipt
	receiptErr       error
	receiptSessionID string
	receiptRequestID string
	receiptCalls     int
}

func (f *promptSessionReaderFake) ResolveID(target string) (string, error) {
	f.resolveCalls++
	f.resolveTarget = target
	return f.resolvedID, f.resolveErr
}

func (f *promptSessionReaderFake) GetPersistedResponse(id string) (sessiondomain.Info, sessiondomain.PersistedResponse, error) {
	f.persistedCalls++
	if f.info.ID != "" && f.info.ID != id {
		return sessiondomain.Info{}, sessiondomain.PersistedResponse{}, fmt.Errorf("unexpected session id %q", id)
	}
	return f.info, f.response, f.persistedErr
}

func (f *promptSessionReaderFake) GetRequest(sessionID, requestID string) (sessiondomain.RequestReceipt, error) {
	f.receiptCalls++
	f.receiptSessionID, f.receiptRequestID = sessionID, requestID
	return f.receipt, f.receiptErr
}

type promptSessionSubmitterFake struct {
	calls      int
	sessionID  string
	requestID  string
	generation int
	message    string
	receipt    sessiondomain.RequestReceipt
	err        error
}

func (f *promptSessionSubmitterFake) SubmitRequest(_ context.Context, sessionID, requestID string, generation int, message string) (sessiondomain.RequestReceipt, error) {
	f.calls++
	f.sessionID, f.requestID, f.generation, f.message = sessionID, requestID, generation, message
	return f.receipt, f.err
}
