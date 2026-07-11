package plugin

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInspectFound(t *testing.T) {
	dir := t.TempDir()
	writeFileT(t, filepath.Join(dir, ManifestFile), "name: demo\ncontributes:\n  skills: [skills/a.md]\n")
	writeFileT(t, filepath.Join(dir, "skills", "a.md"), "# a\n")
	writeFileT(t, filepath.Join(dir, "Readme.md"), "# Demo plugin\n")

	info, err := Inspect(context.Background(), dir)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if info.Manifest.Name != "demo" {
		t.Errorf("Manifest.Name = %q, want demo", info.Manifest.Name)
	}
	if info.README != "# Demo plugin\n" {
		t.Errorf("README = %q, want the Readme.md body (case-tolerant match)", info.README)
	}
}

func TestInspectNoManifest(t *testing.T) {
	_, err := Inspect(context.Background(), t.TempDir())
	if !errors.Is(err, ErrNoManifest) {
		t.Fatalf("Inspect on empty dir: err = %v, want errors.Is ErrNoManifest", err)
	}
}

func TestInspectInvalidManifest(t *testing.T) {
	dir := t.TempDir()
	writeFileT(t, filepath.Join(dir, ManifestFile), "name: x\n") // contributes nothing
	if _, err := Inspect(context.Background(), dir); err == nil {
		t.Fatal("Inspect accepted an invalid manifest")
	}
}

func TestReadReadmeCapAndMissing(t *testing.T) {
	dir := t.TempDir()
	// Missing README is empty, not an error.
	got, err := ReadReadme(dir)
	if err != nil || got != "" {
		t.Fatalf("ReadReadme(no readme) = %q, %v; want empty, nil", got, err)
	}
	// Oversized README is capped at 16 KiB.
	writeFileT(t, filepath.Join(dir, "README.md"), strings.Repeat("x", readmeCap+100))
	got, err = ReadReadme(dir)
	if err != nil {
		t.Fatalf("ReadReadme: %v", err)
	}
	if len(got) != readmeCap {
		t.Fatalf("README length = %d, want capped at %d", len(got), readmeCap)
	}
}

// writeFileT writes content to path, creating parent directories.
func writeFileT(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
