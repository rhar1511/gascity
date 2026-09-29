package main

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/spf13/cobra"
)

func newSessionRequestCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{Use: "request", Short: "Submit, inspect, and acknowledge tracked session requests"}
	get := &cobra.Command{Use: "get <session-id> <request-id>", Short: "Read the server's durable request receipt", Args: cobra.ExactArgs(2), RunE: func(_ *cobra.Command, args []string) error {
		c, err := sessionRequestClient()
		if err != nil {
			return err
		}
		receipt, err := c.GetSessionRequest(args[0], args[1])
		if err != nil {
			return err
		}
		return writeSessionRequestReceiptJSON(stdout, receipt)
	}}
	var generation int
	submit := &cobra.Command{Use: "submit <session-id> <request-id> <message...>", Short: "Submit a request to an exact live execution", Args: cobra.MinimumNArgs(3), RunE: func(_ *cobra.Command, args []string) error {
		if generation <= 0 {
			return fmt.Errorf("a positive --generation is required")
		}
		c, err := sessionRequestClient()
		if err != nil {
			return err
		}
		receipt, err := c.SubmitSessionRequest(args[0], args[1], generation, strings.Join(args[2:], " "))
		if err != nil {
			return err
		}
		return writeSessionRequestReceiptJSON(stdout, receipt)
	}}
	submit.Flags().IntVar(&generation, "generation", 0, "exact intended execution generation")
	ack := &cobra.Command{Use: "ack <request-id>", Short: "Acknowledge receipt from this session execution", Long: "Acknowledge receipt using GC_SESSION_ID, GC_RUNTIME_EPOCH, and GC_INSTANCE_TOKEN from this execution. An acknowledgement does not verify the requested effect.", Args: cobra.ExactArgs(1), RunE: func(_ *cobra.Command, args []string) error {
		id, token := os.Getenv("GC_SESSION_ID"), os.Getenv("GC_INSTANCE_TOKEN")
		generation, err := strconv.Atoi(os.Getenv("GC_RUNTIME_EPOCH"))
		if err != nil || generation <= 0 || id == "" || token == "" {
			return fmt.Errorf("acknowledgement requires this execution's GC_SESSION_ID, GC_RUNTIME_EPOCH, and GC_INSTANCE_TOKEN")
		}
		c, err := sessionRequestClient()
		if err != nil {
			return err
		}
		receipt, err := c.AcknowledgeSessionRequest(id, args[0], generation, token)
		if err != nil {
			return err
		}
		return writeSessionRequestReceiptJSON(stdout, receipt)
	}}
	cmd.SetErr(stderr)
	cmd.AddCommand(get, submit, ack)
	return cmd
}

// Request receipts are server-authoritative. A transport failure cannot fall
// through to a local store and silently establish a different request history.
func sessionRequestClient() (*api.Client, error) {
	cityPath, err := resolveCity()
	if err != nil {
		return nil, err
	}
	client := apiClient(cityPath)
	if client == nil {
		return nil, fmt.Errorf("tracked session requests require the Gas City server")
	}
	return client, nil
}

func writeSessionRequestReceiptJSON(stdout io.Writer, receipt api.SessionRequestReceipt) error {
	return writeCLIJSONLine(stdout, struct {
		SchemaVersion string `json:"schema_version"`
		OK            bool   `json:"ok"`
		api.SessionRequestReceipt
	}{SchemaVersion: "1", OK: true, SessionRequestReceipt: receipt})
}
