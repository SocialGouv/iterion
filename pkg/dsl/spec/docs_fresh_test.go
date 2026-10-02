package spec_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/internal/docfences"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
	"github.com/SocialGouv/iterion/pkg/dsl/spec"
)

// The committed renderings of the registry — the property reference, the
// grammar's tables, the skills' property section, the Monaco module, the
// author schema artefacts — must be what the registry renders today.
// Regenerate with `task dsl:gen` (`iterion dsl spec --write`) and commit the
// result.
func TestGeneratedDSLDocsAreFresh(t *testing.T) {
	root := filepath.Join("..", "..", "..")
	stale, err := spec.Stale(root, parser.Keywords(), parser.MaxProfile)
	if err != nil {
		t.Fatal(err)
	}
	if len(stale) > 0 {
		t.Fatalf("stale generated regions in %v — run `task dsl:gen` and commit", stale)
	}
}

// A generated region in a document nobody regenerates rots in silence:
// every documentation file carrying a region marker — the set the docs
// fence tests read — is one `task dsl:gen` rewrites and `task dsl:check`
// holds fresh (spec.Files).
func TestEveryDocumentWithARegionIsRegenerated(t *testing.T) {
	root := filepath.Join("..", "..", "..")
	files, err := docfences.Files(root)
	if err != nil {
		t.Fatal(err)
	}
	listed := map[string]bool{}
	for _, f := range spec.Files {
		listed[f] = true
	}
	regions := 0
	for _, path := range files {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(raw), "<!-- dsl-spec:begin ") {
			continue
		}
		regions++
		rel, err := filepath.Rel(root, path)
		if err != nil {
			t.Fatal(err)
		}
		if !listed[filepath.ToSlash(rel)] {
			t.Errorf("%s carries a dsl-spec region but is not in spec.Files: `task dsl:gen` never rewrites it and `task dsl:check` never reads it", rel)
		}
	}
	if regions < len(spec.Files) {
		t.Fatalf("%d documents carry a region, spec.Files lists %d: the walk misses documents", regions, len(spec.Files))
	}
}

// Every region kind renders, and an unknown one is refused rather than
// spliced as nothing.
func TestRenderRefusesAnUnknownRegion(t *testing.T) {
	for _, what := range []string{"reference", "skill", "author", "table agent", "table sandbox.network"} {
		if _, err := spec.Render(what); err != nil {
			t.Errorf("Render(%q): %v", what, err)
		}
	}
	for _, what := range []string{"table nokind", "tables", ""} {
		if _, err := spec.Render(what); err == nil {
			t.Errorf("Render(%q) accepted", what)
		}
	}
	if _, err := spec.Splice("no region here"); err == nil {
		t.Error("Splice accepted a document without a region")
	}
}
