package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/githubmonitor"
)

// PRActionPrepare and the related constants identify supported central PR
// actions and their durable state values.
const (
	PRActionPrepare     PRActionKind = "prepare"
	PRActionQueueReview PRActionKind = "queue_review"
	PRActionMerge       PRActionKind = "merge"

	PRActionAvailabilityReady      = "ready"
	PRActionAvailabilityPartial    = "partial"
	PRActionAvailabilityUnknown    = "unavailable"
	PRActionSourceReady            = "ready"
	PRActionSourceUnavailable      = "unavailable"
	PRActionEvidenceVerified       = "verified"
	PRActionEvidenceMissing        = "missing"
	PRActionEvidenceUnavailable    = "unavailable"
	PRActionEvidenceStale          = "stale"
	PRActionStatusPending          = "pending"
	PRActionStatusVerified         = "verified"
	PRActionStatusRejected         = "rejected"
	PRActionStatusUnknown          = "unknown"
	PRActionStatusFailed           = "failed"
	PRActionOutcomePrepared        = "work_prepared"
	PRActionOutcomeReviewQueued    = "review_queued"
	PRActionOutcomeMerged          = "merged"
	PRActionDiffCandidateCommit    = "candidate_commit_delta"
	PRActionWorkingTreeClean       = "clean"
	prActionRecordSource           = "api"
	prActionSourceMetadataKey      = beadmeta.PRActionSourceMetadataKey
	prActionQueueIndexMetadataKey  = beadmeta.PRActionQueueIndexMetadataKey
	prActionIdempotencyMetadataKey = beadmeta.PRActionIdempotencyMetadataKey
	prActionFingerprintMetadataKey = beadmeta.PRActionFingerprintMetadataKey
	prActionRecordMetadataKey      = beadmeta.PRActionRecordMetadataKey
	prActionTargetMetadataKey      = beadmeta.PRActionTargetMetadataKey
	prActionClaimMetadataKey       = beadmeta.PRActionClaimMetadataKey
	prActionRouteProposalKey       = beadmeta.PRActionRouteProposalMetadataKey
	prActionExternalHoldLabel      = "hold:external"
	prActionIdempotencyMaxLength   = 200
)

const prActionClaimLease = 5 * time.Minute

// ErrPRActionUnavailable and the related errors explain why a requested
// central PR action could not be safely evaluated or completed.
var (
	ErrPRActionUnavailable           = errors.New("PR action source is unavailable")
	ErrPRActionStale                 = errors.New("PR action request is stale")
	ErrPRActionUnauthorized          = errors.New("PR action requires an authenticated city-write grant")
	ErrPRActionHumanRequired         = errors.New("PR merge requires an exact human approval grant")
	ErrPRActionOutcomeUnknown        = errors.New("PR action outcome is unknown")
	ErrPRActionEvidenceMissing       = errors.New("PR action requires immutable attempt evidence")
	ErrPRActionConflict              = errors.New("PR action idempotency key conflicts with an earlier request")
	ErrPRActionInProgress            = errors.New("PR action with this idempotency key is already in progress")
	ErrPRActionExactMergeUnavailable = errors.New("forge cannot bind merge to the approved base and head atomically")
)

var prActionIDPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._:-]{7,199}$`)

// PRActionKind identifies an operation offered by the central PR queue.
type PRActionKind string

// PRActionSource reports the availability of one configured repository source.
type PRActionSource struct {
	Monitor string `json:"monitor"`
	Owner   string `json:"owner"`
	Repo    string `json:"repo"`
	Rig     string `json:"rig"`
	State   string `json:"state"`
	Detail  string `json:"detail,omitempty"`
}

// PRActionQueue is the controller's authoritative, revision-bound PR view.
type PRActionQueue struct {
	Availability  string              `json:"availability"`
	PolicyState   string              `json:"policy_state"`
	PolicyDetail  string              `json:"policy_detail,omitempty"`
	PolicyVersion string              `json:"policy_version"`
	ObservedAt    time.Time           `json:"observed_at"`
	FreshUntil    time.Time           `json:"fresh_until"`
	Sources       []PRActionSource    `json:"sources"`
	Items         []PRActionQueueItem `json:"items"`
}

// PRActionQueueItem joins a fresh forge revision with server-resolved work,
// evidence, submission receipts, and currently available actions.
type PRActionQueueItem struct {
	Monitor         string                     `json:"monitor"`
	Owner           string                     `json:"owner"`
	Repo            string                     `json:"repo"`
	PullRequest     int                        `json:"pull_request"`
	Title           string                     `json:"title"`
	URL             string                     `json:"url,omitempty"`
	BaseRefName     string                     `json:"base_ref_name"`
	HeadRefName     string                     `json:"head_ref_name,omitempty"`
	HeadSHA         string                     `json:"head_sha"`
	BaseSHA         string                     `json:"base_sha"`
	MergeState      string                     `json:"merge_state"`
	IsDraft         bool                       `json:"is_draft"`
	PolicyVersion   string                     `json:"policy_version"`
	ObservedAt      time.Time                  `json:"observed_at"`
	FreshUntil      time.Time                  `json:"fresh_until"`
	WorkRecords     []PRActionWorkRecord       `json:"work_records"`
	EvidenceState   string                     `json:"evidence_state"`
	AttemptEvidence []PRActionAttemptReference `json:"attempt_evidence"`
	ActionReceipts  []PRActionResult           `json:"action_receipts"`
	Actions         []PRActionOption           `json:"actions"`
}

// PRActionWorkRecord identifies durable repair work for a pull request.
type PRActionWorkRecord struct {
	ID              string `json:"id"`
	Status          string `json:"status"`
	Assignee        string `json:"assignee,omitempty"`
	CandidateSHA    string `json:"candidate_sha"`
	BaseSHA         string `json:"base_sha"`
	CurrentRevision bool   `json:"current_revision"`
}

// PRActionOption reports whether an action is available and its requirements.
type PRActionOption struct {
	Action                PRActionKind `json:"action"`
	Available             bool         `json:"available"`
	RequiresHumanApproval bool         `json:"requires_human_approval"`
	Reason                string       `json:"reason"`
}

// HasAction reports whether the queue currently offers action.
func (item PRActionQueueItem) HasAction(action PRActionKind) bool {
	for _, option := range item.Actions {
		if option.Action == action && option.Available {
			return true
		}
	}
	return false
}

// ActionRequiresHumanApproval reports whether action requires a separate human
// grant according to the current queue policy.
func (item PRActionQueueItem) ActionRequiresHumanApproval(action PRActionKind) bool {
	for _, option := range item.Actions {
		if option.Action == action {
			return option.RequiresHumanApproval
		}
	}
	return false
}

// PRActionAttemptReference is the compact reference returned by the immutable
// attempt-evidence store. No captured diff is copied into a PR action record.
type PRActionAttemptReference struct {
	StoreRef          string `json:"store_ref"`
	WorkID            string `json:"work_id"`
	AttemptID         string `json:"attempt_id"`
	BaseSHA           string `json:"base_sha"`
	CandidateSHA      string `json:"candidate_sha"`
	DiffSHA256        string `json:"diff_sha256"`
	DiffSource        string `json:"diff_source"`
	WorkingTreeStatus string `json:"working_tree_status"`
}

// PRActionAttemptEvidence is the exact immutable evidence record joined to a
// compact queue reference.
type PRActionAttemptEvidence struct {
	OwnerBeadID       string
	ExecutionBeadID   string
	BaseSHA           string
	CandidateSHA      string
	DiffSHA256        string
	DiffSource        string
	WorkingTreeStatus string
}

// PRActionAttemptEvidenceReader is the adapter seam for the immutable attempt
// ledger. Implementations must enumerate exact indexed attempts and perform an
// exact read; a latest-attempt fallback is forbidden.
type PRActionAttemptEvidenceReader interface {
	References(store beads.Store, storeRef, workID string) ([]PRActionAttemptReference, error)
	Read(store beads.Store, workID, attemptID string) (PRActionAttemptEvidence, error)
}

// PRActionAttemptEvidenceProvider is an optional State extension implemented
// by controller composition when the immutable evidence store is available.
// The API never constructs a reader from caller input or a mutable latest
// lookup.
type PRActionAttemptEvidenceProvider interface {
	PRActionAttemptEvidenceReader() PRActionAttemptEvidenceReader
}

// PRActionForge reads fresh pull request state and performs supported forge
// mutations. Implementations must report whether base-bound merges are atomic.
type PRActionForge interface {
	SupportsAtomicBaseBoundMerge() bool
	ListOpenPullRequests(ctx context.Context, owner, repo string) ([]githubmonitor.PullRequest, error)
	MergePullRequest(ctx context.Context, owner, repo string, number int, headSHA, baseSHA string, requiredChecks []string) (PRActionMergeReceipt, error)
	ReadPullRequest(ctx context.Context, owner, repo string, number int) (githubmonitor.PullRequest, error)
}

// PRActionMergeReceipt is a forge adapter's verified receipt for an atomic
// exact-base merge. The server accepts it only when every revision matches the
// signed approval and subsequent readback confirms the same merge commit.
type PRActionMergeReceipt struct {
	HeadSHA        string
	BaseSHA        string
	MergeCommitSHA string
}

// PRActionServiceOptions supplies trusted controller dependencies for the
// central PR action service.
type PRActionServiceOptions struct {
	State         State
	Forge         PRActionForge
	Evidence      PRActionAttemptEvidenceReader
	HumanVerifier *PRHumanGrantVerifier
	Policy        *TrustedPRActionPolicy
	Now           func() time.Time
}

// PRActionService implements the controller-owned PR queue and action ledger.
type PRActionService struct {
	state         State
	forge         PRActionForge
	evidence      PRActionAttemptEvidenceReader
	humanVerifier *PRHumanGrantVerifier
	policy        *TrustedPRActionPolicy
	now           func() time.Time
	mu            sync.Mutex // serializes idempotency admission in the single city controller
}

// NewPRActionService constructs a service from trusted controller dependencies.
func NewPRActionService(opts PRActionServiceOptions) *PRActionService {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &PRActionService{
		state: opts.State, forge: opts.Forge, evidence: opts.Evidence,
		humanVerifier: opts.HumanVerifier, policy: opts.Policy, now: now,
	}
}

// Queue returns current forge state joined with trusted work and evidence.
func (s *PRActionService) Queue(ctx context.Context) (PRActionQueue, error) {
	if s == nil {
		return PRActionQueue{Availability: PRActionAvailabilityUnknown}, ErrPRActionUnavailable
	}
	observed := s.now().UTC()
	queue := PRActionQueue{
		Availability: PRActionAvailabilityUnknown,
		PolicyState:  PRActionSourceUnavailable,
		ObservedAt:   observed,
		FreshUntil:   observed.Add(30 * time.Second),
		Sources:      []PRActionSource{},
		Items:        []PRActionQueueItem{},
	}
	if s.state == nil {
		return queue, ErrPRActionUnavailable
	}
	if s.policy == nil || s.policy.City != s.state.CityName() || s.policy.Version == "" {
		queue.PolicyDetail = "signed controller PR policy is unavailable or invalid"
		return queue, nil
	}
	queue.PolicyState = PRActionSourceReady
	queue.PolicyVersion = s.policy.Version
	monitors := append([]PRActionPolicyMonitor(nil), s.policy.Monitors...)
	if len(monitors) == 0 {
		queue.PolicyState = PRActionSourceUnavailable
		queue.PolicyDetail = "trusted controller PR policy has no monitors"
		return queue, nil
	}
	if s.forge == nil {
		for _, monitor := range monitors {
			queue.Sources = append(queue.Sources, PRActionSource{
				Monitor: monitor.Name, Owner: monitor.Owner, Repo: monitor.Repo, Rig: monitor.Rig,
				State: PRActionSourceUnavailable, Detail: "controller GitHub credential is unavailable",
			})
		}
		return queue, nil
	}
	readySources := 0
	for _, monitor := range monitors {
		githubMonitor := monitor.githubMonitor()
		source := PRActionSource{
			Monitor: monitor.Name, Owner: monitor.Owner, Repo: monitor.Repo, Rig: monitor.Rig, State: PRActionSourceReady,
		}
		store := s.state.BeadStore(source.Rig)
		if store == nil {
			source.State = PRActionSourceUnavailable
			source.Detail = "configured work-record store is unavailable"
			queue.Sources = append(queue.Sources, source)
			continue
		}
		if !prActionAdmissionStoreReady(store) {
			source.State = PRActionSourceUnavailable
			source.Detail = "work-record store cannot provide atomic durable action admission"
			queue.Sources = append(queue.Sources, source)
			continue
		}
		prs, err := s.forge.ListOpenPullRequests(ctx, source.Owner, source.Repo)
		if err != nil {
			source.State = PRActionSourceUnavailable
			source.Detail = "fresh forge state is unavailable"
			queue.Sources = append(queue.Sources, source)
			continue
		}
		readySources++
		queue.Sources = append(queue.Sources, source)
		sourceItemsStart := len(queue.Items)
		for _, pr := range prs {
			if !monitorAllowsBase(githubMonitor, pr.BaseRefName) {
				continue
			}
			if pr.Number <= 0 || !validPRCommitSHA(pr.HeadSHA) || !validPRCommitSHA(pr.BaseSHA) {
				continue // incomplete forge records never become actionable
			}
			works, err := listPRWorkRecords(store, source.Owner, source.Repo, pr.Number)
			if err != nil {
				queue.Sources[len(queue.Sources)-1].State = PRActionSourceUnavailable
				queue.Sources[len(queue.Sources)-1].Detail = "durable work records could not be read"
				readySources--
				queue.Items = queue.Items[:sourceItemsStart]
				break
			}
			item := PRActionQueueItem{
				Monitor: source.Monitor, Owner: source.Owner, Repo: source.Repo, PullRequest: pr.Number,
				Title: pr.Title, URL: pr.URL, BaseRefName: pr.BaseRefName, HeadRefName: pr.HeadRefName,
				HeadSHA: pr.HeadSHA, BaseSHA: pr.BaseSHA, MergeState: strings.ToUpper(strings.TrimSpace(pr.MergeStateStatus)),
				IsDraft: pr.IsDraft, PolicyVersion: s.policy.Version, ObservedAt: observed,
				FreshUntil: queue.FreshUntil, WorkRecords: []PRActionWorkRecord{}, AttemptEvidence: []PRActionAttemptReference{}, ActionReceipts: []PRActionResult{},
				EvidenceState: PRActionEvidenceMissing, Actions: []PRActionOption{},
			}
			receipts, err := listPRActionReceipts(store, monitor.Name, source.Owner, source.Repo, pr.Number)
			if err != nil {
				queue.Sources[len(queue.Sources)-1].State = PRActionSourceUnavailable
				queue.Sources[len(queue.Sources)-1].Detail = "durable action receipts could not be read or verified"
				readySources--
				queue.Items = queue.Items[:sourceItemsStart]
				break
			}
			item.ActionReceipts = receipts
			currentWork := 0
			for _, work := range works {
				candidate := strings.TrimSpace(work.Metadata["github.head_sha"])
				base := strings.TrimSpace(work.Metadata["github.base_sha"])
				current := candidate == pr.HeadSHA && base == pr.BaseSHA
				if current {
					currentWork++
				}
				item.WorkRecords = append(item.WorkRecords, PRActionWorkRecord{
					ID: work.ID, Status: work.Status, Assignee: work.Assignee,
					CandidateSHA: candidate, BaseSHA: base, CurrentRevision: current,
				})
				if current {
					refs, state := s.attemptEvidenceFor(store, source.Rig, work.ID, pr)
					switch state {
					case PRActionEvidenceUnavailable:
						item.EvidenceState = PRActionEvidenceUnavailable
					case PRActionEvidenceVerified:
						item.EvidenceState = PRActionEvidenceVerified
						item.AttemptEvidence = append(item.AttemptEvidence, refs...)
					case PRActionEvidenceStale:
						if item.EvidenceState != PRActionEvidenceMissing {
							break
						}
						item.EvidenceState = PRActionEvidenceStale
					}
				}
			}
			sort.Slice(item.WorkRecords, func(i, j int) bool { return item.WorkRecords[i].ID < item.WorkRecords[j].ID })
			sort.Slice(item.AttemptEvidence, func(i, j int) bool { return item.AttemptEvidence[i].AttemptID < item.AttemptEvidence[j].AttemptID })
			if currentWork == 0 && isActionablePR(githubMonitor, pr) {
				item.Actions = append(item.Actions, PRActionOption{Action: PRActionPrepare, Available: true, Reason: "no durable repair work is recorded for this revision"})
			}
			if item.EvidenceState == PRActionEvidenceVerified && len(item.AttemptEvidence) > 0 && !pr.IsDraft {
				item.Actions = append(item.Actions, PRActionOption{Action: PRActionQueueReview, Available: true, Reason: "server verified immutable evidence for the current revision"})
				if prMergeReady(pr, monitor.RequiredChecks) {
					merge := PRActionOption{Action: PRActionMerge, RequiresHumanApproval: true}
					if s.forge.SupportsAtomicBaseBoundMerge() {
						merge.Available = true
						merge.Reason = "checks are clear; exact human approval is still required"
					} else {
						merge.Reason = "the configured forge cannot atomically bind merge to the approved base SHA"
					}
					item.Actions = append(item.Actions, merge)
				}
			}
			queue.Items = append(queue.Items, item)
		}
	}
	sort.Slice(queue.Items, func(i, j int) bool {
		if queue.Items[i].Owner != queue.Items[j].Owner {
			return queue.Items[i].Owner < queue.Items[j].Owner
		}
		if queue.Items[i].Repo != queue.Items[j].Repo {
			return queue.Items[i].Repo < queue.Items[j].Repo
		}
		return queue.Items[i].PullRequest < queue.Items[j].PullRequest
	})
	switch {
	case queue.PolicyState != PRActionSourceReady:
		queue.Availability = PRActionAvailabilityUnknown
	case readySources == len(monitors):
		queue.Availability = PRActionAvailabilityReady
	case readySources > 0:
		queue.Availability = PRActionAvailabilityPartial
	default:
		queue.Availability = PRActionAvailabilityUnknown
	}
	return queue, nil
}

// Execute persists and runs a revision-bound action under a durable idempotency
// claim, revalidating the exact selected work and attempt before any effect.
func (s *PRActionService) Execute(ctx context.Context, request PRActionRequest, actor PRActionActor) (PRActionResult, error) {
	if s == nil || s.state == nil || s.policy == nil || s.forge == nil {
		return PRActionResult{}, ErrPRActionUnavailable
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !validPRActionIdempotencyKey(request.IdempotencyKey) {
		return PRActionResult{}, fmt.Errorf("invalid idempotency key")
	}
	if actor.CityWrite.KeyID == "" || actor.CityWrite.City != s.state.CityName() {
		return PRActionResult{}, ErrPRActionUnauthorized
	}
	if request.PolicyVersion != s.policy.Version {
		return PRActionResult{}, ErrPRActionStale
	}
	monitor, ok := findPRActionMonitor(s.policy.Monitors, request.Monitor)
	if !ok || monitor.Owner != request.Owner || monitor.Repo != request.Repo {
		return PRActionResult{}, ErrPRActionStale
	}
	store := s.state.BeadStore(monitor.Rig)
	if store == nil {
		return PRActionResult{}, ErrPRActionUnavailable
	}
	if !prActionAdmissionStoreReady(store) {
		return PRActionResult{}, ErrPRActionUnavailable
	}
	if request.Action != PRActionPrepare && request.Action != PRActionQueueReview && request.Action != PRActionMerge {
		return PRActionResult{}, fmt.Errorf("unsupported PR action %q", request.Action)
	}
	if request.Action == PRActionMerge {
		if actor.Human == nil || s.humanVerifier == nil {
			return PRActionResult{}, ErrPRActionHumanRequired
		}
		if err := s.humanVerifier.Authorize(*actor.Human, PRActionScopeMerge); err != nil {
			return PRActionResult{}, ErrPRActionHumanRequired
		}
	}
	fingerprint, err := prActionFingerprint(request, actor)
	if err != nil {
		return PRActionResult{}, err
	}
	prior, found, err := findPRActionRecord(store, request.IdempotencyKey)
	if err != nil {
		return PRActionResult{}, fmt.Errorf("read durable PR action ledger: %w", err)
	}
	if found {
		if prior.Fingerprint != fingerprint {
			return PRActionResult{}, ErrPRActionConflict
		}
		// A completed action may have changed the queue (prepare adds work,
		// merge removes the PR). Return its durable result before checking that
		// the action remains eligible, so an exact retry stays idempotent.
		if prior.Status == PRActionStatusVerified || prior.Status == PRActionStatusRejected || prior.Status == PRActionStatusFailed {
			return prior, nil
		}
		if prior.Status == PRActionStatusUnknown && request.Action == PRActionMerge {
			return s.reconcileUnknownMerge(ctx, store, prior, request)
		}
	}
	pendingPrepare := found && (prior.Status == PRActionStatusPending || prior.Status == PRActionStatusUnknown) && request.Action == PRActionPrepare
	queue, err := s.Queue(ctx)
	if err != nil || queue.Availability != PRActionAvailabilityReady {
		return PRActionResult{}, ErrPRActionUnavailable
	}
	item, ok := findPRActionItem(queue.Items, request.Monitor, request.Owner, request.Repo, request.PullRequest)
	if !ok || !samePRActionRevision(item, request, s.now()) {
		return PRActionResult{}, ErrPRActionStale
	}
	if !item.HasAction(request.Action) && !pendingPrepare {
		concurrent, exists, err := findPRActionRecord(store, request.IdempotencyKey)
		if err != nil {
			return PRActionResult{}, fmt.Errorf("recheck durable PR action ledger: %w", err)
		}
		if exists {
			if concurrent.Fingerprint != fingerprint {
				return PRActionResult{}, ErrPRActionConflict
			}
			if concurrent.Status == PRActionStatusVerified || concurrent.Status == PRActionStatusRejected || concurrent.Status == PRActionStatusFailed {
				return concurrent, nil
			}
			if request.Action == PRActionPrepare && concurrent.Action == PRActionPrepare && (concurrent.Status == PRActionStatusPending || concurrent.Status == PRActionStatusUnknown) {
				prior, found, pendingPrepare = concurrent, true, true
			}
		}
	}
	if !item.HasAction(request.Action) && !pendingPrepare {
		return PRActionResult{}, ErrPRActionStale
	}
	if pendingPrepare && !item.HasAction(PRActionPrepare) {
		if _, exists, err := findPRRepairWork(store, monitor.githubMonitor(), item); err != nil {
			return PRActionResult{}, fmt.Errorf("check durable prepared work: %w", err)
		} else if !exists {
			return PRActionResult{}, ErrPRActionStale
		}
	}
	if request.Action == PRActionPrepare {
		if request.WorkID != "" || request.AttemptID != "" {
			return PRActionResult{}, ErrPRActionStale
		}
	} else if !requestHasVerifiedAttempt(item, request) {
		return PRActionResult{}, ErrPRActionEvidenceMissing
	}
	if !found {
		prior, err = createPRActionIntent(store, request, actor, fingerprint, s.now().UTC())
		if err != nil {
			return PRActionResult{}, fmt.Errorf("persist PR action intent: %w", err)
		}
		if prior.Fingerprint != fingerprint {
			return PRActionResult{}, ErrPRActionConflict
		}
		if prior.Status == PRActionStatusVerified || prior.Status == PRActionStatusRejected || prior.Status == PRActionStatusFailed {
			return prior, nil
		}
		pendingPrepare = request.Action == PRActionPrepare && prior.Action == PRActionPrepare && (prior.Status == PRActionStatusPending || prior.Status == PRActionStatusUnknown)
	}
	lease := prActionClaimLease
	if request.Action == PRActionMerge {
		lease = 24 * time.Hour
	}
	claimToken, err := claimPRActionRecord(store, prior.ID, fingerprint, s.now().UTC(), lease)
	if err != nil {
		latest, found, readErr := findPRActionRecord(store, request.IdempotencyKey)
		if readErr == nil && found && latest.Fingerprint == fingerprint {
			if latest.Status == PRActionStatusVerified || latest.Status == PRActionStatusRejected || latest.Status == PRActionStatusFailed {
				return latest, nil
			}
			if latest.Status == PRActionStatusUnknown && request.Action == PRActionMerge {
				return latest, ErrPRActionOutcomeUnknown
			}
		}
		return prior, err
	}
	defer func() {
		if prior.Status == PRActionStatusPending || (prior.Status == PRActionStatusUnknown && request.Action != PRActionMerge) {
			_ = releasePRActionRecordClaim(store, prior.ID, claimToken)
		}
	}()

	// Re-resolve the forge and work record after the intent is durable. Staleness
	// may leave an audit record, but it can never cross into a forge mutation.
	freshQueue, err := s.Queue(ctx)
	if err != nil || freshQueue.Availability != PRActionAvailabilityReady {
		prior.Status = PRActionStatusUnknown
		prior.Detail = "source state became unavailable after action intent was stored"
		_ = persistPRActionResultClaimed(store, &prior, claimToken, s.now().UTC())
		return prior, ErrPRActionOutcomeUnknown
	}
	freshItem, ok := findPRActionItem(freshQueue.Items, request.Monitor, request.Owner, request.Repo, request.PullRequest)
	if !ok || !samePRActionRevision(freshItem, request, s.now()) || (!freshItem.HasAction(request.Action) && !pendingPrepare) {
		prior.Status = PRActionStatusRejected
		prior.Detail = "forge, work, evidence, or policy state changed before action execution"
		if err := persistPRActionResultClaimed(store, &prior, claimToken, s.now().UTC()); err != nil {
			return prior, ErrPRActionOutcomeUnknown
		}
		return prior, ErrPRActionStale
	}
	if pendingPrepare && !freshItem.HasAction(PRActionPrepare) {
		if _, exists, err := findPRRepairWork(store, monitor.githubMonitor(), freshItem); err != nil {
			prior.Status = PRActionStatusUnknown
			prior.Detail = "durable prepared work could not be verified after the action intent"
			_ = persistPRActionResultClaimed(store, &prior, claimToken, s.now().UTC())
			return prior, ErrPRActionOutcomeUnknown
		} else if !exists {
			prior.Status = PRActionStatusRejected
			prior.Detail = "prepare is no longer eligible and its durable work transition was not found"
			if err := persistPRActionResultClaimed(store, &prior, claimToken, s.now().UTC()); err != nil {
				return prior, ErrPRActionOutcomeUnknown
			}
			return prior, ErrPRActionStale
		}
	}
	if request.Action != PRActionPrepare && !requestHasVerifiedAttempt(freshItem, request) {
		prior.Status = PRActionStatusRejected
		prior.Detail = "the exact work and attempt evidence changed before action execution"
		if err := persistPRActionResultClaimed(store, &prior, claimToken, s.now().UTC()); err != nil {
			return prior, ErrPRActionOutcomeUnknown
		}
		return prior, ErrPRActionEvidenceMissing
	}
	if err := verifyPRActionRecordClaim(store, prior.ID, claimToken, s.now().UTC()); err != nil {
		return prior, err
	}

	switch request.Action {
	case PRActionPrepare:
		work, err := ensurePRRepairWork(store, monitor.githubMonitor(), freshItem)
		if err != nil {
			prior.Status = PRActionStatusUnknown
			prior.Detail = "repair work transition did not reach a verified result"
			_ = persistPRActionResultClaimed(store, &prior, claimToken, s.now().UTC())
			return prior, ErrPRActionOutcomeUnknown
		}
		prior.WorkID = work.ID
		prior.Outcome = PRActionOutcomePrepared
	case PRActionQueueReview:
		prior.Outcome = PRActionOutcomeReviewQueued
	case PRActionMerge:
		if !freshItem.ActionRequiresHumanApproval(PRActionMerge) || actor.Human == nil || s.humanVerifier.Authorize(*actor.Human, PRActionScopeMerge) != nil {
			prior.Status = PRActionStatusRejected
			prior.Detail = "human approval authority did not authorize merge"
			_ = persistPRActionResultClaimed(store, &prior, claimToken, s.now().UTC())
			return prior, ErrPRActionHumanRequired
		}
		// Record a non-retryable submission state before crossing the forge
		// boundary. If this process is interrupted after the write, retries can
		// reconcile or report unknown but can never submit the same merge again.
		prior.Status = PRActionStatusUnknown
		prior.Detail = "merge submission entered a non-retryable state before the forge mutation"
		if err := persistPRActionResultClaimed(store, &prior, claimToken, s.now().UTC()); err != nil {
			return prior, ErrPRActionOutcomeUnknown
		}
		receipt, mergeErr := s.forge.MergePullRequest(ctx, request.Owner, request.Repo, request.PullRequest, request.HeadSHA, request.BaseSHA, monitor.RequiredChecks)
		observed, readErr := s.forge.ReadPullRequest(ctx, request.Owner, request.Repo, request.PullRequest)
		if errors.Is(mergeErr, ErrPRActionStale) && readErr == nil && !observed.IsMerged {
			prior.Status = PRActionStatusRejected
			prior.Detail = "the exact base/head merge precondition changed before the forge mutation"
			if err := persistPRActionResultClaimed(store, &prior, claimToken, s.now().UTC()); err != nil {
				return prior, ErrPRActionOutcomeUnknown
			}
			return prior, ErrPRActionStale
		}
		if mergeErr == nil && receipt.HeadSHA == request.HeadSHA && receipt.BaseSHA == request.BaseSHA && validPRCommitSHA(receipt.MergeCommitSHA) && readErr == nil && observed.IsMerged && observed.HeadSHA == request.HeadSHA && observed.BaseSHA == request.BaseSHA && observed.MergeCommitSHA == receipt.MergeCommitSHA {
			prior.Status = PRActionStatusVerified
			prior.Outcome = PRActionOutcomeMerged
			prior.MergeCommitSHA = receipt.MergeCommitSHA
			prior.VerifiedAt = s.now().UTC()
			prior.Detail = "merge receipt and forge readback verify the approved base, candidate, and merge commit"
		} else {
			prior.Status = PRActionStatusUnknown
			prior.Detail = "merge effect could not be verified"
			if mergeErr != nil {
				prior.Detail = "merge call failed and the resulting forge state could not prove the effect"
			}
			if err := persistPRActionResultClaimed(store, &prior, claimToken, s.now().UTC()); err != nil {
				return prior, ErrPRActionOutcomeUnknown
			}
			return prior, ErrPRActionOutcomeUnknown
		}
	}
	if prior.Status != PRActionStatusVerified {
		prior.Status = PRActionStatusVerified
	}
	if prior.VerifiedAt.IsZero() {
		prior.VerifiedAt = s.now().UTC()
	}
	if err := persistPRActionResultClaimed(store, &prior, claimToken, s.now().UTC()); err != nil {
		return prior, ErrPRActionOutcomeUnknown
	}
	return prior, nil
}

func (s *PRActionService) reconcileUnknownMerge(ctx context.Context, store beads.Store, prior PRActionResult, request PRActionRequest) (PRActionResult, error) {
	_ = ctx
	_ = store
	_ = request
	// The merge intent stores the approved base, but head-only forge readback
	// cannot prove which base was used. Keep an outcome unknown until an exact
	// adapter receipt is available; merged=true by itself is insufficient.
	return prior, ErrPRActionOutcomeUnknown
}

// PRActionActor contains the verified city-write identity and optional
// separately authorized human identity used for one action.
type PRActionActor struct {
	CityWrite VerifiedCityWritePrincipal
	Human     *VerifiedPRPrincipal
}

// PRActionRequest identifies the exact queue item and action submitted by a
// client.
type PRActionRequest struct {
	Monitor        string       `json:"monitor"`
	Owner          string       `json:"owner"`
	Repo           string       `json:"repo"`
	PullRequest    int          `json:"pull_request"`
	Action         PRActionKind `json:"action"`
	WorkID         string       `json:"work_id,omitempty"`
	AttemptID      string       `json:"attempt_id,omitempty"`
	HeadSHA        string       `json:"head_sha"`
	BaseSHA        string       `json:"base_sha"`
	PolicyVersion  string       `json:"policy_version"`
	IdempotencyKey string       `json:"idempotency_key"`
}

// PRActionResult is the durable, idempotent result for one PR action request.
type PRActionResult struct {
	ID             string       `json:"id"`
	Action         PRActionKind `json:"action"`
	Status         string       `json:"status"`
	Outcome        string       `json:"outcome,omitempty"`
	MergeCommitSHA string       `json:"merge_commit_sha,omitempty"`
	Detail         string       `json:"detail,omitempty"`
	IdempotencyKey string       `json:"idempotency_key"`
	Fingerprint    string       `json:"-"`
	Monitor        string       `json:"monitor"`
	Owner          string       `json:"owner"`
	Repo           string       `json:"repo"`
	PullRequest    int          `json:"pull_request"`
	WorkID         string       `json:"work_id,omitempty"`
	AttemptID      string       `json:"attempt_id,omitempty"`
	HeadSHA        string       `json:"head_sha"`
	BaseSHA        string       `json:"base_sha"`
	PolicyVersion  string       `json:"policy_version"`
	ActorKeyID     string       `json:"actor_key_id"`
	ActorIssuer    string       `json:"actor_issuer,omitempty"`
	ActorSubject   string       `json:"actor_subject,omitempty"`
	CreatedAt      time.Time    `json:"created_at"`
	VerifiedAt     time.Time    `json:"verified_at,omitzero"`
}

func (s *PRActionService) attemptEvidenceFor(store beads.Store, rig, workID string, pr githubmonitor.PullRequest) ([]PRActionAttemptReference, string) {
	if s.evidence == nil {
		return nil, PRActionEvidenceUnavailable
	}
	storeRef := "rig:" + strings.TrimSpace(rig)
	refs, err := s.evidence.References(store, storeRef, workID)
	if err != nil {
		return nil, PRActionEvidenceUnavailable
	}
	if len(refs) == 0 {
		return nil, PRActionEvidenceMissing
	}
	valid := make([]PRActionAttemptReference, 0, len(refs))
	stale := false
	for _, ref := range refs {
		if ref.StoreRef != storeRef || ref.WorkID != workID || strings.TrimSpace(ref.AttemptID) == "" {
			return nil, PRActionEvidenceUnavailable
		}
		evidence, err := s.evidence.Read(store, workID, ref.AttemptID)
		if err != nil {
			return nil, PRActionEvidenceUnavailable
		}
		if evidence.OwnerBeadID != workID || evidence.ExecutionBeadID == "" || evidence.BaseSHA != ref.BaseSHA || evidence.CandidateSHA != ref.CandidateSHA || evidence.DiffSHA256 != ref.DiffSHA256 || evidence.DiffSource != ref.DiffSource || evidence.WorkingTreeStatus != ref.WorkingTreeStatus || ref.DiffSource != PRActionDiffCandidateCommit || ref.WorkingTreeStatus != PRActionWorkingTreeClean || !validPRCommitSHA(ref.BaseSHA) || !validPRCommitSHA(ref.CandidateSHA) || !validPRDiffSHA(ref.DiffSHA256) {
			return nil, PRActionEvidenceUnavailable
		}
		if ref.BaseSHA != pr.BaseSHA || ref.CandidateSHA != pr.HeadSHA {
			stale = true
			continue
		}
		valid = append(valid, ref)
	}
	if len(valid) > 0 {
		return valid, PRActionEvidenceVerified
	}
	if stale {
		return nil, PRActionEvidenceStale
	}
	return nil, PRActionEvidenceMissing
}

func prActionAdmissionStoreReady(store beads.Store) bool {
	if !beads.StableCreateIDFor(store) {
		return false
	}
	_, ok := beads.ConditionalWriterFor(store)
	return ok
}

func prActionRecordBeadID(idempotencyKey string) string {
	digest := sha256.Sum256([]byte(idempotencyKey))
	return "gc-pr-action-" + hex.EncodeToString(digest[:])
}

func prActionQueueIndexKey(monitor, owner, repo string, pullRequest int) string {
	digest := sha256.Sum256([]byte(strings.Join([]string{
		monitor, owner, repo, strconv.Itoa(pullRequest),
	}, "\x00")))
	return hex.EncodeToString(digest[:])
}

func prRepairWorkBeadID(item PRActionQueueItem) string {
	digest := sha256.Sum256([]byte(strings.Join([]string{
		item.Monitor, item.Owner, item.Repo, strconv.Itoa(item.PullRequest), item.BaseSHA, item.HeadSHA,
	}, "\x00")))
	return "gc-pr-repair-" + hex.EncodeToString(digest[:])
}

func prRepairTargetKey(item PRActionQueueItem) string {
	digest := sha256.Sum256([]byte(strings.Join([]string{
		item.Monitor, item.Owner, item.Repo, strconv.Itoa(item.PullRequest), item.BaseSHA, item.HeadSHA,
	}, "\x00")))
	return hex.EncodeToString(digest[:])
}

func listPRWorkRecords(store beads.Store, owner, repo string, number int) ([]beads.Bead, error) {
	return store.List(beads.ListQuery{
		Metadata: map[string]string{
			"source": "github-pr-monitor", "github.owner": owner, "github.repo": repo,
			"github.pr": strconv.Itoa(number),
		},
		IncludeClosed: true,
	})
}

func monitorAllowsBase(monitor config.GitHubPRMonitor, base string) bool {
	for _, configured := range monitor.BaseBranches {
		if strings.TrimSpace(configured) == strings.TrimSpace(base) {
			return true
		}
	}
	return false
}

func isActionablePR(monitor config.GitHubPRMonitor, pr githubmonitor.PullRequest) bool {
	for _, result := range githubmonitor.EvaluatePullRequests(monitor, []githubmonitor.PullRequest{pr}) {
		if result.Actionable {
			return true
		}
	}
	return false
}

func prMergeReady(pr githubmonitor.PullRequest, requiredChecks []string) bool {
	if pr.IsDraft || strings.ToUpper(strings.TrimSpace(pr.MergeStateStatus)) != "CLEAN" {
		return false
	}
	if len(pr.Checks) == 0 || len(requiredChecks) == 0 {
		return false
	}
	seen := make(map[string]bool, len(pr.Checks))
	for _, check := range pr.Checks {
		status := strings.ToUpper(strings.TrimSpace(check.Status))
		conclusion := strings.ToUpper(strings.TrimSpace(check.Conclusion))
		if status != "SUCCESS" && status != "COMPLETED" {
			return false
		}
		if conclusion != "SUCCESS" {
			return false
		}
		seen[strings.TrimSpace(check.Name)] = true
	}
	for _, required := range requiredChecks {
		if !seen[strings.TrimSpace(required)] {
			return false
		}
	}
	return true
}

func findPRActionMonitor(monitors []PRActionPolicyMonitor, name string) (PRActionPolicyMonitor, bool) {
	for _, monitor := range monitors {
		if monitor.Name == strings.TrimSpace(name) {
			return monitor, true
		}
	}
	return PRActionPolicyMonitor{}, false
}

func findPRActionItem(items []PRActionQueueItem, monitor, owner, repo string, number int) (PRActionQueueItem, bool) {
	for _, item := range items {
		if item.Monitor == monitor && item.Owner == owner && item.Repo == repo && item.PullRequest == number {
			return item, true
		}
	}
	return PRActionQueueItem{}, false
}

func samePRActionRevision(item PRActionQueueItem, request PRActionRequest, now time.Time) bool {
	return item.Monitor == request.Monitor && item.Owner == request.Owner && item.Repo == request.Repo && item.PullRequest == request.PullRequest && item.HeadSHA == request.HeadSHA && item.BaseSHA == request.BaseSHA && item.PolicyVersion == request.PolicyVersion && now.Before(item.FreshUntil)
}

func requestHasVerifiedAttempt(item PRActionQueueItem, request PRActionRequest) bool {
	if item.EvidenceState != PRActionEvidenceVerified || request.WorkID == "" || request.AttemptID == "" {
		return false
	}
	for _, ref := range item.AttemptEvidence {
		if ref.WorkID == request.WorkID && ref.AttemptID == request.AttemptID && ref.BaseSHA == request.BaseSHA && ref.CandidateSHA == request.HeadSHA {
			for _, work := range item.WorkRecords {
				if work.ID == request.WorkID && work.CurrentRevision {
					return true
				}
			}
		}
	}
	return false
}

func validPRActionIdempotencyKey(key string) bool {
	return prActionIDPattern.MatchString(strings.TrimSpace(key)) && len(key) <= prActionIdempotencyMaxLength
}

func prActionFingerprint(request PRActionRequest, actor PRActionActor) (string, error) {
	fingerprintInput := struct {
		Request PRActionRequest `json:"request"`
		KeyID   string          `json:"actor_key_id"`
		Issuer  string          `json:"actor_issuer,omitempty"`
		Subject string          `json:"actor_subject,omitempty"`
	}{Request: request, KeyID: actor.CityWrite.KeyID}
	if actor.Human != nil {
		fingerprintInput.KeyID = actor.Human.KeyID
		fingerprintInput.Issuer = actor.Human.Issuer
		fingerprintInput.Subject = actor.Human.Subject
	}
	encoded, err := json.Marshal(fingerprintInput)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func createPRActionIntent(store beads.Store, request PRActionRequest, actor PRActionActor, fingerprint string, now time.Time) (PRActionResult, error) {
	result := PRActionResult{
		ID:     prActionRecordBeadID(request.IdempotencyKey),
		Action: request.Action, Status: PRActionStatusPending, IdempotencyKey: request.IdempotencyKey,
		Fingerprint: fingerprint, Monitor: request.Monitor, Owner: request.Owner, Repo: request.Repo,
		PullRequest: request.PullRequest, WorkID: request.WorkID, AttemptID: request.AttemptID,
		HeadSHA: request.HeadSHA, BaseSHA: request.BaseSHA, PolicyVersion: request.PolicyVersion,
		ActorKeyID: actor.CityWrite.KeyID, CreatedAt: now.UTC(),
	}
	if actor.Human != nil {
		result.ActorKeyID, result.ActorIssuer, result.ActorSubject = actor.Human.KeyID, actor.Human.Issuer, actor.Human.Subject
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return PRActionResult{}, err
	}
	title := fmt.Sprintf("PR action %s %s/%s#%d", request.Action, request.Owner, request.Repo, request.PullRequest)
	created, err := store.Create(beads.Bead{
		// Durable receipts must never appear as candidate work in Ready.
		ID: result.ID, Title: title, Type: "gate", Labels: []string{"gc:pr-action"},
		Metadata: beads.StringMap{
			prActionSourceMetadataKey:      prActionRecordSource,
			prActionQueueIndexMetadataKey:  prActionQueueIndexKey(request.Monitor, request.Owner, request.Repo, request.PullRequest),
			prActionIdempotencyMetadataKey: request.IdempotencyKey,
			prActionFingerprintMetadataKey: fingerprint,
			prActionRecordMetadataKey:      string(encoded),
		},
	})
	if err != nil {
		if prior, found, readErr := findPRActionRecord(store, request.IdempotencyKey); readErr == nil && found && prior.Fingerprint == fingerprint {
			return prior, nil
		}
		return PRActionResult{}, err
	}
	if created.ID != result.ID || created.Type != "gate" {
		return PRActionResult{}, fmt.Errorf("store did not preserve the unique non-runnable PR action intent ID")
	}
	stored, err := store.Get(result.ID)
	if err != nil {
		return PRActionResult{}, err
	}
	if stored.Type != "gate" || stored.Metadata[prActionIdempotencyMetadataKey] != request.IdempotencyKey || stored.Metadata[prActionFingerprintMetadataKey] != fingerprint || stored.Metadata[prActionRecordMetadataKey] != string(encoded) {
		return PRActionResult{}, fmt.Errorf("durable PR action intent did not verify on readback")
	}
	return result, nil
}

func findPRActionRecord(store beads.Store, idempotencyKey string) (PRActionResult, bool, error) {
	rows, err := store.List(beads.ListQuery{
		Metadata:      map[string]string{prActionSourceMetadataKey: prActionRecordSource, prActionIdempotencyMetadataKey: idempotencyKey},
		IncludeClosed: true,
	})
	if err != nil {
		return PRActionResult{}, false, err
	}
	if len(rows) == 0 {
		return PRActionResult{}, false, nil
	}
	if len(rows) > 1 {
		return PRActionResult{}, false, fmt.Errorf("multiple durable action records share one idempotency key")
	}
	if rows[0].ID != prActionRecordBeadID(idempotencyKey) || rows[0].Type != "gate" {
		return PRActionResult{}, false, fmt.Errorf("PR action ledger lacks its unique non-runnable durable ID")
	}
	var result PRActionResult
	if err := json.Unmarshal([]byte(rows[0].Metadata[prActionRecordMetadataKey]), &result); err != nil {
		return PRActionResult{}, false, fmt.Errorf("decode durable action record %s: %w", rows[0].ID, err)
	}
	result.ID = rows[0].ID
	result.Fingerprint = rows[0].Metadata[prActionFingerprintMetadataKey]
	if result.IdempotencyKey != idempotencyKey || result.Fingerprint == "" || !validPRActionStatus(result.Status) || !validPRActionKind(result.Action) {
		return PRActionResult{}, false, fmt.Errorf("durable action record %s failed its request identity check", rows[0].ID)
	}
	return result, true, nil
}

func listPRActionReceipts(store beads.Store, monitor, owner, repo string, pullRequest int) ([]PRActionResult, error) {
	index := prActionQueueIndexKey(monitor, owner, repo, pullRequest)
	rows, err := store.List(beads.ListQuery{
		Metadata: map[string]string{
			prActionSourceMetadataKey: prActionRecordSource, prActionQueueIndexMetadataKey: index,
		},
		IncludeClosed: true,
	})
	if err != nil {
		return nil, err
	}
	receipts := make([]PRActionResult, 0, len(rows))
	for _, row := range rows {
		if row.Type != "gate" || row.Metadata[prActionSourceMetadataKey] != prActionRecordSource || row.Metadata[prActionQueueIndexMetadataKey] != index {
			return nil, fmt.Errorf("action receipt %q failed its durable queue index check", row.ID)
		}
		var receipt PRActionResult
		if err := json.Unmarshal([]byte(row.Metadata[prActionRecordMetadataKey]), &receipt); err != nil {
			return nil, fmt.Errorf("decode action receipt %q: %w", row.ID, err)
		}
		if receipt.ID != row.ID || receipt.ID != prActionRecordBeadID(receipt.IdempotencyKey) || receipt.Monitor != monitor || receipt.Owner != owner || receipt.Repo != repo || receipt.PullRequest != pullRequest || prActionQueueIndexKey(receipt.Monitor, receipt.Owner, receipt.Repo, receipt.PullRequest) != index {
			return nil, fmt.Errorf("action receipt %q does not match its durable queue index", row.ID)
		}
		receipt.Fingerprint = row.Metadata[prActionFingerprintMetadataKey]
		if receipt.Fingerprint == "" || !validPRActionStatus(receipt.Status) || !validPRActionKind(receipt.Action) || receipt.IdempotencyKey != row.Metadata[prActionIdempotencyMetadataKey] {
			return nil, fmt.Errorf("action receipt %q has incomplete authority or status", row.ID)
		}
		receipts = append(receipts, receipt)
	}
	sort.Slice(receipts, func(i, j int) bool {
		if !receipts[i].CreatedAt.Equal(receipts[j].CreatedAt) {
			return receipts[i].CreatedAt.Before(receipts[j].CreatedAt)
		}
		return receipts[i].ID < receipts[j].ID
	})
	return receipts, nil
}

func validPRActionStatus(status string) bool {
	switch status {
	case PRActionStatusPending, PRActionStatusVerified, PRActionStatusRejected, PRActionStatusUnknown, PRActionStatusFailed:
		return true
	default:
		return false
	}
}

func validPRActionKind(action PRActionKind) bool {
	switch action {
	case PRActionPrepare, PRActionQueueReview, PRActionMerge:
		return true
	default:
		return false
	}
}

type prActionRecordClaim struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

func claimPRActionRecord(store beads.Store, id, fingerprint string, now time.Time, lease time.Duration) (string, error) {
	if !prActionAdmissionStoreReady(store) || lease <= 0 {
		return "", ErrPRActionUnavailable
	}
	writer, ok := beads.ConditionalWriterFor(store)
	if !ok {
		return "", ErrPRActionUnavailable
	}
	for range 3 {
		row, err := store.Get(id)
		if err != nil {
			return "", fmt.Errorf("read PR action admission record: %w", err)
		}
		if row.ID != id || row.Type != "gate" || row.Metadata[prActionSourceMetadataKey] != prActionRecordSource {
			return "", ErrPRActionUnavailable
		}
		if row.Metadata[prActionFingerprintMetadataKey] != fingerprint {
			return "", ErrPRActionConflict
		}
		var result PRActionResult
		if err := json.Unmarshal([]byte(row.Metadata[prActionRecordMetadataKey]), &result); err != nil || result.ID != id || result.IdempotencyKey == "" || prActionRecordBeadID(result.IdempotencyKey) != id || !validPRActionStatus(result.Status) || !validPRActionKind(result.Action) {
			return "", ErrPRActionUnavailable
		}
		if result.Action == PRActionMerge && result.Status == PRActionStatusUnknown {
			return "", ErrPRActionOutcomeUnknown
		}
		if result.Status == PRActionStatusVerified || result.Status == PRActionStatusRejected || result.Status == PRActionStatusFailed {
			return "", ErrPRActionInProgress
		}
		if result.Status != PRActionStatusPending && result.Status != PRActionStatusUnknown {
			return "", ErrPRActionUnavailable
		}
		expected := row.Metadata[prActionClaimMetadataKey]
		if expected != "" {
			var current prActionRecordClaim
			if err := json.Unmarshal([]byte(expected), &current); err != nil || current.Token == "" || current.ExpiresAt.IsZero() {
				return "", ErrPRActionUnavailable
			}
			if now.Before(current.ExpiresAt) {
				return "", ErrPRActionInProgress
			}
		}
		random := make([]byte, 24)
		if _, err := rand.Read(random); err != nil {
			return "", fmt.Errorf("create PR action admission token: %w", err)
		}
		token := hex.EncodeToString(random)
		encoded, err := json.Marshal(prActionRecordClaim{Token: token, ExpiresAt: now.Add(lease).UTC()})
		if err != nil {
			return "", err
		}
		if err := writer.UpdateIfMatch(id, row.Revision, beads.UpdateOpts{Metadata: map[string]string{prActionClaimMetadataKey: string(encoded)}}); err != nil {
			if beads.IsPreconditionFailed(err) {
				continue
			}
			return "", fmt.Errorf("claim PR action idempotency key: %w", err)
		}
		stored, err := store.Get(id)
		if err != nil {
			return "", fmt.Errorf("verify PR action idempotency claim: %w", err)
		}
		if stored.Metadata[prActionClaimMetadataKey] == string(encoded) {
			return token, nil
		}
	}
	return "", ErrPRActionInProgress
}

func verifyPRActionRecordClaim(store beads.Store, id, token string, now time.Time) error {
	row, err := store.Get(id)
	if err != nil {
		return fmt.Errorf("read PR action claim: %w", err)
	}
	var claim prActionRecordClaim
	if err := json.Unmarshal([]byte(row.Metadata[prActionClaimMetadataKey]), &claim); err != nil || claim.Token != token || !now.Before(claim.ExpiresAt) {
		return ErrPRActionInProgress
	}
	return nil
}

func releasePRActionRecordClaim(store beads.Store, id, token string) error {
	writer, ok := beads.ConditionalWriterFor(store)
	if !ok {
		return ErrPRActionUnavailable
	}
	row, err := store.Get(id)
	if err != nil {
		return err
	}
	expected := row.Metadata[prActionClaimMetadataKey]
	var claim prActionRecordClaim
	if expected == "" || json.Unmarshal([]byte(expected), &claim) != nil || claim.Token != token {
		return nil
	}
	_, err = writer.CompareAndSetMetadataKey(id, prActionClaimMetadataKey, expected, "")
	return err
}

func persistPRActionResultClaimed(store beads.Store, result *PRActionResult, token string, now time.Time) error {
	writer, ok := beads.ConditionalWriterFor(store)
	if !ok {
		return ErrPRActionUnavailable
	}
	row, err := store.Get(result.ID)
	if err != nil {
		return err
	}
	var claim prActionRecordClaim
	if err := json.Unmarshal([]byte(row.Metadata[prActionClaimMetadataKey]), &claim); err != nil || claim.Token != token || !now.Before(claim.ExpiresAt) {
		return ErrPRActionInProgress
	}
	encoded, err := json.Marshal(result)
	if err != nil {
		return err
	}
	if err := writer.UpdateIfMatch(result.ID, row.Revision, beads.UpdateOpts{Metadata: map[string]string{
		prActionRecordMetadataKey:      string(encoded),
		prActionFingerprintMetadataKey: result.Fingerprint,
	}}); err != nil {
		return err
	}
	stored, err := store.Get(result.ID)
	if err != nil {
		return err
	}
	var verified PRActionResult
	if err := json.Unmarshal([]byte(stored.Metadata[prActionRecordMetadataKey]), &verified); err != nil {
		return err
	}
	if verified.ID != result.ID || verified.Status != result.Status || verified.Outcome != result.Outcome || verified.WorkID != result.WorkID || verified.MergeCommitSHA != result.MergeCommitSHA {
		return fmt.Errorf("durable PR action record readback mismatch")
	}
	return nil
}

func ensurePRRepairWork(store beads.Store, monitor config.GitHubPRMonitor, item PRActionQueueItem) (beads.Bead, error) {
	if existing, found, err := findPRRepairWork(store, monitor, item); err != nil {
		return beads.Bead{}, err
	} else if found {
		return existing, nil
	}
	priority := 1
	created, err := store.Create(beads.Bead{
		ID:    prRepairWorkBeadID(item),
		Title: fmt.Sprintf("Repair GitHub PR %s/%s#%d", item.Owner, item.Repo, item.PullRequest),
		Type:  "task", Priority: &priority,
		Description: fmt.Sprintf("Repair the monitored pull request at exact candidate %s against base %s. Submit this revision for review only through the central PR action API.", item.HeadSHA, item.BaseSHA),
		Labels:      []string{"github", "ci", "repair", "pr-monitor", prActionExternalHoldLabel},
		Metadata: beads.StringMap{
			"source": "github-pr-monitor", "github.owner": item.Owner, "github.repo": item.Repo,
			"github.pr": strconv.Itoa(item.PullRequest), "github.head_sha": item.HeadSHA,
			"github.base": item.BaseRefName, "github.base_sha": item.BaseSHA,
			"github.url": item.URL, "github.monitor": monitor.Name,
			prActionRouteProposalKey: monitor.RepairRoute, prActionTargetMetadataKey: prRepairTargetKey(item),
		},
	})
	if err != nil {
		if existing, found, readErr := findPRRepairWork(store, monitor, item); readErr == nil && found {
			return existing, nil
		}
		return beads.Bead{}, err
	}
	if created.ID != prRepairWorkBeadID(item) || created.Type != "task" || !hasPRActionExternalHold(created.Labels) || created.Metadata[beadmeta.RoutedToMetadataKey] != "" {
		return beads.Bead{}, errors.New("store did not preserve held, uniquely identified triage work")
	}
	verified, err := store.Get(created.ID)
	if err != nil {
		return beads.Bead{}, err
	}
	if !validPRRepairWork(verified, monitor, item) {
		return beads.Bead{}, errors.New("prepared repair work did not verify as held triage work for the requested revision")
	}
	return verified, nil
}

func findPRRepairWork(store beads.Store, monitor config.GitHubPRMonitor, item PRActionQueueItem) (beads.Bead, bool, error) {
	rows, err := store.List(beads.ListQuery{Metadata: map[string]string{
		"source": "github-pr-monitor", "github.owner": item.Owner, "github.repo": item.Repo,
		"github.pr": strconv.Itoa(item.PullRequest), "github.head_sha": item.HeadSHA,
		"github.base_sha": item.BaseSHA, "github.monitor": monitor.Name,
		prActionTargetMetadataKey: prRepairTargetKey(item),
	}, IncludeClosed: true})
	if err != nil {
		return beads.Bead{}, false, err
	}
	if len(rows) == 0 {
		return beads.Bead{}, false, nil
	}
	if len(rows) != 1 {
		return beads.Bead{}, false, fmt.Errorf("multiple repair work records share semantic target %q", prRepairTargetKey(item))
	}
	row := rows[0]
	if !validPRRepairWork(row, monitor, item) {
		return beads.Bead{}, false, errors.New("stored repair work does not verify as held or lifecycle-admitted work for the requested revision")
	}
	return row, true, nil
}

func validPRRepairWork(row beads.Bead, monitor config.GitHubPRMonitor, item PRActionQueueItem) bool {
	if row.ID != prRepairWorkBeadID(item) || row.Type != "task" || row.Metadata[prActionTargetMetadataKey] != prRepairTargetKey(item) ||
		row.Metadata["github.head_sha"] != item.HeadSHA || row.Metadata["github.base_sha"] != item.BaseSHA ||
		row.Metadata["github.base"] != item.BaseRefName || row.Metadata["github.monitor"] != monitor.Name ||
		row.Metadata[prActionRouteProposalKey] != monitor.RepairRoute {
		return false
	}
	if hasPRActionExternalHold(row.Labels) {
		return row.Metadata[beadmeta.RoutedToMetadataKey] == ""
	}
	// After explicit lifecycle admission, the signed receipt and serving route
	// are written by that separate authority. Prepare itself never removes the
	// hold or installs a serving route.
	return row.Metadata[beadmeta.RoutedToMetadataKey] == monitor.RepairRoute && row.Metadata[beadmeta.LifecycleAdmissionReceiptMetadataKey] != ""
}

func hasPRActionExternalHold(labels []string) bool {
	for _, label := range labels {
		if label == prActionExternalHoldLabel {
			return true
		}
	}
	return false
}

func validPRCommitSHA(value string) bool {
	value = strings.TrimSpace(value)
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func validPRDiffSHA(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}
