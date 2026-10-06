package api

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/attemptevidence"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

type attemptEvidenceAuthorizerFunc func(context.Context, attemptevidence.ReadAuthorizationRequest) error

func (f attemptEvidenceAuthorizerFunc) AuthorizeAttemptEvidenceRead(ctx context.Context, request attemptevidence.ReadAuthorizationRequest) error {
	return f(ctx, request)
}

func TestAttemptEvidencePublicReadRequiresExactScopeAuthorization(t *testing.T) {
	state := newFakeState(t)
	store, err := beads.OpenFileStore(fsys.OSFS{}, filepath.Join(t.TempDir(), "beads.json"))
	if err != nil {
		t.Fatalf("OpenFileStore: %v", err)
	}
	state.cityBeadStore = store
	owner, err := store.Create(beads.Bead{Title: "work item", Type: "task"})
	if err != nil {
		t.Fatalf("Create owner: %v", err)
	}
	identity := attemptevidence.Identity{Kind: attemptevidence.KindRetry, OwnerBeadID: owner.ID, ExecutionBeadID: "attempt-row"}
	attemptID, err := attemptevidence.AttemptID(identity)
	if err != nil {
		t.Fatalf("AttemptID: %v", err)
	}
	spec := attemptevidence.CaptureSpec{
		Identity: identity,
		StoreRef: "city:test-city",
		Permission: attemptevidence.PermissionScope{
			StoreRef: "city:test-city", WorkID: owner.ID,
			RepositoryRoot: "/private/repository", WorkspaceRoot: "/private/repository/worktrees/attempt-row",
		},
	}
	evidence := attemptevidence.MakeUnavailable(spec, attemptID, "source_absent_before_capture")
	artifactContent := []byte("private exact-attempt stdout")
	evidence.Artifacts = []attemptevidence.PayloadArtifact{{
		Name: "stdout", MediaType: "text/plain", Status: attemptevidence.StatusAvailable, Content: artifactContent,
	}}
	sealed, err := attemptevidence.Seal(store, evidence)
	if err != nil {
		t.Fatalf("Seal evidence: %v", err)
	}
	artifactDigest := sealed.Artifacts[0].SHA256
	if err := store.Delete(owner.ID); err != nil {
		t.Fatalf("Delete owner after durable archive: %v", err)
	}

	requestPath := cityURL(state, "/bead/"+owner.ID+"/attempt-evidence/"+attemptID)
	w := httptest.NewRecorder()
	newTestCityHandler(t, state).ServeHTTP(w, httptest.NewRequest(http.MethodGet, requestPath, nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("city-only read status = %d, want 503 when no exact-scope authorizer is composed: %s", w.Code, w.Body.String())
	}

	var authorized attemptevidence.ReadAuthorizationRequest
	authorizer := attemptEvidenceAuthorizerFunc(func(_ context.Context, request attemptevidence.ReadAuthorizationRequest) error {
		authorized = request
		if request.Scope.StoreRef != "city:test-city" || request.Scope.WorkID != owner.ID ||
			request.Scope.RepositoryRoot != "/private/repository" ||
			request.Scope.WorkspaceRoot != "/private/repository/worktrees/attempt-row" ||
			request.AttemptID != attemptID {
			return ErrAttemptEvidenceReadDenied
		}
		return nil
	})
	srv := New(state)
	srv.attemptEvidenceReadAuthorizer = authorizer
	handler := newTestCityHandlerWith(t, state, srv)
	// The default fake rig has no private archive reader. A found city archive
	// cannot make that incomplete search authoritative, even with a read grant.
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, requestPath, nil))
	if w.Code != http.StatusServiceUnavailable || authorized.AttemptID != "" {
		t.Fatalf("incomplete archive search status = %d, authorized attempt = %q; want 503 before authorization", w.Code, authorized.AttemptID)
	}
	if strings.Contains(w.Body.String(), spec.Permission.RepositoryRoot) {
		t.Fatal("incomplete archive search exposed retained repository scope")
	}
	rigStore, err := beads.OpenFileStore(fsys.OSFS{}, filepath.Join(t.TempDir(), "rig-beads.json"))
	if err != nil {
		t.Fatalf("OpenFileStore rig: %v", err)
	}
	state.stores["myrig"] = rigStore
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, requestPath, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("authorized exact attempt read status = %d, want 200: %s", w.Code, w.Body.String())
	}
	if authorized.AttemptID != attemptID || authorized.Scope.WorkID != owner.ID {
		t.Fatalf("authorizer received request %+v, want exact archive scope", authorized)
	}
	var response attemptevidence.Evidence
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode attempt response: %v", err)
	}
	if response.AttemptID != attemptID || response.Permission.RepositoryRoot != "/private/repository" {
		t.Fatalf("attempt body = %+v, want exact persisted attempt provenance; raw=%s", response, w.Body.String())
	}
	if strings.Contains(w.Body.String(), string(artifactContent)) {
		t.Fatal("exact attempt metadata response embedded raw artifact content")
	}

	artifactPath := cityURL(state, "/bead/"+owner.ID+"/attempt-evidence/"+attemptID+"/artifact/"+artifactDigest)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, artifactPath, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("authorized artifact read status = %d, want 200: %s", w.Code, w.Body.String())
	}
	var payload attemptevidence.PayloadRead
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode artifact response: %v", err)
	}
	if payload.Status != attemptevidence.StatusAvailable || payload.SHA256 != artifactDigest || string(payload.Content) != string(artifactContent) {
		t.Fatalf("artifact response = %#v, want exact verified content", payload)
	}

	srv.attemptEvidenceReadAuthorizer = attemptEvidenceAuthorizerFunc(func(context.Context, attemptevidence.ReadAuthorizationRequest) error {
		return ErrAttemptEvidenceReadDenied
	})
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, artifactPath, nil))
	if w.Code != http.StatusForbidden {
		t.Fatalf("denied artifact read status = %d, want 403: %s", w.Code, w.Body.String())
	}
	srv.attemptEvidenceReadAuthorizer = authorizer
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, cityURL(state, "/bead/"+owner.ID+"/attempt-evidence/"+attemptID+"/artifact/"+strings.Repeat("f", 64)), nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("unreferenced artifact read status = %d, want 404: %s", w.Code, w.Body.String())
	}
}

func TestAttemptEvidenceReadDenialAndListUseStoredPermissionScope(t *testing.T) {
	state := newFakeState(t)
	store, err := beads.OpenFileStore(fsys.OSFS{}, filepath.Join(t.TempDir(), "beads.json"))
	if err != nil {
		t.Fatalf("OpenFileStore: %v", err)
	}
	state.cityBeadStore = store
	rigStore, err := beads.OpenFileStore(fsys.OSFS{}, filepath.Join(t.TempDir(), "rig-beads.json"))
	if err != nil {
		t.Fatalf("OpenFileStore rig: %v", err)
	}
	state.stores["myrig"] = rigStore
	owner, err := store.Create(beads.Bead{Title: "work item", Type: "task"})
	if err != nil {
		t.Fatalf("Create owner: %v", err)
	}
	identity := attemptevidence.Identity{Kind: attemptevidence.KindRetry, OwnerBeadID: owner.ID, ExecutionBeadID: "attempt-row"}
	attemptID, err := attemptevidence.AttemptID(identity)
	if err != nil {
		t.Fatalf("AttemptID: %v", err)
	}
	spec := attemptevidence.CaptureSpec{
		Identity: identity,
		StoreRef: "city:test-city",
		Permission: attemptevidence.PermissionScope{
			StoreRef: "city:test-city", WorkID: owner.ID,
			RepositoryRoot: "/repo", WorkspaceRoot: "/repo/worktree",
		},
	}
	if _, err := attemptevidence.Seal(store, attemptevidence.MakeUnavailable(spec, attemptID, "source_absent_before_capture")); err != nil {
		t.Fatalf("Seal evidence: %v", err)
	}
	deny := attemptEvidenceAuthorizerFunc(func(_ context.Context, request attemptevidence.ReadAuthorizationRequest) error {
		if request.AttemptID != attemptID || request.Scope.WorkID != owner.ID || request.Scope.RepositoryRoot != "/repo" {
			t.Fatalf("authorizer saw unexpected scope: %+v", request)
		}
		return ErrAttemptEvidenceReadDenied
	})
	srv := New(state)
	srv.attemptEvidenceReadAuthorizer = deny
	handler := newTestCityHandlerWith(t, state, srv)

	for _, path := range []string{
		"/bead/" + owner.ID + "/attempt-evidence/" + attemptID,
		"/bead/" + owner.ID + "/attempt-evidence",
	} {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, cityURL(state, path), nil))
		if w.Code != http.StatusForbidden {
			t.Fatalf("denied read %s status = %d, want 403: %s", path, w.Code, w.Body.String())
		}
	}

	badStore := attemptEvidenceAuthorizerFunc(func(_ context.Context, _ attemptevidence.ReadAuthorizationRequest) error {
		return errors.New("permission provider unavailable")
	})
	srv.attemptEvidenceReadAuthorizer = badStore
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, cityURL(state, "/bead/"+owner.ID+"/attempt-evidence/"+attemptID), nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("authorization provider error status = %d, want 503: %s", w.Code, w.Body.String())
	}
}

func TestAttemptEvidenceSearchUsesRigLegAndSeparatesWrongStoreCopiesFromAuthoritativeDuplicates(t *testing.T) {
	openStore := func(path string) *beads.FileStore {
		t.Helper()
		store, err := beads.OpenFileStore(fsys.OSFS{}, path)
		if err != nil {
			t.Fatalf("OpenFileStore(%s): %v", path, err)
		}
		return store
	}
	cityStore := openStore(filepath.Join(t.TempDir(), "city-beads.json"))
	rigStore := openStore(filepath.Join(t.TempDir(), "rig-beads.json"))
	cityStore.HonorExplicitIDs = true
	rigStore.HonorExplicitIDs = true
	state := newFakeState(t)
	state.cityBeadStore = cityStore
	state.cfg.Rigs = []config.Rig{{Name: "blue", Path: filepath.Join(state.cityPath, "rig-blue")}}
	state.stores = map[string]beads.Store{"blue": rigStore}

	const ownerID = "gw-archive-owner"
	identity := attemptevidence.Identity{Kind: attemptevidence.KindRetry, OwnerBeadID: ownerID, ExecutionBeadID: "gw-attempt-1"}
	attemptID, err := attemptevidence.AttemptID(identity)
	if err != nil {
		t.Fatalf("AttemptID: %v", err)
	}
	createOwner := func(store beads.Store) {
		t.Helper()
		created, err := store.Create(beads.Bead{ID: ownerID, Title: "work", Type: "task"})
		if err != nil {
			t.Fatalf("create owner in %T: %v", store, err)
		}
		if created.ID != ownerID {
			t.Fatalf("store minted owner ID %q; test requires the same pinned owner ID %q in both isolated stores", created.ID, ownerID)
		}
	}
	makeEvidence := func(storeRef string) attemptevidence.Evidence {
		return attemptevidence.MakeUnavailable(attemptevidence.CaptureSpec{
			Identity: identity,
			StoreRef: storeRef,
			Permission: attemptevidence.PermissionScope{
				StoreRef: storeRef, WorkID: ownerID,
				RepositoryRoot: "/private/blue", WorkspaceRoot: "/private/blue/worktrees/attempt-1",
			},
		}, attemptID, "source_absent_before_capture")
	}

	// A row physically copied into the city store but labeled with a rig scope
	// cannot satisfy a rig lookup. The owner ID and attempt ID alone do not
	// authorize a cross-store substitute.
	createOwner(cityStore)
	if _, err := attemptevidence.Seal(cityStore, makeEvidence("rig:blue")); err != nil {
		t.Fatalf("seal wrong-store copy: %v", err)
	}
	srv := New(state)
	if _, err := srv.exactAttemptEvidence(context.Background(), ownerID, attemptID); err == nil {
		t.Fatal("wrong-store archive copy satisfied the rig-scoped lookup")
	}

	// The exact same owner/attempt identity archived in its configured rig leg
	// is found there. The city copy has a known foreign StoreRef, so it is
	// ignored rather than merged or chosen.
	createOwner(rigStore)
	if _, err := attemptevidence.Seal(rigStore, makeEvidence("rig:blue")); err != nil {
		t.Fatalf("seal rig archive: %v", err)
	}
	got, err := srv.exactAttemptEvidence(context.Background(), ownerID, attemptID)
	if err != nil {
		t.Fatalf("read rig archive: %v", err)
	}
	if got.StoreRef != "rig:blue" || got.AttemptID != attemptID {
		t.Fatalf("archive = store %q attempt %q, want rig:blue/%s", got.StoreRef, got.AttemptID, attemptID)
	}
	listed, err := srv.listAttemptEvidence(context.Background(), ownerID)
	if err != nil || len(listed) != 1 || listed[0].StoreRef != "rig:blue" {
		t.Fatalf("list = %+v, error %v; want only the authoritative rig archive", listed, err)
	}

	// A separate city backend with its own correctly scoped copy is not a
	// foreign mislabeled row. It is a real cross-store conflict and must fail.
	cityConflictStore := openStore(filepath.Join(t.TempDir(), "city-conflict-beads.json"))
	cityConflictStore.HonorExplicitIDs = true
	createOwner(cityConflictStore)
	state.cityBeadStore = cityConflictStore
	if _, err := attemptevidence.Seal(cityConflictStore, makeEvidence("city:test-city")); err != nil {
		t.Fatalf("seal authoritative city duplicate: %v", err)
	}
	if _, err := srv.exactAttemptEvidence(context.Background(), ownerID, attemptID); err == nil || !strings.Contains(err.Error(), "attempt evidence exists in multiple stores") {
		t.Fatalf("authoritative cross-store duplicate error = %v, want explicit conflict", err)
	}
	if _, err := srv.listAttemptEvidence(context.Background(), ownerID); err == nil || !strings.Contains(err.Error(), "attempt evidence exists in multiple stores") {
		t.Fatalf("authoritative cross-store duplicate list error = %v, want explicit conflict", err)
	}
}

func TestGenericBeadReadsHideAttemptEvidencePayloadAndArchiveRows(t *testing.T) {
	state := newFakeState(t)
	store, err := beads.OpenFileStore(fsys.OSFS{}, filepath.Join(t.TempDir(), "beads.json"))
	if err != nil {
		t.Fatalf("OpenFileStore: %v", err)
	}
	state.cityBeadStore = store
	state.stores = map[string]beads.Store{"myrig": store}
	owner, err := store.Create(beads.Bead{Title: "work item", Type: "task"})
	if err != nil {
		t.Fatalf("Create owner: %v", err)
	}
	identity := attemptevidence.Identity{Kind: attemptevidence.KindRetry, OwnerBeadID: owner.ID, ExecutionBeadID: "attempt-row"}
	attemptID, err := attemptevidence.AttemptID(identity)
	if err != nil {
		t.Fatalf("AttemptID: %v", err)
	}
	spec := attemptevidence.CaptureSpec{
		Identity: identity,
		StoreRef: "city:test-city",
		Permission: attemptevidence.PermissionScope{
			StoreRef: "city:test-city", WorkID: owner.ID,
			RepositoryRoot: "/private/repository", WorkspaceRoot: "/private/repository/worktree",
		},
	}
	if _, err := attemptevidence.Seal(store, attemptevidence.MakeUnavailable(spec, attemptID, "source_absent_before_capture")); err != nil {
		t.Fatalf("Seal evidence: %v", err)
	}
	archives, err := store.ListByMetadata(map[string]string{
		beadmeta.AttemptEvidenceArchiveOwnerIDMetadataKey: owner.ID,
	}, 0, beads.IncludeClosed)
	if err != nil || len(archives) != 1 {
		t.Fatalf("list archive rows = %d, error %v", len(archives), err)
	}
	payloadContent := []byte("secret")
	payloadDigest := fmt.Sprintf("%x", sha256.Sum256(payloadContent))
	payload, err := beads.CreatePrivatePayloadValue(store, beads.Bead{
		ID:    beads.AttemptEvidencePayloadID(payloadDigest),
		Title: "private content payload", Type: "molecule", Status: "closed",
		Metadata: beads.StringMap{
			beadmeta.AttemptEvidencePayloadDigestMetadataKey: payloadDigest,
			beadmeta.AttemptEvidencePayloadDataMetadataKey:   base64.StdEncoding.EncodeToString(payloadContent),
		},
	})
	if err != nil {
		t.Fatalf("Create payload: %v", err)
	}
	if err := store.SetMetadata(owner.ID, beadmeta.SessionRequestReceiptPrefix+"private-request", `{"message_digest":"private"}`); err != nil {
		t.Fatalf("SetMetadata request receipt: %v", err)
	}

	srv := New(state)
	ownerOutput, err := srv.humaHandleBeadGet(context.Background(), &BeadGetInput{ID: owner.ID})
	if err != nil {
		t.Fatalf("generic owner GET: %v", err)
	}
	for key := range ownerOutput.Body.Metadata {
		if isPrivateGenericBeadMetadataKey(key) {
			t.Fatalf("generic owner GET exposed reserved attempt metadata key %q", key)
		}
	}
	archiveOutput, err := srv.humaHandleBeadGet(context.Background(), &BeadGetInput{ID: archives[0].ID})
	if err == nil || archiveOutput != nil {
		t.Fatal("generic bead GET exposed the private archive row")
	}
	if payloadOutput, err := srv.humaHandleBeadGet(context.Background(), &BeadGetInput{ID: payload.ID}); err == nil || payloadOutput != nil {
		t.Fatal("generic bead GET exposed the private content payload row")
	}
	listOutput, err := srv.humaHandleBeadList(context.Background(), &BeadListInput{All: true, Type: "molecule"})
	if err != nil {
		t.Fatalf("generic bead list: %v", err)
	}
	for _, b := range listOutput.Body.Items {
		if beads.IsProtectedAttemptEvidenceRecord(b) || b.ID == archives[0].ID || b.ID == payload.ID {
			t.Fatalf("generic bead list exposed protected attempt evidence row %+v", b)
		}
		for key := range b.Metadata {
			if isPrivateGenericBeadMetadataKey(key) {
				t.Fatalf("generic bead list exposed reserved attempt metadata key %q", key)
			}
		}
	}
}
