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
	key := base64.StdEncoding.EncodeToString(make([]byte, 32))
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
			name: "recovery needs admission and acceptance gates",
			toml: "[lifecycle]\nrecovery_enabled = true\nescalation_target = \"ops\"\n",
			want: "requires lifecycle.admission_enabled",
		},
		{
			name: "recovery requires escalation recipient",
			toml: "[lifecycle]\nadmission_enabled = true\nrecovery_enabled = true\n[lifecycle.admission_authorities]\ntriage = \"" + key + "\"\n[lifecycle.acceptance_authorities]\nreviewer = \"" + key + "\"\n",
			want: "requires lifecycle.escalation_target",
		},
		{
			name: "valid opt-in config",
			toml: "[lifecycle]\nadmission_enabled = true\nrecovery_enabled = true\nescalation_target = \"ops\"\n[lifecycle.admission_authorities]\ntriage = \"" + key + "\"\n[lifecycle.acceptance_authorities]\nreviewer = \"" + key + "\"\n",
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
