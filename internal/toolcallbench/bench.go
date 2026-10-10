// Package toolcallbench evaluates frozen, individual pre-call decisions offline.
// It executes no calls and grants no runtime or promotion authority.
package toolcallbench

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"reflect"
	"strings"

	"github.com/gastownhall/gascity/internal/storybench"
)

type call struct {
	Kind       string          `json:"kind"`
	Target     string          `json:"target"`
	WorkingDir string          `json:"working_dir"`
	Arguments  json.RawMessage `json:"arguments"`
}

type testCase struct {
	ID        string `json:"id"`
	StoryID   string `json:"story_id"`
	Category  string `json:"category"`
	Context   string `json:"context"`
	Permitted []call `json:"permitted"`
	Accepted  []call `json:"accepted"`
}

type suite struct {
	Version int        `json:"version"`
	ID      string     `json:"id"`
	Cases   []testCase `json:"cases"`
}

type run struct {
	SuiteSHA256 string     `json:"suite_sha256"`
	BundleID    string     `json:"bundle_id"`
	CapturedBy  string     `json:"captured_by"`
	Proposals   []proposal `json:"proposals"`
}

type proposal struct {
	CaseID string     `json:"case_id"`
	Action string     `json:"action"`
	Call   *call      `json:"call,omitempty"`
	Reason string     `json:"reason,omitempty"`
	Cost   *costInput `json:"cost"`
}

type costInput struct {
	Calls      *int64 `json:"calls"`
	Tokens     *int64 `json:"tokens"`
	DurationMS *int64 `json:"duration_ms"`
}

// Cost records independently measured preflight overhead, not task execution.
type Cost struct {
	Calls      int64 `json:"calls"`
	Tokens     int64 `json:"tokens"`
	DurationMS int64 `json:"duration_ms"`
}

// Totals keeps correctness, omission, authorization and overhead separate.
type Totals struct {
	Correct         int  `json:"correct"`
	InvalidCalls    int  `json:"invalid_calls"`
	UnsafeProposals int  `json:"unsafe_proposals"`
	Omissions       int  `json:"omissions"`
	FalseRejections int  `json:"false_rejections"`
	Cost            Cost `json:"cost"`
}

// CaseResult links one paired decision to its user story and failure category.
type CaseResult struct {
	CaseID    string `json:"case_id"`
	StoryID   string `json:"story_id"`
	Category  string `json:"category"`
	Baseline  string `json:"baseline"`
	Candidate string `json:"candidate"`
}

// Report is diagnostic evidence, not a promotion verdict or weighted score.
type Report struct {
	SuiteID             string       `json:"suite_id"`
	SuiteSHA256         string       `json:"suite_sha256"`
	BaselineBundleID    string       `json:"baseline_bundle_id"`
	CandidateBundleID   string       `json:"candidate_bundle_id"`
	BaselineCapturedBy  string       `json:"baseline_captured_by"`
	CandidateCapturedBy string       `json:"candidate_captured_by"`
	Baseline            Totals       `json:"baseline"`
	Candidate           Totals       `json:"candidate"`
	Improvements        int          `json:"improvements"`
	Regressions         int          `json:"regressions"`
	Cases               []CaseResult `json:"cases"`
}

// Evaluate compares complete proposals against the same frozen suite bytes.
// Capture identities are declarations: the caller must authenticate provenance.
func Evaluate(rawSuite, rawBaseline, rawCandidate []byte, improver string) (Report, error) {
	var s suite
	var base, candidate run
	for _, input := range []struct {
		raw  []byte
		dest any
	}{
		{rawSuite, &s}, {rawBaseline, &base}, {rawCandidate, &candidate},
	} {
		if err := decodeStrict(input.raw, input.dest); err != nil {
			return Report{}, err
		}
	}
	if strings.TrimSpace(improver) == "" {
		return Report{}, fmt.Errorf("improver identity missing")
	}
	if err := validateSuite(s); err != nil {
		return Report{}, err
	}
	hash := storybench.SuiteHash(rawSuite)
	baseByID, err := validateRun(s, base, hash, improver)
	if err != nil {
		return Report{}, fmt.Errorf("baseline: %w", err)
	}
	candidateByID, err := validateRun(s, candidate, hash, improver)
	if err != nil {
		return Report{}, fmt.Errorf("candidate: %w", err)
	}
	if base.BundleID == candidate.BundleID {
		return Report{}, fmt.Errorf("baseline and candidate bundle IDs identical")
	}
	report := Report{
		SuiteID: s.ID, SuiteSHA256: hash, BaselineBundleID: base.BundleID,
		CandidateBundleID: candidate.BundleID, BaselineCapturedBy: base.CapturedBy, CandidateCapturedBy: candidate.CapturedBy,
		Cases: make([]CaseResult, 0, len(s.Cases)),
	}
	for _, c := range s.Cases {
		b := score(c, baseByID[c.ID], &report.Baseline)
		n := score(c, candidateByID[c.ID], &report.Candidate)
		report.Cases = append(report.Cases, CaseResult{c.ID, c.StoryID, c.Category, b, n})
		if b != "correct" && n == "correct" {
			report.Improvements++
		}
		if b == "correct" && n != "correct" {
			report.Regressions++
		}
	}
	return report, nil
}

func score(c testCase, p proposal, t *Totals) string {
	if p.Cost != nil {
		t.Cost.Calls += *p.Cost.Calls
		t.Cost.Tokens += *p.Cost.Tokens
		t.Cost.DurationMS += *p.Cost.DurationMS
	}
	switch {
	case p.Action == "defer":
		if len(c.Accepted) > 0 {
			t.Omissions++
			t.FalseRejections++
			return "necessary_call_omitted"
		}
	case !containsCall(c.Permitted, *p.Call):
		t.UnsafeProposals++
		return "outside_frozen_authorization"
	case !containsCall(c.Accepted, *p.Call):
		t.InvalidCalls++
		return "invalid_call"
	}
	t.Correct++
	return "correct"
}

func containsCall(calls []call, wanted call) bool {
	for _, c := range calls {
		if c.Kind == wanted.Kind && c.Target == wanted.Target && c.WorkingDir == wanted.WorkingDir && canonicalArgs(c.Arguments) == canonicalArgs(wanted.Arguments) {
			return true
		}
	}
	return false
}

func canonicalArgs(raw []byte) string {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return ""
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return string(encoded)
}

func validateSuite(s suite) error {
	if s.Version != 1 || strings.TrimSpace(s.ID) == "" || len(s.Cases) == 0 || len(s.Cases) > 4096 {
		return fmt.Errorf("suite requires version 1, ID and 1..4096 cases")
	}
	seen := map[string]bool{}
	for _, c := range s.Cases {
		if strings.TrimSpace(c.ID) == "" || seen[c.ID] || strings.TrimSpace(c.StoryID) == "" || strings.TrimSpace(c.Context) == "" || c.Permitted == nil || c.Accepted == nil || len(c.Permitted) > 64 || len(c.Accepted) > 64 {
			return fmt.Errorf("case %q requires unique ID, story, pre-call context, permitted and accepted arrays", c.ID)
		}
		seen[c.ID] = true
		switch c.Category {
		case "wrong_tool", "invalid_arguments", "unsupported_capability", "wrong_directory", "missing_prerequisite", "valid_control", "denied_operation", "stale_skill":
		default:
			return fmt.Errorf("case %q has unknown category", c.ID)
		}
		for _, calls := range [][]call{c.Permitted, c.Accepted} {
			for i, item := range calls {
				if err := validateCall(item); err != nil {
					return fmt.Errorf("case %q: %w", c.ID, err)
				}
				if containsCall(calls[:i], item) {
					return fmt.Errorf("case %q has duplicate calls", c.ID)
				}
			}
		}
		for _, accepted := range c.Accepted {
			if !containsCall(c.Permitted, accepted) {
				return fmt.Errorf("case %q accepts an unauthorized call", c.ID)
			}
		}
	}
	return nil
}

func validateRun(s suite, r run, hash, improver string) (map[string]proposal, error) {
	if r.SuiteSHA256 != hash {
		return nil, fmt.Errorf("suite hash mismatch")
	}
	if strings.TrimSpace(r.BundleID) == "" || r.BundleID != strings.TrimSpace(r.BundleID) {
		return nil, fmt.Errorf("bundle ID missing or padded")
	}
	if strings.TrimSpace(r.CapturedBy) == "" || strings.TrimSpace(r.CapturedBy) == strings.TrimSpace(improver) {
		return nil, fmt.Errorf("capture identity missing or same as improver")
	}
	if len(r.Proposals) != len(s.Cases) {
		return nil, fmt.Errorf("incomplete proposals")
	}
	allowed := map[string]bool{}
	for _, c := range s.Cases {
		allowed[c.ID] = true
	}
	out := make(map[string]proposal, len(r.Proposals))
	for _, p := range r.Proposals {
		if !allowed[p.CaseID] {
			return nil, fmt.Errorf("unknown case %q", p.CaseID)
		}
		if _, ok := out[p.CaseID]; ok {
			return nil, fmt.Errorf("duplicate case %q", p.CaseID)
		}
		if err := validateProposal(p); err != nil {
			return nil, fmt.Errorf("case %q: %w", p.CaseID, err)
		}
		out[p.CaseID] = p
	}
	return out, nil
}

func validateProposal(p proposal) error {
	if p.Cost == nil {
		return fmt.Errorf("preflight cost missing")
	}
	for _, v := range []*int64{p.Cost.Calls, p.Cost.Tokens, p.Cost.DurationMS} {
		if v == nil || *v < 0 || *v > 1_000_000_000_000 {
			return fmt.Errorf("cost must contain bounded nonnegative calls, tokens and duration_ms")
		}
	}
	switch p.Action {
	case "call":
		if p.Call == nil {
			return fmt.Errorf("proposed call missing")
		}
		return validateCall(*p.Call)
	case "defer":
		if p.Call != nil || strings.TrimSpace(p.Reason) == "" {
			return fmt.Errorf("deferral requires reason and no call")
		}
	default:
		return fmt.Errorf("unknown action %q", p.Action)
	}
	return nil
}

func validateCall(c call) error {
	switch c.Kind {
	case "cli", "mcp", "skill":
	default:
		return fmt.Errorf("unknown call kind %q", c.Kind)
	}
	if strings.TrimSpace(c.Target) == "" || strings.TrimSpace(c.WorkingDir) == "" || len(c.Arguments) == 0 || bytes.Equal(bytes.TrimSpace(c.Arguments), []byte("null")) {
		return fmt.Errorf("call requires target, explicit working_dir and non-null JSON arguments")
	}
	return nil
}

// Strict decoding also rejects duplicate nested keys rather than silently
// accepting the last value. Depth/byte bounds keep offline evidence bounded.
func decodeStrict(raw []byte, dest any) error {
	if len(raw) > 8<<20 {
		return fmt.Errorf("JSON exceeds 8 MiB")
	}
	keys := json.NewDecoder(bytes.NewReader(raw))
	keys.UseNumber()
	if err := checkValue(keys, 0); err != nil {
		return err
	}
	if _, err := keys.Token(); err != io.EOF {
		return fmt.Errorf("trailing JSON")
	}
	if err := checkFields(raw, reflect.TypeOf(dest).Elem()); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	return decoder.Decode(dest)
}

func checkValue(d *json.Decoder, depth int) error {
	if depth > 64 {
		return fmt.Errorf("JSON nesting exceeds 64")
	}
	token, err := d.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for d.More() {
			key, err := d.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			if !ok || seen[name] {
				return fmt.Errorf("duplicate or invalid JSON key %q", name)
			}
			seen[name] = true
			if err := checkValue(d, depth+1); err != nil {
				return err
			}
		}
	case '[':
		for d.More() {
			if err := checkValue(d, depth+1); err != nil {
				return err
			}
		}
	default:
		return fmt.Errorf("unexpected JSON delimiter")
	}
	_, err = d.Token()
	return err
}

// Contexts exports only case identity and pre-call context for a candidate.
// Keep the full suite and oracle in a separate evaluator-owned workspace.
func Contexts(rawSuite []byte) ([]byte, error) {
	var s suite
	if err := decodeStrict(rawSuite, &s); err != nil {
		return nil, err
	}
	if err := validateSuite(s); err != nil {
		return nil, err
	}
	type contextCase struct {
		ID      string `json:"id"`
		StoryID string `json:"story_id"`
		Context string `json:"context"`
	}
	projection := struct {
		Version     int           `json:"version"`
		ID          string        `json:"id"`
		SuiteSHA256 string        `json:"suite_sha256"`
		Cases       []contextCase `json:"cases"`
	}{Version: s.Version, ID: s.ID, SuiteSHA256: storybench.SuiteHash(rawSuite), Cases: make([]contextCase, 0, len(s.Cases))}
	for _, c := range s.Cases {
		projection.Cases = append(projection.Cases, contextCase{c.ID, c.StoryID, c.Context})
	}
	return json.MarshalIndent(projection, "", "  ")
}

// encoding/json accepts case-insensitive struct keys; require the exact wire
// spelling so aliases cannot smuggle a second typed value past duplicate checks.
// Arguments remain opaque JSON and retain their tool's own key spelling.
func checkFields(raw []byte, typ reflect.Type) error {
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	if typ == reflect.TypeFor[json.RawMessage]() {
		return nil
	}
	switch typ.Kind() {
	case reflect.Struct:
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(raw, &fields); err != nil {
			return err
		}
		allowed := make(map[string]reflect.Type, typ.NumField())
		for i := 0; i < typ.NumField(); i++ {
			field := typ.Field(i)
			name := strings.Split(field.Tag.Get("json"), ",")[0]
			allowed[name] = field.Type
		}
		for name, value := range fields {
			fieldType, ok := allowed[name]
			if !ok {
				return fmt.Errorf("unknown JSON field %q", name)
			}
			if err := checkFields(value, fieldType); err != nil {
				return err
			}
		}
	case reflect.Slice:
		var values []json.RawMessage
		if err := json.Unmarshal(raw, &values); err != nil {
			return err
		}
		for _, value := range values {
			if err := checkFields(value, typ.Elem()); err != nil {
				return err
			}
		}
	}
	return nil
}
