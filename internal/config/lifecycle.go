package config

import (
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"sort"
	"strings"
	"time"
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
	// RecoveryAuthorities maps separately authorized recovery identities to
	// public keys and exact action/store scopes. These keys must differ from
	// admission and acceptance keys. Private recovery keys stay outside city
	// configuration and the bead store.
	RecoveryAuthorities map[string]LifecycleRecoveryAuthority `toml:"recovery_authorities,omitempty"`
	// EscalationTarget is the configured recipient for one exhaustion
	// escalation per work item. Empty deliberately leaves recovery disabled.
	EscalationTarget string `toml:"escalation_target,omitempty"`
	// CompletionReceiptMaxAge bounds how long after signing an acceptance
	// receipt may close work. Operators must choose this from their acceptance
	// and rollout policy; no default is inferred. Empty disables completion
	// reconciliation.
	CompletionReceiptMaxAge string `toml:"completion_receipt_max_age,omitempty"`
	// CompletionClockSkew is the largest accepted future timestamp allowance.
	// Operators must choose this from expected signer/controller clock drift.
	// It must be set with CompletionReceiptMaxAge; empty disables completion
	// reconciliation rather than choosing a controller-specific default.
	CompletionClockSkew string `toml:"completion_clock_skew,omitempty"`
}

// LifecycleRecoveryAuthority grants one signing identity exact recovery
// actions in exact city/store scopes. Wildcards are not supported: operators
// must list each enabled scope explicitly.
type LifecycleRecoveryAuthority struct {
	PublicKey string   `toml:"public_key"`
	Actions   []string `toml:"actions"`
	Scopes    []string `toml:"scopes"`
}

func validateLifecycleConfig(cfg LifecycleConfig) error {
	if err := validateCompletionFreshnessConfig(cfg); err != nil {
		return err
	}
	if err := validateLifecycleAuthorities("lifecycle.admission_authorities", cfg.AdmissionAuthorities); err != nil {
		return err
	}
	if err := validateLifecycleAuthorities("lifecycle.acceptance_authorities", cfg.AcceptanceAuthorities); err != nil {
		return err
	}
	if err := validateRecoveryAuthorities(cfg); err != nil {
		return err
	}
	if cfg.AdmissionEnabled && len(cfg.AdmissionAuthorities) == 0 {
		return fmt.Errorf("lifecycle.admission_enabled requires at least one admission authority")
	}
	if cfg.AdmissionEnabled && len(cfg.AcceptanceAuthorities) == 0 {
		return fmt.Errorf("lifecycle.admission_enabled requires at least one acceptance authority")
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
		if len(cfg.RecoveryAuthorities) == 0 {
			return fmt.Errorf("lifecycle.recovery_enabled requires at least one recovery authority")
		}
	}
	return nil
}

func validateRecoveryAuthorities(cfg LifecycleConfig) error {
	usedKeys := make(map[string]string)
	for _, authorities := range []map[string]string{cfg.AdmissionAuthorities, cfg.AcceptanceAuthorities} {
		for identity, encoded := range authorities {
			key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
			if err == nil {
				usedKeys[string(key)] = identity
			}
		}
	}
	for identity, authority := range cfg.RecoveryAuthorities {
		identity = strings.TrimSpace(identity)
		if identity == "" {
			return fmt.Errorf("lifecycle.recovery_authorities contains an empty identity")
		}
		encoded := strings.TrimSpace(authority.PublicKey)
		key, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil || len(key) != ed25519.PublicKeySize {
			return fmt.Errorf("lifecycle.recovery_authorities.%s.public_key must be a base64-encoded Ed25519 public key", identity)
		}
		if _, exists := usedKeys[string(key)]; exists {
			return fmt.Errorf("lifecycle.recovery_authorities.%s.public_key must use credentials separate from admission and acceptance", identity)
		}
		usedKeys[string(key)] = identity
		if len(authority.Actions) == 0 || len(authority.Scopes) == 0 {
			return fmt.Errorf("lifecycle.recovery_authorities.%s requires explicit actions and scopes", identity)
		}
		seenActions := make(map[string]struct{}, len(authority.Actions))
		for _, action := range authority.Actions {
			action = strings.TrimSpace(action)
			if action != "nudge" {
				return fmt.Errorf("lifecycle.recovery_authorities.%s has unsupported action %q", identity, action)
			}
			if _, exists := seenActions[action]; exists {
				return fmt.Errorf("lifecycle.recovery_authorities.%s repeats action %q", identity, action)
			}
			seenActions[action] = struct{}{}
		}
		seenScopes := make(map[string]struct{}, len(authority.Scopes))
		for _, scope := range authority.Scopes {
			scope = strings.TrimSpace(scope)
			if scope == "" || strings.ContainsAny(scope, "*?\n\r\t") {
				return fmt.Errorf("lifecycle.recovery_authorities.%s scopes must be explicit and cannot contain wildcards", identity)
			}
			if _, exists := seenScopes[scope]; exists {
				return fmt.Errorf("lifecycle.recovery_authorities.%s repeats scope %q", identity, scope)
			}
			seenScopes[scope] = struct{}{}
		}
	}
	return nil
}

func validateCompletionFreshnessConfig(cfg LifecycleConfig) error {
	maxAge := strings.TrimSpace(cfg.CompletionReceiptMaxAge)
	skew := strings.TrimSpace(cfg.CompletionClockSkew)
	if maxAge == "" && skew == "" {
		return nil
	}
	if maxAge == "" || skew == "" {
		return fmt.Errorf("lifecycle.completion_receipt_max_age and lifecycle.completion_clock_skew must be configured together")
	}
	parsedMaxAge, err := time.ParseDuration(maxAge)
	if err != nil || parsedMaxAge <= 0 {
		return fmt.Errorf("lifecycle.completion_receipt_max_age must be a positive duration")
	}
	parsedSkew, err := time.ParseDuration(skew)
	if err != nil || parsedSkew < 0 {
		return fmt.Errorf("lifecycle.completion_clock_skew must be a non-negative duration")
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
