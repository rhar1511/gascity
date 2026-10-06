package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

func TestPRActionReceiptReadersRejectIdentityDrift(t *testing.T) {
	for _, field := range []string{"id", "work_id", "attempt_id"} {
		t.Run(field, func(t *testing.T) {
			fx := newPRActionFixture(t, true)
			request := fx.actionRequest(PRActionQueueReview)
			result, err := fx.service.Execute(context.Background(), request, fx.workerActor())
			if err != nil {
				t.Fatal(err)
			}
			data, err := json.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
			var record map[string]any
			if err := json.Unmarshal(data, &record); err != nil {
				t.Fatal(err)
			}
			record[field] = ""
			data, err = json.Marshal(record)
			if err != nil {
				t.Fatal(err)
			}
			if err := fx.store.SetMetadata(result.ID, prActionRecordMetadataKey, string(data)); err != nil {
				t.Fatal(err)
			}
			if _, _, err := findPRActionRecord(fx.store, request.IdempotencyKey); err == nil {
				t.Fatal("retry lookup trusted corrupted receipt identity")
			}
			if _, err := listPRActionReceipts(fx.store, request.Monitor, request.Owner, request.Repo, request.PullRequest); err == nil {
				t.Fatal("queue lookup trusted corrupted receipt identity")
			}
		})
	}
}

func TestPRActionResultRequiresExactPersistedReadback(t *testing.T) {
	fx := newPRActionFixture(t, true)
	request := fx.actionRequest(PRActionQueueReview)
	fingerprint, err := prActionFingerprint(request, fx.workerActor())
	if err != nil {
		t.Fatal(err)
	}
	result, err := createPRActionIntent(fx.store, request, fx.workerActor(), fingerprint, fx.now, nil)
	if err != nil {
		t.Fatal(err)
	}
	token, err := claimPRActionRecord(fx.store, result.ID, fingerprint, fx.now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	result.Status = PRActionStatusVerified
	result.Outcome = PRActionOutcomeReviewQueued
	writer, ok := beads.ConditionalWriterFor(fx.store)
	if !ok {
		t.Fatal("fixture lacks conditional writer")
	}
	store := &prActionAlteredResultStore{Store: fx.store, ConditionalWriter: writer}
	if err := persistPRActionResultClaimed(store, &result, token, fx.now); err == nil {
		t.Fatal("changed attempt identity was accepted as a verified persisted result")
	}
}

type prActionAlteredResultStore struct {
	beads.Store
	beads.ConditionalWriter
}

func (s *prActionAlteredResultStore) UpdateIfMatch(id string, revision int64, opts beads.UpdateOpts) error {
	var record map[string]any
	if err := json.Unmarshal([]byte(opts.Metadata[prActionRecordMetadataKey]), &record); err != nil {
		return err
	}
	record["attempt_id"] = "different-attempt"
	data, err := json.Marshal(record)
	if err != nil {
		return err
	}
	opts.Metadata[prActionRecordMetadataKey] = string(data)
	return s.ConditionalWriter.UpdateIfMatch(id, revision, opts)
}

func TestPRActionMetadataCannotBeInjectedByGenericUpdate(t *testing.T) {
	fx := newPRActionFixture(t, true)
	row, err := fx.store.Create(beads.Bead{ID: "ordinary-gate", Title: "ordinary gate", Type: "gate"})
	if err != nil {
		t.Fatal(err)
	}
	handler := newTestCityHandler(t, fx.state)
	req := httptest.NewRequest(http.MethodPost, cityURL(fx.state, "/bead/"+row.ID+"/update"), strings.NewReader(`{"metadata":{"gc.pr_action.source":"api","gc.pr_action.record":"{}"}}`))
	req.Header.Set("X-GC-Request", "true")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("inject ledger metadata status=%d body=%s", rec.Code, rec.Body.String())
	}
	stored, err := fx.store.Get(row.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Metadata[prActionSourceMetadataKey] != "" {
		t.Fatal("generic update injected controller-owned ledger metadata")
	}
}
