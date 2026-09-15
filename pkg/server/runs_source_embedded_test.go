package server

import (
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/unit"
)

// wholeUnitAt fails the test unless the main at p loads as a unit of more
// than one file with no error: the embedded bot landed with its fragments.
func wholeUnitAt(t *testing.T, p string) {
	t.Helper()
	u := unit.LoadDir(p)
	if u.HasErrors() {
		for _, d := range u.Diagnostics {
			t.Errorf("%s", d.Error())
		}
		t.FailNow()
	}
	if len(u.Files) < 2 {
		t.Fatalf("the main was materialised alone: %d file(s) in the unit, want its fragments beside it", len(u.Files))
	}
}

// The store-side copy of an embedded bot is the whole program, so a launch
// that resolves to it compiles what the tree compiles.
func TestMaterializeEmbeddedRecipeWritesTheWholeBot(t *testing.T) {
	srv, _ := newTestServer(t)
	got, ok := srv.materializeEmbeddedRecipe("feature-dev/main.bot")
	if !ok {
		t.Fatal("feature-dev/main.bot is not materialised from the embed")
	}
	wholeUnitAt(t, got)
	if _, ok := srv.materializeEmbeddedRecipe("nope/main.bot"); ok {
		t.Error("a name the binary does not carry was materialised")
	}
}

// A server with no working directory resolves a launch by basename through
// the embed, and hands the launch the whole program.
func TestResolveWorkflowPathFallsBackToTheWholeEmbeddedBot(t *testing.T) {
	srv := &Server{cfg: Config{StoreDir: t.TempDir()}}
	got, err := srv.resolveWorkflowPath("feature-dev/main.bot", "")
	if err != nil {
		t.Fatalf("resolve by basename with no WorkDir: %v", err)
	}
	wholeUnitAt(t, got)
}
