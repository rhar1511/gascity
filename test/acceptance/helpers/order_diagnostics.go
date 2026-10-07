package acceptancehelpers

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const (
	orderDiagnosticTailBytes  = 1 << 20
	orderDiagnosticHeadBytes  = 16 << 10
	orderDiagnosticRowBytes   = 64 << 10
	orderDiagnosticRows       = 64
	orderDiagnosticValueBytes = 128
	orderDiagnosticBudget     = 5 * time.Second
	orderDiagnosticTraceRoot  = ".gc/runtime/session-reconciler-trace/"
)

var orderDiagnosticSegment = regexp.MustCompile(`^segments/[0-9]{4}/[0-9]{2}/[0-9]{2}/segment-[0-9]{6,}\.jsonl$`)

type orderDiagnosticInput struct {
	Source      string `json:"source"`
	Status      string `json:"status"`
	ByteLimit   bool   `json:"byte_limit"`
	RowLimit    bool   `json:"row_limit"`
	PartialRow  bool   `json:"partial_row"`
	InvalidRows int    `json:"invalid_rows"`
}

type orderDiagnosticEvent struct {
	Seq     uint64    `json:"seq"`
	Type    string    `json:"type"`
	Ts      time.Time `json:"ts"`
	Subject string    `json:"subject,omitempty"`
}

type orderDiagnosticTrace struct {
	Seq                      uint64     `json:"seq"`
	RecordType               string     `json:"record_type"`
	Ts                       time.Time  `json:"ts"`
	TickID                   string     `json:"tick_id"`
	TickTrigger              string     `json:"tick_trigger,omitempty"`
	ControllerInstanceSHA256 string     `json:"controller_instance_sha256,omitempty"`
	ControllerPID            int        `json:"controller_pid,omitempty"`
	ControllerStartedAt      *time.Time `json:"controller_started_at,omitempty"`
	CycleOffsetMS            int64      `json:"cycle_offset_ms,omitempty"`
	DurationMS               int64      `json:"duration_ms,omitempty"`
	SiteCode                 string     `json:"site_code,omitempty"`
	CompletionStatus         string     `json:"completion_status,omitempty"`
}

type orderDiagnosticTraceWire struct {
	Seq                  uint64     `json:"seq"`
	RecordType           string     `json:"record_type"`
	Ts                   time.Time  `json:"ts"`
	TickID               string     `json:"tick_id"`
	TickTrigger          string     `json:"tick_trigger"`
	ControllerInstanceID string     `json:"controller_instance_id"`
	ControllerPID        int        `json:"controller_pid"`
	ControllerStartedAt  *time.Time `json:"controller_started_at"`
	CycleOffsetMS        int64      `json:"cycle_offset_ms"`
	DurationMS           int64      `json:"duration_ms"`
	SiteCode             string     `json:"site_code"`
	CompletionStatus     string     `json:"completion_status"`
}

// OrderFailureDiagnostics returns bounded, sanitized passive fixture evidence
// only for an order-firing-current error. It never invokes or changes runtime
// state. The five-second cooperative budget cannot cancel filesystem syscalls.
func OrderFailureDiagnostics(cityDir, checkName, checkStatus string) string {
	return orderFailureDiagnostics(cityDir, checkName, checkStatus, time.Now)
}

func orderFailureDiagnostics(cityDir, checkName, checkStatus string, now func() time.Time) string {
	if checkName != "order-firing-current" || checkStatus != "error" {
		return ""
	}
	started := now()
	observed := started.UTC()
	deadline := started.Add(orderDiagnosticBudget)
	result := struct {
		ObservedAt   time.Time              `json:"observed_at"`
		Coverage     string                 `json:"coverage"`
		StartupPhase string                 `json:"startup_phase"`
		Inputs       []orderDiagnosticInput `json:"inputs"`
		Events       []orderDiagnosticEvent `json:"events"`
		Trace        []orderDiagnosticTrace `json:"trace"`
	}{ObservedAt: observed, Coverage: "UNVALIDATED_PARTIAL_ACTIVE_SEGMENT", StartupPhase: "STARTUP_ORDERS_NOT_TRACED"}
	finish := func() string {
		data, err := json.Marshal(result)
		if err != nil {
			return `{"coverage":"incomplete","status":"encoding_error"}`
		}
		return string(data)
	}
	// A root symlink is not a second source of fixture ownership.
	info, err := os.Lstat(cityDir)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		result.Inputs = append(result.Inputs, orderDiagnosticInput{Source: "root", Status: "rejected"})
		return finish()
	}
	root, err := os.OpenRoot(cityDir)
	if err != nil {
		result.Inputs = append(result.Inputs, orderDiagnosticInput{Source: "root", Status: "unavailable"})
		return finish()
	}
	defer root.Close() //nolint:errcheck // read-only fixture handle
	data, input := readOrderDiagnosticTail(root, ".gc/events.jsonl", "events", orderDiagnosticTailBytes, deadline, now)
	lines := orderDiagnosticLines(data, &input)
	for _, line := range lines {
		if !now().Before(deadline) {
			input.Status = "budget_exhausted"
			break
		}
		var event orderDiagnosticEvent
		if len(line) > orderDiagnosticRowBytes || json.Unmarshal(line, &event) != nil || event.Ts.IsZero() {
			input.InvalidRows++
			continue
		}
		switch event.Type {
		case "controller.started", "controller.stopped":
			event.Subject = ""
		case "order.fired", "order.completed", "order.failed", "order.suppressed", "order.skipped":
			if !orderDiagnosticIdentifier(event.Subject) {
				input.InvalidRows++
				continue
			}
		default:
			continue
		}
		event.Ts = event.Ts.UTC()
		result.Events = append(result.Events, event)
		if len(result.Events) > orderDiagnosticRows {
			input.RowLimit = true
			result.Events = result.Events[1:]
		}
	}
	result.Inputs = append(result.Inputs, input)
	data, input = readOrderDiagnosticTail(root, orderDiagnosticTraceRoot+"head.json", "trace_head", orderDiagnosticHeadBytes, deadline, now)
	var head struct {
		CurrentPath string `json:"current_path"`
	}
	if input.Status != "ok" {
		result.Inputs = append(result.Inputs, input)
		return finish()
	}
	if input.ByteLimit || json.Unmarshal(data, &head) != nil {
		input.Status = "invalid_head"
		result.Inputs = append(result.Inputs, input)
		return finish()
	}
	if len(head.CurrentPath) > orderDiagnosticValueBytes || !orderDiagnosticSegment.MatchString(head.CurrentPath) {
		input.Status = "rejected"
		result.Inputs = append(result.Inputs, input)
		return finish()
	}
	result.Inputs = append(result.Inputs, input)
	data, input = readOrderDiagnosticTail(root, orderDiagnosticTraceRoot+head.CurrentPath, "trace_segment", orderDiagnosticTailBytes, deadline, now)
	lines = orderDiagnosticLines(data, &input)
	// Only cycles positively identified in the bounded window may contribute
	// metadata-only operation/result rows. Missing starts remain incomplete.
	var wires []orderDiagnosticTraceWire
	ticks := make(map[string]bool)
	var tickOrder []string
	for _, line := range lines {
		if !now().Before(deadline) {
			input.Status = "budget_exhausted"
			break
		}
		var row orderDiagnosticTraceWire
		if len(line) > orderDiagnosticRowBytes || json.Unmarshal(line, &row) != nil || row.Ts.IsZero() || !orderDiagnosticIdentifier(row.TickID) {
			input.InvalidRows++
			continue
		}
		switch row.RecordType {
		case "cycle_start", "cycle_result", "operation":
		default:
			continue
		}
		if row.RecordType == "cycle_start" && (row.TickTrigger == "orders" || row.TickTrigger == "startup") {
			if !ticks[row.TickID] {
				ticks[row.TickID] = true
				tickOrder = append(tickOrder, row.TickID)
				if len(tickOrder) > orderDiagnosticRows {
					delete(ticks, tickOrder[0])
					tickOrder = tickOrder[1:]
					input.RowLimit = true
				}
			}
		}
		if !ticks[row.TickID] {
			continue
		}
		wires = append(wires, row)
		if len(wires) > orderDiagnosticRows {
			input.RowLimit = true
			wires = wires[1:]
		}
	}
	for _, row := range wires {
		if !now().Before(deadline) {
			input.Status = "budget_exhausted"
			break
		}
		if !ticks[row.TickID] {
			continue
		}
		if len(row.ControllerInstanceID) > orderDiagnosticValueBytes || row.ControllerPID < 0 || row.DurationMS < 0 || row.CycleOffsetMS < 0 {
			input.InvalidRows++
			continue
		}
		if row.TickTrigger != "" && row.TickTrigger != "orders" && row.TickTrigger != "startup" {
			continue
		}
		if row.SiteCode != "" && row.SiteCode != "orders.dispatch" && row.SiteCode != "controller.tick.phase" {
			continue
		}
		switch row.CompletionStatus {
		case "", "completed", "trace_error", "panic_recovered", "aborted":
		default:
			input.InvalidRows++
			continue
		}
		projected := orderDiagnosticTrace{Seq: row.Seq, RecordType: row.RecordType, Ts: row.Ts.UTC(), TickID: row.TickID, TickTrigger: row.TickTrigger, ControllerPID: row.ControllerPID, ControllerStartedAt: row.ControllerStartedAt, CycleOffsetMS: row.CycleOffsetMS, DurationMS: row.DurationMS, SiteCode: row.SiteCode, CompletionStatus: row.CompletionStatus}
		if row.ControllerStartedAt != nil {
			startedAt := row.ControllerStartedAt.UTC()
			projected.ControllerStartedAt = &startedAt
		}
		if row.ControllerInstanceID != "" {
			sum := sha256.Sum256([]byte(row.ControllerInstanceID))
			projected.ControllerInstanceSHA256 = hex.EncodeToString(sum[:])
		}
		result.Trace = append(result.Trace, projected)
		if len(result.Trace) > orderDiagnosticRows {
			input.RowLimit = true
			result.Trace = result.Trace[1:]
		}
	}
	result.Inputs = append(result.Inputs, input)
	return finish()
}

func orderDiagnosticIdentifier(value string) bool {
	if value == "" || len(value) > orderDiagnosticValueBytes {
		return false
	}
	for _, c := range value {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', strings.ContainsRune("-_/.:", c):
		default:
			return false
		}
	}
	return true
}

func readOrderDiagnosticTail(root *os.Root, name, source string, limit int64, deadline time.Time, now func() time.Time) ([]byte, orderDiagnosticInput) {
	input := orderDiagnosticInput{Source: source, Status: "ok"}
	if !now().Before(deadline) {
		input.Status = "budget_exhausted"
		return nil, input
	}
	if !filepath.IsLocal(name) {
		input.Status = "rejected"
		return nil, input
	}
	var info os.FileInfo
	var err error
	// OpenRoot confines resolution but allows internal symlinks, so inspect
	// every component too. The opened file must match the inspected inode.
	current := ""
	for _, part := range strings.Split(filepath.ToSlash(name), "/") {
		current = filepath.Join(current, part)
		info, err = root.Lstat(current)
		if err != nil {
			if os.IsNotExist(err) {
				input.Status = "missing"
			} else {
				input.Status = "unavailable"
			}
			return nil, input
		}
		if info.Mode()&os.ModeSymlink != 0 {
			input.Status = "rejected"
			return nil, input
		}
	}
	if !info.Mode().IsRegular() {
		input.Status = "rejected"
		return nil, input
	}
	file, err := root.Open(name)
	if err != nil {
		input.Status = "unavailable"
		return nil, input
	}
	defer file.Close() //nolint:errcheck // read-only fixture file
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		input.Status = "changed"
		return nil, input
	}
	size := opened.Size()
	start := max(int64(0), size-limit)
	input.ByteLimit = start > 0
	data := make([]byte, size-start)
	n, err := file.ReadAt(data, start)
	if err != nil && !errors.Is(err, io.EOF) {
		input.Status = "read_error"
		return nil, input
	}
	data = data[:n]
	if int64(n) != size-start {
		input.Status = "changed"
	}
	if start > 0 {
		if end := bytes.IndexByte(data, '\n'); end >= 0 {
			data = data[end+1:]
		} else {
			data = nil
		}
	}
	return data, input
}

func orderDiagnosticLines(data []byte, input *orderDiagnosticInput) [][]byte {
	if len(data) == 0 {
		return nil
	}
	if data[len(data)-1] != '\n' {
		input.PartialRow = true
		end := bytes.LastIndexByte(data, '\n')
		if end < 0 {
			return nil
		}
		data = data[:end+1]
	}
	return bytes.Split(bytes.TrimSuffix(data, []byte{'\n'}), []byte{'\n'})
}
