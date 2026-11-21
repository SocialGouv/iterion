package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/SocialGouv/iterion/internal/testfs"
)

func writeErasureNote(t *testing.T, path, author string) {
	t.Helper()
	if e := os.MkdirAll(filepath.Dir(path), 0700); e != nil {
		t.Fatal(e)
	}
	raw, e := json.Marshal(RunNote{Author: author, Body: "keep-body"})
	if e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(path, raw, 0600); e != nil {
		t.Fatal(e)
	}
}
func assertErasureNote(t *testing.T, path, author string) {
	t.Helper()
	raw, e := os.ReadFile(path)
	if e != nil {
		t.Fatal(e)
	}
	var note RunNote
	if e = json.Unmarshal(raw, &note); e != nil {
		t.Fatal(e)
	}
	if note.Author != author || note.Body != "keep-body" {
		t.Fatalf("note changed incorrectly: %#v", note)
	}
}
func TestIncarnationGlobalNoteErasureIncludesRetiredOrphans(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprint(legacy), func(t *testing.T) {
			s, e := New(t.TempDir())
			if e != nil {
				t.Fatal(e)
			}
			var targets, untouched []string
			if legacy {
				p := filepath.Join(s.root, "runs", "legacy", "notes", "0000.json")
				writeErasureNote(t, p, " old@example.test ")
				targets = append(targets, p)
			} else {
				testfs.RemoveAll(t, s.root, filepath.Join(s.root, "runs"))
			}
			for _, format := range []ValueFormat{LegacyValueFormat, ExactValueFormat} {
				for _, birth := range []string{"retired", "orphan"} {
					_, scope := scopeFixture(t, RunDataHeader{RunID: "r", BirthID: birth, Format: format, Protocol: IncarnationRunData})
					root, e := scope.FilesystemRoot(s.root)
					if e != nil {
						t.Fatal(e)
					}
					p := filepath.Join(root, "notes", "0000.json")
					writeErasureNote(t, p, "OLD@example.test")
					targets = append(targets, p)
					p = filepath.Join(root, "notes", "0001.json")
					writeErasureNote(t, p, "other@example.test")
					untouched = append(untouched, p)
				}
			}
			n, e := s.ReattributeRunNotes(t.Context(), "old@example.test", "account-id")
			if e != nil || n != int64(len(targets)) {
				t.Fatal(n, e)
			}
			for _, p := range targets {
				assertErasureNote(t, p, "account-id")
			}
			for _, p := range untouched {
				assertErasureNote(t, p, "other@example.test")
			}
			n, e = s.ReattributeRunNotes(t.Context(), "old@example.test", "account-id")
			if e != nil || n != 0 {
				t.Fatal("erasure retry", n, e)
			}
		})
	}
}
func TestIncarnationGlobalNoteErasureRefusesSymlink(t *testing.T) {
	s, e := New(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	_, scope := scopeFixture(t, RunDataHeader{RunID: "r", BirthID: "orphan", Format: LegacyValueFormat, Protocol: IncarnationRunData})
	root, e := scope.FilesystemRoot(s.root)
	if e != nil {
		t.Fatal(e)
	}
	if e = os.MkdirAll(root, 0700); e != nil {
		t.Fatal(e)
	}
	outside := t.TempDir()
	file := filepath.Join(outside, "0000.json")
	writeErasureNote(t, file, "old@example.test")
	if e = os.Symlink(outside, filepath.Join(root, "notes")); e != nil {
		t.Fatal(e)
	}
	n, e := s.ReattributeRunNotes(t.Context(), "old@example.test", "account-id")
	if n != 0 || !errors.Is(e, ErrRunDataIdentityChanged) {
		t.Fatal("unsafe erasure reported complete", n, e)
	}
	assertErasureNote(t, file, "old@example.test")
}
func TestIncarnationGlobalNoteErasureRefusesFormatAliases(t *testing.T) {
	for _, format := range []string{"f0", "f01", "f99"} {
		t.Run(format, func(t *testing.T) {
			s, e := New(t.TempDir())
			if e != nil {
				t.Fatal(e)
			}
			p := filepath.Join(s.root, ".run-data-v1", format, "r", "orphan", "notes", "0000.json")
			writeErasureNote(t, p, "old@example.test")
			n, e := s.ReattributeRunNotes(t.Context(), "old@example.test", "account-id")
			if n != 0 || e == nil {
				t.Fatal("unknown format/alias accepted", n, e)
			}
			assertErasureNote(t, p, "old@example.test")
		})
	}
}
