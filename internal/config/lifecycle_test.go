package config

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestParseLifecycleDefaultsDisabled(t *testing.T) {
	cfg, err := Parse([]byte("[workspace]\nname = \"test\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Lifecycle.AdmissionEnabled || cfg.Lifecycle.RecoveryEnabled {
		t.Fatalf("lifecycle defaults = %+v, want both gates disabled", cfg.Lifecycle)
	}
}

func TestParseLifecycleValidatesTrustedAuthorities(t *testing.T) {
	key := lifecycleTestPublicKey(1)
	otherKey := lifecycleTestPublicKey(2)
	recoveryKey := lifecycleTestPublicKey(3)
	recoveryTable := "[lifecycle.recovery_authorities]\noperator = { public_key = \"" + recoveryKey + "\", actions = [\"nudge\"], scopes = [\"city:test/city:test\"] }\n"
	cases := []struct {
		name string
		toml string
		want string
	}{
		{
			name: "admission gate requires authority",
			toml: "[lifecycle]\nadmission_enabled = true\n",
			want: "requires at least one admission authority",
		},
		{
			name: "malformed public key is rejected",
			toml: "[lifecycle]\nadmission_authorities = { triage = \"not-a-key\" }\n",
			want: "must be a base64-encoded Ed25519 public key",
		},
		{
			name: "admission gate requires acceptance authority",
			toml: "[lifecycle]\nadmission_enabled = true\n[lifecycle.admission_authorities]\ntriage = \"" + key + "\"\n",
			want: "requires at least one acceptance authority",
		},
		{
			name: "recovery needs admission and acceptance gates",
			toml: "[lifecycle]\nrecovery_enabled = true\nescalation_target = \"ops\"\n",
			want: "requires lifecycle.admission_enabled",
		},
		{
			name: "recovery requires escalation recipient",
			toml: "[lifecycle]\nadmission_enabled = true\nrecovery_enabled = true\n[lifecycle.admission_authorities]\ntriage = \"" + key + "\"\n[lifecycle.acceptance_authorities]\nreviewer = \"" + otherKey + "\"\n" + recoveryTable,
			want: "requires lifecycle.escalation_target",
		},
		{
			name: "recovery requires a separate recovery authority",
			toml: "[lifecycle]\nadmission_enabled = true\nrecovery_enabled = true\nescalation_target = \"ops\"\n[lifecycle.admission_authorities]\ntriage = \"" + key + "\"\n[lifecycle.acceptance_authorities]\nreviewer = \"" + otherKey + "\"\n",
			want: "requires at least one recovery authority",
		},
		{
			name: "recovery authority credential is separate",
			toml: "[lifecycle]\nadmission_enabled = true\nrecovery_enabled = true\nescalation_target = \"ops\"\n[lifecycle.admission_authorities]\ntriage = \"" + key + "\"\n[lifecycle.acceptance_authorities]\nreviewer = \"" + otherKey + "\"\n[lifecycle.recovery_authorities]\noperator = { public_key = \"" + key + "\", actions = [\"nudge\"], scopes = [\"city:test/city:test\"] }\n",
			want: "credentials separate",
		},
		{
			name: "recovery authority requires explicit scope",
			toml: "[lifecycle]\nadmission_enabled = true\nrecovery_enabled = true\nescalation_target = \"ops\"\n[lifecycle.admission_authorities]\ntriage = \"" + key + "\"\n[lifecycle.acceptance_authorities]\nreviewer = \"" + otherKey + "\"\n[lifecycle.recovery_authorities]\noperator = { public_key = \"" + recoveryKey + "\", actions = [\"nudge\"], scopes = [\"*\"] }\n",
			want: "cannot contain wildcards",
		},
		{
			name: "valid opt-in config",
			toml: "[lifecycle]\nadmission_enabled = true\nrecovery_enabled = true\nescalation_target = \"ops\"\n[lifecycle.admission_authorities]\ntriage = \"" + key + "\"\n[lifecycle.acceptance_authorities]\nreviewer = \"" + otherKey + "\"\n" + recoveryTable,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte("[workspace]\nname = \"test\"\n" + tc.toml))
			if tc.want == "" {
				if err != nil {
					t.Fatalf("Parse() error = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Parse() error = %v, want containing %q", err, tc.want)
			}
		})
	}
}

func lifecycleTestPublicKey(last byte) string {
	key := make([]byte, 32)
	key[len(key)-1] = last
	return base64.StdEncoding.EncodeToString(key)
}

func TestValidateCompletionFreshnessPolicy(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  LifecycleConfig
		want string
	}{
		{name: "unset disables policy"},
		{name: "both durations required", cfg: LifecycleConfig{CompletionReceiptMaxAge: "24h"}, want: "configured together"},
		{name: "positive maximum age", cfg: LifecycleConfig{CompletionReceiptMaxAge: "0s", CompletionClockSkew: "0s"}, want: "positive duration"},
		{name: "non-negative skew", cfg: LifecycleConfig{CompletionReceiptMaxAge: "24h", CompletionClockSkew: "-1s"}, want: "non-negative duration"},
		{name: "valid explicit policy", cfg: LifecycleConfig{CompletionReceiptMaxAge: "168h", CompletionClockSkew: "5m"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateCompletionFreshnessConfig(tc.cfg)
			if tc.want == "" && err != nil {
				t.Fatalf("validateCompletionFreshnessConfig() = %v", err)
			}
			if tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("validateCompletionFreshnessConfig() = %v, want containing %q", err, tc.want)
			}
		})
	}
}
