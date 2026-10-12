package beads

import (
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
)

func TestPrivatePayloadStockHTTPCreateAndRead(t *testing.T) {
	payload := protectedAttemptEvidencePayload()
	var created bool
	posts := 0
	listMode := "normal"
	store := newBdStoreWithPrivateEvidenceHTTPForTest(t, func(_, _ string, _ ...string) ([]byte, error) {
		t.Fatal("private payload reached CLI transport")
		return nil, errors.New("CLI refused")
	}, func(w http.ResponseWriter, r *http.Request) {
		writeRow := func() {
			_ = json.NewEncoder(w).Encode(map[string]any{"id": payload.ID, "title": payload.Title, "issue_type": payload.Type, "status": payload.Status, "metadata": payload.Metadata})
		}
		switch {
		case r.URL.Path == "/v0/beads/context":
			writePrivateEvidenceTestContext(w)
		case r.URL.Path == "/v0/beads/issues" && r.Method == http.MethodPost:
			var request struct {
				ID          string            `json:"id"`
				ForcePrefix bool              `json:"force_id_prefix"`
				Status      string            `json:"status"`
				Type        string            `json:"issue_type"`
				Metadata    map[string]string `json:"metadata"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Fatal(err)
			}
			if request.ID != payload.ID || !request.ForcePrefix || request.Status != "closed" || request.Type != "molecule" || !reflect.DeepEqual(request.Metadata, map[string]string(payload.Metadata)) {
				t.Fatal("create request changed the deterministic identity or private content")
			}
			posts++
			if created {
				w.WriteHeader(http.StatusConflict)
				return
			}
			created = true
			writeRow()
		case r.URL.Path == "/v0/beads/issues/"+payload.ID:
			if !created {
				http.NotFound(w, r)
				return
			}
			writeRow()
		case r.URL.Path == "/v0/beads/issues":
			rows := []any{}
			if created && r.URL.Query().Get("metadata_field") == beadmeta.AttemptEvidencePayloadDigestMetadataKey+"="+payload.Metadata[beadmeta.AttemptEvidencePayloadDigestMetadataKey] {
				if r.URL.Query().Get("all") != "true" {
					t.Fatal("payload lookup omitted closed rows")
				}
				row := map[string]any{"id": payload.ID, "issue_type": payload.Type, "status": payload.Status, "metadata": payload.Metadata}
				switch listMode {
				case "outside-filter":
					row["metadata"] = map[string]string{beadmeta.AttemptEvidencePayloadDigestMetadataKey: "different"}
				case "missing-pagination":
					_ = json.NewEncoder(w).Encode(map[string]any{"items": []any{row}})
					return
				case "cursor-cycle":
					_ = json.NewEncoder(w).Encode(map[string]any{"items": []any{row}, "has_more": true, "next_cursor": "same"})
					return
				case "second-page":
					if r.URL.Query().Get("cursor") == "" {
						_ = json.NewEncoder(w).Encode(map[string]any{"items": []any{row}, "has_more": true, "next_cursor": "next"})
						return
					}
					row["id"] = "historical-duplicate"
				}
				rows = append(rows, row)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"items": rows, "has_more": false})
		default:
			http.NotFound(w, r)
		}
	})
	if !SupportsPrivatePayloadValues(NewCachingStore(store, nil)) {
		t.Fatal("configured stock HTTP store does not support private content")
	}
	got, err := CreatePrivatePayloadValue(store, payload)
	if err != nil || got.ID != payload.ID || !reflect.DeepEqual(got.Metadata, payload.Metadata) {
		t.Fatalf("create failed: %v", err)
	}
	backend, ok := PrivatePayloadValueBackend(store)
	if !ok {
		t.Fatal("private backend unavailable")
	}
	got, err = backend.Get(payload.ID)
	if err != nil || !reflect.DeepEqual(got.Metadata, payload.Metadata) {
		t.Fatalf("exact HTTP read failed: %v", err)
	}
	for _, mode := range []string{"normal", "second-page", "outside-filter", "missing-pagination", "cursor-cycle"} {
		t.Run(mode, func(t *testing.T) {
			listMode = mode
			rows, err := ListPrivatePayloadValues(NewCachingStore(store, nil), payload.Metadata[beadmeta.AttemptEvidencePayloadDigestMetadataKey])
			if mode == "normal" || mode == "second-page" {
				want := 1
				if mode == "second-page" {
					want = 2
				}
				if err != nil || len(rows) != want {
					t.Fatalf("private list rows=%d, error=%v; want %d", len(rows), err, want)
				}
			} else if err == nil {
				t.Fatal("malformed or incomplete private list was accepted")
			}
		})
	}
	listMode = "normal"
	// A duplicate deterministic create may return a conflict. It must never
	// update the row or mint a different ID; capture resolves it by exact read.
	if _, err := CreatePrivatePayloadValue(store, payload); err == nil {
		t.Fatal("duplicate create did not report conflict")
	}
	if posts != 2 {
		t.Fatalf("create requests=%d, want 2", posts)
	}
}

func TestPrivatePayloadStockHTTPRejectsInvalidCreateResponse(t *testing.T) {
	for _, mode := range []string{"wrong-id", "wrong-content", "open", "ephemeral", "unavailable"} {
		t.Run(mode, func(t *testing.T) {
			payload := protectedAttemptEvidencePayload()
			posts := 0
			store := newBdStoreWithPrivateEvidenceHTTPForTest(t, func(_, _ string, _ ...string) ([]byte, error) {
				t.Fatal("failed private HTTP write fell back to CLI")
				return nil, errors.New("CLI refused")
			}, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v0/beads/context" {
					writePrivateEvidenceTestContext(w)
					return
				}
				if r.URL.Path == "/v0/beads/issues" && r.Method == http.MethodPost {
					posts++
					row := map[string]any{"id": payload.ID, "issue_type": payload.Type, "status": payload.Status, "metadata": payload.Metadata}
					switch mode {
					case "wrong-id":
						row["id"] = "gc-wrong"
					case "wrong-content":
						row["metadata"] = map[string]string{beadmeta.AttemptEvidencePayloadDigestMetadataKey: payload.Metadata[beadmeta.AttemptEvidencePayloadDigestMetadataKey], beadmeta.AttemptEvidencePayloadDataMetadataKey: "YWx0ZXJlZA=="}
					case "open":
						row["status"] = "open"
					case "ephemeral":
						row["ephemeral"] = true
					case "unavailable":
						w.WriteHeader(http.StatusServiceUnavailable)
						return
					}
					_ = json.NewEncoder(w).Encode(row)
					return
				}
				http.NotFound(w, r)
			})
			if _, err := store.CreatePrivatePayloadValue(payload); err == nil {
				t.Fatal("invalid create response was accepted")
			}
			if posts != 1 {
				t.Fatalf("POST attempts=%d, want exactly 1", posts)
			}
		})
	}
}
