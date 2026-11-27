package bots

import (
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/pkg/bundle"
)

// TestCatalogBotsDeclareKnownCategory is the anti-absence gate for the
// navigation spine: every shipped bot manifest declares a category from
// the closed six-slug set, and every declared tag is in the curated seed.
// The bundlelint consistency gate next door already reddens UNKNOWN
// values (C270/C271 surface as warnings there); this test closes the
// ABSENT-category hole — a shipped bot silently drifting to Uncategorized
// is a catalog regression, not a legitimate state.
func TestCatalogBotsDeclareKnownCategory(t *testing.T) {
	manifests, err := filepath.Glob("*/manifest.yaml")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(manifests) == 0 {
		t.Fatal("no manifests found under bots/*/ — the discovery glob broke")
	}
	for _, path := range manifests {
		m, err := bundle.LoadManifest(path)
		if err != nil {
			t.Errorf("%s: load: %v", path, err)
			continue
		}
		if m == nil {
			continue
		}
		if _, ok := bundle.BotCategoryBySlug(m.Category); !ok {
			t.Errorf("%s: category %q is not in the closed set (%s) — every shipped bot declares one",
				path, m.Category, bundle.KnownBotCategorySlugs())
		}
		for _, tag := range m.Tags {
			if !bundle.IsKnownBotTag(tag) {
				t.Errorf("%s: tag %q is outside the curated seed — seed it in pkg/bundle/vocab.go if intentional",
					path, tag)
			}
		}
		// The safety pair is EXCLUSIVE by definition: a run either commits
		// into the target repo's history or it writes nothing anywhere.
		// Both at once is a contradiction the vocabulary refuses. (Neither
		// is legitimate for bots that never target a repository — the
		// conversational and board-only crew.)
		hasShips, hasReadonly := false, false
		for _, tag := range m.Tags {
			switch tag {
			case "ships-code":
				hasShips = true
			case "read-only":
				hasReadonly = true
			}
		}
		if hasShips && hasReadonly {
			t.Errorf("%s: carries both ships-code and read-only — the safety pair is exclusive", path)
		}
	}
}
