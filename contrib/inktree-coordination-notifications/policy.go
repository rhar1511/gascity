// Package inktreecoordinationnotifications provides Inktree's notification
// policy without adding Inktree vocabulary to Gas City's generic primitives.
package inktreecoordinationnotifications

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"sort"

	"github.com/gastownhall/gascity/internal/coordinationnotify"
)

//go:embed policy.json replay/corpus.v1.json
var policyFS embed.FS

// Policy loads the versioned Inktree mapping and fixed copy table.
func Policy() (coordinationnotify.Policy, error) {
	data, err := policyFS.ReadFile("policy.json")
	if err != nil {
		return coordinationnotify.Policy{}, fmt.Errorf("read embedded Inktree policy: %w", err)
	}
	return ParsePolicy(data)
}

// PolicyDocument returns the exact retained policy bytes used by Policy.
func PolicyDocument() ([]byte, error) {
	data, err := policyFS.ReadFile("policy.json")
	if err != nil {
		return nil, fmt.Errorf("read embedded Inktree policy: %w", err)
	}
	return append([]byte(nil), data...), nil
}

// ReplayCorpusDocument returns the exact retained synthetic replay corpus.
func ReplayCorpusDocument() ([]byte, error) {
	data, err := policyFS.ReadFile("replay/corpus.v1.json")
	if err != nil {
		return nil, fmt.Errorf("read embedded Inktree replay corpus: %w", err)
	}
	return append([]byte(nil), data...), nil
}

// ParsePolicy strictly parses a retained policy document for offline replay.
func ParsePolicy(data []byte) (coordinationnotify.Policy, error) {
	var policy coordinationnotify.Policy
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&policy); err != nil {
		return coordinationnotify.Policy{}, fmt.Errorf("decode embedded Inktree policy: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return coordinationnotify.Policy{}, fmt.Errorf("decode embedded Inktree policy: trailing content")
	}
	return policy, nil
}

// ReasonCoverage returns the sorted consumer status/source keys represented
// by the policy table. It is used by offline evidence reports and tests.
func ReasonCoverage() ([]string, error) {
	policy, err := Policy()
	if err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(policy.Reasons))
	for key := range policy.Reasons {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys, nil
}
