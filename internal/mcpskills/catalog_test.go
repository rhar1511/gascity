package mcpskills

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func fixture(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	write := func(path, data string) {
		t.Helper()
		p := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("review/SKILL.md", "---\nname: review\ndescription: Review a candidate\nallowed-tools: Read\nmetadata:\n  owner: engineering\n---\nRead references/check.md\n")
	write("review/references/check.md", "Check the contract\n")
	write("review/empty.txt", "")
	write("review/asset.bin", string([]byte{0xff, 0, 1}))
	write("review/nested/SKILL.md", "---\nname: nested\ndescription: Nested workflow\n---\nInspect\n")
	return root
}

func TestCatalogSnapshotIncludesNestedManifestsAndRawBytes(t *testing.T) {
	root := fixture(t)
	c, err := Load(map[string]string{"team": root})
	if err != nil {
		t.Fatal(err)
	}
	if len(c.skills) != 2 {
		t.Fatalf("skills: %d", len(c.skills))
	}
	s := c.skills[0]
	if s.URI != "skill://team/review/SKILL.md" || len(s.Resources) != 5 || s.Frontmatter["allowed-tools"] != "Read" {
		t.Fatalf("entry: %+v", s)
	}
	for _, r := range s.Resources {
		data := c.files[r.URI]
		if r.Digest != fmt.Sprintf("sha256:%x", sha256.Sum256(data)) || r.Size != len(data) {
			t.Fatalf("manifest mismatch: %+v", r)
		}
	}
	uri := "skill://team/review/references/check.md"
	before := bytes.Clone(c.files[uri])
	if err := os.WriteFile(filepath.Join(root, "review/references/check.md"), []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(c.files[uri], before) {
		t.Fatal("snapshot changed")
	}
}

func TestCatalogRejectsInvalidSources(t *testing.T) {
	for _, kind := range []string{"name", "yaml", "symlink", "oversize", "namespace"} {
		t.Run(kind, func(t *testing.T) {
			root := fixture(t)
			switch kind {
			case "name":
				if err := os.WriteFile(filepath.Join(root, "review/SKILL.md"), []byte("---\nname: other\ndescription: x\n---\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "yaml":
				if err := os.WriteFile(filepath.Join(root, "review/SKILL.md"), []byte("---\nname: review\nname: duplicate\ndescription: x\n---\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Symlink(filepath.Join(root, "review/SKILL.md"), filepath.Join(root, "review/leak")); err != nil {
					t.Fatal(err)
				}
			case "oversize":
				f, err := os.Create(filepath.Join(root, "review/huge"))
				if err != nil {
					t.Fatal(err)
				}
				err = f.Truncate(16 << 20)
				closeErr := f.Close()
				if err != nil || closeErr != nil {
					t.Fatal(err, closeErr)
				}
			}
			ns := "team"
			if kind == "namespace" {
				ns = "../escape"
			}
			if _, err := Load(map[string]string{ns: root}); err == nil {
				t.Fatal("invalid catalog accepted")
			}
		})
	}
}
