package runview

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestResolveBundleFromFilePathPromotesASiblingEntry: a root-level .bot
// beside the bundle's main.bot is an ENTRY of the bundle (the golden-master
// shape — extend.bot, sync-harness.bot next to main.bot, #1367), opened
// with the file itself as the main so every surface that opens a workflow
// by path — run, resume, validate, doctor — sees the workflow it named
// with the bundle's manifest, prompts and skills around it, the way
// main.bot and the directory form are seen. A fragment under lib/ is a
// unit's, never an entry, and keeps the bare-file behavior.
func TestResolveBundleFromFilePathPromotesASiblingEntry(t *testing.T) {
	dir := promptedBundle(t)
	// The sibling references the bundle's prompt, as main.bot does: a bare
	// compile fails C003, so a green compile through the bundle is the
	// promotion itself.
	src, err := os.ReadFile(filepath.Join(dir, "main.bot"))
	if err != nil {
		t.Fatal(err)
	}
	sibling := filepath.Join(dir, "extend.bot")
	if err := os.WriteFile(sibling, src, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := CompileWorkflowWithHash(sibling); err == nil || !strings.Contains(err.Error(), "C003") {
		t.Fatalf("the bare compile of the sibling did not fail on the prompt reference (err=%v); the fixture no longer arms the case", err)
	}

	b, err := ResolveBundleFromFilePath(sibling)
	if err != nil {
		t.Fatalf("ResolveBundleFromFilePath(sibling): %v", err)
	}
	if b == nil {
		t.Fatal("a root-level sibling of the bundle's main.bot resolved to no bundle")
	}
	if filepath.Clean(b.IterPath) != filepath.Clean(sibling) {
		t.Fatalf("IterPath = %q, want the sibling %q: the entry is the main of its own open, not main.bot", b.IterPath, sibling)
	}
	if filepath.Clean(b.Dir) != filepath.Clean(dir) {
		t.Fatalf("Dir = %q, want the bundle at %q", b.Dir, dir)
	}

	// A fragment under lib/ is not an entry: the promotion does not reach
	// into the bundle's subdirectories.
	frag := filepath.Join(dir, "lib", "frag.bot")
	if err := os.MkdirAll(filepath.Dir(frag), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(frag, []byte("agent frag:\n  description: \"x\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if b, err := ResolveBundleFromFilePath(frag); b != nil || err != nil {
		t.Fatalf("ResolveBundleFromFilePath(lib/frag.bot) = (%+v, %v), want (nil, nil)", b, err)
	}
}

// TestResolveBundleFromFilePathRefusesASiblingWhoseBundleDoesNotOpen: the
// sibling of a main.bot whose manifest does not decode is that bundle's
// entry by the same markers, so the open fails loudly — never a silent
// fall-through to a bare file that runs without the bundle's prompts and
// skills.
func TestResolveBundleFromFilePathRefusesASiblingWhoseBundleDoesNotOpen(t *testing.T) {
	dir := promptedBundle(t)
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte("schema_version: 99\nname: [broken\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	sibling := filepath.Join(dir, "extend.bot")
	if err := os.WriteFile(sibling, []byte("workflow w:\n  entry: done\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if b, err := ResolveBundleFromFilePath(sibling); err == nil || b != nil || !strings.Contains(err.Error(), "does not open") {
		t.Fatalf("ResolveBundleFromFilePath(sibling) = (%+v, %v), want the bundle's refusal", b, err)
	}
}
