package bundle

import (
	"path/filepath"
	"testing"
)

// A bundle whose main is handed over opens without a main.bot on disk — the
// author document's case — with its manifest and its resource directories;
// the main has to sit at the bundle's root, and OpenDir still wants the file.
func TestOpenDirWithMainNeedsNoMainOnDisk(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir, map[string]string{"manifest.yaml": "schema_version: 1\nname: probe\n", "prompts/ask.md": "Say hello.\n"})
	main := filepath.Join(dir, "main.bot")
	b, err := OpenDirWithMain(dir, main)
	if err != nil {
		t.Fatalf("OpenDirWithMain: %v", err)
	}
	if b.IterPath != main {
		t.Fatalf("IterPath = %s, want the main handed over %s", b.IterPath, main)
	}
	if b.PromptsDir == "" || b.Manifest == nil || b.Manifest.Name != "probe" || b.Kind != KindBundleDir {
		t.Fatalf("the bundle around the main was not assembled: prompts=%q manifest=%+v kind=%v", b.PromptsDir, b.Manifest, b.Kind)
	}
	if _, err := OpenDir(dir); err == nil {
		t.Fatal("OpenDir opened a bundle that has no main.bot")
	}
	if _, err := OpenDirWithMain(dir, filepath.Join(dir, "lib", "main.bot")); err == nil {
		t.Fatal("a main outside the bundle's root was accepted")
	}
	if _, err := OpenDirWithMain(dir, ""); err == nil {
		t.Fatal("an empty main was accepted")
	}
}
