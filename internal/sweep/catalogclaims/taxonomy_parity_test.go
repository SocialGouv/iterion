package catalogclaims

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/SocialGouv/iterion/internal/sweepwalk"
	"github.com/SocialGouv/iterion/pkg/bundle"
)

// TestBotTaxonomyVocabularyParity holds the studio's TS mirror of the
// navigation vocabulary to the Go source of truth (pkg/bundle/vocab.go).
// The mirror drives display order on every studio surface; two
// hand-synced copies without a guard drift in silence. Only the
// CATEGORIES are mirrored — the tag seed's consumers are Go-only, so a
// TS copy would be a third source with no reader.
func TestBotTaxonomyVocabularyParity(t *testing.T) {
	body, err := os.ReadFile(filepath.Join(sweepwalk.ModuleRoot(t), "studio", "src", "lib", "botTaxonomy.ts"))
	if err != nil {
		t.Fatalf("read studio mirror: %v", err)
	}
	src := string(body)

	slugs := regexp.MustCompile(`slug: "([a-z]+)"`).FindAllStringSubmatch(src, -1)
	if len(slugs) != len(bundle.BotCategories) {
		t.Fatalf("studio mirror declares %d categories, Go declares %d", len(slugs), len(bundle.BotCategories))
	}
	for i, m := range slugs {
		if m[1] != bundle.BotCategories[i].Slug {
			t.Errorf("mirror category[%d] = %q, want %q (canonical ORDER is part of the contract)", i, m[1], bundle.BotCategories[i].Slug)
		}
	}
}
