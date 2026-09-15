package bots

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
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

// The bot's directory is the parent of the nearest lib/ ancestor, at any
// depth, and the write order puts every fragment before every main
// whatever the names sort like — pinned on shapes the embed does not
// carry, since in the embed lib/ happens to sort before main.bot.
func TestBotDirAndWriteOrder(t *testing.T) {
	for name, want := range map[string]string{
		"b/main.bot":          "b",
		"b/agents.bot":        "b",
		"b/lib/x.bot":         "b",
		"b/lib/sub/x.bot":     "b",
		"b/lib/sub/lib/y.bot": "b/lib/sub",
		"lib/main.bot":        "lib",
	} {
		if got := botDirOf(name); got != want {
			t.Errorf("botDirOf(%q) = %q, want %q", name, got, want)
		}
	}
	got := orderForWrite("b", []string{"b/main.bot", "b/lib/z.bot", "b/agents.bot", "b/lib/sub/a.bot", "b/lib/a.bot"})
	want := []string{"b/lib/a.bot", "b/lib/sub/a.bot", "b/lib/z.bot", "b/agents.bot", "b/main.bot"}
	if !slices.Equal(got, want) {
		t.Fatalf("orderForWrite = %v, want %v", got, want)
	}
}

// A bot's files come fragments first and mains last, whichever of them
// names the bot, so a main on disk never wants for a fragment.
func TestBotFilesOrderFragmentsBeforeMains(t *testing.T) {
	files, _, ok := Sources("feature-dev/main.bot")
	if !ok {
		t.Fatal("feature-dev/main.bot is not embedded")
	}
	var fragment string
	for rel := range files {
		if strings.HasPrefix(rel, unit.FragmentDir+"/") {
			fragment = rel
			break
		}
	}
	if fragment == "" {
		t.Fatal("feature-dev embeds no fragment: the test lost its witness")
	}
	for _, name := range []string{"feature-dev/main.bot", path.Join("feature-dev", fragment)} {
		_, cleaned, paths, err := botFiles(name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if cleaned != name {
			t.Errorf("%s: cleaned to %q", name, cleaned)
		}
		if last := paths[len(paths)-1]; strings.Contains(last, "/"+unit.FragmentDir+"/") {
			t.Errorf("%s: the fragment %s is written last, want a main", name, last)
		}
		seenMain := false
		for _, p := range paths {
			if !strings.Contains(p, "/"+unit.FragmentDir+"/") {
				seenMain = true
			} else if seenMain {
				t.Errorf("%s: fragment %s is written after a main", name, p)
			}
		}
	}
}

// A file Materialize rewrites is published atomically: a reader that holds
// the file open through the rewrite keeps reading the bytes it opened,
// whole — never a truncated file. A write in place would show it the new
// bytes through the same descriptor.
func TestMaterializePublishesEachFileAtomically(t *testing.T) {
	root := t.TempDir()
	dst, err := Materialize(root, "feature-dev/main.bot")
	if err != nil {
		t.Fatal(err)
	}
	u := unit.LoadDir(dst)
	if len(u.Files) < 2 {
		t.Fatalf("%d file(s) in the unit, want a fragment to drift", len(u.Files))
	}
	drifted := filepath.Join(root, "feature-dev", filepath.FromSlash(u.Files[1].Rel))
	stale := bytes.Repeat([]byte("x"), len(u.Files[1].Source))
	if err := os.WriteFile(drifted, stale, 0o644); err != nil {
		t.Fatal(err)
	}
	reader, err := os.Open(drifted)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()

	if _, err := Materialize(root, "feature-dev/main.bot"); err != nil {
		t.Fatal(err)
	}
	seen, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(seen, stale) {
		t.Fatalf("a reader holding the file open saw it change under it (%d bytes, %q…): the rewrite was not a rename", len(seen), string(seen[:min(20, len(seen))]))
	}
	now, err := os.ReadFile(drifted)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(now, u.Files[1].Source) {
		t.Fatal("the file was not restored")
	}
	entries, err := os.ReadDir(filepath.Dir(drifted))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			t.Errorf("a temp file was left behind: %s", e.Name())
		}
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

	// A fragment names its bot: the whole bot is written, the fragment's
	// path handed back, and Sources keys the files from the bot's directory.
	var fragment string
	for _, f := range u.Files[1:] {
		fragment = f.Rel
		break
	}
	fragRoot := t.TempDir()
	got, err := Materialize(fragRoot, path.Join("feature-dev", fragment))
	if err != nil {
		t.Fatalf("materialize a fragment: %v", err)
	}
	if want := filepath.Join(fragRoot, "feature-dev", filepath.FromSlash(fragment)); got != want {
		t.Errorf("fragment materialised at %s, want %s", got, want)
	}
	if byFragment := unit.LoadDir(filepath.Join(fragRoot, "feature-dev", "main.bot")); byFragment.HasErrors() || byFragment.Digest != u.Digest {
		t.Errorf("naming a fragment did not write its whole bot beside it")
	}
	files, main, ok := Sources(path.Join("feature-dev", fragment))
	if !ok || main != fragment || files["main.bot"] == "" {
		t.Errorf("Sources by fragment: ok %v, main %q, has main.bot %v", ok, main, files["main.bot"] != "")
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
