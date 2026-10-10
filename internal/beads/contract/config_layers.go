package contract

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/fsys"
	"gopkg.in/yaml.v3"
)

// Explicit local layers must never enter the legacy malformed-YAML repair
// path: silently dropping one could select a different endpoint.
var errLocalConfig = errors.New("local config layer")

func localConfigPath(fs fsys.FS, path string) (string, bool, error) {
	if filepath.Base(path) != "config.yaml" {
		return "", false, nil
	}
	local := filepath.Join(filepath.Dir(path), "config.local.yaml")
	if _, err := fs.Lstat(local); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("%w %s: %w", errLocalConfig, local, err)
	}
	return local, true, nil
}

func readConfigDoc(fs fsys.FS, path string) (*yaml.Node, error) {
	local, present, err := localConfigPath(fs, path)
	if err != nil {
		return nil, err
	}
	if !present {
		return readSingleConfigDoc(fs, path)
	}
	base, err := readSingleConfigDoc(fs, path)
	if err != nil {
		return nil, fmt.Errorf("%w %s: %w", errLocalConfig, local, err)
	}
	override, err := readSingleConfigDoc(fs, local)
	if err != nil {
		return nil, fmt.Errorf("%w %s: %w", errLocalConfig, local, err)
	}
	root, overlay := mappingRoot(base), mappingRoot(override)
	types := MergeCustomTypes(parseCustomTypesValue(configValue(root, "types.custom")), parseCustomTypesValue(configValue(overlay, "types.custom")))
	// These spellings name the same setting. An explicit local spelling must
	// take precedence over either portable spelling.
	if findValue(overlay, "issue_prefix") != nil || findValue(overlay, "issue-prefix") != nil {
		deleteKeys(root, "issue_prefix", "issue-prefix")
	}
	flatFlush := findValue(overlay, "dolt.disable-event-flush") != nil || findValue(overlay, "dolt.disable_event_flush") != nil
	nestedFlush := findValue(findValue(overlay, "dolt"), "disable-event-flush") != nil || findValue(findValue(overlay, "dolt"), "disable_event_flush") != nil
	if flatFlush || nestedFlush {
		deleteKeys(root, "dolt.disable-event-flush", "dolt.disable_event_flush")
		deleteKeys(findValue(root, "dolt"), "disable-event-flush", "disable_event_flush")
	}
	clearConfigOverlayAliases(root, overlay, "")
	mergeConfigMapping(root, overlay)
	if len(types) > 0 {
		setConfigString(root, "types.custom", strings.Join(types, ","))
	}
	return base, nil
}

func mergeConfigMapping(base, local *yaml.Node) {
	for i := 0; i+1 < len(local.Content); i += 2 {
		key, value := local.Content[i], local.Content[i+1]
		inherited := findValue(base, key.Value)
		if inherited != nil && inherited.Kind == yaml.MappingNode && value.Kind == yaml.MappingNode {
			mergeConfigMapping(inherited, value)
			continue
		}
		deleteKeys(base, key.Value)
		base.Content = append(base.Content, key, value)
	}
}

// Select the same local layer used by the readers. Unknown portable keys are
// not copied into it, so future team defaults remain inherited.
func canonicalConfigWriteLayer(fs fsys.FS, path string, state ConfigState) (string, *yaml.Node, ConfigState, error) {
	local, present, err := localConfigPath(fs, path)
	if err != nil || !present {
		return path, nil, state, err
	}
	effective, err := readConfigDoc(fs, path)
	if err != nil {
		return "", nil, state, err
	}
	portable, err := readSingleConfigDoc(fs, path)
	if err != nil {
		return "", nil, state, fmt.Errorf("%w %s: %w", errLocalConfig, local, err)
	}
	root := mappingRoot(effective)
	if strings.TrimSpace(state.IssuePrefix) == "" {
		state.IssuePrefix = configValue(root, "issue_prefix", "issue-prefix")
	}
	if state.Dolt.DisableEventFlush == nil {
		state.Dolt = readDoltConfigFromRoot(root)
	}
	if len(state.CustomTypes) > 0 {
		state.CustomTypes = MergeCustomTypes(parseCustomTypesValue(configValue(root, "types.custom")), state.CustomTypes)
	}
	return local, mappingRoot(portable), state, nil
}

func clearCanonicalConfigKey(root, portable *yaml.Node, key string) bool {
	if findConfigValue(portable, key) != nil {
		// Deleting only the local value would resurrect the portable endpoint.
		// A YAML null masks it while leaving the portable file untouched.
		changed := setScalar(root, key, "null", "!!null")
		return deleteNestedConfigKeys(root, key) || changed
	}
	return deleteConfigKeys(root, key)
}

// A local value wins across the flat and nested spellings bd accepts.
func clearConfigOverlayAliases(base, local *yaml.Node, prefix string) {
	for i := 0; i+1 < len(local.Content); i += 2 {
		key, value := local.Content[i].Value, local.Content[i+1]
		path := key
		if prefix != "" {
			path = prefix + "." + key
			deleteKeys(base, path)
		} else {
			deleteNestedConfigKeys(base, key)
		}
		if value.Kind == yaml.MappingNode {
			clearConfigOverlayAliases(base, value, path)
			continue
		}
		// A null or scalar section must also mask inherited dotted children.
		var children []string
		for j := 0; j+1 < len(base.Content); j += 2 {
			if strings.HasPrefix(base.Content[j].Value, path+".") {
				children = append(children, base.Content[j].Value)
			}
		}
		deleteKeys(base, children...)
	}
}
