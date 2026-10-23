package botregistry

// The two spellings of the scaffold-staging convention — discovery's
// predicate and botscaffold's — must agree, or a staging leftover one of
// them misses is listed, enabled and routable again. A table, not a
// property: both sides are one pattern and a shape.

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/botscaffold"
)

func TestTheStagingSpellingsAgree(t *testing.T) {
	for _, name := range []string{
		".ghost.scaffold-424242", ".a.scaffold-x", ".scaffold-1",
		"ghost.scaffold-1", ".ghost.scaffold", "scaffold", ".hidden",
		"bots", "main.bot", "",
	} {
		if got, want := isScaffoldStagingDir(name), botscaffold.IsStagingDir(name); got != want {
			t.Errorf("isScaffoldStagingDir(%q) = %v, botscaffold.IsStagingDir = %v — the two spellings diverged", name, got, want)
		}
	}
}
