package botscaffold

import (
	"os"
	"path/filepath"
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
	taxonomyChecked := 0
	for _, tpl := range Templates() {
		if tpl.Spec.Category != "" {
			taxonomyChecked++
		}
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
			// The predicate judges BOTH halves of what the scaffold wrote:
			// the rendered sources and the keys its manifest declares.
			if pf := bundle.CheckSyntaxFloor(m, req); !pf.OK {
				t.Fatalf("the scaffold declares %q but its bundle asks for %s, which needs %s (C252 on a fresh scaffold)", pf.Declared, pf.Uses, pf.Need)
			}
			// The keys the scaffold ASKS the table for must be the
			// floor-bearing keys it actually WROTE: a template that starts
			// writing another one reddens here instead of shipping a floor
			// that is short.
			body, err := os.ReadFile(filepath.Join(dir, bundle.ManifestFile))
			if err != nil {
				t.Fatal(err)
			}
			written, err := bundle.DeclaredManifestKeys(body)
			if err != nil {
				t.Fatal(err)
			}
			fromWritten, reason := bundle.RequiredReleaseOf(bundle.FloorSubject{Keys: written})
			fromAsked, _ := bundle.RequiredReleaseOf(bundle.FloorSubject{Keys: spec.manifestKeys()})
			if fromWritten != fromAsked {
				t.Fatalf("the rendered manifest asks for %s (%s) through its keys, the scaffold asked the table for %s: Spec.manifestKeys does not name every floor-bearing key the template writes", fromWritten, reason, fromAsked)
			}
			if tpl.Spec.Category != "" {
				if m.Requires == nil {
					t.Fatal("a categorized scaffold declares no engine floor")
				}
				c, err := bundle.ParseEngineConstraint(m.Requires.Iterion)
				if err != nil {
					t.Fatalf("the categorized template declares %q: %v", m.Requires.Iterion, err)
				}
				// The release is read from the pin the floors table carries,
				// never spelled here: the day the cut realigns it, the
				// scaffold follows and this case follows with it.
				if cmp, ok := c.CompareFloor(bundle.CatalogTaxonomySince); !ok || cmp < 0 {
					t.Fatalf("the categorized template declares %q, below the release that reads category/tags (%s)", m.Requires.Iterion, bundle.CatalogTaxonomySince)
				}
			}
			if tpl.Spec.Shape == "library" {
				if !req.UsesImport() {
					t.Fatal("the library template no longer imports: the test proves nothing")
				}
				c, err := bundle.ParseEngineConstraint(m.Requires.Iterion)
				if err != nil {
					t.Fatalf("the library template declares %q: %v", m.Requires.Iterion, err)
				}
				if cmp, ok := c.CompareFloor(parser.ImportSince); !ok || cmp < 0 {
					t.Fatalf("the library template declares %q, below the release that reads import (%s)", m.Requires.Iterion, parser.ImportSince)
				}
			}
		})
	}
	if taxonomyChecked != 14 {
		t.Fatalf("checked %d categorized templates, want the gallery's 14", taxonomyChecked)
	}
}

func TestScaffoldKeepsExplicitEngineFloorWithCategory(t *testing.T) {
	tpl, ok := TemplateByID("daily-digest")
	if !ok {
		t.Fatal("daily-digest template not found")
	}
	spec := tpl.Spec
	spec.Slug = "floor-explicit"
	spec.EngineFloor = "3.224.0"
	dir := filepath.Join(t.TempDir(), spec.Slug)
	if _, err := Scaffold(dir, spec); err != nil {
		t.Fatalf("Scaffold: %v", err)
	}
	m, err := bundle.LoadManifest(filepath.Join(dir, bundle.ManifestFile))
	if err != nil {
		t.Fatal(err)
	}
	if m.Requires == nil || m.Requires.Iterion != ">= 3.224.0" {
		t.Fatalf("explicit EngineFloor was replaced: %+v", m.Requires)
	}
}
