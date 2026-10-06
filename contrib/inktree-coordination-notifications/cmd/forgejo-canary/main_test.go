package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	inktreecoordinationnotifications "github.com/gastownhall/gascity/contrib/inktree-coordination-notifications"
)

func TestCanaryPostsOnceAndSuppressesSameEpochRefire(t *testing.T) {
	var mu sync.Mutex
	comments := []forgeComment{}
	posts := 0
	client := newTestHTTPClient(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "token fixture-token" {
			http.Error(writer, "unauthorized", http.StatusUnauthorized)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		switch request.Method + " " + request.URL.Path {
		case "GET /api/v1/user":
			_ = json.NewEncoder(writer).Encode(forgeUser{Login: "canary-author"})
		case "GET /api/v1/repos/inktri/inktree/issues/777":
			mu.Lock()
			defer mu.Unlock()
			_ = json.NewEncoder(writer).Encode(forgeIssue{Number: 777, Title: "[synthetic] req-syn-0003 notification canary", State: "open", User: forgeUser{Login: "canary-author"}, Comments: len(comments)})
		case "GET /api/v1/repos/inktri/inktree/issues/777/comments":
			mu.Lock()
			defer mu.Unlock()
			_ = json.NewEncoder(writer).Encode(comments)
		case "POST /api/v1/repos/inktri/inktree/issues/777/comments":
			payload := map[string]string{}
			if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
				http.Error(writer, "bad request", http.StatusBadRequest)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			posts++
			comment := forgeComment{ID: 901, Body: payload["body"], User: forgeUser{Login: "canary-author"}}
			comments = append(comments, comment)
			writer.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(writer).Encode(comment)
		default:
			http.NotFound(writer, request)
		}
	}))

	output := filepath.Join(t.TempDir(), "canary.json")
	verification, authorization, reviewedHead := writeGateFixtures(t, filepath.Dir(output))
	config := canaryConfig{
		ForgejoURL: "http://127.0.0.1", Repository: "inktri/inktree", Issue: 777,
		RequestID: "req-syn-0003", BeadID: "inktree-syn0003", Output: output,
		Token: "fixture-token", HTTPClient: client,
		Verification: verification, Authorization: authorization, SourceHead: reviewedHead,
		AllowLoopback: true,
		JEVVersion:    "jev-1.13-free", RLCDVersion: "rlcd-local-v1", SemIFVersion: "semif-qwen-local-v1",
	}
	intentPath, err := canaryIntentPath(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(intentPath) })
	errorsCh := make(chan error, 2)
	var workers sync.WaitGroup
	for range 2 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			errorsCh <- runCanary(context.Background(), config)
		}()
	}
	workers.Wait()
	close(errorsCh)
	for err := range errorsCh {
		if err != nil {
			t.Fatal(err)
		}
	}
	if posts != 1 || len(comments) != 1 {
		t.Fatalf("canary deliveries = %d posts / %d comments, want exactly one", posts, len(comments))
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	evidence := canaryEvidence{}
	if err := json.Unmarshal(data, &evidence); err != nil {
		t.Fatal(err)
	}
	if string(data) == "" || bytes.Contains(data, []byte("fixture-token")) || bytes.Contains(data, []byte("canary-author")) {
		t.Fatal("retained evidence contains a credential or author identifier")
	}
	if !evidence.AuthorAttested || evidence.LiveDispatch || !evidence.NotificationDispatch ||
		!evidence.RefireSuppressed || evidence.NotificationCount != 1 || len(evidence.Ledger.Receipts) != 2 ||
		evidence.Transport.CommentID != 901 || !evidence.Transport.Posted || evidence.Transport.Recovered {
		t.Fatalf("invalid retained canary evidence: %#v", evidence)
	}
	if evidence.Projection.Decision.LiveDispatch || !evidence.Projection.Decision.Deliver {
		t.Fatalf("notification-only switch separation failed: %#v", evidence.Projection.Decision)
	}
	if evidence.ExecutionTrace.ForgejoCommentWrites != 1 || evidence.ExecutionTrace.GasCityClientLinked ||
		evidence.ExecutionTrace.RouteCapabilityLinked || evidence.ExecutionTrace.WorkerCapabilityLinked {
		t.Fatalf("invalid bounded execution trace: %#v", evidence.ExecutionTrace)
	}
	if err := runCanary(context.Background(), config); err != nil {
		t.Fatalf("stable third canary invocation: %v", err)
	}
	mu.Lock()
	retainedComment := comments[0]
	comments = nil
	mu.Unlock()
	if err := os.Remove(output); err != nil {
		t.Fatal(err)
	}
	if err := runCanary(context.Background(), config); err == nil || posts != 1 {
		t.Fatalf("deleted remote receipt reused completed authorization: err=%v posts=%d", err, posts)
	}
	writeFixtureJSON(t, output, evidence)
	mu.Lock()
	comments = []forgeComment{retainedComment}
	mu.Unlock()
	evidence.Transport.CommentID++
	writeFixtureJSON(t, output, evidence)
	if err := runCanary(context.Background(), config); err == nil {
		t.Fatal("tampered retained transport receipt was accepted")
	}
}

func TestCanaryRejectsUnattestedAuthor(t *testing.T) {
	client := newTestHTTPClient(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		if request.URL.Path == "/api/v1/user" {
			_ = json.NewEncoder(writer).Encode(forgeUser{Login: "token-owner"})
			return
		}
		_ = json.NewEncoder(writer).Encode(forgeIssue{Number: 777, Title: "req-syn-0003", State: "open", User: forgeUser{Login: "different-author"}})
	}))
	dir := t.TempDir()
	verification, authorization, reviewedHead := writeGateFixtures(t, dir)
	err := runCanary(context.Background(), canaryConfig{
		ForgejoURL: "http://127.0.0.1", Repository: "inktri/inktree", Issue: 777,
		RequestID: "req-syn-0003", BeadID: "inktree-syn0003", Output: filepath.Join(dir, "canary.json"),
		Token: "fixture-token", HTTPClient: client,
		Verification: verification, Authorization: authorization, SourceHead: reviewedHead,
		AllowLoopback: true,
		JEVVersion:    "jev-1", RLCDVersion: "rlcd-1", SemIFVersion: "semif-1",
	})
	if err == nil {
		t.Fatal("unattested synthetic author was accepted")
	}
}

func TestCanaryRejectsUnreviewedHeadBeforeForgejo(t *testing.T) {
	requests := 0
	client := newTestHTTPClient(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		requests++
		http.Error(writer, "unexpected", http.StatusInternalServerError)
	}))
	dir := t.TempDir()
	verification, authorization, _ := writeGateFixtures(t, dir)
	err := runCanary(context.Background(), canaryConfig{
		ForgejoURL: "http://127.0.0.1", Repository: "inktri/inktree", Issue: 777,
		RequestID: "req-syn-0003", BeadID: "inktree-syn0003", Output: filepath.Join(dir, "canary.json"),
		Token: "fixture-token", HTTPClient: client, Verification: verification, Authorization: authorization,
		SourceHead: strings.Repeat("b", 40), JEVVersion: "jev-1", RLCDVersion: "rlcd-1", SemIFVersion: "semif-1",
		AllowLoopback: true,
	})
	if err == nil || requests != 0 {
		t.Fatalf("unreviewed source head reached Forgejo: err=%v requests=%d", err, requests)
	}
}

func TestCanaryRejectsUnauthorizedSyntheticBeadBeforeForgejo(t *testing.T) {
	requests := 0
	client := newTestHTTPClient(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		requests++
		http.Error(writer, "unexpected", http.StatusInternalServerError)
	}))
	dir := t.TempDir()
	verification, authorization, reviewedHead := writeGateFixtures(t, dir)
	err := runCanary(context.Background(), canaryConfig{
		ForgejoURL: "http://127.0.0.1", Repository: "inktri/inktree", Issue: 777,
		RequestID: "req-syn-0003", BeadID: "inktree-syn0004", Output: filepath.Join(dir, "canary.json"),
		Token: "fixture-token", HTTPClient: client, Verification: verification, Authorization: authorization,
		SourceHead: reviewedHead, JEVVersion: "jev-1", RLCDVersion: "rlcd-1", SemIFVersion: "semif-1",
		AllowLoopback: true,
	})
	if err == nil || requests != 0 {
		t.Fatalf("unauthorized synthetic bead reached Forgejo: err=%v requests=%d", err, requests)
	}
}

func TestCanaryRejectsForgejoRedirect(t *testing.T) {
	redirectedRequests := 0
	client := newTestHTTPClient(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Host == "redirect.invalid" {
			redirectedRequests++
			writer.WriteHeader(http.StatusOK)
			return
		}
		http.Redirect(writer, request, "http://redirect.invalid", http.StatusTemporaryRedirect)
	}))
	dir := t.TempDir()
	verification, authorization, reviewedHead := writeGateFixtures(t, dir)
	err := runCanary(context.Background(), canaryConfig{
		ForgejoURL: "http://127.0.0.1", Repository: "inktri/inktree", Issue: 777,
		RequestID: "req-syn-0003", BeadID: "inktree-syn0003", Output: filepath.Join(dir, "canary.json"),
		Token: "fixture-token", HTTPClient: client, Verification: verification, Authorization: authorization,
		SourceHead: reviewedHead, JEVVersion: "jev-1", RLCDVersion: "rlcd-1", SemIFVersion: "semif-1",
		AllowLoopback: true,
	})
	if err == nil || redirectedRequests != 0 {
		t.Fatalf("Forgejo redirect was followed: err=%v redirected_requests=%d", err, redirectedRequests)
	}
}

func TestCanaryLeavesUncertainPostIntentAndRefusesRetry(t *testing.T) {
	posts := 0
	client := newTestHTTPClient(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch request.Method + " " + request.URL.Path {
		case "GET /api/v1/user":
			_ = json.NewEncoder(writer).Encode(forgeUser{Login: "canary-author"})
		case "GET /api/v1/repos/inktri/inktree/issues/777":
			_ = json.NewEncoder(writer).Encode(forgeIssue{Number: 777, Title: "req-syn-0003", State: "open", User: forgeUser{Login: "canary-author"}})
		case "GET /api/v1/repos/inktri/inktree/issues/777/comments":
			_ = json.NewEncoder(writer).Encode([]forgeComment{})
		case "POST /api/v1/repos/inktri/inktree/issues/777/comments":
			posts++
			http.Error(writer, "uncertain", http.StatusInternalServerError)
		default:
			http.NotFound(writer, request)
		}
	}))
	dir := t.TempDir()
	verification, authorization, reviewedHead := writeGateFixtures(t, dir)
	config := canaryConfig{
		ForgejoURL: "http://127.0.0.1", Repository: "inktri/inktree", Issue: 777,
		RequestID: "req-syn-0003", BeadID: "inktree-syn0003", Output: filepath.Join(dir, "canary.json"),
		Token: "fixture-token", HTTPClient: client, Verification: verification, Authorization: authorization,
		SourceHead: reviewedHead, JEVVersion: "jev-1", RLCDVersion: "rlcd-1", SemIFVersion: "semif-1", AllowLoopback: true,
	}
	intentPath, err := canaryIntentPath(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(intentPath) })
	if err := runCanary(context.Background(), config); err == nil {
		t.Fatal("failed Forgejo post unexpectedly succeeded")
	}
	if err := runCanary(context.Background(), config); err == nil || posts != 1 {
		t.Fatalf("uncertain Forgejo post was retried: err=%v posts=%d", err, posts)
	}
}

func TestCanaryRecoversVisibleCommentAfterEvidenceWriteFailure(t *testing.T) {
	var mu sync.Mutex
	comments := []forgeComment{}
	posts := 0
	client := newTestHTTPClient(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		switch request.Method + " " + request.URL.Path {
		case "GET /api/v1/user":
			_ = json.NewEncoder(writer).Encode(forgeUser{Login: "canary-author"})
		case "GET /api/v1/repos/inktri/inktree/issues/777":
			mu.Lock()
			defer mu.Unlock()
			_ = json.NewEncoder(writer).Encode(forgeIssue{Number: 777, Title: "req-syn-0003", State: "open", User: forgeUser{Login: "canary-author"}, Comments: len(comments)})
		case "GET /api/v1/repos/inktri/inktree/issues/777/comments":
			mu.Lock()
			defer mu.Unlock()
			_ = json.NewEncoder(writer).Encode(comments)
		case "POST /api/v1/repos/inktri/inktree/issues/777/comments":
			payload := map[string]string{}
			if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
				http.Error(writer, "bad request", http.StatusBadRequest)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			posts++
			comment := forgeComment{ID: 902, Body: payload["body"], User: forgeUser{Login: "canary-author"}}
			comments = append(comments, comment)
			writer.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(writer).Encode(comment)
		default:
			http.NotFound(writer, request)
		}
	}))
	dir := t.TempDir()
	verification, authorization, reviewedHead := writeGateFixtures(t, dir)
	readOnlyDirectory := filepath.Join(dir, "read-only")
	if err := os.Mkdir(readOnlyDirectory, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(readOnlyDirectory, 0o700) })
	failedOutput := filepath.Join(readOnlyDirectory, "canary.json")
	config := canaryConfig{
		ForgejoURL: "http://127.0.0.1", Repository: "inktri/inktree", Issue: 777,
		RequestID: "req-syn-0003", BeadID: "inktree-syn0003", Output: failedOutput,
		Token: "fixture-token", HTTPClient: client, Verification: verification, Authorization: authorization,
		SourceHead: reviewedHead, JEVVersion: "jev-1", RLCDVersion: "rlcd-1", SemIFVersion: "semif-1", AllowLoopback: true,
	}
	intentPath, err := canaryIntentPath(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(intentPath) })
	if err := runCanary(context.Background(), config); err == nil {
		t.Fatal("evidence write failure unexpectedly succeeded")
	}
	config.Output = filepath.Join(dir, "recovered.json")
	if err := runCanary(context.Background(), config); err != nil {
		t.Fatalf("recover visible Forgejo comment: %v", err)
	}
	if err := runCanary(context.Background(), config); err != nil {
		t.Fatalf("suppress refire after recovery: %v", err)
	}
	data, err := os.ReadFile(config.Output)
	if err != nil {
		t.Fatal(err)
	}
	evidence := canaryEvidence{}
	if err := json.Unmarshal(data, &evidence); err != nil {
		t.Fatal(err)
	}
	if posts != 1 || !evidence.Transport.Recovered || evidence.Transport.Posted ||
		evidence.ExecutionTrace.ForgejoCommentWrites != 0 || evidence.ExecutionTrace.ForgejoReadCalls != 6 ||
		!evidence.RefireSuppressed || len(evidence.Ledger.Receipts) != 2 {
		t.Fatalf("invalid recovered evidence: posts=%d evidence=%#v", posts, evidence)
	}
	if err := runCanary(context.Background(), config); err != nil {
		t.Fatalf("stable third recovered invocation: %v", err)
	}
}

func TestProductionConfigRejectsLoopback(t *testing.T) {
	_, err := validateCanaryConfig(canaryConfig{
		ForgejoURL: "http://127.0.0.1:3000", Repository: "inktri/inktree", Issue: 777,
		RequestID: "req-syn-0003", BeadID: "inktree-syn0003", Output: "/tmp/out", Verification: "/tmp/verification",
		Authorization: "/tmp/authorization", Token: "token", HTTPClient: http.DefaultClient,
	})
	if err == nil {
		t.Fatal("production configuration accepted loopback HTTP")
	}
}

func TestReviewedWorktreeRejectsUntrackedSource(t *testing.T) {
	repository := t.TempDir()
	runGit := func(arguments ...string) {
		t.Helper()
		command := gitCommand(context.Background(), repository, arguments...)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", arguments, err, output)
		}
	}
	runGit("init", "-q")
	tracked := filepath.Join(repository, "tracked.go")
	if err := os.WriteFile(tracked, []byte("package reviewed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit("add", "tracked.go")
	runGit("-c", "user.name=Canary Test", "-c", "user.email=canary@example.invalid", "commit", "-qm", "reviewed")
	if err := os.WriteFile(filepath.Join(repository, "unreviewed.go"), []byte("package reviewed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	external := t.TempDir()
	authorization := filepath.Join(external, "authorization.json")
	if err := os.WriteFile(authorization, []byte("{}"), 0o400); err != nil {
		t.Fatal(err)
	}
	err := validateReviewedWorktree(context.Background(), canaryConfig{
		WorkDir: repository, Output: filepath.Join(external, "out.json"), Authorization: authorization,
	})
	if err == nil {
		t.Fatal("untracked source was accepted as reviewed")
	}
}

func TestReviewedWorktreeRejectsSymlinkedExternalOutput(t *testing.T) {
	repository := t.TempDir()
	runGit := func(arguments ...string) {
		t.Helper()
		command := gitCommand(context.Background(), repository, arguments...)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", arguments, err, output)
		}
	}
	runGit("init", "-q")
	tracked := filepath.Join(repository, "tracked.go")
	if err := os.WriteFile(tracked, []byte("package reviewed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit("add", "tracked.go")
	runGit("-c", "user.name=Canary Test", "-c", "user.email=canary@example.invalid", "commit", "-qm", "reviewed")
	external := t.TempDir()
	link := filepath.Join(external, "apparent-external")
	if err := os.Symlink(repository, link); err != nil {
		t.Fatal(err)
	}
	authorization := filepath.Join(external, "authorization.json")
	if err := os.WriteFile(authorization, []byte("{}"), 0o400); err != nil {
		t.Fatal(err)
	}
	err := validateReviewedWorktree(context.Background(), canaryConfig{
		WorkDir: repository, Output: filepath.Join(link, "canary.json"), Authorization: authorization,
	})
	if err == nil {
		t.Fatal("symlinked output path re-entered the reviewed worktree")
	}
}

func TestProductionDescriptorsBindEvidenceAndOutput(t *testing.T) {
	repository := t.TempDir()
	runGit := func(arguments ...string) {
		t.Helper()
		command := gitCommand(context.Background(), repository, arguments...)
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", arguments, err, output)
		}
	}
	runGit("init", "-q")
	evidencePath := filepath.Join(repository, "verification.json")
	if err := os.WriteFile(evidencePath, []byte("{\"passed\":true}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runGit("add", "verification.json")
	runGit("-c", "user.name=Canary Test", "-c", "user.email=canary@example.invalid", "commit", "-qm", "reviewed")
	outputDirectory := t.TempDir()
	if err := os.Chmod(outputDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	config := canaryConfig{WorkDir: repository, Output: filepath.Join(outputDirectory, "canary.json")}
	data, err := readReviewedEvidenceFile(context.Background(), config, evidencePath, "fixture evidence")
	if err != nil || string(data) != "{\"passed\":true}\n" {
		t.Fatalf("read reviewed evidence descriptor: data=%q err=%v", data, err)
	}
	directory, err := openStableOutputDirectory(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = directory.Close() }()
	if err := writeJSON(context.Background(), config, config.Output, directory, map[string]bool{"passed": true}); err != nil {
		t.Fatal(err)
	}
	written, err := os.ReadFile(config.Output)
	if err != nil || !bytes.Contains(written, []byte(`"passed": true`)) {
		t.Fatalf("write stable output descriptor: data=%q err=%v", written, err)
	}
}

func TestCanaryStateKeyCanonicalizesEquivalentTargetConfiguration(t *testing.T) {
	first := canaryConfig{ForgejoURL: authorizedForgejoOrigin, Repository: "inktri/inktree", Issue: 777, RequestID: "req-syn-0003", Authorization: "/tmp/one"}
	second := first
	second.Authorization = "/elsewhere/two"
	second.ForgejoURL += "/"
	firstPath, err := canaryStatePath(first, ".lock")
	if err != nil {
		t.Fatal(err)
	}
	secondPath, err := canaryStatePath(second, ".lock")
	if err != nil {
		t.Fatal(err)
	}
	if firstPath != secondPath {
		t.Fatalf("same Forgejo target produced different locks: %q != %q", firstPath, secondPath)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func newTestHTTPClient(handler http.Handler) *http.Client {
	return &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		return recorder.Result(), nil
	})}
}

func writeGateFixtures(t *testing.T, dir string) (string, string, string) {
	t.Helper()
	policy, err := inktreecoordinationnotifications.PolicyDocument()
	if err != nil {
		t.Fatal(err)
	}
	corpus, err := inktreecoordinationnotifications.ReplayCorpusDocument()
	if err != nil {
		t.Fatal(err)
	}
	verificationPath := filepath.Join(dir, "verification.json")
	replayPath := filepath.Join(dir, "replay.json")
	authorizationPath := filepath.Join(dir, "authorization.json")
	reviewedHead := strings.Repeat("a", 40)
	manifest := verificationManifest{SyntheticCanary: "not_run_by_instruction"}
	manifest.PolicyDispatchSwitches.Live = "off"
	manifest.PolicyDispatchSwitches.Notification = "off"
	manifest.FocusedGoTests.Status = "passed"
	manifest.FocusedGoTests.Packages = []string{"contrib/inktree-coordination-notifications/cmd/forgejo-canary"}
	manifest.FocusedGoVet.Status = "passed"
	manifest.ReplayBinding.Status = "passed"
	manifest.ReplayBinding.PolicyHash = sha256Hex(policy)
	manifest.ReplayBinding.CorpusHash = sha256Hex(corpus)
	manifest.ReviewFindings = map[string]string{"canary_adapter": "passed"}
	manifest.RepositoryChecks = map[string]string{"check_docs": "passed", "check_hooks": "passed", "json_parse": "passed"}
	writeFixtureJSON(t, verificationPath, manifest)
	writeFixtureJSON(t, replayPath, replayHashes{PolicyHash: sha256Hex(policy), CorpusHash: sha256Hex(corpus)})
	verificationData, err := os.ReadFile(verificationPath)
	if err != nil {
		t.Fatal(err)
	}
	writeFixtureJSON(t, authorizationPath, canaryAuthorization{
		Version: "inktree-forgejo-canary-authorization/v1", RequestID: "req-syn-0003", BeadID: "inktree-syn0003",
		Repository: "inktri/inktree", Issue: 777, BindingID: "binding-0123456789abcdef0123456789abcdef",
		ReviewedHead: reviewedHead, VerificationSHA256: sha256Hex(verificationData),
		OfflineGatesPassed: true, IndependentReviewPassed: true,
		SyntheticCanaryAuthorized: true,
	})
	if err := os.Chmod(authorizationPath, 0o400); err != nil {
		t.Fatal(err)
	}
	return verificationPath, authorizationPath, reviewedHead
}

func writeFixtureJSON(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}
