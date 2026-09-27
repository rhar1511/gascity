package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/api"
)

func TestSessionRequestAcknowledgementRequiresExecutionIdentity(t *testing.T) {
	for _, missing := range []string{"GC_SESSION_ID", "GC_RUNTIME_EPOCH", "GC_INSTANCE_TOKEN"} {
		t.Run(missing, func(t *testing.T) {
			t.Setenv("GC_SESSION_ID", "gc-test")
			t.Setenv("GC_RUNTIME_EPOCH", "2")
			t.Setenv("GC_INSTANCE_TOKEN", "private-credential")
			t.Setenv(missing, "")
			var out, errOut bytes.Buffer
			cmd := newSessionRequestCmd(&out, &errOut)
			cmd.SetArgs([]string{"ack", "request-test"})
			if err := cmd.Execute(); err == nil {
				t.Fatal("acknowledgement accepted without execution identity")
			}
			if bytes.Contains(out.Bytes(), []byte("private-credential")) || bytes.Contains(errOut.Bytes(), []byte("private-credential")) {
				t.Fatal("credential exposed")
			}
		})
	}
}

func TestSessionRequestJSONSchemas(t *testing.T) {
	for _, action := range []string{"submit", "get", "ack"} {
		t.Run(action, func(t *testing.T) {
			var out bytes.Buffer
			if err := writeSessionRequestReceiptJSON(&out, api.SessionRequestReceipt{RequestId: "req-1", SessionId: "gc-1", Generation: 2, AcceptedAt: time.Now().UTC(), Delivery: "pending", Effect: "unverified", MessageDigest: strings.Repeat("a", 64)}); err != nil {
				t.Fatal(err)
			}
			validateJSONResultSchema(t, []string{"session", "request", action}, out.Bytes())
		})
	}
}

func TestSessionRequestAttributedJSONSchemas(t *testing.T) {
	// Decode the server wire format through the same generated type as the
	// client, then verify the CLI preserves attribution in its public output.
	var receipt api.SessionRequestReceipt
	if err := json.Unmarshal([]byte(`{"request_id":"req-1","session_id":"gc-1","generation":2,"accepted_at":"2026-09-27T00:00:00Z","delivery":"accepted","effect":"unverified","message_digest":"digest","attempt":{"store_ref":"city:test","attempt_id":"attempt-1","work_revision":"9223372036854775807","identity":{"kind":"workbench","owner_bead_id":"work-1","execution_bead_id":"work-1","session_id":"gc-1","session_generation":"2","claim_generation":"claim-1"}}}`), &receipt); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := writeSessionRequestReceiptJSON(&out, receipt); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"work_revision":"9223372036854775807"`) {
		t.Fatalf("CLI discarded or rounded server attribution: %s", &out)
	}
	for _, action := range []string{"submit", "get", "ack"} {
		t.Run(action, func(t *testing.T) {
			validateJSONResultSchema(t, []string{"session", "request", action}, out.Bytes())
		})
	}
}

func TestSessionRequestClientUsesSupervisorForAliveCityWithoutStandalonePort(t *testing.T) {
	cityPath := writeBeadsTestCity(t)
	t.Setenv("GC_CITY", cityPath)
	t.Setenv("GC_NO_API", "")
	oldAlive, oldSupervisor := apiRouteControllerAliveHook, apiRouteSupervisorClientHook
	t.Cleanup(func() {
		apiRouteControllerAliveHook, apiRouteSupervisorClientHook = oldAlive, oldSupervisor
	})
	want := api.NewCityScopedClient("http://127.0.0.1:1", "test-city")
	apiRouteControllerAliveHook = func(string) int { return 1 }
	apiRouteSupervisorClientHook = func(string) *api.Client { return want }
	got, err := sessionRequestClient()
	if err != nil || got != want {
		t.Fatalf("supervisor-managed session request client = %v, %v; want supervisor client", got, err)
	}
	t.Setenv("GC_NO_API", "1")
	if got, err := sessionRequestClient(); err == nil || got != nil {
		t.Fatal("disabled API must reject tracked request without a local fallback")
	}
}

func TestSessionRequestFailuresReportDiagnosticsThroughRoot(t *testing.T) {
	cityPath := writeBeadsTestCity(t)
	t.Setenv("GC_NO_API", "1")
	t.Setenv("GC_SESSION_ID", "")
	t.Setenv("GC_RUNTIME_EPOCH", "2")
	t.Setenv("GC_INSTANCE_TOKEN", "private-credential")
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"get", []string{"get", "gc-test", "req-test"}, "require the Gas City server"},
		{"submit", []string{"submit", "gc-test", "req-test", "message"}, "positive --generation"},
		{"ack", []string{"ack", "req-test"}, "requires this execution"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var out, errOut bytes.Buffer
			args := append([]string{"--city", cityPath, "session", "request"}, tc.args...)
			if code := run(args, &out, &errOut); code == 0 {
				t.Fatal("invalid request reported success")
			}
			if !strings.Contains(errOut.String(), tc.want) {
				t.Fatalf("missing diagnostic: stdout=%s stderr=%s", &out, &errOut)
			}
			if strings.Contains(out.String()+errOut.String(), "private-credential") {
				t.Fatal("execution credential leaked")
			}
		})
	}
}
