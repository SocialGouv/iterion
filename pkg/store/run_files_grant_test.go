package store

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestIncarnationRunFilesEnsureRefusesAncestorLinks(t *testing.T) {
	for _, f := range []ValueFormat{1, 2} {
		t.Run(fmt.Sprint(f), func(t *testing.T) {
			s, ctx := directoryFixture(t, f)
			root, e := s.EnsureRunFilesDir(ctx, "r")
			if e != nil {
				t.Fatal(e)
			}
			parent := filepath.Dir(root)
			held := parent + "-held"
			if e = os.Rename(parent, held); e != nil {
				t.Fatal(e)
			}
			if e = os.Symlink(held, parent); e != nil {
				t.Fatal(e)
			}
			if p, e := s.EnsureRunFilesDir(ctx, "r"); e == nil {
				t.Fatalf("ancestor link accepted: %s", p)
			}
		})
	}
}
