package spec_test

import (
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/spec"
)

// The committed renderings of the registry — the property reference, the
// grammar's tables, the skills' property section — must be what the
// registry renders today. Regenerate with `task dsl:gen` (`iterion dsl spec
// --write`) and commit the result.
func TestGeneratedDSLDocsAreFresh(t *testing.T) {
	root := filepath.Join("..", "..", "..")
	stale, err := spec.Stale(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(stale) > 0 {
		t.Fatalf("stale generated regions in %v — run `task dsl:gen` and commit", stale)
	}
}

// Every region kind renders, and an unknown one is refused rather than
// spliced as nothing.
func TestRenderRefusesAnUnknownRegion(t *testing.T) {
	for _, what := range []string{"reference", "skill", "table agent", "table sandbox.network"} {
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
