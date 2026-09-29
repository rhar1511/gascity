package main

import (
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

type controllerReconcileJSON struct {
	SchemaVersion string `json:"schema_version"`
	OK            bool   `json:"ok"`
	Status        string `json:"status"`
	CityPath      string `json:"city_path"`
}

func newControllerCmd(stdout, _ io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "controller",
		Short: "Request controller reconciliation",
	}
	cmd.AddCommand(newControllerReconcileCmd(stdout, resolveCommandCity,
		func(cityPath, command string) ([]byte, error) {
			return sendControllerCommandWithReadTimeout(cityPath, command, 5*time.Second)
		}))
	return cmd
}

func newControllerReconcileCmd(
	stdout io.Writer,
	resolveCity func([]string) (string, error),
	send func(string, string) ([]byte, error),
) *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "reconcile",
		Short: "Request a tick from the running local controller",
		Long: `Request a reconciliation tick using the running local controller's
existing configuration and policy. A successful reply acknowledges the request;
it does not establish session readiness, useful progress, or completed recovery.
Requests may be coalesced with a pending tick.`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			cityPath, err := resolveCity(nil)
			if err != nil {
				return fmt.Errorf("gc controller reconcile: %w", err)
			}
			reply, err := send(cityPath, "poke")
			if err != nil {
				return fmt.Errorf("gc controller reconcile: requesting tick: %w", err)
			}
			if strings.TrimSpace(string(reply)) != "ok" {
				return fmt.Errorf("gc controller reconcile: unexpected controller acknowledgement %q", reply)
			}
			if jsonOut {
				return writeCLIJSONLine(stdout, controllerReconcileJSON{
					SchemaVersion: "1", OK: true, Status: "requested", CityPath: cityPath,
				})
			}
			_, err = fmt.Fprintln(stdout, "Controller reconciliation requested; effects pending verification.")
			return err
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit a JSON request acknowledgement")
	return cmd
}
