package bots

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SocialGouv/iterion/pkg/dsl/ir"
	"github.com/SocialGouv/iterion/pkg/dsl/unit"
)

// Every embedded main.bot is a whole program INSIDE the embed: the
// fragments its imports reach are embedded beside it, and the merged unit
// compiles. The witness is a bot in several files — were none embedded the
// test could not bite, so it refuses to pass without one.
func TestEmbeddedMainsAreWholePrograms(t *testing.T) {
	mains, several := 0, 0
	for _, name := range List() {
		if path.Base(name) != "main.bot" {
			continue
		}
		mains++
		dir := path.Dir(name)
		files := map[string]string{}
		err := fs.WalkDir(Files, dir, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			data, err := Files.ReadFile(p)
			if err != nil {
				return err
			}
			files[strings.TrimPrefix(p, dir+"/")] = string(data)
			return nil
		})
		if err != nil {
			t.Fatalf("%s: walk the embed: %v", dir, err)
		}
		u := unit.LoadMap(files, "main.bot")
		if u.HasErrors() {
			for _, d := range u.Diagnostics {
				t.Errorf("%s: %s", name, d.Error())
			}
			continue
		}
		if len(u.Files) > 1 {
			several++
		}
		cr := ir.Compile(u.Merged)
		if cr.Workflow == nil {
			t.Errorf("%s: compile produced no Workflow", name)
		}
		for _, d := range cr.Diagnostics {
			if d.Severity == ir.SeverityError {
				t.Errorf("%s: compile error: %s", name, d.Error())
			}
		}
	}
	if mains == 0 {
		t.Fatal("no main.bot is embedded — the embed directive lost its recipes")
	}
	if several == 0 {
		t.Fatal("no embedded bot is in several files: the test lost its witness, embed one or retire the test")
	}
}

// List names recipes: a fragment under a bot's lib/ is a piece of one, not
// one, and Sources hands the whole bot over as a files map.
func TestListNamesRecipesNotFragments(t *testing.T) {
	names := List()
	for _, n := range names {
		if strings.Contains(n, "/"+unit.FragmentDir+"/") {
			t.Errorf("a fragment is listed as a recipe: %s", n)
		}
	}
	files, main, ok := Sources("feature-dev/main.bot")
	if !ok || main != "main.bot" {
		t.Fatalf("Sources(feature-dev/main.bot): ok %v, main %q", ok, main)
	}
	if len(files) < 2 {
		t.Fatalf("Sources holds %d file(s), want the fragments too", len(files))
	}
	for rel := range files {
		if rel != "main.bot" && !strings.HasPrefix(rel, unit.FragmentDir+"/") {
			t.Errorf("Sources key %q is neither the main nor under %s/", rel, unit.FragmentDir)
		}
	}
	if _, _, ok := Sources("feature-dev/lib"); ok {
		t.Error("a directory was handed over as a bot")
	}
}

// Materialize writes the whole bot, so the main on disk is the program it
// is in the tree; it restores a fragment that drifted; a miss and a
// directory write nothing.
func TestMaterializeWritesTheWholeBot(t *testing.T) {
	root := t.TempDir()
	dst, err := Materialize(root, "feature-dev/main.bot")
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if want := filepath.Join(root, "feature-dev", "main.bot"); dst != want {
		t.Fatalf("materialised at %s, want %s", dst, want)
	}
	u := unit.LoadDir(dst)
	if u.HasErrors() {
		for _, d := range u.Diagnostics {
			t.Errorf("%s", d.Error())
		}
		t.FailNow()
	}
	if len(u.Files) < 2 {
		t.Fatalf("the main was materialised alone: %d file(s) in the unit, want its fragments beside it", len(u.Files))
	}

	// Same length as the embedded bytes: a writer that compared lengths
	// would keep the drift.
	drifted := filepath.Join(root, "feature-dev", filepath.FromSlash(u.Files[1].Rel))
	if err := os.WriteFile(drifted, bytes.Repeat([]byte("x"), len(u.Files[1].Source)), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Materialize(root, "feature-dev/main.bot"); err != nil {
		t.Fatalf("materialize again: %v", err)
	}
	if again := unit.LoadDir(dst); again.HasErrors() || again.Digest != u.Digest {
		t.Fatalf("a drifted fragment was not rewritten (errors %v, digest %s vs %s)", again.HasErrors(), again.Digest, u.Digest)
	}

	for _, name := range []string{"nope/main.bot", "feature-dev", "feature-dev/lib", "../feature-dev/main.bot"} {
		if _, err := Materialize(root, name); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("Materialize(%q): err %v, want fs.ErrNotExist", name, err)
		}
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "feature-dev" {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("the root holds %v, want feature-dev alone: a miss must write nothing", names)
	}
}
