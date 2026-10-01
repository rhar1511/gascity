package main

import (
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/spf13/cobra"
)

type bdStoreBridgeCreateRequest struct {
	Title       string            `json:"title"`
	Type        string            `json:"type,omitempty"`
	Priority    *int              `json:"priority,omitempty"`
	Labels      []string          `json:"labels,omitempty"`
	ParentID    string            `json:"parent_id,omitempty"`
	Ref         string            `json:"ref,omitempty"`
	Needs       []string          `json:"needs,omitempty"`
	Description string            `json:"description,omitempty"`
	Assignee    string            `json:"assignee,omitempty"`
	From        string            `json:"from,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty"`
}

type bdStoreBridgeUpdateRequest struct {
	Title        *string           `json:"title,omitempty"`
	Status       *string           `json:"status,omitempty"`
	Type         *string           `json:"type,omitempty"`
	Priority     *int              `json:"priority,omitempty"`
	Description  *string           `json:"description,omitempty"`
	ParentID     *string           `json:"parent_id,omitempty"`
	Assignee     *string           `json:"assignee,omitempty"`
	Labels       []string          `json:"labels,omitempty"`
	RemoveLabels []string          `json:"remove_labels,omitempty"`
	Metadata     map[string]string `json:"metadata,omitempty"`
}

type bdStoreBridgeBead struct {
	ID          string            `json:"id"`
	Title       string            `json:"title"`
	Status      string            `json:"status"`
	Type        string            `json:"type"`
	Priority    *int              `json:"priority,omitempty"`
	CreatedAt   time.Time         `json:"created_at"`
	Assignee    string            `json:"assignee,omitempty"`
	From        string            `json:"from,omitempty"`
	ParentID    string            `json:"parent_id,omitempty"`
	Ref         string            `json:"ref,omitempty"`
	Needs       []string          `json:"needs,omitempty"`
	Description string            `json:"description,omitempty"`
	Labels      []string          `json:"labels,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty"`
	Revision    int64             `json:"revision"`
}

func newBdStoreBridgeCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:                "bd-store-bridge <op> [args...]",
		Short:              "Internal bd-backed exec store bridge",
		Hidden:             true,
		DisableFlagParsing: true,
		RunE: func(_ *cobra.Command, args []string) error {
			op, opArgs, dir, host, port, user, err := parseBdStoreBridgeCommandArgs(args)
			if err != nil {
				fmt.Fprintf(stderr, "gc bd-store-bridge: %v\n", err) //nolint:errcheck
				return errExit
			}
			if err := runBdStoreBridge(op, opArgs, dir, host, port, user, os.Stdin, stdout); err != nil {
				fmt.Fprintf(stderr, "gc bd-store-bridge: %v\n", err) //nolint:errcheck
				return errExit
			}
			return nil
		},
	}
	return cmd
}

func parseBdStoreBridgeCommandArgs(args []string) (op string, opArgs []string, dir, host, port, user string, err error) {
	user = "root"
	i := 0
	for i < len(args) {
		arg := args[i]
		if arg == "--" {
			i++
			break
		}
		if !strings.HasPrefix(arg, "-") {
			break
		}
		name, value, hasValue := strings.Cut(arg, "=")
		if !hasValue {
			if i+1 >= len(args) {
				return "", nil, "", "", "", "", fmt.Errorf("flag %s requires a value", name)
			}
			value = args[i+1]
			i++
		}
		switch name {
		case "--dir":
			dir = value
		case "--host":
			host = value
		case "--port":
			port = value
		case "--user":
			user = value
		default:
			return "", nil, "", "", "", "", fmt.Errorf("unknown bridge flag %s", name)
		}
		i++
	}
	if i >= len(args) {
		return "", nil, "", "", "", "", fmt.Errorf("usage: bd-store-bridge --dir <dir> --host <host> --port <port> <op> [args...]")
	}
	return args[i], args[i+1:], dir, host, port, user, nil
}

func bdStoreBridgePassword() string {
	password := strings.TrimSpace(os.Getenv("GC_DOLT_PASSWORD"))
	if password == "" {
		password = strings.TrimSpace(os.Getenv("BEADS_DOLT_PASSWORD"))
	}
	return password
}

func runBdStoreBridge(op string, args []string, dir, host, port, user string, stdin io.Reader, stdout io.Writer) error {
	if strings.TrimSpace(dir) == "" {
		return fmt.Errorf("missing --dir")
	}
	if strings.TrimSpace(host) == "" {
		return fmt.Errorf("missing --host")
	}
	if strings.TrimSpace(port) == "" {
		return fmt.Errorf("missing --port")
	}
	privateRead := strings.HasPrefix(op, "private-")
	if privateRead {
		if os.Getenv("GC_BD_STORE_BRIDGE_PRIVATE") != "1" {
			return fmt.Errorf("private bridge read requires internal exec-store mode")
		}
		op = strings.TrimPrefix(op, "private-")
		switch op {
		case "get", "list", "ready", "children", "list-by-label":
		default:
			return fmt.Errorf("unsupported private bridge operation %q", op)
		}
	}
	password := bdStoreBridgePassword()
	env := bdStoreBridgeEnv(dir, host, port, user, password)
	// Bridge operations can trigger bd hooks that recursively invoke gc. Pin
	// those callbacks to this exact executable so an ambient GC_BIN cannot
	// cross the city boundary or select a different gc installation.
	if err := pinBdGCEnvironment(env); err != nil {
		return fmt.Errorf("resolve invoking gc executable: %w", err)
	}
	store := beads.NewBdStore(dir, beads.ExecCommandRunnerWithEnv(env))
	switch op {
	case "create":
		var req bdStoreBridgeCreateRequest
		if err := decodeJSON(stdin, &req); err != nil {
			return err
		}
		if err := session.ValidateUnownedRequestMetadata(req.Metadata); err != nil {
			return fmt.Errorf("protected session request metadata: %w", err)
		}
		created, err := store.Create(beads.Bead{
			Title:       req.Title,
			Type:        req.Type,
			Priority:    req.Priority,
			Labels:      req.Labels,
			ParentID:    req.ParentID,
			Ref:         req.Ref,
			Needs:       req.Needs,
			Description: req.Description,
			Assignee:    req.Assignee,
			From:        req.From,
			Metadata:    req.Metadata,
		})
		if err != nil {
			return err
		}
		return writeJSON(stdout, bridgeBead(created))
	case "get":
		if len(args) < 1 {
			return fmt.Errorf("usage: get <id>")
		}
		bead, err := store.Get(args[0])
		if err != nil {
			return err
		}
		return writeJSON(stdout, bridgeReadBead(bead, privateRead))
	case "update":
		if len(args) < 1 {
			return fmt.Errorf("usage: update <id>")
		}
		var req bdStoreBridgeUpdateRequest
		if err := decodeJSON(stdin, &req); err != nil {
			return err
		}
		if err := session.ValidateUnownedRequestMetadata(req.Metadata); err != nil {
			return fmt.Errorf("protected session request metadata: %w", err)
		}
		opts := beads.UpdateOpts{
			Title:        req.Title,
			Status:       req.Status,
			Type:         req.Type,
			Priority:     req.Priority,
			Description:  req.Description,
			ParentID:     req.ParentID,
			Assignee:     req.Assignee,
			Labels:       req.Labels,
			RemoveLabels: req.RemoveLabels,
			Metadata:     req.Metadata,
		}
		return updateBdStoreBridge(store, args[0], opts)
	case "close":
		if len(args) < 1 {
			return fmt.Errorf("usage: close <id>")
		}
		closed := "closed"
		return updateBdStoreBridge(store, args[0], beads.UpdateOpts{Status: &closed})
	case "reopen":
		if len(args) < 1 {
			return fmt.Errorf("usage: reopen <id>")
		}
		open := "open"
		return updateBdStoreBridge(store, args[0], beads.UpdateOpts{Status: &open})
	case "list":
		query := beads.ListQuery{AllowScan: true}
		for _, arg := range args {
			switch {
			case strings.HasPrefix(arg, "--status="):
				query.Status = strings.TrimPrefix(arg, "--status=")
			case strings.HasPrefix(arg, "--assignee="):
				query.Assignee = strings.TrimPrefix(arg, "--assignee=")
			case strings.HasPrefix(arg, "--type="):
				query.Type = strings.TrimPrefix(arg, "--type=")
			case strings.HasPrefix(arg, "--limit="):
				parsed, err := strconv.Atoi(strings.TrimPrefix(arg, "--limit="))
				if err != nil {
					return fmt.Errorf("parse limit %q: %w", arg, err)
				}
				query.Limit = parsed
			}
		}
		items, err := store.List(query)
		if err != nil {
			return err
		}
		return writeJSON(stdout, bridgeReadBeads(items, privateRead))
	case "ready":
		items, err := beads.HandlesFor(store).Live.Ready()
		if err != nil {
			return err
		}
		return writeJSON(stdout, bridgeReadBeads(items, privateRead))
	case "children":
		if len(args) < 1 {
			return fmt.Errorf("usage: children <parent-id>")
		}
		items, err := store.Children(args[0], beads.IncludeClosed)
		if err != nil {
			return err
		}
		return writeJSON(stdout, bridgeReadBeads(items, privateRead))
	case "list-by-label":
		if len(args) < 1 {
			return fmt.Errorf("usage: list-by-label <label> [limit]")
		}
		limit := 0
		if len(args) > 1 && strings.TrimSpace(args[1]) != "" {
			parsed, err := strconv.Atoi(args[1])
			if err != nil {
				return fmt.Errorf("parse limit %q: %w", args[1], err)
			}
			limit = parsed
		}
		items, err := store.ListByLabel(args[0], limit, beads.IncludeClosed)
		if err != nil {
			return err
		}
		return writeJSON(stdout, bridgeReadBeads(items, privateRead))
	case "set-metadata":
		if len(args) < 2 {
			return fmt.Errorf("usage: set-metadata <id> <key>")
		}
		if beadmeta.IsGenericMutationReservedKey(args[1]) {
			return fmt.Errorf("protected session lifecycle metadata: %w", session.ErrRequestConflict)
		}
		value, err := io.ReadAll(stdin)
		if err != nil {
			return fmt.Errorf("read stdin: %w", err)
		}
		return updateBdStoreBridge(store, args[0], beads.UpdateOpts{Metadata: map[string]string{args[1]: string(value)}})
	case "delete":
		return deleteBdStoreBridge(store, args)
	case "dep-add":
		if len(args) < 3 {
			return fmt.Errorf("usage: dep-add <issue-id> <depends-on-id> <type>")
		}
		return store.DepAdd(args[0], args[1], args[2])
	case "dep-remove":
		if len(args) < 2 {
			return fmt.Errorf("usage: dep-remove <issue-id> <depends-on-id>")
		}
		return store.DepRemove(args[0], args[1])
	case "dep-list":
		if len(args) < 1 {
			return fmt.Errorf("usage: dep-list <id> [direction]")
		}
		direction := ""
		if len(args) > 1 {
			direction = args[1]
		}
		deps, err := store.DepList(args[0], direction)
		if err != nil {
			return err
		}
		return writeJSON(stdout, deps)
	default:
		return fmt.Errorf("unsupported operation %q", op)
	}
}

func updateBdStoreBridge(store beads.Store, id string, opts beads.UpdateOpts) error {
	current, err := store.Get(id)
	if err != nil {
		return err
	}
	return updateBdStoreBridgeAtRevision(store, current, opts)
}

func updateBdStoreBridgeAtRevision(store beads.Store, current beads.Bead, opts beads.UpdateOpts) error {
	if opts.Title == nil && opts.Status == nil && opts.Type == nil && opts.Priority == nil &&
		opts.Description == nil && opts.ParentID == nil && opts.Assignee == nil && len(opts.Labels) == 0 && len(opts.RemoveLabels) == 0 && len(opts.Metadata) == 0 {
		return store.Update(current.ID, opts)
	}
	if err := session.GuardGenericMutation(current, opts); err != nil {
		return err
	}
	writer, ok := beads.ConditionalWriterFor(store)
	if !ok || !beads.InspectConditionalWrites(store).Capable || current.Revision == 0 {
		return beads.ErrConditionalWriteUnsupported
	}
	return writer.UpdateIfMatch(current.ID, current.Revision, opts)
}

// parseFencedBatchClose recognizes only the batch-close options the typed
// mutation can preserve. Other forms retain the fail-closed passthrough guard.
func parseFencedBatchClose(args []string) (ids []string, reason string, jsonOutput, ok bool) {
	if len(args) == 0 || args[0] != "close" {
		return nil, "", false, false
	}
	for i := 1; i < len(args); i++ {
		switch {
		case args[i] == "--json":
			jsonOutput = true
		case args[i] == "--reason" || args[i] == "-r":
			i++
			if i == len(args) {
				return nil, "", false, false
			}
			reason = args[i]
		case strings.HasPrefix(args[i], "--reason="):
			reason = strings.TrimPrefix(args[i], "--reason=")
		case strings.HasPrefix(args[i], "-"):
			return nil, "", false, false
		default:
			ids = append(ids, args[i])
		}
	}
	return ids, reason, jsonOutput, len(ids) > 1
}

// closeBdBatchAtRevisions retains the batched exact-ID read while fencing each
// close. All rows are checked before the first mutation; a race after that
// point stops the remaining writes and reports the successfully closed prefix.
func closeBdBatchAtRevisions(store beads.Store, observed map[string]beads.Bead, ids []string, reason string) ([]beads.Bead, error) {
	if store == nil {
		return nil, beads.ErrConditionalWriteUnsupported
	}
	closed := "closed"
	opts := beads.UpdateOpts{Status: &closed}
	if reason != "" {
		opts.Metadata = map[string]string{"close_reason": reason}
	}
	writer, ok := beads.ConditionalWriterFor(store)
	if !ok || !beads.InspectConditionalWrites(store).Capable {
		return nil, beads.ErrConditionalWriteUnsupported
	}
	for _, id := range ids {
		b, found := observed[id]
		if !found || b.ID != id || b.Revision == 0 {
			return nil, fmt.Errorf("%s: %w", id, beads.ErrConditionalWriteUnsupported)
		}
		if err := session.GuardGenericMutation(b, opts); err != nil {
			return nil, fmt.Errorf("%s: %w", id, err)
		}
	}
	var written []beads.Bead
	for _, id := range ids {
		b := observed[id]
		if err := writer.UpdateIfMatch(id, b.Revision, opts); err != nil {
			return written, fmt.Errorf("%s after %d close(s): %w", id, len(written), err)
		}
		b.Status = closed
		if reason != "" {
			b.Metadata = maps.Clone(b.Metadata)
			if b.Metadata == nil {
				b.Metadata = make(map[string]string)
			}
			b.Metadata["close_reason"] = reason
		}
		written = append(written, b)
	}
	return written, nil
}

func deleteBdStoreBridge(store beads.Store, args []string) error {
	if len(args) != 2 {
		return fmt.Errorf("usage: delete <id> <revision>")
	}
	revision, err := strconv.ParseInt(args[1], 10, 64)
	if err != nil || revision < 0 {
		return fmt.Errorf("invalid delete revision %q", args[1])
	}
	writer, ok := beads.ConditionalWriterFor(store)
	if !ok || !beads.InspectConditionalWrites(store).Capable {
		return beads.ErrConditionalWriteUnsupported
	}
	b, err := store.Get(args[0])
	if err != nil {
		return err
	}
	if b.Revision != revision {
		return &beads.PreconditionFailedError{ID: args[0], Expected: revision, Current: b.Revision}
	}
	if session.HasRequestEvidence(b) {
		return session.ErrRequestEvidenceRetained
	}
	return writer.DeleteIfMatch(args[0], revision)
}

func bdStoreBridgeEnv(dir, host, port, user, password string) map[string]string {
	env := map[string]string{}
	for _, key := range []string{
		"BEADS_BACKEND",
		"BEADS_DIR",
		"BEADS_CREDENTIALS_FILE",
		"BEADS_DOLT_AUTO_START",
		"BEADS_DOLT_DATABASE",
		"BEADS_DOLT_PASSWORD",
		"BEADS_DOLT_SERVER_DATABASE",
		"BEADS_DOLT_SERVER_HOST",
		"BEADS_DOLT_SERVER_PORT",
		"BEADS_DOLT_SERVER_USER",
		"BD_EXPORT_AUTO",
		"GC_BEADS",
		"GC_BEADS_BACKEND",
		"GC_BEADS_PREFIX",
		"GC_DOLT_DATABASE",
		"GC_DOLT_HOST",
		"GC_DOLT_PASSWORD",
		"GC_DOLT_PORT",
		"GC_DOLT_USER",
	} {
		env[key] = ""
	}
	env["BEADS_DIR"] = dir + "/.beads"
	if bdStoreBridgeUsesDoltliteBackend(dir) {
		env["GC_BEADS_BACKEND"] = "doltlite"
		env["BEADS_BACKEND"] = "doltlite"
	} else {
		env["GC_DOLT_HOST"] = host
		env["BEADS_DOLT_SERVER_HOST"] = host
		env["GC_DOLT_PORT"] = port
		env["BEADS_DOLT_SERVER_PORT"] = port
		env["GC_DOLT_USER"] = user
		env["BEADS_DOLT_SERVER_USER"] = user
		env["GC_DOLT_PASSWORD"] = password
		env["BEADS_DOLT_PASSWORD"] = password
		env["BEADS_DOLT_AUTO_START"] = "0"
	}
	env["BD_EXPORT_AUTO"] = "false"
	return env
}

func bdStoreBridgeUsesDoltliteBackend(dir string) bool {
	if strings.EqualFold(strings.TrimSpace(os.Getenv("GC_BEADS_BACKEND")), "doltlite") ||
		strings.EqualFold(strings.TrimSpace(os.Getenv("BEADS_BACKEND")), "doltlite") {
		return true
	}
	data, err := os.ReadFile(filepath.Join(dir, ".beads", "metadata.json"))
	if err != nil {
		return false
	}
	var meta struct {
		Backend  string `json:"backend"`
		Database string `json:"database"`
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(meta.Backend), "doltlite") ||
		strings.EqualFold(strings.TrimSpace(meta.Database), "doltlite")
}

func decodeJSON(r io.Reader, dest any) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return fmt.Errorf("read stdin: %w", err)
	}
	payload := strings.TrimSpace(string(data))
	if payload == "" {
		payload = "{}"
	}
	if err := json.Unmarshal([]byte(payload), dest); err != nil {
		return fmt.Errorf("parse stdin JSON: %w", err)
	}
	return nil
}

func writeJSON(w io.Writer, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("marshal JSON: %w", err)
	}
	if _, err := fmt.Fprintln(w, string(data)); err != nil {
		return fmt.Errorf("write stdout: %w", err)
	}
	return nil
}

func bridgeBeads(items []beads.Bead) []bdStoreBridgeBead {
	return bridgeReadBeads(items, false)
}

func bridgeReadBeads(items []beads.Bead, private bool) []bdStoreBridgeBead {
	out := make([]bdStoreBridgeBead, 0, len(items))
	for _, item := range items {
		out = append(out, bridgeReadBead(item, private))
	}
	return out
}

func bridgeBead(item beads.Bead) bdStoreBridgeBead {
	return bridgeReadBead(item, false)
}

func bridgeReadBead(item beads.Bead, private bool) bdStoreBridgeBead {
	metadata := beadmeta.RedactGenericMetadata(item.Metadata)
	if private {
		metadata = item.Metadata
	}
	return bdStoreBridgeBead{
		ID:          item.ID,
		Title:       item.Title,
		Status:      item.Status,
		Type:        item.Type,
		Priority:    item.Priority,
		CreatedAt:   item.CreatedAt,
		Assignee:    item.Assignee,
		From:        item.From,
		ParentID:    item.ParentID,
		Ref:         item.Ref,
		Needs:       item.Needs,
		Description: item.Description,
		Labels:      item.Labels,
		Metadata:    metadata,
		Revision:    item.Revision,
	}
}
