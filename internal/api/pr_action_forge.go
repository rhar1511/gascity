package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/githubmonitor"
)

const defaultGitHubRESTEndpoint = "https://api.github.com"

// GitHubPRActionForge combines the fresh GraphQL readiness reader with REST
// readback. Its token belongs to the controller environment and is never read
// from an API request. GitHub's current merge endpoint has no base-SHA
// precondition, so this adapter refuses merges instead of claiming an
// exact-base guarantee it cannot provide.
type GitHubPRActionForge struct {
	graphql  *githubmonitor.GraphQLClient
	endpoint string
	token    string
	http     *http.Client
}

// NewGitHubPRActionForge constructs a PR action adapter using the controller's
// GitHub token. Passing an empty token leaves the forge unavailable.
func NewGitHubPRActionForge(token string, opts ...GitHubPRActionForgeOption) *GitHubPRActionForge {
	f := &GitHubPRActionForge{
		endpoint: defaultGitHubRESTEndpoint,
		token:    strings.TrimSpace(token),
		http:     &http.Client{Timeout: 30 * time.Second},
	}
	if f.token != "" {
		f.graphql = githubmonitor.NewGraphQLClient(f.token)
	}
	for _, opt := range opts {
		if opt != nil {
			opt(f)
		}
	}
	return f
}

// GitHubPRActionForgeOption configures API endpoints and HTTP transport for
// tests and GitHub Enterprise installations.
type GitHubPRActionForgeOption func(*GitHubPRActionForge)

// WithGitHubPRActionEndpoints configures GitHub REST and GraphQL endpoints.
func WithGitHubPRActionEndpoints(rest, graphql string) GitHubPRActionForgeOption {
	return func(f *GitHubPRActionForge) {
		if strings.TrimSpace(rest) != "" {
			f.endpoint = strings.TrimRight(strings.TrimSpace(rest), "/")
		}
		if strings.TrimSpace(graphql) != "" {
			f.graphql = githubmonitor.NewGraphQLClient(f.token, githubmonitor.WithEndpoint(strings.TrimSpace(graphql)))
		}
	}
}

// WithGitHubPRActionHTTPClient supplies the HTTP client used for REST reads.
func WithGitHubPRActionHTTPClient(client *http.Client) GitHubPRActionForgeOption {
	return func(f *GitHubPRActionForge) {
		if client != nil {
			f.http = client
		}
	}
}

// ListOpenPullRequests reads current pull requests from GitHub GraphQL.
func (f *GitHubPRActionForge) ListOpenPullRequests(ctx context.Context, owner, repo string) ([]githubmonitor.PullRequest, error) {
	if f == nil || f.graphql == nil {
		return nil, fmt.Errorf("GitHub controller token is unavailable")
	}
	return f.graphql.ListOpenPullRequests(ctx, owner, repo)
}

// SupportsAtomicBaseBoundMerge reports whether GitHub can enforce the approved
// base and head in one atomic merge operation.
func (f *GitHubPRActionForge) SupportsAtomicBaseBoundMerge() bool {
	// GitHub's merge endpoint accepts a head SHA but no base SHA precondition.
	// A fresh read followed by that endpoint still races with base movement, so
	// this adapter deliberately does not advertise an exact-base merge.
	return false
}

// MergePullRequest refuses the current GitHub merge endpoint because it cannot
// atomically bind the approved base SHA.
func (f *GitHubPRActionForge) MergePullRequest(context.Context, string, string, int, string, string, []string) (PRActionMergeReceipt, error) {
	return PRActionMergeReceipt{}, ErrPRActionExactMergeUnavailable
}

// ReadPullRequest fetches exact pull-request state through the GitHub REST API.
func (f *GitHubPRActionForge) ReadPullRequest(ctx context.Context, owner, repo string, number int) (githubmonitor.PullRequest, error) {
	if f == nil || f.token == "" {
		return githubmonitor.PullRequest{}, fmt.Errorf("GitHub controller token is unavailable")
	}
	endpoint, err := f.pullRequestURL(owner, repo, number, "")
	if err != nil {
		return githubmonitor.PullRequest{}, err
	}
	resp, err := f.request(ctx, http.MethodGet, endpoint)
	if err != nil {
		return githubmonitor.PullRequest{}, err
	}
	defer resp.Body.Close() //nolint:errcheck // response body close is best-effort
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return githubmonitor.PullRequest{}, fmt.Errorf("GitHub pull request read returned HTTP %d", resp.StatusCode)
	}
	var wire struct {
		Number         int    `json:"number"`
		Title          string `json:"title"`
		URL            string `json:"html_url"`
		State          string `json:"state"`
		Draft          bool   `json:"draft"`
		Merged         bool   `json:"merged"`
		MergeCommitSHA string `json:"merge_commit_sha"`
		Base           struct {
			Ref string `json:"ref"`
			SHA string `json:"sha"`
		} `json:"base"`
		Head struct {
			Ref string `json:"ref"`
			SHA string `json:"sha"`
		} `json:"head"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&wire); err != nil {
		return githubmonitor.PullRequest{}, fmt.Errorf("decode GitHub pull request: %w", err)
	}
	if wire.Number != number || !validPRCommitSHA(wire.Head.SHA) || !validPRCommitSHA(wire.Base.SHA) {
		return githubmonitor.PullRequest{}, fmt.Errorf("GitHub returned an incomplete pull request revision")
	}
	return githubmonitor.PullRequest{
		Number: wire.Number, Title: wire.Title, URL: wire.URL,
		State: strings.ToUpper(strings.TrimSpace(wire.State)), IsDraft: wire.Draft, IsMerged: wire.Merged,
		MergeCommitSHA: wire.MergeCommitSHA,
		BaseRefName:    wire.Base.Ref, BaseSHA: wire.Base.SHA, HeadRefName: wire.Head.Ref, HeadSHA: wire.Head.SHA,
	}, nil
}

func (f *GitHubPRActionForge) pullRequestURL(owner, repo string, number int, suffix string) (string, error) {
	if f == nil || strings.TrimSpace(f.endpoint) == "" || strings.TrimSpace(owner) == "" || strings.TrimSpace(repo) == "" || number <= 0 {
		return "", fmt.Errorf("GitHub pull request address is incomplete")
	}
	base, err := url.Parse(f.endpoint)
	if err != nil || (base.Scheme != "https" && base.Scheme != "http") || base.Host == "" {
		return "", fmt.Errorf("GitHub REST endpoint is invalid")
	}
	if !validGitHubPathSegment(owner) || !validGitHubPathSegment(repo) {
		return "", fmt.Errorf("GitHub owner or repository name is invalid")
	}
	base.Path = strings.TrimRight(base.Path, "/") + "/repos/" + owner + "/" + repo + "/pulls/" + fmt.Sprint(number)
	if suffix != "" {
		base.Path += "/" + suffix
	}
	return base.String(), nil
}

func validGitHubPathSegment(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			continue
		}
		return false
	}
	return true
}

func (f *GitHubPRActionForge) request(ctx context.Context, method, endpoint string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+f.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	return f.http.Do(req)
}
