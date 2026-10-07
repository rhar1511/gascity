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
