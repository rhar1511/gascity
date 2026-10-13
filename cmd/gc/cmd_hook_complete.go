package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worklifecycle"
)

func newHookCompleteCmd(stdout, stderr io.Writer) *cobra.Command {
	var storeRef, outcome string
	cmd := &cobra.Command{
		Use: "complete", Short: "Complete the current protected execution step", Args: cobra.NoArgs,
		Long: `Complete the persisted current execution claim through the controller.
Requires GC_SESSION_ID, GC_INSTANCE_TOKEN and GC_RUNTIME_EPOCH. Verifies the
protected claim, current parent policy and exact workflow membership. Closes
only the step, records its outcome and returns the durable receipt as JSON.
The source still requires separately signed reviewer acceptance.

The current claim stamp is retained for retry and workflow lookup. An exact
retry returns the same receipt; a changed outcome conflicts. Requires a running
controller and a verified parent source; no generic write fallback is used.`,
		RunE: func(command *cobra.Command, _ []string) error {
			result, err := completeHookExecution(command.Context(), storeRef, outcome, stderr)
			if err == nil {
				err = json.NewEncoder(stdout).Encode(result)
			}
			if err != nil {
				fmt.Fprintf(stderr, "gc hook complete: %v\n", err) //nolint:errcheck // best-effort diagnostic
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&storeRef, "store-ref", "", "exact city or rig store containing the execution step")
	cmd.Flags().StringVar(&outcome, "outcome", "", "step outcome: pass, fail, or skipped")
	return cmd
}

func completeHookExecution(ctx context.Context, storeRef, outcome string, stderr io.Writer) (api.LifecycleCompletionResult, error) {
	var empty api.LifecycleCompletionResult
	sessionID := strings.TrimSpace(os.Getenv("GC_SESSION_ID"))
	token := strings.TrimSpace(os.Getenv("GC_INSTANCE_TOKEN"))
	epoch := strings.TrimSpace(os.Getenv("GC_RUNTIME_EPOCH"))
	if sessionID == "" || token == "" || !canonicalHookClaimEpoch(epoch) || (outcome != "pass" && outcome != "fail" && outcome != "skipped") {
		return empty, fmt.Errorf("managed session incarnation and outcome pass, fail, or skipped are required")
	}
	sessions, err := hookCurrentSessionFrontDoor()
	if err != nil {
		return empty, fmt.Errorf("open current session claim: %w", err)
	}
	info, err := sessions.Get(sessionID)
	if err != nil || info.Closed || info.InstanceToken != token || info.Generation != epoch {
		return empty, fmt.Errorf("managed session incarnation is unavailable or changed")
	}
	id, generation, err := sessions.CurrentClaim(sessionID)
	if err != nil || id == "" || !canonicalHookClaimEpoch(generation) {
		return empty, fmt.Errorf("current execution step and claim generation are required")
	}
	c, err := openBeadsAdmissionContext(storeRef, stderr)
	if err != nil {
		return empty, err
	}
	defer c.close()
	step, err := c.snapshot(id)
	if err != nil {
		return empty, fmt.Errorf("read exact execution step: %w", err)
	}
	head := step.Metadata[beadmeta.LifecycleTransitionHeadMetadataKey]
	if step.ID != id || step.Revision == 0 || head == "" || !worklifecycle.HasDurableEnrollment(step) ||
		step.Metadata[beadmeta.LifecycleAdmissionReceiptV2MetadataKey] != "" || step.Assignee != session.AssigneeIdentifier(info) ||
		step.Metadata[beadmeta.SessionIDMetadataKey] != sessionID || step.Metadata[beadmeta.ClaimGenerationMetadataKey] != generation ||
		!rootStoreRefMatchesCandidate(step.Metadata[beadmeta.RootStoreRefMetadataKey], c.sourceLeg.ref) {
		return empty, fmt.Errorf("current work is not this session's exact protected execution claim")
	}
	client, reason := supervisorFallthroughAPIClient(c.cityPath)
	if client == nil {
		return empty, fmt.Errorf("controller execution API is unavailable (%s)", reason)
	}
	result, err := client.CompleteLifecycleExecution(ctx, api.LifecycleExecutionCompletionRequest{
		LifecycleExecutionCompletionIntent: api.LifecycleExecutionCompletionIntent{ClaimGeneration: generation, Outcome: outcome},
		WorkID:                             id, SourceStoreRef: c.sourceLeg.ref, ExpectedRevision: step.Revision, ExpectedTransitionHead: head,
		SessionID: sessionID, InstanceToken: token, RuntimeEpoch: epoch,
	})
	if err != nil {
		return empty, err
	}
	if result.WorkID != id || result.ReceiptID == "" || result.Revision == 0 {
		return empty, fmt.Errorf("controller execution receipt does not match this step")
	}
	current, err := c.snapshot(id)
	if err != nil || current.ID != id || current.Revision != result.Revision || current.Status != "closed" || current.Assignee != step.Assignee ||
		current.Metadata[beadmeta.LifecycleTransitionHeadMetadataKey] != result.ReceiptID || current.Metadata[beadmeta.OutcomeMetadataKey] != outcome ||
		current.Metadata[beadmeta.SessionIDMetadataKey] != sessionID || current.Metadata[beadmeta.ClaimGenerationMetadataKey] != generation {
		return empty, fmt.Errorf("execution readback differs from the controller receipt")
	}
	currentID, currentGeneration, err := sessions.CurrentClaim(sessionID)
	if err != nil || currentID != id || currentGeneration != generation {
		return empty, fmt.Errorf("current execution claim changed during completion")
	}
	return result, nil
}
