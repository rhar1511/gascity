package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worklifecycle"
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
	internalSessionWrite := strings.HasPrefix(op, "internal-")
	if internalSessionWrite {
		if os.Getenv("GC_BD_STORE_BRIDGE_PRIVATE") != "1" {
			return fmt.Errorf("internal bridge write requires internal exec-store mode")
		}
		op = strings.TrimPrefix(op, "internal-")
		switch op {
		case "create", "update", "set-metadata":
		default:
			return fmt.Errorf("unsupported internal bridge operation %q", op)
		}
	}
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
	runner := beads.ExecCommandRunnerWithEnv(env)
	store := beads.NewBdStore(dir, func(dir, name string, args ...string) ([]byte, error) {
		out, err := runner(dir, name, args...)
		if err != nil {
			return nil, bdBridgeProviderError{cause: err}
		}
		return out, nil
	})
	switch op {
	case "create":
		var req bdStoreBridgeCreateRequest
		if err := decodeJSON(stdin, &req); err != nil {
			return err
		}
		metadata := req.Metadata
		if internalSessionWrite && req.Type == session.BeadType {
			metadata = genericBridgeSessionMetadata(metadata)
		}
		if err := validateBdStoreBridgeAuthorityMetadata(metadata, nil); err != nil {
			return err
		}
		if err := session.ValidateUnownedRequestMetadata(metadata); err != nil {
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
		return writeJSON(stdout, bridgeReadBead(created, internalSessionWrite && req.Type == session.BeadType))
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
		if !internalSessionWrite {
			if err := validateBdStoreBridgeAuthorityMetadata(req.Metadata, nil); err != nil {
				return err
			}
		}
		if !internalSessionWrite {
			if err := session.ValidateUnownedRequestMetadata(req.Metadata); err != nil {
				return fmt.Errorf("protected session request metadata: %w", err)
			}
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
		return updateBdStoreBridgeForMode(store, args[0], opts, internalSessionWrite)
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
		if !internalSessionWrite || !beadmeta.IsExecutionIdentityMetadataKey(args[1]) {
			if err := session.ValidateUnownedRequestMetadata(map[string]string{args[1]: ""}); err != nil {
				return fmt.Errorf("controller-owned session lifecycle metadata: %w", err)
			}
		}
		if protectedSessionAuthorityMetadata(args[1]) {
			return fmt.Errorf("refusing controller-owned session authority metadata %q; use the signed session permission-mode API", args[1])
		}
		if sessionAuthorityOptionMetadata(args[1]) {
			current, err := store.Get(args[0])
			if err != nil {
				return err
			}
			if sessionAuthorityMetadataPreviouslyProtected(current.Metadata) {
				return fmt.Errorf("refusing controller-owned session authority metadata %q on a protected session; use the signed session permission-mode API", args[1])
			}
		}
		if err := validateBridgeGenericMutation(store, args[0]); err != nil {
			return err
		}
		value, err := io.ReadAll(stdin)
		if err != nil {
			return fmt.Errorf("read stdin: %w", err)
		}
		return updateBdStoreBridgeForMode(store, args[0], beads.UpdateOpts{Metadata: map[string]string{args[1]: string(value)}}, internalSessionWrite)
	case "delete":
		return deleteBdStoreBridge(store, args)
	case "dep-add":
		if len(args) < 3 {
			return fmt.Errorf("usage: dep-add <issue-id> <depends-on-id> <type>")
		}
		if err := validateBridgeGenericMutation(store, args[0]); err != nil {
			return err
		}
		return store.DepAdd(args[0], args[1], args[2])
	case "dep-remove":
		if len(args) < 2 {
			return fmt.Errorf("usage: dep-remove <issue-id> <depends-on-id>")
		}
		if err := validateBridgeGenericMutation(store, args[0]); err != nil {
			return err
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

// bdBridgeProviderError retains error identity for store handling while keeping
// provider stdout/stderr out of the bridge's generic diagnostic channel.
type bdBridgeProviderError struct{ cause error }

func (bdBridgeProviderError) Error() string {
	return "bd provider command failed (private diagnostic withheld)"
}
func (e bdBridgeProviderError) Unwrap() error { return e.cause }

func validateBdStoreBridgeAuthorityMetadata(metadata, current map[string]string) error {
	if err := session.ValidateUnownedRequestMetadata(metadata); err != nil {
		return fmt.Errorf("controller-owned session request metadata: %w", err)
	}
	for key := range metadata {
		if strings.HasPrefix(strings.TrimSpace(key), beadmeta.SessionRequestReceiptPrefix) {
			return fmt.Errorf("refusing controller-owned session request receipt metadata %q; use the tracked session protocol", key)
		}
		if protectedSessionAuthorityMetadata(key) || (sessionAuthorityOptionMetadata(key) && sessionAuthorityMetadataPreviouslyProtected(current)) {
			return fmt.Errorf("refusing controller-owned session authority metadata %q; use the signed session permission-mode API", key)
		}
	}
	return nil
}

func validateBridgeGenericMutation(store beads.Store, id string) error {
	current, err := store.Get(id)
	if err != nil {
		return err
	}
	return worklifecycle.ValidateGenericMutation(current)
}

func updateBdStoreBridge(store beads.Store, id string, opts beads.UpdateOpts) error {
	return updateBdStoreBridgeForMode(store, id, opts, false)
}

// genericBridgeSessionMetadata removes only identity fields carried by the
// internal session backend. It never waives receipt, purge, RSI, or signed
// permission-mode guards. This transport has the backend's host credentials;
// its mode marker is not a worker permission or an HTTP authorization grant.
func genericBridgeSessionMetadata(metadata map[string]string) map[string]string {
	filtered := make(map[string]string, len(metadata))
	for key, value := range metadata {
		if !beadmeta.IsExecutionIdentityMetadataKey(key) {
			filtered[key] = value
		}
	}
	return filtered
}

func updateBdStoreBridgeForMode(store beads.Store, id string, opts beads.UpdateOpts, internal bool) error {
	current, err := store.Get(id)
	if err != nil {
		return err
	}
	if internal && session.IsSessionBeadOrRepairable(current) {
		if session.IsRequestPurgeFenced(current) {
			return session.ErrRequestConflict
		}
		generic := opts
		generic.Metadata = genericBridgeSessionMetadata(opts.Metadata)
		if err := validateBdStoreBridgeAuthorityMetadata(generic.Metadata, current.Metadata); err != nil {
			return err
		}
		if err := session.GuardGenericMutation(current, generic); err != nil {
			return err
		}
		if err := worklifecycle.ValidateEnrolledMutation(current, opts); err != nil {
			return err
		}
		if opts.Title != nil || opts.Status != nil || opts.Type != nil || opts.Priority != nil || opts.Description != nil ||
			opts.ParentID != nil || opts.Assignee != nil || len(opts.Labels) > 0 || len(opts.RemoveLabels) > 0 {
			// Ordinary row fields remain on the generic fenced path. The
			// internal exception carries only a session-owned metadata patch.
			return updateBdStoreBridgeAtRevision(store, current, opts)
		}
		return session.NewStore(beads.SessionStore{Store: store}).ApplyBackendIdentityPatchIfMatch(id, current.Revision, session.MetadataPatch(opts.Metadata))
	}
	return updateBdStoreBridgeAtRevision(store, current, opts)
}

func updateBdStoreBridgeAtRevision(store beads.Store, current beads.Bead, opts beads.UpdateOpts) error {
	if err := validateBdStoreBridgeAuthorityMetadata(opts.Metadata, current.Metadata); err != nil {
		return err
	}
	if err := worklifecycle.ValidateEnrolledMutation(current, opts); err != nil {
		return err
	}
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

func deleteBdStoreBridge(store beads.Store, args []string) error {
	if len(args) != 2 {
		return fmt.Errorf("usage: delete <id> <revision>")
	}
	revision, err := strconv.ParseInt(args[1], 10, 64)
	if err != nil || revision == 0 || strconv.FormatInt(revision, 10) != args[1] {
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
	if err := worklifecycle.ValidateGenericMutation(b); err != nil {
		return err
	}
	if err := beads.ValidateLifecycleDelete(b); err != nil {
		return err
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
	if !private {
		item = beads.PublicBead(item)
	}
	metadata := item.Metadata
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
