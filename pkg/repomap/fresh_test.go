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

// A gate that cannot redden is not a gate. Rendering a map over a tree
// whose sources changed must produce different bytes than the committed
// artifact — otherwise the test above would pass over any drift.
func TestTheFreshnessGateBites(t *testing.T) {
	generated, err := repomap.Generate(repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(generated) == 0 {
		t.Fatal("no map generated at all")
	}

	// Copy the repo's shape into a scratch root, then remove a package
	// from it. The map of the scratch tree must differ from the map of
	// the real one — if it does not, the extractor is not reading the
	// tree it claims to describe.
	scratch := t.TempDir()
	mustLink(t, repoRoot, scratch, "docs")
	mustLink(t, repoRoot, scratch, "bots")
	mustCopyGoTree(t, repoRoot, scratch)

	if err := os.RemoveAll(filepath.Join(scratch, "pkg", "repomap")); err != nil {
		t.Fatal(err)
	}
	mutated, err := repomap.Generate(scratch)
	if err != nil {
		t.Fatal(err)
	}
	before := generated[repomap.Path(repomap.Extractors()[0])]
	after := mutated[repomap.Path(repomap.Extractors()[0])]
	if before == after {
		t.Fatal("removing a package left the package map byte-identical — " +
			"the extractor is not reading the tree")
	}
	if strings.Contains(after, "`pkg/repomap`") {
		t.Fatal("the package map still lists a package that is no longer there")
	}
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

// mustLink symlinks a subtree into the scratch root, so the test reads
// the real docs and bots without copying megabytes.
func mustLink(t *testing.T, root, scratch, name string) {
	t.Helper()
	abs, err := filepath.Abs(filepath.Join(root, name))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(abs, filepath.Join(scratch, name)); err != nil {
		t.Fatal(err)
	}
}

// mustCopyGoTree copies the Go sources the package map describes. Only
// the directory shape and the .go files matter, so the copy stays small.
func mustCopyGoTree(t *testing.T, root, scratch string) {
	t.Helper()
	for _, top := range []string{"pkg", "cmd", "internal"} {
		src := filepath.Join(root, top)
		if _, err := os.Stat(src); err != nil {
			continue
		}
		err := filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			dst := filepath.Join(scratch, rel)
			if d.IsDir() {
				return os.MkdirAll(dst, 0o755)
			}
			if !strings.HasSuffix(d.Name(), ".go") {
				return nil
			}
			body, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			return os.WriteFile(dst, body, 0o644)
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}
