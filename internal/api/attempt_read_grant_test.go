package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/attemptevidence"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/citywriteauth"
	"github.com/gastownhall/gascity/internal/fsys"
)

func TestAttemptReadGrantRequiresSignedReaderAndOriginalScope(t *testing.T) {
	state := newFakeState(t)
	store, err := beads.OpenFileStore(fsys.OSFS{}, filepath.Join(t.TempDir(), "beads.json"))
	if err != nil {
		t.Fatal(err)
	}
	state.cityBeadStore = store
	work, err := store.Create(beads.Bead{Title: "private work", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	spec := attemptevidence.CaptureSpec{
		Identity: attemptevidence.Identity{Kind: attemptevidence.KindRetry, OwnerBeadID: work.ID, ExecutionBeadID: "execution-one"},
		StoreRef: "city:" + state.CityName(),
		Permission: attemptevidence.PermissionScope{
			StoreRef: "city:" + state.CityName(), WorkID: work.ID,
			RepositoryRoot: "/private/original", WorkspaceRoot: "/private/original/worktrees/execution-one",
		},
	}
	id, err := attemptevidence.AttemptID(spec.Identity)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := attemptevidence.Seal(store, attemptevidence.MakeUnavailable(spec, id, "source_absent_before_capture")); err != nil {
		t.Fatal(err)
	}
	scopeRequest := attemptevidence.ReadAuthorizationRequest{Scope: spec.Permission, AttemptID: id}
	scope, err := attemptevidence.ReadGrantScope(scopeRequest)
	if err != nil {
		t.Fatal(err)
	}
	spec.Identity.ExecutionBeadID = "execution-two"
	secondID, err := attemptevidence.AttemptID(spec.Identity)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := attemptevidence.Seal(store, attemptevidence.MakeUnavailable(spec, secondID, "source_absent_before_capture")); err != nil {
		t.Fatal(err)
	}
	secondScope, err := attemptevidence.ReadGrantScope(attemptevidence.ReadAuthorizationRequest{Scope: spec.Permission, AttemptID: secondID})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(work.ID); err != nil {
		t.Fatal(err)
	}
	path := cityURL(state, "/bead/"+work.ID+"/attempt-evidence/"+id)
	now := time.Unix(1_700_000_000, 0)
	pub, priv := mustKeypair(t)
	tests := []struct {
		name   string
		change func(*citywriteauth.Grant)
		want   int
	}{
		{name: "exact historical grant", want: http.StatusOK},
		{name: "city access alone", change: func(g *citywriteauth.Grant) { g.ReadScopes = nil }, want: http.StatusForbidden},
		{name: "no authenticated reader", change: func(g *citywriteauth.Grant) { g.Subject = "" }, want: http.StatusForbidden},
		{name: "different repository", change: func(g *citywriteauth.Grant) {
			request := scopeRequest
			request.Scope.RepositoryRoot = "/private/replacement"
			g.ReadScopes[0], err = attemptevidence.ReadGrantScope(request)
			if err != nil {
				t.Fatal(err)
			}
		}, want: http.StatusForbidden},
		{name: "expired grant", change: func(g *citywriteauth.Grant) { g.IAT -= 300; g.Exp -= 300 }, want: http.StatusForbidden},
		{name: "different request", change: func(g *citywriteauth.Grant) { g.Req = citywriteauth.ReqDigest(http.MethodGet, path, "other=1", nil) }, want: http.StatusForbidden},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			grant := readGrant(now, state.CityName(), http.MethodGet, path, "", tc.name)
			grant.Subject = "reader-account"
			grant.ReadScopes = []string{scope}
			if tc.change != nil {
				tc.change(&grant)
			}
			sm := NewSupervisorMux(&stateCityResolver{state: state}, nil, false, "test", "", now)
			sm.WithReadAuth(newTestReadVerifier(t, pub, now))
			handler := wrapTestSupervisorMiddleware(sm)
			request := httptest.NewRequest(http.MethodGet, path, nil)
			request.Header.Set(readAuthHeader, mintToken(t, priv, grant))
			request.Header.Set("X-GC-Read-Actor", "forged-reader")
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != tc.want {
				t.Fatalf("status=%d want=%d body=%s", response.Code, tc.want, response.Body.String())
			}
			if tc.want == http.StatusOK {
				var evidence attemptevidence.Evidence
				if err := json.Unmarshal(response.Body.Bytes(), &evidence); err != nil {
					t.Fatal(err)
				}
				if evidence.AttemptID != id || evidence.Permission != spec.Permission {
					t.Fatalf("wrong historical scope: %+v", evidence)
				}
				response = httptest.NewRecorder()
				handler.ServeHTTP(response, request)
				if response.Code != http.StatusForbidden {
					t.Fatalf("replayed grant status=%d", response.Code)
				}
			}
		})
	}

	// A list is returned only when the same authenticated request covers every
	// exact retained attempt. One permitted row cannot expose a sibling.
	listPath := cityURL(state, "/bead/"+work.ID+"/attempt-evidence")
	for _, all := range []bool{false, true} {
		grant := readGrant(now, state.CityName(), http.MethodGet, listPath, "", "list-test")
		grant.Subject = "reader-account"
		grant.ReadScopes = []string{scope}
		want := http.StatusForbidden
		if all {
			grant.ReadScopes = append(grant.ReadScopes, secondScope)
			want = http.StatusOK
		}
		sm := NewSupervisorMux(&stateCityResolver{state: state}, nil, false, "test", "", now)
		sm.WithReadAuth(newTestReadVerifier(t, pub, now))
		request := httptest.NewRequest(http.MethodGet, listPath, nil)
		request.Header.Set(readAuthHeader, mintToken(t, priv, grant))
		response := httptest.NewRecorder()
		wrapTestSupervisorMiddleware(sm).ServeHTTP(response, request)
		if response.Code != want {
			t.Fatalf("all scopes=%v status=%d want=%d body=%s", all, response.Code, want, response.Body.String())
		}
		if all {
			var rows []attemptevidence.Evidence
			if err := json.Unmarshal(response.Body.Bytes(), &rows); err != nil || len(rows) != 2 {
				t.Fatalf("list rows=%d err=%v", len(rows), err)
			}
		}
	}
}
