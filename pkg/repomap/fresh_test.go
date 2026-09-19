package repomap_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/repomap"
)

// repoRoot is where this package sits relative to the module root.
const repoRoot = "../.."

// The committed commons must describe the tree that is committed beside
// them. This is the gate: it rides the required `test` check, so a stale
// map fails the same run as a broken unit test and no separate CI job
// can be forgotten.
func TestGeneratedMapsAreFresh(t *testing.T) {
	stale, err := repomap.Stale(repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(stale) > 0 {
		t.Fatalf("stale generated maps in %v — run `task map:gen` and commit", stale)
	}
}

// A gate that cannot redden is not a gate — and it has to redden for
// EVERY corpus it claims to cover. An earlier version of this test
// symlinked docs/ and bots/ into the scratch root; filepath.WalkDir
// Lstats its root, so a symlinked root is "not a directory" and the walk
// ended immediately. Both those extractors rendered zero rows and the
// test passed anyway: it proved one of three.
func TestTheFreshnessGateBites(t *testing.T) {
	root := syntheticTree(t)
	before, err := repomap.Generate(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != 3 {
		t.Fatalf("generated %d maps, want 3", len(before))
	}
	for path, body := range before {
		if !strings.Contains(body, "|") {
			t.Fatalf("%s rendered no table row — the extractor read nothing:\n%s", path, body)
		}
	}

	// One removal per corpus, each of which must move its own artifact.
	for _, tc := range []struct {
		name   string
		remove string
		stem   string
		gone   string
	}{
		{"a Go package", "pkg/beta", "packages", "`pkg/beta`"},
		{"a docs page", "docs/adr/001-a-decision.md", "docs", "001-a-decision"},
		{"a bot skill", "bots/demo/skills/one.md", "bots", "demo-skill"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scratch := syntheticTree(t)
			if err := os.RemoveAll(filepath.Join(scratch, tc.remove)); err != nil {
				t.Fatal(err)
			}
			after, err := repomap.Generate(scratch)
			if err != nil {
				t.Fatal(err)
			}
			var stem repomap.Extractor
			for _, e := range repomap.Extractors() {
				if e.Stem() == tc.stem {
					stem = e
				}
			}
			path := repomap.Path(stem)
			if before[path] == after[path] {
				t.Fatalf("removing %s left %s byte-identical — that extractor is not reading the tree",
					tc.remove, path)
			}
			if strings.Contains(after[path], tc.gone) {
				t.Fatalf("%s still lists %q after it was removed", path, tc.gone)
			}
		})
	}
}

// syntheticTree writes a miniature repository exercising all three
// corpora: two Go packages, two docs pages (one an ADR), one bundle with
// a manifest, a workflow and a skill.
func syntheticTree(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		abs := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module example.test/demo\n\ngo 1.26\n")
	write("pkg/alpha/alpha.go", "// Package alpha does the first thing.\npackage alpha\n\n// Doer is a seam.\ntype Doer interface{ Do() }\n")
	write("pkg/beta/beta.go", "// Package beta does the second thing.\npackage beta\n\n// Run runs.\nfunc Run() {}\n")
	write("docs/guide.md", "# A guide\n\nIt explains the thing.\n")
	write("docs/adr/001-a-decision.md", "# ADR-001: A decision\n\n- **Status**: Accepted\n\nBecause of the reason.\n")
	write("bots/demo/manifest.yaml", "name: demo\ndisplay_name: Demo\nicon: \"🤖\"\nversion: 1.0.0\ndescription: A demo bundle.\n")
	write("bots/demo/main.bot", "workflow main:\n  entry: done\n")
	write("bots/demo/skills/one.md", "---\nname: demo-skill\ndescription: The demo skill.\n---\n\nBody.\n")
	if err := os.MkdirAll(filepath.Join(root, repomap.OutputDir), 0o755); err != nil {
		t.Fatal(err)
	}
	return root
}

// An artifact path must stay inside the committed commons directory:
// a generator that can write anywhere is a generator that will.
func TestArtifactPathsStayUnderTheCommonsDirectory(t *testing.T) {
	for _, e := range repomap.Extractors() {
		p := filepath.ToSlash(repomap.Path(e))
		if !strings.HasPrefix(p, repomap.OutputDir+"/") {
			t.Errorf("%s renders to %q, outside %s", e.Stem(), p, repomap.OutputDir)
		}
		if !strings.HasSuffix(p, ".md") {
			t.Errorf("%s renders to %q, which is not markdown", e.Stem(), p)
		}
	}
}
