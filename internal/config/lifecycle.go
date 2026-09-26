package config

import (
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"sort"
	"strings"
)

// LifecycleConfig controls controller-owned admission and recovery. The
// controller keeps both paths disabled unless an operator explicitly enables
// them and supplies trusted authorities. No identity or deadline is inferred
// from work-item metadata.
//
// Authority maps use canonical identity -> base64-encoded Ed25519 public key.
// The identity is only a lookup key: a receipt is trusted only when its
// signature verifies with the configured key. Private keys stay outside city
// configuration and the bead store.
type LifecycleConfig struct {
	// AdmissionEnabled requires a signed admission receipt for work carrying
	// explicit lifecycle admission intent. Defaults to false.
	AdmissionEnabled bool `toml:"admission_enabled,omitempty"`
	// RecoveryEnabled permits the controller lifecycle recovery policy. It is
	// separately gated because an admission authority does not grant permission
	// to recover work. Defaults to false.
	RecoveryEnabled bool `toml:"recovery_enabled,omitempty"`
	// AdmissionAuthorities maps trusted triage/contract authors to their
	// Ed25519 public keys. Keys are base64-encoded 32-byte public keys.
	AdmissionAuthorities map[string]string `toml:"admission_authorities,omitempty"`
	// AcceptanceAuthorities maps trusted completion authorities to their
	// Ed25519 public keys. Keys are base64-encoded 32-byte public keys.
	AcceptanceAuthorities map[string]string `toml:"acceptance_authorities,omitempty"`
	// EscalationTarget is the configured recipient for one exhaustion
	// escalation per work item. Empty deliberately leaves recovery disabled.
	EscalationTarget string `toml:"escalation_target,omitempty"`
}

func validateLifecycleConfig(cfg LifecycleConfig) error {
	if err := validateLifecycleAuthorities("lifecycle.admission_authorities", cfg.AdmissionAuthorities); err != nil {
		return err
	}
	if err := validateLifecycleAuthorities("lifecycle.acceptance_authorities", cfg.AcceptanceAuthorities); err != nil {
		return err
	}
	if cfg.AdmissionEnabled && len(cfg.AdmissionAuthorities) == 0 {
		return fmt.Errorf("lifecycle.admission_enabled requires at least one admission authority")
	}
	if cfg.RecoveryEnabled {
		if !cfg.AdmissionEnabled {
			return fmt.Errorf("lifecycle.recovery_enabled requires lifecycle.admission_enabled")
		}
		if strings.TrimSpace(cfg.EscalationTarget) == "" {
			return fmt.Errorf("lifecycle.recovery_enabled requires lifecycle.escalation_target")
		}
		if len(cfg.AcceptanceAuthorities) == 0 {
			return fmt.Errorf("lifecycle.recovery_enabled requires at least one acceptance authority")
		}
	}
	return nil
}

func validateLifecycleAuthorities(path string, authorities map[string]string) error {
	identities := make([]string, 0, len(authorities))
	for identity := range authorities {
		identities = append(identities, identity)
	}
	sort.Strings(identities)
	for _, identity := range identities {
		if strings.TrimSpace(identity) == "" {
			return fmt.Errorf("%s contains an empty identity", path)
		}
		encoded := strings.TrimSpace(authorities[identity])
		key, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil || len(key) != ed25519.PublicKeySize {
			return fmt.Errorf("%s.%s must be a base64-encoded Ed25519 public key", path, identity)
		}
	}
	return nil
}
