package attemptevidence

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

const installedPrivateEvidenceProbeEnv = "GC_PRIVATE_EVIDENCE_INSTALLED_PROBE"

type installedPrivateEvidenceFixture struct {
	endpoint  string
	projectID string
	database  string
	scopeRef  string
	tokenFile string
	workspace string
	root      string
	bdBinary  string
	command   *installedPrivateEvidenceCommandRunner
}

type installedPrivateEvidenceCommandRunner struct {
	delegate     beads.CommandRunner
	forbidden    []string
	mu           sync.Mutex
	calls        int
	violated     bool
	failAfterArm bool
}

func (r *installedPrivateEvidenceCommandRunner) Run(dir, name string, args ...string) ([]byte, error) {
	r.mu.Lock()
	r.calls++
	for _, arg := range args {
		for _, forbidden := range r.forbidden {
			if forbidden != "" && strings.Contains(arg, forbidden) {
				r.violated = true
				r.mu.Unlock()
				return nil, errors.New("private evidence reached the bd command line")
			}
		}
	}
	if r.failAfterArm {
		r.violated = true
		r.mu.Unlock()
		return nil, errors.New("private evidence adapter invoked the bd command runner")
	}
	r.mu.Unlock()
	return r.delegate(dir, name, args...)
}

func (r *installedPrivateEvidenceCommandRunner) arm() {
	r.mu.Lock()
	r.calls = 0
	r.violated = false
	r.failAfterArm = true
	r.mu.Unlock()
}

func (r *installedPrivateEvidenceCommandRunner) assertNoCalls(t *testing.T) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.violated {
		t.Fatal("private evidence or its key reached the bd command line")
	}
	if r.calls != 0 {
		t.Fatalf("private evidence adapter invoked the bd command runner %d times", r.calls)
	}
}

func TestInstalledBdPrivateEvidenceAdapter(t *testing.T) {
	if os.Getenv(installedPrivateEvidenceProbeEnv) != "1" {
		t.Skip("set GC_PRIVATE_EVIDENCE_INSTALLED_PROBE=1 through the disposable bd serve fixture")
	}
	fixture := readInstalledPrivateEvidenceFixture(t)
	token, err := os.ReadFile(fixture.tokenFile)
	if err != nil {
		t.Fatalf("read disposable controller token: %v", err)
	}
	tokenValue := strings.TrimSpace(string(token))
	if len(tokenValue) < 16 {
		t.Fatal("disposable controller token is too short")
	}
	privateMarker := "private-attempt-evidence-" + tokenValue[:16]
	commandEnv := map[string]string{
		"HOME":            filepath.Join(fixture.root, "home"),
		"XDG_CONFIG_HOME": filepath.Join(fixture.root, "config"),
		"DOLT_ROOT_PATH":  filepath.Join(fixture.root, "dolt-home"),
		"BEADS_DIR":       filepath.Join(fixture.workspace, ".beads"),
		"BD_BIN":          fixture.bdBinary,
		"BD_EXPORT_AUTO":  "false",
		"PATH":            os.Getenv("PATH"),
		"GOMAXPROCS":      "2",
	}
	fixture.command = &installedPrivateEvidenceCommandRunner{
		delegate: beads.ExecCommandRunnerWithExactEnvContext(context.Background(), commandEnv),
		forbidden: []string{
			privateMarker,
			beadmeta.AttemptEvidenceArchivePayloadMetadataKey,
			beadmeta.AttemptEvidenceIndexPrefix,
			tokenValue,
		},
	}

	newStore := func(endpoint string) *beads.BdStore {
		return beads.NewBdStoreWithPrefix(fixture.workspace, fixture.command.Run, "probe",
			beads.WithBdStorePrivateEvidenceHTTP(beads.PrivateEvidenceHTTPConfig{
				Endpoint: endpoint, ProjectID: fixture.projectID, Database: fixture.database,
				ScopeRef: fixture.scopeRef, TokenFile: fixture.tokenFile,
			}),
		)
	}

	store := newStore(fixture.endpoint)
	owner, err := store.Create(beads.Bead{Title: "Disposable private evidence owner", Type: "task"})
	if err != nil {
		t.Fatalf("create disposable owner through bd: %v", err)
	}
	competingOwner, err := store.Create(beads.Bead{Title: "Disposable competing CAS owner", Type: "task"})
	if err != nil {
		t.Fatalf("create competing-CAS owner through bd: %v", err)
	}
	lostOwner, err := store.Create(beads.Bead{Title: "Disposable lost-response owner", Type: "task"})
	if err != nil {
		t.Fatalf("create lost-response owner through bd: %v", err)
	}
	fixture.command.arm()
	identity := Identity{Kind: KindRetry, OwnerBeadID: owner.ID, ExecutionBeadID: owner.ID}
	proposed := installedPrivateEvidence(identity, fixture.scopeRef, privateMarker)
	sealed, err := Seal(store, proposed)
	if err != nil {
		t.Fatalf("Seal through installed Beads service: %v", err)
	}
	if !samePayload(sealed, proposed) {
		t.Fatal("Seal returned a different snapshot than the installed service stored")
	}
	fixture.command.assertNoCalls(t)

	// A second BdStore proves that the index and archive are durable service
	// records, not values held by the first adapter instance.
	reopened := newStore(fixture.endpoint)
	ownerRead, err := reopened.Get(owner.ID)
	if err != nil || ownerRead.ID != owner.ID {
		t.Fatalf("configured BdStore owner read after reopen failed: %v", err)
	}
	read, err := Read(reopened, owner.ID, sealed.AttemptID)
	if err != nil || !samePayload(read, sealed) {
		t.Fatalf("exact attempt read through fresh adapter failed: %v", err)
	}
	listed, err := List(reopened, owner.ID)
	if err != nil || len(listed) != 1 || !samePayload(listed[0], sealed) {
		t.Fatalf("attempt list through fresh adapter did not return the exact snapshot: %v", err)
	}
	fixture.command.assertNoCalls(t)

	// Delete the owner through the disposable installed CLI. The separate
	// archive row must still answer after another fresh adapter construction.
	runInstalledPrivateEvidenceBD(t, fixture, "delete", "--force", "--json", owner.ID)
	afterDelete := newStore(fixture.endpoint)
	if _, err := afterDelete.Get(owner.ID); !errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("owner read after disposable CLI deletion returned %v, want ErrNotFound", err)
	}
	archived, err := Read(afterDelete, owner.ID, sealed.AttemptID)
	if err != nil || !samePayload(archived, sealed) {
		t.Fatalf("exact archive read after owner deletion failed: %v", err)
	}
	listed, err = List(afterDelete, owner.ID)
	if err != nil || len(listed) != 1 || !samePayload(listed[0], sealed) {
		t.Fatalf("archive list after owner deletion did not return the exact snapshot: %v", err)
	}
	fixture.command.assertNoCalls(t)

	competingIdentity := Identity{Kind: KindRetry, OwnerBeadID: competingOwner.ID, ExecutionBeadID: competingOwner.ID}
	competingA := installedPrivateEvidence(competingIdentity, fixture.scopeRef, privateMarker+"-winner-a")
	competingB := installedPrivateEvidence(competingIdentity, fixture.scopeRef, privateMarker+"-winner-b")
	key := installedOwnerIndexKey(competingA.AttemptID)
	firstStore, secondStore := newStore(fixture.endpoint), newStore(fixture.endpoint)
	type casResult struct {
		swapped bool
		err     error
	}
	start := make(chan struct{})
	results := make(chan casResult, 2)
	for _, candidate := range []struct {
		store    *beads.BdStore
		evidence Evidence
	}{{firstStore, competingA}, {secondStore, competingB}} {
		candidate := candidate
		go func() {
			<-start
			encoded, marshalErr := json.Marshal(candidate.evidence)
			if marshalErr != nil {
				results <- casResult{err: marshalErr}
				return
			}
			swapped, casErr := candidate.store.CompareAndSetPrivateEvidenceMetadataKey(
				competingOwner.ID, key, "", string(encoded))
			results <- casResult{swapped: swapped, err: casErr}
		}()
	}
	close(start)
	resultA, resultB := <-results, <-results
	if resultA.err != nil || resultB.err != nil {
		t.Fatalf("installed metadata CAS returned errors: %v / %v", resultA.err, resultB.err)
	}
	if resultA.swapped == resultB.swapped {
		t.Fatalf("competing installed CAS results were %t and %t; want exactly one winner", resultA.swapped, resultB.swapped)
	}
	winner, present, err := beads.ReadPrivateEvidenceMetadataKey(firstStore, competingOwner.ID, key)
	if err != nil || !present {
		t.Fatalf("read installed CAS winner: present=%t error=%v", present, err)
	}
	var winnerEvidence Evidence
	if err := json.Unmarshal([]byte(winner), &winnerEvidence); err != nil {
		t.Fatalf("decode installed CAS winner: %v", err)
	}
	if !samePayload(winnerEvidence, competingA) && !samePayload(winnerEvidence, competingB) {
		t.Fatal("installed CAS stored a value different from both competing owner-index candidates")
	}
	fixture.command.assertNoCalls(t)

	lostIdentity := Identity{Kind: KindRetry, OwnerBeadID: lostOwner.ID, ExecutionBeadID: lostOwner.ID}
	lostProposal := installedPrivateEvidence(lostIdentity, fixture.scopeRef, privateMarker+"-lost-response")
	lossyEndpoint, droppedCAS := installedCommitThenDropProxy(t, fixture.endpoint)
	lostResult, err := Seal(newStore(lossyEndpoint), lostProposal)
	if err != nil {
		t.Fatalf("Seal did not resolve a committed CAS with exact owner-index readback: %v", err)
	}
	if !samePayload(lostResult, lostProposal) || droppedCAS.Load() != 1 {
		t.Fatalf("lost-response Seal result mismatch or CAS replay: drops=%d", droppedCAS.Load())
	}
	finalRead, err := Read(newStore(fixture.endpoint), lostOwner.ID, lostProposal.AttemptID)
	if err != nil || !samePayload(finalRead, lostProposal) {
		t.Fatalf("installed owner-index winner changed after response loss: %v", err)
	}
	fixture.command.assertNoCalls(t)
}

func readInstalledPrivateEvidenceFixture(t *testing.T) installedPrivateEvidenceFixture {
	t.Helper()
	read := func(name string) string {
		value := strings.TrimSpace(os.Getenv("GC_PRIVATE_EVIDENCE_PROBE_" + name))
		if value == "" {
			t.Fatalf("installed private evidence probe is missing GC_PRIVATE_EVIDENCE_PROBE_%s", name)
		}
		return value
	}
	return installedPrivateEvidenceFixture{
		endpoint: read("ENDPOINT"), projectID: read("PROJECT_ID"), database: read("DATABASE"),
		scopeRef: read("SCOPE_REF"), tokenFile: read("TOKEN_FILE"), workspace: read("WORKSPACE"),
		root: read("ROOT"), bdBinary: read("BD_BINARY"),
	}
}

func installedPrivateEvidence(identity Identity, scopeRef, marker string) Evidence {
	attemptID, _ := AttemptID(identity)
	return Evidence{
		SchemaVersion: SchemaVersion, AttemptID: attemptID, Identity: identity, StoreRef: scopeRef,
		Permission: PermissionScope{StoreRef: scopeRef, WorkID: identity.OwnerBeadID},
		CapturedAt: time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC), Outcome: marker,
		SourceStatus: StatusUnavailable, SourceReason: "source_unavailable",
		BaseStatus: StatusUnavailable, BaseReason: "base_unavailable",
		CandidateStatus: StatusUnavailable, CandidateReason: "candidate_unavailable",
		WorkingTreeStatus: WorkingTreeUnknown,
		Diff:              DiffSnapshot{Status: StatusUnavailable, Reason: "base_unavailable", Source: DiffSourceCandidateCommitDelta},
		WorkspaceDiff:     DiffSnapshot{Status: StatusUnavailable, Reason: "workspace_unavailable", Source: DiffSourceWorkingTree},
		Policy:            unavailableFacet("not_linked"), Actions: unavailableFacet("not_linked"),
		Acknowledgements: unavailableFacet("not_linked"), Redaction: unavailableFacet("not_performed"),
	}
}

func installedOwnerIndexKey(attemptID string) string {
	digest := sha256.Sum256([]byte(attemptID))
	return beadmeta.AttemptEvidenceIndexPrefix + hex.EncodeToString(digest[:])
}

func runInstalledPrivateEvidenceBD(t *testing.T, fixture installedPrivateEvidenceFixture, args ...string) {
	t.Helper()
	cmd := exec.Command(fixture.bdBinary, args...)
	cmd.Dir = fixture.workspace
	cmd.Env = []string{
		"HOME=" + filepath.Join(fixture.root, "home"),
		"XDG_CONFIG_HOME=" + filepath.Join(fixture.root, "config"),
		"DOLT_ROOT_PATH=" + filepath.Join(fixture.root, "dolt-home"),
		"BEADS_DIR=" + filepath.Join(fixture.workspace, ".beads"),
		"BD_EXPORT_AUTO=false",
		"PATH=" + os.Getenv("PATH"),
		"GOMAXPROCS=2",
	}
	if _, err := cmd.CombinedOutput(); err != nil {
		// The exact command is useful evidence and contains only the disposable
		// owner id. Avoid printing output, which may include stored metadata.
		t.Fatalf("disposable bd %s failed: %v", args[0], err)
	}
}

func installedCommitThenDropProxy(t *testing.T, backend string) (string, *atomic.Int32) {
	t.Helper()
	target, err := url.Parse(backend)
	if err != nil {
		t.Fatalf("parse disposable service endpoint: %v", err)
	}
	dropped := &atomic.Int32{}
	forwarder := &http.Transport{Proxy: nil}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamRequest := r.Clone(r.Context())
		upstreamURL := *target
		upstreamURL.Path = r.URL.Path
		upstreamURL.RawPath = r.URL.RawPath
		upstreamURL.RawQuery = r.URL.RawQuery
		upstreamRequest.URL = &upstreamURL
		upstreamRequest.Host = target.Host
		upstreamRequest.RequestURI = ""
		response, err := forwarder.RoundTrip(upstreamRequest)
		if err != nil {
			http.Error(w, "disposable proxy upstream failed", http.StatusBadGateway)
			return
		}
		body, err := io.ReadAll(response.Body)
		closeErr := response.Body.Close()
		if err != nil || closeErr != nil {
			http.Error(w, "disposable proxy response read failed", http.StatusBadGateway)
			return
		}
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, ":casMetadata") && response.StatusCode == http.StatusOK {
			var result struct {
				Swapped bool `json:"swapped"`
			}
			if json.Unmarshal(body, &result) == nil && result.Swapped {
				hijacker, ok := w.(http.Hijacker)
				if !ok {
					http.Error(w, "disposable proxy cannot drop response", http.StatusInternalServerError)
					return
				}
				connection, _, err := hijacker.Hijack()
				if err == nil {
					dropped.Add(1)
					_ = connection.Close()
				}
				return
			}
		}
		for key, values := range response.Header {
			for _, value := range values {
				w.Header().Add(key, value)
			}
		}
		w.WriteHeader(response.StatusCode)
		_, _ = w.Write(body)
	}))
	t.Cleanup(func() {
		server.Close()
		forwarder.CloseIdleConnections()
	})
	return server.URL, dropped
}
