package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/worklifecycle"
	"github.com/gastownhall/gascity/internal/workrecord"
	"github.com/spf13/cobra"
)

// The reviewed timestamp and observed head stay fixed across signing retries.
// Admission is review material and must equal the current signed contract.
type beadsCompletionDraft struct {
	SchemaVersion          int                              `json:"schema_version"`
	StoreRef               string                           `json:"store_ref"`
	ExpectedRevision       int64                            `json:"expected_revision"`
	ExpectedTransitionHead string                           `json:"expected_transition_head"`
	Admission              worklifecycle.AdmissionReceiptV2 `json:"admission"`
	Receipt                worklifecycle.CompletionReceipt  `json:"receipt"`
}

func newBeadsCompletionCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{Use: "completion", Short: "Prepare and submit reviewed lifecycle acceptance"}
	var prepareStore, deliverable, verification string
	var record workrecord.Record
	prepare := &cobra.Command{
		Use: "prepare <bead-id>", Short: "Prepare an unsigned acceptance draft without changing work", Args: cobra.ExactArgs(1),
		RunE: func(command *cobra.Command, args []string) error {
			var submitted *workrecord.Record
			for _, name := range []string{"work-outcome", "work-commit", "work-branch", "work-verification"} {
				if command.Flags().Changed(name) {
					submitted = &record
					break
				}
			}
			draft, err := prepareBeadsCompletion(args[0], prepareStore, deliverable, verification, submitted, stderr)
			return printBeadsCompletionResult("prepare", draft, err, stdout, stderr)
		},
	}
	prepare.Flags().StringVar(&prepareStore, "store-ref", "", "exact city or rig work store")
	prepare.Flags().StringVar(&deliverable, "deliverable-ref", "", "reviewed delivered artifact reference")
	prepare.Flags().StringVar(&verification, "verification-ref", "", "reviewed verification evidence reference")
	prepare.Flags().StringVar(&record.Outcome, "work-outcome", "", "reviewed work outcome: shipped, no-op, blocked, or abandoned")
	prepare.Flags().StringVar(&record.Commit, "work-commit", "", "commit that delivered the reviewed work")
	prepare.Flags().StringVar(&record.Branch, "work-branch", "", "branch containing the reviewed commit")
	prepare.Flags().StringVar(&record.Verification, "work-verification", "", "work-record verification summary")
	var acceptStore, keyFile string
	accept := &cobra.Command{
		Use: "accept <reviewed-draft.json>", Short: "Sign reviewed acceptance and complete work through the controller", Args: cobra.ExactArgs(1),
		Long: `Verify the reviewed draft against the current signed admission, sign with
its configured acceptance authority, and submit the exact observed source head
to the controller. The key must be an owner-only Ed25519 PKCS#8 PEM file in an
owner-only directory. The key remains local and is never sent to the controller.

Retain the reviewed draft. Retrying the same draft recovers its committed
acceptance and close without another completion attempt. A controller is
required; this command has no generic metadata or status-write fallback.`,
		RunE: func(command *cobra.Command, args []string) error {
			result, err := acceptBeadsCompletion(command.Context(), args[0], acceptStore, keyFile, stderr)
			return printBeadsCompletionResult("accept", result, err, stdout, stderr)
		},
	}
	accept.Flags().StringVar(&acceptStore, "store-ref", "", "exact source store named in the reviewed draft")
	accept.Flags().StringVar(&keyFile, "key-file", "", "owner-only PKCS#8 file for the configured acceptance signer")
	cmd.AddCommand(prepare, accept)
	return cmd
}

func printBeadsCompletionResult(action string, value any, err error, stdout, stderr io.Writer) error {
	if err == nil {
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		err = encoder.Encode(value)
	}
	if err != nil {
		fmt.Fprintf(stderr, "gc beads completion %s: %v\n", action, err) //nolint:errcheck // best-effort diagnostic
		return errExit
	}
	return nil
}

func prepareBeadsCompletion(id, storeRef, deliverable, verification string, record *workrecord.Record, stderr io.Writer) (beadsCompletionDraft, error) {
	var empty beadsCompletionDraft
	if record != nil && !workrecord.ValidOutcome(record.Outcome) {
		return empty, fmt.Errorf("--work-outcome must be one of shipped, no-op, blocked, abandoned")
	}
	if !validMetadataCASToken(id, metadataCASMaxBeadIDBytes) || strings.TrimSpace(deliverable) == "" || strings.TrimSpace(verification) == "" {
		return empty, fmt.Errorf("exact work ID, --deliverable-ref and --verification-ref are required")
	}
	c, err := openBeadsAdmissionContext(storeRef, stderr)
	if err != nil {
		return empty, err
	}
	defer c.close()
	if c.cfg.Lifecycle.CompletionReceiptMaxAge == "" || c.cfg.Lifecycle.CompletionClockSkew == "" {
		return empty, fmt.Errorf("completion receipt maximum age and clock skew must be configured")
	}
	source, admission, err := readBeadsCompletionContract(c, id)
	if err != nil {
		return empty, err
	}
	if source.Status != "in_progress" || !lifecycleCompletionMutable(source) {
		return empty, fmt.Errorf("completion preparation requires claimed work without a dispatch hold or deferral")
	}
	if source.Metadata[beadmeta.LifecycleCompletionReceiptMetadataKey] != "" || source.Metadata[beadmeta.LifecycleCompletionBudgetMetadataKey] != "" {
		return empty, fmt.Errorf("work already has acceptance evidence; retry its original reviewed draft")
	}
	digest, err := worklifecycle.AdmissionDigestV2(admission)
	if err != nil {
		return empty, err
	}
	return beadsCompletionDraft{
		SchemaVersion: 1, StoreRef: storeRef, ExpectedRevision: source.Revision,
		ExpectedTransitionHead: source.Metadata[beadmeta.LifecycleTransitionHeadMetadataKey], Admission: admission,
		Receipt: worklifecycle.CompletionReceipt{
			Version: 1, WorkItemID: id, Scope: admission.Scope, AdmissionDigest: digest, WorkRecord: record,
			DeliverableRef: deliverable, VerificationRef: verification, AcceptedBy: admission.AcceptanceAuthority, AcceptedAt: time.Now().UTC().Format(time.RFC3339Nano),
		},
	}, nil
}

func readBeadsCompletionContract(c *beadsAdmissionContext, id string) (beads.Bead, worklifecycle.AdmissionReceiptV2, error) {
	var empty worklifecycle.AdmissionReceiptV2
	source, err := c.snapshot(id)
	if err != nil {
		return source, empty, err
	}
	head := source.Metadata[beadmeta.LifecycleTransitionHeadMetadataKey]
	if source.ID != id || source.Revision == 0 || head == "" || strings.TrimSpace(head) != head || len(head) > 200 || !rootStoreRefMatchesCandidate(source.Metadata[beadmeta.RootStoreRefMetadataKey], c.sourceLeg.ref) {
		return source, empty, fmt.Errorf("source has no exact admitted revision, transition head or store identity")
	}
	admission, err := worklifecycle.VerifyAdmissionReceiptV2(source, c.cfg.Lifecycle, worklifecycle.ScopeForStore(c.cityName, c.sourceLeg.ref))
	if err != nil {
		return source, empty, err
	}
	policy, err := c.policy(source, admission)
	if err != nil {
		return source, empty, err
	}
	digest, err := worklifecycle.DigestAdmissionPolicyV2(policy)
	if err != nil || digest != admission.RoutingPolicyDigest {
		return source, empty, fmt.Errorf("current route or workflow policy differs from signed admission")
	}
	if c.cfg.Lifecycle.AcceptanceAuthorities[admission.AcceptanceAuthority] == "" {
		return source, empty, fmt.Errorf("admission acceptance authority is not configured")
	}
	return source, admission, nil
}

func acceptBeadsCompletion(ctx context.Context, path, storeRef, keyFile string, stderr io.Writer) (api.LifecycleCompletionResult, error) {
	var empty api.LifecycleCompletionResult
	if keyFile == "" {
		return empty, fmt.Errorf("--key-file is required")
	}
	data, err := readBeadsAdmissionFile(path, 2<<20, false)
	if err != nil {
		return empty, err
	}
	var draft beadsCompletionDraft
	if err := decodeHostBeadsPermitJSON(data, &draft); err != nil {
		return empty, fmt.Errorf("reviewed draft is not strict completion JSON: %w", err)
	}
	if draft.SchemaVersion != 1 || draft.StoreRef != storeRef || draft.ExpectedRevision == 0 || draft.ExpectedTransitionHead == "" ||
		strings.TrimSpace(draft.ExpectedTransitionHead) != draft.ExpectedTransitionHead || len(draft.ExpectedTransitionHead) > 200 ||
		draft.Receipt.Version != 1 || draft.Receipt.Signature != "" || !validMetadataCASToken(draft.Receipt.WorkItemID, metadataCASMaxBeadIDBytes) {
		return empty, fmt.Errorf("draft must be unsigned schema 1 with an exact source store, revision and transition head")
	}
	c, err := openBeadsAdmissionContext(storeRef, stderr)
	if err != nil {
		return empty, err
	}
	defer c.close()
	source, admission, err := readBeadsCompletionContract(c, draft.Receipt.WorkItemID)
	if err != nil {
		return empty, err
	}
	digest, err := worklifecycle.AdmissionDigestV2(admission)
	if err != nil {
		return empty, err
	}
	if !reflect.DeepEqual(draft.Admission, admission) || draft.Receipt.Scope != admission.Scope || draft.Receipt.AdmissionDigest != digest || draft.Receipt.AcceptedBy != admission.AcceptanceAuthority {
		return empty, fmt.Errorf("reviewed acceptance differs from the current signed admission or its acceptance authority")
	}
	if source.Status != "in_progress" && source.Status != "closed" {
		return empty, fmt.Errorf("source is neither claimed nor already completed")
	}
	key, err := readBeadsAdmissionSigningKey(keyFile)
	if err != nil {
		return empty, fmt.Errorf("load acceptance signing key: %w", err)
	}
	defer zeroHostBeadsPermitBytes(key)
	trusted, err := base64.StdEncoding.DecodeString(c.cfg.Lifecycle.AcceptanceAuthorities[admission.AcceptanceAuthority])
	if err != nil || !bytes.Equal(trusted, key.Public().(ed25519.PublicKey)) {
		return empty, fmt.Errorf("key does not match the acceptance authority named in admission")
	}
	encoded, err := worklifecycle.SignCompletionReceipt(draft.Receipt, key)
	if err != nil {
		return empty, err
	}
	if source.Revision != draft.ExpectedRevision || source.Metadata[beadmeta.LifecycleTransitionHeadMetadataKey] != draft.ExpectedTransitionHead {
		if source.Metadata[beadmeta.LifecycleCompletionReceiptMetadataKey] != encoded || source.Metadata[beadmeta.LifecycleCompletionBudgetMetadataKey] == "" {
			return empty, fmt.Errorf("source changed since review and has no matching committed acceptance; prepare and review again")
		}
	} else if source.Status != "in_progress" || !lifecycleCompletionMutable(source) || source.Metadata[beadmeta.LifecycleCompletionReceiptMetadataKey] != "" {
		return empty, fmt.Errorf("source is no longer eligible for this fresh acceptance")
	}
	client, reason := supervisorFallthroughAPIClient(c.cityPath)
	if client == nil {
		return empty, fmt.Errorf("controller completion API is unavailable (%s)", reason)
	}
	return client.CompleteLifecycleWork(ctx, api.LifecycleCompletionSubmitRequest{
		WorkID: source.ID, SourceStoreRef: storeRef,
		ExpectedRevision: draft.ExpectedRevision, ExpectedTransitionHead: draft.ExpectedTransitionHead, CompletionReceipt: encoded,
	})
}
