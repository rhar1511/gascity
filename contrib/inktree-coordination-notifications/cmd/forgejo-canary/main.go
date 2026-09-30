// Package main runs one explicitly authorized synthetic Forgejo notification canary.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	inktreecoordinationnotifications "github.com/gastownhall/gascity/contrib/inktree-coordination-notifications"
	"github.com/gastownhall/gascity/internal/coordinationnotify"
)

const authorizedForgejoOrigin = "https://forgejo.harjanto.id.au"

var (
	repositoryPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*/[a-z0-9][a-z0-9._-]*$`)
	requestIDPattern  = regexp.MustCompile(`^req-syn-[0-9]{4,}$`)
	beadIDPattern     = regexp.MustCompile(`^inktree-syn[0-9]{4,}$`)
	bindingIDPattern  = regexp.MustCompile(`^binding-[0-9a-f]{32}$`)
	commitPattern     = regexp.MustCompile(`^[0-9a-f]{40}$`)
	digestPattern     = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

type canaryConfig struct {
	ForgejoURL    string
	Repository    string
	Issue         int64
	RequestID     string
	BeadID        string
	Output        string
	Verification  string
	Authorization string
	Token         string
	HTTPClient    *http.Client
	JEVVersion    string
	RLCDVersion   string
	SemIFVersion  string
	SourceHead    string
	AllowLoopback bool
	WorkDir       string
}

type forgeUser struct {
	Login string `json:"login"`
}

type forgeIssue struct {
	Number   int64     `json:"number"`
	Title    string    `json:"title"`
	State    string    `json:"state"`
	User     forgeUser `json:"user"`
	Comments int       `json:"comments"`
}

type forgeComment struct {
	ID   int64     `json:"id"`
	Body string    `json:"body"`
	User forgeUser `json:"user"`
}

type canaryAuthorization struct {
	Version                   string `json:"version"`
	RequestID                 string `json:"request_id"`
	BeadID                    string `json:"bead_id"`
	Repository                string `json:"repository"`
	Issue                     int64  `json:"issue"`
	BindingID                 string `json:"binding_id"`
	ReviewedHead              string `json:"reviewed_head"`
	VerificationSHA256        string `json:"verification_sha256"`
	OfflineGatesPassed        bool   `json:"offline_gates_passed"`
	IndependentReviewPassed   bool   `json:"independent_review_passed"`
	SyntheticCanaryAuthorized bool   `json:"synthetic_canary_authorized"`
}

type verificationManifest struct {
	PolicyDispatchSwitches struct {
		Live         string `json:"live_dispatch"`
		Notification string `json:"notification_dispatch"`
	} `json:"policy_dispatch_switches"`
	FocusedGoTests struct {
		Status   string   `json:"status"`
		Packages []string `json:"packages"`
	} `json:"focused_go_tests"`
	FocusedGoVet struct {
		Status string `json:"status"`
	} `json:"focused_go_vet"`
	ReplayBinding struct {
		Status     string `json:"status"`
		CorpusHash string `json:"corpus_hash"`
		PolicyHash string `json:"policy_hash"`
	} `json:"replay_binding"`
	ReviewFindings   map[string]string `json:"review_findings"`
	RepositoryChecks map[string]string `json:"repository_checks"`
	SyntheticCanary  string            `json:"synthetic_canary"`
}

type replayHashes struct {
	CorpusHash string `json:"corpus_hash"`
	PolicyHash string `json:"policy_hash"`
}

type canaryInputDocument struct {
	RequestID        string                           `json:"request_id"`
	Repository       string                           `json:"repository"`
	Issue            int64                            `json:"issue"`
	Status           string                           `json:"status"`
	SourceReason     string                           `json:"source_reason"`
	BeadID           string                           `json:"bead_id"`
	HoldEpoch        uint64                           `json:"hold_epoch"`
	Mode             coordinationnotify.Mode          `json:"mode"`
	Channel          string                           `json:"channel"`
	RecipientRole    string                           `json:"recipient_role"`
	BindingSource    string                           `json:"binding_source"`
	BindingID        string                           `json:"binding_id"`
	PolicyVersion    string                           `json:"policy_version"`
	RouterConfigHash string                           `json:"router_config_hash"`
	ModelVersions    coordinationnotify.ModelVersions `json:"model_versions"`
}

type transportReceipt struct {
	CommentID int64  `json:"comment_id"`
	ThreadURL string `json:"thread_url"`
	Posted    bool   `json:"posted"`
	Recovered bool   `json:"recovered_existing"`
}

type canaryEvidence struct {
	Version              string                            `json:"version"`
	RequestID            string                            `json:"request_id"`
	Repository           string                            `json:"repository"`
	Issue                int64                             `json:"issue"`
	ReviewedHead         string                            `json:"reviewed_head"`
	OfflineGateValidated bool                              `json:"offline_gate_validated"`
	AuthorAttested       bool                              `json:"author_attested"`
	LiveDispatch         bool                              `json:"live_dispatch"`
	NotificationDispatch bool                              `json:"notification_dispatch"`
	NotificationCount    int                               `json:"notification_count"`
	RefireSuppressed     bool                              `json:"same_epoch_refire_suppressed"`
	Projection           coordinationnotify.Projection     `json:"projection"`
	Rendered             string                            `json:"rendered"`
	Ledger               coordinationnotify.LedgerSnapshot `json:"ledger"`
	LatestReceipt        coordinationnotify.Receipt        `json:"latest_receipt"`
	Transport            transportReceipt                  `json:"transport"`
	ExecutionTrace       executionTrace                    `json:"execution_trace"`
}

type executionTrace struct {
	ForgejoReadCalls       int  `json:"forgejo_read_calls"`
	ForgejoCommentWrites   int  `json:"forgejo_comment_writes"`
	GitHeadChecks          int  `json:"git_head_checks"`
	GasCityClientLinked    bool `json:"gas_city_client_linked"`
	RouteCapabilityLinked  bool `json:"route_capability_linked"`
	WorkerCapabilityLinked bool `json:"worker_capability_linked"`
}

type deliveryIntent struct {
	Version        string `json:"version"`
	State          string `json:"state"`
	RequestID      string `json:"request_id"`
	BeadID         string `json:"bead_id"`
	Repository     string `json:"repository"`
	Issue          int64  `json:"issue"`
	NotificationID string `json:"notification_id"`
	CommentID      int64  `json:"comment_id,omitempty"`
}

type canaryRuntime struct {
	trace executionTrace
}

func main() {
	forgejoURL := flag.String("forgejo-url", "https://forgejo.harjanto.id.au", "Forgejo origin")
	repository := flag.String("repo", "inktri/inktree", "synthetic thread repository")
	issue := flag.Int64("issue", 0, "authorized synthetic issue number")
	requestID := flag.String("request-id", "req-syn-0003", "synthetic request identifier")
	beadID := flag.String("bead-id", "inktree-syn0003", "synthetic bead identifier")
	out := flag.String("out", "contrib/inktree-coordination-notifications/evidence/forgejo-canary.json", "retained canary evidence")
	verification := flag.String("verification", "contrib/inktree-coordination-notifications/evidence/verification.json", "validated offline verification manifest")
	authorization := flag.String("authorization", "", "one-run synthetic canary authorization JSON")
	tokenFile := flag.String("token-file", "", "mode-0600 Forgejo token file")
	jev := flag.String("jev-version", "jev-1.13-free", "JEV version")
	rlcd := flag.String("rlcd-version", "rlcd-local-v1", "RLCD version")
	semif := flag.String("semif-version", "semif-qwen-local-v1", "SemIF version")
	flag.Parse()
	token, err := readSecretFile(*tokenFile, 0o600, "Forgejo token")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	config := canaryConfig{
		ForgejoURL: *forgejoURL, Repository: *repository, Issue: *issue,
		RequestID: *requestID, BeadID: *beadID, Output: *out,
		Verification: *verification, Authorization: *authorization,
		Token: token, HTTPClient: &http.Client{Timeout: 20 * time.Second},
		JEVVersion: *jev, RLCDVersion: *rlcd, SemIFVersion: *semif,
	}
	if err := runCanary(context.Background(), config); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func runCanary(ctx context.Context, config canaryConfig) error {
	baseURL, err := validateCanaryConfig(config)
	if err != nil {
		return err
	}
	reviewedHead, headChecks, authorization, err := validateOfflineAdmission(ctx, config)
	if err != nil {
		return err
	}
	var outputDirectory *os.File
	if config.SourceHead == "" {
		outputDirectory, err = openStableOutputDirectory(ctx, config)
		if err != nil {
			return err
		}
		defer func() { _ = outputDirectory.Close() }()
	}
	release, err := acquireCanaryLock(config)
	if err != nil {
		return err
	}
	defer release()
	client := *config.HTTPClient
	client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return errors.New("forgejo redirects are forbidden for credentialed canary requests")
	}
	config.HTTPClient = &client
	runtime := &canaryRuntime{trace: executionTrace{GitHeadChecks: headChecks}}
	currentUser := forgeUser{}
	if err := runtime.forgeJSON(ctx, config, http.MethodGet, "/api/v1/user", nil, &currentUser); err != nil {
		return fmt.Errorf("resolve Forgejo token identity: %w", err)
	}
	issue := forgeIssue{}
	issuePath := fmt.Sprintf("/api/v1/repos/%s/issues/%d", config.Repository, config.Issue)
	if err := runtime.forgeJSON(ctx, config, http.MethodGet, issuePath, nil, &issue); err != nil {
		return fmt.Errorf("read synthetic Forgejo issue: %w", err)
	}
	if currentUser.Login == "" || issue.Number != config.Issue || issue.State != "open" ||
		issue.User.Login != currentUser.Login || !strings.Contains(issue.Title, config.RequestID) {
		return errors.New("synthetic Forgejo thread is not open or author-attested to the token identity")
	}
	policy, err := inktreecoordinationnotifications.Policy()
	if err != nil {
		return err
	}
	policy.Switches.LiveDispatchValue = "off"
	policy.Switches.NotificationDispatchValue = "on"
	policy.Delivery.FailoverChannel = "discord"
	effectivePolicy, err := json.Marshal(policy)
	if err != nil {
		return fmt.Errorf("encode effective canary policy: %w", err)
	}
	inputDocument := canaryInputDocument{
		RequestID: config.RequestID, Repository: config.Repository, Issue: config.Issue,
		Status: "hold", SourceReason: "jev_selected_hold", BeadID: config.BeadID, HoldEpoch: 1,
		Mode: coordinationnotify.ModeReminder, Channel: "pr_update", RecipientRole: "author",
		BindingSource: "attested_forge_author", BindingID: authorization.BindingID,
		PolicyVersion: policy.Version, RouterConfigHash: sha256Hex(effectivePolicy),
		ModelVersions: coordinationnotify.ModelVersions{JEV: config.JEVVersion, RLCD: config.RLCDVersion, SemIF: config.SemIFVersion},
	}
	inputData, err := json.Marshal(inputDocument)
	if err != nil {
		return fmt.Errorf("encode canary input: %w", err)
	}
	projection, err := coordinationnotify.Build(policy, coordinationnotify.Input{
		Status: inputDocument.Status, SourceReason: inputDocument.SourceReason,
		BeadID: inputDocument.BeadID, HoldEpoch: inputDocument.HoldEpoch, Mode: inputDocument.Mode,
		Channel: inputDocument.Channel, RecipientRole: inputDocument.RecipientRole,
		BindingSource: inputDocument.BindingSource, BindingID: inputDocument.BindingID,
		PolicyVersion: inputDocument.PolicyVersion, EnvelopeHash: sha256Hex(inputData),
		RouterConfigHash: inputDocument.RouterConfigHash, ModelVersions: inputDocument.ModelVersions,
	})
	if err != nil {
		return fmt.Errorf("build canary projection: %w", err)
	}
	if projection.Envelope == nil || !projection.Decision.RenderAllowed || !projection.Decision.Deliver || projection.Decision.LiveDispatch {
		return errors.New("canary switches did not authorize notification-only delivery")
	}
	rendered, err := coordinationnotify.Render(policy, projection.Envelope)
	if err != nil {
		return fmt.Errorf("render canary notification: %w", err)
	}
	body := rendered + "\nNotification: " + projection.Envelope.NotificationID + "\nSynthetic request: " + config.RequestID
	threadURL := strings.TrimRight(baseURL.String(), "/") + "/" + config.Repository + "/issues/" + fmt.Sprint(config.Issue)
	prior := canaryEvidence{}
	if data, readErr := os.ReadFile(config.Output); readErr == nil {
		if err := decodeStrict(data, &prior); err != nil {
			return fmt.Errorf("decode prior canary evidence: %w", err)
		}
		if prior.RequestID != config.RequestID || prior.Repository != config.Repository || prior.Issue != config.Issue ||
			prior.Projection.Envelope == nil || prior.Projection.Envelope.NotificationID != projection.Envelope.NotificationID {
			return errors.New("prior canary evidence belongs to a different synthetic decision")
		}
		if err := validatePriorEvidence(prior, projection, rendered, reviewedHead, eventIDFor(projection.Envelope), threadURL, config.SourceHead == ""); err != nil {
			return err
		}
		runtime.trace.ForgejoReadCalls += prior.ExecutionTrace.ForgejoReadCalls
		runtime.trace.ForgejoCommentWrites += prior.ExecutionTrace.ForgejoCommentWrites
		runtime.trace.GitHeadChecks += prior.ExecutionTrace.GitHeadChecks
	} else if !errors.Is(readErr, os.ErrNotExist) {
		return fmt.Errorf("read prior canary evidence: %w", readErr)
	}
	ledger := coordinationnotify.NewLedgerFromSnapshot(prior.Ledger)
	eventID := eventIDFor(projection.Envelope)
	receipt := prior.LatestReceipt
	if !prior.RefireSuppressed {
		receipt = ledger.Record(projection.Envelope, coordinationnotify.Attempt{EventID: eventID, Channel: projection.Envelope.Channel})
	}
	comments := []forgeComment{}
	commentsPath := issuePath + "/comments?limit=50"
	if err := runtime.forgeJSON(ctx, config, http.MethodGet, commentsPath, nil, &comments); err != nil {
		return fmt.Errorf("list synthetic Forgejo comments: %w", err)
	}
	if issue.Comments != len(comments) || len(comments) > 1 {
		return errors.New("synthetic Forgejo thread is not dedicated or its complete comment set was not observed")
	}
	matches := make([]forgeComment, 0, 1)
	for i := range comments {
		if comments[i].Body == body {
			if comments[i].User.Login != currentUser.Login {
				return errors.New("matching canary comment was not authored by the attested Forgejo identity")
			}
			matches = append(matches, comments[i])
		}
	}
	if len(matches) > 1 {
		return fmt.Errorf("synthetic Forgejo thread contains %d duplicate canary comments", len(matches))
	}
	var existing *forgeComment
	if len(matches) == 1 {
		existing = &matches[0]
	}
	if len(comments) != len(matches) {
		return errors.New("synthetic Forgejo thread contains an unrelated comment")
	}
	intentPath, err := canaryIntentPath(config)
	if err != nil {
		return err
	}
	intent, err := readDeliveryIntent(intentPath, config, projection.Envelope.NotificationID)
	if err != nil {
		return err
	}
	if intent != nil && intent.State == "completed" && existing != nil && intent.CommentID != existing.ID {
		return errors.New("durable completion receipt does not match the attested Forgejo comment")
	}
	if prior.RefireSuppressed {
		if existing == nil {
			return errors.New("retained refire proof has no matching Forgejo receipt")
		}
		if existing.ID != prior.Transport.CommentID {
			return errors.New("retained Forgejo receipt does not match the attested comment")
		}
		if intent != nil && intent.CommentID != 0 && intent.CommentID != existing.ID {
			return errors.New("durable completion receipt does not match the attested Forgejo comment")
		}
		if err := completeDeliveryIntent(intentPath, config, projection.Envelope.NotificationID, existing.ID); err != nil {
			return err
		}
		return nil
	}
	transport := prior.Transport
	if existing != nil {
		if prior.Transport.CommentID != 0 && existing.ID != prior.Transport.CommentID {
			return errors.New("forgejo comment ID does not match prior canary evidence")
		}
		transport.CommentID = existing.ID
		if prior.Transport.CommentID == 0 {
			transport.Posted = false
			transport.Recovered = true
		}
	} else {
		if intent != nil {
			return errors.New("an earlier Forgejo delivery attempt has an uncertain outcome; refusing to post again")
		}
		if receipt.Suppressed {
			return errors.New("ledger suppressed canary delivery but the Forgejo receipt is absent")
		}
		if err := createDeliveryIntent(intentPath, config, projection.Envelope.NotificationID); err != nil {
			return err
		}
		created := forgeComment{}
		if err := runtime.forgeJSON(ctx, config, http.MethodPost, issuePath+"/comments", map[string]string{"body": body}, &created); err != nil {
			return fmt.Errorf("deliver synthetic Forgejo comment: %w", err)
		}
		if created.ID == 0 || created.Body != body || created.User.Login != currentUser.Login {
			return errors.New("forgejo returned an invalid canary receipt")
		}
		transport.CommentID = created.ID
		transport.Posted = true
		transport.Recovered = false
	}
	transport.ThreadURL = threadURL
	evidence := canaryEvidence{
		Version: "inktree-forgejo-notification-canary/v1", RequestID: config.RequestID,
		Repository: config.Repository, Issue: config.Issue, ReviewedHead: reviewedHead,
		OfflineGateValidated: true, AuthorAttested: true,
		LiveDispatch: false, NotificationDispatch: true, NotificationCount: 1,
		RefireSuppressed: receipt.Suppressed, Projection: projection, Rendered: rendered,
		Ledger: ledger.Snapshot(), LatestReceipt: receipt, Transport: transport,
		ExecutionTrace: runtime.trace,
	}
	if err := writeJSON(ctx, config, config.Output, outputDirectory, evidence); err != nil {
		return err
	}
	if err := completeDeliveryIntent(intentPath, config, projection.Envelope.NotificationID, transport.CommentID); err != nil {
		return err
	}
	return nil
}

func validateCanaryConfig(config canaryConfig) (*url.URL, error) {
	if config.Token == "" || config.HTTPClient == nil || !repositoryPattern.MatchString(config.Repository) ||
		config.Issue < 1 || !requestIDPattern.MatchString(config.RequestID) || config.Output == "" ||
		!beadIDPattern.MatchString(config.BeadID) || config.Verification == "" || config.Authorization == "" {
		return nil, errors.New("canary configuration is incomplete or malformed")
	}
	parsed, err := url.Parse(config.ForgejoURL)
	if err != nil || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Host == "" ||
		(parsed.Path != "" && parsed.Path != "/") ||
		(parsed.Scheme != "https" && (!config.AllowLoopback || parsed.Scheme != "http" || parsed.Hostname() != "127.0.0.1")) {
		return nil, errors.New("forgejo URL must be credential-free HTTPS or loopback HTTP")
	}
	if parsed.Scheme == "https" && strings.TrimRight(config.ForgejoURL, "/") != authorizedForgejoOrigin {
		return nil, errors.New("forgejo HTTPS origin is not the authorized canary origin")
	}
	return parsed, nil
}

func validateOfflineAdmission(ctx context.Context, config canaryConfig) (string, int, canaryAuthorization, error) {
	authorization, err := readAuthorization(config.Authorization)
	if err != nil {
		return "", 0, canaryAuthorization{}, err
	}
	if authorization.Version != "inktree-forgejo-canary-authorization/v1" ||
		authorization.RequestID != config.RequestID || authorization.BeadID != config.BeadID || authorization.Repository != config.Repository ||
		authorization.Issue != config.Issue || !bindingIDPattern.MatchString(authorization.BindingID) ||
		!commitPattern.MatchString(authorization.ReviewedHead) || !digestPattern.MatchString(authorization.VerificationSHA256) ||
		!authorization.OfflineGatesPassed ||
		!authorization.IndependentReviewPassed || !authorization.SyntheticCanaryAuthorized {
		return "", 0, canaryAuthorization{}, errors.New("synthetic canary authorization is missing, mismatched, or incomplete")
	}
	head := config.SourceHead
	headChecks := 0
	if head == "" {
		command := gitCommand(ctx, config.WorkDir, "rev-parse", "HEAD")
		output, err := command.Output()
		if err != nil {
			return "", 1, canaryAuthorization{}, fmt.Errorf("resolve canary source head: %w", err)
		}
		head = strings.TrimSpace(string(output))
		if err := validateReviewedWorktree(ctx, config); err != nil {
			return "", 2, canaryAuthorization{}, err
		}
		headChecks = 2
	}
	if head != authorization.ReviewedHead {
		return "", headChecks, canaryAuthorization{}, errors.New("canary source head does not match the independently reviewed head")
	}
	if config.SourceHead == "" {
		if err := validateRunningBuild(head); err != nil {
			return "", headChecks, canaryAuthorization{}, err
		}
	}
	verificationData, err := readReviewedEvidenceFile(ctx, config, config.Verification, "offline verification manifest")
	if err != nil {
		return "", headChecks, canaryAuthorization{}, fmt.Errorf("read offline verification manifest: %w", err)
	}
	verification := verificationManifest{}
	if err := json.Unmarshal(verificationData, &verification); err != nil {
		return "", headChecks, canaryAuthorization{}, fmt.Errorf("decode offline verification manifest: %w", err)
	}
	requiredPackage := "contrib/inktree-coordination-notifications/cmd/forgejo-canary"
	if sha256Hex(verificationData) != authorization.VerificationSHA256 ||
		verification.PolicyDispatchSwitches.Live != "off" || verification.PolicyDispatchSwitches.Notification != "off" ||
		verification.FocusedGoTests.Status != "passed" || !containsString(verification.FocusedGoTests.Packages, requiredPackage) ||
		verification.FocusedGoVet.Status != "passed" || verification.ReplayBinding.Status != "passed" ||
		verification.ReviewFindings["canary_adapter"] != "passed" ||
		verification.RepositoryChecks["check_docs"] != "passed" || verification.RepositoryChecks["check_hooks"] != "passed" ||
		verification.RepositoryChecks["json_parse"] != "passed" || verification.SyntheticCanary != "not_run_by_instruction" {
		return "", headChecks, canaryAuthorization{}, errors.New("offline verification manifest has not passed every canary prerequisite")
	}
	policyData, err := inktreecoordinationnotifications.PolicyDocument()
	if err != nil {
		return "", headChecks, canaryAuthorization{}, err
	}
	corpusData, err := inktreecoordinationnotifications.ReplayCorpusDocument()
	if err != nil {
		return "", headChecks, canaryAuthorization{}, err
	}
	replayPath := filepath.Join(filepath.Dir(config.Verification), "replay.json")
	replayData, err := readReviewedEvidenceFile(ctx, config, replayPath, "retained replay evidence")
	if err != nil {
		return "", headChecks, canaryAuthorization{}, fmt.Errorf("read retained replay evidence: %w", err)
	}
	replay := replayHashes{}
	if err := json.Unmarshal(replayData, &replay); err != nil {
		return "", headChecks, canaryAuthorization{}, fmt.Errorf("decode retained replay evidence: %w", err)
	}
	if replay.PolicyHash != sha256Hex(policyData) || replay.CorpusHash != sha256Hex(corpusData) ||
		verification.ReplayBinding.PolicyHash != replay.PolicyHash || verification.ReplayBinding.CorpusHash != replay.CorpusHash {
		return "", headChecks, canaryAuthorization{}, errors.New("offline replay hashes do not match the retained policy and corpus")
	}
	return head, headChecks, authorization, nil
}

func gitCommand(ctx context.Context, dir string, arguments ...string) *exec.Cmd {
	command := exec.CommandContext(ctx, "git", arguments...)
	command.Dir = dir
	return command
}

func readAuthorization(path string) (canaryAuthorization, error) {
	if err := validatePrivateFile(path, 0o400, "synthetic canary authorization"); err != nil {
		return canaryAuthorization{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return canaryAuthorization{}, fmt.Errorf("read synthetic canary authorization: %w", err)
	}
	authorization := canaryAuthorization{}
	if err := decodeStrict(data, &authorization); err != nil {
		return canaryAuthorization{}, fmt.Errorf("decode synthetic canary authorization: %w", err)
	}
	return authorization, nil
}

func validateReviewedWorktree(ctx context.Context, config canaryConfig) error {
	rootCommand := gitCommand(ctx, config.WorkDir, "rev-parse", "--show-toplevel")
	rootData, err := rootCommand.Output()
	if err != nil {
		return fmt.Errorf("resolve reviewed worktree: %w", err)
	}
	root := strings.TrimSpace(string(rootData))
	physicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return fmt.Errorf("resolve physical reviewed worktree: %w", err)
	}
	for index, path := range []string{config.Output, config.Authorization} {
		var physical string
		if index == 0 {
			physical, err = prospectivePhysicalPath(path)
		} else {
			physical, err = filepath.EvalSymlinks(path)
		}
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(physicalRoot, physical)
		if err != nil {
			return err
		}
		if relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return errors.New("canary authorization and output must be outside the reviewed worktree")
		}
	}
	statusCommand := gitCommand(ctx, root, "status", "--porcelain=v1", "--untracked-files=all", "--no-renames", "-z")
	statusData, err := statusCommand.Output()
	if err != nil {
		return fmt.Errorf("inspect reviewed worktree: %w", err)
	}
	for _, entry := range bytes.Split(statusData, []byte{0}) {
		if len(entry) == 0 {
			continue
		}
		return fmt.Errorf("reviewed source worktree is dirty: %q", string(entry))
	}
	return nil
}

func prospectivePhysicalPath(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if info, err := os.Lstat(absolute); err == nil {
		if !info.Mode().IsRegular() {
			return "", errors.New("canary output must be a regular file when it already exists")
		}
		return filepath.EvalSymlinks(absolute)
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(absolute))
	if err != nil {
		return "", errors.New("canary output parent must already exist and resolve outside the reviewed worktree")
	}
	return filepath.Join(parent, filepath.Base(absolute)), nil
}

func readReviewedEvidenceFile(ctx context.Context, config canaryConfig, path, label string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", label, err)
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("inspect %s: %w", label, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s must be a regular file", label)
	}
	if config.SourceHead != "" {
		return io.ReadAll(io.LimitReader(file, 1<<20))
	}
	physical, err := filepath.EvalSymlinks(fmt.Sprintf("/proc/self/fd/%d", file.Fd()))
	if err != nil {
		return nil, fmt.Errorf("resolve opened %s: %w", label, err)
	}
	rootCommand := gitCommand(ctx, config.WorkDir, "rev-parse", "--show-toplevel")
	rootData, err := rootCommand.Output()
	if err != nil {
		return nil, fmt.Errorf("resolve reviewed worktree for %s: %w", label, err)
	}
	root, err := filepath.EvalSymlinks(strings.TrimSpace(string(rootData)))
	if err != nil {
		return nil, err
	}
	relative, err := filepath.Rel(root, physical)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("%s must resolve inside the reviewed worktree", label)
	}
	trackedCommand := gitCommand(ctx, root, "ls-files", "--error-unmatch", "--", relative)
	if err := trackedCommand.Run(); err != nil {
		return nil, fmt.Errorf("%s must be tracked at the reviewed head", label)
	}
	return io.ReadAll(io.LimitReader(file, 1<<20))
}

func openStableOutputDirectory(ctx context.Context, config canaryConfig) (*os.File, error) {
	absolute, err := filepath.Abs(config.Output)
	if err != nil {
		return nil, err
	}
	physicalParent, err := filepath.EvalSymlinks(filepath.Dir(absolute))
	if err != nil {
		return nil, errors.New("canary output parent must already exist")
	}
	directory, err := os.Open(physicalParent)
	if err != nil {
		return nil, fmt.Errorf("open stable canary output directory: %w", err)
	}
	failed := true
	defer func() {
		if failed {
			_ = directory.Close()
		}
	}()
	if err := validateStableOutputDirectory(ctx, config, directory); err != nil {
		return nil, err
	}
	failed = false
	return directory, nil
}

func validateStableOutputDirectory(ctx context.Context, config canaryConfig, directory *os.File) error {
	info, err := directory.Stat()
	if err != nil || !info.IsDir() {
		return errors.New("stable canary output parent is not a directory")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Getuid()) || info.Mode().Perm()&0o077 != 0 {
		return errors.New("stable canary output parent must be private and owned by the current user")
	}
	openedPath, err := filepath.EvalSymlinks(fmt.Sprintf("/proc/self/fd/%d", directory.Fd()))
	if err != nil {
		return fmt.Errorf("resolve stable canary output directory: %w", err)
	}
	rootCommand := gitCommand(ctx, config.WorkDir, "rev-parse", "--show-toplevel")
	rootData, err := rootCommand.Output()
	if err != nil {
		return err
	}
	physicalRoot, err := filepath.EvalSymlinks(strings.TrimSpace(string(rootData)))
	if err != nil {
		return err
	}
	relative, err := filepath.Rel(physicalRoot, openedPath)
	if err != nil || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))) {
		return errors.New("stable canary output directory resolves inside the reviewed worktree")
	}
	return nil
}

func validateRunningBuild(reviewedHead string) error {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return errors.New("running canary has no Go build identity")
	}
	revision := ""
	modified := ""
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			revision = setting.Value
		case "vcs.modified":
			modified = setting.Value
		}
	}
	if revision != reviewedHead || modified != "false" {
		return errors.New("running canary binary is not an unmodified build of the reviewed head")
	}
	return nil
}

func readSecretFile(path string, mode os.FileMode, label string) (string, error) {
	if err := validatePrivateFile(path, mode, label); err != nil {
		return "", err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", label, err)
	}
	value := strings.TrimSpace(string(data))
	if value == "" || strings.ContainsAny(value, "\r\n") {
		return "", fmt.Errorf("%s is empty or malformed", label)
	}
	return value, nil
}

func validatePrivateFile(path string, mode os.FileMode, label string) error {
	if path == "" {
		return fmt.Errorf("%s path is required", label)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect %s: %w", label, err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || info.Mode().Perm() != mode || !ok || stat.Uid != uint32(os.Getuid()) {
		return fmt.Errorf("%s must be a regular mode-%04o file owned by the current user", label, mode)
	}
	return nil
}

func validatePriorEvidence(prior canaryEvidence, projection coordinationnotify.Projection, rendered, reviewedHead, eventID, threadURL string, production bool) error {
	if prior.Version != "inktree-forgejo-notification-canary/v1" || !prior.AuthorAttested ||
		prior.LiveDispatch || !prior.NotificationDispatch || !prior.OfflineGateValidated ||
		prior.ReviewedHead != reviewedHead || prior.NotificationCount != 1 || prior.Transport.CommentID == 0 || prior.Transport.ThreadURL != threadURL ||
		prior.ExecutionTrace.GitHeadChecks < 0 || prior.ExecutionTrace.GasCityClientLinked ||
		prior.ExecutionTrace.RouteCapabilityLinked || prior.ExecutionTrace.WorkerCapabilityLinked ||
		prior.Rendered != rendered || prior.Projection.Envelope == nil || prior.Projection.Envelope.NotificationID != projection.Envelope.NotificationID {
		return errors.New("prior canary evidence failed semantic validation")
	}
	priorProjection, _ := json.Marshal(prior.Projection)
	currentProjection, _ := json.Marshal(projection)
	if !bytes.Equal(priorProjection, currentProjection) || len(prior.Ledger.Receipts) < 1 || len(prior.Ledger.Receipts) > 2 {
		return errors.New("prior canary projection or ledger does not match the current decision")
	}
	first := prior.Ledger.Receipts[0]
	if first.Suppressed || first.EventID != eventID || first.Channel != projection.Envelope.Channel || first.Attempt != 1 || first.ElapsedSeconds != 0 {
		return errors.New("prior canary ledger has an invalid accepted receipt")
	}
	if len(prior.Ledger.Receipts) == 2 {
		second := prior.Ledger.Receipts[1]
		if !prior.RefireSuppressed || !second.Suppressed || second.SuppressionCause != "duplicate_event" ||
			second.EventID != eventID || second.Channel != projection.Envelope.Channel || second.Attempt != 1 {
			return errors.New("prior canary ledger has an invalid refire receipt")
		}
	} else if prior.RefireSuppressed {
		return errors.New("prior canary claims a refire without its suppression receipt")
	}
	if prior.Transport.Posted == prior.Transport.Recovered {
		return errors.New("prior delivery must identify exactly one posted or recovered Forgejo receipt")
	}
	expectedWrites := 0
	if prior.Transport.Posted {
		expectedWrites = 1
	}
	if prior.ExecutionTrace.ForgejoCommentWrites != expectedWrites || prior.ExecutionTrace.ForgejoReadCalls != len(prior.Ledger.Receipts)*3 {
		return errors.New("prior canary evidence has an invalid Forgejo execution trace")
	}
	if production && prior.ExecutionTrace.GitHeadChecks != len(prior.Ledger.Receipts)*2 {
		return errors.New("prior canary evidence has an invalid reviewed-head trace")
	}
	latest := prior.Ledger.Receipts[len(prior.Ledger.Receipts)-1]
	latestData, _ := json.Marshal(latest)
	retainedLatestData, _ := json.Marshal(prior.LatestReceipt)
	if !bytes.Equal(latestData, retainedLatestData) {
		return errors.New("prior canary latest receipt does not match its ledger")
	}
	return nil
}

func acquireCanaryLock(config canaryConfig) (func(), error) {
	lockPath, err := canaryStatePath(config, ".lock")
	if err != nil {
		return nil, err
	}
	fd, err := syscall.Open(lockPath, syscall.O_CREAT|syscall.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open canary lock: %w", err)
	}
	file := os.NewFile(uintptr(fd), lockPath)
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("acquire canary lock: %w", err)
	}
	return func() {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		_ = file.Close()
	}, nil
}

func canaryIntentPath(config canaryConfig) (string, error) {
	return canaryStatePath(config, ".intent.json")
}

func canaryStatePath(config canaryConfig, suffix string) (string, error) {
	account, err := user.LookupId(fmt.Sprint(os.Getuid()))
	if err != nil {
		return "", fmt.Errorf("resolve canary state owner: %w", err)
	}
	if account.HomeDir == "" {
		return "", errors.New("canary state owner has no home directory")
	}
	directory := filepath.Join(account.HomeDir, ".local", "state", "gascity-forgejo-canary")
	if err := ensureStateDirectory(account.HomeDir, ".local", "state", "gascity-forgejo-canary"); err != nil {
		return "", fmt.Errorf("create canary state directory: %w", err)
	}
	if err := validatePrivateDirectory(directory); err != nil {
		return "", err
	}
	parsed, err := url.Parse(config.ForgejoURL)
	if err != nil {
		return "", err
	}
	parsed.Path = ""
	canonicalOrigin := strings.TrimRight(parsed.String(), "/")
	target := strings.Join([]string{canonicalOrigin, config.Repository, fmt.Sprint(config.Issue), config.RequestID}, "\x00")
	targetHash := sha256.Sum256([]byte(target))
	return filepath.Join(directory, hex.EncodeToString(targetHash[:])+suffix), nil
}

func ensureStateDirectory(root string, elements ...string) error {
	current := root
	for _, element := range elements {
		next := filepath.Join(current, element)
		if err := os.Mkdir(next, 0o700); err == nil {
			if err := syncDirectory(current); err != nil {
				return err
			}
		} else if !errors.Is(err, os.ErrExist) {
			return err
		}
		info, err := os.Lstat(next)
		if err != nil {
			return err
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !info.IsDir() || !ok || stat.Uid != uint32(os.Getuid()) || info.Mode().Perm()&0o022 != 0 {
			return fmt.Errorf("canary state ancestor %s is not an owner-controlled directory", next)
		}
		current = next
	}
	return nil
}

func validatePrivateDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || info.Mode().Perm()&0o077 != 0 || !ok || stat.Uid != uint32(os.Getuid()) {
		return errors.New("canary state directory must be private and owned by the current user")
	}
	return nil
}

func readDeliveryIntent(path string, config canaryConfig, notificationID string) (*deliveryIntent, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read canary delivery intent: %w", err)
	}
	intent := deliveryIntent{}
	if err := decodeStrict(data, &intent); err != nil {
		return nil, fmt.Errorf("decode canary delivery intent: %w", err)
	}
	if intent.Version != "inktree-forgejo-delivery-intent/v1" || intent.RequestID != config.RequestID ||
		intent.BeadID != config.BeadID || intent.Repository != config.Repository || intent.Issue != config.Issue ||
		intent.NotificationID != notificationID ||
		(intent.State != "started" && intent.State != "completed") ||
		(intent.State == "started" && intent.CommentID != 0) || (intent.State == "completed" && intent.CommentID == 0) {
		return nil, errors.New("canary delivery intent does not match the authorized target")
	}
	return &intent, nil
}

func createDeliveryIntent(path string, config canaryConfig, notificationID string) error {
	intent := deliveryIntent{
		Version: "inktree-forgejo-delivery-intent/v1", State: "started", RequestID: config.RequestID, BeadID: config.BeadID,
		Repository: config.Repository, Issue: config.Issue, NotificationID: notificationID,
	}
	data, err := json.Marshal(intent)
	if err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create durable canary delivery intent: %w", err)
	}
	if _, err := file.Write(append(data, '\n')); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := syncDirectory(filepath.Dir(path)); err != nil {
		return err
	}
	return nil
}

func completeDeliveryIntent(path string, config canaryConfig, notificationID string, commentID int64) error {
	if commentID == 0 {
		return errors.New("cannot complete canary delivery without a Forgejo receipt")
	}
	intent := deliveryIntent{
		Version: "inktree-forgejo-delivery-intent/v1", State: "completed", RequestID: config.RequestID, BeadID: config.BeadID,
		Repository: config.Repository, Issue: config.Issue, NotificationID: notificationID, CommentID: commentID,
	}
	data, err := json.Marshal(intent)
	if err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".canary-completed-*.tmp")
	if err != nil {
		return fmt.Errorf("create completed canary state: %w", err)
	}
	tempName := temp.Name()
	defer func() { _ = os.Remove(tempName) }()
	if _, err := temp.Write(append(data, '\n')); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tempName, path); err != nil {
		return fmt.Errorf("retain completed canary state: %w", err)
	}
	return syncDirectory(filepath.Dir(path))
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open canary state directory for sync: %w", err)
	}
	defer func() { _ = directory.Close() }()
	if err := directory.Sync(); err != nil {
		return fmt.Errorf("sync canary state directory: %w", err)
	}
	return nil
}

func eventIDFor(envelope *coordinationnotify.Envelope) string {
	return "evt-" + envelope.NotificationID
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func (runtime *canaryRuntime) forgeJSON(ctx context.Context, config canaryConfig, method, path string, body, output any) error {
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(data)
	}
	switch {
	case method == http.MethodGet:
		runtime.trace.ForgejoReadCalls++
	case method == http.MethodPost && strings.HasSuffix(path, "/comments"):
		runtime.trace.ForgejoCommentWrites++
	default:
		return errors.New("canary attempted an unauthorized Forgejo operation")
	}
	request, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(config.ForgejoURL, "/")+path, reader)
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "token "+config.Token)
	request.Header.Set("Accept", "application/json")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := config.HTTPClient.Do(request)
	if err != nil {
		return err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		return fmt.Errorf("forgejo API %s %s returned HTTP %d", method, path, response.StatusCode)
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, 1<<20))
	if err := decoder.Decode(output); err != nil {
		return err
	}
	return nil
}

func decodeStrict(data []byte, output any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(output); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return errors.New("trailing JSON content")
	}
	return nil
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func writeJSON(ctx context.Context, config canaryConfig, path string, stableDirectory *os.File, value any) (retErr error) {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("encode canary evidence: %w", err)
	}
	data = append(data, '\n')
	directory := filepath.Dir(path)
	if stableDirectory != nil {
		if err := validateStableOutputDirectory(ctx, config, stableDirectory); err != nil {
			return err
		}
		directory = fmt.Sprintf("/proc/self/fd/%d", stableDirectory.Fd())
	}
	temp, err := os.CreateTemp(directory, ".forgejo-canary-*.tmp")
	if err != nil {
		return fmt.Errorf("create canary evidence temp: %w", err)
	}
	tempName := temp.Name()
	defer func() {
		if err := os.Remove(tempName); err != nil && !errors.Is(err, os.ErrNotExist) && retErr == nil {
			retErr = err
		}
	}()
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	destination := path
	if stableDirectory != nil {
		destination = filepath.Join(directory, filepath.Base(path))
	}
	if err := os.Rename(tempName, destination); err != nil {
		return err
	}
	if stableDirectory != nil {
		if err := stableDirectory.Sync(); err != nil {
			return fmt.Errorf("sync canary evidence directory: %w", err)
		}
	}
	return nil
}
