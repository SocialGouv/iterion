package docfences

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every markdown file of the repository that carries a ```iter or a
// ```yaml author fence is one Files lists: a fence in a file the walk skips
// is a program, or a document, no test ever reads. CHANGELOG.md is the one
// exception: the release pipeline writes it, a record of past releases, not
// a page a reader copies from.
func TestFilesListsEveryMarkdownWithAFence(t *testing.T) {
	root := filepath.Join("..", "..", "..", "..")
	files, err := Files(root)
	if err != nil {
		t.Fatal(err)
	}
	listed := map[string]bool{}
	for _, f := range files {
		listed[filepath.Clean(f)] = true
	}
	carries := func(path string) bool {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		return bytes.Contains(raw, []byte("```iter")) || bytes.Contains(raw, []byte("```yaml author"))
	}
	candidates, err := filepath.Glob(filepath.Join(root, "*.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{"docs", "bots", "examples"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				switch d.Name() {
				case "node_modules", ".iterion", ".claude":
					return filepath.SkipDir
				}
				return nil
			}
			if strings.HasSuffix(p, ".md") {
				candidates = append(candidates, p)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	read := 0
	for _, p := range candidates {
		if filepath.Base(p) == "CHANGELOG.md" && filepath.Dir(p) == root || !carries(p) {
			continue
		}
		read++
		if !listed[filepath.Clean(p)] {
			t.Errorf("%s carries a DSL fence the docs tests never read: add it to Files", p)
		}
	}
	if read == 0 {
		t.Fatal("no markdown file carries a DSL fence: the walk is broken")
	}
}
