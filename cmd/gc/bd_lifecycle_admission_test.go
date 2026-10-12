package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// Stock bd has no --if-revision CLI flag. An explicitly admission-disabled
// city must still support ordinary routing, while enrolled work and explicit
// CAS requests retain their protection.
func TestBdStoreAdmissionDisabledStockCLI(t *testing.T) {
	status := "in_progress"
	route := map[string]string{beadmeta.WorkflowIDMetadataKey: "wf-1"}
	writes := []struct {
		name  string
		write func(*beads.BdStore) error
	}{
		{"status", func(s *beads.BdStore) error { return s.Update("EX-1", beads.UpdateOpts{Status: &status}) }},
		{"routing", func(s *beads.BdStore) error { return s.Update("EX-1", beads.UpdateOpts{Metadata: route}) }},
		{"metadata", func(s *beads.BdStore) error { return s.SetMetadata("EX-1", beadmeta.WorkflowIDMetadataKey, "wf-1") }},
		{"metadata-batch", func(s *beads.BdStore) error { return s.SetMetadataBatch("EX-1", route) }},
		{"update-all", func(s *beads.BdStore) error {
			n, err := s.UpdateAll([]string{"EX-1"}, beads.UpdateOpts{Status: &status})
			if err == nil && n != 1 {
				return fmt.Errorf("updated %d beads, want 1", n)
			}
			return err
		}},
	}
	for _, tc := range writes {
		t.Run(tc.name, func(t *testing.T) {
			for _, mode := range []string{"disabled", "enabled", "unknown", "enrolled"} {
				t.Run(mode, func(t *testing.T) {
					cfg := &config.City{}
					metadata := map[string]string{}
					if mode == "enabled" {
						cfg.Lifecycle.AdmissionEnabled = true
					}
					if mode == "unknown" {
						cfg = nil
					}
					if mode == "enrolled" {
						metadata[beadmeta.LifecycleAdmissionReceiptMetadataKey] = "durable-receipt"
					}
					mutations := 0
					store := admissionStockCLIStore(t, cfg, metadata, &mutations)
					err := tc.write(store)
					if mode == "disabled" {
						if err != nil || mutations != 1 {
							t.Fatalf("ordinary write = %v, mutations=%d; want success and one stock CLI write", err, mutations)
						}
					} else if err == nil || mutations != 0 {
						t.Fatalf("protected write = %v, mutations=%d; want refusal without mutation", err, mutations)
					}
				})
			}
		})
	}
	t.Run("explicit-CAS", func(t *testing.T) {
		mutations := 0
		store := admissionStockCLIStore(t, &config.City{}, nil, &mutations)
		err := store.UpdateIfMatch("EX-1", 12, beads.UpdateOpts{Status: &status})
		if !errors.Is(err, beads.ErrConditionalWriteUnsupported) || mutations != 0 {
			t.Fatalf("explicit CAS = %v, mutations=%d; want unsupported without mutation", err, mutations)
		}
	})
	t.Run("enrollment-write", func(t *testing.T) {
		mutations := 0
		store := admissionStockCLIStore(t, &config.City{}, nil, &mutations)
		err := store.SetMetadata("EX-1", beadmeta.LifecycleAdmissionReceiptMetadataKey, "receipt")
		if err == nil || mutations != 0 {
			t.Fatalf("enrollment write = %v, mutations=%d; want refusal without mutation", err, mutations)
		}
	})
}

func admissionStockCLIStore(t *testing.T, cfg *config.City, metadata map[string]string, mutations *int) *beads.BdStore {
	t.Helper()
	row, err := json.Marshal([]map[string]any{{"id": "EX-1", "status": "open", "issue_type": "task", "revision": 12, "metadata": metadata}})
	if err != nil {
		t.Fatal(err)
	}
	return beads.NewBdStore(t.TempDir(), func(_, _ string, args ...string) ([]byte, error) {
		command := strings.Join(args, " ")
		switch {
		case command == "show --json EX-1":
			return row, nil
		case strings.HasSuffix(command, " --help"):
			return []byte("stock bd update options"), nil
		case strings.HasPrefix(command, "update --json "):
			if strings.Contains(command, "--if-revision") {
				return nil, errors.New("unknown flag: --if-revision")
			}
			*mutations++
			return row, nil
		default:
			return nil, fmt.Errorf("unexpected bd command: %s", command)
		}
	}, bdStoreOptionsForConfig(cfg)...)
}
