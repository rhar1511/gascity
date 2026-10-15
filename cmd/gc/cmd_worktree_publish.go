package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/spf13/cobra"
)

type worktreePublishResult struct {
	SchemaVersion  string `json:"schema_version"`
	OK             bool   `json:"ok"`
	WorkID         string `json:"work_id"`
	SourceStoreRef string `json:"source_store_ref"`
	ReceiptID      string `json:"receipt_id"`
	Revision       int64  `json:"revision"`
	Replayed       bool   `json:"replayed"`
}

var worktreePublishTarget = resolveWorktreePublishTarget

func resolveWorktreePublishTarget() (*api.Client, error) {
	remote, isRemote, _, err := resolveWriteTarget()
	if err != nil || isRemote {
		return remote, err
	}
	cityPath, err := resolveCity()
	if err != nil {
		return nil, err
	}
	client, reason := supervisorFallthroughAPIClient(cityPath)
	if client == nil {
		return nil, fmt.Errorf("workspace publication requires a running mutable controller API: %s", reason)
	}
	return client, nil
}

func newWorktreePublishCmd(stdout, stderr io.Writer) *cobra.Command {
	var requestPath, requestSHA string
	var jsonOutput bool
	command := &cobra.Command{
		Use: "publish", Short: "Publish verified native ownership for an admitted source",
		Long: `Publish the exact request saved by gc worktree prepare.

Supply the saved request file and its exact-byte SHA-256 digest. The command
validates that snapshot without refreshing its source revision, transition
head, or native creation attempt. The controller independently verifies
signed admission and genuine native ownership before publication.
After an ambiguous response, retry with the same file and digest.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			request, err := readWorktreePublishRequest(requestPath, requestSHA)
			if err != nil {
				fmt.Fprintf(stderr, "gc worktree publish: %v\n", err) //nolint:errcheck
				return errExit
			}
			client, err := worktreePublishTarget()
			if err != nil {
				fmt.Fprintf(stderr, "gc worktree publish: %v\n", err) //nolint:errcheck
				return errExit
			}
			receipt, err := client.PublishLifecycleWorkspace(cmd.Context(), request)
			if err != nil {
				fmt.Fprintf(stderr, "gc worktree publish: %v; retain the exact source snapshot and creation attempt\n", err) //nolint:errcheck
				return errExit
			}
			if jsonOutput {
				return writeCLIJSONLineOrErr(stdout, stderr, "gc worktree publish", worktreePublishResult{SchemaVersion: "1", OK: true, WorkID: receipt.WorkID, SourceStoreRef: request.SourceStoreRef, ReceiptID: receipt.ReceiptID, Revision: receipt.Revision, Replayed: receipt.Replayed})
			}
			_, err = fmt.Fprintf(stdout, "Published native workspace for %s in %s (receipt %s)\n", receipt.WorkID, request.SourceStoreRef, receipt.ReceiptID)
			return err
		},
	}
	command.Flags().StringVar(&requestPath, "request-file", "", "immutable request saved by gc worktree prepare (required)")
	command.Flags().StringVar(&requestSHA, "request-sha256", "", "exact-byte SHA-256 returned by prepare (required)")
	command.Flags().BoolVar(&jsonOutput, "json", false, "output JSON")
	_ = command.MarkFlagRequired("request-file")
	_ = command.MarkFlagRequired("request-sha256")
	return command
}

// readWorktreePublishRequest binds decoding to the exact saved bytes. The
// digest pins retry inputs; it is not admission or native ownership authority.
func readWorktreePublishRequest(path, expectedSHA string) (api.LifecycleWorkspaceSubmitRequest, error) {
	var request api.LifecycleWorkspaceSubmitRequest
	expected, err := hex.DecodeString(expectedSHA)
	if err != nil || len(expected) != sha256.Size || path == "" {
		return request, fmt.Errorf("saved request path and SHA-256 digest are required")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return request, fmt.Errorf("read saved workspace request: %w", err)
	}
	actual := sha256.Sum256(data)
	if !bytes.Equal(actual[:], expected) {
		return request, fmt.Errorf("saved workspace request digest differs; retain original request and digest")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return request, fmt.Errorf("decode saved workspace request: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return request, fmt.Errorf("saved workspace request must contain one JSON value")
	}
	if err := request.Validate(); err != nil {
		return request, fmt.Errorf("invalid saved workspace request: %w", err)
	}
	return request, nil
}
