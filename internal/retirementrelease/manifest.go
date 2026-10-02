package retirementrelease

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/gastownhall/gascity/internal/qualification"
)

const maxManifestBytes = 16 << 20

// SourceIdentity is the complete source/backend/runtime identity independently
// derived by trusted composition and compared to the retained expected context.
type SourceIdentity = sourceIdentity

// Context is the exact expected context protocol; CurrentContext is independently
// derived by trusted composition, not decoded from a caller-provided snapshot.
type Context = releaseContext

type sourceIdentity struct {
	ControllerSourceCommit string `json:"controller_source_commit"`
	PackSourceCommit       string `json:"pack_source_commit"`
	BackendSourceCommit    string `json:"backend_source_commit"`
	ControllerBinarySHA256 string `json:"controller_binary_sha256"`
	BackendBinarySHA256    string `json:"backend_binary_sha256"`
	RuntimeIdentitySHA256  string `json:"runtime_identity_sha256"`
	BackendIdentitySHA256  string `json:"backend_identity_sha256"`
	EffectiveConfigSHA256  string `json:"effective_config_sha256"`
}

type releaseContext struct {
	SchemaVersion        int            `json:"schema_version"`
	Audience             string         `json:"audience"`
	Workspace            string         `json:"workspace"`
	HostSHA256           string         `json:"host_sha256"`
	BootSHA256           string         `json:"boot_sha256"`
	ControllerGeneration uint64         `json:"controller_generation"`
	ExecutionGeneration  string         `json:"execution_generation"`
	Source               sourceIdentity `json:"source"`
	Rigs                 []string       `json:"rigs"`
	ObservedAt           string         `json:"observed_at"`
	ExpiresAt            string         `json:"expires_at"`
}

type manifest struct {
	SchemaVersion           int             `json:"schema_version"`
	CandidateManifestSHA256 string          `json:"candidate_manifest_sha256"`
	ReleaseBundleSHA256     string          `json:"release_bundle_sha256"`
	InventorySHA256         string          `json:"inventory_sha256"`
	Candidate               json.RawMessage `json:"candidate"`
	Inventory               json.RawMessage `json:"inventory"`
	Context                 releaseContext  `json:"context"`
	Activation              json.RawMessage `json:"activation"`
	TrustedNow              string          `json:"trusted_now"`
	Gate                    string          `json:"gate"`
}

var gates = map[string]bool{
	"runtime-identities": true, "trial-ledger": true, "external-writers": true,
	"manual-audit": true, "in-flight-resolution": true, "human-review": true,
	"compatibility": true, "release-authorization": true, "retirement-authorization": true,
	"expansion": true, "rollback": true, "startup-recovery": true, "admission-v2": true,
	"owner-fencing": true, "retirement-effect": true, "activation-authorization": true,
	"human-gate-workflow": true,
}

func parseManifest(text string) (manifest, error) {
	var m manifest
	if len(text) == 0 || len(text) > maxManifestBytes || !utf8.ValidString(text) {
		return m, errors.New("manifest size or encoding invalid")
	}
	d := json.NewDecoder(strings.NewReader(text))
	d.UseNumber()
	v, err := strictValue(d, 0)
	if err != nil {
		return m, err
	}
	if _, err := d.Token(); err != io.EOF {
		return m, errors.New("manifest has trailing JSON")
	}
	canonical, err := qualification.CanonicalJSON(v)
	if err != nil || !bytes.Equal(canonical, []byte(text)) {
		return m, errors.New("manifest is not canonical JSON")
	}
	root, err := exact(v, "schema_version candidate_manifest_sha256 release_bundle_sha256 inventory_sha256 candidate inventory context activation trusted_now gate")
	if err != nil {
		return m, err
	}
	contextObject, err := exact(root["context"], "schema_version audience workspace host_sha256 boot_sha256 controller_generation execution_generation source rigs observed_at expires_at")
	if err != nil {
		return m, err
	}
	if _, err := exact(contextObject["source"], "controller_source_commit pack_source_commit backend_source_commit controller_binary_sha256 backend_binary_sha256 runtime_identity_sha256 backend_identity_sha256 effective_config_sha256"); err != nil {
		return m, err
	}
	if err := json.Unmarshal(canonical, &m); err != nil {
		return m, err
	}
	if m.SchemaVersion != 1 || !gates[m.Gate] || !validDigest(m.CandidateManifestSHA256) || !validDigest(m.ReleaseBundleSHA256) || !validDigest(m.InventorySHA256) {
		return m, errors.New("manifest identity invalid")
	}
	candidate, err := exact(root["candidate"], "schema_version claim_scope phases trial decision_observations safety_scenarios completions human_review")
	if err != nil || candidate["schema_version"] != json.Number("1") {
		return m, errors.New("candidate envelope invalid")
	}
	if err := validateCandidateShape(candidate); err != nil {
		return m, err
	}
	trial, err := exact(candidate["trial"], "id mode rig started_at ended_at source selector_baseline excluded_orders occupancy_intervals")
	if err != nil {
		return m, err
	}
	source, err := exact(trial["source"], "controller_source_commit pack_source_commit backend_source_commit controller_binary_sha256 backend_binary_sha256 runtime_identity_sha256 backend_identity_sha256 effective_config_sha256 identity_evidence")
	if err != nil {
		return m, err
	}
	if err := validateReference(source["identity_evidence"]); err != nil {
		return m, err
	}
	baseline, ok := trial["selector_baseline"].(map[string]any)
	if !ok {
		return m, errors.New("selector baseline must be an object")
	}
	for path, ref := range baseline {
		if !relativePath(path) {
			return m, errors.New("baseline path invalid")
		}
		if err := validateReference(ref); err != nil {
			return m, err
		}
	}
	intervals, ok := trial["occupancy_intervals"].([]any)
	if !ok {
		return m, errors.New("occupancy intervals must be an array")
	}
	for _, interval := range intervals {
		if _, err := exact(interval, "started_at ended_at active_workers"); err != nil {
			return m, err
		}
	}
	comparableSource := make(map[string]any, len(source)-1)
	for key, value := range source {
		if key != "identity_evidence" {
			comparableSource[key] = value
		}
	}
	if !reflect.DeepEqual(comparableSource, contextObject["source"]) {
		return m, errors.New("candidate source/context mismatch")
	}
	sha, err := qualification.DigestJSON(candidate)
	if err != nil || sha != m.CandidateManifestSHA256 {
		return m, errors.New("candidate manifest digest mismatch")
	}
	sha, err = qualification.DigestJSON(root["inventory"])
	if err != nil || sha != m.InventorySHA256 {
		return m, errors.New("inventory digest mismatch")
	}
	if err := validateInventoryShape(root["inventory"]); err != nil {
		return m, err
	}
	activation, err := exact(root["activation"], "pack workflow mayor_session rigs retired_scripts")
	if err != nil {
		return m, err
	}
	if !equalSets(activation["rigs"], m.Context.Rigs) {
		return m, errors.New("activation rig scope mismatch")
	}
	scripts, ok := activation["retired_scripts"].([]any)
	if !ok || len(scripts) == 0 {
		return m, errors.New("retirement script scope missing")
	}
	seenScripts := map[string]bool{}
	for _, script := range scripts {
		s, ok := script.(string)
		if !ok || !relativePath(s) || seenScripts[s] {
			return m, errors.New("retired script scope invalid")
		}
		seenScripts[s] = true
	}
	for _, field := range []string{"pack", "workflow", "mayor_session"} {
		if s, ok := activation[field].(string); !ok || !validAtom(s) {
			return m, errors.New("activation identity invalid")
		}
	}
	if _, err := time.Parse(time.RFC3339, m.TrustedNow); err != nil {
		return m, errors.New("trusted_now invalid")
	}
	return m, nil
}

func validateCandidateShape(candidate map[string]any) error {
	phases, err := exact(candidate["phases"], "comparison preflight trial")
	if err != nil {
		return err
	}
	for _, phase := range phases {
		p, err := exact(phase, "status started_at ended_at evidence")
		if err != nil {
			return err
		}
		if err := validateReference(p["evidence"]); err != nil {
			return err
		}
	}
	review, err := exact(candidate["human_review"], "reviewer reviewed_at decision evidence reviewed_delta_ids")
	if err != nil {
		return err
	}
	if err := validateReference(review["evidence"]); err != nil {
		return err
	}
	for _, descriptor := range []struct{ name, fields string }{
		{"decision_observations", "id observed_at work_id work_revision work_class phase case legacy controller comparison rationale"},
		{"safety_scenarios", "name phase observed_at result evidence details"},
		{"completions", "id work_id attempt_id completed_at phase useful_outcome_verified execution_identity independent_verifier evidence execution_count false_completion hold_bypass"},
	} {
		rows, ok := candidate[descriptor.name].([]any)
		if !ok {
			return errors.New("candidate rows must be arrays")
		}
		for _, value := range rows {
			row, err := exact(value, descriptor.fields)
			if err != nil {
				return err
			}
			if descriptor.name == "decision_observations" {
				for _, side := range []string{"legacy", "controller"} {
					if _, err := exact(row[side], "eligibility capacity priority assignment outcome"); err != nil {
						return err
					}
				}
			} else if err := validateReference(row["evidence"]); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateInventoryShape(value any) error {
	inventory, err := exact(value, "writers baseline_edges effective_edges scopes in_flight")
	if err != nil {
		return err
	}
	if !equalSets(inventory["scopes"], []string{"orders", "startup", "services", "cron", "manual"}) {
		return errors.New("mandatory inventory scopes missing")
	}
	for _, descriptor := range []struct{ name, fields string }{
		{"writers", "id baseline effective disposition"},
		{"baseline_edges", "caller target"},
		{"effective_edges", "caller target"},
		{"in_flight", "id writer resolution evidence"},
	} {
		rows, ok := inventory[descriptor.name].([]any)
		if !ok {
			return errors.New("inventory rows must be arrays")
		}
		for _, value := range rows {
			row, err := exact(value, descriptor.fields)
			if err != nil {
				return err
			}
			if descriptor.name == "writers" {
				for _, kind := range []string{"baseline", "effective"} {
					if err := validateReference(row[kind]); err != nil {
						return err
					}
				}
			}
			if descriptor.name == "in_flight" {
				if err := validateReference(row["evidence"]); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func validateReference(value any) error {
	ref, err := exact(value, "path sha256")
	if err != nil {
		return err
	}
	path, pathOK := ref["path"].(string)
	digest, digestOK := ref["sha256"].(string)
	if !pathOK || !digestOK || !relativePath(path) || !validDigest(digest) {
		return errors.New("retained reference invalid")
	}
	return nil
}

func relativePath(s string) bool {
	if !validAtom(s) || strings.HasPrefix(s, "/") || strings.Contains(s, "\\") {
		return false
	}
	for _, part := range strings.Split(s, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}

func (c releaseContext) validate(now time.Time, maximumAge time.Duration) error {
	observed, err := time.Parse(time.RFC3339, c.ObservedAt)
	if err != nil {
		return err
	}
	expires, err := time.Parse(time.RFC3339, c.ExpiresAt)
	if err != nil {
		return err
	}
	if now.IsZero() || maximumAge <= 0 || c.SchemaVersion != 1 || c.ControllerGeneration == 0 || !validAtom(c.Audience) || !validAtom(c.Workspace) || !validAtom(c.ExecutionGeneration) || !validDigest(c.HostSHA256) || !validDigest(c.BootSHA256) || !equalSets(stringArray(c.Rigs), c.Rigs) || len(c.Rigs) == 0 || observed.After(now) || !now.Before(expires) || expires.Sub(observed) > maximumAge || !expires.After(observed) || now.Sub(observed) > maximumAge {
		return errors.New("context invalid, future or stale")
	}
	for _, rev := range []string{c.Source.ControllerSourceCommit, c.Source.PackSourceCommit, c.Source.BackendSourceCommit} {
		if len(rev) != 40 || !validDigest(rev+strings.Repeat("0", 24)) {
			return errors.New("source revision invalid")
		}
	}
	for _, sha := range []string{c.Source.ControllerBinarySHA256, c.Source.BackendBinarySHA256, c.Source.RuntimeIdentitySHA256, c.Source.BackendIdentitySHA256, c.Source.EffectiveConfigSHA256} {
		if !validDigest(sha) {
			return errors.New("source digest invalid")
		}
	}
	return nil
}

func strictValue(d *json.Decoder, depth int) (any, error) {
	if depth > 64 {
		return nil, errors.New("JSON nesting too deep")
	}
	t, err := d.Token()
	if err != nil {
		return nil, err
	}
	if delimiter, ok := t.(json.Delim); ok {
		switch delimiter {
		case '{':
			object := map[string]any{}
			for d.More() {
				keyToken, err := d.Token()
				if err != nil {
					return nil, err
				}
				key, ok := keyToken.(string)
				if !ok {
					return nil, errors.New("invalid object key")
				}
				if _, exists := object[key]; exists {
					return nil, fmt.Errorf("duplicate key %q", key)
				}
				value, err := strictValue(d, depth+1)
				if err != nil {
					return nil, err
				}
				object[key] = value
			}
			end, err := d.Token()
			if err != nil || end != json.Delim('}') {
				return nil, errors.New("unterminated object")
			}
			return object, nil
		case '[':
			array := []any{}
			for d.More() {
				value, err := strictValue(d, depth+1)
				if err != nil {
					return nil, err
				}
				array = append(array, value)
			}
			end, err := d.Token()
			if err != nil || end != json.Delim(']') {
				return nil, errors.New("unterminated array")
			}
			return array, nil
		default:
			return nil, errors.New("unexpected delimiter")
		}
	}
	return t, nil
}

func exact(v any, fields string) (map[string]any, error) {
	o, ok := v.(map[string]any)
	if !ok {
		return nil, errors.New("expected object")
	}
	keys := strings.Fields(fields)
	if len(o) != len(keys) {
		return nil, errors.New("missing or extra fields")
	}
	for _, key := range keys {
		if _, ok := o[key]; !ok {
			return nil, fmt.Errorf("missing field %s", key)
		}
	}
	return o, nil
}

func equalSets(value any, expected []string) bool {
	array, ok := value.([]any)
	if !ok || len(array) != len(expected) {
		return false
	}
	a := make([]string, len(array))
	b := append([]string(nil), expected...)
	for i, v := range array {
		s, ok := v.(string)
		if !ok || !validAtom(s) {
			return false
		}
		a[i] = s
	}
	sort.Strings(a)
	sort.Strings(b)
	for i := range a {
		if a[i] != b[i] || (i > 0 && a[i] == a[i-1]) {
			return false
		}
	}
	return true
}

func stringArray(values []string) []any {
	result := make([]any, len(values))
	for i, v := range values {
		result[i] = v
	}
	return result
}

func validAtom(s string) bool {
	return s != "" && len(s) <= 512 && utf8.ValidString(s) && strings.TrimSpace(s) == s && !strings.ContainsAny(s, "\x00\r\n")
}

func validDigest(s string) bool {
	if len(s) != 64 || strings.ToLower(s) != s {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}
func digestBytes(raw []byte) string { sum := sha256.Sum256(raw); return hex.EncodeToString(sum[:]) }
