package config

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"path/filepath"
	"strings"
)

// RSIConfig points the controller at signed, controller-owned evaluation and
// human-approval records. Private keys are never stored in city.toml.
// An unset evaluator record deliberately leaves RSI promotion fail-closed.
type RSIConfig struct {
	EvaluationFile         string `toml:"evaluation_file,omitempty"`
	EvaluationKeyID        string `toml:"evaluation_key_id,omitempty"`
	EvaluationPublicKey    string `toml:"evaluation_public_key,omitempty"`
	HumanApprovalFile      string `toml:"human_approval_file,omitempty"`
	HumanApprovalKeyID     string `toml:"human_approval_key_id,omitempty"`
	HumanApprovalPublicKey string `toml:"human_approval_public_key,omitempty"`
}

func validateRSIConfig(cfg RSIConfig) error {
	evaluationParts := []string{cfg.EvaluationFile, cfg.EvaluationKeyID, cfg.EvaluationPublicKey}
	if countRSIConfigured(evaluationParts) != 0 && countRSIConfigured(evaluationParts) != len(evaluationParts) {
		return fmt.Errorf("rsi evaluator requires evaluation_file, evaluation_key_id, and evaluation_public_key")
	}
	if cfg.EvaluationFile != "" {
		if err := validateRSIRelativePath("rsi.evaluation_file", cfg.EvaluationFile); err != nil {
			return err
		}
		if strings.TrimSpace(cfg.EvaluationKeyID) == "" {
			return fmt.Errorf("rsi.evaluation_key_id must not be empty")
		}
		if _, err := decodeRSIPublicKey(cfg.EvaluationPublicKey); err != nil {
			return fmt.Errorf("rsi.evaluation_public_key: %w", err)
		}
	}

	approvalParts := []string{cfg.HumanApprovalFile, cfg.HumanApprovalKeyID, cfg.HumanApprovalPublicKey}
	if countRSIConfigured(approvalParts) != 0 && countRSIConfigured(approvalParts) != len(approvalParts) {
		return fmt.Errorf("rsi human approval requires human_approval_file, human_approval_key_id, and human_approval_public_key")
	}
	if cfg.HumanApprovalFile != "" {
		if cfg.EvaluationFile == "" {
			return fmt.Errorf("rsi human approval requires a configured evaluator")
		}
		if err := validateRSIRelativePath("rsi.human_approval_file", cfg.HumanApprovalFile); err != nil {
			return err
		}
		if strings.TrimSpace(cfg.HumanApprovalKeyID) == "" {
			return fmt.Errorf("rsi.human_approval_key_id must not be empty")
		}
		approvalKey, err := decodeRSIPublicKey(cfg.HumanApprovalPublicKey)
		if err != nil {
			return fmt.Errorf("rsi.human_approval_public_key: %w", err)
		}
		evaluatorKey, _ := decodeRSIPublicKey(cfg.EvaluationPublicKey)
		if bytes.Equal(approvalKey, evaluatorKey) {
			return fmt.Errorf("rsi human approval public key must differ from evaluator public key")
		}
	}
	return nil
}

func countRSIConfigured(values []string) int {
	count := 0
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			count++
		}
	}
	return count
}

func validateRSIRelativePath(name, value string) error {
	if filepath.IsAbs(value) {
		return fmt.Errorf("%s must be relative to the controller city directory", name)
	}
	clean := filepath.Clean(value)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return fmt.Errorf("%s must remain inside the controller city directory", name)
	}
	return nil
}

func decodeRSIPublicKey(value string) (ed25519.PublicKey, error) {
	encoded := strings.TrimSpace(value)
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(encoded)
	if err != nil || len(decoded) != ed25519.PublicKeySize || base64.RawURLEncoding.EncodeToString(decoded) != encoded {
		return nil, fmt.Errorf("must be a canonical base64url Ed25519 public key")
	}
	return ed25519.PublicKey(decoded), nil
}
