package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPRActionLedgerCannotBeForgedOrChangedThroughBeadAPI(t *testing.T) {
	fx := newPRActionFixture(t, true)
	request := fx.actionRequest(PRActionQueueReview)
	request.IdempotencyKey = "ledger-guard-action-1"
	result, err := fx.service.Execute(context.Background(), request, fx.workerActor())
	if err != nil || result.Status != PRActionStatusVerified {
		t.Fatalf("create verified action result = %+v, %v", result, err)
	}

	handler := newTestCityHandler(t, fx.state)
	path := "/bead/" + result.ID
	attempts := []struct {
		method string
		path   string
		body   string
	}{
		{method: http.MethodPost, path: path + "/update", body: `{"type":"task","metadata":{"gc.pr_action.record":"{\"status\":\"forged\"}"}}`},
		{method: http.MethodPost, path: path + "/close"},
		{method: http.MethodPost, path: path + "/reopen"},
		{method: http.MethodPost, path: path + "/assign", body: `{"assignee":"worker"}`},
		{method: http.MethodDelete, path: path},
	}
	for _, attempt := range attempts {
		req := httptest.NewRequest(attempt.method, cityURL(fx.state, attempt.path), strings.NewReader(attempt.body))
		req.Header.Set("X-GC-Request", "true")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s %s status = %d body=%s, want forbidden", attempt.method, attempt.path, rec.Code, rec.Body.String())
		}
	}

	stored, found, err := findPRActionRecord(fx.store, request.IdempotencyKey)
	if err != nil || !found || stored.ID != result.ID || stored.Status != PRActionStatusVerified {
		t.Fatalf("ordinary bead writes altered the verified action receipt: %+v found=%v err=%v", stored, found, err)
	}
	replay, err := fx.service.Execute(context.Background(), request, fx.workerActor())
	if err != nil || replay.ID != result.ID || replay.Status != PRActionStatusVerified {
		t.Fatalf("action replay after rejected writes = %+v err=%v", replay, err)
	}
}

func TestPRActionLedgerMetadataCannotBeCreatedThroughBeadAPI(t *testing.T) {
	fx := newPRActionFixture(t, true)
	handler := newTestCityHandler(t, fx.state)
	req := httptest.NewRequest(http.MethodPost, cityURL(fx.state, "/beads"), strings.NewReader(`{"rig":"myrig","title":"forged action","metadata":{"gc.pr_action.source":"api","gc.pr_action.record":"{}"}}`))
	req.Header.Set("X-GC-Request", "true")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("create forged ledger metadata status = %d body=%s, want forbidden", rec.Code, rec.Body.String())
	}
	if got := fx.actionRecordCount(); got != 0 {
		t.Fatalf("forged ledger create wrote %d action records", got)
	}
}
