package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
)

// bdPresentationCommand is deliberately closed: an unstructured provider
// renderer cannot be trusted with private ledger rows. Administrative commands
// that do not render ledger contents retain their native transport.
func bdPresentationCommand(args []string) (project bool, err error) {
	if len(args) == 0 {
		return false, fmt.Errorf("missing bd command")
	}
	if len(args) == 1 {
		switch args[0] {
		case "--help", "-h", "--version", "-V":
			return false, nil
		}
	}
	for _, arg := range args {
		name, value, _ := strings.Cut(arg, "=")
		switch name {
		case "--fields", "--columns", "--template", "--output", "--output-file", "-o", "--brief", "--short":
			return false, fmt.Errorf("bd output selection has no private-safe presentation")
		case "--format":
			if value != "json" {
				return false, fmt.Errorf("bd output format has no private-safe presentation")
			}
		}
	}
	verb, verbArgs, ok := bdRelocatedClassVerb(args)
	if !ok {
		return false, fmt.Errorf("bd command has no private-safe presentation")
	}
	switch verb {
	case "show", "list", "ready", "search", "blocked", "children", "create", "update", "close", "reopen", "label", "set-state":
		return true, nil
	case "dep":
		if len(verbArgs) > 0 {
			switch verbArgs[0] {
			case "list", "tree":
				return true, nil
			}
		}
	case "version", "help", "init", "config", "context", "migrate", "heartbeat", "stats", "count":
		return false, nil
	}
	return false, fmt.Errorf("bd command has no private-safe presentation; use gc bead, gc ready, or a scoped private reader")
}

// projectBdProviderJSON parses externally owned bd output, preserving its public
// shape while replacing private records and removing reserved owner fields.
func projectBdProviderJSON(data []byte) ([]byte, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return data, nil
	}
	var value any
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&value); err != nil {
		return nil, fmt.Errorf("invalid provider JSON")
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("unexpected provider output after JSON")
	}
	var walk func(any) any
	changed := false
	walk = func(v any) any {
		switch x := v.(type) {
		case []any:
			for i := range x {
				x[i] = walk(x[i])
			}
		case map[string]any:
			if metadata, ok := x["metadata"].(map[string]any); ok {
				b := beads.Bead{Metadata: make(map[string]string, len(metadata))}
				for key, value := range metadata {
					b.Metadata[key], _ = value.(string)
				}
				if beads.IsPrivatePresentationRecord(b) {
					changed = true
					b.ID, _ = x["id"].(string)
					return beads.PublicBead(b)
				}
				for key := range metadata {
					if beads.IsPrivatePresentationMetadataKey(key) {
						changed = true
						delete(metadata, key)
					}
				}
			}
			for key, value := range x {
				x[key] = walk(value)
			}
		}
		return v
	}
	value = walk(value)
	if !changed {
		return data, nil
	}
	return json.MarshalIndent(value, "", "  ")
}

func bdPresentationJSONRequested(args []string) bool {
	for _, arg := range args {
		if arg == "--json" || arg == "--format=json" {
			return true
		}
	}
	return false
}

func writeBdProviderPresentation(data []byte, jsonOut bool, stdout, stderr io.Writer) int {
	projected, err := projectBdProviderJSON(data)
	if err != nil {
		fmt.Fprintf(stderr, "gc bd: private-safe presentation: %v\n", err) //nolint:errcheck
		return 1
	}
	if len(bytes.TrimSpace(projected)) == 0 {
		return 0
	}
	if !jsonOut {
		var rows []beads.Bead
		if json.Unmarshal(projected, &rows) == nil {
			for _, b := range rows {
				if printBdByIDBead(b, false, "provider projection", stdout, stderr) != 0 {
					return 1
				}
			}
			return 0
		}
		var row beads.Bead
		if json.Unmarshal(projected, &row) == nil && strings.TrimSpace(row.ID) != "" {
			return printBdByIDBead(row, false, "provider projection", stdout, stderr)
		}
	}
	if _, err := stdout.Write(projected); err != nil {
		fmt.Fprintf(stderr, "gc bd: write presentation: %v\n", err) //nolint:errcheck
		return 1
	}
	if projected[len(projected)-1] != '\n' {
		fmt.Fprintln(stdout) //nolint:errcheck
	}
	return 0
}
