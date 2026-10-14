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
)

func newHookBeginSourceCmd(stdout, stderr io.Writer) *cobra.Command {
	var storeRef string
	cmd := &cobra.Command{
		Use: "begin-source <source-id>", Short: "Begin admitted source work while retaining the current execution step", Args: cobra.ExactArgs(1),
		Long: `Begin the admitted source of this session's currently claimed generated step.
The session's current step and generation come from its persisted claim, never
from a launcher bead ID. The controller verifies the managed incarnation,
protected step claim, signed parent policy and workflow membership before
claiming the source. The execution step remains the session's current claim.

Requires GC_SESSION_ID, GC_INSTANCE_TOKEN, GC_RUNTIME_EPOCH and a running
controller. Prints the durable source claim receipt as JSON. Exact retries
return the existing claim. No signing key or generic write fallback is used.`,
		RunE: func(command *cobra.Command, args []string) error {
			result, err := beginHookSource(command.Context(), args[0], storeRef, stderr)
			if err == nil {
				err = json.NewEncoder(stdout).Encode(result)
			}
			if err != nil {
				fmt.Fprintf(stderr, "gc hook begin-source: %v\n", err) //nolint:errcheck // best-effort diagnostic
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&storeRef, "store-ref", "", "exact city or rig store shared by the source and execution step")
	return cmd
}

func beginHookSource(ctx context.Context, id, storeRef string, stderr io.Writer) (api.LifecycleClaimResult, error) {
	var empty api.LifecycleClaimResult
	sessionID := strings.TrimSpace(os.Getenv("GC_SESSION_ID"))
	token := strings.TrimSpace(os.Getenv("GC_INSTANCE_TOKEN"))
	epoch := strings.TrimSpace(os.Getenv("GC_RUNTIME_EPOCH"))
	if !validMetadataCASToken(id, metadataCASMaxBeadIDBytes) || sessionID == "" || token == "" || !canonicalHookClaimEpoch(epoch) {
		return empty, fmt.Errorf("an exact source ID and managed session ID, instance token and runtime epoch are required")
	}
	sessions, err := hookCurrentSessionFrontDoor()
	if err != nil {
		return empty, fmt.Errorf("open current session claim: %w", err)
	}
	info, err := sessions.Get(sessionID)
	if err != nil || info.Closed || info.InstanceToken != token || info.Generation != epoch {
		return empty, fmt.Errorf("managed session incarnation is unavailable or changed")
	}
	stepID, stepGeneration, err := sessions.CurrentClaim(sessionID)
	if err != nil || stepID == "" || stepID == id || !canonicalHookClaimEpoch(stepGeneration) {
		return empty, fmt.Errorf("a distinct current execution step and claim generation are required")
	}
	c, err := openBeadsAdmissionContext(storeRef, stderr)
	if err != nil {
		return empty, err
	}
	defer c.close()
	source, err := c.snapshot(id)
	if err != nil {
		return empty, fmt.Errorf("read exact admitted source: %w", err)
	}
	head := source.Metadata[beadmeta.LifecycleTransitionHeadMetadataKey]
	if source.ID != id || source.Revision == 0 || head == "" || source.Metadata[beadmeta.LifecycleAdmissionReceiptV2MetadataKey] == "" ||
		!rootStoreRefMatchesCandidate(source.Metadata[beadmeta.RootStoreRefMetadataKey], c.sourceLeg.ref) {
		return empty, fmt.Errorf("source has no exact admitted revision, transition head or store identity")
	}
	client, reason := supervisorFallthroughAPIClient(c.cityPath)
	if client == nil {
		return empty, fmt.Errorf("controller claim API is unavailable (%s)", reason)
	}
	claim, err := client.ClaimLifecycleWork(ctx, api.LifecycleClaimSubmitRequest{
		WorkID: id, SourceStoreRef: c.sourceLeg.ref, ExpectedRevision: source.Revision, ExpectedTransitionHead: head,
		SessionID: sessionID, InstanceToken: token, RuntimeEpoch: epoch,
		ExecutionClaim: &api.LifecycleExecutionClaim{WorkID: stepID, ClaimGeneration: stepGeneration},
	})
	if err != nil {
		return empty, err
	}
	if claim.WorkID != id || claim.Actor != session.AssigneeIdentifier(info) || claim.ReceiptID == "" || !canonicalHookClaimEpoch(claim.ClaimGeneration) {
		return empty, fmt.Errorf("controller source claim does not match this work and session")
	}
	current, err := c.snapshot(id)
	if err != nil || current.ID != id || current.Status != "in_progress" || current.Assignee != claim.Actor ||
		current.Metadata[beadmeta.SessionIDMetadataKey] != sessionID || current.Metadata[beadmeta.ClaimGenerationMetadataKey] != claim.ClaimGeneration ||
		current.Metadata[beadmeta.LifecycleTransitionHeadMetadataKey] != claim.ReceiptID {
		return empty, fmt.Errorf("source claim readback differs from the controller receipt")
	}
	currentStep, currentGeneration, err := sessions.CurrentClaim(sessionID)
	if err != nil || currentStep != stepID || currentGeneration != stepGeneration {
		return empty, fmt.Errorf("current execution claim changed while beginning source work")
	}
	return claim, nil
}
