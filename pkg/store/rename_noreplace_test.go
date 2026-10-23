package store

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// RenameNoReplace publishes under a free name and refuses every name that
// exists — a file, a directory holding files, an EMPTY directory — with
// fs.ErrExist, the source left where it was and the name untouched.
func TestRenameNoReplaceTakesNoExistingName(t *testing.T) {
	dir := t.TempDir()
	stage := filepath.Join(dir, ".stage")
	if err := os.MkdirAll(stage, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stage, "main.bot"), []byte("mine\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name string
		make func(path string) error
	}{
		{"a file", func(p string) error { return os.WriteFile(p, []byte("theirs\n"), 0o644) }},
		{"a directory holding a file", func(p string) error {
			if err := os.Mkdir(p, 0o755); err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(p, "README.md"), []byte("theirs\n"), 0o644)
		}},
		{"an empty directory", func(p string) error { return os.Mkdir(p, 0o755) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			target := filepath.Join(dir, "taken-"+filepath.Base(t.Name()))
			if err := c.make(target); err != nil {
				t.Fatal(err)
			}
			err := RenameNoReplace(stage, target)
			if !errors.Is(err, fs.ErrExist) {
				t.Fatalf("RenameNoReplace onto %s = %v, want fs.ErrExist", c.name, err)
			}
			if _, err := os.Stat(filepath.Join(stage, "main.bot")); err != nil {
				t.Fatalf("the source moved: %v", err)
			}
			if _, err := os.Stat(filepath.Join(target, "main.bot")); err == nil {
				t.Fatalf("the source landed in %s", c.name)
			}
		})
	}
	free := filepath.Join(dir, "free")
	if err := RenameNoReplace(stage, free); err != nil {
		t.Fatalf("RenameNoReplace onto a free name = %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(free, "main.bot")); err != nil || string(b) != "mine\n" {
		t.Fatalf("the published directory holds %q (%v)", b, err)
	}
}
