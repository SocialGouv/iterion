package bundle

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// #1896: a loose catalogue entry (bots/smoke/board_smoke.bot — no main.bot
// beside it, so no bundle's entry) is frozen as its entry file plus the
// lib/ fragments its imports reach. The directory it sits in is NOT the
// bundle: the canaries a parent directory may hold — an .env, a
// devbox.json, an unrelated document, another loose workflow — never cross
// (freezing the parent whole was the rejected alias fix's transport).
func TestSnapshotLooseEntryFreezesTheUnitAlone(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, body string) {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("board_smoke.bot", "workflow board_smoke:\n  entry: done\n")
	write("lib/frag.bot", "agent frag:\n  description: \"x\"\n")
	write(".env", "PRIVATE_CANARY=1\n")
	write("devbox.json", "{}\n")
	write("notes.md", "unrelated\n")
	write("other.bot", "workflow other:\n  entry: done\n")

	s := &Snapshot{Root: "smoke", Entry: "smoke/board_smoke.bot"}
	if err := s.AddLooseEntry("smoke", filepath.Join(dir, "board_smoke.bot"), "lib"); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"smoke/board_smoke.bot", "smoke/lib/frag.bot"} {
		if _, ok := s.Files[want]; !ok {
			t.Errorf("%s was not frozen; files: %v", want, s.Files)
		}
	}
	for _, never := range []string{"smoke/.env", "smoke/devbox.json", "smoke/notes.md", "smoke/other.bot"} {
		if _, ok := s.Files[never]; ok {
			t.Errorf("%s crossed: the parent directory is no bundle", never)
		}
	}
	if len(s.Files) != 2 {
		t.Fatalf("the loose unit is two files, froze %d: %v", len(s.Files), s.Files)
	}

	body, digest, err := s.Encode()
	if err != nil {
		t.Fatal(err)
	}
	frozen, err := DecodeSnapshot(body, digest)
	if err != nil {
		t.Fatal(err)
	}
	if frozen.EntryPath() != "smoke/board_smoke.bot" {
		t.Fatalf("entry lost over the wire: %q", frozen.EntryPath())
	}
	root, cleanup, err := frozen.Materialize()
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	// The materialized root opens with the ENTRY as its main — there is no
	// main.bot to find, and none of the parent directory's other files.
	b, err := OpenDirEntry(root, frozen.EntryPath())
	if err != nil {
		t.Fatal(err)
	}
	if b.IterPath != filepath.Join(root, "board_smoke.bot") {
		t.Fatalf("main = %q, want the entry", b.IterPath)
	}
	if b.SkillsDir != "" || b.Manifest != nil {
		t.Fatalf("the loose unit gained bundle resources: %+v", b)
	}
	if _, err := os.Stat(filepath.Join(root, ".env")); !os.IsNotExist(err) {
		t.Fatalf("the canary materialized: %v", err)
	}
}

// The entry Validate requires is the LAUNCHED file, not the literal
// main.bot; an entry that is not a root-level file of the root bundle is
// no entry at all.
func TestSnapshotEntryValidation(t *testing.T) {
	// A snapshot frozen before Entry travelled still answers main.bot.
	legacy := &Snapshot{Root: "root", Files: map[string]SnapshotFile{}}
	if legacy.EntryPath() != "root/main.bot" {
		t.Fatalf("legacy entry = %q", legacy.EntryPath())
	}
	if _, _, err := legacy.Encode(); err == nil || !strings.Contains(err.Error(), "parent main.bot is missing") {
		t.Fatalf("legacy refusal changed: %v", err)
	}
	// A named entry that is absent is refused by its own name.
	s := &Snapshot{Root: "smoke", Entry: "smoke/board_smoke.bot", Files: map[string]SnapshotFile{}}
	if _, _, err := s.Encode(); err == nil || !strings.Contains(err.Error(), "entry smoke/board_smoke.bot is missing") {
		t.Fatalf("missing entry = %v", err)
	}
	// An entry that is not a root-level file of the root bundle is refused.
	for _, entry := range []string{"other/main.bot", "root/lib/frag.bot", "root//x.bot", "../x.bot", "/abs.bot"} {
		bad := &Snapshot{Root: "root", Entry: entry, Files: map[string]SnapshotFile{entry: {Content: []byte("x")}}}
		if _, _, err := bad.Encode(); err == nil {
			t.Errorf("entry %q accepted", entry)
		}
	}
	// A promoted sibling entry beside its main.bot validates.
	sib := &Snapshot{Root: "x", Entry: "x/extend.bot", Files: map[string]SnapshotFile{
		"x/main.bot":   {Content: []byte("workflow w:\n  entry: done\n")},
		"x/extend.bot": {Content: []byte("workflow e:\n  entry: done\n")},
	}}
	if _, _, err := sib.Encode(); err != nil {
		t.Fatalf("promoted sibling entry: %v", err)
	}
	// The entry with no content is missing, named or not.
	empty := &Snapshot{Root: "x", Entry: "x/extend.bot", Files: map[string]SnapshotFile{"x/extend.bot": {}}}
	if _, _, err := empty.Encode(); err == nil {
		t.Fatal("an empty entry validated")
	}
}

// A loose entry is no alias for a hostile path either: the file it freezes
// is the file on disk, symlink-free, as AddDir demands of a bundle.
func TestSnapshotLooseEntryRefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "real.bot")
	if err := os.WriteFile(target, []byte("workflow w:\n  entry: done\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "board_smoke.bot")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if err := (&Snapshot{Root: "smoke", Entry: "smoke/board_smoke.bot"}).AddLooseEntry("smoke", link, "lib"); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("symlink entry: %v", err)
	}
}
