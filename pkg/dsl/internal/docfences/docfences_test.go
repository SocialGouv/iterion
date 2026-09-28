package docfences

import (
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
		// The same spellings the extractor reads — a file a renderer reads
		// a fence from is a file this package must list (#1814).
		return HasFence(raw, "iter") || HasFence(raw, "yaml author")
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

// Every fence spelling a renderer reads, Extract reads too: tilde fences,
// a fence inside a blockquote, a space before the language, a four-backtick
// opener — and a fence closes on its own marker only (#1814).
func TestExtractReadsEverySpellingARendererReads(t *testing.T) {
	for _, tc := range []struct {
		name    string
		doc     string
		lang    string
		fences  int
		info    string
		wantErr string
	}{
		{"a plain backtick fence", "```iter\nprint()\n```\n", "iter", 1, "", ""},
		{"a tilde fence", "~~~iter\nprint()\n~~~\n", "iter", 1, "", ""},
		{"a fence inside a blockquote", "> ```yaml author\n> dsl: 2\n> workflow:\n>   name: w\n>   entry: done\n> ```\n", "yaml", 1, "author", ""},
		{"a space before the language", "``` yaml author\ndsl: 2\n```\n", "yaml", 1, "author", ""},
		{"a blockquote marker without a space", "> ```iter\n> print()\n> ```\n", "iter", 1, "", ""}, {"a closing fence at a lower indent", "  ```iter\n  print()\n```\n", "iter", 1, "", ""}, {"a four-backtick opener", "````iter\n```\nprint()\n````\n", "iter", 1, "", ""},
		{"a longer backtick run closes", "```iter\n````\n````\n```\n", "iter", 1, "", ""},
		{"a tilde fence is not closed by backticks", "~~~iter\nprint()\n```\n", "iter", 0, "", "never closed"},
		{"another language does not open", "```python\nprint()\n```\n", "iter", 0, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "doc.md")
			if err := os.WriteFile(path, []byte(tc.doc), 0o644); err != nil {
				t.Fatal(err)
			}
			fences, err := Extract(path, tc.lang)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("Extract err = %v, want it to say %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(fences) != tc.fences {
				t.Fatalf("got %d fences, want %d", len(fences), tc.fences)
			}
			if len(fences) > 0 && fences[0].Info != tc.info {
				t.Errorf("info = %q, want %q", fences[0].Info, tc.info)
			}
		})
	}
}
