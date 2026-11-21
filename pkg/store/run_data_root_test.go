//go:build linux || darwin

package store

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestIncarnationRootRejectsAncestorLinks(t *testing.T) {
	_, scope := scopeFixture(t, RunDataHeader{RunID: "r", BirthID: "birth", Format: LegacyValueFormat, Protocol: IncarnationRunData})
	for depth := 0; depth < 4; depth++ {
		t.Run(fmt.Sprint(depth), func(t *testing.T) {
			root := t.TempDir()
			outside := t.TempDir()
			parts := []string{".run-data-v1", "f1", "r", "birth"}
			parent := filepath.Join(append([]string{root}, parts[:depth]...)...)
			if e := os.MkdirAll(parent, 0700); e != nil {
				t.Fatal(e)
			}
			if e := os.Symlink(outside, filepath.Join(parent, parts[depth])); e != nil {
				t.Fatal(e)
			}
			for _, create := range []bool{false, true} {
				f, e := openIncarnationDataRoot(root, scope, create)
				if f != nil {
					f.Close()
				}
				if e == nil {
					t.Fatal("accepted symlink")
				}
			}
			entries, e := os.ReadDir(outside)
			if e != nil || len(entries) != 0 {
				t.Fatal("created outside scope", entries, e)
			}
		})
	}
}
func TestIncarnationRootRetainsOpenDirectory(t *testing.T) {
	_, scope := scopeFixture(t, RunDataHeader{RunID: "r", BirthID: "birth", Format: LegacyValueFormat, Protocol: IncarnationRunData})
	root := t.TempDir()
	f, e := openIncarnationDataRoot(root, scope, true)
	if e != nil {
		t.Fatal(e)
	}
	defer f.Close()
	p, e := scope.FilesystemRoot(root)
	if e != nil {
		t.Fatal(e)
	}
	if e = os.Rename(p, p+"-old"); e != nil {
		t.Fatal(e)
	}
	outside := t.TempDir()
	if e = os.Symlink(outside, p); e != nil {
		t.Fatal(e)
	}
	file, e := f.OpenFile("payload", os.O_CREATE|os.O_WRONLY, 0600)
	if e != nil {
		t.Fatal(e)
	}
	file.Close()
	if _, e = os.Stat(filepath.Join(p+"-old", "payload")); e != nil {
		t.Fatal(e)
	}
	if _, e = os.Stat(filepath.Join(outside, "payload")); !os.IsNotExist(e) {
		t.Fatal("payload escaped", e)
	}
	if f2, e := openIncarnationDataRoot(root, scope, true); e == nil {
		f2.Close()
		t.Fatal("replacement symlink adopted")
	}
}
