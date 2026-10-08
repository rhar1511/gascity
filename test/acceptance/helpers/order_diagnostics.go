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
	"strconv"
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

var (
	orderDiagnosticSegment           = regexp.MustCompile(`^segments/[0-9]{4}/[0-9]{2}/[0-9]{2}/segment-[0-9]{6,}\.jsonl$`)
	orderDiagnosticSupervisorBudget  = regexp.MustCompile(`(?:^| )gc: order dispatch: per-tick budget ([0-9]{1,9}) spent; the rotation did not reach ([0-9]{1,9}) more order\(s\) this tick \(due-ness not evaluated\): (.+)$`)
	orderDiagnosticSupervisorOmitted = regexp.MustCompile(`^\(\+([0-9]{1,9}) more\)$`)
)

type orderDiagnosticSupervisor struct {
	Kind       string   `json:"kind"`
	Budget     int      `json:"budget,omitempty"`
	Unreached  int      `json:"unreached,omitempty"`
	Orders     []string `json:"orders,omitempty"`
	Omitted    int      `json:"omitted,omitempty"`
	DurationMS int64    `json:"duration_ms,omitempty"`
}

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
	return orderFailureDiagnosticSources(cityDir, "", false, checkName, checkStatus, now)
}

// OrderFailureDiagnosticsWithSupervisorLog also projects bounded metadata from
// the explicitly supplied private fixture home. An empty home never falls back
// to the operator's environment. Its log window is not controller correlation.
func OrderFailureDiagnosticsWithSupervisorLog(cityDir, fixtureHome, checkName, checkStatus string) string {
	return orderFailureDiagnosticsWithSupervisorLog(cityDir, fixtureHome, checkName, checkStatus, time.Now)
}

func orderFailureDiagnosticsWithSupervisorLog(cityDir, fixtureHome, checkName, checkStatus string, now func() time.Time) string {
	return orderFailureDiagnosticSources(cityDir, fixtureHome, true, checkName, checkStatus, now)
}

func orderFailureDiagnosticSources(cityDir, fixtureHome string, includeSupervisor bool, checkName, checkStatus string, now func() time.Time) string {
	if checkName != "order-firing-current" || checkStatus != "error" {
		return ""
	}
	started := now()
	observed := started.UTC()
	deadline := started.Add(orderDiagnosticBudget)
	result := struct {
		ObservedAt         time.Time                   `json:"observed_at"`
		Coverage           string                      `json:"coverage"`
		StartupPhase       string                      `json:"startup_phase"`
		Inputs             []orderDiagnosticInput      `json:"inputs"`
		Events             []orderDiagnosticEvent      `json:"events"`
		Trace              []orderDiagnosticTrace      `json:"trace"`
		SupervisorCoverage string                      `json:"supervisor_coverage,omitempty"`
		Supervisor         []orderDiagnosticSupervisor `json:"supervisor,omitempty"`
	}{ObservedAt: observed, Coverage: "UNVALIDATED_PARTIAL_ACTIVE_SEGMENT", StartupPhase: "STARTUP_ORDERS_NOT_TRACED"}
	finish := func() string {
		if includeSupervisor {
			result.SupervisorCoverage = "UNVALIDATED_PRIVATE_SUPERVISOR_LOG_WINDOW"
			rows, input := readOrderSupervisorDiagnostics(fixtureHome, deadline, now)
			result.Supervisor = rows
			result.Inputs = append(result.Inputs, input)
		}
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

func readOrderSupervisorDiagnostics(home string, deadline time.Time, now func() time.Time) ([]orderDiagnosticSupervisor, orderDiagnosticInput) {
	input := orderDiagnosticInput{Source: "supervisor"}
	if home == "" {
		input.Status = "not_declared"
		return nil, input
	}
	if !now().Before(deadline) {
		input.Status = "budget_exhausted"
		return nil, input
	}
	info, err := os.Lstat(home)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		input.Status = "rejected"
		return nil, input
	}
	root, err := os.OpenRoot(home)
	if err != nil {
		input.Status = "unavailable"
		return nil, input
	}
	defer root.Close() //nolint:errcheck // read-only private fixture handle
	opened, err := root.Stat(".")
	if err != nil || !os.SameFile(info, opened) {
		input.Status = "changed"
		return nil, input
	}
	data, input := readOrderDiagnosticTail(root, "supervisor.log", "supervisor", orderDiagnosticTailBytes, deadline, now)
	var rows []orderDiagnosticSupervisor
	for _, line := range orderDiagnosticLines(data, &input) {
		if !now().Before(deadline) {
			input.Status = "budget_exhausted"
			break
		}
		if len(line) > orderDiagnosticRowBytes {
			input.InvalidRows++
			continue
		}
		row, valid := projectOrderSupervisorLine(string(line))
		if !valid {
			if row.Kind != "" {
				input.InvalidRows++
			}
			continue
		}
		rows = append(rows, row)
		if len(rows) > orderDiagnosticRows {
			input.RowLimit = true
			rows = rows[1:]
		}
	}
	return rows, input
}

func projectOrderSupervisorLine(line string) (orderDiagnosticSupervisor, bool) {
	if match := orderDiagnosticSupervisorBudget.FindStringSubmatch(line); match != nil {
		row := orderDiagnosticSupervisor{Kind: "budget_unreached"}
		row.Budget, _ = strconv.Atoi(match[1]) // bounded decimal regex
		row.Unreached, _ = strconv.Atoi(match[2])
		if row.Budget == 0 || row.Unreached == 0 {
			return row, false
		}
		parts := strings.Split(match[3], ", ")
		if len(parts) > 9 {
			return row, false
		}
		for i, part := range parts {
			if omitted := orderDiagnosticSupervisorOmitted.FindStringSubmatch(part); omitted != nil {
				if i != len(parts)-1 || i != 8 {
					return row, false
				}
				row.Omitted, _ = strconv.Atoi(omitted[1])
				if row.Omitted == 0 {
					return row, false
				}
			} else {
				if !orderDiagnosticIdentifier(part) || len(row.Orders) == 8 {
					return row, false
				}
				row.Orders = append(row.Orders, part)
			}
		}
		return row, len(row.Orders)+row.Omitted == row.Unreached
	}
	const startup = "startup phase=startup-orders elapsed="
	if at := strings.Index(line, startup); at >= 0 {
		row := orderDiagnosticSupervisor{Kind: "startup_orders"}
		value := strings.TrimSpace(line[at+len(startup):])
		if len(value) > orderDiagnosticValueBytes {
			return row, false
		}
		duration, err := time.ParseDuration(value)
		if err != nil || duration < 0 || duration > 24*time.Hour {
			return row, false
		}
		row.DurationMS = duration.Milliseconds()
		return row, true
	}
	const gate = "gc: order dispatch: checking open work for "
	if at := strings.Index(line, gate); at >= 0 {
		row := orderDiagnosticSupervisor{Kind: "open_work_gate_error"}
		value := line[at+len(gate):]
		end := strings.Index(value, ": ")
		if end < 0 || !orderDiagnosticIdentifier(value[:end]) || end+2 == len(value) {
			return row, false
		}
		row.Orders = []string{value[:end]}
		return row, true
	}
	return orderDiagnosticSupervisor{}, false
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
