package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gastownhall/gascity/internal/attemptevidence"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
)

func TestSessionRequestSubmitHTTPBindsAuthoritativeAttempt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fs := newSessionFakeState(t)
		info := createTestSession(t, fs.cityBeadStore, fs.sp, "bound target")
		front := session.NewStore(fs.SessionsBeadStore())
		info, err := front.Get(info.ID)
		if err != nil {
			t.Fatal(err)
		}
		generation, _ := strconv.Atoi(info.Generation)
		work, err := fs.cityBeadStore.Create(beads.Bead{Title: "bound work", Type: "task", Metadata: beads.StringMap{
			beadmeta.SessionIDMetadataKey: info.ID, beadmeta.ClaimGenerationMetadataKey: "http-claim",
		}})
		if err != nil {
			t.Fatal(err)
		}
		status := "in_progress"
		if err := fs.cityBeadStore.Update(work.ID, beads.UpdateOpts{Status: &status, Assignee: &info.ID}); err != nil {
			t.Fatal(err)
		}
		if _, err := front.SetCurrentClaim(info.ID, work.ID); err != nil {
			t.Fatal(err)
		}
		work, err = fs.cityBeadStore.Get(work.ID)
		if err != nil {
			t.Fatal(err)
		}
		h := newTestCityHandler(t, fs)
		response := httptest.NewRecorder()
		body, err := json.Marshal(map[string]any{"request_id": "http-bound", "generation": generation, "message": "report progress"})
		if err != nil {
			t.Fatal(err)
		}
		h.ServeHTTP(response, newPostRequest(cityURL(fs, "/session/"+info.ID+"/requests"), strings.NewReader(string(body))))
		if response.Code != http.StatusAccepted {
			t.Fatalf("submit=%d %s", response.Code, response.Body.String())
		}
		synctest.Wait()
		receipt, err := front.GetRequest(info.ID, "http-bound")
		if err != nil || receipt.Attempt == nil {
			t.Fatalf("missing binding: %+v %v", receipt, err)
		}
		binding := receipt.Attempt
		if binding.Identity.OwnerBeadID != work.ID || binding.Identity.SessionID != info.ID || binding.Identity.ClaimGeneration != "http-claim" || binding.StoreRef != "city:"+fs.CityName() || binding.WorkRevision != strconv.FormatInt(work.Revision, 10) {
			t.Fatalf("incorrect server attribution: %+v", binding)
		}
		if receipt.Delivery != session.RequestDeliveryAccepted || receipt.AcknowledgedAt != nil || receipt.Effect != "unverified" {
			t.Fatalf("delivery invented acknowledgement or effect: %+v", receipt)
		}
	})
}

func TestAttemptAcknowledgementsReportUnattributedAndCorruptReceipts(t *testing.T) {
	fs := newSessionFakeState(t)
	info := createTestSession(t, fs.cityBeadStore, fs.sp, "legacy target")
	front := session.NewStore(fs.SessionsBeadStore())
	info, err := front.Get(info.ID)
	if err != nil {
		t.Fatal(err)
	}
	generation, _ := strconv.Atoi(info.Generation)
	identity := attemptevidence.Identity{
		Kind: attemptevidence.KindWorkbench, OwnerBeadID: "original-work", ExecutionBeadID: "original-work",
		SessionID: info.ID, SessionGeneration: info.Generation, ClaimGeneration: "original-claim",
	}
	attemptID, err := attemptevidence.AttemptID(identity)
	if err != nil {
		t.Fatal(err)
	}
	evidence := attemptevidence.Evidence{Identity: identity, AttemptID: attemptID, StoreRef: "city:" + fs.CityName()}
	srv := New(fs)
	if result := srv.attemptAcknowledgements(evidence); result.Status != attemptevidence.StatusMissing {
		t.Fatalf("empty history=%+v", result)
	}
	if _, err := front.AcceptRequest(info.ID, "legacy", generation, "report", time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := front.AcknowledgeRequest(info.ID, "legacy", generation, info.InstanceToken, time.Now()); err != nil {
		t.Fatal(err)
	}
	result := srv.attemptAcknowledgements(evidence)
	if result.Status != attemptevidence.StatusUnavailable || result.UnattributedRequests != 1 || len(result.Records) != 0 {
		t.Fatalf("legacy acknowledgement inferred attribution: %+v", result)
	}
	// Simulate corrupt persisted evidence below the API boundary. It must not
	// become a successful empty history or an acknowledgement for this attempt.
	if err := fs.cityBeadStore.SetMetadata(info.ID, beadmeta.SessionRequestReceiptPrefix+"legacy", "malformed"); err != nil {
		t.Fatal(err)
	}
	if result := srv.attemptAcknowledgements(evidence); result.Status != attemptevidence.StatusUnavailable || result.Reason != "session_request_ledger_unavailable" || len(result.Records) != 0 {
		t.Fatalf("corrupt ledger hidden: %+v", result)
	}
}

func TestSessionRequestReceiptCannotBeForgedThroughGenericBeadUpdate(t *testing.T) {
	fs := newSessionFakeState(t)
	info := createTestSession(t, fs.cityBeadStore, fs.sp, "protected receipt")
	front := session.NewStore(fs.SessionsBeadStore())
	info, err := front.Get(info.ID)
	if err != nil {
		t.Fatal(err)
	}
	gen, _ := strconv.Atoi(info.Generation)
	if _, err := front.AcceptRequest(info.ID, "protected-request", gen, "report", time.Now()); err != nil {
		t.Fatal(err)
	}
	row, err := fs.cityBeadStore.Get(info.ID)
	if err != nil {
		t.Fatal(err)
	}
	key := beadmeta.SessionRequestReceiptPrefix + "protected-request"
	var forged map[string]any
	if err := json.Unmarshal([]byte(row.Metadata[key]), &forged); err != nil {
		t.Fatal(err)
	}
	forged["acknowledged_at"] = time.Now().UTC().Format(time.RFC3339Nano)
	raw, err := json.Marshal(forged)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(map[string]any{"metadata": map[string]string{key: string(raw)}})
	if err != nil {
		t.Fatal(err)
	}
	h := newTestCityHandler(t, fs)
	response := httptest.NewRecorder()
	req := newPostRequest(cityURL(fs, "/bead/"+info.ID+"/update"), strings.NewReader(string(body)))
	h.ServeHTTP(response, req)
	if response.Code < 400 {
		t.Fatalf("generic API forged acknowledgement: %d %s", response.Code, response.Body.String())
	}
	receipt, err := front.GetRequest(info.ID, "protected-request")
	if err != nil || receipt.AcknowledgedAt != nil {
		t.Fatalf("protected receipt changed: %+v %v", receipt, err)
	}
	response = httptest.NewRecorder()
	h.ServeHTTP(response, newPostRequest(cityURL(fs, "/beads"), strings.NewReader(`{"title":"forged session","type":"session","metadata":{"`+key+`":"forged"}}`)))
	if response.Code < 400 || response.Code == http.StatusNotFound {
		t.Fatalf("generic create must reject managed receipt metadata: %d %s", response.Code, response.Body.String())
	}
}

func TestSessionRequestAttemptResolutionUsesCurrentWorkAndClaim(t *testing.T) {
	fs := newSessionFakeState(t)
	info := createTestSession(t, fs.cityBeadStore, fs.sp, "attributed request")
	front := session.NewStore(fs.SessionsBeadStore())
	info, err := front.Get(info.ID)
	if err != nil {
		t.Fatal(err)
	}
	generation, _ := strconv.Atoi(info.Generation)
	work, err := fs.cityBeadStore.Create(beads.Bead{
		Title: "current work", Type: "task", Status: "in_progress", Assignee: info.ID,
		Metadata: beads.StringMap{beadmeta.SessionIDMetadataKey: info.ID, beadmeta.ClaimGenerationMetadataKey: "claim-one"},
	})
	if err != nil {
		t.Fatal(err)
	}
	status := "in_progress"
	if err := fs.cityBeadStore.Update(work.ID, beads.UpdateOpts{Status: &status, Assignee: &info.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := front.SetCurrentClaim(info.ID, work.ID); err != nil {
		t.Fatal(err)
	}
	srv := New(fs)
	binding, err := srv.resolveSessionRequestAttempt(info.ID, generation)
	if err != nil || binding == nil || binding.Identity.OwnerBeadID != work.ID || binding.Identity.ClaimGeneration != "claim-one" || binding.StoreRef != "city:"+fs.CityName() {
		t.Fatalf("controller binding=%+v err=%v", binding, err)
	}
	if _, err := srv.resolveSessionRequestAttempt(info.ID, generation+1); err == nil {
		t.Fatal("stale generation resolved")
	}
	if err := fs.cityBeadStore.SetMetadata(work.ID, beadmeta.SessionIDMetadataKey, "another-session"); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.resolveSessionRequestAttempt(info.ID, generation); err == nil {
		t.Fatal("unrelated work owner resolved")
	}
}

func TestAttemptAcknowledgementsRemainExactAfterOwnerDeletionAndSQLiteReopen(t *testing.T) {
	dir := t.TempDir()
	store, err := beads.OpenSQLiteStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	row, err := store.Create(beads.Bead{Type: "session", Labels: []string{session.LabelSession}, Metadata: beads.StringMap{
		"generation": "2", "instance_token": "private-execution-token", "state": "active",
	}})
	if err != nil {
		t.Fatal(err)
	}
	front := session.NewStore(beads.SessionStore{Store: store})
	fs := newFakeState(t)
	fs.cityBeadStore = store
	srv := New(fs)
	var first attemptevidence.Evidence
	for _, suffix := range []string{"first", "second"} {
		work, err := store.Create(beads.Bead{
			Type: "task", Title: suffix, Status: "in_progress", Assignee: row.ID,
			Metadata: beads.StringMap{beadmeta.SessionIDMetadataKey: row.ID, beadmeta.ClaimGenerationMetadataKey: suffix},
		})
		if err != nil {
			t.Fatal(err)
		}
		if err := store.SetMetadata(work.ID, "test_initialized", "true"); err != nil {
			t.Fatal(err)
		}
		if _, err := front.SetCurrentClaim(row.ID, work.ID); err != nil {
			t.Fatal(err)
		}
		binding, err := srv.resolveSessionRequestAttempt(row.ID, 2)
		if err != nil || binding == nil {
			t.Fatalf("binding=%+v err=%v", binding, err)
		}
		if _, err := front.AcceptRequestForAttempt(row.ID, "request-"+suffix, 2, "report progress", *binding, time.Now()); err != nil {
			t.Fatal(err)
		}
		if _, err := front.AcknowledgeRequest(row.ID, "request-"+suffix, 2, "private-execution-token", time.Now()); err != nil {
			t.Fatal(err)
		}
		captured, err := attemptevidence.Capture(context.Background(), store, attemptevidence.CaptureSpec{
			Identity: binding.Identity, StoreRef: binding.StoreRef,
			Permission: attemptevidence.PermissionScope{StoreRef: binding.StoreRef, WorkID: work.ID},
		})
		if err != nil {
			t.Fatal(err)
		}
		if suffix == "first" {
			first = captured
		}
		if err := store.Delete(work.ID); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.(interface{ CloseStore() error }).CloseStore(); err != nil {
		t.Fatal(err)
	}
	store, err = beads.OpenSQLiteStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.(interface{ CloseStore() error }).CloseStore() })
	fs.cityBeadStore = store
	archived, err := attemptevidence.Read(store, first.Identity.OwnerBeadID, first.AttemptID)
	if err != nil {
		t.Fatal(err)
	}
	result := New(fs).attemptAcknowledgements(archived)
	if result.Status != attemptevidence.StatusAvailable || len(result.Records) != 1 || result.Records[0].RequestID != "request-first" || result.Records[0].AcknowledgedAt == nil || result.Records[0].Effect != "unverified" {
		t.Fatalf("historical acknowledgement attribution=%+v", result)
	}
	archived.StoreRef = "rig:unrelated"
	if result := New(fs).attemptAcknowledgements(archived); result.Status != attemptevidence.StatusUnavailable || len(result.Records) != 0 {
		t.Fatalf("wrong original scope exposed receipts: %+v", result)
	}
}
