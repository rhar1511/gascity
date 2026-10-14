package main

import (
	"fmt"
	"io"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/spf13/cobra"
)

type beadsCreateResult struct {
	SchemaVersion  string `json:"schema_version"`
	OK             bool   `json:"ok"`
	BeadID         string `json:"bead_id"`
	StoreRef       string `json:"store_ref"`
	IdempotencyKey string `json:"idempotency_key"`
}

// Indirected for tests, like the other CLI API-routing seams. There is no
// direct-store or raw-bd fallback, including when the controller is unavailable.
var beadsCreateTarget = resolveBeadsCreateTarget

func resolveBeadsCreateTarget() (*api.Client, error) {
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
		return nil, fmt.Errorf("task creation requires a running mutable controller API: %s", reason)
	}
	return client, nil
}

func newBeadsCreateCmd(stdout, stderr io.Writer) *cobra.Command {
	var request api.TaskCreateRequest
	var jsonOutput bool
	cmd := &cobra.Command{
		Use:   "create <title>",
		Short: "Create an open, unassigned task through the controller API",
		Long: `Create an open, unassigned task in an explicitly named rig.

Both --rig and --idempotency-key are required. The task receives no execution
grant, owner or controller metadata. This command uses the existing typed API
and never falls back to raw bd or direct store writes. A failed or ambiguous
request must be inspected before retrying with the same key and identical input.`,
		Example: `  gc --rig compute beads create "Inspect isolated workspace" --idempotency-key=qualification-1 --json`,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			rig := cmd.Flag("rig")
			if rig == nil || !rig.Changed {
				fmt.Fprintln(stderr, "gc beads create: --rig is required") //nolint:errcheck
				return errExit
			}
			request.Rig, request.Title = rig.Value.String(), args[0]
			if err := request.Validate(); err != nil {
				fmt.Fprintf(stderr, "gc beads create: %v\n", err) //nolint:errcheck
				return errExit
			}
			client, err := beadsCreateTarget()
			if err != nil {
				fmt.Fprintf(stderr, "gc beads create: %v\n", err) //nolint:errcheck
				return errExit
			}
			row, err := client.CreateUnassignedTask(cmd.Context(), request)
			if err != nil {
				fmt.Fprintf(stderr, "gc beads create: %v (retain idempotency key %q)\n", err, request.IdempotencyKey) //nolint:errcheck
				return errExit
			}
			if jsonOutput {
				return writeCLIJSONLineOrErr(stdout, stderr, "gc beads create", beadsCreateResult{
					SchemaVersion: "1", OK: true, BeadID: row.ID,
					StoreRef: "rig:" + request.Rig, IdempotencyKey: request.IdempotencyKey,
				})
			}
			_, err = fmt.Fprintf(stdout, "Created %s in rig:%s\n", row.ID, request.Rig)
			return err
		},
	}
	cmd.Flags().StringVar(&request.Description, "description", "", "task description (no authority metadata)")
	cmd.Flags().StringVar(&request.IdempotencyKey, "idempotency-key", "", "explicit stable key for this exact task creation")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "emit the canonical task creation receipt")
	return cmd
}
