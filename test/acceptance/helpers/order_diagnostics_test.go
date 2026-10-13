package acceptancehelpers

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOrderFailureDiagnosticsWithSupervisorLogPrivacy(t *testing.T) {
	city, home := t.TempDir(), t.TempDir()
	writeOrderDiagnosticFixture(t, home, "supervisor.log", "gc: order dispatch: per-tick budget 4 spent; the rotation did not reach 2 more order(s) this tick (due-ness not evaluated): dolt-health, beads-health\n"+
		"gc supervisor: startup phase=startup-orders elapsed=7.25s\n"+
		"gc: order dispatch: checking open work for dolt-health: SECRET_GATE_TOKEN\n"+
		"unrelated actor=SECRET_ACTOR argv=SECRET_ARGV credentials=SECRET_CREDENTIAL\n")
	out := OrderFailureDiagnosticsWithSupervisorLog(city, home, "order-firing-current", "error")
	if !json.Valid([]byte(out)) || strings.Contains(out, "SECRET_") {
		t.Fatalf("invalid or unsanitized supervisor diagnostics: %s", out)
	}
	var result struct {
		SupervisorCoverage string `json:"supervisor_coverage"`
		Supervisor         []struct {
			Kind       string   `json:"kind"`
			Budget     int      `json:"budget"`
			Unreached  int      `json:"unreached"`
			Orders     []string `json:"orders"`
			DurationMS int64    `json:"duration_ms"`
		} `json:"supervisor"`
	}
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatal(err)
	}
	if result.SupervisorCoverage != "UNVALIDATED_PRIVATE_SUPERVISOR_LOG_WINDOW" || len(result.Supervisor) != 3 {
		t.Fatalf("missing passive evidence or unsupported correlation claim: %+v", result)
	}
	budget, startup, gate := result.Supervisor[0], result.Supervisor[1], result.Supervisor[2]
	if budget.Kind != "budget_unreached" || budget.Budget != 4 || budget.Unreached != 2 || strings.Join(budget.Orders, ",") != "dolt-health,beads-health" {
		t.Fatalf("budget sample changed into due-order evidence: %+v", budget)
	}
	if startup.Kind != "startup_orders" || startup.DurationMS != 7250 || gate.Kind != "open_work_gate_error" || strings.Join(gate.Orders, ",") != "dolt-health" {
		t.Fatalf("startup or gate metadata missing: %+v", result.Supervisor)
	}
}

func TestOrderFailureDiagnosticsWithSupervisorLogOwnershipAndBounds(t *testing.T) {
	city, home, outside := t.TempDir(), t.TempDir(), t.TempDir()
	writeOrderDiagnosticFixture(t, outside, "supervisor.log", "SECRET_OUTSIDE\n")
	if err := os.Symlink(filepath.Join(outside, "supervisor.log"), filepath.Join(home, "supervisor.log")); err != nil {
		t.Fatal(err)
	}
	out := OrderFailureDiagnosticsWithSupervisorLog(city, home, "order-firing-current", "error")
	if strings.Contains(out, "SECRET_") || !strings.Contains(out, `"source":"supervisor"`) || !strings.Contains(out, `"status":"rejected"`) {
		t.Fatalf("foreign source not rejected: %s", out)
	}
	if err := os.Remove(filepath.Join(home, "supervisor.log")); err != nil {
		t.Fatal(err)
	}
	line := "gc: order dispatch: per-tick budget 4 spent; the rotation did not reach 1 more order(s) this tick (due-ness not evaluated): dolt-health\n"
	writeOrderDiagnosticFixture(t, home, "supervisor.log", strings.Repeat(line, orderDiagnosticRows+1)+"SECRET_PARTIAL")
	out = OrderFailureDiagnosticsWithSupervisorLog(city, home, "order-firing-current", "error")
	var result struct {
		Inputs     []orderDiagnosticInput `json:"inputs"`
		Supervisor []json.RawMessage      `json:"supervisor"`
	}
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Supervisor) != orderDiagnosticRows || strings.Contains(out, "SECRET_") {
		t.Fatalf("supervisor row or partial-tail bound failed: %s", out)
	}
	found := false
	for _, input := range result.Inputs {
		if input.Source == "supervisor" {
			found = input.RowLimit && input.PartialRow
		}
	}
	if !found {
		t.Fatalf("bounded evidence was presented as complete: %s", out)
	}
	if got := OrderFailureDiagnosticsWithSupervisorLog(city, outside, "order-firing-current", "ok"); got != "" {
		t.Fatalf("successful doctor caused evidence read: %s", got)
	}
}

func TestOrderFailureDiagnosticsWithSupervisorLogPrivateHome(t *testing.T) {
	city, home := t.TempDir(), t.TempDir()
	writeOrderDiagnosticFixture(t, home, "supervisor.log", "SECRET_HOME\n")
	link := filepath.Join(t.TempDir(), "linked-home")
	if err := os.Symlink(home, link); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ name, home, status string }{
		{"undeclared", "", "not_declared"},
		{"missing", filepath.Join(t.TempDir(), "absent"), "rejected"},
		{"symlink", link, "rejected"},
	} {
		t.Run(c.name, func(t *testing.T) {
			out := OrderFailureDiagnosticsWithSupervisorLog(city, c.home, "order-firing-current", "error")
			if !json.Valid([]byte(out)) || strings.Contains(out, "SECRET_") || !strings.Contains(out, `"source":"supervisor"`) || !strings.Contains(out, `"status":"`+c.status+`"`) {
				t.Fatalf("private home boundary not explicit: %s", out)
			}
		})
	}
}

func TestOrderFailureDiagnosticsWithSupervisorLogSizeBounds(t *testing.T) {
	line := "gc: order dispatch: per-tick budget 4 spent; the rotation did not reach 1 more order(s) this tick (due-ness not evaluated): dolt-health\n"
	for _, c := range []struct {
		name, data string
		byteLimit  bool
		invalid    bool
		rows       int
	}{
		{"tail", strings.Repeat("x", orderDiagnosticTailBytes) + "\n" + line, true, false, 1},
		{"row", strings.Repeat("x", orderDiagnosticRowBytes+1) + "SECRET_ROW\n", false, true, 0},
		{"identifier", strings.Replace(line, "dolt-health", strings.Repeat("x", orderDiagnosticValueBytes+1), 1), false, true, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			city, home := t.TempDir(), t.TempDir()
			writeOrderDiagnosticFixture(t, home, "supervisor.log", c.data)
			out := OrderFailureDiagnosticsWithSupervisorLog(city, home, "order-firing-current", "error")
			var result struct {
				Inputs     []orderDiagnosticInput `json:"inputs"`
				Supervisor []json.RawMessage      `json:"supervisor"`
			}
			if err := json.Unmarshal([]byte(out), &result); err != nil {
				t.Fatal(err)
			}
			if len(result.Supervisor) != c.rows || strings.Contains(out, "SECRET_") || strings.Contains(out, strings.Repeat("x", orderDiagnosticValueBytes+1)) {
				t.Fatalf("supervisor size or privacy boundary failed: %s", out)
			}
			found := false
			for _, input := range result.Inputs {
				if input.Source == "supervisor" {
					found = input.ByteLimit == c.byteLimit && (input.InvalidRows > 0) == c.invalid
				}
			}
			if !found {
				t.Fatalf("incomplete size-bound evidence hidden: %s", out)
			}
		})
	}
}

func TestOrderFailureDiagnosticsWithSupervisorLogSharedBudget(t *testing.T) {
	city, home := t.TempDir(), t.TempDir()
	writeOrderDiagnosticFixture(t, home, "supervisor.log", "gc supervisor: startup phase=startup-orders elapsed=7.25s\n")
	base, calls := time.Date(2026, 10, 8, 0, 0, 0, 0, time.UTC), 0
	now := func() time.Time {
		calls++
		if calls == 1 {
			return base
		}
		return base.Add(orderDiagnosticBudget + time.Second)
	}
	out := orderFailureDiagnosticsWithSupervisorLog(city, home, "order-firing-current", "error", now)
	var result struct {
		Inputs     []orderDiagnosticInput `json:"inputs"`
		Supervisor []json.RawMessage      `json:"supervisor"`
	}
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Supervisor) != 0 {
		t.Fatalf("supervisor collector reset the shared deadline: %s", out)
	}
	found := false
	for _, input := range result.Inputs {
		if input.Source == "supervisor" {
			found = input.Status == "budget_exhausted"
		}
	}
	if !found {
		t.Fatalf("shared budget exhaustion hidden: %s", out)
	}
}

func TestOrderFailureDiagnosticsSupervisorProjectionEdges(t *testing.T) {
	budget := func(count, names string) string {
		return "gc: order dispatch: per-tick budget 4 spent; the rotation did not reach " + count + " more order(s) this tick (due-ness not evaluated): " + names
	}
	eight := "a, b, c, d, e, f, g, h"
	for _, c := range []struct {
		name, line, kind string
		valid            bool
		orders, omitted  int
		durationMS       int64
	}{
		{"eight-orders", budget("8", eight), "budget_unreached", true, 8, 0, 0},
		{"omitted-orders", budget("11", eight+", (+3 more)"), "budget_unreached", true, 8, 3, 0},
		{"inconsistent-count", budget("10", eight+", (+3 more)"), "budget_unreached", false, 0, 0, 0},
		{"early-omission", budget("4", "a, (+3 more)"), "budget_unreached", false, 0, 0, 0},
		{"zero-omission", budget("8", eight+", (+0 more)"), "budget_unreached", false, 0, 0, 0},
		{"ninth-order", budget("9", eight+", i"), "budget_unreached", false, 0, 0, 0},
		{"zero-count", budget("0", "a"), "budget_unreached", false, 0, 0, 0},
		{"negative-count", budget("-1", "a"), "", false, 0, 0, 0},
		{"oversized-count", budget("1000000000", "a"), "", false, 0, 0, 0},
		{"compound-duration", "startup phase=startup-orders elapsed=1m2.5s", "startup_orders", true, 0, 0, 62500},
		{"negative-duration", "startup phase=startup-orders elapsed=-1s", "startup_orders", false, 0, 0, 0},
		{"malformed-duration", "startup phase=startup-orders elapsed=SECRET_DURATION", "startup_orders", false, 0, 0, 0},
		{"overflow-duration", "startup phase=startup-orders elapsed=999999999999999999999h", "startup_orders", false, 0, 0, 0},
		{"outside-duration-window", "startup phase=startup-orders elapsed=25h", "startup_orders", false, 0, 0, 0},
		{"scoped-gate", "gc: order dispatch: checking open work for rig:health: SECRET_GATE", "open_work_gate_error", true, 1, 0, 0},
		{"empty-gate-error", "gc: order dispatch: checking open work for health: ", "open_work_gate_error", false, 0, 0, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			row, valid := projectOrderSupervisorLine(c.line)
			if valid != c.valid || row.Kind != c.kind {
				t.Fatalf("projection validity = %v, kind = %q; want %v, %q", valid, row.Kind, c.valid, c.kind)
			}
			if !valid {
				return
			}
			if len(row.Orders) != c.orders || row.Omitted != c.omitted || row.DurationMS != c.durationMS {
				t.Fatalf("projected bounds changed: %+v", row)
			}
			data, err := json.Marshal(row)
			if err != nil || strings.Contains(string(data), "SECRET_") {
				t.Fatalf("projection leaked raw evidence: %s (%v)", data, err)
			}
		})
	}
}

func writeOrderDiagnosticFixture(t *testing.T, root, name, data string) {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestOrderFailureDiagnosticsPrivacyAndCorrelation(t *testing.T) {
	root := t.TempDir()
	writeOrderDiagnosticFixture(t, root, ".gc/events.jsonl", `{"seq":1,"type":"order.fired","ts":"2026-10-07T12:00:00.123456789Z","subject":"dolt-health","actor":"SECRET_ACTOR","message":"SECRET_MESSAGE","payload":{"token":"SECRET_TOKEN"}}`+"\n")
	traceRoot := ".gc/runtime/session-reconciler-trace/"
	writeOrderDiagnosticFixture(t, root, traceRoot+"head.json", `{"current_path":"segments/2026/10/07/segment-000001.jsonl"}`)
	writeOrderDiagnosticFixture(t, root, traceRoot+"segments/2026/10/07/segment-000001.jsonl", `{"seq":4,"record_type":"cycle_start","ts":"2026-10-07T12:00:01Z","tick_id":"tick-1","tick_trigger":"orders","controller_instance_id":"SECRET_HOST:123","controller_pid":123,"controller_started_at":"2026-10-07T11:59:00Z","session_key":"SECRET_KEY","fields":{"token":"SECRET_FIELD"}}`+"\n"+`{"seq":5,"record_type":"operation","ts":"2026-10-07T12:00:02Z","tick_id":"tick-1","duration_ms":1750,"site_code":"orders.dispatch","fields":{"operation":"dispatch_orders","token":"SECRET_OPERATION"}}`+"\n"+`{"seq":6,"record_type":"cycle_start","ts":"2026-10-07T12:00:03Z","tick_id":"unrelated","tick_trigger":"patrol","controller_instance_id":"SECRET_OTHER"}`+"\n")
	out := OrderFailureDiagnostics(root, "order-firing-current", "error")
	if !json.Valid([]byte(out)) {
		t.Fatalf("diagnostics are not JSON: %s", out)
	}
	for _, secret := range []string{"SECRET_", "session_key", "payload", "message", "actor", "fields"} {
		if strings.Contains(out, secret) {
			t.Fatalf("diagnostics expose %q: %s", secret, out)
		}
	}
	for _, want := range []string{"dolt-health", "2026-10-07T12:00:00.123456789Z", `"duration_ms":1750`, `"controller_pid":123`, "controller_instance_sha256", "UNVALIDATED_PARTIAL_ACTIVE_SEGMENT", "STARTUP_ORDERS_NOT_TRACED"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing diagnostic %q: %s", want, out)
		}
	}
	if strings.Contains(out, "unrelated") {
		t.Fatalf("unrelated controller cycle included: %s", out)
	}
	var result struct {
		Trace []struct {
			ControllerInstanceSHA256 string `json:"controller_instance_sha256"`
		} `json:"trace"`
	}
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Trace) != 2 || result.Trace[0].ControllerInstanceSHA256 != fmt.Sprintf("%x", sha256.Sum256([]byte("SECRET_HOST:123"))) {
		t.Fatalf("positive bounded cycle correlation or instance hash missing: %+v", result.Trace)
	}
}

func TestOrderFailureDiagnosticsTrigger(t *testing.T) {
	for _, c := range [][2]string{{"order-firing-current", "ok"}, {"order-firing-current", "warning"}, {"beads-store", "error"}} {
		if got := OrderFailureDiagnostics("unavailable-root", c[0], c[1]); got != "" {
			t.Fatalf("unexpected diagnostics for %v: %s", c, got)
		}
	}
}

func TestOrderFailureDiagnosticsMissingEvidence(t *testing.T) {
	out := OrderFailureDiagnostics(t.TempDir(), "order-firing-current", "error")
	if !json.Valid([]byte(out)) || !strings.Contains(out, "missing") || !strings.Contains(out, "STARTUP_ORDERS_NOT_TRACED") {
		t.Fatalf("missing evidence hidden: %s", out)
	}
}

func TestOrderFailureDiagnosticsRejectsUnsafePaths(t *testing.T) {
	for _, component := range []bool{false, true} {
		t.Run(fmt.Sprintf("component-%v", component), func(t *testing.T) {
			root, outside := t.TempDir(), t.TempDir()
			writeOrderDiagnosticFixture(t, outside, "events.jsonl", "SECRET_OUTSIDE\n")
			link, target := filepath.Join(root, ".gc", "events.jsonl"), filepath.Join(outside, "events.jsonl")
			if component {
				link, target = filepath.Join(root, ".gc"), outside
			} else if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, link); err != nil {
				t.Fatal(err)
			}
			out := OrderFailureDiagnostics(root, "order-firing-current", "error")
			if !strings.Contains(out, "rejected") || strings.Contains(out, "SECRET_OUTSIDE") || strings.Contains(out, outside) {
				t.Fatalf("unsafe path evidence: %s", out)
			}
		})
	}
	t.Run("head-traversal", func(t *testing.T) {
		root := t.TempDir()
		writeOrderDiagnosticFixture(t, root, ".gc/runtime/session-reconciler-trace/head.json", `{"current_path":"../../../../SECRET_OUTSIDE"}`)
		out := OrderFailureDiagnostics(root, "order-firing-current", "error")
		if !strings.Contains(out, "rejected") || strings.Contains(out, "SECRET_OUTSIDE") {
			t.Fatalf("untrusted head path exposed or followed: %s", out)
		}
	})
}

func TestOrderFailureDiagnosticsBoundsAndPartialRows(t *testing.T) {
	root := t.TempDir()
	var data strings.Builder
	data.WriteString(strings.Repeat("x", 1<<20))
	data.WriteByte('\n')
	data.WriteString(`{"seq":999,"type":"order.fired","ts":"2026-10-07T12:00:00Z","subject":"oversized","message":"` + strings.Repeat("x", 64<<10) + `"}` + "\n")
	data.WriteString("malformed-json\n")
	for i := 1; i <= 70; i++ {
		fmt.Fprintf(&data, "{\"seq\":%d,\"type\":\"order.fired\",\"ts\":\"2026-10-07T12:00:00Z\",\"subject\":\"beads-health\"}\n", i)
	}
	data.WriteString(`{"seq":1234,"type":"order.fired"`)
	writeOrderDiagnosticFixture(t, root, ".gc/events.jsonl", data.String())
	out := OrderFailureDiagnostics(root, "order-firing-current", "error")
	var result struct {
		Events []struct {
			Seq uint64 `json:"seq"`
		} `json:"events"`
		Inputs []struct {
			Source      string `json:"source"`
			ByteLimit   bool   `json:"byte_limit"`
			RowLimit    bool   `json:"row_limit"`
			PartialRow  bool   `json:"partial_row"`
			InvalidRows int    `json:"invalid_rows"`
		} `json:"inputs"`
	}
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Events) != 64 || result.Events[0].Seq != 7 || result.Events[63].Seq != 70 {
		t.Fatalf("recent event bound lost: %+v", result.Events)
	}
	if len(result.Inputs) == 0 || result.Inputs[0].Source != "events" || !result.Inputs[0].ByteLimit || !result.Inputs[0].RowLimit || !result.Inputs[0].PartialRow || result.Inputs[0].InvalidRows < 2 {
		t.Fatalf("missing positive incomplete evidence: %+v", result.Inputs)
	}
	if strings.Contains(out, "oversized") || strings.Contains(out, "1234") {
		t.Fatalf("invalid/oversized row included: %s", out)
	}
}

func TestOrderFailureDiagnosticsCooperativeDeadline(t *testing.T) {
	start := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	calls := 0
	now := func() time.Time {
		calls++
		return start.Add(time.Duration(calls-1) * 6 * time.Second)
	}
	out := orderFailureDiagnostics(t.TempDir(), "order-firing-current", "error", now)
	if !strings.Contains(out, "budget_exhausted") {
		t.Fatalf("elapsed budget ignored: %s", out)
	}
}

func TestOrderFailureDiagnosticsTraceWindow(t *testing.T) {
	root := t.TempDir()
	traceRoot := ".gc/runtime/session-reconciler-trace/"
	writeOrderDiagnosticFixture(t, root, traceRoot+"head.json", `{"current_path":"segments/2026/10/07/segment-000001.jsonl"}`)
	var data strings.Builder
	data.WriteString(`{"seq":1,"record_type":"cycle_start","ts":"2026-10-07T12:00:00Z","tick_id":"tick-1","tick_trigger":"orders"}` + "\n")
	for seq := 2; seq <= 71; seq++ {
		fmt.Fprintf(&data, "{\"seq\":%d,\"record_type\":\"operation\",\"ts\":\"2026-10-07T12:00:01Z\",\"tick_id\":\"tick-1\",\"site_code\":\"orders.dispatch\",\"duration_ms\":1}\n", seq)
	}
	data.WriteString(`{"seq":999,"record_type":"operation","ts":"2026-10-07T12:00:02Z","tick_id":"missing-start","site_code":"orders.dispatch"}` + "\n")
	writeOrderDiagnosticFixture(t, root, traceRoot+"segments/2026/10/07/segment-000001.jsonl", data.String())
	out := OrderFailureDiagnostics(root, "order-firing-current", "error")
	var result struct {
		Trace []struct {
			Seq uint64 `json:"seq"`
		} `json:"trace"`
		Inputs []struct {
			Source   string `json:"source"`
			RowLimit bool   `json:"row_limit"`
		} `json:"inputs"`
	}
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Trace) != 64 || result.Trace[0].Seq != 8 || result.Trace[63].Seq != 71 {
		t.Fatalf("trace window or positive correlation lost: %+v", result.Trace)
	}
	if len(result.Inputs) != 3 || result.Inputs[2].Source != "trace_segment" || !result.Inputs[2].RowLimit {
		t.Fatalf("trace window incorrectly described as complete: %+v", result.Inputs)
	}
}

func TestOrderFailureDiagnosticsOversizedHead(t *testing.T) {
	root := t.TempDir()
	writeOrderDiagnosticFixture(t, root, ".gc/runtime/session-reconciler-trace/head.json", `{"current_path":"segments/2026/10/07/segment-000001.jsonl","padding":"`+strings.Repeat("x", 16<<10)+`"}`)
	out := OrderFailureDiagnostics(root, "order-firing-current", "error")
	if !strings.Contains(out, `"status":"invalid_head"`) || strings.Contains(out, "padding") {
		t.Fatalf("oversized head treated as complete or disclosed: %s", out)
	}
}
