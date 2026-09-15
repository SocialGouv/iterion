package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/unit"
)

// An embedded bot named by its basename from a directory that does not
// hold it is materialised as the whole program — a bot in several files
// lands with its fragments — and validates through the operator's own
// entry point. A name the binary does not carry comes back as typed and
// leaves no cache directory behind.
func TestResolveRecipePathMaterialisesTheWholeBot(t *testing.T) {
	cache := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cache)
	t.Chdir(t.TempDir())

	got := ResolveRecipePath("feature-dev/main.bot")
	if !strings.HasPrefix(got, cache) {
		t.Fatalf("resolved to %q, want a path under the cache %q", got, cache)
	}
	u := unit.LoadDir(got)
	if u.HasErrors() {
		for _, d := range u.Diagnostics {
			t.Errorf("%s", d.Error())
		}
		t.FailNow()
	}
	if len(u.Files) < 2 {
		t.Fatalf("the main was materialised alone: %d file(s) in the unit, want its fragments beside it", len(u.Files))
	}

	var out bytes.Buffer
	if err := RunValidate("feature-dev/main.bot", &Printer{W: &out, Format: OutputHuman}); err != nil {
		t.Fatalf("validate by basename: %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "result: OK") {
		t.Fatalf("validate by basename did not report OK:\n%s", out.String())
	}

	if got := ResolveRecipePath("nope/main.bot"); got != "nope/main.bot" {
		t.Errorf("a name the binary does not carry resolved to %q, want it back as typed", got)
	}
	if _, err := os.Stat(filepath.Join(cache, "iterion", "embedded-recipes", "nope")); !os.IsNotExist(err) {
		t.Errorf("a miss left something in the cache: %v", err)
	}
}
