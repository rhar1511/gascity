package formula

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func TestCompileTracksTransitiveFormulaSourceFiles(t *testing.T) {
	dir := t.TempDir()
	base := []byte("formula = \"base\"\n[[steps]]\nid = \"base\"\ntitle = \"Base\"\n")
	child := []byte("formula = \"child\"\nextends = [\"base\"]\n[[steps]]\nid = \"child\"\ntitle = \"Child\"\n")
	for name, data := range map[string][]byte{"base.toml": base, "child.toml": child} {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	recipe, err := Compile(context.Background(), "child", []string{dir}, nil)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if len(recipe.FormulaSources) != 2 {
		t.Fatalf("FormulaSources = %#v, want child and inherited parent", recipe.FormulaSources)
	}
	want := map[string]string{
		filepath.Join(dir, "base.toml"):  digestFormulaSource(base),
		filepath.Join(dir, "child.toml"): digestFormulaSource(child),
	}
	for _, source := range recipe.FormulaSources {
		if want[source.Path] != source.ContentSHA256 {
			t.Errorf("source %q digest = %q, want %q", source.Path, source.ContentSHA256, want[source.Path])
		}
		delete(want, source.Path)
	}
	if len(want) != 0 {
		t.Fatalf("missing source files: %#v", want)
	}
}

func digestFormulaSource(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
