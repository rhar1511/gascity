package session

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// SubmitRequest sends a tracked request only to the selected live execution.
// It never wakes, resumes, interrupts, or restarts a session. The durable send
// reservation precedes provider I/O. After an uncertain send, retries read the
// existing receipt rather than risking a second delivery. The mutation lock
// fences cooperating in-process incarnation changes; provider/credential
// isolation is still required against external writers and runtime replacement.
func (m *Manager) SubmitRequest(ctx context.Context, id, requestID string, generation int, message string) (RequestReceipt, error) {
	var result RequestReceipt
	err := withSessionMutationLock(id, func() error {
		b, name, err := m.sessionBead(id)
		if err != nil {
			return err
		}
		info := infoFromPersistedBead(b)
		if info.Generation != strconv.Itoa(generation) || info.Closed || pendingConversationRestart(b) || (info.State != StateActive && info.State != StateAwake) || !m.sp.IsRunning(name) {
			return ErrRequestConflict
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := m.pendingInteractionLocked(name); err != nil {
			return err
		}
		front := NewStore(beads.SessionStore{Store: m.store})
		accepted, err := front.AcceptRequest(id, requestID, generation, message, time.Now())
		if err != nil {
			return err
		}
		result = accepted.RequestReceipt
		claimed := false
		result, err = front.mutateRequestReceipt(id, requestID, func(current beads.Bead, record *storedRequestReceipt) (bool, error) {
			claimed = false
			if current.Metadata["generation"] != strconv.Itoa(generation) || requestDigest(current.Metadata["instance_token"]) != record.ExecutionTokenDigest || current.Status == "closed" {
				return false, ErrRequestConflict
			}
			if record.Delivery != RequestDeliveryPending {
				return false, nil
			}
			stamp := time.Now().UTC()
			record.DeliveryAttemptedAt = &stamp
			record.Delivery = RequestDeliveryUnknown
			claimed = true
			return true, nil
		})
		if err != nil || !claimed {
			return err
		}
		// JSON keeps arbitrary message text distinct from the request identity.
		envelope, err := json.Marshal(struct {
			RequestID  string `json:"request_id"`
			SessionID  string `json:"session_id"`
			Generation int    `json:"generation"`
			Message    string `json:"message"`
		}{requestID, id, generation, message})
		if err != nil {
			return err
		}
		sendErr := m.nudgeSession(ctx, name, string(envelope), false)
		delivery := RequestDeliveryAccepted
		if sendErr != nil {
			delivery = RequestDeliveryUnknown
		}
		result, err = front.RecordRequestDelivery(id, requestID, generation, delivery, time.Now())
		return errors.Join(sendErr, err)
	})
	return result, err
}
