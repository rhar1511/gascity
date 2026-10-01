//go:build cross_language_harness

package api

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/citywriteauth"
	"github.com/gastownhall/gascity/internal/clientgrant"
	"github.com/gastownhall/gascity/internal/decisionfrontier"
	"github.com/gastownhall/gascity/internal/retirementrelease"
	"github.com/gastownhall/gascity/internal/session"
)

// This wrapper preserves every qualified native backend port. Only the protected
// answer writer is intercepted, after reservation, before the immutable create.
// It follows interruptedHumanStore in decisionfrontier/human_domain_test.go.
type crossLanguageStore struct {
	*beads.MemStore
	interrupt bool
}

func (s *crossLanguageStore) StableCreateIDResolveTarget() beads.Store { return s.MemStore }
func (s *crossLanguageStore) DecisionFrontierRecordWriterHandle() (beads.DecisionFrontierRecordWriter, bool) {
	return s, true
}

func (s *crossLanguageStore) DecisionFrontierAtomicBackendHandle() (beads.DecisionFrontierAtomicBackend, bool) {
	return s.MemStore, true
}

func (s *crossLanguageStore) ConditionalWriterHandle() (beads.ConditionalWriter, bool) {
	return s.MemStore, true
}

func (s *crossLanguageStore) RevisionTransitionWriterHandle() (beads.RevisionTransitionWriter, bool) {
	return s.MemStore, true
}

func (s *crossLanguageStore) DecisionFrontierSourceReaderHandle() (beads.DecisionFrontierSourceReader, bool) {
	return s.MemStore, true
}

func (s *crossLanguageStore) RevisionTransitionReceiptReaderHandle() (beads.RevisionTransitionReceiptReader, bool) {
	return s.MemStore, true
}

func (s *crossLanguageStore) CreateDecisionFrontierRecord(b beads.Bead) (beads.Bead, error) {
	if s.interrupt && b.Metadata[beadmeta.DecisionFrontierRecordMetadataKey] == "decision-frontier/answer/v1" {
		s.interrupt = false
		return beads.Bead{}, errors.New("fixture interrupted protected answer create")
	}
	return s.MemStore.CreateDecisionFrontierRecord(b)
}

type crossLanguageState struct {
	*decisionFrontierState
	retirement *retirementrelease.Adapter
}

func (s *crossLanguageState) RetirementSourceAdapter() *retirementrelease.Adapter {
	return s.retirement
}

type crossLanguageInfo struct {
	BaseURL             string                    `json:"base_url"`
	City                string                    `json:"city"`
	WorkID              string                    `json:"work_id"`
	WorkRevision        string                    `json:"work_revision"`
	PhysicalRevision    string                    `json:"physical_revision"`
	CID                 string                    `json:"cid"`
	TargetName          string                    `json:"target_name"`
	SessionID           string                    `json:"session_id"`
	ExecutionGeneration int                       `json:"execution_generation"`
	ReadHeader          string                    `json:"read_header"`
	WriteHeader         string                    `json:"write_header"`
	ReadAudience        string                    `json:"read_audience"`
	WriteAudience       string                    `json:"write_audience"`
	GrantVersion        string                    `json:"grant_version"`
	GrantInfoFields     []string                  `json:"grant_info_fields"`
	Now                 time.Time                 `json:"now"`
	CityPublicKey       string                    `json:"city_public_key"`
	HumanPublicKey      string                    `json:"human_public_key"`
	Retirement          retirementrelease.Request `json:"retirement"`
}

type crossLanguageControl struct {
	TicketID   string `json:"ticket_id,omitempty"`
	Held       bool   `json:"held,omitempty"`
	Generation *int   `json:"generation,omitempty"`
}

type crossLanguageSnapshot struct {
	Rows         []beads.Bead                      `json:"rows"`
	Dependencies []beads.Dep                       `json:"dependencies"`
	Session      session.Info                      `json:"session"`
	Receipts     map[string]session.RequestReceipt `json:"session_receipts"`
	Messages     []string                          `json:"session_messages"`
}
type crossLanguageControlResult struct {
	Action string `json:"action"`
}

type crossLanguageHarness struct {
	fixture  *humanSourceHTTPFixture
	store    *crossLanguageStore
	state    *crossLanguageState
	info     crossLanguageInfo
	handler  http.Handler
	server   *http.Server
	client   *http.Client
	mu       sync.Mutex
	stop     chan struct{}
	stopOnce sync.Once
}

func newCrossLanguageHarness(t *testing.T) *crossLanguageHarness {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	f := newHumanSourceHTTPFixture(t)
	// Give the source a real pre-frontier edit. Revision 1 is then an actually
	// stale query for the consumer's signed-query tampering regression.
	title := "Native cross-language human source"
	if err := f.store.Update("wrk-frontier-api", beads.UpdateOpts{Title: &title}); err != nil {
		t.Fatal(err)
	}
	x := &crossLanguageHarness{fixture: f, store: &crossLanguageStore{MemStore: f.store}, stop: make(chan struct{}), client: &http.Client{Timeout: 5 * time.Second}}
	f.state.cityBeadStore = x.store
	request, adapter := crossLanguageRetirementFixture(t, f.now)
	x.state = &crossLanguageState{decisionFrontierState: f.wrapped, retirement: adapter}
	work, err := f.store.Get("wrk-frontier-api")
	if err != nil {
		t.Fatal(err)
	}
	revision, err := decisionfrontier.WorkRevision(work)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	x.info = crossLanguageInfo{
		BaseURL: "http://" + ln.Addr().String(), City: f.state.CityName(), WorkID: work.ID, WorkRevision: revision, Now: f.now,
		PhysicalRevision: strconv.FormatInt(work.Revision, 10), CID: "cross-language-fixture-tenant", TargetName: f.sessions.info.ConfiguredNamedIdentity, SessionID: f.sessions.info.ID, ExecutionGeneration: 7,
		ReadHeader: readAuthHeader, WriteHeader: writeAuthHeader, ReadAudience: readAuthAudience, WriteAudience: writeAuthAudience, GrantVersion: clientgrant.Version,
		GrantInfoFields: []string{"version", "aud", "city", "method", "path", "canonical_query", "body_sha256", "req_digest"},
		CityPublicKey:   base64.StdEncoding.EncodeToString(f.workerKey.Public().(ed25519.PublicKey)), HumanPublicKey: base64.StdEncoding.EncodeToString(f.humanKey.Public().(ed25519.PublicKey)), Retirement: request,
	}
	x.compose(t)
	x.server = &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		x.mu.Lock()
		defer x.mu.Unlock()
		if strings.HasPrefix(r.URL.Path, "/__fixture/") {
			x.control(t, w, r)
			return
		}
		x.handler.ServeHTTP(w, r)
	}), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second, IdleTimeout: 10 * time.Second}
	done := make(chan error, 1)
	go func() { done <- x.server.Serve(ln) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := x.server.Shutdown(ctx); err != nil {
			t.Error(err)
		}
		if err := <-done; !errors.Is(err, http.ErrServerClosed) {
			t.Errorf("fixture serve: %v", err)
		}
		x.client.CloseIdleConnections()
	})
	return x
}

func (x *crossLanguageHarness) compose(t *testing.T) {
	pub := x.fixture.workerKey.Public().(ed25519.PublicKey)
	verifier := func(audience string) *citywriteauth.Verifier {
		v, err := citywriteauth.New(citywriteauth.Options{Aud: audience, CID: x.info.CID, Keys: map[string]ed25519.PublicKey{"k1": pub}, MaxTTL: 2 * time.Minute, Skew: 30 * time.Second, Now: func() time.Time { return x.fixture.now }})
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	x.handler = readAuthMiddleware(verifier(readAuthAudience), writeAuthMiddleware(verifier(writeAuthAudience), false, newTestCityHandler(t, x.state)))
}

func (x *crossLanguageHarness) control(t *testing.T, w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method == http.MethodGet && r.URL.Path == "/__fixture/snapshot" {
		rows, err := x.store.List(beads.ListQuery{AllowScan: true, IncludeClosed: true, TierMode: beads.TierBoth})
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
		deps := []beads.Dep{}
		for _, row := range rows {
			edges, err := x.store.DepList(row.ID, "down")
			if err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			deps = append(deps, edges...)
		}
		sort.Slice(deps, func(i, j int) bool {
			a, b := deps[i], deps[j]
			if a.IssueID != b.IssueID {
				return a.IssueID < b.IssueID
			}
			if a.DependsOnID != b.DependsOnID {
				return a.DependsOnID < b.DependsOnID
			}
			return a.Type < b.Type
		})
		if err := json.NewEncoder(w).Encode(crossLanguageSnapshot{Rows: rows, Dependencies: deps, Session: x.fixture.sessions.info, Receipts: x.fixture.sessions.receipts, Messages: x.fixture.sessions.messages}); err != nil {
			t.Error(err)
		}
		return
	}
	if r.Method == http.MethodGet && r.URL.Path == "/__fixture/info" {
		row, err := x.store.Get(x.info.WorkID)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		x.info.PhysicalRevision = strconv.FormatInt(row.Revision, 10)
		if err := json.NewEncoder(w).Encode(x.info); err != nil {
			t.Error(err)
		}
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "fixture POST required", 405)
		return
	}
	var c *crossLanguageControl
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
	d.DisallowUnknownFields()
	var empty *struct{}
	var generation *struct {
		Generation *int `json:"generation"`
	}
	var target any = &c
	emptyControl := r.URL.Path == "/__fixture/source-change" || r.URL.Path == "/__fixture/recompose" || r.URL.Path == "/__fixture/stop" || r.URL.Path == "/__fixture/interrupt-next-answer"
	if emptyControl {
		target = &empty
	} else if r.URL.Path == "/__fixture/generation" {
		target = &generation
	}
	if err := d.Decode(target); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}
	if emptyControl && empty != nil {
		c = &crossLanguageControl{}
	} else if r.URL.Path == "/__fixture/generation" && generation != nil {
		c = &crossLanguageControl{Generation: generation.Generation}
	}
	if c == nil {
		http.Error(w, "fixture JSON object required", 400)
		return
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		http.Error(w, "one JSON object required", 400)
		return
	}
	var err error
	switch r.URL.Path {
	case "/__fixture/generation":
		if c.Generation == nil || *c.Generation <= 0 || c.TicketID != "" || c.Held {
			http.Error(w, "positive integer generation required", 400)
			return
		}
		generation := strconv.Itoa(*c.Generation)
		err = x.store.SetMetadata(x.fixture.sessions.info.ID, "generation", generation)
		if err == nil {
			x.fixture.sessions.info.Generation = generation
			x.info.ExecutionGeneration = *c.Generation
		}
	case "/__fixture/interrupt-next-answer":
		x.store.interrupt = true
	case "/__fixture/hold", "/__fixture/recompose", "/__fixture/source-change":
		if r.URL.Path == "/__fixture/source-change" && (c.Generation != nil || c.TicketID != "" || c.Held) {
			http.Error(w, "empty object required", 400)
			return
		}
		if r.URL.Path == "/__fixture/hold" {
			row, e := x.store.Get(c.TicketID)
			if e != nil || row.Metadata[beadmeta.DecisionFrontierRecordMetadataKey] != "decision-frontier/question/v1" {
				http.Error(w, "fixture question ticket required", 400)
				return
			}
		}
		// Reconstruct the native memory image using the same canonical row/dependency
		// codec as the human-domain held/recovery fixtures. No persistence is claimed.
		rows, e := x.store.List(beads.ListQuery{AllowScan: true, IncludeClosed: true, TierMode: beads.TierBoth})
		err = e
		var deps []beads.Dep
		if err == nil {
			for i := range rows {
				edges, e := x.store.DepList(rows[i].ID, "down")
				if e != nil {
					err = e
					break
				}
				deps = append(deps, edges...)
				if rows[i].ID == x.info.WorkID && r.URL.Path == "/__fixture/source-change" {
					// Fixture-only reconstruction models an independent source writer.
					// Generic Update correctly refuses a controller-managed source row.
					rows[i].Title += " (fixture source change)"
					rows[i].Revision++
					x.info.PhysicalRevision = strconv.FormatInt(rows[i].Revision, 10)
				}
				if rows[i].ID == c.TicketID && r.URL.Path == "/__fixture/hold" {
					labels := rows[i].Labels[:0]
					for _, label := range rows[i].Labels {
						if label != "hold:external" {
							labels = append(labels, label)
						}
					}
					rows[i].Labels = labels
					if c.Held {
						rows[i].Labels = append(rows[i].Labels, "hold:external")
					}
				}
			}
		}
		if err == nil {
			x.store.MemStore = beads.NewMemStoreFrom(1000, rows, deps)
			x.store.HonorExplicitIDs = true
			x.fixture.store = x.store.MemStore
			x.compose(t)
		}
	case "/__fixture/stop":
		x.stopOnce.Do(func() { close(x.stop) })
	default:
		http.NotFound(w, r)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	if err := json.NewEncoder(w).Encode(crossLanguageControlResult{Action: strings.TrimPrefix(r.URL.Path, "/__fixture/")}); err != nil {
		t.Error(err)
	}
}

func TestCrossLanguageHarnessServe(t *testing.T) {
	if os.Getenv("GC_CROSS_LANGUAGE_HARNESS") != "1" {
		t.Skip("set GC_CROSS_LANGUAGE_HARNESS=1 to launch disposable fixture")
	}
	x := newCrossLanguageHarness(t)
	b, err := json.Marshal(x.info)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Printf("GC_HARNESS_READY=%s\n", b)
	timer := time.NewTimer(120 * time.Second)
	defer timer.Stop()
	select {
	case <-x.stop:
	case <-timer.C:
		t.Fatal("fixture deadline exceeded; client must stop the harness")
	}
}

func (x *crossLanguageHarness) request(t *testing.T, method, tail, key string, body any, authenticated bool) (int, []byte) {
	t.Helper()
	var b []byte
	var err error
	if body != nil {
		b, err = json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
	}
	path := tail
	if !strings.HasPrefix(tail, "/__fixture/") {
		path = cityURL(x.fixture.state, tail)
	}
	r, err := http.NewRequest(method, x.info.BaseURL+path, bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set(csrfHeaderName, "true")
	if key != "" {
		r.Header.Set("Idempotency-Key", key)
	}
	if authenticated {
		x.fixture.sequence++
		jti := fmt.Sprintf("socket-%d", x.fixture.sequence)
		if method == http.MethodGet {
			g := readGrant(x.fixture.now, x.info.City, method, r.URL.Path, r.URL.RawQuery, jti)
			g.CID = x.info.CID
			r.Header.Set(readAuthHeader, mintToken(t, x.fixture.workerKey, g))
		} else {
			g := grantForQuery(x.fixture.now, x.info.City, method, r.URL.Path, r.URL.RawQuery, b, jti)
			g.CID = x.info.CID
			r.Header.Set(writeAuthHeader, mintToken(t, x.fixture.workerKey, g))
		}
	}
	res, err := x.client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, 2<<20))
	if err != nil {
		t.Fatal(err)
	}
	return res.StatusCode, data
}

func crossLanguageDecode[T any](t *testing.T, status int, b []byte) T {
	t.Helper()
	var v T
	if status != 200 {
		t.Fatalf("socket HTTP %d: %s", status, b)
	}
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	return v
}
