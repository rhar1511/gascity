package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/spf13/cobra"
)

type githubPRBackfillOptions struct {
	monitorName   string
	jsonOutput    bool
	includeClean  bool
	timeout       time.Duration
	createRepairs bool
}

type githubPRBackfillResult struct {
	SchemaVersion string               `json:"schema_version"`
	Target        string               `json:"target"`
	Queue         api.PRActionQueue    `json:"queue"`
	Actions       []api.PRActionResult `json:"actions"`
}

func newGitHubCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use: "github", Short: "GitHub integration commands", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	cmd.AddCommand(newGitHubPRCmd(stdout, stderr))
	return cmd
}

func newGitHubPRCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use: "pr", Short: "Read and act on the central PR queue", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}
	cmd.AddCommand(newGitHubPRBackfillCmd(stdout, stderr), newGitHubPRActionCmd(stdout, stderr))
	return cmd
}

func newGitHubPRBackfillCmd(stdout, stderr io.Writer) *cobra.Command {
	opts := githubPRBackfillOptions{timeout: 45 * time.Second}
	cmd := &cobra.Command{
		Use:   "backfill [monitor-name]",
		Short: "Read the Gas City server's PR queue and policy verdicts",
		Long: `Read the Gas City server's PR queue and policy verdicts.

The server supplies repository revisions, policy versions, evidence and permitted
actions. By default, show items with an available action; --all includes blocked
items. --create-repair-beads submits only server-permitted prepare actions with
stable revision-specific idempotency keys. Prepared work is not dispatched by
this command. A server failure never falls back to local policy or ledger writes.
JSON schema version 2 contains the server queue and verified action receipts.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				opts.monitorName = args[0]
			}
			if err := doGitHubPRBackfill(cmd.Context(), opts, stdout, stderr); err != nil {
				fmt.Fprintf(stderr, "gc github pr backfill: %v\n", err) //nolint:errcheck // best-effort diagnostic
				return err
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&opts.jsonOutput, "json", false, "emit JSON")
	cmd.Flags().BoolVar(&opts.includeClean, "all", false, "include items without available actions")
	cmd.Flags().BoolVar(&opts.createRepairs, "create-repair-beads", false, "submit server-permitted prepare actions")
	cmd.Flags().DurationVar(&opts.timeout, "timeout", opts.timeout, "server request timeout")
	return cmd
}

func githubPRActionClient() (string, *api.Client, error) {
	resolved, err := resolveContextAllowRemote()
	if err != nil {
		return "", nil, err
	}
	if resolved.Remote != nil {
		endpoint, err := url.Parse(resolved.Remote.BaseURL)
		if err != nil {
			return "", nil, fmt.Errorf("invalid remote PR server URL")
		}
		endpoint.User = nil
		endpoint.RawQuery = ""
		endpoint.ForceQuery = false
		endpoint.Fragment = ""
		endpoint.RawFragment = ""
		target := strings.TrimRight(endpoint.String(), "/") + "/v0/city/" + url.PathEscape(resolved.Remote.CityName)
		client, err := buildRemoteWriteClient(resolved.Remote)
		return target, client, err
	}
	cityPath := resolved.CityPath
	client, _ := supervisorFallthroughAPIClient(cityPath)
	if client == nil {
		return "", nil, fmt.Errorf("PR queue and actions require the Gas City server")
	}
	return cityPath, client, nil
}

func doGitHubPRBackfill(parent context.Context, opts githubPRBackfillOptions, stdout, stderr io.Writer) error {
	if opts.timeout <= 0 {
		return fmt.Errorf("--timeout must be positive")
	}
	cityPath, client, err := githubPRActionClient()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(parent, opts.timeout)
	defer cancel()
	queue, err := client.GetPRActionQueue(ctx)
	if err != nil {
		return fmt.Errorf("read central PR queue: %w", err)
	}
	if opts.createRepairs && (queue.Availability != api.PRActionAvailabilityReady || queue.PolicyState != api.PRActionSourceReady) {
		return fmt.Errorf("central PR queue is %s (policy %s); no prepare actions submitted", queue.Availability, queue.PolicyState)
	}
	if opts.monitorName != "" {
		found := false
		for _, source := range queue.Sources {
			if source.Monitor == opts.monitorName {
				found = true
			}
		}
		if !found {
			return fmt.Errorf("monitor %q is absent from the central PR queue", opts.monitorName)
		}
	}
	result := githubPRBackfillResult{SchemaVersion: "2", Target: cityPath, Queue: queue, Actions: []api.PRActionResult{}}
	result.Queue.Items = []api.PRActionQueueItem{}
	for _, item := range queue.Items {
		if opts.monitorName != "" && item.Monitor != opts.monitorName {
			continue
		}
		available := false
		for _, action := range item.Actions {
			if action.Available {
				available = true
			}
		}
		if opts.includeClean || available {
			result.Queue.Items = append(result.Queue.Items, item)
		}
		if !opts.createRepairs || !item.HasAction(api.PRActionPrepare) {
			continue
		}
		request := api.PRActionRequest{Monitor: item.Monitor, Owner: item.Owner, Repo: item.Repo, PullRequest: item.PullRequest, Action: api.PRActionPrepare, HeadSHA: item.HeadSHA, BaseSHA: item.BaseSHA, PolicyVersion: item.PolicyVersion}
		request.IdempotencyKey, err = githubPRPrepareKey(cityPath, request)
		if err != nil {
			return err
		}
		// Expose the durable key before I/O so a timeout can be retried exactly.
		fmt.Fprintf(stderr, "prepare %s/%s#%d idempotency_key=%s\n", request.Owner, request.Repo, request.PullRequest, request.IdempotencyKey) //nolint:errcheck // best-effort progress
		receipt, err := client.ExecutePRAction(ctx, request)
		if err != nil {
			return fmt.Errorf("prepare outcome unverified; retry the exact request with idempotency key %s: %w", request.IdempotencyKey, err)
		}
		if err := verifyCLIPRActionResult(request, receipt); err != nil {
			return err
		}
		result.Actions = append(result.Actions, receipt)
	}
	if opts.jsonOutput {
		return writeCLIJSONLine(stdout, result)
	}
	fmt.Fprintf(stdout, "PR queue: %s; policy=%s version=%s\n", queue.Availability, queue.PolicyState, queue.PolicyVersion) //nolint:errcheck
	for _, source := range queue.Sources {
		if source.State != api.PRActionSourceReady {
			fmt.Fprintf(stdout, "  %s: %s %s\n", source.Monitor, source.State, source.Detail) //nolint:errcheck // best-effort text output
		}
	}
	for _, item := range result.Queue.Items {
		fmt.Fprintf(stdout, "%s %s/%s#%d head=%s base=%s evidence=%s\n", item.Monitor, item.Owner, item.Repo, item.PullRequest, item.HeadSHA, item.BaseSHA, item.EvidenceState) //nolint:errcheck
		for _, action := range item.Actions {
			fmt.Fprintf(stdout, "  %s available=%t human_approval=%t: %s\n", action.Action, action.Available, action.RequiresHumanApproval, action.Reason) //nolint:errcheck // best-effort text output
		}
	}
	for _, receipt := range result.Actions {
		fmt.Fprintf(stdout, "%s: %s work=%s key=%s\n", receipt.ID, receipt.Outcome, receipt.WorkID, receipt.IdempotencyKey) //nolint:errcheck // best-effort text output
	}
	return nil
}

func githubPRPrepareKey(cityPath string, request api.PRActionRequest) (string, error) {
	encoded, err := json.Marshal(struct {
		City    string
		Request api.PRActionRequest
	}{cityPath, request})
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return "cli-prepare-" + hex.EncodeToString(digest[:]), nil
}

func newGitHubPRActionCmd(stdout, stderr io.Writer) *cobra.Command {
	var request api.PRActionRequest
	var repository string
	timeout := 45 * time.Second
	cmd := &cobra.Command{
		Use:   "action <prepare|queue_review>",
		Short: "Submit an exact revision to the central PR action API",
		Long: `Submit a prepare or queue_review action using revisions and policy from
backfill --all --json. Reuse the same idempotency key and exact arguments after
an uncertain response. queue_review requires --work-id and --attempt-id from the
server queue. GitHub merge actions remain unavailable. Output is JSON.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) (runErr error) {
			defer func() {
				if runErr != nil {
					fmt.Fprintf(stderr, "gc github pr action: %v\n", runErr) //nolint:errcheck // best-effort diagnostic
				}
			}()
			request.Action = api.PRActionKind(args[0])
			if request.Action != api.PRActionPrepare && request.Action != api.PRActionQueueReview {
				return fmt.Errorf("action must be prepare or queue_review")
			}
			parts := strings.Split(repository, "/")
			if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
				return fmt.Errorf("--repo must be owner/repository")
			}
			request.Owner, request.Repo = parts[0], parts[1]
			if request.PullRequest <= 0 || timeout <= 0 {
				return fmt.Errorf("--pr and --timeout must be positive")
			}
			if request.Action == api.PRActionQueueReview && (request.WorkID == "" || request.AttemptID == "") {
				return fmt.Errorf("queue_review requires --work-id and --attempt-id")
			}
			_, client, err := githubPRActionClient()
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
			defer cancel()
			receipt, err := client.ExecutePRAction(ctx, request)
			if err != nil {
				return fmt.Errorf("PR action outcome unverified; retry the exact request with idempotency key %s: %w", request.IdempotencyKey, err)
			}
			if err := verifyCLIPRActionResult(request, receipt); err != nil {
				return err
			}
			return writeCLIJSONLine(stdout, struct {
				SchemaVersion string `json:"schema_version"`
				api.PRActionResult
			}{SchemaVersion: "1", PRActionResult: receipt})
		},
	}
	cmd.SetErr(stderr)
	cmd.Flags().StringVar(&repository, "repo", "", "exact owner/repository from server queue")
	cmd.Flags().StringVar(&request.Monitor, "monitor", "", "server monitor name")
	cmd.Flags().IntVar(&request.PullRequest, "pr", 0, "pull request number")
	cmd.Flags().StringVar(&request.HeadSHA, "head-sha", "", "exact candidate revision")
	cmd.Flags().StringVar(&request.BaseSHA, "base-sha", "", "exact base revision")
	cmd.Flags().StringVar(&request.PolicyVersion, "policy-version", "", "server policy version")
	cmd.Flags().StringVar(&request.IdempotencyKey, "idempotency-key", "", "stable key reused for retries of this exact request")
	cmd.Flags().StringVar(&request.WorkID, "work-id", "", "exact work record from the queue")
	cmd.Flags().StringVar(&request.AttemptID, "attempt-id", "", "exact immutable attempt from the queue")
	cmd.Flags().DurationVar(&timeout, "timeout", timeout, "server request timeout")
	for _, name := range []string{"repo", "monitor", "pr", "head-sha", "base-sha", "policy-version", "idempotency-key"} {
		_ = cmd.MarkFlagRequired(name)
	}
	return cmd
}

func verifyCLIPRActionResult(request api.PRActionRequest, result api.PRActionResult) error {
	if result.Status != api.PRActionStatusVerified {
		return fmt.Errorf("PR action %s has status %s; outcome remains unverified (key %s)", result.ID, result.Status, request.IdempotencyKey)
	}
	if result.ID == "" || result.IdempotencyKey != request.IdempotencyKey || result.Action != request.Action || result.Monitor != request.Monitor || result.Owner != request.Owner || result.Repo != request.Repo || result.PullRequest != request.PullRequest || result.HeadSHA != request.HeadSHA || result.BaseSHA != request.BaseSHA || result.PolicyVersion != request.PolicyVersion || (request.WorkID != "" && result.WorkID != request.WorkID) || result.AttemptID != request.AttemptID {
		return fmt.Errorf("PR action receipt does not match the exact request (key %s)", request.IdempotencyKey)
	}
	expected := api.PRActionOutcomePrepared
	if request.Action == api.PRActionQueueReview {
		expected = api.PRActionOutcomeReviewQueued
	}
	if result.Outcome != expected || result.WorkID == "" {
		return fmt.Errorf("PR action receipt lacks the verified %s outcome (key %s)", expected, request.IdempotencyKey)
	}
	return nil
}
