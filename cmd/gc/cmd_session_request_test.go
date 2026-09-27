package main

import (
	"bytes"
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
