// Package mcpskills publishes immutable, explicitly selected skill catalogs.
// It neither executes skill scripts nor activates instructions for a host.
package mcpskills

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	maxSkillBytes    = 16 << 20
	maxSnapshotBytes = 64 << 20
	maxFiles         = 8192
	maxSkills        = 1024
)

var validName = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// Skill is the extension's full entry. Frontmatter is deliberately open-ended:
// the protocol requires every author-defined YAML field to pass through.
type Skill struct {
	URI         string         `json:"uri"`
	Frontmatter map[string]any `json:"frontmatter"`
	Resources   []ManifestFile `json:"resources"`
}

// ManifestFile binds a URI to exact raw bytes.
type ManifestFile struct {
	URI    string `json:"uri"`
	Digest string `json:"digest"`
	Size   int    `json:"size"`
}

// Catalog is immutable after Load. No request reads from the source filesystem.
type Catalog struct {
	skills    []Skill
	files     map[string][]byte
	resources []Resource
	identity  string
}

// Resource is the standard MCP resource descriptor.
type Resource struct {
	URI         string `json:"uri"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	MIMEType    string `json:"mimeType"`
	Size        int    `json:"size"`
}

// Load snapshots namespace-to-directory catalogs. Symlinks and special files
// are refused; os.Root confines reads even if a source changes during loading.
func Load(roots map[string]string) (*Catalog, error) {
	if len(roots) == 0 {
		return nil, fmt.Errorf("at least one catalog is required")
	}
	c := &Catalog{files: make(map[string][]byte)}
	total := 0
	namespaces := make([]string, 0, len(roots))
	for ns := range roots {
		namespaces = append(namespaces, ns)
	}
	sort.Strings(namespaces)
	for _, ns := range namespaces {
		if !validName.MatchString(ns) || len(ns) > 64 {
			return nil, fmt.Errorf("invalid catalog namespace %q", ns)
		}
		if err := c.loadRoot(ns, roots[ns], &total); err != nil {
			return nil, fmt.Errorf("catalog %q: %w", ns, err)
		}
	}
	sort.Slice(c.skills, func(i, j int) bool { return c.skills[i].URI < c.skills[j].URI })
	for uri, data := range c.files {
		r := Resource{URI: uri, Name: path.Base(uri), MIMEType: "application/octet-stream", Size: len(data)}
		if strings.HasSuffix(uri, ".md") {
			r.MIMEType = "text/markdown"
		}
		c.resources = append(c.resources, r)
	}
	for _, s := range c.skills {
		for i := range c.resources {
			if c.resources[i].URI == s.URI {
				c.resources[i].Name = s.Frontmatter["name"].(string)
				c.resources[i].Description = s.Frontmatter["description"].(string)
			}
		}
	}
	sort.Slice(c.resources, func(i, j int) bool { return c.resources[i].URI < c.resources[j].URI })
	manifest, err := json.Marshal(c.skills)
	if err != nil {
		return nil, fmt.Errorf("frontmatter is not JSON-compatible: %w", err)
	}
	c.identity = fmt.Sprintf("%x", sha256.Sum256(manifest))
	return c, nil
}

func (c *Catalog) loadRoot(ns, dir string, total *int) error {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close() //nolint:errcheck // Read-only root handle.
	paths := []string{}
	skillPaths := []string{}
	err = fs.WalkDir(root.FS(), ".", func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return fmt.Errorf("symlink is not publishable: %s", p)
		}
		if d.IsDir() {
			return nil
		}
		info, e := d.Info()
		if e != nil {
			return e
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("non-regular file: %s", p)
		}
		paths = append(paths, p)
		if len(paths) > maxFiles {
			return fmt.Errorf("catalog exceeds %d files", maxFiles)
		}
		if path.Base(p) == "SKILL.md" {
			skillPaths = append(skillPaths, p)
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, skillPath := range skillPaths {
		if len(c.skills) >= maxSkills {
			return fmt.Errorf("snapshot exceeds %d skills", maxSkills)
		}
		skillDir := path.Dir(skillPath)
		if skillDir == "." {
			return fmt.Errorf("SKILL.md must be inside a named skill directory")
		}
		s := Skill{URI: resourceURI(ns, skillPath), Resources: []ManifestFile{}}
		size := 0
		for _, p := range paths {
			if !strings.HasPrefix(p, skillDir+"/") {
				continue
			}
			uri := resourceURI(ns, p)
			data, ok := c.files[uri]
			if !ok {
				f, e := root.Open(p)
				if e != nil {
					return e
				}
				data, e = io.ReadAll(io.LimitReader(f, maxSkillBytes+1))
				closeErr := f.Close()
				if e != nil {
					return e
				}
				if closeErr != nil {
					return closeErr
				}
				*total += len(data)
				if *total > maxSnapshotBytes {
					return fmt.Errorf("snapshot exceeds 64 MiB")
				}
				c.files[uri] = data
			}
			size += len(data)
			s.Resources = append(s.Resources, ManifestFile{URI: uri, Digest: fmt.Sprintf("sha256:%x", sha256.Sum256(data)), Size: len(data)})
			if len(s.Resources) > 512 || size > maxSkillBytes {
				return fmt.Errorf("skill %s exceeds 512 files or 16 MiB", skillDir)
			}
		}
		s.Frontmatter, err = parseFrontmatter(c.files[s.URI], path.Base(skillDir))
		if err != nil {
			return fmt.Errorf("%s: %w", skillPath, err)
		}
		c.skills = append(c.skills, s)
	}
	return nil
}

func resourceURI(ns, p string) string {
	segments := strings.Split(p, "/")
	for i := range segments {
		segments[i] = url.PathEscape(segments[i])
	}
	return "skill://" + ns + "/" + strings.Join(segments, "/")
}

func parseFrontmatter(data []byte, name string) (map[string]any, error) {
	normalized := bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n"))
	if !bytes.HasPrefix(normalized, []byte("---\n")) {
		return nil, fmt.Errorf("missing YAML frontmatter")
	}
	rest := normalized[4:]
	end := bytes.Index(rest, []byte("\n---\n"))
	if end < 0 {
		return nil, fmt.Errorf("unterminated YAML frontmatter")
	}
	var fm map[string]any
	if err := yaml.Unmarshal(rest[:end], &fm); err != nil {
		return nil, err
	}
	declared, ok := fm["name"].(string)
	if !ok || declared != name || len(name) > 64 || !validName.MatchString(name) {
		return nil, fmt.Errorf("name must match its directory and skill naming rules")
	}
	description, ok := fm["description"].(string)
	if !ok || len(description) > 1024 || strings.TrimSpace(description) == "" {
		return nil, fmt.Errorf("description must be a nonempty string up to 1024 bytes")
	}
	if _, err := json.Marshal(fm); err != nil {
		return nil, fmt.Errorf("non-JSON frontmatter: %w", err)
	}
	return fm, nil
}
