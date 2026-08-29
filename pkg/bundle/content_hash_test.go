package bundle

import (
	"os"
	"path/filepath"
	"testing"
)

func TestContentHashDirMatchesPackedBundle(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, MainBotFile), []byte("workflow sample {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ManifestFile), []byte("name: sample\nschema_version: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".git", "ignored"), []byte("not logical content"), 0o644); err != nil {
		t.Fatal(err)
	}

	want, err := ContentHashDir(dir)
	if err != nil {
		t.Fatalf("ContentHashDir: %v", err)
	}
	archive := filepath.Join(t.TempDir(), "sample.botz")
	packed, err := PackDir(dir, archive)
	if err != nil {
		t.Fatalf("PackDir: %v", err)
	}
	if want != packed.Hash {
		t.Fatalf("directory hash = %s, packed hash = %s", want, packed.Hash)
	}
}
