//go:build unix

package store

import (
	"context"
	"errors"
	"golang.org/x/sys/unix"
	"os"
	"path/filepath"
	"testing"
)

func TestRegularRunFileRootRefusesSpecialEntries(t *testing.T) {
	p := t.TempDir()
	root, e := os.OpenRoot(p)
	if e != nil {
		t.Fatal(e)
	}
	defer root.Close()
	if e = root.WriteFile("résultat..txt", []byte("bytes"), 0600); e != nil {
		t.Fatal(e)
	}
	f, info, e := OpenRegularRunFileRoot(context.Background(), root, "résultat..txt")
	if e != nil {
		t.Fatal(e)
	}
	f.Close()
	if info.Size() != 5 {
		t.Fatal(info)
	}
	if e = os.Symlink("résultat..txt", filepath.Join(p, "link")); e != nil {
		t.Fatal(e)
	}
	if e = unix.Mkfifo(filepath.Join(p, "fifo"), 0600); e != nil {
		t.Fatal(e)
	}
	if e = root.Mkdir("directory", 0700); e != nil {
		t.Fatal(e)
	}
	for _, name := range []string{"link", "fifo", "directory", "../outside", ".", "", "/"} {
		if f, _, e := OpenRegularRunFileRoot(context.Background(), root, name); e == nil {
			f.Close()
			t.Fatalf("accepted %q", name)
		}
	}
}

func TestRegularRunFileRootRejectsAbsoluteNameBeforeOpen(t *testing.T) {
	r, e := os.OpenRoot(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	defer r.Close()
	f, _, e := OpenRegularRunFileRoot(context.Background(), r, "/")
	if f != nil {
		f.Close()
	}
	if !errors.Is(e, os.ErrInvalid) {
		t.Fatalf("absolute entry reached opener: %v", e)
	}
}
