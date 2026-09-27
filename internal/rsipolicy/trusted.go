package rsipolicy

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/reviewquorum"
)

// TrustedEvaluationSchemaV1 is the signed evaluator manifest schema accepted
// by FileResolver.
const TrustedEvaluationSchemaV1 = "gc.rsi.trusted-evaluation.v1"

// HumanApprovalSchemaV1 is the separate human approval schema accepted by
// FileResolver.
const HumanApprovalSchemaV1 = "gc.rsi.human-approval.v1"

// PolicyVersionV1 identifies the promotion-policy contract bound into signed
// evaluation and approval records.
const PolicyVersionV1 = "gc.rsipolicy.v1"

const maxTrustedFileSize = 4 << 20

// ErrTrustedEvaluationPending means the controller has not received the
// external evaluator or approval artifact yet. Dispatch leaves the promotion
// gate open so the live control dispatcher can retry when the file arrives.
// Invalid signatures, unknown authorities, and malformed evidence are hard
// errors and must never be classified as pending.
var ErrTrustedEvaluationPending = errors.New("trusted RSI evaluation or approval pending")

// FileResolverConfig is supplied only by the root city configuration. Private
// signing keys do not belong in this config or in candidate worktrees.
type FileResolverConfig struct {
	EvaluationFile      string
	EvaluationKeyID     string
	EvaluationPublicKey string
	HumanApprovalFile   string
	HumanApprovalKeyID  string
	HumanApprovalPubKey string
}

// CandidateRecord contains the candidate bead as read by the controller. The
// actor and session come from durable assignment metadata, not candidate JSON.
type CandidateRecord struct {
	BeadID        string
	ControlBeadID string
	Attempt       int
	MaxAttempts   int
	ActorID       string
	SessionID     string
	Status        string
	Outcome       string
	RawOutput     string
	Proposal      CandidateProposal
}

// CandidateProposal contains only the exact candidate revision the worker
// proposes for evaluation. Baselines, measurements, thresholds, attempts,
// authority classes, and judge identities come from trusted controller input.
type CandidateProposal struct {
	Candidate Bundle `json:"candidate"`
}

// CandidateEvidence is retained as a source-compatible name for the safe
// candidate proposal shape. Worker evidence cannot carry policy inputs.
type CandidateEvidence = CandidateProposal

// JudgeRecord contains a judge bead and its controller-stamped execution
// identity. LaneOutput itself is worker-produced and is trusted only when its
// exact bytes match the signed evaluation record.
type JudgeRecord struct {
	BeadID        string
	ControlBeadID string
	ActorID       string
	SessionID     string
	Status        string
	Outcome       string
	RawOutput     string
	Lane          reviewquorum.LaneOutput
}

// ResolveRequest contains controller-resolved candidate and judge executions.
type ResolveRequest struct {
	Candidate CandidateRecord
	Judges    []JudgeRecord
}

// ResolveTrustedEvaluationFunc reads trusted evidence for one RSI candidate.
type ResolveTrustedEvaluationFunc func(context.Context, ResolveRequest) (TrustedEvaluation, error)

// TrustedEvaluation is assembled by FileResolver from verified controller
// evidence. CandidateEvidence.Input is intentionally not used to build it.
type TrustedEvaluation struct {
	Input                    Input
	EvaluationID             string
	EvaluationManifestSHA256 string
	HumanApprovalVerified    bool
	ApproverID               string
}

// ExecutionPermissions describes an evaluator-attested worker access profile.
type ExecutionPermissions struct {
	CandidateRead         bool `json:"candidate_read"`
	CandidateWrite        bool `json:"candidate_write"`
	PolicyRead            bool `json:"policy_read"`
	PolicyWrite           bool `json:"policy_write"`
	HeldOutDataRead       bool `json:"held_out_data_read"`
	ControllerConfigWrite bool `json:"controller_config_write"`
	SigningKeyRead        bool `json:"signing_key_read"`
	AuthoritativeWrite    bool `json:"authoritative_write"`
	OwnOutputWrite        bool `json:"own_output_write"`
}

// CandidateExecution binds the candidate worker identity and access profile.
type CandidateExecution struct {
	ActorID     string               `json:"actor_id"`
	SessionID   string               `json:"session_id"`
	Permissions ExecutionPermissions `json:"permissions"`
}

// JudgeAuthorization binds one judge lane to its controller-stamped execution.
type JudgeAuthorization struct {
	LaneID        string               `json:"lane_id"`
	BeadID        string               `json:"bead_id"`
	ControlBeadID string               `json:"control_bead_id"`
	ActorID       string               `json:"actor_id"`
	SessionID     string               `json:"session_id"`
	OutputSHA256  string               `json:"output_sha256"`
	Permissions   ExecutionPermissions `json:"permissions"`
}

// EvidenceReference binds one evaluator evidence file by kind, digest, bundle,
// and frozen suite identity.
type EvidenceReference struct {
	Kind          string `json:"kind"`
	Path          string `json:"path"`
	SHA256        string `json:"sha256"`
	BundleID      string `json:"bundle_id,omitempty"`
	EvalSuiteHash string `json:"eval_suite_hash"`
}

// ArtifactIdentityEvidence binds an artifact record to an evaluation and suite.
type ArtifactIdentityEvidence struct {
	SchemaVersion string `json:"schema_version"`
	EvaluationID  string `json:"evaluation_id"`
	Bundle        Bundle `json:"bundle"`
	EvalSuiteHash string `json:"eval_suite_hash"`
}

// AcceptanceLedgerEvidence records frozen work-unit outcomes for both bundles.
type AcceptanceLedgerEvidence struct {
	SchemaVersion          string   `json:"schema_version"`
	EvaluationID           string   `json:"evaluation_id"`
	EvalSuiteHash          string   `json:"eval_suite_hash"`
	AcceptanceUnitIDs      []string `json:"acceptance_unit_ids"`
	BaselineUsefulUnitIDs  []string `json:"baseline_useful_unit_ids"`
	CandidateUsefulUnitIDs []string `json:"candidate_useful_unit_ids"`
}

// WorkerInterval records one worker's interval on an evaluated bundle.
type WorkerInterval struct {
	BundleID   string `json:"bundle_id"`
	ActorID    string `json:"actor_id"`
	SessionID  string `json:"session_id"`
	Phase      string `json:"phase"`
	StartedAt  string `json:"started_at"`
	FinishedAt string `json:"finished_at"`
}

// WorkerTimeLedgerEvidence records all work intervals for one evaluation.
type WorkerTimeLedgerEvidence struct {
	SchemaVersion     string           `json:"schema_version"`
	EvaluationID      string           `json:"evaluation_id"`
	EvalSuiteHash     string           `json:"eval_suite_hash"`
	BaselineBundleID  string           `json:"baseline_bundle_id"`
	CandidateBundleID string           `json:"candidate_bundle_id"`
	Intervals         []WorkerInterval `json:"intervals"`
}

// TrustedEvaluationManifest is signed by the configured evaluator key. It
// binds one candidate bead, exact source bundles, frozen suite, measured work,
// judge executions, and immutable evidence files.
type TrustedEvaluationManifest struct {
	SchemaVersion          string               `json:"schema_version"`
	ID                     string               `json:"id"`
	PolicyVersion          string               `json:"policy_version"`
	EvaluatorKeyID         string               `json:"evaluator_key_id"`
	IssuedAt               string               `json:"issued_at"`
	ExpiresAt              string               `json:"expires_at"`
	CandidateBeadID        string               `json:"candidate_bead_id"`
	CandidateControlBeadID string               `json:"candidate_control_bead_id"`
	CandidateOutputSHA256  string               `json:"candidate_output_sha256"`
	Objective              string               `json:"objective"`
	Current                Bundle               `json:"current"`
	Candidate              Bundle               `json:"candidate"`
	EvalSuiteHash          string               `json:"eval_suite_hash"`
	Baseline               Metrics              `json:"baseline"`
	CandidateMetrics       Metrics              `json:"candidate_metrics"`
	Limits                 Limits               `json:"limits"`
	AuthorityClass         string               `json:"authority_class"`
	Attempt                int                  `json:"attempt"`
	MaxAttempts            int                  `json:"max_attempts"`
	WorkAccounting         WorkAccounting       `json:"work_accounting"`
	CandidateExecution     CandidateExecution   `json:"candidate_execution"`
	Judges                 []JudgeAuthorization `json:"judges"`
	Evidence               []EvidenceReference  `json:"evidence"`
}

// HumanApprovalManifest is signed by a separate human key. Approval is bound
// to the exact signed evaluation file, both bundles, the frozen suite, and the
// policy version; it is not a reusable "promote=true" flag.
type HumanApprovalManifest struct {
	SchemaVersion            string `json:"schema_version"`
	Decision                 string `json:"decision"`
	PolicyVersion            string `json:"policy_version"`
	ApprovalKeyID            string `json:"approval_key_id"`
	ApproverID               string `json:"approver_id"`
	EvaluationID             string `json:"evaluation_id"`
	EvaluationManifestSHA256 string `json:"evaluation_manifest_sha256"`
	CandidateBeadID          string `json:"candidate_bead_id"`
	CandidateBundleID        string `json:"candidate_bundle_id"`
	BaselineBundleID         string `json:"baseline_bundle_id"`
	EvalSuiteHash            string `json:"eval_suite_hash"`
	ApprovedAt               string `json:"approved_at"`
}

type signedEnvelope struct {
	Payload   json.RawMessage `json:"payload"`
	Signature string          `json:"signature"`
}

// FileResolver verifies controller-owned evaluator and human-signed artifacts.
type FileResolver struct {
	cityPath string
	config   FileResolverConfig
}

// NewFileResolver creates a resolver rooted at one controller city directory.
func NewFileResolver(cityPath string, cfg FileResolverConfig) FileResolver {
	return FileResolver{cityPath: cityPath, config: cfg}
}

// Resolve loads and validates the signed evaluator report and separate human
// approval for request.
func (r FileResolver) Resolve(ctx context.Context, request ResolveRequest) (TrustedEvaluation, error) {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return TrustedEvaluation{}, err
		}
	}
	if strings.TrimSpace(r.config.EvaluationFile) == "" || strings.TrimSpace(r.config.EvaluationKeyID) == "" || strings.TrimSpace(r.config.EvaluationPublicKey) == "" {
		return TrustedEvaluation{}, errors.New("trusted RSI evaluation is not configured")
	}
	evaluationPath, err := controllerOwnedPath(r.cityPath, r.config.EvaluationFile)
	if err != nil {
		return TrustedEvaluation{}, fmt.Errorf("evaluation file path: %w", err)
	}
	publicKey, err := decodePublicKey(r.config.EvaluationPublicKey)
	if err != nil {
		return TrustedEvaluation{}, fmt.Errorf("evaluation public key: %w", err)
	}
	manifestBytes, err := readRegularFile(evaluationPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return TrustedEvaluation{}, fmt.Errorf("%w: evaluation file has not arrived", ErrTrustedEvaluationPending)
		}
		return TrustedEvaluation{}, fmt.Errorf("read trusted evaluation: %w", err)
	}
	var manifest TrustedEvaluationManifest
	if err := verifySignedPayload(manifestBytes, publicKey, &manifest); err != nil {
		return TrustedEvaluation{}, fmt.Errorf("verify trusted evaluation: %w", err)
	}
	if err := validateManifest(manifest, r.config.EvaluationKeyID, evaluationPath, request); err != nil {
		return TrustedEvaluation{}, err
	}

	evaluationDigest := sha256.Sum256(manifestBytes)
	input := manifest.input(request)
	approvalID, approvalVerified, err := r.resolveApproval(manifest, evaluationDigest, publicKey)
	if err != nil {
		return TrustedEvaluation{}, err
	}
	input.HumanApprovalVerified = approvalVerified
	return TrustedEvaluation{
		Input:                    input,
		EvaluationID:             manifest.ID,
		EvaluationManifestSHA256: hex.EncodeToString(evaluationDigest[:]),
		HumanApprovalVerified:    approvalVerified,
		ApproverID:               approvalID,
	}, nil
}

func (r FileResolver) resolveApproval(manifest TrustedEvaluationManifest, evaluationDigest [32]byte, evaluatorKey ed25519.PublicKey) (string, bool, error) {
	approvalPathText := strings.TrimSpace(r.config.HumanApprovalFile)
	approvalKeyID := strings.TrimSpace(r.config.HumanApprovalKeyID)
	approvalKeyText := strings.TrimSpace(r.config.HumanApprovalPubKey)
	if approvalPathText == "" && approvalKeyID == "" && approvalKeyText == "" {
		return "", false, fmt.Errorf("%w: human approval trust is not configured", ErrTrustedEvaluationPending)
	}
	if approvalPathText == "" || approvalKeyID == "" || approvalKeyText == "" {
		return "", false, errors.New("human approval requires file, key id, and public key")
	}
	approvalKey, err := decodePublicKey(approvalKeyText)
	if err != nil {
		return "", false, fmt.Errorf("human approval public key: %w", err)
	}
	if bytes.Equal(approvalKey, evaluatorKey) {
		return "", false, errors.New("human approval key must differ from evaluator key")
	}
	approvalPath, err := controllerOwnedPath(r.cityPath, approvalPathText)
	if err != nil {
		return "", false, fmt.Errorf("human approval file path: %w", err)
	}
	approvalBytes, err := readRegularFile(approvalPath)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, fmt.Errorf("%w: human approval file has not arrived", ErrTrustedEvaluationPending)
	}
	if err != nil {
		return "", false, fmt.Errorf("read human approval: %w", err)
	}
	var approval HumanApprovalManifest
	if err := verifySignedPayload(approvalBytes, approvalKey, &approval); err != nil {
		return "", false, fmt.Errorf("verify human approval: %w", err)
	}
	if err := validateApproval(approval, manifest, r.config.HumanApprovalKeyID, evaluationDigest); err != nil {
		return "", false, err
	}
	return approval.ApproverID, true, nil
}

func (manifest TrustedEvaluationManifest) input(request ResolveRequest) Input {
	judges := make([]string, 0, len(manifest.Judges))
	executions := make([]JudgeExecutionIdentity, 0, len(manifest.Judges))
	lanes := make([]reviewquorum.LaneOutput, 0, len(request.Judges))
	for _, judge := range manifest.Judges {
		judges = append(judges, judge.LaneID)
		for _, record := range request.Judges {
			if record.BeadID == judge.BeadID {
				executions = append(executions, JudgeExecutionIdentity{LaneID: judge.LaneID, ActorID: record.ActorID, SessionID: record.SessionID})
				lanes = append(lanes, record.Lane)
				break
			}
		}
	}
	return Input{
		Objective:        manifest.Objective,
		Current:          manifest.Current,
		Candidate:        manifest.Candidate,
		Baseline:         manifest.Baseline,
		CandidateMetrics: manifest.CandidateMetrics,
		Limits:           manifest.Limits,
		WorkAccounting:   manifest.WorkAccounting,
		AuthorityClass:   manifest.AuthorityClass,
		Attempts:         manifest.Attempt,
		MaxAttempts:      manifest.MaxAttempts,
		Review: Review{
			Improver:        request.Candidate.ActorID,
			ImproverSession: request.Candidate.SessionID,
			Judges:          judges,
			Executions:      executions,
			Subject:         "candidate:" + manifest.Candidate.ID,
			BaseRef:         manifest.Current.ID,
			LaneOutputs:     lanes,
		},
	}
}

func validateManifest(manifest TrustedEvaluationManifest, expectedKeyID string, manifestPath string, request ResolveRequest) error {
	if manifest.SchemaVersion != TrustedEvaluationSchemaV1 || manifest.PolicyVersion != PolicyVersionV1 {
		return errors.New("trusted evaluation schema or policy version is unsupported")
	}
	if strings.TrimSpace(manifest.ID) == "" || manifest.EvaluatorKeyID != expectedKeyID {
		return errors.New("trusted evaluation identity does not match controller configuration")
	}
	if err := validateFreshness(manifest.IssuedAt, manifest.ExpiresAt); err != nil {
		return fmt.Errorf("trusted evaluation freshness: %w", err)
	}
	if strings.TrimSpace(manifest.Objective) == "" || strings.TrimSpace(manifest.AuthorityClass) == "" ||
		strings.TrimSpace(manifest.CandidateBeadID) == "" || manifest.CandidateBeadID != request.Candidate.BeadID ||
		strings.TrimSpace(manifest.CandidateControlBeadID) == "" || manifest.CandidateControlBeadID != request.Candidate.ControlBeadID {
		return errors.New("trusted evaluation does not identify the candidate bead and objective")
	}
	if request.Candidate.Status != "closed" || request.Candidate.Outcome != "pass" {
		return errors.New("candidate evidence bead is not closed with a passing outcome")
	}
	if request.Candidate.ActorID != manifest.CandidateExecution.ActorID || request.Candidate.SessionID != manifest.CandidateExecution.SessionID ||
		request.Candidate.ActorID == "" || request.Candidate.SessionID == "" {
		return errors.New("candidate execution identity does not match trusted evaluation")
	}
	if request.Candidate.Attempt < 1 || request.Candidate.MaxAttempts < 1 ||
		manifest.Attempt != request.Candidate.Attempt || manifest.MaxAttempts != request.Candidate.MaxAttempts {
		return errors.New("trusted evaluation attempt budget does not match the controller retry state")
	}
	if !candidatePermissionsValid(manifest.CandidateExecution.Permissions) {
		return errors.New("candidate execution has access to evaluator policy or authoritative results")
	}
	if !validBundle(manifest.Current) || !validBundle(manifest.Candidate) ||
		manifest.Candidate.Parent != manifest.Current.ID || manifest.EvalSuiteHash == "" ||
		manifest.Current.EvalSuiteHash != manifest.EvalSuiteHash || manifest.Candidate.EvalSuiteHash != manifest.EvalSuiteHash ||
		manifest.Current.ID == manifest.Candidate.ID || !bundleRevisionChanged(manifest.Current, manifest.Candidate) {
		return errors.New("trusted evaluation bundle lineage or suite identity is invalid")
	}
	if !equalBundle(request.Candidate.Proposal.Candidate, manifest.Candidate) {
		return errors.New("candidate revision does not match the trusted evaluation")
	}
	if digestText(request.Candidate.RawOutput) != manifest.CandidateOutputSHA256 {
		return errors.New("candidate output digest does not match trusted evaluation")
	}
	if !validMetrics(manifest.Baseline) || !validMetrics(manifest.CandidateMetrics) || !validLimits(manifest.Limits) ||
		!validWorkAccounting(manifest.WorkAccounting) {
		return errors.New("trusted evaluation is missing valid measurements, limits, or fixed work accounting")
	}
	if len(manifest.Judges) < 2 || len(request.Judges) != len(manifest.Judges) {
		return errors.New("trusted evaluation requires at least two bound judge executions")
	}
	if err := validateJudgeBindings(manifest, request); err != nil {
		return err
	}
	if err := validateEvidenceReferences(manifest.Evidence, manifestPath, manifest, request); err != nil {
		return err
	}
	return nil
}

func bundleRevisionChanged(current, candidate Bundle) bool {
	return current.TypesCommit != candidate.TypesCommit || current.AICommit != candidate.AICommit ||
		current.FrontendCommit != candidate.FrontendCommit || current.InktreeCommit != candidate.InktreeCommit
}

func validateJudgeBindings(manifest TrustedEvaluationManifest, request ResolveRequest) error {
	seenBeads := make(map[string]struct{}, len(manifest.Judges))
	seenLanes := make(map[string]struct{}, len(manifest.Judges))
	seenActors := map[string]struct{}{request.Candidate.ActorID: {}}
	seenSessions := map[string]struct{}{request.Candidate.SessionID: {}}
	actualByBead := make(map[string]JudgeRecord, len(request.Judges))
	for _, record := range request.Judges {
		if _, exists := actualByBead[record.BeadID]; exists {
			return errors.New("judge evidence bead is duplicated")
		}
		actualByBead[record.BeadID] = record
	}
	for _, expected := range manifest.Judges {
		if expected.LaneID == "" || expected.BeadID == "" {
			return errors.New("judge authorization is missing lane or bead identity")
		}
		if _, exists := seenBeads[expected.BeadID]; exists {
			return errors.New("trusted evaluation repeats a judge bead")
		}
		if _, exists := seenLanes[expected.LaneID]; exists {
			return errors.New("trusted evaluation repeats a judge lane")
		}
		seenBeads[expected.BeadID] = struct{}{}
		seenLanes[expected.LaneID] = struct{}{}
		record, ok := actualByBead[expected.BeadID]
		if !ok || record.ControlBeadID != expected.ControlBeadID || record.Status != "closed" || record.Outcome != "pass" ||
			record.ActorID != expected.ActorID || record.SessionID != expected.SessionID ||
			record.ActorID == "" || record.SessionID == "" {
			return errors.New("judge execution identity or outcome does not match trusted evaluation")
		}
		if record.Lane.LaneID != expected.LaneID || digestText(record.RawOutput) != expected.OutputSHA256 {
			return errors.New("judge lane output does not match its signed evidence digest")
		}
		if _, exists := seenActors[record.ActorID]; exists {
			return errors.New("candidate and judges must use separate actors")
		}
		if _, exists := seenSessions[record.SessionID]; exists {
			return errors.New("candidate and judges must use separate sessions")
		}
		seenActors[record.ActorID] = struct{}{}
		seenSessions[record.SessionID] = struct{}{}
		if !judgePermissionsValid(expected.Permissions) {
			return fmt.Errorf("judge lane %q has write access to candidate artifacts, policy, or authoritative results", expected.LaneID)
		}
		lane := record.Lane
		if !lane.ReadOnlyEnforcement.Observed || !lane.ReadOnlyEnforcement.Enabled || !lane.ReadOnlyEnforcement.Passed ||
			lane.ReadOnlyEnforcement.BaselineCommand == "" || lane.ReadOnlyEnforcement.AfterCommand == "" || len(lane.MutationsDelta.Changed) > 0 {
			return fmt.Errorf("judge lane %q lacks verified read-only execution evidence", expected.LaneID)
		}
	}
	return nil
}

func candidatePermissionsValid(p ExecutionPermissions) bool {
	return p.CandidateRead && p.CandidateWrite && !p.PolicyWrite && !p.HeldOutDataRead &&
		!p.ControllerConfigWrite && !p.SigningKeyRead && !p.AuthoritativeWrite && p.OwnOutputWrite
}

func judgePermissionsValid(p ExecutionPermissions) bool {
	return p.CandidateRead && !p.CandidateWrite && p.PolicyRead && !p.PolicyWrite &&
		!p.HeldOutDataRead && !p.ControllerConfigWrite && !p.SigningKeyRead && !p.AuthoritativeWrite && p.OwnOutputWrite
}

func validateEvidenceReferences(refs []EvidenceReference, manifestPath string, manifest TrustedEvaluationManifest, request ResolveRequest) error {
	required := map[string]string{
		"baseline-artifact":  manifest.Current.ID,
		"candidate-artifact": manifest.Candidate.ID,
		"acceptance-ledger":  "",
		"worker-time-ledger": "",
	}
	for _, judge := range manifest.Judges {
		required["judge-lane:"+judge.LaneID] = manifest.Candidate.ID
	}
	baseDir := filepath.Dir(manifestPath)
	contents := make(map[string][]byte, len(refs))
	for _, ref := range refs {
		if _, ok := required[ref.Kind]; !ok {
			return fmt.Errorf("trusted evaluation contains unknown evidence kind %q", ref.Kind)
		}
		if required[ref.Kind] != ref.BundleID {
			return fmt.Errorf("trusted evaluation evidence %q is bound to the wrong artifact", ref.Kind)
		}
		if ref.EvalSuiteHash != manifest.EvalSuiteHash || !validSHA256(ref.SHA256) || strings.TrimSpace(ref.Path) == "" {
			return fmt.Errorf("trusted evaluation evidence %q is missing a digest or suite binding", ref.Kind)
		}
		data, err := readEvidenceFile(baseDir, ref.Path)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("%w: signed evidence artifact %q has not arrived", ErrTrustedEvaluationPending, ref.Kind)
			}
			return fmt.Errorf("trusted evaluation evidence %q: %w", ref.Kind, err)
		}
		if digestText(string(data)) != strings.ToLower(ref.SHA256) {
			return fmt.Errorf("trusted evaluation evidence %q: file digest does not match the signed reference", ref.Kind)
		}
		contents[ref.Kind] = data
		delete(required, ref.Kind)
	}
	if len(required) > 0 {
		missing := make([]string, 0, len(required))
		for kind := range required {
			missing = append(missing, kind)
		}
		return fmt.Errorf("trusted evaluation is missing evidence references: %s", strings.Join(missing, ", "))
	}
	for _, item := range []struct {
		kind   string
		bundle Bundle
	}{
		{kind: "baseline-artifact", bundle: manifest.Current},
		{kind: "candidate-artifact", bundle: manifest.Candidate},
	} {
		var evidence ArtifactIdentityEvidence
		if err := decodeStrictJSON(contents[item.kind], &evidence); err != nil {
			return fmt.Errorf("trusted evaluation evidence %q is malformed: %w", item.kind, err)
		}
		if evidence.SchemaVersion != "gc.rsi.artifact-identity.v1" || evidence.EvaluationID != manifest.ID ||
			!equalBundle(evidence.Bundle, item.bundle) || evidence.EvalSuiteHash != manifest.EvalSuiteHash {
			return fmt.Errorf("trusted evaluation evidence %q does not identify the bound artifact", item.kind)
		}
	}
	var acceptance AcceptanceLedgerEvidence
	if err := decodeStrictJSON(contents["acceptance-ledger"], &acceptance); err != nil {
		return fmt.Errorf("trusted acceptance ledger is malformed: %w", err)
	}
	if acceptance.SchemaVersion != "gc.rsi.acceptance-ledger.v1" || acceptance.EvaluationID != manifest.ID ||
		acceptance.EvalSuiteHash != manifest.EvalSuiteHash ||
		!equalStringSet(acceptance.AcceptanceUnitIDs, manifest.WorkAccounting.AcceptanceUnitIDs) ||
		!equalStringSet(acceptance.BaselineUsefulUnitIDs, manifest.WorkAccounting.BaselineUsefulUnitIDs) ||
		!equalStringSet(acceptance.CandidateUsefulUnitIDs, manifest.WorkAccounting.CandidateUsefulUnitIDs) {
		return errors.New("trusted acceptance ledger does not match the frozen acceptance set and per-unit results")
	}
	var workerTime WorkerTimeLedgerEvidence
	if err := decodeStrictJSON(contents["worker-time-ledger"], &workerTime); err != nil {
		return fmt.Errorf("trusted worker-time ledger is malformed: %w", err)
	}
	if err := validateWorkerTimeLedger(workerTime, manifest, request); err != nil {
		return err
	}
	for _, judge := range manifest.Judges {
		var record *JudgeRecord
		for i := range request.Judges {
			if request.Judges[i].BeadID == judge.BeadID {
				record = &request.Judges[i]
				break
			}
		}
		if record == nil || !bytes.Equal(contents["judge-lane:"+judge.LaneID], []byte(record.RawOutput)) {
			return fmt.Errorf("trusted judge evidence for lane %q differs from the assigned judge output", judge.LaneID)
		}
	}
	return nil
}

func validateWorkerTimeLedger(ledger WorkerTimeLedgerEvidence, manifest TrustedEvaluationManifest, request ResolveRequest) error {
	if ledger.SchemaVersion != "gc.rsi.worker-time-ledger.v1" || ledger.EvaluationID != manifest.ID ||
		ledger.EvalSuiteHash != manifest.EvalSuiteHash || ledger.BaselineBundleID != manifest.Current.ID ||
		ledger.CandidateBundleID != manifest.Candidate.ID || len(ledger.Intervals) == 0 {
		return errors.New("trusted worker-time ledger does not identify the bound evaluation")
	}
	type intervalWithTime struct {
		WorkerInterval
		start time.Time
		end   time.Time
	}
	intervals := make([]intervalWithTime, 0, len(ledger.Intervals))
	type timeTotals struct{ implementation, recovery, evaluation float64 }
	totals := map[string]*timeTotals{
		manifest.Current.ID:   {},
		manifest.Candidate.ID: {},
	}
	seen := make(map[string]struct{}, len(ledger.Intervals))
	candidateWorkBound := false
	judgeEvaluationBound := make(map[string]bool, len(manifest.Judges))
	for _, judge := range manifest.Judges {
		var actual *JudgeRecord
		for i := range request.Judges {
			if request.Judges[i].BeadID == judge.BeadID {
				actual = &request.Judges[i]
				break
			}
		}
		if actual == nil {
			return errors.New("worker-time ledger cannot bind an absent judge execution")
		}
		judgeEvaluationBound[actual.ActorID+"\x00"+actual.SessionID] = false
	}
	for _, interval := range ledger.Intervals {
		if _, ok := totals[interval.BundleID]; !ok || strings.TrimSpace(interval.ActorID) == "" || strings.TrimSpace(interval.SessionID) == "" {
			return errors.New("worker-time interval is missing a bound bundle or execution identity")
		}
		start, startErr := time.Parse(time.RFC3339, interval.StartedAt)
		end, endErr := time.Parse(time.RFC3339, interval.FinishedAt)
		if startErr != nil || endErr != nil || !end.After(start) {
			return errors.New("worker-time interval has invalid timestamps")
		}
		key := strings.Join([]string{interval.BundleID, interval.ActorID, interval.SessionID, interval.Phase, interval.StartedAt, interval.FinishedAt}, "\x00")
		if _, duplicate := seen[key]; duplicate {
			return errors.New("worker-time ledger repeats an interval")
		}
		seen[key] = struct{}{}
		if interval.BundleID == manifest.Candidate.ID && interval.ActorID == request.Candidate.ActorID && interval.SessionID == request.Candidate.SessionID &&
			(interval.Phase == "implementation" || interval.Phase == "recovery") {
			candidateWorkBound = true
		}
		if interval.BundleID == manifest.Candidate.ID && interval.Phase == "evaluation" {
			if _, ok := judgeEvaluationBound[interval.ActorID+"\x00"+interval.SessionID]; ok {
				judgeEvaluationBound[interval.ActorID+"\x00"+interval.SessionID] = true
			}
		}
		switch interval.Phase {
		case "implementation", "recovery", "evaluation":
		default:
			return fmt.Errorf("worker-time interval has unknown phase %q", interval.Phase)
		}
		intervals = append(intervals, intervalWithTime{WorkerInterval: interval, start: start, end: end})
		hours := end.Sub(start).Hours()
		total := totals[interval.BundleID]
		switch interval.Phase {
		case "implementation":
			total.implementation += hours
		case "recovery":
			total.recovery += hours
		case "evaluation":
			total.evaluation += hours
		}
	}
	sort.Slice(intervals, func(i, j int) bool {
		left, right := intervals[i], intervals[j]
		if left.BundleID != right.BundleID {
			return left.BundleID < right.BundleID
		}
		if left.ActorID != right.ActorID {
			return left.ActorID < right.ActorID
		}
		if left.SessionID != right.SessionID {
			return left.SessionID < right.SessionID
		}
		return left.start.Before(right.start)
	})
	for i := 1; i < len(intervals); i++ {
		previous, current := intervals[i-1], intervals[i]
		if previous.BundleID == current.BundleID && previous.ActorID == current.ActorID && previous.SessionID == current.SessionID && current.start.Before(previous.end) {
			return errors.New("worker-time intervals overlap for one actor session")
		}
	}
	if !candidateWorkBound {
		return errors.New("worker-time ledger does not bind implementation or recovery work to the assigned candidate execution")
	}
	for identity, bound := range judgeEvaluationBound {
		if !bound {
			return fmt.Errorf("worker-time ledger does not bind evaluation time to judge execution %q", strings.ReplaceAll(identity, "\x00", "/"))
		}
	}
	if !workerTimeEqual(timeTotalsForComparison{
		ImplementationHours: totals[manifest.Current.ID].implementation,
		RecoveryHours:       totals[manifest.Current.ID].recovery,
		EvaluationHours:     totals[manifest.Current.ID].evaluation,
	}, manifest.WorkAccounting.BaselineWorkerTime) ||
		!workerTimeEqual(timeTotalsForComparison{
			ImplementationHours: totals[manifest.Candidate.ID].implementation,
			RecoveryHours:       totals[manifest.Candidate.ID].recovery,
			EvaluationHours:     totals[manifest.Candidate.ID].evaluation,
		}, manifest.WorkAccounting.CandidateWorkerTime) {
		return errors.New("worker-time ledger totals do not match the signed measurements")
	}
	return nil
}

func workerTimeEqual(actual timeTotalsForComparison, expected WorkerTime) bool {
	return floatNear(actual.ImplementationHours, expected.ImplementationHours) &&
		floatNear(actual.RecoveryHours, expected.RecoveryHours) && floatNear(actual.EvaluationHours, expected.EvaluationHours)
}

type timeTotalsForComparison struct {
	ImplementationHours float64
	RecoveryHours       float64
	EvaluationHours     float64
}

func floatNear(actual, expected float64) bool {
	tolerance := 0.000001
	return math.Abs(actual-expected) <= tolerance
}

func equalStringSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	copyA := append([]string(nil), a...)
	copyB := append([]string(nil), b...)
	sort.Strings(copyA)
	sort.Strings(copyB)
	for i := range copyA {
		if copyA[i] != copyB[i] {
			return false
		}
	}
	return true
}

func validateApproval(approval HumanApprovalManifest, manifest TrustedEvaluationManifest, expectedKeyID string, evaluationDigest [32]byte) error {
	if approval.SchemaVersion != HumanApprovalSchemaV1 || approval.Decision != "approve" || approval.PolicyVersion != PolicyVersionV1 {
		return errors.New("human approval schema, decision, or policy version is invalid")
	}
	if approval.ApprovalKeyID != expectedKeyID || strings.TrimSpace(approval.ApproverID) == "" || approval.ApproverID != expectedKeyID {
		return errors.New("human approver identity does not match controller configuration")
	}
	if approval.EvaluationID != manifest.ID || approval.EvaluationManifestSHA256 != hex.EncodeToString(evaluationDigest[:]) ||
		approval.CandidateBeadID != manifest.CandidateBeadID || approval.CandidateBundleID != manifest.Candidate.ID ||
		approval.BaselineBundleID != manifest.Current.ID || approval.EvalSuiteHash != manifest.EvalSuiteHash {
		return errors.New("human approval is not bound to this exact candidate evaluation")
	}
	approvedAt, err := time.Parse(time.RFC3339, approval.ApprovedAt)
	if err != nil || approvedAt.After(time.Now().Add(time.Minute)) {
		return errors.New("human approval timestamp is invalid")
	}
	return nil
}

func validateFreshness(issuedText, expiresText string) error {
	issued, issuedErr := time.Parse(time.RFC3339, issuedText)
	expires, expiresErr := time.Parse(time.RFC3339, expiresText)
	now := time.Now()
	if issuedErr != nil || expiresErr != nil || !expires.After(issued) || issued.After(now.Add(time.Minute)) || !expires.After(now) {
		return errors.New("issued_at or expires_at is invalid or expired")
	}
	return nil
}

func equalBundle(a, b Bundle) bool { return a == b }

func digestText(text string) string {
	digest := sha256.Sum256([]byte(text))
	return hex.EncodeToString(digest[:])
}

func validSHA256(text string) bool {
	decoded, err := hex.DecodeString(strings.TrimSpace(text))
	return err == nil && len(decoded) == sha256.Size
}

func decodePublicKey(text string) (ed25519.PublicKey, error) {
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(strings.TrimSpace(text))
	if err != nil || len(decoded) != ed25519.PublicKeySize || base64.RawURLEncoding.EncodeToString(decoded) != strings.TrimSpace(text) {
		return nil, errors.New("must be a canonical base64url Ed25519 public key")
	}
	return ed25519.PublicKey(decoded), nil
}

func verifySignedPayload(file []byte, publicKey ed25519.PublicKey, dst any) error {
	var envelope signedEnvelope
	if err := decodeStrictJSON(file, &envelope); err != nil {
		return err
	}
	if len(envelope.Payload) == 0 {
		return errors.New("signed payload is empty")
	}
	signature, err := base64.RawURLEncoding.Strict().DecodeString(envelope.Signature)
	if err != nil || len(signature) != ed25519.SignatureSize || base64.RawURLEncoding.EncodeToString(signature) != envelope.Signature {
		return errors.New("signature is not canonical base64url Ed25519 data")
	}
	if !ed25519.Verify(publicKey, envelope.Payload, signature) {
		return errors.New("signature verification failed")
	}
	return decodeStrictJSON(envelope.Payload, dst)
}

func decodeStrictJSON(data []byte, dst any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values are not allowed")
		}
		return err
	}
	return nil
}

func controllerOwnedPath(cityPath, configured string) (string, error) {
	if strings.TrimSpace(cityPath) == "" || filepath.IsAbs(configured) {
		return "", errors.New("path must be relative to the controller city directory")
	}
	clean := filepath.Clean(configured)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", errors.New("path must remain inside the controller city directory")
	}
	root, err := filepath.Abs(cityPath)
	if err != nil {
		return "", err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	current := root
	parts := strings.Split(clean, string(filepath.Separator))
	for i, part := range parts {
		current = filepath.Join(current, part)
		info, statErr := os.Lstat(current)
		if errors.Is(statErr, os.ErrNotExist) {
			return filepath.Join(root, clean), nil
		}
		if statErr != nil {
			return "", statErr
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return "", errors.New("symlink paths are not allowed")
		}
		if i < len(parts)-1 && !info.IsDir() {
			return "", errors.New("path component is not a directory")
		}
	}
	return current, nil
}

func readRegularFile(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > maxTrustedFileSize {
		return nil, errors.New("trusted evidence must be a regular file within the size limit")
	}
	return os.ReadFile(path)
}

func readEvidenceFile(baseDir, relativePath string) ([]byte, error) {
	if filepath.IsAbs(relativePath) {
		return nil, errors.New("evidence path must be relative to the evaluation file")
	}
	clean := filepath.Clean(relativePath)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return nil, errors.New("evidence path must remain inside the evaluation directory")
	}
	base, err := filepath.EvalSymlinks(baseDir)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(baseDir, clean)
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, err
	}
	rel, err := filepath.Rel(base, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return nil, errors.New("evidence path resolves outside the evaluation directory")
	}
	data, err := readRegularFile(resolved)
	if err != nil {
		return nil, err
	}
	return data, nil
}
