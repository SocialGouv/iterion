package bundle

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// A subbot child resolves beside its parent, within the collection that
// holds the bundle — a sibling bundle is read, a child two levels up, an
// absolute path or a link out of the collection is not.
func TestResolveChildStaysWithinTheCollection(t *testing.T) {
	collection := t.TempDir()
	dir := filepath.Join(collection, "parent")
	for rel, body := range map[string]string{
		"parent/main.bot":   "subbot k:\n  source: \"kids/k.bot\"\n\nworkflow w:\n  entry: k\n  k -> done\n",
		"parent/kids/k.bot": "workflow k:\n  entry: done\n",
		"sibling/main.bot":  "workflow s:\n  entry: done\n",
		"../outside.bot":    "workflow o:\n  entry: done\n",
	} {
		full := filepath.Join(collection, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	parent := filepath.Join(dir, "main.bot")
	if path, src, ok := ResolveChild(dir, parent, "kids/k.bot"); !ok || len(src) == 0 || filepath.Base(path) != "k.bot" {
		t.Fatalf("a child beside the parent: ok=%v path=%q", ok, path)
	}
	if _, _, ok := ResolveChild(dir, parent, "../sibling/main.bot"); !ok {
		t.Fatal("a sibling bundle in the collection was not read")
	}
	for _, source := range []string{"../../outside.bot", filepath.Join(collection, "..", "outside.bot"), "", "kids/missing.bot"} {
		if _, _, ok := ResolveChild(dir, parent, source); ok {
			t.Fatalf("%q was read", source)
		}
	}
	if err := os.Symlink(filepath.Join(collection, "..", "outside.bot"), filepath.Join(dir, "kids", "link.bot")); err == nil {
		if _, _, ok := ResolveChild(dir, parent, "kids/link.bot"); ok {
			t.Fatal("a link out of the collection was read")
		}
	}
}

// A bundle reached through a symlinked directory resolves its children as
// the OS does — the parent's directory first, then the source — so
// `../sib/main.bot` from `link/parent` is the sibling beside the real
// bundle, not a lexical `link/sib` that is not there.
func TestResolveChildThroughASymlinkedDirectory(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real")
	for rel, body := range map[string]string{
		"parent/main.bot": "subbot k:\n  source: \"../sib/main.bot\"\n\nworkflow w:\n  entry: k\n  k -> done\n",
		"sib/main.bot":    "contract s:\n  version: 1\n\nworkflow s:\n  contract: s\n  entry: done\n",
	} {
		full := filepath.Join(real, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(base, "link")
	if err := os.MkdirAll(link, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(real, "parent"), filepath.Join(link, "parent")); err != nil {
		t.Skipf("no symlink here: %v", err)
	}
	dir := filepath.Join(link, "parent")
	path, src, ok := ResolveChild(dir, filepath.Join(dir, "main.bot"), "../sib/main.bot")
	if !ok || len(src) == 0 {
		t.Fatalf("the sibling beside the real bundle was not read through the link: ok=%v", ok)
	}
	if want, _ := filepath.EvalSymlinks(filepath.Join(real, "sib", "main.bot")); path != want {
		t.Fatalf("resolved %q, want %q", path, want)
	}
	// The syntax walk reads the same sibling through the link — the two
	// readers share the confinement — so its contract counts and nothing is
	// reported unread.
	req := MaxSyntaxRequirementsDir(dir)
	if len(req.Unread) != 0 || !slices.Contains(req.ContractedBy, "../sib/main.bot") {
		t.Fatalf("the walk through the link: unread=%v contracted_by=%v", req.Unread, req.ContractedBy)
	}
}
