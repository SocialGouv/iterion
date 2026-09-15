package botscaffold

import (
	"path/filepath"
	"slices"
	"testing"

	"github.com/SocialGouv/iterion/pkg/bundle"
	"github.com/SocialGouv/iterion/pkg/dsl/parser"
)

// Every template scaffolds a bundle whose manifest declares the floor its
// own sources need: a shape that imports asks for the release that reads
// `import`, whatever build scaffolds it. A fresh scaffold that drew C252
// — or was refused at push — on its own floor would be a defect of the
// scaffold, not of the operator.
func TestEveryTemplateDeclaresTheFloorItsSourcesNeed(t *testing.T) {
	for _, tpl := range Templates() {
		t.Run(tpl.ID, func(t *testing.T) {
			spec := tpl.Spec
			spec.Slug = "floor-" + tpl.ID
			spec.DisplayName = "Floor"
			dir := filepath.Join(t.TempDir(), spec.Slug)
			if _, err := Scaffold(dir, spec); err != nil {
				t.Fatalf("Scaffold: %v", err)
			}
			m, err := bundle.LoadManifest(filepath.Join(dir, bundle.ManifestFile))
			if err != nil {
				t.Fatal(err)
			}
			req := bundle.MaxSyntaxRequirementsDir(dir)
			if pf := bundle.CheckSyntaxFloor(m, req); !pf.OK {
				t.Fatalf("the scaffold declares %q but its sources use %s, which needs %s (C252 on a fresh scaffold)", pf.Declared, req.Describe(), pf.Need)
			}
			if tpl.Spec.Shape == "library" {
				if !req.UsesImport() {
					t.Fatal("the library template no longer imports: the test proves nothing")
				}
				c, err := bundle.ParseEngineConstraint(m.Requires.Iterion)
				if err != nil {
					t.Fatalf("the library template declares %q: %v", m.Requires.Iterion, err)
				}
				need, _ := bundle.ParseEngineConstraint(">= " + parser.ImportSince)
				if slices.Compare(c.Min, need.Min) < 0 {
					t.Fatalf("the library template declares %q, below the release that reads import (%s)", m.Requires.Iterion, parser.ImportSince)
				}
			}
		})
	}
}
