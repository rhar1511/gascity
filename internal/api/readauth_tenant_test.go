package api

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

func TestProductionReadAuthInheritsControllerTenantAndRejectsForeignCID(t *testing.T) {
	key := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{99}, ed25519.SeedSize))
	t.Setenv("GC_CITY_READ_PUBKEY", "k1:"+base64.StdEncoding.EncodeToString(key.Public().(ed25519.PublicKey)))
	t.Setenv("GC_CITY_READ_REQUIRED", "1")
	t.Setenv("GC_CITY_READ_EPOCH_FLOOR", "")
	t.Setenv("GC_CITY_READ_CID", "")
	t.Setenv("GC_CITY_WRITE_CID", "fixture-tenant-city")
	v, err := ResolveReadAuthVerifier("", true)
	if err != nil {
		t.Fatal(err)
	}
	called := 0
	h := readAuthMiddleware(v, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called++
		principal, ok := verifiedCityReadPrincipal(r.Context())
		if !ok || principal.CID != "fixture-tenant-city" {
			t.Errorf("wrong authenticated tenant: %+v", principal)
		}
		w.WriteHeader(http.StatusOK)
	}))
	for i, cid := range []string{"foreign-tenant-city", "", "fixture-tenant-city"} {
		path := "/v0/city/same-name/beads"
		g := readGrant(time.Now(), "same-name", http.MethodGet, path, "", "tenant-case-"+strconv.Itoa(i))
		g.CID = cid
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.Header.Set(readAuthHeader, mintToken(t, key, g))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		want := http.StatusForbidden
		if cid == "fixture-tenant-city" {
			want = http.StatusOK
		}
		if rec.Code != want {
			t.Fatalf("CID=%q status=%d want=%d", cid, rec.Code, want)
		}
	}
	if called != 1 {
		t.Fatalf("foreign/missing CID reached generic handler: %d calls", called)
	}
	t.Setenv("GC_CITY_READ_CID", "conflicting-tenant")
	if _, err := ResolveReadAuthVerifier("", true); err == nil {
		t.Fatal("conflicting controller tenancy identities accepted")
	}
}
