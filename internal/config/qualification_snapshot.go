package config

import "github.com/gastownhall/gascity/internal/qualification"

// RefreshQualificationSnapshot binds the effective config object to the input
// closure captured by the same loader invocation. Call it only after runtime
// identity fields have been applied; it never rereads the filesystem.
func RefreshQualificationSnapshot(cfg *City, prov *Provenance) qualification.Snapshot {
	snapshot := qualification.Snapshot{
		SchemaVersion: qualification.SchemaVersion,
		Status:        qualification.StatusUnavailable,
		Reason:        "loader_input_capture_unavailable",
	}
	if cfg == nil {
		snapshot.Reason = "effective_config_missing"
		return snapshot
	}
	closure := cfg.qualificationInputs
	if prov != nil {
		closure = prov.qualificationInputs
	}
	effective := struct {
		Config                  *City  `json:"config"`
		ResolvedWorkspaceName   string `json:"resolved_workspace_name,omitempty"`
		ResolvedWorkspacePrefix string `json:"resolved_workspace_prefix,omitempty"`
	}{
		Config:                  cfg,
		ResolvedWorkspaceName:   cfg.ResolvedWorkspaceName,
		ResolvedWorkspacePrefix: cfg.ResolvedWorkspacePrefix,
	}
	computed, err := qualification.NewSnapshot(effective, closure)
	if err != nil {
		snapshot.Reason = "effective_config_digest_failed"
	} else {
		snapshot = computed
	}
	cfg.qualificationSnapshot = &snapshot
	return snapshot
}

// QualificationSnapshot returns the immutable qualification snapshot paired
// with this loaded City, or an explicit unavailable result when the caller
// did not use the qualification-aware loader path.
func (c *City) QualificationSnapshot() qualification.Snapshot {
	if c == nil || c.qualificationSnapshot == nil {
		return qualification.Snapshot{
			SchemaVersion: qualification.SchemaVersion,
			Status:        qualification.StatusUnavailable,
			Reason:        "loader_input_capture_unavailable",
		}
	}
	snapshot := *c.qualificationSnapshot
	snapshot.InputRoots = append([]qualification.InputRoot(nil), snapshot.InputRoots...)
	return snapshot
}

// QualificationInputs returns the loader-observed input summary held by
// Provenance. Values and host paths are represented only by hashes.
func (p *Provenance) QualificationInputs() qualification.InputClosure {
	if p == nil {
		return qualification.InputClosure{
			SchemaVersion: qualification.SchemaVersion,
			Status:        qualification.StatusUnavailable,
			Reason:        "loader_input_capture_unavailable",
		}
	}
	closure := p.qualificationInputs
	closure.Roots = append([]qualification.InputRoot(nil), closure.Roots...)
	return closure
}
